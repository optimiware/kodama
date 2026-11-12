//go:build component

// Hermetic tests for scripts/stage-reset, the guarded reset driver
// for the Scaleway stage. The fake kubectl/helm in a temporary PATH record
// every call; no cluster, network or pod is touched.
package deploy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeStageResetKubectl answers the read-only guards, the one-shot pod
// lifecycle and the bounded waits. Mutations are recorded so a --print-plan
// test can prove none happened.
const fakeStageResetKubectl = `#!/usr/bin/env bash
set -u
line="kubectl"
for arg in "$@"; do line="${line}"$'\t'"${arg}"; done
printf '%s\n' "$line" >> "$KUBECTL_ARGV_LOG"

args=" $* "
if [[ "$args" == *" -f - "* ]]; then
  cat >> "$KUBECTL_STDIN_LOG"
fi
case "$args" in
  *" config current-context "*)
    printf '%s\n' "${KUBECTL_CONTEXT:-admin@kodama-stage-k8s}"
    exit 0
    ;;
  *" get secret "*)
    for missing in ${KUBECTL_MISSING_SECRETS:-}; do
      if [[ "$args" == *" $missing "* ]]; then
        echo "Error from server (NotFound): secrets \"$missing\" not found" >&2
        exit 1
      fi
    done
    exit 0
    ;;
  *" get deployment/kodama -o go-template="*)
    printf '%s\n' "${KUBECTL_SELECTOR:-app.kubernetes.io/name=kodama,}"
    exit 0
    ;;
  *" get deployment/kodama "*|*" get deployment/kodama"*)
    printf '%s\n' "${KUBECTL_REPLICAS:-0}"
    exit 0
    ;;
  *" get pods -l "*)
    count=0
    if [[ -f "$KUBECTL_PODS_COUNTER" ]]; then
      count="$(cat "$KUBECTL_PODS_COUNTER")"
    fi
    count=$((count + 1))
    printf '%s\n' "$count" >"$KUBECTL_PODS_COUNTER"
    if (( count <= ${KUBECTL_PODS_REMAIN_FOR:-0} )); then
      printf 'pod/kodama-%s 1/1 Running 0 1m\n' "$count"
    fi
    exit 0
    ;;
  *" get pod/"*)
    if [[ "${KUBECTL_HANG:-0}" == "1" ]]; then
      sleep 30
    fi
    printf '%s\n' "${KUBECTL_POD_PHASE:-Succeeded}"
    exit 0
    ;;
  *" logs pod/"*)
    printf '%s\n' "${KUBECTL_LOGS:-stage reset pod log}"
    exit 0
    ;;
  *" create -o name -f - "*)
    count=0
    if [[ -f "$KUBECTL_POD_COUNTER" ]]; then
      count="$(cat "$KUBECTL_POD_COUNTER")"
    fi
    count=$((count + 1))
    printf '%s\n' "$count" >"$KUBECTL_POD_COUNTER"
    printf 'pod/kodama-stage-reset-%s\n' "$count"
    exit 0
    ;;
  *" delete pod/"*)
    exit 0
    ;;
  *" scale deployment/kodama "*)
    exit 0
    ;;
  *" delete namespace "*|*" delete pvc/"*)
    exit 0
    ;;
  *" get namespace "*|*" get pvc/"*|*" get deployments,statefulsets "*)
    printf '%s\n' "${KUBECTL_GET_OUTPUT:-present}"
    exit 0
    ;;
esac
exit 0
`

// fakeStageResetHelm answers the release-existence guard and records argv.
const fakeStageResetHelm = `#!/usr/bin/env bash
set -u
{
  printf 'helm'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$HELM_ARGV_LOG"
if [[ "${HELM_STATUS_EXIT:-0}" != "0" ]]; then
  echo 'Error: release: not found' >&2
  exit "${HELM_STATUS_EXIT}"
fi
case " $* " in
  *" status "*) printf '# fake helm status\n' ; exit 0 ;;
esac
exit 0
`

type resetSandbox struct {
	root       string
	bin        string
	evidence   string
	kubeconfig string
	kubectlLog string
	kubectlIn  string
	counter    string
	podsCount  string
	helmLog    string
	script     string
	env        []string
	extraEnv   []string
}

