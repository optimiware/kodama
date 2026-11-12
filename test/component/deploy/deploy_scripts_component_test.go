//go:build component

// Adoption tests for the reference deploy candidate (6289127) on master: the
// `migrate` action renders a one-shot migration pod from a heredoc, runs it
// before Helm (the chart declares no Job), and removes it on every exit path;
// the image reference cannot inject YAML; `--dry-run` never contacts a cluster
// or OpenTofu; and the rendered values never emit the ignored
// nats.tls.verifyClient flag.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// fakeDeployKubectl records every call and answers the migrate sequence. The
// creation call prints a deterministic pod name; the wait call returns
// KUBECTL_POD_PHASE; delete is recorded. KUBECTL_HANG=1 stalls the wait so a
// signal test can prove the trap deletes the pod.
const fakeDeployKubectl = `#!/usr/bin/env bash
set -u
line="kubectl"
for arg in "$@"; do line="${line}"$'\t'"${arg}"; done
printf '%s\n' "$line" >> "$KUBECTL_ARGV_LOG"

args=" $* "
if [[ "$args" == *" -f - "* ]]; then
  cat >> "$KUBECTL_STDIN_LOG"
  printf '\n' >> "$KUBECTL_STDIN_LOG"
fi

case "$args" in
  *" get secret kodama-database-admin "*)
    if [[ "${KUBECTL_ADMIN_MISSING:-0}" == "1" ]]; then
      echo 'Error from server (NotFound): secrets "kodama-database-admin" not found' >&2
      exit 1
    fi
    exit 0
    ;;
  *" get pod "*|*" get pod/"*)
    if [[ "${KUBECTL_HANG:-0}" == "1" ]]; then
      sleep 30
    fi
    printf '%s\n' "${KUBECTL_POD_PHASE:-Succeeded}"
    exit 0
    ;;
  *" logs pod/"*)
    printf '%s\n' "${KUBECTL_LOGS:-migration complete}"
    exit 0
    ;;
  *" delete pod/"*)
    exit 0
    ;;
  *" create -o name "*)
    count=0
    if [[ -f "$KUBECTL_POD_COUNTER" ]]; then
      count="$(cat "$KUBECTL_POD_COUNTER")"
    fi
    count=$((count + 1))
    printf '%s\n' "$count" >"$KUBECTL_POD_COUNTER"
    printf 'pod/kodama-migrate-%s\n' "$count"
    exit 0
    ;;
esac
exit 0
`

// fakeDeployHelm answers `dependency build` and `template`; `upgrade` fails
// unless HELM_ALLOW_UPGRADE=1 is explicitly set for live-install tests, so a
// dry-run that accidentally installs is still caught.
const fakeDeployHelm = `#!/usr/bin/env bash
set -u
{
  printf 'helm'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$HELM_ARGV_LOG"
args=" $* "
case "$args" in
  *" dependency build "*) exit 0 ;;
  *" template "*) printf '# Source: fake render\n'; exit 0 ;;
  *" upgrade "*)
    if [[ "${HELM_ALLOW_UPGRADE:-0}" == "1" ]]; then
      exit 0
    fi
    echo 'fake helm: upgrade must not run in dry-run' >&2
    exit 1
    ;;
esac
exit 1
`

// fakeDeployTofu records its argv and always fails, so any real OpenTofu call
// during a dry-run run is observable.
const fakeDeployTofu = `#!/usr/bin/env bash
set -u
{
  printf 'tofu'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$TOFU_ARGV_LOG"
exit 1
`

// fakeDeployPodman records build/push and writes a valid digest to the
// --digestfile path.
const fakeDeployPodman = `#!/usr/bin/env bash
set -u
{
  printf 'podman'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$PODMAN_ARGV_LOG"
case "${1:-}" in
  build) exit 0 ;;
  push)
    previous=""
    for arg in "$@"; do
      if [[ "$previous" == "--digestfile" ]]; then
        printf 'sha256:%s\n' "1111111111111111111111111111111111111111111111111111111111111111" >"$arg"
      fi
      previous="$arg"
    done
    exit 0
    ;;
esac
exit 1
`

// deploySandbox is one isolated scripts/deploy run.
type deploySandbox struct {
	root       string
	stateDir   string
	workDir    string
	tempDir    string
	recordsDir string
	kubeconfig string
	descriptor string
	kubectlLog string
	kubectlIn  string
	podCounter string
	helmLog    string
	tofuLog    string
	podmanLog  string
	script     string
	env        []string
	extraEnv   []string
}

const deployDescriptor = `{
  "cloud": "scw",
  "cluster_name": "stage",
  "installation_id": "kodama-stage",
  "environment": "production",
  "account_or_project_id": "project-1",
  "region": "fr-par",
  "release_namespace": "kodama-system",
  "postgres": {"host": "db.example", "port": 5432, "database_name": "kodama", "admin_user": "kodama_admin", "ssl_mode": "verify-full"},
  "kms": {"key_id": "fr-par/00000000-0000-0000-0000-000000000000", "key_ref": "scw-kms:fr-par/00000000-0000-0000-0000-000000000000", "key_adapter": "scw-kms"},
  "kubernetes": {"cluster_name": "stage", "storage_class": "sbs-default"},
  "tenants": ["tenant-a"]
}
`

func newDeploySandbox(t *testing.T) *deploySandbox {
	t.Helper()
	box := newDeploySandboxRoot(t)
	if err := os.MkdirAll(box.workDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", box.workDir, err)
	}
	writeFile(t, box.descriptor, deployDescriptor)
	writeFile(t, box.kubeconfig, "")
	return box
}

