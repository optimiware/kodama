package entrypoint

// Test doubles for the consumer-owned ports: a scriptable Runtime and an
// Uploader, plus a builder for a fully valid attempt Context and Step.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

const testImage = "registry.example.invalid/step@sha256:1111111111111111111111111111111111111111111111111111111111111111"

// scriptRT is a Runtime whose every operation can be replaced. Defaults behave
// like a cooperative control plane; all calls are recorded.
type scriptRT struct {
	mu sync.Mutex

	ctx   runtimev1.Context
	admit func(runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error)
	fetch func() (runtimev1.Context, error)
	renew func() (runtimev1.TokenResponse, error)

	resolve  func(runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error)
	reserve  func(runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error)
	complete func(domain.ArtifactID, runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error)
	propose  func(n int, req runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error)
	submit   func(n int, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error)

	calls     []string
	reserved  []runtimev1.ArtifactReserveRequest
	proposals []runtimev1.SourceProposalRequest
	submitted []runtimev1.ResultRequest
	nextArt   int
}

func (s *scriptRT) rec(op string) { s.calls = append(s.calls, op) }

func (s *scriptRT) count(op string) (n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == op {
			n++
		}
	}
	return
}

func timestamp(d time.Duration) runtimev1.Timestamp {
	return runtimev1.Timestamp(time.Now().Add(d).UTC().Format(time.RFC3339Nano))
}

func tokenInfo(token string, ttl, max time.Duration) runtimev1.TokenInfo {
	return runtimev1.TokenInfo{Token: token, TokenType: "Bearer", Audience: runtimev1.Audience, ExpiresAt: timestamp(ttl), MaxExpiresAt: timestamp(max)}
}

func (s *scriptRT) Admit(_ context.Context, req runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("admit")
	if s.admit != nil {
		return s.admit(req)
	}
	return runtimev1.AdmitResponse{Token: tokenInfo("rt_unit_token_0123456789abcdef", time.Hour, 2*time.Hour), Context: s.ctx}, nil
}

func (s *scriptRT) RenewToken(context.Context) (runtimev1.TokenResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("renewToken")
	if s.renew != nil {
		return s.renew()
	}
	return runtimev1.TokenResponse{Token: tokenInfo("rt_unit_renewed_0123456789abcdef", time.Hour, 2*time.Hour)}, nil
}

func (s *scriptRT) Context(context.Context) (runtimev1.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("getContext")
	if s.fetch != nil {
		return s.fetch()
	}
	return s.ctx, nil
}

func (s *scriptRT) ResolveSecret(_ context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("resolveSecret")
	if s.resolve != nil {
		return s.resolve(req)
	}
	return runtimev1.SecretResolveResponse{}, &APIError{Op: "resolveSecret", Status: 403, Code: domain.CodeForbidden}
}

func (s *scriptRT) ReserveArtifact(_ context.Context, req runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("reserveArtifact")
	s.reserved = append(s.reserved, req)
	if s.reserve != nil {
		return s.reserve(req)
	}
	s.nextArt++
	id := domain.ArtifactID(fmt.Sprintf("art_unit%03d", s.nextArt))
	if req.Kind == runtimev1.KindOCI {
		return runtimev1.ArtifactReserveResponse{ArtifactID: id, State: runtimev1.StateRegistered}, nil
	}
	return runtimev1.ArtifactReserveResponse{ArtifactID: id, State: runtimev1.StateReserved, Upload: &runtimev1.Upload{
		Method: "PUT", URL: "https://objects.example.invalid/" + string(id), Headers: map[string]string{}, ExpiresAt: timestamp(time.Hour),
	}}, nil
}

func (s *scriptRT) CompleteArtifact(_ context.Context, id domain.ArtifactID, req runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("completeArtifact")
	if s.complete != nil {
		return s.complete(id, req)
	}
	return runtimev1.ArtifactCompleteResponse{ArtifactID: id, State: runtimev1.StateUsable, Digest: req.Digest, SizeBytes: req.SizeBytes}, nil
}

func (s *scriptRT) ProposeSource(_ context.Context, req runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("proposeSource")
	s.proposals = append(s.proposals, req)
	if s.propose != nil {
		return s.propose(len(s.proposals), req)
	}
	c := req.Commit
	return runtimev1.SourceProposalResponse{Status: runtimev1.PublishAccepted, Commit: req.Commit, AcceptedCommit: &c}, nil
}

func (s *scriptRT) SubmitResult(_ context.Context, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec("submitResult")
	s.submitted = append(s.submitted, req)
	if s.submit != nil {
		return s.submit(len(s.submitted), req)
	}
	return s.ack(req), nil
}

func (s *scriptRT) ack(req runtimev1.ResultRequest) runtimev1.ResultResponse {
	status := runtimev1.AttemptFailed
	if req.Result.Outcome == runtimev1.OutcomeCompleted {
		status = runtimev1.AttemptSucceeded
	}
	return runtimev1.ResultResponse{AttemptID: s.ctx.AttemptID, ResultHash: req.ResultHash, AttemptStatus: status, AcceptedAt: timestamp(0)}
}

