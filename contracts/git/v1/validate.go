package gitv1

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

func checkNonNil[T any](path string, list []T) error {
	if list == nil {
		return fail(path, "must be a list (an empty list is [], not null)")
	}
	return nil
}

func ttl(path string, issued, expires Timestamp, max time.Duration) error {
	if err := firstError(checkTimestamp(path+".issuedAt", issued), checkTimestamp(path+".expiresAt", expires)); err != nil {
		return err
	}
	i, _ := issued.Time()
	e, _ := expires.Time()
	if !e.After(i) {
		return fail(path+".expiresAt", "must be after issuedAt")
	}
	if e.Sub(i) > max {
		return fail(path+".expiresAt", "lifetime exceeds %s", max)
	}
	return nil
}

// ---- Repository ----

// EffectivePolicy resolves a create-time policy: an omitted policy, or an omitted
// directPushToDefault, is false (O-33).
func EffectivePolicy(in *RepositoryPolicyInput) RepositoryPolicy {
	if in == nil || in.DirectPushToDefault == nil {
		return RepositoryPolicy{}
	}
	return RepositoryPolicy{DirectPushToDefault: *in.DirectPushToDefault}
}

// EffectiveDefaultBranch is the requested default branch or DefaultBranchName.
func (r CreateRepositoryRequest) EffectiveDefaultBranch() string {
	if r.DefaultBranch == nil {
		return DefaultBranchName
	}
	return *r.DefaultBranch
}

func (p *RepositoryPolicyInput) validate(path string, required bool) error {
	if p != nil && required && p.DirectPushToDefault == nil {
		return fail(path+".directPushToDefault", "is required when a policy is patched")
	}
	return nil
}

func (r Repository) Validate() error {
	if err := firstError(checkID("id", r.ID), matches("name", "RepositoryName", r.Name), ValidateBranchName(r.DefaultBranch),
		oneOf("hosting", string(r.Hosting), string(HostingInternal), string(HostingExternal)),
		oneOf("status", string(r.Status), string(StatusProvisioning), string(StatusReady), string(StatusArchived)),
		matches("cloneUrl", "RepositoryURL", r.CloneURL), inRange("version", r.Version, 1, MaxSafeInteger), checkTimestamp("createdAt", r.CreatedAt)); err != nil {
		return err
	}
	if r.Reason != nil {
		if err := checkCode("reason", *r.Reason); err != nil {
			return err
		}
		if r.Status == StatusReady {
			return fail("reason", "a ready repository has no reason")
		}
	}
	return nil
}

func (r CreateRepositoryRequest) Validate() error {
	if err := matches("name", "RepositoryName", r.Name); err != nil {
		return err
	}
	if r.DefaultBranch != nil {
		if err := ValidateBranchName(*r.DefaultBranch); err != nil {
			return err
		}
	}
	return r.Policy.validate("policy", false)
}

func (r PatchRepositoryRequest) Validate() error {
	if r.DefaultBranch == nil && r.Policy == nil {
		return fail("", "a patch changes defaultBranch or policy")
	}
	if r.DefaultBranch != nil {
		if err := ValidateBranchName(*r.DefaultBranch); err != nil {
			return err
		}
	}
	return r.Policy.validate("policy", true)
}

func (l RepositoryList) Validate() error {
	if err := checkNonNil("items", l.Items); err != nil {
		return err
	}
	if len(l.Items) > MaxPage {
		return fail("items", "lists more than %d entries", MaxPage)
	}
	for i, r := range l.Items {
		if err := r.Validate(); err != nil {
			return wrap(indexed("items", i), err)
		}
	}
	return checkOptionalCursor("nextCursor", l.NextCursor)
}

func checkOptionalCursor(path string, c *string) error {
	if c == nil {
		return nil
	}
	return matches(path, "Cursor", *c)
}

func (t RefTip) Validate() error {
	if err := firstError(ValidateRefName(t.Ref), checkObjectID("commit", t.Commit)); err != nil {
		return err
	}
	if IsZeroObjectID(t.Commit) {
		return fail("commit", "a tip is never the all-zero ID")
	}
	return nil
}

func (r RefsResponse) Validate() error {
	if err := checkNonNil("refs", r.Refs); err != nil {
		return err
	}
	if len(r.Refs) > MaxPage {
		return fail("refs", "lists more than %d refs", MaxPage)
	}
	for i, t := range r.Refs {
		if err := t.Validate(); err != nil {
			return wrap(indexed("refs", i), err)
		}
		if i > 0 && r.Refs[i-1].Ref >= t.Ref {
			return fail(indexed("refs", i), "refs must be strictly ordered by name")
		}
	}
	if r.Empty && len(r.Refs) > 0 {
		return fail("empty", "a repository that lists refs is not empty")
	}
	return checkOptionalCursor("nextCursor", r.NextCursor)
}

// ---- Actor, grants, credentials ----

func (a Actor) Validate() error {
	if err := firstError(oneOf("type", string(a.Type), string(ActorUser), string(ActorWorkload)), checkID("id", a.ID),
		checkOptionalID("runId", a.RunID), checkOptionalID("attemptId", a.AttemptID)); err != nil {
		return err
	}
	switch a.Type {
	case ActorUser:
		if a.RunID != nil || a.AttemptID != nil {
			return fail("actor", "a user has no run or attempt")
		}
	case ActorWorkload:
		if a.RunID == nil {
			return fail("runId", "a workload belongs to a run")
		}
	}
	return nil
}

