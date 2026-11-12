package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/session"
)

type saveResult struct {
	ref session.Ref
	err error
}

func saveAsync(store session.Store, record session.Record) <-chan saveResult {
	results := make(chan saveResult, 1)
	go func() {
		ref, err := store.Save(context.Background(), record)
		results <- saveResult{ref: ref, err: err}
	}()
	return results
}

// TestNewStoreRequiresBoundaries pins construction: the store opens no storage
// and refuses an incomplete boundary.
func TestNewStoreRequiresBoundaries(t *testing.T) {
	storage := newFakeStorage()
	clock := &fakeClock{now: testNow}
	if _, err := session.NewStore(session.StoreConfig{}); err == nil {
		t.Fatal("NewStore accepted no boundaries")
	}
	if _, err := session.NewStore(session.StoreConfig{Storage: storage}); err == nil {
		t.Fatal("NewStore accepted no clock")
	}
	if _, err := session.NewStore(session.StoreConfig{Clock: clock}); err == nil {
		t.Fatal("NewStore accepted no storage")
	}
}

// TestSaveReturnsProofOnlyAfterCommit pins the checkpoint-before-ask ordering:
// while the durable commit is unacknowledged Save has returned no reference, and
// the acknowledged reference immediately restores the saved context.
func TestSaveReturnsProofOnlyAfterCommit(t *testing.T) {
	storage := newFakeStorage()
	storage.enter = make(chan struct{}, 1)
	storage.release = make(chan struct{})
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	record := baseRecord()
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.Messages = append(record.Messages, session.Message{Role: "user", Content: "which option should I take?"})
	results := saveAsync(store, record)

	select {
	case <-storage.enter:
	case <-time.After(2 * time.Second):
		t.Fatal("Save never reached the storage commit")
	}
	select {
	case result := <-results:
		t.Fatalf("Save returned %q/%v before the commit was acknowledged", result.ref, result.err)
	case <-time.After(50 * time.Millisecond):
	}

	close(storage.release)
	var result saveResult
	select {
	case result = <-results:
	case <-time.After(2 * time.Second):
		t.Fatal("Save never returned after the commit was acknowledged")
	}
	if result.err != nil {
		t.Fatalf("Save: %v", result.err)
	}
	if _, err := session.ParseRef(string(result.ref)); err != nil {
		t.Fatalf("Save returned an invalid reference %q: %v", result.ref, err)
	}
	if storage.casCount() != 1 {
		t.Fatalf("CompareAndSwap calls = %d; want 1", storage.casCount())
	}

	restored, err := store.Restore(context.Background(), result.ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Revision != 1 {
		t.Fatalf("restored revision = %d; want 1", restored.Revision)
	}
	if !reflect.DeepEqual(restored.Messages, record.Messages) {
		t.Fatalf("restored messages = %+v; want the saved ask context", restored.Messages)
	}
}

// TestSaveCrashBeforeEffectReturnsNoProof pins the crash-before-effect
// boundary: no reference is returned, nothing durable exists, and a definite
// failure may be retried from the same expected revision.
func TestSaveCrashBeforeEffectReturnsNoProof(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})
	storage.failBefore = errors.New("storage unavailable")

	ref, err := store.Save(context.Background(), baseRecord())
	if err == nil {
		t.Fatal("Save reported success without a durable commit")
	}
	if ref != "" {
		t.Fatalf("failed Save returned proof %q", ref)
	}
	if storage.head("sess-1") != 0 {
		t.Fatalf("failed Save advanced the head to %d", storage.head("sess-1"))
	}
	if committed, ok := storage.refFor("sess-1"); ok {
		t.Fatalf("failed Save committed %q", committed)
	}

	storage.failBefore = nil
	retried, err := store.Save(context.Background(), baseRecord())
	if err != nil {
		t.Fatalf("retry after a definite failure: %v", err)
	}
	if retried == "" {
		t.Fatal("retry returned no reference")
	}
	restored, err := store.Restore(context.Background(), retried)
	if err != nil {
		t.Fatalf("Restore after retry: %v", err)
	}
	if restored.Revision != 1 {
		t.Fatalf("revision after retry = %d; want 1", restored.Revision)
	}
}

// TestSaveLostResponseReturnsNoProof pins the lost-response-after-effect
// boundary: the commit happened but Save reports no proof, and a blind retry
// conflicts instead of duplicating the checkpoint.
func TestSaveLostResponseReturnsNoProof(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})
	storage.failAfter = errors.New("response lost")

	ref, err := store.Save(context.Background(), baseRecord())
	if err == nil {
		t.Fatal("Save reported success after a lost response")
	}
	if ref != "" {
		t.Fatalf("uncertain Save returned proof %q", ref)
	}
	if storage.head("sess-1") != 1 {
		t.Fatalf("head after the uncertain commit = %d; want 1", storage.head("sess-1"))
	}
	committed, ok := storage.refFor("sess-1")
	if !ok {
		t.Fatal("the uncertain commit left no checkpoint")
	}

	retried, retryErr := store.Save(context.Background(), baseRecord())
	if retryErr == nil {
		t.Fatal("blind retry duplicated the commit")
	}
	if class, ok := fault.ClassOf(retryErr); !ok || class != fault.VersionConflict {
		t.Fatalf("blind retry error = %v; want %s", retryErr, fault.VersionConflict)
	}
	if retried != "" {
		t.Fatalf("conflicting retry returned proof %q", retried)
	}

	restored, err := store.Restore(context.Background(), committed)
	if err != nil {
		t.Fatalf("Restore of the committed checkpoint: %v", err)
	}
	if restored.Revision != 1 {
		t.Fatalf("committed revision = %d; want 1", restored.Revision)
	}
}

// TestConcurrentSavesHaveOneWinner pins compare-and-swap serialization: two
// saves of the same expected revision cannot both commit, and the loser is a
// typed version conflict with no proof.
func TestConcurrentSavesHaveOneWinner(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	start := make(chan struct{})
	results := make(chan saveResult, 2)
	for range 2 {
		go func() {
			<-start
			ref, err := store.Save(context.Background(), baseRecord())
			results <- saveResult{ref: ref, err: err}
		}()
	}
	close(start)

	var winner session.Ref
	var conflicts int
	for range 2 {
		var result saveResult
		select {
		case result = <-results:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent Save hung")
		}
		if result.err == nil {
			if winner != "" {
				t.Fatal("two concurrent saves both committed")
			}
			if result.ref == "" {
				t.Fatal("winning Save returned no reference")
			}
			winner = result.ref
			continue
		}
		if result.ref != "" {
			t.Fatalf("losing Save returned proof %q", result.ref)
		}
		if class, ok := fault.ClassOf(result.err); !ok || class != fault.VersionConflict {
			t.Fatalf("losing Save error = %v; want %s", result.err, fault.VersionConflict)
		}
		conflicts++
	}
	if winner == "" || conflicts != 1 {
		t.Fatalf("winner = %q, conflicts = %d; want one winner and one conflict", winner, conflicts)
	}
	if storage.head("sess-1") != 1 {
		t.Fatalf("head = %d; want exactly one committed revision", storage.head("sess-1"))
	}
	restored, err := store.Restore(context.Background(), winner)
	if err != nil {
		t.Fatalf("Restore(winner): %v", err)
	}
	if restored.Revision != 1 {
		t.Fatalf("winner revision = %d; want 1", restored.Revision)
	}
}

