package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slidebolt/quickstart/catalog"
	"github.com/slidebolt/quickstart/install"
	runtimehost "github.com/slidebolt/quickstart/runtime"
	contract "github.com/slidebolt/sb-contract"
	"golang.org/x/term"
)

type App struct {
	In         io.Reader
	Out        io.Writer
	Err        io.Writer
	Executable string
}

var Version = "development"

func (a App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		if err := a.interactive(ctx); err != nil {
			fmt.Fprintln(a.Err, err)
			return 1
		}
		return 0
	}
	var err error
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Fprintln(a.Out, Version)
		return 0
	case "build":
		err = a.build(ctx, args[1:])
	case "configure":
		err = a.configure(ctx, args[1:])
	case "upgrade":
		err = a.upgrade(ctx, args[1:])
	case "run":
		err = a.runForeground(ctx, args[1:])
	case "start":
		err = a.start(args[1:])
	case "stop":
		err = a.stop(args[1:])
	case "status":
		err = a.status(args[1:])
	case "destroy":
		err = a.destroy(args[1:])
	case "api":
		err = a.api(args[1:])
	case "devices", "entities", "scripts", "schedules", "logs", "definitions", "runtime":
		path := map[string]string{"devices": "/devices", "entities": "/entities", "scripts": "/scripts", "schedules": "/schedules", "logs": "/logs", "definitions": "/definitions", "runtime": "/runtime/components"}[args[0]]
		err = a.get(args[1:], path)
	case "command":
		err = a.command(args[1:])
	case "_supervise":
		err = a.supervise(ctx, args[1:])
	default:
		usage(a.Err)
		return 2
	}
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: slidebolt [build|configure|upgrade|run|start|stop|status|destroy|devices|entities|command|scripts|schedules|logs|definitions|runtime|api|version]")
}

