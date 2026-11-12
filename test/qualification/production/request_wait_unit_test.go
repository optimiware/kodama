//go:build qualification

package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// requestWaitFakeRequest is one recorded envelope at the fake edge.
type requestWaitFakeRequest struct {
	Method          string
	Path            string
	Query           string
	Authorization   string
	Input           json.RawMessage
	ExpectedVersion *uint64
	IdempotencyKey  string
}

// requestWaitFakeEdge models the request-wait lifecycle one call at a time. It
// keeps no credential material in evidence: tokens are synthetic fixtures.
type requestWaitFakeEdge struct {
	mu sync.Mutex

	requests []requestWaitFakeRequest

	serviceID      string
	serviceVersion uint64
	runMarker      string
	answer         string

	tokens    map[string]bool
	phase     string
	proven    bool
	keys      map[string]api.ExecutionRequestResolveResult
	deadline  time.Time
	activated time.Time
	answered  time.Time

	resolvedBeforeWait bool
	executionVersion   uint64
	actionVersion      uint64
	// staleStatus overrides the refusal status for the first (pre-revocation)
	// service token so a probe that accepts a non-401 refusal is observable.
	staleStatus int

	// refuse names one operation the fake edge refuses with the served public
	// invalid_input body and a secret-free field detail.
	refuse string
	// inboxMaxPageSize is the served task.inbox page bound
	// (internal/shell/inbox.MaxPageSize). The product and the fake both enforce
	// api.MaxPageLimit; a larger page.limit is invalid_input.
	inboxMaxPageSize int
	// beforeState, beforeActivated and beforeStop shape the pre-wait action so a
	// probe that counts a not-yet-started workload as failure is observable.
	beforeState     string
	beforeActivated bool
	beforeStop      *api.ExecutionStopEvidence
	// cleanupState overrides the observed cleanup state after the phase switch,
	// so a terminal success whose cleanup has not converged is observable.
	cleanupState string

	// Deviation knobs. Each makes the fake edge serve one wrong answer so a
	// probe assertion that does not refuse it is observable.
	//
	// capturedAfterWait overrides the status served to the captured execution
	// credential once the request is activated (0 keeps the served 401).
	capturedAfterWait int
	// staleInboxStatus and staleResolveStatus override the status served to a
	// non-live service credential on exactly one surface (0 keeps the 401).
	staleInboxStatus   int
	staleResolveStatus int
	// revokedReplayStatus overrides the status served to the revoked token
	// replaying a resolve (0 keeps the 401).
	revokedReplayStatus int
	// replayDiffers makes an identical authorized idempotency replay return a
	// different action version than the recorded result.
	replayDiffers bool
	// afterDecision records a complete decision on the still-waiting request, as
	// a refused resolution must never do. A partial decision is not a valid
	// public result body, so the recorded decision carries every member.
	afterDecision bool
	// successorActivated activates the successor while the request still waits.
	successorActivated bool
	// waitingState overrides the execution state while the request waits.
	waitingState string
	// resolvedActor overrides the actor recorded on the settled request.
	resolvedActor string
	// markerCount overrides how many completion markers the retained logs
	// carry (0 keeps exactly one).
	markerCount int
	// fingerprintOverride makes the pre-wait action settle a fingerprint that
	// is not the captured credential's digest.
	fingerprintOverride string
	// disabled records whether the service was disabled.
	disabled bool
}

// fingerprintLocked returns the settled pre-wait fingerprint the fake serves.
// The caller holds the lock.
func (edge *requestWaitFakeEdge) fingerprintLocked() string {
	if edge.fingerprintOverride != "" {
		return edge.fingerprintOverride
	}
	return requestWaitFakeFingerprint()
}

// serviceState reports the live service credential count and disabled state.
func (edge *requestWaitFakeEdge) serviceState() (int, bool) {
	edge.mu.Lock()
	defer edge.mu.Unlock()
	return len(edge.tokens), edge.disabled
}

// requestWaitFakeInboxMaxPageSize is the served task.inbox page bound
// (internal/shell/inbox.MaxPageSize = api.MaxPageLimit = 200). The fake models
// the product handler exactly, so a probe page the contract accepts is not
// refused by the fixture and a page the handler refuses is refused here too.
const requestWaitFakeInboxMaxPageSize = api.MaxPageLimit

// requestWaitFakeRefusalDetail is the secret-free field pointer the fake edge
// attaches to every invalid_input refusal, like the served edge's public message.
const requestWaitFakeRefusalDetail = "field page.limit is outside the accepted bound"

// inboxBound returns the served inbox page bound the edge models.
func (edge *requestWaitFakeEdge) inboxBound() int {
	if edge.inboxMaxPageSize > 0 {
		return edge.inboxMaxPageSize
	}
	return requestWaitFakeInboxMaxPageSize
}

