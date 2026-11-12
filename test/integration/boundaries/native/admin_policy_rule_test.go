//go:build integration

package native_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

// The proposed policies below all parse. They differ only in whether the rule
// evaluator lets the @admin role write the trusted policy ref of the `admin`
// repository, which is the self-governance invariant.

// policyWithoutAdminRepository is a valid policy that omits the admin
// repository entirely: accepting it would leave nobody able to push the next
// policy.
const policyWithoutAdminRepository = `
repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminReadOnly parses and grants the admin role only a read of its own
// repository. A read rule does not cover an update.
const policyAdminReadOnly = `
repo admin
    R = @admin

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminWriteToOtherRole grants write on the admin repository, but to a
// principal that is not the admin identity. The evaluator's rule list has no
// group construct: a rule whose principal list does not match @admin (the
// equivalent of a group without the admin member) leaves the policy ref
// unwritable by the role that governs it.
const policyAdminWriteToOtherRole = `
repo admin
    RW = @engineer

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminWriteToService grants write on the admin repository only to one
// explicit service id. The admin role is not a service identity, so the
// evaluator denies the policy push.
const policyAdminWriteToService = `
repo admin
    RW = service-i03c

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminWriteOtherRef grants write to @admin but only on an unrelated
// work ref. The trusted policy ref itself stays unwritable.
const policyAdminWriteOtherRef = `
repo admin
    RW refs/heads/work/ = @admin

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminDeniedThenWildcardWrite relies on ordered evaluation: the first
// matching rule for @admin is a deny, so the later wildcard write never makes
// admin writable.
const policyAdminDeniedThenWildcardWrite = `
repo admin
    - = @admin
    RW = @all

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminWriteFirstCommitShape is the documented first-commit shape.
const policyAdminWriteFirstCommitShape = `
repo admin
    RW = @admin
`

// policyAdminWriteWildcard grants write through the @all wildcard, which the
// evaluator matches for the admin identity.
const policyAdminWriteWildcard = `
repo admin
    RW = @all
`

// policyAdminWritePrincipalList includes @admin in a principal list, the only
// "group expansion" the grammar has.
const policyAdminWritePrincipalList = `
repo admin
    RW = @engineer @admin @reviewer
`

// policyAdminWriteRefPrefix covers refs/heads/main through the ref prefix form.
const policyAdminWriteRefPrefix = `
repo admin
    RW refs/heads/ = @admin
`

// The proposed policies below all grant @admin a write of the trusted policy
// ref, so the write-rule invariant alone accepts them. They additionally carry
// an approval guard that covers refs/heads/main. The real push path refuses a
// guarded ref with ErrApprovalRequired unless the identity is an execution
// carrying approval, and a human is never an execution, so accepting one of
// these policies would lock every later human policy push out of the admin
// repository. They must be refused up front.

// policyAdminWriteWithExactApprovalGuard guards the trusted policy ref itself.
const policyAdminWriteWithExactApprovalGuard = `
repo admin
    RW = @admin
    option approve.refs/heads/main = reviewer

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminWriteWithPrefixApprovalGuard covers the trusted policy ref through
// the ref prefix form, exactly as the evaluator's ApprovalFor matches it.
const policyAdminWriteWithPrefixApprovalGuard = `
repo admin
    RW = @admin
    option approve.refs/heads/ = reviewer

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminWriteWithOtherApproverGuard shows the guard refuses for any
// approver value, not only for a particular role.
const policyAdminWriteWithOtherApproverGuard = `
repo admin
    RW = @admin
    option approve.refs/heads/main = operator

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// policyAdminApprovalGuardOnAnotherRef guards only an unrelated work ref. It
// does not cover the trusted policy ref, so it is accepted and leaves the admin
// main pushable.
const policyAdminApprovalGuardOnAnotherRef = `
repo admin
    RW = @admin
    option approve.refs/heads/work/ = reviewer

repo shop/web
    R = @all
    RW refs/heads/ = @execution
