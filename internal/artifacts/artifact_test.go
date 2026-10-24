package artifacts_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	scope  = domain.Scope{TenantID: "t_a", ProjectID: "p_1"}
	prov   = artifacts.Provenance{RepositoryURL: "https://git.example.test/org/app.git", Commit: strings.Repeat("a", 40)}
	t0     = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	hello  = []byte("hello")
	digest = artifacts.DigestOf(hello)
)

func fileRes() artifacts.Reservation {
	s := int64(len(hello))
	return artifacts.Reservation{Scope: scope, Attempt: "att_1", Run: "run_1", Step: "build", Name: "report", Kind: artifacts.KindFile, Role: artifacts.RoleOutput,
		Digest: digest, MediaType: "text/plain", Size: &s, Provenance: prov}
}

func ociRes() artifacts.Reservation {
	return artifacts.Reservation{Scope: scope, Attempt: "att_1", Run: "run_1", Step: "build", Name: "image", Kind: artifacts.KindOCI, Role: artifacts.RoleOutput,
		Digest: digest, Registry: "registry.example.test/team/app", Provenance: prov}
}

func newFile(t *testing.T) artifacts.Artifact {
	t.Helper()
	a, err := artifacts.NewFile("art_1", fileRes(), "artifacts/t_a/p_1/art_1/staging/n", t0.Add(time.Minute), t0)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestArtifactTransitions: valid and rejected transitions, immutable
// provenance, role/kind rules, bounds, and the incomplete-reference behavior.
func TestArtifactTransitions(t *testing.T) {
	t.Run("a reserved file is never usable and completes only with the declared content", func(t *testing.T) {
		a := newFile(t)
		if a.State() != artifacts.StateReserved || a.Usable() || !a.Open() || a.Version() != 1 {
			t.Fatalf("new reservation: %v usable=%v", a.State(), a.Usable())
		}
		ref := a.Reference()
		if ref.Usable || ref.State != artifacts.StateReserved || ref.Attempt != "att_1" || ref.Scope != scope {
			t.Fatalf("an incomplete artifact must be a visible, unusable reference: %+v", ref)
		}
		bad := artifacts.Verification{Digest: artifacts.DigestOf([]byte("other")), Size: 5}
		if _, err := a.Complete(bad, "artifacts/t_a/p_1/art_1/sealed/x", t0); !errors.Is(err, artifacts.ErrDigestMismatch) {
			t.Fatalf("different content must not complete: %v", err)
		}
		short := artifacts.Verification{Digest: digest, Size: 4}
		if _, err := a.Complete(short, "artifacts/t_a/p_1/art_1/sealed/x", t0); !errors.Is(err, artifacts.ErrDigestMismatch) {
			t.Fatalf("a wrong size must not complete: %v", err)
		}
		good := artifacts.Verification{Digest: digest, Size: 5}
		if _, err := a.Complete(good, a.StagingKey(), t0); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("staging bytes cannot be the sealed destination: %v", err)
		}
		if _, err := a.Complete(good, "", t0); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("a destination is required: %v", err)
		}
		done, err := a.Complete(good, "artifacts/t_a/p_1/art_1/sealed/x", t0.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if done.State() != artifacts.StateUsable || !done.Usable() || done.Version() != 2 || done.SealedKey() == "" || done.VerifiedAt() == nil {
			t.Fatalf("completed: %+v", done)
		}
		if a.State() != artifacts.StateReserved {
			t.Fatal("a transition must not mutate the original value")
		}
		if _, err := done.Complete(good, "artifacts/t_a/p_1/art_1/sealed/y", t0); err == nil {
			t.Fatal("a usable artifact cannot be completed again")
		}
		if done.Provenance() != prov || done.Digest() != digest || done.Attempt() != "att_1" {
			t.Fatal("provenance, content and producer are immutable across transitions")
		}
	})

	t.Run("abandon and remove are bookkeeping, not runtime states", func(t *testing.T) {
		a := newFile(t)
		ab, err := a.Abandon(t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if ab.State() != artifacts.StateReserved || ab.Disposition() != artifacts.DispositionAbandoned || ab.Usable() || ab.Open() {
			t.Fatalf("abandoned: %v %v", ab.State(), ab.Disposition())
		}
		if _, err := ab.Complete(artifacts.Verification{Digest: digest, Size: 5}, "artifacts/t_a/p_1/art_1/sealed/x", t0); err == nil {
			t.Fatal("an abandoned reservation cannot complete")
		}
		if _, err := ab.Abandon(t0); err == nil {
			t.Fatal("abandoning twice must fail")
		}
		if _, err := ab.Remove(t0); err == nil {
			t.Fatal("unfinished content is never retention-removed")
		}
		done, _ := a.Complete(artifacts.Verification{Digest: digest, Size: 5}, "artifacts/t_a/p_1/art_1/sealed/x", t0)
		if _, err := done.Abandon(t0); err == nil {
			t.Fatal("completed content cannot be abandoned")
		}
		rm, err := done.Remove(t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if rm.Usable() || rm.State() != artifacts.StateUsable || rm.Disposition() != artifacts.DispositionRemoved {
			t.Fatalf("removed: %+v", rm)
		}
		if rm.Reference().Usable {
			t.Fatal("removed content is not a usable reference")
		}
		if _, err := rm.Remove(t0); err == nil {
			t.Fatal("removing twice must fail")
		}
	})

	t.Run("a live completion lease blocks abandonment", func(t *testing.T) {
		a, err := artifacts.Restore(artifacts.Record{ID: "art_1", Reservation: fileRes(), State: artifacts.StateReserved, Disposition: artifacts.DispositionActive,
			StagingKey: "artifacts/t_a/p_1/art_1/staging/n", Version: 1, CreatedAt: t0, UpdatedAt: t0, TransferExpiresAt: ptr(t0),
			Claim: artifacts.Claim{Token: "tok", Generation: 1, Until: t0.Add(time.Minute)}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Abandon(t0.Add(time.Second)); err == nil {
			t.Fatal("a live lease must block abandonment")
		}
		if _, err := a.Abandon(t0.Add(2 * time.Minute)); err != nil {
			t.Fatalf("an expired lease must not: %v", err)
		}
	})

	t.Run("OCI registration exists only once verified and binds provenance", func(t *testing.T) {
		v := artifacts.OCIVerification{Repository: "registry.example.test/team/app", Digest: digest, ManifestSize: 100, Provenance: prov}
		a, err := artifacts.NewRegistered("art_2", ociRes(), v, t0)
		if err != nil {
			t.Fatal(err)
		}
		if a.State() != artifacts.StateRegistered || !a.Usable() || a.Kind() != artifacts.KindOCI {
			t.Fatalf("registered: %v", a.State())
		}
		if size, ok := a.Size(); !ok || size != 100 {
			t.Fatalf("size of a registration is the verified manifest: %d %v", size, ok)
		}
		for name, mutate := range map[string]func(*artifacts.OCIVerification){
			"other digest":     func(v *artifacts.OCIVerification) { v.Digest = artifacts.DigestOf([]byte("x")) },
			"other repository": func(v *artifacts.OCIVerification) { v.Repository = "registry.example.test/foreign/app" },
			"no provenance":    func(v *artifacts.OCIVerification) { v.Provenance = artifacts.Provenance{} },
			"other provenance": func(v *artifacts.OCIVerification) { v.Provenance.Commit = strings.Repeat("b", 40) },
		} {
			bad := v
			mutate(&bad)
			if _, err := artifacts.NewRegistered("art_2", ociRes(), bad, t0); !errors.Is(err, artifacts.ErrRegistryContent) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := artifacts.NewFile("art_3", ociRes(), "k", t0, t0); err == nil {
			t.Error("an OCI reservation is not a file")
		}
		if _, err := a.Complete(artifacts.Verification{Digest: digest, Size: 1}, "artifacts/x", t0); err == nil {
			t.Error("a registration is never completed as a file")
		}
	})

	t.Run("reservation rules", func(t *testing.T) {
		mut := func(name string, f func(*artifacts.Reservation)) {
			r := fileRes()
			f(&r)
			if err := r.Validate(); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("%s: accepted (%v)", name, err)
			}
		}
		mut("bad name", func(r *artifacts.Reservation) { r.Name = "../etc" })
		mut("bad digest", func(r *artifacts.Reservation) { r.Digest = "sha256:abc" })
		mut("uppercase digest", func(r *artifacts.Reservation) { r.Digest = artifacts.Digest("sha256:" + strings.Repeat("A", 64)) })
		mut("unknown role", func(r *artifacts.Reservation) { r.Role = "secret" })
		mut("unknown kind", func(r *artifacts.Reservation) { r.Kind = "dir" })
		mut("file without media type", func(r *artifacts.Reservation) { r.MediaType = "" })
		mut("file with bad media type", func(r *artifacts.Reservation) { r.MediaType = "not a type" })
		mut("file without size", func(r *artifacts.Reservation) { r.Size = nil })
		mut("negative size", func(r *artifacts.Reservation) { n := int64(-1); r.Size = &n })
		mut("oversized", func(r *artifacts.Reservation) { n := artifacts.MaxFileBytes + 1; r.Size = &n })
		mut("file with registry", func(r *artifacts.Reservation) { r.Registry = "registry.example.test/x" })
		mut("credentials in provenance", func(r *artifacts.Reservation) { r.Provenance.RepositoryURL = "https://user:pw@git.example.test/x.git" })
		mut("branch as commit", func(r *artifacts.Reservation) { r.Provenance.Commit = "main" })
		mut("foreign attempt syntax", func(r *artifacts.Reservation) { r.Attempt = "../x" })
		mut("empty step", func(r *artifacts.Reservation) { r.Step = "" })
		oci := func(name string, f func(*artifacts.Reservation)) {
			r := ociRes()
			f(&r)
			if err := r.Validate(); !errors.Is(err, artifacts.ErrInvalid) {
				t.Errorf("oci %s: accepted (%v)", name, err)
			}
		}
		oci("diagnostic role", func(r *artifacts.Reservation) { r.Role = artifacts.RoleDiagnostic })
		oci("bundle role", func(r *artifacts.Reservation) { r.Role = artifacts.RoleBundle })
		oci("with size", func(r *artifacts.Reservation) { n := int64(1); r.Size = &n })
		oci("with media type", func(r *artifacts.Reservation) { r.MediaType = "text/plain" })
		oci("without registry", func(r *artifacts.Reservation) { r.Registry = "" })
		oci("without provenance", func(r *artifacts.Reservation) { r.Provenance = artifacts.Provenance{} })
		if err := fileRes().Validate(); err != nil {
			t.Fatal(err)
		}
		r := fileRes()
		r.Provenance = artifacts.Provenance{}
		if err := r.Validate(); err != nil {
			t.Fatalf("a file produced without source is valid: %v", err)
		}
		r = fileRes()
		zero := int64(0)
		r.Size = &zero
		if err := r.Validate(); err != nil {
			t.Fatalf("an empty file is valid: %v", err)
		}
	})

	t.Run("replay compares the immutable declaration only", func(t *testing.T) {
		a := newFile(t)
		same := fileRes()
		same.Provenance = artifacts.Provenance{} // server-derived, not compared
		if !a.Matches(same) {
			t.Fatal("an identical declaration must match")
		}
		for name, f := range map[string]func(*artifacts.Reservation){
			"digest":    func(r *artifacts.Reservation) { r.Digest = artifacts.DigestOf([]byte("x")) },
			"size":      func(r *artifacts.Reservation) { n := int64(9); r.Size = &n },
			"media":     func(r *artifacts.Reservation) { r.MediaType = "application/json" },
			"role":      func(r *artifacts.Reservation) { r.Role = artifacts.RoleDiagnostic },
			"kind":      func(r *artifacts.Reservation) { *r = ociRes() },
			"name":      func(r *artifacts.Reservation) { r.Name = "other" },
			"attempt":   func(r *artifacts.Reservation) { r.Attempt = "att_2" },
			"tenant":    func(r *artifacts.Reservation) { r.Scope.TenantID = "t_b" },
			"project":   func(r *artifacts.Reservation) { r.Scope.ProjectID = "p_2" },
			"size null": func(r *artifacts.Reservation) { r.Size = nil },
		} {
			r := fileRes()
			f(&r)
			if a.Matches(r) {
				t.Errorf("%s change matched", name)
			}
		}
	})

	t.Run("completion declaration is compared to the reservation", func(t *testing.T) {
		a := newFile(t)
		if err := a.CheckCompletion(digest, 5); err != nil {
			t.Fatal(err)
		}
		if err := a.CheckCompletion(artifacts.DigestOf([]byte("x")), 5); !errors.Is(err, artifacts.ErrDigestMismatch) {
			t.Fatalf("digest: %v", err)
		}
		if err := a.CheckCompletion(digest, 6); !errors.Is(err, artifacts.ErrDigestMismatch) {
			t.Fatalf("size: %v", err)
		}
	})

	t.Run("restoring a corrupted row fails closed", func(t *testing.T) {
		usable := artifacts.Record{ID: "art_1", Reservation: fileRes(), State: artifacts.StateUsable, Disposition: artifacts.DispositionActive,
			StagingKey: "s", SealedKey: "s", Version: 2, CreatedAt: t0, UpdatedAt: t0, VerifiedAt: ptr(t0), VerifiedSize: i64(5)}
		for name, mutate := range map[string]func(*artifacts.Record){
			"sealed equals staging": func(r *artifacts.Record) {},
			"usable without seal":   func(r *artifacts.Record) { r.SealedKey = "" },
			"usable wrong size":     func(r *artifacts.Record) { r.SealedKey = "k"; r.VerifiedSize = i64(4) },
			"usable unverified":     func(r *artifacts.Record) { r.SealedKey = "k"; r.VerifiedAt = nil },
			"reserved with seal":    func(r *artifacts.Record) { r.State = artifacts.StateReserved; r.SealedKey = "k" },
			"abandoned usable":      func(r *artifacts.Record) { r.SealedKey = "k"; r.Disposition = artifacts.DispositionAbandoned },
			"unknown disposition":   func(r *artifacts.Record) { r.SealedKey = "k"; r.Disposition = "gone" },
			"bad version":           func(r *artifacts.Record) { r.SealedKey = "k"; r.Version = 0 },
			"file registered":       func(r *artifacts.Record) { r.SealedKey = "k"; r.State = artifacts.StateRegistered },
			"file without staging":  func(r *artifacts.Record) { r.SealedKey = "k"; r.StagingKey = "" },
			"removed but unfinished": func(r *artifacts.Record) {
				r.State = artifacts.StateReserved
				r.Disposition = artifacts.DispositionRemoved
				r.SealedKey = ""
				r.VerifiedAt = nil
			},
		} {
			rec := usable
			mutate(&rec)
			if _, err := artifacts.Restore(rec); !errors.Is(err, artifacts.ErrInconsistent) {
				t.Errorf("%s: restored (%v)", name, err)
			}
		}
		rec := usable
		rec.SealedKey = "k"
		if _, err := artifacts.Restore(rec); err != nil {
			t.Fatalf("a consistent row restores: %v", err)
		}
	})
}

func TestRegistryRef(t *testing.T) {
	ok := map[string]string{
		"registry.example.test/team/app":       "registry.example.test/team/app",
		"registry.example.test:5000/team/app":  "registry.example.test:5000/team/app",
		"127.0.0.1:5000/t015/app:dev":          "127.0.0.1:5000/t015/app",
		"localhost:5000/a_b/c.d-e":             "localhost:5000/a_b/c.d-e",
		"registry.example.test/team/app:1.2.3": "registry.example.test/team/app",
	}
	for in, want := range ok {
		ref, err := artifacts.ParseRegistryRef(in)
		if err != nil || ref.String() != want {
			t.Errorf("%q: %v %v", in, ref, err)
		}
	}
	for _, in := range []string{
		"", "app", "registry.example.test", "registry.example.test/", "https://registry.example.test/app", "user:pw@registry.example.test/app",
		"registry.example.test/app@sha256:" + strings.Repeat("a", 64), "registry.example.test/App", "registry.example.test/../x", "registry.example.test/a//b",
		"registry.example.test/a/./b", "registry.example.test/a b", "registry.example.test/a?x=1", "registry.example.test:0/a", "registry.example.test:99999/a",
		"Registry.example.test/a", "-bad.example/a", "registry.example.test/a:", "registry.example.test/a:bad tag", "registry.example.test/a/:tag",
		"[::1]:5000/a", strings.Repeat("a", 600) + "/x",
	} {
		if _, err := artifacts.ParseRegistryRef(in); !errors.Is(err, artifacts.ErrInvalid) {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestDigest(t *testing.T) {
	d := artifacts.DigestOf([]byte("abc"))
	if d != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("digest: %s", d)
	}
	if d.Base64() != "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0=" {
		t.Fatalf("base64: %s", d.Base64())
	}
	for _, bad := range []string{"", "md5:abc", "sha256:ABC", "sha256:" + strings.Repeat("a", 63)} {
		if _, err := artifacts.NewDigest(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }
func i64(n int64) *int64         { return &n }
