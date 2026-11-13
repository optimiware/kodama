package publiccontract

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	git "github.com/optimiware/kodama/internal/userland/git"
)

func loadGitConfig(t *testing.T) git.Config {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, "test", "contracts", "public", "testdata", "git", "git.conf"))
	if err != nil {
		t.Fatalf("read git.conf vector: %v", err)
	}
	config, err := git.ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig refused the vector: %v", err)
	}
	return config
}

func humanIdentity(roles ...string) git.Identity {
	return git.Identity{Roles: roles}
}

func executionIdentity() git.Identity {
	return git.Identity{Execution: true}
}

func serviceIdentity(id string) git.Identity {
	return git.Identity{ServiceID: id}
}

// TestGitRuleParserAndEvaluation pins S09's ordered rule semantics: the first
// matching rule whose permission covers the attempt allows, a matching deny
// refuses, prefix matching only applies to refs ending in "/", ref-less rules
// cover every ref, and no match refuses.
func TestGitRuleParserAndEvaluation(t *testing.T) {
	config := loadGitConfig(t)

	policy, ok := config.Policy("shop/web")
	if !ok {
		t.Fatal("Policy(shop/web) is missing")
	}
	if policy.Name != "shop/web" {
		t.Fatalf("policy name = %q", policy.Name)
	}
	if len(policy.Rules) != 3 {
		t.Fatalf("shop/web rules = %d; want 3", len(policy.Rules))
	}
	for _, rule := range policy.Rules {
		if rule.Line <= 0 {
			t.Errorf("rule %+v has no source line", rule)
		}
	}
	for _, approval := range policy.Approvals {
		if approval.Line <= 0 {
			t.Errorf("approval %+v has no source line", approval)
		}
	}

	cases := []struct {
		name     string
		repo     string
		identity git.Identity
		ref      string
		update   git.UpdateKind
		want     git.Decision
	}{
		{"viewer reads via @all", "shop/web", humanIdentity("viewer"), "refs/heads/main", git.UpdateRead, git.DecisionAllow},
		{"viewer cannot create", "shop/web", humanIdentity("viewer"), "refs/heads/main", git.UpdateCreate, git.DecisionDeny},
		{"engineer creates work branch", "shop/web", humanIdentity("engineer"), "refs/heads/work/x", git.UpdateCreate, git.DecisionAllow},
		{"engineer rewinds work branch", "shop/web", humanIdentity("engineer"), "refs/heads/work/x", git.UpdateRewind, git.DecisionAllow},
		{"engineer deletes work branch", "shop/web", humanIdentity("engineer"), "refs/heads/work/x", git.UpdateDelete, git.DecisionAllow},
		{"engineer cannot create main", "shop/web", humanIdentity("engineer"), "refs/heads/main", git.UpdateCreate, git.DecisionDeny},
		{"prefix needs the slash boundary", "shop/web", humanIdentity("engineer"), "refs/heads/work-notes/x", git.UpdateCreate, git.DecisionDeny},
		{"execution fast-forwards main", "shop/web", executionIdentity(), "refs/heads/main", git.UpdateFastForward, git.DecisionAllow},
		{"execution creates main", "shop/web", executionIdentity(), "refs/heads/main", git.UpdateCreate, git.DecisionAllow},
		{"execution cannot rewind main", "shop/web", executionIdentity(), "refs/heads/main", git.UpdateRewind, git.DecisionDeny},
		{"execution cannot delete main", "shop/web", executionIdentity(), "refs/heads/main", git.UpdateDelete, git.DecisionDeny},
		{"unmatched ref refuses", "shop/web", executionIdentity(), "refs/heads/feature", git.UpdateCreate, git.DecisionDeny},
		{"operator reads deploy", "shop/deploy", humanIdentity("operator"), "refs/heads/main", git.UpdateRead, git.DecisionAllow},
		{"engineer cannot read deploy", "shop/deploy", humanIdentity("engineer"), "refs/heads/main", git.UpdateRead, git.DecisionDeny},
		{"execution rewinds release", "shop/deploy", executionIdentity(), "refs/heads/release/2026", git.UpdateRewind, git.DecisionAllow},
		{"execution cannot rewind deploy main", "shop/deploy", executionIdentity(), "refs/heads/main", git.UpdateRewind, git.DecisionDeny},
		{"deny rule wins over later allow", "shop/guarded", humanIdentity("engineer"), "refs/heads/secret", git.UpdateCreate, git.DecisionDeny},
		{"engineer allowed on other refs", "shop/guarded", humanIdentity("engineer"), "refs/heads/other", git.UpdateCreate, git.DecisionAllow},
		{"named service allowed", "shop/guarded", serviceIdentity("shop-svc"), "refs/heads/svc", git.UpdateCreate, git.DecisionAllow},
		{"other service refused", "shop/guarded", serviceIdentity("other-svc"), "refs/heads/svc", git.UpdateCreate, git.DecisionDeny},
		{"admin rule is ref-less", "admin", humanIdentity("admin"), "refs/heads/anything", git.UpdateCreate, git.DecisionAllow},
		{"non-admin refused on admin repo", "admin", humanIdentity("engineer"), "refs/heads/anything", git.UpdateCreate, git.DecisionDeny},
	}
	for _, testCase := range cases {
		repositoryPolicy, ok := config.Policy(testCase.repo)
		if !ok {
			t.Errorf("%s: Policy(%q) is missing", testCase.name, testCase.repo)
			continue
		}
		request := git.AccessRequest{Identity: testCase.identity, Ref: testCase.ref, Update: testCase.update}
		if err := request.Validate(); err != nil {
			t.Errorf("%s: AccessRequest.Validate refused %+v: %v", testCase.name, request, err)
			continue
		}
		decision, err := repositoryPolicy.Authorize(request)
		if err != nil {
			t.Errorf("%s: Authorize returned error: %v", testCase.name, err)
			continue
		}
		if decision != testCase.want {
			t.Errorf("%s: decision = %q; want %q", testCase.name, decision, testCase.want)
		}
	}

	if _, ok := config.Policy("shop/missing"); ok {
		t.Error("Policy(shop/missing) unexpectedly exists")
	}
	if _, ok := config.Policy(""); ok {
		t.Error("Policy(empty) unexpectedly exists")
	}

	// Approval options: exact refs and prefix refs ending in "/", human role only.
	web, _ := config.Policy("shop/web")
	if role, ok := web.ApprovalFor("refs/heads/main"); !ok || role != "reviewer" {
		t.Errorf("ApprovalFor(main) = %q,%v; want reviewer,true", role, ok)
	}
	if role, ok := web.ApprovalFor("refs/heads/work/x"); ok {
		t.Errorf("ApprovalFor(work/x) = %q; want no approval", role)
	}
	guarded, _ := config.Policy("shop/guarded")
	if role, ok := guarded.ApprovalFor("refs/heads/release/2026"); !ok || role != "reviewer" {
		t.Errorf("ApprovalFor(release/2026) = %q,%v; want reviewer,true", role, ok)
	}
	if _, ok := guarded.ApprovalFor("refs/heads/release-notes"); ok {
		t.Error("prefix approval matched without the slash boundary")
	}
}