// writeRequestWaitTestError answers one public refusal with a field detail, the
// way every served edge refusal carries a bounded public message.
func writeRequestWaitTestError(t *testing.T, writer http.ResponseWriter, status int, code, detail string) {
	t.Helper()
	payload, err := json.Marshal(api.Error{Code: code, Message: detail})
	if err != nil {
		t.Errorf("encode public error: %v", err)
		return
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

// requestWaitCodeForStatus maps one fake-edge refusal status to its public code.
func requestWaitCodeForStatus(status int) string {
	if status == http.StatusUnauthorized {
		return api.CodeUnauthenticated
	}
	return api.CodeForbidden
}

// requestWaitFakeOperation resolves the frozen C03 operation of one concrete
// method and path. A request the product would not route is observable here.
func requestWaitFakeOperation(method, path string) string {
	route, _, ok := api.Match(api.Method(method), path)
	if !ok {
		return ""
	}
	return route.Operation
}

// record reads and records one request envelope.
func (edge *requestWaitFakeEdge) record(t *testing.T, request *http.Request) api.Request {
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
	edge.requests = append(edge.requests, requestWaitFakeRequest{
		Method:          request.Method,
		Path:            request.URL.Path,
		Query:           request.URL.RawQuery,
		Authorization:   request.Header.Get("Authorization"),
		Input:           envelope.Input,
		ExpectedVersion: envelope.ExpectedVersion,
		IdempotencyKey:  envelope.IdempotencyKey,
	})
	edge.mu.Unlock()
	return envelope
}

// recorded returns a copy of the recorded requests.
func (edge *requestWaitFakeEdge) recorded() []requestWaitFakeRequest {
	edge.mu.Lock()
	defer edge.mu.Unlock()
	return append([]requestWaitFakeRequest(nil), edge.requests...)
}

// validService reports whether one bearer value is a live service credential.
func (edge *requestWaitFakeEdge) validService(authorization string) bool {
	token := strings.TrimPrefix(authorization, "Bearer ")
	edge.mu.Lock()
	defer edge.mu.Unlock()
	return edge.tokens[token]
}

// writeStaleRefusal answers one invalid service credential. The staleStatus
// override exists so a probe that treats a 403 as revocation is observable.
func (edge *requestWaitFakeEdge) writeStaleRefusal(_ *testing.T, writer http.ResponseWriter, authorization string) {
	edge.mu.Lock()
	status := edge.staleStatus
	edge.mu.Unlock()
	if status != 0 && authorization == "Bearer "+requestWaitFakeFirstService {
		code := api.CodeForbidden
		if status == http.StatusUnauthorized {
			code = api.CodeUnauthenticated
		}
		writePublicTestError(writer, status, code)
		return
	}
	writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
}

// inspection builds one valid execution.inspect observation for the phase.
func (edge *requestWaitFakeEdge) inspection() api.ExecutionInspectResult {
	edge.mu.Lock()
	defer edge.mu.Unlock()
	deadline := edge.deadline
	activated := edge.activated
	subject := &api.Subject{Fields: []api.SubjectField{
		{Name: requestWaitRunField, Kind: "string", Value: edge.runMarker},
		{Name: requestWaitFingerprintField, Kind: "string", Value: edge.fingerprintLocked()},
	}}
	execution := api.ExecutionInspectResult{
		ExecutionID:   "exec-rw-1",
		State:         "running",
		Version:       edge.executionVersion,
		CurrentAction: 1,
		Actions: []api.ExecutionAction{
			{
				ActionID:       "action-before",
				Index:          0,
				Name:           requestWaitBeforeName,
				Kind:           "container",
				State:          "running",
				Version:        2,
				TimeoutSeconds: int64(requestWaitBeforeTimeout / time.Second),
				Deadline:       &deadline,
			},
			{
				ActionID:       "action-request",
				Index:          1,
				Name:           requestWaitDecisionName,
				Kind:           "request",
				State:          "pending",
				Version:        1,
				TimeoutSeconds: int64(requestWaitDecisionTimeout / time.Second),
			},
			{
				ActionID:       "action-after",
				Index:          2,
				Name:           requestWaitAfterName,
				Kind:           "container",
				State:          "pending",
				Version:        1,
				TimeoutSeconds: int64(requestWaitAfterTimeout / time.Second),
			},
		},
		Cleanup: api.ExecutionCleanup{State: "none", WorkspaceID: "workspace-rw-1"},
	}
	switch edge.phase {
	case "wait":
		execution.Actions[0].State = "succeeded"
		execution.Actions[0].Outputs = map[string]string{requestWaitFingerprint: edge.fingerprintLocked()}
		execution.Actions[0].Stop = &api.ExecutionStopEvidence{State: "confirmed"}
		execution.Actions[1].State = "running"
		execution.Actions[1].Version = edge.actionVersion
		execution.Actions[1].ActivatedAt = &activated
		execution.Actions[1].Deadline = &deadline
		execution.Actions[1].Subject = subject
		execution.Actions[1].Assignment = "service/" + edge.serviceID
		if edge.afterDecision {
			answered := time.Now().UTC()
			execution.Actions[1].Decision = "pass"
			execution.Actions[1].Actor = edge.serviceID
			execution.Actions[1].AnsweredAt = &answered
			execution.Actions[1].Outputs = map[string]string{requestWaitAnswerOutput: edge.answer}
		}
		if edge.successorActivated {
			execution.Actions[2].ActivatedAt = &activated
		}
		if edge.waitingState != "" {
			execution.State = edge.waitingState
		}
	case "resolved":
		execution.State = "succeeded"
		execution.CurrentAction = 2
		execution.Actions[0].State = "succeeded"
		execution.Actions[0].Outputs = map[string]string{requestWaitFingerprint: edge.fingerprintLocked()}
		execution.Actions[0].Stop = &api.ExecutionStopEvidence{State: "confirmed"}
		execution.Actions[1].State = "succeeded"
		execution.Actions[1].Version = edge.actionVersion + 1
		execution.Actions[1].ActivatedAt = &activated
		execution.Actions[1].Deadline = &deadline
		execution.Actions[1].Subject = subject
		execution.Actions[1].Assignment = "service/" + edge.serviceID
		execution.Actions[1].Decision = "pass"
		execution.Actions[1].Actor = edge.serviceID
		if edge.resolvedActor != "" {
			execution.Actions[1].Actor = edge.resolvedActor
		}
		execution.Actions[1].AnsweredAt = &edge.answered
		execution.Actions[1].Outputs = map[string]string{requestWaitAnswerOutput: edge.answer}
		execution.Actions[2].State = "succeeded"
		execution.Actions[2].ActivatedAt = &edge.answered
		execution.Cleanup.State = "complete"
	case "cancelled":
		execution.State = "cancelled"
		execution.Cleanup.State = "complete"
	case "stuck":
		// The pre-wait action stopped, but its stop stayed unconfirmed and the
		// cleanup remained pending: product finding F-4.
		execution.State = "cancelled"
		execution.Actions[0].State = "cancelled"
		execution.Actions[0].Stop = &api.ExecutionStopEvidence{State: "unconfirmed", Detail: "stop unconfirmed: 0 workload pods observed"}
		execution.Cleanup.State = "pending"
		execution.Cleanup.WorkloadID = "wl-rw-stuck"
		execution.Cleanup.Attempts = 2
	case "before":
		if edge.beforeState != "" {
			execution.Actions[0].State = edge.beforeState
		}
		if edge.beforeActivated {
			execution.Actions[0].ActivatedAt = &activated
		} else {
			// A not-yet-activated action carries no deadline; the served edge
			// stamps it when the action activates.
			execution.Actions[0].Deadline = nil
		}
		if edge.beforeStop != nil {
			stop := *edge.beforeStop
			execution.Actions[0].Stop = &stop
		}
	}
	if edge.cleanupState != "" {
		execution.Cleanup.State = edge.cleanupState
	}
	return execution
}

// inboxItem builds the request item for the waiting phase.
func (edge *requestWaitFakeEdge) inboxItem() api.InboxItem {
	edge.mu.Lock()
	defer edge.mu.Unlock()
	deadline := edge.deadline
	return api.InboxItem{
		Kind:        "request",
		ExecutionID: "exec-rw-1",
		ActionID:    "action-request",
		Assignment:  "service/" + edge.serviceID,
		Subject: &api.Subject{Fields: []api.SubjectField{
			{Name: requestWaitRunField, Kind: "string", Value: edge.runMarker},
			{Name: requestWaitFingerprintField, Kind: "string", Value: edge.fingerprintLocked()},
		}},
		Outputs: []string{requestWaitAnswerOutput},
		Version: edge.actionVersion,
		At:      deadline,
	}
}

// fillerItem is a valid unrelated request item used to force pagination.
func requestWaitFakeFillerItem() api.InboxItem {
	return api.InboxItem{
		Kind:        "request",
		ExecutionID: "exec-other",
		ActionID:    "action-other",
		Assignment:  "service/other-service",
		Subject:     &api.Subject{Fields: []api.SubjectField{{Name: "run", Kind: "string", Value: "other"}}},
		Outputs:     []string{"answer"},
		Version:     3,
		At:          time.Now().UTC().Add(time.Minute),
	}
}

// requestWaitFakeFingerprint is the sha256 of the synthetic execution token.
func requestWaitFakeFingerprint() string {
	digest := sha256.Sum256([]byte(requestWaitFakeExecutionToken))
	return hex.EncodeToString(digest[:])
}

const (
	requestWaitFakeAdminToken     = "admin-token-value"
	requestWaitFakeExecutionToken = "kodex.exec-one.mac"
	requestWaitFakeFirstService   = "kodsvc.stage.first"
	requestWaitFakeFreshService   = "kodsvc.stage.second"
	requestWaitFakeProject        = "proj"
	requestWaitFakeTenantNS       = "tenant-a"
	requestWaitFakeActionVersion  = 7
)

// requestWaitFakeHandler serves the fake edge.
func requestWaitFakeHandler(t *testing.T, edge *requestWaitFakeEdge) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		envelope := edge.record(t, request)
		authorization := request.Header.Get("Authorization")
		operation := requestWaitFakeOperation(request.Method, request.URL.Path)
		if operation == "" {
			writePublicTestError(writer, http.StatusNotFound, api.CodeNotFound)
			return
		}
		if edge.refuse != "" && operation == edge.refuse {
			writeRequestWaitTestError(t, writer, http.StatusBadRequest, api.CodeInvalidInput, requestWaitFakeRefusalDetail)
			return
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/projects/"+requestWaitFakeProject+"/services":
			if authorization != "Bearer "+requestWaitFakeAdminToken {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			var input api.ServiceCreateRequest
			if err := json.Unmarshal(envelope.Input, &input); err != nil || input.ServiceID == "" {
				writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
				return
			}
			edge.mu.Lock()
			edge.serviceID = input.ServiceID
			edge.serviceVersion = 1
			edge.mu.Unlock()
			writePublicTestResult(t, writer, "service.create", fmt.Sprintf(`{"service_id":%q,"version":1}`, input.ServiceID))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/credentials"):
			if authorization != "Bearer "+requestWaitFakeAdminToken {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			edge.mu.Lock()
			defer edge.mu.Unlock()
			if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != edge.serviceVersion {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			var token string
			switch edge.serviceVersion {
			case 1:
				token = requestWaitFakeFirstService
				edge.serviceVersion = 2
			case 3:
				token = requestWaitFakeFreshService
				edge.serviceVersion = 4
			default:
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			edge.tokens[token] = true
			writePublicTestResult(t, writer, "service.credential.issue",
				fmt.Sprintf(`{"state":"current","version":%d,"token":%q}`, edge.serviceVersion, token))
		case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/credentials"):
			if authorization != "Bearer "+requestWaitFakeAdminToken {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			edge.mu.Lock()
			defer edge.mu.Unlock()
			if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != edge.serviceVersion {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			for token := range edge.tokens {
				delete(edge.tokens, token)
			}
			edge.serviceVersion++
			writePublicTestResult(t, writer, "service.credential.revoke",
				fmt.Sprintf(`{"version":%d,"invalidated":true}`, edge.serviceVersion))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/disable"):
			if authorization != "Bearer "+requestWaitFakeAdminToken {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			edge.mu.Lock()
			defer edge.mu.Unlock()
			if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != edge.serviceVersion {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			for token := range edge.tokens {
				delete(edge.tokens, token)
			}
			edge.disabled = true
			edge.serviceVersion++
			writePublicTestResult(t, writer, "service.disable",
				fmt.Sprintf(`{"state":"disabled","version":%d,"revoked_credentials":1}`, edge.serviceVersion))
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/projects/"+requestWaitFakeProject+"/executions":
			if authorization != "Bearer "+requestWaitFakeAdminToken {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			edge.mu.Lock()
			edge.phase = "before"
			edge.executionVersion = 1
			edge.actionVersion = requestWaitFakeActionVersion
			edge.deadline = time.Now().UTC().Add(4 * time.Minute)
			edge.activated = time.Now().UTC()
			edge.mu.Unlock()
			writePublicTestResult(t, writer, "pipeline.start", `{"execution_id":"exec-rw-1","version":1}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/executions/exec-rw-1":
			token := strings.TrimPrefix(authorization, "Bearer ")
			edge.mu.Lock()
			active := token == requestWaitFakeExecutionToken && edge.phase == "before"
			admin := token == requestWaitFakeAdminToken
			service := edge.tokens[token]
			afterWaitStatus := 0
			if token == requestWaitFakeExecutionToken && edge.phase != "before" {
				afterWaitStatus = edge.capturedAfterWait
			}
			if active {
				edge.proven = true
			}
			if admin && edge.phase == "before" && edge.proven {
				edge.phase = "wait"
			}
			edge.mu.Unlock()
			if afterWaitStatus != 0 {
				if afterWaitStatus != http.StatusOK {
					writePublicTestError(writer, afterWaitStatus, requestWaitCodeForStatus(afterWaitStatus))
					return
				}
			} else if !admin && !service && !active {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			body, err := json.Marshal(edge.inspection())
			if err != nil {
				t.Errorf("encode fake inspection: %v", err)
				return
			}
			writePublicTestResult(t, writer, "execution.inspect", string(body))
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/"+requestWaitFakeProject+"/executions":
			if !edge.validService(authorization) && authorization != "Bearer "+requestWaitFakeAdminToken {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			writePublicTestResult(t, writer, "execution.list", `{"executions":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/"+requestWaitFakeProject+"/inbox":
			if !edge.validService(authorization) && authorization != "Bearer "+requestWaitFakeAdminToken {
				edge.mu.Lock()
				status := edge.staleInboxStatus
				edge.mu.Unlock()
				switch status {
				case 0:
					edge.writeStaleRefusal(t, writer, authorization)
				case http.StatusOK:
					writePublicTestResult(t, writer, "task.inbox", `{"items":[]}`)
				default:
					writePublicTestError(writer, status, requestWaitCodeForStatus(status))
				}
				return
			}
			var input api.TaskInboxRequest
			_ = json.Unmarshal(envelope.Input, &input)
			// Model the served task.inbox handler bound: a larger page limit is
			// refused with invalid_input even though api.Page accepts it.
			if input.Page != nil && (input.Page.Limit < 0 || input.Page.Limit > edge.inboxBound()) {
				writeRequestWaitTestError(t, writer, http.StatusBadRequest, api.CodeInvalidInput,
					fmt.Sprintf("inbox limit %d is outside 0..%d", input.Page.Limit, edge.inboxBound()))
				return
			}
			edge.mu.Lock()
			phase := edge.phase
			edge.mu.Unlock()
			if phase != "wait" && phase != "resolved" {
				writePublicTestResult(t, writer, "task.inbox", `{"items":[]}`)
				return
			}
			cursor := ""
			if input.Page != nil {
				cursor = input.Page.Cursor
			}
			if phase == "resolved" {
				writePublicTestResult(t, writer, "task.inbox", `{"items":[]}`)
				return
			}
			if cursor == "" {
				body, err := json.Marshal(api.TaskInboxResult{Items: []api.InboxItem{requestWaitFakeFillerItem()}, NextCursor: "page-2"})
				if err != nil {
					t.Errorf("encode fake inbox page: %v", err)
					return
				}
				writePublicTestResult(t, writer, "task.inbox", string(body))
				return
			}
			if cursor != "page-2" {
				writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
				return
			}
			body, err := json.Marshal(api.TaskInboxResult{Items: []api.InboxItem{edge.inboxItem()}})
			if err != nil {
				t.Errorf("encode fake inbox page: %v", err)
				return
			}
			writePublicTestResult(t, writer, "task.inbox", string(body))
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/executions/exec-rw-1/actions/action-request/resolve":
			if !edge.validService(authorization) && authorization != "Bearer "+requestWaitFakeAdminToken {
				edge.mu.Lock()
				stale := edge.staleResolveStatus
				revokedReplay := edge.revokedReplayStatus
				edge.mu.Unlock()
				switch {
				case revokedReplay != 0 && authorization == "Bearer "+requestWaitFakeFirstService:
					if revokedReplay != http.StatusOK {
						writePublicTestError(writer, revokedReplay, requestWaitCodeForStatus(revokedReplay))
						return
					}
					writePublicTestResult(t, writer, "execution.request.resolve",
						fmt.Sprintf(`{"execution_version":2,"action_version":%d}`, requestWaitFakeActionVersion+1))
				case stale == 0:
					edge.writeStaleRefusal(t, writer, authorization)
				case stale == http.StatusOK:
					writePublicTestResult(t, writer, "execution.request.resolve",
						fmt.Sprintf(`{"execution_version":2,"action_version":%d}`, requestWaitFakeActionVersion+1))
				default:
					writePublicTestError(writer, stale, requestWaitCodeForStatus(stale))
				}
				return
			}
			var input api.ExecutionRequestResolveRequest
			if err := json.Unmarshal(envelope.Input, &input); err != nil {
				writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
				return
			}
			edge.mu.Lock()
			defer edge.mu.Unlock()
			if envelope.ExpectedVersion != nil {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			if envelope.IdempotencyKey == "" {
				writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
				return
			}
			// An identical authorized key replays the recorded result after
			// settlement; a different identity is refused.
			if result, ok := edge.keys[envelope.IdempotencyKey]; ok {
				actionVersion := result.ActionVersion
				if edge.replayDiffers {
					actionVersion++
				}
				writePublicTestResult(t, writer, "execution.request.resolve",
					fmt.Sprintf(`{"execution_version":%d,"action_version":%d}`, result.ExecutionVersion, actionVersion))
				return
			}
			if edge.phase != "wait" {
				if edge.phase == "" {
					edge.resolvedBeforeWait = true
				}
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			if input.ExpectedActionVersion != edge.actionVersion ||
				input.Decision != "pass" ||
				input.Outputs[requestWaitAnswerOutput] != edge.answer {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			edge.answered = time.Now().UTC()
			edge.phase = "resolved"
			edge.executionVersion = 2
			edge.keys[envelope.IdempotencyKey] = api.ExecutionRequestResolveResult{ExecutionVersion: 2, ActionVersion: edge.actionVersion + 1}
			result := edge.keys[envelope.IdempotencyKey]
			writePublicTestResult(t, writer, "execution.request.resolve",
				fmt.Sprintf(`{"execution_version":%d,"action_version":%d}`, result.ExecutionVersion, result.ActionVersion))
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/executions/exec-rw-1/logs":
			if authorization != "Bearer "+requestWaitFakeAdminToken {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			var input api.ExecutionLogsReadRequest
			_ = json.Unmarshal(envelope.Input, &input)
			if input.ActionID != "action-after" || input.Stream != "stdout" {
				writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
				return
			}
			markers := 1
			if edge.markerCount > 0 {
				markers = edge.markerCount
			}
			logs := "q02: fresh credential\n"
			for index := 0; index < markers; index++ {
				logs += requestWaitMarker(edge.runMarker) + "\n"
			}
			body, err := json.Marshal(api.ExecutionLogsReadResult{Chunks: []api.LogChunk{
				{ActionID: "action-after", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte(logs)},
			}})
			if err != nil {
				t.Errorf("encode fake logs: %v", err)
				return
			}
			writePublicTestResult(t, writer, "execution.logs.read", string(body))
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/executions/exec-rw-1/cancel":
			if authorization != "Bearer "+requestWaitFakeAdminToken {
				writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
				return
			}
			edge.mu.Lock()
			defer edge.mu.Unlock()
			if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != edge.executionVersion {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			edge.phase = "cancelled"
			edge.executionVersion++
			writePublicTestResult(t, writer, "execution.cancel",
				fmt.Sprintf(`{"execution_version":%d}`, edge.executionVersion))
		default:
			writePublicTestError(writer, http.StatusNotFound, api.CodeNotFound)
		}
	})
}

// requestWaitTestEdge starts one fake edge and returns it with a client.
func requestWaitTestEdge(t *testing.T, configure func(*requestWaitFakeEdge)) (*requestWaitFakeEdge, *API) {
	t.Helper()
	edge := &requestWaitFakeEdge{
		runMarker:     "run-marker-one",
		answer:        "q02-request-wait-answer-run-marker-one",
		tokens:        map[string]bool{},
		keys:          map[string]api.ExecutionRequestResolveResult{},
		actionVersion: requestWaitFakeActionVersion,
		deadline:      time.Now().UTC().Add(4 * time.Minute),
		activated:     time.Now().UTC(),
	}
	if configure != nil {
		configure(edge)
	}
	server, caPath := startAuthorityTLSServer(t, requestWaitFakeHandler(t, edge))
	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build the fake-edge API helper: %v", err)
	}
	return edge, client
}

// requestWaitFakeSecret is one fake credential Secret.
type requestWaitFakeSecret struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data map[string][]byte `json:"data"`
}

// requestWaitFakePod is one fake workload pod.
type requestWaitFakePod struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
	Spec struct {
		Volumes []struct {
			Name   string `json:"name"`
			Secret *struct {
				SecretName string `json:"secretName"`
			} `json:"secret"`
		} `json:"volumes"`
		Containers []struct {
			VolumeMounts []struct {
				Name      string `json:"name"`
				MountPath string `json:"mountPath"`
			} `json:"volumeMounts"`
		} `json:"containers"`
	} `json:"spec"`
}

// requestWaitFakeCluster serves labelled object lists for the probe. It applies
// the kubectl label selector the probe sent, so a selector that drops one of
// the pinned keys is observable in the unit tests. Every fixture object carries
// the complete product label set (internal/shell/runtime/kube labels) and the
// Secret uses the product credential data key (internal/id.SecretTokenName).
type requestWaitFakeCluster struct {
	mu sync.Mutex

	calls []string

	phase       func() string
	fingerprint string
	secretName  string
	token       string

	mutateSecret func(*requestWaitFakeSecret)
	mutatePod    func(*requestWaitFakePod)
	extraSecrets int
	extraPods    int
	// foreignActionSecrets adds Secrets for the same execution and another
	// action, so an action-less selector is ambiguous.
	foreignActionSecrets int
	// secretDelay is the number of Secret list reads that return nothing before
	// the credential Secret materializes (activation lag).
	secretDelay int
	secretReads int
	// podPhase is the pod phase reported while the workload is live.
	podPhase func() string
	podReads int
	// podDelay is the number of pods list reads that return nothing before the
	// workload Pod materializes.
	podDelay int
	// podPhases records the phase served on each pods list read.
	podPhases   []string
	pvcRetained bool
	noPVC       bool
	// jobAction overrides the action label served on the Job list, so a Job
	// that belongs to the successor action is observable.
	jobAction string
	// failPods makes every pods list read fail, so a read error is attributable.
	failPods bool
}

// requestWaitFakeLabels is the complete product label set for one workload.
func requestWaitFakeLabels(action string) map[string]string {
	return map[string]string{
		requestWaitLabelTenant:    requestWaitFakeTenantNS,
		requestWaitLabelExecution: "exec-rw-1",
		requestWaitLabelAction:    action,
		requestWaitLabelWorkload:  "workload-rw-1",
		requestWaitLabelWorkspace: "workspace-rw-1",
	}
}

// requestWaitFakeWorkloadVisible reports whether the fake cluster serves the
// pre-wait workload for a phase. The product keeps that Job and its completed
// Pod until the terminal cleanup (internal/kernel/engine/cleanup.go removeOwned)
// or the Job TTL (cmd/kodama/production.go pins 600 s), so it is still present
// during the request wait and gone once cleanup has completed.
func requestWaitFakeWorkloadVisible(phase string) bool {
	return phase == "before" || phase == "wait"
}

// requestWaitFakeMatches reports whether one object satisfies an equality
// label selector of the form key=value,key=value.
func requestWaitFakeMatches(labels map[string]string, selector string) bool {
	if selector == "" {
		return true
	}
	for term := range strings.SplitSeq(selector, ",") {
		key, value, ok := strings.Cut(term, "=")
		if !ok || labels[key] != value {
			return false
		}
	}
	return true
}

// ObjectList returns one justified fake object list.
func (cluster *requestWaitFakeCluster) ObjectList(_ context.Context, namespace, resource, selector string) ([]byte, error) {
	cluster.mu.Lock()
	cluster.calls = append(cluster.calls, resource+" "+selector)
	cluster.mu.Unlock()
	if namespace != requestWaitFakeTenantNS {
		return nil, fmt.Errorf("namespace %q is outside the fake cluster", namespace)
	}
	if resource == "pods" {
		cluster.mu.Lock()
		cluster.podReads++
		cluster.mu.Unlock()
	}
	phase := "before"
	if cluster.phase != nil {
		phase = cluster.phase()
	}
	secret := requestWaitFakeSecret{Data: map[string][]byte{requestWaitTokenKey: []byte(cluster.token)}}
	secret.Metadata.Name = cluster.secretName
	secret.Metadata.Labels = requestWaitFakeLabels("action-before")
	if cluster.mutateSecret != nil {
		cluster.mutateSecret(&secret)
	}
	podPhase := "Running"
	if cluster.podPhase != nil {
		podPhase = cluster.podPhase()
	}
	pod := requestWaitFakePod{}
	pod.Metadata.Name = "kodama-job-rw-1"
	pod.Metadata.Labels = requestWaitFakeLabels("action-before")
	pod.Status.Phase = podPhase
	pod.Spec.Volumes = append(pod.Spec.Volumes, struct {
		Name   string `json:"name"`
		Secret *struct {
			SecretName string `json:"secretName"`
		} `json:"secret"`
	}{Name: "credential", Secret: &struct {
		SecretName string `json:"secretName"`
	}{SecretName: cluster.secretName}})
	pod.Spec.Containers = append(pod.Spec.Containers, struct {
		VolumeMounts []struct {
			Name      string `json:"name"`
			MountPath string `json:"mountPath"`
		} `json:"volumeMounts"`
	}{VolumeMounts: []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	}{{Name: "credential", MountPath: requestWaitSecretMountPath}}})
	if cluster.mutatePod != nil {
		cluster.mutatePod(&pod)
	}
	switch resource {
	case "secrets":
		cluster.mu.Lock()
		cluster.secretReads++
		reads := cluster.secretReads
		cluster.mu.Unlock()
		items := make([]requestWaitFakeSecret, 0, 1+cluster.extraSecrets+cluster.foreignActionSecrets)
		if phase == "before" && reads > cluster.secretDelay {
			items = append(items, secret)
		}
		for index := 0; index < cluster.extraSecrets; index++ {
			items = append(items, secret)
		}
		for index := 0; index < cluster.foreignActionSecrets; index++ {
			foreign := secret
			foreign.Metadata.Name = fmt.Sprintf("kodama-credential-foreign-%d", index)
			foreign.Metadata.Labels = requestWaitFakeLabels("action-other")
			items = append(items, foreign)
		}
		return json.Marshal(map[string]any{"items": requestWaitSelected(items, selector, func(item requestWaitFakeSecret) map[string]string {
			return item.Metadata.Labels
		})})
	case "pods", "jobs":
		if resource == "pods" && cluster.failPods {
			return nil, fmt.Errorf("the fake cluster refuses the pods list")
		}
		if resource == "jobs" && cluster.jobAction != "" {
			// A successor-workload Job carries the successor's action label.
			pod.Metadata.Labels = requestWaitFakeLabels(cluster.jobAction)
		}
		if phase != "before" && requestWaitFakeWorkloadVisible(phase) && cluster.podPhase == nil {
			// The product keeps the pre-wait Job and its Pod until the
			// execution's terminal cleanup or the Job TTL, so the workload is
			// still attributable while the request waits; by then the Pod has
			// exited. Serving an empty list during the request wait modelled a
			// removal the product never performs.
			pod.Status.Phase = "Succeeded"
		}
		if resource == "pods" {
			cluster.mu.Lock()
			cluster.podPhases = append(cluster.podPhases, pod.Status.Phase)
			cluster.mu.Unlock()
		}
		items := make([]requestWaitFakePod, 0)
		if requestWaitFakeWorkloadVisible(phase) && cluster.podReads > cluster.podDelay {
			items = append(items, pod)
		}
		for index := 0; index < cluster.extraPods; index++ {
			items = append(items, pod)
		}
		return json.Marshal(map[string]any{"items": requestWaitSelected(items, selector, func(item requestWaitFakePod) map[string]string {
			return item.Metadata.Labels
		})})
	case "persistentvolumeclaims":
		if cluster.noPVC {
			return json.Marshal(map[string]any{"items": []any{}})
		}
		retained := cluster.pvcRetained
		claim := map[string]any{
			"metadata": map[string]any{
				"name":        "kodama-workspace-rw-1",
				"labels":      map[string]string{requestWaitLabelWorkspace: "workspace-rw-1", requestWaitRetainedLabel: fmt.Sprintf("%t", retained)},
				"annotations": map[string]string{requestWaitRetainedAnnotation: fmt.Sprintf("%t", retained)},
			},
		}
		if !requestWaitFakeMatches(map[string]string{requestWaitLabelWorkspace: "workspace-rw-1"}, selector) {
			return json.Marshal(map[string]any{"items": []any{}})
		}
		return json.Marshal(map[string]any{"items": []any{claim}})
	default:
		return nil, fmt.Errorf("the fake cluster does not serve resource %q", resource)
	}
}

// requestWaitSelected filters one fake list by the exact selector the probe sent.
func requestWaitSelected[T any](items []T, selector string, labels func(T) map[string]string) []T {
	selected := make([]T, 0, len(items))
	for _, item := range items {
		if requestWaitFakeMatches(labels(item), selector) {
			selected = append(selected, item)
		}
	}
	return selected
}

// podReadCount returns the number of pods list reads so far.
func (cluster *requestWaitFakeCluster) podReadCount() int {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	return cluster.podReads
}

// TestHarnessRequestWaitDocument pins the inline document shape: three ordered
// actions, a container fingerprint producer, a declared service request over
// that exact immutable subject, and a post-wait container.
func TestHarnessRequestWaitDocument(t *testing.T) {
	probe := &requestWaitProbe{
		ServiceID:       "q02-rw-svc-1",
		ProjectID:       requestWaitFakeProject,
		RunMarker:       "run-marker-one",
		Answer:          "q02-answer-one",
		Image:           "docker.io/curlimages/curl@sha256:" + strings.Repeat("a", 64),
		Paths:           requestWaitDefaultPaths(),
		BeforeTimeout:   requestWaitBeforeTimeout,
		DecisionTimeout: requestWaitDecisionTimeout,
		AfterTimeout:    requestWaitAfterTimeout,
		CaptureWindow:   requestWaitCaptureWindow,
	}
	if err := probe.validateDocument(); err != nil {
		t.Fatalf("the pinned probe is not well formed: %v", err)
	}
	document, err := probe.document()
	if err != nil {
		t.Fatalf("build the request-wait document: %v", err)
	}
	decoded, err := pipeline.Decode(document, pipeline.DefaultLimits)
	if err != nil {
		t.Fatalf("the request-wait document was not admitted: %v", err)
	}
	if len(decoded.Actions) != 3 {
		t.Fatalf("actions = %d; want the three ordered actions", len(decoded.Actions))
	}
	before := decoded.Actions[0]
	if before.Name != requestWaitBeforeName || before.Container == nil || before.Request != nil {
		t.Fatalf("first action = %+v; want the container fingerprint producer", before)
	}
	if before.Timeout != requestWaitBeforeTimeout {
		t.Fatalf("pre-wait timeout = %s; want %s", before.Timeout, requestWaitBeforeTimeout)
	}
	if len(before.Container.Outputs) != 1 || string(before.Container.Outputs[0]) != requestWaitFingerprint {
		t.Fatalf("pre-wait outputs = %v; want only the fingerprint", before.Container.Outputs)
	}
	decision := decoded.Actions[1]
	if decision.Name != requestWaitDecisionName || decision.Request == nil || decision.Container != nil {
		t.Fatalf("second action = %+v; want the declared request", decision)
	}
	if decision.Timeout != requestWaitDecisionTimeout {
		t.Fatalf("request timeout = %s; want %s", decision.Timeout, requestWaitDecisionTimeout)
	}
	if decision.Request.Assignment != "service/"+probe.ServiceID {
		t.Fatalf("assignment = %q; want the probe service", decision.Request.Assignment)
	}
	if len(decision.Request.Outputs) != 1 || string(decision.Request.Outputs[0]) != requestWaitAnswerOutput {
		t.Fatalf("request outputs = %v; want only the answer", decision.Request.Outputs)
	}
	fields := decision.Request.Subject.Fields()
	if len(fields) != 2 {
		t.Fatalf("subject fields = %d; want the run marker and fingerprint", len(fields))
	}
	if fields[requestWaitRunField].Kind != "literal" || fields[requestWaitRunField].Literal != probe.RunMarker {
		t.Fatalf("subject run field = %+v; want the literal run marker", fields[requestWaitRunField])
	}
	reference := fields[requestWaitFingerprintField]
	if reference.Kind != "reference" || reference.Reference.Input || reference.Reference.Action != requestWaitBeforeName || reference.Reference.Key != requestWaitFingerprint {
		t.Fatalf("subject fingerprint field = %+v; want before.outputs.fingerprint", reference)
	}
	after := decoded.Actions[2]
	if after.Name != requestWaitAfterName || after.Container == nil || after.Request != nil {
		t.Fatalf("third action = %+v; want the post-wait container", after)
	}
	if after.Timeout != requestWaitAfterTimeout {
		t.Fatalf("post-wait timeout = %s; want %s", after.Timeout, requestWaitAfterTimeout)
	}
}

// TestHarnessRequestWaitProbe drives the WHOLE probe against a fake edge and
// fake cluster. It pins ordered calls, exact credentials, the action-only
// envelope, inbox pagination, the barrier, unchanged decision facts, receipt
// revalidation, output retry behaviour, cleanup and redaction.
func TestHarnessRequestWaitProbe(t *testing.T) {
	edge, client := requestWaitTestEdge(t, nil)
	cluster := &requestWaitFakeCluster{
		phase:       func() string { edge.mu.Lock(); defer edge.mu.Unlock(); return edge.phase },
		fingerprint: requestWaitFakeFingerprint(),
		secretName:  "kodama-credential-rw-1",
		token:       requestWaitFakeExecutionToken,
		pvcRetained: true,
	}
	probe := newRequestWaitProbe(Config{
		AdminToken: Secret(requestWaitFakeAdminToken),
		ProjectID:  requestWaitFakeProject,
		Image:      "docker.io/curlimages/curl@sha256:" + strings.Repeat("a", 64),
	}, client, cluster, "q02-rw-svc-1", edge.runMarker)
	probe.TenantNamespace = requestWaitFakeTenantNS
	cleanup := newRequestWaitCleanup(probe)
	if probe.Cleanup != cleanup {
		t.Fatal("the cleanup was not registered with the probe")
	}

	var runErr error
	stdout := requestWaitCaptureStdout(t, func() { runErr = probe.run(context.Background()) })
	if runErr != nil {
		t.Fatalf("the request-wait probe failed against the fake edge: %v", runErr)
	}
	for _, token := range []string{
		requestWaitFakeAdminToken,
		requestWaitFakeFirstService,
		requestWaitFakeFreshService,
		requestWaitFakeExecutionToken,
	} {
		if strings.Contains(stdout, token) {
			t.Fatal("the successful probe wrote credential material to stdout")
		}
	}
	if probe.MarkerCount != 1 {
		t.Fatalf("marker count = %d; want exactly one retained marker", probe.MarkerCount)
	}
	if probe.FinalInspection.State != "succeeded" || probe.FinalInspection.Cleanup.State != "complete" {
		t.Fatalf("final inspection = %+v; want succeeded with complete cleanup", probe.FinalInspection)
	}
	if probe.ReplayResult != probe.ResolveResult {
		t.Fatalf("the authorized replay result %+v differs from the original %+v", probe.ReplayResult, probe.ResolveResult)
	}
	if probe.BarrierItem.Version != requestWaitFakeActionVersion {
		t.Fatalf("barrier action version = %d; want %d", probe.BarrierItem.Version, requestWaitFakeActionVersion)
	}

	edge.mu.Lock()
	resolvedBeforeWait := edge.resolvedBeforeWait
	serviceVersion := edge.serviceVersion
	edge.mu.Unlock()
	if resolvedBeforeWait {
		t.Fatal("a resolution reached the fake edge before the wait barrier")
	}
	requests := edge.recorded()
	var (
		issues          int
		resolves        []requestWaitFakeRequest
		sawRevoke       bool
		sawStaleResolve bool
	)
	for _, request := range requests {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/credentials"):
			issues++
		case request.Method == http.MethodDelete && strings.HasSuffix(request.Path, "/credentials"):
			sawRevoke = true
		case request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/resolve"):
			resolves = append(resolves, request)
			if request.Authorization == "Bearer "+requestWaitFakeFirstService {
				sawStaleResolve = true
			}
		}
	}
	if issues != 2 {
		t.Fatalf("credential issues = %d; want the initial and fresh ones", issues)
	}
	if !sawRevoke {
		t.Fatal("the admin never revoked the service credential")
	}
	if !sawStaleResolve {
		t.Fatal("the revoked service credential was never exercised on resolve")
	}
	if len(resolves) != 4 {
		t.Fatalf("resolve requests = %d; want stale refusal, resolution, replay and revoked replay", len(resolves))
	}
	successful := resolves[1]
	replay := resolves[2]
	if successful.Authorization != "Bearer "+requestWaitFakeFreshService || replay.Authorization != successful.Authorization {
		t.Fatal("the resolve and its replay did not use the fresh service credential")
	}
	if successful.IdempotencyKey == "" || replay.IdempotencyKey != successful.IdempotencyKey {
		t.Fatal("the authorized replay did not reuse the exact idempotency key")
	}
	if successful.ExpectedVersion != nil || replay.ExpectedVersion != nil {
		t.Fatal("an action-only resolve carried the envelope expected_version")
	}
	if strings.Contains(string(successful.Input), "expected_execution_version") {
		t.Fatal("the resolve body carried the obsolete expected_execution_version")
	}
	var input api.ExecutionRequestResolveRequest
	if err := json.Unmarshal(successful.Input, &input); err != nil {
		t.Fatalf("decode the resolve input: %v", err)
	}
	if input.ExpectedActionVersion != requestWaitFakeActionVersion || input.Decision != "pass" ||
		input.Outputs[requestWaitAnswerOutput] != probe.Answer {
		t.Fatalf("resolve input = %+v; want action-only pass with the declared answer", input)
	}
	if requestWaitFakeHandlerSawExpectedExecutionVersion(t, requests) {
		t.Fatal("an obsolete expected_execution_version member was sent")
	}
	if serviceVersion < 4 {
		t.Fatalf("service version = %d; want revoke and disable to have advanced it", serviceVersion)
	}

	if err := cleanup.run(context.Background()); err != nil {
		t.Fatalf("the strict cleanup failed after a successful probe: %v", err)
	}
	requests = edge.recorded()
	var sawDisable bool
	for _, request := range requests {
		if request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/disable") {
			sawDisable = true
		}
	}
	if !sawDisable {
		t.Fatal("the strict cleanup did not disable the service")
	}
}

// requestWaitFakeHandlerSawExpectedExecutionVersion reports whether a resolve
// input carried the obsolete execution-version member.
func requestWaitFakeHandlerSawExpectedExecutionVersion(t *testing.T, requests []requestWaitFakeRequest) bool {
	t.Helper()
	for _, request := range requests {
		if strings.HasSuffix(request.Path, "/resolve") && strings.Contains(string(request.Input), "expected_execution_version") {
			return true
		}
	}
	return false
}

// TestHarnessRequestWaitProbeRejectsNon401StaleToken proves a non-401 refusal
// (for example 403 or a transport failure) is never accepted as revocation.
func TestHarnessRequestWaitProbeRejectsNon401StaleToken(t *testing.T) {
	edge, client := requestWaitTestEdge(t, func(edge *requestWaitFakeEdge) {
		edge.staleStatus = http.StatusForbidden
	})
	cluster := &requestWaitFakeCluster{
		phase:       func() string { edge.mu.Lock(); defer edge.mu.Unlock(); return edge.phase },
		fingerprint: requestWaitFakeFingerprint(),
		secretName:  "kodama-credential-rw-1",
		token:       requestWaitFakeExecutionToken,
		pvcRetained: true,
	}
	probe := newRequestWaitProbe(Config{
		AdminToken: Secret(requestWaitFakeAdminToken),
		ProjectID:  requestWaitFakeProject,
		Image:      "docker.io/curlimages/curl@sha256:" + strings.Repeat("a", 64),
	}, client, cluster, "q02-rw-svc-1", edge.runMarker)
	probe.TenantNamespace = requestWaitFakeTenantNS
	newRequestWaitCleanup(probe)
	err := probe.run(context.Background())
	if err == nil {
		t.Fatal("the probe accepted a 403 stale-credential refusal as revocation")
	}
	if !strings.Contains(err.Error(), "status 403") ||
		(!strings.Contains(err.Error(), "revoked service credential's task.inbox") &&
			!strings.Contains(err.Error(), "revoked service credential's execution.request.resolve")) {
		t.Fatalf("the probe did not refuse the stale token on both refused surfaces: %v", err)
	}
}

// TestHarnessRequestWaitBarrier pin the barrier predicate family: each required
// decision fact is individually refused when it is wrong.
func TestHarnessRequestWaitBarrier(t *testing.T) {
	edge, _ := requestWaitTestEdge(t, func(edge *requestWaitFakeEdge) {
		edge.phase = "wait"
	})
	inspection := edge.inspection()
	item := edge.inboxItem()
	if err := requestWaitValidateBarrier(inspection, item, edge.serviceID, edge.runMarker); err != nil {
		t.Fatalf("the well-formed wait barrier was refused: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*api.ExecutionInspectResult, *api.InboxItem)
		want   string
	}{
		{"predecessor not succeeded", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			inspect.Actions[0].State = "running"
		}, "pre-wait action state"},
		{"predecessor stop unconfirmed", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			inspect.Actions[0].Stop = &api.ExecutionStopEvidence{State: "unconfirmed"}
		}, "stop is not confirmed"},
		{"request not running", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			inspect.Actions[1].State = "pending"
		}, "request action kind/state"},
		{"wrong assignment", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			inspect.Actions[1].Assignment = "service/other-service"
		}, "request assignment"},
		{"subject run marker changed", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			inspect.Actions[1].Subject.Fields[0].Value = "other-run"
		}, "run marker"},
		{"successor activated", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			now := time.Now().UTC()
			inspect.Actions[2].ActivatedAt = &now
		}, "activated before the request"},
		{"successor not pending", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			inspect.Actions[2].State = "running"
		}, "successor state"},
		{"output declaration changed", func(_ *api.ExecutionInspectResult, item *api.InboxItem) {
			item.Outputs = []string{"other"}
		}, "declared response outputs"},
		{"inbox assignment changed", func(_ *api.ExecutionInspectResult, item *api.InboxItem) {
			item.Assignment = "service/other-service"
		}, "inbox assignment"},
		{"inbox names another action", func(_ *api.ExecutionInspectResult, item *api.InboxItem) {
			item.ActionID = "action-other"
		}, "inbox item does not name the running request"},
		{"inbox version is stale", func(_ *api.ExecutionInspectResult, item *api.InboxItem) {
			item.Version++
		}, "inbox version"},
		{"inbox deadline differs", func(_ *api.ExecutionInspectResult, item *api.InboxItem) {
			item.At = item.At.Add(time.Second)
		}, "inbox deadline"},
		{"inbox subject differs from the inspection", func(_ *api.ExecutionInspectResult, item *api.InboxItem) {
			item.Subject.Fields[1].Value = "other-fingerprint"
		}, "inbox subject does not match"},
		{"request subject fingerprint is not the settled output", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			inspect.Actions[1].Subject.Fields[1].Value = "other-fingerprint"
		}, "subject fingerprint"},
		{"request has no deadline", func(inspect *api.ExecutionInspectResult, _ *api.InboxItem) {
			inspect.Actions[1].Deadline = nil
		}, "no deadline"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inspect := inspection
			inspect.Actions = append([]api.ExecutionAction(nil), inspection.Actions...)
			for index := range inspect.Actions {
				if inspect.Actions[index].Subject != nil {
					copied := *inspect.Actions[index].Subject
					copied.Fields = append([]api.SubjectField(nil), inspect.Actions[index].Subject.Fields...)
					inspect.Actions[index].Subject = &copied
				}
			}
			copiedItem := item
			if item.Subject != nil {
				copied := *item.Subject
				copied.Fields = append([]api.SubjectField(nil), item.Subject.Fields...)
				copiedItem.Subject = &copied
			}
			testCase.mutate(&inspect, &copiedItem)
			err := requestWaitValidateBarrier(inspect, copiedItem, edge.serviceID, edge.runMarker)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("barrier violation %q returned %v", testCase.name, err)
			}
		})
	}
}

