package install

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slidebolt/quickstart/catalog"
	contract "github.com/slidebolt/sb-contract"
	runtimecontract "github.com/slidebolt/sb-runtime"
)

const MarkerName = ".slidebolt-quickstart"
const markerMagic = "slidebolt-quickstart-v1"

type Marker struct {
	Magic   string `json:"magic"`
	ID      string `json:"id"`
	Release string `json:"release"`
}

type Configuration struct {
	CatalogURL string                       `json:"catalogURL"`
	Release    string                       `json:"release"`
	Plugins    []string                     `json:"plugins"`
	Values     map[string]map[string]string `json:"values"`
	Tools      map[string]string            `json:"tools,omitempty"`
}

type Builder struct {
	Root       string
	CatalogURL string
	Catalog    catalog.Catalog
	Plugins    []string
	Values     map[string]map[string]string
	Tools      map[string]string
	ToolPrompt func(contract.ToolRequirement) (string, error)
	Stdout     io.Writer
}

// Update replaces an owned, stopped installation with the selected catalog
// release. Runtime data, logs, API credentials, and the installation identity
// are preserved. Every artifact and configuration file is staged and verified
// before the first installed path is changed.
func (b *Builder) Update(ctx context.Context) error {
	root, marker, err := ValidateOwnedRoot(b.Root)
	if err != nil {
		return err
	}
	return b.install(ctx, root, marker)
}

func Platform() string { return runtime.GOOS + "_" + runtime.GOARCH }

func (b *Builder) Build(ctx context.Context) (buildErr error) {
	root, err := filepath.Abs(b.Root)
	if err != nil {
		return err
	}
	if err := ensureBuildTarget(root); err != nil {
		return err
	}
	marker := Marker{Magic: markerMagic, ID: randomHex(16), Release: b.Catalog.Release}
	if err := b.install(ctx, root, marker); err != nil {
		return err
	}
	return nil
}

func (b *Builder) install(ctx context.Context, root string, marker Marker) (installErr error) {
	platform := Platform()
	manager, ok := b.Catalog.Manager[platform]
	if !ok {
		return fmt.Errorf("release %s does not support %s", b.Catalog.Release, platform)
	}
	selected, err := resolve(b.Catalog, b.Plugins)
	if err != nil {
		return err
	}
	toolEnvironment, resolvedTools, err := b.resolveTools(ctx, selected)
	if err != nil {
		return err
	}
	stage := filepath.Join(root, ".staging")
	if _, err := os.Lstat(stage); err == nil {
		return fmt.Errorf("staging path already exists: %s", stage)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Join(stage, "bin"), 0700); err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(stage); installErr == nil && err != nil {
			installErr = err
		}
	}()
	if err := download(ctx, manager, filepath.Join(stage, "sb-manager")); err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	for _, component := range selected {
		artifact, ok := component.Artifacts[platform]
		if !ok {
			return fmt.Errorf("component %s does not support %s", component.ID, platform)
		}
		path := filepath.Join(stage, "bin", component.ID)
		if err := download(ctx, artifact, path); err != nil {
			return fmt.Errorf("component %s: %w", component.ID, err)
		}
		if err := verifyHello(ctx, path, component); err != nil {
			return err
		}
	}
	for _, dir := range []string{"config", "data", "logs", "run"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			return err
		}
	}
	values := cloneValues(b.Values)
	if err := preserveGeneratedValues(root, selected, values); err != nil {
		return err
	}
	resolvedValues, err := writeRuntimeConfig(root, filepath.Join(stage, "runtime.json"), selected, values, toolEnvironment)
	if err != nil {
		return err
	}
	marker.Release = b.Catalog.Release
	if err := writeJSON(filepath.Join(stage, MarkerName), marker, 0600); err != nil {
		return err
	}
	config := Configuration{CatalogURL: b.CatalogURL, Release: b.Catalog.Release, Plugins: append([]string(nil), b.Plugins...), Values: resolvedValues, Tools: resolvedTools}
	if err := writeJSON(filepath.Join(stage, "install.json"), config, 0600); err != nil {
		return err
	}
	return commitInstallation(root, stage)
}

