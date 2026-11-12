// Durable-session implementation API.
//
// This file is the test-side declaration of the behavior of
// internal/userland/session. The base declarations in types.go (RefPrefix,
// RefVersion, MaxRefBytes, Ref, ParseRef, Ref.Version, Reason, Reason.Valid,
// RestoreError, Message, Record and its Validate rules, Store) are not
// redefined here.
//
// Package declarations:
//
//	type SessionID string
//	func ParseSessionID(string) (SessionID, error)
//	// 1..128 ASCII bytes [A-Za-z0-9_-], never "." or "..".
//
//	// Record carries these fields:
//	//   SessionID   SessionID       `json:"session_id,omitempty"`
//	//   Revision    uint64          `json:"revision,omitempty"`
//	//   ContextRefs []Ref           `json:"context_refs,omitempty"`
//	//   Retention   RetentionPolicy `json:"retention,omitempty"`
//	//   Usage       []Usage         `json:"usage,omitempty"`
//	//   Outcome     Outcome         `json:"outcome,omitempty"`
//	// Revision is the numeric session-stream revision, never the format
//	// version. Record.Validate refuses an invalid non-empty SessionID, an
//	// unparseable ContextRef, a negative retention, a negative usage count,
//	// a usage entry without PrincipalID or Model, an outcome status longer
//	// than 64 bytes and an outcome summary longer than 4096 bytes. An empty
//	// SessionID stays valid; Save requires it.
//
//	type Usage struct {
//	    PrincipalID string `json:"principal_id"`
//	    Model       string `json:"model"`
//	    Prompt      int    `json:"prompt"`
//	    Completion  int    `json:"completion"`
//	}
//	type Outcome struct {
//	    Status  string `json:"status"`
//	    Summary string `json:"summary"`
//	}
//
//	type RetentionPolicy struct {
//	    RawContent bool          `json:"raw_content,omitempty"`
//	    Retention  time.Duration `json:"retention,omitempty"`
//	}
//	const DefaultRawContentRetention = 7 * 24 * time.Hour
//	func (RetentionPolicy) Validate() error
//	func (RetentionPolicy) EffectiveRetention() time.Duration
//	func (RetentionPolicy) ExpiresAt(createdAt time.Time) (time.Time, bool)
//	func (RetentionPolicy) RetainRaw(now, createdAt time.Time, referenced bool) bool
//	// Validate refuses a negative Retention. EffectiveRetention is 0 when
//	// RawContent is false and 7 days when RawContent is true and Retention is 0.
//	// ExpiresAt reports false when RawContent is false and otherwise
//	// createdAt.Add(EffectiveRetention()). RetainRaw is false when RawContent is
//	// false, true when referenced (an active checkpoint reference protects
//	// retained raw content past expiry), and otherwise
//	// now.Before(createdAt.Add(EffectiveRetention())).
//
//	type ContextLimits struct {
//	    MaxMessages int
//	    MaxBytes    int
//	}
//	func DefaultContextLimits() ContextLimits // {MaxMessages: 128, MaxBytes: 256 << 10}
//	type ContextWindow struct {
//	    Messages []Message
//	    Refs     []Ref
//	    Trimmed  int
//	}
//	func (Record) Window(ContextLimits) (ContextWindow, error)
//	// Window keeps the newest messages, in original order, that fit both
//	// limits. One message costs len(Role)+len(Content) bytes. It drops the
//	// oldest messages first and reports the dropped count in Trimmed. Refs are
//	// copied verbatim: they are never trimmed and their content is never
//	// inlined. Non-positive limits refuse, and so does a newest message that
//	// alone exceeds MaxBytes.
//
//	var ErrCheckpointMissing = errors.New("checkpoint missing")
//	type Storage interface {
//	    Read(ctx context.Context, ref Ref) (Record, error)
//	    CompareAndSwap(ctx context.Context, record Record, expectedRevision uint64) (Ref, error)
//	}
//	// Read returns an error wrapping ErrCheckpointMissing for an absent ref.
//	// CompareAndSwap commits record as revision expectedRevision+1 of the
//	// stream identified by record.SessionID only when the stored head revision
//	// equals expectedRevision, and returns that revision's new immutable
//	// checkpoint reference. A stale expectedRevision returns a
//	// fault.VersionConflict error and no reference. Any other error means no
//	// durable proof of the commit.
//
//	type Clock interface {
//	    Now() time.Time
//	}
//	type StoreConfig struct {
//	    Storage Storage
//	    Clock   Clock
//	    Secrets []string
//	}
//	func NewStore(StoreConfig) (Store, error)
//
// DurableStore.Save:
//  1. refuses a nil Storage or Clock at construction;
//  2. validates the record, requires a non-empty SessionID and refuses
//     Revision == math.MaxUint64 because the next revision would overflow;
//  3. replaces every supplied secret value and every credential-shaped value
//     (`Bearer <token>`, a PEM private key block, an AWS access key id and a
//     password/secret/token/API-key assignment, all case-insensitively,
//     including an identifier-prefixed label such as `GITHUB_TOKEN` or
//     `AWS_SECRET_ACCESS_KEY` and quoted JSON/YAML values) with `[redacted]`
//     in message content and the outcome summary before the storage boundary
//     sees the record;
//  4. when Retention.RawContent is false, persists every message role with an
//     empty Content and an empty outcome summary (raw content is off by
//     default);
//  5. calls Storage.CompareAndSwap exactly once with expectedRevision =
//     record.Revision and returns its reference. A reference that ParseRef
//     refuses, or any commit error, returns "" and the error, so a failed or
//     uncertain commit never yields durable proof.
//
// DurableStore.Restore:
//  1. refuses an unparseable reference; a checkpoint-family reference whose
//     version is not RefVersion becomes a typed
//     RestoreError{ReasonVersionMismatch} before any storage read;
//  2. Storage.Read(ref): an error wrapping ErrCheckpointMissing becomes
//     RestoreError{ReasonMissing}; any other read error is returned unchanged;
//  3. a record whose Version is not RefVersion becomes
//     RestoreError{ReasonVersionMismatch} before validation;
//  4. a record failing Record.Validate becomes RestoreError{ReasonCorrupt};
//  5. every ContextRef is Read; any failure becomes
//     RestoreError{ReasonContextMissing};
//  6. retained raw content (message contents and the outcome summary) is
//     withheld when the record's RetentionPolicy no longer retains it. Usage,
//     Outcome.Status, ContextRefs, SessionID and Revision always survive.
//  7. Restore never invents process, scratch, worktree or approval state.
package session_test