func newResetSandbox(t *testing.T) *resetSandbox {
	t.Helper()
	root := t.TempDir()
	box := &resetSandbox{
		root:       root,
		bin:        filepath.Join(root, "bin"),
		evidence:   filepath.Join(root, "evidence"),
		kubeconfig: filepath.Join(root, "kubeconfig.yaml"),
		kubectlLog: filepath.Join(root, "kubectl.argv"),
		kubectlIn:  filepath.Join(root, "kubectl.stdin"),
		counter:    filepath.Join(root, "kubectl.pods"),
		podsCount:  filepath.Join(root, "kubectl.podlist.count"),
		helmLog:    filepath.Join(root, "helm.argv"),
		script:     filepath.Join(repoRoot(t), "scripts", "stage-reset"),
	}
	if err := os.MkdirAll(box.bin, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeFile(t, box.kubeconfig, "# fake kubeconfig\n")
	if err := os.Chmod(box.kubeconfig, 0o600); err != nil {
		t.Fatalf("chmod kubeconfig: %v", err)
	}
	writeExecutable(t, filepath.Join(box.bin, "kubectl"), fakeStageResetKubectl)
	writeExecutable(t, filepath.Join(box.bin, "helm"), fakeStageResetHelm)

	box.env = append(os.Environ(),
		"PATH="+box.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG="+box.kubeconfig,
		"KODAMA_STAGE_CONTEXT=admin@kodama-stage-k8s",
		"KODAMA_STAGE_NAMESPACE=kodama-system",
		"KODAMA_STAGE_TENANT_NAMESPACE=kodama-tenant-tenant-a",
		"KODAMA_STAGE_EVIDENCE_DIR="+box.evidence,
		"KODAMA_STAGE_RESET_CONFIRM=reset-kodama-stage",
		"KUBECTL_ARGV_LOG="+box.kubectlLog,
		"KUBECTL_STDIN_LOG="+box.kubectlIn,
		"KUBECTL_POD_COUNTER="+box.counter,
		"KUBECTL_PODS_COUNTER="+box.podsCount,
		"HELM_ARGV_LOG="+box.helmLog,
	)
	return box
}

func (b *resetSandbox) withEnv(entries ...string) *resetSandbox {
	b.extraEnv = append(b.extraEnv, entries...)
	return b
}

func (b *resetSandbox) command(args ...string) *exec.Cmd {
	command := exec.Command("bash", append([]string{b.script}, args...)...)
	command.Env = append(append([]string(nil), b.env...), b.extraEnv...)
	return command
}

func (b *resetSandbox) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	output, err := b.command(args...).CombinedOutput()
	code := 0
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run scripts/stage-reset: %v\n%s", err, output)
		}
		code = exitError.ExitCode()
	}
	return code, string(output)
}

func readArgvLog(t *testing.T, label, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s log: %v", label, err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func (b *resetSandbox) kubectlCalls(t *testing.T) []string {
	t.Helper()
	return readArgvLog(t, "kubectl", b.kubectlLog)
}

func (b *resetSandbox) helmCalls(t *testing.T) []string {
	t.Helper()
	return readArgvLog(t, "helm", b.helmLog)
}

// stripScopeFlags removes kubeconfig, context and namespace flags from a
// tab-split argv slice, failing closed if a flag is missing its argument.
func stripScopeFlags(args []string, valueFlags map[string]bool) ([]string, bool) {
	rest := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if valueFlags[arg] {
			if index+1 >= len(args) || args[index+1] == "" {
				return nil, false
			}
			index++
			continue
		}
		matchedPrefix := false
		for flag := range valueFlags {
			prefix := flag + "="
			if strings.HasPrefix(arg, prefix) {
				if len(arg) == len(prefix) {
					return nil, false
				}
				matchedPrefix = true
				break
			}
		}
		if matchedPrefix {
			continue
		}
		rest = append(rest, arg)
	}
	return rest, true
}