// TestSaveRevisionStaleness pins the numeric revision: a stale expected
// revision conflicts, the next revision commits, and an older checkpoint
// reference stays immutable.
func TestSaveRevisionStaleness(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	first := baseRecord()
	first.Retention = session.RetentionPolicy{RawContent: true}
	firstRef, err := store.Save(context.Background(), first)
	if err != nil {
		t.Fatalf("first Save: %v", err)
	}

	second := baseRecord()
	second.Revision = 1
	second.Retention = session.RetentionPolicy{RawContent: true}
	second.Messages = append(second.Messages, session.Message{Role: "user", Content: "next step"})
	secondRef, err := store.Save(context.Background(), second)
	if err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if secondRef == firstRef {
		t.Fatal("a new revision reused the previous checkpoint reference")
	}

	stale := baseRecord()
	stale.Revision = 0
	if ref, err := store.Save(context.Background(), stale); err == nil {
		t.Fatalf("stale Save committed %q", ref)
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("stale Save error = %v; want %s", err, fault.VersionConflict)
	}

	oldRestored, err := store.Restore(context.Background(), firstRef)
	if err != nil {
		t.Fatalf("Restore(first): %v", err)
	}
	if oldRestored.Revision != 1 {
		t.Fatalf("first checkpoint revision = %d; want 1", oldRestored.Revision)
	}
	if !reflect.DeepEqual(oldRestored.Messages, first.Messages) {
		t.Fatalf("first checkpoint changed: %+v", oldRestored.Messages)
	}
	if oldRestored.Version != session.RefVersion {
		t.Fatalf("format version = %q; want %q", oldRestored.Version, session.RefVersion)
	}

	newRestored, err := store.Restore(context.Background(), secondRef)
	if err != nil {
		t.Fatalf("Restore(second): %v", err)
	}
	if newRestored.Revision != 2 {
		t.Fatalf("second checkpoint revision = %d; want 2", newRestored.Revision)
	}
	if !reflect.DeepEqual(newRestored.Messages, second.Messages) {
		t.Fatalf("second checkpoint messages = %+v; want %+v", newRestored.Messages, second.Messages)
	}
}

// TestSaveRefusesInvalidRecordsBeforeStorage pins validation and revision
// overflow: the storage boundary is never reached.
func TestSaveRefusesInvalidRecordsBeforeStorage(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	cases := map[string]func(*session.Record){
		"missing session id": func(record *session.Record) { record.SessionID = "" },
		"invalid session id": func(record *session.Record) { record.SessionID = "sess/1" },
		"no messages":        func(record *session.Record) { record.Messages = nil },
		"revision overflow":  func(record *session.Record) { record.Revision = math.MaxUint64 },
	}
	for name, mutate := range cases {
		record := baseRecord()
		mutate(&record)
		ref, err := store.Save(context.Background(), record)
		if err == nil {
			t.Errorf("%s: Save committed %q", name, ref)
		}
		if ref != "" {
			t.Errorf("%s: Save returned proof %q", name, ref)
		}
	}
	if storage.casCount() != 0 {
		t.Fatalf("CompareAndSwap calls = %d; want 0", storage.casCount())
	}
}

// TestSaveRefusesUnparseableStorageReference pins the durable-proof rule: a
// backend reference that is not a checkpoint reference is not proof.
func TestSaveRefusesUnparseableStorageReference(t *testing.T) {
	storage := newFakeStorage()
	storage.badRef = true
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	ref, err := store.Save(context.Background(), baseRecord())
	if err == nil {
		t.Fatal("Save accepted an unparseable storage reference")
	}
	if ref != "" {
		t.Fatalf("Save returned the unparseable reference %q", ref)
	}
}

// TestSaveFiltersSecretsBeforePersistence pins redaction: supplied sensitive
// values and bearer credentials never reach the storage boundary.
func TestSaveFiltersSecretsBeforePersistence(t *testing.T) {
	const supplied = "hunter2-supplied"
	const bearer = "sk-live-abc123"
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow}, supplied)

	record := baseRecord()
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.Messages = []session.Message{
		{Role: "user", Content: "key=" + supplied},
		{Role: "assistant", Content: "Authorization: Bearer " + bearer},
	}
	record.Outcome = session.Outcome{Status: "completed", Summary: "used " + supplied}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	persisted := storage.lastRecord()
	if len(persisted.Messages) != 2 {
		t.Fatalf("persisted messages = %+v", persisted.Messages)
	}
	if persisted.Messages[0].Content != "key=[redacted]" {
		t.Fatalf("persisted supplied secret content = %q", persisted.Messages[0].Content)
	}
	if persisted.Messages[1].Content != "Authorization: Bearer [redacted]" {
		t.Fatalf("persisted bearer content = %q", persisted.Messages[1].Content)
	}
	if persisted.Outcome.Summary != "used [redacted]" {
		t.Fatalf("persisted outcome summary = %q", persisted.Outcome.Summary)
	}
	for _, value := range []string{supplied, bearer} {
		if strings.Contains(persisted.Messages[0].Content+persisted.Messages[1].Content+persisted.Outcome.Summary, value) {
			t.Fatalf("persisted record still carries %q", value)
		}
	}

	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if strings.Contains(restored.Messages[0].Content+restored.Messages[1].Content+restored.Outcome.Summary, bearer) {
		t.Fatal("restored record leaked a credential")
	}
}

// TestSaveFiltersMixedCaseCredentialShapes pins that the persistence filter
// covers the same credential forms as the knowledge refusal gate regardless of
// letter case: API-key assignments, bearer headers, PEM private key blocks and
// AWS access key ids never reach the storage boundary.
func TestSaveFiltersMixedCaseCredentialShapes(t *testing.T) {
	const (
		apiKey  = "sk-live-APIKEY-0001"
		bearer  = "sk-live-BEARER-0002"
		pemBody = "MIIEowIBAAKCAQEA0003"
		awsKey  = "AKIAIOSFODNN7EXAMPLE"
	)
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	record := baseRecord()
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.Messages = []session.Message{
		{Role: "user", Content: "API_KEY=" + apiKey},
		{Role: "assistant", Content: "Authorization: BEARER " + bearer},
		{Role: "tool", Content: "-----BEGIN private key-----\n" + pemBody + "\n-----END private key-----"},
		{Role: "tool", Content: "aws_access_key_id = " + awsKey},
	}
	record.Outcome = session.Outcome{Status: "completed", Summary: "used api_key=" + apiKey}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	persisted := storage.lastRecord()
	if len(persisted.Messages) != 4 {
		t.Fatalf("persisted messages = %+v", persisted.Messages)
	}
	if persisted.Messages[0].Content != "API_KEY=[redacted]" {
		t.Fatalf("persisted API-key content = %q", persisted.Messages[0].Content)
	}
	if persisted.Messages[1].Content != "Authorization: BEARER [redacted]" {
		t.Fatalf("persisted bearer content = %q", persisted.Messages[1].Content)
	}
	if persisted.Messages[2].Content != "[redacted]" {
		t.Fatalf("persisted private-key content = %q", persisted.Messages[2].Content)
	}
	if persisted.Messages[3].Content != "aws_access_key_id = [redacted]" {
		t.Fatalf("persisted AWS-key content = %q", persisted.Messages[3].Content)
	}
	if persisted.Outcome.Summary != "used api_key=[redacted]" {
		t.Fatalf("persisted outcome summary = %q", persisted.Outcome.Summary)
	}
	combined := ""
	for _, message := range persisted.Messages {
		combined += message.Content
	}
	combined += persisted.Outcome.Summary
	for _, canary := range []string{apiKey, bearer, pemBody, awsKey} {
		if strings.Contains(combined, canary) {
			t.Fatalf("persisted record still carries %q", canary)
		}
	}

	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, message := range restored.Messages {
		for _, canary := range []string{apiKey, bearer, pemBody, awsKey} {
			if strings.Contains(message.Content, canary) {
				t.Fatalf("restored record leaked %q", canary)
			}
		}
	}
}