func (c IntegrationConstraint) Validate() error {
	if err := firstError(ValidateBranchRef(c.Ref), checkObjectID("from", c.From), checkObjectID("to", c.To), checkID("gateId", c.GateID)); err != nil {
		return err
	}
	if len(c.From) != len(c.To) || c.From == c.To {
		return fail("integration", "from and to must be different object IDs of one length")
	}
	if IsZeroObjectID(c.From) || IsZeroObjectID(c.To) {
		return fail("integration", "an integration advances an existing branch to an existing commit")
	}
	return nil
}

// HumanUpdateCeiling is the recorded update set of a human grant that may push:
// the maximum class set. It is not a frozen permission: the person's current
// project roles and the ref policy restrict every update at use time, refs/kodama
// is never matched, and a read-only person's grant records [] instead.
func HumanUpdateCeiling() []string { return []string{HeadsPrefix + "*", TagsPrefix + "*"} }

func isHumanCeiling(update []string) bool {
	c := HumanUpdateCeiling()
	return len(update) == len(c) && update[0] == c[0] && update[1] == c[1]
}

func (g GrantRepository) validate(path string, human bool) error {
	patterns := checkRefPatterns(path+".update", g.Update, MaxGrantPatterns)
	if human {
		patterns = nil
		if len(g.Update) != 0 && !isHumanCeiling(g.Update) {
			patterns = fail(path+".update", "a human grant records [] or exactly %s", strings.Join(HumanUpdateCeiling(), ", "))
		}
	}
	if err := firstError(checkID(path+".id", g.ID), checkNonNil(path+".update", g.Update), patterns); err != nil {
		return err
	}
	if !g.Read {
		return fail(path+".read", "a grant names a repository to read it")
	}
	if g.Integration != nil {
		return wrap(path+".integration", g.Integration.Validate())
	}
	return nil
}

// Validate checks the claims structurally and against the rules of their kind.
// A seed grant's update must be creating one branch; use CheckSeed to compare it
// with the repository's default branch.
func (g GrantClaims) Validate() error {
	if err := firstError(checkID("grantId", g.GrantID), checkID("tenantId", g.TenantID), checkID("projectId", g.ProjectID), g.Actor.Validate(),
		oneOf("kind", string(g.Kind), string(GrantHuman), string(GrantSeed), string(GrantPreparation), string(GrantAttempt), string(GrantIntegration)),
		checkNonNil("repositories", g.Repositories)); err != nil {
		return err
	}
	if g.Audience != GrantAudience {
		return fail("audience", "must be %s", GrantAudience)
	}
	if len(g.Repositories) > MaxGrantRepositories {
		return fail("repositories", "lists more than %d repositories", MaxGrantRepositories)
	}
	seen := map[string]bool{}
	for i, r := range g.Repositories {
		if err := r.validate(indexed("repositories", i), g.Kind == GrantHuman); err != nil {
			return err
		}
		if seen[r.ID] {
			return fail(indexed("repositories", i), "repeats repository %q", truncate(r.ID, 64))
		}
		seen[r.ID] = true
	}
	return g.validateKind()
}

func (g GrantClaims) validateKind() error {
	max := MaxWorkloadGrantLifetime
	switch g.Kind {
	case GrantHuman:
		max = MaxHumanGrantLifetime
	case GrantSeed:
		max = MaxSeedLifetime
	}
	if err := ttl("grant", g.IssuedAt, g.ExpiresAt, max); err != nil {
		return err
	}
	if len(g.Repositories) == 0 {
		return fail("repositories", "a %s grant names at least one repository", g.Kind)
	}
	for i, r := range g.Repositories {
		p := indexed("repositories", i)
		if (r.Integration != nil) != (g.Kind == GrantIntegration) {
			return fail(p+".integration", "is set exactly on the repository of an integration grant")
		}
		if r.Seed != (g.Kind == GrantSeed) {
			return fail(p+".seed", "is set exactly on the repository of a seed grant")
		}
	}
	switch g.Kind {
	case GrantHuman:
		if g.Actor.Type != ActorUser {
			return fail("actor", "a human grant belongs to a user")
		}
		if len(g.Repositories) != 1 {
			return fail("repositories", "a human credential is for exactly one project and repository")
		}
	case GrantSeed:
		return g.validateSeed()
	case GrantPreparation:
		if g.Actor.Type != ActorWorkload || g.Actor.AttemptID != nil {
			return fail("actor", "a preparation grant belongs to a run's preparation workload, which has no attempt")
		}
		for i, r := range g.Repositories {
			if len(r.Update) != 0 {
				return fail(indexed("repositories", i)+".update", "a preparation grant is read-only")
			}
		}
	case GrantAttempt:
		if g.Actor.Type != ActorWorkload || g.Actor.AttemptID == nil {
			return fail("actor", "an attempt grant belongs to one attempt")
		}
	case GrantIntegration:
		if g.Actor.Type != ActorWorkload || g.Actor.AttemptID == nil {
			return fail("actor", "an integration grant belongs to one attempt")
		}
		if len(g.Repositories) != 1 {
			return fail("repositories", "an integration grant names exactly one repository")
		}
		r := g.Repositories[0]
		if len(r.Update) != 1 || r.Update[0] != r.Integration.Ref {
			return fail("repositories[0].update", "an integration grant constrains exactly one update: the integration ref itself")
		}
	}
	return nil
}

func (g GrantClaims) validateSeed() error {
	if g.Actor.Type == ActorWorkload && g.Actor.AttemptID == nil {
		return fail("actor", "a seed grant to a workload belongs to the provisioning run's attempt")
	}
	if len(g.Repositories) != 1 {
		return fail("repositories", "a seed grant names exactly one repository")
	}
	r := g.Repositories[0]
	if len(r.Update) != 1 {
		return fail("repositories[0].update", "a seed grant updates exactly one ref: the default branch it creates")
	}
	branch, ok := strings.CutPrefix(r.Update[0], HeadsPrefix)
	if !ok || strings.HasSuffix(r.Update[0], "*") || ValidateBranchName(branch) != nil {
		return fail("repositories[0].update", "a seed grant creates exactly refs/heads/<defaultBranch>, never a pattern or another namespace")
	}
	return nil
}

