package entrypoint

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

type fakeRuntime struct {
	secrets  map[string]runtimev1.SecretResolveResponse
	resolved []runtimev1.SecretResolveRequest
	err      error
}

func (f *fakeRuntime) Admit(context.Context, runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
	return runtimev1.AdmitResponse{}, errors.New("unused")
}
func (f *fakeRuntime) RenewToken(context.Context) (runtimev1.TokenResponse, error) {
	return runtimev1.TokenResponse{}, errors.New("unused")
}
func (f *fakeRuntime) Context(context.Context) (runtimev1.Context, error) {
	return runtimev1.Context{}, errors.New("unused")
}
func (f *fakeRuntime) ResolveSecret(_ context.Context, r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	f.resolved = append(f.resolved, r)
	if f.err != nil {
		return runtimev1.SecretResolveResponse{}, f.err
	}
	resp, ok := f.secrets[r.Ref]
	if !ok {
		return runtimev1.SecretResolveResponse{}, &APIError{Op: "resolveSecret", Status: 403, Code: domain.CodeForbidden}
	}
	return resp, nil
}
func (f *fakeRuntime) ReserveArtifact(context.Context, runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
	return runtimev1.ArtifactReserveResponse{}, errors.New("unused")
}
func (f *fakeRuntime) CompleteArtifact(context.Context, domain.ArtifactID, runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
	return runtimev1.ArtifactCompleteResponse{}, errors.New("unused")
}
func (f *fakeRuntime) ProposeSource(context.Context, runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
	return runtimev1.SourceProposalResponse{}, errors.New("unused")
}
func (f *fakeRuntime) SubmitResult(context.Context, runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
	return runtimev1.ResultResponse{}, errors.New("unused")
}

func binding(ref, name string, d runtimev1.SecretDelivery) runtimev1.SecretBinding {
	return runtimev1.SecretBinding{Ref: ref, Version: 2, Name: name, Delivery: d}
}

func response(b runtimev1.SecretBinding, value string) runtimev1.SecretResolveResponse {
	return runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery, ValueBase64: base64.StdEncoding.EncodeToString([]byte(value))}
}

func TestMaterializeSecretsFileAndEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	file := binding("secret://p/erp-key", "erp-key", runtimev1.DeliveryFile)
	env := binding("secret://p/api-token", "api-token", runtimev1.DeliveryEnv)
	rt := &fakeRuntime{secrets: map[string]runtimev1.SecretResolveResponse{file.Ref: response(file, "file-value"), env.Ref: response(env, "env-value")}}
	set, err := materializeSecrets(context.Background(), rt, []runtimev1.SecretBinding{file, env}, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Files live in an exclusive, private child of the supplied root; the command
	// is handed the child.
	if filepath.Dir(set.dir) != dir || set.dir == dir {
		t.Fatalf("secrets are not in a child of the root: %q under %q", set.dir, dir)
	}
	fi, _ := os.Stat(set.dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("child mode = %v", fi.Mode().Perm())
	}
	p := filepath.Join(set.dir, "erp-key")
	fi, err = os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "file-value" {
		t.Fatalf("content = %q", b)
	}
	if set.env["API_TOKEN"] != "env-value" || len(set.env) != 1 {
		t.Fatalf("env = %v", set.env)
	}
	if _, err := os.Stat(filepath.Join(set.dir, "api-token")); err == nil {
		t.Fatal("env-delivered secret must not be written to disk")
	}
	if len(rt.resolved) != 2 {
		t.Fatalf("resolved = %v", rt.resolved)
	}
	child := set.dir
	set.cleanup()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove files")
	}
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the child directory")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the secrets root is not left as found: %v", entries)
	}
	set.cleanup() // idempotent
	if set.env != nil {
		t.Fatal("cleanup must forget env")
	}
}

func TestMaterializeSecretsNoneDeclaredDoesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	rt := &fakeRuntime{}
	if _, err := materializeSecrets(context.Background(), rt, nil, dir); err != nil {
		t.Fatal(err)
	}
	if len(rt.resolved) != 0 {
		t.Fatalf("resolved %v with nothing declared", rt.resolved)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("secrets directory created though no secret is declared")
	}
}

