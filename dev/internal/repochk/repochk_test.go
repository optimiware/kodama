package repochk

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/dev/internal/devenv"
)

func TestUnitGoResults(t *testing.T) {
	stream := strings.Join([]string{
		`{"Package":"github.com/optimiware/kodama/internal/execution","Action":"run","Test":"Test_unit_dispatch_replay"}`,
		`{"Package":"github.com/optimiware/kodama/internal/execution","Action":"pass","Test":"Test_unit_dispatch_replay"}`,
		`{"Package":"github.com/optimiware/kodama/internal/execution","Action":"pass","Test":"TestParent/sub_skipped"}`,
		`{"Package":"github.com/optimiware/kodama/internal/execution","Action":"skip","Test":"TestParent/sub_skipped"}`,
		`{"Package":"github.com/optimiware/kodama/internal/execution","Action":"pass","Test":"TestParent"}`,
		`{"Package":"github.com/optimiware/kodama/internal/execution","Action":"pass"}`,
		`{"Package":"github.com/optimiware/kodama/internal/execution/executiontest","Action":"output","Output":"?   \tgithub.com/optimiware/kodama/internal/execution/executiontest\t[no test files]\n"}`,
		`{"Package":"github.com/optimiware/kodama/internal/execution/executiontest","Action":"skip"}`,
	}, "\n") + "\n"
	passed, failed, skipped, packageFailed := ParseGoTestJSON(strings.NewReader(stream), io.Discard)
	if packageFailed {
		t.Fatal("a skipped package without test files failed the run")
	}
	if !contains(passed, "unitdispatchreplay") || !contains(passed, "Test_unit_dispatch_replay") {
		t.Fatalf("passed = %v", passed)
	}
	if contains(passed, "parent") {
		t.Fatalf("a parent with a skipped child counted as passed: %v", passed)
	}
	if !contains(skipped, "subskipped") || len(failed) != 0 {
		t.Fatalf("skipped = %v failed = %v", skipped, failed)
	}
}

func TestUnitGoResultsInfraPause(t *testing.T) {
	results := filepath.Join(t.TempDir(), "results.json")
	t.Setenv("FORGE_RESULTS", results)
	t.Setenv("KODAMA_AUTO_PROVISION", "0")
	t.Setenv("DEV_SCOPE", "t-unit-missing-scope")
	t.Setenv("DEV_PROVIDER", "podman")
	t.Setenv("DEV_STACK", "services")
	environment, code := prepareEnv(true)
	if code != ExitTempFail {
		t.Fatalf("exit code = %d", code)
	}
	if environment == nil {
		t.Fatal("no environment")
	}
	data, err := os.ReadFile(results)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "infra_pause") || !strings.Contains(string(data), "missing dev scope") {
		t.Fatalf("results = %s", data)
	}
}

func TestUnitDocsAnchors(t *testing.T) {
	text := "# Title\n\n## Same\n\n## Same\n"
	anchors := Anchors(text)
	for _, want := range []string{"title", "same", "same-1"} {
		if !anchors[want] {
			t.Fatalf("missing anchor %q in %v", want, anchors)
		}
	}
	root := t.TempDir()
	for _, directory := range []string{"docs", "dev", "contracts", filepath.Join(".opencode", "agents")} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# Root\n\nSee [the guide](docs/guide.md#usage).\n")
	write(filepath.Join("docs", "guide.md"), "# Guide\n\n## Usage\n\n```json\n{\"ok\": true}\n```\n\n```sh\ntrue\n```\n")
	if err := Docs(root); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("docs", "guide.md"), "# Guide\n\n## Other\n")
	if err := Docs(root); err == nil || !strings.Contains(err.Error(), "broken anchor") {
		t.Fatalf("docs error = %v", err)
	}
	write(filepath.Join("docs", "guide.md"), "# Guide\n\n## Usage\n\n```json\n{not json}\n```\n")
	if err := Docs(root); err == nil || !strings.Contains(err.Error(), "fenced JSON") {
		t.Fatalf("docs error = %v", err)
	}
}

func TestUnitConfigCheck(t *testing.T) {
	images := containerfileImages("FROM --platform=linux/amd64 alpine:3.22@sha256:" + strings.Repeat("a", 64) + " AS build\nRUN true\n")
	if len(images) != 1 || !strings.HasPrefix(images[0], "alpine:") {
		t.Fatalf("images = %v", images)
	}
	if err := Config(devenv.Root()); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