// newDeploySandboxRoot prepares the process fixtures and the state root but
// leaves <stateDir>/scw/stage absent (and writes no descriptor or kubeconfig),
// so a test can observe the script's own ensure_cluster_work_dir mkdir.
func newDeploySandboxRoot(t *testing.T) *deploySandbox {
	t.Helper()
	root := t.TempDir()
	box := &deploySandbox{
		root:       root,
		stateDir:   filepath.Join(root, "state"),
		tempDir:    filepath.Join(root, "tmp"),
		recordsDir: filepath.Join(root, "records"),
		kubectlLog: filepath.Join(root, "records", "kubectl.argv"),
		kubectlIn:  filepath.Join(root, "records", "kubectl.stdin"),
		podCounter: filepath.Join(root, "records", "kubectl.pods"),
		helmLog:    filepath.Join(root, "records", "helm.argv"),
		tofuLog:    filepath.Join(root, "records", "tofu.argv"),
		podmanLog:  filepath.Join(root, "records", "podman.argv"),
	}
	box.workDir = filepath.Join(box.stateDir, "scw", "stage")
	box.kubeconfig = filepath.Join(box.workDir, "kubeconfig.yaml")
	box.descriptor = filepath.Join(box.workDir, "descriptor.json")
	box.script = filepath.Join(repoRoot(t), "scripts", "deploy")

	for _, dir := range []string{
		box.tempDir,
		box.recordsDir,
		filepath.Join(root, "home"),
		filepath.Join(root, "bin"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeExecutable(t, filepath.Join(root, "bin", "kubectl"), fakeDeployKubectl)
	writeExecutable(t, filepath.Join(root, "bin", "helm"), fakeDeployHelm)
	writeExecutable(t, filepath.Join(root, "bin", "tofu"), fakeDeployTofu)
	writeExecutable(t, filepath.Join(root, "bin", "podman"), fakeDeployPodman)

	box.env = append(os.Environ(),
		"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+filepath.Join(root, "home"),
		"TMPDIR="+box.tempDir,
		"KODAMA_DEPLOY_STATE_DIR="+box.stateDir,
		"KODAMA_CLUSTER=stage",
		"KODAMA_IMAGE_REPOSITORY=registry.example.com/kodama",
		"KODAMA_IMAGE_DIGEST=sha256:2222222222222222222222222222222222222222222222222222222222222222",
		"KUBECTL_ARGV_LOG="+box.kubectlLog,
		"KUBECTL_STDIN_LOG="+box.kubectlIn,
		"KUBECTL_POD_COUNTER="+box.podCounter,
		"HELM_ARGV_LOG="+box.helmLog,
		"TOFU_ARGV_LOG="+box.tofuLog,
		"PODMAN_ARGV_LOG="+box.podmanLog,
	)
	return box
}

func (b *deploySandbox) withEnv(entries ...string) *deploySandbox {
	b.extraEnv = append(b.extraEnv, entries...)
	return b
}

func (b *deploySandbox) resetLogs(t *testing.T) {
	t.Helper()
	for _, path := range []string{b.kubectlLog, b.kubectlIn, b.podCounter, b.helmLog, b.tofuLog, b.podmanLog} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("reset %s: %v", path, err)
		}
	}
}

func (b *deploySandbox) command(args ...string) *exec.Cmd {
	command := exec.Command("bash", append([]string{b.script, "scw"}, args...)...)
	command.Env = append(append([]string(nil), b.env...), b.extraEnv...)
	return command
}

func (b *deploySandbox) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	command := b.command(args...)
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run scripts/deploy: %v\n%s", err, output)
		}
		code = exitError.ExitCode()
	}
	return code, string(output)
}

// repoRootForDeploy is not needed; newDeploySandbox resolves the module root.