`

// readAdminRefBytes returns the raw accepted-ref file so a refusal can be
// proved byte-identical, not merely equal after trimming.
func readAdminRefBytes(t *testing.T, harness *nativefix.Harness) []byte {
	t.Helper()
	data, err := os.ReadFile(adminRefPath(t, harness))
	if err != nil {
		t.Fatalf("read admin trusted ref: %v", err)
	}
	return data
}

// requireProposalObjectsAbsent proves a refused proposal left no reachable
// object behind: the candidate commit is not readable from the accepted object
// store and receive-pack removed its quarantine directory.
func requireProposalObjectsAbsent(t *testing.T, harness *nativefix.Harness, candidate string) {
	t.Helper()
	adminPath := harness.RepositoryPath(nativefix.AdminRepository)
	if _, _, code := harness.Git(t, "--git-dir="+adminPath, "cat-file", "-e", candidate+"^{commit}"); code == 0 {
		t.Fatalf("refused proposal commit %s is still readable in the accepted object store", candidate)
	}
	entries, err := os.ReadDir(filepath.Join(adminPath, "objects"))
	if err != nil {
		t.Fatalf("read admin object store: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "incoming-") {
			t.Fatalf("refused proposal left quarantine directory objects/%s behind", entry.Name())
		}
	}
}

// TestAdminPolicyUpdateWithoutAdminWriteRuleRefused covers the update path: an
// existing valid policy is replaced by a parseable proposal that leaves the
// admin policy ref unwritable, and every such proposal must be refused before
// receive-pack moves the trusted ref.
func TestAdminPolicyUpdateWithoutAdminWriteRuleRefused(t *testing.T) {
	tests := []struct {
		name   string
		policy string
	}{
		{name: "no admin repository", policy: policyWithoutAdminRepository},
		{name: "admin read only", policy: policyAdminReadOnly},
		{name: "admin write to another role", policy: policyAdminWriteToOtherRole},
		{name: "admin write to a service only", policy: policyAdminWriteToService},
		{name: "admin write limited to another ref", policy: policyAdminWriteOtherRef},
		{name: "earlier deny matches admin", policy: policyAdminDeniedThenWildcardWrite},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newAdminHarness(t, nativefix.Options{})
			admin := cloneAdmin(t, harness, "admin-rule-refused-"+strings.ReplaceAll(test.name, " ", "-"))
			accepted := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")
			if accepted == "" {
				t.Fatal("the fixture admin repository has no committed policy")
			}

			writeAdminPolicy(t, admin, test.policy)
			commitAdminPolicy(t, harness, admin, "proposal without admin write")
			stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/main")
			requireFailure(t, stderr, code)
			requireRejectionMessage(t, stderr)
			if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
				t.Fatalf("refused policy moved admin main to %q; want %q", observed, accepted)
			}
		})
	}
}

// TestAdminPolicyUpdateWithAdminWriteRuleAccepted proves the positive side of
// the same update path. Each shape makes @admin writable on the policy ref
// according to the evaluator, so the push is accepted.
func TestAdminPolicyUpdateWithAdminWriteRuleAccepted(t *testing.T) {
	tests := []struct {
		name   string
		policy string
	}{
		{name: "first-commit shape", policy: policyAdminWriteFirstCommitShape},
		{name: "wildcard principal", policy: policyAdminWriteWildcard},
		{name: "principal list includes admin", policy: policyAdminWritePrincipalList},
		{name: "ref prefix covers policy ref", policy: policyAdminWriteRefPrefix},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newAdminHarness(t, nativefix.Options{})
			admin := cloneAdmin(t, harness, "admin-rule-accepted-"+strings.ReplaceAll(test.name, " ", "-"))

			writeAdminPolicy(t, admin, test.policy)
			candidate := commitAdminPolicy(t, harness, admin, "valid admin write proposal")
			requireAdminPush(t, harness, admin)
			if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != candidate {
				t.Fatalf("accepted policy ref = %q; want candidate %q", observed, candidate)
			}
		})
	}
}

// TestAdminPolicyRefusedUpdateLeavesTrustedRefByteIdentical is the strict
// before/after proof for the update path: a refused parseable proposal must not
// rewrite the accepted ref file at all.
func TestAdminPolicyRefusedUpdateLeavesTrustedRefByteIdentical(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	admin := cloneAdmin(t, harness, "admin-rule-byte-identical")
	accepted := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")
	before := readAdminRefBytes(t, harness)

	writeAdminPolicy(t, admin, policyAdminReadOnly)
	commitAdminPolicy(t, harness, admin, "read-only admin proposal")
	stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)

	after := readAdminRefBytes(t, harness)
	if !bytes.Equal(before, after) {
		t.Fatalf("refused proposal rewrote the trusted ref: before %q after %q", before, after)
	}
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
		t.Fatalf("refused policy moved admin main to %q; want %q", observed, accepted)
	}
}

// TestAdminPolicyRefusedUpdateLeavesObjectsUnreachable proves the quarantine
// is discarded: a refused proposal's commit is not reachable from the accepted
// repository and no incoming object directory remains.
func TestAdminPolicyRefusedUpdateLeavesObjectsUnreachable(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	admin := cloneAdmin(t, harness, "admin-rule-unreachable")

	writeAdminPolicy(t, admin, policyAdminWriteToOtherRole)
	candidate := commitAdminPolicy(t, harness, admin, "unreachable proposal")
	if candidate == "" {
		t.Fatal("candidate commit is empty")
	}
	stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	requireProposalObjectsAbsent(t, harness, candidate)
}

// TestAdminPolicyUpdateWithApprovalGuardedPolicyRefRefused is the update-path
// red case for the approval invariant: the proposal grants @admin write but
// also approval-guards the trusted policy ref, so no later human push could move
// it. The push must be refused before receive-pack moves the trusted ref; the
// ref stays byte-identical and the proposal objects stay unreachable.
func TestAdminPolicyUpdateWithApprovalGuardedPolicyRefRefused(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		approver string
	}{
		{name: "exact policy ref guard", policy: policyAdminWriteWithExactApprovalGuard, approver: "reviewer"},
		{name: "policy ref prefix guard", policy: policyAdminWriteWithPrefixApprovalGuard, approver: "reviewer"},
		{name: "another approver value", policy: policyAdminWriteWithOtherApproverGuard, approver: "operator"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newAdminHarness(t, nativefix.Options{})
			admin := cloneAdmin(t, harness, "admin-approval-guard-refused-"+strings.ReplaceAll(test.name, " ", "-"))
			accepted := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")
			if accepted == "" {
				t.Fatal("the fixture admin repository has no committed policy")
			}
			before := readAdminRefBytes(t, harness)

			writeAdminPolicy(t, admin, test.policy)
			candidate := commitAdminPolicy(t, harness, admin, "approval-guarded admin policy")
			stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/main")
			requireFailure(t, stderr, code)
			requireRejectionMessage(t, stderr)
			if strings.Contains(stderr, test.approver) {
				t.Fatalf("refusal echoes the approver value %q: %q", test.approver, stderr)
			}

			after := readAdminRefBytes(t, harness)
			if !bytes.Equal(before, after) {
				t.Fatalf("refused policy rewrote the trusted ref: before %q after %q", before, after)
			}
			if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
				t.Fatalf("refused policy moved admin main to %q; want %q", observed, accepted)
			}
			requireProposalObjectsAbsent(t, harness, candidate)
		})
	}
}

// TestAdminPolicyApprovalGuardOnAnotherRefAcceptedAndPolicyRecoverable is the
// negative control for the approval invariant: a guard on refs/heads/work/ does
// not cover the trusted policy ref, so the policy is accepted AND the admin can
// still push a plain policy afterwards. That recovery push proves accepting the
// work-ref guard causes no lockout.
func TestAdminPolicyApprovalGuardOnAnotherRefAcceptedAndPolicyRecoverable(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	admin := cloneAdmin(t, harness, "admin-approval-other-ref")

	writeAdminPolicy(t, admin, policyAdminApprovalGuardOnAnotherRef)
	guarded := commitAdminPolicy(t, harness, admin, "accepted guard on another ref")
	requireAdminPush(t, harness, admin)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != guarded {
		t.Fatalf("accepted guard-on-another-ref policy ref = %q; want candidate %q", observed, guarded)
	}

	// The accepted policy guards only refs/heads/work/, so a human admin still
	// pushes admin main. This is the lockout recovery proof.
	writeAdminPolicy(t, admin, policyAdminWriteFirstCommitShape)
	recovery := commitAdminPolicy(t, harness, admin, "recovery plain admin policy")
	requireAdminPush(t, harness, admin)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != recovery {
		t.Fatalf("recovery policy ref = %q; want candidate %q", observed, recovery)
	}
}
