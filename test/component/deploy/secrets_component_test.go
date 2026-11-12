//go:build component

// Package deploy runs the shell deployment scripts against fake cloud/Kubernetes
// binaries. It is a component test because it starts bash, openssl and the fake
// executables; `make check` must not start processes, so this package carries
// the component build tag and runs only under `go test -tags=component`.
//
// The tests here pin the credential-custody contract of
// scripts/bootstrap-secrets and scripts/lib/scw.sh: a key-encryption key and
// the database passwords are never arguments, plaintext files live in a
// private (0700) temporary directory with 0600 mode, every exit path removes
// the plaintext material, and no script turns on shell xtrace (which would
// print the values). The fake Kubernetes CLI also records the mode of the
// files bootstrap-secrets hands to `kubectl create secret`, so a chmod
// regression on the KEK directory or file dies here.
package deploy

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stubKEKRaw is the deterministic 32-byte value the fake `openssl rand 32`
// prints. The test knows the key material before the script runs, so the argv
// assertion can run before any observation (the fake scw plaintext log) that a
// regression passing the KEK on the command line would make unavailable.
const stubKEKRaw = "0123456789abcdef0123456789abcdef"

// minimumSplitNeedle is the shortest credential fragment treated as a leak
// when a secret is split across two argv elements. Twelve bytes is the floor:
// every credential this file checks is at least 24 bytes long (a full 32-byte
// KEK, its 44-byte base64 form, its 64-byte hex form and the 30+ byte database
// sentinels), so a real split leaves at least one fragment of 12 or more
// bytes. Shorter fragments carry too little information to reconstruct the
// credential and would make ordinary tokens such as "kodama" or "tls://"
// plausible false positives.
const minimumSplitNeedle = 12

// dbSentinels are the four known database passwords injected through the
// documented KODAMA_DB_* environment variables. They must appear in the
// env-file handed to `kubectl create secret` and must not survive the run on
// disk, in either the failing or the succeeding path.
var dbSentinels = []string{
	"P28-SENTINEL-DB-RUNTIME-4c1f9a7e",
	"P28-SENTINEL-DB-CONTROLLER-8e2b5d10",
	"P28-SENTINEL-DB-PUBLISHER-6a7d3f92",
	"P28-SENTINEL-DB-AUTH-1b9e4c65",
}

// repoRoot walks up from the test working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// sandbox is one isolated bootstrap-secrets run.
type sandbox struct {
	root             string
	stateDir         string
	tempDir          string
	recordsDir       string
	workDir          string
	argvLog          string
	secretLog        string
	kubectlLog       string
	kubectlSecretLog string
	kubectlStdinLog  string
	kubectlPodCount  string
	kubectlStateDir  string
	opensslLog       string
	chmodLog         string
	tofuLog          string
	allArgvLog       string
	realOpenssl      string
	env              []string
	extraEnv         []string
}

// argvRecorder is a fake external command that appends its full argv to
// ALL_ARGV_LOG and then execs the real binary. Wrapping every command the
// scripts invoke means a secret passed through --arg, -d, a positional
// argument or any other argv slot is visible to the tests, wherever it leaks.
func argvRecorder(name string) string {
	realVar := "REAL_" + strings.ToUpper(name)
	return "#!/usr/bin/env bash\n" +
		"set -u\n" +
		"{\n" +
		"  printf '%s' \"" + name + "\"\n" +
		"  for arg in \"$@\"; do printf '\\t%s' \"$arg\"; done\n" +
		"  printf '\\n'\n" +
		"} >> \"$ALL_ARGV_LOG\"\n" +
		"exec \"${" + realVar + ":?" + realVar + " must be set to avoid recursion}\" \"$@\"\n"
}

// fakeKubectl answers the bootstrap-secrets queries. Every `get secret` is
// reported missing so every reconcile branch runs; everything else succeeds.
// It also records the mode of every path passed through --from-file (notably
// the KEK ciphertext handed to `kubectl create secret kodama-keys`) and the
// content of every --from-env-file (the database URLs and passwords), which is
// the only way to observe those temporary files before the trap removes them.
// Client-side dry-run calls emit the manifest kubectl would have produced, so
// the combined single-request path (create secret from an env-file plus a
// file) is exercised instead of silently piping empty input to jq. KUBECTL_SECRETS_PRESENT=1
// makes `get secret` succeed, which models a second run against a cluster where
// the Secrets already exist.
const fakeKubectl = `#!/usr/bin/env bash
set -u
line="kubectl"
for arg in "$@"; do line="${line}"$'\t'"${arg}"; done
printf '%s\n' "$line" >> "$KUBECTL_ARGV_LOG"
printf '%s\n' "$line" >> "$ALL_ARGV_LOG"

# Fault injection: a failure on one Secret's create/apply proves the temp-file
# cleanup runs on the failing path, and a hang lets a signal test interrupt the
# script after the credential file exists but before kubectl consumed it.
if [[ -n "${KUBECTL_FAIL_PATTERN:-}" && " $* " == *" ${KUBECTL_FAIL_PATTERN} "* ]]; then
  echo 'fake kubectl: simulated failure' >&2
  exit 1
fi
if [[ -n "${KUBECTL_HANG_PATTERN:-}" && " $* " == *" ${KUBECTL_HANG_PATTERN} "* ]]; then
  sleep 30
  exit 1
fi

dry_run=0
format=""
name=""
cert_file=""
stdin_body=""
previous=""
for arg in "$@"; do
  case "$arg" in
    --dry-run=client) dry_run=1 ;;
    --cert=*) cert_file="${arg#--cert=}" ;;
    --from-file=*)
      spec="${arg#--from-file=}"
      key=""
      path="$spec"
      if [[ "$spec" == *=* ]]; then
        key="${spec%%=*}"
        path="${spec#*=}"
      fi
      {
        printf 'from_file_key=%s\n' "$key"
        printf 'from_file_path=%s\n' "$path"
        printf 'from_file_mode=%s\n' "$(stat -c '%a' "$path" 2>/dev/null || printf 'missing')"
        printf 'from_file_dir_mode=%s\n' "$(stat -c '%a' "$(dirname "$path")" 2>/dev/null || printf 'missing')"
        printf 'from_file_end\n'
      } >> "$KUBECTL_SECRET_LOG"
      if [[ -n "${KUBECTL_SECRET_CONTENT_LOG:-}" ]]; then
        {
          printf 'from_file_key=%s\n' "$key"
          printf 'from_file_content_begin\n'
          cat "$path" 2>/dev/null || true
          printf '\nfrom_file_content_end\n'
        } >> "$KUBECTL_SECRET_CONTENT_LOG"
      fi
      ;;
    --from-env-file=*)
      path="${arg#--from-env-file=}"
      {
        printf 'env_file_begin=%s\n' "$path"
        printf 'env_file_mode=%s\n' "$(stat -c '%a' "$path" 2>/dev/null || printf 'missing')"
        printf 'env_file_dir_mode=%s\n' "$(stat -c '%a' "$(dirname "$path")" 2>/dev/null || printf 'missing')"
        cat "$path" 2>/dev/null || true
        printf 'env_file_end\n'
      } >> "$KUBECTL_SECRET_LOG"
      ;;
    json|yaml) format="$arg" ;;
  esac
  case "$previous" in
    generic|tls|namespace) name="$arg" ;;
  esac
  previous="$arg"
done

if [[ " $* " == *" -f - "* ]]; then
  stdin_body="$(cat)"
  printf '%s\n' "$stdin_body" >> "$KUBECTL_STDIN_LOG"
fi

# The oauthsonas-tls Secret is modelled so a test can compare the certificate
# the run published with the local leaf file. The client-side
# create --dry-run=client stages the certificate; only a successful apply
# publishes it, so a failing publish leaves the previous value in the cluster
# exactly as kubectl would.
if [[ "$name" == "oauthsonas-tls" && -n "$cert_file" && -n "${KUBECTL_SECRET_STATE_DIR:-}" ]]; then
  base64 <"$cert_file" | tr -d '\n' >"${KUBECTL_SECRET_STATE_DIR}/oauthsonas-tls.pending"
fi
if [[ " $* " == *" apply "* && "$stdin_body" == *"name: oauthsonas-tls"* && -n "${KUBECTL_SECRET_STATE_DIR:-}" ]]; then
  if [[ -f "${KUBECTL_SECRET_STATE_DIR}/oauthsonas-tls.pending" ]]; then
    mv "${KUBECTL_SECRET_STATE_DIR}/oauthsonas-tls.pending" "${KUBECTL_SECRET_STATE_DIR}/oauthsonas-tls.tls.crt"
  fi
fi

previous=""
args=("$@")
for ((index = 0; index < ${#args[@]}; index++)); do
  if [[ "${args[index]}" == "get" && $((index + 1)) -lt ${#args[@]} && "${args[index + 1]}" == "secret" ]]; then
    get_name="${args[index + 2]:-}"
    # A per-Secret fixture models the values an existing cluster returns, so a
    # rerun can prove it reuses them instead of regenerating.
    if [[ -n "$get_name" && -n "${KUBECTL_SECRET_STATE_DIR:-}" && -f "${KUBECTL_SECRET_STATE_DIR}/${get_name}.json" ]]; then
      cat "${KUBECTL_SECRET_STATE_DIR}/${get_name}.json"
      exit 0
    fi
    if [[ "${KUBECTL_SECRETS_PRESENT:-0}" == "1" ]]; then
      if [[ "$get_name" == "kodama-nats-auth" ]]; then
        printf '%s\n' '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"kodama-nats-auth"},"data":{"issuer.seed":"c2VlZA==","issuer.pub":"cHVi","shell-user":"c2hlbGw=","shell-password":"c2gtcGFzcw==","callout-user":"Y2FsbG91dA==","callout-password":"Y2wtcGFzcw==","account":"S09EQU1B"}}'
      fi
      exit 0
    fi
    # Model the real CLI: an absent object is an explicit NotFound, not a bare
    # non-zero exit. bootstrap-secrets must treat only this message as absence
    # and die on every other probe failure.
    echo "Error from server (NotFound): secrets \"${get_name}\" not found" >&2
    exit 1
  fi
done

if [[ $dry_run == 1 ]]; then
  if [[ "$format" == "json" ]]; then
    printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s"},"data":{}}\n' "$name"
  else
    printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\ndata: {}\n' "$name"
  fi
  exit 0
fi
exit 0
`