// TestSaveRedactsIdentifierPrefixedCredentialLabels pins that a credential
// label carried as the suffix of an identifier is redacted before persistence.
// '_' is a word character, so such labels have no word boundary of their own;
// the persistence filter must still match them, in plain, quoted and export
// form, so no canary reaches the storage boundary.
func TestSaveRedactsIdentifierPrefixedCredentialLabels(t *testing.T) {
	const (
		githubToken = "ghp_PREFIXED_0001"
		awsSecret   = "aws-prefixed-0002"
		jsonValue   = "json-prefixed-0003"
	)
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	record := baseRecord()
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.Messages = []session.Message{
		{Role: "user", Content: "GITHUB_TOKEN=" + githubToken},
		{Role: "tool", Content: "export AWS_SECRET_ACCESS_KEY=" + awsSecret},
		{Role: "assistant", Content: `{"api_key": "` + jsonValue + `"}`},
	}
	record.Outcome = session.Outcome{Status: "completed", Summary: "used db_password=" + awsSecret}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	persisted := storage.lastRecord()
	if len(persisted.Messages) != 3 {
		t.Fatalf("persisted messages = %+v", persisted.Messages)
	}
	if persisted.Messages[0].Content != "GITHUB_TOKEN=[redacted]" {
		t.Fatalf("persisted prefixed token content = %q", persisted.Messages[0].Content)
	}
	if persisted.Messages[1].Content != "export AWS_SECRET_ACCESS_KEY=[redacted]" {
		t.Fatalf("persisted prefixed access-key content = %q", persisted.Messages[1].Content)
	}
	if persisted.Messages[2].Content != `{"api_key": "[redacted]"}` {
		t.Fatalf("persisted quoted api-key content = %q", persisted.Messages[2].Content)
	}
	if persisted.Outcome.Summary != "used db_password=[redacted]" {
		t.Fatalf("persisted outcome summary = %q", persisted.Outcome.Summary)
	}
	combined := persisted.Outcome.Summary
	for _, message := range persisted.Messages {
		combined += message.Content
	}
	for _, canary := range []string{githubToken, awsSecret, jsonValue} {
		if strings.Contains(combined, canary) {
			t.Fatalf("persisted record still carries %q", canary)
		}
	}

	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restoredText := restored.Outcome.Summary
	for _, message := range restored.Messages {
		restoredText += message.Content
	}
	for _, canary := range []string{githubToken, awsSecret, jsonValue} {
		if strings.Contains(restoredText, canary) {
			t.Fatalf("restored record leaked %q", canary)
		}
	}
}

// TestRawContentDefaultOff pins retention: raw model/tool content is not
// persisted by default, while attributable usage and the outcome status are.
func TestRawContentDefaultOff(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	record := baseRecord()
	record.Usage = []session.Usage{{PrincipalID: "prn-1", Model: "model-a", Prompt: 10, Completion: 5}}
	record.Outcome = session.Outcome{Status: "completed", Summary: "raw tool output"}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	persisted := storage.lastRecord()
	for _, message := range persisted.Messages {
		if message.Content != "" {
			t.Fatalf("default retention persisted raw content %q", message.Content)
		}
	}
	if persisted.Outcome.Summary != "" {
		t.Fatalf("default retention persisted the raw outcome summary %q", persisted.Outcome.Summary)
	}

	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, message := range restored.Messages {
		if message.Content != "" {
			t.Fatalf("default retention restored raw content %q", message.Content)
		}
	}
	if len(restored.Messages) != len(record.Messages) {
		t.Fatalf("restored %d messages; want %d roles", len(restored.Messages), len(record.Messages))
	}
	for index, message := range restored.Messages {
		if message.Role != record.Messages[index].Role {
			t.Fatalf("restored role %d = %q; want %q", index, message.Role, record.Messages[index].Role)
		}
	}
	if !reflect.DeepEqual(restored.Usage, record.Usage) {
		t.Fatalf("restored usage = %+v; want %+v", restored.Usage, record.Usage)
	}
	if restored.Outcome.Status != "completed" {
		t.Fatalf("restored outcome status = %q; want completed", restored.Outcome.Status)
	}
}

// TestOptInRetentionExpiry pins authorized raw-content retention: content is
// available before the seven-day default deadline and withheld at it, while
// attribution survives.
func TestOptInRetentionExpiry(t *testing.T) {
	storage := newFakeStorage()
	clock := &fakeClock{now: testNow}
	store := newTestStore(t, storage, clock)

	record := baseRecord()
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.Usage = []session.Usage{{PrincipalID: "prn-1", Model: "model-a", Prompt: 10, Completion: 5}}
	record.Outcome = session.Outcome{Status: "completed", Summary: "raw tool output"}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	clock.now = testNow.Add(session.DefaultRawContentRetention - time.Nanosecond)
	before, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore before expiry: %v", err)
	}
	if !reflect.DeepEqual(before.Messages, record.Messages) {
		t.Fatalf("raw content was withheld before expiry: %+v", before.Messages)
	}
	if before.Outcome.Summary != "raw tool output" {
		t.Fatalf("raw outcome summary was withheld before expiry: %q", before.Outcome.Summary)
	}

	clock.now = testNow.Add(session.DefaultRawContentRetention)
	expired, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore at expiry: %v", err)
	}
	for _, message := range expired.Messages {
		if message.Content != "" {
			t.Fatalf("expired retention restored raw content %q", message.Content)
		}
	}
	if expired.Outcome.Summary != "" {
		t.Fatalf("expired retention restored the raw outcome summary %q", expired.Outcome.Summary)
	}
	if !reflect.DeepEqual(expired.Usage, record.Usage) {
		t.Fatalf("expiry dropped usage attribution: %+v", expired.Usage)
	}
	if expired.Outcome.Status != "completed" {
		t.Fatalf("expiry dropped the outcome status: %q", expired.Outcome.Status)
	}
	if expired.Revision != 1 {
		t.Fatalf("expiry changed the revision to %d", expired.Revision)
	}
}

