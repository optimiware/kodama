//go:build qualification

package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// fakeEdgeRequest is one recorded request to a whole-probe fake edge.
type fakeEdgeRequest struct {
	Method          string
	Path            string
	Authorization   string
	Input           json.RawMessage
	ExpectedVersion *uint64
	IdempotencyKey  string
}

// fakeEdge records requests safely for sequential probe tests.
type fakeEdge struct {
	mu       sync.Mutex
	requests []fakeEdgeRequest
}

// record reads and records one request envelope.
func (edge *fakeEdge) record(t *testing.T, request *http.Request) api.Request {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Errorf("read fake-edge request: %v", err)
		return api.Request{}
	}
	var envelope api.Request
	if len(body) > 0 {
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("fake-edge request body is not the frozen envelope: %v", err)
		}
	}
	edge.mu.Lock()
	edge.requests = append(edge.requests, fakeEdgeRequest{
		Method:          request.Method,
		Path:            request.URL.Path,
		Authorization:   request.Header.Get("Authorization"),
		Input:           envelope.Input,
		ExpectedVersion: envelope.ExpectedVersion,
		IdempotencyKey:  envelope.IdempotencyKey,
	})
	edge.mu.Unlock()
	return envelope
}

// recorded returns a copy of the requests seen so far.
func (edge *fakeEdge) recorded() []fakeEdgeRequest {
	edge.mu.Lock()
	defer edge.mu.Unlock()
	return append([]fakeEdgeRequest(nil), edge.requests...)
}