// TestHarnessRequestWaitCredentialCapture pins the capture guard: exactly one
// Secret for the observed execution/action, matching labels, the product data
// key, and a pod that mounts that Secret at the reserved path. Every refusal is
// token-free.
func TestHarnessRequestWaitCredentialCapture(t *testing.T) {
	newProbe := func(cluster *requestWaitFakeCluster) *requestWaitProbe {
		return &requestWaitProbe{
			Cluster:             cluster,
			TenantNamespace:     requestWaitFakeTenantNS,
			Paths:               requestWaitDefaultPaths(),
			MaterializationWait: time.Second,
			MaterializationPoll: 5 * time.Millisecond,
		}
	}
	newCluster := func() *requestWaitFakeCluster {
		return &requestWaitFakeCluster{
			phase:       func() string { return "before" },
			fingerprint: requestWaitFakeFingerprint(),
			secretName:  "kodama-credential-rw-1",
			token:       requestWaitFakeExecutionToken,
			pvcRetained: true,
		}
	}
	t.Run("valid association", func(t *testing.T) {
		captured, err := newProbe(newCluster()).captureCredential(context.Background(), "exec-rw-1", "action-before")
		if err != nil {
			t.Fatalf("capture the valid credential: %v", err)
		}
		if captured.Token.Reveal() != requestWaitFakeExecutionToken || captured.WorkspaceID != "workspace-rw-1" {
			t.Fatal("the captured credential is not the observed execution/action credential")
		}
	})
	t.Run("materialization lags the running action", func(t *testing.T) {
		cluster := newCluster()
		cluster.secretDelay = 3
		captured, err := newProbe(cluster).captureCredential(context.Background(), "exec-rw-1", "action-before")
		if err != nil {
			t.Fatalf("a credential Secret that materialized after the action ran was not captured: %v", err)
		}
		if captured.Token.Reveal() != requestWaitFakeExecutionToken {
			t.Fatal("the late capture is not the observed execution/action credential")
		}
		if cluster.secretReads != 4 {
			t.Fatalf("credential Secret reads = %d; want the three empty reads and the materialized one", cluster.secretReads)
		}
	})
	t.Run("selector keeps the action key", func(t *testing.T) {
		cluster := newCluster()
		cluster.foreignActionSecrets = 1
		captured, err := newProbe(cluster).captureCredential(context.Background(), "exec-rw-1", "action-before")
		if err != nil {
			t.Fatalf("a same-execution Secret for another action made the capture ambiguous: %v", err)
		}
		if captured.SecretName != "kodama-credential-rw-1" {
			t.Fatalf("the capture selected %q; want the observed action's Secret", captured.SecretName)
		}
		var sawSelector bool
		for _, call := range cluster.calls {
			if strings.HasPrefix(call, "secrets ") &&
				strings.Contains(call, requestWaitLabelExecution+"=exec-rw-1") &&
				strings.Contains(call, requestWaitLabelAction+"=action-before") {
				sawSelector = true
			}
		}
		if !sawSelector {
			t.Fatalf("the capture never read the execution/action selector: %v", cluster.calls)
		}
	})
	cases := []struct {
		name   string
		mutate func(*requestWaitFakeCluster)
	}{
		{"two secrets", func(cluster *requestWaitFakeCluster) { cluster.extraSecrets = 1 }},
		{"wrong action label", func(cluster *requestWaitFakeCluster) {
			cluster.mutateSecret = func(secret *requestWaitFakeSecret) { secret.Metadata.Labels[requestWaitLabelAction] = "other" }
		}},
		{"wrong execution label", func(cluster *requestWaitFakeCluster) {
			cluster.mutateSecret = func(secret *requestWaitFakeSecret) { secret.Metadata.Labels[requestWaitLabelExecution] = "other" }
		}},
		{"missing workload label", func(cluster *requestWaitFakeCluster) {
			cluster.mutateSecret = func(secret *requestWaitFakeSecret) { delete(secret.Metadata.Labels, requestWaitLabelWorkload) }
		}},
		{"workload label disagrees with the pod", func(cluster *requestWaitFakeCluster) {
			cluster.mutateSecret = func(secret *requestWaitFakeSecret) {
				secret.Metadata.Labels[requestWaitLabelWorkload] = "workload-other"
			}
		}},
		{"workspace label disagrees with the pod", func(cluster *requestWaitFakeCluster) {
			cluster.mutateSecret = func(secret *requestWaitFakeSecret) {
				secret.Metadata.Labels[requestWaitLabelWorkspace] = "workspace-other"
			}
		}},
		{"token under a non-product data key", func(cluster *requestWaitFakeCluster) {
			cluster.mutateSecret = func(secret *requestWaitFakeSecret) {
				secret.Data = map[string][]byte{"token": secret.Data[requestWaitTokenKey]}
			}
		}},
		{"missing token", func(cluster *requestWaitFakeCluster) {
			cluster.mutateSecret = func(secret *requestWaitFakeSecret) { secret.Data = map[string][]byte{} }
		}},
		{"two pods", func(cluster *requestWaitFakeCluster) { cluster.extraPods = 1 }},
		{"pod not mounting the secret", func(cluster *requestWaitFakeCluster) {
			cluster.mutatePod = func(pod *requestWaitFakePod) { pod.Spec.Volumes[0].Secret.SecretName = "other-secret" }
		}},
		{"secret mounted at the wrong path", func(cluster *requestWaitFakeCluster) {
			cluster.mutatePod = func(pod *requestWaitFakePod) { pod.Spec.Containers[0].VolumeMounts[0].MountPath = "/tmp/secrets" }
		}},
		{"pod labels another action", func(cluster *requestWaitFakeCluster) {
			cluster.mutatePod = func(pod *requestWaitFakePod) { pod.Metadata.Labels[requestWaitLabelAction] = "other" }
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cluster := newCluster()
			testCase.mutate(cluster)
			_, err := newProbe(cluster).captureCredential(context.Background(), "exec-rw-1", "action-before")
			if err == nil {
				t.Fatalf("the capture accepted %s", testCase.name)
			}
			if strings.Contains(err.Error(), requestWaitFakeExecutionToken) {
				t.Fatalf("the capture error leaked the credential: %v", err)
			}
		})
	}
	t.Run("timeout diagnostic names the selector keys and counts", func(t *testing.T) {
		cluster := newCluster()
		cluster.secretDelay = 1 << 20
		probe := newProbe(cluster)
		probe.MaterializationWait = 20 * time.Millisecond
		_, err := probe.captureCredential(context.Background(), "exec-rw-1", "action-before")
		if err == nil {
			t.Fatal("the capture accepted a Secret that never materialized")
		}
		for _, want := range []string{requestWaitLabelExecution, requestWaitLabelAction, "0 Secrets", "attempts", "want exactly one"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the capture timeout diagnostic %q does not name %q", err, want)
			}
		}
		if strings.Contains(err.Error(), requestWaitFakeExecutionToken) {
			t.Fatalf("the capture timeout leaked the credential: %v", err)
		}
	})
}

