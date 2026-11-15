package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/slidebolt/quickstart/catalog"
	contract "github.com/slidebolt/sb-contract"
)

func TestBuildResolvesComponentsWritesEnvironmentAndDestroys(t *testing.T) {
	platform := Platform()
	setup := contract.SetupDefinition{
		Mode: contract.SetupRequired, Category: "Core",
		Fields: []contract.SetupField{
			{Key: "enabled", Environment: "EXAMPLE_ENABLED", Name: "Enabled", Control: contract.SetupBoolean, Default: json.RawMessage(`true`)},
			{Key: "path", Environment: "EXAMPLE_PATH", Name: "Path", Control: contract.SetupDirectory, Default: json.RawMessage(`"data/example"`), RelativeTo: "sandbox"},
		},
	}
	hello := contract.HelloResponse{ID: "core", Kind: contract.KindService, ContractVersion: contract.ContractVersion, Setup: &setup}
	rawHello, _ := json.Marshal(hello)
	component := append([]byte("#!/bin/sh\nprintf '%s\\n' '"), rawHello...)
	component = append(component, []byte("'\n")...)
	manager := []byte("#!/bin/sh\nexit 0\n")
	server := artifactServer(t, map[string][]byte{"/component": component, "/manager": manager})
	defer server.Close()

	cat := catalog.Catalog{
		SchemaVersion: 2, Release: "v1.2.3",
		Manager: map[string]catalog.Artifact{platform: artifact(server.URL+"/manager", manager)},
		Components: []catalog.Component{{
			ID: "core", Version: "v1.0.0", Kind: contract.KindService, Setup: setup,
			Artifacts: map[string]catalog.Artifact{platform: artifact(server.URL+"/component", component)},
		}},
	}
	root := filepath.Join(t.TempDir(), "sandbox")
	builder := Builder{Root: root, CatalogURL: "test-catalog", Catalog: cat, Values: map[string]map[string]string{}}
	if err := builder.Build(context.Background()); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".staging")); !os.IsNotExist(err) {
		t.Fatalf("staging remains after build: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "config", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"EXAMPLE_ENABLED": "true"`) || !strings.Contains(string(raw), filepath.Join(root, "data/example")) {
		t.Fatalf("runtime config did not serialize defaults:\n%s", raw)
	}
	if err := Destroy(root); err != nil {
		t.Fatalf("Destroy() = %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("sandbox remains after destroy: %v", err)
	}
}

func TestBuildCleansStagingAfterChecksumFailure(t *testing.T) {
	platform := Platform()
	server := artifactServer(t, map[string][]byte{"/manager": []byte("bad")})
	defer server.Close()
	root := filepath.Join(t.TempDir(), "sandbox")
	cat := catalog.Catalog{SchemaVersion: 2, Release: "v1", Manager: map[string]catalog.Artifact{platform: {URL: server.URL + "/manager", SHA256: strings.Repeat("0", 64)}}}
	err := (&Builder{Root: root, Catalog: cat}).Build(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Build() = %v, want checksum mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".staging")); !os.IsNotExist(statErr) {
		t.Fatalf("staging remains after failed build: %v", statErr)
	}
}