// isReadOnlyResetCall enforces a strict read-only allowlist for pre-mutation
// guards and --print-plan: only `kubectl config current-context`,
// `kubectl get ...` and `helm status ...` are permitted after stripping
// kubeconfig/context/namespace scope flags.
func isReadOnlyResetCall(line string) bool {
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return false
	}
	switch fields[0] {
	case "kubectl":
		rest, ok := stripScopeFlags(fields[1:], map[string]bool{
			"--kubeconfig": true,
			"--context":    true,
			"-n":           true,
			"--namespace":  true,
		})
		if !ok || len(rest) < 2 {
			return false
		}
		if rest[0] == "config" {
			return len(rest) == 2 && rest[1] == "current-context"
		}
		if rest[0] == "get" {
			for _, token := range rest[1:] {
				switch {
				case token == "-f", token == "--filename",
					strings.HasPrefix(token, "-f="), strings.HasPrefix(token, "--filename="),
					token == "-w", token == "--watch", token == "--watch-only",
					strings.HasPrefix(token, "--watch="), strings.HasPrefix(token, "--watch-only="):
					return false
				}
			}
			return true
		}
		return false
	case "helm":
		rest, ok := stripScopeFlags(fields[1:], map[string]bool{
			"--kubeconfig":   true,
			"--kube-context": true,
			"-n":             true,
			"--namespace":    true,
		})
		if !ok || len(rest) < 2 || rest[0] != "status" || rest[1] == "" || strings.HasPrefix(rest[1], "-") {
			return false
		}
		return len(rest) == 2
	default:
		return false
	}
}

// mutationCall returns the first recorded kubectl or helm invocation that is
// outside the strict read-only allowlist, or "" when every call is read-only.
func mutationCall(lines []string) string {
	for _, line := range lines {
		if !isReadOnlyResetCall(line) {
			return line
		}
	}
	return ""
}

func (b *resetSandbox) assertReadOnlyCallsOnly(t *testing.T, label string) {
	t.Helper()
	if line := mutationCall(b.kubectlCalls(t)); line != "" {
		t.Errorf("%s executed non-allowlisted kubectl call:\n%s", label, line)
	}
	if line := mutationCall(b.helmCalls(t)); line != "" {
		t.Errorf("%s executed non-allowlisted helm call:\n%s", label, line)
	}
	if _, err := os.Stat(b.kubectlIn); !os.IsNotExist(err) {
		t.Errorf("%s wrote stdin to kubectl (%s)", label, b.kubectlIn)
	}
}

var resetRoleLine = regexp.MustCompile(`(?m)^DROP ROLE IF EXISTS (kodama_[a-z]+);$`)

// TestStageResetRefusesUnsafeTargets proves the static and read-only guards
// stop every wrong stage identity, confirmation or target before a mutation.
func TestStageResetRefusesUnsafeTargets(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, box *resetSandbox)
		want   string
	}{
		{
			name: "kubeconfig mode",
			mutate: func(t *testing.T, box *resetSandbox) {
				if err := os.Chmod(box.kubeconfig, 0o644); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
			want: "KUBECONFIG",
		},
		{
			name:   "wrong configured context",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KODAMA_STAGE_CONTEXT=admin@other") },
			want:   "KODAMA_STAGE_CONTEXT",
		},
		{
			name:   "current context mismatch",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KUBECTL_CONTEXT=admin@other") },
			want:   "current context",
		},
		{
			name:   "wrong namespace",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KODAMA_STAGE_NAMESPACE=kodama-other") },
			want:   "KODAMA_STAGE_NAMESPACE",
		},
		{
			name: "wrong tenant namespace",
			mutate: func(t *testing.T, box *resetSandbox) {
				box.withEnv("KODAMA_STAGE_TENANT_NAMESPACE=kodama-tenant-tenant-b")
			},
			want: "KODAMA_STAGE_TENANT_NAMESPACE",
		},
		{
			name: "prefix tenant namespace",
			mutate: func(t *testing.T, box *resetSandbox) {
				box.withEnv("KODAMA_STAGE_TENANT_NAMESPACE=kodama-tenant-tenant-a-extra")
			},
			want: "KODAMA_STAGE_TENANT_NAMESPACE",
		},
		{
			name:   "wildcard tenant namespace",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KODAMA_STAGE_TENANT_NAMESPACE=kodama-tenant-*") },
			want:   "KODAMA_STAGE_TENANT_NAMESPACE",
		},
		{
			name:   "missing confirmation",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KODAMA_STAGE_RESET_CONFIRM=") },
			want:   "KODAMA_STAGE_RESET_CONFIRM",
		},
		{
			name:   "wrong confirmation",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KODAMA_STAGE_RESET_CONFIRM=reset-kodama-stag") },
			want:   "KODAMA_STAGE_RESET_CONFIRM",
		},
		{
			name:   "relative evidence directory",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KODAMA_STAGE_EVIDENCE_DIR=relative/evidence") },
			want:   "absolute",
		},
		{
			name:   "missing Secret",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KUBECTL_MISSING_SECRETS=kodama-broker") },
			want:   "Secret",
		},
		{
			name:   "missing NATS auth Secret",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("KUBECTL_MISSING_SECRETS=kodama-nats-auth") },
			want:   "Secret",
		},
		{
			name:   "missing Helm release",
			mutate: func(t *testing.T, box *resetSandbox) { box.withEnv("HELM_STATUS_EXIT=1") },
			want:   "Helm release",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			box := newResetSandbox(t)
			test.mutate(t, box)
			code, output := box.run(t)
			if code == 0 {
				t.Fatalf("stage-reset accepted an unsafe target:\n%s", output)
			}
			if !strings.Contains(output, test.want) {
				t.Errorf("refusal does not mention %q:\n%s", test.want, output)
			}
			box.assertReadOnlyCallsOnly(t, "refused stage-reset")
			if _, err := os.Stat(box.evidence); !os.IsNotExist(err) {
				t.Errorf("a refused run created the evidence directory %s", box.evidence)
			}
		})
	}
}