// CheckSeed checks that a seed grant may create exactly refs/heads/<defaultBranch>
// of its one repository, the repository's current default branch.
func (g GrantClaims) CheckSeed(defaultBranch string) error {
	if err := g.Validate(); err != nil {
		return err
	}
	if g.Kind != GrantSeed {
		return fail("kind", "not a seed grant")
	}
	if err := ValidateBranchName(defaultBranch); err != nil {
		return err
	}
	if g.Repositories[0].Update[0] != HeadsPrefix+defaultBranch {
		return fail("repositories[0].update", "must be exactly refs/heads/%s", defaultBranch)
	}
	return nil
}

// CheckSeedEligible is the pure admission rule of a seed grant (S8): the
// repository must be internal, ready and empty and have no unsettled push record
// of any kind (git-repositories.md, "Bootstrap by push"): see UnsettledPush,
// which counts a receive still in flight and an unknown outcome alike. Refusals,
// in order, are hosting_mismatch, repository_archived, repository_not_ready,
// repository_not_empty and seed_pending. The caller separately requires the
// engineer role (403) and the repository's existence in the caller's project.
func CheckSeedEligible(repo Repository, unsettledPush bool) error {
	refuse := func(code domain.ErrorCode, msg string) error { return &ContractError{Code: code, Message: msg} }
	switch {
	case repo.Hosting != HostingInternal:
		return refuse(CodeHostingMismatch, "only an internally hosted repository can be seeded")
	case repo.Status == StatusArchived:
		return refuse(CodeRepositoryArchived, "the repository is archived")
	case repo.Status != StatusReady:
		return refuse(CodeRepositoryNotReady, "the repository is not ready")
	case !repo.Empty:
		return refuse(CodeRepositoryNotEmpty, "the repository already has refs")
	case unsettledPush:
		return refuse(CodeSeedPending, "an earlier push is unsettled")
	}
	return nil
}

// UnsettledPush reports whether any of a repository's push records is
// unsettled: its status is authorized (an entry is still authorized or
// committing, whatever its other entries say) or unknown. A settled record
// (applied or refused) does not count. It is the input of CheckSeedEligible.
func UnsettledPush(records []PushRecord) bool {
	for _, r := range records {
		if st := DeriveStatus(r.Entries); st == PushAuthorized || st == PushUnknown {
			return true
		}
	}
	return false
}

func (r CredentialRequest) Validate() error {
	return firstError(checkID("projectId", r.ProjectID), checkID("repositoryId", r.RepositoryID))
}

func (r CredentialResponse) Validate() error {
	return firstError(matches("username", "Username", r.Username), matches("password", "Credential", r.Password), checkID("grantId", r.GrantID),
		checkTimestamp("expiresAt", r.ExpiresAt))
}

// ---- Forwarding assertion and worker identity ----

// ClockSkew is the tolerance a verifier applies to issuedAt.
const ClockSkew = 30 * time.Second

func (a ForwardingAssertion) Validate() error {
	if err := firstError(checkID("tenantId", a.TenantID), checkID("repositoryId", a.RepositoryID), checkID("grantId", a.GrantID),
		checkID("requestId", a.RequestID), ttl("assertion", a.IssuedAt, a.ExpiresAt, MaxForwardingLifetime)); err != nil {
		return err
	}
	if a.Audience != WorkerAudience(a.TenantID) {
		return fail("audience", "must be %s<tenantId>", WorkerAudiencePrefix)
	}
	return nil
}

func (w WorkerIdentity) Validate() error {
	if err := firstError(checkID("tenantId", w.TenantID), checkID("workerInstance", w.WorkerInstance), ttl("identity", w.IssuedAt, w.ExpiresAt, MaxWorkerIdentityLifetime)); err != nil {
		return err
	}
	if w.Audience != ControlAudience {
		return fail("audience", "must be %s", ControlAudience)
	}
	return nil
}

func checkWindow(issued, expires Timestamp, now time.Time) error {
	i, _ := issued.Time()
	e, _ := expires.Time()
	if now.Add(ClockSkew).Before(i) || !now.Before(e) {
		return &ContractError{Code: domain.CodeInvalidCredential, Message: "the credential is not valid at this time"}
	}
	return nil
}

// CheckForwarding is the pure verification of a forwarding assertion's claims by
// the worker of workerTenant (GR-03, GR-05): a malformed or expired assertion is
// invalid_credential, an assertion for another tenant's worker is forbidden. It
// does not verify a signature.
func CheckForwarding(a ForwardingAssertion, workerTenant string, now time.Time) error {
	if err := a.Validate(); err != nil {
		return &ContractError{Code: domain.CodeInvalidCredential, Message: err.Error()}
	}
	if a.TenantID != workerTenant || a.Audience != WorkerAudience(workerTenant) {
		return &ContractError{Code: domain.CodeForbidden, Message: "the assertion is for another tenant's worker"}
	}
	return checkWindow(a.IssuedAt, a.ExpiresAt, now)
}

// CheckService reports whether the assertion permits the Git service: upload-pack
// always, receive-pack only when ReceivePack is set; anything else is forbidden.
func (a ForwardingAssertion) CheckService(service string) error {
	switch {
	case service == "git-upload-pack", service == "git-receive-pack" && a.ReceivePack:
		return nil
	}
	return &ContractError{Code: domain.CodeForbidden, Message: "the assertion does not permit this service"}
}