// TestHarnessAPIPutBinding proves the PUT helpers emit the frozen method, path
// and envelope, and that a versioned PUT refuses zero before any request.
func TestHarnessAPIPutBinding(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   api.Request
		requests  int
	)
	server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		gotMethod = request.Method
		gotPath = request.URL.Path
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read PUT body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("PUT body is not the public envelope: %v", err)
			return
		}
		if strings.HasSuffix(request.URL.Path, "/memberships/principal-1") {
			writePublicTestResult(t, writer, "membership.grant", `{"version":2}`)
			return
		}
		writePublicTestResult(t, writer, "pipeline.apply", `{"pipeline_id":"pipe-1","version":4}`)
	}))
	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	if _, err := client.Put(context.Background(), Secret("admin-token-value"), "membership.grant",
		api.MembershipGrantRequest{ProjectID: "tenant-a", PrincipalID: "principal-1", Roles: []string{"viewer"}},
		"key-grant"); err != nil {
		t.Fatalf("membership.grant PUT failed: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/projects/tenant-a/memberships/principal-1" {
		t.Fatalf("membership.grant target = %s %s, want PUT on the frozen route", gotMethod, gotPath)
	}
	if gotBody.ExpectedVersion != nil {
		t.Fatalf("membership.grant expected_version = %v, want absent", gotBody.ExpectedVersion)
	}
	var grantInput api.MembershipGrantRequest
	if err := json.Unmarshal(gotBody.Input, &grantInput); err != nil || grantInput.PrincipalID != "principal-1" {
		t.Fatalf("membership.grant envelope input = %s", gotBody.Input)
	}

	if _, err := client.PutVersioned(context.Background(), Secret("admin-token-value"), "pipeline.apply",
		api.PipelineApplyRequest{ProjectID: "tenant-a", Name: "pipe-1", YAML: []byte("actions: []\n")}, "key-apply", 3); err != nil {
		t.Fatalf("pipeline.apply versioned PUT failed: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/projects/tenant-a/pipelines/pipe-1" {
		t.Fatalf("pipeline.apply target = %s %s, want PUT on the frozen route", gotMethod, gotPath)
	}
	if gotBody.ExpectedVersion == nil || *gotBody.ExpectedVersion != 3 {
		t.Fatalf("pipeline.apply expected_version = %v, want 3", gotBody.ExpectedVersion)
	}

	before := requests
	if _, err := client.PutVersioned(context.Background(), Secret("admin-token-value"), "pipeline.apply",
		api.PipelineApplyRequest{ProjectID: "tenant-a", Name: "pipe-1", YAML: []byte("actions: []\n")}, "key-apply", 0); err == nil {
		t.Fatal("a zero expected version was accepted by PutVersioned")
	}
	if requests != before {
		t.Fatal("a zero expected version reached the endpoint")
	}
}

// TestHarnessRoleAuthorityProbe drives the WHOLE roles probe against a fake
// edge that models the stage's actual principals: one admin token and a
// viewer-service credential created through the public API. It pins the viewer
// service creation/issue, the positive viewer read, the authenticated 403
// refusals of membership.grant (PUT) and pipeline.start, the admin read
// control, the accidental-grant revert at the returned version, the loud
// leftover failure when the admin cannot revoke, and the final service disable.
func TestHarnessRoleAuthorityProbe(t *testing.T) {
	type roleEdge struct {
		*fakeEdge
		serviceID  string
		allowGrant bool
		denyRevert bool
	}
	newEdge := func(t *testing.T, allowGrant, denyRevert bool) (*roleEdge, *API) {
		t.Helper()
		edge := &roleEdge{fakeEdge: &fakeEdge{}, allowGrant: allowGrant, denyRevert: denyRevert}
		server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			envelope := edge.record(t, request)
			authorization := request.Header.Get("Authorization")
			switch {
			case request.Method == http.MethodPost && request.URL.Path == "/api/v1/projects/proj/services":
				var input api.ServiceCreateRequest
				_ = json.Unmarshal(envelope.Input, &input)
				if input.ServiceID == "" || len(input.Roles) != 1 || input.Roles[0] != "viewer" {
					writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
					return
				}
				edge.serviceID = input.ServiceID
				writePublicTestResult(t, writer, "service.create", fmt.Sprintf(`{"service_id":%q,"version":1}`, edge.serviceID))
			case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/credentials"):
				if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != 1 {
					writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
					return
				}
				writePublicTestResult(t, writer, "service.credential.issue",
					`{"state":"current","version":2,"token":"kodsvc.viewer.mac"}`)
			case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/proj/executions":
				switch authorization {
				case "Bearer admin-token-value", "Bearer kodsvc.viewer.mac":
					writePublicTestResult(t, writer, "execution.list", `{"executions":[]}`)
				default:
					writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				}
			case request.Method == http.MethodPut && request.URL.Path == "/api/v1/projects/proj/memberships/throwaway-principal":
				if authorization != "Bearer kodsvc.viewer.mac" {
					writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
					return
				}
				if !edge.allowGrant {
					writePublicTestError(writer, http.StatusForbidden, api.CodeForbidden)
					return
				}
				writePublicTestResult(t, writer, "membership.grant", `{"version":7}`)
			case request.Method == http.MethodPost && request.URL.Path == "/api/v1/projects/proj/executions":
				if authorization != "Bearer kodsvc.viewer.mac" {
					writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
					return
				}
				writePublicTestError(writer, http.StatusForbidden, api.CodeForbidden)
			case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/disable"):
				if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != 2 {
					writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
					return
				}
				writePublicTestResult(t, writer, "service.disable",
					`{"state":"disabled","version":3,"revoked_credentials":1}`)
			case request.Method == http.MethodDelete && request.URL.Path == "/api/v1/projects/proj/memberships/throwaway-principal":
				if authorization != "Bearer admin-token-value" {
					writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
					return
				}
				if edge.denyRevert {
					writePublicTestError(writer, http.StatusForbidden, api.CodeForbidden)
					return
				}
				if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != 7 {
					writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
					return
				}
				writePublicTestResult(t, writer, "membership.revoke", `{"version":8}`)
			default:
				writePublicTestError(writer, http.StatusNotFound, api.CodeNotFound)
			}
		}))
		client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
		if err != nil {
			t.Fatalf("build API helper: %v", err)
		}
		return edge, client
	}
	newProbe := func(client *API) *roleAuthorityProbe {
		return &roleAuthorityProbe{
			client:      client,
			adminToken:  Secret("admin-token-value"),
			projectID:   "proj",
			principalID: "throwaway-principal",
		}
	}
	find := func(requests []fakeEdgeRequest, method, suffix string) *fakeEdgeRequest {
		for index := range requests {
			if requests[index].Method == method && strings.HasSuffix(requests[index].Path, suffix) {
				return &requests[index]
			}
		}
		return nil
	}

	t.Run("viewer is refused and the service is disabled", func(t *testing.T) {
		edge, client := newEdge(t, false, false)
		probe := newProbe(client)
		if err := probe.run(context.Background()); err != nil {
			t.Fatalf("the roles probe failed on the forbidden path: %v", err)
		}
		requests := edge.recorded()
		if len(requests) != 7 {
			t.Fatalf("requests = %d, want create, issue, viewer read, grant, start, admin read and disable: %+v", len(requests), requests)
		}
		if probe.serviceID == "" || probe.serviceID != edge.serviceID || !probe.disabled {
			t.Fatalf("service identity/disabled = %q/%q/%v", probe.serviceID, edge.serviceID, probe.disabled)
		}
		if len(probe.issued) != 1 || probe.issued[0].Reveal() != "kodsvc.viewer.mac" {
			t.Fatal("the viewer credential was not registered as issued")
		}
		grant := find(requests, http.MethodPut, "/memberships/throwaway-principal")
		if grant == nil {
			t.Fatal("the probe sent no membership.grant")
		}
		if grant.Path != "/api/v1/projects/proj/memberships/throwaway-principal" ||
			grant.Authorization != "Bearer kodsvc.viewer.mac" {
			t.Fatalf("grant = %+v, want the viewer credential on the frozen PUT route", grant)
		}
		if grant.IdempotencyKey == "" || grant.ExpectedVersion != nil {
			t.Fatalf("grant envelope = %+v, want an idempotency key and no expected version", grant)
		}
		var grantInput api.MembershipGrantRequest
		if err := json.Unmarshal(grant.Input, &grantInput); err != nil ||
			grantInput.ProjectID != "proj" || grantInput.PrincipalID != "throwaway-principal" ||
			len(grantInput.Roles) != 1 || grantInput.Roles[0] != "viewer" {
			t.Fatalf("grant input = %s", grant.Input)
		}
		start := find(requests, http.MethodPost, "/projects/proj/executions")
		if start == nil || start.Authorization != "Bearer kodsvc.viewer.mac" || start.IdempotencyKey == "" {
			t.Fatalf("pipeline.start = %+v, want the viewer credential and an idempotency key", start)
		}
		if find(requests, http.MethodDelete, "") != nil {
			t.Fatal("a refused grant still sent a revert")
		}
		if requests[len(requests)-1].Method != http.MethodPost || !strings.HasSuffix(requests[len(requests)-1].Path, "/disable") {
			t.Fatalf("last request = %+v, want the service disable", requests[len(requests)-1])
		}
	})

	t.Run("accidental grant is reverted at the returned version", func(t *testing.T) {
		edge, client := newEdge(t, true, false)
		probe := newProbe(client)
		err := probe.run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "allowed") || !strings.Contains(err.Error(), "reverted") {
			t.Fatalf("the roles probe did not report and revert an accidental grant: %v", err)
		}
		if strings.Contains(err.Error(), "kodsvc.viewer.mac") {
			t.Fatal("the roles probe error leaked the viewer token")
		}
		requests := edge.recorded()
		revoke := find(requests, http.MethodDelete, "/memberships/throwaway-principal")
		if revoke == nil {
			t.Fatalf("the accidental grant was not reverted: %+v", requests)
		}
		if revoke.Authorization != "Bearer admin-token-value" ||
			revoke.ExpectedVersion == nil || *revoke.ExpectedVersion != 7 {
			t.Fatalf("revert = %+v, want the admin token at the granted version 7", revoke)
		}
		if find(requests, http.MethodPost, "/disable") == nil {
			t.Fatal("the viewer service was not disabled after the accidental grant")
		}
	})

	t.Run("failed revert fails loudly naming the leftover principal", func(t *testing.T) {
		edge, client := newEdge(t, true, true)
		probe := newProbe(client)
		err := probe.run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "throwaway-principal") ||
			!strings.Contains(err.Error(), "leftover") {
			t.Fatalf("a failed revert did not name the leftover principal: %v", err)
		}
		if find(edge.recorded(), http.MethodPost, "/disable") == nil {
			t.Fatal("the viewer service was not disabled after a failed revert")
		}
	})
}

