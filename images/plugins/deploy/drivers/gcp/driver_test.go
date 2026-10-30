package gcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/images/plugins/deploy/driver"
)

func TestApplyAndObserveWithFakeGcloud(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gcloud")
	script := `#!/bin/sh
case "$1 $2 $3" in
  "run deploy") echo 'deployed' ;;
  "run services describe") echo '{"status":{"latestReadyRevisionName":"website-00001","traffic":[{"latestRevision":true,"revisionName":"website-00001","percent":100}]}}' ;;
  "run services delete") echo 'deleted' ;;
  "run services update-traffic") echo 'updated' ;;
  *) echo '{}' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Project: "proj", Region: "europe-west1", Service: "website", Image: "registry.example.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	raw, _ := json.Marshal(cfg)
	d := &Driver{Gcloud: bin}
	id, err := d.Identify(context.Background(), driver.IdentityRequest{Configuration: raw})
	if err != nil || id.ResourceKey != "gcp/proj/europe-west1/website" {
		t.Fatalf("identify: %+v %v", id, err)
	}
	res, err := d.Apply(context.Background(), driver.Request{OperationID: "op", Configuration: raw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "confirmed" || res.ProviderVersion != "website-00001" {
		t.Fatalf("unexpected result: %+v", res)
	}
	obs, err := d.Observe(context.Background(), driver.Request{Configuration: raw})
	if err != nil || obs.State != driver.Ready {
		t.Fatalf("observe: %+v %v", obs, err)
	}
	if _, err := d.Destroy(context.Background(), driver.Request{Configuration: raw}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRequiresImage(t *testing.T) {
	cfg := Config{Project: "p", Region: "r", Service: "s"}
	raw, _ := json.Marshal(cfg)
	d := &Driver{Gcloud: "/bin/false"}
	if _, err := d.Apply(context.Background(), driver.Request{Configuration: raw}, nil); err == nil {
		t.Fatal("expected missing image error")
	}
}
