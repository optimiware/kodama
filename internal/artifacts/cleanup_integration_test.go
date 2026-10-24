package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
)

type retentionFunc func(artifacts.RetentionSubject) bool

func (f retentionFunc) Eligible(_ context.Context, s artifacts.RetentionSubject, _ time.Time) (bool, error) {
	return f(s), nil
}

func stagingKeyOf(t *testing.T, env *artifactstest.Env, id domain.ArtifactID) string {
	t.Helper()
	a, err := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.StagingKey()
}

// Test_orphan_cleanup: abandoned uploads and stranded sealing objects are
// removed; active, referenced, retained and foreign-prefix objects survive;
// restarts and repeated deletion converge. Real PostgreSQL and S3.
func Test_orphan_cleanup(t *testing.T) {
	data := []byte("cleanup subject bytes")

	t.Run("abandoned uploads are removed once their transfers and grace have passed", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		rec := newReconciler(t, env, fo)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		key := stagingKeyOf(t, env, id)

		// An active upload survives: its latest transfer is still valid.
		if rep := sweepUntilQuiet(t, rec, 3); rep.Deleted != 0 || rep.Abandoned != 0 {
			t.Fatalf("an active upload was touched: %+v", rep)
		}
		env.Clock.Advance(5 * time.Minute)
		if rep := sweepUntilQuiet(t, rec, 3); rep.Deleted != 0 || rep.Abandoned != 0 {
			t.Fatalf("an upload whose instruction is still valid was touched: %+v", rep)
		}
		if !env.ObjectExists(key) {
			t.Fatal("an active upload disappeared")
		}

		// Past the instruction's expiry plus the abandonment age: abandoned and removed.
		env.Clock.Advance(20 * time.Minute)
		rep := sweepUntilQuiet(t, rec, 4)
		if rep.Abandoned != 1 || rep.Deleted != 1 {
			t.Fatalf("sweep: %+v", rep)
		}
		if env.ObjectExists(key) {
			t.Fatal("the abandoned upload still exists")
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if a.Disposition() != artifacts.DispositionAbandoned || a.State() != artifacts.StateReserved || a.Usable() || a.Version() != 2 {
			t.Fatalf("abandoned artifact: %v %v v%d", a.State(), a.Disposition(), a.Version())
		}
		if got := env.ObjectRecords(id); got[key] != "deleted" {
			t.Fatalf("deletion must be acknowledged: %v", got)
		}
		if n := env.Count(`SELECT count(*) FROM audit WHERE resource_id = $1 AND action = 'artifact.abandoned'`, string(id)); n != 1 {
			t.Fatalf("%d abandonment audits", n)
		}
		// Late completion or replay of the closed reservation never revives it.
		if _, err := env.Complete(caller, attempt, id, data); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("completing an abandoned reservation: %v", err)
		}
		if _, err := env.Service.Reserve(env.Ctx(), caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "req"); !errors.Is(err, artifacts.ErrIdempotencyConflict) {
			t.Fatalf("replaying an abandoned reservation: %v", err)
		}
		// Repeat sweeps are a no-op.
		if rep := sweepUntilQuiet(t, rec, 3); rep.Deleted+rep.Abandoned != 0 {
			t.Fatalf("repeat sweep did work: %+v", rep)
		}
	})

	t.Run("a stranded sealing object is removed but a live claim's destination is not", func(t *testing.T) {
		fr, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		rec := newReconciler(t, env, fo)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		fr.Set(func(f *artifactstest.FaultRepo) {
			f.OnCommit = func(artifacts.CommitCommand, func() (artifacts.Artifact, error)) (artifacts.Artifact, error) {
				return artifacts.Artifact{}, errors.New("crash before commit")
			}
		})
		if _, err := env.Complete(caller, attempt, id, data); err == nil {
			t.Fatal("expected failure")
		}
		dest := fo.PutKeys()[0]

		// The lease is live: the destination belongs to an in-flight completion.
		env.Clock.Advance(10 * time.Second)
		sweepUntilQuiet(t, rec, 3)
		if !env.ObjectExists(dest) {
			t.Fatal("cleanup deleted the destination of a live completion")
		}
		// Lease lapsed and the grace passed: stranded.
		env.Clock.Advance(time.Minute)
		rep := sweepUntilQuiet(t, rec, 4)
		if rep.Deleted != 1 || env.ObjectExists(dest) {
			t.Fatalf("stranded sealing object survived: %+v", rep)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if !a.Open() {
			t.Fatal("cleanup of a stranded object must leave the reservation completable")
		}
		fr.Set(func(f *artifactstest.FaultRepo) { f.OnCommit = nil })
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatalf("the reservation completes after cleanup: %v", err)
		}
	})

	t.Run("referenced content survives and staging is dropped after its horizon", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		rec := newReconciler(t, env, fo)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatal(err)
		}
		winner := sealed(t, env, id)
		staging := stagingKeyOf(t, env, id)

		sweepUntilQuiet(t, rec, 3)
		if !env.ObjectExists(staging) {
			t.Fatal("staging must outlive the still-valid upload instruction")
		}
		env.Clock.Advance(24 * time.Hour)
		rep := sweepUntilQuiet(t, rec, 4)
		if rep.Deleted != 1 || env.ObjectExists(staging) {
			t.Fatalf("staging of usable content should be removed: %+v", rep)
		}
		if !env.ObjectExists(winner) || !bytes.Equal(env.ObjectBytes(winner), data) {
			t.Fatal("the usable content was deleted")
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if !a.Usable() || a.Disposition() != artifacts.DispositionActive {
			t.Fatal("cleanup changed a usable artifact")
		}
	})

	t.Run("a late staging write after cleanup is caught by a repeat sweep", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		rec := newReconciler(t, env, fo)
		out, err := env.Service.Reserve(env.Ctx(), caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "req")
		if err != nil {
			t.Fatal(err)
		}
		env.Upload(*out.Upload, data)
		if _, err := env.Complete(caller, attempt, out.ID, data); err != nil {
			t.Fatal(err)
		}
		staging := stagingKeyOf(t, env, out.ID)
		env.Clock.Advance(24 * time.Hour)
		sweepUntilQuiet(t, rec, 4)
		if env.ObjectExists(staging) {
			t.Fatal("staging should be gone")
		}
		// A replay renews the instruction (recorded durably first); the producer uploads late.
		env.Renew(caller)
		replay, err := env.Service.Reserve(env.Ctx(), caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "req")
		if err != nil || replay.Upload == nil {
			t.Fatalf("replay: %v", err)
		}
		if got := env.ObjectRecords(out.ID)[staging]; got != "live" {
			t.Fatalf("a renewed transfer must reopen the staging record before the instruction exists: %q", got)
		}
		if code, body := env.Upload(*replay.Upload, data); code != 200 {
			t.Fatalf("late upload: %d %s", code, body)
		}
		if !env.ObjectExists(staging) {
			t.Fatal("late write did not land")
		}
		sweepUntilQuiet(t, rec, 3)
		if !env.ObjectExists(staging) {
			t.Fatal("a staging object with a valid instruction must not be deleted")
		}
		env.Clock.Advance(24 * time.Hour)
		sweepUntilQuiet(t, rec, 4)
		if env.ObjectExists(staging) {
			t.Fatal("the late staging write was never cleaned")
		}
		if !env.ObjectExists(sealed(t, env, out.ID)) {
			t.Fatal("usable content lost")
		}
	})

	t.Run("foreign and unrecorded objects survive every sweep", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		rec := newReconciler(t, env, fo)
		put := func(key string) {
			t.Helper()
			if _, err := env.S3.Client.PutObject(context.Background(), &awss3.PutObjectInput{Bucket: aws.String(env.S3.Bucket()), Key: aws.String(key), Body: strings.NewReader("keep")}); err != nil {
				t.Fatal(err)
			}
		}
		exists := func(key string) bool {
			_, err := env.S3.Client.HeadObject(context.Background(), &awss3.HeadObjectInput{Bucket: aws.String(env.S3.Bucket()), Key: aws.String(key)})
			return err == nil
		}
		outside := env.S3.Prefix + "foreign/keep.txt"                                              // inside the test prefix, outside the namespace
		unrecorded := env.Objects.Prefix() + "artifacts/t_acme/p_inventory/art_unknown/staging/zz" // the namespace, but never recorded
		sibling := env.S3.Config.Prefix + "t015-sibling-" + fmt.Sprint(time.Now().UnixNano()) + "/keep.txt"
		put(outside)
		put(unrecorded)
		put(sibling)
		t.Cleanup(func() {
			_, _ = env.S3.Client.DeleteObject(context.Background(), &awss3.DeleteObjectInput{Bucket: aws.String(env.S3.Bucket()), Key: aws.String(sibling)})
		})

		env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		env.Clock.Advance(48 * time.Hour)
		sweepUntilQuiet(t, rec, 5)
		for _, k := range []string{outside, unrecorded, sibling} {
			if !exists(k) {
				t.Errorf("a sweep deleted %s", k)
			}
		}
		for _, k := range fo.DeleteKeys() {
			if !strings.HasPrefix(k, "artifacts/") {
				t.Errorf("deletion outside the artifact namespace attempted: %s", k)
			}
		}
	})

	t.Run("deletion survives restarts and repeats", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		rec := newReconciler(t, env, fo)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		key := stagingKeyOf(t, env, id)
		env.Clock.Advance(24 * time.Hour)

		// The provider times out: the intent is durable, nothing is acknowledged.
		fo.Set(func(f *artifactstest.FaultObjects) {
			f.OnDelete = func(string, func() error) error { return fmt.Errorf("%w: provider timeout", artifacts.ErrUnavailable) }
		})
		rep, err := rec.Sweep(env.Ctx())
		if err == nil || rep.Retryable == 0 {
			t.Fatalf("a failing provider must leave retryable work: %+v %v", rep, err)
		}
		if got := env.ObjectRecords(id)[key]; got != "delete_pending" {
			t.Fatalf("deletion intent must be durable and unacknowledged: %q", got)
		}
		if !env.ObjectExists(key) {
			t.Fatal("object should still exist")
		}

		// A restarted worker finds the work after the lease lapsed; the delete
		// reaches the provider but the acknowledgement is "lost".
		env.Clock.Advance(time.Minute)
		fo.Set(func(f *artifactstest.FaultObjects) {
			f.OnDelete = func(_ string, next func() error) error {
				if err := next(); err != nil {
					return err
				}
				return errors.New("response lost after the provider deleted the object")
			}
		})
		if _, err := rec.Sweep(env.Ctx()); err == nil {
			t.Fatal("expected the lost response to be reported")
		}
		if env.ObjectExists(key) {
			t.Fatal("the provider delete did not happen")
		}
		if got := env.ObjectRecords(id)[key]; got != "delete_pending" {
			t.Fatalf("an unacknowledged deletion stays pending: %q", got)
		}

		// Repeating the deletion of a missing object is success and acknowledges.
		env.Clock.Advance(time.Minute)
		fo.Set(func(f *artifactstest.FaultObjects) { f.OnDelete = nil })
		sweepUntilQuiet(t, rec, 4)
		if got := env.ObjectRecords(id)[key]; got != "deleted" {
			t.Fatalf("convergence: %q", got)
		}
		rep, err = rec.Sweep(env.Ctx())
		if err != nil || rep.Deleted != 0 {
			t.Fatalf("a repeat sweep must be a no-op: %+v %v", rep, err)
		}
	})

	t.Run("concurrent workers delete each object once and acknowledgements are fenced", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		for i := 0; i < 6; i++ {
			name := fmt.Sprintf("o%d", i)
			env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) {
				g.Outputs = append(g.Outputs, artifacts.OutputDecl{Name: name, Kind: artifacts.KindFile})
			})
			id := env.ReserveAndUpload(caller, attempt, name, artifacts.RoleOutput, []byte(name))
			if _, err := env.Complete(caller, attempt, id, []byte(name)); err != nil {
				t.Fatal(err)
			}
		}
		env.Clock.Advance(24 * time.Hour)
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := newReconciler(t, env, fo)
				for i := 0; i < 3; i++ {
					if _, err := r.Sweep(env.Ctx()); err != nil {
						t.Error(err)
					}
				}
			}()
		}
		wg.Wait()
		// Every staging object of a usable artifact is gone, each record converged.
		if n := env.Count(`SELECT count(*) FROM artifact_objects WHERE purpose = 'staging' AND state <> 'deleted'`); n != 0 {
			t.Fatalf("%d staging records not converged", n)
		}
		keys, _ := env.S3.Keys(env.Ctx())
		for _, k := range keys {
			if strings.Contains(k, "/staging/") {
				t.Fatalf("staging object remains: %s", k)
			}
		}
		if len(keys) != 6 {
			t.Fatalf("exactly the six sealed objects must remain: %v", keys)
		}
	})

	t.Run("a superseded deletion lease cannot acknowledge", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatal(err)
		}
		env.Clock.Advance(24 * time.Hour)
		p := testPolicy()
		tenant := artifactstest.TenantA
		first, err := env.Store.ClaimDeletions(env.Ctx(), tenant, env.Clock.Now(), p, "tok-A")
		if err != nil || len(first) != 1 {
			t.Fatalf("first claim: %v %v", first, err)
		}
		// Another worker cannot claim while the lease is live.
		if again, _ := env.Store.ClaimDeletions(env.Ctx(), tenant, env.Clock.Now(), p, "tok-B"); len(again) != 0 {
			t.Fatalf("a live lease was claimed twice: %v", again)
		}
		env.Clock.Advance(time.Minute)
		second, err := env.Store.ClaimDeletions(env.Ctx(), tenant, env.Clock.Now(), p, "tok-B")
		if err != nil || len(second) != 1 {
			t.Fatalf("takeover: %v %v", second, err)
		}
		if err := env.Store.AckDeletion(env.Ctx(), first[0], env.Clock.Now(), p); !errors.Is(err, artifacts.ErrClaimLost) {
			t.Fatalf("a superseded worker must not acknowledge: %v", err)
		}
		if err := env.Store.AckDeletion(env.Ctx(), second[0], env.Clock.Now(), p); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("retention is disabled by default and respects pins", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		for _, n := range []string{"keep", "pinned", "drop"} {
			name := n
			env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) {
				g.Outputs = append(g.Outputs, artifacts.OutputDecl{Name: name, Kind: artifacts.KindFile})
			})
		}
		ids := map[string]domain.ArtifactID{}
		for _, n := range []string{"keep", "pinned", "drop"} {
			ids[n] = env.ReserveAndUpload(caller, attempt, n, artifacts.RoleOutput, []byte("content-"+n))
			if _, err := env.Complete(caller, attempt, ids[n], []byte("content-"+n)); err != nil {
				t.Fatal(err)
			}
		}
		env.Clock.Advance(1000 * time.Hour)

		// No policy injected: nothing usable is ever deleted, however old.
		noPolicy := newReconciler(t, env, fo)
		sweepUntilQuiet(t, noPolicy, 4)
		for n, id := range ids {
			if !env.ObjectExists(sealed(t, env, id)) {
				t.Fatalf("%s: usable content deleted without a retention policy", n)
			}
		}

		if err := env.Service.Pin(env.Ctx(), artifactstest.ScopeA, ids["pinned"], artifacts.PinOwner{Kind: "gate", ID: "g_1"}); err != nil {
			t.Fatal(err)
		}
		policy := retentionFunc(func(s artifacts.RetentionSubject) bool { return s.ID != ids["keep"] })
		rec := newReconcilerWith(t, env, fo, testPolicy(), policy)
		rep := sweepUntilQuiet(t, rec, 5)
		if rep.Removed != 1 {
			t.Fatalf("retention removed %d artifacts: %+v", rep.Removed, rep)
		}
		for n, wantGone := range map[string]bool{"keep": false, "pinned": false, "drop": true} {
			a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, ids[n])
			gone := a.Disposition() == artifacts.DispositionRemoved
			if gone != wantGone {
				t.Errorf("%s: removed=%v", n, gone)
			}
			if exists := env.ObjectExists(a.SealedKey()); exists == wantGone {
				t.Errorf("%s: object exists=%v but removed=%v", n, exists, wantGone)
			}
		}
		// Metadata and provenance stay; the reference is no longer usable.
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, ids["drop"])
		if a.Provenance() != artifactstest.Source || a.Usable() {
			t.Fatal("removed content keeps provenance and is not usable")
		}
		refs, err := env.Service.References(env.Ctx(), artifactstest.ScopeA, attempt, []domain.ArtifactID{ids["drop"], ids["keep"]})
		if err != nil || refs[ids["drop"]].Usable || !refs[ids["keep"]].Usable {
			t.Fatalf("references: %+v %v", refs, err)
		}
		// A removed artifact can no longer be pinned; unpinning releases protection.
		if err := env.Service.Pin(env.Ctx(), artifactstest.ScopeA, ids["drop"], artifacts.PinOwner{Kind: "gate", ID: "g_2"}); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("pinning removed content: %v", err)
		}
		if err := env.Service.Unpin(env.Ctx(), artifactstest.ScopeA, ids["pinned"], artifacts.PinOwner{Kind: "gate", ID: "g_1"}); err != nil {
			t.Fatal(err)
		}
		sweepUntilQuiet(t, rec, 5)
		a, _ = env.Store.Get(env.Ctx(), artifactstest.ScopeA, ids["pinned"])
		if a.Disposition() != artifacts.DispositionRemoved {
			t.Fatal("an unpinned artifact becomes eligible")
		}
	})

	t.Run("pin and retention deletion are serialized", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		const n = 8
		ids := make([]domain.ArtifactID, n)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("r%d", i)
			env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) {
				g.Outputs = append(g.Outputs, artifacts.OutputDecl{Name: name, Kind: artifacts.KindFile})
			})
			ids[i] = env.ReserveAndUpload(caller, attempt, name, artifacts.RoleOutput, []byte(name))
			if _, err := env.Complete(caller, attempt, ids[i], []byte(name)); err != nil {
				t.Fatal(err)
			}
		}
		rec := newReconcilerWith(t, env, fo, testPolicy(), retentionFunc(func(artifacts.RetentionSubject) bool { return true }))
		var wg sync.WaitGroup
		pinned := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				pinned[i] = env.Service.Pin(env.Ctx(), artifactstest.ScopeA, ids[i], artifacts.PinOwner{Kind: "gate", ID: "g_race"})
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = rec.Sweep(env.Ctx())
		}()
		close(start)
		wg.Wait()
		for i := 0; i < n; i++ {
			a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, ids[i])
			isPinned := env.Count(`SELECT count(*) FROM artifact_pins WHERE artifact_id = $1`, string(ids[i])) == 1
			removed := a.Disposition() == artifacts.DispositionRemoved
			if isPinned && removed {
				t.Fatalf("artifact %d is both pinned and removed", i)
			}
			if pinned[i] == nil && !isPinned {
				t.Fatalf("a successful pin was lost")
			}
			if pinned[i] != nil && !errors.Is(pinned[i], artifacts.ErrNotFound) {
				t.Fatalf("pin outcome: %v", pinned[i])
			}
			if !removed && !env.ObjectExists(a.SealedKey()) {
				t.Fatalf("artifact %d lost its content without being removed", i)
			}
		}
	})
}