func (b *deploySandbox) readLog(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func (b *deploySandbox) kubectlLines(t *testing.T) []string {
	t.Helper()
	var lines []string
	for line := range strings.SplitSeq(b.readLog(t, b.kubectlLog), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// indexOfLine returns the index of the first recorded line containing needle.
func indexOfLine(lines []string, needle string) int {
	for index, line := range lines {
		if strings.Contains(line, needle) {
			return index
		}
	}
	return -1
}

// TestDeployMigrateCreatesOneShotPodFromSecrets proves the migrate action
// builds one pod from a heredoc that reads every credential from a Secret (no
// literal URL or password), waits for it, and deletes it. The call order is
// the one the deploy order depends on: check the admin Secret, create, wait,
// logs, delete.
func TestDeployMigrateCreatesOneShotPodFromSecrets(t *testing.T) {
	box := newDeploySandbox(t)
	code, output := box.run(t, "migrate", "--cluster", "stage")
	if code != 0 {
		t.Fatalf("scripts/deploy migrate failed: exit %d\n%s", code, output)
	}

	manifest := box.readLog(t, box.kubectlIn)
	for _, want := range []string{
		"kind: Pod",
		"generateName: kodama-migrate-",
		"restartPolicy: Never",
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"runAsUser: 65532",
		// Hardening: the migration runs SQL and writes nothing to its own
		// filesystem, so a read-only root and a full capability drop are
		// compatible with the command.
		"seccompProfile:",
		"type: RuntimeDefault",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		`capabilities: {drop: ["ALL"]}`,
		"name: kodama-database-admin",
		"key: admin_url",
		"name: kodama-database",
		"key: database_url",
		"KODAMA_DATABASE_URL",
		"image: \"registry.example.com/kodama@sha256:2222222222222222222222222222222222222222222222222222222222222222\"",
		`args: ["migrate"]`,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("migration pod manifest is missing %q:\n%s", want, manifest)
		}
	}
	if strings.Contains(manifest, "postgres://") {
		t.Errorf("migration pod manifest carries a literal database URL:\n%s", manifest)
	}
	if strings.Contains(manifest, "password") || strings.Contains(manifest, "PASSWORD") {
		t.Errorf("migration pod manifest mentions a password:\n%s", manifest)
	}

	lines := box.kubectlLines(t)
	adminCheck := indexOfLine(lines, "get\tsecret\tkodama-database-admin")
	create := indexOfLine(lines, "create\t-o\tname\t-f\t-")
	wait := indexOfLine(lines, "get\tpod/kodama-migrate-1")
	logs := indexOfLine(lines, "logs\tpod/kodama-migrate-1")
	remove := indexOfLine(lines, "delete\tpod/kodama-migrate-1")
	for name, index := range map[string]int{
		"admin preflight": adminCheck,
		"create":          create,
		"wait":            wait,
		"logs":            logs,
		"delete":          remove,
	} {
		if index < 0 {
			t.Fatalf("kubectl never recorded the %s step:\n%s", name, strings.Join(lines, "\n"))
		}
	}
	if !(adminCheck < create && create < wait && wait < logs && logs < remove) {
		t.Errorf("migrate call order = admin %d, create %d, wait %d, logs %d, delete %d; want that order",
			adminCheck, create, wait, logs, remove)
	}
	if strings.Contains(output, "postgres://") {
		t.Errorf("the migrate output printed a database URL: %s", output)
	}
	// Positive control: the script output was captured, so the URL absence
	// check cannot pass against an empty string.
	if !strings.Contains(output, "running pod/kodama-migrate-1") {
		t.Errorf("the migrate output was not captured; the URL absence check would be vacuous: %s", output)
	}
}

// TestDeployMigrateDeletesPodOnFailure proves a failed migration exits non-zero
// and still deletes the pod.
func TestDeployMigrateDeletesPodOnFailure(t *testing.T) {
	box := newDeploySandbox(t).withEnv("KUBECTL_POD_PHASE=Failed")
	code, output := box.run(t, "migrate", "--cluster", "stage")
	if code == 0 {
		t.Fatalf("scripts/deploy migrate succeeded although the pod failed:\n%s", output)
	}
	if indexOfLine(box.kubectlLines(t), "delete\tpod/kodama-migrate-1") < 0 {
		t.Errorf("the failed migration pod was not deleted:\n%s", strings.Join(box.kubectlLines(t), "\n"))
	}
}

// TestDeployMigrateDeleteIsReal proves the cleanup is a real delete, not a
// client-side dry run that would leave the one-shot pod behind. The recorded
// argv must carry no --dry-run and must either not wait for finalizers or
// ignore a pod that is already gone.
func TestDeployMigrateDeleteIsReal(t *testing.T) {
	box := newDeploySandbox(t)
	code, output := box.run(t, "migrate", "--cluster", "stage")
	if code != 0 {
		t.Fatalf("scripts/deploy migrate failed: exit %d\n%s", code, output)
	}
	var deleteLine string
	for _, line := range box.kubectlLines(t) {
		if strings.Contains(line, "delete\tpod/kodama-migrate-1") {
			deleteLine = line
			break
		}
	}
	if deleteLine == "" {
		t.Fatalf("no pod delete was recorded:\n%s", strings.Join(box.kubectlLines(t), "\n"))
	}
	fields := strings.Split(deleteLine, "\t")
	realWait := false
	for _, field := range fields {
		if strings.HasPrefix(field, "--dry-run") {
			t.Errorf("the pod delete is a dry run: %s", deleteLine)
		}
		if field == "--wait=false" || field == "--ignore-not-found" {
			realWait = true
		}
	}
	if !realWait {
		t.Errorf("the pod delete has neither --wait=false nor --ignore-not-found: %s", deleteLine)
	}
}

// TestDeployMigrateRefusesWithoutAdminSecret proves the action fails before
// creating a pod when bootstrap-secrets has not applied
// kodama-database-admin: the container would otherwise crash on a missing
// Secret reference.
func TestDeployMigrateRefusesWithoutAdminSecret(t *testing.T) {
	box := newDeploySandbox(t).withEnv("KUBECTL_ADMIN_MISSING=1")
	code, output := box.run(t, "migrate", "--cluster", "stage")
	if code == 0 {
		t.Fatalf("scripts/deploy migrate succeeded without kodama-database-admin:\n%s", output)
	}
	if !strings.Contains(output, "kodama-database-admin") {
		t.Errorf("the refusal does not name the missing Secret: %s", output)
	}
	if indexOfLine(box.kubectlLines(t), "create\t-o\tname\t-f\t-") >= 0 {
		t.Errorf("a migration pod was created before the admin Secret check")
	}
}

// TestDeployMigrateDryRunRendersWithoutCluster proves --dry-run prints the pod
// manifest and makes no kubectl call.
func TestDeployMigrateDryRunRendersWithoutCluster(t *testing.T) {
	box := newDeploySandbox(t)
	code, output := box.run(t, "migrate", "--cluster", "stage", "--dry-run")
	if code != 0 {
		t.Fatalf("scripts/deploy migrate --dry-run failed: exit %d\n%s", code, output)
	}
	if !strings.Contains(output, "kind: Pod") || !strings.Contains(output, "secretKeyRef") {
		t.Errorf("dry-run did not print the migration pod manifest: %s", output)
	}
	if lines := box.kubectlLines(t); len(lines) != 0 {
		t.Errorf("dry-run contacted the cluster: %s", strings.Join(lines, "\n"))
	}
}

// TestDeployMigrateRefusesImageInjection proves the image reference cannot
// inject a YAML document into the heredoc manifest.
func TestDeployMigrateRefusesImageInjection(t *testing.T) {
	cases := []struct{ name, env string }{
		{name: "repository", env: "KODAMA_IMAGE_REPOSITORY=registry.example.com/kodama\"\n  evil: true"},
		{name: "digest", env: "KODAMA_IMAGE_DIGEST=sha256:../../etc/passwd"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newDeploySandbox(t).withEnv(testCase.env)
			code, output := box.run(t, "migrate", "--cluster", "stage")
			if code == 0 {
				t.Fatalf("scripts/deploy migrate accepted an injectable image %s:\n%s", testCase.name, output)
			}
			if strings.Contains(box.readLog(t, box.kubectlIn), "evil") {
				t.Errorf("an injectable image %s reached the pod manifest", testCase.name)
			}
			if lines := box.kubectlLines(t); len(lines) != 0 {
				t.Errorf("an injectable image %s reached kubectl: %s", testCase.name, strings.Join(lines, "\n"))
			}
		})
	}
}

// TestDeployMigrateDeletesPodOnSignal proves the EXIT/TERM trap removes the
// pod when the operator interrupts the wait, so a one-shot pod does not leak
// into the namespace. TERM has a dedicated handler; USR1 has none, so the USR1
// case pins the EXIT trap on its own (bash runs an EXIT trap on an untrapped
// fatal signal). Removing the EXIT trap therefore fails the USR1 case, and
// removing the TERM handler fails the TERM case through TestDeployMigrateTraps
// because bash would otherwise run EXIT on TERM and make the two equivalent.
func TestDeployMigrateDeletesPodOnSignal(t *testing.T) {
	cases := []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "term", signal: syscall.SIGTERM},
		{name: "usr1", signal: syscall.SIGUSR1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newDeploySandbox(t).withEnv("KUBECTL_HANG=1")
			command := box.command("migrate", "--cluster", "stage")
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var captured strings.Builder
			command.Stdout = &captured
			command.Stderr = &captured
			if err := command.Start(); err != nil {
				t.Fatalf("start scripts/deploy migrate: %v", err)
			}

			deadline := time.Now().Add(5 * time.Second)
			for {
				log := box.readLog(t, box.kubectlLog)
				if strings.Contains(log, "create\t-o\tname\t-f\t-") && strings.Contains(log, "get\tpod/") {
					break
				}
				if time.Now().After(deadline) {
					_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
					_ = command.Wait()
					t.Fatalf("the script never entered the migration pod wait:\n%s", captured.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := syscall.Kill(-command.Process.Pid, testCase.signal); err != nil {
				t.Fatalf("signal the script: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("interrupted migration exited zero\n%s", captured.String())
				}
			case <-time.After(10 * time.Second):
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				<-done
				t.Fatalf("interrupted migration did not exit:\n%s", captured.String())
			}

			if indexOfLine(box.kubectlLines(t), "delete\tpod/kodama-migrate-1") < 0 {
				t.Errorf("the trap did not delete the migration pod on SIG%s:\n%s\nscript output:\n%s", strings.ToUpper(testCase.name), strings.Join(box.kubectlLines(t), "\n"), captured.String())
			}
		})
	}
}

// TestDeployMigrateTraps pins the trap commands for the one-shot pod. The
// behavioral TERM and HUP cases above show cleanup runs; this check keeps both
// the EXIT handler and the TERM handler in place, because bash also runs EXIT
// on TERM, so removing TERM alone cannot be observed behaviorally.
func TestDeployMigrateTraps(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "deploy"))
	if err != nil {
		t.Fatalf("read scripts/deploy: %v", err)
	}
	assertTrapCoverage(t, "scripts/deploy", string(body), "cleanup_migrate_pod")
}

