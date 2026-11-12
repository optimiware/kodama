//go:build e2e

package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stagePersonaGrant is one membership.grant request the fake public API saw.
type stagePersonaGrant struct {
	Method    string
	Path      string
	Headers   string
	Body      string
	Expected  *uint64
	Roles     []string
	Project   string
	Principal string
}

type stagePersonasRun struct {
	output      string
	err         error
	curlLog     string
	flagsLog    string
	grants      *[]stagePersonaGrant
	evidenceDir string
	tokenValue  string
}

// TestStagePersonasRefusesUnsafeInput proves the bootstrap refuses a non-https
// API, a foreign host, a missing or non-private token file, a token supplied in
// argv or the environment, and an absent issuer/CA before any request starts.
func TestStagePersonasRefusesUnsafeInput(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		args      []string
		mutate    func(t *testing.T, setup *stagePersonasSetup)
	}{
		{name: "http api", overrides: map[string]string{"KODAMA_STAGE_API_URL": "http://127.0.0.1:18443"}},
		{name: "foreign host", overrides: map[string]string{"KODAMA_STAGE_API_URL": "https://api.example.com"}},
		{name: "user information", overrides: map[string]string{"KODAMA_STAGE_API_URL": "https://user:pass@127.0.0.1:18443"}},
		{name: "missing token file", overrides: map[string]string{"KODAMA_STAGE_ADMIN_TOKEN_FILE": "/nonexistent/stage-admin-token"}},
		{name: "token in environment", overrides: map[string]string{"KODAMA_STAGE_ADMIN_TOKEN": "supplied-in-environment"}},
		{name: "token in argv", args: []string{"supplied-in-argv"}},
		{name: "missing issuer", overrides: map[string]string{"KODAMA_STAGE_ISSUER": ""}},
		{name: "missing ca file", overrides: map[string]string{"KODAMA_STAGE_CA_FILE": "/nonexistent/ca.pem"}},
		{
			name: "token file is not 0600",
			mutate: func(t *testing.T, setup *stagePersonasSetup) {
				if err := os.Chmod(setup.tokenFile, 0o644); err != nil {
					t.Fatalf("chmod token file: %v", err)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			run := runStagePersonas(t, testCase.overrides, testCase.args, testCase.mutate)
			if run.err == nil {
				t.Fatalf("stage-personas accepted unsafe input %q:\n%s", testCase.name, run.output)
			}
			if data, err := os.ReadFile(run.curlLog); err == nil && len(data) != 0 {
				t.Fatalf("stage-personas started a request before refusing input %q:\n%s", testCase.name, data)
			}
		})
	}
}

