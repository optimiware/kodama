package execution

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// EnvironmentRequest identifies an authorized admission, never a workload's
// requested destination. Deployment-bound cleanup has a separate integration
// checkpoint; it must not resolve the environment's current configuration.
type EnvironmentRequest struct {
	Scope         domain.Scope
	Actor         Actor
	EnvironmentID domain.EnvironmentID
}

// EnvironmentSnapshot is delivery's immutable configuration identity and exact
// credential bindings. It contains no credential plaintext. Scope must match the
// admission's verified tenant and project.
type EnvironmentSnapshot struct {
	Scope   domain.Scope
	Pin     runtimev1.EnvironmentPin
	Secrets []runtimev1.SecretBinding
}

// EnvironmentResolver is the execution-owned admission seam implemented by
// delivery. ResolveEnvironment checks current access and returns an immutable,
// retained snapshot. Edits never rewrite that snapshot. It runs outside the
// admission transaction; existing store authority/secret-pin checks still run in
// that transaction. Delivery's transaction-bound retention/revocation adapter is
// qualified separately before production wiring enables this capability.
type EnvironmentResolver interface {
	ResolveEnvironment(context.Context, EnvironmentRequest) (EnvironmentSnapshot, error)
}

// StepEnvironment is the persisted environment identity of one declared step.
type StepEnvironment struct {
	Step string                   `json:"step"`
	Pin  runtimev1.EnvironmentPin `json:"pin"`
}

// NewServiceWithEnvironments adds the optional delivery admission capability.
// The existing constructor remains suitable for M1: without this binding,
// environment-declaring runs fail closed while ordinary runs remain available.
func NewServiceWithEnvironments(d Dependencies, o Options, resolver EnvironmentResolver) (*Service, error) {
	if resolver == nil {
		return nil, fmt.Errorf("%w: an environment resolver is required", ErrNotConfigured)
	}
	s, err := NewService(d, o)
	if err != nil {
		return nil, err
	}
	s.environments = resolver
	return s, nil
}

var bindingName = regexp.MustCompile(runtimev1.Patterns["SafeName"])

// EnvironmentBinding is one retained configuration snapshot an admission pins,
// handed to the store so the admission transaction can revalidate it. Secrets
// are the snapshot's exact credential bindings (no plaintext). Deployment is
// set only for a cleanup run and names the recorded deployment whose retained
// configuration the snapshot is.
type EnvironmentBinding struct {
	Scope      domain.Scope
	Actor      Actor
	Pin        runtimev1.EnvironmentPin
	Secrets    []runtimev1.SecretBinding
	Deployment string
}

// TxEnvironments is the delivery revalidation bound to one open admission
// transaction. It reads current authority and the retained snapshot through that
// transaction and makes no network call. nil allows the pin; ErrDenied or
// ErrHidden refuse for authority; ErrNotFound or ErrAdmissionRefused refuse a
// missing, foreign, revoked or altered snapshot; anything else is an outage
// (ErrUnavailable), never admission. It takes no lock that the documented lock
// order does not already allow.
type TxEnvironments interface {
	BindEnvironment(ctx context.Context, b EnvironmentBinding) error
}

// BindEnvironments is the single transaction-time step every Store runs while
// admitting a plan: after the work/authority checks and before the run is saved
// it revalidates each environment the plan pins through tx. The admission aborts
// (and the store's transaction rolls back every write) on the first failure. A
// plan that pins environments needs a binding for exactly those pins, and a
// store without a transactional binding (tx == nil) fails closed. A plan
// without environments is not affected, preserving no-environment admission.
func BindEnvironments(ctx context.Context, tx TxEnvironments, p *AdmissionPlan) error {
	if p == nil || p.Aggregate == nil || p.Aggregate.Run == nil {
		return invalidf("an admission plan is required")
	}
	run := p.Aggregate.Run
	if len(run.Snapshot.Environments) == 0 && len(p.Environments) == 0 {
		return nil
	}
	if tx == nil {
		return fmt.Errorf("%w: transactional environment binding is not available", ErrNotConfigured)
	}
	type key struct {
		id     domain.EnvironmentID
		digest runtimev1.Digest
	}
	pinned := map[key]bool{}
	for _, e := range run.Snapshot.Environments {
		pinned[key{e.Pin.ID, e.Pin.ConfigDigest}] = true
	}
	bound := map[key]bool{}
	for _, b := range p.Environments {
		k := key{b.Pin.ID, b.Pin.ConfigDigest}
		if !pinned[k] || b.Scope != run.Scope || b.Pin.ID.Validate() != nil || b.Pin.ConfigDigest.Validate() != nil || b.Actor != p.Actor {
			return Refusedf("the environment binding does not match the admitted run")
		}
		if (run.Purpose == PurposeCleanup) != (b.Deployment != "") || b.Deployment != run.DeploymentID {
			return Refusedf("the environment binding does not match the run's purpose")
		}
		bound[k] = true
	}
	for k := range pinned {
		if !bound[k] {
			return Refusedf("an admitted environment pin has no transactional binding")
		}
	}
	for _, b := range p.Environments {
		b.Secrets = append([]runtimev1.SecretBinding(nil), b.Secrets...)
		if err := tx.BindEnvironment(ctx, b); err != nil {
			switch {
			case errors.Is(err, ErrDenied), errors.Is(err, ErrHidden):
				return ErrDenied
			case errors.Is(err, ErrNotFound), errors.Is(err, ErrAdmissionRefused):
				return Refusedf("the retained environment is no longer admissible")
			case errors.Is(err, ErrNotConfigured), errors.Is(err, ErrUnavailable):
				return err
			}
			return Unavailablef(err, "environment revalidation is unavailable")
		}
	}
	return nil
}

