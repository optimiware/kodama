// Package integration holds the observation acceptance tests: real PostgreSQL
// under the non-owner runtime role, the real event store and its retention, the
// real identity authority over persisted memberships, the merged work tables,
// the shared router and a real HTTP server. They need no NATS, S3 or Git;
// delivery never depends on the broker. Skipped acceptance is not passing
// evidence: run with KODAMA_REQUIRE_POSTGRES=1.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/observation"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
	"github.com/optimiware/kodama/internal/storage"
)

const issuer = "https://issuer.t024.test"

type fixture struct {
	*ot.Env
	viewer, otherViewer, admin, projectOnly, tenantBViewer, outsider identity.UserID
	viewerBinding                                                    identity.BindingID
}

// newFixture seeds users: a tenant-wide viewer, a project-only viewer of the
// second project, an administrator (no work or run read), a viewer of the other
// tenant and an unbound user.
func newFixture(t testing.TB, o ot.Options) *fixture {
	t.Helper()
	e := ot.New(t, o)
	f := &fixture{Env: e}
	f.viewer = e.User(issuer, "viewer")
	f.otherViewer = e.User(issuer, "other-viewer")
	f.admin = e.User(issuer, "admin")
	f.projectOnly = e.User(issuer, "project-only")
	f.tenantBViewer = e.User(issuer, "tenant-b-viewer")
	f.outsider = e.User(issuer, "outsider")
	f.viewerBinding = e.Bind(ot.TenantA, f.viewer, "", identity.RoleViewer)
	e.Bind(ot.TenantA, f.otherViewer, "", identity.RoleEngineer)
	e.Bind(ot.TenantA, f.admin, "", identity.RoleAdmin)
	e.Bind(ot.TenantA, f.projectOnly, ot.ProjectA2, identity.RoleViewer)
	e.Bind(ot.TenantB, f.tenantBViewer, "", identity.RoleViewer)
	return f
}

// revoke removes the user's binding as a committed membership change would.
func (f *fixture) revoke(id identity.BindingID, user identity.UserID, tenant domain.TenantID) {
	f.T.Helper()
	f.OwnerExec(`DELETE FROM memberships WHERE tenant_id = $1 AND id = $2`, string(tenant), string(id))
	f.OwnerExec(`UPDATE identity_authz_revisions SET revision = revision + 1 WHERE tenant_id = $1 AND user_id = $2`, string(tenant), string(user))
}

// scopeOf builds a validated scope.
func scopeOf(t testing.TB, tenant domain.TenantID, project domain.ProjectID, run string, types ...string) observation.Scope {
	t.Helper()
	s, err := observation.NewScope(tenant, project, domain.RunID(run), types)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// collector is an observation.Sink recording everything, safe for concurrent reads.
type collector struct {
	mu        sync.Mutex
	snapshot  *observation.Snapshot
	events    []observation.Delivery
	positions []int64 // every position emitted by an event or checkpoint, in order
	resync    error
	onEvent   func(d observation.Delivery) error
	onFlush   func() error
}

func (c *collector) Snapshot(_ context.Context, s observation.Snapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = &s
	return nil
}

func (c *collector) Event(_ context.Context, d observation.Delivery) error {
	if c.onEvent != nil {
		if err := c.onEvent(d); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, d)
	c.positions = append(c.positions, d.Position)
	return nil
}

func (c *collector) Checkpoint(_ context.Context, p int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.positions = append(c.positions, p)
	return nil
}

func (c *collector) Heartbeat(context.Context) error { return nil }

func (c *collector) Flush(context.Context) error {
	if c.onFlush != nil {
		return c.onFlush()
	}
	return nil
}

func (c *collector) Resync(_ context.Context, cause error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resync = cause
	return nil
}

func (c *collector) eventCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func (c *collector) lastPosition() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.positions) == 0 {
		return -1
	}
	return c.positions[len(c.positions)-1]
}

func (c *collector) eventIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, d := range c.events {
		out[i] = d.Envelope.ID
	}
	return out
}

// run starts the stream and returns a function that waits for its end.
func run(t testing.TB, st *observation.Stream, sink observation.Sink) (cancel func(), done <-chan error) {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- st.Run(ctx, sink) }()
	t.Cleanup(cancelFn)
	return cancelFn, ch
}

func waitFor(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitEnd(t testing.TB, ch <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		t.Fatal("the stream did not end")
		return nil
	}
}

// bump commits one work-item state change and its event atomically and returns
// the new version. When rollback is set the transaction is abandoned after both
// the state change and the append, so neither may ever be observable.
func bump(e *ot.Env, tenant domain.TenantID, project domain.ProjectID, item, eventID string, rollback bool) (int64, error) {
	var version int64
	err := storage.WithTenant(context.Background(), e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE work_items SET state = 'active', version = version + 1, updated_at = now()
			WHERE tenant_id = kodama_tenant_id() AND project_id = $1 AND id = $2 RETURNING version`, string(project), item).Scan(&version); err != nil {
			return err
		}
		if _, err := e.Events.Append(ctx, tx, ot.WorkEvent(tenant, project, eventID, item, version)); err != nil {
			return err
		}
		if rollback {
			return errRollback
		}
		return nil
	})
	if err == errRollback {
		return 0, nil
	}
	return version, err
}

var errRollback = fmt.Errorf("rolled back on purpose")

// createWork inserts a work item with its created event in one transaction.
func createWork(e *ot.Env, tenant domain.TenantID, project domain.ProjectID, item, eventID string) error {
	_, err := e.TryCommit(tenant, ot.InsertWork(project, item, "backlog"), ot.WorkCreated(tenant, project, eventID, item))
	return err
}

// eventResource decodes the observed resource of a delivered event.
func versionOf(d observation.Delivery) (string, int64) {
	return d.Envelope.Resource.ID, d.Envelope.Resource.Version
}

var _ = json.Marshal
var _ = strings.Join
var _ eventsv1.Envelope

// jsonField reads a top-level string member of a JSON object.
func jsonField(t testing.TB, body []byte, name string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, body)
	}
	s, _ := m[name].(string)
	if s == "" {
		t.Fatalf("no %q in %s", name, body)
	}
	return s
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func newTestLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }
