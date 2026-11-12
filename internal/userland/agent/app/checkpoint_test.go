package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
	"github.com/optimiware/kodama/internal/userland/session"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// rawContentRetention is the explicit trusted opt-in used where a test asserts
// raw transcript preservation. The session store supplies the default seven-day
// window and structural argument redaction.
var rawContentRetention = session.RetentionPolicy{RawContent: true}

type fakeClock struct{ now time.Time }

func (clock fakeClock) Now() time.Time { return clock.now }

func newStorage(t *testing.T) *CheckpointStorage {
	t.Helper()
	return newStorageAt(t, t.TempDir())
}

func newStorageAt(t *testing.T, root string) *CheckpointStorage {
	t.Helper()
	storage, err := NewCheckpointStorage(StorageConfig{Root: root, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatalf("NewCheckpointStorage: %v", err)
	}
	return storage
}

func testRecord(sessionID session.SessionID, revision uint64) session.Record {
	return session.Record{
		Version:     session.RefVersion,
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		SessionID:   sessionID,
		Revision:    revision,
		Messages:    []session.Message{{Role: "user", Content: "hello"}},
		CreatedAt:   testNow,
	}
}

func TestStorageMissingCheckpoint(t *testing.T) {
	storage := newStorage(t)
	ref := checkpointRef(checkpointKey("sess-1"), 1)
	_, err := storage.Read(context.Background(), ref)
	if !errors.Is(err, session.ErrCheckpointMissing) {
		t.Fatalf("Read error = %v; want ErrCheckpointMissing", err)
	}
	store, err := session.NewStore(session.StoreConfig{Storage: storage, Clock: fakeClock{now: testNow}})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_, err = store.Restore(context.Background(), ref)
	var restoreErr session.RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Reason != session.ReasonMissing {
		t.Fatalf("Restore error = %v; want ReasonMissing", err)
	}
}

func TestStorageCompareAndSwapIsImmutable(t *testing.T) {
	storage := newStorage(t)
	first, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 0)
	if err != nil {
		t.Fatalf("first CompareAndSwap: %v", err)
	}
	second, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 1)
	if err != nil {
		t.Fatalf("second CompareAndSwap: %v", err)
	}
	if first == second {
		t.Fatalf("both revisions returned the same reference %q", first)
	}
	record, err := storage.Read(context.Background(), first)
	if err != nil {
		t.Fatalf("Read first: %v", err)
	}
	if record.Revision != 1 {
		t.Fatalf("first revision = %d; want 1", record.Revision)
	}
	record, err = storage.Read(context.Background(), second)
	if err != nil {
		t.Fatalf("Read second: %v", err)
	}
	if record.Revision != 2 {
		t.Fatalf("second revision = %d; want 2", record.Revision)
	}
}

func TestStorageCompareAndSwapRaceHasOneWinner(t *testing.T) {
	storage := newStorage(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 0)
			results <- err
		}()
	}
	close(start)
	var winners, conflicts int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			winners++
		default:
			if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
				conflicts++
				continue
			}
			t.Fatalf("loser error = %v; want VersionConflict", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d; want one of each", winners, conflicts)
	}
}

func TestStoragePermissionsAreOwnerOnly(t *testing.T) {
	root := t.TempDir()
	storage, err := NewCheckpointStorage(StorageConfig{Root: root})
	if err != nil {
		t.Fatalf("NewCheckpointStorage: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("root mode = %o; want 700", info.Mode().Perm())
	}
	ref, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 0)
	if err != nil {
		t.Fatalf("CompareAndSwap: %v", err)
	}
	key, revision, _ := parseCheckpointRef(ref)
	for _, name := range []string{recordName(key, revision), headName(key)} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("Stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o; want 600", name, info.Mode().Perm())
		}
	}
}

func TestStorageRefusesCorruptFile(t *testing.T) {
	storage := newStorage(t)
	key := checkpointKey("sess-1")
	ref := checkpointRef(key, 1)
	if err := os.WriteFile(filepath.Join(storage.Root(), recordName(key, 1)), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := storage.Read(context.Background(), ref); !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("Read error = %v; want ErrCorruptCheckpoint", err)
	}
}

