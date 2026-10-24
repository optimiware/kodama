package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/logs/logstest"
)

var t0 = time.Now().UTC().Truncate(time.Microsecond)

func ctx() context.Context { return context.Background() }

// logRef uploads a diagnostic file as the wrapper would and returns its
// verified reference.
func logRef(t *testing.T, e *logstest.Env, a logstest.Attempt, name, text string) logs.ArtifactRef {
	t.Helper()
	id := e.WrapperUpload(a, name, contract.LogMediaType, []byte(text))
	return logs.ArtifactRef{ID: id, Digest: string(artifacts.DigestOf([]byte(text))), Size: int64(len(text))}
}

func newRecord(t *testing.T, e *logstest.Env, a logstest.Attempt, src logs.Source) *logs.Record {
	t.Helper()
	info := e.Info(a)
	tmpl, err := logs.NewRecord(a.Scope, logs.RandomIDs{}.NewRecordID(), a.ID, a.Run, info.Step, src, 1, t0)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := e.Logs.Ensure(ctx(), *tmpl)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return rec
}

func complete() logs.Statement { return logs.Statement{Completeness: logs.Complete} }

func wrapperLink(ref logs.ArtifactRef) logs.Link { return logs.Link{Log: ref, Statement: complete()} }

func observe(e *logstest.Env, scope domain.Scope, id logs.RecordID, l logs.Link) (*logs.Record, error) {
	return e.Logs.Update(ctx(), scope, id, func(r *logs.Record) (bool, logs.Audit, error) {
		ch, err := r.Observe(l, t0)
		return ch, logs.Audit{Action: "log.linked"}, err
	})
}