// TestStagePersonasGrantsExactRoles pins the authority-sensitive role table. Any
// role added, removed or swapped for any of the four personas makes this fail.
func TestStagePersonasGrantsExactRoles(t *testing.T) {
	issuer := "https://oauthsonas.kodama-system.svc.cluster.local:8443"
	run := runStagePersonas(t, map[string]string{
		"KODAMA_STAGE_ISSUER":        issuer,
		"FAKE_CURL_PROJECT_CONFLICT": "1",
	}, nil, func(*testing.T, *stagePersonasSetup) {})
	if run.err != nil {
		t.Fatalf("stage-personas failed: %v\n%s", run.err, run.output)
	}
	if strings.Contains(run.output, run.tokenValue) {
		t.Fatal("stage-personas printed the administrator token")
	}
	if data, err := os.ReadFile(run.curlLog); err != nil {
		t.Fatalf("read fake-curl invocation log: %v", err)
	} else if strings.Contains(string(data), run.tokenValue) {
		t.Fatal("the administrator token appeared in a curl argument vector")
	}
	if data, err := os.ReadFile(run.flagsLog); err != nil {
		t.Fatalf("read fake-curl header log: %v", err)
	} else {
		if strings.Contains(string(data), "header-inline yes") {
			t.Fatal("stage-personas passed an inline credential header")
		}
		if !strings.Contains(string(data), "auth-bearer yes") {
			t.Fatal("stage-personas did not send the bearer token from a private header file")
		}
	}

	want := []struct {
		principal string
		roles     []string
		update    bool
	}{
		{principal: expectedStagePrincipal(issuer, "admin"), roles: []string{"engineer", "reviewer", "operator"}, update: true},
		{principal: expectedStagePrincipal(issuer, "engineer"), roles: []string{"engineer"}},
		{principal: expectedStagePrincipal(issuer, "reviewer"), roles: []string{"reviewer"}},
		{principal: expectedStagePrincipal(issuer, "operator"), roles: []string{"operator"}},
	}
	grants := *run.grants
	if len(grants) != len(want) {
		t.Fatalf("stage-personas sent %d membership.grant request(s); want %d:\n%s", len(grants), len(want), run.output)
	}
	for index, expected := range want {
		got := grants[index]
		if got.Method != "PUT" {
			t.Fatalf("grant %d used %s; want PUT", index, got.Method)
		}
		if got.Path != "/api/v1/projects/tenant-a/memberships/"+expected.principal {
			t.Fatalf("grant %d path = %q; want the derived principal %q", index, got.Path, expected.principal)
		}
		if got.Project != "tenant-a" || got.Principal != expected.principal {
			t.Fatalf("grant %d input = project %q principal %q; want tenant-a/%s", index, got.Project, got.Principal, expected.principal)
		}
		if strings.Join(got.Roles, ",") != strings.Join(expected.roles, ",") {
			t.Fatalf("grant %d roles = %v; want exactly %v", index, got.Roles, expected.roles)
		}
		if expected.update {
			if got.Expected == nil || *got.Expected != 4 {
				t.Fatalf("update grant for %s sent expected_version %v; want the observed 4", expected.principal, got.Expected)
			}
		} else if got.Expected != nil {
			t.Fatalf("first grant for %s sent expected_version %d; want it omitted", expected.principal, *got.Expected)
		}
	}
}

// TestStagePersonasFailsWhenReadbackOmitsAGrant proves the final membership.list
// read-back is enforced, not merely logged.
func TestStagePersonasFailsWhenReadbackOmitsAGrant(t *testing.T) {
	run := runStagePersonas(t, map[string]string{
		"FAKE_CURL_DROP_PRINCIPAL": expectedStagePrincipal("https://oauthsonas.kodama-system.svc.cluster.local:8443", "operator"),
	}, nil, func(*testing.T, *stagePersonasSetup) {})
	if run.err == nil {
		t.Fatalf("stage-personas accepted a read-back without the operator grant:\n%s", run.output)
	}
	if !strings.Contains(run.output, "operator") {
		t.Fatalf("the read-back failure does not name the missing persona:\n%s", run.output)
	}
}

// TestStagePersonasAcceptsNullMemberships proves a membership.list body whose
// Memberships slice is nil on the wire — encoded as JSON null — is an empty
// membership set, not an unusable response. The stage observed
// {"memberships":null} before the first grant; the bootstrap must proceed to
// grant all four personas.
func TestStagePersonasAcceptsNullMemberships(t *testing.T) {
	run := runStagePersonas(t, map[string]string{
		"FAKE_CURL_NULL_FIRST": "1",
	}, nil, func(t *testing.T, setup *stagePersonasSetup) {
		if err := os.WriteFile(setup.stateFile, nil, 0o600); err != nil {
			t.Fatalf("empty the fake membership state: %v", err)
		}
	})
	if run.err != nil {
		t.Fatalf("stage-personas refused a null memberships body: %v\n%s", run.err, run.output)
	}
	grants := *run.grants
	if len(grants) != 4 {
		t.Fatalf("stage-personas sent %d grant(s) after a null membership list; want 4:\n%s", len(grants), run.output)
	}
	if grants[0].Expected != nil {
		t.Fatalf("the first grant after a null list sent expected_version %d; want it omitted", *grants[0].Expected)
	}
}

