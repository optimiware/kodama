package delivery_test

import (
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
)

func bundleRegistration() delivery.Registration {
	digest := strings.Repeat("d", 64)
	return delivery.Registration{
		Scope: scope, Environment: "env_cf", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("e", 64)), Source: registration().Source,
		Artifact: delivery.DeployableArtifact{Kind: delivery.ArtifactBundle, ArtifactID: "art_bundle", Digest: runtimev1.Digest("sha256:" + digest),
			MediaType: "application/vnd.kodama.bundle.v1+tar+zstd"},
		Producer: registration().Producer,
		Run:      "run_1", Step: "deploy", ActionKey: "act:bundle", Attempt: "att_1",
	}
}

func TestBundleDeploymentLifecycle(t *testing.T) {
	d, err := delivery.NewDeployment(bundleRegistration(), now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Record().Artifact.Kind != delivery.ArtifactBundle || d.Record().Image != (delivery.Image{}) {
		t.Fatalf("bundle record = %+v", d.Record())
	}
	worker := delivery.ResourceRef{Provider: "cloudflare", Type: "worker-version", ID: "v1", Name: "website-prod"}
	if _, err := d.BeginApplyResources([]delivery.ResourceRef{worker}, now); err != nil {
		t.Fatal(err)
	}
	worker.Ready = true
	worker.Version = "v1"
	ready := worker
	if _, err := d.Settle(delivery.Settlement{Succeeded: true, Resources: []delivery.ResourceRef{ready}}, now); err != nil {
		t.Fatal(err)
	}
	if d.Status() != delivery.StatusSucceeded || len(d.Record().Resources) != 1 || !d.Record().Resources[0].Ready {
		t.Fatalf("settled = %+v", d.Record())
	}
	// A replay of the same settlement is a no-op; a different one conflicts.
	if _, err := d.Settle(delivery.Settlement{Succeeded: true, Resources: []delivery.ResourceRef{ready}}, now); err != nil {
		t.Fatal(err)
	}
	other := ready
	other.Version = "v2"
	if _, err := d.Settle(delivery.Settlement{Succeeded: true, Resources: []delivery.ResourceRef{other}}, now); err == nil {
		t.Fatal("a different settlement rewrote history")
	}
}

func TestBundleDeploymentRejectsMixedShape(t *testing.T) {
	reg := bundleRegistration()
	reg.Image = delivery.Image{ArtifactID: "art_other", Digest: runtimev1.Digest("sha256:" + strings.Repeat("f", 64))}
	if _, err := delivery.NewDeployment(reg, now); err == nil {
		t.Fatal("a mismatched legacy image projection was accepted")
	}
	reg = bundleRegistration()
	reg.Artifact.MediaType = ""
	if _, err := delivery.NewDeployment(reg, now); err == nil {
		t.Fatal("a bundle without a media type was accepted")
	}
}

func TestResourceValidation(t *testing.T) {
	good := delivery.ResourceRef{Provider: "cloudflare", Type: "worker-route", ID: "route_1", Metadata: map[string]string{"hostname": "optimi.com"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*delivery.ResourceRef){
		"provider": func(r *delivery.ResourceRef) { r.Provider = "Cloudflare" },
		"type":     func(r *delivery.ResourceRef) { r.Type = "!bad" },
		"id":       func(r *delivery.ResourceRef) { r.ID = "" },
		"metadata": func(r *delivery.ResourceRef) { r.Metadata = map[string]string{"bad key": "x"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := good
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
