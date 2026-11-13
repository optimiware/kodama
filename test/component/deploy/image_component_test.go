//go:build component

// Static and behavioral checks for the adopted image build: the build context
// excludes local state, environments and test fixtures; the Containerfile runs
// non-root from digest-pinned bases and copies no secret; scripts/image builds,
// pushes and prints the registry digest without ever printing a credential.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakePodman records build/push argv and writes a digest to --digestfile.
// PODMAN_BAD_DIGEST=1 writes a syntactically invalid digest so the script's
// post-push validation is observable.
const fakePodman = `#!/usr/bin/env bash
set -u
{
  printf 'podman'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$PODMAN_ARGV_LOG"
case "${1:-}" in
  build) exit 0 ;;
  tag) exit 0 ;;
  manifest)
    case "${2:-}" in
      exists) exit 1 ;;
      create|rm) exit 0 ;;
      push)
        previous=""
        for arg in "$@"; do
          if [[ "$previous" == "--digestfile" ]]; then
            if [[ "${PODMAN_BAD_DIGEST:-0}" == "1" ]]; then
              printf 'not-a-digest\n' >"$arg"
            else
              printf 'sha256:%s\n' "2222222222222222222222222222222222222222222222222222222222222222" >"$arg"
            fi
          fi
          previous="$arg"
        done
        exit 0
        ;;
    esac
    exit 1
    ;;
  push)
    previous=""
    for arg in "$@"; do
      if [[ "$previous" == "--digestfile" ]]; then
        if [[ "${PODMAN_BAD_DIGEST:-0}" == "1" ]]; then
          printf 'not-a-digest\n' >"$arg"
        else
          printf 'sha256:%s\n' "1111111111111111111111111111111111111111111111111111111111111111" >"$arg"
        fi
      fi
      previous="$arg"
    done
    exit 0
    ;;
esac
exit 1
`

// TestContainerBuildContextExcludesLocalState pins the patterns that keep
// operator state, environment files and test fixtures out of the image build
// context. Podman reads .containerignore from the context root; a regression
// that deletes a pattern copies ~/.local/state material or .env files into the
// build context (and possibly the image layer cache).
func TestContainerBuildContextExcludesLocalState(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".containerignore"))
	if err != nil {
		t.Fatalf("read .containerignore: %v", err)
	}
	patterns := make(map[string]bool)
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns[line] = true
		}
	}
	for _, required := range []string{
		".git",
		".work",
		".harness",
		".env*",
		"**/.env*",
		"*.env*",
		"*.pem",
		"**/*.pem",
		"*.key",
		"**/*.key",
		"kubeconfig*",
		"**/kubeconfig*",
		"*.tfvars*",
		"**/*.tfvars*",
		"**/.terraform",
		"**/*.tfstate*",
		"**/*.tfplan",
		"**/*.secret.tfvars*",
		"node_modules",
		"**/node_modules",
		"docs",
		"/test",
	} {
		if !patterns[required] {
			t.Errorf(".containerignore does not exclude %q", required)
		}
	}
}

// TestContainerfileIsNonRootAndPinned pins the runtime contract the chart
// relies on: a non-root UID, digest-pinned bases, no secret copied or set by
// ARG/ENV, and the kodama binary as the entrypoint.
func TestContainerfileIsNonRootAndPinned(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "images", "kodama", "Containerfile"))
	if err != nil {
		t.Fatalf("read Containerfile: %v", err)
	}
	text := string(body)
	froms := 0
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "FROM ") {
			froms++
			if !strings.Contains(trimmed, "@sha256:") {
				t.Errorf("FROM is not digest-pinned: %s", trimmed)
			}
		}
	}
	if froms < 2 {
		t.Errorf("Containerfile has %d FROM stages, want a builder and a runtime stage", froms)
	}
	if !strings.Contains(text, "USER 65532:65532") {
		t.Errorf("Containerfile does not run as UID/GID 65532")
	}
	secretDirective := regexp.MustCompile(`(?i)^\s*(ARG|ENV)\b[^#\n]*(SECRET|PASSWORD|PASSWD|TOKEN|ACCESS_KEY)`)
	for index, line := range strings.Split(text, "\n") {
		if secretDirective.MatchString(line) {
			t.Errorf("Containerfile:%d declares a credential build input: %s", index+1, line)
		}
		if strings.Contains(line, "COPY .env") {
			t.Errorf("Containerfile:%d copies an environment file: %s", index+1, line)
		}
	}
	if !strings.Contains(text, `ENTRYPOINT ["/usr/local/bin/kodama"]`) {
		t.Errorf("Containerfile entrypoint is not the kodama binary")
	}
}

// imageSandbox runs scripts/image against the fake podman.
type imageSandbox struct {
	root    string
	tempDir string
	log     string
	env     []string
}

func newImageSandbox(t *testing.T) *imageSandbox {
	t.Helper()
	root := t.TempDir()
	box := &imageSandbox{
		root:    root,
		tempDir: filepath.Join(root, "tmp"),
		log:     filepath.Join(root, "podman.argv"),
	}
	for _, dir := range []string{box.tempDir, filepath.Join(root, "bin"), filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeExecutable(t, filepath.Join(root, "bin", "podman"), fakePodman)
	box.env = append(os.Environ(),
		"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+filepath.Join(root, "home"),
		"TMPDIR="+box.tempDir,
		"PODMAN_ARGV_LOG="+box.log,
	)
	return box
}

func (b *imageSandbox) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "image")
	command := exec.Command("bash", append([]string{script}, args...)...)
	command.Env = b.env
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run scripts/image: %v\n%s", err, output)
		}
		code = exitError.ExitCode()
	}
	return code, string(output)
}

func (b *imageSandbox) podmanArgv(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(b.log)
	if err != nil {
		t.Fatalf("read podman argv log: %v", err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestImageScriptBuildPushPrintsRegistryDigest proves the script builds, then
// pushes, prints <repository>@sha256:<digest>, and removes the digest file.
// The sentinel in the environment proves no credential is printed.
func TestImageScriptBuildPushPrintsRegistryDigest(t *testing.T) {
	box := newImageSandbox(t)
	secretSentinel := "SCW-SECRET-SENTINEL-P28-a7"
	box.env = append(box.env, "SCW_SECRET_KEY="+secretSentinel)

	code, output := box.run(t, "build-push", "--repository", "registry.example.com/kodama", "--tag", "test-tag")
	if code != 0 {
		t.Fatalf("scripts/image build-push failed: exit %d\n%s", code, output)
	}
	want := "registry.example.com/kodama@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if !strings.Contains(output, want) {
		t.Errorf("output %q does not contain the pushed digest %q", output, want)
	}
	if strings.Contains(output, secretSentinel) {
		t.Errorf("scripts/image printed an environment credential")
	}

	lines := box.podmanArgv(t)
	if len(lines) != 2 {
		t.Fatalf("podman was invoked %d times, want build then push:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[0], "podman\tbuild\t") {
		t.Errorf("first podman call is not build: %s", lines[0])
	}
	if !strings.Contains(lines[0], "-t\tregistry.example.com/kodama:test-tag") {
		t.Errorf("build did not tag the requested reference: %s", lines[0])
	}
	if !strings.HasPrefix(lines[1], "podman\tpush\t") {
		t.Errorf("second podman call is not push: %s", lines[1])
	}

	// The digest file handed to podman was a temporary file and must be gone.
	fields := strings.Split(lines[1], "\t")
	for index, field := range fields {
		if field != "--digestfile" || index+1 >= len(fields) {
			continue
		}
		digestPath := fields[index+1]
		if _, err := os.Stat(digestPath); err == nil {
			t.Errorf("digest file survived the run: %s", digestPath)
		}
	}
}

// TestImageScriptBuildOnlyNeverPushes proves `build` runs the same build and
// prints the local reference without a push.
func TestImageScriptBuildOnlyNeverPushes(t *testing.T) {
	box := newImageSandbox(t)
	code, output := box.run(t, "build", "--repository", "registry.example.com/kodama", "--tag", "test-tag")
	if code != 0 {
		t.Fatalf("scripts/image build failed: exit %d\n%s", code, output)
	}
	if strings.TrimSpace(output) != "registry.example.com/kodama:test-tag" {
		t.Errorf("output = %q; want the local reference", output)
	}
	lines := box.podmanArgv(t)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "podman\tbuild\t") {
		t.Fatalf("podman calls = %q; want one build and no push", lines)
	}
}

// TestImageScriptStampsVersionAndRevision proves the build receives the
// version (the VERSION file by default, --version when given) and the full
// git revision as build arguments and labels, so `kodama version` and the
// image labels name the same source.
func TestImageScriptStampsVersionAndRevision(t *testing.T) {
	versionFile, err := os.ReadFile(filepath.Join(repoRoot(t), "VERSION"))
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	for _, testCase := range []struct {
		name    string
		args    []string
		version string
	}{
		{"default", nil, strings.TrimSpace(string(versionFile))},
		{"explicit", []string{"--version", "9.8.7"}, "9.8.7"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			box := newImageSandbox(t)
			args := append([]string{"build-push", "--repository", "registry.example.com/kodama", "--tag", "test-tag"}, testCase.args...)
			if code, output := box.run(t, args...); code != 0 {
				t.Fatalf("scripts/image build-push failed: exit %d\n%s", code, output)
			}
			build := box.podmanArgv(t)[0]
			for _, want := range []string{
				"--build-arg\tVERSION=" + testCase.version + "\t",
				"--label\torg.opencontainers.image.version=" + testCase.version + "\t",
			} {
				if !strings.Contains(build, want) {
					t.Errorf("build does not carry %q: %s", want, build)
				}
			}
			revision := regexp.MustCompile(`--build-arg\tREVISION=([0-9a-f]{40})\t`).FindStringSubmatch(build)
			if revision == nil {
				t.Fatalf("build does not carry a full REVISION build argument: %s", build)
			}
			if !strings.Contains(build, "--label\torg.opencontainers.image.revision="+revision[1]+"\t") {
				t.Errorf("the revision label differs from the REVISION build argument: %s", build)
			}
		})
	}

	t.Run("refused version", func(t *testing.T) {
		box := newImageSandbox(t)
		code, output := box.run(t, "build-push", "--repository", "registry.example.com/kodama", "--version", "1.0 -X main.revision=x")
		if code == 0 {
			t.Fatalf("scripts/image accepted a version with spaces:\n%s", output)
		}
	})
}

// TestImageScriptRejectsMissingRepositoryAndBadDigest proves the script fails
// closed on the two inputs that would otherwise produce an unusable reference.
func TestImageScriptRejectsMissingRepositoryAndBadDigest(t *testing.T) {
	t.Run("missing_repository", func(t *testing.T) {
		box := newImageSandbox(t)
		code, output := box.run(t, "build-push", "--tag", "test-tag")
		if code == 0 {
			t.Fatalf("scripts/image accepted a missing --repository:\n%s", output)
		}
	})

	t.Run("bad_digest", func(t *testing.T) {
		box := newImageSandbox(t)
		box.env = append(box.env, "PODMAN_BAD_DIGEST=1")
		code, output := box.run(t, "build-push", "--repository", "registry.example.com/kodama", "--tag", "test-tag")
		if code == 0 {
			t.Fatalf("scripts/image accepted an invalid pushed digest:\n%s", output)
		}
	})
}

// TestImageScriptMultiArchAndAlsoTag proves multi-arch manifest builds and
// --also-tag latest work cleanly while printing the pushed manifest digest.
func TestImageScriptMultiArchAndAlsoTag(t *testing.T) {
	box := newImageSandbox(t)
	code, output := box.run(t,
		"build-push",
		"--repository", "registry.example.com/kodama",
		"--tag", "0.2.0",
		"--platforms", "linux/amd64,linux/arm64",
		"--also-tag", "latest",
	)
	if code != 0 {
		t.Fatalf("scripts/image multi-arch build-push failed: exit %d\n%s", code, output)
	}
	want := "registry.example.com/kodama@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	if strings.TrimSpace(output) != want {
		t.Fatalf("output = %q; want %q", strings.TrimSpace(output), want)
	}
	joined := strings.Join(box.podmanArgv(t), "\n")
	for _, expected := range []string{
		"podman\tmanifest\tcreate\tregistry.example.com/kodama:0.2.0",
		"--platform\tlinux/amd64,linux/arm64\t--manifest\tregistry.example.com/kodama:0.2.0",
		"podman\tmanifest\tpush\t--all\t--digestfile",
		"docker://registry.example.com/kodama:0.2.0",
		"podman\tmanifest\tpush\t--all\tregistry.example.com/kodama:0.2.0\tdocker://registry.example.com/kodama:latest",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("multi-arch podman calls missing %q:\n%s", expected, joined)
		}
	}
}
