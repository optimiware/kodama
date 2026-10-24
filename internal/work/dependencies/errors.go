package dependencies

import (
	"errors"
	"fmt"
)

// ErrGraph is the graph failure class: the requested link would make a work
// item depend on itself or close a cycle, or it exceeds the bounded number of
// prerequisites of one item. It is distinct from work.ErrPrerequisite, which is
// an admission refusal. The concrete error carries a fixed client-safe message
// and never a path, so it discloses no other work item.
//
// Every other failure reuses the work module's classes (work.ErrNotFound,
// work.ErrDenied, work.ErrHidden, work.ErrVersionConflict, work.ErrInvalid,
// work.ErrInvalidTransition, work.ErrUnavailable): hidden, foreign and missing
// references are all work.ErrNotFound.
var ErrGraph = errors.New("work dependencies: invalid dependency graph")

func graphf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrGraph, fmt.Sprintf(format, args...))
}