// fakeScw records its argv and, when the KEK plaintext is passed as @file,
// records the file mode, parent mode and content so the test can compare the
// secret material against every recorded argument.
const fakeScw = `#!/usr/bin/env bash
set -u
{
  printf 'scw'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} | tee -a "$SCW_ARGV_LOG" >> "$ALL_ARGV_LOG"

plaintext_arg=""
for arg in "$@"; do
  case "$arg" in
    plaintext=*) plaintext_arg="${arg#plaintext=}" ;;
  esac
done

if [[ "$plaintext_arg" == @* ]]; then
  file="${plaintext_arg#@}"
  {
    printf 'file_mode=%s\n' "$(stat -c '%a' "$file" 2>/dev/null || printf 'missing')"
    printf 'dir_mode=%s\n' "$(stat -c '%a' "$(dirname "$file")" 2>/dev/null || printf 'missing')"
    printf 'plaintext=%s\n' "$(tr -d '\n' <"$file" 2>/dev/null || true)"
  } >> "$SCW_SECRET_LOG"
fi

if [[ "${SCW_FAIL:-0}" == "1" ]]; then
  echo 'scw: simulated key manager failure' >&2
  exit 1
fi
printf '{"ciphertext":"%s"}\n' "$(printf 'wrapped-kek' | base64 | tr -d '\n')"
exit 0
`

// fakeTofu serves the two outputs bootstrap-secrets reads: the provider's
// database CA certificate and the KMS credentials. It is faithful to the real
// roots: database_ca_certificate is served only when the root named by
// `-chdir` actually declares that output in its outputs.tf, so a cloud whose
// root lacks it reproduces the real refusal instead of passing against a
// friendlier fake. TOFU_FAIL_CA=1 makes the CA output fail, so the fail-closed
// path (no internal-CA fallback) is observable.
// TOFU_KMS_ACCESS/TOFU_KMS_SECRET override the credential values for injection
// tests.
const fakeTofu = `#!/usr/bin/env bash
set -u
{
  printf 'tofu'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} | tee -a "$TOFU_ARGV_LOG" >> "$ALL_ARGV_LOG"

iac_dir=""
for arg in "$@"; do
  case "$arg" in
    -chdir=*) iac_dir="${arg#-chdir=}" ;;
  esac
done

case "$*" in
  *"output -raw database_ca_certificate"*)
    if [[ "${TOFU_FAIL_CA:-0}" == "1" ]]; then
      echo 'tofu: output database_ca_certificate is unavailable' >&2
      exit 1
    fi
    if [[ -z "$iac_dir" || ! -f "${iac_dir}/outputs.tf" ]] ||
      ! grep -Eq '^[[:space:]]*output[[:space:]]+"database_ca_certificate"[[:space:]]*\{' "${iac_dir}/outputs.tf"; then
      echo 'tofu: output database_ca_certificate is unavailable' >&2
      exit 1
    fi
    printf '%s\n' "${TOFU_CA_PEM:-fake-provider-ca}"
    exit 0
    ;;
  *"output -json kms_credentials"*)
    printf '{"access_key":"%s","secret_key":"%s"}\n' \
      "${TOFU_KMS_ACCESS:-SCWACCESSKEY0123456789}" \
      "${TOFU_KMS_SECRET:-11111111-2222-3333-4444-555555555555}"
    exit 0
    ;;
esac
exit 1
`

// fakeChmod interposes on the real chmod and records the mode of the scw KEK
// plaintext file immediately before the first chmod is applied to it. Nothing
// changes the mode between the redirect that creates the file and this chmod,
// so the recorded value is the creation mode: it is exactly what the
// `umask 077` subshell in scripts/lib/scw.sh is responsible for.
const fakeChmod = `#!/usr/bin/env bash
set -u
{
  printf 'chmod'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$ALL_ARGV_LOG"
for arg in "$@"; do
  case "$arg" in
    */kodama-scw-kek.*/plaintext)
      {
        printf 'chmod_path=%s\n' "$arg"
        printf 'chmod_requested=%s\n' "${1:-}"
        printf 'chmod_pre_mode=%s\n' "$(stat -c '%a' "$arg" 2>/dev/null || printf 'missing')"
        printf 'chmod_pre_dir_mode=%s\n' "$(stat -c '%a' "$(dirname "$arg")" 2>/dev/null || printf 'missing')"
        printf 'chmod_end\n'
      } >> "$CHMOD_LOG"
      ;;
    */db-ca.*)
      # bootstrap-secrets chmods the provider CA file separately from the
      # password env-file. Record the requested mode before exec'ing chmod so
      # a 600/644 regression on the public CA is observable after cleanup.
      {
        printf 'db_ca_path=%s\n' "$arg"
        printf 'db_ca_requested=%s\n' "${1:-}"
        printf 'db_ca_end\n'
      } >> "$CHMOD_LOG"
      ;;
  esac
done
exec "${REAL_CHMOD:?REAL_CHMOD must be set to avoid recursion}" "$@"
`