const toolCorrelationSecret = "sk-live-CORRELATION-0001"

// correlatedRecord is a valid checkpoint whose conversation carries the
// tool-call correlation: an assistant call and the tool message answering it.
func correlatedRecord() session.Record {
	record := baseRecord()
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.Messages = []session.Message{
		{Role: "user", Content: "read the file"},
		{Role: "assistant", Content: "calling", ToolCalls: []session.ToolCall{
			{ID: "call-1", Name: "repo_read", Arguments: `{"api_key":"` + toolCorrelationSecret + `","path":"README.md"}`},
		}},
		{Role: "tool", Content: "result api_key=" + toolCorrelationSecret, ToolCallID: "call-1"},
	}
	return record
}

// TestRecordValidatesToolCorrelation pins the record-level role rules and
// bounds for a present correlation member while an absent member stays valid,
// so a legacy uncorrelated tool message keeps its shape.
func TestRecordValidatesToolCorrelation(t *testing.T) {
	valid := correlatedRecord()
	if err := valid.Validate(); err != nil {
		t.Fatalf("correlated record refused: %v", err)
	}

	legacy := baseRecord()
	legacy.Messages = []session.Message{{Role: "tool", Content: "legacy output"}}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy tool message without a call identity refused: %v", err)
	}

	call := session.ToolCall{ID: "call-1", Name: "repo_read", Arguments: `{}`}
	tooMany := make([]session.ToolCall, 33)
	for index := range tooMany {
		tooMany[index] = session.ToolCall{ID: "call-" + strings.Repeat("x", index+1), Name: "repo_read", Arguments: `{}`}
	}
	invalid := []session.Record{
		recordWithMessages([]session.Message{{Role: "assistant", ToolCallID: "call-1"}}),
		recordWithMessages([]session.Message{{Role: "tool", ToolCalls: []session.ToolCall{call}}}),
		recordWithMessages([]session.Message{{Role: "user", ToolCalls: []session.ToolCall{call}}}),
		recordWithMessages([]session.Message{{Role: "system", ToolCallID: "call-1"}}),
		recordWithMessages([]session.Message{{Role: "tool", ToolCallID: "call/1"}}),
		recordWithMessages([]session.Message{{Role: "assistant", ToolCalls: []session.ToolCall{call, call}}}),
		recordWithMessages([]session.Message{{Role: "assistant", ToolCalls: tooMany}}),
		recordWithMessages([]session.Message{{Role: "assistant", ToolCalls: []session.ToolCall{{ID: "call-1", Name: "repo.read", Arguments: `{}`}}}}),
		recordWithMessages([]session.Message{{Role: "assistant", ToolCalls: []session.ToolCall{{ID: "call-1", Name: "repo_read", Arguments: `[]`}}}}),
		recordWithMessages([]session.Message{{Role: "assistant", ToolCalls: []session.ToolCall{{ID: "call-1", Name: "repo_read", Arguments: `{"p":"` + strings.Repeat("a", 64<<10) + `"}`}}}}),
	}
	for _, record := range invalid {
		if err := record.Validate(); err == nil {
			t.Errorf("Record.Validate accepted %+v", record.Messages)
		}
	}
}

// recordWithMessages builds a valid base record around one message list.
func recordWithMessages(messages []session.Message) session.Record {
	record := baseRecord()
	record.Messages = messages
	return record
}

