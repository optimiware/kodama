package postgres_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/storage"
)

// Real-SQL coverage of the run-resource grants (T-113): the append-only
// migration, tenant isolation, one-time consumption, replay, revocation ordered
// against a stop, and the immutability the triggers and column grants enforce.
// The resource authority is a fixture over a real table (rtest.ResourceAuthority);
// execution's own committed state is bridged by T-116. These tests skip in the
// unit layer and without PostgreSQL.

var (
	rScope   = domain.Scope{TenantID: "t113_acme", ProjectID: "p113_inventory"}
	rRun     = domain.RunID("run_113")
	rUID     = "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f"
	rNonce   = "nnnnnnnnnnnnnnnnnnnnnn"
	rSvcUID  = "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071"
	rPodName = "kr-0123456789abcdef01234567-x7k2p"
	rPodUID  = "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"
)

type resStack struct {
	*rtest.Stack
	prep, svc runtimeauth.ResourceRef
	deadline  time.Time
}

func seedResources(t *testing.T) *resStack {
	t.Helper()
	s := rtest.New(t, rtest.Options{Conns: 32})
	s.SeedProject(rScope)
	r := &resStack{Stack: s, deadline: s.Clock.Now().Add(time.Hour)}
	f := func(uid string) rtest.ResourceFixture {
		return rtest.ResourceFixture{Scope: rScope, RunID: rRun, WorkloadUID: uid, DeadlineAt: r.deadline}
	}
	binding := runtimev1.SecretBinding{Ref: "secret://project/test-db-password", Version: 3, Name: "postgres-password", Delivery: runtimev1.DeliveryFile}
	s.Resources.Put(rtest.PreparationState(f(rUID)))
	s.Resources.Put(rtest.ServiceState(f(rSvcUID), "db", []runtimev1.SecretBinding{binding}))
	s.Resources.BindPod(rUID, rPodName, rPodUID)
	r.prep = runtimeauth.ResourceRef{Scope: rScope, RunID: rRun, Key: "preparation"}
	r.svc = runtimeauth.ResourceRef{Scope: rScope, RunID: rRun, Key: "service.db"}
	return r
}

func (r *resStack) register(ref runtimeauth.ResourceRef) runtimeauth.Credential {
	r.T.Helper()
	st, err := r.Resources.ObserveResource(bg, ref)
	if err != nil {
		r.T.Fatal(err)
	}
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		r.T.Fatal(err)
	}
	if err := r.Service.RegisterResource(bg, runtimeauth.ResourceRegisterCommand{
		Scope: ref.Scope, RunID: ref.RunID, Key: ref.Key, Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt,
	}); err != nil {
		r.T.Fatalf("register %s: %v", ref.Key, err)
	}
	return cred
}

func (r *resStack) request(ref runtimeauth.ResourceRef, cred runtimeauth.Credential, nonce string) runtimev2.ResourceAdmitRequest {
	st, _ := r.Resources.ObserveResource(bg, ref)
	id := runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID}
	if ref.Key == "preparation" {
		n, u := rPodName, rPodUID
		id.PodName, id.PodUID = &n, &u
	}
	return runtimev2.ResourceAdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: nonce, ClientVersion: "1.4.0", Identity: id}
}

func (r *resStack) revoked(ref runtimeauth.ResourceRef) (consumed, revoked bool) {
	r.T.Helper()
	var s, v *time.Time
	if err := r.Owner.QueryRow(bg, `SELECT consumed_at, revoked_at FROM runtime_resource_grants WHERE run_id = $1 AND resource_key = $2`, string(ref.RunID), ref.Key).Scan(&s, &v); err != nil {
		r.T.Fatal(err)
	}
	return s != nil, v != nil
}

