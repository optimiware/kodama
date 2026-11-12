package factory

// Unit coverage for the bounded, file-backed external checkpoint service.
//
// The service is the fixture half of the agent workload proof: workloads run
// in their own containers with no durable local state, and their checkpoints
// live in a host directory outside every execution workspace and outside the
// runtime root. These tests drive the real HTTP boundary and the real Userland
// file checkpoint storage; they need no database, broker or container.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/session"
)

// agentCheckpointTestRecord builds one valid session record for the service
// tests. The message content is the only variable, so a test can size a record
// precisely.
func agentCheckpointTestRecord(sessionID, taskID, executionID, content string) session.Record {
	return session.Record{
		Version:     session.RefVersion,
		TaskID:      taskID,
		ExecutionID: executionID,
		SessionID:   session.SessionID(sessionID),
		Messages:    []session.Message{{Role: "user", Content: content}},
		Retention:   session.RetentionPolicy{RawContent: true},
		Outcome:     session.Outcome{Status: "checkpoint"},
		CreatedAt:   time.Now().UTC(),
	}
}

// agentCheckpointTestSave posts one save envelope with the service token.
func agentCheckpointTestSave(t *testing.T, service *AgentCheckpointService, record session.Record, expected uint64, token string) (int, agentCheckpointSaveResponse, []byte) {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	body := []byte(fmt.Sprintf(`{"expected_revision":%d,"record":%s}`, expected, encoded))
	status, data := agentCheckpointTestPost(t, service.Endpoint(), token, "/v1/checkpoints/save", body)
	var response agentCheckpointSaveResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatalf("decode save response: %v; body=%s", err, data)
		}
	}
	return status, response, data
}

// agentCheckpointTestRead posts one read envelope.
func agentCheckpointTestRead(t *testing.T, service *AgentCheckpointService, ref string, token string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(agentCheckpointReadRequest{Ref: ref})
	if err != nil {
		t.Fatalf("encode read request: %v", err)
	}
	return agentCheckpointTestPost(t, service.Endpoint(), token, "/v1/checkpoints/read", body)
}

// agentCheckpointTestPost performs one authenticated service request.
func agentCheckpointTestPost(t *testing.T, endpoint, token, path string, body []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build checkpoint request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("perform checkpoint request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read checkpoint response: %v", err)
	}
	return response.StatusCode, data
}

// agentCheckpointTestService starts one service on loopback. No workload host is
// configured, so the fixture creates no routable relay.
func agentCheckpointTestService(t *testing.T, config AgentCheckpointConfig) *AgentCheckpointService {
	t.Helper()
	config.T = t
	if config.Root == "" {
		config.Root = t.TempDir()
	}
	return StartAgentCheckpointService(t, config)
}

// TestAgentCheckpointServiceSavesAndReadsDurably proves the happy path crosses
// the real file storage: a save returns a versioned reference, the record is
// readable again, and the service can enumerate its committed heads.
func TestAgentCheckpointServiceSavesAndReadsDurably(t *testing.T) {
	service := agentCheckpointTestService(t, AgentCheckpointConfig{})
	record := agentCheckpointTestRecord("sess-agent-h-1", "task-agent-h", "exec-agent-h-1", "durable transcript")

	status, saved, body := agentCheckpointTestSave(t, service, record, 0, service.Token())
	if status != http.StatusOK {
		t.Fatalf("save status = %d; body=%s", status, body)
	}
	if !strings.HasPrefix(saved.Ref, session.RefPrefix+session.RefVersion+"/") {
		t.Fatalf("saved reference = %q; want a versioned checkpoint reference", saved.Ref)
	}

	status, body = agentCheckpointTestRead(t, service, saved.Ref, service.Token())
	if status != http.StatusOK {
		t.Fatalf("read status = %d; body=%s", status, body)
	}
	var read agentCheckpointReadResponse
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("decode read response: %v; body=%s", err, body)
	}
	if len(read.Record.Messages) != 1 || read.Record.Messages[0].Content != "durable transcript" {
		t.Fatalf("restored record messages = %+v; want the saved transcript", read.Record.Messages)
	}
	if read.Record.Revision != 1 {
		t.Fatalf("restored revision = %d; want 1", read.Record.Revision)
	}

	refs, err := service.Refs()
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(refs) != 1 || refs[0] != saved.Ref {
		t.Fatalf("service refs = %v; want [%s]", refs, saved.Ref)
	}
	bytesStored, err := service.StoredBytes()
	if err != nil {
		t.Fatalf("stored bytes: %v", err)
	}
	if bytesStored <= 0 {
		t.Fatalf("stored bytes = %d; want the durable files", bytesStored)
	}
	if !strings.HasPrefix(service.Root(), filepath.Clean(service.Root())) {
		t.Fatalf("service root %q is not clean", service.Root())
	}
}