// TestDeployMigrateIsIdempotentAcrossRuns proves a second run creates and
// removes its own pod (generateName) rather than reusing a finished one.
func TestDeployMigrateIsIdempotentAcrossRuns(t *testing.T) {
	box := newDeploySandbox(t)
	for run := 1; run <= 2; run++ {
		code, output := box.run(t, "migrate", "--cluster", "stage")
		if code != 0 {
			t.Fatalf("run %d failed: exit %d\n%s", run, code, output)
		}
	}
	lines := box.kubectlLines(t)
	if got := strings.Count(strings.Join(lines, "\n"), "create\t-o\tname\t-f\t-"); got != 2 {
		t.Errorf("two runs created %d pods, want 2", got)
	}
	for _, pod := range []string{"delete\tpod/kodama-migrate-1", "delete\tpod/kodama-migrate-2"} {
		if indexOfLine(lines, pod) < 0 {
			t.Errorf("two runs did not delete %s:\n%s", pod, strings.Join(lines, "\n"))
		}
	}
}

// TestDeployApplyAndDestroyHonorDryRun proves both destructive actions stop
// before OpenTofu or Helm when --dry-run is set.
func TestDeployApplyAndDestroyHonorDryRun(t *testing.T) {
	for _, action := range []string{"apply", "destroy"} {
		t.Run(action, func(t *testing.T) {
			box := newDeploySandbox(t)
			code, output := box.run(t, action, "--cluster", "stage", "--dry-run")
			if code != 0 {
				t.Fatalf("scripts/deploy %s --dry-run failed: exit %d\n%s", action, code, output)
			}
			if body := box.readLog(t, box.tofuLog); body != "" {
				t.Errorf("%s --dry-run invoked OpenTofu:\n%s", action, body)
			}
			if body := box.readLog(t, box.helmLog); body != "" {
				t.Errorf("%s --dry-run invoked Helm:\n%s", action, body)
			}
		})
	}
}

// TestDeployRenderedValuesOmitsVerifyClient proves the shared values renderer
// does not emit the ignored nats.tls.verifyClient flag and uses verify-full
// for the database DSN exactly once. The gcp and aws descriptors emit
// verify-full too; internal/installation's TestDeployDescriptorsEmitVerifyFull
// pins all three providers.
func TestDeployRenderedValuesOmitsVerifyClient(t *testing.T) {
	box := newDeploySandbox(t)
	code, output := box.run(t, "render-values", "--cluster", "stage")
	if code != 0 {
		t.Fatalf("scripts/deploy render-values failed: exit %d\n%s", code, output)
	}
	valuesPath := filepath.Join(box.workDir, "values.generated.yaml")
	body, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read rendered values: %v", err)
	}
	values := string(body)
	if strings.Contains(values, "verifyClient") {
		t.Errorf("rendered values still emit the ignored nats.tls.verifyClient flag:\n%s", values)
	}
	if strings.Contains(values, "sslmode=verify-ca") {
		t.Errorf("rendered values still emit sslmode=verify-ca:\n%s", values)
	}
	if got := strings.Count(values, "sslmode=verify-full"); got != 1 {
		t.Errorf("rendered values contain sslmode=verify-full %d times, want exactly 1", got)
	}
	if !strings.Contains(values, `"kmsCredentials":`) || !strings.Contains(values, "kodama-kms-credentials") {
		t.Errorf("rendered values do not reference the kodama-kms-credentials Secret:\n%s", values)
	}
}

// TestDeployRenderedValuesWireNATSAuth proves the stage values path names the
// callout Secret and account explicitly instead of relying on chart defaults.
// The same values feed the Helm render, so a stage deploy cannot inherit a
// different Secret name or account from a chart change.
func TestDeployRenderedValuesWireNATSAuth(t *testing.T) {
	box := newDeploySandbox(t)
	code, output := box.run(t, "render-values", "--cluster", "stage")
	if code != 0 {
		t.Fatalf("scripts/deploy render-values failed: exit %d\n%s", code, output)
	}
	body, err := os.ReadFile(filepath.Join(box.workDir, "values.generated.yaml"))
	if err != nil {
		t.Fatalf("read rendered values: %v", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(body, &values); err != nil {
		t.Fatalf("parse rendered values: %v", err)
	}
	nats, ok := values["nats"].(map[string]any)
	if !ok {
		t.Fatalf("rendered values have no nats section:\n%s", body)
	}
	auth, _ := nats["auth"].(map[string]any)
	callout, _ := auth["callout"].(map[string]any)
	if got := callout["existingSecret"]; got != "kodama-nats-auth" {
		t.Errorf("nats.auth.callout.existingSecret = %v, want kodama-nats-auth", got)
	}
	account, _ := callout["account"].(string)
	if account == "" {
		t.Fatalf("nats.auth.callout.account is empty:\n%s", body)
	}
	kodama, ok := values["kodama"].(map[string]any)
	if !ok {
		t.Fatalf("rendered values have no kodama section:\n%s", body)
	}
	secrets, _ := kodama["secrets"].(map[string]any)
	natsAuth, _ := secrets["natsAuth"].(map[string]any)
	if got := natsAuth["existingSecret"]; got != "kodama-nats-auth" {
		t.Errorf("kodama.secrets.natsAuth.existingSecret = %v, want kodama-nats-auth", got)
	}
	if got := natsAuth["account"]; got != account {
		t.Errorf("kodama.secrets.natsAuth.account = %v; nats.auth.callout.account = %v", got, account)
	}
}

// TestDeployOrderIsMigrateThenHelm pins the documented order and proves the
// two actions are independent: migrate never invokes Helm and the Helm
// dry-run never creates a pod. The chart itself declares no Job, which
// internal/installation and the rendered-chart tests pin separately.
func TestDeployOrderIsMigrateThenHelm(t *testing.T) {
	usage, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "deploy"))
	if err != nil {
		t.Fatalf("read scripts/deploy: %v", err)
	}
	text := string(usage)
	migrate := strings.Index(text, "\n  migrate ")
	helm := strings.Index(text, "\n  helm ")
	if migrate < 0 || helm < 0 || migrate > helm {
		t.Fatalf("usage does not list migrate before helm")
	}
	if !strings.Contains(text, "run before helm") {
		t.Errorf("usage does not state that migrate runs before helm")
	}

	box := newDeploySandbox(t)
	if code, output := box.run(t, "migrate", "--cluster", "stage"); code != 0 {
		t.Fatalf("migrate failed: %s", output)
	}
	if body := box.readLog(t, box.helmLog); body != "" {
		t.Errorf("migrate invoked Helm: %s", body)
	}

	box.resetLogs(t)
	if code, output := box.run(t, "helm", "--cluster", "stage", "--dry-run"); code != 0 {
		t.Fatalf("helm --dry-run failed: %s", output)
	}
	if lines := box.kubectlLines(t); len(lines) != 0 {
		t.Errorf("helm --dry-run created a migration pod: %s", strings.Join(lines, "\n"))
	}
}

