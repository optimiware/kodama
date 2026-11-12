// Git service implementation API.
//
// This file is the test-side declaration of the behavior of
// internal/userland/git. The contract declarations (types.go, ports.go,
// contract_rules.go, contract_journal.go) are not redefined here. Every test
// drives the deterministic in-memory Git/verification fakes in fake_test.go
// and a real temporary directory for storage/journal bytes: no Git binary,
// process, network, database, broker or cloud call is involved.
//
// Seams declared elsewhere (not pinned here): Permission, UpdateKind,
// PrincipalRef, Identity, AccessRequest, ReadRequest, Decision, Rule,
// ApprovalRule, RepositoryPolicy (Authorize, AuthorizeRead, ApprovalFor), Config
// (Policy, PolicyNames), ParseConfig, JournalVersion, JournalState,
// RecoveryOutcome, JournalEntry (Validate, Reconcile, Reportable,
// ReportRequest), Command, CommandResult, RefState, CommitRefs, CommandExecutor,
// ReadAuthorizer, RefObserver, JournalStore, VerificationClient, RetentionQuery
// and Clock.
//
// Package declarations:
//
//	func NewService(ServiceConfig) (*Service, error)
//	type Service struct{ ... } // implements ReadAuthorizer
//
//	type ServiceConfig struct {
//	    TenantID  id.TenantID          // the one tenant this service is bound to
//	    Storage   Storage
//	    Policies  PolicySource
//	    Executor  CommandExecutor
//	    Observer  RefObserver
//	    Journal   JournalStore
//	    Verify    VerificationClient
//	    Retention RetentionQuery
//	    Clock     Clock
//	}
//	// Every field is required; a nil boundary or an invalid tenant refuses.
//
//	type PolicySource interface {
//	    Current(context.Context) (Config, error)
//	}
//	// The service reads Current for every attempt. It never caches the parsed
//	// config across attempts, so a successful admin push changes the next attempt.
//
//	type Storage interface {
//	    RepositoryPath(id.TenantID, string) (string, error)
//	    EnsureRepository(id.TenantID, string) (RepositoryLocation, error)
//	}
//	type RepositoryLocation struct {
//	    Path    string
//	    Created bool
//	}
//	func NewLocalStorage(root string) (*LocalStorage, error)
//	type LocalStorage struct{ ... } // implements Storage
//	// RepositoryPath validates the tenant and the tenant-relative repository
//	// name and returns an absolute path strictly inside root. The tenant is one
//	// path segment, so two tenants never share a directory for the same
//	// repository name. Traversal, absolute names, empty segments, uppercase,
//	// spaces, glob characters and parent segments refuse before any filesystem
//	// access. EnsureRepository creates the directory (and parents) and reports
//	// whether it created it; a second call reports Created=false.
//
//	type UpdateRequest struct {
//	    ResourceID      string // registered resource metadata identity
//	    Repository      string // tenant-relative repository name
//	    Ref             string // anchored ref; never a rule prefix
//	    ExpectedBase    string // empty means the ref must not exist yet
//	    Candidate       string // non-empty; the journal cannot carry a delete
//	    ActorCredential string // forwarded actor credential; never persisted
//	    SourceEventID   string // stable receive-event identity
//	}
//	type UpdateResult struct {
//	    Entry    JournalEntry
//	    Ref      RefState
//	    Report   api.ReportResponse
//	    Reported bool
//	}
//	type RecoveryResult struct {
//	    Entry    JournalEntry
//	    Outcome  RecoveryOutcome // empty for an already settled entry
//	    Report   api.ReportResponse
//	    Reported bool
//	}
//	type RetentionPlan struct {
//	    Repository string
//	    Commits    []string // deduplicated, first-seen order
//	    Pages      int
//	}
//	type BackupManifest struct {
//	    TenantID   id.TenantID
//	    Repository string
//	    Path       string
//	    Refs       []RefState // sorted by ref
//	    Retained   []string
//	}
//
//	func (*Service) AuthorizeRead(context.Context, string, Identity) (Decision, error)
//	func (*Service) Provision(context.Context, string) (RepositoryLocation, error)
//	func (*Service) Prepare(context.Context, UpdateRequest) (JournalEntry, error)
//	func (*Service) Apply(context.Context, JournalEntry) (JournalEntry, error)
//	func (*Service) Report(context.Context, JournalEntry) (api.ReportResponse, error)
//	func (*Service) Update(context.Context, UpdateRequest) (UpdateResult, error)
//	func (*Service) Recover(context.Context, string) (RecoveryResult, error)
//	func (*Service) Retention(context.Context, string, []string) (RetentionPlan, error)
//	func (*Service) BackupManifest(context.Context, string) (BackupManifest, error)
//
//	func NewFileJournal(directory string) (*FileJournal, error)
//	type FileJournal struct{ ... } // implements JournalStore
//
// The file journal stores exactly one JSON file per stable source-event ID
// directly under its directory and replaces it atomically. A state never
// regresses: `intended` may advance to `accepted`, `refused` or `uncertain`;
// `uncertain` may advance to `accepted`, `refused` or repeat `uncertain`;
// `accepted` may advance to `reported` or repeat `accepted`; `reported` may
// only repeat; `refused` is terminal. A stale or illegal write is an error and
// leaves the durable state unchanged. Unreadable bytes are a load error, never
// a silent missing entry.
//
// Sentinel errors (errors.Is):
//
//	var (
//	    ErrInvalidRepository   // malformed tenant-relative repository name
//	    ErrRepositoryUnknown   // the tenant config has no such repository
//	    ErrPolicyDenied        // ordered rules refuse the attempt
//	    ErrApprovalRequired    // approval-guarded ref without execution approval
//	    ErrStaleBase           // the observed ref is not the expected base
//	    ErrUncertain           // the ref effect may or may not have happened
//	    ErrNoJournalEntry      // Recover found no entry for the source event id
//	    ErrSourceEventConflict // same source event id with different content
//	)
//
// Command vocabulary (the only Git effects; the service never starts a process
// itself, it calls the injected CommandExecutor):
//
//	git -C <repository path> merge-base --is-ancestor <expected> <candidate>
//	    exit 0 = fast-forward; exit 1 = not an ancestor; anything else = failure
//	git -C <repository path> update-ref <ref> <candidate> <expected>
//	    compare-and-swap; a non-zero exit means the ref was not changed
//	git -C <repository path> for-each-ref --format=%(refname) %(objectname)
//	    one "ref commit" line per ref, used only by BackupManifest
//
// Receive sequence pinned for Update/Prepare/Apply/Report:
//
//  1. Prepare validates the request, reads the current tenant config, derives
//     the required approval role, verifies the actor through
//     VerificationClient.Verify with the canonical `git.ref.update` subject
//     (`operation`, `resource`, `ref`, `expected_base`, `candidate`; no
//     `repository` alias) and the exact `role/<role>` approval when guarded,
//     validates the verified principal and roles, performs a preliminary write
//     permission check before any repository command, determines the update
//     kind, evaluates the exact rule, enforces approval, and persists an
//     `intended` entry. A permanent refusal after verification records a
//     `refused` entry. Prepare never performs a ref effect.
//  2. Apply loads the latest entry, observes the ref, records acceptance only
//     when the candidate is present, refuses a stale base, and performs the
//     compare-and-swap only when the ref still equals the expected base. An
//     executor error, an unobservable ref or a lost acceptance write records
//     `uncertain` and returns ErrUncertain. Apply never re-verifies or
//     re-evaluates policy.
//  3. Report reports only a durably accepted entry, records `reported` after a
//     successful report, and retries reporting with identical identity/content.
//     It never performs a ref update.
//  4. Recover reconciles an `intended` or `uncertain` entry from observed ref
//     state: applied records acceptance and reports; not-applied records
//     `refused`; unknown records `uncertain` and never reports or re-applies.
//     An `accepted` entry is report-only. `reported` and `refused` are settled
//     no-ops with an empty outcome.
//  5. Update is the serialized entry point: per repository/ref and per stable
//     source event id, one attempt at a time. A duplicate delivery of identical
//     content never re-verifies or pushes the ref twice; changed content under
//     the same source event id refuses with ErrSourceEventConflict. A duplicate
//     that finds a pre-effect `intended` entry reconciles it from observed ref
//     state only (observed candidate records acceptance and reports, observed
//     base records `refused`, anything else stays `uncertain`) and never applies
//     the stored compare-and-swap. A permanent refusal returns the recorded
//     `refused` entry in UpdateResult.Entry with its sentinel error; a settled
//     duplicate returns that entry with no error.
//
// A delete (empty candidate) is not representable in the journal and is
// refused by request validation; delete permission semantics stay covered by the
// RepositoryPolicy.Authorize contract vector.
package git_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/id"
	git "github.com/optimiware/kodama/internal/userland/git"
)