// TestGitReadAuthorization pins the repository-level Git read decision: reads
// are local RBAC rule evaluations for users, service accounts and pipeline
// service accounts, never a git.ref.read verification operation. Only a rule
// that covers the whole repository decides; R, RW and RW+ cover a read, and a
// matching deny or no repository-wide match refuses. An approval option gates
// updates, not reads.
func TestGitReadAuthorization(t *testing.T) {
	config := loadGitConfig(t)

	cases := []struct {
		name     string
		repo     string
		identity git.Identity
		want     git.Decision
	}{
		{"viewer reads via @all", "shop/web", humanIdentity("viewer"), git.DecisionAllow},
		{"engineer reads via @all", "shop/web", humanIdentity("engineer"), git.DecisionAllow},
		{"pipeline execution reads via @all", "shop/web", executionIdentity(), git.DecisionAllow},
		{"pipeline service account reads via @all", "shop/web", serviceIdentity("ci-pipeline"), git.DecisionAllow},
		{"a ref-scoped service rule does not cover the repository", "shop/guarded", serviceIdentity("shop-svc"), git.DecisionDeny},
		{"unlisted service account refuses", "shop/guarded", serviceIdentity("other-svc"), git.DecisionDeny},
		{"a ref-scoped deny and ref-prefix allow do not cover the repository", "shop/guarded", humanIdentity("engineer"), git.DecisionDeny},
		{"operator reads deploy", "shop/deploy", humanIdentity("operator"), git.DecisionAllow},
		{"engineer cannot read deploy", "shop/deploy", humanIdentity("engineer"), git.DecisionDeny},
	}
	for _, testCase := range cases {
		policy, ok := config.Policy(testCase.repo)
		if !ok {
			t.Errorf("%s: Policy(%q) is missing", testCase.name, testCase.repo)
			continue
		}
		request := git.ReadRequest{Identity: testCase.identity}
		if err := request.Validate(); err != nil {
			t.Errorf("%s: ReadRequest.Validate refused %+v: %v", testCase.name, request, err)
			continue
		}
		decision, err := policy.AuthorizeRead(request)
		if err != nil {
			t.Errorf("%s: AuthorizeRead returned error: %v", testCase.name, err)
			continue
		}
		if decision != testCase.want {
			t.Errorf("%s: decision = %q; want %q", testCase.name, decision, testCase.want)
		}
	}

	// An approval option gates updates only. The read seam never needs the
	// verification client or a git.ref.update approval fact.
	web, _ := config.Policy("shop/web")
	if role, ok := web.ApprovalFor("refs/heads/main"); !ok || role != "reviewer" {
		t.Fatalf("ApprovalFor(main) = %q,%v; want reviewer,true", role, ok)
	}
	decision, err := web.AuthorizeRead(git.ReadRequest{Identity: executionIdentity()})
	if err != nil || decision != git.DecisionAllow {
		t.Fatalf("AuthorizeRead = %q,%v; want allow despite the update approval option", decision, err)
	}

	if err := (git.ReadRequest{Identity: git.Identity{}}).Validate(); err == nil {
		t.Error("ReadRequest.Validate accepted an empty identity")
	}

	// The smart-HTTP read seam is context-first and resolves the repository's
	// local policy; no verification operation is involved.
	authorizer := fakeReadAuthorizer{config: config}
	decision, err = authorizer.AuthorizeRead(context.Background(), "shop/web", serviceIdentity("ci-pipeline"))
	if err != nil || decision != git.DecisionAllow {
		t.Fatalf("ReadAuthorizer service read = %q,%v; want allow", decision, err)
	}
	if _, err := authorizer.AuthorizeRead(context.Background(), "shop/missing", serviceIdentity("shop-svc")); err == nil {
		t.Fatal("ReadAuthorizer accepted an unconfigured repository")
	}
}

