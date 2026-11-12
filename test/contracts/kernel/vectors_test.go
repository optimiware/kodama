package kernelcontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

func readVector(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read vector %s: %v", name, err)
	}
	return data
}

func assertJSONEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var wantValue, gotValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode actual JSON: %v", err)
	}
	if !reflect.DeepEqual(wantValue, gotValue) {
		t.Fatalf("JSON round-trip mismatch:\nwant %s\ngot  %s", want, got)
	}
}

func TestObservationVectors(t *testing.T) {
	raw := readVector(t, "observations.json")
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("decode observation entries: %v", err)
	}
	if len(entries) != 6 {
		t.Fatalf("observation vectors = %d, want 6", len(entries))
	}

	observations := make([]kernel.Observation, len(entries))
	for index, entry := range entries {
		if err := json.Unmarshal(entry, &observations[index]); err != nil {
			t.Fatalf("decode observation %d: %v", index, err)
		}
	}

	unknown := observations[0]
	if unknown.Kind != kernel.ObservationUnknown || unknown.ProofOfNonexecution() || unknown.StopConfirmed() {
		t.Fatalf("unknown vector = %+v", unknown)
	}
	absent := observations[1]
	if absent.Kind != kernel.ObservationAbsent || absent.ProofOfNonexecution() || absent.StopConfirmed() {
		t.Fatalf("absent vector = %+v", absent)
	}
	neverStarted := observations[2]
	if neverStarted.Kind != kernel.ObservationNeverStarted || !neverStarted.ProofOfNonexecution() || neverStarted.StopConfirmed() {
		t.Fatalf("never-started vector = %+v", neverStarted)
	}
	running := observations[3]
	if running.Kind != kernel.ObservationRunning || running.ProofOfNonexecution() || running.StopConfirmed() {
		t.Fatalf("running vector = %+v", running)
	}
	exited := observations[4]
	if code, ok := exited.ExitStatus(); !ok || code != 0 {
		t.Fatalf("exited vector status = (%d, %v)", code, ok)
	}
	if !exited.StopConfirmed() {
		t.Fatal("exited vector with confirmed stop reported unconfirmed")
	}
	pendingStop := observations[5]
	if code, ok := pendingStop.ExitStatus(); !ok || code != 137 {
		t.Fatalf("exited-with-pending-stop status = (%d, %v)", code, ok)
	}
	if pendingStop.StopConfirmed() {
		t.Fatal("exited-with-pending-stop vector collapsed the stop barrier")
	}

	for index, observation := range observations {
		encoded, err := json.Marshal(observation)
		if err != nil {
			t.Fatalf("encode observation %d: %v", index, err)
		}
		assertJSONEqual(t, entries[index], encoded)
	}
}

