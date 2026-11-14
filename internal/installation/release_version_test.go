package installation

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestReleaseVersionIsConsistent proves the VERSION file is the one release
// version of the tree: both charts, the stack chart's dependency on the
// control-plane chart, the default image tags and the deploy script's default
// tag all carry it. The release workflow refuses a tag that differs from it.
func TestReleaseVersionIsConsistent(t *testing.T) {
	root := repoRoot(t)
	version := strings.TrimSpace(readText(t, filepath.Join(root, "VERSION")))
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(version) {
		t.Fatalf("VERSION = %q; want MAJOR.MINOR.PATCH with an optional pre-release suffix", version)
	}
	if readText(t, filepath.Join(root, "VERSION")) != version+"\n" {
		t.Errorf("VERSION is not exactly the version and one newline")
	}
	quoted := regexp.QuoteMeta(version)

	for _, check := range []struct {
		path        string
		expression  string
		description string
	}{
		{chartPath(t, "Chart.yaml"), `(?m)^version: ` + quoted + `$`, "kodama chart version"},
		{chartPath(t, "Chart.yaml"), `(?m)^appVersion: "` + quoted + `"$`, "kodama chart appVersion"},
		{chartPath(t, "values.yaml"), `(?m)^image:\n  repository: \S+\n  tag: "` + quoted + `"$`, "kodama chart default image tag"},
		{stackChartPath(t, "Chart.yaml"), `(?m)^version: ` + quoted + `$`, "kodama-stack chart version"},
		{stackChartPath(t, "Chart.yaml"), `(?m)^appVersion: "` + quoted + `"$`, "kodama-stack chart appVersion"},
		{stackChartPath(t, "Chart.yaml"), `(?m)^  - name: kodama\n    version: "` + quoted + `"$`, "kodama-stack dependency on the kodama chart"},
		{stackChartPath(t, "values.yaml"), `(?m)^  image:\n    repository: \S+\n    tag: "` + quoted + `"$`, "kodama-stack default image tag"},
		{filepath.Join(root, "scripts", "lib", "deploy.sh"), `\$\{KODAMA_IMAGE_TAG:-` + quoted + `\}`, "deploy script default image tag"},
	} {
		if !regexp.MustCompile(check.expression).MatchString(readText(t, check.path)) {
			t.Errorf("%s does not carry VERSION %s (%s)", check.path, version, check.description)
		}
	}

	// The documents install a release. A release names itself there; a release
	// candidate or a development version keeps naming one earlier release.
	released := !strings.Contains(version, "-")
	documented := map[string]string{}
	for _, document := range []struct {
		path       string
		expression string
	}{
		{filepath.Join(root, "README.md"), `git clone --branch v(\S+) `},
		{filepath.Join(root, "README.md"), `kodama/kodama:([0-9A-Za-z.-]+)`},
		{filepath.Join(root, "docs", "runbooks", "podman-quickstart.md"), `kodama/kodama:([0-9A-Za-z.-]+)`},
	} {
		matches := regexp.MustCompile(document.expression).FindAllStringSubmatch(readText(t, document.path), -1)
		if len(matches) == 0 {
			t.Errorf("%s names no released version (%s)", document.path, document.expression)
		}
		for _, match := range matches {
			documented[match[1]] = document.path
		}
	}
	for name, path := range documented {
		switch {
		case released && name != version:
			t.Errorf("%s installs version %s; VERSION is the release %s", path, name, version)
		case !released && !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(name):
			t.Errorf("%s installs %s, which is not a release", path, name)
		}
	}
	if len(documented) > 1 {
		t.Errorf("the documents install more than one version: %v", documented)
	}

	// The changelog has a section for a release, and an Unreleased section
	// while the tree is a release candidate or a development version.
	changelog := readText(t, filepath.Join(root, "CHANGELOG.md"))
	section := "## Unreleased"
	if released {
		section = "## " + version
	}
	if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(section) + `$`).MatchString(changelog) {
		t.Errorf("CHANGELOG.md has no %q section for VERSION %s", section, version)
	}
	if released && strings.Contains(changelog, "\n## Unreleased\n") {
		t.Errorf("CHANGELOG.md still has an Unreleased section; VERSION is the release %s", version)
	}
}