// TestHarnessRequestWaitPinsProductVocabulary refuses drift from the product's
// pinned label keys and credential data key. The live probe selects only
// execution/action-labelled Secrets; if the product renamed a key, every live
// capture would match nothing, so the harness fails here first.
func TestHarnessRequestWaitPinsProductVocabulary(t *testing.T) {
	if requestWaitLabelTenant != runtimekube.LabelTenant ||
		requestWaitLabelExecution != runtimekube.LabelExecution ||
		requestWaitLabelAction != runtimekube.LabelAction ||
		requestWaitLabelWorkload != runtimekube.LabelWorkload ||
		requestWaitLabelWorkspace != runtimekube.LabelWorkspace {
		t.Fatalf("the request-wait label keys do not match internal/shell/runtime/kube: tenant=%q execution=%q action=%q workload=%q workspace=%q",
			requestWaitLabelTenant, requestWaitLabelExecution, requestWaitLabelAction,
			requestWaitLabelWorkload, requestWaitLabelWorkspace)
	}
	if requestWaitTokenKey != id.SecretTokenName {
		t.Fatalf("the credential data key %q is not internal/id.SecretTokenName %q", requestWaitTokenKey, id.SecretTokenName)
	}
}

// TestHarnessRequestWaitCommands runs the generated workload commands with the
// real curl and sha256sum. The pre-wait submit retries a 409; the post-wait
// action refuses a reused credential and an unreadable answer.
func TestHarnessRequestWaitCommands(t *testing.T) {
	for _, tool := range []string{"curl", "sha256sum", "cut", "grep", "head", "cat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("real %s is not available on the test host", tool)
		}
	}
	dir := t.TempDir()
	paths := requestWaitPaths{
		TokenFile:    filepath.Join(dir, "KODAMA_TOKEN"),
		CAFile:       filepath.Join(dir, "ca.crt"),
		BeforeOutput: filepath.Join(dir, "fingerprint"),
		AnswerOutput: filepath.Join(dir, "answer"),
	}
	probe := &requestWaitProbe{Paths: paths, Answer: "q02-command-answer", RunMarker: "run-marker-one", CaptureWindow: 0}

	var (
		mu       sync.Mutex
		puts     []string
		getCount int
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/executions/exec-one":
			mu.Lock()
			getCount++
			mu.Unlock()
			writer.Header().Set("Content-Type", api.MediaType)
			_, _ = writer.Write([]byte(fakeInspectBody))
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/executions/exec-one/actions/action-one/outputs":
			mu.Lock()
			puts = append(puts, string(body))
			count := len(puts)
			mu.Unlock()
			if count <= 2 {
				writePublicTestError(writer, http.StatusConflict, api.CodeVersionConflict)
				return
			}
			writePublicTestResult(t, writer, "execution.action.outputs", `{"execution_version":8,"action_version":2}`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	mustWriteFile(t, paths.CAFile, "unused for plain HTTP")
	mustWriteFile(t, paths.TokenFile, "token-one")
	if err := runShellURL(t, joinCommands(probe.beforeCommands()), server.URL); err != nil {
		t.Fatalf("the pre-wait command failed: %v", err)
	}
	mu.Lock()
	putCount := len(puts)
	mu.Unlock()
	if putCount != 3 {
		t.Fatalf("output submissions = %d; want two refused and one accepted", putCount)
	}
	fingerprint := sha256Hex("token-one")
	for index, put := range puts {
		if !strings.Contains(put, `"fingerprint":"`+fingerprint+`"`) {
			t.Fatalf("submission %d does not carry the mounted credential fingerprint", index)
		}
	}

	mustWriteFile(t, paths.BeforeOutput, fingerprint+"\n")
	mustWriteFile(t, paths.AnswerOutput, probe.Answer)
	mustWriteFile(t, paths.TokenFile, "token-two")
	if err := runShellURL(t, joinCommands(probe.afterCommands()), server.URL); err != nil {
		t.Fatalf("the post-wait command failed: %v", err)
	}
	mu.Lock()
	gets := getCount
	mu.Unlock()
	if gets < 3 {
		t.Fatalf("execution GETs = %d; want the pre-wait inspections and the fresh post-wait one", gets)
	}

	mustWriteFile(t, paths.TokenFile, "token-one")
	if err := runShellURL(t, joinCommands(probe.afterCommands()), server.URL); err == nil ||
		!strings.Contains(err.Error(), "reused the old mounted credential") {
		t.Fatalf("the post-wait command accepted a reused credential: %v", err)
	}
	mustWriteFile(t, paths.TokenFile, "token-two")
	if err := os.Remove(paths.AnswerOutput); err != nil {
		t.Fatalf("remove the answer fixture: %v", err)
	}
	if err := runShellURL(t, joinCommands(probe.afterCommands()), server.URL); err == nil {
		t.Fatal("the post-wait command accepted an unreadable answer")
	}
}

// TestHarnessRequestWaitCleanupOnFailure proves the strict cleanup always
// attempts cancellation, revocation, disable and refusal checks, reports every
// failure, and never accepts a non-401 refusal as revocation.
func TestHarnessRequestWaitCleanupOnFailure(t *testing.T) {
	newProbe := func(client *API) *requestWaitProbe {
		return &requestWaitProbe{
			Client:          client,
			AdminToken:      Secret(requestWaitFakeAdminToken),
			ProjectID:       requestWaitFakeProject,
			TenantNamespace: requestWaitFakeTenantNS,
			ServiceID:       "q02-rw-svc-1",
			serviceVersion:  1,
			serviceToken:    Secret(requestWaitFakeFirstService),
		}
	}
	newCluster := func() *requestWaitFakeCluster {
		return &requestWaitFakeCluster{
			phase:       func() string { return "before" },
			fingerprint: requestWaitFakeFingerprint(),
			secretName:  "kodama-credential-rw-1",
			token:       requestWaitFakeExecutionToken,
			pvcRetained: true,
		}
	}

	t.Run("all failures are reported and every step is attempted", func(t *testing.T) {
		edge, client := requestWaitTestEdge(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.executionVersion = 1
			edge.serviceVersion = 1
			edge.tokens[requestWaitFakeFirstService] = true
		})
		probe := newProbe(client)
		probe.Cluster = newCluster()
		probe.Cluster.(*requestWaitFakeCluster).phase = func() string {
			edge.mu.Lock()
			defer edge.mu.Unlock()
			return edge.phase
		}
		cleanup := newRequestWaitCleanup(probe)
		cleanup.executionID = "exec-rw-1"
		cleanup.serviceID = probe.ServiceID
		cleanup.version = 1
		cleanup.token = probe.serviceToken
		// Replace the main handler with one that refuses both lifecycle
		// mutations while still serving inspect and cancel.
		server, caPath := startAuthorityTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			switch {
			case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/cancel"):
				edge.record(t, request)
				edge.mu.Lock()
				edge.phase = "cancelled"
				edge.mu.Unlock()
				writePublicTestResult(t, writer, "execution.cancel", `{"execution_version":2}`)
			case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/credentials"):
				edge.record(t, request)
				writePublicTestError(writer, http.StatusForbidden, api.CodeForbidden)
			case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/disable"):
				edge.record(t, request)
				writePublicTestError(writer, http.StatusForbidden, api.CodeForbidden)
			case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/executions/exec-rw-1"):
				edge.mu.Lock()
				phase := edge.phase
				edge.mu.Unlock()
				if phase == "cancelled" {
					writePublicTestResult(t, writer, "execution.inspect",
						`{"execution_id":"exec-rw-1","state":"cancelled","version":2,"current_action":-1,"actions":[],"cleanup":{"state":"complete"}}`)
					return
				}
				writePublicTestResult(t, writer, "execution.inspect",
					`{"execution_id":"exec-rw-1","state":"running","version":1,"current_action":1,"actions":[],"cleanup":{"state":"none"}}`)
			default:
				writePublicTestError(writer, http.StatusNotFound, api.CodeNotFound)
			}
		}))
		refused, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
		if err != nil {
			t.Fatalf("build the refusing API helper: %v", err)
		}
		probe.Client = refused
		cleanup.probe = probe
		err = cleanup.run(context.Background())
		if err == nil {
			t.Fatal("the strict cleanup did not report the refused revoke and disable")
		}
		for _, want := range []string{"service.credential.revoke", "service.disable"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the strict cleanup error %q does not name %q", err, want)
			}
		}
		var sawCancel, sawRevoke, sawDisable bool
		for _, request := range edge.recorded() {
			switch {
			case strings.HasSuffix(request.Path, "/cancel"):
				sawCancel = true
			case request.Method == http.MethodDelete && strings.HasSuffix(request.Path, "/credentials"):
				sawRevoke = true
			case strings.HasSuffix(request.Path, "/disable"):
				sawDisable = true
			}
		}
		if !sawCancel || !sawRevoke || !sawDisable {
			t.Fatalf("cleanup attempts = cancel:%t revoke:%t disable:%t; want every step", sawCancel, sawRevoke, sawDisable)
		}
	})

	t.Run("successful cleanup cancels, revokes, disables and verifies refusal", func(t *testing.T) {
		edge, client := requestWaitTestEdge(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.executionVersion = 1
			edge.serviceVersion = 1
			edge.tokens[requestWaitFakeFirstService] = true
		})
		probe := newProbe(client)
		probe.Cluster = newCluster()
		probe.Cluster.(*requestWaitFakeCluster).phase = func() string {
			edge.mu.Lock()
			defer edge.mu.Unlock()
			return edge.phase
		}
		cleanup := newRequestWaitCleanup(probe)
		cleanup.executionID = "exec-rw-1"
		cleanup.serviceID = probe.ServiceID
		cleanup.version = 1
		cleanup.token = probe.serviceToken
		if err := cleanup.run(context.Background()); err != nil {
			t.Fatalf("the successful strict cleanup failed: %v", err)
		}
		var sawCancel, sawRevoke, sawDisable, sawRefusal bool
		for _, request := range edge.recorded() {
			switch {
			case strings.HasSuffix(request.Path, "/cancel"):
				sawCancel = true
			case request.Method == http.MethodDelete && strings.HasSuffix(request.Path, "/credentials"):
				sawRevoke = true
			case strings.HasSuffix(request.Path, "/disable"):
				sawDisable = true
			case request.Method == http.MethodGet && strings.HasSuffix(request.Path, "/executions"):
				if request.Authorization == "Bearer "+requestWaitFakeFirstService {
					sawRefusal = true
				}
			}
		}
		if !sawCancel || !sawRevoke || !sawDisable || !sawRefusal {
			t.Fatalf("cleanup = cancel:%t revoke:%t disable:%t refusal:%t", sawCancel, sawRevoke, sawDisable, sawRefusal)
		}
	})
}

