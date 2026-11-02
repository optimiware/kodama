package gitpushtest

import (
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// WantState fails the test unless the record's entry of ref has the state and
// reason (for other modules' tests that drive the real push service).
func WantState(t testing.TB, rec gitv1.PushRecord, ref string, state gitv1.EntryState, reason string) {
	t.Helper()
	wantState(t, rec, ref, state, reason)
}

// WantStatus fails the test unless the record has the status.
func WantStatus(t testing.TB, rec gitv1.PushRecord, status gitv1.PushStatus) {
	t.Helper()
	wantStatus(t, rec, status)
}

// MustIntent runs commit intent for the listed refs (all when none) and fails the
// test on error.
func (f *Fixture) MustIntent(pushID string, req gitv1.PushAuthorizeRequest, refs ...string) {
	f.T.Helper()
	f.mustIntent(pushID, req, refs...)
}