func TestStorageRefusesLoosePermissions(t *testing.T) {
	storage := newStorage(t)
	ref, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 0)
	if err != nil {
		t.Fatalf("CompareAndSwap: %v", err)
	}
	key, revision, _ := parseCheckpointRef(ref)
	path := filepath.Join(storage.Root(), recordName(key, revision))
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if _, err := storage.Read(context.Background(), ref); !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("Read error = %v; want ErrCorruptCheckpoint", err)
	}
}

func TestStorageCrashBeforeRenameLeavesHeadUnchanged(t *testing.T) {
	storage := newStorage(t)
	crashed := errors.New("simulated crash")
	storage.beforeRename = func() error { return crashed }
	if _, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 0); !errors.Is(err, crashed) {
		t.Fatalf("CompareAndSwap error = %v; want the simulated crash", err)
	}
	key := checkpointKey("sess-1")
	head, err := storage.readHead(key)
	if err != nil {
		t.Fatalf("readHead: %v", err)
	}
	if head != 0 {
		t.Fatalf("head = %d; want 0 after a crash before rename", head)
	}
	entries, err := os.ReadDir(storage.Root())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatalf("temporary file %q survived the crash", entry.Name())
		}
	}
	storage.beforeRename = nil
	if _, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 0); err != nil {
		t.Fatalf("retry after a definite failure: %v", err)
	}
}

func TestCheckpointPortRoundTripsEffects(t *testing.T) {
	storage := newStorage(t)
	port, err := NewCheckpointPort(storage, fakeClock{now: testNow}, session.RetentionPolicy{})
	if err != nil {
		t.Fatalf("NewCheckpointPort: %v", err)
	}
	checkpoint := agent.Checkpoint{
		Version:     agent.CheckpointVersion,
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Messages:    []agent.Message{{Role: "user", Content: "do the work"}},
		Completed:   []string{"effect-1"},
		InFlight:    []string{"effect-2"},
		Uncertain:   []string{"effect-3"},
		Usage:       agent.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		CreatedAt:   testNow,
	}
	ref, err := port.Save(context.Background(), checkpoint)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	restored, err := port.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.TaskID != checkpoint.TaskID || restored.ExecutionID != checkpoint.ExecutionID {
		t.Fatalf("restored identity = %+v", restored)
	}
	if len(restored.Completed) != 1 || restored.Completed[0] != "effect-1" {
		t.Fatalf("completed = %v", restored.Completed)
	}
	if len(restored.InFlight) != 1 || restored.InFlight[0] != "effect-2" {
		t.Fatalf("in flight = %v", restored.InFlight)
	}
	if len(restored.Uncertain) != 1 || restored.Uncertain[0] != "effect-3" {
		t.Fatalf("uncertain = %v", restored.Uncertain)
	}
	if restored.Usage != checkpoint.Usage {
		t.Fatalf("usage = %+v; want %+v", restored.Usage, checkpoint.Usage)
	}
}

func TestCheckpointPortRefusesMissingCheckpoint(t *testing.T) {
	storage := newStorage(t)
	port, err := NewCheckpointPort(storage, fakeClock{now: testNow}, session.RetentionPolicy{})
	if err != nil {
		t.Fatalf("NewCheckpointPort: %v", err)
	}
	_, err = port.Restore(context.Background(), "kodama-checkpoint/v1/"+checkpointKey("sess-1")+"-1")
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("Restore error = %v; want NotFound", err)
	}
}

// correlatedPortCheckpoint is a closed conversation with two provider call
// identities and their tool answers.
func correlatedPortCheckpoint(createdAt time.Time) agent.Checkpoint {
	return agent.Checkpoint{
		Version:     agent.CheckpointVersion,
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Messages: []agent.Message{
			{Role: "system", Content: "you are a test"},
			{Role: "user", Content: "do the thing"},
			{Role: "assistant", ToolCalls: []agent.ToolCall{
				{ID: "call-1", Name: "repo_read", Arguments: `{"path":"a","ref":"main"}`},
				{ID: "call-2", Name: "repo_write", Arguments: `{"path":"b","ref":"main"}`},
			}},
			{Role: "tool", Content: "first result", ToolCallID: "call-1"},
			{Role: "tool", Content: "second result", ToolCallID: "call-2"},
		},
		Completed: []string{"effect-1"},
		InFlight:  []string{"effect-2"},
		Uncertain: []string{"effect-3"},
		Usage:     agent.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		CreatedAt: createdAt,
	}
}

