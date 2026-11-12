package resource

import "context"

// BindingPort applies one complete binding-change commit unit.
//
// Apply is the only register/update mutation seam: the adapter must check the
// expected binding version and the supplied current-authority reads inside the
// same transaction as the binding write, receipt, audit and event publication
// intent. It must not expose Save/Update/SetState/Persist or any generic
// repository method.
type BindingPort interface {
	Apply(context.Context, BindingUnit) (BindingResult, error)
}

// BindingDeletePort applies one complete binding-deletion commit unit.
type BindingDeletePort interface {
	DeleteBinding(context.Context, BindingDeleteUnit) (BindingDeleteResult, error)
}

// BindingReadPort reads one binding by its stable identity inside the caller's
// scope. It is a read seam, never durable permission.
type BindingReadPort interface {
	ReadBinding(context.Context, BindingReadRequest) (Binding, error)
}

// BindingQueryPort returns one authoritative, stably paginated binding page.
// The adapter applies the caller's current scope at query time and must not
// expose a generic repository method.
type BindingQueryPort interface {
	QueryBindings(context.Context, BindingQuery) (BindingPage, error)
}
