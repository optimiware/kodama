package factory

// Live external-resource evidence for one execution.
//
// The public execution inspect exposes the retained workspace identity only
// while a cleanup record carries it, so a terminal "none" record has no public
// workspace id. The factory's controller-owned database row is the
// authoritative source of the identity; the volume probe then checks the real
// Podman state through the same run-labelled CLI path as the container helper.
// These helpers add no product behavior and never synthesize a workspace id.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// ExecutionWorkspaceID reads the durable retained workspace identity of one
// execution. The caller must have created the execution through the served
// composition; the row is read through the runtime role's transaction-local
// scope, exactly like the other fixture evidence reads.
func (h *Harness) ExecutionWorkspaceID(ctx context.Context, executionID string) (string, error) {
	h.T.Helper()
	rows, err := pgtest.ScopedQuery(ctx, h.runtimeDB, h.Scope, 1,
		"SELECT workspace_id FROM executions WHERE id = $1", executionID)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("execution %s returned %d workspace rows; want 1", executionID, len(rows))
	}
	return rows[0][0], nil
}

// RequestSubject reads the persisted immutable subject of one request action
// through the runtime role's transaction-local scope. Activation writes the
// resolved subject once; a later pipeline edit and the settlement never rewrite
// it, so the caller can compare the same persisted binding before and after
// resolution.
func (h *Harness) RequestSubject(ctx context.Context, executionID, actionName string) (map[string]definition.SubjectValue, error) {
	h.T.Helper()
	rows, err := pgtest.ScopedQuery(ctx, h.runtimeDB, h.Scope, 1,
		"SELECT coalesce(request_subject::text, '') FROM actions WHERE execution_id = $1 AND name = $2",
		executionID, actionName)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return nil, fmt.Errorf("request action %s/%s returned %d subject rows; want 1", executionID, actionName, len(rows))
	}
	if rows[0][0] == "" {
		return nil, fmt.Errorf("request action %s/%s has no persisted subject", executionID, actionName)
	}
	var subject map[string]definition.SubjectValue
	if err := json.Unmarshal([]byte(rows[0][0]), &subject); err != nil {
		return nil, fmt.Errorf("decode request subject of %s/%s: %w", executionID, actionName, err)
	}
	return subject, nil
}

// ReadExecutionWorkspaceFile reads one regular file from the retained workspace
// volume of one execution. It exports the run's deterministic volume through
// the CLI and starts no container, so it is only meaningful while no workload
// of the execution is running (for example during a request wait).
func (h *Harness) ReadExecutionWorkspaceFile(ctx context.Context, executionID, logical string) ([]byte, bool, error) {
	h.T.Helper()
	workspaceID, err := h.ExecutionWorkspaceID(ctx, executionID)
	if err != nil {
		return nil, false, err
	}
	name, err := runtimepodman.WorkspaceVolumeName(id.WorkspaceID(workspaceID))
	if err != nil {
		return nil, false, err
	}
	data, found := runtimesupport.ReadVolumeFile(ctx, h.T, name, logical)
	return data, found, nil
}

// ExecutionCredential reads the execution credential the composition
// materialized for one already-activated container action. It is test evidence
// only: the caller uses the value to present it to the served edge and prove
// that a task handoff revoked it. The value is never logged, recorded in a
// receipt or written into task metadata.
//
// The runtime adapter materializes the sole mutable credential file under the
// deterministic workload root it owns; the workload identity comes from the
// durable action row, exactly like the other fixture evidence reads. A missing
// or empty credential file is reported, never synthesized.
func (h *Harness) ExecutionCredential(ctx context.Context, executionID, actionName string) (string, error) {
	h.T.Helper()
	row, err := h.ActionRow(ctx, executionID, actionName)
	if err != nil {
		return "", fmt.Errorf("read action %s/%s: %w", executionID, actionName, err)
	}
	if row.WorkloadID == "" {
		return "", fmt.Errorf("action %s/%s has no workload identity yet", executionID, actionName)
	}
	path := filepath.Join(h.runtimeRoot, row.WorkloadID, "kodama", "secrets", id.SecretTokenName)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read materialized execution credential: %w", err)
	}
	credential := strings.TrimSpace(string(data))
	if credential == "" {
		return "", fmt.Errorf("materialized execution credential is empty")
	}
	return credential, nil
}

// ExecutionCredentialNoncePresent reports whether one execution still stores
// its current credential nonce. The edge also disables an execution principal
// as soon as the execution leaves the running state, so a post-terminal
// authentication refusal alone cannot prove that a handoff revoked the
// credential: the state refusal and the revocation are indistinguishable at the
// public edge. This durable probe separates them by reading the authoritative
// nonce column through the runtime role's transaction-local scope; it never
// returns the nonce value.
func (h *Harness) ExecutionCredentialNoncePresent(ctx context.Context, executionID string) (bool, error) {
	h.T.Helper()
	rows, err := pgtest.ScopedQuery(ctx, h.runtimeDB, h.Scope, 1,
		"SELECT token_nonce IS NOT NULL FROM executions WHERE id = $1", executionID)
	if err != nil {
		return false, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return false, fmt.Errorf("execution %s returned %d credential-presence rows; want 1", executionID, len(rows))
	}
	switch rows[0][0] {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("execution %s credential presence %q is not boolean", executionID, rows[0][0])
	}
}

// ExecutionPrincipal reads the durable capability identity one execution was
// started with. A materialized execution credential authenticates as exactly
// this principal, so a caller can attribute a durable row to the execution
// instead of to the service or human that initiated the task. The value is the
// executions.principal_id column, read through the runtime role's
// transaction-local scope; it is an identity, never credential material.
func (h *Harness) ExecutionPrincipal(ctx context.Context, executionID string) (string, error) {
	h.T.Helper()
	rows, err := pgtest.ScopedQuery(ctx, h.runtimeDB, h.Scope, 1,
		"SELECT principal_id FROM executions WHERE id = $1", executionID)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("execution %s returned %d principal rows; want 1", executionID, len(rows))
	}
	if rows[0][0] == "" {
		return "", fmt.Errorf("execution %s has no principal identity", executionID)
	}
	return rows[0][0], nil
}

// WorkspaceVolumePresent reports whether the deterministic retained workspace
// volume of one workspace still exists in Podman. An invalid workspace id is
// refused instead of being looked up. The probe fails closed: `podman volume
// exists` exits 1 exactly when the volume is absent, so only that status is
// reported as absence. Any other failure (a missing binary, an unreachable
// store, a permission error) is returned, because silently reading it as
// absence would let a cleanup assertion pass without observing Podman.
func (h *Harness) WorkspaceVolumePresent(ctx context.Context, workspaceID string) (bool, error) {
	h.T.Helper()
	name, err := runtimepodman.WorkspaceVolumeName(id.WorkspaceID(workspaceID))
	if err != nil {
		return false, err
	}
	result, err := runtimesupport.RunPodman(ctx, "volume", "exists", name)
	if err != nil {
		if result.ExitCode == 1 {
			return false, nil
		}
		return false, fmt.Errorf("probe workspace volume %s: %w", name, err)
	}
	return true, nil
}