func (s *Service) resolveEnvironments(ctx context.Context, scope domain.Scope, actor Actor, doc pipeline.Document, snap Snapshot) (Snapshot, error) {
	snap, _, err := s.resolveEnvironmentSet(ctx, scope, actor, doc, snap)
	return snap, err
}

// checkEnvironmentSnapshot validates a resolved snapshot against the admission
// and returns an independent, name-ordered copy.
func checkEnvironmentSnapshot(scope domain.Scope, id domain.EnvironmentID, pin EnvironmentSnapshot) (EnvironmentSnapshot, error) {
	if pin.Scope != scope || pin.Pin.ID != id || pin.Pin.ConfigDigest.Validate() != nil {
		return EnvironmentSnapshot{}, Refusedf("the environment snapshot does not match the admission")
	}
	if len(pin.Secrets) > 256 { // runtime context's per-step binding limit
		return EnvironmentSnapshot{}, Refusedf("the environment has too many credential bindings")
	}
	pin.Secrets = append([]runtimev1.SecretBinding(nil), pin.Secrets...)
	sort.Slice(pin.Secrets, func(i, j int) bool { return pin.Secrets[i].Name < pin.Secrets[j].Name })
	return pin, nil
}

// resolveEnvironmentSet pins each declared environment once and returns the
// bindings the admission transaction revalidates.
func (s *Service) resolveEnvironmentSet(ctx context.Context, scope domain.Scope, actor Actor, doc pipeline.Document, snap Snapshot) (Snapshot, []EnvironmentBinding, error) {
	// Configuration owns ordinary inputs/secrets, not environment identities.
	if len(snap.Environments) != 0 {
		return Snapshot{}, nil, Unavailablef(nil, "configuration supplied undeclared environment pins")
	}
	resolved := map[domain.EnvironmentID]EnvironmentSnapshot{}
	var bindings []EnvironmentBinding
	for _, step := range doc.Spec.Steps {
		if step.Environment == "" {
			continue
		}
		if s.environments == nil {
			return Snapshot{}, nil, fmt.Errorf("%w: environment admission is not available", ErrNotConfigured)
		}
		id := domain.EnvironmentID(step.Environment)
		if !step.Executable() || id.Validate() != nil {
			return Snapshot{}, nil, Refusedf("an executable step with a valid environment is required")
		}
		pin, ok := resolved[id]
		if !ok {
			var err error
			pin, err = s.environments.ResolveEnvironment(ctx, EnvironmentRequest{Scope: scope, Actor: actor, EnvironmentID: id})
			if err != nil {
				switch {
				case errors.Is(err, ErrDenied), errors.Is(err, ErrHidden), errors.Is(err, ErrNotFound), errors.Is(err, ErrAdmissionRefused):
					return Snapshot{}, nil, Refusedf("the declared environment is not admissible")
				default:
					return Snapshot{}, nil, Unavailablef(err, "environment resolution is unavailable")
				}
			}
			if pin, err = checkEnvironmentSnapshot(scope, id, pin); err != nil {
				return Snapshot{}, nil, err
			}
			resolved[id] = pin
			bindings = append(bindings, EnvironmentBinding{Scope: scope, Actor: actor, Pin: pin.Pin, Secrets: append([]runtimev1.SecretBinding(nil), pin.Secrets...)})
		}
		var err error
		if snap, err = pinStepEnvironment(snap, step.ID, pin); err != nil {
			return Snapshot{}, nil, err
		}
	}
	return snap, bindings, nil
}

func pinStepEnvironment(snap Snapshot, step string, pin EnvironmentSnapshot) (Snapshot, error) {
	snap.Environments = append(snap.Environments, StepEnvironment{Step: step, Pin: pin.Pin})
	if err := appendEnvironmentSecrets(&snap, step, pin.Secrets); err != nil {
		return Snapshot{}, err
	}
	if len(snap.SecretsOf(step)) > 256 {
		return Snapshot{}, Refusedf("the step has too many credential bindings")
	}
	return snap, nil
}

