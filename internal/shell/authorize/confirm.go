package authorize

import (
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
)

// Confirm compares the current authority facts an adapter read inside a commit
// with the reads the caller requested, and refuses the commit unless every fact
// answers exactly its read.
//
// Facts must be one per read and in input order; check, scope, principal and
// target identity must match. A fact read as not allowed is a forbidden
// refusal even when an earlier grant allowed the attempt. A versioned read whose
// observed version differs from the expected version is a version conflict. A
// fact that does not answer its read is an internal failure: the adapter
// answered something the caller never asked for. Confirm never mutates the
// reads, so the commit unit keeps the operation/grant context it was built
// with.
func Confirm(reads []AuthorityRead, facts []AuthorityFact) error {
	if len(facts) != len(reads) {
		return fault.New(fault.InternalFailure, fmt.Sprintf("authority reader returned %d facts for %d reads", len(facts), len(reads)))
	}
	for index, read := range reads {
		fact := facts[index]
		if !read.Check.Valid() {
			return fault.New(fault.InternalFailure, fmt.Sprintf("authority read %d carries an unknown check %q", index, read.Check))
		}
		if fact.Check != read.Check ||
			fact.Scope != read.Scope ||
			fact.PrincipalID != read.PrincipalID ||
			fact.TargetID != read.TargetID {
			return fault.New(fault.InternalFailure, fmt.Sprintf("authority fact %d does not answer its read", index))
		}
		if !fact.Allowed {
			return fault.New(fault.Forbidden, fmt.Sprintf("current authority check %q did not hold", read.Check))
		}
		if read.ExpectedVersion != 0 && fact.Version != read.ExpectedVersion {
			return fault.New(fault.VersionConflict, fmt.Sprintf("current authority check %q observed version %d; want %d", read.Check, fact.Version, read.ExpectedVersion))
		}
	}
	return nil
}
