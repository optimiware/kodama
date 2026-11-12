//go:build integration

package factorytest

// Shared fixture for the host Git/Shell composition.
//
// The fixture starts the real factory over Podman with the optional Git
// service. Everything the tests observe is the served composition: the Shell
// authenticates every Git actor, records the verification, the real native Git
// hooks journal and report the ref update, and the fixture reads those facts
// back read-only. No contract server, fabricated verifier or fixture admission
// fact participates.

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/userland/git"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// gitEngineerSubject is the OIDC subject of the one human this suite
	// exercises. Its authority is the committed project membership, never the
	// minted token alone.
	gitEngineerSubject = "git-engineer"

	// gitWorkRef is a ref the fixture policy grants both the engineer and a
	// live execution.
	gitWorkRef = "refs/heads/work/git-service"
	// gitMainRef is the protected ref: only an execution may update it, and
	// only with a reviewer approval.
	gitMainRef = "refs/heads/main"

	// gitWorkflowTimeout bounds one wait for a durable fact the served push
	// path must produce. The Git hook subprocesses are synchronous, so this is
	// a generous barrier, not a sleep.
	gitWorkflowTimeout = 60 * time.Second
)

// gitHarness is one real factory composition with Git plus the OIDC engineer
// that drives the host-side Git client.
type gitHarness struct {
	*factory.Harness

	git               *factory.GitFixture
	engineerToken     string
	engineerPrincipal string
}

// startGitHarness starts the host Git composition and grants the OIDC engineer
// its project role through the served membership operation.
func startGitHarness(t *testing.T) *gitHarness {
	t.Helper()
	return startGitHarnessWith(t)
}

// startGitHarnessWith starts the same host Git composition with extra factory
// options. The Git composition and OIDC selection stay fixed; callers add a
// labelled seam such as factory.WithGitTransport or the image resolver.
func startGitHarnessWith(t *testing.T, options ...factory.Option) *gitHarness {
	t.Helper()
	selected := append([]factory.Option{factory.WithGitService(), factory.WithOIDC()}, options...)
	harness := factory.Start(t, selected...)
	gitFixture := harness.Git
	if gitFixture == nil {
		t.Fatalf("factory.WithGitService did not expose a Git fixture")
	}

	token := harness.OIDC.Mint(t, identityfixture.TokenSpec{Subject: gitEngineerSubject})
	principal, err := shelloidc.PrincipalIDFor(harness.OIDC.Issuer, gitEngineerSubject)
	if err != nil {
		t.Fatalf("derive engineer principal: %v", err)
	}
	harness.MustREST(harness.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(principal),
		Roles:       []string{"engineer"},
	}, nil, "grant-"+gitEngineerSubject)

	return &gitHarness{
		Harness:           harness,
		git:               gitFixture,
		engineerToken:     token,
		engineerPrincipal: string(principal),
	}
}

// clone clones one repository with the given actor credential.
func (fixture *gitHarness) clone(t *testing.T, token, repository, name string) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), name)
	stderr, code := fixture.git.Clone(t, token, repository, directory)
	if code != 0 {
		t.Fatalf("clone %s: exit %d: %s", repository, code, stderr)
	}
	return directory
}

// commit writes one deterministic commit on the checked-out branch.
func (fixture *gitHarness) commit(t *testing.T, directory, message string) string {
	t.Helper()
	return fixture.git.Commit(t, directory, message)
}

// push pushes one refspec with the given actor credential and returns the
// stderr and exit code so a refusal can be asserted without failing the test.
func (fixture *gitHarness) push(directory, token string, refspecs ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return fixture.git.Push(ctx, directory, token, refspecs...)
}

// readBinding reads one registered binding back through the public operation.
func (fixture *gitHarness) readBinding(t *testing.T, bindingID string) api.ResourceBindingReadResult {
	t.Helper()
	result := fixture.MustREST(fixture.ProjectToken, "resource.binding.read", api.ResourceBindingReadRequest{
		BindingID: bindingID,
	}, nil, "")
	var binding api.ResourceBindingReadResult
	fixture.Decode(result, &binding)
	return binding
}