// resolveCleanupEnvironments pins the deployment's retained configuration for
// the environment-declaring steps of a cleanup definition. The only admissible
// environment is the deployment's own: the retained snapshot is read through the
// deployment binding, so the environment's current settings are never consulted
// and an edited, revoked or removed environment cannot change what cleanup
// uses. An unresolvable retained snapshot fails closed.
func (s *Service) resolveCleanupEnvironments(ctx context.Context, scope domain.Scope, deployment string, doc pipeline.Document, snap Snapshot) (Snapshot, []EnvironmentBinding, error) {
	if len(snap.Environments) != 0 {
		return Snapshot{}, nil, Unavailablef(nil, "configuration supplied undeclared environment pins")
	}
	var retained *EnvironmentSnapshot
	var bindings []EnvironmentBinding
	for _, step := range doc.Spec.Steps {
		if step.Environment == "" {
			continue
		}
		if s.d.CleanupEnvironments == nil {
			return Snapshot{}, nil, fmt.Errorf("%w: retained deployment environment admission requires the cleanup binding", ErrNotConfigured)
		}
		id := domain.EnvironmentID(step.Environment)
		if !step.Executable() || id.Validate() != nil {
			return Snapshot{}, nil, Refusedf("an executable step with a valid environment is required")
		}
		if retained == nil {
			got, err := s.d.CleanupEnvironments.RetainedEnvironment(ctx, scope, deployment)
			if err != nil {
				switch {
				case errors.Is(err, ErrDenied), errors.Is(err, ErrHidden), errors.Is(err, ErrNotFound), errors.Is(err, ErrAdmissionRefused):
					return Snapshot{}, nil, Refusedf("the deployment's retained configuration is not admissible")
				default:
					return Snapshot{}, nil, Unavailablef(err, "the deployment's retained configuration is unavailable")
				}
			}
			if got, err = checkEnvironmentSnapshot(scope, got.Pin.ID, got); err != nil {
				return Snapshot{}, nil, err
			}
			retained = &got
			bindings = append(bindings, EnvironmentBinding{Scope: scope, Actor: System, Pin: got.Pin, Secrets: append([]runtimev1.SecretBinding(nil), got.Secrets...), Deployment: deployment})
		}
		if retained.Pin.ID != id {
			return Snapshot{}, nil, Refusedf("a cleanup step may declare only the deployment's own environment")
		}
		var err error
		if snap, err = pinStepEnvironment(snap, step.ID, *retained); err != nil {
			return Snapshot{}, nil, err
		}
	}
	return snap, bindings, nil
}

func appendEnvironmentSecrets(snap *Snapshot, step string, bindings []runtimev1.SecretBinding) error {
	names := map[string]bool{}
	for _, b := range bindings {
		if (runtimev1.SecretResolveRequest{Ref: b.Ref, Version: b.Version}).Validate() != nil || !bindingName.MatchString(b.Name) ||
			(b.Delivery != runtimev1.DeliveryFile && b.Delivery != runtimev1.DeliveryEnv) || names[b.Name] {
			return Refusedf("the environment credential bindings are invalid")
		}
		names[b.Name] = true
		duplicate := false
		for _, existing := range snap.Secrets {
			if existing.Binding.Ref == b.Ref && existing.Binding.Version != b.Version {
				return Refusedf("environment and step credential versions conflict")
			}
			// Runtime resolution identifies a credential by reference/version,
			// so one step cannot request aliases with different names/delivery.
			if existing.Step == step && existing.Binding.Ref == b.Ref && existing.Binding != b {
				return Refusedf("a step credential must have one unambiguous binding")
			}
			if existing.Step == step && existing.Binding.Name == b.Name {
				if existing.Binding != b {
					return Refusedf("environment and step credential names conflict")
				}
				duplicate = true
			}
		}
		if !duplicate {
			snap.Secrets = append(snap.Secrets, StepSecret{Step: step, Binding: b})
		}
	}
	return nil
}

func (s Snapshot) environmentOf(step *StepRun) (*runtimev1.EnvironmentPin, error) {
	var pin *runtimev1.EnvironmentPin
	for _, candidate := range s.Environments {
		if candidate.Step != step.ID {
			continue
		}
		if pin != nil || string(candidate.Pin.ID) != step.Spec.Environment || candidate.Pin.ID.Validate() != nil || candidate.Pin.ConfigDigest.Validate() != nil {
			return nil, Refusedf("the persisted environment pin is invalid")
		}
		copy := candidate.Pin
		pin = &copy
	}
	if step.Spec.Environment != "" && pin == nil {
		return nil, Refusedf("the declared environment has no admitted pin")
	}
	return pin, nil
}
