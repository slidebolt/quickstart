package runtime

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadinessRequiresEveryConfiguredComponent(t *testing.T) {
	root := t.TempDir()
	writeRuntimeFixture(t, root, `{"components":{"api":{},"plugin-example":{}}}`)
	state, closeServer := statusServer(t, `[{"id":"api","state":"ready","restartCount":0}]`)
	defer closeServer()
	problem, err := readinessProblem(root, state)
	if err != nil || !strings.Contains(problem, "plugin-example was not discovered") {
		t.Fatalf("readinessProblem() = %q, %v", problem, err)
	}
}

func TestReadinessRejectsRestartedComponent(t *testing.T) {
	root := t.TempDir()
	writeRuntimeFixture(t, root, `{"components":{"api":{}}}`)
	if err := os.WriteFile(filepath.Join(root, "config", "api-token"), []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	state, closeServer := statusServer(t, `[{"id":"api","state":"ready","restartCount":1}]`)
	defer closeServer()
	problem, err := readinessProblem(root, state)
	if err != nil || !strings.Contains(problem, "restarted 1 time") {
		t.Fatalf("readinessProblem() = %q, %v", problem, err)
	}
}

func TestReadinessRejectsDegradedResourceComponent(t *testing.T) {
	root := t.TempDir()
	writeRuntimeFixture(t, root, `{"components":{"plugin-example":{}}}`)
	state, closeServer := statusServer(t, `[{"id":"plugin-example","state":"degraded","restartCount":0,"lastError":"resource example-api is unavailable"}]`)
	defer closeServer()
	problem, err := readinessProblem(root, state)
	if err != nil || !strings.Contains(problem, "resource example-api is unavailable") {
		t.Fatalf("readinessProblem() = %q, %v", problem, err)
	}
}

func writeRuntimeFixture(t *testing.T, root, document string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "runtime.json"), []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
}

func statusServer(t *testing.T, body string) (State, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	})}
	go server.Serve(listener)
	return State{ManagerPort: listener.Addr().(*net.TCPAddr).Port}, func() { _ = server.Close() }
}
