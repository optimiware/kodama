package deploymenttools_test

// Static contract checks. They need no cluster or container and keep the image's
// pins, the scripts, the documented contract and the recorded evidence from
// drifting apart.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	deliveryhttp "github.com/optimiware/kodama/internal/delivery/http"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func scriptPaths(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, n := range []string{"apply.sh", "lookup.sh", "delete.sh", "lib.sh"} {
		out = append(out, filepath.Join(toolsDir(), "scripts", n))
	}
	return out
}

// The image pins kubectl exactly as the repository's tool lock does.
func Test_tools_image_pins_match_the_tool_lock(t *testing.T) {
	var lock struct {
		Kubectl struct {
			Version string            `json:"version"`
			SHA256  map[string]string `json:"sha256"`
		} `json:"kubectl"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(repoRoot(), "dev", "versions.lock.json"))), &lock); err != nil {
		t.Fatal(err)
	}
	cf := readFile(t, filepath.Join(toolsDir(), "Containerfile"))
	for _, want := range []string{
		"ARG KUBECTL_VERSION=" + lock.Kubectl.Version + "\n",
		"ARG KUBECTL_SHA256_AMD64=" + lock.Kubectl.SHA256["linux-amd64"] + "\n",
		"ARG KUBECTL_SHA256_ARM64=" + lock.Kubectl.SHA256["linux-arm64"] + "\n",
		"io.kodama.kubectl.version=\"" + lock.Kubectl.Version + "\"",
	} {
		if !strings.Contains(cf, want) {
			t.Errorf("Containerfile lacks %q", strings.TrimSpace(want))
		}
	}
}

func Test_tools_image_is_pinned_and_unprivileged(t *testing.T) {
	cf := readFile(t, filepath.Join(toolsDir(), "Containerfile"))
	var instructions []string // comments explain; only instructions count
	for _, l := range strings.Split(cf, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			instructions = append(instructions, l)
		}
	}
	code := strings.Join(instructions, "\n")
	if !regexp.MustCompile(`(?m)^FROM \S+@sha256:[0-9a-f]{64}$`).MatchString(cf) {
		t.Error("the base image is not pinned by digest")
	}
	if !regexp.MustCompile(`(?m)^ARG SNAPSHOT=\d{8}T\d{6}Z$`).MatchString(cf) {
		t.Error("apt is not pinned to a snapshot")
	}
	if !strings.Contains(cf, "sha256sum -c -") {
		t.Error("the kubectl download is not verified")
	}
	if !regexp.MustCompile(`(?m)^USER 65532:65532$`).MatchString(cf) {
		t.Error("the image does not run as an unprivileged numeric user")
	}
	for _, bad := range []string{"ENTRYPOINT", "CMD ", "--privileged", "sudo", "docker.sock", "--insecure", "curl -k", "setcap", "chmod u+s", "chmod 4"} {
		if strings.Contains(code, bad) {
			t.Errorf("Containerfile contains %q", bad)
		}
	}
	for _, p := range []string{"docker", "podman", "buildah", "kaniko", "crictl", "nerdctl", "helm"} {
		if regexp.MustCompile(`apt-get install[^\n]*\b` + p + `\b`).MatchString(code) {
			t.Errorf("the image installs %s", p)
		}
	}
}

func Test_tools_scripts_are_safe_and_parse(t *testing.T) {
	for _, p := range scriptPaths(t) {
		src := readFile(t, p)
		name := filepath.Base(p)
		if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v %s", name, err, out)
		}
		if name != "lib.sh" {
			if !strings.Contains(src, "set -euo pipefail") {
				t.Errorf("%s does not run with set -euo pipefail", name)
			}
			info, err := os.Stat(p)
			if err != nil || info.Mode()&0o111 == 0 {
				t.Errorf("%s is not executable", name)
			}
		}
		for _, bad := range []string{"--insecure", "curl -k", "--token", "--privileged", "sudo ", "--kubeconfig", "eval "} {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
		// The token never travels in argv: it is piped to curl's header reader.
		if strings.Contains(src, "Authorization: Bearer") && name != "lib.sh" {
			t.Errorf("%s builds an authorization header itself", name)
		}
	}
	lib := readFile(t, filepath.Join(toolsDir(), "scripts", "lib.sh"))
	if !strings.Contains(lib, "--header @-") || !regexp.MustCompile(`printf 'Authorization: Bearer %s\\n' "\$token" \| curl`).MatchString(lib) {
		t.Error("the runtime token must reach curl on stdin, not in its arguments")
	}
}

// Every runtime route the scripts call exists in the delivery module's route
// table (the contract T-035 owns), with the same method.
func Test_tools_scripts_call_only_published_routes(t *testing.T) {
	published := map[string]bool{}
	for _, r := range deliveryhttp.RuntimeRoutes(deliveryhttp.DefaultRuntimeActions()) {
		published[r.Method+" "+r.Path] = true
	}
	re := regexp.MustCompile(`rt (GET|POST) "/runtime/v1(/attempts/\$ATTEMPT_ID/deployments[^"]*)"`)
	seen := map[string]bool{}
	for _, p := range scriptPaths(t) {
		for _, m := range re.FindAllStringSubmatch(readFile(t, p), -1) {
			segs := strings.Split(m[2], "/")
			for i, s := range segs {
				switch {
				case s == "$ATTEMPT_ID":
					segs[i] = "{attemptId}"
				case strings.HasPrefix(s, "$"):
					if segs[i-1] == "by-action" {
						segs[i] = "{actionKey}"
					} else {
						segs[i] = "{id}"
					}
				}
			}
			route := m[1] + " " + strings.Join(segs, "/")
			seen[route] = true
			if !published[route] {
				t.Errorf("%s calls %s, which the delivery runtime routes do not publish", filepath.Base(p), route)
			}
		}
	}
	for route := range published {
		if !seen[route] {
			t.Errorf("no script uses the published route %s", route)
		}
	}
}