// fakeDeployTofuDescriptor serves `tofu output -json deployment_descriptor`
// from a fixture file, so the deploy script's own descriptor write (and its
// umask) can be exercised without OpenTofu.
const fakeDeployTofuDescriptor = `#!/usr/bin/env bash
set -u
{
  printf 'tofu'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$TOFU_ARGV_LOG"
cat "${TOFU_DESCRIPTOR_FILE:?TOFU_DESCRIPTOR_FILE must point at the descriptor fixture}"
`

// TestDeployCreatesPrivateWorkDirAndDescriptor proves the script, not the
// test fixture, creates <state>/<cloud>/<cluster> under its own umask 077:
// running with a deployment umask of 022 must still leave the work directory
// and its tfdata/pki children at 0700 and the exported descriptor at 0600.
// The sandbox deliberately does not pre-create the work directory, so deleting
// either `umask 077` (scripts/lib/deploy.sh ensure_cluster_work_dir or the
// descriptor redirects in scripts/deploy) is observable.
func TestDeployCreatesPrivateWorkDirAndDescriptor(t *testing.T) {
	box := newDeploySandboxRoot(t)
	writeExecutable(t, filepath.Join(box.root, "bin", "tofu"), fakeDeployTofuDescriptor)
	descriptorSource := filepath.Join(box.root, "descriptor.source.json")
	writeFile(t, descriptorSource, deployDescriptor)
	box.withEnv("TOFU_DESCRIPTOR_FILE=" + descriptorSource)

	if _, err := os.Stat(box.workDir); !os.IsNotExist(err) {
		t.Fatalf("the sandbox pre-created %s; the script's own mkdir would not be exercised", box.workDir)
	}

	command := exec.Command("bash", "-c", `umask "$1"; shift; exec "$@"`, "kodama-umask", "022",
		"bash", box.script, "scw", "output", "--cluster", "stage")
	command.Env = append(append([]string(nil), box.env...), box.extraEnv...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("scripts/deploy output under umask 022 failed: %v\n%s", err, output)
	}

	for _, dir := range []string{
		box.workDir,
		filepath.Join(box.workDir, "tfdata"),
		filepath.Join(box.workDir, "pki"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Errorf("%s mode = %o, want 700 (the script must apply umask 077 itself)", dir, mode)
		}
	}

	info, err := os.Stat(box.descriptor)
	if err != nil {
		t.Fatalf("stat exported descriptor: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("exported descriptor mode = %o, want 600", mode)
	}
	// Positive control: the file the mode was checked on is the rendered
	// descriptor, not an empty or partial write.
	body := box.readLog(t, box.descriptor)
	if !strings.Contains(body, `"cluster_name": "stage"`) || !strings.Contains(body, `"cloud": "scw"`) {
		t.Errorf("exported descriptor does not contain the rendered deployment descriptor:\n%s", body)
	}
}

// yamlMap decodes one YAML document into a string-keyed map.
func yamlMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("parse rendered manifest: %v\n%s", err, body)
	}
	return document
}

// mapAt returns a nested map and fails when it is missing or of another type.
func mapAt(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key]
	if !ok {
		t.Fatalf("%s is missing", key)
	}
	child, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want a map", key, value)
	}
	return child
}

// listAt returns a nested list and fails when it is missing or of another type.
func listAt(t *testing.T, parent map[string]any, key string) []any {
	t.Helper()
	value, ok := parent[key]
	if !ok {
		t.Fatalf("%s is missing", key)
	}
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T, want a list", key, value)
	}
	return list
}

// exactKeys fails unless the map carries exactly the wanted keys.
func exactKeys(t *testing.T, label string, object map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	wanted := append([]string(nil), want...)
	sort.Strings(wanted)
	if strings.Join(got, ",") != strings.Join(wanted, ",") {
		t.Errorf("%s keys = %v, want exactly %v", label, got, wanted)
	}
}