// TestHarnessRequestWaitCleanupWaitsForPendingWorkloadBeforeCancel proves the
// failure-path cleanup does not cancel a workload whose pod is still Pending:
// it first observes the pod leave Pending, so the cancel can record a confirmed
// stop instead of F-4's "0 workload pods observed". The wait is bounded, so a
// pod that never starts still gets cancelled.
func TestHarnessRequestWaitCleanupWaitsForPendingWorkloadBeforeCancel(t *testing.T) {
	newCleanup := func(t *testing.T, pending time.Duration) (*requestWaitFixture, *requestWaitCleanup) {
		t.Helper()
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.executionVersion = 1
			edge.serviceVersion = 1
			edge.tokens[requestWaitFakeFirstService] = true
		})
		cleanup := newRequestWaitCleanup(fixture.probe)
		cleanup.executionID = "exec-rw-1"
		cleanup.serviceID = fixture.probe.ServiceID
		cleanup.version = 1
		cleanup.token = Secret(requestWaitFakeFreshService)
		cleanup.pending = pending
		return fixture, cleanup
	}

	t.Run("cancel waits for the pending pod to start", func(t *testing.T) {
		fixture, cleanup := newCleanup(t, 5*time.Second)
		cluster := fixture.cluster
		cluster.podPhase = func() string {
			fixture.edge.mu.Lock()
			phase := fixture.edge.phase
			fixture.edge.mu.Unlock()
			if phase == "cancelled" {
				return "Succeeded"
			}
			if cluster.podReadCount() <= 1 {
				return "Pending"
			}
			return "Running"
		}
		if err := cleanup.run(context.Background()); err != nil {
			t.Fatalf("the strict cleanup failed: %v", err)
		}
		phases := cluster.podPhases
		if len(phases) < 2 || phases[0] != "Pending" || phases[1] != "Running" {
			t.Fatalf("pod phases observed around the cancel = %v; want Pending then Running before any post-cancel read", phases)
		}
	})

	t.Run("the pending wait is bounded", func(t *testing.T) {
		fixture, cleanup := newCleanup(t, 300*time.Millisecond)
		cluster := fixture.cluster
		cluster.podPhase = func() string { return "Pending" }
		started := time.Now()
		if err := cleanup.run(context.Background()); err != nil {
			t.Fatalf("the strict cleanup failed: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("the bounded pending wait took %s; want the cancel to proceed anyway", elapsed)
		}
		var cancelled bool
		for _, request := range fixture.edge.recorded() {
			if strings.HasSuffix(request.Path, "/cancel") {
				cancelled = true
			}
		}
		if !cancelled {
			t.Fatal("a pod that never left Pending prevented the cancellation")
		}
	})

	t.Run("cancel waits for a not-yet-materialized pod", func(t *testing.T) {
		fixture, cleanup := newCleanup(t, 5*time.Second)
		fixture.cluster.podDelay = 2
		fixture.cluster.podPhase = func() string { return "Running" }
		if err := cleanup.run(context.Background()); err != nil {
			t.Fatalf("the strict cleanup failed: %v", err)
		}
		// Two empty reads, then the materialized Running pod, then one post-cancel
		// read. Cancelling before the pod appeared would read the list once only.
		if reads := fixture.cluster.podReadCount(); reads < 4 {
			t.Fatalf("pods list reads = %d; want the cleanup to wait for the workload Pod to appear before cancelling", reads)
		}
	})
}

// TestHarnessRequestWaitCleanupAuthorityFirst proves the service-authority
// teardown never depends on the workload cleanup converging: revoke, disable
// and the revoked-token refusal run on their own bounded context before the
// cancel and the workload waits, and a refused revoke still lets the cancel and
// the workload waits run.
func TestHarnessRequestWaitCleanupAuthorityFirst(t *testing.T) {
	newCleanup := func(t *testing.T, configure func(*requestWaitFakeEdge)) (*requestWaitFixture, *requestWaitCleanup) {
		t.Helper()
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.executionVersion = 1
			edge.serviceVersion = 1
			edge.tokens[requestWaitFakeFirstService] = true
			if configure != nil {
				configure(edge)
			}
		})
		cleanup := newRequestWaitCleanup(fixture.probe)
		cleanup.executionID = "exec-rw-1"
		cleanup.serviceID = fixture.probe.ServiceID
		cleanup.version = 1
		cleanup.token = Secret(requestWaitFakeFirstService)
		cleanup.pending = 100 * time.Millisecond
		cleanup.settle = 200 * time.Millisecond
		return fixture, cleanup
	}
	requestIndexes := func(fixture *requestWaitFixture) (revoke, disable, cancel, refusal int) {
		revoke, disable, cancel, refusal = -1, -1, -1, -1
		for index, request := range fixture.edge.recorded() {
			switch {
			case request.Method == http.MethodDelete && strings.HasSuffix(request.Path, "/credentials"):
				revoke = index
			case request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/disable"):
				disable = index
			case strings.HasSuffix(request.Path, "/cancel"):
				cancel = index
			case request.Method == http.MethodGet && strings.HasSuffix(request.Path, "/executions") &&
				request.Authorization == "Bearer "+requestWaitFakeFirstService:
				refusal = index
			}
		}
		return revoke, disable, cancel, refusal
	}

	t.Run("a workload cleanup that never converges still leaves no live service credential", func(t *testing.T) {
		fixture, cleanup := newCleanup(t, func(edge *requestWaitFakeEdge) {
			// The known product finding F-4: the execution is cancelled but its
			// cleanup never converges, so the workload waits consume their whole
			// budget.
			edge.cleanupState = "pending"
		})
		fixture.cluster.podPhase = func() string { return "Succeeded" }
		err := cleanup.run(context.Background())
		if err == nil {
			t.Fatal("the strict cleanup accepted a workload cleanup that never converged")
		}
		if !strings.Contains(err.Error(), "cleanup did not converge") {
			t.Fatalf("the stuck-cleanup error %q does not report the non-converging cleanup", err)
		}
		live, disabled := fixture.edge.serviceState()
		if live != 0 || !disabled {
			t.Fatalf("after the stuck workload cleanup: live service credentials %d, disabled %t; want 0 and disabled", live, disabled)
		}
		revoke, disable, cancel, refusal := requestIndexes(fixture)
		if revoke < 0 || disable < 0 || refusal < 0 {
			t.Fatalf("stuck workload cleanup: revoke %d disable %d refusal %d; want all attempted", revoke, disable, refusal)
		}
		if cancel >= 0 && (revoke > cancel || disable > cancel) {
			t.Fatalf("service teardown at %d/%d ran after the cancel at %d; want the authority first", revoke, disable, cancel)
		}
		if refusal < revoke || refusal < disable {
			t.Fatalf("the revoked-token refusal at %d did not follow the revoke %d and disable %d", refusal, revoke, disable)
		}
	})

	t.Run("a refused cancel still revokes and disables the service", func(t *testing.T) {
		defer requestWaitShortenWait(&requestWaitRemovalWait, 200*time.Millisecond)()
		fixture, cleanup := newCleanup(t, func(edge *requestWaitFakeEdge) {
			edge.refuse = "execution.cancel"
		})
		fixture.cluster.podPhase = func() string { return "Succeeded" }
		err := cleanup.run(context.Background())
		if err == nil {
			t.Fatal("the strict cleanup accepted a refused execution.cancel")
		}
		if !strings.Contains(err.Error(), "execution.cancel") {
			t.Fatalf("the cleanup error %q does not name the refused cancel", err)
		}
		live, disabled := fixture.edge.serviceState()
		if live != 0 || !disabled {
			t.Fatalf("after the refused cancel: live service credentials %d, disabled %t; want 0 and disabled", live, disabled)
		}
		revoke, disable, cancel, _ := requestIndexes(fixture)
		if revoke < 0 || disable < 0 || cancel < 0 {
			t.Fatalf("refused cancel: revoke %d disable %d cancel %d; want every step attempted", revoke, disable, cancel)
		}
		if revoke > cancel || disable > cancel {
			t.Fatalf("service teardown at %d/%d ran after the refused cancel at %d; want the authority first", revoke, disable, cancel)
		}
	})

	t.Run("a refused revoke still cancels and runs the workload cleanup", func(t *testing.T) {
		fixture, cleanup := newCleanup(t, func(edge *requestWaitFakeEdge) {
			edge.refuse = "service.credential.revoke"
		})
		fixture.cluster.podPhase = func() string { return "Succeeded" }
		err := cleanup.run(context.Background())
		if err == nil {
			t.Fatal("the strict cleanup accepted a refused service.credential.revoke")
		}
		if !strings.Contains(err.Error(), "service.credential.revoke") {
			t.Fatalf("the cleanup error %q does not name the refused revoke", err)
		}
		revoke, disable, cancel, _ := requestIndexes(fixture)
		if revoke < 0 || disable < 0 || cancel < 0 {
			t.Fatalf("refused revoke: revoke %d disable %d cancel %d; want the cancel and disable to still run", revoke, disable, cancel)
		}
		if cancel < revoke {
			t.Fatalf("the cancel at %d ran before the refused revoke at %d; want both attempted", cancel, revoke)
		}
		if fixture.cluster.podReadCount() == 0 {
			t.Fatal("the workload cleanup waits never ran after the refused revoke")
		}
	})
}