// TestAgentCheckpointServiceRefusesOversizedRecord proves the per-record bound
// is enforced before the storage boundary: nothing is committed and the caller
// sees a typed refusal.
func TestAgentCheckpointServiceRefusesOversizedRecord(t *testing.T) {
	service := agentCheckpointTestService(t, AgentCheckpointConfig{MaxRecordBytes: 1024})
	record := agentCheckpointTestRecord("sess-agent-h-big", "task-agent-h", "exec-agent-h-big", strings.Repeat("x", 4096))

	status, _, body := agentCheckpointTestSave(t, service, record, 0, service.Token())
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized save status = %d; want %d; body=%s", status, http.StatusRequestEntityTooLarge, body)
	}
	var refusal agentCheckpointError
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode refusal: %v; body=%s", err, body)
	}
	if refusal.Code != agentCheckpointCodeRecordTooLarge {
		t.Fatalf("refusal code = %q; want %q", refusal.Code, agentCheckpointCodeRecordTooLarge)
	}
	refs, err := service.Refs()
	if err != nil {
		t.Fatalf("list refs after refusal: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs after an oversized refusal = %v; want none", refs)
	}
}

// TestAgentCheckpointServiceRefusesOverFullStore proves the total-store bound
// stops a second transcript from consuming the durable directory while the
// first remains readable.
func TestAgentCheckpointServiceRefusesOverFullStore(t *testing.T) {
	service := agentCheckpointTestService(t, AgentCheckpointConfig{
		MaxRecordBytes: 1 << 20,
		MaxStoreBytes:  1 << 20,
	})
	first := agentCheckpointTestRecord("sess-agent-h-full-1", "task-agent-h", "exec-agent-h-full-1", strings.Repeat("a", 600<<10))
	status, saved, body := agentCheckpointTestSave(t, service, first, 0, service.Token())
	if status != http.StatusOK {
		t.Fatalf("first save status = %d; body=%s", status, body)
	}

	second := agentCheckpointTestRecord("sess-agent-h-full-2", "task-agent-h", "exec-agent-h-full-2", strings.Repeat("b", 600<<10))
	status, _, body = agentCheckpointTestSave(t, service, second, 0, service.Token())
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("second save status = %d; want %d; body=%s", status, http.StatusRequestEntityTooLarge, body)
	}
	var refusal agentCheckpointError
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode refusal: %v; body=%s", err, body)
	}
	if refusal.Code != agentCheckpointCodeStoreFull {
		t.Fatalf("refusal code = %q; want %q", refusal.Code, agentCheckpointCodeStoreFull)
	}

	status, body = agentCheckpointTestRead(t, service, saved.Ref, service.Token())
	if status != http.StatusOK {
		t.Fatalf("first record read after refusal status = %d; body=%s", status, body)
	}
}

// TestAgentCheckpointServiceCompareAndSwap proves revision CAS still belongs to
// the storage and is not replaced by the fixture: a stale expected revision is
// refused and the committed revisions stay immutable.
func TestAgentCheckpointServiceCompareAndSwap(t *testing.T) {
	service := agentCheckpointTestService(t, AgentCheckpointConfig{})
	record := agentCheckpointTestRecord("sess-agent-h-cas", "task-agent-h", "exec-agent-h-cas", "first")

	status, first, body := agentCheckpointTestSave(t, service, record, 0, service.Token())
	if status != http.StatusOK {
		t.Fatalf("first save status = %d; body=%s", status, body)
	}
	status, _, body = agentCheckpointTestSave(t, service, record, 0, service.Token())
	if status != http.StatusConflict {
		t.Fatalf("stale save status = %d; want %d; body=%s", status, http.StatusConflict, body)
	}

	second := agentCheckpointTestRecord("sess-agent-h-cas", "task-agent-h", "exec-agent-h-cas", "second")
	status, advanced, body := agentCheckpointTestSave(t, service, second, 1, service.Token())
	if status != http.StatusOK {
		t.Fatalf("advanced save status = %d; body=%s", status, body)
	}
	if advanced.Ref == first.Ref {
		t.Fatalf("advanced reference = %q; want a new revision reference", advanced.Ref)
	}

	for ref, want := range map[string]string{first.Ref: "first", advanced.Ref: "second"} {
		status, body := agentCheckpointTestRead(t, service, ref, service.Token())
		if status != http.StatusOK {
			t.Fatalf("read %s status = %d; body=%s", ref, status, body)
		}
		var read agentCheckpointReadResponse
		if err := json.Unmarshal(body, &read); err != nil {
			t.Fatalf("decode %s: %v", ref, err)
		}
		if read.Record.Messages[0].Content != want {
			t.Fatalf("record %s content = %q; want %q", ref, read.Record.Messages[0].Content, want)
		}
	}
}