// CheckWorkerCall is the pure verification of a worker identity on a route of
// tenant pathTenant: a malformed or expired identity is invalid_credential and a
// worker calling another tenant's route is forbidden.
func CheckWorkerCall(w WorkerIdentity, pathTenant string, now time.Time) error {
	if err := w.Validate(); err != nil {
		return &ContractError{Code: domain.CodeInvalidCredential, Message: err.Error()}
	}
	if w.TenantID != pathTenant {
		return &ContractError{Code: domain.CodeForbidden, Message: "the worker identity is bound to another tenant"}
	}
	return checkWindow(w.IssuedAt, w.ExpiresAt, now)
}

// ---- Push authorization, commit intent, outcome ----

func (u RefUpdate) Validate() error {
	if err := firstError(ValidateRefName(u.Ref), checkObjectID("old", u.Old), checkObjectID("new", u.New)); err != nil {
		return err
	}
	if len(u.Old) != len(u.New) || u.Old == u.New {
		return fail("update", "old and new must be different object IDs of one length")
	}
	return nil
}

func validateUpdates(path string, updates []RefUpdate, min int) error {
	if err := checkNonNil(path, updates); err != nil {
		return err
	}
	if len(updates) < min || len(updates) > MaxPushUpdates {
		return fail(path, "must list %d to %d updates", min, MaxPushUpdates)
	}
	seen := map[string]bool{}
	for i, u := range updates {
		if err := u.Validate(); err != nil {
			return wrap(indexed(path, i), err)
		}
		if seen[u.Ref] {
			return fail(indexed(path, i), "repeats ref %q", truncate(u.Ref, 64))
		}
		seen[u.Ref] = true
	}
	return nil
}

func (f GitFact) Validate() error { return ValidateRefName(f.Ref) }

func (r PushAuthorizeRequest) Validate() error {
	if err := firstError(checkID("repositoryId", r.RepositoryID), checkID("grantId", r.GrantID), checkID("receiveId", r.ReceiveID),
		validateUpdates("updates", r.Updates, 1), checkNonNil("pushOptions", r.PushOptions), checkNonNil("gitFacts", r.GitFacts)); err != nil {
		return err
	}
	if len(r.PushOptions) > MaxPushOptions {
		return fail("pushOptions", "lists more than %d options", MaxPushOptions)
	}
	for i, o := range r.PushOptions {
		if len(o) > MaxPushOptionBytes || !utf8.ValidString(o) || matches(indexed("pushOptions", i), "PushOption", o) != nil {
			return fail(indexed("pushOptions", i), "must be at most %d bytes of text without control characters", MaxPushOptionBytes)
		}
	}
	if len(r.GitFacts) != len(r.Updates) {
		return fail("gitFacts", "must hold exactly one fact per update, in order")
	}
	for i, f := range r.GitFacts {
		p := indexed("gitFacts", i)
		u := r.Updates[i]
		if err := f.Validate(); err != nil {
			return wrap(p, err)
		}
		if f.Ref != u.Ref {
			return fail(p+".ref", "must be the ref of updates[%d]", i)
		}
		if f.RepositoryEmpty != r.GitFacts[0].RepositoryEmpty {
			return fail(p+".repositoryEmpty", "must be the same in every fact")
		}
		if _, err := KindOf(u.Old, u.New, f.FastForward); err != nil {
			return wrap(p, err)
		}
		if f.FastForward != nil && (IsZeroObjectID(u.Old) || IsZeroObjectID(u.New)) {
			return fail(p+".fastForward", "is null for a create or delete")
		}
		if f.RepositoryEmpty && !IsZeroObjectID(u.Old) {
			return fail(p+".repositoryEmpty", "an empty repository has no ref to update: every update is a create")
		}
	}
	return nil
}

func (v UpdateVerdict) Validate() error {
	if err := firstError(ValidateRefName(v.Ref), oneOf("verdict", string(v.Verdict), string(VerdictAllow), string(VerdictDeny))); err != nil {
		return err
	}
	switch {
	case v.Verdict == VerdictAllow && v.Reason != nil:
		return fail("reason", "an allowed update has no reason")
	case v.Verdict == VerdictDeny && (v.Reason == nil || !IsPushRefusalReason(*v.Reason)):
		return fail("reason", "a denied update carries one of %s", strings.Join(pushRefusalReasons, ", "))
	}
	return nil
}

func (r PushAuthorizeResponse) Validate() error {
	if err := firstError(checkID("pushId", r.PushID), checkTimestamp("deadline", r.Deadline), checkNonNil("updates", r.Updates)); err != nil {
		return err
	}
	if len(r.Updates) < 1 || len(r.Updates) > MaxPushUpdates {
		return fail("updates", "must list 1 to %d verdicts", MaxPushUpdates)
	}
	seen := map[string]bool{}
	for i, v := range r.Updates {
		if err := v.Validate(); err != nil {
			return wrap(indexed("updates", i), err)
		}
		if seen[v.Ref] {
			return fail(indexed("updates", i), "repeats ref %q", truncate(v.Ref, 64))
		}
		seen[v.Ref] = true
	}
	return nil
}

// Allowed reports whether every update was allowed; a receive with any deny
// fails as a whole.
func (r PushAuthorizeResponse) Allowed() bool {
	for _, v := range r.Updates {
		if v.Verdict != VerdictAllow {
			return false
		}
	}
	return len(r.Updates) > 0
}

// CheckAuthorizeResponse checks that a response answers its request: one
// verdict per requested update, in order.
func CheckAuthorizeResponse(req PushAuthorizeRequest, resp PushAuthorizeResponse) error {
	if len(resp.Updates) != len(req.Updates) {
		return fail("updates", "must hold one verdict per requested update")
	}
	for i, v := range resp.Updates {
		if v.Ref != req.Updates[i].Ref {
			return fail(indexed("updates", i)+".ref", "must be the requested ref")
		}
	}
	return nil
}