// TestHarnessRequestWaitNegativeAssertions is the negative table for every
// core revocation assertion: the fake edge serves each deviation and the probe
// must fail with the assertion's named message. Each case would pass if its
// assertion were deleted, loosened or pointed at another credential.
func TestHarnessRequestWaitNegativeAssertions(t *testing.T) {
	t.Run("captured credential after activation", func(t *testing.T) {
		for _, status := range []int{http.StatusOK, http.StatusForbidden} {
			fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
				edge.phase = "wait"
				edge.serviceID = "q02-rw-svc-1"
				edge.capturedAfterWait = status
			})
			fixture.probe.credential = Secret(requestWaitFakeExecutionToken)
			err := fixture.probe.assertCapturedCredentialRefused(context.Background(), "exec-rw-1", fixture.edge.inspection())
			if err == nil {
				t.Fatalf("the probe accepted the captured execution credential with status %d after activation", status)
			}
			if !strings.Contains(err.Error(), "the captured execution credential after request activation") {
				t.Fatalf("the captured-credential refusal %q does not name its assertion", err)
			}
			var exercised bool
			for _, request := range fixture.edge.recorded() {
				if requestWaitFakeOperation(request.Method, request.Path) == "execution.inspect" &&
					request.Authorization == "Bearer "+requestWaitFakeExecutionToken {
					exercised = true
				}
			}
			if !exercised {
				t.Fatal("the captured-credential assertion never inspected with the captured token")
			}
		}
	})

	t.Run("captured credential accepted after activation fails the whole run", func(t *testing.T) {
		for _, status := range []int{http.StatusOK, http.StatusForbidden} {
			fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
				edge.capturedAfterWait = status
			})
			newRequestWaitCleanup(fixture.probe)
			err := fixture.probe.run(context.Background())
			if err == nil {
				t.Fatalf("the whole probe accepted the captured execution credential with status %d after activation", status)
			}
			if !strings.Contains(err.Error(), "the captured execution credential after request activation") {
				t.Fatalf("the whole-run captured-credential refusal %q does not name its assertion", err)
			}
		}
	})

	t.Run("captured credential is not the pre-wait mounted token", func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "wait"
			edge.serviceID = "q02-rw-svc-1"
			edge.fingerprintOverride = sha256Hex("some-other-mounted-credential")
		})
		fixture.probe.credential = Secret(requestWaitFakeExecutionToken)
		err := fixture.probe.assertCapturedCredentialRefused(context.Background(), "exec-rw-1", fixture.edge.inspection())
		if err == nil {
			t.Fatal("the probe accepted a captured credential that is not the pre-wait mounted token")
		}
		if !strings.Contains(err.Error(), "not the pre-wait action's mounted credential") {
			t.Fatalf("the fingerprint mismatch %q does not name the captured credential", err)
		}
	})

	t.Run("request unchanged after refused authority", func(t *testing.T) {
		cases := []struct {
			name      string
			configure func(*requestWaitFakeEdge)
			cluster   func(*requestWaitFakeCluster)
			want      string
		}{
			{
				name:      "recorded decision",
				configure: func(edge *requestWaitFakeEdge) { edge.afterDecision = true },
				want:      "the refused resolution recorded a decision",
			},
			{
				name:      "activated successor",
				configure: func(edge *requestWaitFakeEdge) { edge.successorActivated = true },
				want:      "the refused resolution advanced the successor",
			},
			{
				name:      "advanced execution",
				configure: func(edge *requestWaitFakeEdge) { edge.waitingState = "succeeded" },
				want:      "the execution advanced to",
			},
			{
				name:    "live successor workload",
				cluster: func(cluster *requestWaitFakeCluster) { cluster.podPhase = func() string { return "Running" } },
				want:    "did not leave the successor idle",
			},
			{
				name:    "successor workload job",
				cluster: func(cluster *requestWaitFakeCluster) { cluster.jobAction = "action-after" },
				want:    "did not leave the successor idle",
			},
			{
				name:    "workspace claim not retained",
				cluster: func(cluster *requestWaitFakeCluster) { cluster.pvcRetained = false },
				want:    "not marked retained across the request wait",
			},
			{
				name:    "workspace claim missing",
				cluster: func(cluster *requestWaitFakeCluster) { cluster.noPVC = true },
				want:    "matched 0 claims",
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
					edge.phase = "wait"
					edge.serviceID = "q02-rw-svc-1"
					if testCase.configure != nil {
						testCase.configure(edge)
					}
				})
				fixture.probe.workspaceID = "workspace-rw-1"
				fixture.probe.SuccessorIdleWait = 200 * time.Millisecond
				if testCase.cluster != nil {
					testCase.cluster(fixture.cluster)
				}
				err := fixture.probe.assertRequestUnchanged(context.Background(), "exec-rw-1",
					fixture.edge.inspection(), fixture.edge.inboxItem())
				if err == nil {
					t.Fatalf("the unchanged-request assertion accepted %s", testCase.name)
				}
				if !strings.Contains(err.Error(), testCase.want) {
					t.Fatalf("the unchanged-request error %q does not name %q", err, testCase.want)
				}
			})
		}
	})

	t.Run("revoked replay of the successful resolve", func(t *testing.T) {
		for _, status := range []int{http.StatusOK, http.StatusForbidden} {
			fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
				edge.phase = "resolved"
				edge.serviceID = "q02-rw-svc-1"
				edge.revokedReplayStatus = status
			})
			fixture.probe.revokedToken = Secret(requestWaitFakeFirstService)
			fixture.probe.replayKey = "q02-key-1"
			err := fixture.probe.assertRevokedReplayRefused(context.Background(), "exec-rw-1", fixture.edge.inboxItem())
			if err == nil {
				t.Fatalf("the probe accepted the revoked credential replaying the successful key with status %d", status)
			}
			if !strings.Contains(err.Error(), "the revoked credential's replay of the successful key") {
				t.Fatalf("the revoked-replay refusal %q does not name its assertion", err)
			}
		}
	})

	t.Run("identical authorized replay returns a different result", func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.replayDiffers = true
		})
		newRequestWaitCleanup(fixture.probe)
		err := fixture.probe.run(context.Background())
		if err == nil {
			t.Fatal("the probe accepted an identical replay that returned a different result")
		}
		if !strings.Contains(err.Error(), "the identical authorized replay returned a different result") {
			t.Fatalf("the replay-equality error %q does not name the differing result", err)
		}
	})

	t.Run("terminal completion facts", func(t *testing.T) {
		cases := []struct {
			name      string
			configure func(*requestWaitFakeEdge)
			want      string
		}{
			{
				name:      "wrong recorded service actor",
				configure: func(edge *requestWaitFakeEdge) { edge.resolvedActor = "other-service" },
				want:      "the recorded request actor",
			},
			{
				name:      "duplicated completion marker",
				configure: func(edge *requestWaitFakeEdge) { edge.markerCount = 2 },
				want:      "the completion marker occurs 2 times",
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
					edge.phase = "resolved"
					edge.serviceID = "q02-rw-svc-1"
					edge.answered = time.Now().UTC()
					if testCase.configure != nil {
						testCase.configure(edge)
					}
				})
				fixture.probe.credential = Secret(requestWaitFakeExecutionToken)
				fixture.probe.serviceToken = Secret(requestWaitFakeFreshService)
				err := fixture.probe.assertCompleted(context.Background(), "exec-rw-1")
				if err == nil {
					t.Fatalf("the completed-execution assertion accepted %s", testCase.name)
				}
				if !strings.Contains(err.Error(), testCase.want) {
					t.Fatalf("the completed-execution error %q does not name %q", err, testCase.want)
				}
			})
		}
	})

	t.Run("revoked service credential on each refused surface", func(t *testing.T) {
		cases := []struct {
			name      string
			configure func(*requestWaitFakeEdge)
			operation string
			want      string
		}{
			{
				name:      "inbox accepts the revoked token",
				configure: func(edge *requestWaitFakeEdge) { edge.staleInboxStatus = http.StatusOK },
				operation: "task.inbox",
				want:      "the revoked service credential's task.inbox",
			},
			{
				name:      "inbox answers 403 for the revoked token",
				configure: func(edge *requestWaitFakeEdge) { edge.staleInboxStatus = http.StatusForbidden },
				operation: "task.inbox",
				want:      "the revoked service credential's task.inbox",
			},
			{
				name:      "resolve accepts the revoked token",
				configure: func(edge *requestWaitFakeEdge) { edge.staleResolveStatus = http.StatusOK },
				operation: "execution.request.resolve",
				want:      "the revoked service credential's execution.request.resolve",
			},
			{
				name:      "resolve answers 403 for the revoked token",
				configure: func(edge *requestWaitFakeEdge) { edge.staleResolveStatus = http.StatusForbidden },
				operation: "execution.request.resolve",
				want:      "the revoked service credential's execution.request.resolve",
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
					edge.phase = "wait"
					edge.serviceID = "q02-rw-svc-1"
					testCase.configure(edge)
				})
				fixture.probe.revokedToken = Secret(requestWaitFakeFirstService)
				err := fixture.probe.assertStaleServiceCredentialRefused(context.Background(), "exec-rw-1", fixture.edge.inboxItem())
				if err == nil {
					t.Fatalf("the stale-credential assertion accepted %s", testCase.name)
				}
				if !strings.Contains(err.Error(), testCase.want) {
					t.Fatalf("the stale-credential error %q does not name %q", err, testCase.want)
				}
				// Each surface is pinned by credential identity: substituting
				// another (even authorized) token here must die.
				var exercised bool
				for _, request := range fixture.edge.recorded() {
					if requestWaitFakeOperation(request.Method, request.Path) == testCase.operation &&
						request.Authorization == "Bearer "+requestWaitFakeFirstService {
						exercised = true
					}
				}
				if !exercised {
					t.Fatalf("the %s assertion never sent the revoked service token", testCase.operation)
				}
			})
		}
	})
}

