package adapters_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/logs/logstest"
)

func bg() context.Context { return context.Background() }

func ref(id domain.ArtifactID, text string) logs.ArtifactRef {
	return logs.ArtifactRef{ID: id, Digest: string(artifacts.DigestOf([]byte(text))), Size: int64(len(text))}
}

func sidecar(t *testing.T, doc contract.Metadata) []byte {
	t.Helper()
	b, err := contract.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func metaDoc(id domain.ArtifactID, text string, c contract.Completeness, reasons ...contract.Reason) contract.Metadata {
	return contract.Metadata{Version: contract.Version, Source: contract.SourceWrapper, LogName: contract.CommandLogName,
		Log:          contract.Ref{ArtifactID: id, Digest: string(artifacts.DigestOf([]byte(text))), SizeBytes: int64(len(text))},
		Completeness: c, Reasons: reasons}
}

// TestArtifactMetadataBinding: reserved, removed and foreign artifacts are
// unusable; a sidecar that does not describe exactly this artifact is not
// trusted; legacy text is unknown-completeness; verified bytes are readable.
// Real PostgreSQL and S3; no NATS, Git or Kubernetes.
func TestArtifactMetadataBinding(t *testing.T) {
	e := logstest.New(t)
	metaName, _ := contract.MetadataName(contract.CommandLogName)

	t.Run("verified wrapper log with a bound sidecar", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		id := e.WrapperLog(a, "line 1\nline 2\n", contract.Complete)
		link, err := e.Reader.WrapperLog(bg(), a.Scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if link.Log != ref(id, "line 1\nline 2\n") || link.Completeness != logs.Complete || len(link.Reasons) != 0 || link.Meta == nil {
			t.Fatalf("link = %+v", link)
		}
		rc, err := e.Reader.Open(bg(), a.Scope, a.ID, link.Log)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil || string(b) != "line 1\nline 2\n" {
			t.Fatalf("bytes = %q, %v", b, err)
		}
	})

	t.Run("legacy plain text has unknown completeness", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		id := e.LegacyWrapperLog(a, "legacy\n")
		link, err := e.Reader.WrapperLog(bg(), a.Scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if link.Log.ID != id || link.Completeness != logs.Unknown || link.Meta != nil || len(link.Reasons) != 1 || link.Reasons[0] != contract.ReasonUnverified {
			t.Fatalf("a log with no sidecar must be unknown, never complete: %+v", link)
		}
	})

	t.Run("partial and failed uploads keep their classified reasons", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		e.WrapperLog(a, "cut\n", contract.Partial, contract.ReasonTruncated, contract.ReasonUploadFailed)
		link, err := e.Reader.WrapperLog(bg(), a.Scope, a.ID)
		if err != nil || link.Completeness != logs.Partial || len(link.Reasons) != 2 {
			t.Fatalf("%+v %v", link, err)
		}
	})

	t.Run("a sidecar that does not describe this artifact is not trusted", func(t *testing.T) {
		cases := map[string]func(a logstest.Attempt, id domain.ArtifactID, text string) []byte{
			"copied from another artifact": func(a logstest.Attempt, id domain.ArtifactID, text string) []byte {
				return sidecar(t, metaDoc("art_someone_else", text, contract.Complete))
			},
			"describes other bytes": func(a logstest.Attempt, id domain.ArtifactID, text string) []byte {
				return sidecar(t, metaDoc(id, "different content", contract.Complete))
			},
			"claims the controller as its source": func(a logstest.Attempt, id domain.ArtifactID, text string) []byte {
				d := metaDoc(id, text, contract.Unknown, contract.ReasonControllerCapture)
				d.Source = contract.SourceController
				return sidecar(t, d)
			},
			"names another log": func(a logstest.Attempt, id domain.ArtifactID, text string) []byte {
				d := metaDoc(id, text, contract.Complete)
				d.LogName = contract.EntrypointLogName
				return sidecar(t, d)
			},
			"is not a document": func(a logstest.Attempt, id domain.ArtifactID, text string) []byte {
				return []byte(`{"completeness":"complete"}`)
			},
			"has unknown fields": func(a logstest.Attempt, id domain.ArtifactID, text string) []byte {
				return []byte(`{"version":1,"source":"wrapper","logName":"command.log","log":{"artifactId":"` + string(id) + `","digest":"` + string(artifacts.DigestOf([]byte(text))) + `","sizeBytes":6},"completeness":"complete","reasons":[],"success":true}`)
			},
		}
		for name, build := range cases {
			t.Run(name, func(t *testing.T) {
				a := e.NewAttempt(logstest.ScopeA, true)
				text := "abcde\n"
				id := e.WrapperUpload(a, contract.CommandLogName, contract.LogMediaType, []byte(text))
				e.WrapperUpload(a, metaName, contract.MediaType, build(a, id, text))
				link, err := e.Reader.WrapperLog(bg(), a.Scope, a.ID)
				if err != nil {
					t.Fatal(err)
				}
				if link.Completeness != logs.Unknown || link.Meta != nil {
					t.Fatalf("an unbound sidecar must not upgrade completeness: %+v", link)
				}
			})
		}
		t.Run("wrong media type", func(t *testing.T) {
			a := e.NewAttempt(logstest.ScopeA, true)
			text := "abcde\n"
			id := e.WrapperUpload(a, contract.CommandLogName, contract.LogMediaType, []byte(text))
			e.WrapperUpload(a, metaName, "text/plain", sidecar(t, metaDoc(id, text, contract.Complete)))
			link, err := e.Reader.WrapperLog(bg(), a.Scope, a.ID)
			if err != nil || link.Completeness != logs.Unknown {
				t.Fatalf("%+v %v", link, err)
			}
		})
	})

	t.Run("reserved, removed and missing content are unusable", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		text := "x\n"
		caller := artifactstest.Caller(a.ID)
		e.ArtAuth.Set(caller.Subject, artifacts.AttemptGrant{Scope: a.Scope, Attempt: a.ID, Run: a.Run, Step: "build", ExpiresAt: e.Clock.Now().Add(1e9 * 3600),
			Limits: artifacts.Limits{DiagnosticBytes: 1 << 20}})
		mt, size := contract.LogMediaType, int64(len(text))
		out, err := e.ArtSvc.Reserve(bg(), caller, a.ID, artifacts.ReserveInput{Name: contract.CommandLogName, Kind: artifacts.KindFile, Role: artifacts.RoleDiagnostic,
			Digest: artifacts.DigestOf([]byte(text)), MediaType: &mt, Size: &size}, "req")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Reader.WrapperLog(bg(), a.Scope, a.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a reserved, unfinished upload has no log: %v", err)
		}
		if _, err := e.Reader.Open(bg(), a.Scope, a.ID, ref(out.ID, text)); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a reserved artifact is never readable: %v", err)
		}

		b := e.NewAttempt(logstest.ScopeA, true)
		id := e.LegacyWrapperLog(b, text)
		r := ref(id, text)
		if ok, err := e.Reader.Usable(bg(), b.Scope, b.ID, r); err != nil || !ok {
			t.Fatalf("usable: %v %v", ok, err)
		}
		// Retention removes the content: metadata stays, bytes are unavailable.
		e.OwnerExec(`UPDATE artifacts SET disposition = 'removed', version = version + 1 WHERE id = $1`, string(id))
		if ok, err := e.Reader.Usable(bg(), b.Scope, b.ID, r); err != nil || ok {
			t.Fatalf("a removed artifact is not usable: %v %v", ok, err)
		}
		if _, err := e.Reader.Open(bg(), b.Scope, b.ID, r); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a removed artifact is not readable: %v", err)
		}
		if _, err := e.Reader.WrapperLog(bg(), b.Scope, b.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a removed wrapper log is no log: %v", err)
		}

		// The sealed object vanished from the store though the metadata says usable.
		c := e.NewAttempt(logstest.ScopeA, true)
		cid := e.LegacyWrapperLog(c, text)
		art, _ := e.ArtStore.Get(bg(), c.Scope, cid)
		if err := e.Objects.Delete(bg(), art.SealedKey()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Reader.Open(bg(), c.Scope, c.ID, ref(cid, text)); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a missing object is no log, not an error body: %v", err)
		}
	})

	t.Run("foreign references are not found", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		id := e.LegacyWrapperLog(a, "mine\n")
		r := ref(id, "mine\n")
		other := e.NewAttempt(logstest.ScopeA, true)
		b := e.NewAttempt(logstest.ScopeB, true)
		for name, c := range map[string]struct {
			scope   domain.Scope
			attempt domain.AttemptID
		}{
			"another attempt of the same project": {a.Scope, other.ID},
			"another project":                     {logstest.ScopeA2, a.ID},
			"another tenant":                      {b.Scope, a.ID},
		} {
			if _, err := e.Reader.Open(bg(), c.scope, c.attempt, r); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: Open = %v", name, err)
			}
			if ok, err := e.Reader.Usable(bg(), c.scope, c.attempt, r); ok || !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: Usable = %v %v", name, ok, err)
			}
		}
		if _, err := e.Reader.WrapperLog(bg(), other.Scope, other.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("an attempt never sees another attempt's log: %v", err)
		}
	})

	t.Run("the record must name the artifact's real content", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		id := e.LegacyWrapperLog(a, "content\n")
		bad := ref(id, "content\n")
		bad.Size++
		if _, err := e.Reader.Open(bg(), a.Scope, a.ID, bad); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("size: %v", err)
		}
		bad = ref(id, "content\n")
		bad.Digest = string(artifacts.DigestOf([]byte("other")))
		if _, err := e.Reader.Open(bg(), a.Scope, a.ID, bad); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("digest: %v", err)
		}
	})

	t.Run("an output artifact is not a diagnostic log", func(t *testing.T) {
		a := e.NewAttempt(logstest.ScopeA, true)
		caller := artifactstest.Caller(a.ID)
		e.ArtAuth.Set(caller.Subject, artifacts.AttemptGrant{Scope: a.Scope, Attempt: a.ID, Run: a.Run, Step: "build", ExpiresAt: e.Clock.Now().Add(1e9 * 3600),
			Outputs: []artifacts.OutputDecl{{Name: contract.CommandLogName, Kind: artifacts.KindFile}}, Limits: artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 2}})
		text := "output\n"
		mt, size := contract.LogMediaType, int64(len(text))
		out, err := e.ArtSvc.Reserve(bg(), caller, a.ID, artifacts.ReserveInput{Name: contract.CommandLogName, Kind: artifacts.KindFile, Role: artifacts.RoleOutput,
			Digest: artifacts.DigestOf([]byte(text)), MediaType: &mt, Size: &size}, "req")
		if err != nil {
			t.Fatal(err)
		}
		_ = out
		if _, err := e.Reader.WrapperLog(bg(), a.Scope, a.ID); err == nil {
			t.Fatal("an output named command.log must never be served as a log")
		}
	})
}
