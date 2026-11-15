package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	contract "github.com/slidebolt/sb-contract"
)

func TestValidateAcceptsSystemTool(t *testing.T) {
	cat := validCatalog()
	cat.Components[0].Setup.Tools = []contract.ToolRequirement{{ID: "ffmpeg", Name: "FFmpeg", Source: contract.ToolSourceSystem, Kind: contract.ToolKindExecutable, Required: true, Checks: []contract.ToolCheck{{Arguments: []string{"-version"}}}}}
	if err := cat.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateAcceptsSerializedSetup(t *testing.T) {
	cat := validCatalog()
	cat.Components[0].Setup.Fields = []contract.SetupField{{
		Key: "enabled", Environment: "EXAMPLE_ENABLED", Name: "Enabled", Control: contract.SetupBoolean, Default: json.RawMessage(`true`),
	}}
	if err := cat.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateAcceptsExternalResources(t *testing.T) {
	cat := validCatalog()
	cat.Components[0].Setup.Fields = []contract.SetupField{{Key: "endpoint", Environment: "EXAMPLE_ENDPOINT", Name: "Endpoint", Control: contract.SetupText}}
	cat.Components[0].Setup.Resources = []contract.ExternalResourceDefinition{{
		ID: "example-api", Kind: contract.ExternalResourceService, Name: "Example API", Description: "Supplies example data.",
		Fields: []string{"endpoint"}, Activation: contract.ResourceActivation{Mode: contract.ResourceActivationConfigured, Fields: []string{"endpoint"}},
	}}
	if err := cat.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func validCatalog() Catalog {
	artifact := Artifact{URL: "https://example.invalid/component", SHA256: strings.Repeat("a", 64)}
	return Catalog{
		SchemaVersion: 2,
		Release:       "v1.0.0",
		Manager:       map[string]Artifact{"linux_amd64": artifact},
		Components: []Component{{
			ID: "core", Version: "v1.0.0", Kind: contract.KindService,
			Setup:     contract.SetupDefinition{Mode: contract.SetupRequired, Category: "Core"},
			Artifacts: map[string]Artifact{"linux_amd64": artifact},
		}},
	}
}
