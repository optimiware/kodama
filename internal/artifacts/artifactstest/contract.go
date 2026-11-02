package artifactstest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
)

// RepoEnv is the artifact persistence under contract: the Repository and the
// CleanupStore ports of one adapter, plus the observation hooks the contract
// needs. The tenants TenantA and TenantB with ProjectA, ProjectA2 and ProjectB
// (see the constants of this package) exist in every implementation.
type RepoEnv interface {
	Repository() artifacts.Repository
	Cleanup() artifacts.CleanupStore
	// ObjectStates lists the artifact's object records by key: the state, with
	// ":winner" appended for the sealed candidate.
	ObjectStates(t testing.TB, scope domain.Scope, id domain.ArtifactID) map[string]string
	// AuditActions lists the artifact's audit actions, sorted.
	AuditActions(t testing.TB, scope domain.Scope, id domain.ArtifactID) []string
}

// OpenRepo builds an empty RepoEnv.
type OpenRepo func(testing.TB) RepoEnv

var (
	contractNow = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	contractLim = artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 2, DiagnosticBytes: 1 << 20, DiagnosticFiles: 1, BundleBytes: 1 << 20, BundleFiles: 1}
	worker      = artifacts.Actor{Type: "workload", ID: "attempt:att_1"}
	policy      = artifacts.CleanupPolicy{StagingGrace: time.Minute, AbandonAfter: time.Hour, DeletionLease: 5 * time.Minute, Batch: 10}
)