func (b *Builder) resolveTools(ctx context.Context, components []catalog.Component) (map[string]map[string]string, map[string]string, error) {
	componentEnvironment := map[string]map[string]string{}
	resolved := map[string]string{}
	for _, component := range components {
		for _, requirement := range component.Setup.Tools {
			path := resolved[requirement.ID]
			configured := path
			if configured == "" {
				configured = b.Tools[requirement.ID]
			}
			check, cancel := context.WithTimeout(ctx, 10*time.Second)
			result, err := runtimecontract.ResolveTool(check, requirement, configured)
			cancel()
			if err != nil && requirement.Required && b.ToolPrompt != nil {
				provided, promptErr := b.ToolPrompt(requirement)
				if promptErr != nil {
					return nil, nil, promptErr
				}
				if strings.TrimSpace(provided) != "" {
					check, cancel = context.WithTimeout(ctx, 10*time.Second)
					result, err = runtimecontract.ResolveTool(check, requirement, provided)
					cancel()
				}
			}
			if err != nil {
				if requirement.Required {
					return nil, nil, fmt.Errorf("component %s requires %s: %w", component.ID, requirement.ID, err)
				}
				continue
			}
			path = result.Path
			resolved[requirement.ID] = path
			if b.Stdout != nil && configured == "" {
				fmt.Fprintf(b.Stdout, "verified %s at %s\n", requirement.Name, path)
			}
			if requirement.Environment != "" {
				if componentEnvironment[component.ID] == nil {
					componentEnvironment[component.ID] = map[string]string{}
				}
				componentEnvironment[component.ID][requirement.Environment] = path
			}
		}
	}
	return componentEnvironment, resolved, nil
}

func resolve(cat catalog.Catalog, requested []string) ([]catalog.Component, error) {
	selected := map[string]catalog.Component{}
	for _, component := range cat.Components {
		if component.Setup.Mode == contract.SetupRequired {
			selected[component.ID] = component
		}
	}
	for _, id := range requested {
		component, ok := cat.Component(id)
		if !ok || component.Setup.Mode != contract.SetupSelectable {
			return nil, fmt.Errorf("unknown selectable plugin %q", id)
		}
		selected[id] = component
	}
	changed := true
	for changed {
		changed = false
		for _, component := range selected {
			for _, id := range component.DependsOn {
				if _, ok := selected[id]; ok {
					continue
				}
				dependency, ok := cat.Component(id)
				if !ok {
					return nil, fmt.Errorf("component %s has missing dependency %s", component.ID, id)
				}
				selected[id] = dependency
				changed = true
			}
		}
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]catalog.Component, 0, len(ids))
	for _, id := range ids {
		out = append(out, selected[id])
	}
	return out, nil
}

func writeRuntimeConfig(root, destination string, components []catalog.Component, values map[string]map[string]string, toolEnvironment map[string]map[string]string) (map[string]map[string]string, error) {
	type componentConfig struct {
		Environment map[string]string `json:"environment,omitempty"`
	}
	document := struct {
		Components map[string]componentConfig `json:"components"`
	}{Components: map[string]componentConfig{}}
	resolvedDocumentValues := map[string]map[string]string{}
	for _, component := range components {
		environment := map[string]string{}
		for key, value := range toolEnvironment[component.ID] {
			environment[key] = value
		}
		resolvedValues := map[string]string{}
		for _, field := range component.Setup.Fields {
			value := values[component.ID][field.Key]
			if value == "" && field.Generate != "" {
				value = generated(field.Generate)
			}
			if value == "" && len(field.Default) > 0 {
				var err error
				value, err = defaultValue(field.Default, field.Export)
				if err != nil {
					return nil, fmt.Errorf("%s.%s default: %w", component.ID, field.Key, err)
				}
			}
			if field.RelativeTo == "sandbox" && value != "" && !filepath.IsAbs(value) {
				value = filepath.Join(root, filepath.Clean(value))
			}
			if field.Required && value == "" {
				return nil, fmt.Errorf("%s.%s is required", component.ID, field.Key)
			}
			resolvedValues[field.Key] = value
			exportedValue := value
			if field.Export == "file_contents" && value != "" {
				raw, err := os.ReadFile(value)
				if err != nil {
					return nil, fmt.Errorf("%s.%s: %w", component.ID, field.Key, err)
				}
				exportedValue = string(raw)
			}
			if field.Environment != "" && exportedValue != "" {
				environment[field.Environment] = exportedValue
			}
		}
		for _, constraint := range component.Setup.Constraints {
			if constraint.Kind != "all_or_none" {
				continue
			}
			set := 0
			for _, key := range constraint.Fields {
				if resolvedValues[key] != "" {
					set++
				}
			}
			if set != 0 && set != len(constraint.Fields) {
				return nil, fmt.Errorf("%s settings %s must be provided together", component.ID, strings.Join(constraint.Fields, ", "))
			}
		}
		resolvedDocumentValues[component.ID] = resolvedValues
		document.Components[component.ID] = componentConfig{Environment: environment}
	}
	if err := writeJSON(destination, document, 0600); err != nil {
		return nil, err
	}
	return resolvedDocumentValues, nil
}

