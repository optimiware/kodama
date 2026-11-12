//go:build component

// Scripted-login checks for scripts/oauthsonas-token. The script signs in to a
// development OAuthSonas as one persona and prints only the access token.
//
// The tests start the real bash script with fake curl and jq wrappers on PATH.
// The wrappers record their argv and then speak a small fake OAuthSonas (gate
// page, authorization redirect, token endpoint). The recorded argv is then
// asserted against every secret sentinel: the picker access code, the
// authorization code and the returned tokens must never be arguments, which
// would expose them through /proc/<pid>/cmdline and `ps`. The fake token
// endpoint returns an id token and a refresh token too, so a script that
// prints more than the access token fails here.
//
// The harness style follows test/component/deploy/secrets_component_test.go:
// a test-owned temporary directory, a per-run PATH of recorders, and a real
// openssl from the host (only for PKCE material; no secret reaches it).
package deploy

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	oauthTokenAccessCode = "OAUTHSONAS-ACCESS-CODE-SENTINEL-7f3a"
	oauthTokenAuthCode   = "AUTH-CODE-SENTINEL-91bd"
	oauthTokenAccess     = "ACCESS-TOKEN-SENTINEL-4c2e"
	oauthTokenID         = "ID-TOKEN-SENTINEL-8a1f"
	oauthTokenRefresh    = "REFRESH-TOKEN-SENTINEL-2d6b"
	oauthTokenBody       = "BODY-SECRET-SENTINEL-5e0c"
)

// fakeOAuthSonasCurl records its argv, then answers the authorization flow.
// Secret material is only accepted through `name@file` data arguments (the
// access code, the authorization code and the PKCE verifier), which is how a
// script keeps it out of argv.
const fakeOAuthSonasCurl = `#!/usr/bin/env bash
set -u
{
  printf 'curl'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$CURL_ARGV_LOG"

out=""
dump=""
url=""
get=0
declare -a data=()
while (($#)); do
  case "$1" in
    -o) out="${2:-}"; shift 2 ;;
    -D) dump="${2:-}"; shift 2 ;;
    -b|-c|--cacert|--resolve|--max-time|--connect-timeout|--retry|-H|--header) shift 2 ;;
    -G) get=1; shift ;;
    -d|--data|--data-raw|--data-binary|--data-urlencode) data+=("${2:-}"); shift 2 ;;
    -s|-S|-sS|-f|--silent|--show-error|--fail) shift ;;
    -*) shift ;;
    *) url="${1:-}"; shift ;;
  esac
done

scheme_stripped="${url#*://}"
case "$scheme_stripped" in
  */*) path="/${scheme_stripped#*/}" ;;
  *) path="/" ;;
esac
path="${path%%\?*}"

declare -A params=()
for item in "${data[@]:-}"; do
  name=""
  value=""
  source=""
  if [[ "$item" == *=* ]]; then
    name="${item%%=*}"
    rest="${item#*=}"
    if [[ "$rest" == @* ]]; then source="${rest#@}"; else value="$rest"; fi
  elif [[ "$item" == *@* ]]; then
    name="${item%%@*}"
    source="${item#*@}"
  else
    continue
  fi
  if [[ -n "$source" ]]; then
    if [[ "$source" == "-" ]]; then
      value="$(cat)"
    else
      value="$(cat -- "$source" 2>/dev/null || true)"
    fi
  fi
  params["$name"]="$value"
done

write_headers() {
  local status="$1"; shift
  [[ -n "$dump" ]] || return 0
  if [[ "$dump" == "-" ]]; then
    printf 'HTTP/1.1 %s\r\n' "$status"
    printf '%s\r\n' "$@"
    printf '\r\n'
  else
    { printf 'HTTP/1.1 %s\r\n' "$status"; printf '%s\r\n' "$@"; printf '\r\n'; } >> "$dump"
  fi
}

emit_body() {
  local body="$1"
  if [[ -z "$out" || "$out" == "-" ]]; then
    printf '%s' "$body"
  elif [[ "$out" != "/dev/null" ]]; then
    printf '%s' "$body" > "$out"
  fi
}

case "$path" in
  /oauth2/auth)
    count_file="$CURL_STATE_DIR/auth-count"
    count="$(cat "$count_file" 2>/dev/null || printf '0')"
    printf '%s' "$((count + 1))" > "$count_file"
    if [[ "${FAKE_OAUTHSONAS_GATE:-0}" == "1" && "$count" -eq 0 ]]; then
      emit_body "<html><body><form method=\"post\" action=\"/oauth2/gate\"><input type=\"hidden\" name=\"return_to\" value=\"${params[state]:-}\"/></form></body></html>"
      write_headers "200 OK" "Content-Type: text/html"
    else
      if [[ "${FAKE_OAUTHSONAS_NO_REDIRECT:-0}" == "1" ]]; then
        emit_body "<html><body>no redirect</body></html>"
        write_headers "200 OK" "Content-Type: text/html"
        exit 0
      fi
      redirect="${params[redirect_uri]:-http://127.0.0.1:52790/callback}"
      state="${params[state]:-}"
      if [[ "${FAKE_OAUTHSONAS_BAD_STATE:-0}" == "1" ]]; then state="wrong-state"; fi
      write_headers "302 Found" "Location: ${redirect}?code=${FAKE_OAUTHSONAS_AUTH_CODE:-AUTH-CODE-SENTINEL-91bd}&state=${state}"
      emit_body ""
    fi
    ;;
  /oauth2/gate)
    expected="$(cat "$FAKE_OAUTHSONAS_EXPECTED_CODE_FILE" 2>/dev/null || true)"
    if [[ -n "$expected" && "${params[code]:-}" == "$expected" ]]; then
      printf 'ok\n' > "$CURL_STATE_DIR/gate-ok"
      write_headers "200 OK"
      emit_body ""
    else
      write_headers "403 Forbidden" "Content-Type: application/json"
      emit_body '{"error":"invalid_access_code","error_description":"'"${FAKE_OAUTHSONAS_BODY_SENTINEL}"'"}'
    fi
    ;;
  /oauth2/token)
    if [[ "${FAKE_OAUTHSONAS_TOKEN_FAIL:-0}" == "1" ]]; then
      write_headers "400 Bad Request" "Content-Type: application/json"
      emit_body '{"error":"invalid_grant","error_description":"'"${FAKE_OAUTHSONAS_BODY_SENTINEL}"'"}'
    else
      write_headers "200 OK" "Content-Type: application/json"
      emit_body '{"access_token":"'"${FAKE_OAUTHSONAS_ACCESS_TOKEN:-ACCESS-TOKEN-SENTINEL-4c2e}"'","token_type":"Bearer","expires_in":3600,"id_token":"'"${FAKE_OAUTHSONAS_ID_TOKEN:-ID-TOKEN-SENTINEL-8a1f}"'","refresh_token":"'"${FAKE_OAUTHSONAS_REFRESH_TOKEN:-REFRESH-TOKEN-SENTINEL-2d6b}"'"}'
    fi
    ;;
  *)
    write_headers "404 Not Found"
    emit_body "not found"
    ;;
esac
exit 0
`

