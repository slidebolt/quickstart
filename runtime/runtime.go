package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/slidebolt/quickstart/install"
)

type State struct {
	PID         int `json:"pid"`
	ManagerPort int `json:"managerPort"`
	APIPort     int `json:"apiPort"`
}

type ComponentStatus struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	RestartCount int    `json:"restartCount"`
	LastError    string `json:"lastError,omitempty"`
}

func StartDetached(root, executable string) error {
	if _, _, err := install.ValidateOwnedRoot(root); err != nil {
		return err
	}
	if state, _ := LoadState(root); state.PID > 0 && processAlive(state.PID) {
		return fmt.Errorf("quickstart is already running with pid %d", state.PID)
	}
	logPath := filepath.Join(root, "logs", "runtime.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(executable, "_supervise", "--dir", root)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func Supervise(ctx context.Context, root string, stdout, stderr io.Writer) error {
	root, _, err := install.ValidateOwnedRoot(root)
	if err != nil {
		return err
	}
	managerPort, err := freePort()
	if err != nil {
		return err
	}
	apiPort, err := freePort()
	if err != nil {
		return err
	}
	if err := injectCoreEnvironment(root, managerPort, apiPort); err != nil {
		return err
	}
	state := State{PID: os.Getpid(), ManagerPort: managerPort, APIPort: apiPort}
	if err := writeState(root, state); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(root, "run", "state.json"))

	cmd := exec.Command(filepath.Join(root, "sb-manager"), "--bin-dir", filepath.Join(root, "bin"), "--config", filepath.Join(root, "config", "runtime.json"), "--status-addr", fmt.Sprintf("127.0.0.1:%d", managerPort))
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, stdout, stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go bootstrapToken(ctx, root, apiPort)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
			return nil
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			return fmt.Errorf("manager did not stop gracefully within 15s")
		}
	}
}

func Stop(root string) error {
	state, err := LoadState(root)
	if err != nil {
		return err
	}
	process, err := os.FindProcess(state.PID)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for processAlive(state.PID) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(state.PID) {
		return fmt.Errorf("runtime did not stop within 15s")
	}
	return nil
}

func LoadState(root string) (State, error) {
	raw, err := os.ReadFile(filepath.Join(root, "run", "state.json"))
	if err != nil {
		return State{}, err
	}
	var state State
	err = json.Unmarshal(raw, &state)
	return state, err
}

func WaitReady(root string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastProblem string
	for time.Now().Before(deadline) {
		state, err := LoadState(root)
		if err != nil {
			lastProblem = err.Error()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !processAlive(state.PID) {
			return fmt.Errorf("runtime exited before becoming ready; see %s", filepath.Join(root, "logs", "runtime.log"))
		}
		problem, err := readinessProblem(root, state)
		if err == nil && problem == "" {
			return nil
		}
		if err != nil {
			lastProblem = err.Error()
		} else {
			lastProblem = problem
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("runtime did not become ready within %s: %s; see %s", timeout, lastProblem, filepath.Join(root, "logs", "runtime.log"))
}

func readinessProblem(root string, state State) (string, error) {
	expected, err := configuredComponents(root)
	if err != nil {
		return "", err
	}
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status/components", state.ManagerPort))
	if err != nil {
		return "manager status unavailable", nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Sprintf("manager status returned HTTP %d", response.StatusCode), nil
	}
	var statuses []ComponentStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&statuses); err != nil {
		return "", err
	}
	byID := make(map[string]ComponentStatus, len(statuses))
	for _, status := range statuses {
		byID[status.ID] = status
	}
	for _, id := range expected {
		status, ok := byID[id]
		if !ok {
			return fmt.Sprintf("configured component %s was not discovered", id), nil
		}
		if status.State != "ready" {
			return fmt.Sprintf("component %s is %s: %s", id, status.State, status.LastError), nil
		}
		if status.RestartCount != 0 {
			return fmt.Sprintf("component %s restarted %d time(s)", id, status.RestartCount), nil
		}
	}
	if _, err := os.Stat(filepath.Join(root, "config", "api-token")); err != nil {
		return "authenticated API is not ready", nil
	}
	return "", nil
}

func configuredComponents(root string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "config", "runtime.json"))
	if err != nil {
		return nil, err
	}
	var document struct {
		Components map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(document.Components))
	for id := range document.Components {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func APIRequest(root, method, path string, body []byte) ([]byte, int, error) {
	state, err := LoadState(root)
	if err != nil {
		return nil, 0, err
	}
	token, err := os.ReadFile(filepath.Join(root, "config", "api-token"))
	if err != nil {
		return nil, 0, fmt.Errorf("API token unavailable: %w", err)
	}
	request, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", state.APIPort, path), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	return data, response.StatusCode, err
}

func injectCoreEnvironment(root string, managerPort, apiPort int) error {
	path := filepath.Join(root, "config", "runtime.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Components map[string]struct {
			Environment map[string]string `json:"environment,omitempty"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	api := document.Components["api"]
	if api.Environment == nil {
		api.Environment = map[string]string{}
	}
	api.Environment["SB_API_PORT"] = strconv.Itoa(apiPort)
	api.Environment["SB_MANAGER_STATUS_URL"] = fmt.Sprintf("http://127.0.0.1:%d", managerPort)
	document.Components["api"] = api
	updated, _ := json.MarshalIndent(document, "", "  ")
	return os.WriteFile(path, append(updated, '\n'), 0600)
}

func bootstrapToken(ctx context.Context, root string, port int) {
	tokenPath := filepath.Join(root, "config", "api-token")
	if _, err := os.Stat(tokenPath); err == nil {
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/tokens", port), strings.NewReader(`{"name":"Quickstart","scopes":["read","control","write","admin"]}`))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			var result struct {
				Token string `json:"token"`
			}
			if response.StatusCode == http.StatusOK {
				_ = json.NewDecoder(response.Body).Decode(&result)
			}
			response.Body.Close()
			if result.Token != "" {
				_ = os.WriteFile(tokenPath, []byte(result.Token+"\n"), 0600)
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

func writeState(root string, state State) error {
	raw, _ := json.MarshalIndent(state, "", "  ")
	return os.WriteFile(filepath.Join(root, "run", "state.json"), append(raw, '\n'), 0600)
}