func (r CommitIntentRequest) Validate() error {
	return firstError(checkID("receiveId", r.ReceiveID), validateUpdates("updates", r.Updates, 1))
}

func (r CommitIntentResponse) Validate() error {
	if !r.Committing {
		return fail("committing", "must be true (a refusal is a 409 error)")
	}
	return nil
}

func (r OutcomeRequest) Validate() error {
	if err := firstError(checkID("receiveId", r.ReceiveID), validateUpdates("applied", r.Applied, 0)); err != nil {
		return err
	}
	if r.JournalSequence != nil {
		if err := inRange("journalSequence", *r.JournalSequence, 1, MaxSafeInteger); err != nil {
			return err
		}
	}
	if r.JournalPosition != nil {
		return inRange("journalPosition", *r.JournalPosition, 1, MaxSafeInteger)
	}
	return nil
}

func (r OutcomeResponse) Validate() error {
	return firstError(checkID("pushId", r.PushID),
		oneOf("status", string(r.Status), string(PushAuthorized), string(PushApplied), string(PushRefused), string(PushUnknown)))
}

func (r InitializedRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), ValidateBranchName(r.DefaultBranch))
}

func (r InitializedResponse) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID),
		oneOf("status", string(r.Status), string(StatusProvisioning), string(StatusReady), string(StatusArchived)), inRange("version", r.Version, 1, MaxSafeInteger))
}

func (a Assignment) Validate() error {
	if err := firstError(oneOf("kind", string(a.Kind), string(AssignInitialize), string(AssignArchive), string(AssignMaintain), string(AssignReconcile)),
		checkID("repositoryId", a.RepositoryID), ValidateBranchName(a.DefaultBranch)); err != nil {
		return err
	}
	if a.JournalAck != nil {
		return inRange("journalAck", *a.JournalAck, 0, MaxSafeInteger)
	}
	return nil
}

func (r AssignmentsResponse) Validate() error {
	if err := checkNonNil("items", r.Items); err != nil {
		return err
	}
	if len(r.Items) > MaxAssignments {
		return fail("items", "lists more than %d assignments", MaxAssignments)
	}
	seen := map[[2]string]bool{}
	for i, a := range r.Items {
		if err := a.Validate(); err != nil {
			return wrap(indexed("items", i), err)
		}
		k := [2]string{string(a.Kind), a.RepositoryID}
		if seen[k] {
			return fail(indexed("items", i), "repeats an assignment")
		}
		seen[k] = true
	}
	return nil
}

// ---- Push records ----

func (e EntryState) valid() bool {
	switch e {
	case EntryAuthorized, EntryCommitting, EntryApplied, EntryRefused, EntryUnknown:
		return true
	}
	return false
}

// InFlight reports whether the state is neither final nor unknown: the entry
// waits for its receive.
func (e EntryState) InFlight() bool { return e == EntryAuthorized || e == EntryCommitting }

// DeriveStatus derives a push record's status from its entry states: unknown
// while any entry is unknown (an unresolved entry blocks the record, so its one
// git.refs_updated event is emitted exactly once, when it settles); authorized
// while any entry is still in flight; otherwise applied if any entry applied,
// else refused. A record with no entries is not valid and reads as authorized.
func DeriveStatus(entries []PushEntry) PushStatus {
	var inFlight, applied bool
	for _, e := range entries {
		switch {
		case e.State == EntryUnknown:
			return PushUnknown
		case e.State.InFlight():
			inFlight = true
		case e.State == EntryApplied:
			applied = true
		}
	}
	switch {
	case inFlight || len(entries) == 0:
		return PushAuthorized
	case applied:
		return PushApplied
	}
	return PushRefused
}

func (e PushEntry) Validate() error {
	if err := (RefUpdate{Ref: e.Ref, Old: e.Old, New: e.New}).Validate(); err != nil {
		return err
	}
	if !e.State.valid() {
		return fail("state", "%q is not a push entry state", truncate(string(e.State), 32))
	}
	if err := checkKind(e.Kind, e.Old, e.New); err != nil {
		return err
	}
	switch e.State {
	case EntryRefused:
		if e.Reason == nil || !IsRefusedEntryReason(*e.Reason) {
			return fail("reason", "a refused entry carries a push refusal or settlement refusal reason")
		}
	case EntryUnknown:
		if e.Reason == nil || !IsUnknownEntryReason(*e.Reason) {
			return fail("reason", "an unknown entry carries a visible unknown reason")
		}
	default:
		if e.Reason != nil {
			return fail("reason", "only a refused or unknown entry has a reason")
		}
	}
	return nil
}

func (r PushRecord) Validate() error {
	if err := firstError(checkID("pushId", r.PushID), checkID("repositoryId", r.RepositoryID), checkID("receiveId", r.ReceiveID), r.Actor.Validate(),
		checkID("grantId", r.GrantID), checkOptionalID("originRunId", r.OriginRunID),
		oneOf("status", string(r.Status), string(PushAuthorized), string(PushApplied), string(PushRefused), string(PushUnknown)),
		checkNonNil("entries", r.Entries), checkTimestamp("createdAt", r.CreatedAt)); err != nil {
		return err
	}
	switch {
	case r.Actor.Type == ActorWorkload && (r.OriginRunID == nil || *r.OriginRunID != *r.Actor.RunID):
		return fail("originRunId", "a run's push carries that run as its origin")
	case r.Actor.Type == ActorUser && r.OriginRunID != nil:
		return fail("originRunId", "a person's push has no origin run")
	}
	if len(r.Entries) < 1 || len(r.Entries) > MaxPushUpdates {
		return fail("entries", "must list 1 to %d entries", MaxPushUpdates)
	}
	seen := map[string]bool{}
	for i, e := range r.Entries {
		if err := e.Validate(); err != nil {
			return wrap(indexed("entries", i), err)
		}
		if seen[e.Ref] {
			return fail(indexed("entries", i), "repeats ref %q", truncate(e.Ref, 64))
		}
		seen[e.Ref] = true
	}
	if want := DeriveStatus(r.Entries); r.Status != want {
		return fail("status", "is %s but the entries derive %s", r.Status, want)
	}
	return nil
}