// fakeOAuthSonasJQ records its argv, then runs the real jq. The token reply is
// fed on stdin, so a script that puts the JSON on jq's argv is visible here.
const fakeOAuthSonasJQ = `#!/usr/bin/env bash
set -u
{
  printf 'jq'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} >> "$JQ_ARGV_LOG"
exec "${REAL_JQ:?REAL_JQ must be set}" "$@"
`

// oauthTokenSandbox is one isolated scripts/oauthsonas-token run.
type oauthTokenSandbox struct {
	root      string
	binDir    string
	tempDir   string
	recordDir string
	stateDir  string
	codeFile  string
	argvLog   string
	jqLog     string
	env       []string
}

// newOAuthTokenSandbox prepares the fake PATH and the operator's access-code
// file (0600).
func newOAuthTokenSandbox(t *testing.T) *oauthTokenSandbox {
	t.Helper()
	realJQ, err := exec.LookPath("jq")
	if err != nil {
		t.Fatalf("the component test needs a real jq on PATH: %v", err)
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Fatalf("the component test needs a real openssl on PATH: %v", err)
	}
	root := t.TempDir()
	box := &oauthTokenSandbox{
		root:      root,
		binDir:    filepath.Join(root, "bin"),
		tempDir:   filepath.Join(root, "tmp"),
		recordDir: filepath.Join(root, "records"),
		stateDir:  filepath.Join(root, "state"),
		codeFile:  filepath.Join(root, "oauthsonas-access-code"),
		argvLog:   filepath.Join(root, "records", "curl.argv"),
		jqLog:     filepath.Join(root, "records", "jq.argv"),
	}
	for _, dir := range []string{box.binDir, box.tempDir, box.recordDir, box.stateDir, filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeExecutable(t, filepath.Join(box.binDir, "curl"), fakeOAuthSonasCurl)
	writeExecutable(t, filepath.Join(box.binDir, "jq"), fakeOAuthSonasJQ)
	box.writeCodeFile(t, oauthTokenAccessCode, 0o600)

	expected := filepath.Join(root, "expected-code")
	if err := os.WriteFile(expected, []byte(oauthTokenAccessCode), 0o600); err != nil {
		t.Fatalf("write expected access code: %v", err)
	}

	box.env = []string{
		"PATH=" + box.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + filepath.Join(root, "home"),
		"TMPDIR=" + box.tempDir,
		"LANG=C",
		"CURL_ARGV_LOG=" + box.argvLog,
		"JQ_ARGV_LOG=" + box.jqLog,
		"CURL_STATE_DIR=" + box.stateDir,
		"REAL_JQ=" + realJQ,
		"FAKE_OAUTHSONAS_EXPECTED_CODE_FILE=" + expected,
		"FAKE_OAUTHSONAS_GATE=1",
		"FAKE_OAUTHSONAS_AUTH_CODE=" + oauthTokenAuthCode,
		"FAKE_OAUTHSONAS_ACCESS_TOKEN=" + oauthTokenAccess,
		"FAKE_OAUTHSONAS_ID_TOKEN=" + oauthTokenID,
		"FAKE_OAUTHSONAS_REFRESH_TOKEN=" + oauthTokenRefresh,
		"FAKE_OAUTHSONAS_BODY_SENTINEL=" + oauthTokenBody,
		"OAUTHSONAS_URL=https://oauthsonas.kodama-system.svc.cluster.local:8443",
		"OAUTHSONAS_ACCESS_CODE_FILE=" + box.codeFile,
	}
	return box
}

// writeCodeFile writes the operator's access-code file with the given mode.
func (b *oauthTokenSandbox) writeCodeFile(t *testing.T, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(b.codeFile, []byte(content), mode); err != nil {
		t.Fatalf("write access code file: %v", err)
	}
	if err := os.Chmod(b.codeFile, mode); err != nil {
		t.Fatalf("chmod access code file: %v", err)
	}
}

// run starts the real script with the sandbox environment plus overrides.
func (b *oauthTokenSandbox) run(t *testing.T, argv []string, stdin string, overrides map[string]string) (int, string, string) {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "oauthsonas-token")
	command := exec.Command("bash", append([]string{script}, argv...)...)
	env := append([]string(nil), b.env...)
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	command.Env = env
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run oauthsonas-token: %v", err)
		}
		code = exitError.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// recordedArgv returns every argv line recorded by a fake wrapper.
