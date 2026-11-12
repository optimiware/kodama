package opsapp

import (
	"context"
	"strconv"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// eventHandlers binds custom observation publication and the authorized
// committed-event read/subscription setup. The public DTOs carry only the facts
// the client may choose; identity, time, source, scope and receipt identity are
// derived by the real capability service from the authenticated principal.
func eventHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"event.publish": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.EventPublishRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			attempt := event.PublishAttempt{
				CallerKey:      call.Request.IdempotencyKey,
				Content:        receiptContent(call, request),
				Type:           request.Type,
				SubjectID:      request.SubjectID,
				SubjectVersion: request.SubjectVersion,
				CorrelationID:  request.CorrelationID,
				CausationID:    request.CausationID,
				Data:           append([]byte(nil), request.Data...),
			}
			// The ingress derives the event's scope from the publisher it
			// receives. A tenant-wide credential naming a project resolves to
			// that project's scope; passing the credential's tenant-only scope
			// would widen the observation to a tenant-wide event.
			publisher := call.Principal
			publisher.Scope = call.Scope
			committed, err := deps.Events.Publish(ctx, publisher, attempt)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.EventPublishResult{
				EventID:   string(committed.EventID),
				ReceiptID: string(committed.ReceiptID),
				Replayed:  committed.Replayed,
			})
		}),
		"event.read": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.EventReadRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			page := request.Page
			if page == nil {
				page = &api.Page{}
			}
			after, err := decodeEventCursor(page.Cursor)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.EventRead.Read(ctx, event.ReadCommand{
				Actor: call.Principal,
				Scope: call.Scope,
				Query: event.ReadQuery{
					Scope:         call.Scope,
					AfterSequence: after,
					Types:         eventTypes(request.Types),
					Limit:         page.EffectiveLimit(),
				},
			})
			if err != nil {
				return api.Result{}, err
			}
			result := api.EventReadResult{HasMore: committed.HasMore}
			for _, envelope := range committed.Events {
				converted, err := publicEvent(envelope)
				if err != nil {
					return api.Result{}, err
				}
				result.Events = append(result.Events, converted)
			}
			result.Next, err = encodeEventCursor(committed.Next)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, result)
		}),
		"event.subscribe": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.EventSubscribeRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			cursor, err := decodeEventCursor(request.Cursor)
			if err != nil {
				return api.Result{}, err
			}
			grant, err := deps.EventSubscribe.Setup(ctx, event.SubscribeCommand{
				Actor: call.Principal,
				Scope: call.Scope,
				Setup: event.SubscribeSetup{
					Scope:  call.Scope,
					Types:  eventTypes(request.Types),
					Cursor: cursor,
				},
			})
			if err != nil {
				return api.Result{}, err
			}
			encoded, err := encodeEventCursor(grant.Cursor)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.EventSubscribeResult{
				Subjects:  append([]string(nil), grant.Subjects...),
				Cursor:    encoded,
				ExpiresAt: grant.ExpiresAt,
			})
		}),
	}
}

// eventTypes converts the public lifecycle type filter.
func eventTypes(names []string) []event.Type {
	if names == nil {
		return nil
	}
	types := make([]event.Type, len(names))
	for index, name := range names {
		types[index] = event.Type(name)
	}
	return types
}

// decodeEventCursor maps the opaque public cursor to the capability's exclusive
// decimal sequence. An empty cursor is the beginning.
func decodeEventCursor(cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	sortKey, err := api.DecodeCursor(cursor)
	if err != nil {
		return 0, wireRefusal(err)
	}
	sequence, err := strconv.ParseUint(sortKey, 10, 64)
	if err != nil || strconv.FormatUint(sequence, 10) != sortKey {
		return 0, fault.New(fault.InvalidInput, "event cursor does not carry a canonical decimal sequence")
	}
	return sequence, nil
}

// encodeEventCursor encodes the capability's sequence as the opaque public
// cursor. The next cursor is always present, including at sequence zero.
func encodeEventCursor(sequence uint64) (string, error) {
	encoded, err := api.EncodeCursor(strconv.FormatUint(sequence, 10))
	if err != nil {
		return "", wireRefusal(err)
	}
	return encoded, nil
}

// publicEvent maps one committed event envelope onto the public envelope. The
// type is canonicalized at the public edge; an unknown type is an internal
// contract failure rather than a fabricated value.
func publicEvent(envelope event.Envelope) (api.Event, error) {
	eventType, ok := api.CanonicalEventType(string(envelope.Type))
	if !ok {
		return api.Event{}, fault.New(fault.InternalFailure, "committed event carries an unknown lifecycle type")
	}
	return api.Event{
		ID:             string(envelope.ID),
		Type:           eventType,
		ReportType:     string(envelope.ReportType),
		Source:         api.EventSource{Kind: string(envelope.Source.Kind), PrincipalID: string(envelope.Source.PrincipalID)},
		At:             envelope.At,
		Scope:          api.EventScope{TenantID: string(envelope.Scope.TenantID), ProjectID: string(envelope.Scope.ProjectID)},
		SubjectID:      envelope.SubjectID,
		SubjectVersion: envelope.SubjectVersion,
		CorrelationID:  envelope.CorrelationID,
		CausationID:    envelope.CausationID,
		Data:           append([]byte(nil), envelope.Data...),
	}, nil
}