// TestSessionMessageDecodesToolCorrelation pins strict session-message
// decoding: correlation members decode, absent members decode as before, and
// an unknown member at any nesting level is refused.
func TestSessionMessageDecodesToolCorrelation(t *testing.T) {
	var message session.Message
	if err := json.Unmarshal([]byte(`{"role":"tool","content":"done","tool_call_id":"call-1"}`), &message); err != nil {
		t.Fatalf("tool message with a call identity refused: %v", err)
	}
	if message.Role != "tool" || message.Content != "done" || message.ToolCallID != "call-1" || len(message.ToolCalls) != 0 {
		t.Fatalf("decoded tool message = %+v", message)
	}

	var legacy session.Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &legacy); err != nil {
		t.Fatalf("legacy message refused: %v", err)
	}
	if legacy.Role != "user" || legacy.Content != "hi" || legacy.ToolCallID != "" || len(legacy.ToolCalls) != 0 {
		t.Fatalf("legacy message decoded as %+v", legacy)
	}

	var assistant session.Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"","tool_calls":[{"id":"call-1","name":"repo_read","arguments":"{}"}]}`), &assistant); err != nil {
		t.Fatalf("assistant message with tool calls refused: %v", err)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-1" {
		t.Fatalf("decoded assistant tool calls = %+v", assistant.ToolCalls)
	}

	for _, raw := range []string{
		`{"role":"user","content":"hi","refusal":null}`,
		`{"role":"assistant","content":"","tool_calls":[{"id":"call-1","name":"repo_read","arguments":"{}","type":"function"}]}`,
		`{"role":"user","content":"hi"} {"role":"user","content":"again"}`,
	} {
		var decoded session.Message
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			t.Errorf("strict decode accepted %s as %+v", raw, decoded)
		}
	}
}

// TestStoreTreatsToolCorrelationAsRawContent pins the retention decision: the
// tool-call list and the tool-call id are raw transcript content. Without
// raw-content retention they are withheld with the content; with retention the
// call arguments are redacted like content and the correlation identity
// survives; expiry withholds the correlation again, leaving a valid record.
func TestStoreTreatsToolCorrelationAsRawContent(t *testing.T) {
	storage := newFakeStorage()
	clock := &fakeClock{now: testNow}
	store := newTestStore(t, storage, clock)

	record := correlatedRecord()
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	persisted := storage.lastRecord()
	assistant := persisted.Messages[1]
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("persisted assistant tool calls = %+v; want one retained call", assistant.ToolCalls)
	}
	if assistant.ToolCalls[0].ID != "call-1" || assistant.ToolCalls[0].Name != "repo_read" {
		t.Fatalf("persisted call identity = %+v; want call-1/repo_read", assistant.ToolCalls[0])
	}
	if strings.Contains(assistant.ToolCalls[0].Arguments, toolCorrelationSecret) {
		t.Fatalf("persisted call arguments leaked the secret: %s", assistant.ToolCalls[0].Arguments)
	}
	if !json.Valid([]byte(assistant.ToolCalls[0].Arguments)) || !strings.HasPrefix(assistant.ToolCalls[0].Arguments, "{") {
		t.Fatalf("redacted call arguments are not a JSON object: %s", assistant.ToolCalls[0].Arguments)
	}
	if persisted.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("persisted tool-call id = %q; want call-1", persisted.Messages[2].ToolCallID)
	}
	if persisted.Messages[2].Content != "result api_key=[redacted]" {
		t.Fatalf("persisted tool content = %q", persisted.Messages[2].Content)
	}

	before, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore before expiry: %v", err)
	}
	if len(before.Messages[1].ToolCalls) != 1 || before.Messages[1].ToolCalls[0].ID != "call-1" {
		t.Fatalf("retained correlation was lost before expiry: %+v", before.Messages[1])
	}
	if strings.Contains(before.Messages[1].ToolCalls[0].Arguments, toolCorrelationSecret) {
		t.Fatal("restored call arguments leaked the secret")
	}
	if before.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("restored tool-call id = %q; want call-1", before.Messages[2].ToolCallID)
	}

	clock.now = testNow.Add(session.DefaultRawContentRetention)
	expired, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore at expiry: %v", err)
	}
	for index, message := range expired.Messages {
		if message.Content != "" || len(message.ToolCalls) != 0 || message.ToolCallID != "" {
			t.Fatalf("expired message %d kept raw correlation/content: %+v", index, message)
		}
	}
	if err := expired.Validate(); err != nil {
		t.Fatalf("withheld record is not valid: %v", err)
	}
}

// TestRawContentDefaultOffWithholdsToolCorrelation pins the default retention
// rule for the new fields: no call list and no tool-call id reaches storage or
// a restore.
func TestRawContentDefaultOffWithholdsToolCorrelation(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	record := correlatedRecord()
	record.Retention = session.RetentionPolicy{}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertCorrelationWithheld(t, "persisted", storage.lastRecord())
	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assertCorrelationWithheld(t, "restored", restored)
}

func TestPlaceholderTranscriptPreservesRedactedPromptsAndSyntheticCorrelation(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	record := baseRecord()
	record.Retention = session.RetentionPolicy{PlaceholderTranscript: true}
	record.Messages = []session.Message{
		{Role: "system", Content: "guide Bearer top-secret-system"},
		{Role: "user", Content: "run API_KEY=" + toolCorrelationSecret},
		{Role: "assistant", Content: "raw assistant thinking", ToolCalls: []session.ToolCall{
			{ID: "provider-id-1", Name: "kd_repo_0read", Arguments: `{"api_key":"` + toolCorrelationSecret + `"}`},
		}},
		{Role: "tool", Content: "raw tool output", ToolCallID: "provider-id-1"},
		{Role: "assistant", Content: "raw final answer"},
	}
	record.Outcome = session.Outcome{Status: "checkpoint", Summary: "raw summary"}

	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, check := range []struct {
		label  string
		record session.Record
	}{
		{"persisted", storage.lastRecord()},
	} {
		if check.record.Outcome.Summary != "" {
			t.Fatalf("%s outcome summary = %q; want empty", check.label, check.record.Outcome.Summary)
		}
		if check.record.Messages[0].Content != "guide Bearer [redacted]" {
			t.Fatalf("%s system content = %q", check.label, check.record.Messages[0].Content)
		}
		if check.record.Messages[1].Content != "run API_KEY=[redacted]" {
			t.Fatalf("%s user content = %q", check.label, check.record.Messages[1].Content)
		}
		if check.record.Messages[2].Content != "" || len(check.record.Messages[2].ToolCalls) != 1 {
			t.Fatalf("%s assistant call turn = %+v", check.label, check.record.Messages[2])
		}
		call := check.record.Messages[2].ToolCalls[0]
		if call.ID != "call-durable-1" || call.Arguments != "{}" {
			t.Fatalf("%s synthetic call = %+v", check.label, call)
		}
		if check.record.Messages[3].ToolCallID != "call-durable-1" || check.record.Messages[3].Content != "[tool output not retained]" {
			t.Fatalf("%s tool message = %+v", check.label, check.record.Messages[3])
		}
		if check.record.Messages[4].Content != "[assistant output not retained]" {
			t.Fatalf("%s final assistant = %+v", check.label, check.record.Messages[4])
		}
	}

	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !reflect.DeepEqual(restored.Messages, storage.lastRecord().Messages) {
		t.Fatalf("restored messages = %+v; want %+v", restored.Messages, storage.lastRecord().Messages)
	}
	if restored.Outcome.Summary != "" {
		t.Fatalf("restored summary = %q; want empty", restored.Outcome.Summary)
	}
}

func assertCorrelationWithheld(t *testing.T, what string, record session.Record) {
	t.Helper()
	for index, message := range record.Messages {
		if message.Content != "" {
			t.Fatalf("%s message %d kept raw content %q", what, index, message.Content)
		}
		if len(message.ToolCalls) != 0 {
			t.Fatalf("%s message %d kept tool calls %+v", what, index, message.ToolCalls)
		}
		if message.ToolCallID != "" {
			t.Fatalf("%s message %d kept tool-call id %q", what, index, message.ToolCallID)
		}
	}
}

// TestStoreCopiesToolCorrelation pins copying: a caller mutating its message
// after Save, or a caller mutating a restored record, cannot change the record
// the storage boundary holds.
func TestStoreCopiesToolCorrelation(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	record := correlatedRecord()
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	record.Messages[1].ToolCalls[0].Arguments = `{"mutated":true}`
	record.Messages[1].ToolCalls = append(record.Messages[1].ToolCalls, session.ToolCall{ID: "call-2", Name: "repo_write", Arguments: `{}`})
	record.Messages[2].ToolCallID = "call-2"

	persisted := storage.lastRecord()
	if len(persisted.Messages[1].ToolCalls) != 1 || !strings.Contains(persisted.Messages[1].ToolCalls[0].Arguments, "README.md") {
		t.Fatalf("storage saw a caller mutation: %+v", persisted.Messages[1].ToolCalls)
	}
	if persisted.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("storage saw a mutated tool-call id %q", persisted.Messages[2].ToolCallID)
	}

	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored.Messages[1].ToolCalls[0].Arguments = `{"mutated":true}`
	again := storage.lastRecord()
	if !strings.Contains(again.Messages[1].ToolCalls[0].Arguments, "README.md") {
		t.Fatalf("mutating the restored record changed storage: %+v", again.Messages[1].ToolCalls)
	}
}

// recordWithToolArguments builds a valid correlated checkpoint with one call
// whose arguments carry the supplied JSON-object string.
func recordWithToolArguments(arguments string) session.Record {
	record := baseRecord()
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.Messages = []session.Message{
		{Role: "user", Content: "run it"},
		{Role: "assistant", Content: "calling", ToolCalls: []session.ToolCall{
			{ID: "call-1", Name: "repo_read", Arguments: arguments},
		}},
		{Role: "tool", Content: "ok", ToolCallID: "call-1"},
	}
	return record
}

// decodeToolArguments decodes a persisted call-arguments string strictly enough
// for an assertion: it must be a JSON object, and numbers keep their original
// spelling so a float round-trip cannot hide a precision loss.
func decodeToolArguments(t *testing.T, arguments string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("call arguments are not a JSON object: %q: %v", arguments, err)
	}
	if decoder.More() {
		t.Fatalf("call arguments carry trailing JSON data: %q", arguments)
	}
	return decoded
}

// toolArgumentsCase is one call-arguments redaction expectation: the arguments
// string to save, the extra configured secrets, the decoded object that must
// persist and the substrings that must not.
type toolArgumentsCase struct {
	name      string
	arguments string
	secrets   []string
	want      map[string]any
	forbidden []string
}

// runToolArgumentsCases saves each case through a fresh store and asserts the
// persisted call arguments are a bounded JSON object equal to want with nothing
// forbidden in it, and that Restore returns exactly the persisted string.
func runToolArgumentsCases(t *testing.T, cases []toolArgumentsCase) {
	t.Helper()
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			storage := newFakeStorage()
			store := newTestStore(t, storage, &fakeClock{now: testNow}, testCase.secrets...)

			ref, err := store.Save(context.Background(), recordWithToolArguments(testCase.arguments))
			if err != nil {
				t.Fatalf("Save: %v", err)
			}

			persisted := storage.lastRecord().Messages[1].ToolCalls[0].Arguments
			if !strings.HasPrefix(persisted, "{") || !json.Valid([]byte(persisted)) {
				t.Fatalf("persisted call arguments are not a JSON object: %q", persisted)
			}
			if len(persisted) > session.MaxToolArgumentsBytes {
				t.Fatalf("persisted call arguments are %d bytes; want at most %d", len(persisted), session.MaxToolArgumentsBytes)
			}
			for _, forbidden := range testCase.forbidden {
				if strings.Contains(persisted, forbidden) {
					t.Fatalf("persisted call arguments leaked %q: %q", forbidden, persisted)
				}
			}
			if got := decodeToolArguments(t, persisted); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("persisted call arguments = %s; want %v", persisted, testCase.want)
			}

			restored, err := store.Restore(context.Background(), ref)
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			restoredArguments := restored.Messages[1].ToolCalls[0].Arguments
			if restoredArguments != persisted {
				t.Fatalf("restored call arguments = %q; want the persisted %q", restoredArguments, persisted)
			}
			if got := decodeToolArguments(t, restoredArguments); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("restored call arguments = %s; want %v", restoredArguments, testCase.want)
			}
		})
	}
}

// TestStoreRedactsToolArgumentsStructurally pins that persisted call
// arguments are redacted structurally, not textually. Every string leaf
// loses its credential, a credential-shaped object key still redacts its whole
// string value, and the JSON shape, keys and numbers survive. Restore must
// succeed for every case; textual redaction can consume the closing quote
// and persist invalid JSON, losing the checkpoint.
func TestStoreRedactsToolArgumentsStructurally(t *testing.T) {
	cases := []toolArgumentsCase{
		{
			name:      "labelled assignment",
			arguments: `{"command":"export TOKEN=abc123"}`,
			want:      map[string]any{"command": "export TOKEN=[redacted]"},
			forbidden: []string{"abc123"},
		},
		{
			name:      "password assignment",
			arguments: `{"command":"password=hunter2"}`,
			want:      map[string]any{"command": "password=[redacted]"},
			forbidden: []string{"hunter2"},
		},
		{
			name:      "bearer header",
			arguments: `{"command":"Authorization: Bearer abc"}`,
			want:      map[string]any{"command": "Authorization: Bearer [redacted]"},
			forbidden: []string{"abc"},
		},
		{
			name:      "escaped quote in value",
			arguments: `{"command":"api_key=abc\"x"}`,
			want:      map[string]any{"command": "api_key=[redacted]"},
			forbidden: []string{`abc"x`},
		},
		{
			name:      "credential in a URL",
			arguments: `{"command":"https://h/?token=abc&x=1"}`,
			want:      map[string]any{"command": "https://h/?token=[redacted]"},
			forbidden: []string{"abc&x=1"},
		},
		{
			name:      "quoted credential key",
			arguments: `{"api_key":"` + toolCorrelationSecret + `","path":"README.md","nested":{"token":"another-secret","keep":true}}`,
			want: map[string]any{
				"api_key": "[redacted]",
				"path":    "README.md",
				"nested":  map[string]any{"token": "[redacted]", "keep": true},
			},
			forbidden: []string{toolCorrelationSecret, "another-secret"},
		},
		{
			name:      "nested objects and arrays",
			arguments: `{"command":"go","env":{"TOKEN":"topsecret","list":["password=hunter2",{"api_key":"nested-secret"}],"code":1}}`,
			want: map[string]any{
				"command": "go",
				"env": map[string]any{
					"TOKEN": "[redacted]",
					"list": []any{
						"password=[redacted]",
						map[string]any{"api_key": "[redacted]"},
					},
					"code": json.Number("1"),
				},
			},
			forbidden: []string{"topsecret", "hunter2", "nested-secret"},
		},
		{
			name:      "numbers preserved",
			arguments: `{"command":"no secret","count":1e3,"big":123456789012345678901234567890,"ratio":0.10}`,
			want: map[string]any{
				"command": "no secret",
				"count":   json.Number("1e3"),
				"big":     json.Number("123456789012345678901234567890"),
				"ratio":   json.Number("0.10"),
			},
		},
		{
			name:      "unicode and escapes preserved",
			arguments: `{"text":"caf\u00e9 <x> \"quoted\" \\ slash","emoji":"\ud83d\ude00"}`,
			want: map[string]any{
				"text":  "café <x> \"quoted\" \\ slash",
				"emoji": "😀",
			},
		},
	}

	runToolArgumentsCases(t, cases)
}