func defaultValue(raw json.RawMessage, export string) (string, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	switch typed := value.(type) {
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			text, ok := item.(string)
			if !ok {
				return "", fmt.Errorf("array defaults must contain strings")
			}
			parts[i] = text
		}
		if export != "csv" {
			return "", fmt.Errorf("array default requires csv export")
		}
		return strings.Join(parts, ","), nil
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("unsupported default type")
	}
}

func generated(kind string) string {
	switch kind {
	case "secret":
		return randomHex(32)
	case "uuid":
		raw := randomHex(16)
		return fmt.Sprintf("%s-%s-%s-%s-%s", raw[:8], raw[8:12], raw[12:16], raw[16:20], raw[20:])
	case "local_mac":
		raw := randomHex(5)
		parts := []string{"02"}
		for i := 0; i < len(raw); i += 2 {
			parts = append(parts, raw[i:i+2])
		}
		return strings.Join(parts, ":")
	case "free_port":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return ""
		}
		defer listener.Close()
		return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	default:
		return ""
	}
}

func cloneValues(source map[string]map[string]string) map[string]map[string]string {
	result := map[string]map[string]string{}
	for component, fields := range source {
		result[component] = map[string]string{}
		for key, value := range fields {
			result[component][key] = value
		}
	}
	return result
}