func (b *oauthTokenSandbox) recordedArgv(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read argv log %s: %v", path, err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertNoSecretInArgv fails when any recorded wrapper argument contains a
// secret sentinel. This is the "never argv" invariant.
func (b *oauthTokenSandbox) assertNoSecretInArgv(t *testing.T) {
	t.Helper()
	sentinels := []string{
		oauthTokenAccessCode,
		oauthTokenAuthCode,
		oauthTokenAccess,
		oauthTokenID,
		oauthTokenRefresh,
		oauthTokenBody,
	}
	for _, logPath := range []string{b.argvLog, b.jqLog} {
		for _, line := range b.recordedArgv(t, logPath) {
			for _, sentinel := range sentinels {
				if strings.Contains(line, sentinel) {
					t.Errorf("%s recorded a secret in argv: %s", filepath.Base(logPath), line)
				}
			}
		}
	}
}

// assertNoAccessCodeInTempDir proves the script does not leave the access code
// in its private temporary directory.
func (b *oauthTokenSandbox) assertNoAccessCodeInTempDir(t *testing.T) {
	t.Helper()
	err := filepath.WalkDir(b.tempDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if strings.Contains(string(body), oauthTokenAccessCode) {
			t.Errorf("%s still contains the access code after the run", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk temp dir: %v", err)
	}
}

// TestOAuthSonasTokenPrintsOnlyTheAccessToken pins the happy path: the gate
// accepts the code read from a 0600 file, and stdout is exactly the access
// token, never the id token, refresh token or JSON envelope.
func TestOAuthSonasTokenPrintsOnlyTheAccessToken(t *testing.T) {
	box := newOAuthTokenSandbox(t)
	code, stdout, stderr := box.run(t, []string{"tenant-a-admin"}, "", nil)
	if code != 0 {
		t.Fatalf("oauthsonas-token failed: exit %d\nstderr:\n%s", code, stderr)
	}
	if stdout != oauthTokenAccess+"\n" {
		t.Errorf("stdout is %q, want only the access token", stdout)
	}
	for _, forbidden := range []string{oauthTokenID, oauthTokenRefresh, "token_type", "expires_in"} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("stdout contains %q; only the access token may be printed", forbidden)
		}
		if strings.Contains(stderr, forbidden) {
			t.Errorf("stderr contains %q", forbidden)
		}
	}
	if _, err := os.Stat(filepath.Join(box.stateDir, "gate-ok")); err != nil {
		t.Fatalf("the fake gate never accepted the access code: %v", err)
	}
	argv := strings.Join(box.recordedArgv(t, box.argvLog), "\n")
	if !strings.Contains(argv, "code@") {
		t.Errorf("the access code was not passed by file reference:\n%s", argv)
	}
	// The request flags are part of the contract: a bounded timeout, no
	// insecure TLS shortcut, and the PKCE S256 challenge.
	for _, want := range []string{"--max-time\t20", "code_challenge_method=S256", "code_challenge="} {
		if !strings.Contains(argv, want) {
			t.Errorf("curl argv does not carry %q:\n%s", want, argv)
		}
	}
	for _, forbidden := range []string{"\t-k\t", "\t--insecure\t", "verifier="} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("curl argv carries %q:\n%s", forbidden, argv)
		}
	}
	box.assertNoSecretInArgv(t)
	box.assertNoAccessCodeInTempDir(t)
}

