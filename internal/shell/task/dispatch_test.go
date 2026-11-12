package task_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestDispatchRequiresPredecessorStopEvidence proves a queued execution is
// dispatched only when there is no predecessor or the predecessor's stop is
// affirmatively confirmed. An unreadable predecessor is unknown evidence, so
// the durable queued execution stays undispatched rather than starting beside
// possibly running work.
func TestDispatchRequiresPredecessorStopEvidence(t *testing.T) {
	t.Run("start with unknown predecessor", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
		// baseTask is open with a current execution but no readable execution
		// row: absence is not proof of stop.
		env.reader.put(baseTask())

		_, err := env.service.Start(t.Context(), startCommand(actor, 4))
		requireNoError(t, err, "Start")
		unit := env.ports.lastStart(t)
		requirePublish(t, unit.PostCommit)
		requireNoDispatch(t, unit.PostCommit)
	})

	t.Run("reply retry with unknown predecessor", func(t *testing.T) {
		env := newTestEnv(t)
		actor := humanActor(reviewerPrincipal, tenancy.RoleReviewer)
		env.reader.put(baseTask())
		env.ports.setReply(func(unit task.ReplyUnit) (task.ReplyResult, error) {
			return task.ReplyResult{ExecutionID: "execution-original", LifecycleVersion: 5, QuestionVersion: 2, Replayed: true}, nil
		})

		_, err := env.service.Reply(t.Context(), replyCommand(actor, 4, questionID, 1, "Answer"))
		requireNoError(t, err, "Reply retry")
		unit := env.ports.lastReply(t)
		requirePublish(t, unit.PostCommit)
		requireNoDispatch(t, unit.PostCommit)
	})
}