// TestHarnessServiceAuthorityProbe drives the WHOLE service lifecycle against a
// fake edge with the frozen version chain. It pins every method/path/version,
// the positive-then-negative credential reads, and the issued tokens the
// evidence writer must register.
func TestHarnessServiceAuthorityProbe(t *testing.T) {
	type serviceEdge struct {
		*fakeEdge
		serviceID string
		version   uint64
		valid     map[string]bool
	}
	edge := &serviceEdge{fakeEdge: &fakeEdge{}, valid: map[string]bool{}}
	server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		envelope := edge.record(t, request)
		authorization := request.Header.Get("Authorization")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/projects/proj/services":
			var input api.ServiceCreateRequest
			_ = json.Unmarshal(envelope.Input, &input)
			if input.ServiceID == "" {
				writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
				return
			}
			edge.serviceID = input.ServiceID
			edge.version = 1
			writePublicTestResult(t, writer, "service.create", fmt.Sprintf(`{"service_id":%q,"version":1}`, edge.serviceID))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/credentials/rotate"):
			if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != edge.version {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			edge.version = 3
			delete(edge.valid, "kodsvc.svc1.mac")
			edge.valid["kodsvc.svc2.mac"] = true
			writePublicTestResult(t, writer, "service.credential.rotate",
				`{"state":"current","version":3,"token":"kodsvc.svc2.mac"}`)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/credentials"):
			if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != edge.version {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			switch edge.version {
			case 1:
				edge.version = 2
				edge.valid["kodsvc.svc1.mac"] = true
				writePublicTestResult(t, writer, "service.credential.issue",
					`{"state":"current","version":2,"token":"kodsvc.svc1.mac"}`)
			case 4:
				edge.version = 5
				edge.valid["kodsvc.svc3.mac"] = true
				writePublicTestResult(t, writer, "service.credential.issue",
					`{"state":"current","version":5,"token":"kodsvc.svc3.mac"}`)
			default:
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
			}
		case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/credentials"):
			if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != edge.version {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			edge.version = 4
			for token := range edge.valid {
				delete(edge.valid, token)
			}
			writePublicTestResult(t, writer, "service.credential.revoke", `{"version":4,"invalidated":true}`)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/disable"):
			if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != edge.version {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			edge.version = 6
			for token := range edge.valid {
				delete(edge.valid, token)
			}
			writePublicTestResult(t, writer, "service.disable",
				`{"state":"disabled","version":6,"revoked_credentials":1}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/proj/executions":
			token := strings.TrimPrefix(authorization, "Bearer ")
			edge.mu.Lock()
			allowed := edge.valid[token]
			edge.mu.Unlock()
			if !allowed {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			writePublicTestResult(t, writer, "execution.list", `{"executions":[]}`)
		default:
			writePublicTestError(writer, http.StatusNotFound, api.CodeNotFound)
		}
	}))
	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	probe := &serviceAuthorityProbe{
		client: client,
		config: Config{AdminToken: Secret("admin-token-value"), ProjectID: "proj"},
	}
	if err := probe.run(context.Background()); err != nil {
		t.Fatalf("the service probe failed against the frozen fake edge: %v", err)
	}
	if probe.serviceID == "" || edge.serviceID != probe.serviceID {
		t.Fatalf("service identity = %q / %q", probe.serviceID, edge.serviceID)
	}
	if len(probe.issued) != 3 {
		t.Fatalf("issued credentials = %d, want 3", len(probe.issued))
	}
	for index, want := range []string{"kodsvc.svc1.mac", "kodsvc.svc2.mac", "kodsvc.svc3.mac"} {
		if probe.issued[index].Reveal() != want {
			t.Fatalf("issued credential %d is not the token the fake edge returned", index)
		}
	}
	evidence := probe.evidence(time.Now().UTC(), time.Now().UTC())
	if len(evidence.ExtraSecrets) != 3 {
		t.Fatalf("evidence registers %d credentials, want all 3 issued ones", len(evidence.ExtraSecrets))
	}
	for index, secret := range evidence.ExtraSecrets {
		if secret != probe.issued[index] {
			t.Fatalf("evidence credential %d is not an issued credential", index)
		}
	}
	requests := edge.recorded()
	wantSequence := []string{
		"POST /api/v1/projects/proj/services",
		"POST /api/v1/services/" + probe.serviceID + "/credentials",
		"GET /api/v1/projects/proj/executions",
		"POST /api/v1/services/" + probe.serviceID + "/credentials/rotate",
		"GET /api/v1/projects/proj/executions",
		"GET /api/v1/projects/proj/executions",
		"DELETE /api/v1/services/" + probe.serviceID + "/credentials",
		"GET /api/v1/projects/proj/executions",
		"POST /api/v1/services/" + probe.serviceID + "/credentials",
		"GET /api/v1/projects/proj/executions",
		"POST /api/v1/services/" + probe.serviceID + "/disable",
		"GET /api/v1/projects/proj/executions",
	}
	if len(requests) != len(wantSequence) {
		t.Fatalf("requests = %d, want %d: %+v", len(requests), len(wantSequence), requests)
	}
	for index, want := range wantSequence {
		got := requests[index].Method + " " + requests[index].Path
		if got != want {
			t.Fatalf("request %d = %q, want %q", index, got, want)
		}
	}
	for index, request := range requests {
		if request.Method == http.MethodGet {
			if request.IdempotencyKey != "" {
				t.Fatalf("read %d carries an idempotency key", index)
			}
			continue
		}
		if request.IdempotencyKey == "" {
			t.Fatalf("mutation %d carries no idempotency key", index)
		}
	}
}

// TestHarnessMCPTransportFailures pins the MCP helper's path and non-200
// handling: a redirect is never followed, and any non-200 status is a
// transport failure.
func TestHarnessMCPTransportFailures(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"q02-1","method":"tools/call","params":{"name":"execution.list","arguments":{"input":{"project_id":"proj"}}}}`)
	t.Run("redirect is never followed", func(t *testing.T) {
		var followed int
		server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/elsewhere" {
				followed++
				writer.WriteHeader(http.StatusOK)
				return
			}
			http.Redirect(writer, request, "/elsewhere", http.StatusFound)
		}))
		client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
		if err != nil {
			t.Fatalf("build API helper: %v", err)
		}
		if _, err := client.PostMCP(context.Background(), Secret("token-value"), body); err == nil ||
			!strings.Contains(strings.ToLower(err.Error()), "redirect") {
			t.Fatalf("a redirect returned %v", err)
		}
		if followed != 0 {
			t.Fatal("the MCP redirect target was followed")
		}
	})
	for _, status := range []int{http.StatusCreated, http.StatusNoContent, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("status %d is refused", status), func(t *testing.T) {
			var gotPath string
			server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				gotPath = request.URL.Path
				writer.WriteHeader(status)
			}))
			client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
			if err != nil {
				t.Fatalf("build API helper: %v", err)
			}
			if _, err := client.PostMCP(context.Background(), Secret("token-value"), body); err == nil ||
				!strings.Contains(err.Error(), fmt.Sprintf("%d", status)) {
				t.Fatalf("status %d returned %v", status, err)
			}
			if gotPath != "/mcp" {
				t.Fatalf("MCP path = %q, want the served /mcp path", gotPath)
			}
		})
	}
}

