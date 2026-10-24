package delivery_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.invalid/inventory/internal/delivery"
)

func registryServer(t *testing.T, repoPath, digest string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/v2/"+repoPath+"/manifests/"+digest {
			w.Header().Set("Docker-Content-Digest", digest)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyBuildRecordsEvidenceAndOCIOutput(t *testing.T) {
	srv := registryServer(t, "acme/inventory", digest)
	host := strings.TrimPrefix(srv.URL, "http://")
	reg := &delivery.Registry{Insecure: true}
	out, results := t.TempDir(), filepath.Join(t.TempDir(), "results.json")
	in := delivery.BuildInput{Ref: host + "/acme/inventory@" + digest, SourceCommit: commit, ToolImage: "reg.example.invalid/acme/tools@sha256:" + strings.Repeat("c", 64), GoVersion: "1.27.2",
		Platforms: []string{"linux/arm64", "linux/amd64"}, ContextCommit: commit, ContextToolImage: "reg.example.invalid/acme/tools@sha256:" + strings.Repeat("c", 64), OutputsDir: out, ResultsFile: results, Images: reg}
	ev, err := delivery.VerifyBuild(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Image.Digest != digest || ev.SourceCommit != commit || ev.ToolImage != in.ToolImage || strings.Join(ev.Platforms, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("%+v", ev)
	}
	var manifest map[string]any
	raw, _ := os.ReadFile(results)
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest["outcome"] != "completed" {
		t.Fatalf("%s %v", raw, err)
	}
	image := manifest["outputs"].(map[string]any)["image"].(map[string]any)
	if image["digest"] != digest || image["registryRef"] != host+"/acme/inventory" {
		t.Fatalf("the OCI output records digest and repository: %v", image)
	}
	if _, err := os.Stat(filepath.Join(out, "build-metadata.json")); err != nil {
		t.Fatal("the build metadata artifact is written under the output root:", err)
	}
	// ko prints repository:tag@digest; the digest is the identity.
	tagged := in
	tagged.Ref = host + "/acme/inventory:abc123@" + digest
	if ev2, err := delivery.VerifyBuild(context.Background(), tagged); err != nil || ev2.Image.Repository != host+"/acme/inventory" {
		t.Fatalf("a tagged reference: %v %+v", err, ev2)
	}
	// Failures: wrong commit, missing digest, mutable reference, not a commit.
	bad := map[string]delivery.BuildInput{}
	c := in
	c.ContextCommit = strings.Repeat("2", 40)
	bad["not the pinned commit"] = c
	c = in
	c.Ref = host + "/acme/inventory@sha256:" + strings.Repeat("9", 64)
	bad["digest not in the registry"] = c
	c = in
	c.Ref = host + "/acme/inventory:latest"
	bad["mutable reference"] = c
	c = in
	c.SourceCommit = "HEAD"
	bad["not a commit id"] = c
	for name, b := range bad {
		if _, err := delivery.VerifyBuild(context.Background(), b); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestResultsManifestShape(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.json")
	if err := delivery.WriteResults(p, "completed", "", map[string]any{"deploymentId": "dep_x", "appUrl": "http://x"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != `{"outcome":"completed","outputs":{"appUrl":"http://x","deploymentId":"dep_x"}}` {
		t.Fatalf("%s", raw)
	}
	if err := delivery.WriteResults(p, "failed", "Bad Reason!", nil); err == nil {
		t.Fatal("failure reasons are entrypoint reason codes")
	}
	if err := delivery.WriteResults(p, "failed", "rollout_timeout", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(p)
	if string(raw) != `{"outcome":"failed","reason":"rollout_timeout"}` {
		t.Fatalf("a failure carries no outputs: %s", raw)
	}
	if err := delivery.WriteArtifact(t.TempDir(), "../escape.json", 1); err == nil {
		t.Fatal("artifacts cannot escape the output root")
	}
}

// TestBuildEvidenceIdentity: a build that cannot name the digest-pinned tools
// image that executed it, or that disagrees with the pinned command image, source
// or platform record, produces no completed evidence.
func TestBuildEvidenceIdentity(t *testing.T) {
	srv := registryServer(t, "acme/inventory", digest)
	host := strings.TrimPrefix(srv.URL, "http://")
	tools := "reg.example.invalid/acme/tools@sha256:" + strings.Repeat("c", 64)
	base := func() delivery.BuildInput {
		return delivery.BuildInput{Ref: host + "/acme/inventory@" + digest, SourceCommit: commit, ToolImage: tools, GoVersion: "1.27.2",
			Platforms: []string{"linux/amd64"}, ContextCommit: commit, ContextToolImage: tools, OutputsDir: t.TempDir(), ResultsFile: filepath.Join(t.TempDir(), "results.json"),
			Images: &delivery.Registry{Insecure: true}}
	}
	if _, err := delivery.VerifyBuild(context.Background(), base()); err != nil {
		t.Fatalf("the baseline must verify: %v", err)
	}
	cases := map[string]func(*delivery.BuildInput){
		"absent tool image":   func(b *delivery.BuildInput) { b.ToolImage = "" },
		"mutable tool tag":    func(b *delivery.BuildInput) { b.ToolImage = "reg.example.invalid/acme/tools:latest" },
		"tag only with colon": func(b *delivery.BuildInput) { b.ToolImage = "reg.example.invalid/acme/tools:1.2" },
		"malformed digest":    func(b *delivery.BuildInput) { b.ToolImage = "reg.example.invalid/acme/tools@sha256:abc" },
		"another tools image": func(b *delivery.BuildInput) {
			b.ContextToolImage = "reg.example.invalid/acme/tools@sha256:" + strings.Repeat("d", 64)
		},
		"another tools repository": func(b *delivery.BuildInput) {
			b.ContextToolImage = "reg.example.invalid/other/tools@sha256:" + strings.Repeat("c", 64)
		},
		"another source commit": func(b *delivery.BuildInput) { b.ContextCommit = strings.Repeat("2", 40) },
		"no platform record":    func(b *delivery.BuildInput) { b.Platforms = nil },
		"unsupported platform":  func(b *delivery.BuildInput) { b.Platforms = []string{"windows/amd64"} },
		"duplicate platform":    func(b *delivery.BuildInput) { b.Platforms = []string{"linux/amd64", "linux/amd64"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := base()
			mutate(&in)
			if _, err := delivery.VerifyBuild(context.Background(), in); err == nil {
				t.Fatal("completed evidence must not be produced")
			}
			if _, err := os.Stat(in.ResultsFile); err == nil {
				t.Fatal("no completed results manifest may be written")
			}
			if _, err := os.Stat(filepath.Join(in.OutputsDir, "build-metadata.json")); err == nil {
				t.Fatal("no build metadata may be written")
			}
		})
	}
}

func TestParsePlatforms(t *testing.T) {
	got, err := delivery.ParsePlatforms("linux/arm64, linux/amd64")
	if err != nil || strings.Join(got, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "linux/amd64,", "linux/s390x", "linux/amd64,linux/amd64"} {
		if _, err := delivery.ParsePlatforms(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
