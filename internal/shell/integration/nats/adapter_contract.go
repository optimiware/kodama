// Package nats declares the scope-qualified broker seam.
//
// Subjects are scope-qualified so a subscriber for one tenant/project cannot
// receive another scope's events, logs or live invalidations:
//
//	kodama.v1.<tenant>.<project|_>.events
//	kodama.v1.<tenant>.<project|_>.invalidations
//	kodama.v1.<tenant>.<project|_>.logs.<execution>
//	kodama.v1.<tenant>.<project|_>.ingress
//
// The broker publishes already-committed event rows and live log chunks; it is
// never the durable source of either. The ingress subject is the separate
// authenticated observation-ingress subject: a message on it is authenticated,
// attributed to the verified principal and handed to the observation sink, so
// it can never be mistaken for a committed event on the event subject. A broker
// outage is degraded publication, not lost execution progress. Wildcard
// subscriptions are not part of the contract: every subject names exactly one
// scope.
package nats

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// Subject prefix and the tenant-wide project placeholder.
const (
	// SubjectPrefix is the declared subject prefix.
	SubjectPrefix = "kodama.v1"
	// TenantWideProject is the project token for a tenant-scoped subject. A
	// project identity is never encoded as this token, even though "_" is a
	// syntactically valid id: buildSubject refuses such a project so the
	// encoding stays injective and ParseScope never maps a project subject to
	// a tenant-wide scope.
	TenantWideProject = "_"
)

// Subject kinds.
const (
	subjectEvents        = "events"
	subjectLogs          = "logs"
	subjectInvalidations = "invalidations"
	subjectIngress       = "ingress"
)

// Subject is one scope-qualified broker subject.
type Subject string

// EventSubject returns the scope-qualified event subject.
func EventSubject(scope tenancy.Scope) (Subject, error) {
	return buildSubject(scope, subjectEvents, "")
}

// LogSubject returns the scope-qualified live-log subject for one execution.
func LogSubject(scope tenancy.Scope, executionID id.ExecutionID) (Subject, error) {
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "log subject execution id is invalid", err)
	}
	return buildSubject(scope, subjectLogs, string(executionID))
}

// InvalidationSubject returns the scope-qualified live-invalidation subject.
func InvalidationSubject(scope tenancy.Scope) (Subject, error) {
	return buildSubject(scope, subjectInvalidations, "")
}

// IngressSubject returns the scope-qualified authenticated observation-ingress
// subject. It is deliberately separate from the committed-event subject: the
// ingress carries observations attributed to a verified principal, never an
// already-committed fact.
func IngressSubject(scope tenancy.Scope) (Subject, error) {
	return buildSubject(scope, subjectIngress, "")
}

func buildSubject(scope tenancy.Scope, kind, executionID string) (Subject, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	project := TenantWideProject
	if scope.ProjectID != "" {
		if string(scope.ProjectID) == TenantWideProject {
			return "", fault.New(fault.InvalidInput, "project id \"_\" collides with the tenant-wide subject placeholder")
		}
		project = string(scope.ProjectID)
	}
	subject := SubjectPrefix + "." + string(scope.TenantID) + "." + project + "." + kind
	if executionID != "" {
		subject += "." + executionID
	}
	return Subject(subject), nil
}

// Validate checks the exact scope-qualified subject shape. Wildcards, empty
// tokens and unknown kinds are refused.
func (subject Subject) Validate() error {
	tokens := strings.Split(string(subject), ".")
	if len(tokens) < 5 {
		return fault.New(fault.InvalidInput, fmt.Sprintf("subject %q is not scope-qualified", subject))
	}
	if tokens[0] != "kodama" || tokens[1] != "v1" {
		return fault.New(fault.InvalidInput, fmt.Sprintf("subject %q has an unknown prefix", subject))
	}
	if _, err := id.ParseTenantID(tokens[2]); err != nil {
		return fault.Wrap(fault.InvalidInput, "subject tenant token is invalid", err)
	}
	if tokens[3] != TenantWideProject {
		if _, err := id.ParseProjectID(tokens[3]); err != nil {
			return fault.Wrap(fault.InvalidInput, "subject project token is invalid", err)
		}
	}
	switch tokens[4] {
	case subjectEvents, subjectInvalidations, subjectIngress:
		if len(tokens) != 5 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("subject %q has trailing tokens", subject))
		}
	case subjectLogs:
		if len(tokens) != 6 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("log subject %q must name exactly one execution", subject))
		}
		if _, err := id.ParseExecutionID(tokens[5]); err != nil {
			return fault.Wrap(fault.InvalidInput, "subject execution token is invalid", err)
		}
	default:
		return fault.New(fault.InvalidInput, fmt.Sprintf("subject %q has an unknown kind", subject))
	}
	return nil
}