var dieCode = regexp.MustCompile(`\bdie\s+"?([a-z][a-z0-9_]{2,63})"?\s`)

// Every reason code a script can report is documented, and every documented
// output name is produced.
func Test_tools_contract_is_documented(t *testing.T) {
	readme := readFile(t, filepath.Join(toolsDir(), "README.md"))
	codes := map[string]bool{}
	for _, p := range scriptPaths(t) {
		for _, m := range dieCode.FindAllStringSubmatch(readFile(t, p), -1) {
			codes[m[1]] = true
		}
	}
	// Reason codes that are chosen dynamically.
	for _, c := range []string{"apply_not_observed", "apply_partial", "cluster_forbidden", "cluster_unavailable", "callback_unavailable", "callback_denied",
		"callback_conflict", "callback_rejected", "deployment_not_found", "outcome_unknown"} {
		codes[c] = true
	}
	if len(codes) < 20 {
		t.Fatalf("found only %d reason codes; the extraction is broken", len(codes))
	}
	var missing []string
	for c := range codes {
		if !strings.Contains(readme, "`"+c+"`") {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("reason codes missing from images/deployment-tools/README.md: %v", missing)
	}
	for _, out := range []string{"deployment_id", "deployment_status", "configuration_digest", "image_digest", "source_commit", "ready", "object_count", "verdict", "cleanup_state", "deleted_count"} {
		if !strings.Contains(readme, "`"+out+"`") {
			t.Errorf("output %s is not documented", out)
		}
	}
}

// The recorded build evidence describes the scripts in this tree.
func Test_tools_build_evidence_matches_the_scripts(t *testing.T) {
	var ev struct {
		Task    string `json:"task"`
		Scripts string `json:"scripts"`
		Kubectl string `json:"kubectlVersion"`
		ImageID string `json:"localImageId"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(toolsDir(), "evidence", "build.json"))), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Task != "T-065" || ev.ImageID == "" {
		t.Fatalf("evidence is incomplete: %+v", ev)
	}
	fields := strings.Fields(ev.Scripts)
	recorded := map[string]string{}
	for i := 0; i+1 < len(fields); i += 2 {
		recorded[fields[i+1]] = fields[i]
	}
	for _, p := range scriptPaths(t) {
		name := filepath.Base(p)
		if got := sha256Hex([]byte(readFile(t, p))); recorded[name] != got {
			t.Errorf("evidence/build.json records %s for %s but the script is %s: rebuild with images/deployment-tools/build.sh", recorded[name], name, got)
		}
	}
}