// requirePortToolCorrelation asserts both directions of the correlation
// mapping: the assistant call list is complete and the tool messages answer it.
func requirePortToolCorrelation(t *testing.T, restored agent.Checkpoint) {
	t.Helper()
	if len(restored.Messages) != 5 {
		t.Fatalf("restored messages = %d; want 5", len(restored.Messages))
	}
	calls := restored.Messages[2].ToolCalls
	want := []agent.ToolCall{
		{ID: "call-1", Name: "repo_read", Arguments: `{"path":"a","ref":"main"}`},
		{ID: "call-2", Name: "repo_write", Arguments: `{"path":"b","ref":"main"}`},
	}
	if len(calls) != len(want) {
		t.Fatalf("restored tool calls = %+v; want %+v", calls, want)
	}
	for index := range want {
		if calls[index] != want[index] {
			t.Fatalf("restored tool call %d = %+v; want %+v", index, calls[index], want[index])
		}
	}
	if restored.Messages[3].ToolCallID != "call-1" || restored.Messages[4].ToolCallID != "call-2" {
		t.Fatalf("restored tool call ids = %q, %q", restored.Messages[3].ToolCallID, restored.Messages[4].ToolCallID)
	}
}

func TestCheckpointPortRoundTripsToolCorrelationAfterReopen(t *testing.T) {
	root := t.TempDir()
	port, err := NewCheckpointPort(newStorageAt(t, root), fakeClock{now: testNow}, rawContentRetention)
	if err != nil {
		t.Fatalf("NewCheckpointPort: %v", err)
	}
	checkpoint := correlatedPortCheckpoint(testNow)
	ref, err := port.Save(context.Background(), checkpoint)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A new process opens the same owner-only directory.
	reopenedStorage := newStorageAt(t, root)
	reopened, err := NewCheckpointPort(reopenedStorage, fakeClock{now: testNow.Add(time.Hour)}, rawContentRetention)
	if err != nil {
		t.Fatalf("NewCheckpointPort after reopen: %v", err)
	}
	restored, err := reopened.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	requirePortToolCorrelation(t, restored)
	if len(restored.Completed) != 1 || restored.Completed[0] != "effect-1" ||
		len(restored.InFlight) != 1 || restored.InFlight[0] != "effect-2" ||
		len(restored.Uncertain) != 1 || restored.Uncertain[0] != "effect-3" {
		t.Fatalf("restored effects = %+v", restored)
	}
	if restored.Usage != checkpoint.Usage {
		t.Fatalf("restored usage = %+v; want %+v", restored.Usage, checkpoint.Usage)
	}

	// The restore advanced the reopened session revision, so the next save
	// continues the same stream instead of colliding with the stored head.
	next := correlatedPortCheckpoint(testNow.Add(time.Hour))
	next.Completed = []string{"effect-1", "effect-4"}
	second, err := reopened.Save(context.Background(), next)
	if err != nil {
		t.Fatalf("Save after reopen: %v", err)
	}
	if second == ref {
		t.Fatalf("a save after reopen reused reference %q", ref)
	}
	record, err := reopenedStorage.Read(context.Background(), session.Ref(second))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if record.Revision != 2 {
		t.Fatalf("reopened revision = %d; want 2", record.Revision)
	}
}

func TestCheckpointPortRestoresIntoNewExecutionIdentity(t *testing.T) {
	storage := newStorage(t)
	port, err := NewCheckpointPort(storage, fakeClock{now: testNow}, rawContentRetention)
	if err != nil {
		t.Fatalf("NewCheckpointPort: %v", err)
	}
	first, err := port.Save(context.Background(), correlatedPortCheckpoint(testNow))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	restored, err := port.Restore(context.Background(), first)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	next := restored
	next.ExecutionID = "execution-2"
	next.CreatedAt = testNow.Add(time.Minute)
	second, err := port.Save(context.Background(), next)
	if err != nil {
		t.Fatalf("Save under the new execution: %v", err)
	}
	record, err := storage.Read(context.Background(), session.Ref(second))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if record.TaskID != "task-1" || record.ExecutionID != "execution-2" {
		t.Fatalf("new checkpoint identity = task %q execution %q; want the new execution", record.TaskID, record.ExecutionID)
	}
	if want := checkpointSessionID("task-1", "execution-2"); record.SessionID != want {
		t.Fatalf("new checkpoint session = %q; want %q", record.SessionID, want)
	}
	if len(record.Messages) != 5 || len(record.Messages[2].ToolCalls) != 2 {
		t.Fatalf("restored context was not carried into the new execution: %+v", record.Messages)
	}
}

