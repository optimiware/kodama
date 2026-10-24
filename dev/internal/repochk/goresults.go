package repochk

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/optimiware/kodama/dev/internal/devenv"
)

// DefaultPackages is the infrastructure-free Go scope.
var DefaultPackages = []string{
	"./cmd/...", "./internal/...", "./api/...", "./contracts/...", "./pkg/...", "./test/support/...",
}

// serviceEnv are blanked for a unit run and loaded from the scope for an
// integration run.
var serviceEnv = []string{
	"KODAMA_TEST_POSTGRES_ADMIN_URL", "KODAMA_NATS_URL", "KODAMA_REGISTRY_URL",
	"KODAMA_S3_ENDPOINT", "KODAMA_S3_BUCKET", "KODAMA_S3_PREFIX", "KODAMA_GIT_URL",
	"KODAMA_TEST_KUBECONFIG", "KODAMA_KUBECONFIG", "KODAMA_SECRETS_DIR",
	"KODAMA_REQUIRE_POSTGRES", "KODAMA_REQUIRE_S3", "KODAMA_REQUIRE_REGISTRY", "KODAMA_REQUIRE_KUBERNETES",
}

// ExitTempFail tells the harness that the environment, not the code, is
// unavailable.
const ExitTempFail = 75

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]`)

// GoCheck runs the scoped format/vet/test gate and emits FORGE_RESULTS.
func GoCheck(args []string) int {
	integration := false
	packages := []string{}
	for _, arg := range args {
		if arg == "--integration" {
			integration = true
			continue
		}
		for _, pkg := range strings.Fields(arg) {
			if pkg != "" {
				packages = append(packages, pkg)
			}
		}
	}
	if len(packages) == 0 {
		packages = append([]string{}, DefaultPackages...)
	}
	environment, code := prepareEnv(integration)
	if code >= 0 {
		return code
	}
	root := devenv.Root()
	if err := Fmt(root, packages); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	goBinary := environmentValue(environment, "GO", "go")
	vet := []string{"vet"}
	if !integration {
		vet = append(vet, "-tags", "unit")
	}
	vet = append(vet, packages...)
	if code := stream(goBinary, environment, vet...); code != 0 {
		return code
	}
	test := []string{"test", "-json", "-race", "-count=1"}
	if integration {
		timeout := environmentValue(environment, "KODAMA_GO_TEST_TIMEOUT", "30m")
		test = append(test, "-timeout", timeout)
	} else {
		test = append(test, "-tags", "unit")
	}
	test = append(test, packages...)
	passed, failed, skipped, packageFailed, code := runTests(goBinary, environment, test)
	resultsPath := environmentValue(environment, "FORGE_RESULTS", "")
	if resultsPath != "" {
		if err := writeResults(resultsPath, map[string]any{
			"passed": passed, "failed": failed, "skipped": skipped,
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if code != 0 {
		return code
	}
	if packageFailed {
		return 1
	}
	return 0
}

// prepareEnv removes inherited service variables and, for an integration run,
// provisions and preflights the scope, then loads its private endpoint file.
func prepareEnv(integration bool) ([]string, int) {
	environment := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		skip := false
		for _, key := range serviceEnv {
			if name == key {
				skip = true
				break
			}
		}
		if !skip {
			environment = append(environment, entry)
		}
	}
	if !integration {
		return environment, -1
	}
	scope := environmentValue(environment, "DEV_SCOPE", "")
	if scope == "" {
		scope = environmentValue(environment, "KODAMA_RESOURCE_PREFIX", "")
	}
	if scope == "" {
		scope = "t-001-local"
	}
	provider := environmentValue(environment, "DEV_PROVIDER", "podman")
	requested, err := devenv.RequestedStack("", environment)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return environment, 1
	}
	environment = setValue(environment, "DEV_SCOPE", scope)
	environment = setValue(environment, "DEV_PROVIDER", provider)
	target, err := devenv.Load(scope, provider, requested)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return environment, 1
	}
	hostEnv := filepath.Join(devenv.StateDir(), "environments", scope, "host-env.json")
	if !exists(hostEnv) && environmentValue(environment, "KODAMA_AUTO_PROVISION", "1") != "0" {
		stack := environmentValue(environment, "DEV_STACK", "cluster")
		fmt.Fprintf(os.Stderr, "development scope %s is missing; provisioning stack %s\n", scope, stack)
		if err := target.Up(); err != nil {
			return environment, failEnvironment(environment, fmt.Errorf("provisioning scope %s: %w", scope, err))
		}
	}
	if err := target.Preflight(); err != nil {
		message := err.Error()
		fmt.Fprintln(os.Stderr, message)
		writeInfraPause(environment, message)
		return environment, ExitTempFail
	}
	values, err := readHostEnv(hostEnv)
	if err != nil {
		message := fmt.Sprintf("fixture unavailable: unreadable %s: %v", hostEnv, err)
		fmt.Fprintln(os.Stderr, message)
		writeInfraPause(environment, message)
		return environment, ExitTempFail
	}
	for key, value := range values {
		if strings.HasPrefix(key, "KODAMA_REQUIRE_") {
			continue
		}
		environment = setValue(environment, key, value)
	}
	return environment, -1
}

func readHostEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func failEnvironment(environment []string, err error) int {
	message := err.Error() + "\n"
	fmt.Fprintln(os.Stderr, message)
	writeInfraPause(environment, message)
	return ExitTempFail
}

func writeInfraPause(environment []string, reason string) {
	path := environmentValue(environment, "FORGE_RESULTS", "")
	if path == "" {
		return
	}
	if err := writeResults(path, map[string]any{"infra_pause": strings.TrimSpace(reason), "passed": []string{}, "failed": []string{}, "skipped": []string{}}); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func writeResults(path string, value map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func environmentValue(environment []string, name, fallback string) string {
	prefix := name + "="
	value := fallback
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
		}
	}
	return value
}

func setValue(environment []string, name, value string) []string {
	prefix := name + "="
	updated := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			updated = append(updated, entry)
		}
	}
	return append(updated, prefix+value)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// stream runs a command with inherited streams and returns its exit code.
func stream(name string, environment []string, args ...string) int {
	command := exec.Command(name, args...)
	command.Dir = devenv.Root()
	command.Env = environment
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// runTests streams `go test -json`, echoes its textual output and returns the
// recorded outcomes plus the process exit code.
func runTests(name string, environment []string, args []string) ([]string, []string, []string, bool, int) {
	command := exec.Command(name, args...)
	command.Dir = devenv.Root()
	command.Env = environment
	reader, writer, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, nil, nil, true, 1
	}
	command.Stdout, command.Stderr = writer, writer
	if err := command.Start(); err != nil {
		writer.Close()
		reader.Close()
		fmt.Fprintln(os.Stderr, err)
		return nil, nil, nil, true, 1
	}
	wait := make(chan error, 1)
	go func() {
		err := command.Wait()
		writer.Close()
		wait <- err
	}()
	passed, failed, skipped, packageFailed := ParseGoTestJSON(reader, os.Stdout)
	reader.Close()
	code := 0
	if err := <-wait; err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	return passed, failed, skipped, packageFailed, code
}

type packageState struct {
	action      string
	noTestFiles bool
	passed      map[string]bool
	failed      map[string]bool
	skipped     map[string]bool
}

// ParseGoTestJSON is the FORGE_RESULTS interpretation of `go test -json`:
// every passing test contributes its name and a normalized alias, while a
// failed or skipped parent, child or sibling never counts as passed.
func ParseGoTestJSON(stream io.Reader, echo io.Writer) (passed, failed, skipped []string, packageFailed bool) {
	byPackage := map[string]*packageState{}
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "{") {
			fmt.Fprintln(echo, raw)
			continue
		}
		var event struct {
			Action  string `json:"Action"`
			Package string `json:"Package"`
			Test    string `json:"Test"`
			Output  string `json:"Output"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			fmt.Fprintln(echo, raw)
			continue
		}
		if event.Action == "output" && event.Output != "" {
			fmt.Fprint(echo, event.Output)
		}
		if event.Package == "" || event.Action == "" {
			continue
		}
		state := byPackage[event.Package]
		if state == nil {
			state = &packageState{passed: map[string]bool{}, failed: map[string]bool{}, skipped: map[string]bool{}}
			byPackage[event.Package] = state
		}
		if event.Action == "output" && strings.Contains(event.Output, "[no test files]") {
			state.noTestFiles = true
		}
		switch {
		case event.Test != "":
			switch event.Action {
			case "pass":
				state.passed[event.Test] = true
			case "fail":
				state.failed[event.Test] = true
			case "skip":
				state.skipped[event.Test] = true
			}
		case event.Action == "pass" || event.Action == "fail" || event.Action == "skip":
			if (state.action != "fail" && state.action != "skip") || event.Action == "fail" {
				state.action = event.Action
			}
		}
	}
	passedSet := map[string]bool{}
	failedSet := map[string]bool{}
	skippedSet := map[string]bool{}
	for _, state := range byPackage {
		if state.action == "fail" || ((state.action == "skip" || state.action == "") && !state.noTestFiles) {
			packageFailed = true
		}
		invalid := map[string]bool{}
		for test := range state.failed {
			invalid[test] = true
		}
		for test := range state.skipped {
			invalid[test] = true
		}
		for test := range state.failed {
			for _, part := range strings.Split(test, "/") {
				failedSet[part] = true
				failedSet[normalize(part)] = true
			}
		}
		for test := range state.skipped {
			for _, part := range strings.Split(test, "/") {
				skippedSet[part] = true
				skippedSet[normalize(part)] = true
			}
		}
		for test := range state.passed {
			parts := strings.Split(test, "/")
			for index, part := range parts {
				prefix := strings.Join(parts[:index+1], "/")
				blocked := false
				for bad := range invalid {
					if bad == prefix || strings.HasPrefix(bad, prefix+"/") || strings.HasPrefix(prefix, bad+"/") {
						blocked = true
						break
					}
				}
				if blocked {
					continue
				}
				passedSet[part] = true
				passedSet[normalize(part)] = true
			}
		}
	}
	for name := range failedSet {
		delete(passedSet, name)
	}
	for name := range skippedSet {
		delete(passedSet, name)
	}
	return sorted(passedSet), sorted(failedSet), sorted(skippedSet), packageFailed
}

func sorted(set map[string]bool) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func normalize(name string) string {
	normalized := nonAlphanumeric.ReplaceAllString(strings.ToLower(name), "")
	return strings.TrimPrefix(normalized, "test")
}
