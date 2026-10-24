package delivery

import (
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// CheckPinned verifies that an attempt's immutable pinned context belongs to
// the verified principal: scope, run and attempt all match, and the step has an
// action key and a declared, pinned environment. An absent or mismatched pin is
// ErrPinRequired; nothing here consults the environment's current settings.
func CheckPinned(c runtimev1.Context, p Principal) error {
	deny := func() error {
		return Fail(ErrPinRequired, nil, "the attempt's pinned context does not match the request")
	}
	if c.Scope() != p.Scope || c.RunID != p.Run || c.AttemptID != p.Attempt {
		return deny()
	}
	if !actionKeyPattern.MatchString(c.ActionKey) || c.StepID == "" {
		return deny()
	}
	if c.Environment == nil || c.Environment.ID.Validate() != nil || c.Environment.ConfigDigest.Validate() != nil {
		return Fail(ErrPinRequired, nil, "the attempt has no pinned environment")
	}
	return nil
}

// CheckEnvironment verifies that the retained snapshot is the one the context
// pins, and that the context carries the snapshot's exact credential bindings:
// same reference, version, name and delivery form. A binding that differs, or
// is missing, is a mismatch.
func CheckEnvironment(c runtimev1.Context, snap ConfigurationSnapshot) error {
	deny := func() error {
		return Fail(ErrPinRequired, nil, "the pinned environment does not match its retained configuration")
	}
	if c.Environment == nil || *c.Environment != snap.Pin() || c.Scope() != snap.Scope {
		return deny()
	}
	have := map[string]runtimev1.SecretBinding{}
	for _, b := range c.Secrets {
		have[b.Name] = b
	}
	for _, want := range snap.Secrets {
		if got, ok := have[want.Name]; !ok || got != want {
			return deny()
		}
	}
	return nil
}

// pinnedValues returns every admitted scalar the step can reference: its pinned
// inputs, resolved arguments and environment variables.
func pinnedValues(c runtimev1.Context) []string {
	var out []string
	for _, v := range c.Inputs {
		if v.IsString() {
			out = append(out, v.String())
		}
	}
	out = append(out, c.Command.Args...)
	for _, v := range c.Command.Env {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ReferencesArtifact reports whether the admitted context names the artifact as
// one of the step's pinned inputs. An image the step was never given is not
// eligible, even if it exists in the project.
func ReferencesArtifact(c runtimev1.Context, id domain.ArtifactID) bool {
	vals := pinnedValues(c)
	i := sort.SearchStrings(vals, string(id))
	return i < len(vals) && vals[i] == string(id)
}

// BoundTo reports whether the deployment belongs to the context's logical
// action: the same run, step, action key and pinned environment/configuration.
func (d Deployment) BoundTo(c runtimev1.Context) bool {
	return c.Environment != nil && d.rec.Scope == c.Scope() && d.rec.Run == c.RunID && d.rec.Step == c.StepID &&
		d.rec.ActionKey == c.ActionKey && d.Pin() == *c.Environment
}
