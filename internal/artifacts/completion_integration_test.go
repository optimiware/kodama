package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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

const attempt = domain.AttemptID("att_1")

func setup(t *testing.T, opts artifactstest.Options) (*artifactstest.Env, artifacts.Caller) {
	t.Helper()
	env := artifactstest.New(t, opts)
	caller := env.Attempt(artifactstest.ScopeA, attempt,
		artifacts.OutputDecl{Name: "report", Kind: artifacts.KindFile},
		artifacts.OutputDecl{Name: "image", Kind: artifacts.KindOCI})
	return env, caller
}

func tempFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func sealed(t *testing.T, env *artifactstest.Env, id domain.ArtifactID) string {
	t.Helper()
	a, err := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.SealedKey()
}

// TestCompletionRecovery: concurrent completion, response loss, uncertain
// commits, crash after sealing, lease takeover and cleanup races cannot publish
// wrong bytes or delete the winner. Real PostgreSQL and S3.
func TestCompletionRecovery(t *testing.T) {
	data := []byte("the report bytes, verified before they become usable")

	t.Run("completion verifies, seals and is idempotent", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)

		before := env.Authority.Calls()
		done, err := env.Complete(caller, attempt, id, data)
		if err != nil {
			t.Fatal(err)
		}
		if done.ID != id || done.State != artifacts.StateUsable || done.Digest != artifacts.DigestOf(data) || done.Size != int64(len(data)) {
			t.Fatalf("completion: %+v", done)
		}
		if env.Authority.Calls() < before+2 {
			t.Fatal("completion must reauthorize on entry and again before finalization")
		}
		key := sealed(t, env, id)
		if key == "" || !bytes.Equal(env.ObjectBytes(key), data) {
			t.Fatal("sealed bytes differ from the verified bytes")
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if key == a.StagingKey() || !strings.Contains(key, "/sealed/") {
			t.Fatalf("usable content must live at a server-only destination: %s", key)
		}
		recs := env.ObjectRecords(id)
		if recs[key] != "live:winner" || len(recs) != 2 {
			t.Fatalf("object records: %v", recs)
		}
		if a.Version() != 2 || a.State() != artifacts.StateUsable {
			t.Fatalf("artifact: %v v%d", a.State(), a.Version())
		}
		if n := env.Count(`SELECT count(*) FROM audit WHERE resource_id = $1 AND action = 'artifact.completed'`, string(id)); n != 1 {
			t.Fatalf("%d completion audit records", n)
		}

		again, err := env.Complete(caller, attempt, id, data)
		if err != nil || again != done {
			t.Fatalf("a repeat must return the same immutable result: %+v %v", again, err)
		}
		if sealed(t, env, id) != key || env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1`, string(id)) != 2 {
			t.Fatal("a repeat must not seal again")
		}
		if n := tempFiles(t, env.Config.SpoolDir); n != 0 {
			t.Fatalf("%d scratch files left behind", n)
		}
	})

	t.Run("concurrent completions publish one winner", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		const n = 8
		results := make([]error, n)
		out := make([]artifacts.Completion, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for tries := 0; tries < 50; tries++ {
					out[i], results[i] = env.Complete(caller, attempt, id, data)
					if !errors.Is(results[i], artifacts.ErrInProgress) {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
		}
		close(start)
		wg.Wait()
		for i := range results {
			if results[i] != nil || out[i].State != artifacts.StateUsable || out[i].Digest != artifacts.DigestOf(data) {
				t.Fatalf("completion %d: %+v %v", i, out[i], results[i])
			}
		}
		if got := env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1 AND winner`, string(id)); got != 1 {
			t.Fatalf("%d winners", got)
		}
		if got := env.Count(`SELECT count(*) FROM audit WHERE resource_id = $1 AND action = 'artifact.completed'`, string(id)); got != 1 {
			t.Fatalf("%d completion audits", got)
		}
		if !bytes.Equal(env.ObjectBytes(sealed(t, env, id)), data) {
			t.Fatal("winner bytes differ")
		}
	})

	t.Run("a lost response after a durable commit is recovered, never doubled", func(t *testing.T) {
		fr, _, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		lost := errors.New("connection reset after commit")
		fr.Set(func(f *artifactstest.FaultRepo) {
			f.OnCommit = func(_ artifacts.CommitCommand, next func() (artifacts.Artifact, error)) (artifacts.Artifact, error) {
				if _, err := next(); err != nil {
					return artifacts.Artifact{}, err
				}
				return artifacts.Artifact{}, lost
			}
		})
		done, err := env.Complete(caller, attempt, id, data)
		if err != nil {
			t.Fatalf("the committed result must be recovered from durable state: %v", err)
		}
		if done.State != artifacts.StateUsable {
			t.Fatalf("%+v", done)
		}
		if got := env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1 AND winner`, string(id)); got != 1 {
			t.Fatalf("%d winners", got)
		}
	})

	t.Run("an unknown outcome that never committed is not success and retries converge", func(t *testing.T) {
		fr, _, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		fr.Set(func(f *artifactstest.FaultRepo) {
			f.OnCommit = func(artifacts.CommitCommand, func() (artifacts.Artifact, error)) (artifacts.Artifact, error) {
				return artifacts.Artifact{}, errors.New("timeout: commit outcome unknown")
			}
		})
		if _, err := env.Complete(caller, attempt, id, data); !errors.Is(err, artifacts.ErrUnavailable) {
			t.Fatalf("an unconfirmed commit must be unavailable, not success: %v", err)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if a.Usable() {
			t.Fatal("nothing committed, yet the artifact is usable")
		}
		fr.Set(func(f *artifactstest.FaultRepo) { f.OnCommit = nil })
		// The lease from the failed attempt still holds; once it lapses the retry
		// takes over and completes.
		if _, err := env.Complete(caller, attempt, id, data); !errors.Is(err, artifacts.ErrInProgress) {
			t.Fatalf("a live lease must be respected: %v", err)
		}
		env.Clock.Advance(time.Minute)
		done, err := env.Complete(caller, attempt, id, data)
		if err != nil || done.State != artifacts.StateUsable {
			t.Fatalf("retry after the lease: %+v %v", done, err)
		}
		if got := env.Count(`SELECT claim_generation FROM artifacts WHERE id = $1`, string(id)); got != 2 {
			t.Fatalf("generation %d: takeover must fence the first attempt", got)
		}
		recs := env.ObjectRecords(id)
		winners := 0
		for _, s := range recs {
			if strings.HasSuffix(s, ":winner") {
				winners++
			}
		}
		if winners != 1 || len(recs) != 3 {
			t.Fatalf("every candidate destination is recorded, one wins: %v", recs)
		}
	})

	t.Run("a crash after sealing strands bytes that cleanup removes without touching the winner", func(t *testing.T) {
		fr, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		crash := errors.New("process crashed")
		fr.Set(func(f *artifactstest.FaultRepo) {
			f.OnCommit = func(artifacts.CommitCommand, func() (artifacts.Artifact, error)) (artifacts.Artifact, error) {
				return artifacts.Artifact{}, crash
			}
		})
		if _, err := env.Complete(caller, attempt, id, data); err == nil {
			t.Fatal("a crash must not look like success")
		}
		stranded := fo.PutKeys()
		if len(stranded) != 1 || !env.ObjectExists(stranded[0]) {
			t.Fatalf("the sealed copy must exist after the crash: %v", stranded)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if a.Usable() {
			t.Fatal("the artifact became usable without a commit")
		}
		// Recovery by retry after the lease lapsed.
		fr.Set(func(f *artifactstest.FaultRepo) { f.OnCommit = nil })
		env.Clock.Advance(time.Minute)
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatal(err)
		}
		winner := sealed(t, env, id)
		if winner == stranded[0] {
			t.Fatal("the winner must be the new destination")
		}
		rec := newReconciler(t, env, fo)
		sweepUntilQuiet(t, rec, 5)
		if env.ObjectExists(stranded[0]) {
			t.Fatal("the stranded sealed copy was not cleaned")
		}
		if !env.ObjectExists(winner) || !bytes.Equal(env.ObjectBytes(winner), data) {
			t.Fatal("cleanup damaged the winner")
		}
	})

	t.Run("an expired lease fences a slow holder out of the commit", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		fo.Set(func(f *artifactstest.FaultObjects) {
			f.OnPut = func(_ artifacts.PutSpec, next func() error) error {
				err := next()
				env.Clock.Advance(time.Minute) // the lease lapsed while the holder was slow
				return err
			}
		})
		if _, err := env.Complete(caller, attempt, id, data); err == nil {
			t.Fatal("a holder whose lease lapsed must not publish")
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if a.Usable() || env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1 AND winner`, string(id)) != 0 {
			t.Fatal("a fenced-out completion changed state")
		}
		fo.Set(func(f *artifactstest.FaultObjects) { f.OnPut = nil })
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatalf("a later completion succeeds: %v", err)
		}
		if len(fo.PutKeys()) != 2 {
			t.Fatalf("two sealing attempts expected: %v", fo.PutKeys())
		}
	})

	t.Run("a takeover during a stalled holder leaves the takeover as the only winner", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		stalled, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		fo.Set(func(f *artifactstest.FaultObjects) {
			f.OnOpen = func(key string, next func() (artifacts.Object, error)) (artifacts.Object, error) {
				first := false
				once.Do(func() { first = true })
				if first {
					close(stalled)
					<-release // holder 1 stalls before reading
				}
				return next()
			}
		})
		var res1 error
		done1 := make(chan struct{})
		go func() {
			defer close(done1)
			_, res1 = env.Complete(caller, attempt, id, data)
		}()
		<-stalled
		env.Clock.Advance(time.Minute)
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatalf("takeover: %v", err)
		}
		winner := sealed(t, env, id)
		close(release)
		<-done1
		// Holder 1 resumes after losing the claim; whatever it reports, the
		// stored result is the takeover's and its bytes are correct.
		if res1 != nil && !errors.Is(res1, artifacts.ErrInProgress) && !errors.Is(res1, artifacts.ErrUnavailable) {
			t.Fatalf("holder 1: %v", res1)
		}
		if sealed(t, env, id) != winner || !bytes.Equal(env.ObjectBytes(winner), data) {
			t.Fatal("the stalled holder changed the published content")
		}
		if got := env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1 AND winner`, string(id)); got != 1 {
			t.Fatalf("%d winners", got)
		}
	})

	t.Run("wrong bytes never become usable and a corrected upload completes", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		out, err := env.Service.Reserve(env.Ctx(), caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "req")
		if err != nil {
			t.Fatal(err)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, out.ID)
		wrong := bytes.ToUpper(data)
		putStaging(t, env, a.StagingKey(), wrong)
		_, err = env.Complete(caller, attempt, out.ID, data)
		if !errors.Is(err, artifacts.ErrDigestMismatch) {
			t.Fatalf("different bytes: %v", err)
		}
		if len(fo.PutKeys()) != 0 {
			t.Fatalf("nothing may be sealed for mismatching bytes: %v", fo.PutKeys())
		}
		a, _ = env.Store.Get(env.Ctx(), artifactstest.ScopeA, out.ID)
		if a.Usable() || a.Claim().Token != "" {
			t.Fatalf("mismatch must leave a reserved artifact with a released lease: %v %+v", a.State(), a.Claim())
		}
		putStaging(t, env, a.StagingKey(), data)
		if _, err := env.Complete(caller, attempt, out.ID, data); err != nil {
			t.Fatalf("corrected upload: %v", err)
		}
	})

	t.Run("a cleanup sweep racing completion cannot delete the winner or in-flight bytes", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		rec := newReconciler(t, env, fo)
		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("report-%d", i)
			env.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) {
				g.Outputs = append(g.Outputs, artifacts.OutputDecl{Name: name, Kind: artifacts.KindFile})
			})
			payload := []byte(strings.Repeat("x", 100+i))
			id := env.ReserveAndUpload(caller, attempt, name, artifacts.RoleOutput, payload)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						_, _ = rec.Sweep(env.Ctx())
					}
				}
			}()
			done, err := env.Complete(caller, attempt, id, payload)
			close(stop)
			wg.Wait()
			if err != nil || done.State != artifacts.StateUsable {
				t.Fatalf("%s: %+v %v", name, done, err)
			}
			sweepUntilQuiet(t, rec, 4)
			key := sealed(t, env, id)
			if !env.ObjectExists(key) || !bytes.Equal(env.ObjectBytes(key), payload) {
				t.Fatalf("%s: the winner was damaged by cleanup", name)
			}
		}
	})

	t.Run("a revoked attempt cannot finalize", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		env.Authority.Revoke(attempt)
		if _, err := env.Complete(caller, attempt, id, data); !errors.Is(err, artifacts.ErrAttemptInactive) {
			t.Fatalf("revoked: %v", err)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if a.Usable() {
			t.Fatal("a revoked attempt made content usable")
		}
	})

	t.Run("revocation between sealing and commit blocks the commit", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		fo.Set(func(f *artifactstest.FaultObjects) {
			f.OnPut = func(_ artifacts.PutSpec, next func() error) error {
				err := next()
				env.Authority.Revoke(attempt) // revoked right after the bytes were sealed
				return err
			}
		})
		if _, err := env.Complete(caller, attempt, id, data); !errors.Is(err, artifacts.ErrAttemptInactive) {
			t.Fatalf("finalization must reauthorize: %v", err)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if a.Usable() {
			t.Fatal("content became usable after revocation")
		}
	})

	t.Run("provider failures are retryable and leave no usable content", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		fail := errors.New("provider timeout")
		fo.Set(func(f *artifactstest.FaultObjects) {
			f.OnPut = func(artifacts.PutSpec, func() error) error {
				return fmt.Errorf("%w: %v", artifacts.ErrUnavailable, fail)
			}
		})
		if _, err := env.Complete(caller, attempt, id, data); !errors.Is(err, artifacts.ErrUnavailable) {
			t.Fatalf("sealing failure: %v", err)
		}
		fo.Set(func(f *artifactstest.FaultObjects) {
			f.OnPut = nil
			f.OnOpen = func(string, func() (artifacts.Object, error)) (artifacts.Object, error) {
				return artifacts.Object{}, fmt.Errorf("%w: read timeout", artifacts.ErrUnavailable)
			}
		})
		env.Clock.Advance(time.Minute)
		if _, err := env.Complete(caller, attempt, id, data); !errors.Is(err, artifacts.ErrUnavailable) {
			t.Fatalf("read failure is unavailable, not incomplete: %v", err)
		}
		fo.Set(func(f *artifactstest.FaultObjects) { f.OnOpen = nil })
		env.Clock.Advance(time.Minute)
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
		if n := tempFiles(t, env.Config.SpoolDir); n != 0 {
			t.Fatalf("%d scratch files left behind on failure paths", n)
		}
	})
}

// TestStagingOverwriteCannotChangeUsableArtifact: overwrite and replay during
// and after completion cannot change verified downloads.
func TestStagingOverwriteCannotChangeUsableArtifact(t *testing.T) {
	data := []byte("original verified content")
	evil := []byte("ORIGINAL VERIFIED CONTENT") // same size, different bytes

	t.Run("overwrite after completion", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		env.Reader.Allow("user:alice", artifactstest.ScopeA)
		out, err := env.Service.Reserve(env.Ctx(), caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "req")
		if err != nil {
			t.Fatal(err)
		}
		if code, body := env.Upload(*out.Upload, data); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if _, err := env.Complete(caller, attempt, out.ID, data); err != nil {
			t.Fatal(err)
		}
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, out.ID)

		// The still-valid upload instruction is bound to the declared checksum: other bytes are refused.
		if code, _ := env.Upload(*out.Upload, evil); code == 200 {
			t.Fatal("the signed checksum must refuse different bytes")
		}
		// A holder of storage credentials overwriting the staging object changes nothing usable.
		putStaging(t, env, a.StagingKey(), evil)

		assertDownload(t, env, "user:alice", out.ID, data)

		// Replaying the reservation re-issues staging instructions and leaves the usable content untouched.
		replay, err := env.Service.Reserve(env.Ctx(), caller, attempt, artifactstest.FileInput("report", artifacts.RoleOutput, data), "req")
		if err != nil || replay.ID != out.ID || replay.Created || replay.State != artifacts.StateReserved || replay.Upload == nil {
			t.Fatalf("replay must keep the frozen reservation shape: %+v %v", replay, err)
		}
		if code, _ := env.Upload(*replay.Upload, data); code != 200 {
			t.Fatalf("replayed upload: %d", code)
		}
		putStaging(t, env, a.StagingKey(), evil)
		again, err := env.Complete(caller, attempt, out.ID, data)
		if err != nil || again.State != artifacts.StateUsable {
			t.Fatalf("repeat completion after staging changes: %+v %v", again, err)
		}
		assertDownload(t, env, "user:alice", out.ID, data)
		got, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, out.ID)
		if got.SealedKey() != a.SealedKey() || got.Version() != a.Version() {
			t.Fatal("usable content was reopened or modified by a replay")
		}
	})

	t.Run("overwrite after the bytes were opened for verification", func(t *testing.T) {
		_, fo, opts := artifactstest.Wire()
		env, caller := setup(t, opts)
		env.Reader.Allow("user:alice", artifactstest.ScopeA)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		fo.Set(func(f *artifactstest.FaultObjects) {
			f.OnOpen = func(key string, next func() (artifacts.Object, error)) (artifacts.Object, error) {
				obj, err := next()
				if err == nil && key == a.StagingKey() {
					putStaging(t, env, a.StagingKey(), evil) // swapped while verification is in flight
				}
				return obj, err
			}
		})
		if _, err := env.Complete(caller, attempt, id, data); err != nil {
			t.Fatalf("the opened stream is the verified version: %v", err)
		}
		assertDownload(t, env, "user:alice", id, data)
	})

	t.Run("overwrite before completion is a digest mismatch", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		putStaging(t, env, a.StagingKey(), evil)
		if _, err := env.Complete(caller, attempt, id, data); !errors.Is(err, artifacts.ErrDigestMismatch) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("overwrite racing many completions", func(t *testing.T) {
		env, caller := setup(t, artifactstest.Options{})
		env.Reader.Allow("user:alice", artifactstest.ScopeA)
		id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			flip := false
			for {
				select {
				case <-stop:
					return
				default:
					if flip {
						putStaging(t, env, a.StagingKey(), data)
					} else {
						putStaging(t, env, a.StagingKey(), evil)
					}
					flip = !flip
				}
			}
		}()
		var final error
		for i := 0; i < 30; i++ {
			if _, err := env.Complete(caller, attempt, id, data); err == nil {
				final = nil
				break
			} else if !errors.Is(err, artifacts.ErrDigestMismatch) && !errors.Is(err, artifacts.ErrInProgress) {
				final = err
				break
			}
			env.Clock.Advance(time.Minute)
		}
		close(stop)
		wg.Wait()
		a2, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if final != nil {
			t.Fatalf("unexpected error: %v", final)
		}
		if a2.Usable() {
			assertDownload(t, env, "user:alice", id, data)
		}
	})
}

func assertDownload(t *testing.T, env *artifactstest.Env, subject string, id domain.ArtifactID, want []byte) {
	t.Helper()
	v, err := env.Service.Metadata(env.Ctx(), artifacts.Caller{Subject: subject}, artifactstest.ScopeA, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Download == nil {
		t.Fatal("no download instruction")
	}
	code, body := env.Download(*v.Download)
	if code != 200 || !bytes.Equal(body, want) {
		t.Fatalf("download: %d, content differs from the verified bytes", code)
	}
}

func putStaging(t *testing.T, env *artifactstest.Env, logicalKey string, data []byte) {
	t.Helper()
	// Direct provider write with storage credentials (not via an instruction).
	_, err := env.S3.Client.PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String(env.S3.Bucket()), Key: aws.String(env.Objects.Prefix() + logicalKey), Body: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatal(err)
	}
}