func TestUpdatePreservesIdentityGeneratedValuesAndRuntimeData(t *testing.T) {
	platform := Platform()
	setup := contract.SetupDefinition{Mode: contract.SetupRequired, Category: "Core", Fields: []contract.SetupField{{
		Key: "token", Environment: "CORE_TOKEN", Name: "Token", Control: contract.SetupSecret, Sensitive: true, Generate: "secret", Required: true,
	}}}
	componentV1 := helloExecutable(t, "core", contract.KindService, setup, "v1")
	componentV2 := helloExecutable(t, "core", contract.KindService, setup, "v2")
	managerV1 := []byte("#!/bin/sh\n# manager-v1\nexit 0\n")
	managerV2 := []byte("#!/bin/sh\n# manager-v2\nexit 0\n")
	server := artifactServer(t, map[string][]byte{
		"/component-v1": componentV1, "/component-v2": componentV2,
		"/manager-v1": managerV1, "/manager-v2": managerV2,
	})
	defer server.Close()
	makeCatalog := func(release, suffix string, componentBytes, managerBytes []byte) catalog.Catalog {
		return catalog.Catalog{
			SchemaVersion: 2, Release: release,
			Manager:    map[string]catalog.Artifact{platform: artifact(server.URL+"/manager-"+suffix, managerBytes)},
			Components: []catalog.Component{{ID: "core", Version: release, Kind: contract.KindService, Setup: setup, Artifacts: map[string]catalog.Artifact{platform: artifact(server.URL+"/component-"+suffix, componentBytes)}}},
		}
	}
	root := filepath.Join(t.TempDir(), "sandbox")
	v1 := makeCatalog("v1", "v1", componentV1, managerV1)
	if err := (&Builder{Root: root, CatalogURL: "catalog-v1", Catalog: v1}).Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeMarkerRaw, _ := os.ReadFile(filepath.Join(root, MarkerName))
	var beforeMarker Marker
	_ = json.Unmarshal(beforeMarkerRaw, &beforeMarker)
	before, err := LoadConfiguration(root)
	if err != nil {
		t.Fatal(err)
	}
	beforeToken := before.Values["core"]["token"]
	if beforeToken == "" {
		t.Fatal("generated token was not persisted")
	}
	if err := os.WriteFile(filepath.Join(root, "data", "kept"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "api-token"), []byte("api-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	v2 := makeCatalog("v2", "v2", componentV2, managerV2)
	if err := (&Builder{Root: root, CatalogURL: "catalog-v2", Catalog: v2, Values: before.Values}).Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := LoadConfiguration(root)
	if err != nil {
		t.Fatal(err)
	}
	if after.Release != "v2" || after.Values["core"]["token"] != beforeToken {
		t.Fatalf("configuration after update = %#v", after)
	}
	_, afterMarker, err := ValidateOwnedRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if afterMarker.ID != beforeMarker.ID || afterMarker.Release != "v2" {
		t.Fatalf("marker after update = %#v; before = %#v", afterMarker, beforeMarker)
	}
	for path, want := range map[string]string{
		filepath.Join(root, "data", "kept"):        "data",
		filepath.Join(root, "config", "api-token"): "api-secret",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("preserved file %s = %q, %v", path, got, err)
		}
	}
	manager, _ := os.ReadFile(filepath.Join(root, "sb-manager"))
	if !strings.Contains(string(manager), "manager-v2") {
		t.Fatalf("manager was not upgraded:\n%s", manager)
	}
}

func TestFailedUpdateLeavesInstalledReleaseUntouched(t *testing.T) {
	platform := Platform()
	setup := contract.SetupDefinition{Mode: contract.SetupRequired, Category: "Core"}
	component := helloExecutable(t, "core", contract.KindService, setup, "component")
	manager := []byte("#!/bin/sh\n# original-manager\nexit 0\n")
	server := artifactServer(t, map[string][]byte{"/component": component, "/manager": manager, "/bad-manager": []byte("bad")})
	defer server.Close()
	v1 := catalog.Catalog{SchemaVersion: 2, Release: "v1", Manager: map[string]catalog.Artifact{platform: artifact(server.URL+"/manager", manager)}, Components: []catalog.Component{{ID: "core", Version: "v1", Kind: contract.KindService, Setup: setup, Artifacts: map[string]catalog.Artifact{platform: artifact(server.URL+"/component", component)}}}}
	root := filepath.Join(t.TempDir(), "sandbox")
	if err := (&Builder{Root: root, Catalog: v1}).Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	v2 := v1
	v2.Release = "v2"
	v2.Manager = map[string]catalog.Artifact{platform: {URL: server.URL + "/bad-manager", SHA256: strings.Repeat("0", 64)}}
	err := (&Builder{Root: root, Catalog: v2}).Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Update() = %v, want checksum mismatch", err)
	}
	configuration, loadErr := LoadConfiguration(root)
	if loadErr != nil || configuration.Release != "v1" {
		t.Fatalf("installed configuration = %#v, %v", configuration, loadErr)
	}
	installed, _ := os.ReadFile(filepath.Join(root, "sb-manager"))
	if !strings.Contains(string(installed), "original-manager") {
		t.Fatalf("installed manager changed after failed update:\n%s", installed)
	}
}