// Compile-time pins for the new implementation surface.
var (
	_ git.ReadAuthorizer     = (*git.Service)(nil)
	_ git.Storage            = (*git.LocalStorage)(nil)
	_ git.JournalStore       = (*git.FileJournal)(nil)
	_ git.CommandExecutor    = (*fakeExecutor)(nil)
	_ git.RefObserver        = (*fakeObserver)(nil)
	_ git.VerificationClient = (*fakeVerifier)(nil)
	_ git.RetentionQuery     = (*fakeRetention)(nil)
	_ git.Clock              = (*fakeClock)(nil)
	_ git.PolicySource       = (*fakePolicySource)(nil)
)

// TestNewServiceRequiresEveryBoundary pins fail-closed construction: a missing
// boundary or an invalid tenant refuses instead of producing a half-wired
// service that silently skips verification, journaling or observation.
func TestNewServiceRequiresEveryBoundary(t *testing.T) {
	log := &callLog{}
	storage, err := git.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	base := git.ServiceConfig{
		TenantID:  testTenant,
		Storage:   storage,
		Policies:  &fakePolicySource{config: mustConfig(t, testGitConfig)},
		Executor:  newFakeExecutor(log),
		Observer:  &fakeObserver{executor: newFakeExecutor(log), log: log},
		Journal:   newFakeJournal(log),
		Verify:    newFakeVerifier(log),
		Retention: &fakeRetention{log: log},
		Clock:     &fakeClock{now: testNow},
	}

	service, err := git.NewService(base)
	if err != nil {
		t.Fatalf("NewService with every boundary: %v", err)
	}
	if service == nil {
		t.Fatal("NewService returned a nil service")
	}

	omissions := []struct {
		name   string
		mutate func(*git.ServiceConfig)
	}{
		{"missing tenant", func(config *git.ServiceConfig) { config.TenantID = "" }},
		{"invalid tenant", func(config *git.ServiceConfig) { config.TenantID = id.TenantID("../escape") }},
		{"missing storage", func(config *git.ServiceConfig) { config.Storage = nil }},
		{"missing policies", func(config *git.ServiceConfig) { config.Policies = nil }},
		{"missing executor", func(config *git.ServiceConfig) { config.Executor = nil }},
		{"missing observer", func(config *git.ServiceConfig) { config.Observer = nil }},
		{"missing journal", func(config *git.ServiceConfig) { config.Journal = nil }},
		{"missing verify", func(config *git.ServiceConfig) { config.Verify = nil }},
		{"missing retention", func(config *git.ServiceConfig) { config.Retention = nil }},
		{"missing clock", func(config *git.ServiceConfig) { config.Clock = nil }},
	}
	for _, omission := range omissions {
		t.Run(omission.name, func(t *testing.T) {
			config := base
			omission.mutate(&config)
			if _, err := git.NewService(config); err == nil {
				t.Fatalf("NewService accepted a config with a %s", omission.name)
			}
		})
	}
}