// waitJournal waits for the final durable journal entry of one accepted ref
// update and returns it. On timeout the fixture prints the journal listing and
// the newest verification rows so the missing fact is attributable.
func (fixture *gitHarness) waitJournal(t *testing.T, ref, candidate string) git.JournalEntry {
	t.Helper()
	var found git.JournalEntry
	fixture.git.WaitFor(t,
		fmt.Sprintf("journal entry ref=%s candidate=%s state=%s", ref, candidate, git.JournalReported),
		gitWorkflowTimeout, func() bool {
			for _, entry := range fixture.git.JournalEntries(t) {
				if entry.Ref == ref && entry.Candidate == candidate && entry.State == git.JournalReported {
					found = entry
					return true
				}
			}
			return false
		})
	return found
}

// waitVerifications waits for at least one verification row of one operation
// whose actor is the fixture engineer and returns the newest matching row. The
// actor filter is deliberate: harness setup performs legitimate readiness reads
// with the shared admin and engineer tokens, and those rows must not be
// mistaken for the OIDC engineer's own traffic.
func (fixture *gitHarness) waitVerifications(t *testing.T, operation string) factory.VerificationRow {
	t.Helper()
	var found factory.VerificationRow
	fixture.git.WaitFor(t,
		fmt.Sprintf("verification operation=%s actor=%s", operation, fixture.engineerPrincipal),
		gitWorkflowTimeout, func() bool {
			rows, err := fixture.git.Verifications(t.Context())
			if err != nil {
				return false
			}
			for _, row := range rows {
				if row.Operation == operation && row.ActorID == fixture.engineerPrincipal {
					found = row
					return true
				}
			}
			return false
		})
	return found
}

// verificationByID waits for one exact verification row.
func (fixture *gitHarness) verificationByID(t *testing.T, verificationID string) factory.VerificationRow {
	t.Helper()
	var found factory.VerificationRow
	fixture.git.WaitFor(t,
		fmt.Sprintf("verification id=%s", verificationID),
		gitWorkflowTimeout, func() bool {
			rows, err := fixture.git.Verifications(t.Context())
			if err != nil {
				return false
			}
			for _, row := range rows {
				if row.ID == verificationID {
					found = row
					return true
				}
			}
			return false
		})
	return found
}

// countVerificationRows counts the durable verification rows of one operation.
func (fixture *gitHarness) countVerificationRows(t *testing.T, operation string) int {
	t.Helper()
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	count := 0
	for _, row := range rows {
		if row.Operation == operation {
			count++
		}
	}
	return count
}

// refusedVerification waits for one refused verification row of one operation
// recorded for an execution actor.
func (fixture *gitHarness) refusedVerification(t *testing.T, executionID, operation string) factory.VerificationRow {
	t.Helper()
	var found factory.VerificationRow
	fixture.git.WaitFor(t,
		fmt.Sprintf("refused verification operation=%s execution=%s", operation, executionID),
		gitWorkflowTimeout, func() bool {
			rows, err := fixture.git.Verifications(t.Context())
			if err != nil {
				return false
			}
			for _, row := range rows {
				if row.Operation == operation && row.ExecutionID == executionID && row.Result == "refused" {
					found = row
					return true
				}
			}
			return false
		})
	return found
}

// waitReportEvent waits for the durable resource.verified event caused by one
// verification.
func (fixture *gitHarness) waitReportEvent(t *testing.T, verificationID string) factory.EventRow {
	t.Helper()
	var found factory.EventRow
	fixture.git.WaitFor(t,
		fmt.Sprintf("resource.verified event caused by verification=%s", verificationID),
		gitWorkflowTimeout, func() bool {
			event, ok, err := fixture.git.ReportEvent(t.Context(), verificationID)
			if err != nil {
				return false
			}
			if ok {
				found = event
				return true
			}
			return false
		})
	return found
}