// fakeOpenssl records the mode of the file that receives the 32-byte KEK
// (`openssl rand 32 > "$tmp_plain"`) and emits the deterministic stubKEKRaw.
// OPENSSL_FAIL_RAND=1 fails only the "-hex" calls (the four database passwords
// and the OAuthSonas access code) and lets `rand 32` (the KEK) succeed: a
// swallowed password-generation failure must then be caught by the test's own
// assertion, not by a later, unrelated KEK failure that exits non-zero anyway.
// OPENSSL_FAIL_OAUTHSONAS=1 fails only the `x509 -req` signing of the
// OAuthSonas leaf, so a masked issue step (for example `|| true`) is
// observable. Every other invocation delegates to the real openssl.
const fakeOpenssl = `#!/usr/bin/env bash
set -u
{
  printf 'openssl'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$ALL_ARGV_LOG"
if [[ "${1:-}" == "rand" ]]; then
  if [[ "${OPENSSL_FAIL_RAND:-0}" == "1" && "${2:-}" == "-hex" ]]; then
    echo 'openssl: simulated entropy failure' >&2
    exit 1
  fi
  target="$(readlink -f /proc/$$/fd/1 2>/dev/null || true)"
  {
    printf 'rand_target=%s\n' "$target"
    printf 'rand_target_mode=%s\n' "$(stat -c '%a' "$target" 2>/dev/null || printf 'missing')"
    printf 'rand_target_dir_mode=%s\n' "$(stat -c '%a' "$(dirname "$target")" 2>/dev/null || printf 'missing')"
  } >> "$OPENSSL_LOG"
  if [[ "${2:-}" == "-hex" ]]; then
    # The 24- and 16-byte callers use the historical fixed stub, which several
    # tests assert by value. Callout bypass passwords request 32 bytes; emit a
    # distinct 64-hex value per process so rotation is observable.
    if [[ "${3:-}" == "32" ]]; then
      printf '%08x%08x%08x%08x%08x%08x%08x%08x' \
        "$$" "$(( $$ + 1 ))" "$(( $$ + 2 ))" "$(( $$ + 3 ))" \
        "$(( $$ + 4 ))" "$(( $$ + 5 ))" "$(( $$ + 6 ))" "$(( $$ + 7 ))"
      exit 0
    fi
    printf '00112233445566778899aabbccddeeff0011223344556677'
  else
    printf '%s' '` + stubKEKRaw + `'
  fi
  exit 0
fi
if [[ "${OPENSSL_FAIL_OAUTHSONAS:-0}" == "1" && "${1:-}" == "x509" && " $* " == *"oauthsonas.csr"* ]]; then
  echo 'openssl: simulated OAuthSonas leaf signing failure' >&2
  exit 1
fi
exec "${REAL_OPENSSL:-openssl}" "$@"
`

// newSandbox prepares a descriptor, kubeconfig, reusable PKI and a fake PATH.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	realOpenssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Fatalf("the component test needs a real openssl on PATH: %v", err)
	}
	realChmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatalf("the component test needs a real chmod on PATH: %v", err)
	}
	root := t.TempDir()
	box := &sandbox{
		root:             root,
		stateDir:         filepath.Join(root, "state"),
		tempDir:          filepath.Join(root, "tmp"),
		recordsDir:       filepath.Join(root, "records"),
		argvLog:          filepath.Join(root, "records", "scw.argv"),
		secretLog:        filepath.Join(root, "records", "scw.secret"),
		kubectlLog:       filepath.Join(root, "records", "kubectl.argv"),
		kubectlSecretLog: filepath.Join(root, "records", "kubectl.secret"),
		kubectlStdinLog:  filepath.Join(root, "records", "kubectl.stdin"),
		kubectlPodCount:  filepath.Join(root, "records", "kubectl.pods"),
		kubectlStateDir:  filepath.Join(root, "records", "kubectl.secrets"),
		opensslLog:       filepath.Join(root, "records", "openssl.log"),
		chmodLog:         filepath.Join(root, "records", "chmod.log"),
		tofuLog:          filepath.Join(root, "records", "tofu.argv"),
		allArgvLog:       filepath.Join(root, "records", "all.argv"),
		realOpenssl:      realOpenssl,
	}
	box.workDir = filepath.Join(box.stateDir, "scw", "stage")

	for _, dir := range []string{
		box.tempDir,
		box.recordsDir,
		filepath.Join(root, "home"),
		filepath.Join(root, "bin"),
		filepath.Join(box.workDir, "pki"),
		filepath.Join(box.workDir, "tfdata"),
		box.kubectlStateDir,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeExecutable(t, filepath.Join(root, "bin", "scw"), fakeScw)
	writeExecutable(t, filepath.Join(root, "bin", "kubectl"), fakeKubectl)
	writeExecutable(t, filepath.Join(root, "bin", "tofu"), fakeTofu)
	writeExecutable(t, filepath.Join(root, "bin", "openssl"), fakeOpenssl)
	writeExecutable(t, filepath.Join(root, "bin", "chmod"), fakeChmod)

	// Wrap every other command the script can hand data to, so the argv
	// recorder observes it before exec'ing the real binary. od/sed/awk/curl
	// are not used today; cat/cut/head/xxd/python3/perl/tee are instrumented
	// the same way so a secret handed to any of them is still visible. (printf
	// is a bash builtin here, not an external command, so it has no recorder.)
	realCommands := []string(nil)
	for _, name := range []string{"jq", "base64", "tr", "od", "sed", "awk", "curl", "cat", "cut", "head", "xxd", "python3", "perl", "tee"} {
		realPath, lookupErr := exec.LookPath(name)
		if lookupErr != nil {
			continue
		}
		writeExecutable(t, filepath.Join(root, "bin", name), argvRecorder(name))
		realCommands = append(realCommands, "REAL_"+strings.ToUpper(name)+"="+realPath)
	}

	descriptor := `{
  "cloud": "scw",
  "cluster_name": "stage",
  "environment": "production",
  "account_or_project_id": "project-1",
  "region": "fr-par",
  "release_namespace": "kodama-system",
  "postgres": {"host": "db.example", "port": 5432, "database_name": "kodama", "admin_user": "kodama_admin"},
  "kms": {"key_id": "fr-par/00000000-0000-0000-0000-000000000000", "key_ref": "scw-kms:fr-par/00000000-0000-0000-0000-000000000000", "key_adapter": "scw-kms"},
  "kubernetes": {"cluster_name": "stage", "storage_class": "sbs-default"},
  "tenants": ["tenant-a"]
}
`
	writeFile(t, filepath.Join(box.workDir, "descriptor.json"), descriptor)
	writeFile(t, filepath.Join(box.workDir, "kubeconfig.yaml"), "")
	// generate_cluster_pki reuses an existing complete PKI; the fakes never
	// read these files, so their content is irrelevant.
	for _, name := range []string{"ca.crt", "ca.key", "kodama.crt", "nats.crt", "client.crt"} {
		writeFile(t, filepath.Join(box.workDir, "pki", name), "test-"+name)
	}

	box.env = append(os.Environ(),
		"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+filepath.Join(root, "home"),
		"TMPDIR="+box.tempDir,
		"KODAMA_DEPLOY_STATE_DIR="+box.stateDir,
		"KODAMA_CLUSTER=stage",
		"KODAMA_DB_RUNTIME_PASSWORD="+dbSentinels[0],
		"KODAMA_DB_CONTROLLER_PASSWORD="+dbSentinels[1],
		"KODAMA_DB_PUBLISHER_PASSWORD="+dbSentinels[2],
		"KODAMA_DB_AUTH_PASSWORD="+dbSentinels[3],
		"SCW_ARGV_LOG="+box.argvLog,
		"SCW_SECRET_LOG="+box.secretLog,
		"KUBECTL_ARGV_LOG="+box.kubectlLog,
		"KUBECTL_SECRET_LOG="+box.kubectlSecretLog,
		"KUBECTL_STDIN_LOG="+box.kubectlStdinLog,
		"KUBECTL_POD_COUNTER="+box.kubectlPodCount,
		"KUBECTL_SECRET_STATE_DIR="+box.kubectlStateDir,
		"TOFU_ARGV_LOG="+box.tofuLog,
		"OPENSSL_LOG="+box.opensslLog,
		"CHMOD_LOG="+box.chmodLog,
		"REAL_OPENSSL="+realOpenssl,
		"REAL_CHMOD="+realChmod,
		"ALL_ARGV_LOG="+box.allArgvLog,
		"KODAMA_NATS_AUTH_KEY_TOOL="+natsAuthKeyToolPath,
	)
	box.env = append(box.env, realCommands...)
	return box
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// runBootstrapSecrets runs the real script against the fake PATH with a
// permissive umask (000), so the KEK plaintext file's creation mode reflects
// only the script's own `umask 077` subshell.
func (b *sandbox) run(t *testing.T, failScw bool) (int, string) {
	t.Helper()
	return b.runWithUmask(t, failScw, "000")
}

// runWithUmask runs the real script with the given shell umask. The wrapper
// changes only the child shell's umask, so removing `umask 077` from
// scripts/lib/scw.sh is observable (the plaintext file is then created 0666
// until the later chmod).
func (b *sandbox) runWithUmask(t *testing.T, failScw bool, umask string) (int, string) {
	t.Helper()
	return b.runArgs(t, failScw, umask)
}

// runArgs runs the real script with the given shell umask and extra arguments
// after the descriptor and kubeconfig (for example --rotate-pki).
func (b *sandbox) runArgs(t *testing.T, failScw bool, umask string, extra ...string) (int, string) {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "bootstrap-secrets")
	args := []string{"bash", script, "scw", "--cluster", "stage",
		"--descriptor", filepath.Join(b.workDir, "descriptor.json"),
		"--kubeconfig", filepath.Join(b.workDir, "kubeconfig.yaml")}
	args = append(args, extra...)
	args = append([]string{"bash", "-c", `umask "$1"; shift; exec "$@"`, "kodama-umask", umask}, args...)
	command := exec.Command(args[0], args[1:]...)
	env := append([]string(nil), b.env...)
	env = append(env, b.extraEnv...)
	if failScw {
		env = append(env, "SCW_FAIL=1")
	}
	command.Env = env
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run bootstrap-secrets: %v\n%s", err, output)
		}
		code = exitError.ExitCode()
	}
	return code, string(output)
}

