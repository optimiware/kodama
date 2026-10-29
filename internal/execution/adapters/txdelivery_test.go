package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
)

// ---- a scripted transaction ---------------------------------------------------

// fakeRow assigns scripted column values to Scan's destinations.
type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("column count mismatch")
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(r.values[i]))
	}
	return nil
}

type query struct {
	sql  string
	args []any
}

// scriptedTx is the row reader of one open transaction: it records every
// statement and answers it from a table keyed by the table read.
type scriptedTx struct {
	snapshots   map[string]fakeRow // by environment|digest
	deployments map[string]fakeRow // by deployment id
	failure     error
	queries     []query
}

func (s *scriptedTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.queries = append(s.queries, query{sql: sql, args: args})
	if s.failure != nil {
		return fakeRow{err: s.failure}
	}
	switch {
	case contains(sql, "FROM environment_snapshots"):
		if r, ok := s.snapshots[args[2].(string)+"|"+args[3].(string)]; ok {
			return r
		}
	case contains(sql, "FROM deployments"):
		if r, ok := s.deployments[args[2].(string)]; ok {
			return r
		}
	}
	return fakeRow{err: pgx.ErrNoRows}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func snapshotRow(t *testing.T, s delivery.ConfigurationSnapshot) fakeRow {
	t.Helper()
	config, err := json.Marshal(s.Config)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := json.Marshal(s.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := json.Marshal(s.TargetArtifactKinds)
	if err != nil {
		t.Fatal(err)
	}
	egress, err := json.Marshal(s.TargetEgress)
	if err != nil {
		t.Fatal(err)
	}
	targetConfig, err := json.Marshal(s.TargetConfig)
	if err != nil {
		t.Fatal(err)
	}
	return fakeRow{values: []any{string(s.Scope.TenantID), string(s.Scope.ProjectID), string(s.Environment), string(s.Digest), string(s.Kind),
		s.Target, s.Namespace, config, int64(s.PreviewTTL / time.Second), s.Kubeconfig, secrets,
		s.TargetProfile, s.TargetProfileDigest, s.TargetProvider, kinds, egress, targetConfig, s.CreatedAt}}
}

func deploymentRow(t *testing.T, d delivery.Deployment) fakeRow {
	t.Helper()
	r := d.Record()
	objects, err := json.Marshal(r.Objects)
	if err != nil {
		t.Fatal(err)
	}
	resources, err := json.Marshal(r.Resources)
	if err != nil {
		t.Fatal(err)
	}
	return fakeRow{values: []any{string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.ID), string(r.Environment), string(r.ConfigDigest), r.Namespace,
		r.Source.RepositoryURL, r.Source.Commit, string(r.Image.ArtifactID), string(r.Image.Digest), string(r.Artifact.Kind), r.Artifact.MediaType,
		string(r.Producer.Attempt), string(r.Producer.Run), r.Producer.Step, r.Producer.ToolImage, string(r.Run), r.Step, r.ActionKey, string(r.Attempt),
		string(r.Fingerprint), string(r.Status), r.Reason, objects, resources, r.ExpiresAt, r.CleanupRequestedAt, string(r.CleanupState), r.CleanupReason,
		r.DeletedAt, int64(r.Version), r.CreatedAt, r.UpdatedAt}}
}

// access is the transaction-bound current-authority check.
type access struct {
	err   error
	calls []delivery.Permission
	order *[]string
}

func (a *access) Authorize(_ context.Context, actor delivery.Actor, sc domain.Scope, perm delivery.Permission) error {
	a.calls = append(a.calls, perm)
	if a.order != nil {
		*a.order = append(*a.order, "authorize")
	}
	if actor.ID != engnr.ID || sc != scope {
		return delivery.ErrDenied
	}
	return a.err
}

func txBinding(t *testing.T, tx *scriptedTx, a *access) execution.TxEnvironments {
	t.Helper()
	reader, err := adapters.NewTxDeliveryReader(tx, a)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := adapters.NewTxEnvironments(reader)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func workBinding(s delivery.ConfigurationSnapshot) execution.EnvironmentBinding {
	return execution.EnvironmentBinding{Scope: s.Scope, Actor: engnr, Pin: s.Pin(), Secrets: s.Secrets}
}

// Test_delivery_tx_environments proves the production transaction-bound reader
// behind execpg.Peers.Environments: the pin, the credential snapshot and the
// authority are all read through the admission transaction and scoped by tenant
// and project; a stale or revoked authority refuses, an outage is unavailable
// and nothing is admitted on an unreadable or foreign record.
func Test_delivery_tx_environments(t *testing.T) {
	ctx := context.Background()
	snap := snapshot(t, scope, map[string]string{"HOST": "staging.example.test"}, 7)
	key := string(snap.Environment) + "|" + string(snap.Digest)

	t.Run("reads the pin and the snapshot inside the transaction, scoped by tenant and project", func(t *testing.T) {
		tx := &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, snap)}}
		a := &access{}
		if err := txBinding(t, tx, a).BindEnvironment(ctx, workBinding(snap)); err != nil {
			t.Fatalf("a retained, matching snapshot under current authority binds: %v", err)
		}
		if len(a.calls) != 1 || a.calls[0] != delivery.PermDeploymentManage {
			t.Fatalf("authority checks %v, want exactly deployment:manage", a.calls)
		}
		if len(tx.queries) != 1 {
			t.Fatalf("%d statements, want exactly the snapshot read", len(tx.queries))
		}
		q := tx.queries[0]
		if q.args[0] != string(scope.TenantID) || q.args[1] != string(scope.ProjectID) || q.args[2] != string(snap.Environment) || q.args[3] != string(snap.Digest) {
			t.Fatalf("snapshot read args %v are not the admission's scope, environment and digest", q.args)
		}
	})

	t.Run("a revoked or stale authority refuses before anything is read", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err  error
			want error
		}{
			"revoked":      {err: delivery.ErrDenied, want: execution.ErrDenied},
			"hidden":       {err: delivery.ErrNotFound, want: execution.ErrAdmissionRefused},
			"outage":       {err: errors.New("authority down"), want: execution.ErrUnavailable},
			"unclassified": {err: delivery.Fail(delivery.ErrUnavailable, nil, "down"), want: execution.ErrUnavailable},
		} {
			tx := &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, snap)}}
			err := txBinding(t, tx, &access{err: tc.err}).BindEnvironment(ctx, workBinding(snap))
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
			if len(tx.queries) != 0 {
				t.Errorf("%s: the snapshot was read without authority", name)
			}
		}
		// A foreign actor holds no authority in this scope.
		tx := &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, snap)}}
		b := workBinding(snap)
		b.Actor = execution.Actor{Type: execution.ActorUser, ID: "u_foreign"}
		if err := txBinding(t, tx, &access{}).BindEnvironment(ctx, b); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("an actor without authority bound an environment: %v", err)
		}
	})

	t.Run("a missing or foreign snapshot refuses, an outage is unavailable", func(t *testing.T) {
		// Absent: nothing retained under the pin.
		err := txBinding(t, &scriptedTx{}, &access{}).BindEnvironment(ctx, workBinding(snap))
		if !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("an unretained pin: %v, want a refusal", err)
		}
		// Foreign: the row answers but belongs to another scope.
		other := domain.Scope{TenantID: scope.TenantID, ProjectID: "p_other"}
		foreign := snap
		foreign.Scope = other
		err = txBinding(t, &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, foreign)}}, &access{}).BindEnvironment(ctx, workBinding(snap))
		if err == nil || errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("a snapshot of another scope was accepted or reported as an outage: %v", err)
		}
		// Outage: the database cannot answer.
		err = txBinding(t, &scriptedTx{failure: errors.New("connection reset")}, &access{}).BindEnvironment(ctx, workBinding(snap))
		if !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("a database outage: %v, want unavailable", err)
		}
		// Corrupt: undecodable JSON is an outage, never a pass.
		bad := snapshotRow(t, snap)
		bad.values[7] = []byte("{")
		err = txBinding(t, &scriptedTx{snapshots: map[string]fakeRow{key: bad}}, &access{}).BindEnvironment(ctx, workBinding(snap))
		if !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("an unreadable snapshot: %v, want unavailable", err)
		}
	})

	t.Run("a changed credential set or digest is refused", func(t *testing.T) {
		tx := &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, snap)}}
		b := workBinding(snap)
		b.Secrets = nil
		if err := txBinding(t, tx, &access{}).BindEnvironment(ctx, b); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a binding missing the snapshot's credentials was admitted: %v", err)
		}
		other := snapshot(t, scope, map[string]string{"HOST": "other.example.test"}, 7)
		moved := snapshotRow(t, other)
		tx = &scriptedTx{snapshots: map[string]fakeRow{key: moved}}
		if err := txBinding(t, tx, &access{}).BindEnvironment(ctx, workBinding(snap)); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("content that does not match the pinned digest was admitted: %v", err)
		}
	})

	t.Run("a cleanup run binds its own deployment's retained configuration without actor authority", func(t *testing.T) {
		dep := deployment(t, snap)
		tx := &scriptedTx{
			snapshots:   map[string]fakeRow{key: snapshotRow(t, snap)},
			deployments: map[string]fakeRow{string(dep.ID()): deploymentRow(t, dep)},
		}
		a := &access{}
		b := execution.EnvironmentBinding{Scope: scope, Actor: execution.System, Pin: snap.Pin(), Secrets: snap.Secrets, Deployment: string(dep.ID())}
		if err := txBinding(t, tx, a).BindEnvironment(ctx, b); err != nil {
			t.Fatalf("cleanup requested and not complete: %v", err)
		}
		if len(a.calls) != 0 {
			t.Fatalf("a cleanup run was authorized as a user: %v", a.calls)
		}
		if len(tx.queries) != 2 || tx.queries[1].args[0] != string(scope.TenantID) || tx.queries[1].args[1] != string(scope.ProjectID) || tx.queries[1].args[2] != string(dep.ID()) {
			t.Fatalf("deployment read %v, want one scoped read inside the transaction", tx.queries)
		}

		// A deployment of another project is not found, never a grant.
		foreignDep := deploymentRow(t, dep)
		foreignDep.values[1] = "p_other"
		tx = &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, snap)}, deployments: map[string]fakeRow{string(dep.ID()): foreignDep}}
		if err := txBinding(t, tx, &access{}).BindEnvironment(ctx, b); err == nil || errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("a deployment of another project bound a cleanup run: %v", err)
		}
		// Cleanup not requested (or deleted) is refused.
		rec := dep.Record()
		rec.CleanupRequestedAt, rec.CleanupState = nil, delivery.CleanupNone
		live, err := delivery.RestoreDeployment(rec)
		if err != nil {
			t.Fatal(err)
		}
		tx = &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, snap)}, deployments: map[string]fakeRow{string(dep.ID()): deploymentRow(t, live)}}
		if err := txBinding(t, tx, &access{}).BindEnvironment(ctx, b); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("a cleanup that was never requested bound: %v", err)
		}
		tx = &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, snap)}, deployments: map[string]fakeRow{string(dep.ID()): deploymentRow(t, withDeleted(t, dep))}}
		if err := txBinding(t, tx, &access{}).BindEnvironment(ctx, b); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("a deleted deployment bound a cleanup run: %v", err)
		}
		// An outage reading the deployment is unavailable.
		tx = &scriptedTx{snapshots: map[string]fakeRow{key: snapshotRow(t, snap)}}
		tx.deployments = nil
		failing := &failAfter{scriptedTx: tx, after: 1, err: errors.New("db down")}
		reader, err := adapters.NewTxDeliveryReader(failing, &access{})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := adapters.NewTxEnvironments(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := bound.BindEnvironment(ctx, b); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("an outage reading the deployment: %v, want unavailable", err)
		}
	})

	t.Run("a reader without its collaborators cannot be built", func(t *testing.T) {
		if _, err := adapters.NewTxDeliveryReader(nil, &access{}); err == nil {
			t.Error("a reader without a transaction was built")
		}
		if _, err := adapters.NewTxDeliveryReader(&scriptedTx{}, nil); err == nil {
			t.Error("a reader without an authority check was built")
		}
	})
}

// failAfter answers the first reads and then fails.
type failAfter struct {
	*scriptedTx
	after int
	err   error
	n     int
}

func (f *failAfter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	f.n++
	if f.n > f.after {
		return fakeRow{err: f.err}
	}
	return f.scriptedTx.QueryRow(ctx, sql, args...)
}

// A binder that cannot be joined to a transaction refuses as an outage; it
// never admits without the check.
func Test_delivery_tx_environments_unbound(t *testing.T) {
	err := adapters.BindDeliveryEnvironments(nil).BindEnvironment(context.Background(), execution.EnvironmentBinding{Scope: scope})
	if !errors.Is(err, execution.ErrUnavailable) && !errors.Is(err, execution.ErrAdmissionRefused) {
		t.Fatalf("an unbound binder: %v, want a closed failure", err)
	}
}
