//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
)

// TestE2E_InstallationAccess is the explicit installation preflight: the four
// configured personas prove their public authorities, the host reads the served
// Git repository over authenticated HTTPS, and one tools-image workload proves
// mounted-CA HTTPS access to the API and Git with the installation's public CA
// bytes in its workspace. It is selected explicitly and is not part of the
// ordinary seven-test selection. In required installation mode every missing
// input fails; it never becomes a skip.
func TestE2E_InstallationAccess(t *testing.T) {
	e2eParallel(t)
	instance := startLive(t)
	missing := instance.config.InstallationMissing()
	if !missing.Empty() {
		if instance.config.RequireInstallation() {
			t.Fatalf("required installation preflight input(s) are missing: %s", missing)
		}
		t.Skipf("unconfigured installation preflight input(s): %s", missing)
	}
	started := time.Now().UTC()

	// Every configured persona is a distinct verified human. The installed
	// server is project-scoped, so membership.list (tenant-scoped) is not a
	// usable preflight read; the public role checks below prove the authority.
	seen := map[string]bool{}
	for _, persona := range harness.Personas {
		view := instance.config.Installation.OAuth.Personas[string(persona)]
		if view.PrincipalID == "" {
			t.Fatalf("%s persona has no derived principal id in the descriptor", persona)
		}
		if seen[view.PrincipalID] {
			t.Fatalf("principal %s is shared by two personas", view.PrincipalID)
		}
		seen[view.PrincipalID] = true
	}

	// Positive authority for each persona, and a negative control that proves
	// the token is not silently an administrator.
	instance.requireAllowed(t, harness.PersonaAdmin, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireAllowed(t, harness.PersonaEngineer, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireAllowed(t, harness.PersonaReviewer, api.MethodGet, "task.inbox",
		api.TaskInboxRequest{ProjectID: instance.projectID})
	instance.requireAllowed(t, harness.PersonaOperator, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireForbidden(t, harness.PersonaEngineer, api.MethodGet, "resource.binding.read",
		api.ResourceBindingReadRequest{BindingID: "w49-preflight-binding"})
	instance.requireForbidden(t, harness.PersonaReviewer, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireForbidden(t, harness.PersonaOperator, api.MethodGet, "pipeline.read",
		api.PipelineReadRequest{ProjectID: instance.projectID, PipelineID: "w49-preflight-missing"})

	// Authenticated host Git read of the served repository.
	repository, ok := instance.config.Repository()
	if !ok {
		t.Fatal("installation descriptor names no Git repository")
	}
	probe, err := harness.NewGitProbe(instance.config.GitURL(), instance.config.GitCAFile(), instance.tokens[harness.PersonaAdmin])
	if err != nil {
		t.Fatalf("build the host Git probe: %v", err)
	}
	readCtx, cancel := context.WithTimeout(instance.ctx, 60*time.Second)
	defer cancel()
	refs, err := probe.ListRefs(readCtx, repository.Name)
	if err != nil {
		t.Fatalf("authenticated host Git read of %s failed: %v", repository.Name, err)
	}
	if !strings.Contains(string(refs), "git-upload-pack") && !strings.Contains(string(refs), "refs/heads/") {
		t.Fatalf("host Git read returned no served refs (bytes=%d)", len(refs))
	}

	// One tools-image workload proves an explicitly declared CA file is enough
	// for verified HTTPS to the API and Git; the execution token is re-read
	// from the mounted secret for each call and never appears in the command.
	// The certificate is public data materialized in the workspace because the
	// test profile's secret-name allow-list is deny-by-default; see
	// docs/runbooks/release-smoke.md.
	ca := instance.materializeCA(t)
	document, err := (harness.Document{
		Actions: []harness.Action{{
			Name:    "installation-access",
			Image:   instance.image,
			Timeout: "5m",
			Env:     ca.environment(),
			Commands: append([]string{"echo w49-preflight-command-start"},
				ca.commands(preflightCommand(instance.projectID, instance.config.GitWorkloadURL(), repository.Name, ca.path)...)...),
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the installation preflight pipeline: %v", err)
	}
	admitted := instance.startInline(t, document, instance.key("installation-access"))
	view := instance.waitComplete(t, admitted.ExecutionID)
	if status, err := harness.Interpret(view); err != nil || !status.Succeeded {
		dumpFailedActionLogs(t, instance, view)
		t.Fatalf("installation preflight execution %s state = %q (failure=%+v)", view.ExecutionID, view.State, view.Failure)
	}
	action := actionByName(t, view, "installation-access")
	log := instance.readLogs(t, admitted.ExecutionID, action.ActionID, "stdout")
	if err := harness.ScanCredentials(string(log), append(instance.config.PersonaSecrets(), instance.token)...); err != nil {
		t.Fatalf("installation preflight log: %v", err)
	}
	requireLogContains(t, "installation-access", log, "w49-preflight-api=200")
	requireLogContains(t, "installation-access", log, "w49-preflight-git=ok")

	finished := time.Now().UTC()
	instance.recordScenario(t, "InstallationAccess", "TestE2E_InstallationAccess", &view, started, finished, map[string]any{
		"resource_id": repository.ResourceID,
	})
}

// requireAllowed asserts a persona's legitimate public operation succeeds.
func (instance *live) requireAllowed(t *testing.T, persona harness.Persona, method api.Method, operation string, input any) api.Result {
	t.Helper()
	token, ok := instance.tokens[persona]
	if !ok {
		t.Fatalf("%s persona credential is not configured", persona)
	}
	result, err := instance.invoke(instance.ctx, token, method, operation, input, "", nil)
	if err != nil {
		t.Fatalf("%s %s was refused: %v", persona, operation, err)
	}
	return result
}

// requireForbidden asserts an authenticated persona without the role is
// refused with the public forbidden code, not a transport or input failure.
func (instance *live) requireForbidden(t *testing.T, persona harness.Persona, method api.Method, operation string, input any) {
	t.Helper()
	token, ok := instance.tokens[persona]
	if !ok {
		t.Fatalf("%s persona credential is not configured", persona)
	}
	_, err := instance.invoke(instance.ctx, token, method, operation, input, "", nil)
	var refusal *harness.APIError
	if !errors.As(err, &refusal) || refusal.Code != api.CodeForbidden {
		t.Fatalf("%s %s = %v; want the public forbidden refusal", persona, operation, err)
	}
}

// dumpFailedActionLogs renders the retained streams of a failed preflight so
// the caller sees the exact workload refusal; it scans credentials first.
func dumpFailedActionLogs(t *testing.T, instance *live, view api.ExecutionInspectResult) {
	t.Helper()
	for _, action := range view.Actions {
		for _, stream := range []string{"stdout", "stderr"} {
			log := instance.readLogs(t, view.ExecutionID, action.ActionID, stream)
			if err := harness.ScanCredentials(string(log), append(instance.config.PersonaSecrets(), instance.token)...); err != nil {
				t.Logf("action %s %s was withheld: %v", action.Name, stream, err)
				continue
			}
			t.Logf("action %s %s:\n%s", action.Name, stream, log)
		}
	}
}

// preflightCommand builds the tools-image script. The CA path is the declared
// file the action materializes in its workspace; the execution credential is
// read from its mount for each call.
func preflightCommand(projectID, gitWorkloadURL, repository, caPath string) []string {
	script := fmt.Sprintf(`set -eu
printf 'w49-preflight-script-start\n'
CA=%q
TOKEN=/kodama/secrets/KODAMA_TOKEN
if ! test -r "$CA"; then printf 'w49-preflight: CA file is not readable\n' >&2; exit 1; fi
if ! test -r "$TOKEN"; then printf 'w49-preflight: execution token is not readable\n' >&2; exit 1; fi
export SSL_CERT_FILE="$CA" CURL_CA_BUNDLE="$CA" GIT_SSL_CAINFO="$CA"
status="$(printf 'header = "Authorization: Bearer %%s"\n' "$(cat "$TOKEN")" | \
  curl -sS --noproxy '*' --max-time 30 --cacert "$CA" -X GET -K - \
  -H 'Content-Type: application/json' \
  --data '{"input":{}}' -o /workspace/api-probe.json -w '%%{http_code}' \
  "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION")"
if [ "$status" != 200 ]; then
  printf 'w49-preflight: API inspect status %%s\n' "$status" >&2
  cat /workspace/api-probe.json >&2 || true
  exit 1
fi
if ! grep -q '"execution_id"' /workspace/api-probe.json; then
  printf 'w49-preflight: API body has no execution_id\n' >&2
  cat /workspace/api-probe.json >&2
  exit 1
fi
cat > /workspace/git-askpass <<'ASKPASS'
#!/bin/sh
case "$1" in
  *Username*) printf '%%s' kodama ;;
  *) cat /kodama/secrets/KODAMA_TOKEN ;;
esac
ASKPASS
chmod 700 /workspace/git-askpass
# A freshly provisioned repository may have no refs yet; a clean authenticated
# advertisement (exit 0) still proves the HTTPS edge accepted the execution
# credential and served the repository. An unauthenticated request fails here.
if ! GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/workspace/git-askpass \
  git -c credential.helper= -c http.sslCAInfo="$CA" -c http.followRedirects=false \
  ls-remote %q > /workspace/git-refs.txt; then
  printf 'w49-preflight: git ls-remote failed\n' >&2
  exit 1
fi
printf 'w49-preflight-api=%%s\n' "$status"
printf 'w49-preflight-refs=%%s\n' "$(wc -l < /workspace/git-refs.txt)"
printf 'w49-preflight-git=ok\n'
`, caPath, strings.TrimSuffix(gitWorkloadURL, "/")+"/"+repository)
	return []string{
		"cat > /workspace/preflight.sh <<'W49_PREFLIGHT'\n" + script + "W49_PREFLIGHT",
		"sh /workspace/preflight.sh",
	}
}
