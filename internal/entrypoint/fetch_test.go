package entrypoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

type fakeInputFetcher struct {
	allowed artifactsv1.InputFetchRequest
	payload []byte
	corrupt bool
}

func (f *fakeInputFetcher) FetchInput(_ context.Context, req artifactsv1.InputFetchRequest) (artifactsv1.InputFetchResponse, error) {
	if req.Input != f.allowed.Input || req.ArtifactID != f.allowed.ArtifactID || req.Digest != f.allowed.Digest {
		return artifactsv1.InputFetchResponse{}, errors.New("forbidden: input not admitted")
	}
	sz := int64(len(f.payload))
	mt := "application/x-tar"
	return artifactsv1.InputFetchResponse{
		Input:      req.Input,
		ArtifactID: req.ArtifactID,
		Kind:       runtimev1.KindFile,
		Digest:     req.Digest,
		MediaType:  &mt,
		SizeBytes:  &sz,
		Download: &runtimev1.Upload{
			Method:    "GET",
			URL:       "https://objects.example.invalid/sealed/art_bundle",
			Headers:   map[string]string{},
			ExpiresAt: "2026-10-30T12:05:00Z",
		},
	}, nil
}

func (f *fakeInputFetcher) DownloadInput(_ context.Context, _ runtimev1.Upload) (io.ReadCloser, error) {
	data := append([]byte(nil), f.payload...)
	if f.corrupt && len(data) > 0 {
		data[0] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func Test_unit_admitted_artifact_fetch(t *testing.T) {
	root := t.TempDir()
	payload := []byte("verified-build-archive-tar-bytes")
	sum := sha256.Sum256(payload)
	dg := runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:]))
	req := artifactsv1.InputFetchRequest{
		Input:      "bundle",
		ArtifactID: "art_bundle",
		Digest:     dg,
	}
	fetcher := &fakeInputFetcher{allowed: req, payload: payload}

	// 1. Valid admitted input downloads, verifies SHA-256 and size, and places atomically.
	if _, err := FetchArtifactInput(t.Context(), fetcher, root, "inputs/build.tar", req, false); err != nil {
		t.Fatalf("FetchArtifactInput valid: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "inputs/build.tar"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("downloaded file mismatch: %v", err)
	}

	// 2. Accidental overwrite is refused when overwrite=false.
	if _, err := FetchArtifactInput(t.Context(), fetcher, root, "inputs/build.tar", req, false); !errors.Is(err, ErrInputExists) {
		t.Fatalf("expected ErrInputExists when overwrite=false, got %v", err)
	}

	// 3. Path traversal and forbidden system roots are refused before any fetch.
	for _, bad := range []string{"../escape.tar", "/etc/passwd", "."} {
		if _, err := FetchArtifactInput(t.Context(), fetcher, root, bad, req, true); !errors.Is(err, ErrInputPathUnsafe) {
			t.Fatalf("expected ErrInputPathUnsafe for %q, got %v", bad, err)
		}
	}
	if _, err := FetchArtifactInput(t.Context(), fetcher, "/kodama/secrets", "tok", req, true); !errors.Is(err, ErrInputPathUnsafe) {
		t.Fatalf("expected ErrInputPathUnsafe for /kodama/secrets, got %v", err)
	}

	// 4. Corrupted bytes fail digest verification and leave no partial destination file.
	fetcher.corrupt = true
	if _, err := FetchArtifactInput(t.Context(), fetcher, root, "inputs/corrupt.tar", req, true); !errors.Is(err, ErrInputDigest) {
		t.Fatalf("expected ErrInputDigest on corrupted stream, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "inputs/corrupt.tar")); !os.IsNotExist(err) {
		t.Fatalf("corrupted download must not be placed at destination: %v", err)
	}

	// 5. Foreign / unadmitted artifact ID is denied.
	fetcher.corrupt = false
	badReq := req
	badReq.ArtifactID = "art_foreign"
	if _, err := FetchArtifactInput(t.Context(), fetcher, root, "inputs/foreign.tar", badReq, true); err == nil {
		t.Fatal("expected foreign artifact ID fetch to fail")
	}
}

func Test_unit_direct_harness_container(t *testing.T) {
	// 1. A direct harness container step (e.g. Claude Code / OpenCode with an
	// explicit secretRef in env) validates as an ordinary container step without
	// any managed agent profile, model gateway, or MCP proxy configured.
	rawYAML := []byte(`apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: direct-claude-step
spec:
  timeoutSeconds: 1800
  steps:
    - id: implement
      kind: container
      image: registry.example.invalid/claude-code@sha256:` + strings.Repeat("3", 64) + `
      workspace: {access: write}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && claude --print 'implement feature' > /kodama/outputs/summary.json"]
      env:
        ANTHROPIC_API_KEY: {secretRef: "secret://project/anthropic-api-key"}
      timeoutSeconds: 1200
      outputs:
        summary: {type: artifact, path: /kodama/outputs/summary.json}
`)
	val, err := pipeline.NewValidator(pipeline.Config{
		Capabilities:   pipeline.EnabledKinds([]pipeline.StepKind{pipeline.KindContainer}),
		EnableV1Alpha2: true,
		Profiles:       nil, // no managed agent profile lookup configured
		References: pipelinetest.References{
			Secrets: map[string]bool{"anthropic-api-key": true},
		},
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	validated, err := val.ValidateYAML(t.Context(), scope, pipeline.Actor{Type: "user", ID: "usr_eng"}, rawYAML)
	if err != nil || validated.Digest() == "" {
		t.Fatalf("direct harness container pipeline failed validation: %v", err)
	}

	// 2. An optional managed agent profile expands onto the exact same container
	// execution primitive while recording its mediated model/tool claims.
	maxReq := int64(25)
	maxTok := int64(100000)
	content, err := agents.NewContent(agents.Draft{
		Name:         "coding-v1",
		Image:        "registry.example.invalid/opencode-wrapper@sha256:" + strings.Repeat("4", 64),
		Wrapper:      agents.Wrapper{ID: "opencode", Version: "1.0.0", Command: []string{"/kodama/bin/opencode-wrapper"}, Harness: agents.Harness{ID: "opencode", Version: "0.1.0"}},
		Instructions: "Follow the specification.",
		DesignBaseline: runtimev1.DesignBaseline{
			Sources:        []runtimev1.SourceRef{{Repository: "https://git.example.invalid/acme/inventory.git", Revision: runtimev1.GitCommit(strings.Repeat("1", 40)), Path: "docs/README.md"}},
			Requirements:   []string{"AR-01"},
			Decisions:      []string{},
			RequiredChecks: []string{},
		},
		Model:  agents.Model{Name: "gpt-5", Endpoint: "https://models.example.invalid/v1"},
		Tools:  []string{"mcp/search"},
		Limits: agents.Limits{WallSeconds: 900, MaxRequests: &maxReq, MaxTokens: &maxTok},
	})
	if err != nil {
		t.Fatalf("agents.NewContent: %v", err)
	}
	pv, err := agents.NewProfileVersion(agents.Candidate{
		Scope:     scope,
		ID:        "ap_01",
		Content:   content,
		CreatedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		CreatedBy: agents.Actor{Type: "user", ID: "usr_eng"},
	}, 1)
	if err != nil {
		t.Fatalf("NewProfileVersion: %v", err)
	}
	exp, err := agents.Expand(pv)
	if err != nil {
		t.Fatalf("agents.Expand: %v", err)
	}
	if exp.Image == "" || len(exp.Command) == 0 || exp.MaxRequests != 25 || exp.MaxTokens != 100000 {
		t.Fatalf("unexpected profile expansion: %+v", exp)
	}
}