// ParseScope returns the ownership scope named by a validated subject. The
// tenant-wide placeholder decodes to an empty project id.
func ParseScope(subject Subject) (tenancy.Scope, error) {
	if err := subject.Validate(); err != nil {
		return tenancy.Scope{}, err
	}
	tokens := strings.Split(string(subject), ".")
	scope := tenancy.Scope{TenantID: id.TenantID(tokens[2])}
	if tokens[3] != TenantWideProject {
		scope.ProjectID = id.ProjectID(tokens[3])
	}
	return scope, nil
}

// LiveLogSubject returns the scope-qualified log subject for one live chunk.
// It derives the subject only from the supplied scope, so a publisher cannot
// widen or reinterpret the scope from chunk fields.
func LiveLogSubject(scope tenancy.Scope, chunk log.Chunk) (Subject, error) {
	return LogSubject(scope, chunk.ExecutionID)
}

// Publisher publishes committed event rows and live log chunks. Retained
// publication is not part of this seam. Live publication carries the explicit
// ownership scope and derives the log subject from that scope only.
type Publisher interface {
	PublishEvent(context.Context, event.Envelope) error
	PublishLive(context.Context, tenancy.Scope, log.Chunk) error
}

// Client is the fakeable broker client seam. PublishJetStream carries the
// stable event id as the broker publication identity so a retry with the same
// id is deduplicated; PublishCore carries live progress only. Subscribe hands
// one scope-qualified message to the handler; the handler's error is returned
// to the client so at-least-once delivery stays retryable.
type Client interface {
	PublishJetStream(ctx context.Context, subject Subject, eventID id.EventID, payload []byte) error
	PublishCore(ctx context.Context, subject Subject, payload []byte) error
	Subscribe(ctx context.Context, subject Subject, handler func(context.Context, []byte) error) (Subscription, error)
	Close(ctx context.Context) error
}

// Connect dials one broker client. It is called lazily and again after a
// failed publication discards the suspect client.
type Connect func(context.Context) (Client, error)

// Clock is the injected time source. Sleep returns ctx.Err() when the context
// ends.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, duration time.Duration) error
}

// ObservationSink receives one authenticated observation from the ingress. It
// is the only effect of the ingress: the adapter never publishes an
// observation directly to a committed-event subject.
type ObservationSink interface {
	Observe(context.Context, Observation) error
}

// Observation is one authenticated ingress observation. Scope and Source are
// derived from the verified principal, never from the submitted body.
type Observation struct {
	Scope  tenancy.Scope
	Source event.Source
	Type   event.Type
	Data   []byte
}

// ObservationRequest is one submitted ingress observation. The optional
// DeclaredSource/DeclaredScope may restate the authenticated identity but can
// never change it.
type ObservationRequest struct {
	Credential     string
	Type           event.Type
	Data           []byte
	DeclaredSource *event.Source
	DeclaredScope  *tenancy.Scope
}

// SubscriptionRequest is one explicit-credential invalidation subscription.
type SubscriptionRequest struct {
	Credential string
	Scope      tenancy.Scope
	Handler    InvalidationHandler
}

// AuthenticatedSubscriber opens a scope-qualified invalidation subscription for
// an explicitly presented credential.
type AuthenticatedSubscriber interface {
	Subscribe(context.Context, SubscriptionRequest) (Subscription, error)
}

// BrokerConfig configures the broker adapter. Every dependency and bound is
// explicit; construction never dials the broker.
type BrokerConfig struct {
	Connect             Connect
	Clock               Clock
	Principal           authorize.CurrentPrincipal
	Authorizer          authorize.Authorizer
	Sink                ObservationSink
	MaxReconnectBackoff time.Duration
	IdleValidation      time.Duration
	MaxPublishAttempts  int
}

// InvalidationHandler receives one live invalidation. It must treat delivery as
// retryable and must not treat a missed delivery as re-enabling a credential.
type InvalidationHandler func(context.Context, token.Invalidation) error

// Subscription is one open invalidation subscription.
type Subscription interface {
	Close(context.Context) error
}

// Subscriber opens a scope-qualified invalidation subscription. A subscriber
// for one scope never receives another scope's invalidations.
type Subscriber interface {
	SubscribeInvalidations(context.Context, tenancy.Scope, InvalidationHandler) (Subscription, error)
}
