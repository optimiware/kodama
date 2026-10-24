package deliverytest

import (
	"context"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/oci"
	"github.com/optimiware/kodama/internal/domain"
)

type ociEndpoint = oci.Endpoint

// fixedProvenance resolves the trusted provenance a test configured.
type fixedProvenance struct{ p artifacts.Provenance }

func (f fixedProvenance) Resolve(context.Context, oci.ProvenanceQuery) (artifacts.Provenance, error) {
	return f.p, nil
}

// RegisterImage publishes an image manifest in the registry fixture, verifies it
// with the real OCI verifier (digest-addressed fetch, bounded) and stores the
// registered artifact through the artifact repository for the attempt, as the
// artifact module's reservation does. The verification reports prov as the
// trusted source provenance.
func (e *Env) RegisterImage(a *Attempt, repository string, prov artifacts.Provenance) (domain.ArtifactID, runtimev1.Digest) {
	e.T.Helper()
	ctx := context.Background()
	digest := e.Registry.Add(repository, e.ID("img"))
	ver, err := oci.NewVerifier(oci.Config{Policy: policy{e.Registry}, Provenance: fixedProvenance{prov}})
	must(e.T, err)
	registry := e.Registry.Host() + "/" + repository
	v, err := ver.Verify(ctx, artifacts.OCIRequest{Scope: a.Scope, Attempt: a.ID(), Run: a.runID(), Registry: registry, Digest: digest})
	must(e.T, err)
	id := artifacts.RandomIDs{}.NewArtifactID()
	res := artifacts.Reservation{
		Scope: a.Scope, Attempt: a.ID(), Run: a.runID(), Step: a.Context.StepID, Name: e.ID("image"), Kind: artifacts.KindOCI, Role: artifacts.RoleOutput,
		Digest: digest, Registry: v.Repository, Provenance: v.Provenance,
	}
	now := e.X.Clock.Now().Truncate(time.Microsecond)
	art, err := artifacts.NewRegistered(id, res, v, now)
	must(e.T, err)
	out, err := e.ArtStore.Reserve(ctx, artifacts.ReserveCommand{
		Artifact: art, Limits: artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 100, DiagnosticBytes: 1 << 20, DiagnosticFiles: 100, BundleBytes: 1 << 20, BundleFiles: 10},
		Actor: artifacts.Actor{Type: "workload", ID: string(a.ID())}, RequestID: "req_t035", Now: now,
	})
	must(e.T, err)
	if !out.Created {
		e.T.Fatal("the image registration replayed an existing artifact")
	}
	return id, runtimev1.Digest(digest)
}

func (a *Attempt) runID() domain.RunID { return a.Context.RunID }

// RegisterFile reserves, without completing, an output file artifact of the
// attempt: an incomplete artifact that is not usable and not an image.
func (e *Env) RegisterFile(a *Attempt, name string, prov artifacts.Provenance) domain.ArtifactID {
	e.T.Helper()
	data := []byte("content-" + name)
	size := int64(len(data))
	id := artifacts.RandomIDs{}.NewArtifactID()
	res := artifacts.Reservation{
		Scope: a.Scope, Attempt: a.ID(), Run: a.runID(), Step: a.Context.StepID, Name: name, Kind: artifacts.KindFile, Role: artifacts.RoleOutput,
		Digest: artifacts.DigestOf(data), MediaType: "text/plain", Size: &size, Provenance: prov,
	}
	now := e.X.Clock.Now().Truncate(time.Microsecond)
	art, err := artifacts.NewFile(id, res, artifacts.ObjectKey(a.Scope, id, artifacts.SegmentStaging, e.ID("tok")), now.Add(time.Hour), now)
	must(e.T, err)
	out, err := e.ArtStore.Reserve(context.Background(), artifacts.ReserveCommand{
		Artifact: art, Limits: artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 100, DiagnosticBytes: 1 << 20, DiagnosticFiles: 100, BundleBytes: 1 << 20, BundleFiles: 10},
		Actor: artifacts.Actor{Type: "workload", ID: string(a.ID())}, RequestID: "req_t035", Now: now,
	})
	must(e.T, err)
	if !out.Created {
		e.T.Fatal("the file registration replayed an existing artifact")
	}
	return id
}