// Test_resource_grants_migration: the contribution applies after the baseline,
// reapplies as a no-op and creates the constraints and guards the module relies on.
func Test_resource_grants_migration(t *testing.T) {
	manifest, err := storage.NewManifest(storage.BaseContribution(), rtpg.ResourceMigrations())
	if err != nil {
		t.Fatal(err)
	}
	var manifestIDs []string
	for _, m := range manifest.Migrations() {
		manifestIDs = append(manifestIDs, m.ID)
	}
	baseline := []string{"001_schema.sql", "002_grants.sql", "003_git_hosting.sql", "004_pipeline_runtime_expand.sql"}
	at := slices.Index(manifestIDs, rtpg.ResourceGrantsMigrationID)
	if at != len(baseline) || !slices.Equal(manifestIDs[:at], baseline) {
		t.Fatalf("the contribution is not appended directly after the four-file baseline: %v", manifestIDs)
	}
	s := rtest.New(t, rtest.Options{})
	opts := storage.MigrateOptions{RuntimeRole: s.DB.RuntimeRole}
	again, err := storage.Migrate(bg, s.Owner, manifest, opts)
	if err != nil || len(again.Applied) != 0 {
		t.Fatalf("reapply: %+v %v", again, err)
	}
	for _, name := range []string{
		"runtime_resource_grants_credential_unique", "runtime_resource_grants_session_unique", "runtime_resource_grants_consumption_complete",
		"runtime_resource_grants_key_syntax", "runtime_resource_grants_kind_known", "runtime_resource_grants_expiry_order",
		"runtime_resource_grants_lifetime_order", "runtime_resource_grants_fingerprint_syntax", "runtime_resource_grants_pkey",
		"runtime_resource_grants_tenant_id_project_id_fkey",
	} {
		var n int
		if err := s.Owner.QueryRow(bg, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = 'runtime_resource_grants'::regclass`, name).Scan(&n); err != nil || n != 1 {
			t.Errorf("constraint %s missing (%d, %v)", name, n, err)
		}
	}
	// The baseline ledger is untouched: the new migration only appends.
	var applied []string
	rows, err := s.Owner.Query(bg, `SELECT id FROM schema_migrations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		applied = append(applied, id)
	}
	rows.Close()
	// The ledger is exactly the manifest, and the contribution is found by its ID.
	if !slices.Equal(applied, manifestIDs) || !slices.Contains(applied, rtpg.ResourceGrantsMigrationID) ||
		!slices.Equal(applied[:4], []string{"001_schema.sql", "002_grants.sql", "003_git_hosting.sql", "004_pipeline_runtime_expand.sql"}) {
		t.Fatalf("ledger = %v, manifest = %v", applied, manifestIDs)
	}
}

func Test_resource_grants_isolation_and_immutability(t *testing.T) {
	r := seedResources(t)
	cred := r.register(r.svc)

	scopeB := domain.Scope{TenantID: "t113_globex", ProjectID: "p113_ledger"}
	r.SeedProject(scopeB)
	other := rtest.ServiceState(rtest.ResourceFixture{Scope: scopeB, RunID: rRun, WorkloadUID: rSvcUID, DeadlineAt: r.deadline}, "db", nil)
	r.Resources.Put(other)
	refB := runtimeauth.ResourceRef{Scope: scopeB, RunID: rRun, Key: "service.db"}
	credB := r.register(refB)

	count := func(tenant domain.TenantID) (n int) {
		t.Helper()
		if err := storage.WithTenant(bg, r.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM runtime_resource_grants`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(rScope.TenantID) != 1 || count(scopeB.TenantID) != 1 {
		t.Fatal("each tenant sees exactly its own resource grant")
	}
	var none int
	if err := r.Runtime.QueryRow(bg, `SELECT count(*) FROM runtime_resource_grants`).Scan(&none); err != nil || none != 0 {
		t.Fatalf("no tenant context sees %d grants (%v)", none, err)
	}

	// The restricted lookup returns only the owner locator of the one digest.
	loc, err := r.ResourceStore.LocateResource(bg, cred.Digest())
	if err != nil || loc.Ref != r.svc {
		t.Fatalf("locate: %+v %v", loc, err)
	}
	var unknown runtimeauth.Digest
	unknown[0] = 1
	if _, err := r.ResourceStore.LocateResource(bg, unknown); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
		t.Fatalf("an unknown digest: %v", err)
	}
	// A credential names its own tenant's resource: it never opens another
	// route, and admitting B's resource leaves A's grant untouched.
	if _, err := r.Service.AdmitResource(bg, runtimeauth.ResourceRef{RunID: "run_other", Key: "service.db"}, r.request(refB, credB, rNonce)); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
		t.Fatalf("a credential on another run's route: %v", err)
	}
	if _, err := r.Service.AdmitResource(bg, r.svc, r.request(refB, credB, rNonce)); err != nil {
		t.Fatalf("tenant B admitting its own resource: %v", err)
	}
	if consumed, _ := r.revoked(r.svc); consumed {
		t.Fatal("tenant B's admission consumed tenant A's grant")
	}
	if err := r.Service.RevokeResource(bg, runtimeauth.ResourceRef{Scope: scopeB, RunID: rRun, Key: "service.db"}); err != nil {
		t.Fatal(err)
	}
	if _, revoked := r.revoked(r.svc); revoked {
		t.Fatal("B's revocation reached A's grant")
	}

	// Least privilege and write-once transitions.
	for _, stmt := range []string{
		`DELETE FROM runtime_resource_grants`,
		`UPDATE runtime_resource_grants SET spec_fingerprint = 'sha256:` + strings.Repeat("e", 64) + `'`,
		`UPDATE runtime_resource_grants SET deadline_at = deadline_at + interval '1 day'`,
	} {
		err := storage.WithTenant(bg, r.Runtime, rScope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("%q by the runtime role: %v (want insufficient_privilege)", stmt, err)
		}
	}
	// The owner is stopped by the guard triggers even against registration edits.
	if _, err := r.Owner.Exec(bg, `UPDATE runtime_resource_grants SET spec_fingerprint = 'sha256:`+strings.Repeat("e", 64)+`' WHERE tenant_id = $1`, string(rScope.TenantID)); pgCode(err) != "23514" {
		t.Errorf("registration edit by the owner: %v", err)
	}
	// Consumption is write-once; revocation irreversible.
	if _, err := r.Service.AdmitResource(bg, r.svc, r.request(r.svc, cred, rNonce)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Owner.Exec(bg, `UPDATE runtime_resource_grants SET nonce_digest = decode(repeat('00', 32), 'hex') WHERE tenant_id = $1`, string(rScope.TenantID)); pgCode(err) != "23514" {
		t.Errorf("consumption rewrite: %v", err)
	}
	if err := r.Service.RevokeResource(bg, r.svc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Owner.Exec(bg, `UPDATE runtime_resource_grants SET revoked_at = NULL WHERE tenant_id = $1`, string(rScope.TenantID)); pgCode(err) != "23514" {
		t.Errorf("revocation reversal: %v", err)
	}
}

func Test_resource_grants_admission_replay_and_concurrency(t *testing.T) {
	r := seedResources(t)
	cred := r.register(r.prep)
	// Idempotent registration after a lost response, conflicting otherwise.
	st, _ := r.Resources.ObserveResource(bg, r.prep)
	other, _ := runtimeauth.GenerateCredential(nil)
	if err := r.Service.RegisterResource(bg, runtimeauth.ResourceRegisterCommand{Scope: rScope, RunID: rRun, Key: "preparation", Credential: other, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt}); !errors.Is(err, runtimeauth.ErrConflict) {
		t.Fatalf("another credential for the same resource: %v", err)
	}

	// Simultaneous same-nonce exchanges converge on one consumption and session.
	const n = 16
	resps := make([]runtimev2.ResourceAdmitResponse, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resps[i], errs[i] = r.Service.AdmitResource(bg, r.prep, r.request(r.prep, cred, rNonce))
		}()
	}
	close(start)
	wg.Wait()
	var session string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("exchange %d: %v", i, err)
		}
		claims, err := r.Verifier.VerifyResource(bg, resps[i].Token.Token)
		if err != nil {
			t.Fatal(err)
		}
		if session == "" {
			session = string(claims.Session)
		}
		if string(claims.Session) != session || resps[i].Token.MaxExpiresAt != resps[0].Token.MaxExpiresAt {
			t.Fatalf("exchange %d: another session or maximum", i)
		}
	}
	var rows int
	if err := r.Owner.QueryRow(bg, `SELECT count(session_id) FROM runtime_resource_grants WHERE resource_key = 'preparation'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("consumptions: %d %v", rows, err)
	}
	// Another nonce after the consumption.
	if _, err := r.Service.AdmitResource(bg, r.prep, r.request(r.prep, cred, "mmmmmmmmmmmmmmmmmmmmmm")); !errors.Is(err, runtimeauth.ErrAdmissionConsumed) {
		t.Fatalf("another nonce: %v", err)
	}
	// Token operations on the real store; maximum never beyond the deadline.
	renewed, err := r.Service.RenewResource(bg, rRun, "preparation", resps[0].Token.Token)
	if err != nil {
		t.Fatal(err)
	}
	maxAt, _ := renewed.Token.MaxExpiresAt.Time()
	if maxAt.After(r.deadline) {
		t.Fatalf("maximum %s beyond the deadline %s", maxAt, r.deadline)
	}
	if _, err := r.Service.AuthorizeResource(bg, runtimeauth.ResourceAuthorizeRequest{RunID: rRun, Key: "preparation", Bearer: renewed.Token.Token, Op: runtimeauth.OpResourceComplete}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Service.AuthorizeResource(bg, runtimeauth.ResourceAuthorizeRequest{RunID: rRun, Key: "preparation", Bearer: renewed.Token.Token, Op: runtimeauth.OpResourceSecrets}); !errors.Is(err, runtimeauth.ErrForbidden) {
		t.Fatalf("a preparation resolving secrets: %v", err)
	}
}

// Test_resource_stop_admit_race: a stop that commits first denies the admission
// (and it consumes nothing); an admission that commits first is then ended by the
// stop; neither interleaves, because the stop's lock conflicts with the grant
// transaction's lock on the resource state.
func Test_resource_stop_admit_race(t *testing.T) {
	t.Run("stop first", func(t *testing.T) {
		r := seedResources(t)
		cred := r.register(r.svc)
		if err := r.Resources.Stop(bg, r.svc, true); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Service.AdmitResource(bg, r.svc, r.request(r.svc, cred, rNonce)); !errors.Is(err, runtimeauth.ErrResourceRevoked) {
			t.Fatalf("admission after a stop: %v", err)
		}
		if consumed, _ := r.revoked(r.svc); consumed {
			t.Fatal("a denied admission consumed the grant")
		}
	})

	t.Run("stop in flight, owner did not revoke", func(t *testing.T) {
		r := seedResources(t)
		cred := r.register(r.svc)
		held, release := make(chan struct{}), make(chan struct{})
		r.Resources.SetStopPause(func() { close(held); <-release })
		done := make(chan error, 1)
		go func() { done <- r.Resources.Stop(bg, r.svc, false) }()
		<-held
		admitted := make(chan error, 1)
		go func() {
			_, err := r.Service.AdmitResource(bg, r.svc, r.request(r.svc, cred, rNonce))
			admitted <- err
		}()
		select {
		case err := <-admitted:
			t.Fatalf("the admission did not wait for the stop's lock: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-admitted; !errors.Is(err, runtimeauth.ErrResourceRevoked) {
			t.Fatalf("the admission after the stop committed: %v", err)
		}
		if consumed, revoked := r.revoked(r.svc); consumed || !revoked {
			t.Fatalf("the denied admission must revoke without consuming (consumed=%v revoked=%v)", consumed, revoked)
		}
	})

	t.Run("admitted tokens stop working once the state ended", func(t *testing.T) {
		r := seedResources(t)
		cred := r.register(r.svc)
		resp, err := r.Service.AdmitResource(bg, r.svc, r.request(r.svc, cred, rNonce))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Resources.Stop(bg, r.svc, false); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Service.RenewResource(bg, rRun, "service.db", resp.Token.Token); !errors.Is(err, runtimeauth.ErrResourceRevoked) {
			t.Fatalf("renewal after a stop: %v", err)
		}
		// Run cancellation revokes through the module's transaction seam.
		r2 := seedResources(t)
		c2 := r2.register(r2.prep)
		if _, err := r2.Service.AdmitResource(bg, r2.prep, r2.request(r2.prep, c2, rNonce)); err != nil {
			t.Fatal(err)
		}
		if err := storage.WithTenant(bg, r2.Runtime, rScope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			n, err := rtpg.RevokeRunResources(ctx, tx, rScope, rRun, r2.Clock.Now())
			if err == nil && n != 1 {
				t.Errorf("revoked %d grants", n)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, revoked := r2.revoked(r2.prep); !revoked {
			t.Fatal("the run revocation did not reach the grant")
		}
	})
}