// TestOAuthSonasTokenPassesCAAndResolveToCurl pins the operator-supplied TLS
// trust and port-forward resolution: both reach curl as their own flags and
// never as part of the URL.
func TestOAuthSonasTokenPassesCAAndResolveToCurl(t *testing.T) {
	box := newOAuthTokenSandbox(t)
	code, stdout, stderr := box.run(t, []string{"tenant-a-admin"}, "", map[string]string{
		"OAUTHSONAS_CA":      "/etc/kodama/oidc-ca/ca.crt",
		"OAUTHSONAS_RESOLVE": "oauthsonas.kodama-system.svc.cluster.local:8443:127.0.0.1",
	})
	if code != 0 {
		t.Fatalf("oauthsonas-token failed: exit %d\nstderr:\n%s", code, stderr)
	}
	if stdout != oauthTokenAccess+"\n" {
		t.Errorf("stdout is %q, want only the access token", stdout)
	}
	argv := strings.Join(box.recordedArgv(t, box.argvLog), "\n")
	for _, want := range []string{
		"--cacert\t/etc/kodama/oidc-ca/ca.crt",
		"--resolve\toauthsonas.kodama-system.svc.cluster.local:8443:127.0.0.1",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("curl argv does not carry %q:\n%s", want, argv)
		}
	}
	box.assertNoSecretInArgv(t)
}

// TestOAuthSonasTokenReadsAccessCodeFromStdin pins the stdin source: the code
// arrives on stdin, never as an argument.
func TestOAuthSonasTokenReadsAccessCodeFromStdin(t *testing.T) {
	box := newOAuthTokenSandbox(t)
	code, stdout, stderr := box.run(t, []string{"tenant-a-admin"}, oauthTokenAccessCode+"\n",
		map[string]string{"OAUTHSONAS_ACCESS_CODE_FILE": "-"})
	if code != 0 {
		t.Fatalf("oauthsonas-token failed: exit %d\nstderr:\n%s", code, stderr)
	}
	if stdout != oauthTokenAccess+"\n" {
		t.Errorf("stdout is %q, want only the access token", stdout)
	}
	box.assertNoSecretInArgv(t)
	box.assertNoAccessCodeInTempDir(t)
}