func TestCheckpointPortRetentionPolicyAndExpiry(t *testing.T) {
	t.Run("raw content is off by default", func(t *testing.T) {
		storage := newStorage(t)
		port, err := NewCheckpointPort(storage, fakeClock{now: testNow}, session.RetentionPolicy{})
		if err != nil {
			t.Fatalf("NewCheckpointPort: %v", err)
		}
		checkpoint := correlatedPortCheckpoint(testNow)
		checkpoint.Messages[2].ToolCalls[0].Arguments = `{"path":"a","api_key":"sk-live-secret"}`
		ref, err := port.Save(context.Background(), checkpoint)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		record, err := storage.Read(context.Background(), session.Ref(ref))
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		requireWithheldStoredTranscript(t, record.Messages)
		restored, err := port.Restore(context.Background(), ref)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		requireWithheldTranscript(t, restored.Messages)
		if len(restored.Completed) != 1 || restored.Completed[0] != "effect-1" {
			t.Fatalf("completed = %v", restored.Completed)
		}
		if len(restored.InFlight) != 1 || len(restored.Uncertain) != 1 {
			t.Fatalf("in flight / uncertain were dropped: %+v", restored)
		}
		if restored.Usage != checkpoint.Usage {
			t.Fatalf("usage = %+v; want %+v", restored.Usage, checkpoint.Usage)
		}
	})

	t.Run("opt in retains for seven days with structural redaction", func(t *testing.T) {
		storage := newStorage(t)
		port, err := NewCheckpointPort(storage, fakeClock{now: testNow}, rawContentRetention)
		if err != nil {
			t.Fatalf("NewCheckpointPort: %v", err)
		}
		checkpoint := correlatedPortCheckpoint(testNow)
		checkpoint.Messages[2].ToolCalls[0].Arguments = `{"path":"a","api_key":"sk-live-secret"}`
		checkpoint.Messages[3].Content = "Bearer abc123"
		ref, err := port.Save(context.Background(), checkpoint)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		record, err := storage.Read(context.Background(), session.Ref(ref))
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got := record.Messages[3].Content; got != "Bearer [redacted]" {
			t.Fatalf("retained tool content = %q; want the redacted form", got)
		}
		calls := record.Messages[2].ToolCalls
		if len(calls) != 2 {
			t.Fatalf("retained tool calls = %d; want 2", len(calls))
		}
		var arguments map[string]any
		if err := json.Unmarshal([]byte(calls[0].Arguments), &arguments); err != nil {
			t.Fatalf("retained arguments are not a JSON object: %v", err)
		}
		if arguments["api_key"] != "[redacted]" || arguments["path"] != "a" {
			t.Fatalf("structural argument redaction = %v", arguments)
		}

		// Six days after creation the seven-day window still retains the
		// transcript; eight days after, the store withholds the raw content and
		// the port refuses the incomplete restoration. The durable record keeps
		// its effect bookkeeping and usage, so a refusal never loses that state.
		retainedPort, err := NewCheckpointPort(storage, fakeClock{now: testNow.Add(6 * 24 * time.Hour)}, rawContentRetention)
		if err != nil {
			t.Fatalf("NewCheckpointPort at six days: %v", err)
		}
		retained, err := retainedPort.Restore(context.Background(), ref)
		if err != nil {
			t.Fatalf("Restore at six days: %v", err)
		}
		if len(retained.Messages) != 5 || len(retained.Messages[2].ToolCalls) != 2 || retained.Messages[3].ToolCallID != "call-1" {
			t.Fatalf("the seven-day window withheld retained correlation: %+v", retained.Messages)
		}

		expiredPort, err := NewCheckpointPort(storage, fakeClock{now: testNow.Add(8 * 24 * time.Hour)}, rawContentRetention)
		if err != nil {
			t.Fatalf("NewCheckpointPort at eight days: %v", err)
		}
		expired, err := expiredPort.Restore(context.Background(), ref)
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("Restore at eight days = %+v, %v; want NotFound for an expired transcript", expired, err)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "expired") {
			t.Fatalf("Restore at eight days error = %v; want it to name the expired transcript", err)
		}
		if expired.TaskID != "" || len(expired.Messages) != 0 {
			t.Fatalf("a refused expired restore fabricated a checkpoint: %+v", expired)
		}

		// The expiry is a restore-time policy, not data loss: the stored record
		// still carries the effect bookkeeping and usage.
		stored, err := storage.Read(context.Background(), session.Ref(ref))
		if err != nil {
			t.Fatalf("Read stored record at eight days: %v", err)
		}
		if len(stored.Usage) == 0 {
			t.Fatal("expiry dropped the stored usage bookkeeping")
		}
	})
}

