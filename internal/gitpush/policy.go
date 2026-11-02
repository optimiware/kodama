package gitpush

import (
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// PolicyRepository are the facts about the repository the ref policy needs.
type PolicyRepository struct {
	DefaultBranch string
	// DirectPushToDefault is the repository's policy (default false, O-33).
	DirectPushToDefault bool
	Archived            bool
}

// PolicyInput is everything the ref policy matrix decides from. It is pure
// data: the use case reads the claims (current authority), the writer
// reservation and the attempt's liveness before it evaluates, so no policy
// decision calls anything while a SQL lock is held.
type PolicyInput struct {
	Repo         PolicyRepository
	RepositoryID string
	// Claims are the grant's effective claims (gitauth.Verifier.Claims): for a
	// human grant the update set is empty once the person lost the engineer role.
	Claims  gitv1.GrantClaims
	Request gitv1.PushAuthorizeRequest
	// AttemptLive is false when the grant belongs to an attempt that is no longer
	// live; it is true for a person and for a workload without an attempt.
	AttemptLive bool
	// WriterHeld holds, per work item, whether the grant's attempt currently holds
	// the work item's writer reservation (WS-03). Missing means not held.
	WriterHeld map[string]bool
}

// PolicyResult is the verdict of the matrix: per update, the push refusal
// reason or "" for allow. SeedReceive is true when the receive is an allowed
// seed (single default-branch create of an empty repository): the caller must
// then consume the seed grant for the receive before the record is created.
type PolicyResult struct {
	Denials     []string
	SeedReceive bool
}

// Denied reports whether any update was denied.
func (p PolicyResult) Denied() bool {
	for _, d := range p.Denials {
		if d != "" {
			return true
		}
	}
	return false
}

func (p *PolicyResult) denyAll(reason string) {
	for i := range p.Denials {
		p.Denials[i] = reason
	}
}

// Evaluate applies the ref policy matrix of git-repositories.md ("Access and ref
// policy") to every update of a receive. It decides per ref; a receive with any
// deny fails as a whole, which the caller records. The first applicable rule
// decides, in this order: archived repository; an attempt no longer live; (seed
// grant: the whole-receive seed rules); server-only and unclassified refs; then
// the rules of the grant kind and ref class:
//
//	human (engineer ceiling)  default: only a fast-forward and only with directPushToDefault
//	                          (never a create, delete or rewrite: a create needs a seed);
//	                          other classes: any update (a non-fast-forward is a rewrite)
//	attempt                   default: never; agent and run branches: any attempt of the named
//	                          run; task branch: create or fast-forward by the reserved writer;
//	                          other branches: the pattern the grant names; tags: creation only
//	                          for a release grant naming the pattern, never moved or deleted
//	integration               exactly the constrained update, as a fast-forward
//	preparation               nothing
func Evaluate(in PolicyInput) (PolicyResult, error) {
	req := in.Request
	entries, err := gitv1.NewPushEntries(req)
	if err != nil {
		return PolicyResult{}, err
	}
	res := PolicyResult{Denials: make([]string, len(req.Updates))}
	if in.Repo.Archived {
		res.denyAll(gitv1.ReasonRepositoryArchived)
		return res, nil
	}
	if !in.AttemptLive {
		res.denyAll(gitv1.ReasonAttemptInactive)
		return res, nil
	}
	if in.Claims.Kind == gitv1.GrantSeed {
		return evaluateSeed(in, res), nil
	}
	for i, u := range req.Updates {
		d, err := decide(in, u, entries[i].Kind)
		if err != nil {
			return PolicyResult{}, err
		}
		res.Denials[i] = d
	}
	return res, nil
}

// evaluateSeed applies the whole-receive seed rules (GR-18): the one allowed
// receive creates exactly the default branch of an empty repository; any other
// update, alone or beside it, refuses the whole receive as seed_only.
func evaluateSeed(in PolicyInput, res PolicyResult) PolicyResult {
	req := in.Request
	want := gitv1.HeadsPrefix + in.Repo.DefaultBranch
	if len(req.Updates) != 1 || req.Updates[0].Ref != want || !gitv1.IsZeroObjectID(req.Updates[0].Old) {
		res.denyAll(gitv1.ReasonSeedOnly)
		return res
	}
	if in.Claims.CheckSeed(in.Repo.DefaultBranch) != nil || !in.Claims.PermitsUpdate(in.RepositoryID, req.Updates[0]) {
		res.denyAll(gitv1.ReasonGrantScope)
		return res
	}
	if !req.GitFacts[0].RepositoryEmpty {
		// A repository with any ref is never seedable.
		res.denyAll(gitv1.ReasonRefProtected)
		return res
	}
	res.SeedReceive = true
	return res
}

func decide(in PolicyInput, u gitv1.RefUpdate, kind gitv1.UpdateKind) (string, error) {
	ref, err := gitv1.ClassifyRef(in.Repo.DefaultBranch, u.Ref)
	if err != nil {
		return "", err
	}
	if ref.Class == gitv1.ClassServer || ref.Class == gitv1.ClassOther {
		return gitv1.ReasonRefProtected, nil
	}
	permits := in.Claims.PermitsUpdate(in.RepositoryID, u)
	switch in.Claims.Kind {
	case gitv1.GrantHuman:
		return decideHuman(in, ref, kind, permits), nil
	case gitv1.GrantAttempt:
		return decideAttempt(in, ref, kind, permits), nil
	case gitv1.GrantIntegration:
		if !permits {
			return gitv1.ReasonIntegrationMismatch, nil
		}
		if ref.Class != gitv1.ClassDefault || kind != gitv1.KindFastForward {
			return gitv1.ReasonNotFastForward, nil
		}
		return "", nil
	case gitv1.GrantPreparation:
		return gitv1.ReasonGrantScope, nil
	}
	return "", fmt.Errorf("gitpush: unknown grant kind %q", in.Claims.Kind)
}

func decideHuman(in PolicyInput, ref gitv1.Ref, kind gitv1.UpdateKind, permits bool) string {
	if !permits {
		return gitv1.ReasonGrantScope // not (or no longer) an engineer
	}
	if ref.Class != gitv1.ClassDefault {
		return ""
	}
	switch {
	case !in.Repo.DirectPushToDefault, kind == gitv1.KindCreate, kind == gitv1.KindDelete:
		return gitv1.ReasonRefProtected
	case kind == gitv1.KindRewrite:
		return gitv1.ReasonNotFastForward
	}
	return ""
}

func decideAttempt(in PolicyInput, ref gitv1.Ref, kind gitv1.UpdateKind, permits bool) string {
	actor := in.Claims.Actor
	ownRun := actor.RunID != nil && ref.RunID != "" && *actor.RunID == ref.RunID
	switch ref.Class {
	case gitv1.ClassDefault:
		return gitv1.ReasonRefProtected
	case gitv1.ClassAgent, gitv1.ClassRun:
		// Any executable attempt of the named run, whatever its declared access:
		// agent branches publish no checkpoint.
		if ownRun {
			return ""
		}
		return gitv1.ReasonGrantScope
	case gitv1.ClassTask:
		switch {
		case !permits:
			return gitv1.ReasonGrantScope
		case kind == gitv1.KindRewrite:
			return gitv1.ReasonNotFastForward
		case kind == gitv1.KindDelete, !in.WriterHeld[ref.WorkItemID]:
			return gitv1.ReasonRefProtected
		}
		return ""
	case gitv1.ClassTag:
		switch {
		case !permits:
			return gitv1.ReasonGrantScope
		case kind != gitv1.KindCreate:
			return gitv1.ReasonRefProtected // never moved or deleted by a workload
		}
		return ""
	}
	if permits { // other branches: only a pattern the grant names
		return ""
	}
	return gitv1.ReasonGrantScope
}
