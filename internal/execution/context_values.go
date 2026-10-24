package execution

import "context"

type ctxKey int

const (
	actorKey ctxKey = iota + 1
	requestIDKey
)

// WithActor attaches the actor recorded in the history, audit and events of the
// transactions made with ctx. Without it the module's own System actor is used.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey, a)
}

// ActorFrom returns the actor set by WithActor, or System.
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey).(Actor); ok && a.Validate() == nil {
		return a
	}
	return System
}

// WithRequestID attaches the request/correlation identifier recorded in audit
// and events.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the identifier set by WithRequestID, or "".
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}
