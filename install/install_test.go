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
		SchemaVersion: 1, Release: "v1.2.3",
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
	cat := catalog.Catalog{SchemaVersion: 1, Release: "v1", Manager: map[string]catalog.Artifact{platform: {URL: server.URL + "/manager", SHA256: strings.Repeat("0", 64)}}}
	err := (&Builder{Root: root, Catalog: cat}).Build(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Build() = %v, want checksum mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".staging")); !os.IsNotExist(statErr) {
		t.Fatalf("staging remains after failed build: %v", statErr)
	}
}

func TestDestroyRejectsUnownedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := Destroy(root); err == nil || !strings.Contains(err.Error(), "not a quickstart installation") {
		t.Fatalf("Destroy() = %v, want ownership error", err)
	}
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

func TestPlatformIsLinuxInReleaseVerification(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("quickstart currently publishes Linux artifacts")
	}
}
