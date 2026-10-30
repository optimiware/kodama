package eventsv1

import (
	"encoding/json"
	"fmt"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// The internal Git service event types (docs/specs/git-repositories.md, "Push
// journal and events", and docs/contracts/git-service.md). Their aggregate is
// the repository (envelope resource type "repository", resource ID the
// repository ID); the project is required. They are additive: the envelope, the
// existing types and their digests are unchanged.
const (
	TypeRepositoryCreated      = "repository.created"
	TypeRepositoryStateChanged = "repository.state_changed"
	TypeGitRefsUpdated         = "git.refs_updated"

	// ResourceTypeRepository is the envelope resource type of repository.created
	// and repository.state_changed.
	ResourceTypeRepository = "repository"
	// ResourceTypeGitPush is the envelope resource type of git.refs_updated: its
	// aggregate is the push, which is its receive (resource ID the receiveId of
	// the payload, resource version the version at settlement). The repository's
	// own version is not the push's, and several pushes of one repository
	// interleave with its lifecycle events, so the repository cannot be the
	// aggregate.
	ResourceTypeGitPush = "git_push"
)

// Repository hosting values and status names (the git contract's).
const (
	RepositoryHostingInternal = string(gitv1.HostingInternal)
	RepositoryHostingExternal = string(gitv1.HostingExternal)
)

// RepositoryCreated is committed when the repository becomes ready: after the
// tenant's worker confirmed its initialization (provisioning to ready, spec
// "Provisioning" step 3), or when an externally hosted repository is
// registered. It is not emitted at reservation. It carries references only.
type RepositoryCreated struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"`
	Hosting       string `json:"hosting"`
}

// RepositoryStateChanged records one explicit lifecycle transition: provisioning
// to ready, ready to archived, archived to ready. A provisioning failure is a
// reason on a provisioning repository and is not an event.
type RepositoryStateChanged struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// GitRefActor is who made the push: a person or a workload (an attempt).
type GitRefActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// GitRefUpdate is one applied ref update of a receive: the ref, its value before
// and after (the all-zero object ID for a ref that did not exist, or no longer
// does) and the kind decided from them and the worker's fast-forward fact.
type GitRefUpdate struct {
	Ref  string `json:"ref"`
	Old  string `json:"old"`
	New  string `json:"new"`
	Kind string `json:"kind"`
}

// Update kinds.
const (
	GitKindCreate      = string(gitv1.KindCreate)
	GitKindFastForward = string(gitv1.KindFastForward)
	GitKindRewrite     = string(gitv1.KindRewrite)
	GitKindDelete      = string(gitv1.KindDelete)
)

// MaxGitRefsUpdatedUpdates bounds the updates one event carries: the git
// contract's ceiling on a receive's updates.
const MaxGitRefsUpdatedUpdates = gitv1.MaxPushUpdates

// GitRefsUpdated is committed once per receive that applied at least one update,
// with the receive's outcome. It carries the applied updates inline, so a
// consumer needs no second read to match a ref pattern, pin a commit or exclude
// a run's own push. ReceiveID identifies the receive within its repository and
// is the push's identity: the event's deterministic identity derives from the
// repository and ReceiveID, so a retried outcome is the same event. OriginRunID
// is the run that made the push (explicit null for a person).
//
// The payload carries references only: no file content, commit message or
// credential, and never a server ref (refs/kodama/...). A very large receive's
// payload can exceed the generic envelope's data bound (MaxDataBytes); the
// payload itself is valid up to MaxGitRefsUpdatedUpdates updates, and a producer
// that carries it outside an envelope (the kernel's outbox rows) validates it
// with DecodeGitRefsUpdated.
type GitRefsUpdated struct {
	ReceiveID    string         `json:"receiveId"`
	RepositoryID string         `json:"repositoryId"`
	Actor        GitRefActor    `json:"actor"`
	OriginRunID  *string        `json:"originRunId"`
	Updates      []GitRefUpdate `json:"updates"`
	// JournalPosition is the worker's journal position of the receive (the
	// sequence of its first journal entry; gitv1.OutcomeRequest.JournalPosition).
	// It orders the pushes of a repository as their updates were applied, which
	// the order in which outcomes are reported and events are committed does not:
	// a consumer that keeps the latest of a ref's content (pipeline indexing)
	// compares it. Optional, omitted when unknown, and absent from a record store
	// that has no journal.
	JournalPosition *int64 `json:"journalPosition,omitempty"`
}