// TestStoreRedactsToolArgumentsUnderCredentialKeys pins that a
// credential-shaped object key withholds its WHOLE value whatever its JSON
// type, so an object, array, number, boolean or null under it never persists
// in clear. A key that itself embeds a credential assignment or a supplied
// secret is replaced by a deterministic, collision-free placeholder and its
// value is withheld; a plain sibling key keeps its value. Restore must keep
// every case byte-identical to what the store persisted.
func TestStoreRedactsToolArgumentsUnderCredentialKeys(t *testing.T) {
	cases := []toolArgumentsCase{
		{
			name:      "object value under secret key",
			arguments: `{"secret":{"value":"hunter2"}}`,
			want:      map[string]any{"secret": "[redacted]"},
			forbidden: []string{"hunter2"},
		},
		{
			name:      "string array under token key",
			arguments: `{"token":["abc","def"]}`,
			want:      map[string]any{"token": "[redacted]"},
			forbidden: []string{"abc", "def"},
		},
		{
			name:      "object value under api key",
			arguments: `{"api_key":{"v":"s1"}}`,
			want:      map[string]any{"api_key": "[redacted]"},
			forbidden: []string{"s1"},
		},
		{
			name:      "number under password key",
			arguments: `{"password":123456}`,
			want:      map[string]any{"password": "[redacted]"},
			forbidden: []string{"123456"},
		},
		{
			name:      "null and booleans under credential keys",
			arguments: `{"token":null,"secret":true,"password":false}`,
			want:      map[string]any{"token": "[redacted]", "secret": "[redacted]", "password": "[redacted]"},
		},
		{
			name:      "arrays of objects keep their shape",
			arguments: `{"list":[{"token":"array-leak-one"},{"note":"kept"},{"nest":{"api_key":"array-leak-two"}}]}`,
			want: map[string]any{"list": []any{
				map[string]any{"token": "[redacted]"},
				map[string]any{"note": "kept"},
				map[string]any{"nest": map[string]any{"api_key": "[redacted]"}},
			}},
			forbidden: []string{"array-leak-one", "array-leak-two"},
		},
		{
			name:      "nested credential key in a non-credential parent",
			arguments: `{"request":{"headers":{"api_key":"deep-leak"},"path":"/v1/items"}}`,
			want: map[string]any{"request": map[string]any{
				"headers": map[string]any{"api_key": "[redacted]"},
				"path":    "/v1/items",
			}},
			forbidden: []string{"deep-leak"},
		},
		{
			name:      "non-credential sibling stays intact",
			arguments: `{"api_key":"sibling-secret","command":"go test ./...","count":2,"dry_run":true}`,
			want: map[string]any{
				"api_key": "[redacted]",
				"command": "go test ./...",
				"count":   json.Number("2"),
				"dry_run": true,
			},
			forbidden: []string{"sibling-secret"},
		},
		{
			name:      "key embedding an assignment is renamed",
			arguments: `{"token=abc123":"v"}`,
			want:      map[string]any{"[redacted-key-1]": "[redacted]"},
			forbidden: []string{"abc123"},
		},
		{
			name:      "container value under an assignment key",
			arguments: `{"token=abc456":{"inner":"container-leak"}}`,
			want:      map[string]any{"[redacted-key-1]": "[redacted]"},
			forbidden: []string{"abc456", "container-leak"},
		},
		{
			name:      "key equal to a supplied secret is renamed",
			arguments: `{"hunter2-supplied":"v"}`,
			secrets:   []string{"hunter2-supplied"},
			want:      map[string]any{"[redacted-key-1]": "[redacted]"},
			forbidden: []string{"hunter2-supplied"},
		},
		{
			name:      "placeholder increments past existing keys",
			arguments: `{"!token=abc789":"v","[redacted-key-1]":"keep-one","[redacted-key-2]":"keep-two"}`,
			want: map[string]any{
				"[redacted-key-3]": "[redacted]",
				"[redacted-key-1]": "keep-one",
				"[redacted-key-2]": "keep-two",
			},
			forbidden: []string{"abc789"},
		},
		// A credential label may be the tail of a key separated by any
		// character, not only by an identifier-style '_' or '-'. The textual
		// assignment pass already redacted these labels; the structural key
		// pass must not let the same key through in clear.
		{
			name:      "dotted password key",
			arguments: `{"db.password":"hunter3"}`,
			want:      map[string]any{"db.password": "[redacted]"},
			forbidden: []string{"hunter3"},
		},
		{
			name:      "dotted token key with a container value",
			arguments: `{"auth.token":{"nested":"container-leak-three"}}`,
			want:      map[string]any{"auth.token": "[redacted]"},
			forbidden: []string{"container-leak-three"},
		},
		{
			name:      "slash-prefixed secret key",
			arguments: `{"x/secret":"h7"}`,
			want:      map[string]any{"x/secret": "[redacted]"},
			forbidden: []string{"h7"},
		},
		{
			name:      "colon-prefixed secret key",
			arguments: `{"aws:secret":"hunter8"}`,
			want:      map[string]any{"aws:secret": "[redacted]"},
			forbidden: []string{"hunter8"},
		},
		{
			name:      "space-separated password key",
			arguments: `{"my password":"h4"}`,
			want:      map[string]any{"my password": "[redacted]"},
			forbidden: []string{"h4"},
		},
		{
			name:      "non-ASCII letter before the label",
			arguments: `{"é_token":"h6"}`,
			want:      map[string]any{"é_token": "[redacted]"},
			forbidden: []string{"h6"},
		},
		{
			name:      "leading space before the label",
			arguments: `{" token":"h5"}`,
			want:      map[string]any{" token": "[redacted]"},
			forbidden: []string{"h5"},
		},
		{
			name:      "dotted password key with a number value",
			arguments: `{"db.password":123456}`,
			want:      map[string]any{"db.password": "[redacted]"},
			forbidden: []string{"123456"},
		},
		{
			name:      "slash-prefixed secret key with an array value",
			arguments: `{"x/secret":["array-leak-three","array-leak-four"]}`,
			want:      map[string]any{"x/secret": "[redacted]"},
			forbidden: []string{"array-leak-three", "array-leak-four"},
		},
		// A key that merely begins with or embeds a label without a separating
		// boundary is not a credential key: its value must survive.
		{
			name:      "identifier-shaped non-credential keys keep their values",
			arguments: `{"max_tokens":1024,"token_count":3,"tokens_used":7,"author":"ada","secretary":"bea"}`,
			want: map[string]any{
				"max_tokens":  json.Number("1024"),
				"token_count": json.Number("3"),
				"tokens_used": json.Number("7"),
				"author":      "ada",
				"secretary":   "bea",
			},
		},
	}

	runToolArgumentsCases(t, cases)
}

