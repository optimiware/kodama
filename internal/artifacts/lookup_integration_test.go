package artifacts_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
)

// TestMetadataAndReferences: scoped, nondisclosing lookup, authorization before
// any download instruction, and the incomplete-versus-usable distinction.
func TestMetadataAndReferences(t *testing.T) {
	data := []byte("evidence")
	env, caller := setup(t, artifactstest.Options{})
	alice := artifacts.Caller{Subject: "user:alice"}
	env.Reader.Allow(alice.Subject, artifactstest.ScopeA)
	env.Reader.Allow("user:bob", artifactstest.ScopeB)
	env.Reader.Allow("user:carol", artifactstest.ScopeA)
	env.Reader.DenyDiagnostics("user:carol")

	other := env.Attempt(artifactstest.ScopeB, "att_b", artifacts.OutputDecl{Name: "report", Kind: artifacts.KindFile})
	foreign := env.ReserveAndUpload(other, "att_b", "report", artifacts.RoleOutput, []byte("foreign"))
	if _, err := env.Complete(other, "att_b", foreign, []byte("foreign")); err != nil {
		t.Fatal(err)
	}

	reserved := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, data)
	diag := env.ReserveAndUpload(caller, attempt, "log", artifacts.RoleDiagnostic, []byte("log text"))
	if _, err := env.Complete(caller, attempt, diag, []byte("log text")); err != nil {
		t.Fatal(err)
	}

	t.Run("an incomplete artifact has metadata but no download", func(t *testing.T) {
		v, err := env.Service.Metadata(env.Ctx(), alice, artifactstest.ScopeA, reserved, false)
		if err != nil || v.Artifact.State() != artifacts.StateReserved || v.Download != nil {
			t.Fatalf("%+v %v", v, err)
		}
		if _, err := env.Service.Metadata(env.Ctx(), alice, artifactstest.ScopeA, reserved, true); !errors.Is(err, artifacts.ErrIncomplete) {
			t.Fatalf("download of unverified bytes: %v", err)
		}
	})

	t.Run("a usable artifact yields a short-lived scoped download", func(t *testing.T) {
		v, err := env.Service.Metadata(env.Ctx(), alice, artifactstest.ScopeA, diag, true)
		if err != nil || v.Download == nil {
			t.Fatalf("%+v %v", v, err)
		}
		if v.Download.Method != "GET" || !v.Download.ExpiresAt.After(env.Clock.Now()) || v.Download.ExpiresAt.After(env.Clock.Now().Add(env.Config.DownloadTTL+2e9)) {
			t.Fatalf("download instruction: %+v", v.Download)
		}
		if code, body := env.Download(*v.Download); code != 200 || string(body) != "log text" {
			t.Fatalf("download: %d %q", code, body)
		}
		if !strings.Contains(v.Download.URL, "/sealed/") || strings.Contains(v.Download.URL, "/staging/") {
			t.Fatal("downloads address only sealed, verified objects")
		}
	})

	t.Run("lookups never disclose foreign artifacts", func(t *testing.T) {
		ctx := env.Ctx()
		for name, tc := range map[string]struct {
			who   artifacts.Caller
			scope domain.Scope
			id    domain.ArtifactID
		}{
			"foreign id in own scope":       {alice, artifactstest.ScopeA, foreign},
			"own id in the foreign scope":   {alice, artifactstest.ScopeB, diag},
			"foreign id in the foreign one": {alice, artifactstest.ScopeB, foreign},
			"other project":                 {alice, artifactstest.ScopeA2, diag},
			"missing id":                    {alice, artifactstest.ScopeA, "art_missing"},
			"member of another tenant":      {artifacts.Caller{Subject: "user:bob"}, artifactstest.ScopeA, diag},
			"stranger":                      {artifacts.Caller{Subject: "user:nobody"}, artifactstest.ScopeA, diag},
		} {
			for _, dl := range []bool{false, true} {
				v, err := env.Service.Metadata(ctx, tc.who, tc.scope, tc.id, dl)
				if !errors.Is(err, artifacts.ErrNotFound) || v.Download != nil {
					t.Errorf("%s (download=%v): %+v %v", name, dl, v, err)
				}
			}
		}
	})

	t.Run("diagnostic visibility is checked before anything is issued", func(t *testing.T) {
		carol := artifacts.Caller{Subject: "user:carol"}
		if _, err := env.Service.Metadata(env.Ctx(), carol, artifactstest.ScopeA, diag, true); !errors.Is(err, artifacts.ErrForbidden) {
			t.Fatalf("diagnostic: %v", err)
		}
		if _, err := env.Service.Metadata(env.Ctx(), carol, artifactstest.ScopeA, reserved, false); err != nil {
			t.Fatalf("an output of the same project is visible: %v", err)
		}
		_, subjects := env.Reader.Calls()
		last := subjects[len(subjects)-1]
		if last.Kind != artifacts.KindFile || last.Role != artifacts.RoleOutput || last.ID != reserved || last.Download {
			t.Fatalf("the authority must be asked about the exact artifact: %+v", last)
		}
	})

	t.Run("revoked access and authority outages deny immediately", func(t *testing.T) {
		env.Reader.Revoke(alice.Subject, artifactstest.ScopeA)
		if _, err := env.Service.Metadata(env.Ctx(), alice, artifactstest.ScopeA, diag, true); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("revoked: %v", err)
		}
		env.Reader.Allow(alice.Subject, artifactstest.ScopeA)
		env.Reader.Fail(errors.New("directory down"))
		if v, err := env.Service.Metadata(env.Ctx(), alice, artifactstest.ScopeA, diag, true); !errors.Is(err, artifacts.ErrUnavailable) || v.Download != nil {
			t.Fatalf("outage: %+v %v", v, err)
		}
		env.Reader.Fail(nil)
	})

	t.Run("reference lookup separates incomplete from usable and hides foreign ids", func(t *testing.T) {
		refs, err := env.Service.References(env.Ctx(), artifactstest.ScopeA, attempt, []domain.ArtifactID{reserved, diag, foreign, "art_missing"})
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 2 || refs[reserved].Usable || refs[reserved].State != artifacts.StateReserved || !refs[diag].Usable || refs[diag].Role != artifacts.RoleDiagnostic {
			t.Fatalf("references: %+v", refs)
		}
		if _, ok := refs[foreign]; ok {
			t.Fatal("a foreign artifact is indistinguishable from a missing one")
		}
		// Another attempt of the same project does not see them either.
		refs, _ = env.Service.References(env.Ctx(), artifactstest.ScopeA, "att_other", []domain.ArtifactID{diag})
		if len(refs) != 0 {
			t.Fatalf("%+v", refs)
		}
		if r, err := env.Service.Reference(env.Ctx(), artifactstest.ScopeA, diag); err != nil || !r.Usable || r.Provenance != artifactstest.Source || r.Digest != artifacts.DigestOf([]byte("log text")) {
			t.Fatalf("scoped reference: %+v %v", r, err)
		}
		if _, err := env.Service.Reference(env.Ctx(), artifactstest.ScopeA, foreign); !errors.Is(err, artifacts.ErrNotFound) {
			t.Fatalf("foreign reference: %v", err)
		}
		if _, err := env.Service.References(env.Ctx(), domain.Scope{}, attempt, nil); !errors.Is(err, artifacts.ErrInvalid) {
			t.Fatalf("invalid scope: %v", err)
		}
	})
}