func TestOutputVectors(t *testing.T) {
	raw := readVector(t, "outputs.json")
	var outputs []kernel.Outputs
	if err := json.Unmarshal(raw, &outputs); err != nil {
		t.Fatalf("decode outputs: %v", err)
	}
	if len(outputs) != 4 {
		t.Fatalf("output vectors = %d, want 4", len(outputs))
	}
	if outputs[0] != nil {
		t.Fatal("null output vector decoded as non-nil map")
	}
	if outputs[1] == nil || len(outputs[1]) != 0 {
		t.Fatalf("empty-object output vector = %#v, want non-nil empty map", outputs[1])
	}
	if err := outputs[0].Validate(nil); err != nil {
		t.Fatalf("null output Validate() error = %v", err)
	}
	if err := outputs[1].Validate(nil); err != nil {
		t.Fatalf("empty output Validate() error = %v", err)
	}
	if err := outputs[2].Validate([]id.OutputKey{"candidate"}); err != nil {
		t.Fatalf("single output Validate() error = %v", err)
	}
	declared := []id.OutputKey{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7"}
	if err := outputs[3].Validate(declared); err != nil {
		t.Fatalf("eight-value output Validate() error = %v", err)
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("decode output entries: %v", err)
	}
	for index, output := range outputs {
		encoded, err := json.Marshal(output)
		if err != nil {
			t.Fatalf("encode output %d: %v", index, err)
		}
		assertJSONEqual(t, entries[index], encoded)
	}
}

func TestSubjectVector(t *testing.T) {
	raw := readVector(t, "subject.json")
	var subject definition.Subject
	if err := json.Unmarshal(raw, &subject); err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	fields := subject.Fields()
	if len(fields) != 3 {
		t.Fatalf("subject fields = %d, want 3", len(fields))
	}
	if fields["operation"].Kind != definition.SubjectLiteral || fields["operation"].Literal != "git.ref.update" {
		t.Fatalf("operation subject value = %+v", fields["operation"])
	}
	resource := fields["resource"]
	if resource.Kind != definition.SubjectReference || !resource.Reference.Input || resource.Reference.Key != "repository" {
		t.Fatalf("resource subject value = %+v", resource)
	}
	candidate := fields["candidate"]
	if candidate.Kind != definition.SubjectReference || candidate.Reference.Action != "implement" || candidate.Reference.Key != "candidate" {
		t.Fatalf("candidate subject value = %+v", candidate)
	}

	encoded, err := json.Marshal(subject)
	if err != nil {
		t.Fatalf("encode subject: %v", err)
	}
	assertJSONEqual(t, raw, encoded)
}

func TestCommitVectorCarriesOneAtomicTransition(t *testing.T) {
	raw := readVector(t, "commit.json")
	var commit flow.Commit
	if err := json.Unmarshal(raw, &commit); err != nil {
		t.Fatalf("decode commit: %v", err)
	}

	if commit.ExecutionID != "exec-0001" || commit.ExpectedExecutionVersion != 7 {
		t.Fatalf("commit execution identity/version = %+v", commit)
	}
	if commit.NextExecutionState != kernel.ExecutionRunning {
		t.Fatalf("commit next execution state = %q", commit.NextExecutionState)
	}
	if commit.ActionID != "act-0002" || commit.ExpectedActionVersion != 1 {
		t.Fatalf("commit action identity/version = %+v", commit)
	}
	if commit.NextActionState != kernel.ActionRunning {
		t.Fatalf("commit next action state = %q", commit.NextActionState)
	}
	wantCancellations := []flow.ActionUpdate{
		{ActionID: "act-0003", ExpectedVersion: 1, NextState: kernel.ActionCancelled},
		{ActionID: "act-0004", ExpectedVersion: 1, NextState: kernel.ActionCancelled},
	}
	if len(commit.PendingCancellations) != len(wantCancellations) {
		t.Fatalf("commit pending cancellations = %+v, want %d updates", commit.PendingCancellations, len(wantCancellations))
	}
	for index, want := range wantCancellations {
		if got := commit.PendingCancellations[index]; got != want {
			t.Fatalf("commit pending cancellation %d = %+v, want %+v", index, got, want)
		}
	}
	if commit.NextCurrentAction == nil || *commit.NextCurrentAction != -1 {
		t.Fatalf("commit next current action = %v, want -1", commit.NextCurrentAction)
	}
	if commit.Output.Action != flow.OutputStage || commit.Output.Values["candidate"] != "abc123" {
		t.Fatalf("commit output = %+v", commit.Output)
	}
	if !commit.InvalidateCredential {
		t.Fatal("commit credential-invalidation intent missing")
	}
	if commit.Stop == nil || commit.Stop.WorkloadID != "wl-0001" || commit.Stop.WorkspaceID != "ws-0001" {
		t.Fatalf("commit stop intent = %+v", commit.Stop)
	}
	if commit.Cleanup == nil || commit.Cleanup.WorkloadID != "wl-0001" || commit.Cleanup.WorkspaceID != "ws-0001" || commit.Cleanup.Reason != "cancelled" {
		t.Fatalf("commit cleanup intent = %+v", commit.Cleanup)
	}
	if commit.Subject == nil || commit.Subject.Len() != 2 {
		t.Fatalf("commit subject = %+v", commit.Subject)
	}
	wantDeadline := time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC)
	if !commit.Deadline.Equal(wantDeadline) {
		t.Fatalf("commit deadline = %v, want %v", commit.Deadline, wantDeadline)
	}
	wantAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !commit.At.Equal(wantAt) {
		t.Fatalf("commit time = %v, want %v", commit.At, wantAt)
	}
	if got := commit.Deadline.Sub(commit.At); got != time.Hour {
		t.Fatalf("vector deadline offset = %v, want 1h", got)
	}
	if commit.StopEvidence == nil || !commit.StopEvidence.IsConfirmed() {
		t.Fatalf("commit stop evidence = %+v", commit.StopEvidence)
	}
	if commit.CleanupUpdate == nil {
		t.Fatal("commit cleanup progress missing")
	}
	if commit.CleanupUpdate.State != kernel.CleanupPending || commit.CleanupUpdate.WorkloadID != "wl-0001" ||
		commit.CleanupUpdate.WorkspaceID != "ws-0001" || commit.CleanupUpdate.Attempts != 2 ||
		commit.CleanupUpdate.LastError != "runtime unavailable" {
		t.Fatalf("commit cleanup progress = %+v", commit.CleanupUpdate)
	}
	if commit.Answer == nil {
		t.Fatal("commit answer missing")
	}
	if commit.Answer.Decision != kernel.DecisionPass || !commit.Answer.DownstreamEligible() {
		t.Fatalf("commit answer = %+v", commit.Answer)
	}
	if commit.Answer.Outputs["decision"] != "approved" {
		t.Fatalf("commit answer outputs = %+v", commit.Answer.Outputs)
	}
	if commit.Answer.Actor != "principal-0001" || !commit.Answer.At.Equal(time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)) {
		t.Fatalf("commit answer actor/time = %+v", commit.Answer)
	}
	if string(commit.Answer.Provenance) != "signature" {
		t.Fatalf("commit answer provenance = %q", commit.Answer.Provenance)
	}
	if commit.Observation == nil || commit.Observation.Kind != kernel.ObservationExited {
		t.Fatalf("commit observation = %+v", commit.Observation)
	}
	if code, ok := commit.Observation.ExitStatus(); !ok || code != 0 {
		t.Fatalf("commit observation exit status = (%d, %v)", code, ok)
	}
	if !commit.Observation.StopConfirmed() {
		t.Fatal("commit observation lost confirmed stop evidence")
	}
	if commit.Launch == nil || commit.Launch.WorkloadID != "wl-0001" || commit.Launch.State != kernel.LaunchIssued ||
		commit.Launch.IdempotencyKey != "start-wl-0001" {
		t.Fatalf("commit launch intent = %+v", commit.Launch)
	}
	if commit.Failure == nil || commit.Failure.Kind != kernel.FailureTimeout ||
		commit.Failure.Reason != "deadline_expired" || commit.Failure.Detail == "" {
		t.Fatalf("commit failure = %+v", commit.Failure)
	}

	encoded, err := json.Marshal(commit)
	if err != nil {
		t.Fatalf("encode commit: %v", err)
	}
	assertJSONEqual(t, raw, encoded)
}

