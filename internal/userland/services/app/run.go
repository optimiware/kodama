// Package app wires the independent service adapters to the public client and
// event boundaries. It is composition glue only: it starts no server and owns
// no adapter decision logic.
//
// The package is a Userland app process boundary. It may import only neutral
// packages, the public API/client contracts and other Userland packages; it
// never imports Shell, Kernel or installation code.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

// ErrDisabled reports that no service adapter was configured. It is a clean
// refusal, never a silently idle process.
var ErrDisabled = errors.New("no service adapter is configured")

// Config is the complete service adapter composition input.
type Config struct {
	Kind           services.ServiceKind
	ServiceID      id.PrincipalID
	TenantID       id.TenantID
	ProjectID      id.ProjectID
	Schedule       time.Duration
	RescanInterval time.Duration

	// Transport is the public client boundary. It is required unless Handler is
	// injected.
	Transport client.Transport
	// Handler is an optional injected public operation handler.
	Handler api.Handler
	// Events is the optional retained-notification source. A nil value uses
	// the public event.read edge.
	Events services.EventSource
	// Clock is the injected time boundary.
	Clock services.Clock
	// Merges is the required merge-coordinator dependency for
	// services.KindMergeCoordinator and is unused by every other kind.
	Merges services.MergeRunner
}

// Run runs one independent service adapter until ctx is canceled. It refuses
// cleanly when no service is configured and never starts an unrelated process.
func Run(ctx context.Context, config Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !config.Kind.Valid() || strings.TrimSpace(string(config.ServiceID)) == "" {
		return ErrDisabled
	}
	handler := config.Handler
	if handler == nil {
		if config.Transport == nil {
			return fmt.Errorf("service adapter requires a public transport or handler")
		}
		handler = config.Transport
	}
	events := config.Events
	if events == nil {
		if config.Transport == nil {
			return fmt.Errorf("service adapter requires an event source or public transport")
		}
		built, err := NewPublicEventSource(config.Transport, config.ProjectID)
		if err != nil {
			return err
		}
		events = built
	}
	clock := config.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	adapter, err := services.NewAdapter(services.AdapterConfig{
		Kind:           config.Kind,
		ServiceID:      config.ServiceID,
		TenantID:       config.TenantID,
		ProjectID:      config.ProjectID,
		Handler:        handler,
		Events:         events,
		Clock:          clock,
		Schedule:       config.Schedule,
		RescanInterval: config.RescanInterval,
		Merges:         config.Merges,
	})
	if err != nil {
		return err
	}
	return adapter.Run(ctx)
}

// PublicEventSource implements services.EventSource over the authorized public
// `event.read` edge. Events are hints only: they never grant assignment
// authority and a repeated or reordered page is harmless.
type PublicEventSource struct {
	transport client.Transport
	projectID string
	limit     int
}

var _ services.EventSource = (*PublicEventSource)(nil)

// NewPublicEventSource validates the boundary and returns the source.
func NewPublicEventSource(transport client.Transport, projectID id.ProjectID) (*PublicEventSource, error) {
	if transport == nil {
		return nil, fmt.Errorf("event source transport is missing")
	}
	return &PublicEventSource{transport: transport, projectID: string(projectID)}, nil
}

// Poll reads one retained event page. The continuation cursor is returned only
// when the public result reports more pages, because the public cursor is
// always present.
func (source *PublicEventSource) Poll(ctx context.Context, cursor string) (services.NotificationPage, error) {
	if source == nil || source.transport == nil {
		return services.NotificationPage{}, fmt.Errorf("event source is not configured")
	}
	page := api.Page{Cursor: cursor}
	if source.limit > 0 {
		page.Limit = source.limit
	}
	input, err := json.Marshal(api.EventReadRequest{ProjectID: source.projectID, Page: &page})
	if err != nil {
		return services.NotificationPage{}, fmt.Errorf("encode event read request: %w", err)
	}
	result, err := source.transport.Invoke(ctx, api.Invocation{
		Operation: "event.read",
		Request:   api.Request{Input: input},
	})
	if err != nil {
		return services.NotificationPage{}, err
	}
	var read api.EventReadResult
	if err := decodePublicResult(result, "event.read", &read); err != nil {
		return services.NotificationPage{}, err
	}
	if err := read.Validate(); err != nil {
		return services.NotificationPage{}, err
	}
	notifications := make([]services.Notification, 0, len(read.Events))
	for _, event := range read.Events {
		notifications = append(notifications, services.Notification{
			EventID:   id.EventID(event.ID),
			Type:      event.Type,
			TenantID:  id.TenantID(event.Scope.TenantID),
			ProjectID: id.ProjectID(event.Scope.ProjectID),
			At:        event.At,
		})
	}
	next := services.NotificationPage{Notifications: notifications}
	if read.HasMore {
		next.Next = api.PageInfo{NextCursor: read.Next}
	}
	return next, nil
}

// decodePublicResult checks the operation identity and decodes one result body.
func decodePublicResult(result api.Result, operation string, target any) error {
	if result.Operation != operation {
		return fmt.Errorf("public result names operation %q, not %q", result.Operation, operation)
	}
	if len(result.Body) == 0 {
		return fmt.Errorf("public result %q has no body", operation)
	}
	if err := json.Unmarshal(result.Body, target); err != nil {
		return fmt.Errorf("decode public result %q: %w", operation, err)
	}
	return nil
}

// SystemClock is the production time boundary.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }
