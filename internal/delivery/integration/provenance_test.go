package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func (s *stack) pins() int {
	return s.count(`SELECT count(*) FROM artifact_pins WHERE tenant_id = $1 AND owner_kind = 'deployment'`, string(scopeA.TenantID))
}

func (s *stack) refuse(t *testing.T, a *deliverytest.Attempt, req delivery.RegistrationRequest, want error) {
	t.Helper()
	_, err := s.register(a, req)
	is(t, err, want)
	// A refused registration leaves no record, event, receipt or retention pin.
	if s.deployments() != 0 || s.events() != 0 || s.pins() != 0 {
		t.Fatalf("a refused registration left state: %d deployments, %d events, %d pins", s.deployments(), s.events(), s.pins())
	}
}

// Test_wrong_image_source proves DQ-01 and DQ-07 with the real record,
// execution and artifact adapters and the real OCI verifier: an image is
// deployable only when it is a usable OCI output of the requested source,
// produced and accepted by a succeeded producer in the same tenant and project.
func Test_wrong_image_source(t *testing.T) {
	t.Run("a wrong repository or commit is rejected", func(t *testing.T) {
		for name, prov := range map[string]artifacts.Provenance{
			"repository": {RepositoryURL: "https://git.example.invalid/elsewhere.git", Commit: executiontestCommit},
			"commit":     {RepositoryURL: "https://git.example.invalid/t020.git", Commit: strings.Repeat("9", 40)},
		} {
			t.Run(name, func(t *testing.T) {
				s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{Provenance: &prov})
				s.refuse(t, s.D.Deploy, s.Req, delivery.ErrProvenance)
			})
		}
	})

	t.Run("a wrong digest is rejected", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		req := s.Req
		req.Image.Digest = runtimev1.Digest("sha256:" + strings.Repeat("a", 64))
		s.refuse(t, s.D.Deploy, req, delivery.ErrProvenance)
	})

	t.Run("a foreign tenant's artifact is rejected", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		s.Kubeconfig(deliverytest.ScopeB, "kubeconfig", "FOREIGN")
		envB := s.Environment(deliverytest.ScopeB, "preview", "kubeconfig")
		foreign := s.NewDeploy(deliverytest.ScopeB, envB, deliverytest.FlowOptions{})
		// The foreign image is pinned as an input of this tenant's attempt, so
		// the eligibility check passes; the artifact owner's scoped lookup must
		// not.
		promo := s.PromoteImage(s.D, s.D.Env, foreign.Image)
		s.refuse(t, promo.Deploy, delivery.RegistrationRequest{Source: s.D.Source, Image: delivery.Image{ArtifactID: foreign.Image, Digest: foreign.ImageDigest}}, delivery.ErrProvenance)
	})

	t.Run("a sibling project's artifact in the same tenant is rejected", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		s.Kubeconfig(deliverytest.ScopeA2, "kubeconfig", "SIBLING")
		envA2 := s.Environment(deliverytest.ScopeA2, "preview", "kubeconfig")
		sibling := s.NewDeploy(deliverytest.ScopeA2, envA2, deliverytest.FlowOptions{})
		// Pinned as an input of this project's attempt, the sibling's image passes
		// the eligibility check; the artifact owner's project-scoped lookup must not.
		promo := s.PromoteImage(s.D, s.D.Env, sibling.Image)
		s.refuse(t, promo.Deploy, delivery.RegistrationRequest{Source: s.D.Source, Image: delivery.Image{ArtifactID: sibling.Image, Digest: sibling.ImageDigest}}, delivery.ErrProvenance)
		// The sibling's own deployment of its own image is unaffected.
		rc, err := s.Svc.RegisterRuntime(context.Background(), sibling.Deploy.ID(), sibling.Deploy.Token,
			delivery.RegistrationRequest{Source: sibling.Source, Image: delivery.Image{ArtifactID: sibling.Image, Digest: sibling.ImageDigest}})
		if err != nil || rc.Deployment.Scope() != deliverytest.ScopeA2 {
			t.Fatalf("the sibling project's own registration: %v", err)
		}
	})

	t.Run("an incomplete or non-image artifact is rejected", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		file := s.RegisterFile(s.D.Build, "report", s.D.Provenance())
		promo := s.PromoteImage(s.D, s.D.Env, file)
		sum := artifacts.DigestOf([]byte("content-report"))
		s.refuse(t, promo.Deploy, delivery.RegistrationRequest{Source: s.D.Source, Image: delivery.Image{ArtifactID: file, Digest: runtimev1.Digest(sum)}}, delivery.ErrProvenance)
	})

	t.Run("an output the producer never accepted is rejected", func(t *testing.T) {
		t.Run("the producer failed", func(t *testing.T) {
			s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{BuildFails: true})
			s.refuse(t, s.D.Deploy, s.Req, delivery.ErrProvenance)
		})
		t.Run("the producer succeeded with a different output", func(t *testing.T) {
			s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{OutputOther: true})
			// D.Image is registered by the build attempt but its accepted result
			// names D.Other.
			s.refuse(t, s.D.Deploy, s.Req, delivery.ErrProvenance)
		})
	})

	t.Run("an image that is not an input of the attempt is rejected", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		other, otherDigest := s.RegisterImage(s.D.Build, "t035/unpinned", s.D.Provenance())
		s.refuse(t, s.D.Deploy, delivery.RegistrationRequest{Source: s.D.Source, Image: delivery.Image{ArtifactID: other, Digest: otherDigest}}, delivery.ErrProvenance)
	})

	t.Run("the same verified digest is promoted with tool-image provenance retained", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		stage := s.Environment(scopeA, "stage-preview", "kubeconfig")
		first := s.mustRegister(t)
		promo := s.Promote(s.D, stage)
		second, err := s.register(promo.Deploy, delivery.RegistrationRequest{Source: promo.Source, Image: delivery.Image{ArtifactID: promo.Image, Digest: promo.ImageDigest}})
		if err != nil {
			t.Fatal(err)
		}
		a, b := first.Deployment.Record(), second.Deployment.Record()
		if a.Image != b.Image || a.Source != b.Source || a.Producer != b.Producer {
			t.Fatalf("promotion changed the verified image or its provenance:\n%+v\n%+v", a, b)
		}
		if a.Environment == b.Environment || a.Attempt == b.Attempt {
			t.Fatal("promotion did not use its own environment and attempt")
		}
		// The tool image is the producing step's immutable command image, never
		// the caller's text.
		if b.Producer.ToolImage != pipelinetest.Image || b.Producer.Attempt != s.D.Build.ID() || b.Producer.Attempt == promo.Deploy.ID() {
			t.Fatalf("producer provenance: %+v", b.Producer)
		}
		if s.pins() != 2 {
			t.Fatalf("each deployment holds its own retention pin on the shared image: %d", s.pins())
		}
	})

	t.Run("a promotion to a stage environment reuses the verified digest and never expires", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		stage := s.Stage(scopeA, "stage", "t035-stage", "kubeconfig")
		if stage.Record().Kind != delivery.KindStage || stage.Record().PreviewTTL != 0 {
			t.Fatalf("not a stage environment: %+v", stage.Record())
		}
		first := s.mustRegister(t) // the preview deployment of the verified image
		promo := s.Promote(s.D, stage)
		second, err := s.register(promo.Deploy, delivery.RegistrationRequest{Source: promo.Source, Image: delivery.Image{ArtifactID: promo.Image, Digest: promo.ImageDigest}})
		if err != nil {
			t.Fatal(err)
		}
		a, b := first.Deployment.Record(), second.Deployment.Record()
		if a.Image != b.Image || a.Source != b.Source || a.Producer != b.Producer {
			t.Fatalf("the promotion changed the verified image or its provenance:\n%+v\n%+v", a, b)
		}
		if b.Environment != stage.ID() || b.Namespace != "t035-stage" || b.ConfigDigest == a.ConfigDigest || a.Namespace == b.Namespace {
			t.Fatalf("the promotion did not pin the stage environment's own configuration: %+v", b)
		}
		if a.ExpiresAt == nil || b.ExpiresAt != nil {
			t.Fatalf("a preview record expires and a stage record does not: preview=%v stage=%v", a.ExpiresAt, b.ExpiresAt)
		}
		// Past every preview TTL the stage record is never enumerated for cleanup.
		s.X.Clock.Advance(90 * 24 * time.Hour)
		expired, err := s.Svc.ExpiredDeployments(context.Background(), scopeA, 10)
		if err != nil || len(expired) != 1 || expired[0].ID() != first.Deployment.ID() {
			t.Fatalf("expired: %v %d", err, len(expired))
		}
		if b.Producer.Attempt != s.D.Build.ID() || b.Producer.ToolImage != pipelinetest.Image {
			t.Fatalf("producer provenance: %+v", b.Producer)
		}
	})

	t.Run("the pure verification rejects every mismatch with one class", func(t *testing.T) {
		scope := scopeA
		src := delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: strings.Repeat("a", 40)}
		img := delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))}
		good := delivery.ImageEvidence{ID: "art_1", Scope: scope, Attempt: "att_1", Kind: runtimev1.KindOCI, Role: runtimev1.RoleOutput, Usable: true, Digest: img.Digest, Provenance: src}
		out := delivery.ProducedOutput{Scope: scope, Attempt: "att_1", Run: "run_1", Step: "build", ToolImage: pipelinetest.Image, Accepted: true}
		if _, err := delivery.VerifyImage(scope, src, img, good, out); err != nil {
			t.Fatal(err)
		}
		mutations := map[string]func(*delivery.ImageEvidence, *delivery.ProducedOutput){
			"artifact id": func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) { e.ID = "art_2" },
			"scope":       func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) { e.Scope = deliverytest.ScopeB },
			"kind":        func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) { e.Kind = runtimev1.KindFile },
			"role":        func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) { e.Role = runtimev1.RoleDiagnostic },
			"incomplete":  func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) { e.Usable = false },
			"digest": func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) {
				e.Digest = runtimev1.Digest("sha256:" + strings.Repeat("c", 64))
			},
			"repository": func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) {
				e.Provenance.RepositoryURL = "https://x.invalid/y.git"
			},
			"commit": func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) {
				e.Provenance.Commit = strings.Repeat("d", 40)
			},
			"no provenance":  func(e *delivery.ImageEvidence, _ *delivery.ProducedOutput) { e.Provenance = delivery.Source{} },
			"producer":       func(_ *delivery.ImageEvidence, o *delivery.ProducedOutput) { o.Attempt = "att_other" },
			"producer scope": func(_ *delivery.ImageEvidence, o *delivery.ProducedOutput) { o.Scope = deliverytest.ScopeB },
			"unaccepted":     func(_ *delivery.ImageEvidence, o *delivery.ProducedOutput) { o.Accepted = false },
			"no tool image":  func(_ *delivery.ImageEvidence, o *delivery.ProducedOutput) { o.ToolImage = "" },
			"mutable tool": func(_ *delivery.ImageEvidence, o *delivery.ProducedOutput) {
				o.ToolImage = "registry.example.invalid/tools:latest"
			},
		}
		for name, mutate := range mutations {
			t.Run(name, func(t *testing.T) {
				ev, o := good, out
				mutate(&ev, &o)
				_, err := delivery.VerifyImage(scope, src, img, ev, o)
				is(t, err, delivery.ErrProvenance)
			})
		}
	})
}

const executiontestCommit = "1111111111111111111111111111111111111111"
