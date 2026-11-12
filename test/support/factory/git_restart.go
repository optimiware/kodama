package factory

// Git service restart and recovery boundaries for the composed factory suite.
//
// The production `git serve` process owns its listener, hook socket, actor
// registry and startup report-recovery sweep. A crash-and-restart test must
// stop exactly that composition over the same durable storage, journal,
// credential file and policy repository, then serve the same address again:
//
//   - Restart closes the current Run call, rebinds the same loopback address
//     (the registered trusted origins and the workload relay keep working),
//     installs a fresh private hook socket and starts a new Run. The durable
//     storage root and journal directory are never recreated or rewritten.
//   - Recover composes the production `git serve` server over the same durable
//     state and invokes the real Service.Recover for one source event. It is
//     the explicit reconciliation boundary: unlike the periodic sweep, it may
//     settle a journaled intent or uncertain entry from observed repository
//     evidence, and it never performs a verification or a second ref effect.
//
// Both helpers are fixture composition only. They inject no approval, journal
// entry, ref or verification: every fact they read or settle was produced by
// the served product.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// Restart stops the running Git composition and starts a fresh one over the
// same durable state at the same loopback address. It returns only after the
// restarted service answers a real authenticated Git read and its startup
// report-recovery sweep has been started.
func (fixture *GitFixture) Restart(t testing.TB) {
	t.Helper()
	if fixture == nil || fixture.service == nil || fixture.service.plan == nil {
		t.Fatalf("factory: the Git fixture has no running service to restart")
	}
	current := fixture.service
	listener := current.plan.config.Listener
	if listener == nil {
		t.Fatalf("factory: the running Git service has no listener")
	}
	address := listener.Addr().String()
	if err := current.close(); err != nil {
		t.Fatalf("factory: stop the Git service for restart: %v", err)
	}

	rebound, err := listenAgain(address, 15*time.Second)
	if err != nil {
		t.Fatalf("factory: rebind the Git service address %s: %v", address, err)
	}

	socketBase, err := os.MkdirTemp("", gitSocketDirectoryPrefix)
	if err != nil {
		_ = rebound.Close()
		t.Fatalf("factory: create the restarted hook socket directory: %v", err)
	}
	socketPath := filepath.Join(socketBase, "hook.sock")
	if len(socketPath) > native.MaxSocketPathBytes {
		_ = os.RemoveAll(socketBase)
		_ = rebound.Close()
		t.Fatalf("%v: restarted Git hook socket path %q is %d bytes; at most %d are allowed",
			ErrHookSocketPathTooLong, socketPath, len(socketPath), native.MaxSocketPathBytes)
	}

	plan, err := newGitServicePlan(gitServicePlanSpec{
		TenantID:         fixture.TenantID,
		BaseURL:          fixture.h.BaseURL,
		CredentialFile:   fixture.CredentialFile,
		RoundTripper:     fixture.transport,
		ResourceIDs:      fixture.resourceIDs,
		StorageRoot:      fixture.StorageRoot,
		JournalDirectory: fixture.JournalDirectory,
		HookBinary:       fixture.BinaryPath,
		HookSocketPath:   socketPath,
		Listener:         rebound,
		TLS:              fixture.h.TLS.ServerConfig(),
		Logger:           fixture.logger,
	})
	if err != nil {
		_ = os.RemoveAll(socketBase)
		_ = rebound.Close()
		t.Fatalf("factory: plan the restarted Git service: %v", err)
	}

	ready := func(checkCtx context.Context) error {
		_, stderr, code, runErr := fixture.client.run(checkCtx, fixture.h.AdminToken, "",
			"ls-remote", "--quiet", fixture.client.repositoryURL(GitAdminRepository), "refs/heads/main")
		if runErr != nil {
			return runErr
		}
		if code != 0 {
			return fmt.Errorf("restarted admin smart-HTTP read exited %d: %s", code, stderr)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), gitServiceStartupTimeout)
	defer cancel()
	service, err := launchGitService(ctx, plan, fixture.h.serveDone, gitapp.Run, ready, gitServiceJoinTimeout)
	if err != nil {
		_ = os.RemoveAll(socketBase)
		t.Fatalf("factory: start the restarted Git service: %v", err)
	}
	fixture.service = service
	fixture.SocketPath = socketPath
}

// listenAgain rebinds one exact TCP address after its previous listener closed.
// The kernel can briefly retain the port, so this is a bounded retry of the
// bind only; it never changes the advertised address.
func listenAgain(address string, timeout time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		listener, err := net.Listen("tcp", address)
		if err == nil {
			return listener, nil
		}
		last = err
		if time.Now().After(deadline) {
			return nil, last
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Recover composes the production `git serve` server over the fixture's
// durable storage and journal and performs the real Service.Recover for one
// source event. The periodic sweep deliberately skips intended and uncertain
// entries; this is the explicit observation boundary that may settle them.
func (fixture *GitFixture) Recover(t testing.TB, sourceEventID string) (git.RecoveryResult, error) {
	t.Helper()
	if fixture == nil || fixture.service == nil || fixture.service.plan == nil {
		t.Fatalf("factory: the Git fixture has no service configuration for recovery")
	}
	server, err := gitapp.New(fixture.service.plan.config)
	if err != nil {
		t.Fatalf("factory: compose the Git recovery server: %v", err)
	}
	return server.Service().Recover(t.Context(), sourceEventID)
}

// SetRef writes one bare-repository ref directly, without any served push or
// journal effect. It is the labelled observed-state seam a crash-boundary test
// uses to prove how recovery reconciles an already journaled intent against
// repository evidence. It never fabricates a journal entry or verification.
func (fixture *GitFixture) SetRef(t testing.TB, repository, ref, commit string) {
	t.Helper()
	if _, stderr, code := fixture.repositoryGit(t, repository, "update-ref", ref, commit); code != 0 {
		t.Fatalf("factory: set %s %s = %s: %s", repository, ref, commit, stderr)
	}
}
