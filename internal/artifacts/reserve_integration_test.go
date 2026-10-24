package artifacts_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
)

func strp(s string) *string { return &s }

// TestReservationLifecycle covers the service's reservation rules over real
// PostgreSQL and S3: replay, conflict, reauthorization, declarations, limits,
// transfer bounds and the OCI registration path.
func TestReservationLifecycle(t *testing.T) {
	data := []byte("reservation subject")

	t.Run("new reservation, identical replay, changed metadata", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		in := artifactstest.FileInput("report", artifacts.RoleOutput, data)
		first, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "req_1")
		if err != nil || !first.Created || first.State != artifacts.StateReserved || first.Upload == nil || first.Upload.Method != "PUT" {
			t.Fatalf("first: %+v %v", first, err)
		}
		if strings.Contains(first.Upload.URL, "artifacts/") == false {
			t.Fatal("instruction should address a staging key")
		}
		before := env.Authority.Calls()
		second, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "req_2")
		if err != nil || second.Created || second.ID != first.ID || second.Upload == nil {
			t.Fatalf("replay: %+v %v", second, err)
		}
		if env.Authority.Calls() <= before {
			t.Fatal("a replay must reauthorize")
		}
		if second.Upload.ExpiresAt.Before(first.Upload.ExpiresAt) {
			t.Fatal("a renewed instruction cannot expire earlier than the one it renews")
		}
		if n := env.Count(`SELECT count(*) FROM artifacts WHERE attempt_id = $1`, string(attempt)); n != 1 {
			t.Fatalf("%d artifacts for one name", n)
		}
		// Instructions are never persisted.
		if n := env.Count(`SELECT count(*) FROM artifacts WHERE staging_key LIKE '%X-Amz%'`); n != 0 {
			t.Fatal("a bearer URL was persisted")
		}
		for name, change := range map[string]func(*artifacts.ReserveInput){
			"digest": func(i *artifacts.ReserveInput) { i.Digest = artifacts.DigestOf([]byte("other")) },
			"size":   func(i *artifacts.ReserveInput) { n := int64(1); i.Size = &n },
			"media":  func(i *artifacts.ReserveInput) { i.MediaType = strp("application/json") },
		} {
			c := in
			change(&c)
			if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, c, "req_3"); !errors.Is(err, artifacts.ErrIdempotencyConflict) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A different role under the same name is also a conflict, whichever is declared.
		diag := artifactstest.FileInput("report", artifacts.RoleDiagnostic, data)
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, diag, "req_4"); !errors.Is(err, artifacts.ErrIdempotencyConflict) {
			t.Errorf("role: %v", err)
		}
	})

	t.Run("replay after completion keeps the reservation shape and the content", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatal(err)
		}
		before, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		out, err := env.Service.Reserve(env.Ctx(), caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "req")
		if err != nil || out.Created || out.State != artifacts.StateReserved || out.Upload == nil || out.ID != id {
			t.Fatalf("replay: %+v %v", out, err)
		}
		after, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if after.State() != artifacts.StateUsable || after.Version() != before.Version() || after.SealedKey() != before.SealedKey() {
			t.Fatal("a replay changed usable content")
		}
	})

	t.Run("every call reauthorizes and fails closed", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		in := artifactstest.FileInput("report", artifacts.RoleOutput, data)
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); err != nil {
			t.Fatal(err)
		}
		env.Authority.Revoke(attempt)
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); !errors.Is(err, artifacts.ErrAttemptInactive) {
			t.Fatalf("a revoked attempt's replay: %v", err)
		}
		env2, caller2 := setup(t, artifactstest.Options{})
		env2.Authority.Fail(errors.New("authority unreachable"))
		if _, err := env2.Service.Reserve(env2.Ctx(), caller2, attempt, in, "r"); !errors.Is(err, artifacts.ErrUnavailable) {
			t.Fatalf("an authority outage must deny: %v", err)
		}
		// Unknown credential, and another attempt addressed with a valid token.
		env2.Authority.Fail(nil)
		if _, err := env2.Service.Reserve(env2.Ctx(), artifacts.Caller{Subject: "stranger"}, attempt, in, "r"); !errors.Is(err, artifacts.ErrAuthorityMissing) {
			t.Fatalf("unknown caller: %v", err)
		}
		if _, err := env2.Service.Reserve(env2.Ctx(), caller2, "att_other", in, "r"); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("another attempt: %v", err)
		}
		if n := env2.Count(`SELECT count(*) FROM artifacts`); n != 0 {
			t.Fatal("a denied call stored an artifact")
		}
		// Missing wiring is a construction error, not a permissive default.
		cfg := env.Config
		cfg.Authority = nil
		if _, err := artifacts.NewService(cfg); err == nil {
			t.Fatal("a service without an attempt authority was constructed")
		}
		for name, mutate := range map[string]func(*artifacts.ServiceConfig){
			"repository": func(c *artifacts.ServiceConfig) { c.Repository = nil },
			"reader":     func(c *artifacts.ServiceConfig) { c.Reader = nil },
			"objects":    func(c *artifacts.ServiceConfig) { c.Objects = nil },
			"registry":   func(c *artifacts.ServiceConfig) { c.Registry = nil },
			"ttl":        func(c *artifacts.ServiceConfig) { c.UploadTTL = 0 },
			"lease":      func(c *artifacts.ServiceConfig) { c.ClaimLease = 0 },
		} {
			c := env.Config
			mutate(&c)
			if _, err := artifacts.NewService(c); err == nil {
				t.Errorf("%s: constructed without it", name)
			}
		}
	})

	t.Run("declarations come from trusted context", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		ctx := env.Ctx()
		reserve := func(in artifacts.ReserveInput) error {
			_, err := env.Service.Reserve(ctx, caller, attempt, in, "r")
			return err
		}
		if err := reserve(artifactstest.FileInput("undeclared", artifacts.RoleOutput, data)); !errors.Is(err, artifacts.ErrOutputInvalid) {
			t.Errorf("an undeclared output: %v", err)
		}
		oci := artifacts.ReserveInput{Name: "report", Kind: artifacts.KindOCI, Role: artifacts.RoleOutput, Digest: artifacts.DigestOf(data), Registry: strp("registry.example.test/x")}
		if err := reserve(oci); !errors.Is(err, artifacts.ErrOutputInvalid) {
			t.Errorf("an output declared as a file registered as OCI: %v", err)
		}
		if err := reserve(artifactstest.FileInput("log", artifacts.RoleDiagnostic, data)); err != nil {
			t.Errorf("diagnostics need no declaration: %v", err)
		}
		if err := reserve(artifactstest.FileInput("source-bundle", artifacts.RoleBundle, data)); err != nil {
			t.Errorf("a writer may produce a bundle: %v", err)
		}
		env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) { g.Writer = false })
		if err := reserve(artifactstest.FileInput("other-bundle", artifacts.RoleBundle, data)); !errors.Is(err, artifacts.ErrOutputInvalid) {
			t.Errorf("a nonwriter's bundle: %v", err)
		}
		// Malformed declarations never reach storage.
		bad := artifactstest.FileInput("../escape", artifacts.RoleDiagnostic, data)
		if err := reserve(bad); !errors.Is(err, artifacts.ErrInvalid) {
			t.Errorf("bad name: %v", err)
		}
		// Provenance and producer come from the grant, not the request.
		a, _ := env.Store.FindByName(ctx, artifactstest.ScopeA, attempt, "log")
		if a.Provenance() != artifactstest.Source || a.Reservation().Run != "run_1" || a.Reservation().Step != "build" {
			t.Fatalf("provenance/producer: %+v", a.Reservation())
		}
	})

	t.Run("aggregate limits come from the attempt context", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) {
			g.Limits = artifacts.Limits{OutputBytes: 100, OutputFiles: 1, DiagnosticBytes: 10}
			g.Outputs = append(g.Outputs, artifacts.OutputDecl{Name: "second", Kind: artifacts.KindFile})
		})
		ctx := env.Ctx()
		if _, err := env.Service.Reserve(ctx, caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "r"); err != nil {
			t.Fatal(err)
		}
		if _, err := env.Service.Reserve(ctx, caller, attempt, artifactstest.FileInput("second", artifacts.RoleOutput, data), "r"); !errors.Is(err, artifacts.ErrLimitExceeded) || !errors.Is(err, artifacts.ErrOutputInvalid) {
			t.Fatalf("file count: %v", err)
		}
		big := make([]byte, 11)
		if _, err := env.Service.Reserve(ctx, caller, attempt, artifactstest.FileInput("log", artifacts.RoleDiagnostic, big), "r"); !errors.Is(err, artifacts.ErrLimitExceeded) {
			t.Fatalf("diagnostic bytes: %v", err)
		}
		// Replaying a reservation that already exists is not new consumption.
		if _, err := env.Service.Reserve(ctx, caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "r"); err != nil {
			t.Fatalf("replay under a full allowance: %v", err)
		}
	})

	t.Run("instructions never outlive the attempt's authority and renewal needs it", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{UploadTTL: time.Hour})
		bound := env.Clock.Now().Add(90 * time.Second)
		env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) { g.ExpiresAt = bound })
		in := artifactstest.FileInput("report", artifacts.RoleOutput, data)
		out, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r")
		if err != nil {
			t.Fatal(err)
		}
		if out.Upload.ExpiresAt.After(bound.Add(time.Second)) {
			t.Fatalf("instruction valid until %v, beyond the authority bound %v", out.Upload.ExpiresAt, bound)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, out.ID)
		if a.TransferExpiresAt() == nil || a.TransferExpiresAt().After(bound) {
			t.Fatalf("recorded transfer expiry %v exceeds the bound", a.TransferExpiresAt())
		}
		env.Clock.Advance(2 * time.Minute)
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); !errors.Is(err, artifacts.ErrAttemptInactive) {
			t.Fatalf("renewal after the authority expired: %v", err)
		}
		// Renewed authority yields a fresh instruction again; the recorded expiry only moves forward.
		env.Renew(caller)
		again, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, out.ID)
		if !b.TransferExpiresAt().After(*a.TransferExpiresAt()) || again.Upload == nil {
			t.Fatal("renewed instruction did not extend the recorded horizon")
		}
	})

	t.Run("identical concurrent reservations yield one identity", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		const n = 10
		ids := make([]domain.ArtifactID, n)
		created := make([]bool, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out, err := env.Service.Reserve(env.Ctx(), caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), fmt.Sprintf("r%d", i))
				ids[i], created[i], errs[i] = out.ID, out.Created, err
			}()
		}
		close(start)
		wg.Wait()
		creators := 0
		for i := range ids {
			if errs[i] != nil || ids[i] != ids[0] {
				t.Fatalf("%d: %v %s", i, errs[i], ids[i])
			}
			if created[i] {
				creators++
			}
		}
		if creators != 1 {
			t.Fatalf("%d callers reported creating the artifact", creators)
		}
	})

	t.Run("an unknown commit outcome is not success and the retry replays the commit", func(t *testing.T) {
		fr, _, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		in := artifactstest.FileInput("report", artifacts.RoleOutput, data)
		fr.Set(func(f *artifactstest.FaultRepo) {
			f.OnReserve = func(_ artifacts.ReserveCommand, next func() (artifacts.ReserveResult, error)) (artifacts.ReserveResult, error) {
				if _, err := next(); err != nil {
					return artifacts.ReserveResult{}, err
				}
				return artifacts.ReserveResult{}, errors.New("connection lost after commit")
			}
		})
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); !errors.Is(err, artifacts.ErrUnavailable) {
			t.Fatalf("an unconfirmed reservation must not be success: %v", err)
		}
		fr.Set(func(f *artifactstest.FaultRepo) { f.OnReserve = nil })
		out, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r")
		if err != nil || out.Created || out.Upload == nil {
			t.Fatalf("the retry must observe the committed reservation: %+v %v", out, err)
		}
		if n := env.Count(`SELECT count(*) FROM artifacts`); n != 1 {
			t.Fatalf("%d artifacts", n)
		}
	})

	t.Run("OCI registration is stored only after verification", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		digest := artifacts.DigestOf([]byte("manifest"))
		in := artifacts.ReserveInput{Name: "image", Kind: artifacts.KindOCI, Role: artifacts.RoleOutput, Digest: digest, Registry: strp("registry.example.test/team/app:dev")}

		// Content unknown to the registry.
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); !errors.Is(err, artifacts.ErrOutputInvalid) {
			t.Fatalf("unverifiable content: %v", err)
		}
		// Registry outage is retryable and stores nothing.
		env.Registry.Fail(artifacts.ErrRegistryUnavailable)
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); !errors.Is(err, artifacts.ErrUnavailable) {
			t.Fatalf("outage: %v", err)
		}
		env.Registry.Fail(nil)
		// Foreign repository / missing provenance map to output_invalid and store nothing.
		for _, e := range []error{artifacts.ErrRegistryDenied, artifacts.ErrProvenanceMissing, artifacts.ErrRegistryContent} {
			env.Registry.Fail(e)
			if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); !errors.Is(err, artifacts.ErrOutputInvalid) {
				t.Fatalf("%v: %v", e, err)
			}
		}
		env.Registry.Fail(errors.New("surprise"))
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); !errors.Is(err, artifacts.ErrUnavailable) {
			t.Fatalf("an unrecognized verifier failure must not be success: %v", err)
		}
		env.Registry.Fail(nil)
		if n := env.Count(`SELECT count(*) FROM artifacts`); n != 0 {
			t.Fatalf("%d unverified registrations stored", n)
		}

		env.Registry.Add("registry.example.test/team/app", digest, artifactstest.Source)
		out, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r")
		if err != nil || !out.Created || out.State != artifacts.StateRegistered || out.Upload != nil {
			t.Fatalf("verified registration: %+v %v", out, err)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, out.ID)
		if a.Registry() != "registry.example.test/team/app" || a.Provenance() != artifactstest.Source || !a.Usable() {
			t.Fatalf("stored registration: %+v", a.Reservation())
		}
		calls := env.Registry.Calls()
		replay, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r")
		if err != nil || replay.Created || replay.ID != out.ID || replay.Upload != nil {
			t.Fatalf("replay: %+v %v", replay, err)
		}
		if env.Registry.Calls() != calls {
			t.Fatal("a replay must not re-verify immutable content")
		}
		// A different digest under the same name is a conflict.
		other := in
		other.Digest = artifacts.DigestOf([]byte("other"))
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, other, "r"); !errors.Is(err, artifacts.ErrIdempotencyConflict) {
			t.Fatalf("changed digest: %v", err)
		}
		// Completing a registration confirms it with its own digest only.
		done, err := env.Service.Complete(env.Ctx(), caller, attempt, out.ID, artifacts.CompleteInput{Digest: digest, Size: 0}, "r")
		if err != nil || done.State != artifacts.StateUsable || done.Digest != digest {
			t.Fatalf("completing a registration: %+v %v", done, err)
		}
		if _, err := env.Service.Complete(env.Ctx(), caller, attempt, out.ID, artifacts.CompleteInput{Digest: artifacts.DigestOf([]byte("x")), Size: 0}, "r"); !errors.Is(err, artifacts.ErrDigestMismatch) {
			t.Fatalf("wrong digest on a registration: %v", err)
		}
		// Malformed registry spellings are rejected before any registry call.
		calls = env.Registry.Calls()
		for _, bad := range []string{"https://registry.example.test/app", "user:pw@registry.example.test/app", "registry.example.test/../x", "registry.example.test/a b"} {
			in := in
			in.Name = "image"
			in.Registry = strp(bad)
			if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, in, "r"); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("%q: %v", bad, err)
			}
		}
		if env.Registry.Calls() != calls {
			t.Fatal("a malformed registry reached the verifier")
		}
	})
}