// RepoContract is the behavior every artifacts.Repository and CleanupStore
// adapter shares: reservation that never overwrites and enforces the attempt's
// aggregate limits, attempt- and tenant-scoped reads, a fenced completion lease
// whose losers commit nothing, pins that order against retention, and
// crash-safe cleanup of staging and candidate objects. It asserts outcomes, not
// lock order or isolation levels.
func RepoContract(t *testing.T, open OpenRepo) {
	t.Helper()
	ctx := context.Background()
	var seq atomic.Int64

	nextID := func() domain.ArtifactID { return domain.ArtifactID(fmt.Sprintf("art_t088_%04d", seq.Add(1))) }
	newFile := func(t *testing.T, scope domain.Scope, attempt domain.AttemptID, name string, role artifacts.Role, data []byte) artifacts.Artifact {
		t.Helper()
		size := int64(len(data))
		id := nextID()
		a, err := artifacts.NewFile(id, artifacts.Reservation{Scope: scope, Attempt: attempt, Run: "run_1", Step: "build", Name: name, Kind: artifacts.KindFile,
			Role: role, Digest: artifacts.DigestOf(data), MediaType: "text/plain", Size: &size, Provenance: Source},
			artifacts.ObjectKey(scope, id, artifacts.SegmentStaging, fmt.Sprintf("n%d", seq.Add(1))), contractNow.Add(10*time.Minute), contractNow)
		if err != nil {
			t.Fatalf("new file: %v", err)
		}
		return a
	}
	reserve := func(env RepoEnv, a artifacts.Artifact) (artifacts.ReserveResult, error) {
		return env.Repository().Reserve(ctx, artifacts.ReserveCommand{Artifact: a, Limits: contractLim, Actor: worker, RequestID: "req_t088", Now: contractNow})
	}
	mustReserve := func(t *testing.T, env RepoEnv, a artifacts.Artifact) artifacts.Artifact {
		t.Helper()
		res, err := reserve(env, a)
		if err != nil || !res.Created {
			t.Fatalf("reserve %s: created %v, %v", a.Name(), res.Created, err)
		}
		return res.Artifact
	}
	begin := func(env RepoEnv, a artifacts.Artifact, token, dest string, now time.Time) (artifacts.ClaimResult, error) {
		return env.Repository().BeginCompletion(ctx, artifacts.BeginCompletion{Scope: a.Scope(), Attempt: a.Attempt(), ID: a.ID(), Token: token,
			Destination: dest, Lease: 30 * time.Second, Now: now})
	}
	commit := func(env RepoEnv, a artifacts.Artifact, c artifacts.Claim, dest string, data []byte, now time.Time) (artifacts.Artifact, error) {
		return env.Repository().CommitUsable(ctx, artifacts.CommitCommand{Scope: a.Scope(), Attempt: a.Attempt(), ID: a.ID(), Claim: c, Destination: dest,
			Verification: artifacts.Verification{Digest: artifacts.DigestOf(data), Size: int64(len(data)), At: now}, Actor: worker, RequestID: "req_t088", Now: now})
	}
	sealed := func(a artifacts.Artifact, n string) string {
		return artifacts.ObjectKey(a.Scope(), a.ID(), artifacts.SegmentSealed, n)
	}
	// complete drives one artifact to usable under a fresh claim.
	complete := func(t *testing.T, env RepoEnv, a artifacts.Artifact, data []byte, now time.Time) artifacts.Artifact {
		t.Helper()
		dest := sealed(a, fmt.Sprintf("s%d", seq.Add(1)))
		res, err := begin(env, a, fmt.Sprintf("tok%d", seq.Add(1)), dest, now)
		if err != nil || res.Claim == nil {
			t.Fatalf("begin: %+v %v", res, err)
		}
		done, err := commit(env, a, *res.Claim, dest, data, now)
		if err != nil || !done.Usable() {
			t.Fatalf("commit: %v %v", done, err)
		}
		return done
	}
	attempt1, attempt2 := domain.AttemptID("att_1"), domain.AttemptID("att_2")

	t.Run("reservation is idempotent and never overwrites", func(t *testing.T) {
		env := open(t)
		first := newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, []byte("one"))
		got := mustReserve(t, env, first)
		if got.ID() != first.ID() || got.State() != artifacts.StateReserved || got.Disposition() != artifacts.DispositionActive || got.Version() != 1 || got.StagingKey() != first.StagingKey() {
			t.Fatalf("reserved: %+v", got)
		}
		if states := env.ObjectStates(t, ScopeA, first.ID()); len(states) != 1 || states[first.StagingKey()] != "live" {
			t.Fatalf("the staging object record: %v", states)
		}
		if actions := env.AuditActions(t, ScopeA, first.ID()); !slices.Equal(actions, []string{"artifact.reserved"}) {
			t.Fatalf("audit: %v", actions)
		}
		// The same attempt asking for the same name again, even with a different
		// identity and content, gets the stored artifact and writes nothing.
		again := newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, []byte("other content"))
		res, err := reserve(env, again)
		if err != nil || res.Created || res.Artifact.ID() != first.ID() || res.Artifact.Digest() != first.Digest() {
			t.Fatalf("repeated reservation: %+v %v", res, err)
		}
		if _, err := env.Repository().Get(ctx, ScopeA, again.ID()); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("a repeated reservation stored the second identity: %v", err)
		}
		if actions := env.AuditActions(t, ScopeA, first.ID()); len(actions) != 1 {
			t.Fatalf("a repeated reservation wrote audit: %v", actions)
		}
		// Names are per attempt, project and tenant.
		for _, a := range []artifacts.Artifact{
			newFile(t, ScopeA, attempt2, "report", artifacts.RoleOutput, []byte("one")),
			newFile(t, ScopeA2, attempt1, "report", artifacts.RoleOutput, []byte("one")),
			newFile(t, ScopeB, attempt1, "report", artifacts.RoleOutput, []byte("one")),
		} {
			mustReserve(t, env, a)
		}
	})

	t.Run("an identity collision never replaces an artifact", func(t *testing.T) {
		env := open(t)
		first := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, []byte("one")))
		// A different name and attempt under the same artifact identity.
		size := int64(3)
		clash, err := artifacts.NewFile(first.ID(), artifacts.Reservation{Scope: ScopeA, Attempt: attempt2, Run: "run_1", Step: "build", Name: "other", Kind: artifacts.KindFile,
			Role: artifacts.RoleOutput, Digest: artifacts.DigestOf([]byte("two")), MediaType: "text/plain", Size: &size, Provenance: Source},
			artifacts.ObjectKey(ScopeA, first.ID(), artifacts.SegmentStaging, "clash"), contractNow.Add(10*time.Minute), contractNow)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := reserve(env, clash); err == nil {
			t.Fatalf("a colliding identity was reserved: %+v", res)
		}
		got, err := env.Repository().Get(ctx, ScopeA, first.ID())
		if err != nil || got.Name() != "report" || got.Attempt() != attempt1 || got.Digest() != first.Digest() || got.StagingKey() != first.StagingKey() {
			t.Fatalf("the stored artifact changed: %+v %v", got, err)
		}
		if _, err := env.Repository().FindByName(ctx, ScopeA, attempt2, "other"); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("the colliding reservation is findable by its name: %v", err)
		}
		if states := env.ObjectStates(t, ScopeA, first.ID()); len(states) != 1 || states[first.StagingKey()] != "live" {
			t.Fatalf("object records after the refused reservation: %v", states)
		}
		if actions := env.AuditActions(t, ScopeA, first.ID()); !slices.Equal(actions, []string{"artifact.reserved"}) {
			t.Fatalf("audit: %v", actions)
		}
	})

	t.Run("an artifact identity is unique within its project, not across projects", func(t *testing.T) {
		env := open(t)
		first := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, []byte("one")))
		size := int64(3)
		sibling, err := artifacts.NewFile(first.ID(), artifacts.Reservation{Scope: ScopeA2, Attempt: attempt2, Run: "run_1", Step: "build", Name: "other", Kind: artifacts.KindFile,
			Role: artifacts.RoleOutput, Digest: artifacts.DigestOf([]byte("two")), MediaType: "text/plain", Size: &size, Provenance: Source},
			artifacts.ObjectKey(ScopeA2, first.ID(), artifacts.SegmentStaging, "sibling"), contractNow.Add(10*time.Minute), contractNow)
		if err != nil {
			t.Fatal(err)
		}
		res, err := reserve(env, sibling)
		if err != nil || !res.Created || res.Artifact.ID() != first.ID() {
			t.Fatalf("the same identity in a sibling project: %+v %v", res, err)
		}
		// Each project sees only its own artifact, objects and audit.
		a, err := env.Repository().Get(ctx, ScopeA, first.ID())
		if err != nil || a.Name() != "report" || a.Digest() != first.Digest() || a.StagingKey() != first.StagingKey() {
			t.Fatalf("the first project's artifact: %+v %v", a, err)
		}
		b, err := env.Repository().Get(ctx, ScopeA2, first.ID())
		if err != nil || b.Name() != "other" || b.Digest() != sibling.Digest() || b.StagingKey() != sibling.StagingKey() {
			t.Fatalf("the sibling project's artifact: %+v %v", b, err)
		}
		if states := env.ObjectStates(t, ScopeA, first.ID()); len(states) != 1 || states[first.StagingKey()] != "live" {
			t.Fatalf("the first project's objects: %v", states)
		}
		if states := env.ObjectStates(t, ScopeA2, first.ID()); len(states) != 1 || states[sibling.StagingKey()] != "live" {
			t.Fatalf("the sibling project's objects: %v", states)
		}
		for _, sc := range []domain.Scope{ScopeA, ScopeA2} {
			if actions := env.AuditActions(t, sc, first.ID()); !slices.Equal(actions, []string{"artifact.reserved"}) {
				t.Fatalf("audit of %s: %v", sc.ProjectID, actions)
			}
		}
		// Reusing the identity inside either project is still refused.
		again, err := artifacts.NewFile(first.ID(), artifacts.Reservation{Scope: ScopeA2, Attempt: attempt1, Run: "run_1", Step: "build", Name: "third", Kind: artifacts.KindFile,
			Role: artifacts.RoleOutput, Digest: artifacts.DigestOf([]byte("three")), MediaType: "text/plain", Size: &size, Provenance: Source},
			artifacts.ObjectKey(ScopeA2, first.ID(), artifacts.SegmentStaging, "third"), contractNow.Add(10*time.Minute), contractNow)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := reserve(env, again); err == nil {
			t.Fatalf("a collision within the sibling project was reserved: %+v", res)
		}
		if got, err := env.Repository().Get(ctx, ScopeA2, first.ID()); err != nil || got.Name() != "other" {
			t.Fatalf("the sibling artifact changed: %+v %v", got, err)
		}
	})

	t.Run("reads are scoped to the project and the producing attempt", func(t *testing.T) {
		env := open(t)
		a := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, []byte("one")))
		repo := env.Repository()
		if got, err := repo.Get(ctx, ScopeA, a.ID()); err != nil || got.ID() != a.ID() {
			t.Fatalf("get: %v %v", got, err)
		}
		if got, err := repo.GetForAttempt(ctx, ScopeA, attempt1, a.ID()); err != nil || got.ID() != a.ID() {
			t.Fatalf("get for attempt: %v %v", got, err)
		}
		if got, err := repo.FindByName(ctx, ScopeA, attempt1, "report"); err != nil || got.ID() != a.ID() {
			t.Fatalf("find by name: %v %v", got, err)
		}
		for name, err := range map[string]error{
			"get from the sibling project":  second(repo.Get(ctx, ScopeA2, a.ID())),
			"get from another tenant":       second(repo.Get(ctx, ScopeB, a.ID())),
			"get for another attempt":       second(repo.GetForAttempt(ctx, ScopeA, attempt2, a.ID())),
			"find in the sibling project":   second(repo.FindByName(ctx, ScopeA2, attempt1, "report")),
			"find in another tenant":        second(repo.FindByName(ctx, ScopeB, attempt1, "report")),
			"find for another attempt":      second(repo.FindByName(ctx, ScopeA, attempt2, "report")),
			"get an unknown artifact":       second(repo.Get(ctx, ScopeA, "art_unknown")),
			"transfer in another tenant":    repo.RecordTransfer(ctx, ScopeB, a.ID(), contractNow.Add(time.Hour), contractNow),
			"completion in another tenant":  second(begin(env, withScope(a, ScopeB), "tok", sealed(a, "x"), contractNow)),
			"completion by another attempt": second(begin(env, withAttempt(a, attempt2), "tok", sealed(a, "x"), contractNow)),
			"pin from another tenant":       repo.Pin(ctx, ScopeB, a.ID(), artifacts.PinOwner{Kind: "run", ID: "r"}, contractNow),
			"retention in another tenant":   env.Cleanup().BeginRetentionDeletion(ctx, ScopeB, a.ID(), contractNow, worker),
		} {
			if !errors.Is(err, artifacts.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		other := mustReserve(t, env, newFile(t, ScopeA, attempt2, "other", artifacts.RoleOutput, []byte("two")))
		refs, err := repo.ReferencesForAttempt(ctx, ScopeA, attempt1, []domain.ArtifactID{a.ID(), other.ID(), "art_unknown"})
		if err != nil || len(refs) != 1 || refs[a.ID()].Usable || refs[a.ID()].State != artifacts.StateReserved || refs[a.ID()].Attempt != attempt1 {
			t.Fatalf("references: %+v %v", refs, err)
		}
		if refs, err := repo.ReferencesForAttempt(ctx, ScopeB, attempt1, []domain.ArtifactID{a.ID()}); err != nil || len(refs) != 0 {
			t.Fatalf("references from another tenant: %+v %v", refs, err)
		}
	})

	t.Run("an attempt's files stay within its aggregate limits", func(t *testing.T) {
		env := open(t)
		mustReserve(t, env, newFile(t, ScopeA, attempt1, "a", artifacts.RoleOutput, []byte("1")))
		mustReserve(t, env, newFile(t, ScopeA, attempt1, "b", artifacts.RoleOutput, []byte("2")))
		if _, err := reserve(env, newFile(t, ScopeA, attempt1, "c", artifacts.RoleOutput, []byte("3"))); !errors.Is(err, artifacts.ErrLimitExceeded) {
			t.Fatalf("a third output file: %v", err)
		}
		if _, err := env.Repository().FindByName(ctx, ScopeA, attempt1, "c"); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("a refused reservation was stored: %v", err)
		}
		// Limits are per role and per attempt.
		mustReserve(t, env, newFile(t, ScopeA, attempt1, "log", artifacts.RoleDiagnostic, []byte("d")))
		if _, err := reserve(env, newFile(t, ScopeA, attempt1, "log2", artifacts.RoleDiagnostic, []byte("d"))); !errors.Is(err, artifacts.ErrLimitExceeded) {
			t.Fatalf("a second diagnostic file: %v", err)
		}
		mustReserve(t, env, newFile(t, ScopeA, attempt2, "a", artifacts.RoleOutput, []byte("1")))
		// Bytes count too.
		big := make([]byte, contractLim.OutputBytes)
		if _, err := reserve(env, newFile(t, ScopeA, attempt2, "big", artifacts.RoleOutput, big)); !errors.Is(err, artifacts.ErrLimitExceeded) {
			t.Fatalf("an output over the byte limit: %v", err)
		}
	})

	t.Run("an OCI registration is usable, has no object and is not a file", func(t *testing.T) {
		env := open(t)
		ref, err := artifacts.ParseRegistryRef("registry.example.test/acme/app")
		if err != nil {
			t.Fatal(err)
		}
		digest := artifacts.DigestOf([]byte("manifest"))
		res := artifacts.Reservation{Scope: ScopeA, Attempt: attempt1, Run: "run_1", Step: "build", Name: "image", Kind: artifacts.KindOCI,
			Role: artifacts.RoleOutput, Digest: digest, Registry: ref.String(), Provenance: Source}
		id := nextID()
		a, err := artifacts.NewRegistered(id, res, artifacts.OCIVerification{Repository: ref.String(), Digest: digest,
			ManifestMediaType: "application/vnd.oci.image.manifest.v1+json", ManifestSize: 512, Provenance: Source}, contractNow)
		if err != nil {
			t.Fatal(err)
		}
		got := mustReserve(t, env, a)
		if !got.Usable() || got.State() != artifacts.StateRegistered {
			t.Fatalf("registration: %+v", got)
		}
		if states := env.ObjectStates(t, ScopeA, id); len(states) != 0 {
			t.Fatalf("a registration has object records: %v", states)
		}
		if actions := env.AuditActions(t, ScopeA, id); !slices.Equal(actions, []string{"artifact.registered"}) {
			t.Fatalf("audit: %v", actions)
		}
		if c, err := begin(env, a, "tok", sealed(a, "x"), contractNow); err != nil || c.Claim != nil || !c.Artifact.Usable() {
			t.Fatalf("completing a registration: %+v %v", c, err)
		}
		if err := env.Repository().RecordTransfer(ctx, ScopeA, id, contractNow.Add(time.Hour), contractNow); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("a transfer for a registration: %v", err)
		}
		// Registrations do not count against the file limits.
		mustReserve(t, env, newFile(t, ScopeA, attempt1, "a", artifacts.RoleOutput, []byte("1")))
		mustReserve(t, env, newFile(t, ScopeA, attempt1, "b", artifacts.RoleOutput, []byte("2")))
	})

	t.Run("source set provenance is recorded with the artifact and replays", func(t *testing.T) {
		env := open(t)
		ref, err := artifacts.ParseRegistryRef("registry.example.test/acme/set-app")
		if err != nil {
			t.Fatal(err)
		}
		digest := artifacts.DigestOf([]byte("set manifest"))
		res := artifacts.Reservation{Scope: ScopeA, Attempt: attempt1, Run: "run_1", Step: "build", Name: "image", Kind: artifacts.KindOCI,
			Role: artifacts.RoleOutput, Digest: digest, Registry: ref.String(), Provenance: SourceSet}
		id := nextID()
		a, err := artifacts.NewRegistered(id, res, artifacts.OCIVerification{Repository: ref.String(), Digest: digest,
			ManifestMediaType: "application/vnd.oci.image.manifest.v1+json", ManifestSize: 512, Provenance: SourceSet}, contractNow)
		if err != nil {
			t.Fatal(err)
		}
		got := mustReserve(t, env, a)
		if got.Provenance() != SourceSet || got.Provenance().RepositoryURL != "" || got.Provenance().Commit != "" {
			t.Fatalf("the registration's provenance: %+v", got.Provenance())
		}
		stored, err := env.Repository().Get(ctx, ScopeA, id)
		if err != nil || stored.Provenance() != SourceSet {
			t.Fatalf("the stored provenance: %+v %v", stored.Provenance(), err)
		}
		commits := stored.Provenance().SourceSet.Commits()
		if len(commits) != 2 || commits[0].RepositoryID != "repo_api" || commits[1].RepositoryID != "repo_web" || stored.Provenance().SourceSet.Digest != SourceSet.SourceSet.Digest {
			t.Fatalf("the commits are ordered by repository: %+v", commits)
		}
		refs, err := env.Repository().ReferencesForAttempt(ctx, ScopeA, attempt1, []domain.ArtifactID{id})
		if err != nil || refs[id].Provenance != SourceSet {
			t.Fatalf("the consumer view: %+v %v", refs[id], err)
		}
		// A file of the same attempt keeps its own provenance; a source set never
		// reads back as a single source.
		file := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, []byte("one")))
		if file.Provenance() != Source {
			t.Fatalf("a single source reads back unchanged: %+v", file.Provenance())
		}
		// A repeat of the registration is the stored one.
		again, err := reserve(env, a)
		if err != nil || again.Created || again.Artifact.Provenance() != SourceSet {
			t.Fatalf("a repeated registration: %+v %v", again, err)
		}
		// Other tenants never see it.
		if _, err := env.Repository().Get(ctx, ScopeB, id); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("a foreign tenant read the provenance: %v", err)
		}
	})

	t.Run("completion is a fenced lease whose losers commit nothing", func(t *testing.T) {
		env := open(t)
		data := []byte("verified content")
		a := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, data))
		destOld, destNew := sealed(a, "old"), sealed(a, "new")

		first, err := begin(env, a, "tok-1", destOld, contractNow)
		if err != nil || first.Claim == nil || first.Claim.Generation != 1 || first.Claim.Token != "tok-1" || !first.Claim.Until.Equal(contractNow.Add(30*time.Second)) {
			t.Fatalf("first claim: %+v %v", first, err)
		}
		if _, err := begin(env, a, "tok-2", destNew, contractNow.Add(10*time.Second)); !errors.Is(err, artifacts.ErrInProgress) {
			t.Fatalf("a second completion while the lease is live: %v", err)
		}
		// The lease lapses: another completion takes over under a new generation.
		later := contractNow.Add(time.Minute)
		second, err := begin(env, a, "tok-2", destNew, later)
		if err != nil || second.Claim == nil || second.Claim.Generation != 2 {
			t.Fatalf("takeover: %+v %v", second, err)
		}
		if _, err := commit(env, a, *first.Claim, destOld, data, later); !errors.Is(err, artifacts.ErrClaimLost) {
			t.Fatalf("a fenced-out completion: %v", err)
		}
		if got, _ := env.Repository().Get(ctx, ScopeA, a.ID()); got.State() != artifacts.StateReserved || got.SealedKey() != "" {
			t.Fatalf("a refused completion changed the artifact: %+v", got)
		}
		if _, err := commit(env, a, artifacts.Claim{Token: "tok-2", Generation: 2}, destNew, data, later.Add(time.Hour)); !errors.Is(err, artifacts.ErrClaimLost) {
			t.Fatalf("a completion after its lease expired: %v", err)
		}
		if _, err := commit(env, a, *second.Claim, destNew, []byte("different bytes"), later); !errors.Is(err, artifacts.ErrDigestMismatch) {
			t.Fatalf("content that is not the declaration: %v", err)
		}
		done, err := commit(env, a, *second.Claim, destNew, data, later)
		if err != nil || !done.Usable() || done.SealedKey() != destNew || done.Version() != 2 || done.VerifiedAt() == nil || done.Claim().Token != "" {
			t.Fatalf("commit: %+v %v", done, err)
		}
		// An identical completion that lost the race finds the winner.
		if again, err := commit(env, a, *second.Claim, destNew, data, later); err != nil || again.SealedKey() != destNew {
			t.Fatalf("a repeated completion: %+v %v", again, err)
		}
		if res, err := begin(env, a, "tok-3", sealed(a, "third"), later); err != nil || res.Claim != nil || !res.Artifact.Usable() {
			t.Fatalf("beginning a usable artifact: %+v %v", res, err)
		}
		states := env.ObjectStates(t, ScopeA, a.ID())
		if states[a.StagingKey()] != "live" || states[destOld] != "live" || states[destNew] != "live:winner" || len(states) != 3 {
			t.Fatalf("object records: %v", states)
		}
		if actions := env.AuditActions(t, ScopeA, a.ID()); !slices.Equal(actions, []string{"artifact.completed", "artifact.reserved"}) {
			t.Fatalf("audit: %v", actions)
		}
	})

	t.Run("a released claim lets another completion begin at once", func(t *testing.T) {
		env := open(t)
		a := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, []byte("x")))
		first, err := begin(env, a, "tok-1", sealed(a, "one"), contractNow)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.Repository().ReleaseClaim(ctx, ScopeA, a.ID(), "someone-else"); err != nil {
			t.Fatal(err)
		}
		if _, err := begin(env, a, "tok-2", sealed(a, "two"), contractNow); !errors.Is(err, artifacts.ErrInProgress) {
			t.Fatalf("a foreign release ended the lease: %v", err)
		}
		if err := env.Repository().ReleaseClaim(ctx, ScopeA, a.ID(), first.Claim.Token); err != nil {
			t.Fatal(err)
		}
		next, err := begin(env, a, "tok-2", sealed(a, "two"), contractNow)
		if err != nil || next.Claim == nil || next.Claim.Generation != 2 {
			t.Fatalf("after the release: %+v %v", next, err)
		}
	})

	t.Run("pins and retention are totally ordered", func(t *testing.T) {
		env := open(t)
		data := []byte("keep me")
		a := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, data))
		owner := artifacts.PinOwner{Kind: "run", ID: "run_1"}
		if err := env.Repository().Pin(ctx, ScopeA, a.ID(), owner, contractNow); !errors.Is(err, artifacts.ErrIncomplete) {
			t.Fatalf("pinning unverified content: %v", err)
		}
		if err := env.Cleanup().BeginRetentionDeletion(ctx, ScopeA, a.ID(), contractNow, worker); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("retention of unverified content: %v", err)
		}
		done := complete(t, env, a, data, contractNow)
		list := func() []domain.ArtifactID {
			subjects, err := env.Cleanup().RetentionCandidates(ctx, ScopeA.TenantID, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			var ids []domain.ArtifactID
			for _, s := range subjects {
				ids = append(ids, s.ID)
			}
			return ids
		}
		if ids := list(); !slices.Equal(ids, []domain.ArtifactID{done.ID()}) {
			t.Fatalf("candidates: %v", ids)
		}
		for i := 0; i < 2; i++ { // pinning twice is idempotent
			if err := env.Repository().Pin(ctx, ScopeA, a.ID(), owner, contractNow); err != nil {
				t.Fatalf("pin %d: %v", i, err)
			}
		}
		if err := env.Repository().Pin(ctx, ScopeA, a.ID(), artifacts.PinOwner{Kind: "gate", ID: "g_1"}, contractNow); err != nil {
			t.Fatal(err)
		}
		if ids := list(); len(ids) != 0 {
			t.Fatalf("a pinned artifact is a retention candidate: %v", ids)
		}
		if err := env.Cleanup().BeginRetentionDeletion(ctx, ScopeA, a.ID(), contractNow, worker); !errors.Is(err, artifacts.ErrPinned) {
			t.Fatalf("retention of a pinned artifact: %v", err)
		}
		// Every owner must release before retention may proceed.
		if err := env.Repository().Unpin(ctx, ScopeA, a.ID(), owner); err != nil {
			t.Fatal(err)
		}
		if err := env.Cleanup().BeginRetentionDeletion(ctx, ScopeA, a.ID(), contractNow, worker); !errors.Is(err, artifacts.ErrPinned) {
			t.Fatalf("retention while one owner remains: %v", err)
		}
		if err := env.Repository().Unpin(ctx, ScopeA, a.ID(), artifacts.PinOwner{Kind: "gate", ID: "g_1"}); err != nil {
			t.Fatal(err)
		}
		if err := env.Repository().Unpin(ctx, ScopeA, a.ID(), artifacts.PinOwner{Kind: "gate", ID: "g_1"}); err != nil {
			t.Fatalf("unpinning twice: %v", err)
		}
		if err := env.Cleanup().BeginRetentionDeletion(ctx, ScopeA, a.ID(), contractNow.Add(time.Hour), worker); err != nil {
			t.Fatalf("retention: %v", err)
		}
		removed, err := env.Repository().Get(ctx, ScopeA, a.ID())
		if err != nil || removed.Disposition() != artifacts.DispositionRemoved || removed.Usable() || removed.Version() != 3 {
			t.Fatalf("removed artifact: %+v %v", removed, err)
		}
		if err := env.Repository().Pin(ctx, ScopeA, a.ID(), owner, contractNow); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("pinning a removed artifact: %v", err)
		}
		if ids := list(); len(ids) != 0 {
			t.Fatalf("a removed artifact is a candidate: %v", ids)
		}
		if refs, _ := env.Repository().ReferencesForAttempt(ctx, ScopeA, attempt1, []domain.ArtifactID{a.ID()}); refs[a.ID()].Usable {
			t.Fatal("a removed artifact is still usable")
		}
		states := env.ObjectStates(t, ScopeA, a.ID())
		pending := 0
		for _, st := range states {
			if st == "delete_pending:winner" {
				pending++
			}
		}
		if pending != 1 {
			t.Fatalf("the sealed object's deletion intent: %v", states)
		}
		if actions := env.AuditActions(t, ScopeA, a.ID()); !slices.Contains(actions, "artifact.removed") {
			t.Fatalf("audit: %v", actions)
		}
		// Candidates are tenant scoped.
		if ids, err := env.Cleanup().RetentionCandidates(ctx, ScopeB.TenantID, "", 10); err != nil || len(ids) != 0 {
			t.Fatalf("tenant B's candidates: %v %v", ids, err)
		}
	})

	t.Run("expired idle reservations are abandoned, live ones are not", func(t *testing.T) {
		env := open(t)
		idle := mustReserve(t, env, newFile(t, ScopeA, attempt1, "idle", artifacts.RoleOutput, []byte("i")))
		claimed := mustReserve(t, env, newFile(t, ScopeA, attempt1, "claimed", artifacts.RoleOutput, []byte("c")))
		renewed := mustReserve(t, env, newFile(t, ScopeA, attempt2, "renewed", artifacts.RoleOutput, []byte("r")))
		foreign := mustReserve(t, env, newFile(t, ScopeB, attempt1, "foreign", artifacts.RoleOutput, []byte("f")))
		expiry := contractNow.Add(10 * time.Minute) // every reservation's transfer horizon
		// A completion lease still held at the sweep keeps its reservation.
		if _, err := begin(env, claimed, "tok", sealed(claimed, "s"), expiry.Add(59*time.Minute)); err != nil {
			t.Fatal(err)
		}
		// A renewed transfer moves the horizon.
		if err := env.Repository().RecordTransfer(ctx, ScopeA, renewed.ID(), expiry.Add(3*time.Hour), contractNow); err != nil {
			t.Fatal(err)
		}
		now := expiry.Add(time.Hour)
		if n, err := env.Cleanup().AbandonExpired(ctx, ScopeA.TenantID, now.Add(-time.Second), policy); err != nil || n != 0 {
			t.Fatalf("before the abandon horizon: %d %v", n, err)
		}
		n, err := env.Cleanup().AbandonExpired(ctx, ScopeA.TenantID, now.Add(time.Minute), policy)
		if err != nil || n != 1 {
			t.Fatalf("abandoned %d: %v", n, err)
		}
		get := func(a artifacts.Artifact, scope domain.Scope) artifacts.Artifact {
			got, err := env.Repository().Get(ctx, scope, a.ID())
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		if got := get(idle, ScopeA); got.Disposition() != artifacts.DispositionAbandoned || got.Version() != 2 || got.Usable() || got.Open() {
			t.Fatalf("idle: %+v", got)
		}
		for name, a := range map[string]artifacts.Artifact{"claimed": claimed, "renewed": renewed} {
			if got := get(a, ScopeA); got.Disposition() != artifacts.DispositionActive {
				t.Errorf("%s was abandoned: %+v", name, got)
			}
		}
		if got := get(foreign, ScopeB); got.Disposition() != artifacts.DispositionActive {
			t.Fatalf("another tenant's reservation was swept: %+v", got)
		}
		if n, err := env.Cleanup().AbandonExpired(ctx, ScopeA.TenantID, now.Add(time.Minute), policy); err != nil || n != 0 {
			t.Fatalf("a repeated sweep: %d %v", n, err)
		}
		if _, err := begin(env, idle, "tok", sealed(idle, "late"), now.Add(2*time.Minute)); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("completing an abandoned reservation: %v", err)
		}
		if tenants, err := env.Cleanup().Tenants(ctx); err != nil || !slices.Contains(tenants, TenantA) || !slices.Contains(tenants, TenantB) {
			t.Fatalf("tenants: %v %v", tenants, err)
		}
	})

	t.Run("unreferenced objects are deleted under a leased, fenced intent", func(t *testing.T) {
		env := open(t)
		data := []byte("sealed")
		a := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, data))
		destLost, destWin := sealed(a, "lost"), sealed(a, "win")
		if _, err := begin(env, a, "tok-1", destLost, contractNow); err != nil {
			t.Fatal(err)
		}
		later := contractNow.Add(2 * time.Minute) // the first lease lapsed
		win, err := begin(env, a, "tok-2", destWin, later)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := commit(env, a, *win.Claim, destWin, data, later); err != nil {
			t.Fatal(err)
		}
		cleanup := env.Cleanup()
		claimed := func(now time.Time, token string) map[string]artifacts.DeletionClaim {
			t.Helper()
			claims, err := cleanup.ClaimDeletions(ctx, ScopeA.TenantID, now, policy, token)
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]artifacts.DeletionClaim{}
			for _, c := range claims {
				if c.Scope != ScopeA || c.ArtifactID != a.ID() || c.Token != token {
					t.Fatalf("claim %+v", c)
				}
				out[c.Key] = c
			}
			if len(out) != len(claims) {
				t.Fatalf("a key was claimed twice: %+v", claims)
			}
			return out
		}
		// The lost candidate is unreferenced and past its in-flight grace; the
		// staging object can still be written by a live transfer; the winner is
		// never claimed.
		first := claimed(later, "w-1")
		if len(first) != 1 || first[destLost].Key == "" {
			t.Fatalf("first claim: %+v", first)
		}
		if again := claimed(later, "w-2"); len(again) != 0 {
			t.Fatalf("a leased deletion was claimed twice: %+v", again)
		}
		if err := cleanup.AckDeletion(ctx, artifacts.DeletionClaim{Scope: ScopeA, ArtifactID: a.ID(), Key: destLost, Token: "w-2"}, later, policy); !errors.Is(err, artifacts.ErrClaimLost) {
			t.Fatalf("a worker without the lease acknowledged: %v", err)
		}
		if err := cleanup.AckDeletion(ctx, first[destLost], later.Add(time.Minute), policy); err != nil {
			t.Fatalf("ack: %v", err)
		}
		if err := cleanup.AckDeletion(ctx, first[destLost], later.Add(time.Minute), policy); !errors.Is(err, artifacts.ErrClaimLost) {
			t.Fatalf("a repeated acknowledgment: %v", err)
		}
		// Once the staging horizon and its grace passed, the usable artifact's
		// staging object is unreferenced too.
		now := later.Add(time.Hour)
		second := claimed(now, "w-3")
		if len(second) != 1 || second[a.StagingKey()].Key == "" {
			t.Fatalf("second claim: %+v", second)
		}
		if err := cleanup.AckDeletion(ctx, second[a.StagingKey()], now.Add(time.Minute), policy); err != nil {
			t.Fatalf("ack staging: %v", err)
		}
		states := env.ObjectStates(t, ScopeA, a.ID())
		if states[a.StagingKey()] != "deleted" || states[destLost] != "deleted" || states[destWin] != "live:winner" {
			t.Fatalf("object records: %v", states)
		}
		if again := claimed(now.Add(time.Hour), "w-4"); len(again) != 0 {
			t.Fatalf("deleted objects or the winner were claimed: %+v", again)
		}
	})

	t.Run("a renewed transfer reopens a staging object queued for deletion", func(t *testing.T) {
		env := open(t)
		data := []byte("r")
		a := mustReserve(t, env, newFile(t, ScopeA, attempt1, "report", artifacts.RoleOutput, data))
		complete(t, env, a, data, contractNow)
		now := contractNow.Add(time.Hour)
		claims, err := env.Cleanup().ClaimDeletions(ctx, ScopeA.TenantID, now, policy, "w-1")
		if err != nil || len(claims) != 1 || claims[0].Key != a.StagingKey() {
			t.Fatalf("claims: %+v %v", claims, err)
		}
		// A renewed instruction can still land a late write: the deletion intent
		// is cancelled and the write horizon moves.
		if err := env.Repository().RecordTransfer(ctx, ScopeA, a.ID(), now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		if err := env.Cleanup().AckDeletion(ctx, claims[0], now.Add(time.Second), policy); !errors.Is(err, artifacts.ErrClaimLost) {
			t.Fatalf("an acknowledgment after the reopen: %v", err)
		}
		if st := env.ObjectStates(t, ScopeA, a.ID())[a.StagingKey()]; st != "live" {
			t.Fatalf("staging object is %q", st)
		}
		if again, err := env.Cleanup().ClaimDeletions(ctx, ScopeA.TenantID, now.Add(30*time.Minute), policy, "w-2"); err != nil || len(again) != 0 {
			t.Fatalf("claimed inside the renewed horizon: %+v %v", again, err)
		}
		later := now.Add(2 * time.Hour)
		again, err := env.Cleanup().ClaimDeletions(ctx, ScopeA.TenantID, later, policy, "w-3")
		if err != nil || len(again) != 1 || again[0].Key != a.StagingKey() {
			t.Fatalf("claims after the horizon: %+v %v", again, err)
		}
		if err := env.Cleanup().AckDeletion(ctx, again[0], later.Add(time.Second), policy); err != nil {
			t.Fatalf("ack: %v", err)
		}
		// A renewal for an abandoned reservation is refused.
		b := mustReserve(t, env, newFile(t, ScopeA, attempt2, "idle", artifacts.RoleOutput, []byte("i")))
		if n, err := env.Cleanup().AbandonExpired(ctx, ScopeA.TenantID, later, policy); err != nil || n != 1 {
			t.Fatalf("abandon: %d %v", n, err)
		}
		if err := env.Repository().RecordTransfer(ctx, ScopeA, b.ID(), later.Add(time.Hour), later); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("a transfer for an abandoned reservation: %v", err)
		}
	})
}

func second[T any](_ T, err error) error { return err }

func withScope(a artifacts.Artifact, scope domain.Scope) artifacts.Artifact {
	r := recordOf(a)
	r.Reservation.Scope = scope
	out, err := artifacts.Restore(r)
	if err != nil {
		panic(err)
	}
	return out
}

func withAttempt(a artifacts.Artifact, attempt domain.AttemptID) artifacts.Artifact {
	r := recordOf(a)
	r.Reservation.Attempt = attempt
	out, err := artifacts.Restore(r)
	if err != nil {
		panic(err)
	}
	return out
}

func recordOf(a artifacts.Artifact) artifacts.Record { return record(a) }