func (l PushRecordList) Validate() error {
	if err := checkNonNil("items", l.Items); err != nil {
		return err
	}
	if len(l.Items) > MaxPage {
		return fail("items", "lists more than %d records", MaxPage)
	}
	for i, r := range l.Items {
		if err := r.Validate(); err != nil {
			return wrap(indexed("items", i), err)
		}
	}
	return checkOptionalCursor("nextCursor", l.NextCursor)
}

// ---- Ref journal ----

func (e JournalEntry) Validate() error {
	if err := firstError(checkOptionalID("receiveId", e.ReceiveID), checkOptionalID("operationId", e.OperationID),
		RefUpdate{Ref: e.Ref, Old: e.Old, New: e.New}.Validate(),
		oneOf("state", string(e.State), string(JournalPrepared), string(JournalCommitted), string(JournalAborted)),
		inRange("sequence", e.Sequence, 1, MaxSafeInteger)); err != nil {
		return err
	}
	if (e.ReceiveID != nil) == (e.OperationID != nil) {
		return fail("receiveId", "exactly one of receiveId and operationId is set")
	}
	if e.State == JournalPrepared {
		if e.Observed == nil {
			return fail("observed", "a prepared entry records the value observed under the lock")
		}
		if err := checkObjectID("observed", *e.Observed); err != nil {
			return err
		}
		if len(*e.Observed) != len(e.Old) {
			return fail("observed", "must have the object ID length of old")
		}
	} else if e.Observed != nil {
		return fail("observed", "only a prepared entry has an observed value (an outcome never orders another transaction)")
	}
	return nil
}

// ---- Worker RPC ----

func (r InitializeRepositoryRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), ValidateBranchName(r.DefaultBranch))
}

func (r InitializeRepositoryResponse) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), ValidateBranchName(r.DefaultBranch))
}

// RefPrefixes are the namespaces ReadRefsRequest may list.
var RefPrefixes = []string{HeadsPrefix, TagsPrefix, ServerRefPrefix}

func (r ReadRefsRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), oneOf("prefix", r.Prefix, RefPrefixes...),
		checkOptionalCursor("cursor", r.Cursor), inRange("limit", r.Limit, 1, MaxPage))
}

func (r ReadRefValuesRequest) Validate() error {
	if err := firstError(checkID("repositoryId", r.RepositoryID), checkNonNil("refs", r.Refs)); err != nil {
		return err
	}
	if len(r.Refs) < 1 || len(r.Refs) > MaxRefValues {
		return fail("refs", "must list 1 to %d refs", MaxRefValues)
	}
	seen := map[string]bool{}
	for i, ref := range r.Refs {
		if err := ValidateRefName(ref); err != nil {
			return wrap(indexed("refs", i), err)
		}
		if seen[ref] {
			return fail(indexed("refs", i), "repeats a ref")
		}
		seen[ref] = true
	}
	return nil
}

func (v RefValue) Validate() error {
	if err := ValidateRefName(v.Ref); err != nil {
		return err
	}
	if v.Commit != nil {
		if err := checkObjectID("commit", *v.Commit); err != nil {
			return err
		}
		if IsZeroObjectID(*v.Commit) {
			return fail("commit", "an absent ref is null, not the all-zero ID")
		}
	}
	return nil
}

func (r ReadRefValuesResponse) Validate() error {
	if err := firstError(checkNonNil("values", r.Values), inRange("journalHead", r.JournalHead, 0, MaxSafeInteger), checkID("workerInstance", r.WorkerInstance)); err != nil {
		return err
	}
	if len(r.Values) > MaxRefValues {
		return fail("values", "lists more than %d refs", MaxRefValues)
	}
	seen := map[string]bool{}
	for i, v := range r.Values {
		if err := v.Validate(); err != nil {
			return wrap(indexed("values", i), err)
		}
		if seen[v.Ref] {
			return fail(indexed("values", i), "repeats a ref")
		}
		seen[v.Ref] = true
	}
	return nil
}

func (r ReadJournalRequest) Validate() error {
	if err := firstError(checkID("repositoryId", r.RepositoryID), checkOptionalID("receiveId", r.ReceiveID),
		inRange("sinceSequence", r.SinceSequence, 0, MaxSafeInteger), inRange("limit", r.Limit, 1, MaxPage)); err != nil {
		return err
	}
	if (r.ReceiveID != nil) == (r.Ref != nil) {
		return fail("receiveId", "exactly one of receiveId and ref selects the entries")
	}
	if r.Ref != nil {
		return ValidateRefName(*r.Ref)
	}
	return nil
}

func (r ReadJournalResponse) Validate() error {
	if err := firstError(checkNonNil("entries", r.Entries), inRange("headSequence", r.HeadSequence, 0, MaxSafeInteger), checkID("workerInstance", r.WorkerInstance)); err != nil {
		return err
	}
	if len(r.Entries) > MaxPage {
		return fail("entries", "lists more than %d entries", MaxPage)
	}
	var last int64
	for i, e := range r.Entries {
		if err := e.Validate(); err != nil {
			return wrap(indexed("entries", i), err)
		}
		if e.Sequence <= last {
			return fail(indexed("entries", i)+".sequence", "entries must be in strictly increasing sequence order")
		}
		if e.Sequence > r.HeadSequence {
			return fail(indexed("entries", i)+".sequence", "is beyond headSequence")
		}
		last = e.Sequence
	}
	return nil
}