// TestHarnessRenewalImagePreflight proves the generated preflight refuses a
// workload image that lacks any of the tools the probe runs.
func TestHarnessRenewalImagePreflight(t *testing.T) {
	dir := t.TempDir()
	paths := renewalPaths{
		TokenFile:     filepath.Join(dir, "KODAMA_TOKEN"),
		CAFile:        filepath.Join(dir, "ca.crt"),
		SettledOutput: filepath.Join(dir, "fingerprint"),
	}
	probe := renewalProbe{Paths: paths, SkipValidation: true}
	commands := joinCommands(probe.firstCommands())
	tools := []string{"curl", "sha256sum", "cut", "sleep", "grep", "head", "cat"}
	for _, missing := range tools {
		t.Run("missing "+missing, func(t *testing.T) {
			bin := t.TempDir()
			for _, tool := range tools {
				if tool == missing {
					continue
				}
				path := filepath.Join(bin, tool)
				if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatalf("write %s stub: %v", tool, err)
				}
				if err := os.Chmod(path, 0o700); err != nil {
					t.Fatalf("chmod %s stub: %v", tool, err)
				}
			}
			t.Setenv("PATH", bin)
			err := runShell(t, commands)
			if err == nil || !strings.Contains(err.Error(), "must provide "+missing) {
				t.Fatalf("a missing %s returned %v", missing, err)
			}
		})
	}
}