type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ",") }
func (r *repeated) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func (a App) build(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	dir := fs.String("dir", "", "installation directory (default: new temporary directory)")
	catalogURL := fs.String("catalog", catalog.DefaultURL, "released quickstart catalog URL")
	var plugins, settings, valueFiles, tools repeated
	fs.Var(&plugins, "plugin", "selectable plugin ID (repeatable)")
	fs.Var(&settings, "set", "component.field=value (repeatable; secrets are rejected)")
	fs.Var(&valueFiles, "set-file", "component.field=/path (required for secret values)")
	fs.Var(&tools, "tool", "system tool override, id=/absolute/path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	automaticDirectory := *dir == ""
	if automaticDirectory {
		created, err := os.MkdirTemp("", "slidebolt-quickstart-")
		if err != nil {
			return err
		}
		*dir = created
	}
	removeAutomaticDirectory := automaticDirectory
	defer func() {
		if removeAutomaticDirectory {
			_ = os.RemoveAll(*dir)
		}
	}()
	cat, err := catalog.Load(*catalogURL)
	if err != nil {
		return err
	}
	values, err := parseSettings(cat, settings)
	if err != nil {
		return err
	}
	files, err := parseValueFiles(cat, valueFiles)
	if err != nil {
		return err
	}
	mergeSettings(values, files)
	toolOverrides, err := parseToolOverrides(tools)
	if err != nil {
		return err
	}
	builder := install.Builder{Root: *dir, CatalogURL: *catalogURL, Catalog: cat, Plugins: plugins, Values: values, Tools: toolOverrides, Stdout: a.Out}
	if err := builder.Build(ctx); err != nil {
		return err
	}
	removeAutomaticDirectory = false
	fmt.Fprintf(a.Out, "built %s in %s\n", cat.Release, *dir)
	return nil
}

func (a App) configure(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	dirValue := fs.String("dir", "", "installation directory")
	catalogURL := fs.String("catalog", "", "catalog override (normally the installed release)")
	var addPlugins, removePlugins, settings, unset, valueFiles, tools repeated
	fs.Var(&addPlugins, "plugin", "selectable plugin ID to add (repeatable)")
	fs.Var(&removePlugins, "remove-plugin", "selectable plugin ID to remove (repeatable)")
	fs.Var(&settings, "set", "component.field=value (repeatable; secrets are rejected)")
	fs.Var(&unset, "unset", "component.field to restore to its catalog default")
	fs.Var(&valueFiles, "set-file", "component.field=/path (required for secret values)")
	fs.Var(&tools, "tool", "system tool override, id=/absolute/path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveDir(*dirValue)
	if err != nil {
		return err
	}
	if err := requireStopped(root); err != nil {
		return err
	}
	current, err := install.LoadConfiguration(root)
	if err != nil {
		return err
	}
	source := *catalogURL
	if source == "" {
		source = current.CatalogURL
		if source == "" || source == catalog.DefaultURL {
			source = catalog.ReleaseURL(current.Release)
		}
	}
	cat, err := catalog.Load(source)
	if err != nil {
		return err
	}
	if cat.Release != current.Release {
		return fmt.Errorf("configure cannot change releases (%s to %s); use upgrade", current.Release, cat.Release)
	}
	plugins, err := mergePlugins(cat, current.Plugins, addPlugins, removePlugins)
	if err != nil {
		return err
	}
	values := cloneSettings(current.Values)
	parsed, err := parseSettings(cat, settings)
	if err != nil {
		return err
	}
	mergeSettings(values, parsed)
	if err := unsetSettings(cat, values, unset); err != nil {
		return err
	}
	files, err := parseValueFiles(cat, valueFiles)
	if err != nil {
		return err
	}
	mergeSettings(values, files)
	resolvedTools := cloneStrings(current.Tools)
	toolOverrides, err := parseToolOverrides(tools)
	if err != nil {
		return err
	}
	for id, path := range toolOverrides {
		resolvedTools[id] = path
	}
	builder := install.Builder{Root: root, CatalogURL: source, Catalog: cat, Plugins: plugins, Values: values, Tools: resolvedTools, Stdout: a.Out}
	if err := builder.Update(ctx); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "configured %s in %s\n", cat.Release, root)
	return nil
}

func (a App) upgrade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	dirValue := fs.String("dir", "", "installation directory")
	release := fs.String("release", "", "target release tag, for example v1.0.38")
	catalogURL := fs.String("catalog", "", "exact target catalog URL or local path")
	allowDowngrade := fs.Bool("allow-downgrade", false, "explicitly allow installing an older release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*release == "") == (*catalogURL == "") {
		return fmt.Errorf("upgrade requires exactly one of --release or --catalog")
	}
	root, err := resolveDir(*dirValue)
	if err != nil {
		return err
	}
	if err := requireStopped(root); err != nil {
		return err
	}
	current, err := install.LoadConfiguration(root)
	if err != nil {
		return err
	}
	source := *catalogURL
	if source == "" {
		source = catalog.ReleaseURL(*release)
	}
	cat, err := catalog.Load(source)
	if err != nil {
		return err
	}
	if *release != "" && cat.Release != *release {
		return fmt.Errorf("catalog release %s does not match requested %s", cat.Release, *release)
	}
	if cat.Release == current.Release {
		fmt.Fprintf(a.Out, "already running release %s\n", current.Release)
		return nil
	}
	order, err := compareReleases(cat.Release, current.Release)
	if err != nil {
		return err
	}
	if order < 0 && !*allowDowngrade {
		return fmt.Errorf("refusing downgrade from %s to %s without --allow-downgrade", current.Release, cat.Release)
	}
	builder := install.Builder{Root: root, CatalogURL: source, Catalog: cat, Plugins: current.Plugins, Values: current.Values, Tools: current.Tools, Stdout: a.Out}
	if err := builder.Update(ctx); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "upgraded %s to %s in %s\n", current.Release, cat.Release, root)
	return nil
}

func compareReleases(left, right string) (int, error) {
	parse := func(value string) ([3]int, error) {
		var result [3]int
		parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
		if len(parts) != len(result) {
			return result, fmt.Errorf("release %q is not a stable semantic version", value)
		}
		for i, part := range parts {
			parsed, err := strconv.Atoi(part)
			if err != nil || parsed < 0 {
				return result, fmt.Errorf("release %q is not a stable semantic version", value)
			}
			result[i] = parsed
		}
		return result, nil
	}
	a, err := parse(left)
	if err != nil {
		return 0, err
	}
	b, err := parse(right)
	if err != nil {
		return 0, err
	}
	for i := range a {
		if a[i] < b[i] {
			return -1, nil
		}
		if a[i] > b[i] {
			return 1, nil
		}
	}
	return 0, nil
}

func parseToolOverrides(raw []string) (map[string]string, error) {
	result := map[string]string{}
	for _, item := range raw {
		id, path, ok := strings.Cut(item, "=")
		id, path = strings.TrimSpace(id), strings.TrimSpace(path)
		if !ok || id == "" || path == "" {
			return nil, fmt.Errorf("invalid --tool %q; want id=/path/to/executable", item)
		}
		if _, exists := result[id]; exists {
			return nil, fmt.Errorf("duplicate --tool %q", id)
		}
		result[id] = path
	}
	return result, nil
}