func (r ReceiveLivenessResponse) Validate() error {
	return firstError(checkID("receiveId", r.ReceiveID), checkID("workerInstance", r.WorkerInstance))
}

func checkRetained(path, ref string) error {
	if _, _, err := ParseRetainedRef(ref); err != nil {
		return wrap(path, err)
	}
	return ValidateRefName(ref)
}

func checkLiveCommit(path, commit string) error {
	if err := checkObjectID(path, commit); err != nil {
		return err
	}
	if IsZeroObjectID(commit) {
		return fail(path, "must not be the all-zero ID")
	}
	return nil
}

func (r SetRetainedRefRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkID("operationId", r.OperationID), checkRetained("ref", r.Ref), checkLiveCommit("commit", r.Commit))
}

func (r SetRetainedRefResponse) Validate() error {
	return firstError(checkRetained("ref", r.Ref), checkLiveCommit("commit", r.Commit))
}

func (r DeleteRetainedRefRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkID("operationId", r.OperationID), checkRetained("ref", r.Ref),
		checkLiveCommit("expectedCommit", r.ExpectedCommit))
}

func (r DeleteRetainedRefResponse) Validate() error { return checkRetained("ref", r.Ref) }

// checkManagedBranch accepts only a task, agent or run branch: the branches the
// server may prune. The default branch name cannot live under refs/heads/kodama/,
// so any valid default branch gives the same answer.
func checkManagedBranch(path, ref string) error {
	if err := ValidateBranchRef(ref); err != nil {
		return wrap(path, err)
	}
	c, err := ClassifyRef(DefaultBranchName, ref)
	if err != nil {
		return wrap(path, err)
	}
	if c.Class != ClassTask && c.Class != ClassAgent && c.Class != ClassRun {
		return fail(path, "only a task, agent or run branch may be pruned")
	}
	return nil
}

func (r DeleteBranchRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkID("operationId", r.OperationID), checkManagedBranch("ref", r.Ref),
		checkLiveCommit("expectedCommit", r.ExpectedCommit))
}

func (r DeleteBranchResponse) Validate() error { return checkManagedBranch("ref", r.Ref) }

func (r MaintainRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkID("operationId", r.OperationID))
}

func (r MaintainResponse) Validate() error {
	return firstError(checkID("operationId", r.OperationID), checkTimestamp("completedAt", r.CompletedAt))
}

// ---- Trigger configuration ----

// Excludes reports whether a push made by a run is excluded from the trigger:
// true unless excludeOriginRuns was explicitly false.
func (t TriggerConfig) Excludes() bool { return t.ExcludeOriginRuns == nil || *t.ExcludeOriginRuns }

func (t TriggerConfig) Validate() error {
	if err := firstError(checkID("triggerId", t.TriggerID), checkID("repositoryId", t.RepositoryID), checkID("definitionId", t.DefinitionID),
		checkNonNil("refPatterns", t.RefPatterns), checkRefPatterns("refPatterns", t.RefPatterns, MaxTriggerPatterns)); err != nil {
		return err
	}
	if len(t.RefPatterns) == 0 {
		return fail("refPatterns", "a trigger names at least one ref pattern")
	}
	return nil
}

// Matches reports whether the trigger's patterns select ref.
func (t TriggerConfig) Matches(ref string) bool {
	for _, p := range t.RefPatterns {
		if MatchRefPattern(p, ref) {
			return true
		}
	}
	return false
}

func (r ErrorResponse) Validate() error {
	e := r.Error
	if _, ok := Status(e.Code); !ok {
		return fail("error.code", "unknown code %q", e.Code)
	}
	if e.Message == "" || len(e.Message) > 2048 {
		return fail("error.message", "must be 1-2048 bytes")
	}
	if e.RequestID == "" || len(e.RequestID) > 128 {
		return fail("error.requestId", "must be 1-128 bytes")
	}
	if e.Details == nil {
		return fail("error.details", "must be an object")
	}
	return nil
}

// ---- Verification helpers for consumers ----

// CheckTime is the pure validity-window check of verified grant claims: an
// expired grant, or one issued in the future beyond ClockSkew, is invalid_credential.
func (g GrantClaims) CheckTime(now time.Time) error { return checkWindow(g.IssuedAt, g.ExpiresAt, now) }

// CheckScope compares verified claims with the scope a request addresses. Any
// mismatch is not_found, so a foreign tenant, project or repository is
// indistinguishable from a missing one (GR-05). Every grant kind, a human
// credential included, names its project and repository and must match both.
func (g GrantClaims) CheckScope(tenantID, projectID, repositoryID string) error {
	notFound := &ContractError{Code: domain.CodeNotFound, Message: "not found"}
	if g.TenantID != tenantID || g.ProjectID != projectID {
		return notFound
	}
	for _, r := range g.Repositories {
		if r.ID == repositoryID {
			return nil
		}
	}
	return notFound
}