// TestDeployMigratePodSpecIsExact parses the rendered migration pod and pins
// the whole security-relevant shape: one container, one Secret-backed volume,
// one read-only mount, exactly five Secret-sourced env entries, a maximally
// restricted security context, no host namespaces, no hostPath/privileged
// escape and a digest-pinned image. No extra field survives: substring checks
// alone would accept an added hostPath, hostNetwork, privileged or env entry.
func TestDeployMigratePodSpecIsExact(t *testing.T) {
	box := newDeploySandbox(t)
	code, output := box.run(t, "migrate", "--cluster", "stage", "--dry-run")
	if code != 0 {
		t.Fatalf("scripts/deploy migrate --dry-run failed: exit %d\n%s", code, output)
	}
	manifest := box.readLog(t, box.tofuLog)
	if manifest != "" {
		t.Fatalf("migrate --dry-run invoked OpenTofu: %s", manifest)
	}
	// The action logs its dry-run note on stderr before printing the manifest,
	// so parse from the document header.
	start := strings.Index(output, "apiVersion: v1")
	if start < 0 {
		t.Fatalf("migrate --dry-run did not print a Pod manifest:\n%s", output)
	}
	document := yamlMap(t, output[start:])

	if got := document["apiVersion"]; got != "v1" {
		t.Errorf("apiVersion = %v, want v1", got)
	}
	if got := document["kind"]; got != "Pod" {
		t.Fatalf("kind = %v, want Pod", got)
	}
	metadata := mapAt(t, document, "metadata")
	exactKeys(t, "metadata", metadata, "generateName", "labels")
	if got := metadata["generateName"]; got != "kodama-migrate-" {
		t.Errorf("generateName = %v, want kodama-migrate-", got)
	}
	labels := mapAt(t, metadata, "labels")
	exactKeys(t, "metadata.labels", labels, "app.kubernetes.io/name", "app.kubernetes.io/component")
	if labels["app.kubernetes.io/name"] != "kodama" || labels["app.kubernetes.io/component"] != "migrate" {
		t.Errorf("metadata.labels = %v", labels)
	}

	spec := mapAt(t, document, "spec")
	exactKeys(t, "spec", spec, "restartPolicy", "automountServiceAccountToken", "securityContext", "containers", "volumes")
	if got := spec["restartPolicy"]; got != "Never" {
		t.Errorf("restartPolicy = %v, want Never", got)
	}
	if got := spec["automountServiceAccountToken"]; got != false {
		t.Errorf("automountServiceAccountToken = %v, want false", got)
	}
	for _, forbidden := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if value, ok := spec[forbidden]; ok && value != false {
			t.Errorf("%s = %v; the migration pod must not share a host namespace", forbidden, value)
		}
	}

	podSecurity := mapAt(t, spec, "securityContext")
	exactKeys(t, "spec.securityContext", podSecurity, "runAsNonRoot", "runAsUser", "seccompProfile")
	if podSecurity["runAsNonRoot"] != true {
		t.Errorf("runAsNonRoot = %v, want true", podSecurity["runAsNonRoot"])
	}
	if podSecurity["runAsUser"] != 65532 {
		t.Errorf("runAsUser = %v, want 65532", podSecurity["runAsUser"])
	}
	seccomp := mapAt(t, podSecurity, "seccompProfile")
	exactKeys(t, "spec.securityContext.seccompProfile", seccomp, "type")
	if seccomp["type"] != "RuntimeDefault" {
		t.Errorf("seccompProfile.type = %v, want RuntimeDefault", seccomp["type"])
	}

	containers := listAt(t, spec, "containers")
	if len(containers) != 1 {
		t.Fatalf("spec.containers has %d entries, want exactly 1", len(containers))
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("container is %T, want a map", containers[0])
	}
	exactKeys(t, "container", container, "name", "image", "args", "env", "securityContext", "volumeMounts", "resources")
	if container["name"] != "migrate" {
		t.Errorf("container.name = %v, want migrate", container["name"])
	}
	image, _ := container["image"].(string)
	if !regexp.MustCompile(`^registry\.example\.com/kodama@sha256:[0-9a-f]{64}$`).MatchString(image) {
		t.Errorf("container.image = %q, want a digest-pinned registry reference", image)
	}
	if _, ok := container["command"]; ok {
		t.Errorf("container.command is set; the migration entrypoint must come from the image")
	}
	args := listAt(t, container, "args")
	if len(args) != 1 || args[0] != "migrate" {
		t.Errorf("container.args = %v, want [migrate]", args)
	}

	wantEnv := []struct{ name, secret, key string }{
		{"KODAMA_ADMIN_DATABASE_URL", "kodama-database-admin", "admin_url"},
		{"KODAMA_DATABASE_URL", "kodama-database", "database_url"},
		{"KODAMA_CONTROLLER_DATABASE_URL", "kodama-database", "controller_url"},
		{"KODAMA_PUBLISHER_DATABASE_URL", "kodama-database", "publisher_url"},
		{"KODAMA_AUTH_DATABASE_URL", "kodama-database", "auth_url"},
	}
	env := listAt(t, container, "env")
	if len(env) != len(wantEnv) {
		t.Fatalf("container.env has %d entries, want exactly %d: %v", len(env), len(wantEnv), env)
	}
	for index, expected := range wantEnv {
		entry, ok := env[index].(map[string]any)
		if !ok {
			t.Fatalf("container.env[%d] is %T, want a map", index, env[index])
		}
		exactKeys(t, "container.env["+expected.name+"]", entry, "name", "valueFrom")
		if entry["name"] != expected.name {
			t.Errorf("container.env[%d].name = %v, want %s", index, entry["name"], expected.name)
		}
		if _, ok := entry["value"]; ok {
			t.Errorf("container.env[%d] carries a literal value; every credential must come from a Secret", index)
		}
		valueFrom := mapAt(t, entry, "valueFrom")
		exactKeys(t, "container.env["+expected.name+"].valueFrom", valueFrom, "secretKeyRef")
		reference := mapAt(t, valueFrom, "secretKeyRef")
		exactKeys(t, "container.env["+expected.name+"].valueFrom.secretKeyRef", reference, "name", "key")
		if reference["name"] != expected.secret || reference["key"] != expected.key {
			t.Errorf("container.env[%d] references %v/%v, want %s/%s", index, reference["name"], reference["key"], expected.secret, expected.key)
		}
	}

	containerSecurity := mapAt(t, container, "securityContext")
	exactKeys(t, "container.securityContext", containerSecurity, "allowPrivilegeEscalation", "readOnlyRootFilesystem", "capabilities")
	if containerSecurity["allowPrivilegeEscalation"] != false {
		t.Errorf("allowPrivilegeEscalation = %v, want false", containerSecurity["allowPrivilegeEscalation"])
	}
	if containerSecurity["readOnlyRootFilesystem"] != true {
		t.Errorf("readOnlyRootFilesystem = %v, want true", containerSecurity["readOnlyRootFilesystem"])
	}
	if _, ok := containerSecurity["privileged"]; ok {
		t.Errorf("container.securityContext.privileged is set; the migration must never run privileged")
	}
	capabilities := mapAt(t, containerSecurity, "capabilities")
	exactKeys(t, "container.securityContext.capabilities", capabilities, "drop")
	drop := listAt(t, capabilities, "drop")
	if len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", drop)
	}

	mounts := listAt(t, container, "volumeMounts")
	if len(mounts) != 1 {
		t.Fatalf("container.volumeMounts has %d entries, want exactly 1", len(mounts))
	}
	mount, ok := mounts[0].(map[string]any)
	if !ok {
		t.Fatalf("volumeMount is %T, want a map", mounts[0])
	}
	exactKeys(t, "container.volumeMounts[0]", mount, "name", "mountPath", "readOnly")
	if mount["name"] != "database-ca" || mount["mountPath"] != "/etc/kodama/pg-ca" || mount["readOnly"] != true {
		t.Errorf("volumeMount = %v, want database-ca at /etc/kodama/pg-ca read-only", mount)
	}

	resources := mapAt(t, container, "resources")
	exactKeys(t, "container.resources", resources, "requests", "limits")
	requests := mapAt(t, resources, "requests")
	exactKeys(t, "container.resources.requests", requests, "cpu", "memory")
	if requests["cpu"] != "50m" || requests["memory"] != "64Mi" {
		t.Errorf("resources.requests = %v, want cpu 50m and memory 64Mi", requests)
	}
	limits := mapAt(t, resources, "limits")
	exactKeys(t, "container.resources.limits", limits, "cpu", "memory")
	if limits["cpu"] != "500m" || limits["memory"] != "256Mi" {
		t.Errorf("resources.limits = %v, want cpu 500m and memory 256Mi", limits)
	}

	volumes := listAt(t, spec, "volumes")
	if len(volumes) != 1 {
		t.Fatalf("spec.volumes has %d entries, want exactly 1", len(volumes))
	}
	volume, ok := volumes[0].(map[string]any)
	if !ok {
		t.Fatalf("volume is %T, want a map", volumes[0])
	}
	exactKeys(t, "spec.volumes[0]", volume, "name", "secret")
	if volume["name"] != "database-ca" {
		t.Errorf("volume.name = %v, want database-ca", volume["name"])
	}
	if _, ok := volume["hostPath"]; ok {
		t.Errorf("volume uses hostPath; the migration pod must not mount the host filesystem")
	}
	secret := mapAt(t, volume, "secret")
	exactKeys(t, "spec.volumes[0].secret", secret, "secretName", "items")
	if secret["secretName"] != "kodama-database" {
		t.Errorf("volume.secret.secretName = %v, want kodama-database", secret["secretName"])
	}
	items := listAt(t, secret, "items")
	if len(items) != 1 {
		t.Fatalf("volume.secret.items has %d entries, want exactly 1", len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("volume item is %T, want a map", items[0])
	}
	exactKeys(t, "spec.volumes[0].secret.items[0]", item, "key", "path")
	if item["key"] != "ca.crt" || item["path"] != "ca.crt" {
		t.Errorf("volume secret item = %v, want key/path ca.crt", item)
	}
}