// withEnv appends environment entries to every subsequent run.
func (b *sandbox) withEnv(entries ...string) *sandbox {
	b.extraEnv = append(b.extraEnv, entries...)
	return b
}

// unsetEnv removes keys from both the base and the per-test environment so a
// test can exercise a path that only runs when a variable is absent (password
// generation) or toggle an opt-in between runs (OAuthSonas).
func (b *sandbox) unsetEnv(keys ...string) *sandbox {
	drop := make(map[string]bool, len(keys))
	for _, key := range keys {
		drop[key] = true
	}
	filter := func(entries []string) []string {
		var kept []string
		for _, entry := range entries {
			name := entry
			if before, _, ok := strings.Cut(entry, "="); ok {
				name = before
			}
			if !drop[name] {
				kept = append(kept, entry)
			}
		}
		return kept
	}
	b.env = filter(b.env)
	b.extraEnv = filter(b.extraEnv)
	return b
}

// bashCommand builds the direct script invocation used by the signal tests.
func (b *sandbox) bashCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "bootstrap-secrets")
	command := exec.Command("bash", script, "scw", "--cluster", "stage",
		"--descriptor", filepath.Join(b.workDir, "descriptor.json"),
		"--kubeconfig", filepath.Join(b.workDir, "kubeconfig.yaml"))
	command.Env = append(append([]string(nil), b.env...), b.extraEnv...)
	return command
}

// readAllArgv returns every argv line recorded by any wrapped command.
func (b *sandbox) readAllArgv(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(b.allArgvLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read all-argv log: %v", err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertSecretFormsAbsentFromAllArgv scans every recorded command line for the
// labelled credential forms. This is the custody invariant: a secret must not
// be an argument of any process (kubectl, scw, jq, openssl, base64, tr, ...),
// because /proc/<pid>/cmdline is world-readable.
func assertSecretFormsAbsentFromAllArgv(t *testing.T, box *sandbox, forms []credentialForm) {
	t.Helper()
	lines := box.readAllArgv(t)
	if len(lines) == 0 {
		t.Fatal("no command argv was recorded; the all-argv credential check would be vacuous")
	}
	for _, line := range lines {
		if hits := argvCredentialHits(line, forms); len(hits) != 0 {
			t.Errorf("%v appears on a command argv: %s", hits, line)
		}
	}
}

// assertEveryRecorderRan proves the argv recorder observed the commands the
// script actually uses, so the all-argv scan is not vacuous. cat is the only
// one of the newly instrumented commands bootstrap-secrets runs; cut, head,
// xxd, python3, perl and tee are wrapped for coverage but not required here.
func assertEveryRecorderRan(t *testing.T, box *sandbox) {
	t.Helper()
	seen := make(map[string]bool)
	for _, line := range box.readAllArgv(t) {
		seen[strings.SplitN(line, "\t", 2)[0]] = true
	}
	for _, want := range []string{"kubectl", "scw", "jq", "openssl", "base64", "tr", "tofu", "cat"} {
		if !seen[want] {
			t.Errorf("the argv recorder never observed %s; the all-argv scan would miss a leak through it", want)
		}
	}
}

// resetLogs truncates the recorded argv/secret logs so a second run can be
// asserted independently of the first.
func (b *sandbox) resetLogs(t *testing.T) {
	t.Helper()
	for _, path := range []string{b.argvLog, b.secretLog, b.kubectlLog, b.kubectlSecretLog, b.kubectlStdinLog, b.opensslLog, b.chmodLog, b.tofuLog, b.allArgvLog} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("reset %s: %v", path, err)
		}
	}
}

// writeDescriptor replaces the descriptor the script reads.
func (b *sandbox) writeDescriptor(t *testing.T, descriptor string) {
	t.Helper()
	writeFile(t, filepath.Join(b.workDir, "descriptor.json"), descriptor)
}

// readKubectlStdin returns everything the fake kubectl read from `-f -`.
func (b *sandbox) readKubectlStdin(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(b.kubectlStdinLog)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read kubectl stdin log: %v", err)
	}
	return string(body)
}

// readArgv returns every recorded scw argv line.
func (b *sandbox) readArgv(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(b.argvLog)
	if err != nil {
		t.Fatalf("read scw argv log: %v", err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// readKubectlArgv returns every recorded kubectl argv line. A missing log
// means kubectl was never invoked, which some refusal tests assert.
func (b *sandbox) readKubectlArgv(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(b.kubectlLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read kubectl argv log: %v", err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// kekMaterial is the key-encryption key exactly as the fake key manager saw
// it: the base64 value the script handed over, the decoded raw bytes, their
// hex form, and the recorded file/directory modes.
type kekMaterial struct {
	b64      string
	rawHex   string
	raw      []byte
	fileMode string
	dirMode  string
}

// secretMaterial returns the KEK material the helper wrote plus the recorded
// file/directory modes.
func (b *sandbox) secretMaterial(t *testing.T) kekMaterial {
	t.Helper()
	body, err := os.ReadFile(b.secretLog)
	if err != nil {
		t.Fatalf("fake scw recorded no plaintext=@file invocation (the KEK was not passed by file): %v", err)
	}
	var kek kekMaterial
	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "plaintext="):
			kek.b64 = strings.TrimPrefix(line, "plaintext=")
		case strings.HasPrefix(line, "file_mode="):
			kek.fileMode = strings.TrimPrefix(line, "file_mode=")
		case strings.HasPrefix(line, "dir_mode="):
			kek.dirMode = strings.TrimPrefix(line, "dir_mode=")
		}
	}
	if kek.b64 == "" {
		t.Fatal("fake scw did not record a plaintext value")
	}
	decoded, err := base64.StdEncoding.DecodeString(kek.b64)
	if err != nil {
		t.Fatalf("plaintext file is not base64: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("KEK plaintext is %d bytes, want 32", len(decoded))
	}
	kek.raw = decoded
	kek.rawHex = hex.EncodeToString(decoded)
	return kek
}

// stubKEKMaterial returns the KEK the fake openssl is known to produce, so the
// argv assertions can run before (and independently of) the fake scw plaintext
// log. A regression that passes the KEK on the command line writes no
// plaintext=@file record, and secretMaterial would abort the test before any
// argv check; using the known key keeps that assertion reachable.
func stubKEKMaterial() kekMaterial {
	raw := []byte(stubKEKRaw)
	return kekMaterial{
		b64:    base64.StdEncoding.EncodeToString(raw),
		rawHex: hex.EncodeToString(raw),
		raw:    raw,
	}
}

// chmodRecord is the mode of the scw KEK plaintext file observed immediately
// before the script's chmod applied 600 to it.
type chmodRecord struct {
	path    string
	request string
	mode    string
	dirMode string
}

// scwPlaintextCreateRecords parses the fake chmod log. A record exists only
// for the scw KEK plaintext file, so an empty result means the file never
// reached a chmod (or chmod was removed from the helper).
func (b *sandbox) scwPlaintextCreateRecords(t *testing.T) []chmodRecord {
	t.Helper()
	body, err := os.ReadFile(b.chmodLog)
	if err != nil {
		return nil
	}
	var records []chmodRecord
	var current chmodRecord
	inRecord := false
	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "chmod_path="):
			current = chmodRecord{path: strings.TrimPrefix(line, "chmod_path=")}
			inRecord = true
		case strings.HasPrefix(line, "chmod_requested="):
			current.request = strings.TrimPrefix(line, "chmod_requested=")
		case strings.HasPrefix(line, "chmod_pre_mode="):
			current.mode = strings.TrimPrefix(line, "chmod_pre_mode=")
		case strings.HasPrefix(line, "chmod_pre_dir_mode="):
			current.dirMode = strings.TrimPrefix(line, "chmod_pre_dir_mode=")
		case line == "chmod_end":
			records = append(records, current)
			inRecord = false
		}
	}
	if inRecord {
		records = append(records, current)
	}
	return records
}

