package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/execution/memory"
)

// Test_unit_git_revocation_fails_closed: Git grants are never issued, and an
// attempt of a source-set run never has its authority ended, by a store that
// cannot revoke the grants in the same transaction (GR-06).
func Test_unit_git_revocation_fails_closed(t *testing.T) {
	m, s := executiontest.NewSourceMem(t)
	so := execution.Options{ClaimLease: 2e9, Batch: 10, ResourceFreshness: 20e9, ResourceRecheck: 1e9, ResourceBackoffBase: 1e9, ResourceBackoffMax: 8e9, TerminationDelay: -1}
	shared := &execution.SharedConfig{Resources: s.Resources, Policies: s.Policies}
	peers := m.World.Peers(m.Auth) // no GitGrants revoker
	peers.GrantFence = executiontest.OpenMemFence{}
	norev, err := memory.NewStore(m.DB, peers, memory.Options{Clock: m.Clock.Now, NewID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if norev.RevokesGitGrants() {
		t.Fatal("a store without the peer claims to revoke")
	}

	t.Run("a Git grant issuer cannot be composed with it", func(t *testing.T) {
		_, err := m.BuildService(executiontest.MemOptions{Shared: shared, ServiceOptions: &so, Store: norev, SourceSets: s.Repos, GitGrants: s.Git})
		if !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("an issuer over a store that cannot revoke: %v", err)
		}
		if _, err := m.BuildService(executiontest.MemOptions{Shared: shared, ServiceOptions: &so, SourceSets: s.Repos, GitGrants: s.Git}); err != nil {
			t.Fatalf("the revoking store composes: %v", err)
		}
	})

	t.Run("a mutation that ends a source attempt's authority is refused", func(t *testing.T) {
		r := executiontest.NewSourceRun(t, s, "write")
		in := r.Claim()
		svc, err := m.BuildService(executiontest.MemOptions{Shared: shared, ServiceOptions: &so, Store: norev, SourceSets: s.Repos})
		if err != nil {
			t.Fatal(err)
		}
		caller := execution.Caller{PrincipalID: executiontest.Engineer}
		if _, err := svc.Cancel(context.Background(), caller, executiontest.ScopeA, r.Run.ID); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a cancellation without a revoker: %v", err)
		}
		agg, err := m.Mem.Get(context.Background(), executiontest.ScopeA, r.Run.ID)
		if err != nil || agg.Run.CancelRequestedAt != nil || agg.Attempt(in.AttemptID).Status != execution.AttemptRunning || s.GitRevoked(in.AttemptID) {
			t.Fatalf("the refused cancellation changed state: %v", err)
		}
		if _, err := m.Svc.Cancel(context.Background(), caller, executiontest.ScopeA, r.Run.ID); err != nil || !s.GitRevoked(in.AttemptID) {
			t.Fatalf("the revoking store cancels and revokes: %v", err)
		}
	})
}