// TestBootstrapStateValidatesInputsAndWrites0600Env proves bootstrap-state
// strictly validates cloud, bucket_name, region and project_or_account before
// writing state-backend.env, and enforces 0600 permissions even over an
// existing broader-mode file.
func TestBootstrapStateValidatesInputsAndWrites0600Env(t *testing.T) {
	bootstrapScript := filepath.Join(repoRoot(t), "scripts", "bootstrap-state")

	t.Run("valid_dry_run_enforces_0600", func(t *testing.T) {
		box := newDeploySandboxRoot(t)
		cloudDir := filepath.Join(box.stateDir, "scw")
		if err := os.MkdirAll(cloudDir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", cloudDir, err)
		}
		defaultEnv := filepath.Join(cloudDir, "state-backend.env")
		if err := os.WriteFile(defaultEnv, []byte("# pre-existing\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", defaultEnv, err)
		}
		if err := os.Chmod(defaultEnv, 0o644); err != nil {
			t.Fatalf("chmod %s: %v", defaultEnv, err)
		}

		cmd := exec.Command("bash", "-c", `umask 022; exec "$@"`, "umask-wrap",
			"bash", bootstrapScript, "scw",
			"--project", "project-1",
			"--region", "fr-par",
			"--bucket", "kodama-state-scw-test",
			"--dry-run",
		)
		cmd.Env = box.env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bootstrap-state --dry-run failed: %v\n%s", err, output)
		}

		scopedEnv := filepath.Join(cloudDir, "state-backend-project-1-fr-par.env")
		for _, envPath := range []string{defaultEnv, scopedEnv} {
			info, err := os.Stat(envPath)
			if err != nil {
				t.Fatalf("stat %s: %v", envPath, err)
			}
			if mode := info.Mode().Perm(); mode != 0o600 {
				t.Errorf("%s mode = %o, want 0600", envPath, mode)
			}
		}
	})

	invalidCases := []struct {
		name string
		args []string
	}{
		{name: "cloud", args: []string{"azure", "--project", "project-1", "--dry-run"}},
		{name: "bucket_command_sub", args: []string{"scw", "--project", "project-1", "--bucket", "bad$(id)bucket", "--dry-run"}},
		{name: "bucket_double_dot", args: []string{"scw", "--project", "project-1", "--bucket", "bad..bucket", "--dry-run"}},
		{name: "region_traversal", args: []string{"scw", "--project", "project-1", "--region", "../evil", "--dry-run"}},
		{name: "region_quotes", args: []string{"scw", "--project", "project-1", "--region", `fr-par";id;#`, "--dry-run"}},
		{name: "project_newline", args: []string{"scw", "--project", "project-1\nEVIL=1", "--dry-run"}},
		{name: "project_spaces", args: []string{"scw", "--project", "project 1", "--dry-run"}},
	}
	for _, tc := range invalidCases {
		t.Run("refuses_"+tc.name, func(t *testing.T) {
			box := newDeploySandboxRoot(t)
			cmd := exec.Command("bash", append([]string{bootstrapScript}, tc.args...)...)
			cmd.Env = box.env
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("bootstrap-state accepted %s:\n%s", tc.name, output)
			}
			defaultEnv := filepath.Join(box.stateDir, "scw", "state-backend.env")
			if _, statErr := os.Stat(defaultEnv); !os.IsNotExist(statErr) {
				t.Errorf("bootstrap-state wrote %s despite invalid %s", defaultEnv, tc.name)
			}
		})
	}
}

// TestLoadStateBackendEnvSafeParser proves scripts/lib/deploy.sh parses
// state-backend.env without sourcing shell code and allowlists only safe
// KODAMA_STATE_* keys.
func TestLoadStateBackendEnvSafeParser(t *testing.T) {
	t.Run("loads_valid_state_env", func(t *testing.T) {
		box := newDeploySandbox(t)
		envPath := filepath.Join(box.stateDir, "scw", "state-backend.env")
		writeFile(t, envPath, strings.Join([]string{
			"# comment",
			`KODAMA_STATE_CLOUD="scw"`,
			`KODAMA_STATE_BUCKET="kodama-custom-state-bucket"`,
			`KODAMA_STATE_REGION="nl-ams"`,
			`KODAMA_STATE_PROJECT_OR_ACCOUNT="proj-from-env"`,
		}, "\n")+"\n")

		code, output := box.run(t, "backend-config", "--cluster", "stage")
		if code != 0 {
			t.Fatalf("backend-config with valid state-backend.env failed: exit %d\n%s", code, output)
		}
		if !strings.Contains(output, `bucket                      = "kodama-custom-state-bucket"`) {
			t.Errorf("backend-config did not honor KODAMA_STATE_BUCKET:\n%s", output)
		}
	})

	canaryCases := []struct {
		name string
		body string
	}{
		{
			name: "command_substitution_in_value",
			body: "KODAMA_STATE_BUCKET=\"$(touch CANARY_PATH)\"\n",
		},
		{
			name: "disallowed_key",
			body: "KODAMA_STATE_CLOUD=\"scw\"\nPATH=\"/tmp/evil\"\n",
		},
		{
			name: "bare_shell_command",
			body: "touch CANARY_PATH\n",
		},
		{
			name: "cloud_mismatch",
			body: "KODAMA_STATE_CLOUD=\"aws\"\n",
		},
	}
	for _, tc := range canaryCases {
		t.Run("refuses_"+tc.name, func(t *testing.T) {
			box := newDeploySandbox(t)
			canary := filepath.Join(box.tempDir, "pwned-"+tc.name)
			envPath := filepath.Join(box.stateDir, "scw", "state-backend.env")
			writeFile(t, envPath, strings.ReplaceAll(tc.body, "CANARY_PATH", canary))

			code, output := box.run(t, "status", "--cluster", "stage")
			if code == 0 {
				t.Fatalf("scripts/deploy accepted unsafe state-backend.env (%s):\n%s", tc.name, output)
			}
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatalf("state-backend.env executed shell code (%s): canary %s exists", tc.name, canary)
			}
		})
	}
}

