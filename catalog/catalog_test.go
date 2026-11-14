package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	contract "github.com/slidebolt/sb-contract"
)

func TestValidateRejectsMissingToolResource(t *testing.T) {
	cat := validCatalog()
	cat.Components[0].Setup.Tools = []contract.ToolRequirement{{ID: "ffmpeg", Required: true}}
	if err := cat.Validate(); err == nil || !strings.Contains(err.Error(), "missing resource ffmpeg") {
		t.Fatalf("Validate() = %v, want missing resource", err)
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

func validCatalog() Catalog {
	artifact := Artifact{URL: "https://example.invalid/component", SHA256: strings.Repeat("a", 64)}
	return Catalog{
		SchemaVersion: 1,
		Release:       "v1.0.0",
		Manager:       map[string]Artifact{"linux_amd64": artifact},
		Components: []Component{{
			ID: "core", Version: "v1.0.0", Kind: contract.KindService,
			Setup:     contract.SetupDefinition{Mode: contract.SetupRequired, Category: "Core"},
			Artifacts: map[string]Artifact{"linux_amd64": artifact},
		}},
	}
}