// requireWithheldTranscript asserts that no raw conversation content survived.
func requireWithheldTranscript(t *testing.T, messages []agent.Message) {
	t.Helper()
	if len(messages) == 0 {
		t.Fatal("withholding removed every message instead of its raw content")
	}
	for index, message := range messages {
		if message.Content != "" || message.ToolCalls != nil || message.ToolCallID != "" {
			t.Fatalf("message %d retained raw transcript content: %+v", index, message)
		}
	}
}

// requireWithheldStoredTranscript asserts the persisted record carries no raw
// conversation content.
func requireWithheldStoredTranscript(t *testing.T, messages []session.Message) {
	t.Helper()
	if len(messages) == 0 {
		t.Fatal("withholding removed every message instead of its raw content")
	}
	for index, message := range messages {
		if message.Content != "" || message.ToolCalls != nil || message.ToolCallID != "" {
			t.Fatalf("stored message %d retained raw transcript content: %+v", index, message)
		}
	}
}

// fixedStorage returns one prepared record without applying store validation, so
// the store's own version and corruption mapping is exercised.
type fixedStorage struct {
	record session.Record
}

func (storage fixedStorage) Read(context.Context, session.Ref) (session.Record, error) {
	return storage.record, nil
}

func (storage fixedStorage) CompareAndSwap(context.Context, session.Record, uint64) (session.Ref, error) {
	return "", errors.New("fixed storage never writes")
}

func TestCheckpointPortRefusesCorruptAndVersionMismatchedContext(t *testing.T) {
	for _, test := range []struct {
		name  string
		build func() session.Record
	}{
		{"corrupt", func() session.Record {
			record := testRecord("sess-1", 0)
			record.Messages = nil
			return record
		}},
		{"version mismatch", func() session.Record {
			record := testRecord("sess-1", 0)
			record.Version = "v2"
			return record
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			port, err := NewCheckpointPort(fixedStorage{record: test.build()}, fakeClock{now: testNow}, session.RetentionPolicy{})
			if err != nil {
				t.Fatalf("NewCheckpointPort: %v", err)
			}
			restored, err := port.Restore(context.Background(), "kodama-checkpoint/v1/record-1")
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("Restore error = %v; want InvalidInput", err)
			}
			if restored.TaskID != "" || len(restored.Messages) != 0 {
				t.Fatalf("a failed restore fabricated a checkpoint: %+v", restored)
			}
		})
	}
}

func TestCheckpointPortConcurrentSavesHaveOneWinner(t *testing.T) {
	storage := newStorage(t)
	port, err := NewCheckpointPort(storage, fakeClock{now: testNow}, session.RetentionPolicy{})
	if err != nil {
		t.Fatalf("NewCheckpointPort: %v", err)
	}
	checkpoint := agent.Checkpoint{
		Version:     agent.CheckpointVersion,
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Messages:    []agent.Message{{Role: "user", Content: "work"}},
		CreatedAt:   testNow,
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Go(func() {
			_, err := port.Save(context.Background(), checkpoint)
			results <- err
		})
	}
	wait.Wait()
	close(results)
	var winners, conflicts int
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
			conflicts++
			continue
		}
		t.Fatalf("loser error = %v; want VersionConflict", err)
	}
	if winners < 1 {
		t.Fatal("no save committed")
	}
	if winners+conflicts != 2 {
		t.Fatalf("winners=%d conflicts=%d", winners, conflicts)
	}
}

