package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/slidebolt/quickstart/catalog"
	contract "github.com/slidebolt/sb-contract"
)

func TestVersionCommand(t *testing.T) {
	var output bytes.Buffer
	application := App{Out: &output, Err: &output}
	if code := application.Run(context.Background(), []string{"version"}); code != 0 || strings.TrimSpace(output.String()) != Version {
		t.Fatalf("version code=%d output=%q", code, output.String())
	}
}

func TestParseSettingsRejectsSecretOnCommandLine(t *testing.T) {
	cat := catalog.Catalog{Components: []catalog.Component{{
		ID:    "plugin-example",
		Setup: contract.SetupDefinition{Fields: []contract.SetupField{{Key: "token", Name: "Token", Control: contract.SetupSecret, Sensitive: true}}},
	}}}
	_, err := parseSettings(cat, []string{"plugin-example.token=visible"})
	if err == nil || !strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("parseSettings() = %v, want sensitive-setting error", err)
	}
}

func TestParseSettingsAcceptsDeclaredValue(t *testing.T) {
	cat := catalog.Catalog{Components: []catalog.Component{{
		ID:    "plugin-example",
		Setup: contract.SetupDefinition{Fields: []contract.SetupField{{Key: "url", Name: "URL", Control: contract.SetupText, Default: json.RawMessage(`"http://localhost"`)}}},
	}}}
	values, err := parseSettings(cat, []string{"plugin-example.url=http://example.invalid"})
	if err != nil || values["plugin-example"]["url"] != "http://example.invalid" {
		t.Fatalf("parseSettings() = %#v, %v", values, err)
	}
}