// assertScwPlaintextCreatedPrivate proves the KEK plaintext file had no
// group/other permission bits at the moment it first existed: the interposed
// chmod records the mode between the redirect that created the file and the
// chmod 600 that follows. Deleting `umask 077` from scripts/lib/scw.sh (with
// the test run under a 000 shell umask) creates the file 0666 and dies here;
// the final 0600 mode alone would not show it.
func assertScwPlaintextCreatedPrivate(t *testing.T, box *sandbox) {
	t.Helper()
	records := box.scwPlaintextCreateRecords(t)
	if len(records) == 0 {
		t.Fatal("fake chmod never observed the scw KEK plaintext file; the creation-mode check would be vacuous")
	}
	for _, record := range records {
		if !strings.HasSuffix(record.path, "/plaintext") {
			t.Errorf("chmod interposer recorded unexpected path %s", record.path)
		}
		if record.mode != "600" {
			t.Errorf("scw KEK plaintext %s existed with mode %s immediately before chmod (want 600; group/other bits mean `umask 077` is gone): %s", record.path, record.mode, record.request)
		}
		if record.dirMode != "700" {
			t.Errorf("scw KEK plaintext directory mode = %s before chmod, want 700", record.dirMode)
		}
		if record.request != "600" {
			t.Errorf("scw KEK plaintext chmod requested %s, want 600", record.request)
		}
	}
}

// credentialForm is one piece of secret material and the label used in failure
// messages. The value itself is never printed.
type credentialForm struct {
	label  string
	secret string
}

// kekForms returns the KEK in every form it could leak: base64, raw hex and
// the decoded 32 bytes. assertNoKEKLeft scans the disk with the same three
// forms, so bytes that die in a file also die on a command line. The raw value
// is a full 32-byte key, long enough that a match on unrelated argv content is
// not plausible.
func kekForms(kek kekMaterial) []credentialForm {
	return []credentialForm{
		{label: "KEK (base64)", secret: kek.b64},
		{label: "KEK (hex)", secret: kek.rawHex},
		{label: "KEK (raw)", secret: string(kek.raw)},
	}
}

// dbForms labels the four database passwords, which only the kubectl argv must
// not carry.
func dbForms() []credentialForm {
	forms := make([]credentialForm, 0, len(dbSentinels))
	for _, sentinel := range dbSentinels {
		forms = append(forms, credentialForm{label: "database password", secret: sentinel})
	}
	return forms
}

// scwCredentialForms is the credential composition the scw argv assertion
// uses: the KEK in base64, raw-hex and decoded raw-byte form. The self-test
// drives this same helper, so dropping a form here also fails the self-test.
func scwCredentialForms(kek kekMaterial) []credentialForm {
	return kekForms(kek)
}

// kubectlCredentialForms is the credential composition the kubectl argv
// assertion uses: the KEK in every form plus the four database passwords. The
// self-test drives this same helper, so dropping the raw KEK form or the
// database forms here also fails the self-test.
func kubectlCredentialForms(kek kekMaterial, db []credentialForm) []credentialForm {
	forms := make([]credentialForm, 0, 3+len(db))
	forms = append(forms, kekForms(kek)...)
	forms = append(forms, db...)
	return forms
}

// argvCredentialHits returns the labels of the credential forms found in one
// recorded argv line. Besides the whole form, a credential split across two
// argv elements is caught by its minimumSplitNeedle-byte prefix: e.g.
// `--from-literal=kek=<first half> <second half>` and
// `plaintext=@file <b64[:20]> <b64[20:]>` leave no single element containing
// the full secret, but one element still carries the first 12 bytes. Forms
// shorter than twice that minimum cannot be split into two qualifying
// fragments and are checked whole only.
func argvCredentialHits(line string, forms []credentialForm) []string {
	var hits []string
	for _, form := range forms {
		if form.secret == "" {
			continue
		}
		if strings.Contains(line, form.secret) {
			hits = append(hits, form.label)
			continue
		}
		if len(form.secret) < 2*minimumSplitNeedle {
			continue
		}
		if strings.Contains(line, form.secret[:minimumSplitNeedle]) {
			hits = append(hits, form.label+" (split prefix)")
		}
	}
	return hits
}

// credentialReporter is the subset of *testing.T the argv assertions use. The
// self-test passes a recorder so the real assertion composition (not a copy of
// it) is what rejects every encoding.
type credentialReporter interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Fatal(args ...any)
}

// assertKEKAbsentFromArgv proves the key never appears on a command line in
// its base64, raw-hex or decoded raw-byte form, nor split across argv elements.
// The raw form matters: the helper hands the key manager a base64 value, so a
// regression that decodes it back to bytes (`plaintext=<raw>`) or splices it
// onto argv as a separate element is caught only by the raw needle (or its
// split prefix).
func assertKEKAbsentFromArgv(t credentialReporter, argv []string, kek kekMaterial) {
	t.Helper()
	if len(argv) == 0 {
		t.Fatal("fake scw was never invoked")
	}
	if len(kek.raw) < 2*minimumSplitNeedle {
		t.Fatalf("raw KEK needle is %d bytes; the raw and split-argv checks would be vacuous", len(kek.raw))
	}
	forms := scwCredentialForms(kek)
	// Positive control: the @file form is what the recorder must have
	// observed. Without it the per-line plaintext= check below would pass
	// against a log that never recorded the key transfer at all.
	observedFileForm := false
	for _, line := range argv {
		if strings.Contains(line, "plaintext=@") {
			observedFileForm = true
			break
		}
	}
	if !observedFileForm {
		t.Fatal("no scw argv line carried plaintext=@<file>; the @file-form check would be vacuous")
	}
	for _, line := range argv {
		if hits := argvCredentialHits(line, forms); len(hits) != 0 {
			t.Fatalf("%v appears on scw argv: %s", hits, line)
		}
		if strings.Contains(line, "plaintext=") && !strings.Contains(line, "plaintext=@") {
			t.Fatalf("plaintext is not passed with the @file form: %s", line)
		}
	}
}

// assertKubectlArgvCarriesNoCredentials proves the key-encryption key and the
// database passwords never reach a kubectl command line, in whole form or
// split across argv elements. The fake kubectl records every argv in
// KUBECTL_ARGV_LOG, so a --from-literal= that echoes a password or the KEK in
// base64, hex or decoded raw-byte form dies here even though the scw argv
// stays clean. The secret-creating calls must hand kubectl the file forms, so
// an empty or unrelated log cannot satisfy the assertion vacuously.
func assertKubectlArgvCarriesNoCredentials(t credentialReporter, lines []string, kek kekMaterial, wantKeySecret bool) {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("fake kubectl recorded no argv lines; the credential-custody check would be vacuous")
	}
	if len(kek.raw) < 2*minimumSplitNeedle {
		t.Fatalf("raw KEK needle is %d bytes; the raw and split-argv checks would be vacuous", len(kek.raw))
	}
	forms := kubectlCredentialForms(kek, dbForms())
	for _, line := range lines {
		if hits := argvCredentialHits(line, forms); len(hits) != 0 {
			t.Errorf("%v appears on kubectl argv: %s", hits, line)
		}
	}

	createSecret := func(name string) []string {
		for _, line := range lines {
			fields := strings.Split(line, "\t")
			if strings.Contains(strings.Join(fields, " "), "create secret generic "+name) {
				return fields
			}
		}
		return nil
	}
	hasArg := func(fields []string, prefix string) bool {
		for _, field := range fields {
			if strings.HasPrefix(field, prefix) {
				return true
			}
		}
		return false
	}

	database := createSecret("kodama-database")
	if database == nil {
		t.Fatal("no `kubectl create secret kodama-database` argv was recorded; the argv check would be vacuous")
	}
	if !hasArg(database, "--from-env-file=") {
		t.Errorf("kodama-database create call did not hand the passwords to kubectl through --from-env-file: %v", database)
	}
	if wantKeySecret {
		keys := createSecret("kodama-keys")
		if keys == nil {
			t.Fatal("no `kubectl create secret kodama-keys` argv was recorded; the argv check would be vacuous")
		}
		if !hasArg(keys, "--from-file=kek-v1.blob=") {
			t.Errorf("kodama-keys create call did not hand the KEK ciphertext to kubectl through --from-file: %v", keys)
		}
	}
}