// PermitsUpdate reports whether the grant's named ref patterns permit u on the
// repository: for an integration grant exactly the constrained update (the
// integration ref, from the base to the approved commit), otherwise any update
// of a ref a pattern selects. It decides scope only; ref policy and current
// authority (roles, attempt liveness, writer reservation, seed consumption,
// direct-push policy) are decided by the push authorizer. A human grant's
// recorded update is the maximum class set (HumanUpdateCeiling): it permits
// every ordinary branch and tag here and never refs/kodama, and the person's
// current roles restrict it at every use.
func (g GrantClaims) PermitsUpdate(repositoryID string, u RefUpdate) bool {
	for _, r := range g.Repositories {
		if r.ID != repositoryID {
			continue
		}
		if r.Integration != nil {
			return u.Ref == r.Integration.Ref && u.Old == r.Integration.From && u.New == r.Integration.To
		}
		for _, p := range r.Update {
			if g.Kind == GrantHuman && isHumanCeiling(r.Update) {
				if prefix := strings.TrimSuffix(p, "*"); ValidateRefName(u.Ref) == nil && strings.HasPrefix(u.Ref, prefix) && len(u.Ref) > len(prefix) {
					return true
				}
				continue
			}
			if MatchRefPattern(p, u.Ref) {
				return true
			}
		}
	}
	return false
}

// ValidRepositoryTransition reports whether a repository may move from one
// status to another: provisioning to ready, ready to archived, archived to ready.
// A provisioning failure is a reason on a provisioning repository, never a
// transition; a repository never returns to provisioning.
func ValidRepositoryTransition(from, to RepositoryStatus) bool {
	switch {
	case from == StatusProvisioning && to == StatusReady,
		from == StatusReady && to == StatusArchived,
		from == StatusArchived && to == StatusReady:
		return true
	}
	return false
}

// CheckUpdateBound is the installation's explicit refusal of an oversized
// receive (GR-16): a receive of more than max updates, max being the
// installation's bound (at most MaxPushUpdates), is too_many_updates. The worker
// refuses before calling the control plane when it can; the control plane
// enforces it in Authorize.
func CheckUpdateBound(updates, max int) error {
	if max < 1 || max > MaxPushUpdates {
		max = MaxPushUpdates
	}
	if updates > max {
		return &ContractError{Code: CodeTooManyUpdates, Message: fmt.Sprintf("a receive may update at most %d refs; push the refs in batches", max)}
	}
	return nil
}

func (r CreateBranchRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkID("operationId", r.OperationID), checkManagedBranch("ref", r.Ref),
		checkLiveCommit("commit", r.Commit))
}

func (r CreateBranchResponse) Validate() error {
	return firstError(checkManagedBranch("ref", r.Ref), checkLiveCommit("commit", r.Commit))
}

// PinnedCommit is the exact commit a push-originated run pins (PX-24): the new
// value of the entry of ref in the applied push record. The record must be
// applied (every entry settled, at least one applied), the entry applied and not
// a delete. It never reads a branch: the branch's later movement is irrelevant,
// and the same push record and ref always give the same commit.
func PinnedCommit(record PushRecord, ref string) (string, error) {
	if err := record.Validate(); err != nil {
		return "", err
	}
	if record.Status != PushApplied {
		return "", fail("status", "a push-originated run references an applied push record, not %s", record.Status)
	}
	for _, e := range record.Entries {
		if e.Ref != ref {
			continue
		}
		if e.State != EntryApplied || IsZeroObjectID(e.New) {
			return "", fail("ref", "the entry of %q was not applied to a commit", truncate(ref, 64))
		}
		return e.New, nil
	}
	return "", fail("ref", "the push record has no entry for %q", truncate(ref, 64))
}

// checkKind requires kind to be consistent with the update: create exactly when
// old is all-zero, delete exactly when new is all-zero, fast_forward and rewrite
// only when both are non-zero.
func checkKind(kind UpdateKind, old, new string) error {
	zeroOld, zeroNew := IsZeroObjectID(old), IsZeroObjectID(new)
	switch kind {
	case KindCreate:
		if zeroOld {
			return nil
		}
		return fail("kind", "a create has an all-zero old value")
	case KindDelete:
		if zeroNew {
			return nil
		}
		return fail("kind", "a delete has an all-zero new value")
	case KindFastForward, KindRewrite:
		if !zeroOld && !zeroNew {
			return nil
		}
		return fail("kind", "%s updates an existing ref to an existing commit", kind)
	}
	return fail("kind", "%q is not create, fast_forward, rewrite or delete", truncate(string(kind), 32))
}

// NewPushEntries builds the authorized entries of a push record from a
// validated authorization request: one per update, in order, with the kind
// derived from the worker's git facts (KindOf), so it is persisted at
// authorization and never recomputed.
func NewPushEntries(req PushAuthorizeRequest) ([]PushEntry, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	out := make([]PushEntry, len(req.Updates))
	for i, u := range req.Updates {
		kind, err := KindOf(u.Old, u.New, req.GitFacts[i].FastForward)
		if err != nil {
			return nil, wrap(indexed("updates", i), err)
		}
		out[i] = PushEntry{Ref: u.Ref, Old: u.Old, New: u.New, Kind: kind, State: EntryAuthorized}
	}
	return out, nil
}

// KindCounts is the number of updates of each kind.
type KindCounts struct {
	Create      int64
	FastForward int64
	Rewrite     int64
	Delete      int64
}

// Total is the number of updates counted.
func (k KindCounts) Total() int64 { return k.Create + k.FastForward + k.Rewrite + k.Delete }

// AppliedKinds counts the applied entries of a record by kind: the counts of the
// git.refs_updated event's kinds summary, whose total is its updateCount.
func AppliedKinds(entries []PushEntry) KindCounts {
	var k KindCounts
	for _, e := range entries {
		if e.State != EntryApplied {
			continue
		}
		switch e.Kind {
		case KindCreate:
			k.Create++
		case KindFastForward:
			k.FastForward++
		case KindRewrite:
			k.Rewrite++
		case KindDelete:
			k.Delete++
		}
	}
	return k
}