func TestLaunchIntentRetiringJSONRoundTrip(t *testing.T) {
	intent := kernel.LaunchIntent{
		WorkloadID:     "wl-1",
		State:          kernel.LaunchRetiring,
		IdempotencyKey: "k",
	}
	const want = `{"workload_id":"wl-1","state":"retiring","idempotency_key":"k"}`
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("encode launch intent: %v", err)
	}
	if string(encoded) != want {
		t.Fatalf("encoded launch intent = %s, want %s", encoded, want)
	}
	var decoded kernel.LaunchIntent
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode launch intent: %v", err)
	}
	if !reflect.DeepEqual(decoded, intent) {
		t.Fatalf("decoded launch intent = %+v, want %+v", decoded, intent)
	}
}

func TestCleanupVectors(t *testing.T) {
	raw := readVector(t, "cleanup.json")
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("decode cleanup entries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("cleanup vectors = %d, want 3", len(entries))
	}
	cleanups := make([]kernel.Cleanup, len(entries))
	for index, entry := range entries {
		if err := json.Unmarshal(entry, &cleanups[index]); err != nil {
			t.Fatalf("decode cleanup %d: %v", index, err)
		}
	}

	if cleanups[0].State != kernel.CleanupNone || cleanups[0].IsPending() {
		t.Fatalf("cleanup-none vector = %+v", cleanups[0])
	}
	pending := cleanups[1]
	if pending.State != kernel.CleanupPending || !pending.IsPending() {
		t.Fatalf("cleanup-pending vector = %+v", pending)
	}
	if pending.WorkloadID != "wl-0001" || pending.WorkspaceID != "ws-0001" || pending.Attempts != 2 || pending.LastError == "" {
		t.Fatalf("pending cleanup is not attributable: %+v", pending)
	}
	complete := cleanups[2]
	if complete.State != kernel.CleanupComplete || complete.IsPending() {
		t.Fatalf("cleanup-complete vector = %+v", complete)
	}

	for index, cleanup := range cleanups {
		encoded, err := json.Marshal(cleanup)
		if err != nil {
			t.Fatalf("encode cleanup %d: %v", index, err)
		}
		assertJSONEqual(t, entries[index], encoded)
	}
}