// TestStagePersonasRefusesNonListMemberships proves a missing key, an object and
// a string are still refused; only JSON null is the accepted empty set.
func TestStagePersonasRefusesNonListMemberships(t *testing.T) {
	for _, mode := range []string{"missing", "object", "string"} {
		t.Run(mode, func(t *testing.T) {
			run := runStagePersonas(t, map[string]string{
				"FAKE_CURL_MEMBERSHIPS_BAD": mode,
			}, nil, func(*testing.T, *stagePersonasSetup) {})
			if run.err == nil {
				t.Fatalf("stage-personas accepted a %s memberships body:\n%s", mode, run.output)
			}
			if !strings.Contains(run.output, "memberships") {
				t.Fatalf("the %s refusal does not name memberships:\n%s", mode, run.output)
			}
			if len(*run.grants) != 0 {
				t.Fatalf("stage-personas granted memberships after refusing a %s body", mode)
			}
		})
	}
}

// TestStagePersonasWritesPrivateEvidence proves the optional evidence record is
// private and carries no credential material.
func TestStagePersonasWritesPrivateEvidence(t *testing.T) {
	run := runStagePersonas(t, nil, nil, func(*testing.T, *stagePersonasSetup) {})
	if run.err != nil {
		t.Fatalf("stage-personas failed: %v\n%s", run.err, run.output)
	}
	dirInfo, err := os.Stat(run.evidenceDir)
	if err != nil {
		t.Fatalf("stat evidence directory: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("evidence directory mode = %o; want 700", dirInfo.Mode().Perm())
	}
	evidence := filepath.Join(run.evidenceDir, "stage-personas.json")
	info, err := os.Stat(evidence)
	if err != nil {
		t.Fatalf("stat evidence record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence record mode = %o; want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatalf("read evidence record: %v", err)
	}
	if strings.Contains(string(data), run.tokenValue) {
		t.Fatal("the evidence record contains the administrator token")
	}
	var record struct {
		ProjectID string `json:"project_id"`
		Grants    []struct {
			PrincipalID string   `json:"principal_id"`
			Roles       []string `json:"roles"`
			Version     uint64   `json:"version"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("evidence record is not JSON: %v", err)
	}
	if record.ProjectID != "tenant-a" || len(record.Grants) != 4 {
		t.Fatalf("evidence record = %+v; want tenant-a and four grants", record)
	}
}

// stagePersonasSetup is one isolated stage-personas fixture.
type stagePersonasSetup struct {
	tokenFile   string
	caFile      string
	evidenceDir string
	stateFile   string
}

const stagePersonasToken = "stage-admin-secret-token"

// runStagePersonas drives scripts/stage-personas against a fake curl. No
// cluster or network is contacted. memberships override the fake's initial
// membership set; overrides are extra environment values.
func runStagePersonas(t *testing.T, overrides map[string]string, args []string, mutate func(*testing.T, *stagePersonasSetup)) stagePersonasRun {
	t.Helper()
	bin := t.TempDir()
	tempDir := t.TempDir()
	root := t.TempDir()
	setup := stagePersonasSetup{
		tokenFile:   filepath.Join(root, "admin-token"),
		caFile:      filepath.Join(root, "ca.pem"),
		evidenceDir: filepath.Join(root, "evidence"),
		stateFile:   filepath.Join(root, "memberships.tsv"),
	}
	if err := os.WriteFile(setup.tokenFile, []byte(stagePersonasToken+"\n"), 0o600); err != nil {
		t.Fatalf("write fake token file: %v", err)
	}
	if err := os.WriteFile(setup.caFile, []byte("test CA placeholder\n"), 0o600); err != nil {
		t.Fatalf("write fake CA: %v", err)
	}
	curlLog := filepath.Join(root, "curl.log")
	flagsLog := filepath.Join(root, "curl.flags")
	state := setup.stateFile
	grantsLog := filepath.Join(root, "grants.tsv")
	issuer := "https://oauthsonas.kodama-system.svc.cluster.local:8443"
	initial := expectedStagePrincipal(issuer, "admin") + "\t" + `["engineer"]` + "\t4\n"
	if err := os.WriteFile(state, []byte(initial), 0o600); err != nil {
		t.Fatalf("write fake membership state: %v", err)
	}

	writeExecutable(t, filepath.Join(bin, "curl"), `#!/bin/sh
set -eu
log="$FAKE_CURL_LOG"
state="$FAKE_CURL_STATE"
flags="$FAKE_CURL_FLAGS"
grants="$FAKE_CURL_GRANTS"
method=GET
url=""
out=""
body=""
header=""
tab="$(printf '\t')"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --request) method="$2"; shift 2 ;;
    --output) out="$2"; shift 2 ;;
    --write-out) shift 2 ;;
    --cacert|--max-time) shift 2 ;;
    --data-binary)
      value="$2"; shift 2
      case "$value" in
        @*) body="$(cat -- "${value#@}")" ;;
        *) body="$value" ;;
      esac
      ;;
    --header)
      value="$2"; shift 2
      case "$value" in
        @*)
          header="$(cat -- "${value#@}")"
          case "$header" in
            "Authorization: Bearer "*) printf 'auth-bearer yes\n' >> "$flags" ;;
            *) printf 'auth-bearer no\n' >> "$flags" ;;
          esac
          ;;
        *) printf 'header-inline yes\n' >> "$flags" ;;
      esac
      ;;
    --silent|--show-error) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s %s\n' "$method" "$url" >> "$log"