// TestDeployLiveHelmRequiresValidDigestAndOIDC proves live Helm installs
// require a valid sha256 image digest and production OIDC configuration (via
// env vars or KODAMA_HELM_EXTRA_VALUES), and render-values validates
// KODAMA_IMAGE_DIGEST and renders config.extraEnv when OIDC env vars are set.
func TestDeployLiveHelmRequiresValidDigestAndOIDC(t *testing.T) {
	t.Run("render_values_rejects_bad_digest_and_renders_oidc_extra_env", func(t *testing.T) {
		badBox := newDeploySandbox(t).withEnv("KODAMA_IMAGE_DIGEST=sha256:not-hex")
		if code, output := badBox.run(t, "render-values", "--cluster", "stage"); code == 0 {
			t.Fatalf("render-values accepted invalid KODAMA_IMAGE_DIGEST:\n%s", output)
		}

		oidcBox := newDeploySandbox(t).withEnv(
			"KODAMA_OIDC_ISSUER=https://idp.example",
			"KODAMA_OIDC_AUDIENCE=kodama-api",
			"KODAMA_OIDC_TENANT_CLAIM=org_id",
			"KODAMA_OIDC_SUBJECT_CLAIM=sub",
		)
		if code, output := oidcBox.run(t, "render-values", "--cluster", "stage"); code != 0 {
			t.Fatalf("render-values with OIDC env failed: exit %d\n%s", code, output)
		}
		raw, err := os.ReadFile(filepath.Join(oidcBox.workDir, "values.generated.yaml"))
		if err != nil {
			t.Fatalf("read rendered values: %v", err)
		}
		doc := yamlMap(t, string(raw))
		cfg := mapAt(t, mapAt(t, doc, "kodama"), "config")
		if _, present := cfg["optionalCapabilities"]; present {
			t.Errorf("rendered values still carry optionalCapabilities")
		}
		extraEnv := mapAt(t, cfg, "extraEnv")
		if extraEnv["KODAMA_OIDC_ISSUER"] != "https://idp.example" ||
			extraEnv["KODAMA_OIDC_AUDIENCE"] != "kodama-api" ||
			extraEnv["KODAMA_OIDC_TENANT_CLAIM"] != "org_id" ||
			extraEnv["KODAMA_OIDC_SUBJECT_CLAIM"] != "sub" {
			t.Errorf("unexpected rendered config.extraEnv: %v", extraEnv)
		}
	})

	t.Run("live_helm_refuses_missing_digest", func(t *testing.T) {
		box := newDeploySandbox(t).withEnv(
			"HELM_ALLOW_UPGRADE=1",
			"KODAMA_IMAGE_REPOSITORY=ghcr.io/example/kodama",
			"KODAMA_IMAGE_DIGEST=",
			"KODAMA_OIDC_ISSUER=https://idp.example",
			"KODAMA_OIDC_AUDIENCE=kodama-api",
			"KODAMA_OIDC_TENANT_CLAIM=org_id",
		)
		code, output := box.run(t, "helm", "--cluster", "stage")
		if code == 0 {
			t.Fatalf("live helm succeeded without KODAMA_IMAGE_DIGEST:\n%s", output)
		}
		if strings.Contains(box.readLog(t, box.helmLog), "\tupgrade\t") {
			t.Errorf("helm upgrade ran despite missing KODAMA_IMAGE_DIGEST")
		}
	})

	t.Run("live_helm_refuses_missing_oidc", func(t *testing.T) {
		box := newDeploySandbox(t).withEnv(
			"HELM_ALLOW_UPGRADE=1",
			"KODAMA_IMAGE_REPOSITORY=ghcr.io/example/kodama",
		)
		code, output := box.run(t, "helm", "--cluster", "stage")
		if code == 0 {
			t.Fatalf("live helm succeeded without production OIDC config:\n%s", output)
		}
		if strings.Contains(box.readLog(t, box.helmLog), "\tupgrade\t") {
			t.Errorf("helm upgrade ran despite missing production OIDC config")
		}
	})

	t.Run("live_helm_accepts_env_oidc", func(t *testing.T) {
		box := newDeploySandbox(t).withEnv(
			"HELM_ALLOW_UPGRADE=1",
			"KODAMA_IMAGE_REPOSITORY=ghcr.io/example/kodama",
			"KODAMA_OIDC_ISSUER=https://idp.example",
			"KODAMA_OIDC_AUDIENCE=kodama-api",
			"KODAMA_OIDC_JWKS_URL=https://idp.example/jwks",
			"KODAMA_OIDC_TENANT_CLAIM=org_id",
		)
		code, output := box.run(t, "helm", "--cluster", "stage")
		if code != 0 {
			t.Fatalf("live helm with OIDC env failed: exit %d\n%s", code, output)
		}
		if !strings.Contains(box.readLog(t, box.helmLog), "\tupgrade\t") {
			t.Errorf("live helm did not invoke helm upgrade:\n%s", box.readLog(t, box.helmLog))
		}
	})

	t.Run("live_helm_accepts_extra_values_oidc", func(t *testing.T) {
		box := newDeploySandbox(t)
		extraValues := filepath.Join(box.tempDir, "oidc-values.yaml")
		writeFile(t, extraValues, strings.Join([]string{
			"kodama:",
			"  config:",
			"    extraEnv:",
			"      KODAMA_OIDC_ISSUER: https://idp.example",
			"      KODAMA_OIDC_AUDIENCE: kodama-api",
			"      KODAMA_OIDC_JWKS_URL: https://idp.example/jwks",
			"      KODAMA_OIDC_TENANT_CLAIM: org_id",
		}, "\n")+"\n")
		box.withEnv(
			"HELM_ALLOW_UPGRADE=1",
			"KODAMA_IMAGE_REPOSITORY=ghcr.io/example/kodama",
			"KODAMA_HELM_EXTRA_VALUES="+extraValues,
		)
		code, output := box.run(t, "helm", "--cluster", "stage")
		if code != 0 {
			t.Fatalf("live helm with KODAMA_HELM_EXTRA_VALUES failed: exit %d\n%s", code, output)
		}
		if !strings.Contains(box.readLog(t, box.helmLog), "\tupgrade\t") {
			t.Errorf("live helm did not invoke helm upgrade:\n%s", box.readLog(t, box.helmLog))
		}
	})
}