func parseSettings(cat catalog.Catalog, raw []string) (map[string]map[string]string, error) {
	values := map[string]map[string]string{}
	for _, item := range raw {
		left, value, ok := strings.Cut(item, "=")
		componentID, fieldKey, split := strings.Cut(left, ".")
		if !ok || !split {
			return nil, fmt.Errorf("invalid --set %q; want component.field=value", item)
		}
		component, found := cat.Component(componentID)
		if !found {
			return nil, fmt.Errorf("unknown component %q", componentID)
		}
		var definition *contract.SetupField
		for i := range component.Setup.Fields {
			if component.Setup.Fields[i].Key == fieldKey {
				definition = &component.Setup.Fields[i]
				break
			}
		}
		if definition == nil {
			return nil, fmt.Errorf("unknown setting %s.%s", componentID, fieldKey)
		}
		if definition.Sensitive {
			return nil, fmt.Errorf("%s.%s is sensitive; enter it in interactive mode", componentID, fieldKey)
		}
		if values[componentID] == nil {
			values[componentID] = map[string]string{}
		}
		values[componentID][fieldKey] = value
	}
	return values, nil
}

func parseValueFiles(cat catalog.Catalog, raw []string) (map[string]map[string]string, error) {
	values := map[string]map[string]string{}
	for _, item := range raw {
		left, path, ok := strings.Cut(item, "=")
		componentID, fieldKey, split := strings.Cut(left, ".")
		if !ok || !split || strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("invalid --set-file %q; want component.field=/path", item)
		}
		field, err := setupField(cat, componentID, fieldKey)
		if err != nil {
			return nil, err
		}
		if !field.Sensitive {
			return nil, fmt.Errorf("%s.%s is not sensitive; use --set", componentID, fieldKey)
		}
		file, err := os.Open(strings.TrimSpace(path))
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", componentID, fieldKey, err)
		}
		rawValue, readErr := io.ReadAll(io.LimitReader(file, 1<<20))
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		value := strings.TrimSuffix(strings.TrimSuffix(string(rawValue), "\n"), "\r")
		if values[componentID] == nil {
			values[componentID] = map[string]string{}
		}
		values[componentID][fieldKey] = value
	}
	return values, nil
}

func setupField(cat catalog.Catalog, componentID, fieldKey string) (*contract.SetupField, error) {
	component, found := cat.Component(componentID)
	if !found {
		return nil, fmt.Errorf("unknown component %q", componentID)
	}
	for i := range component.Setup.Fields {
		if component.Setup.Fields[i].Key == fieldKey {
			return &component.Setup.Fields[i], nil
		}
	}
	return nil, fmt.Errorf("unknown setting %s.%s", componentID, fieldKey)
}

func cloneSettings(source map[string]map[string]string) map[string]map[string]string {
	result := map[string]map[string]string{}
	mergeSettings(result, source)
	return result
}

func mergeSettings(destination, source map[string]map[string]string) {
	for component, fields := range source {
		if destination[component] == nil {
			destination[component] = map[string]string{}
		}
		for key, value := range fields {
			destination[component][key] = value
		}
	}
}

func cloneStrings(source map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range source {
		result[key] = value
	}
	return result
}

func unsetSettings(cat catalog.Catalog, values map[string]map[string]string, raw []string) error {
	for _, item := range raw {
		componentID, fieldKey, ok := strings.Cut(item, ".")
		if !ok {
			return fmt.Errorf("invalid --unset %q; want component.field", item)
		}
		field, err := setupField(cat, componentID, fieldKey)
		if err != nil {
			return err
		}
		if field.Generate != "" {
			return fmt.Errorf("%s.%s is generated and cannot be unset during a safe reconfiguration", componentID, fieldKey)
		}
		delete(values[componentID], fieldKey)
	}
	return nil
}

