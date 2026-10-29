package delivery_test

import (
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
)

const targetProfilesYAML = `apiVersion: kodama/v1alpha1
kind: TargetProfiles
profiles:
  - name: cloudflare-worker
    version: "1"
    provider: cloudflare
    artifactKinds: [bundle]
    credentials:
      - name: CLOUDFLARE_API_TOKEN
        kind: generic
        delivery: file
        required: true
    egress: [cloudflare-api]
    driver:
      mode: image
      image: registry.example.invalid/kodama/deploy-cloudflare@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      commands:
        register: [/opt/deploy/register]
        apply: [/opt/deploy/apply]
        verify: [/opt/deploy/verify]
        observe: [/opt/deploy/observe]
        delete: [/opt/deploy/delete]
    config:
      accountId:
        type: string
        required: true
        pattern: '^[0-9a-f]{32}$'
      workerName:
        type: string
        required: true
        pattern: '^[a-z0-9-]{1,63}$'
      route:
        type: string
        required: true
      preview:
        type: boolean
      replicas:
        type: integer
`

func TestLoadTargetProfilesYAML(t *testing.T) {
	profiles, err := delivery.LoadTargetProfilesYAML(strings.NewReader(targetProfilesYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Name != "cloudflare-worker" || profiles[0].Provider != "cloudflare" {
		t.Fatalf("profiles = %+v", profiles)
	}
	reg, err := delivery.NewTargetRegistry(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get(delivery.KubernetesProfileName); !ok {
		t.Fatal("the builtin Kubernetes profile must always be present")
	}
	p, ok := reg.Get("cloudflare-worker")
	if !ok || len(p.ArtifactKinds) != 1 || p.ArtifactKinds[0] != delivery.ArtifactBundle {
		t.Fatalf("profile = %+v", p)
	}
	if len(p.Driver.Commands["apply"]) != 1 || p.Driver.Protocol != "kodama-deploy/v1" {
		t.Fatalf("driver = %+v", p.Driver)
	}
}

func TestTargetProfileConfigValidation(t *testing.T) {
	profiles, err := delivery.LoadTargetProfilesYAML(strings.NewReader(targetProfilesYAML))
	if err != nil {
		t.Fatal(err)
	}
	p := profiles[0]
	cfg, err := p.ResolveConfig(map[string]string{
		"accountId":  "0123456789abcdef0123456789abcdef",
		"workerName": "website-prod",
		"route":      "optimi.com/*",
		"preview":    "true",
		"replicas":   "03",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["preview"] != "true" || cfg["replicas"] != "3" {
		t.Fatalf("canonical config = %+v", cfg)
	}
	for name, in := range map[string]map[string]string{
		"missing required": {"accountId": "0123456789abcdef0123456789abcdef", "workerName": "website-prod"},
		"unknown key":      {"accountId": "0123456789abcdef0123456789abcdef", "workerName": "website-prod", "route": "/", "extra": "x"},
		"bad pattern":      {"accountId": "not-an-account", "workerName": "website-prod", "route": "/"},
		"bad boolean":      {"accountId": "0123456789abcdef0123456789abcdef", "workerName": "website-prod", "route": "/", "preview": "yes"},
		"bad integer":      {"accountId": "0123456789abcdef0123456789abcdef", "workerName": "website-prod", "route": "/", "replicas": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.ResolveConfig(in); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestTargetProfilesYAMLIsStrict(t *testing.T) {
	for name, body := range map[string]string{
		"wrong apiVersion": strings.Replace(targetProfilesYAML, "kodama/v1alpha1", "kodama/v9", 1),
		"wrong kind":       strings.Replace(targetProfilesYAML, "kind: TargetProfiles", "kind: Profiles", 1),
		"unknown field":    strings.Replace(targetProfilesYAML, "    version: \"1\"", "    version: \"1\"\n    mystery: true", 1),
		"duplicate name":   strings.Replace(targetProfilesYAML, "  - name: cloudflare-worker", "  - name: cloudflare-worker\n  - name: cloudflare-worker\n    version: \"2\"", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := delivery.LoadTargetProfilesYAML(strings.NewReader(body)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestTargetProfileDigestIsStable(t *testing.T) {
	profiles, err := delivery.LoadTargetProfilesYAML(strings.NewReader(targetProfilesYAML))
	if err != nil {
		t.Fatal(err)
	}
	a, err := profiles[0].Digest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := profiles[0].Digest()
	if err != nil || a != b || !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("digest = %q %q %v", a, b, err)
	}
	changed := profiles[0]
	changed.Version = "2"
	c, err := changed.Digest()
	if err != nil || c == a {
		t.Fatalf("a version change must change the digest: %q %q %v", a, c, err)
	}
}

func TestProfileEnvironmentSnapshotUsesV2Identity(t *testing.T) {
	profiles, err := delivery.LoadTargetProfilesYAML(strings.NewReader(targetProfilesYAML))
	if err != nil {
		t.Fatal(err)
	}
	profile := profiles[0]
	digest, err := profile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rec := record()
	rec.TargetProfile, rec.TargetProfileDigest = profile.Name, digest
	rec.TargetProvider, rec.TargetArtifactKinds, rec.TargetEgress = profile.Provider, profile.ArtifactKinds, profile.Egress
	rec.TargetConfig = map[string]string{"accountId": "0123456789abcdef0123456789abcdef", "workerName": "website-prod", "route": "optimi.com/*"}
	rec.Namespace, rec.Kubeconfig = "", delivery.Credential{}
	rec.Secrets = []delivery.Credential{{Name: "CLOUDFLARE_API_TOKEN", Ref: "secret://project/cloudflare-token", Delivery: runtimev1.DeliveryFile}}
	env, err := delivery.NewEnvironment(rec, now)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := delivery.NewSnapshot(env, map[string]int64{"secret://project/cloudflare-token": 4}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.Validate(); err != nil {
		t.Fatal(err)
	}
	if snap.TargetProfile != profile.Name || snap.TargetProvider != "cloudflare" || snap.Kubeconfig != "" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Pin().ConfigDigest == "" || snap.Digest == "" {
		t.Fatal("no digest")
	}
}

func TestProfileEnvironmentNeedsItsCredentials(t *testing.T) {
	profiles, err := delivery.LoadTargetProfilesYAML(strings.NewReader(targetProfilesYAML))
	if err != nil {
		t.Fatal(err)
	}
	profile := profiles[0]
	digest, err := profile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rec := record()
	rec.TargetProfile, rec.TargetProfileDigest = profile.Name, digest
	rec.TargetProvider, rec.TargetArtifactKinds = profile.Provider, profile.ArtifactKinds
	rec.TargetConfig = map[string]string{"accountId": "0123456789abcdef0123456789abcdef", "workerName": "website-prod", "route": "optimi.com/*"}
	rec.Namespace, rec.Kubeconfig = "", delivery.Credential{}
	rec.Secrets = nil
	if _, err := delivery.NewEnvironment(rec, now); err == nil {
		t.Fatal("a profile environment without credentials was accepted")
	}
}