// TestOAuthSonasTokenRefusesInsecureAccessCodeSources pins the custody rules:
// the legacy environment variable is refused, the file must be 0600, and the
// file must exist and be non-empty.
func TestOAuthSonasTokenRefusesInsecureAccessCodeSources(t *testing.T) {
	t.Run("legacy environment variable", func(t *testing.T) {
		box := newOAuthTokenSandbox(t)
		code, stdout, stderr := box.run(t, []string{"tenant-a-admin"}, "", map[string]string{
			"OAUTHSONAS_ACCESS_CODE_FILE": "",
			"OAUTHSONAS_ACCESS_CODE":      oauthTokenAccessCode,
		})
		if code == 0 {
			t.Fatalf("script accepted OAUTHSONAS_ACCESS_CODE in the environment")
		}
		if stdout != "" {
			t.Errorf("failed run printed to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "refusing OAUTHSONAS_ACCESS_CODE in the environment") {
			t.Errorf("refusal does not name the rejected environment source:\n%s", stderr)
		}
		if !strings.Contains(stderr, "OAUTHSONAS_ACCESS_CODE_FILE") {
			t.Errorf("refusal does not tell the operator to use a file or stdin:\n%s", stderr)
		}
		box.assertNoSecretInArgv(t)
	})
	t.Run("world-readable file", func(t *testing.T) {
		box := newOAuthTokenSandbox(t)
		box.writeCodeFile(t, oauthTokenAccessCode, 0o644)
		code, stdout, stderr := box.run(t, []string{"tenant-a-admin"}, "", nil)
		if code == 0 {
			t.Fatalf("script accepted a 0644 access-code file")
		}
		if stdout != "" {
			t.Errorf("failed run printed to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "0600") {
			t.Errorf("refusal does not require mode 0600:\n%s", stderr)
		}
		box.assertNoSecretInArgv(t)
	})
	t.Run("missing file", func(t *testing.T) {
		box := newOAuthTokenSandbox(t)
		code, _, stderr := box.run(t, []string{"tenant-a-admin"}, "", map[string]string{
			"OAUTHSONAS_ACCESS_CODE_FILE": filepath.Join(box.root, "no-such-file"),
		})
		if code == 0 {
			t.Fatalf("script accepted a missing access-code file")
		}
		if !strings.Contains(stderr, "regular file") {
			t.Errorf("refusal does not name the file problem:\n%s", stderr)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		box := newOAuthTokenSandbox(t)
		box.writeCodeFile(t, "", 0o600)
		code, _, stderr := box.run(t, []string{"tenant-a-admin"}, "", nil)
		if code == 0 {
			t.Fatalf("script accepted an empty access-code file")
		}
		if !strings.Contains(stderr, "empty") {
			t.Errorf("refusal does not name the empty file:\n%s", stderr)
		}
	})
	t.Run("access code as extra argument", func(t *testing.T) {
		box := newOAuthTokenSandbox(t)
		code, stdout, stderr := box.run(t, []string{"tenant-a-admin", oauthTokenAccessCode}, "", nil)
		if code == 0 {
			t.Fatalf("script accepted the access code as an extra argument")
		}
		if stdout != "" {
			t.Errorf("failed run printed to stdout: %q", stdout)
		}
		if strings.Contains(stderr, oauthTokenAccessCode) {
			t.Errorf("refusal echoed the access code on stderr")
		}
		box.assertNoSecretInArgv(t)
	})
}

// TestOAuthSonasTokenRefusesBadPersona pins the principal-id grammar. A
// persona such as "oauthsonas|admin" is not a principal id Kodama can parse,
// and a malformed persona must not reach the provider.
func TestOAuthSonasTokenRefusesBadPersona(t *testing.T) {
	for index, persona := range []string{"oauthsonas|admin", "admin space", "admin/admin", "admin:tenant", ""} {
		t.Run(fmt.Sprintf("%d_%s", index, strings.ReplaceAll(persona, " ", "_")), func(t *testing.T) {
			box := newOAuthTokenSandbox(t)
			code, stdout, stderr := box.run(t, []string{persona}, "", nil)
			if code == 0 {
				t.Fatalf("script accepted persona %q", persona)
			}
			if stdout != "" {
				t.Errorf("failed run printed to stdout: %q", stdout)
			}
			if lines := box.recordedArgv(t, box.argvLog); len(lines) != 0 {
				t.Errorf("script contacted the provider with persona %q:\n%s", persona, strings.Join(lines, "\n"))
			}
			if persona != "" && strings.Contains(stderr, persona) && strings.Contains(persona, "|") {
				// Naming the rejected persona is acceptable; leaking a secret is not.
				t.Logf("refusal names the persona: %s", stderr)
			}
		})
	}
}

// TestOAuthSonasTokenRefusesHTTPIssuer pins the https-only issuer: Kodama
// accepts only https issuers, so the script never sends a token request to an
// http:// URL.
func TestOAuthSonasTokenRefusesHTTPIssuer(t *testing.T) {
	box := newOAuthTokenSandbox(t)
	code, stdout, stderr := box.run(t, []string{"tenant-a-admin"}, "", map[string]string{
		"OAUTHSONAS_URL": "http://oauthsonas.kodama-system.svc.cluster.local:8443",
	})
	if code == 0 {
		t.Fatalf("script accepted an http issuer")
	}
	if stdout != "" {
		t.Errorf("failed run printed to stdout: %q", stdout)
	}
	if !strings.Contains(strings.ToLower(stderr), "https") {
		t.Errorf("refusal does not require https:\n%s", stderr)
	}
	if lines := box.recordedArgv(t, box.argvLog); len(lines) != 0 {
		t.Errorf("script contacted an http issuer:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOAuthSonasTokenFailureDoesNotEchoResponseBody pins the failure path: a
// token endpoint error exits non-zero, prints nothing on stdout and never
// echoes the response body (which may describe the grant or the client).
func TestOAuthSonasTokenFailureDoesNotEchoResponseBody(t *testing.T) {
	box := newOAuthTokenSandbox(t)
	code, stdout, stderr := box.run(t, []string{"tenant-a-admin"}, "", map[string]string{
		"FAKE_OAUTHSONAS_TOKEN_FAIL": "1",
	})
	if code == 0 {
		t.Fatalf("script treated a token endpoint error as success")
	}
	if stdout != "" {
		t.Errorf("failed run printed to stdout: %q", stdout)
	}
	for _, forbidden := range []string{oauthTokenBody, "invalid_grant", "error_description"} {
		if strings.Contains(stderr, forbidden) {
			t.Errorf("stderr echoes the response body (%q):\n%s", forbidden, stderr)
		}
	}
	if !strings.Contains(stderr, "token exchange failed") {
		t.Errorf("stderr does not carry the generic failure message:\n%s", stderr)
	}
	box.assertNoSecretInArgv(t)
}

// TestOAuthSonasTokenRejectsStateMismatch pins the authorization-response
// state check: a redirect for another request must not yield a token.
func TestOAuthSonasTokenRejectsStateMismatch(t *testing.T) {
	box := newOAuthTokenSandbox(t)
	code, stdout, _ := box.run(t, []string{"tenant-a-admin"}, "", map[string]string{
		"FAKE_OAUTHSONAS_BAD_STATE": "1",
	})
	if code == 0 {
		t.Fatalf("script accepted an authorization response with a mismatched state")
	}
	if stdout != "" {
		t.Errorf("failed run printed to stdout: %q", stdout)
	}
}

// TestOAuthSonasTokenFailsWithoutRedirect pins the wrong-persona path: no
// authorization redirect means no token and a non-zero exit.
func TestOAuthSonasTokenFailsWithoutRedirect(t *testing.T) {
	box := newOAuthTokenSandbox(t)
	code, stdout, _ := box.run(t, []string{"tenant-a-admin"}, "", map[string]string{
		"FAKE_OAUTHSONAS_GATE":        "0",
		"FAKE_OAUTHSONAS_NO_REDIRECT": "1",
	})
	if code == 0 {
		t.Fatalf("script treated a missing redirect as success")
	}
	if stdout != "" {
		t.Errorf("failed run printed to stdout: %q", stdout)
	}
}

// TestOAuthSonasTokenNamesMissingAccessCodeSource pins the gate path when no
// source is configured.
func TestOAuthSonasTokenNamesMissingAccessCodeSource(t *testing.T) {
	box := newOAuthTokenSandbox(t)
	code, _, stderr := box.run(t, []string{"tenant-a-admin"}, "", map[string]string{
		"OAUTHSONAS_ACCESS_CODE_FILE": "",
	})
	if code == 0 {
		t.Fatalf("script returned a token without an access code")
	}
	if !strings.Contains(stderr, "OAUTHSONAS_ACCESS_CODE_FILE") {
		t.Errorf("refusal does not name the missing source:\n%s", stderr)
	}
}