func TestBuildValidatesAndInjectsSystemTool(t *testing.T) {
	platform := Platform()
	tool := filepath.Join(t.TempDir(), "video-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf 'video-tool ready\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	setup := contract.SetupDefinition{Mode: contract.SetupRequired, Category: "Core", Tools: []contract.ToolRequirement{{
		ID: "video-tool", Name: "Video tool", Source: contract.ToolSourceSystem, Kind: contract.ToolKindExecutable,
		Environment: "VIDEO_TOOL_PATH", Required: true, Checks: []contract.ToolCheck{{Arguments: []string{"-version"}, Contains: []string{"ready"}}},
	}}}
	hello := contract.HelloResponse{ID: "media", Kind: contract.KindService, ContractVersion: contract.ContractVersion, Setup: &setup}
	rawHello, _ := json.Marshal(hello)
	component := []byte("#!/bin/sh\nprintf '%s\\n' '" + string(rawHello) + "'\n")
	manager := []byte("#!/bin/sh\nexit 0\n")
	server := artifactServer(t, map[string][]byte{"/component": component, "/manager": manager})
	defer server.Close()
	cat := catalog.Catalog{SchemaVersion: 2, Release: "v1", Manager: map[string]catalog.Artifact{platform: artifact(server.URL+"/manager", manager)}, Components: []catalog.Component{{ID: "media", Version: "v1", Kind: contract.KindService, Setup: setup, Artifacts: map[string]catalog.Artifact{platform: artifact(server.URL+"/component", component)}}}}
	root := filepath.Join(t.TempDir(), "sandbox")
	builder := Builder{Root: root, Catalog: cat, Tools: map[string]string{"video-tool": tool}}
	if err := builder.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "config", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"VIDEO_TOOL_PATH": "`+tool+`"`) {
		t.Fatalf("runtime config does not contain tool path:\n%s", raw)
	}
}

func TestBuildMissingToolDoesNotCreateTarget(t *testing.T) {
	platform := Platform()
	setup := contract.SetupDefinition{Mode: contract.SetupRequired, Category: "Core", Tools: []contract.ToolRequirement{{
		ID: "definitely-missing-tool", Name: "Missing tool", Source: contract.ToolSourceSystem, Kind: contract.ToolKindExecutable,
		Required: true, InstallHint: "Install the missing package.", Checks: []contract.ToolCheck{{Arguments: []string{"-version"}}},
	}}}
	artifact := catalog.Artifact{URL: "https://example.invalid/unused", SHA256: strings.Repeat("a", 64)}
	cat := catalog.Catalog{SchemaVersion: 2, Release: "v1", Manager: map[string]catalog.Artifact{platform: artifact}, Components: []catalog.Component{{ID: "media", Version: "v1", Kind: contract.KindService, Setup: setup, Artifacts: map[string]catalog.Artifact{platform: artifact}}}}
	root := filepath.Join(t.TempDir(), "must-not-exist")
	err := (&Builder{Root: root, Catalog: cat}).Build(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Install the missing package") {
		t.Fatalf("Build() = %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("build target was created before preflight: %v", statErr)
	}
}

func TestDestroyRejectsUnownedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := Destroy(root); err == nil || !strings.Contains(err.Error(), "not a quickstart installation") {
		t.Fatalf("Destroy() = %v, want ownership error", err)
	}
}

func TestResolveAddsHiddenDependencyOnlyWhenPluginNeedsIt(t *testing.T) {
	cat := catalog.Catalog{Components: []catalog.Component{
		{ID: "core", Setup: contract.SetupDefinition{Mode: contract.SetupRequired}},
		{ID: "media", Setup: contract.SetupDefinition{Mode: contract.SetupDependency}},
		{ID: "plugin-tv", DependsOn: []string{"core", "media"}, Setup: contract.SetupDefinition{Mode: contract.SetupSelectable}},
		{ID: "plugin-light", DependsOn: []string{"core"}, Setup: contract.SetupDefinition{Mode: contract.SetupSelectable}},
	}}
	components, err := resolve(cat, []string{"plugin-light"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := componentIDs(components); strings.Contains(ids, "media") {
		t.Fatalf("unneeded media dependency resolved: %s", ids)
	}
	components, err = resolve(cat, []string{"plugin-tv"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := componentIDs(components); !strings.Contains(ids, "media") {
		t.Fatalf("media dependency missing: %s", ids)
	}
}

func TestResolveAddsNestedAsteriskSpeechDependencies(t *testing.T) {
	cat := catalog.Catalog{Components: []catalog.Component{
		{ID: "messenger", Setup: contract.SetupDefinition{Mode: contract.SetupRequired}},
		{ID: "storage", DependsOn: []string{"messenger"}, Setup: contract.SetupDefinition{Mode: contract.SetupRequired}},
		{ID: "media", DependsOn: []string{"messenger"}, Setup: contract.SetupDefinition{Mode: contract.SetupDependency}},
		{ID: "speech", DependsOn: []string{"messenger"}, Setup: contract.SetupDefinition{Mode: contract.SetupDependency}},
		{ID: "asterisk-host", DependsOn: []string{"messenger", "media", "speech"}, Setup: contract.SetupDefinition{Mode: contract.SetupDependency}},
		{ID: "plugin-asterisk", DependsOn: []string{"messenger", "storage", "asterisk-host"}, Setup: contract.SetupDefinition{Mode: contract.SetupSelectable}},
	}}
	components, err := resolve(cat, []string{"plugin-asterisk"})
	if err != nil {
		t.Fatal(err)
	}
	ids := componentIDs(components)
	for _, want := range []string{"asterisk-host", "media", "speech", "plugin-asterisk"} {
		if !strings.Contains(ids, want) {
			t.Fatalf("%s missing from resolved components: %s", want, ids)
		}
	}
}

func componentIDs(components []catalog.Component) string {
	ids := make([]string, 0, len(components))
	for _, component := range components {
		ids = append(ids, component.ID)
	}
	return strings.Join(ids, ",")
}

func TestDefaultValue(t *testing.T) {
	for _, tc := range []struct{ raw, export, want string }{
		{`true`, "", "true"}, {`42`, "", "42"}, {`["a","b"]`, "csv", "a,b"},
	} {
		got, err := defaultValue(json.RawMessage(tc.raw), tc.export)
		if err != nil || got != tc.want {
			t.Fatalf("defaultValue(%s) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
}

func artifactServer(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
}

func artifact(url string, data []byte) catalog.Artifact {
	sum := sha256.Sum256(data)
	return catalog.Artifact{URL: url, SHA256: hex.EncodeToString(sum[:])}
}

func helloExecutable(t *testing.T, id, kind string, setup contract.SetupDefinition, label string) []byte {
	t.Helper()
	hello := contract.HelloResponse{ID: id, Kind: kind, ContractVersion: contract.ContractVersion, Setup: &setup}
	raw, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	return []byte("#!/bin/sh\n# " + label + "\nprintf '%s\\n' '" + string(raw) + "'\n")
}

func TestPlatformIsLinuxInReleaseVerification(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("quickstart currently publishes Linux artifacts")
	}
}