// NewGitRefsUpdated derives the payload of git.refs_updated from a settled push
// record: one update per applied entry, in record order, with the entry's
// persisted kind. The record must be applied (a record that is unknown or still
// in flight emits nothing yet); refused and unsettled entries are not updates.
func NewGitRefsUpdated(r gitv1.PushRecord) (GitRefsUpdated, error) {
	if err := r.Validate(); err != nil {
		return GitRefsUpdated{}, err
	}
	if r.Status != gitv1.PushApplied {
		return GitRefsUpdated{}, invalidf("only an applied push record emits git.refs_updated, not %s", r.Status)
	}
	updates := make([]GitRefUpdate, 0, len(r.Entries))
	for _, e := range r.Entries {
		if e.State == gitv1.EntryApplied {
			updates = append(updates, GitRefUpdate{Ref: e.Ref, Old: e.Old, New: e.New, Kind: string(e.Kind)})
		}
	}
	return GitRefsUpdated{
		ReceiveID: r.ReceiveID, RepositoryID: r.RepositoryID, Actor: GitRefActor{Type: string(r.Actor.Type), ID: r.Actor.ID},
		OriginRunID: r.OriginRunID, Updates: updates,
	}, nil
}

// DecodeGitRefsUpdated strictly decodes and validates one git.refs_updated
// payload that is not inside an envelope.
func DecodeGitRefsUpdated(data []byte) (GitRefsUpdated, error) {
	spec := specs[TypeGitRefsUpdated]
	if err := spec.validate(data); err != nil {
		return GitRefsUpdated{}, err
	}
	var v GitRefsUpdated
	if err := unmarshalExact(data, &v); err != nil {
		return GitRefsUpdated{}, fmt.Errorf("%w: data: %v", ErrInvalid, err)
	}
	return v, nil
}

func init() {
	register[RepositoryCreated](TypeRepositoryCreated, ResourceTypeRepository, true)
	register[RepositoryStateChanged](TypeRepositoryStateChanged, ResourceTypeRepository, true)
	register[GitRefsUpdated](TypeGitRefsUpdated, ResourceTypeGitPush, true)
	envelopeChecks[TypeGitRefsUpdated] = checkGitRefsUpdatedEnvelope
}

func (v *RepositoryCreated) validate() error {
	if err := firstError(matchPattern("name", "RepositoryName", v.Name), gitDefaultBranch(v.DefaultBranch),
		oneOf("hosting", v.Hosting, RepositoryHostingInternal, RepositoryHostingExternal)); err != nil {
		return err
	}
	return nil
}

func gitDefaultBranch(name string) error {
	if err := gitv1.ValidateBranchName(name); err != nil {
		return invalidf("defaultBranch: %v", err)
	}
	return nil
}

func (v *RepositoryStateChanged) validate() error {
	statuses := []string{string(gitv1.StatusProvisioning), string(gitv1.StatusReady), string(gitv1.StatusArchived)}
	if err := firstError(oneOf("from", v.From, statuses...), oneOf("to", v.To, statuses...), matchCode("reason", v.Reason)); err != nil {
		return err
	}
	if !gitv1.ValidRepositoryTransition(gitv1.RepositoryStatus(v.From), gitv1.RepositoryStatus(v.To)) {
		return invalidf("%s to %s is not a repository transition", v.From, v.To)
	}
	return nil
}