func TestMaterializeSecretsRefusesMismatchedResponse(t *testing.T) {
	b := binding("secret://p/erp-key", "erp-key", runtimev1.DeliveryFile)
	cases := map[string]func(*runtimev1.SecretResolveResponse){
		"other ref":      func(r *runtimev1.SecretResolveResponse) { r.Ref = "secret://p/other" },
		"other version":  func(r *runtimev1.SecretResolveResponse) { r.Version = 9 },
		"other name":     func(r *runtimev1.SecretResolveResponse) { r.Name = "other" },
		"other delivery": func(r *runtimev1.SecretResolveResponse) { r.Delivery = runtimev1.DeliveryEnv },
		"bad base64":     func(r *runtimev1.SecretResolveResponse) { r.ValueBase64 = "!!!" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			resp := response(b, "v")
			mutate(&resp)
			dir := filepath.Join(t.TempDir(), "s")
			set, err := materializeSecrets(context.Background(), &fakeRuntime{secrets: map[string]runtimev1.SecretResolveResponse{b.Ref: resp}}, []runtimev1.SecretBinding{b}, dir)
			if err == nil {
				t.Fatal("accepted")
			}
			if entries, _ := os.ReadDir(set.dir); len(entries) != 0 {
				t.Fatalf("files written despite error: %v", entries)
			}
			// The caller cleans up the returned set on every exit path.
			set.cleanup()
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("a directory outlived the failed delivery: %v", entries)
			}
		})
	}
}

func TestMaterializeSecretsRejectsUnsafeFileNames(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", "/abs", ".."} {
		b := binding("secret://p/x", name, runtimev1.DeliveryFile)
		dir := filepath.Join(t.TempDir(), "s")
		rt := &fakeRuntime{secrets: map[string]runtimev1.SecretResolveResponse{b.Ref: response(b, "v")}}
		if _, err := materializeSecrets(context.Background(), rt, []runtimev1.SecretBinding{b}, dir); err == nil {
			t.Errorf("name %q accepted", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
			t.Errorf("name %q escaped the secrets directory", name)
		}
	}
}

func TestMaterializeSecretsRefusesOverwriteAndDuplicateEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	a := binding("secret://p/a", "same", runtimev1.DeliveryFile)
	b := binding("secret://p/b", "same", runtimev1.DeliveryFile)
	rt := &fakeRuntime{secrets: map[string]runtimev1.SecretResolveResponse{a.Ref: response(a, "1"), b.Ref: response(b, "2")}}
	if _, err := materializeSecrets(context.Background(), rt, []runtimev1.SecretBinding{a, b}, dir); err == nil {
		t.Fatal("second file with the same name must not overwrite the first")
	}
	e1 := binding("secret://p/e1", "my-key", runtimev1.DeliveryEnv)
	e2 := binding("secret://p/e2", "my_key", runtimev1.DeliveryEnv)
	rt = &fakeRuntime{secrets: map[string]runtimev1.SecretResolveResponse{e1.Ref: response(e1, "1"), e2.Ref: response(e2, "2")}}
	if _, err := materializeSecrets(context.Background(), rt, []runtimev1.SecretBinding{e1, e2}, filepath.Join(t.TempDir(), "s")); err == nil {
		t.Fatal("colliding env names must be refused")
	}
}

func TestMaterializeSecretsPropagatesDenial(t *testing.T) {
	b := binding("secret://p/undeclared", "x", runtimev1.DeliveryEnv)
	_, err := materializeSecrets(context.Background(), &fakeRuntime{}, []runtimev1.SecretBinding{b}, filepath.Join(t.TempDir(), "s"))
	if apiCode(err) != domain.CodeForbidden {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvNameFor(t *testing.T) {
	for in, want := range map[string]string{"erp-token": "ERP_TOKEN", "a.b": "A_B", "9lives": "_9LIVES", "ok_1": "OK_1"} {
		if got := envNameFor(in); got != want {
			t.Errorf("envNameFor(%q) = %q, want %q", in, got, want)
		}
	}
}
