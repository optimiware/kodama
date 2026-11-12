//go:build component

package runtimeconformance

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestPodmanRuntimeRetainedLogsAndReadPath proves real runtime log capture is
// ordered, bounded, redacted and stable across a controller restart, and that
// the retained sink behind execution.logs.read preserves ordering, isolation
// and truncation.
func TestPodmanRuntimeRetainedLogsAndReadPath(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-7")
	identityValue := testIdentity("exec-c7", "action-c7", "workload-c7", "idem-c7")
	request := fixture.request(identityValue, []string{
		`printf 'alpha\n'`,
		`printf 'beta\n'`,
		`printf 'err-one\n' >&2`,
		`printf 'err-two\n' >&2`,
		`cat /kodama/secrets/API_KEY`,
		`cat /kodama/secrets/KODAMA_TOKEN`,
	}, nil, []engine.SecretValue{{Name: "API_KEY", Value: "s3cr3t-value"}}, "tok-abc")
	if _, err := fixture.adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: identityValue, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPodmanObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: identityValue, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)

	// Read in one page so the configured values are whole in the chunk the
	// adapter redacts; a value split across caller-chosen pages is outside the
	// per-chunk redaction contract.
	stdout, stderr := readAllLogs(t, ctx, fixture.adapter, identityValue, 1<<20)
	wantStdout := "alpha\nbeta\n[REDACTED][REDACTED]"
	wantStderr := "err-one\nerr-two\n"
	if string(stdout) != wantStdout {
		t.Fatalf("stdout = %q; want %q", stdout, wantStdout)
	}
	if string(stderr) != wantStderr {
		t.Fatalf("stderr = %q; want %q", stderr, wantStderr)
	}
	if bytes.Contains(stdout, []byte("s3cr3t-value")) || bytes.Contains(stdout, []byte("tok-abc")) {
		t.Fatalf("retained output leaked secret material: %q", stdout)
	}

	// A reconstructed controller reads the same retained capture from zero.
	recovered := newPodmanAdapterOnly(t, fixture)
	recoveredStdout, recoveredStderr := readAllLogs(t, ctx, recovered, identityValue, 1<<20)
	if !bytes.Equal(recoveredStdout, stdout) || !bytes.Equal(recoveredStderr, stderr) {
		t.Fatalf("logs changed across controller restart: %q/%q vs %q/%q",
			recoveredStdout, recoveredStderr, stdout, stderr)
	}
	if again, _ := readAllLogs(t, ctx, recovered, identityValue, 1<<20); !bytes.Equal(again, stdout) {
		t.Fatalf("repeated log read is not stable: %q vs %q", again, stdout)
	}

	// A second workload without secrets proves small-page pagination and
	// ordering stay stable.
	paged := testIdentity("exec-c7b", "action-c7b", "workload-c7b", "idem-c7b")
	pagedRequest := fixture.request(paged, []string{
		`printf 'one\n'`,
		`printf 'two\n'`,
		`printf 'three\n'`,
		`printf 'err-a\n' >&2`,
		`printf 'err-b\n' >&2`,
	}, nil, nil, "tok-abc")
	if _, err := recovered.Prepare(ctx, pagedRequest); err != nil {
		t.Fatalf("paged Prepare: %v", err)
	}
	if _, err := recovered.Start(ctx, engine.StartRequest{Identity: paged, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("paged Start: %v", err)
	}
	waitForPodmanObservation(t, ctx, recovered,
		engine.ObserveRequest{Identity: paged, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	pagedStdout, pagedStderr := readAllLogs(t, ctx, recovered, paged, 3)
	if string(pagedStdout) != "one\ntwo\nthree\n" {
		t.Fatalf("paged stdout = %q", pagedStdout)
	}
	if string(pagedStderr) != "err-a\nerr-b\n" {
		t.Fatalf("paged stderr = %q", pagedStderr)
	}
	if again, _ := readAllLogs(t, ctx, recovered, paged, 3); !bytes.Equal(again, pagedStdout) {
		t.Fatalf("paged stdout is not stable: %q vs %q", again, pagedStdout)
	}
	if evidence := waitForPodmanStop(t, ctx, recovered, engine.StopRequest{Identity: paged, WorkspaceID: fixture.workspaceID}); !evidence.IsConfirmed() {
		t.Fatalf("paged stop evidence = %+v; want confirmed", evidence)
	}
	if removal, err := recovered.Remove(ctx, engine.RemoveRequest{Identity: paged, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("paged Remove = %+v, %v; want removed", removal, err)
	}

	// Retained sink: append the captured chunks, reopen, and read them back.
	sinkRoot := t.TempDir()
	scope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	sink := newFileSink(t, sinkRoot, log.DefaultLimits())
	appendCaptured(t, sink, scope, identityValue, stdout, stderr)
	reopened := newFileSink(t, sinkRoot, log.DefaultLimits())
	page, err := reopened.Read(ctx, log.ReadRequest{
		Scope: scope, ExecutionID: identityValue.ExecutionID, ActionID: identityValue.ActionID,
		Stream: log.StreamStdout, AfterOffset: 0, Limit: 100,
	})
	if err != nil {
		t.Fatalf("retained read: %v", err)
	}
	if got := joinChunks(page.Chunks); got != wantStdout {
		t.Fatalf("retained stdout = %q; want %q", got, wantStdout)
	}
	page, err = reopened.Read(ctx, log.ReadRequest{
		Scope: scope, ExecutionID: identityValue.ExecutionID, ActionID: identityValue.ActionID,
		Stream: log.StreamStderr, AfterOffset: 0, Limit: 100,
	})
	if err != nil {
		t.Fatalf("retained stderr read: %v", err)
	}
	if got := joinChunks(page.Chunks); got != wantStderr {
		t.Fatalf("retained stderr = %q; want %q", got, wantStderr)
	}

	// Isolation: another execution or scope never returns these lines.
	for name, read := range map[string]log.ReadRequest{
		"other execution": {
			Scope: scope, ExecutionID: "exec-other", ActionID: identityValue.ActionID,
			Stream: log.StreamStdout, Limit: 100,
		},
		"other scope": {
			Scope:       tenancy.Scope{TenantID: "tenant-b", ProjectID: "project-b"},
			ExecutionID: identityValue.ExecutionID, ActionID: identityValue.ActionID,
			Stream: log.StreamStdout, Limit: 100,
		},
	} {
		page, err := reopened.Read(ctx, read)
		if err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		if len(page.Chunks) != 0 {
			t.Fatalf("%s returned %d chunks; want 0", name, len(page.Chunks))
		}
	}

	// Bounded sizes: an oversized chunk and an oversized page are refused.
	if _, err := reopened.Append(ctx, scope, log.Chunk{
		ExecutionID: identityValue.ExecutionID, ActionID: identityValue.ActionID,
		Stream: log.StreamStdout, Offset: 9999, Sequence: 9999,
		Data: bytes.Repeat([]byte("x"), log.DefaultBounds.MaxChunkBytes+1),
	}); err == nil {
		t.Fatal("oversized retained chunk was accepted")
	}
	if _, err := reopened.Read(ctx, log.ReadRequest{
		Scope: scope, ExecutionID: identityValue.ExecutionID, ActionID: identityValue.ActionID,
		Stream: log.StreamStdout, Limit: log.DefaultLimits().MaxReadChunks + 1,
	}); err == nil {
		t.Fatal("oversized retained page was accepted")
	}

	// Retention truncation: keeping two chunks drops the oldest and marks the
	// continuation truncated.
	truncatedRoot := t.TempDir()
	limits := log.DefaultLimits()
	limits.MaxStoredChunks = 2
	truncatedSink := newFileSink(t, truncatedRoot, limits)
	for index := range 3 {
		if _, err := truncatedSink.Append(ctx, scope, log.Chunk{
			ExecutionID: "exec-trunc", ActionID: "action-trunc", Stream: log.StreamStdout,
			Offset: uint64(10 + index*3), Sequence: uint64(index + 1),
			Data: []byte(fmt.Sprintf("c%d", index)),
		}); err != nil {
			t.Fatalf("truncated append %d: %v", index, err)
		}
	}
	truncatedReopened := newFileSink(t, truncatedRoot, limits)
	page, err = truncatedReopened.Read(ctx, log.ReadRequest{
		Scope: scope, ExecutionID: "exec-trunc", ActionID: "action-trunc",
		Stream: log.StreamStdout, Limit: 10,
	})
	if err != nil {
		t.Fatalf("truncated read: %v", err)
	}
	if !page.Truncated {
		t.Fatal("retention truncation was not reported")
	}
	if got := joinChunks(page.Chunks); got != "c1c2" {
		t.Fatalf("retained chunks after truncation = %q; want c1c2", got)
	}

	// The authorized execution.logs.read path serves the same scoped sink
	// content, paginates and never crosses an execution/scope boundary.
	principal := identity.Principal{
		ID: "principal-engineer", Kind: identity.KindHuman, Scope: scope,
		Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1,
	}
	reader, err := log.NewReader(log.ReaderConfig{
		Authorizer: allowAuthorizer{},
		Authority:  allowAuthority{},
		Principals: staticPrincipals{principal: principal},
		Ownership:  noOwnership{},
		Sink:       reopened,
		Clock:      logClock{},
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var authorized []byte
	cursor := ""
	for {
		result, err := reader.Read(ctx, log.ReadCommand{
			Actor: principal, Scope: scope,
			ExecutionID: identityValue.ExecutionID, ActionID: identityValue.ActionID,
			Stream: log.StreamStdout, Cursor: cursor, Limit: 2,
		})
		if err != nil {
			t.Fatalf("authorized read: %v", err)
		}
		authorized = append(authorized, []byte(joinChunks(result.Chunks))...)
		if result.NextCursor == "" {
			break
		}
		if result.NextCursor == cursor {
			t.Fatal("authorized read made no progress")
		}
		cursor = result.NextCursor
	}
	if string(authorized) != wantStdout {
		t.Fatalf("authorized read = %q; want %q", authorized, wantStdout)
	}
	isolated, err := reader.Read(ctx, log.ReadCommand{
		Actor: principal, Scope: scope,
		ExecutionID: "exec-other", ActionID: identityValue.ActionID,
		Stream: log.StreamStdout, Limit: 10,
	})
	if err != nil {
		t.Fatalf("isolated authorized read: %v", err)
	}
	if len(isolated.Chunks) != 0 {
		t.Fatalf("authorized read crossed an execution boundary: %+v", isolated.Chunks)
	}

	if evidence := waitForPodmanStop(t, ctx, fixture.adapter, engine.StopRequest{Identity: identityValue, WorkspaceID: fixture.workspaceID}); !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	if removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: identityValue, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("Remove = %+v, %v; want removed", removal, err)
	}
	if err := fixture.adapter.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "podman", Scenario: "retained-logs-and-read-path",
		ImageTag: fixture.image.Tag, ImageDigest: fixture.image.Digest,
		Podman: runtimesupport.PodmanVersion(ctx),
	})
}

// readAllLogs pages the adapter's captured streams to completion.
func readAllLogs(t *testing.T, ctx context.Context, adapter interface {
	Logs(context.Context, engine.LogsRequest) (engine.LogBatch, error)
}, identityValue engine.EffectIdentity, pageSize int) ([]byte, []byte) {
	t.Helper()
	var stdout, stderr []byte
	var cursor int64
	for range 10000 {
		batch, err := adapter.Logs(ctx, engine.LogsRequest{
			Identity: identityValue, AfterOffset: cursor, Limit: pageSize,
		})
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		for _, chunk := range batch.Chunks {
			switch chunk.Stream {
			case engine.LogStdout:
				stdout = append(stdout, chunk.Data...)
			case engine.LogStderr:
				stderr = append(stderr, chunk.Data...)
			}
		}
		if batch.NextOffset == cursor {
			return stdout, stderr
		}
		cursor = batch.NextOffset
	}
	t.Fatal("log pagination did not terminate")
	return nil, nil
}

// appendCaptured appends the captured streams to one retained sink with a
// monotonic sequence per stream.
func appendCaptured(t *testing.T, sink *log.FileSink, scope tenancy.Scope, identityValue engine.EffectIdentity, stdout, stderr []byte) {
	t.Helper()
	ctx := context.Background()
	sequence := uint64(1)
	for _, chunk := range splitChunks(stdout, 4) {
		if _, err := sink.Append(ctx, scope, log.Chunk{
			ExecutionID: identityValue.ExecutionID, ActionID: identityValue.ActionID,
			Stream: log.StreamStdout, Offset: chunk.offset, Sequence: sequence, Data: chunk.data,
		}); err != nil {
			t.Fatalf("append stdout: %v", err)
		}
		sequence++
	}
	sequence = 1
	for _, chunk := range splitChunks(stderr, 4) {
		if _, err := sink.Append(ctx, scope, log.Chunk{
			ExecutionID: identityValue.ExecutionID, ActionID: identityValue.ActionID,
			Stream: log.StreamStderr, Offset: chunk.offset, Sequence: sequence, Data: chunk.data,
		}); err != nil {
			t.Fatalf("append stderr: %v", err)
		}
		sequence++
	}
}

type rawChunk struct {
	offset uint64
	data   []byte
}

func splitChunks(data []byte, size int) []rawChunk {
	var chunks []rawChunk
	for offset := 0; offset < len(data); offset += size {
		end := min(offset+size, len(data))
		chunks = append(chunks, rawChunk{offset: uint64(offset), data: append([]byte(nil), data[offset:end]...)})
	}
	return chunks
}

func joinChunks(chunks []log.Chunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		builder.Write(chunk.Data)
	}
	return builder.String()
}

func newFileSink(t *testing.T, root string, limits log.Limits) *log.FileSink {
	t.Helper()
	sink, err := log.NewFileSink(log.SinkConfig{Root: root, Clock: logClock{}, Limits: limits})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	return sink
}

// ---------------------------------------------------------------------------
// execution.logs.read fakes
// ---------------------------------------------------------------------------

type logClock struct{}

func (logClock) Now() time.Time { return time.Now() }

type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
	return authorize.DecisionAllow, nil
}

type allowAuthority struct{}

func (allowAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		facts[index] = authorize.AuthorityFact{
			Check: read.Check, Scope: read.Scope, PrincipalID: read.PrincipalID,
			TargetID: read.TargetID, Allowed: true,
		}
	}
	return facts, nil
}

type staticPrincipals struct {
	principal identity.Principal
}

func (s staticPrincipals) ReadPrincipal(context.Context, identity.PrincipalRequest) (identity.Principal, error) {
	return s.principal, nil
}

type noOwnership struct{}

func (noOwnership) Ownership(context.Context, tenancy.Scope, id.ExecutionID) (log.ExecutionOwnership, error) {
	return log.ExecutionOwnership{}, fault.New(fault.NotFound, "no task association")
}