func mergePlugins(cat catalog.Catalog, current, additions, removals []string) ([]string, error) {
	selected := map[string]bool{}
	for _, id := range current {
		selected[id] = true
	}
	for _, id := range additions {
		component, ok := cat.Component(id)
		if !ok || component.Setup.Mode != contract.SetupSelectable {
			return nil, fmt.Errorf("unknown selectable plugin %q", id)
		}
		selected[id] = true
	}
	for _, id := range removals {
		component, ok := cat.Component(id)
		if !ok || component.Setup.Mode != contract.SetupSelectable {
			return nil, fmt.Errorf("unknown selectable plugin %q", id)
		}
		delete(selected, id)
	}
	result := make([]string, 0, len(selected))
	for id := range selected {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func requireStopped(root string) error {
	running, pid, err := runtimehost.IsRunning(root)
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("quickstart is running with pid %d; stop it before changing the installation", pid)
	}
	return nil
}

func (a App) runForeground(ctx context.Context, args []string) error {
	dir, err := parseDir("run", args, a.Err)
	if err != nil {
		return err
	}
	return runtimehost.Supervise(ctx, dir, a.Out, a.Err)
}

func (a App) start(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	dirValue := fs.String("dir", "", "installation directory")
	_ = fs.Bool("detach", true, "run in the background")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := resolveDir(*dirValue)
	if err != nil {
		return err
	}
	if err := runtimehost.StartDetached(dir, a.Executable); err != nil {
		return err
	}
	if err := runtimehost.WaitReady(dir, 45*time.Second); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "started", dir)
	return nil
}

func (a App) stop(args []string) error {
	dir, err := parseDir("stop", args, a.Err)
	if err != nil {
		return err
	}
	if err := runtimehost.Stop(dir); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "stopped", dir)
	return nil
}

func (a App) status(args []string) error {
	dir, err := parseDir("status", args, a.Err)
	if err != nil {
		return err
	}
	state, err := runtimehost.LoadState(dir)
	if err != nil {
		return fmt.Errorf("stopped: %w", err)
	}
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status/components", state.ManagerPort))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("manager status returned HTTP %d", response.StatusCode)
	}
	_, err = io.Copy(a.Out, response.Body)
	return err
}

func (a App) destroy(args []string) error {
	dir, err := parseDir("destroy", args, a.Err)
	if err != nil {
		return err
	}
	if _, err := runtimehost.LoadState(dir); err == nil {
		if err := runtimehost.Stop(dir); err != nil {
			return err
		}
	}
	if err := install.Destroy(dir); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "destroyed", dir)
	return nil
}

func (a App) get(args []string, path string) error {
	dir, err := parseDir("get", args, a.Err)
	if err != nil {
		return err
	}
	return a.printAPI(dir, http.MethodGet, path, nil)
}

func (a App) api(args []string) error {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	dir := fs.String("dir", "", "installation directory")
	method := fs.String("method", http.MethodGet, "HTTP method")
	path := fs.String("path", "/", "API path")
	body := fs.String("body", "", "JSON request body")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	return a.printAPI(root, strings.ToUpper(*method), *path, []byte(*body))
}

func (a App) command(args []string) error {
	fs := flag.NewFlagSet("command", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	dir := fs.String("dir", "", "installation directory")
	entity := fs.String("entity", "", "plugin.device.entity key")
	action := fs.String("action", "", "serialized action name")
	input := fs.String("input", "{}", "JSON command input")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parts := strings.Split(*entity, ".")
	if len(parts) != 3 || *action == "" || !json.Valid([]byte(*input)) {
		return fmt.Errorf("command requires --entity plugin.device.entity, --action, and valid --input JSON")
	}
	root, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/entities/%s/%s/%s/command/%s", parts[0], parts[1], parts[2], *action)
	return a.printAPI(root, http.MethodPost, path, []byte(*input))
}

func (a App) printAPI(root, method, path string, body []byte) error {
	data, status, err := runtimehost.APIRequest(root, method, path, body)
	if err != nil {
		return err
	}
	if len(data) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, data, "", "  ") == nil {
			fmt.Fprintln(a.Out, pretty.String())
		} else {
			fmt.Fprintln(a.Out, string(data))
		}
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("API returned HTTP %d", status)
	}
	return nil
}

func (a App) supervise(ctx context.Context, args []string) error {
	dir, err := parseDir("_supervise", args, a.Err)
	if err != nil {
		return err
	}
	return runtimehost.Supervise(ctx, dir, a.Out, a.Err)
}

func parseDir(name string, args []string, stderr io.Writer) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "installation directory")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return resolveDir(*dir)
}

func resolveDir(value string) (string, error) {
	if value != "" {
		return filepath.Abs(value)
	}
	if _, err := os.Stat(install.MarkerName); err == nil {
		return filepath.Abs(".")
	}
	return "", fmt.Errorf("--dir is required outside a quickstart directory")
}

