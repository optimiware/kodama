package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// E2EInput is everything the end-to-end check needs, passed explicitly: the
// deployment under test, what it must be running, and test principals.
type E2EInput struct {
	AppURL              string
	DeploymentID        string
	ConfigurationDigest string
	ImageDigest         string
	SourceCommit        string
	// Tenant is an application tenant where ReviewerToken holds the reviewer role.
	Tenant string
	// OtherTenant is a tenant where the reviewer has no right to the first tenant's data.
	OtherTenant   string
	ReviewerToken string
	// OutsiderToken belongs to a user who is a member of OtherTenant only.
	OutsiderToken string
	HTTP          *http.Client
	Settle        time.Duration
}

// Check is one observed e2e assertion.
type Check struct {
	ID      string `json:"id"`
	Passed  bool   `json:"passed"`
	Details string `json:"details,omitempty"`
}

// E2EReport is the retained evidence. It identifies the inspected deployment.
type E2EReport struct {
	DeploymentID        string  `json:"deploymentId"`
	ConfigurationDigest string  `json:"configurationDigest"`
	ImageDigest         string  `json:"imageDigest"`
	SourceCommit        string  `json:"sourceCommit"`
	AppURL              string  `json:"appUrl"`
	Checks              []Check `json:"checks"`
	Passed              bool    `json:"passed"`
	// ResolutionState is the final observed state of the test exception.
	ResolutionState string `json:"resolutionState,omitempty"`
}

// ReasonDeploymentMismatch is the failure reason when the live target is not the intended one.
const ReasonDeploymentMismatch = "deployment_mismatch"

type e2e struct {
	in  E2EInput
	rep E2EReport
}

func (e *e2e) check(id string, ok bool, f string, a ...any) bool {
	e.rep.Checks = append(e.rep.Checks, Check{ID: id, Passed: ok, Details: fmt.Sprintf(f, a...)})
	return ok
}

func (e *e2e) call(ctx context.Context, method, path, token string, body any, headers map[string]string) (int, map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(e.in.AppURL, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.in.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, nil
}

// RunE2E verifies the live target is the intended deployment, then exercises
// the workflow. Nothing is tested against a target that does not match.
func RunE2E(ctx context.Context, in E2EInput) (E2EReport, error) {
	if in.HTTP == nil {
		in.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	if in.Settle <= 0 {
		in.Settle = 30 * time.Second
	}
	e := &e2e{in: in, rep: E2EReport{DeploymentID: in.DeploymentID, ConfigurationDigest: in.ConfigurationDigest, ImageDigest: in.ImageDigest, SourceCommit: in.SourceCommit, AppURL: in.AppURL}}

	// 1. The live target must be exactly the deployment under test.
	status, body, err := e.call(ctx, http.MethodGet, "/version", "", nil, nil)
	if err != nil {
		return e.rep, fmt.Errorf("delivery: the application is unreachable: %w", scrubURL(err))
	}
	want := map[string]string{"deploymentId": in.DeploymentID, "configurationDigest": in.ConfigurationDigest, "imageDigest": in.ImageDigest, "sourceCommit": in.SourceCommit}
	match := status == 200
	var diffs []string
	for k, v := range want {
		if got, _ := body[k].(string); got != v {
			match = false
			diffs = append(diffs, k)
		}
	}
	if !e.check("live-target-matches-deployment", match, "mismatched fields: %v", diffs) {
		return e.rep, nil // never test the wrong deployment
	}
	st, _, err := e.call(ctx, http.MethodGet, "/readyz", "", nil, nil)
	if !e.check("ready", err == nil && st == 200, "readyz status %d", st) {
		return e.rep, nil
	}

	base := "/api/v1/tenants/" + in.Tenant + "/exceptions"
	st, created, _ := e.call(ctx, http.MethodPost, base, in.ReviewerToken, map[string]any{"erpRef": "E2E/" + in.DeploymentID[len(in.DeploymentID)-8:], "description": "e2e", "erpQuantity": 12, "countedQuantity": 9}, nil)
	id, _ := created["id"].(string)
	if !e.check("reviewer-creates-exception", st == 201 && id != "", "status %d", st) {
		return e.rep, nil
	}

	// 2. Cross-tenant denial.
	st, _, _ = e.call(ctx, http.MethodGet, base+"/"+id, in.OutsiderToken, nil, nil)
	e.check("outsider-cannot-read", st == 404, "status %d", st)
	st, _, _ = e.call(ctx, http.MethodGet, "/api/v1/tenants/"+in.OtherTenant+"/exceptions/"+id, in.ReviewerToken, nil, nil)
	e.check("exception-invisible-through-another-tenant", st == 404, "status %d", st)
	st, _, _ = e.call(ctx, http.MethodPost, base+"/"+id+"/resolution", in.OutsiderToken, map[string]any{"adjustedQuantity": 9, "reason": "x"},
		map[string]string{"Idempotency-Key": "e2e-outsider-0001", "If-Match": `"1"`})
	e.check("outsider-cannot-resolve", st == 404, "status %d", st)

	// 3. Resolution: idempotent, never resolved without the ERP's acknowledgment.
	hdr := map[string]string{"Idempotency-Key": "e2e-" + in.DeploymentID[len(in.DeploymentID)-12:], "If-Match": `"1"`}
	req := map[string]any{"adjustedQuantity": 9, "reason": "e2e recount"}
	st, first, _ := e.call(ctx, http.MethodPost, base+"/"+id+"/resolution", in.ReviewerToken, req, hdr)
	e.check("resolution-accepted", st == 202 && first["state"] == "pending", "status %d state %v", st, first["state"])
	st, again, _ := e.call(ctx, http.MethodPost, base+"/"+id+"/resolution", in.ReviewerToken, req, hdr)
	e.check("resolution-replay-is-idempotent", (st == 202 || st == 200) && operationKey(again) != "" && operationKey(again) == operationKey(first), "status %d", st)
	st, _, _ = e.call(ctx, http.MethodPost, base+"/"+id+"/resolution", in.ReviewerToken, map[string]any{"adjustedQuantity": 8, "reason": "e2e recount"}, hdr)
	e.check("changed-payload-conflicts", st == 409, "status %d", st)

	deadline := time.Now().Add(in.Settle)
	final := ""
	for {
		_, cur, _ := e.call(ctx, http.MethodGet, base+"/"+id, in.ReviewerToken, nil, nil)
		final, _ = cur["state"].(string)
		res, _ := cur["resolution"].(map[string]any)
		if final == "resolved" {
			ref, _ := res["erpReference"].(string)
			e.check("resolved-only-with-erp-acknowledgment", ref != "", "erpReference %q", ref)
			break
		}
		if final == "failed" || time.Now().After(deadline) {
			// pending and unknown are honest, visible states; a rejection is a legitimate ERP answer.
			e.check("unresolved-outcome-stays-visible", final == "pending" || final == "unknown" || final == "failed", "state %q", final)
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	e.rep.ResolutionState = final
	e.rep.Passed = true
	for _, c := range e.rep.Checks {
		e.rep.Passed = e.rep.Passed && c.Passed
	}
	return e.rep, nil
}

func operationKey(m map[string]any) string {
	r, _ := m["resolution"].(map[string]any)
	k, _ := r["operationKey"].(string)
	return k
}
