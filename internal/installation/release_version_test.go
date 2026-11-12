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
}