// TestGitRepositoryLevelReadAuthorization pins the repository-granularity read
// decision: only an ordered rule that covers the whole repository (a ref-less
// rule) decides, so a ref-scoped deny alone does not hide the repository. For
// `- refs/heads/private = @viewer` followed by `R = @viewer` the repository
// read is permitted.
func TestGitRepositoryLevelReadAuthorization(t *testing.T) {
	config, err := git.ParseConfig([]byte(`
repo shop/private
    -    refs/heads/private        = @viewer
    R                              = @viewer

repo shop/scoped
    RW   refs/heads/               = @engineer

repo shop/blocked
    -                              = @viewer
    R                              = @viewer
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cases := []struct {
		name       string
		repository string
		identity   git.Identity
		want       git.Decision
	}{
		{"ref-scoped deny does not hide the repository", "shop/private", humanIdentity("viewer"), git.DecisionAllow},
		{"a ref-less read rule covers the repository", "shop/private", humanIdentity("reviewer"), git.DecisionDeny},
		{"a service role list never matches a human role", "shop/private", serviceIdentity("shop-svc"), git.DecisionDeny},
		{"a ref-prefix-only rule does not cover the repository", "shop/scoped", humanIdentity("engineer"), git.DecisionDeny},
		{"a ref-less deny refuses the repository", "shop/blocked", humanIdentity("viewer"), git.DecisionDeny},
		{"an unknown repository refuses", "shop/missing", humanIdentity("viewer"), git.DecisionDeny},
	}
	for _, testCase := range cases {
		policy, ok := config.Policy(testCase.repository)
		if !ok {
			if testCase.want != git.DecisionDeny {
				t.Errorf("%s: Policy(%q) is missing", testCase.name, testCase.repository)
			}
			continue
		}
		decision, err := policy.AuthorizeRead(git.ReadRequest{Identity: testCase.identity})
		if err != nil {
			t.Errorf("%s: AuthorizeRead returned error: %v", testCase.name, err)
			continue
		}
		if decision != testCase.want {
			t.Errorf("%s: decision = %q; want %q", testCase.name, decision, testCase.want)
		}
	}
	if err := (git.ReadRequest{Identity: git.Identity{}}).Validate(); err == nil {
		t.Error("ReadRequest.Validate accepted an empty identity")
	}
}

// TestGitRuleParserRefusals pins malformed input refusal: no regex/glob, no
// implicit principals, no unknown permissions and no unanchored refs.
func TestGitRuleParserRefusals(t *testing.T) {
	refusals := []struct {
		name string
		body string
	}{
		{"rule before repo", "RW = @admin\n"},
		{"repo without name", "repo\n    RW = @admin\n"},
		{"missing equals", "repo admin\n    RW refs/heads/main @admin\n"},
		{"unknown permission", "repo admin\n    RWX = @admin\n"},
		{"empty principals", "repo admin\n    RW =\n"},
		{"glob ref", "repo admin\n    RW refs/heads/* = @admin\n"},
		{"unanchored ref", "repo admin\n    RW heads/main = @admin\n"},
		{"unknown principal", "repo admin\n    RW = @root\n"},
		{"bare principal", "repo admin\n    RW = admin\n"},
		{"empty approval ref", "repo admin\n    option approve. = reviewer\n"},
		{"unknown approval role", "repo admin\n    option approve.refs/heads/main = root\n"},
		{"service approval role", "repo admin\n    option approve.refs/heads/main = @engineer\n"},
		{"duplicate repo", "repo admin\n    RW = @admin\nrepo admin\n    RW = @admin\n"},
		{"trailing garbage", "repo admin\n    RW = @admin extra=\n"},
		{"unbalanced option", "repo admin\n    option approve.refs/heads/main reviewer\n"},
		{"option resource before repo", "option resource = res-1\nrepo admin\n    RW = @admin\n"},
		{"empty option resource", "repo admin\n    option resource =\n    RW = @admin\n"},
		{"invalid option resource slash", "repo admin\n    option resource = res/1\n    RW = @admin\n"},
		{"invalid option resource dot", "repo admin\n    option resource = ..\n    RW = @admin\n"},
		{"duplicate option resource", "repo admin\n    option resource = res-1\n    option resource = res-2\n    RW = @admin\n"},
		{"unknown option key", "repo admin\n    option unknown = res-1\n    RW = @admin\n"},
	}
	for _, testCase := range refusals {
		if _, err := git.ParseConfig([]byte(testCase.body)); err == nil {
			t.Errorf("%s: ParseConfig accepted %q", testCase.name, testCase.body)
		}
	}

	withResource, err := git.ParseConfig([]byte("repo my-app\n    option resource = res_my-app-1\n    R = @all\n"))
	if err != nil {
		t.Fatalf("ParseConfig refused valid option resource: %v", err)
	}
	if policy, ok := withResource.Policy("my-app"); !ok || policy.ResourceID != "res_my-app-1" {
		t.Fatalf("Policy(my-app).ResourceID = %q (ok=%v); want res_my-app-1", policy.ResourceID, ok)
	}

	// Comments and blank lines are insignificant.
	if _, err := git.ParseConfig([]byte("# comment\n\nrepo admin\n    # inline note\n    RW = @admin\n")); err != nil {
		t.Errorf("ParseConfig refused comments and blank lines: %v", err)
	}

	for _, principal := range []string{"@admin", "@engineer", "@reviewer", "@operator", "@viewer", "@execution", "@all", "shop-svc"} {
		if !git.PrincipalRef(principal).Valid() {
			t.Errorf("PrincipalRef(%q).Valid() = false", principal)
		}
	}
	for _, principal := range []string{"", "@", "@root", "@Admin", "shop svc", "admin"} {
		if git.PrincipalRef(principal).Valid() {
			t.Errorf("PrincipalRef(%q).Valid() = true", principal)
		}
	}
	for _, permission := range []git.Permission{git.PermissionRead, git.PermissionWrite, git.PermissionForce, git.PermissionDeny} {
		if !permission.Valid() {
			t.Errorf("Permission(%q).Valid() = false", permission)
		}
	}
	if git.Permission("RWX").Valid() {
		t.Error("Permission(RWX).Valid() = true")
	}
	for _, update := range []git.UpdateKind{git.UpdateRead, git.UpdateCreate, git.UpdateFastForward, git.UpdateRewind, git.UpdateDelete} {
		if !update.Valid() {
			t.Errorf("UpdateKind(%q).Valid() = false", update)
		}
	}
	if git.UpdateKind("force").Valid() {
		t.Error("UpdateKind(force).Valid() = true")
	}

	identityRefusals := []git.Identity{
		{},
		{Execution: true, ServiceID: "shop-svc"},
		{Execution: true, Roles: []string{"engineer"}},
		{Roles: []string{""}},
	}
	for _, identity := range identityRefusals {
		if err := identity.Validate(); err == nil {
			t.Errorf("Identity.Validate accepted %+v", identity)
		}
	}
	requestRefusals := []git.AccessRequest{
		{Identity: humanIdentity("engineer"), Ref: "", Update: git.UpdateRead},
		{Identity: humanIdentity("engineer"), Ref: "heads/main", Update: git.UpdateRead},
		{Identity: humanIdentity("engineer"), Ref: "refs/heads/main", Update: git.UpdateKind("force")},
		{Identity: git.Identity{}, Ref: "refs/heads/main", Update: git.UpdateRead},
	}
	for _, request := range requestRefusals {
		if err := request.Validate(); err == nil {
			t.Errorf("AccessRequest.Validate accepted %+v", request)
		}
	}
}

// TestGitJournalFormatAndRecovery pins the durable receive journal: a versioned
// format, crash-before-effect versus lost-response-after-effect reconciliation,
// and reporting only after a durably recorded acceptance.
func TestGitJournalFormatAndRecovery(t *testing.T) {
	if git.JournalVersion != 1 {
		t.Fatalf("JournalVersion = %d; want 1", git.JournalVersion)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := git.JournalEntry{
		Version:        git.JournalVersion,
		ResourceID:     "res-1",
		Repository:     "shop/web",
		Ref:            "refs/heads/main",
		ExpectedBase:   "base-1",
		Candidate:      "cand-1",
		SourceEventID:  "git-event-1",
		VerificationID: "ver-1",
		State:          git.JournalIntended,
		At:             at,
	}
	if err := entry.Validate(); err != nil {
		t.Fatalf("valid journal entry refused: %v", err)
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal journal entry: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("unmarshal journal entry: %v", err)
	}
	for _, key := range []string{"version", "resource_id", "repository", "ref", "expected_base", "candidate", "source_event_id", "verification_id", "state", "at"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("journal JSON is missing %q: %s", key, encoded)
		}
	}
	var roundTripped git.JournalEntry
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal journal round trip: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, entry) {
		t.Fatalf("journal round trip = %+v; want %+v", roundTripped, entry)
	}
	assertNoFieldMatching(t, reflect.TypeOf(git.JournalEntry{}), "credential", "token", "password", "secret")

	invalid := []git.JournalEntry{
		func() git.JournalEntry { value := entry; value.Version = 0; return value }(),
		func() git.JournalEntry { value := entry; value.Version = 2; return value }(),
		func() git.JournalEntry { value := entry; value.Repository = ""; return value }(),
		func() git.JournalEntry { value := entry; value.Ref = ""; return value }(),
		func() git.JournalEntry { value := entry; value.Candidate = ""; return value }(),
		func() git.JournalEntry { value := entry; value.SourceEventID = ""; return value }(),
		func() git.JournalEntry { value := entry; value.State = "done"; return value }(),
		func() git.JournalEntry { value := entry; value.At = time.Time{}; return value }(),
		func() git.JournalEntry { value := entry; value.ResourceID = ""; return value }(),
	}
	for _, candidate := range invalid {
		if err := candidate.Validate(); err == nil {
			t.Errorf("JournalEntry.Validate accepted %+v", candidate)
		}
	}

	// Crash before the ref update: the ref still equals the expected base.
	outcome, err := entry.Reconcile(git.RefState{Ref: entry.Ref, Commit: entry.ExpectedBase})
	if err != nil {
		t.Fatalf("Reconcile(not applied): %v", err)
	}
	if outcome != git.RecoveryNotApplied {
		t.Fatalf("Reconcile(base) = %q; want not_applied", outcome)
	}
	// Lost response after the effect: the ref already equals the candidate.
	outcome, err = entry.Reconcile(git.RefState{Ref: entry.Ref, Commit: entry.Candidate})
	if err != nil {
		t.Fatalf("Reconcile(applied): %v", err)
	}
	if outcome != git.RecoveryApplied {
		t.Fatalf("Reconcile(candidate) = %q; want applied", outcome)
	}
	// A third commit is neither our effect nor a proven non-effect.
	outcome, err = entry.Reconcile(git.RefState{Ref: entry.Ref, Commit: "someone-else"})
	if err != nil {
		t.Fatalf("Reconcile(unknown): %v", err)
	}
	if outcome != git.RecoveryUnknown {
		t.Fatalf("Reconcile(third commit) = %q; want unknown", outcome)
	}
	if _, err := entry.Reconcile(git.RefState{Ref: "refs/heads/other", Commit: entry.Candidate}); err == nil {
		t.Fatal("Reconcile accepted an observation of a different ref")
	}
	accepted := entry
	accepted.State = git.JournalAccepted
	if _, err := accepted.Reconcile(git.RefState{Ref: entry.Ref, Commit: entry.Candidate}); err == nil {
		t.Fatal("Reconcile accepted an already accepted entry")
	}
	create := entry
	create.ExpectedBase = ""
	if outcome, err := create.Reconcile(git.RefState{Ref: create.Ref, Commit: ""}); err != nil || outcome != git.RecoveryNotApplied {
		t.Fatalf("Reconcile(create not applied) = %q,%v; want not_applied", outcome, err)
	}
	if outcome, err := create.Reconcile(git.RefState{Ref: create.Ref, Commit: create.Candidate}); err != nil || outcome != git.RecoveryApplied {
		t.Fatalf("Reconcile(create applied) = %q,%v; want applied", outcome, err)
	}

	// Reporting is allowed only after a durably recorded acceptance, so an
	// interrupted report retries reporting, never the push.
	for _, state := range []git.JournalState{git.JournalIntended, git.JournalUncertain, git.JournalRefused, git.JournalReported} {
		candidate := entry
		candidate.State = state
		if candidate.Reportable() {
			t.Errorf("Reportable() = true for state %q", state)
		}
	}
	if !accepted.Reportable() {
		t.Fatal("Reportable() = false for an accepted entry")
	}

	report, err := accepted.ReportRequest()
	if err != nil {
		t.Fatalf("ReportRequest: %v", err)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("built report request refused: %v", err)
	}
	if report.ResourceID != entry.ResourceID || report.SourceEventID != entry.SourceEventID ||
		report.VerificationID != entry.VerificationID || report.EventType != "git.ref.updated" ||
		report.Operation != "git.ref.update" {
		t.Fatalf("report request = %+v; want the journal identity and git.ref.update", report)
	}
	wantFields := map[string]string{
		"operation":     "git.ref.update",
		"resource":      "res-1",
		"ref":           "refs/heads/main",
		"expected_base": "base-1",
		"candidate":     "cand-1",
	}
	if len(report.Subject.Fields) != len(wantFields) {
		t.Fatalf("report subject fields = %+v; want %v", report.Subject.Fields, wantFields)
	}
	for _, field := range report.Subject.Fields {
		if field.Kind != "string" {
			t.Errorf("report subject field %q kind = %q; want string", field.Name, field.Kind)
		}
		if field.Name == "repository" {
			t.Error("report subject uses the repository alias; the canonical field is resource")
		}
		want, ok := wantFields[field.Name]
		if !ok || field.Value != want {
			t.Errorf("report subject field %q = %q; want %q", field.Name, field.Value, want)
		}
	}
	again, err := accepted.ReportRequest()
	if err != nil {
		t.Fatalf("ReportRequest retry: %v", err)
	}
	if !reflect.DeepEqual(report, again) {
		t.Fatalf("report retry changed identity/content: %+v vs %+v", report, again)
	}
	unaccepted := entry
	if _, err := unaccepted.ReportRequest(); err == nil {
		t.Fatal("ReportRequest accepted an entry that was never durably accepted")
	}
}

// fakeReadAuthorizer is the compile-only smart-HTTP read seam fake. It resolves
// the local policy and never contacts a verification client.
type fakeReadAuthorizer struct {
	config git.Config
}

func (authorizer fakeReadAuthorizer) AuthorizeRead(_ context.Context, repository string, identity git.Identity) (git.Decision, error) {
	policy, ok := authorizer.config.Policy(repository)
	if !ok {
		return git.DecisionDeny, errors.New("repository policy is missing")
	}
	return policy.AuthorizeRead(git.ReadRequest{Identity: identity})
}

var _ git.ReadAuthorizer = fakeReadAuthorizer{}

// fakeGitExecutor, fakeRefObserver, fakeJournalStore, fakeReportClient,
// fakeRetentionQuery and fakeClock are compile-only seam fakes.
type fakeGitExecutor struct {
	commands []git.Command
}

func (executor *fakeGitExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	executor.commands = append(executor.commands, command)
	return git.CommandResult{ExitCode: 0, Stdout: []byte("ok")}, nil
}

var _ git.CommandExecutor = (*fakeGitExecutor)(nil)

type fakeRefObserver struct {
	state git.RefState
}

func (observer *fakeRefObserver) Observe(_ context.Context, _, _ string) (git.RefState, error) {
	return observer.state, nil
}

var _ git.RefObserver = (*fakeRefObserver)(nil)

type fakeJournalStore struct {
	entries []git.JournalEntry
}

func (store *fakeJournalStore) Load(_ context.Context, key string) (git.JournalEntry, bool, error) {
	for _, entry := range store.entries {
		if entry.SourceEventID == key {
			return entry, true, nil
		}
	}
	return git.JournalEntry{}, false, nil
}

func (store *fakeJournalStore) Record(_ context.Context, entry git.JournalEntry) error {
	store.entries = append(store.entries, entry)
	return nil
}

var _ git.JournalStore = (*fakeJournalStore)(nil)

type fakeReportClient struct {
	reports []api.ReportRequest
}

func (client *fakeReportClient) Verify(_ context.Context, request api.VerifyRequest) (api.VerifyResponse, error) {
	return api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "prn-1", Kind: "human"},
		Approval:       true,
		VerificationID: "ver-1",
	}, nil
}

func (client *fakeReportClient) Report(_ context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	client.reports = append(client.reports, request)
	return api.ReportResponse{VerificationID: request.VerificationID, EventID: "evt-1", Replayed: len(client.reports) > 1}, nil
}

var _ git.VerificationClient = (*fakeReportClient)(nil)

type fakeRetentionQuery struct {
	commits []string
}

func (query *fakeRetentionQuery) ReferencedCommits(_ context.Context, page api.Page) (git.CommitRefs, error) {
	return git.CommitRefs{Commits: query.commits, Next: api.PageInfo{}}, nil
}

var _ git.RetentionQuery = (*fakeRetentionQuery)(nil)

type fakeClock struct {
	now time.Time
}

func (clock fakeClock) Now() time.Time { return clock.now }

var _ git.Clock = fakeClock{}

// TestGitSeamsAndReportingRetry proves the service seams are usable without a
// real Git binary, transport or database, and that an interrupted report
// repeats the same report identity/content instead of re-pushing.
func TestGitSeamsAndReportingRetry(t *testing.T) {
	executor := &fakeGitExecutor{}
	if _, err := executor.Run(context.Background(), git.Command{Name: "git", Args: []string{"receive-pack", "shop/web"}}); err != nil {
		t.Fatalf("fake executor: %v", err)
	}
	if len(executor.commands) != 1 || executor.commands[0].Name != "git" {
		t.Fatalf("executor commands = %+v", executor.commands)
	}

	observer := &fakeRefObserver{state: git.RefState{Ref: "refs/heads/main", Commit: "cand-1"}}
	state, err := observer.Observe(context.Background(), "shop/web", "refs/heads/main")
	if err != nil || state.Commit != "cand-1" {
		t.Fatalf("fake observer = %+v,%v", state, err)
	}

	store := &fakeJournalStore{}
	entry := git.JournalEntry{
		Version:        git.JournalVersion,
		ResourceID:     "res-1",
		Repository:     "shop/web",
		Ref:            "refs/heads/main",
		ExpectedBase:   "base-1",
		Candidate:      "cand-1",
		SourceEventID:  "git-event-1",
		VerificationID: "ver-1",
		State:          git.JournalAccepted,
		At:             fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}.Now(),
	}
	if err := store.Record(context.Background(), entry); err != nil {
		t.Fatalf("fake store: %v", err)
	}
	loaded, ok, err := store.Load(context.Background(), "git-event-1")
	if err != nil || !ok || !reflect.DeepEqual(loaded, entry) {
		t.Fatalf("fake store load = %+v,%v,%v", loaded, ok, err)
	}

	reportClient := &fakeReportClient{}
	report, err := loaded.ReportRequest()
	if err != nil {
		t.Fatalf("ReportRequest: %v", err)
	}
	first, err := reportClient.Report(context.Background(), report)
	if err != nil {
		t.Fatalf("first report: %v", err)
	}
	second, err := reportClient.Report(context.Background(), report)
	if err != nil {
		t.Fatalf("retry report: %v", err)
	}
	if first.Replayed || !second.Replayed {
		t.Fatalf("report replay flags = %v/%v; want false/true", first.Replayed, second.Replayed)
	}
	if len(reportClient.reports) != 2 || !reflect.DeepEqual(reportClient.reports[0], reportClient.reports[1]) {
		t.Fatalf("report retry changed content: %+v", reportClient.reports)
	}

	query := &fakeRetentionQuery{commits: []string{"base-1", "cand-1"}}
	refs, err := query.ReferencedCommits(context.Background(), api.Page{Limit: 10})
	if err != nil || !reflect.DeepEqual(refs.Commits, []string{"base-1", "cand-1"}) {
		t.Fatalf("retention query = %+v,%v", refs, err)
	}

	// No seam exposes a credential or a repository-config write path.
	if strings.Contains(strings.ToLower(strings.Join(commandNames(executor.commands), " ")), "token") {
		t.Fatal("executor command carries a token")
	}
}

func commandNames(commands []git.Command) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Name)
	}
	return names
}