func TestSnapshotPresenceVector(t *testing.T) {
	raw := readVector(t, "snapshot.json")
	var entries []struct {
		Name    string `json:"name"`
		Task    []byte `json:"task"`
		Present bool   `json:"present"`
		TaskID  string `json:"task_id"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("decode snapshot entries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("snapshot vectors = %d, want 3", len(entries))
	}

	for _, entry := range entries {
		input := definition.SnapshotInput{
			YAML:   []byte("actions:\n- name: implement\n"),
			Task:   entry.Task,
			TaskID: id.TaskID(entry.TaskID),
		}
		snapshot := definition.NewSnapshot(input)

		task, present := snapshot.Task()
		if present != entry.Present {
			t.Fatalf("%s: snapshot task presence = %v, want %v", entry.Name, present, entry.Present)
		}
		if entry.Task == nil {
			if task != nil {
				t.Fatalf("%s: absent task bytes = %#v, want nil", entry.Name, task)
			}
		} else if task == nil {
			t.Fatalf("%s: present task bytes collapsed to nil", entry.Name)
		}
		if string(task) != string(entry.Task) {
			t.Fatalf("%s: snapshot task bytes = %q, want %q", entry.Name, task, entry.Task)
		}
		if got := snapshot.TaskID(); got != id.TaskID(entry.TaskID) {
			t.Fatalf("%s: snapshot task id = %q, want %q", entry.Name, got, entry.TaskID)
		}

		// The accessor returns a copy, so a caller cannot rewrite admitted
		// task bytes even when the admitted value is non-empty.
		if len(task) > 0 {
			original := string(task)
			task[0] ^= 0xff
			again, _ := snapshot.Task()
			if string(again) != original {
				t.Fatalf("%s: snapshot task mutated through accessor: %q", entry.Name, again)
			}
		}

		// The constructor copies the admission input. A present-empty task
		// must stay present rather than collapse to task-free.
		if len(entry.Task) > 0 {
			original := string(entry.Task)
			entry.Task[0] ^= 0xff
			again, present := snapshot.Task()
			if !present || string(again) != original {
				t.Fatalf("%s: snapshot task mutated through input: (%q, %v)", entry.Name, again, present)
			}
		}
	}
}