// TestAgentCheckpointServiceRestartReopensDurableStorage proves the fixture's
// "reopen between attempts" behavior is real: the server, listener and storage
// instance are replaced, and only the durable directory carries the record.
func TestAgentCheckpointServiceRestartReopensDurableStorage(t *testing.T) {
	service := agentCheckpointTestService(t, AgentCheckpointConfig{})
	record := agentCheckpointTestRecord("sess-agent-h-reopen", "task-agent-h", "exec-agent-h-reopen", "survives a reopen")

	status, saved, body := agentCheckpointTestSave(t, service, record, 0, service.Token())
	if status != http.StatusOK {
		t.Fatalf("save status = %d; body=%s", status, body)
	}
	if service.Generation() != 1 {
		t.Fatalf("generation before restart = %d; want 1", service.Generation())
	}

	service.Restart()
	if service.Generation() != 2 {
		t.Fatalf("generation after restart = %d; want 2", service.Generation())
	}

	status, body = agentCheckpointTestRead(t, service, saved.Ref, service.Token())
	if status != http.StatusOK {
		t.Fatalf("read after restart status = %d; body=%s", status, body)
	}
	var read agentCheckpointReadResponse
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("decode read after restart: %v; body=%s", err, body)
	}
	if read.Record.Messages[0].Content != "survives a reopen" {
		t.Fatalf("restored content after restart = %q", read.Record.Messages[0].Content)
	}
}

// TestAgentCheckpointServiceRefusesUnauthorizedAndMissing proves the service is
// not an unauthenticated transcript reader and that an absent reference is a
// typed missing result rather than an empty record.
func TestAgentCheckpointServiceRefusesUnauthorizedAndMissing(t *testing.T) {
	service := agentCheckpointTestService(t, AgentCheckpointConfig{})
	record := agentCheckpointTestRecord("sess-agent-h-auth", "task-agent-h", "exec-agent-h-auth", "private transcript")

	status, _, body := agentCheckpointTestSave(t, service, record, 0, "wrong-token")
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthorized save status = %d; want %d; body=%s", status, http.StatusUnauthorized, body)
	}
	refs, err := service.Refs()
	if err != nil {
		t.Fatalf("refs after unauthorized save: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs after unauthorized save = %v; want none", refs)
	}

	missing := session.RefPrefix + session.RefVersion + "/" + strings.Repeat("0", 32) + "-7"
	status, body = agentCheckpointTestRead(t, service, string(missing), service.Token())
	if status != http.StatusNotFound {
		t.Fatalf("missing read status = %d; want %d; body=%s", status, http.StatusNotFound, body)
	}
	var refusal agentCheckpointError
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode missing refusal: %v; body=%s", err, body)
	}
	if refusal.Code != agentCheckpointCodeMissing {
		t.Fatalf("missing refusal code = %q; want %q", refusal.Code, agentCheckpointCodeMissing)
	}
}

// TestAgentCheckpointServiceWirePinsExactJSON pins the exact wire the workload
// binary speaks: endpoint paths, request member names and the response
// reference member. It deliberately builds the body as bytes so a struct rename
// cannot silently pass.
func TestAgentCheckpointServiceWirePinsExactJSON(t *testing.T) {
	service := agentCheckpointTestService(t, AgentCheckpointConfig{})
	record := agentCheckpointTestRecord("sess-agent-h-wire", "task-agent-h", "exec-agent-h-wire", "wire transcript")
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	save := []byte(`{"expected_revision":0,"record":` + string(encoded) + `}`)

	status, body := agentCheckpointTestPost(t, service.Endpoint(), service.Token(), "/v1/checkpoints/save", save)
	if status != http.StatusOK {
		t.Fatalf("wire save status = %d; body=%s", status, body)
	}
	var response struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode wire save response: %v; body=%s", err, body)
	}
	if !strings.HasPrefix(response.Ref, "kodama-checkpoint/v1/") {
		t.Fatalf("wire reference = %q", response.Ref)
	}

	read := []byte(`{"ref":"` + response.Ref + `"}`)
	status, body = agentCheckpointTestPost(t, service.Endpoint(), service.Token(), "/v1/checkpoints/read", read)
	if status != http.StatusOK {
		t.Fatalf("wire read status = %d; body=%s", status, body)
	}
	var readResponse struct {
		Record session.Record `json:"record"`
	}
	if err := json.Unmarshal(body, &readResponse); err != nil {
		t.Fatalf("decode wire read response: %v; body=%s", err, body)
	}
	if len(readResponse.Record.Messages) != 1 || readResponse.Record.Messages[0].Content != "wire transcript" {
		t.Fatalf("wire read record = %+v", readResponse.Record)
	}
}

// TestAgentCheckpointServiceSortsRefsAndReportsBytes proves enumeration is
// deterministic, which the workload proof uses to compare committed heads.
func TestAgentCheckpointServiceSortsRefsAndReportsBytes(t *testing.T) {
	service := agentCheckpointTestService(t, AgentCheckpointConfig{})
	var refs []string
	for index := range 3 {
		record := agentCheckpointTestRecord(
			fmt.Sprintf("sess-agent-h-sort-%d", index), "task-agent-h",
			fmt.Sprintf("exec-agent-h-sort-%d", index), fmt.Sprintf("content %d", index))
		status, saved, body := agentCheckpointTestSave(t, service, record, 0, service.Token())
		if status != http.StatusOK {
			t.Fatalf("save %d status = %d; body=%s", index, status, body)
		}
		refs = append(refs, saved.Ref)
	}
	sort.Strings(refs)
	stored, err := service.Refs()
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if strings.Join(stored, ",") != strings.Join(refs, ",") {
		t.Fatalf("refs = %v; want sorted %v", stored, refs)
	}
}