// TestStoreReservesGeneratedKeyPlaceholders pins placeholder reservation: a
// renamed key's generated placeholder is reserved at once, so a second renamed
// sibling can never reuse the name and overwrite the first value. A sibling
// literally named "[redacted-key-2]" keeps its value; the two generated names
// are dense from the first free ordinal and distinct from it and each other.
func TestStoreReservesGeneratedKeyPlaceholders(t *testing.T) {
	const arguments = `{"[redacted-key-2]":"kept-literal","password=a":"first-value","token=b":"second-value"}`
	want := map[string]any{
		"[redacted-key-2]": "kept-literal",
		"[redacted-key-1]": "[redacted]",
		"[redacted-key-3]": "[redacted]",
	}

	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})
	ref, err := store.Save(context.Background(), recordWithToolArguments(arguments))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	persisted := storage.lastRecord().Messages[1].ToolCalls[0].Arguments
	for _, forbidden := range []string{"first-value", "second-value", "password=a", "token=b"} {
		if strings.Contains(persisted, forbidden) {
			t.Fatalf("persisted call arguments leaked %q: %q", forbidden, persisted)
		}
	}
	decoded := decodeToolArguments(t, persisted)
	if len(decoded) != len(want) {
		t.Fatalf("persisted call arguments = %s; want %d distinct keys", persisted, len(want))
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("persisted call arguments = %s; want %v", persisted, want)
	}

	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := restored.Messages[1].ToolCalls[0].Arguments; got != persisted {
		t.Fatalf("restored call arguments = %q; want the persisted %q", got, persisted)
	}
}