// assertKEKAbsentFromOutput proves a shell that echoes its commands (xtrace)
// or any other diagnostic never printed the key on stdout or stderr.
func assertKEKAbsentFromOutput(t *testing.T, output string, kek kekMaterial) {
	t.Helper()
	for _, form := range kekForms(kek) {
		if form.secret != "" && strings.Contains(output, form.secret) {
			t.Errorf("%s appears in the combined stdout+stderr of the script run", form.label)
		}
	}
}

// assertAbsentFromTrees scans the work and temp trees for any file carrying
// one of the labelled byte sequences.
func assertAbsentFromTrees(t *testing.T, dirs []string, needles map[string][]byte) {
	t.Helper()
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for label, needle := range needles {
				if len(needle) > 0 && bytes.Contains(body, needle) {
					t.Errorf("%s material left behind in %s", label, path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
	}
}

// assertNoKEKLeft scans the work and temp trees for any file carrying the KEK
// in base64, raw-hex or raw-byte form. The argv assertions above use the same
// three forms through kekForms, so disk and command-line custody stay
// symmetric.
func assertNoKEKLeft(t *testing.T, dirs []string, kek kekMaterial) {
	t.Helper()
	needles := make(map[string][]byte, 3)
	for _, form := range kekForms(kek) {
		needles[form.label] = []byte(form.secret)
	}
	assertAbsentFromTrees(t, dirs, needles)
}

// assertDatabaseSecretsCleaned proves the four database passwords were written
// to the 0600 env-file that kubectl consumed, and that neither the passwords
// nor the env-file/CA-file exist on disk after the run. Without the
// register_bootstrap_temp_path calls a failed run leaves db-env.* (four URLs
// with passwords) and db-ca.* in the persistent work directory.
func assertDatabaseSecretsCleaned(t *testing.T, box *sandbox) {
	t.Helper()
	body, err := os.ReadFile(box.kubectlSecretLog)
	if err != nil {
		t.Fatalf("fake kubectl recorded no secret file content: %v", err)
	}
	for _, sentinel := range dbSentinels {
		if !strings.Contains(string(body), sentinel) {
			t.Errorf("fake kubectl never received database password sentinel %q; the env-file path was not exercised", sentinel)
		}
	}

	dirs := []string{box.workDir, box.tempDir}
	needles := make(map[string][]byte, len(dbSentinels))
	for _, sentinel := range dbSentinels {
		needles[sentinel] = []byte(sentinel)
	}
	assertAbsentFromTrees(t, dirs, needles)

	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			name := entry.Name()
			for _, prefix := range []string{"db-env.", "db-ca.", "admin-env.", "creds-env.", "nats-auth."} {
				if strings.HasPrefix(name, prefix) {
					t.Errorf("temporary credential file left behind: %s", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
	}
}

// kubectlFileRecord is one --from-file argument recorded by the fake kubectl.
type kubectlFileRecord struct {
	key     string
	path    string
	mode    string
	dirMode string
}

// kubectlFileRecords parses the fake kubectl secret log.
func (b *sandbox) kubectlFileRecords(t *testing.T) []kubectlFileRecord {
	t.Helper()
	body, err := os.ReadFile(b.kubectlSecretLog)
	if err != nil {
		t.Fatalf("fake kubectl recorded no secret file metadata: %v", err)
	}
	var records []kubectlFileRecord
	var current kubectlFileRecord
	inRecord := false
	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "from_file_key="):
			current = kubectlFileRecord{key: strings.TrimPrefix(line, "from_file_key=")}
			inRecord = true
		case strings.HasPrefix(line, "from_file_path="):
			current.path = strings.TrimPrefix(line, "from_file_path=")
		case strings.HasPrefix(line, "from_file_mode="):
			current.mode = strings.TrimPrefix(line, "from_file_mode=")
		case strings.HasPrefix(line, "from_file_dir_mode="):
			current.dirMode = strings.TrimPrefix(line, "from_file_dir_mode=")
		case line == "from_file_end":
			records = append(records, current)
			inRecord = false
		}
	}
	if inRecord {
		records = append(records, current)
	}
	return records
}

// assertKubernetesKeyFileMode proves the KEK ciphertext handed to
// `kubectl create secret kodama-keys` was a 0600 file inside a 0700 directory.
// A 0755 KEK directory or a 0644 ciphertext would expose the wrapped key to
// other local users.
func assertKubernetesKeyFileMode(t *testing.T, box *sandbox) {
	t.Helper()
	found := false
	for _, record := range box.kubectlFileRecords(t) {
		if record.key != "kek-v1.blob" {
			continue
		}
		found = true
		if record.mode != "600" {
			t.Errorf("kek-v1.blob source %s mode = %s, want 600", record.path, record.mode)
		}
		if record.dirMode != "700" {
			t.Errorf("kek-v1.blob source directory mode = %s, want 700", record.dirMode)
		}
	}
	if !found {
		t.Fatal("fake kubectl never recorded the kodama-keys --from-file=kek-v1.blob argument")
	}
}

// assertOpensslKEKTargetMode proves the file that receives `openssl rand 32`
// (the KEK plaintext) was 0600 inside a 0700 directory at the moment it held
// key material. This also runs on the failing path, before kubectl sees it.
func assertOpensslKEKTargetMode(t *testing.T, box *sandbox) {
	t.Helper()
	body, err := os.ReadFile(box.opensslLog)
	if err != nil {
		t.Fatalf("fake openssl recorded no `openssl rand 32` invocation: %v", err)
	}
	target, mode, dirMode := "", "", ""
	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "rand_target="):
			target = strings.TrimPrefix(line, "rand_target=")
		case strings.HasPrefix(line, "rand_target_mode="):
			mode = strings.TrimPrefix(line, "rand_target_mode=")
		case strings.HasPrefix(line, "rand_target_dir_mode="):
			dirMode = strings.TrimPrefix(line, "rand_target_dir_mode=")
		}
	}
	if target == "" {
		t.Fatal("fake openssl did not record the `openssl rand 32` stdout target")
	}
	if filepath.Base(target) != "plaintext" {
		t.Errorf("openssl rand 32 wrote to %s, want the KEK plaintext file", target)
	}
	if mode != "600" {
		t.Errorf("KEK plaintext file %s mode = %s, want 600", target, mode)
	}
	if dirMode != "700" {
		t.Errorf("KEK plaintext directory mode = %s, want 700", dirMode)
	}
}

// dbCAChmodRecords returns the modes bootstrap-secrets requested for the
// provider CA file. The CA certificate is public material, so the contract is
// 0644; the password env-file next to it stays 0600.
func (b *sandbox) dbCAChmodRecords(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(b.chmodLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read chmod log: %v", err)
	}
	var modes []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if after, ok := strings.CutPrefix(line, "db_ca_requested="); ok {
			modes = append(modes, after)
		}
		if strings.HasPrefix(line, "db_ca_path=") && !strings.Contains(line, "db-ca.") {
			t.Errorf("chmod interposer recorded an unexpected database CA path %q", line)
		}
	}
	return modes
}