if [ -n "$body" ] && [ "$method" = "PUT" ]; then
  printf '%s\t%s\n' "$url" "$body" >> "$grants"
fi
case "$method $url" in
  "POST https://127.0.0.1:18443/api/v1/tenants/tenant-a/projects")
    if [ "${FAKE_CURL_PROJECT_CONFLICT:-0}" = "1" ]; then
      printf '%s' '{"code":"version_conflict","message":"project already exists"}' >"$out"
      printf 409
    else
      printf '%s' '{"operation":"project.create","body":{"project_id":"tenant-a","version":1}}' >"$out"
      printf 200
    fi
    ;;
  "GET https://127.0.0.1:18443/api/v1/projects/tenant-a/memberships")
    case "${FAKE_CURL_MEMBERSHIPS_BAD:-}" in
      missing)
        printf '%s' '{"operation":"membership.list","body":{}}' >"$out"
        printf 200
        exit 0
        ;;
      object)
        printf '%s' '{"operation":"membership.list","body":{"memberships":{}}}' >"$out"
        printf 200
        exit 0
        ;;
      string)
        printf '%s' '{"operation":"membership.list","body":{"memberships":"none"}}' >"$out"
        printf 200
        exit 0
        ;;
    esac
    if [ "${FAKE_CURL_NULL_FIRST:-0}" = "1" ] && [ ! -f "$state.null-done" ]; then
      : > "$state.null-done"
      printf '%s' '{"operation":"membership.list","body":{"memberships":null}}' >"$out"
      printf 200
      exit 0
    fi
    {
      printf '{"operation":"membership.list","body":{"memberships":['
      first=1
      while IFS="$tab" read -r principal roles version; do
        [ -n "$principal" ] || continue
        [ "$principal" = "${FAKE_CURL_DROP_PRINCIPAL:-}" ] && continue
        if [ "$first" -eq 0 ]; then printf ','; fi
        printf '{"principal_id":"%s","roles":%s,"version":%s}' "$principal" "$roles" "$version"
        first=0
      done < "$state"
      printf ']}}'
    } >"$out"
    printf 200
    ;;
  "PUT https://127.0.0.1:18443/api/v1/projects/tenant-a/memberships/"*)
    principal="${url##*/}"
    roles="$(printf '%s' "$body" | sed -n 's/.*"roles":\(\[[^]]*\]\).*/\1/p')"
    expected="$(printf '%s' "$body" | sed -n 's/.*"expected_version":\([0-9][0-9]*\).*/\1/p')"
    if [ -n "$expected" ]; then version=$((expected + 1)); else version=1; fi
    next="$state.next.$$"
    if [ -f "$state" ]; then
      while IFS= read -r line; do
        case "$line" in
          "$principal$tab"*) continue ;;
        esac
        printf '%s\n' "$line"
      done < "$state" > "$next"
    else
      : > "$next"
    fi
    printf '%s\t%s\t%s\n' "$principal" "$roles" "$version" >> "$next"
    mv -f -- "$next" "$state"
    printf '%s' '{"operation":"membership.grant","body":{"version":2}}' >"$out"
    printf 200
    ;;
  *)
    printf '%s' '{"code":"internal_failure","message":"unexpected fake request"}' >"$out"
    printf 500
    ;;