// TestStageResetPrintPlanIsExactAndMutationFree proves --print-plan prints the
// exact SQL and every target without touching the stage.
func TestStageResetPrintPlanIsExactAndMutationFree(t *testing.T) {
	box := newResetSandbox(t)
	// The plan mode is the dry run: it must not require the mutation
	// confirmation variable.
	box.withEnv("KODAMA_STAGE_RESET_CONFIRM=")
	code, output := box.run(t, "--print-plan")
	if code != 0 {
		t.Fatalf("stage-reset --print-plan failed: exit %d\n%s", code, output)
	}
	box.assertReadOnlyCallsOnly(t, "--print-plan")
	if len(box.kubectlCalls(t)) == 0 {
		t.Errorf("--print-plan recorded no read-only kubectl guard calls")
	}
	if len(box.helmCalls(t)) == 0 {
		t.Errorf("--print-plan recorded no read-only helm status guard calls")
	}
	if _, err := os.Stat(box.evidence); !os.IsNotExist(err) {
		t.Errorf("--print-plan created the evidence directory")
	}

	for _, wanted := range []string{
		`GRANT kodama_owner TO :"admin" WITH INHERIT TRUE;`,
		`GRANT kodama_owner TO :"admin" WITH INHERIT FALSE;`,
		`ALTER DATABASE :"db" OWNER TO :"admin";`,
		`SELECT current_database() AS db, current_user AS admin \gset`,
		`DROP SCHEMA IF EXISTS public CASCADE;`,
		`DROP SCHEMA IF EXISTS kodama_meta CASCADE;`,
		`CREATE SCHEMA public AUTHORIZATION pg_database_owner;`,
		`GRANT USAGE ON SCHEMA public TO :"admin" WITH GRANT OPTION;`,
		"KODAMA_EVENTS",
		"KODAMA_INVALIDATIONS",
		"kodama-tenant-tenant-a",
		"kodama-retained-logs",
		"deployment/kodama",
	} {
		if !strings.Contains(output, wanted) {
			t.Errorf("--print-plan is missing %q:\n%s", wanted, output)
		}
	}
	for _, forbidden := range []string{
		"DROP OWNED",
		"DROP DATABASE",
		"kodama_%",
		"DROP ROLE IF EXISTS kodama_admin",
	} {
		if strings.Contains(output, forbidden) {
			t.Errorf("--print-plan contains the unsafe statement %q:\n%s", forbidden, output)
		}
	}

	roles := resetRoleLine.FindAllStringSubmatch(output, -1)
	if len(roles) != 5 {
		t.Fatalf("--print-plan drops %d roles; want the five migration roles:\n%s", len(roles), output)
	}
	want := map[string]bool{
		"kodama_owner": true, "kodama_runtime": true, "kodama_controller": true,
		"kodama_publisher": true, "kodama_auth": true,
	}
	for _, match := range roles {
		if !want[match[1]] {
			t.Errorf("--print-plan drops unexpected role %q", match[1])
		}
		delete(want, match[1])
	}
	if len(want) != 0 {
		t.Errorf("--print-plan is missing roles %v", want)
	}
}

