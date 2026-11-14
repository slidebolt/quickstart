package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	contract "github.com/slidebolt/sb-contract"
)

const DefaultURL = "https://github.com/slidebolt/release/releases/latest/download/quickstart-catalog.json"

type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type Component struct {
	ID        string                   `json:"id"`
	Version   string                   `json:"version"`
	Kind      string                   `json:"kind"`
	DependsOn []string                 `json:"dependsOn,omitempty"`
	Setup     contract.SetupDefinition `json:"setup"`
	Artifacts map[string]Artifact      `json:"artifacts"`
}

type Catalog struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Release       string              `json:"release"`
	Manager       map[string]Artifact `json:"manager"`
	Components    []Component         `json:"components"`
}

func Load(source string) (Catalog, error) {
	var reader io.ReadCloser
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(source)
		if err != nil {
			return Catalog{}, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return Catalog{}, fmt.Errorf("catalog: HTTP %d", resp.StatusCode)
		}
		reader = resp.Body
	} else {
		file, err := os.Open(source)
		if err != nil {
			return Catalog{}, err
		}
		reader = file
	}
	defer reader.Close()
	var result Catalog
	if err := json.NewDecoder(io.LimitReader(reader, 8<<20)).Decode(&result); err != nil {
		return Catalog{}, err
	}
	if err := result.Validate(); err != nil {
		return Catalog{}, err
	}
	return result, nil
}

func (c Catalog) Validate() error {
	if c.SchemaVersion != 2 || c.Release == "" {
		return fmt.Errorf("catalog: invalid schema or release")
	}
	seen := map[string]struct{}{}
	if len(c.Manager) == 0 {
		return fmt.Errorf("catalog: manager artifacts are required")
	}
	for platform, artifact := range c.Manager {
		if err := validateArtifact(platform, artifact); err != nil {
			return fmt.Errorf("catalog: manager: %w", err)
		}
	}
	for _, component := range c.Components {
		if component.ID == "" || component.Version == "" || len(component.Artifacts) == 0 {
			return fmt.Errorf("catalog: incomplete component %q", component.ID)
		}
		if _, ok := seen[component.ID]; ok {
			return fmt.Errorf("catalog: duplicate component %q", component.ID)
		}
		seen[component.ID] = struct{}{}
		if err := component.Setup.Validate(); err != nil {
			return fmt.Errorf("catalog: component %s setup: %w", component.ID, err)
		}
		for platform, artifact := range component.Artifacts {
			if err := validateArtifact(platform, artifact); err != nil {
				return fmt.Errorf("catalog: component %s: %w", component.ID, err)
			}
		}
	}
	for _, component := range c.Components {
		for _, dependency := range component.DependsOn {
			if _, ok := seen[dependency]; !ok {
				return fmt.Errorf("catalog: %s depends on missing %s", component.ID, dependency)
			}
		}
	}
	return nil
}

func validateArtifact(platform string, artifact Artifact) error {
	if platform == "" || artifact.URL == "" || len(artifact.SHA256) != 64 {
		return fmt.Errorf("invalid artifact for %q", platform)
	}
	return nil
}

func (c Catalog) Component(id string) (Component, bool) {
	for _, component := range c.Components {
		if component.ID == id {
			return component, true
		}
	}
	return Component{}, false
}