func TestStorageRecordIsNotFabricated(t *testing.T) {
	storage := newStorage(t)
	ref, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 0)
	if err != nil {
		t.Fatalf("CompareAndSwap: %v", err)
	}
	// A reference that parses as a checkpoint but names an unknown key must be
	// missing, never an empty success.
	unknown := session.Ref("kodama-checkpoint/v1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-1")
	if unknown == ref {
		t.Fatal("test reference collided with the stored reference")
	}
	if _, err := storage.Read(context.Background(), unknown); !errors.Is(err, session.ErrCheckpointMissing) {
		t.Fatalf("Read error = %v; want ErrCheckpointMissing", err)
	}
}

func TestStorageRefusesUnsupportedRefShape(t *testing.T) {
	storage := newStorage(t)
	if _, err := storage.Read(context.Background(), session.Ref("kodama-checkpoint/v1/not-ours")); !errors.Is(err, session.ErrCheckpointMissing) {
		t.Fatalf("Read error = %v; want ErrCheckpointMissing", err)
	}
}

func TestCheckpointStorageRecordBytesAreValidJSON(t *testing.T) {
	storage := newStorage(t)
	ref, err := storage.CompareAndSwap(context.Background(), testRecord("sess-1", 0), 0)
	if err != nil {
		t.Fatalf("CompareAndSwap: %v", err)
	}
	key, revision, _ := parseCheckpointRef(ref)
	data, err := os.ReadFile(filepath.Join(storage.Root(), recordName(key, revision)))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var record session.Record
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("stored checkpoint is not JSON: %v", err)
	}
	if record.Revision != revision {
		t.Fatalf("stored revision = %d; want %d", record.Revision, revision)
	}
}

// TestCheckpointPortRefusesExpiredTranscript proves a checkpoint whose opted-in
// raw transcript has outlived the retention window fails the restoring action.
// The store withholds expired content, so continuing would silently start from
// an incomplete conversation; S06 §4 requires the missing context to fail.
// A checkpoint still inside the window restores, and a raw-off checkpoint is
// never treated as expired: its content was withheld at write time by design.
func TestCheckpointPortRefusesExpiredTranscript(t *testing.T) {
	retained := session.RetentionPolicy{RawContent: true, Retention: session.DefaultRawContentRetention}

	t.Run("expired transcript fails", func(t *testing.T) {
		storage := newStorage(t)
		writer, err := NewCheckpointPort(storage, fakeClock{now: testNow}, retained)
		if err != nil {
			t.Fatalf("NewCheckpointPort: %v", err)
		}
		checkpoint := correlatedPortCheckpoint(testNow.Add(-8 * 24 * time.Hour))
		ref, err := writer.Save(context.Background(), checkpoint)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		_, err = writer.Restore(context.Background(), ref)
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("Restore error = %v; want NotFound for an expired transcript", err)
		}
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "expired") {
			t.Fatalf("Restore error = %v; want it to name the expired transcript", err)
		}
	})

	t.Run("inside the window restores", func(t *testing.T) {
		storage := newStorage(t)
		writer, err := NewCheckpointPort(storage, fakeClock{now: testNow}, retained)
		if err != nil {
			t.Fatalf("NewCheckpointPort: %v", err)
		}
		checkpoint := correlatedPortCheckpoint(testNow.Add(-6 * 24 * time.Hour))
		ref, err := writer.Save(context.Background(), checkpoint)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		restored, err := writer.Restore(context.Background(), ref)
		if err != nil {
			t.Fatalf("Restore inside the retention window: %v", err)
		}
		requirePortToolCorrelation(t, restored)
	})

	t.Run("raw content off is not expiry", func(t *testing.T) {
		storage := newStorage(t)
		writer, err := NewCheckpointPort(storage, fakeClock{now: testNow}, session.RetentionPolicy{})
		if err != nil {
			t.Fatalf("NewCheckpointPort: %v", err)
		}
		checkpoint := correlatedPortCheckpoint(testNow.Add(-30 * 24 * time.Hour))
		ref, err := writer.Save(context.Background(), checkpoint)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		restored, err := writer.Restore(context.Background(), ref)
		if err != nil {
			t.Fatalf("Restore of a raw-off checkpoint: %v", err)
		}
		requireWithheldTranscript(t, restored.Messages)
	})
}