func (v *GitRefsUpdated) validate() error {
	if err := firstError(matchID("receiveId", v.ReceiveID), matchID("repositoryId", v.RepositoryID), oneOf("actor.type", v.Actor.Type, "user", "workload"),
		matchID("actor.id", v.Actor.ID), validateOptional("originRunId", v.OriginRunID, matchID)); err != nil {
		return err
	}
	switch {
	case v.Actor.Type == "workload" && v.OriginRunID == nil:
		return invalidf("a run's push carries that run as originRunId")
	case v.Actor.Type == "user" && v.OriginRunID != nil:
		return invalidf("a person's push has no originRunId")
	}
	if len(v.Updates) < 1 || len(v.Updates) > MaxGitRefsUpdatedUpdates {
		return invalidf("updates must list between 1 and %d updates", MaxGitRefsUpdatedUpdates)
	}
	if v.JournalPosition != nil && (*v.JournalPosition < 1 || *v.JournalPosition > MaxSafeInteger) {
		return invalidf("journalPosition must be between 1 and 2^53-1")
	}
	seen := make(map[string]bool, len(v.Updates))
	for i, u := range v.Updates {
		if err := u.validate(); err != nil {
			return invalidf("updates[%d]: %v", i, err)
		}
		if seen[u.Ref] {
			return invalidf("updates[%d]: %s is updated twice", i, u.Ref)
		}
		seen[u.Ref] = true
	}
	return nil
}

func (u GitRefUpdate) validate() error {
	if err := gitv1.ValidateRefName(u.Ref); err != nil {
		return err
	}
	if strings.HasPrefix(u.Ref, "refs/kodama/") {
		return fmt.Errorf("a server ref is never in an event")
	}
	if err := firstError(matchPattern("old", "GitCommit", u.Old), matchPattern("new", "GitCommit", u.New),
		oneOf("kind", u.Kind, GitKindCreate, GitKindFastForward, GitKindRewrite, GitKindDelete)); err != nil {
		return err
	}
	if len(u.Old) != len(u.New) {
		return fmt.Errorf("old and new must be object IDs of one length")
	}
	oldZero, newZero := gitv1.IsZeroObjectID(u.Old), gitv1.IsZeroObjectID(u.New)
	switch {
	case oldZero && newZero:
		return fmt.Errorf("an update of nothing to nothing")
	case u.Old == u.New:
		return fmt.Errorf("old and new are the same commit")
	case u.Kind == GitKindCreate && (!oldZero || newZero):
		return fmt.Errorf("a create has no old value and a new value")
	case u.Kind == GitKindDelete && (oldZero || !newZero):
		return fmt.Errorf("a delete has an old value and no new value")
	case (u.Kind == GitKindFastForward || u.Kind == GitKindRewrite) && (oldZero || newZero):
		return fmt.Errorf("%s needs an old and a new value", u.Kind)
	}
	return nil
}

// validateMembers requires originRunId as an explicit member (null for a person):
// typed decoding cannot tell an omitted member from an explicit null.
func (v *GitRefsUpdated) validateMembers(members map[string]json.RawMessage) error {
	for _, name := range []string{"receiveId", "repositoryId", "actor", "originRunId", "updates"} {
		if _, ok := members[name]; !ok {
			return invalidf("%s is required", name)
		}
	}
	return nil
}

// checkGitRefsUpdatedEnvelope binds the envelope to its payload: the aggregate is
// the push, which is the receive the payload names.
func checkGitRefsUpdatedEnvelope(e Envelope) error {
	var v GitRefsUpdated
	if err := unmarshalExact(e.Data, &v); err != nil {
		return fmt.Errorf("%w: data: %v", ErrInvalid, err)
	}
	if e.Resource.ID != v.ReceiveID {
		return invalidf("resource.id must be the receiveId of the payload")
	}
	return nil
}
