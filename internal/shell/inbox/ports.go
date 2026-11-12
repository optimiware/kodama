package inbox

import "context"

// QueryPort returns one authoritative, stably paginated inbox page.
//
// The adapter applies the caller's current scope/roles at query time; it must
// not expose Save/Update/SetState/Persist or any generic repository method,
// because the inbox owns no mutable entity.
type QueryPort interface {
	Query(context.Context, Query) (Page, error)
}