// TestHarnessRequestWaitGateAndEvidence pins the environment-only gate and the
// allowlisted evidence record: identifiers are recorded and every credential is
// refused as a value.
func TestHarnessRequestWaitGateAndEvidence(t *testing.T) {
	base := Config{
		APIURL:          "https://127.0.0.1:18443",
		CAPEM:           "ca.pem",
		TLSServerName:   "kodama.test",
		AdminToken:      Secret("admin-token-value"),
		ProjectID:       requestWaitFakeProject,
		TenantNamespace: requestWaitFakeTenantNS,
		KubeContext:     "stage",
		Image:           "docker.io/curlimages/curl@sha256:" + strings.Repeat("a", 64),
		EvidenceDir:     t.TempDir(),
	}
	inputNames := func(missing MissingInputs) string {
		names := make([]string, 0, len(missing))
		for _, input := range missing {
			names = append(names, string(input))
		}
		return strings.Join(names, ",")
	}
	if missing := base.Missing(requestWaitInputs()...); !missing.Empty() {
		t.Fatalf("the complete configuration is missing %s", inputNames(missing))
	}
	missing := Config{}.Missing(requestWaitInputs()...)
	if len(missing) != len(requestWaitInputs()) {
		t.Fatalf("the empty configuration reported %d missing inputs; want %d", len(missing), len(requestWaitInputs()))
	}
	if skip, err := stageGate(false, missing); err != nil || !skip {
		t.Fatalf("an unconfigured optional stage = %v/%v; want a skip", skip, err)
	}
	if skip, err := stageGate(true, missing); err == nil || skip {
		t.Fatalf("a required unconfigured stage = %v/%v; want a named failure", skip, err)
	}
	if skip, err := stageGate(true, nil); err != nil || skip {
		t.Fatalf("a complete required stage = %v/%v; want to proceed", skip, err)
	}

	probe := &requestWaitProbe{
		ServiceID:    "q02-rw-svc-1",
		revokedToken: Secret(requestWaitFakeFirstService),
		freshToken:   Secret(requestWaitFakeFreshService),
		credential:   Secret(requestWaitFakeExecutionToken),
	}
	evidence := probe.evidence(time.Now().UTC(), time.Now().UTC(), "exec-rw-1")
	if evidence.Name != "stage-request-wait-revocation" || evidence.Probe != "TestStageRequestWaitRevocation" ||
		evidence.ExecutionID != "exec-rw-1" || evidence.ResourceID != probe.ServiceID {
		t.Fatalf("evidence identity = %+v", evidence)
	}
	if len(evidence.ExtraSecrets) != 3 {
		t.Fatalf("evidence registers %d credentials; want the revoked, fresh and captured ones", len(evidence.ExtraSecrets))
	}
	writer, err := NewEvidenceWriter(base.EvidenceDir)
	if err != nil {
		t.Fatalf("build the evidence writer: %v", err)
	}
	writer.Secrets = append([]Secret{base.AdminToken}, evidence.ExtraSecrets...)
	path, err := writer.Write(evidence.Name, map[string]any{
		"probe":        evidence.Probe,
		"execution_id": evidence.ExecutionID,
		"resource_id":  evidence.ResourceID,
		"outcome":      "passed",
		"recorded_at":  time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("write the request-wait evidence: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the request-wait evidence: %v", err)
	}
	for _, token := range []string{requestWaitFakeFirstService, requestWaitFakeFreshService, requestWaitFakeExecutionToken, requestWaitFakeAdminToken} {
		if strings.Contains(string(data), token) {
			t.Fatalf("the evidence file contains credential material")
		}
	}
	if _, err := writer.Write("stage-request-wait-leak", map[string]any{
		"probe":       evidence.Probe,
		"resource_id": requestWaitFakeFirstService,
	}); err == nil {
		t.Fatal("the evidence writer accepted a credential-shaped value")
	}
}

// requestWaitFixture is one fake edge, its guarded fake cluster and one probe.
type requestWaitFixture struct {
	edge    *requestWaitFakeEdge
	probe   *requestWaitProbe
	cluster *requestWaitFakeCluster
}

// newRequestWaitFixture builds one probe over the fake public edge.
func newRequestWaitFixture(t *testing.T, configure func(*requestWaitFakeEdge)) *requestWaitFixture {
	t.Helper()
	edge, client := requestWaitTestEdge(t, configure)
	if edge.executionVersion == 0 {
		edge.executionVersion = 1
	}
	cluster := &requestWaitFakeCluster{
		phase:       func() string { edge.mu.Lock(); defer edge.mu.Unlock(); return edge.phase },
		fingerprint: requestWaitFakeFingerprint(),
		secretName:  "kodama-credential-rw-1",
		token:       requestWaitFakeExecutionToken,
		pvcRetained: true,
	}
	probe := newRequestWaitProbe(Config{
		AdminToken: Secret(requestWaitFakeAdminToken),
		ProjectID:  requestWaitFakeProject,
		Image:      "docker.io/curlimages/curl@sha256:" + strings.Repeat("a", 64),
	}, client, cluster, "q02-rw-svc-1", edge.runMarker)
	probe.TenantNamespace = requestWaitFakeTenantNS
	return &requestWaitFixture{edge: edge, probe: probe, cluster: cluster}
}

// TestHarnessRequestWaitInboxPageLimit proves the probe asks for a page the
// served operation accepts and walks the pinned two-page inbox. The fake edge
// now models the served api.MaxPageLimit bound, so this guards the probe
// against asking for a size the product would refuse.
func TestHarnessRequestWaitInboxPageLimit(t *testing.T) {
	fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
		edge.phase = "wait"
		edge.serviceID = "q02-rw-svc-1"
		edge.tokens[requestWaitFakeFirstService] = true
	})
	items, err := fixture.probe.readInbox(context.Background(), Secret(requestWaitFakeFirstService))
	if err != nil {
		t.Fatalf("the request-wait inbox read was refused: %v", err)
	}
	var sawRequest bool
	for _, item := range items {
		if item.ExecutionID == "exec-rw-1" && item.Kind == "request" {
			sawRequest = true
		}
	}
	if !sawRequest {
		t.Fatalf("the inbox pages %+v did not carry the waiting request", items)
	}
	var pages int
	for _, request := range fixture.edge.recorded() {
		if requestWaitFakeOperation(request.Method, request.Path) != "task.inbox" {
			continue
		}
		pages++
		var input api.TaskInboxRequest
		if err := json.Unmarshal(request.Input, &input); err != nil {
			t.Fatalf("the task.inbox input is not the frozen DTO: %v", err)
		}
		if input.Page == nil || input.Page.Limit <= 0 {
			t.Fatalf("the task.inbox input %s carries no positive page limit", request.Input)
		}
		if input.Page.Limit > requestWaitFakeInboxMaxPageSize {
			t.Fatalf("the probe asked task.inbox for page limit %d; the served bound is %d", input.Page.Limit, requestWaitFakeInboxMaxPageSize)
		}
	}
	if pages < 2 {
		t.Fatalf("task.inbox pages = %d; want the pinned two-page walk", pages)
	}
}

// TestHarnessRequestWaitInboxFakeModelsTheServedLimit proves the fake edge
// enforces the same bound as the served inbox handler (api.MaxPageLimit = 200),
// not the retired package-local 100. It is the fake contract the request-wait
// probe runs against, so the fake cannot silently accept a page the product
// refuses (or refuse one the product accepts).
func TestHarnessRequestWaitInboxFakeModelsTheServedLimit(t *testing.T) {
	fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
		edge.phase = "wait"
		edge.serviceID = "q02-rw-svc-1"
		edge.tokens[requestWaitFakeFirstService] = true
	})
	request := func(limit int) error {
		_, err := fixture.probe.Client.Get(context.Background(), Secret(requestWaitFakeFirstService),
			"task.inbox", api.TaskInboxRequest{
				ProjectID: requestWaitFakeProject,
				Page:      &api.Page{Limit: limit},
			}, "")
		return err
	}
	if err := request(api.MaxPageLimit); err != nil {
		t.Fatalf("task.inbox page.limit %d was refused by the fake: %v", api.MaxPageLimit, err)
	}
	err := request(api.MaxPageLimit + 1)
	var refusal *APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("task.inbox page.limit %d returned %v; want a public invalid_input refusal", api.MaxPageLimit+1, err)
	}
	if refusal.Code != api.CodeInvalidInput || refusal.Status != http.StatusBadRequest {
		t.Fatalf("task.inbox page.limit %d refusal = %q (status %d); want %q (status %d)",
			api.MaxPageLimit+1, refusal.Code, refusal.Status, api.CodeInvalidInput, http.StatusBadRequest)
	}
}

// TestHarnessRequestWaitRefusalsNameTheOperation proves every refused public
// request this probe sends names its operation, the frozen public code, the
// status and the server's field detail, and never a credential or a body.
func TestHarnessRequestWaitRefusalsNameTheOperation(t *testing.T) {
	cleanupDrive := func(refuse string) func(*requestWaitFixture) error {
		return func(fixture *requestWaitFixture) error {
			cleanup := newRequestWaitCleanup(fixture.probe)
			cleanup.executionID = "exec-rw-1"
			cleanup.serviceID = fixture.probe.ServiceID
			cleanup.version = 1
			cleanup.token = Secret(requestWaitFakeFreshService)
			return cleanup.run(context.Background())
		}
	}
	cases := []struct {
		operation string
		configure func(*requestWaitFakeEdge)
		setup     func(*requestWaitFixture)
		drive     func(*requestWaitFixture) error
	}{
		{
			operation: "service.create",
			drive: func(fixture *requestWaitFixture) error {
				return fixture.probe.createService(context.Background())
			},
		},
		{
			operation: "service.credential.issue",
			configure: func(edge *requestWaitFakeEdge) { edge.serviceVersion = 1 },
			setup:     func(fixture *requestWaitFixture) { fixture.probe.serviceVersion = 1 },
			drive: func(fixture *requestWaitFixture) error {
				return fixture.probe.issueInitialCredential(context.Background())
			},
		},
		{
			operation: "service.credential.revoke",
			configure: func(edge *requestWaitFakeEdge) { edge.serviceVersion = 1 },
			setup: func(fixture *requestWaitFixture) {
				fixture.probe.serviceVersion = 1
				fixture.probe.serviceToken = Secret(requestWaitFakeFirstService)
			},
			drive: func(fixture *requestWaitFixture) error {
				return fixture.probe.revokeServiceCredential(context.Background())
			},
		},
		{
			operation: "execution.inspect",
			setup:     func(fixture *requestWaitFixture) { fixture.probe.credential = Secret(requestWaitFakeExecutionToken) },
			drive: func(fixture *requestWaitFixture) error {
				return fixture.probe.proveExecutionCredential(context.Background(), "exec-rw-1")
			},
		},
		{
			operation: "task.inbox",
			configure: func(edge *requestWaitFakeEdge) {
				edge.phase = "wait"
				edge.serviceID = "q02-rw-svc-1"
			},
			drive: func(fixture *requestWaitFixture) error {
				_, _, err := fixture.probe.awaitBarrier(context.Background(), "exec-rw-1")
				return err
			},
		},
		{
			operation: "execution.request.resolve",
			configure: func(edge *requestWaitFakeEdge) {
				edge.phase = "wait"
				edge.serviceID = "q02-rw-svc-1"
			},
			drive: func(fixture *requestWaitFixture) error {
				_, err := fixture.probe.resolveWithKey(context.Background(), Secret(requestWaitFakeFirstService),
					"exec-rw-1", fixture.edge.inboxItem(), "q02-key-1")
				return err
			},
		},
		{
			operation: "execution.logs.read",
			drive: func(fixture *requestWaitFixture) error {
				_, err := fixture.probe.readLogs(context.Background(), "exec-rw-1", "action-after")
				return err
			},
		},
		{
			operation: "execution.cancel",
			drive: func(fixture *requestWaitFixture) error {
				return requestWaitCancel(context.Background(), fixture.probe, "exec-rw-1", 1)
			},
		},
		{
			operation: "service.disable",
			configure: func(edge *requestWaitFakeEdge) {
				edge.phase = "cancelled"
				edge.executionVersion = 3
				edge.serviceVersion = 1
			},
			drive: cleanupDrive("service.disable"),
		},
		{
			operation: "execution.list",
			configure: func(edge *requestWaitFakeEdge) {
				edge.phase = "cancelled"
				edge.executionVersion = 3
				edge.serviceVersion = 1
			},
			drive: cleanupDrive("execution.list"),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.operation, func(t *testing.T) {
			fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
				edge.refuse = testCase.operation
				if testCase.configure != nil {
					testCase.configure(edge)
				}
			})
			if testCase.setup != nil {
				testCase.setup(fixture)
			}
			err := testCase.drive(fixture)
			if err == nil {
				t.Fatalf("%s: the refused request was accepted", testCase.operation)
			}
			for _, want := range []string{
				testCase.operation + " was refused",
				api.CodeInvalidInput,
				"status 400",
				requestWaitFakeRefusalDetail,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal %q does not name %q", err, want)
				}
			}
			for _, secret := range []string{
				requestWaitFakeAdminToken,
				requestWaitFakeFirstService,
				requestWaitFakeFreshService,
				requestWaitFakeExecutionToken,
			} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the refusal leaked a credential")
				}
			}
			if strings.Contains(err.Error(), `"input"`) {
				t.Fatalf("the refusal echoed a request body: %v", err)
			}
		})
	}
}

// TestHarnessRequestWaitBeforeStartDiagnostic proves a pre-wait action that has
// not started yet is tolerated to the bounded deadline with its activation and
// stop evidence, while an action that ends before it runs fails fast with the
// same evidence.
func TestHarnessRequestWaitBeforeStartDiagnostic(t *testing.T) {
	t.Run("not yet started is tolerated to the bounded deadline", func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.beforeState = "pending"
		})
		fixture.probe.StartWait = 150 * time.Millisecond
		started := time.Now()
		_, err := fixture.probe.awaitBeforeRunning(context.Background(), "exec-rw-1")
		if err == nil {
			t.Fatal("the probe accepted a pre-wait action that never started")
		}
		if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
			t.Fatalf("the probe gave up after %s; want the bounded deadline", elapsed)
		}
		for _, want := range []string{
			"the pre-wait action did not start within 150ms",
			"execution exec-rw-1",
			`action "before"`,
			`state "pending"`,
			"activated_at none",
			"deadline none",
			"stop none",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the start diagnostic %q does not name %q", err, want)
			}
		}
	})

	t.Run("an activated but not yet running action names its activation", func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.beforeState = "pending"
			edge.beforeActivated = true
		})
		fixture.probe.StartWait = 120 * time.Millisecond
		_, err := fixture.probe.awaitBeforeRunning(context.Background(), "exec-rw-1")
		if err == nil {
			t.Fatal("the probe accepted a pre-wait action that never ran")
		}
		if !strings.Contains(err.Error(), "activated_at 2") {
			t.Fatalf("the start diagnostic %q does not name the activation time", err)
		}
		if !strings.Contains(err.Error(), `state "pending"`) {
			t.Fatalf("the start diagnostic %q does not name the action state", err)
		}
	})

	t.Run("an action that ends before it runs fails fast with its stop evidence", func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.beforeState = "cancelled"
			edge.beforeStop = &api.ExecutionStopEvidence{State: "unconfirmed", Detail: "stop unconfirmed: 0 workload pods observed"}
		})
		fixture.probe.StartWait = 30 * time.Second
		started := time.Now()
		_, err := fixture.probe.awaitBeforeRunning(context.Background(), "exec-rw-1")
		if err == nil {
			t.Fatal("the probe accepted a pre-wait action that ended before it ran")
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("the probe waited %s for an action that had already ended", elapsed)
		}
		for _, want := range []string{
			"the pre-wait action ended before it ran",
			`state "cancelled"`,
			"stop unconfirmed",
			"0 workload pods observed",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the ended-action diagnostic %q does not name %q", err, want)
			}
		}
	})
}

// TestHarnessRequestWaitCleanupStuckDiagnostic proves a cleanup that never
// converges is reported with the execution, the cleanup state and the workload
// identity that remained, so a stuck cleanup is attributable to the product.
func TestHarnessRequestWaitCleanupStuckDiagnostic(t *testing.T) {
	fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
		edge.phase = "stuck"
		edge.executionVersion = 3
		edge.serviceVersion = 1
	})
	fixture.probe.serviceVersion = 1
	cleanup := newRequestWaitCleanup(fixture.probe)
	cleanup.executionID = "exec-rw-1"
	cleanup.serviceID = fixture.probe.ServiceID
	cleanup.version = 1
	cleanup.token = Secret(requestWaitFakeFreshService)
	cleanup.settle = 150 * time.Millisecond
	err := cleanup.run(context.Background())
	if err == nil {
		t.Fatal("the cleanup accepted a cleanup that never converged")
	}
	for _, want := range []string{
		"execution exec-rw-1",
		`cleanup state "pending"`,
		`workload "wl-rw-stuck"`,
		"attempts 2",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the stuck-cleanup diagnostic %q does not name %q", err, want)
		}
	}
	for _, secret := range []string{requestWaitFakeAdminToken, requestWaitFakeFreshService} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the stuck-cleanup diagnostic leaked a credential")
		}
	}
}