// TestBootstrapSecretsDatabaseCAFileIsPublic pins the mode of the provider CA
// certificate file: it is public material (a root certificate mounted into the
// client pods), so it is 0644 while the four-password env-file beside it stays
// 0600. The chmod interposer observes the requested mode because the file is
// removed by the EXIT trap before the test could stat it.
func TestBootstrapSecretsDatabaseCAFileIsPublic(t *testing.T) {
	box := newSandbox(t)
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
	}
	modes := box.dbCAChmodRecords(t)
	if len(modes) == 0 {
		t.Fatal("bootstrap-secrets never chmodded the database CA file; the public-CA mode check would be vacuous")
	}
	for _, mode := range modes {
		if mode != "644" {
			t.Errorf("database CA file requested mode %s, want 644 (public certificate; the password env-file stays 600)", mode)
		}
	}
}

// TestBootstrapSecretsFailsClosedWhenKEKEncryptFails runs the real script with
// a failing key manager and proves the run is non-zero and leaves neither the
// KEK nor the database passwords behind.
func TestBootstrapSecretsFailsClosedWhenKEKEncryptFails(t *testing.T) {
	box := newSandbox(t)
	code, output := box.run(t, true)
	if code == 0 {
		t.Fatalf("bootstrap-secrets succeeded although the KMS encrypt failed:\n%s", output)
	}
	// Read the argv and assert custody before secretMaterial: a regression that
	// passes the KEK as `plaintext=<raw>`/`plaintext=<b64>` writes no
	// plaintext=@file record, and secretMaterial's fatal must not mask the argv
	// assertion. Fake openssl's output is deterministic, so the needles are
	// known without observing the file.
	assertKEKAbsentFromArgv(t, box.readArgv(t), stubKEKMaterial())
	kek := box.secretMaterial(t)
	if kek.fileMode != "600" {
		t.Errorf("KEK plaintext file mode = %s, want 600", kek.fileMode)
	}
	if kek.dirMode != "700" {
		t.Errorf("KEK plaintext directory mode = %s, want 700", kek.dirMode)
	}
	assertKubectlArgvCarriesNoCredentials(t, box.readKubectlArgv(t), kek, false)
	assertKEKAbsentFromOutput(t, output, kek)
	assertNoKEKLeft(t, []string{box.workDir, box.tempDir}, kek)
	assertOpensslKEKTargetMode(t, box)
	assertScwPlaintextCreatedPrivate(t, box)
	assertDatabaseSecretsCleaned(t, box)
}

// TestBootstrapSecretsRemovesKEKTempFilesOnSuccess proves the successful path
// also removes every plaintext file and hands kubectl 0600 files.
func TestBootstrapSecretsRemovesKEKTempFilesOnSuccess(t *testing.T) {
	box := newSandbox(t)
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
	}
	assertKEKAbsentFromArgv(t, box.readArgv(t), stubKEKMaterial())
	kek := box.secretMaterial(t)
	if kek.fileMode != "600" {
		t.Errorf("KEK plaintext file mode = %s, want 600", kek.fileMode)
	}
	if kek.dirMode != "700" {
		t.Errorf("KEK plaintext directory mode = %s, want 700", kek.dirMode)
	}
	assertKubectlArgvCarriesNoCredentials(t, box.readKubectlArgv(t), kek, true)
	assertKEKAbsentFromOutput(t, output, kek)
	assertNoKEKLeft(t, []string{box.workDir, box.tempDir}, kek)
	assertOpensslKEKTargetMode(t, box)
	assertKubernetesKeyFileMode(t, box)
	assertDatabaseSecretsCleaned(t, box)
	assertScwPlaintextCreatedPrivate(t, box)
}

// recordingReporter captures what the real argv assertions report, so the
// self-test exercises the assertions themselves rather than a copy of their
// needle logic. Deleting the hit loop from an assertion leaves leaks
// unreported and fails this test.
type recordingReporter struct {
	errors []string
	fatals []string
}

func (r *recordingReporter) Helper() {}

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingReporter) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recordingReporter) Fatal(args ...any) {
	r.fatals = append(r.fatals, fmt.Sprint(args...))
}

func (r *recordingReporter) reset() {
	r.errors, r.fatals = nil, nil
}

func (r *recordingReporter) failed() bool {
	return len(r.errors)+len(r.fatals) != 0
}

func (r *recordingReporter) reported(want string) bool {
	for _, message := range append(append([]string(nil), r.errors...), r.fatals...) {
		if strings.Contains(message, want) {
			return true
		}
	}
	return false
}

// TestBootstrapSecretsKEKFilePrivateRegardlessOfUmask proves the scw helper's
// own `umask 077` (not the caller's umask or the later chmod) creates the KEK
// plaintext file without group/other bits. The test's shell umask is 000 and a
// chmod interposer records the mode between file creation and chmod 600.
// Deleting `umask 077` from scripts/lib/scw.sh creates the file 0666 and fails
// here; a 077 caller umask would hide the defect, so both are exercised.
func TestBootstrapSecretsKEKFilePrivateRegardlessOfUmask(t *testing.T) {
	for _, umask := range []string{"000", "077"} {
		t.Run("umask_"+umask, func(t *testing.T) {
			box := newSandbox(t)
			code, output := box.runWithUmask(t, false, umask)
			if code != 0 {
				t.Fatalf("bootstrap-secrets failed under umask %s: exit %d\n%s", umask, code, output)
			}
			kek := box.secretMaterial(t)
			if kek.fileMode != "600" {
				t.Errorf("umask %s: KEK plaintext file mode = %s at scw invocation, want 600", umask, kek.fileMode)
			}
			if kek.dirMode != "700" {
				t.Errorf("umask %s: KEK plaintext directory mode = %s, want 700", umask, kek.dirMode)
			}
			assertScwPlaintextCreatedPrivate(t, box)
		})
	}
}

// TestCredentialArgvChecksRejectEveryEncoding drives the real argv assertions
// directly so they cannot become vacuous. Each KEK form (whole or split across
// two elements) and each database password is placed on a representative
// command line and the assertion must report it, while the file forms the
// scripts actually use (--from-file, --from-env-file, plaintext=@file) must
// pass. The raw-byte cases matter: `--from-literal=kek=<raw>` and a bare raw
// element survive when only the base64 and hex needles are checked.
func TestCredentialArgvChecksRejectEveryEncoding(t *testing.T) {
	kek := stubKEKMaterial()
	if len(kek.raw) != 32 {
		t.Fatalf("synthetic KEK is %d bytes, want 32", len(kek.raw))
	}
	for _, form := range kubectlCredentialForms(kek, dbForms()) {
		if len(form.secret) < 2*minimumSplitNeedle {
			t.Errorf("credential form %s is %d bytes; split-needle coverage needs at least %d", form.label, len(form.secret), 2*minimumSplitNeedle)
		}
	}

	cleanKubectl := []string{
		"kubectl\t-n\tkodama-system\tcreate\tsecret\tgeneric\tkodama-database\t--from-env-file=/state/scw/stage/db-env.abc\t--from-file=ca.crt=/state/scw/stage/db-ca.abc",
		"kubectl\t-n\tkodama-system\tcreate\tsecret\tgeneric\tkodama-keys\t--from-env-file=/state/scw/stage/kek.abc/keys-env\t--from-file=kek-v1.blob=/state/scw/stage/kek.abc/ciphertext",
		"kubectl\t-n\tkodama-system\tget\tsecret\tkodama-keys",
	}
	cleanScw := []string{
		"scw\tkeymanager\tkey\tencrypt\t00000000-0000-0000-0000-000000000000\tregion=fr-par\tplaintext=@/tmp/kodama-scw-kek.abc/plaintext\t-o\tjson",
	}

	recorder := &recordingReporter{}
	assertKubectlArgvCarriesNoCredentials(recorder, cleanKubectl, kek, true)
	if recorder.failed() {
		t.Errorf("clean kubectl argv was flagged: %v", recorder.fatals)
	}
	recorder.reset()
	assertKEKAbsentFromArgv(recorder, cleanScw, kek)
	if recorder.failed() {
		t.Errorf("clean scw argv was flagged: %v", recorder.fatals)
	}

	kubectlLeaks := []struct {
		line string
		want string
	}{
		{line: "kubectl\t...\t--from-literal=kek=" + string(kek.raw), want: "KEK (raw)"},
		{line: "kubectl\t...\t--from-literal\tkek\t" + string(kek.raw), want: "KEK (raw)"},
		{line: "kubectl\t...\t--from-literal=kek=" + kek.b64, want: "KEK (base64)"},
		{line: "kubectl\t...\t--from-literal=kek=" + kek.rawHex, want: "KEK (hex)"},
		// Split across two argv elements: no element holds the whole secret,
		// but the first holds the 16-byte half, whose 12-byte prefix must hit.
		{line: "kubectl\t...\t--from-literal=kek=" + string(kek.raw[:16]) + "\t" + string(kek.raw[16:]), want: "KEK (raw) (split prefix)"},
	}
	for _, sentinel := range dbSentinels {
		kubectlLeaks = append(kubectlLeaks, struct {
			line string
			want string
		}{line: "kubectl\t...\t--from-literal=password=" + sentinel, want: "database password"})
	}
	for _, leak := range kubectlLeaks {
		recorder.reset()
		lines := append(append([]string(nil), cleanKubectl...), leak.line)
		assertKubectlArgvCarriesNoCredentials(recorder, lines, kek, true)
		if !recorder.reported(leak.want) {
			t.Errorf("kubectl argv check missed %s: %s", leak.want, leak.line)
		}
	}

	scwLeaks := []struct {
		line string
		want string
	}{
		{line: "scw\t...\tplaintext=" + string(kek.raw), want: "KEK (raw)"},
		{line: "scw\t...\tplaintext=" + kek.b64, want: "KEK (base64)"},
		{line: "scw\t...\tplaintext=@/tmp/kek.abc/plaintext\t" + string(kek.raw), want: "KEK (raw)"},
		// Split base64 across two elements at 20 bytes, mirroring a helper
		// that chops the value before handing it to the key manager.
		{line: "scw\t...\tplaintext=@/tmp/kek.abc/plaintext\t" + kek.b64[:20] + "\t" + kek.b64[20:], want: "KEK (base64) (split prefix)"},
		// At exactly the 12-byte minimum the prefix rule still applies.
		{line: "scw\t...\tplaintext=@/tmp/kek.abc/plaintext\t" + kek.b64[:minimumSplitNeedle] + "\t" + kek.b64[minimumSplitNeedle:], want: "KEK (base64) (split prefix)"},
	}
	for _, leak := range scwLeaks {
		recorder.reset()
		assertKEKAbsentFromArgv(recorder, []string{leak.line}, kek)
		if !recorder.reported(leak.want) {
			t.Errorf("scw argv check missed %s: %s", leak.want, leak.line)
		}
	}
}