func (a App) interactive(ctx context.Context) error {
	reader := bufio.NewReader(a.In)
	fmt.Fprintln(a.Out, "SlideBolt Quickstart")
	fmt.Fprint(a.Out, "Installation directory [temporary]: ")
	dir, _ := reader.ReadString('\n')
	dir = strings.TrimSpace(dir)
	automaticDirectory := dir == ""
	if dir == "" {
		created, err := os.MkdirTemp("", "slidebolt-quickstart-")
		if err != nil {
			return err
		}
		dir = created
	}
	removeAutomaticDirectory := automaticDirectory
	defer func() {
		if removeAutomaticDirectory {
			_ = os.RemoveAll(dir)
		}
	}()
	fmt.Fprintln(a.Out, "Loading released component catalog...")
	cat, err := catalog.Load(catalog.DefaultURL)
	if err != nil {
		return err
	}
	selectable := make([]catalog.Component, 0)
	for _, component := range cat.Components {
		if component.Setup.Mode == contract.SetupSelectable {
			selectable = append(selectable, component)
		}
	}
	sort.Slice(selectable, func(i, j int) bool { return selectable[i].ID < selectable[j].ID })
	for i, component := range selectable {
		fmt.Fprintf(a.Out, "  %d) %s [%s]\n", i+1, component.ID, component.Setup.Category)
	}
	fmt.Fprint(a.Out, "Plugins (comma-separated numbers, blank for none): ")
	selection, _ := reader.ReadString('\n')
	plugins := []string{}
	for _, raw := range strings.Split(strings.TrimSpace(selection), ",") {
		if raw == "" {
			continue
		}
		var index int
		if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &index); err != nil || index < 1 || index > len(selectable) {
			return fmt.Errorf("invalid plugin selection %q", raw)
		}
		plugins = append(plugins, selectable[index-1].ID)
	}
	values := map[string]map[string]string{}
	for _, id := range plugins {
		component, _ := cat.Component(id)
		for _, field := range component.Setup.Fields {
			if field.Generate != "" || field.Advanced {
				continue
			}
			fmt.Fprintf(a.Out, "%s — %s: ", component.ID, field.Name)
			var value string
			if field.Sensitive && term.IsTerminal(int(os.Stdin.Fd())) {
				raw, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(a.Out)
				if err != nil {
					return err
				}
				value = string(raw)
			} else {
				value, _ = reader.ReadString('\n')
				value = strings.TrimSpace(value)
			}
			if value != "" {
				if values[id] == nil {
					values[id] = map[string]string{}
				}
				values[id][field.Key] = value
			}
		}
	}
	builder := install.Builder{Root: dir, CatalogURL: catalog.DefaultURL, Catalog: cat, Plugins: plugins, Values: values, Stdout: a.Out}
	builder.ToolPrompt = func(requirement contract.ToolRequirement) (string, error) {
		fmt.Fprintf(a.Out, "%s was not found. %s\nPath to %s (blank to stop): ", requirement.Name, requirement.InstallHint, requirement.ID)
		value, err := reader.ReadString('\n')
		return strings.TrimSpace(value), err
	}
	if err := builder.Build(ctx); err != nil {
		return err
	}
	if err := runtimehost.StartDetached(dir, a.Executable); err != nil {
		return err
	}
	if err := runtimehost.WaitReady(dir, 45*time.Second); err != nil {
		return err
	}
	removeAutomaticDirectory = false
	fmt.Fprintf(a.Out, "SlideBolt %s is running in %s\n", cat.Release, dir)
	for {
		fmt.Fprint(a.Out, "\n[d]evices [e]ntities [s]cripts schedules [l]ogs [r]untime [q]uit: ")
		choice, _ := reader.ReadString('\n')
		switch strings.TrimSpace(choice) {
		case "d":
			_ = a.printAPI(dir, http.MethodGet, "/devices", nil)
		case "e":
			_ = a.printAPI(dir, http.MethodGet, "/entities", nil)
		case "s":
			_ = a.printAPI(dir, http.MethodGet, "/scripts", nil)
		case "schedules":
			_ = a.printAPI(dir, http.MethodGet, "/schedules", nil)
		case "l":
			_ = a.printAPI(dir, http.MethodGet, "/logs", nil)
		case "r":
			_ = a.printAPI(dir, http.MethodGet, "/runtime/components", nil)
		case "q":
			return runtimehost.Stop(dir)
		}
	}
}