// TestHarnessRequestWaitRequestShapes audits every request the probe sends
// against the frozen C03 route table and the operation's own request DTO: the
// method and path must be one registered binding, the frozen envelope must
// decode for that binding, and every route-derived field must agree with the
// path. The frozen public client sends one envelope body for every method, GET
// included, so every recorded request carries one.
func TestHarnessRequestWaitRequestShapes(t *testing.T) {
	fixture := newRequestWaitFixture(t, nil)
	cleanup := newRequestWaitCleanup(fixture.probe)
	if err := fixture.probe.run(context.Background()); err != nil {
		t.Fatalf("the request-wait probe failed against the fake edge: %v", err)
	}
	requests := fixture.edge.recorded()
	if len(requests) == 0 {
		t.Fatal("the probe sent no public request")
	}
	var sawGet bool
	for _, request := range requests {
		route, params, ok := api.Match(api.Method(request.Method), request.Path)
		if !ok {
			t.Fatalf("%s %s is not a registered public route", request.Method, request.Path)
		}
		if route.Method != api.Method(request.Method) {
			t.Fatalf("%s %s matched %s", request.Method, request.Path, route.Method)
		}
		if len(request.Input) == 0 {
			t.Fatalf("%s %s carried no frozen request envelope body", request.Method, request.Path)
		}
		// No frozen route carries a query string, so every parameter is either
		// a path segment or a strict request-DTO member.
		if request.Query != "" {
			t.Fatalf("%s %s carried the query %q", request.Method, request.Path, request.Query)
		}
		if _, err := api.DecodeRouteRequest(route, request.Input, params); err != nil {
			t.Fatalf("%s %s does not decode for %s: %v", request.Method, request.Path, route.Operation, err)
		}
		var envelope api.Request
		if err := json.Unmarshal(request.Input, &envelope); err != nil {
			t.Fatalf("%s %s does not carry the frozen request envelope: %v", request.Method, request.Path, err)
		}
		// The idempotency key is an envelope member and is present exactly for
		// the operations the frozen codec requires one from.
		if operationRequiresIdempotencyKey(route.Operation) && request.IdempotencyKey == "" {
			t.Fatalf("%s %s carried no required idempotency key", request.Method, request.Path)
		}
		if !operationRequiresIdempotencyKey(route.Operation) && request.IdempotencyKey != "" {
			t.Fatalf("%s %s carried an idempotency key it does not need", request.Method, request.Path)
		}
		if api.Method(request.Method) == api.MethodGet {
			sawGet = true
		}
	}
	if !sawGet {
		t.Fatal("the probe sent no GET, so the GET envelope shape is unpinned")
	}

	// A field the route's schema does not declare must be refused by the same
	// decoder, so this audit cannot pass vacuously.
	route, params, ok := api.Match(api.MethodGet, "/api/v1/projects/"+requestWaitFakeProject+"/inbox")
	if !ok {
		t.Fatal("the task.inbox route is missing")
	}
	if _, err := api.DecodeRouteRequest(route, json.RawMessage(`{"project_id":"proj","undeclared":true}`), params); err == nil {
		t.Fatal("the shape audit accepted an undeclared request field")
	}
	if _, err := api.DecodeRouteRequest(route, json.RawMessage(`{"project_id":"other-project"}`), params); err == nil {
		t.Fatal("the shape audit accepted a body field that disagrees with the route")
	}

	if err := cleanup.run(context.Background()); err != nil {
		t.Fatalf("the strict cleanup failed after the shape audit: %v", err)
	}
	// The cleanup sends its own public requests; audit them too.
	for _, request := range fixture.edge.recorded()[len(requests):] {
		route, params, ok := api.Match(api.Method(request.Method), request.Path)
		if !ok {
			t.Fatalf("cleanup %s %s is not a registered public route", request.Method, request.Path)
		}
		if _, err := api.DecodeRouteRequest(route, request.Input, params); err != nil {
			t.Fatalf("cleanup %s %s does not decode for %s: %v", request.Method, request.Path, route.Operation, err)
		}
	}
}

// requestWaitShortenWait overrides one pinned wait duration for a test and
// returns its restore function.
func requestWaitShortenWait(target *time.Duration, value time.Duration) func() {
	previous := *target
	*target = value
	return func() { *target = previous }
}

// TestHarnessRequestWaitSuccessorIdle pins the corrected request-wait workload
// predicate against the REAL product shape: the completed pre-wait Job and Pod
// are retained until terminal cleanup or the Job TTL
// (internal/kernel/engine/cleanup.go:29-31, cmd/kodama/production.go:343), while
// a live workload or any workload for another action is evidence that the
// refused resolution advanced the wait. The old zero-workload predicate is red
// on the retained predecessor, exactly like the live run.
func TestHarnessRequestWaitSuccessorIdle(t *testing.T) {
	newProbe := func(t *testing.T) *requestWaitFixture {
		t.Helper()
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "wait"
			edge.serviceID = "q02-rw-svc-1"
			edge.tokens[requestWaitFakeFirstService] = true
		})
		fixture.probe.SuccessorIdleWait = 250 * time.Millisecond
		return fixture
	}

	t.Run("the retained completed predecessor workload is tolerated", func(t *testing.T) {
		fixture := newProbe(t)
		if err := fixture.probe.awaitSuccessorIdle(context.Background(), "exec-rw-1", "action-before"); err != nil {
			t.Fatalf("the retained completed predecessor workload was refused: %v", err)
		}
		if fixture.cluster.podReadCount() == 0 {
			t.Fatal("the successor-idle check never read the workload")
		}
		var sawJob bool
		for _, call := range fixture.cluster.calls {
			if strings.HasPrefix(call, "jobs "+requestWaitLabelExecution+"=exec-rw-1") {
				sawJob = true
			}
		}
		if !sawJob {
			t.Fatalf("the successor-idle check never read the Jobs: %v", fixture.cluster.calls)
		}
	})

	cases := []struct {
		name      string
		configure func(*requestWaitFakeCluster)
		want      []string
	}{
		{
			name: "a live predecessor pod is refused",
			configure: func(cluster *requestWaitFakeCluster) {
				cluster.podPhase = func() string { return "Running" }
			},
			want: []string{requestWaitStepSuccessorIdle, "live action-before=Running"},
		},
		{
			name: "a pending predecessor pod is refused",
			configure: func(cluster *requestWaitFakeCluster) {
				cluster.podPhase = func() string { return "Pending" }
			},
			want: []string{requestWaitStepSuccessorIdle, "live action-before=Pending"},
		},
		{
			name: "a successor pod is refused",
			configure: func(cluster *requestWaitFakeCluster) {
				cluster.mutatePod = func(pod *requestWaitFakePod) {
					pod.Metadata.Labels[requestWaitLabelAction] = "action-after"
				}
			},
			want: []string{requestWaitStepSuccessorIdle, "non-predecessor actions action-after"},
		},
		{
			name: "a successor Job alone is refused",
			configure: func(cluster *requestWaitFakeCluster) {
				cluster.jobAction = "action-after"
			},
			want: []string{requestWaitStepSuccessorIdle, "non-predecessor actions action-after"},
		},
		{
			name: "an unlabelled workload is refused",
			configure: func(cluster *requestWaitFakeCluster) {
				cluster.mutatePod = func(pod *requestWaitFakePod) {
					delete(pod.Metadata.Labels, requestWaitLabelAction)
				}
			},
			want: []string{requestWaitStepSuccessorIdle, "non-predecessor actions <unlabelled>"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newProbe(t)
			testCase.configure(fixture.cluster)
			err := fixture.probe.awaitSuccessorIdle(context.Background(), "exec-rw-1", "action-before")
			if err == nil {
				t.Fatalf("the successor-idle check accepted %s", testCase.name)
			}
			for _, want := range append([]string{"did not hold within", "last observed"}, testCase.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the successor-idle diagnostic %q does not name %q", err, want)
				}
			}
		})
	}

	t.Run("an unreadable workload list is attributed to the step", func(t *testing.T) {
		fixture := newProbe(t)
		fixture.cluster.failPods = true
		err := fixture.probe.awaitSuccessorIdle(context.Background(), "exec-rw-1", "action-before")
		if err == nil {
			t.Fatal("the successor-idle check accepted an unreadable workload list")
		}
		for _, want := range []string{requestWaitStepSuccessorIdle, "pods for execution exec-rw-1 were not readable"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the workload read diagnostic %q does not name %q", err, want)
			}
		}
	})
}

// TestHarnessRequestWaitUnchangedAfterRefusal pins the call site that failed the
// live run: after refused authority, the probe re-inspects and independently
// proves the successor idle while the completed pre-wait Job and Pod are still
// retained. A "no workload at all" requirement here is red on the real shape.
func TestHarnessRequestWaitUnchangedAfterRefusal(t *testing.T) {
	fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
		edge.phase = "wait"
		edge.serviceID = "q02-rw-svc-1"
		edge.tokens[requestWaitFakeFirstService] = true
	})
	fixture.probe.workspaceID = "workspace-rw-1"
	fixture.probe.SuccessorIdleWait = 250 * time.Millisecond
	if err := fixture.probe.assertRequestUnchanged(context.Background(), "exec-rw-1",
		fixture.edge.inspection(), fixture.edge.inboxItem()); err != nil {
		t.Fatalf("the unchanged request was refused while the completed pre-wait workload was retained: %v", err)
	}
}

// requestWaitCaptureStdout runs one operation while capturing process stdout,
// so a credential that a diagnostic (or any other test-only output) leaked to
// stdout is observable on the success path.
func requestWaitCaptureStdout(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("capture stdout: %v", err)
	}
	previous := os.Stdout
	os.Stdout = writer
	defer func() {
		os.Stdout = previous
		_ = reader.Close()
	}()
	run()
	if err := writer.Close(); err != nil {
		t.Fatalf("close the stdout capture: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read the stdout capture: %v", err)
	}
	return string(data)
}

// TestHarnessRequestWaitObjectListArgv pins the exact kubectl argv the guarded
// cluster read executes: one namespaced `get <resource> -l <selector> -o json`
// with no shell, no extra verb and no interpolated selector.
func TestHarnessRequestWaitObjectListArgv(t *testing.T) {
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	path := filepath.Join(dir, "kubectl")
	mustWriteFile(t, path, "#!/bin/sh\nprintf '%s\\n' \"$@\" > "+shellQuote(argvPath)+"\nprintf '{\"items\":[]}'\n")
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("make the fake kubectl executable: %v", err)
	}
	guard, err := NewGuard("stage", requestWaitFakeTenantNS)
	if err != nil {
		t.Fatalf("build the confinement guard: %v", err)
	}
	reader := &requestWaitKube{
		client:    &Kubectl{Guard: guard, Path: path, Timeout: 30 * time.Second},
		namespace: requestWaitFakeTenantNS,
	}
	selector := requestWaitLabelExecution + "=exec-rw-1"
	data, err := reader.ObjectList(context.Background(), requestWaitFakeTenantNS, "pods", selector)
	if err != nil {
		t.Fatalf("the guarded object list failed: %v", err)
	}
	if _, err := requestWaitObjectList(data); err != nil {
		t.Fatalf("the guarded object list is not a JSON list: %v", err)
	}
	captured, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read the fake kubectl argv: %v", err)
	}
	want := []string{
		"--context", "stage",
		"--namespace", requestWaitFakeTenantNS,
		"get", "pods",
		"-l", selector,
		"-o", "json",
	}
	got := strings.Fields(string(captured))
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("kubectl argv = %q; want %q", got, want)
	}
	if _, err := reader.ObjectList(context.Background(), "other-namespace", "pods", selector); err == nil {
		t.Fatal("the guarded cluster read accepted a namespace outside its assignment")
	}
}

// TestHarnessRequestWaitStepNames proves every bounded wait names its own step
// and last secret-free observation when it expires, so a live failure is
// attributable to one predicate instead of a bare "did not hold".
func TestHarnessRequestWaitStepNames(t *testing.T) {

	assertStep := func(t *testing.T, err error, step string) {
		t.Helper()
		if err == nil {
			t.Fatalf("the %s wait accepted a predicate that never held", step)
		}
		for _, want := range []string{"did not hold within", "at step " + `"` + step + `"`, "last observed"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the %s timeout diagnostic %q does not name %q", step, err, want)
			}
		}
	}

	t.Run(requestWaitStepBeforeStart, func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.beforeState = "pending"
		})
		fixture.probe.StartWait = 150 * time.Millisecond
		_, err := fixture.probe.awaitBeforeRunning(context.Background(), "exec-rw-1")
		assertStep(t, err, requestWaitStepBeforeStart)
	})

	t.Run(requestWaitStepCredential, func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
		})
		fixture.cluster.secretDelay = 1 << 20
		fixture.probe.MaterializationWait = 150 * time.Millisecond
		fixture.probe.MaterializationPoll = 5 * time.Millisecond
		_, err := fixture.probe.captureCredential(context.Background(), "exec-rw-1", "action-before")
		if err == nil {
			t.Fatal("the credential capture accepted a Secret that never materialized")
		}
		if !strings.Contains(err.Error(), `at step "`+requestWaitStepCredential+`"`) ||
			!strings.Contains(err.Error(), "attempts") {
			t.Fatalf("the credential timeout %q does not name its step and attempts", err)
		}
	})

	t.Run(requestWaitStepBarrier, func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "before"
			edge.serviceID = "q02-rw-svc-1"
			edge.tokens[requestWaitFakeFirstService] = true
		})
		fixture.probe.serviceToken = Secret(requestWaitFakeFirstService)
		defer requestWaitShortenWait(&requestWaitBarrierTimeout, 150*time.Millisecond)()
		_, _, err := fixture.probe.awaitBarrier(context.Background(), "exec-rw-1")
		assertStep(t, err, requestWaitStepBarrier)
		if !strings.Contains(err.Error(), "inbox items 0 request_item false") {
			t.Fatalf("the barrier timeout %q does not name the empty request item", err)
		}
	})

	t.Run(requestWaitStepTerminal, func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "wait"
			edge.serviceID = "q02-rw-svc-1"
		})
		defer requestWaitShortenWait(&requestWaitTerminalTimeout, 150*time.Millisecond)()
		err := fixture.probe.assertCompleted(context.Background(), "exec-rw-1")
		assertStep(t, err, requestWaitStepTerminal)
	})

	t.Run(requestWaitStepExecutionCleanup, func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "stuck"
			edge.executionVersion = 3
		})
		cleanup := newRequestWaitCleanup(fixture.probe)
		cleanup.executionID = "exec-rw-1"
		cleanup.settle = 150 * time.Millisecond
		err := cleanup.awaitCleanup(context.Background(), "step-name test")
		assertStep(t, err, requestWaitStepExecutionCleanup)
	})

	t.Run(requestWaitStepCleanupSettle, func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "resolved"
			edge.serviceID = "q02-rw-svc-1"
			edge.cleanupState = "pending"
			edge.answered = time.Now().UTC()
		})
		defer requestWaitShortenWait(&requestWaitCleanupSettle, 150*time.Millisecond)()
		err := fixture.probe.assertCompleted(context.Background(), "exec-rw-1")
		assertStep(t, err, requestWaitStepCleanupSettle)
	})

	t.Run(requestWaitStepCleanupRemoval, func(t *testing.T) {
		fixture := newRequestWaitFixture(t, func(edge *requestWaitFakeEdge) {
			edge.phase = "wait"
		})
		defer requestWaitShortenWait(&requestWaitRemovalWait, 150*time.Millisecond)()
		err := requestWaitAwaitNoWorkload(context.Background(), fixture.cluster, requestWaitFakeTenantNS, "exec-rw-1")
		assertStep(t, err, requestWaitStepCleanupRemoval)
		if !strings.Contains(err.Error(), "still has live") {
			t.Fatalf("the cleanup-removal timeout %q does not name the retained workload", err)
		}
	})
}