// TestStorePreservesInBoundHTMLHeavyArguments pins that JSON
// encoding must not HTML-escape '<', '>' or '&', because that growth pushes a
// valid in-bound argument object past the 64 KiB call-arguments bound and the
// fallback would silently replace the call content with {}. An HTML-heavy
// object just under the bound and an HTML-free ~60 KiB object both persist
// unchanged; only a redacted form that genuinely exceeds the bound falls back.
func TestStorePreservesInBoundHTMLHeavyArguments(t *testing.T) {
	// One raw chunk stays a reversible JSON string body: quote and newline are
	// escaped, while '<', '>' and '&' stay literal so the fixture is exactly the
	// in-bound text a provider sends.
	chunk := `<div class="row">a &amp; b</div>` + "\n"
	escapedChunk := strings.ReplaceAll(strings.ReplaceAll(chunk, `\`, `\\`), `"`, `\"`)
	escapedChunk = strings.ReplaceAll(escapedChunk, "\n", `\n`)
	htmlArguments := `{"content":"` + strings.Repeat(escapedChunk, 1200) + `","path":"a.html"}`
	if len(htmlArguments) < 40<<10 || len(htmlArguments) > session.MaxToolArgumentsBytes {
		t.Fatalf("HTML-heavy fixture is %d bytes; want 40 KiB..%d", len(htmlArguments), session.MaxToolArgumentsBytes)
	}
	// Prove the fixture reproduces the defect: HTML escaping each of '<', '>'
	// and '&' adds five bytes, which would cross the bound.
	htmlGrowth := 5 * (strings.Count(htmlArguments, "<") + strings.Count(htmlArguments, ">") + strings.Count(htmlArguments, "&"))
	if len(htmlArguments)+htmlGrowth <= session.MaxToolArgumentsBytes {
		t.Fatalf("HTML-heavy fixture grows to %d bytes; want more than %d", len(htmlArguments)+htmlGrowth, session.MaxToolArgumentsBytes)
	}

	assertArgumentsSurvive := func(t *testing.T, arguments string) {
		t.Helper()
		storage := newFakeStorage()
		store := newTestStore(t, storage, &fakeClock{now: testNow})
		ref, err := store.Save(context.Background(), recordWithToolArguments(arguments))
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		persisted := storage.lastRecord().Messages[1].ToolCalls[0].Arguments
		if persisted != arguments {
			t.Fatalf("persisted call arguments are %d bytes; want the %d-byte in-bound form unchanged", len(persisted), len(arguments))
		}
		for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
			if strings.Contains(persisted, escape) {
				t.Fatalf("persisted call arguments carry the HTML escape %q", escape)
			}
		}
		if got, want := decodeToolArguments(t, persisted), decodeToolArguments(t, arguments); !reflect.DeepEqual(got, want) {
			t.Fatalf("persisted call arguments = %s; want %v", persisted, want)
		}
		restored, err := store.Restore(context.Background(), ref)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if restored.Messages[1].ToolCalls[0].Arguments != persisted {
			t.Fatalf("restored call arguments = %q; want the persisted %q", restored.Messages[1].ToolCalls[0].Arguments, persisted)
		}
	}

	t.Run("HTML-heavy 40 KiB object", func(t *testing.T) {
		assertArgumentsSurvive(t, htmlArguments)
	})

	t.Run("HTML-free 60 KiB object", func(t *testing.T) {
		plainArguments := `{"content":"` + strings.Repeat("a", 60_000) + `","path":"a.html"}`
		if len(plainArguments) > session.MaxToolArgumentsBytes {
			t.Fatalf("HTML-free fixture is %d bytes; want at most %d", len(plainArguments), session.MaxToolArgumentsBytes)
		}
		assertArgumentsSurvive(t, plainArguments)
	})

	t.Run("redaction growth past the bound still falls back", func(t *testing.T) {
		const prefix = `{"command":"export TOKEN=x","pad":"`
		const suffix = `"}`
		overBound := prefix + strings.Repeat("a", session.MaxToolArgumentsBytes-len(prefix)-len(suffix)-5) + suffix
		storage := newFakeStorage()
		store := newTestStore(t, storage, &fakeClock{now: testNow})
		if _, err := store.Save(context.Background(), recordWithToolArguments(overBound)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if persisted := storage.lastRecord().Messages[1].ToolCalls[0].Arguments; persisted != "{}" {
			t.Fatalf("persisted over-bound call arguments = %d bytes; want the empty object", len(persisted))
		}
	})
}

// TestStoreFallsBackWhenRedactedArgumentsLeaveTheBound pins the safe fallback:
// re-encoding a redacted object can grow its text, and an object whose growth
// would cross the 64 KiB call-arguments bound is persisted as the empty object
// instead of an unrestorable record. A near-bound object that stays inside the
// bound keeps its redacted content.
func TestStoreFallsBackWhenRedactedArgumentsLeaveTheBound(t *testing.T) {
	const prefix = `{"command":"export TOKEN=x","pad":"`
	const suffix = `"}`
	const growth = len("[redacted]") - len("x")

	oversized := prefix + strings.Repeat("a", session.MaxToolArgumentsBytes-len(prefix)-len(suffix)-5) + suffix
	if len(oversized) != session.MaxToolArgumentsBytes-5 {
		t.Fatalf("oversized fixture is %d bytes; want %d", len(oversized), session.MaxToolArgumentsBytes-5)
	}
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})
	ref, err := store.Save(context.Background(), recordWithToolArguments(oversized))
	if err != nil {
		t.Fatalf("Save oversized: %v", err)
	}
	persisted := storage.lastRecord().Messages[1].ToolCalls[0].Arguments
	if persisted != "{}" {
		t.Fatalf("persisted oversized call arguments = %d bytes; want the empty object", len(persisted))
	}
	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore oversized: %v", err)
	}
	if got := restored.Messages[1].ToolCalls[0].Arguments; got != "{}" {
		t.Fatalf("restored oversized call arguments = %q; want {}", got)
	}

	nearBound := prefix + strings.Repeat("a", session.MaxToolArgumentsBytes-len(prefix)-len(suffix)-20) + suffix
	nearStorage := newFakeStorage()
	nearStore := newTestStore(t, nearStorage, &fakeClock{now: testNow})
	nearRef, err := nearStore.Save(context.Background(), recordWithToolArguments(nearBound))
	if err != nil {
		t.Fatalf("Save near bound: %v", err)
	}
	near := nearStorage.lastRecord().Messages[1].ToolCalls[0].Arguments
	if len(near) != len(nearBound)+growth {
		t.Fatalf("persisted near-bound call arguments = %d bytes; want %d", len(near), len(nearBound)+growth)
	}
	if len(near) > session.MaxToolArgumentsBytes {
		t.Fatalf("persisted near-bound call arguments = %d bytes; want at most %d", len(near), session.MaxToolArgumentsBytes)
	}
	decoded := decodeToolArguments(t, near)
	if decoded["command"] != "export TOKEN=[redacted]" {
		t.Fatalf("persisted near-bound command = %v; want the redacted form", decoded["command"])
	}
	if decoded["pad"] != strings.Repeat("a", len(nearBound)-len(prefix)-len(suffix)) {
		t.Fatalf("persisted near-bound padding was not preserved")
	}
	nearRestored, err := nearStore.Restore(context.Background(), nearRef)
	if err != nil {
		t.Fatalf("Restore near bound: %v", err)
	}
	if nearRestored.Messages[1].ToolCalls[0].Arguments != near {
		t.Fatal("restored near-bound call arguments differ from the persisted form")
	}
}