// membershipVersion reads the current committed version of one project
// membership through the public read operation. A versioned revoke must quote
// it: the operation removes the row at exactly that version.
func (fixture *gitHarness) membershipVersion(t *testing.T, principalID string) uint64 {
	t.Helper()
	result := fixture.MustREST(fixture.AdminToken, "membership.list", api.MembershipListRequest{
		ProjectID: factory.ProjectID,
	}, nil, "")
	var list api.MembershipListResult
	fixture.Decode(result, &list)
	for _, membership := range list.Memberships {
		if membership.PrincipalID == principalID {
			return membership.Version
		}
	}
	t.Fatalf("membership for principal %s is absent from membership.list", principalID)
	return 0
}

// startExecution starts one blocking container action and returns the
// execution identity and its live credential. The caller must stop it; a
// cleanup is registered as soon as the execution has an identity.
func (fixture *gitHarness) startExecution(t *testing.T, name string) (string, string) {
	t.Helper()
	applied := fixture.applyPipeline(name, marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "work",
			Image:   fixture.Image.Reference,
			Timeout: "1h",
			Commands: []string{
				// Prove the workload receives no task handoff material for Git.
				`test ! -e /kodama/task.json`,
				// Hold the execution running while the host pushes with its
				// live credential.
				`while [ ! -e /workspace/release ]; do sleep 1; done`,
			},
		}},
	}))
	started := fixture.startPipeline(applied.PipelineID, "git-execution-"+name)
	t.Cleanup(func() { fixture.stopExecution(t, started.ExecutionID) })

	var credential string
	fixture.git.WaitFor(t,
		fmt.Sprintf("execution credential for execution=%s action=work", started.ExecutionID),
		2*time.Minute, func() bool {
			value, err := fixture.ExecutionCredential(t.Context(), started.ExecutionID, "work")
			if err != nil {
				return false
			}
			credential = strings.TrimSpace(value)
			return credential != ""
		})
	return started.ExecutionID, credential
}

// applyPipeline posts one pipeline document through the public API.
func (fixture *gitHarness) applyPipeline(name string, document []byte) api.PipelineApplyResult {
	result := fixture.MustREST(fixture.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	fixture.Decode(result, &applied)
	return applied
}

// startPipeline starts one stored pipeline at its observed version.
func (fixture *gitHarness) startPipeline(pipelineID, key string) api.PipelineStartResult {
	read := fixture.MustREST(fixture.ProjectToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, "")
	var stored api.PipelineReadResult
	fixture.Decode(read, &stored)
	result := fixture.MustREST(fixture.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, &stored.Version, key)
	var started api.PipelineStartResult
	fixture.Decode(result, &started)
	return started
}

// stopExecution cancels one execution through the public operation and waits
// for a terminal state. It never fails the test from a cleanup: the primary
// assertion already reported.
func (fixture *gitHarness) stopExecution(t *testing.T, executionID string) {
	t.Helper()
	for range 40 {
		result, publicError := fixture.REST(fixture.AdminToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			return
		}
		var current api.ExecutionInspectResult
		fixture.Decode(result, &current)
		switch current.State {
		case "succeeded", "failed", "cancelled":
			return
		}
		result, publicError = fixture.REST(fixture.AdminToken, "execution.cancel", api.ExecutionCancelRequest{
			ExecutionID: executionID,
		}, &current.Version, "cancel-"+executionID)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			return
		}
		var cancelled api.ExecutionCancelResult
		fixture.Decode(result, &cancelled)
	}
}

// directoryFor returns a fresh host directory under the test's temporary tree.
func directoryFor(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// requireRefusal asserts one Git command failed and returns its stderr.
func requireRefusal(t *testing.T, scenario, stderr string, code int) string {
	t.Helper()
	if code == 0 {
		t.Fatalf("%s: git command succeeded; want a refusal", scenario)
	}
	return stderr
}

// basicValue renders the Basic credential value for one Git token the same way
// the host Git helper does, without putting the token in a URL or a file.
func basicValue(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("kodama:" + token))
}