// preserveGeneratedValues supports installations created before resolved setup
// values were persisted in install.json. Only generated fields are recovered;
// declared values already stored in install.json always take precedence.
func preserveGeneratedValues(root string, components []catalog.Component, values map[string]map[string]string) error {
	raw, err := os.ReadFile(filepath.Join(root, "config", "runtime.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var document struct {
		Components map[string]struct {
			Environment map[string]string `json:"environment"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("existing runtime configuration: %w", err)
	}
	for _, component := range components {
		for _, field := range component.Setup.Fields {
			if field.Generate == "" || field.Environment == "" || values[component.ID][field.Key] != "" {
				continue
			}
			value := document.Components[component.ID].Environment[field.Environment]
			if value == "" {
				continue
			}
			if values[component.ID] == nil {
				values[component.ID] = map[string]string{}
			}
			values[component.ID][field.Key] = value
		}
	}
	return nil
}

func commitInstallation(root, stage string) (commitErr error) {
	type replacement struct {
		stage string
		live  string
		name  string
	}
	replacements := []replacement{
		{filepath.Join(stage, "bin"), filepath.Join(root, "bin"), "bin"},
		{filepath.Join(stage, "sb-manager"), filepath.Join(root, "sb-manager"), "sb-manager"},
		{filepath.Join(stage, "runtime.json"), filepath.Join(root, "config", "runtime.json"), "runtime.json"},
		{filepath.Join(stage, "install.json"), filepath.Join(root, "config", "install.json"), "install.json"},
		{filepath.Join(stage, MarkerName), filepath.Join(root, MarkerName), MarkerName},
	}
	backup := filepath.Join(root, ".rollback")
	if _, err := os.Lstat(backup); err == nil {
		return fmt.Errorf("rollback path already exists: %s", backup)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(backup, 0700); err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(backup); commitErr == nil && err != nil {
			commitErr = err
		}
	}()

	backedUp := make([]replacement, 0, len(replacements))
	installed := make([]replacement, 0, len(replacements))
	rollback := func() {
		for i := len(installed) - 1; i >= 0; i-- {
			_ = os.RemoveAll(installed[i].live)
		}
		for i := len(backedUp) - 1; i >= 0; i-- {
			_ = os.Rename(filepath.Join(backup, backedUp[i].name), backedUp[i].live)
		}
	}
	for _, item := range replacements {
		if _, err := os.Lstat(item.live); err == nil {
			if err := os.Rename(item.live, filepath.Join(backup, item.name)); err != nil {
				rollback()
				return err
			}
			backedUp = append(backedUp, item)
		} else if !os.IsNotExist(err) {
			rollback()
			return err
		}
		if err := os.Rename(item.stage, item.live); err != nil {
			rollback()
			return err
		}
		installed = append(installed, item)
	}
	return nil
}

func download(ctx context.Context, artifact catalog.Artifact, destination string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	temporary := destination + ".tmp"
	defer os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, 1<<30))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, artifact.SHA256) {
		return fmt.Errorf("checksum mismatch: got %s", got)
	}
	return os.Rename(temporary, destination)
}

func verifyHello(ctx context.Context, path string, expected catalog.Component) error {
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(check, path, "hello").Output()
	if err != nil {
		return fmt.Errorf("component %s hello: %w", expected.ID, err)
	}
	var hello contract.HelloResponse
	if err := json.Unmarshal(output, &hello); err != nil {
		return fmt.Errorf("component %s hello: %w", expected.ID, err)
	}
	if err := hello.Validate(); err != nil {
		return fmt.Errorf("component %s hello: %w", expected.ID, err)
	}
	if hello.ID != expected.ID || hello.Kind != expected.Kind || !reflect.DeepEqual(hello.DependsOn, expected.DependsOn) || !reflect.DeepEqual(hello.Setup, &expected.Setup) {
		return fmt.Errorf("component %s manifest does not match catalog", expected.ID)
	}
	return nil
}

func ensureBuildTarget(root string) error {
	if root == string(filepath.Separator) {
		return fmt.Errorf("refusing root directory")
	}
	info, statErr := os.Lstat(root)
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("build target must not be a symlink: %s", root)
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("build target must be empty: %s", root)
	}
	return nil
}

func replacePath(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("destination already exists: %s", destination)
	}
	return os.Rename(source, destination)
}

func writeJSON(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func randomHex(size int) string {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func LoadConfiguration(root string) (Configuration, error) {
	raw, err := os.ReadFile(filepath.Join(root, "config", "install.json"))
	if err != nil {
		return Configuration{}, err
	}
	var config Configuration
	err = json.Unmarshal(raw, &config)
	return config, err
}

func ValidateOwnedRoot(root string) (string, Marker, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", Marker{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", Marker{}, err
	}
	home, _ := os.UserHomeDir()
	if resolved == string(filepath.Separator) || resolved == home || resolved == filepath.Dir(home) {
		return "", Marker{}, fmt.Errorf("refusing broad path %s", resolved)
	}
	raw, err := os.ReadFile(filepath.Join(resolved, MarkerName))
	if err != nil {
		return "", Marker{}, fmt.Errorf("not a quickstart installation: %w", err)
	}
	var marker Marker
	if json.Unmarshal(raw, &marker) != nil || marker.Magic != markerMagic || marker.ID == "" {
		return "", Marker{}, fmt.Errorf("invalid quickstart ownership marker")
	}
	return resolved, marker, nil
}

func Destroy(root string) error {
	resolved, _, err := ValidateOwnedRoot(root)
	if err != nil {
		return err
	}
	return os.RemoveAll(resolved)
}