// TestBootstrapSecretCleanupTrapsEveryExitPath pins the signal coverage that a
// behavioral test cannot distinguish (bash runs an EXIT trap on SIGTERM too):
// the caller and the scw helper must trap EXIT, INT and TERM in real trap
// commands, and the INT/TERM handlers must exit non-zero after cleanup.
// Comment lines and inline comments are ignored, so a comment that merely
// names the signals, or a handler that returns instead of exiting, cannot
// satisfy the contract.
func TestBootstrapSecretCleanupTrapsEveryExitPath(t *testing.T) {
	for _, script := range []struct {
		relative string
		cleanup  string
	}{
		{relative: filepath.Join("scripts", "bootstrap-secrets"), cleanup: "cleanup_bootstrap_temp_paths"},
		{relative: filepath.Join("scripts", "lib", "scw.sh"), cleanup: "cleanup_scw_kek"},
	} {
		path := filepath.Join(repoRoot(t), script.relative)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", script.relative, err)
		}
		assertTrapCoverage(t, script.relative, string(body), script.cleanup)
	}
}

// trapCommands returns the actual trap commands in body, with comments removed.
func trapCommands(body string) []string {
	var traps []string
	for raw := range strings.SplitSeq(body, "\n") {
		line := strings.TrimSpace(stripShellComment(raw))
		if strings.HasPrefix(line, "trap ") {
			traps = append(traps, line)
		}
	}
	return traps
}

// assertTrapCoverage requires EXIT, INT and TERM coverage, an invocation of
// the script's real cleanup function in each handler, and a non-zero exit
// after that cleanup in the INT and TERM handlers. A handler that merely
// prints the word "cleanup" (`echo cleanup; exit 130`) or calls some other
// function does not satisfy the contract.
func assertTrapCoverage(t *testing.T, name, body, cleanup string) {
	t.Helper()
	traps := trapCommands(body)
	cleanupCall := regexp.MustCompile(`\b` + regexp.QuoteMeta(cleanup) + `\b`)
	cleanupThenExit := regexp.MustCompile(`\b` + regexp.QuoteMeta(cleanup) + `\b\s*;\s*exit\s+[1-9][0-9]*\b`)
	for _, signal := range []string{"EXIT", "INT", "TERM"} {
		expression := regexp.MustCompile(`\b` + signal + `\b`)
		var handlers []string
		for _, trap := range traps {
			if expression.MatchString(trap) {
				handlers = append(handlers, trap)
			}
		}
		if len(handlers) == 0 {
			t.Errorf("%s has no real trap command covering %s cleanup", name, signal)
			continue
		}
		cleans := false
		exits := false
		for _, handler := range handlers {
			if cleanupCall.MatchString(handler) {
				cleans = true
				if cleanupThenExit.MatchString(handler) {
					exits = true
				}
			}
		}
		if !cleans {
			t.Errorf("%s trap for %s does not invoke %s: %v", name, signal, cleanup, handlers)
		}
		if signal != "EXIT" && !exits {
			t.Errorf("%s %s trap handler does not call %s followed by a non-zero exit: %v", name, signal, cleanup, handlers)
		}
	}
}

// stripShellComment removes a trailing shell comment, leaving quoted text
// intact. Only a '#' at the start of the line or after whitespace starts a
// comment, which is enough for these scripts without a full shell parser.
func stripShellComment(line string) string {
	inSingle, inDouble := false, false
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && (index == 0 || line[index-1] == ' ' || line[index-1] == '\t') {
				return line[:index]
			}
		}
	}
	return line
}

var xtracePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\s*set\s+-[A-Za-z]*x[A-Za-z]*(\s|$)`),
	regexp.MustCompile(`^\s*set\s+-o\s+xtrace(\s|$)`),
	regexp.MustCompile(`^\s*set\s+.*\s-o\s+xtrace(\s|$)`),
}

// TestScriptsNeverEnableXtrace is a static guard against a shell xtrace mode:
// `set -x`, `set -o xtrace` or a bundled `set -euxo pipefail` would print the
// KEK and database passwords to stderr. The runtime KEK-absence assertions
// above would then fail as well, but this check names the cause directly and
// covers scripts the component runs do not execute.
func TestScriptsNeverEnableXtrace(t *testing.T) {
	root := filepath.Join(repoRoot(t), "scripts")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for index, line := range strings.Split(string(body), "\n") {
			code := stripShellComment(line)
			for _, pattern := range xtracePatterns {
				if pattern.MatchString(code) {
					t.Errorf("%s:%d enables shell xtrace: %s", path, index+1, strings.TrimSpace(code))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk scripts: %v", err)
	}
}

// TestDeployScriptsPassBashSyntax is a cheap guard that the edited scripts
// remain parseable; the full behavioral tests above exercise the same files.
func TestDeployScriptsPassBashSyntax(t *testing.T) {
	for _, relative := range []string{
		filepath.Join("scripts", "bootstrap-secrets"),
		filepath.Join("scripts", "lib", "scw.sh"),
	} {
		path := filepath.Join(repoRoot(t), relative)
		command := exec.Command("bash", "-n", path)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", relative, err, output)
		}
	}
}

// TestBootstrapSecretsKeepsAWSAndGCPFileForms pins that the cloud helpers that
// already use files keep their file form and inherit the same trap coverage.
func TestBootstrapSecretsKeepsAWSAndGCPFileForms(t *testing.T) {
	root := repoRoot(t)
	for _, relative := range []string{
		filepath.Join("scripts", "lib", "aws.sh"),
		filepath.Join("scripts", "lib", "gcp.sh"),
	} {
		body, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		if !strings.Contains(string(body), "plaintext_file") && !strings.Contains(string(body), "plaintext-file") {
			t.Errorf("%s no longer reads the plaintext from a file", relative)
		}
	}
}