// fakeUploader records uploaded objects by URL.
type fakeUploader struct {
	mu      sync.Mutex
	objects map[string][]byte
	err     error
}

func (u *fakeUploader) Upload(_ context.Context, target runtimev1.Upload, body io.ReaderAt, size int64) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return u.err
	}
	data := make([]byte, size)
	if _, err := body.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if u.objects == nil {
		u.objects = map[string][]byte{}
	}
	u.objects[target.URL] = data
	return nil
}

// object returns the uploaded bytes of an artifact.
func (u *fakeUploader) object(id domain.ArtifactID) []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.objects["https://objects.example.invalid/"+string(id)]
}

// unitContext is a valid no-workspace Context running script under /bin/sh.
func unitContext(script string) runtimev1.Context {
	return runtimev1.Context{
		TenantID: "t_acme", ProjectID: "p_inventory", RunID: "run_01", StepID: "build", AttemptID: "att_unit01", ActionKey: "run_01.build",
		Definition: runtimev1.Definition{ID: "pd_01", Digest: runtimev1.Digest("sha256:" + strings.Repeat("d", 64))},
		Command:    runtimev1.Command{Image: testImage, Command: []string{"/bin/sh", "-c"}, Args: []string{script}, Env: map[string]string{}},
		Workspace:  runtimev1.Workspace{Access: runtimev1.AccessNone},
		Inputs:     map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 30, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 64 << 10},
	}
}

// unit is one Step over a temporary Pod layout and the doubles.
type unit struct {
	t    *testing.T
	root string
	rt   *scriptRT
	up   *fakeUploader
	cfg  Config
	logs *safeBuf
	step *Step
}

type safeBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}
func (b *safeBuf) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.sb.String() }

const unitAdmission = "adm_unit_credential_0123456789abcdef"

var unitJob = runtimev1.JobIdentity{
	Name: "kodama-att-unit01", UID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f",
	PodName: "kodama-att-unit01-x7k2p", PodUID: "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60",
}

func newUnit(t *testing.T, script string) *unit {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"scratch", "identity", "outputs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "identity", "admission"), []byte(unitAdmission+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := &unit{t: t, root: root, rt: &scriptRT{ctx: unitContext(script)}, up: &fakeUploader{}, logs: &safeBuf{}}
	u.cfg = Config{
		ServerURL: "https://runtime.example.invalid", AttemptID: "att_unit01",
		AdmissionCredentialFile: filepath.Join(root, "identity", "admission"),
		CheckoutCredentialFile:  filepath.Join(root, "identity", "checkout-credential"),
		TokenFile:               filepath.Join(root, "identity", "token"),
		WorkspaceDir:            filepath.Join(root, "workspace"),
		OutputsDir:              filepath.Join(root, "outputs"),
		SecretsDir:              filepath.Join(root, "secrets"),
		IdentityDir:             filepath.Join(root, "identity"),
		ResultsFile:             filepath.Join(root, "results.json"),
		ScratchDir:              filepath.Join(root, "scratch"),
		Job:                     unitJob, ChildEnv: map[string]string{"PATH": os.Getenv("PATH"), "HOME": filepath.Join(root, "home")},
		KillGrace: 200 * time.Millisecond, FinalizeTimeout: 10 * time.Second, SettleTimeout: 5 * time.Second,
		SettleBackoff: time.Millisecond, RenewMinInterval: 10 * time.Millisecond, InterruptTimeout: 5 * time.Second,
		Logger: slog.New(slog.NewJSONHandler(u.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return u
}

// build creates the Step from the (possibly modified) configuration.
func (u *unit) build() *Step {
	u.t.Helper()
	u.step = NewStep(u.cfg, u.rt, u.up)
	return u.step
}

// run executes the lifecycle (bounded) and returns the exit code.
func (u *unit) run() int {
	u.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return u.build().Run(ctx)
}

func (u *unit) wantExit(got, want int) {
	u.t.Helper()
	if got != want {
		u.t.Fatalf("exit = %d, want %d\ncalls: %v\nlogs:\n%s", got, want, u.rt.calls, u.logs.String())
	}
}

// lastResult is the final submitted result.
func (u *unit) lastResult() runtimev1.Result {
	u.t.Helper()
	u.rt.mu.Lock()
	defer u.rt.mu.Unlock()
	if len(u.rt.submitted) == 0 {
		u.t.Fatalf("no result submitted; calls %v\nlogs:\n%s", u.rt.calls, u.logs.String())
	}
	return u.rt.submitted[len(u.rt.submitted)-1].Result
}

func wantReason(t *testing.T, r runtimev1.Result, reason string) {
	t.Helper()
	if r.Outcome != runtimev1.OutcomeFailed || r.Reason == nil || *r.Reason != reason {
		got := "<nil>"
		if r.Reason != nil {
			got = *r.Reason
		}
		t.Fatalf("result = %s/%s, want failed/%s", r.Outcome, got, reason)
	}
}

func digestOfBytes(b []byte) runtimev1.Digest {
	sum := sha256.Sum256(b)
	return runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:]))
}