// TestStageResetApplyIsPrivateAndCleansUp proves the apply mode scales the
// deployment, uses one-shot pods with Secret references and CA mounts, keeps
// pod logs in 0600 evidence files rather than stdout, and removes every pod.
func TestStageResetApplyIsPrivateAndCleansUp(t *testing.T) {
	box := newResetSandbox(t)
	const sentinel = "P28-SENTINEL-STAGE-RESET-LOG"
	// Two pod-list polls still see a pod; the third sees none. This proves the
	// quiesce waits on the deployment selector's pods, not on .status.replicas.
	box.withEnv("KUBECTL_LOGS="+sentinel, "KUBECTL_PODS_REMAIN_FOR=2")
	code, output := box.run(t)
	if code != 0 {
		t.Fatalf("stage-reset apply failed: exit %d\n%s", code, output)
	}
	if strings.Contains(output, sentinel) {
		t.Errorf("stage-reset echoed private pod logs to stdout:\n%s", output)
	}

	manifests, err := os.ReadFile(box.kubectlIn)
	if err != nil {
		t.Fatalf("read pod manifests: %v", err)
	}
	text := string(manifests)
	for _, wanted := range []string{
		"secretKeyRef: {name: kodama-database-admin, key: admin_url}",
		"secretName: kodama-database",
		"key: ca.crt",
		"secretName: kodama-broker",
		"docker.io/library/postgres@sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac",
		"docker.io/natsio/nats-box@sha256:9d5f35d286c3dcfca18bb2339b51345f9f89b580b237ab16ddfe609bdca9c72d",
		"tls://nats.kodama-system.svc.cluster.local:4222",
		// The historical executions are read as the migration owner with the
		// stage tenant pinned; the setup administrator cannot pass FORCE RLS.
		"SET ROLE kodama_owner;",
		"SET kodama.tenant_id = 'tenant-a';",
		"SET kodama.project_id = '';",
	} {
		if !strings.Contains(text, wanted) {
			t.Errorf("one-shot pod manifests are missing %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "postgres://") {
		t.Errorf("a pod manifest carries a literal database URL:\n%s", text)
	}

	lines := box.kubectlCalls(t)
	var creates, deletes int
	for _, line := range lines {
		if strings.Contains(line, "\tcreate\t-o\tname\t-f\t-") {
			creates++
		}
		if strings.Contains(line, "\tdelete\tpod/") {
			deletes++
		}
	}
	if creates == 0 {
		t.Fatalf("no one-shot pods were created:\n%s", strings.Join(lines, "\n"))
	}
	if deletes < creates {
		t.Errorf("created %d one-shot pods but deleted %d", creates, deletes)
	}
	if info, err := os.Stat(box.evidence); err != nil {
		t.Fatalf("stat evidence dir: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("evidence dir mode = %04o; want 0700", info.Mode().Perm())
	}
	for _, name := range []string{"pre-state.log", "boundary.txt", "summary.txt"} {
		path := filepath.Join(box.evidence, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("evidence file %s: %v", name, err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("evidence file %s mode = %04o; want 0600", name, info.Mode().Perm())
		}
	}
	// The quiesce must poll the deployment's owned pods, and only the third
	// poll (after KUBECTL_PODS_REMAIN_FOR=2) sees none.
	countBody, err := os.ReadFile(box.podsCount)
	if err != nil {
		t.Fatalf("read pod-list counter: %v", err)
	}
	if got := strings.TrimSpace(string(countBody)); got != "3" {
		t.Errorf("pod-list polls = %q; want 3 (two with a remaining pod, one empty)", got)
	}
	if indexOfLine(lines, "get\tpods\t-l\tapp.kubernetes.io/name=kodama") < 0 {
		t.Errorf("quiesce never listed pods by the deployment selector:\n%s", strings.Join(lines, "\n"))
	}
	quiesce, err := os.ReadFile(filepath.Join(box.evidence, "quiesce.log"))
	if err != nil {
		t.Fatalf("read quiesce log: %v", err)
	}
	if !strings.Contains(string(quiesce), "selector app.kubernetes.io/name=kodama") {
		t.Errorf("quiesce log does not record the selector:\n%s", quiesce)
	}
	// The summary is the one non-secret record printed to the operator.
	if !strings.Contains(output, "reset complete") && !strings.Contains(output, "reset_start_utc") {
		t.Errorf("stage-reset printed no summary:\n%s", output)
	}
}

// TestStageResetDeletesPodsOnSIGTERM proves the EXIT/TERM trap owns the pod
// records: run_pod must not run in a pipeline subshell, or a signal leaves the
// one-shot pod behind.
func TestStageResetDeletesPodsOnSIGTERM(t *testing.T) {
	box := newResetSandbox(t).withEnv("KUBECTL_HANG=1")
	command := box.command()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start stage-reset: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if indexOfLine(box.kubectlCalls(t), "get\tpod/") >= 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
			t.Fatalf("stage-reset never reached the pod wait; output:\n%s", output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to the process group: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatalf("stage-reset exited 0 after SIGTERM; output:\n%s", output.String())
	}
	if indexOfLine(box.kubectlCalls(t), "delete\tpod/") < 0 {
		t.Errorf("SIGTERM left the one-shot pod behind:\n%s", strings.Join(box.kubectlCalls(t), "\n"))
	}
}

// resetPodDocuments splits the concatenated one-shot pod manifests written to
// kubectl's stdin into one document per created pod. The manifests carry no
// "---" separator, so every top-level apiVersion line starts a new pod.
func resetPodDocuments(text string) []string {
	header := regexp.MustCompile(`(?m)^apiVersion: v1$`)
	var starts []int
	for _, match := range header.FindAllStringIndex(text, -1) {
		starts = append(starts, match[0])
	}
	documents := make([]string, 0, len(starts))
	for index, start := range starts {
		end := len(text)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		if document := strings.TrimSpace(text[start:end]); document != "" {
			documents = append(documents, document)
		}
	}
	return documents
}

// TestStageResetNATSBoxPodsHaveWritableHome proves both nats-box one-shot pods
// keep the hardened root filesystem but give the nats CLI a writable HOME and
// working directory. Without them the CLI cannot load its JSON schemas and the
// stream reads fail with "stat .: permission denied". The subsetting image,
// TLS mounts, commands and read-only root filesystem must stay unchanged.
func TestStageResetNATSBoxPodsHaveWritableHome(t *testing.T) {
	const natsImage = "docker.io/natsio/nats-box@sha256:9d5f35d286c3dcfca18bb2339b51345f9f89b580b237ab16ddfe609bdca9c72d"
	box := newResetSandbox(t)
	code, output := box.run(t)
	if code != 0 {
		t.Fatalf("stage-reset apply failed: exit %d\n%s", code, output)
	}
	body, err := os.ReadFile(box.kubectlIn)
	if err != nil {
		t.Fatalf("read pod manifests: %v", err)
	}

	var natsPods []map[string]any
	for _, document := range resetPodDocuments(string(body)) {
		pod := yamlMap(t, document)
		spec := mapAt(t, pod, "spec")
		containers := listAt(t, spec, "containers")
		if len(containers) != 1 {
			t.Fatalf("one-shot pod has %d containers; want exactly one:\n%s", len(containers), document)
		}
		container, ok := containers[0].(map[string]any)
		if !ok {
			t.Fatalf("pod container is %T, want a map", containers[0])
		}
		image, _ := container["image"].(string)
		if strings.Contains(image, "nats-box") {
			natsPods = append(natsPods, pod)
		}
	}
	if len(natsPods) != 2 {
		t.Fatalf("stage-reset created %d nats-box pods; want the pre-state and reset pods", len(natsPods))
	}

	for index, pod := range natsPods {
		name, _ := mapAt(t, pod, "metadata")["generateName"].(string)
		spec := mapAt(t, pod, "spec")
		// workingDir is a Container field. On the PodSpec the API server would
		// reject or silently drop it, leaving the image's default cwd.
		if got, present := spec["workingDir"]; present {
			t.Errorf("nats pod %d (%s) sets workingDir on the pod spec (%v); want it on the container", index, name, got)
		}

		containers := listAt(t, spec, "containers")
		container, ok := containers[0].(map[string]any)
		if !ok {
			t.Fatalf("nats pod %d container is %T; want a map", index, containers[0])
		}
		if got := container["workingDir"]; got != "/tmp" {
			t.Errorf("nats pod %d (%s) container workingDir = %v; want /tmp", index, name, got)
		}
		image, _ := container["image"].(string)
		if image != natsImage {
			t.Errorf("nats pod %d image = %q; want the pinned digest", index, image)
		}

		homeFound := false
		natsUserRef := false
		natsPasswordRef := false
		for _, rawEnv := range listAt(t, container, "env") {
			env, ok := rawEnv.(map[string]any)
			if !ok {
				t.Fatalf("nats pod %d env entry is %T; want a map", index, rawEnv)
			}
			switch env["name"] {
			case "HOME":
				homeFound = true
				if env["value"] != "/tmp" {
					t.Errorf("nats pod %d HOME = %v; want /tmp", index, env["value"])
				}
				for _, forbidden := range []string{"valueFrom", "secretKeyRef"} {
					if _, present := env[forbidden]; present {
						t.Errorf("nats pod %d HOME reads %q; want the literal /tmp", index, forbidden)
					}
				}
			case "NATS_USER":
				valueFrom := mapAt(t, env, "valueFrom")
				ref := mapAt(t, valueFrom, "secretKeyRef")
				if ref["name"] == "kodama-nats-auth" && ref["key"] == "shell-user" {
					natsUserRef = true
				}
			case "NATS_PASSWORD":
				valueFrom := mapAt(t, env, "valueFrom")
				ref := mapAt(t, valueFrom, "secretKeyRef")
				if ref["name"] == "kodama-nats-auth" && ref["key"] == "shell-password" {
					natsPasswordRef = true
				}
			}
		}
		if !homeFound {
			t.Errorf("nats pod %d sets no HOME=/tmp", index)
		}
		if !natsUserRef || !natsPasswordRef {
			t.Errorf("nats pod %d missing kodama-nats-auth shell bypass secretKeyRef (user=%v password=%v)", index, natsUserRef, natsPasswordRef)
		}

		tmpMounted := false
		for _, rawMount := range listAt(t, container, "volumeMounts") {
			mount, ok := rawMount.(map[string]any)
			if !ok {
				t.Fatalf("nats pod %d volumeMount is %T; want a map", index, rawMount)
			}
			if mount["mountPath"] != "/tmp" {
				continue
			}
			tmpMounted = true
			if mount["name"] != "tmp" {
				t.Errorf("nats pod %d /tmp mount uses volume %v; want tmp", index, mount["name"])
			}
			if readOnly, _ := mount["readOnly"].(bool); readOnly {
				t.Errorf("nats pod %d mounts /tmp read-only; want a writable volume", index)
			}
		}
		if !tmpMounted {
			t.Errorf("nats pod %d mounts no writable /tmp", index)
		}

		tmpVolume := false
		for _, rawVolume := range listAt(t, spec, "volumes") {
			volume, ok := rawVolume.(map[string]any)
			if !ok {
				t.Fatalf("nats pod %d volume is %T; want a map", index, rawVolume)
			}
			if volume["name"] != "tmp" {
				continue
			}
			tmpVolume = true
			if _, present := volume["emptyDir"]; !present {
				t.Errorf("nats pod %d tmp volume is not an emptyDir: %v", index, volume)
			}
			if _, present := volume["hostPath"]; present {
				t.Errorf("nats pod %d tmp volume is a hostPath; want emptyDir", index)
			}
		}
		if !tmpVolume {
			t.Errorf("nats pod %d declares no tmp volume", index)
		}

		// The writable home must not weaken the existing hardening.
		securityContext := mapAt(t, spec, "securityContext")
		if securityContext["runAsNonRoot"] != true {
			t.Errorf("nats pod %d no longer runs as non-root", index)
		}
		if runAsUser, ok := securityContext["runAsUser"].(int); !ok || runAsUser != 65532 {
			t.Errorf("nats pod %d runAsUser = %v; want 65532", index, securityContext["runAsUser"])
		}
		containerContext := mapAt(t, container, "securityContext")
		if containerContext["readOnlyRootFilesystem"] != true {
			t.Errorf("nats pod %d no longer uses a read-only root filesystem", index)
		}
		if containerContext["allowPrivilegeEscalation"] != false {
			t.Errorf("nats pod %d allows privilege escalation", index)
		}
		capabilities, ok := containerContext["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("nats pod %d securityContext.capabilities is %T; want a map", index, containerContext["capabilities"])
		}
		dropped, ok := capabilities["drop"].([]any)
		if !ok || len(dropped) != 1 || dropped[0] != "ALL" {
			t.Errorf("nats pod %d drops %v capabilities; want ALL", index, capabilities["drop"])
		}
	}
}

// TestStageResetReadOnlyAllowlistRejectsNonAllowlistedCalls pins the strict
// allowlist: verbs like patch, replace, exec, rollout, label, config
// use-context and helm upgrade/uninstall/rollback/history are all rejected.
func TestStageResetReadOnlyAllowlistRejectsNonAllowlistedCalls(t *testing.T) {
	allowed := []string{
		"kubectl\t--kubeconfig\t/tmp/kc\tconfig\tcurrent-context",
		"kubectl\t--kubeconfig=/tmp/kc\t--context=admin@kodama-stage-k8s\tconfig\tcurrent-context",
		"kubectl\t--kubeconfig\t/tmp/kc\t--context\tadmin@kodama-stage-k8s\t-n\tkodama-system\tget\tsecret\tkodama-broker",
		"kubectl\t--kubeconfig\t/tmp/kc\t--context\tadmin@kodama-stage-k8s\tget\tnamespace\tkodama-tenant-tenant-a\t-o\tname",
		"helm\t--kubeconfig\t/tmp/kc\t--kube-context\tadmin@kodama-stage-k8s\tstatus\tkodama-stage\t-n\tkodama-system",
		"helm\t--kubeconfig=/tmp/kc\t--kube-context=admin@kodama-stage-k8s\t--namespace=kodama-system\tstatus\tkodama-stage",
	}
	if got := mutationCall(allowed); got != "" {
		t.Fatalf("allowlist rejected legitimate read-only call: %q", got)
	}

	rejected := []string{
		"kubectl\t--kubeconfig\t/tmp/kc\tpatch\tdeployment/kodama\t-p\t{}",
		"kubectl\t--kubeconfig\t/tmp/kc\treplace\t-f\t-",
		"kubectl\t--kubeconfig\t/tmp/kc\texec\tpod/x\t--\tsh",
		"kubectl\t--kubeconfig\t/tmp/kc\trollout\trestart\tdeployment/kodama",
		"kubectl\t--kubeconfig\t/tmp/kc\tlabel\tnamespace\tkodama-system\tx=y",
		"kubectl\t--kubeconfig\t/tmp/kc\tannotate\tnamespace\tkodama-system\tx=y",
		"kubectl\t--kubeconfig\t/tmp/kc\tconfig\tuse-context\tother",
		"kubectl\t--kubeconfig\t/tmp/kc\tconfig\tset-context\tother",
		"kubectl\t--kubeconfig\t/tmp/kc\tget\t-f\t-",
		"kubectl\t--kubeconfig\t/tmp/kc\tget\tpods\t--watch",
		"kubectl\t--kubeconfig",
		"helm\t--kubeconfig\t/tmp/kc\t--kube-context\tadmin@kodama-stage-k8s\tupgrade\tkodama-stage\t.",
		"helm\t--kubeconfig\t/tmp/kc\t--kube-context\tadmin@kodama-stage-k8s\tuninstall\tkodama-stage\t-n\tkodama-system",
		"helm\t--kubeconfig\t/tmp/kc\t--kube-context\tadmin@kodama-stage-k8s\trollback\tkodama-stage\t1",
		"helm\t--kubeconfig\t/tmp/kc\t--kube-context\tadmin@kodama-stage-k8s\thistory\tkodama-stage\t-n\tkodama-system",
		"helm\t--kubeconfig\t/tmp/kc\tstatus\t--show-resources",
		"tofu\tapply",
	}
	for _, call := range rejected {
		if got := mutationCall([]string{call}); got != call {
			t.Errorf("allowlist accepted forbidden call %q (got %q)", call, got)
		}
	}
}
