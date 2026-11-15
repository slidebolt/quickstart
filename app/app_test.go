package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestMergePluginsIsGenericAndDeterministic(t *testing.T) {
	cat := catalog.Catalog{Components: []catalog.Component{
		{ID: "plugin-a", Setup: contract.SetupDefinition{Mode: contract.SetupSelectable}},
		{ID: "plugin-b", Setup: contract.SetupDefinition{Mode: contract.SetupSelectable}},
	}}
	plugins, err := mergePlugins(cat, []string{"plugin-b"}, []string{"plugin-a"}, []string{"plugin-b"})
	if err != nil || strings.Join(plugins, ",") != "plugin-a" {
		t.Fatalf("mergePlugins() = %v, %v", plugins, err)
	}
}

func TestParseValueFilesAcceptsSensitiveValueWithoutPuttingItInArguments(t *testing.T) {
	cat := catalog.Catalog{Components: []catalog.Component{{
		ID: "plugin-example",
		Setup: contract.SetupDefinition{Fields: []contract.SetupField{{
			Key: "token", Sensitive: true,
		}}},
	}}}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values, err := parseValueFiles(cat, []string{"plugin-example.token=" + path})
	if err != nil || values["plugin-example"]["token"] != "secret-value" {
		t.Fatalf("parseValueFiles() = %#v, %v", values, err)
	}
}

func TestBuildAcceptsSetFileFlag(t *testing.T) {
	var output bytes.Buffer
	application := App{Out: &output, Err: &output}
	code := application.Run(context.Background(), []string{"build", "--catalog", filepath.Join(t.TempDir(), "missing-catalog"), "--set-file", "plugin-example.token=/tmp/token"})
	if code != 1 || strings.Contains(output.String(), "flag provided but not defined") {
		t.Fatalf("build code=%d output=%q", code, output.String())
	}
}

func TestUpgradeRequiresPinnedTarget(t *testing.T) {
	var output bytes.Buffer
	application := App{Out: &output, Err: &output}
	if code := application.Run(context.Background(), []string{"upgrade", "--dir", t.TempDir()}); code != 1 || !strings.Contains(output.String(), "exactly one") {
		t.Fatalf("upgrade code=%d output=%q", code, output.String())
	}
}

func TestCompareReleases(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"v1.0.6", "v1.0.5", 1},
		{"v1.0.5", "v1.0.5", 0},
		{"v1.0.5", "v1.1.0", -1},
	} {
		got, err := compareReleases(test.left, test.right)
		if err != nil || got != test.want {
			t.Fatalf("compareReleases(%q, %q) = %d, %v; want %d", test.left, test.right, got, err, test.want)
		}
	}
	if _, err := compareReleases("latest", "v1.0.0"); err == nil {
		t.Fatal("compareReleases accepted an unpinned release")
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