// TestHarnessRenewalProbeRealCurl runs the generated commands with the real
// curl against a loopback server that routes by method. It proves the workload
// inspects with GET, submits the fingerprint with the frozen PUT envelope
// (including the observed execution version and action id), and uses the
// renewed mounted credential on the second action's GET.
func TestHarnessRenewalProbeRealCurl(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		if os.Getenv("Q02_REQUIRE_STAGE") == "1" || os.Getenv("KODAMA_REQUIRE_INFRA") == "1" {
			t.Fatalf("real curl is not available on the test host: %v", err)
		}
		t.Skip("real curl is not available on the test host")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if os.Getenv("Q02_REQUIRE_STAGE") == "1" || os.Getenv("KODAMA_REQUIRE_INFRA") == "1" {
			t.Fatalf("sha256sum is not available on the test host: %v", err)
		}
		t.Skip("sha256sum is not available on the test host")
	}
	type recordedRequest struct{ method, path, body string }
	var (
		mu       sync.Mutex
		requests []recordedRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		requests = append(requests, recordedRequest{method: request.Method, path: request.URL.Path, body: string(body)})
		mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/executions/exec-one" && string(body) == "{}":
			writer.Header().Set("Content-Type", api.MediaType)
			_, _ = writer.Write([]byte(fakeInspectBody))
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/executions/exec-one/actions/action-one/outputs":
			writePublicTestResult(t, writer, "execution.action.outputs", `{"execution_version":8,"action_version":2}`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	paths := renewalPaths{
		TokenFile:     filepath.Join(dir, "KODAMA_TOKEN"),
		CAFile:        filepath.Join(dir, "ca.crt"),
		SettledOutput: filepath.Join(dir, "fingerprint"),
	}
	mustWriteFile(t, paths.CAFile, "unused for plain HTTP")
	mustWriteFile(t, paths.TokenFile, "token-one")
	probe := renewalProbe{
		Image:          "registry.example.com/q02/probe@sha256:" + strings.Repeat("a", 64),
		FirstTimeout:   renewalFirstTimeout,
		SecondTimeout:  renewalSecondTimeout,
		DeadlineWait:   0,
		PollAttempts:   5,
		PollInterval:   time.Second,
		Paths:          paths,
		SkipValidation: true,
	}
	if err := runShellURL(t, joinCommands(probe.firstCommands()), server.URL); err != nil {
		t.Fatalf("the first action failed with real curl: %v", err)
	}
	firstSHA := sha256SumHexFile(t, paths.TokenFile)
	mustWriteFile(t, paths.SettledOutput, firstSHA+"\n")

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(paths.TokenFile, []byte("token-two"), 0o600)
	}()
	err := runShellURL(t, joinCommands(probe.secondCommands()), server.URL)
	<-done
	if err != nil {
		t.Fatalf("the second action failed with real curl: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var submitted *recordedRequest
	getCount := 0
	for index := range requests {
		request := requests[index]
		switch {
		case request.method == http.MethodGet && request.path == "/api/v1/executions/exec-one":
			if request.body != "{}" {
				t.Fatalf("GET %d body = %q, want the frozen envelope", index, request.body)
			}
			getCount++
		case request.method == http.MethodPut && request.path == "/api/v1/executions/exec-one/actions/action-one/outputs":
			submitted = &requests[index]
		default:
			t.Fatalf("unexpected request %d: %s %s", index, request.method, request.path)
		}
	}
	if getCount < 3 {
		t.Fatalf("the server saw %d execution GETs, want two first-action inspections and the second action's check", getCount)
	}
	if submitted == nil {
		t.Fatal("the first action submitted no fingerprint output")
	}
	for _, want := range []string{`"fingerprint":"` + firstSHA + `"`, `"expected_version":7`} {
		if !strings.Contains(submitted.body, want) {
			t.Fatalf("submitted output %q does not contain %q", submitted.body, want)
		}
	}
}

// sha256SumHexFile returns the lower-case hex sha256 of one file's bytes.
func sha256SumHexFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