import (
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/session"
)

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// mustRef parses a checkpoint reference the tests know is valid.
func mustRef(t *testing.T, value string) session.Ref {
	t.Helper()
	ref, err := session.ParseRef(value)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", value, err)
	}
	return ref
}

// baseRecord is the minimal valid checkpoint record.
func baseRecord() session.Record {
	return session.Record{
		Version:     session.RefVersion,
		SessionID:   session.SessionID("sess-1"),
		TaskID:      "tsk-1",
		ExecutionID: "exec-1",
		Messages: []session.Message{
			{Role: "user", Content: "please continue"},
			{Role: "assistant", Content: "continuing"},
		},
		CreatedAt: testNow,
	}
}

// TestSessionIdentity pins the bounded session identity.
func TestSessionIdentity(t *testing.T) {
	for _, value := range []string{"sess-1", "S1_b-2"} {
		if _, err := session.ParseSessionID(value); err != nil {
			t.Errorf("ParseSessionID(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", ".", "..", "sess/1", "sess 1", "sess.1", strings.Repeat("a", 129)} {
		if parsed, err := session.ParseSessionID(value); err == nil {
			t.Errorf("ParseSessionID(%q) = %q; want refusal", value, parsed)
		}
	}
}

// TestRecordValidateExtensions pins the added record fields: session identity,
// numeric revision, durable context references, retention, usage and outcome.
func TestRecordValidateExtensions(t *testing.T) {
	valid := baseRecord()
	valid.Revision = 3
	valid.ContextRefs = []session.Ref{mustRef(t, "kodama-checkpoint/v1/ctx-0001")}
	valid.Retention = session.RetentionPolicy{RawContent: true}
	valid.Usage = []session.Usage{{PrincipalID: "prn-1", Model: "model-a", Prompt: 10, Completion: 5}}
	valid.Outcome = session.Outcome{Status: "completed", Summary: "done"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid record refused: %v", err)
	}

	// A record without a session identity must stay valid while Save is the
	// boundary that requires one.
	compat := baseRecord()
	compat.SessionID = ""
	if err := compat.Validate(); err != nil {
		t.Fatalf("record without SessionID refused: %v", err)
	}

	refusals := []struct {
		name   string
		mutate func(*session.Record)
	}{
		{"invalid session id", func(record *session.Record) { record.SessionID = "sess/1" }},
		{"dot session id", func(record *session.Record) { record.SessionID = ".." }},
		{"unparseable context ref", func(record *session.Record) { record.ContextRefs = []session.Ref{session.Ref("not-a-checkpoint")} }},
		{"negative retention", func(record *session.Record) {
			record.Retention = session.RetentionPolicy{RawContent: true, Retention: -time.Second}
		}},
		{"negative usage", func(record *session.Record) {
			record.Usage = []session.Usage{{PrincipalID: "prn-1", Model: "model-a", Prompt: -1}}
		}},
		{"usage without principal", func(record *session.Record) {
			record.Usage = []session.Usage{{Model: "model-a"}}
		}},
		{"usage without model", func(record *session.Record) {
			record.Usage = []session.Usage{{PrincipalID: "prn-1"}}
		}},
		{"outcome status too long", func(record *session.Record) {
			record.Outcome = session.Outcome{Status: strings.Repeat("x", 65)}
		}},
		{"outcome summary too long", func(record *session.Record) {
			record.Outcome = session.Outcome{Summary: strings.Repeat("y", 4097)}
		}},
	}
	for _, refusal := range refusals {
		record := valid
		refusal.mutate(&record)
		if err := record.Validate(); err == nil {
			t.Errorf("Record.Validate accepted the %s case", refusal.name)
		}
	}
}

// TestRetentionPolicy pins raw-content retention: off by default, a seven-day
// authorized default, exact expiry and active-reference protection.
func TestRetentionPolicy(t *testing.T) {
	if session.DefaultRawContentRetention != 7*24*time.Hour {
		t.Fatalf("DefaultRawContentRetention = %v; want 168h", session.DefaultRawContentRetention)
	}
	created := testNow

	var off session.RetentionPolicy
	if err := off.Validate(); err != nil {
		t.Fatalf("zero retention policy refused: %v", err)
	}
	if off.EffectiveRetention() != 0 {
		t.Fatalf("off EffectiveRetention = %v; want 0", off.EffectiveRetention())
	}
	if _, ok := off.ExpiresAt(created); ok {
		t.Fatal("off ExpiresAt reported a deadline")
	}
	if off.RetainRaw(created, created, false) || off.RetainRaw(created, created, true) {
		t.Fatal("off RetainRaw retained raw content")
	}

	on := session.RetentionPolicy{RawContent: true}
	if err := on.Validate(); err != nil {
		t.Fatalf("opt-in policy refused: %v", err)
	}
	if on.EffectiveRetention() != session.DefaultRawContentRetention {
		t.Fatalf("opt-in EffectiveRetention = %v; want %v", on.EffectiveRetention(), session.DefaultRawContentRetention)
	}
	deadline, ok := on.ExpiresAt(created)
	if !ok || !deadline.Equal(created.Add(session.DefaultRawContentRetention)) {
		t.Fatalf("opt-in ExpiresAt = %v,%v; want %v,true", deadline, ok, created.Add(session.DefaultRawContentRetention))
	}
	if !on.RetainRaw(created.Add(session.DefaultRawContentRetention-time.Nanosecond), created, false) {
		t.Fatal("raw content was withheld before its expiry")
	}
	if on.RetainRaw(created.Add(session.DefaultRawContentRetention), created, false) {
		t.Fatal("raw content was retained at its expiry")
	}
	if !on.RetainRaw(created.Add(30*24*time.Hour), created, true) {
		t.Fatal("an active reference did not protect raw content past expiry")
	}
	if on.RetainRaw(created.Add(30*24*time.Hour), created, false) {
		t.Fatal("an unreferenced checkpoint outlived its retention")
	}

	explicit := session.RetentionPolicy{RawContent: true, Retention: 48 * time.Hour}
	if explicit.EffectiveRetention() != 48*time.Hour {
		t.Fatalf("explicit EffectiveRetention = %v; want 48h", explicit.EffectiveRetention())
	}
	if !explicit.RetainRaw(created.Add(47*time.Hour), created, false) {
		t.Fatal("explicit retention expired early")
	}
	if explicit.RetainRaw(created.Add(48*time.Hour), created, false) {
		t.Fatal("explicit retention outlived its deadline")
	}

	invalid := session.RetentionPolicy{Retention: -time.Hour}
	if err := invalid.Validate(); err == nil {
		t.Fatal("negative retention accepted")
	}
}
