package cloudflare

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/images/plugins/deploy/driver"
)

func TestApplyObservesProviderVersionWithFakeWrangler(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "wrangler")
	script := `#!/bin/sh
case "$1 $2" in
  "deploy --name") echo '{"ok":true}' ;;
  "deployments list") echo '[{"id":"dep-1","versions":[{"version_id":"ver-1"}]}]' ;;
  "delete --name") echo 'deleted' ;;
  *) echo '{}' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	bundlePath := writeBundle(t, dir)
	cfg := Config{AccountID: "acct", Worker: "worker", BundlePath: bundlePath}
	raw, _ := json.Marshal(cfg)
	d := &Driver{Wrangler: bin}
	id, err := d.Identify(context.Background(), driver.IdentityRequest{Configuration: raw})
	if err != nil || id.ResourceKey != "cloudflare/acct/worker" {
		t.Fatalf("identify: %+v %v", id, err)
	}
	res, err := d.Apply(context.Background(), driver.Request{OperationID: "op", Configuration: raw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "confirmed" || res.ProviderVersion != "ver-1" {
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

func TestRejectsMalformedDeploymentList(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "wrangler")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'not-json'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundlePath := writeBundle(t, dir)
	raw, _ := json.Marshal(Config{AccountID: "acct", Worker: "worker", BundlePath: bundlePath})
	d := &Driver{Wrangler: bin}
	obs, err := d.Observe(context.Background(), driver.Request{Configuration: raw})
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != driver.Unknown {
		t.Fatalf("malformed output must be unknown, got %+v", obs)
	}
}

func writeBundle(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "worker.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	data := []byte(`{"name":"worker","main":"index.js"}`)
	if err := tw.WriteHeader(&tar.Header{Name: "wrangler.jsonc", Mode: 0o600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}