esac
`)

	script := os.Getenv("Q01_STAGE_PERSONAS_SCRIPT")
	if script == "" {
		script = filepath.Join(repoRootForTest(t), "scripts", "stage-personas")
	}
	environment := map[string]string{
		"PATH":                          bin + ":/usr/local/bin:/usr/bin:/bin",
		"HOME":                          t.TempDir(),
		"TMPDIR":                        tempDir,
		"KODAMA_STAGE_API_URL":          "https://127.0.0.1:18443",
		"KODAMA_STAGE_CA_FILE":          setup.caFile,
		"KODAMA_STAGE_ADMIN_TOKEN_FILE": setup.tokenFile,
		"KODAMA_STAGE_PROJECT_ID":       "tenant-a",
		"KODAMA_STAGE_ISSUER":           issuer,
		"KODAMA_STAGE_EVIDENCE_DIR":     setup.evidenceDir,
		"FAKE_CURL_LOG":                 curlLog,
		"FAKE_CURL_FLAGS":               flagsLog,
		"FAKE_CURL_STATE":               state,
		"FAKE_CURL_GRANTS":              grantsLog,
		"KODAMA_STAGE_ADMIN_TOKEN":      "",
		"KODAMA_STAGE_TOKEN":            "",
	}
	maps.Copy(environment, overrides)
	if mutate != nil {
		mutate(t, &setup)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, script, args...)
	command.Env = make([]string, 0, len(environment))
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()

	grants := []stagePersonaGrant{}
	if data, readErr := os.ReadFile(grantsLog); readErr == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			partsList := strings.SplitN(line, "\t", 2)
			if len(partsList) != 2 {
				continue
			}
			path := strings.TrimPrefix(partsList[0], "https://127.0.0.1:18443")
			var grant struct {
				Input struct {
					ProjectID   string   `json:"project_id"`
					PrincipalID string   `json:"principal_id"`
					Roles       []string `json:"roles"`
				} `json:"input"`
				ExpectedVersion *uint64 `json:"expected_version"`
			}
			if err := json.Unmarshal([]byte(partsList[1]), &grant); err != nil {
				continue
			}
			grants = append(grants, stagePersonaGrant{
				Method:    "PUT",
				Path:      path,
				Body:      partsList[1],
				Expected:  grant.ExpectedVersion,
				Roles:     grant.Input.Roles,
				Project:   grant.Input.ProjectID,
				Principal: grant.Input.PrincipalID,
			})
		}
	}
	return stagePersonasRun{
		output:      string(output),
		err:         err,
		curlLog:     curlLog,
		flagsLog:    flagsLog,
		grants:      &grants,
		evidenceDir: setup.evidenceDir,
		tokenValue:  stagePersonasToken,
	}
}

// expectedStagePrincipal mirrors the product's verified human principal id.
func expectedStagePrincipal(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "oidc-" + hex.EncodeToString(sum[:16])
}
