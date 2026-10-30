package entrypoint

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

const (
	pgRef  = "secret://project/test-db-password"
	apiRef = "secret://project/api-key"
	other  = "secret://project/some-other-secret"
)

// svcUnit is a service supervisor over a scripted resource port.
type svcUnit struct {
	t      *testing.T
	root   string
	rt     *scriptRes
	cfg    ResourceConfig
	logs   *safeBuf
	stdout *safeBuf
	stderr *safeBuf
	values map[string]string
}

// realTempDir is a temporary directory whose path holds no symlink (the secrets
// directory is never reached through one).
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newSvcUnit(t *testing.T, script string) *svcUnit {
	t.Helper()
	root := realTempDir(t)
	u := &svcUnit{t: t, root: root, rt: &scriptRes{}, logs: &safeBuf{}, stdout: &safeBuf{}, stderr: &safeBuf{},
		values: map[string]string{pgRef + "@3": "pg-secret-value-0123", apiRef + "@1": "api-secret-value-4567", other + "@1": "never-for-this-service"}}
	writeFile(t, filepath.Join(root, "identity", "admission"), unitAdmission+"\n")
	deadline := timestamp(time.Hour)
	u.rt.ctx = runtimev2.ResourceContext{
		ContextVersion: 2, ResourceKind: runtimev2.ResourceService, TenantID: "t_acme", ProjectID: "p_inventory", RunID: "run_01", ResourceKey: "service.db",
		Service: &runtimev2.ServiceContext{
			Name: "db", Role: runtimev2.RoleGeneral, DeadlineAt: deadline,
			Command: runtimev2.ServiceCommand{Image: testImage, Command: []string{"/bin/sh", "-c"}, Args: []string{script}, Env: map[string]string{"POSTGRES_DB": "app", "PGDATA": "/data"}},
			Secrets: []runtimev1.SecretBinding{
				{Ref: pgRef, Version: 3, Name: "postgres-password", Delivery: runtimev1.DeliveryFile},
				{Ref: apiRef, Version: 1, Name: "api-key", Delivery: runtimev1.DeliveryEnv},
			},
		},
		Policy: runtimev2.PolicyPin{ID: "run-v1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("f", 64))},
	}
	u.rt.resolve = func(req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
		for _, b := range u.rt.ctx.Service.Secrets {
			if b.Ref == req.Ref && b.Version == req.Version {
				v := u.values[req.Ref+"@"+itoa(req.Version)]
				return runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery, ValueBase64: base64.StdEncoding.EncodeToString([]byte(v))}, nil
			}
		}
		return runtimev1.SecretResolveResponse{}, &APIError{Op: "resolveResourceSecret", Status: 404, Code: domain.CodeNotFound}
	}
	u.cfg = ResourceConfig{
		ServerURL: "https://runtime.example.invalid", RunID: "run_01", Key: "service.db", AdmissionCredentialFile: filepath.Join(root, "identity", "admission"),
		Identity:   runtimev2.ResourceIdentity{WorkloadName: "s-ab12cd34ef567890-db", WorkloadUID: "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071"},
		SecretsDir: filepath.Join(root, "secrets"), KillGrace: 2 * time.Second, AdmissionWait: 3 * time.Second,
		ChildEnv: serviceBaseEnv(os.Environ()),
		Logger:   slog.New(slog.NewJSONHandler(u.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return u
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (u *svcUnit) argv() []string {
	s := u.rt.ctx.Service
	return append(append([]string{}, s.Command.Command...), s.Command.Args...)
}

func (u *svcUnit) run(ctx context.Context, argv []string) int {
	u.t.Helper()
	return newServiceRun(u.cfg, u.rt, u.stdout, u.stderr).run(ctx, argv)
}

func Test_unit_service_run_resolves_only_pinned_secrets(t *testing.T) {
	t.Run("exactly the committed bindings are resolved and delivered privately", func(t *testing.T) {
		u := newSvcUnit(t, "")
		out := filepath.Join(u.root, "out")
		pw := filepath.Join(u.cfg.SecretsDir, "postgres-password")
		disposed := filepath.Join(u.root, "disposed")
		u.rt.onDispose = func() { writeFile(t, disposed, "x") }
		script := `test -f ` + disposed + ` || exit 99; printf '%s|%s|%s|%s|%s|%s' "$API_KEY" "$POSTGRES_DB" "$PGDATA" "$(cat ` + pw + `)" "${KODAMA_SERVER_URL:-unset}" "$(stat -c %a ` + pw + ` 2>/dev/null || stat -f %Lp ` + pw + `)" > ` + out + `; exit 7`
		u.rt.ctx.Service.Command.Args = []string{script}
		t.Setenv("KODAMA_SERVER_URL", "https://leak.example.invalid")
		u.cfg.ChildEnv = serviceBaseEnv(os.Environ()) // as RunServiceRun builds it
		code := u.run(context.Background(), u.argv())
		if code != 7 {
			t.Fatalf("exit %d, want the service's own 7\n%s", code, u.logs.String())
		}
		got, _ := os.ReadFile(out)
		parts := strings.Split(string(got), "|")
		if len(parts) != 6 || parts[0] != "api-secret-value-4567" || parts[1] != "app" || parts[2] != "/data" || parts[3] != "pg-secret-value-0123" || parts[4] != "unset" || parts[5] != "400" {
			t.Fatalf("the service saw %q", got)
		}
		// Only the two pinned bindings were requested, in order, and nothing else was called.
		if len(u.rt.resolved) != 2 || u.rt.resolved[0] != (runtimev1.SecretResolveRequest{Ref: pgRef, Version: 3}) || u.rt.resolved[1] != (runtimev1.SecretResolveRequest{Ref: apiRef, Version: 1}) {
			t.Fatalf("resolved %+v", u.rt.resolved)
		}
		for _, c := range u.rt.calls {
			if c != "admit" && c != "resolveSecret" && c != "dispose" {
				t.Fatalf("a supervisor called %s (calls %v)", c, u.rt.calls)
			}
		}
		// Authority is dropped before the service runs: no renewal loop at all.
		if u.rt.count("renew") != 0 {
			t.Fatal("the supervisor kept renewing a token while the service ran")
		}
		// The delivered file is gone with the service; the values are in no log.
		if _, err := os.Stat(pw); err == nil {
			t.Fatal("a delivered secret file outlived the service")
		}
		for name, hay := range map[string]string{"logs": u.logs.String(), "stdout": u.stdout.String(), "stderr": u.stderr.String()} {
			for _, v := range u.values {
				if strings.Contains(hay, v) {
					t.Fatalf("a secret value is in the %s", name)
				}
			}
		}
		// The admission carried the committed identity and a fresh nonce.
		if a := u.rt.admits[0]; a.AdmissionCredential != unitAdmission || a.Identity.WorkloadUID != u.cfg.Identity.WorkloadUID || a.Nonce == "" {
			t.Fatalf("admit %+v", a)
		}
	})

	t.Run("a service without bindings resolves nothing", func(t *testing.T) {
		u := newSvcUnit(t, "exit 0")
		u.rt.ctx.Service.Secrets = []runtimev1.SecretBinding{}
		if code := u.run(context.Background(), u.argv()); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if u.rt.count("resolveSecret") != 0 {
			t.Fatal("something was resolved")
		}
	})

	t.Run("the command line must be the committed command", func(t *testing.T) {
		for name, argv := range map[string][]string{
			"another program": {"/bin/sh", "-c", "touch ran-elsewhere"},
			"extra arguments": append(newSvcUnit(t, "exit 0").argv(), "--extra"),
			"nothing":         nil,
		} {
			u := newSvcUnit(t, "exit 0")
			ran := filepath.Join(u.root, "ran-elsewhere")
			if name == "another program" {
				argv = []string{"/bin/sh", "-c", "touch " + ran}
			}
			if code := u.run(context.Background(), argv); code != ExitAdmission {
				t.Errorf("%s: exit %d", name, code)
			}
			if u.rt.count("resolveSecret") != 0 {
				t.Errorf("%s: secrets were resolved for a command that is not the service's", name)
			}
			if _, err := os.Stat(ran); err == nil {
				t.Errorf("%s: it ran", name)
			}
		}
	})

	t.Run("a refusal delivers nothing and starts nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			resolve func(runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error)
			want    int
		}{
			{"revoked", func(runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
				return runtimev1.SecretResolveResponse{}, resErr("resolveResourceSecret", 403, runtimev2.CodeResourceRevoked)
			}, ExitRevoked},
			{"a binding the server does not know", func(runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
				return runtimev1.SecretResolveResponse{}, resErr("resolveResourceSecret", 404, domain.CodeNotFound)
			}, ExitServiceFailed},
			{"another secret echoed", func(req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
				return runtimev1.SecretResolveResponse{Ref: other, Version: 1, Name: "x", Delivery: runtimev1.DeliveryEnv, ValueBase64: "c2VjcmV0"}, nil
			}, ExitServiceFailed},
			{"invalid base64", func(req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
				return runtimev1.SecretResolveResponse{Ref: req.Ref, Version: req.Version, Name: "postgres-password", Delivery: runtimev1.DeliveryFile, ValueBase64: "***"}, nil
			}, ExitServiceFailed},
			{"unreachable", func(runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
				return runtimev1.SecretResolveResponse{}, &TransportError{Op: "resolveResourceSecret", Err: errors.New("reset")}
			}, ExitServiceFailed},
		} {
			t.Run(tc.name, func(t *testing.T) {
				u := newSvcUnit(t, "")
				ran := filepath.Join(u.root, "ran")
				u.rt.ctx.Service.Command.Args = []string{"touch " + ran}
				u.rt.resolve = tc.resolve
				if code := u.run(context.Background(), u.argv()); code != tc.want {
					t.Fatalf("exit %d, want %d\n%s", code, tc.want, u.logs.String())
				}
				if _, err := os.Stat(ran); err == nil {
					t.Fatal("the service started after a failed delivery")
				}
				if entries, _ := os.ReadDir(u.cfg.SecretsDir); len(entries) != 0 {
					t.Fatalf("secret files were left behind: %v", entries)
				}
			})
		}
	})

	t.Run("admission refusals and foreign contexts", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			want int
		}{
			{"revoked", resErr("admitResource", 403, runtimev2.CodeResourceRevoked), ExitRevoked},
			{"foreign identity", resErr("admitResource", 403, domain.CodeForbidden), ExitAdmission},
			{"consumed", resErr("admitResource", 409, runtimev1.CodeAdmissionConsumed), ExitAdmission},
		} {
			u := newSvcUnit(t, "exit 0")
			u.rt.admit = func(int, runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
				return runtimev2.ResourceAdmitResponse{}, tc.err
			}
			if code := u.run(context.Background(), u.argv()); code != tc.want || u.rt.count("resolveSecret") != 0 {
				t.Errorf("%s: exit %d", tc.name, code)
			}
		}
		// A preparation context for a service key is another resource's.
		u := newSvcUnit(t, "exit 0")
		p := newPrepUnit(t, nil)
		u.rt.ctx = p.rt.ctx
		u.rt.admit = func(int, runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
			return runtimev2.ResourceAdmitResponse{Token: p.rt.token(), Context: p.rt.ctx}, nil
		}
		if code := u.run(context.Background(), []string{"true"}); code != ExitAdmission || u.rt.count("resolveSecret") != 0 {
			t.Errorf("a preparation context: exit %d", code)
		}
	})

	t.Run("signals reach the service and its exit status is mirrored", func(t *testing.T) {
		u := newSvcUnit(t, "")
		ready := filepath.Join(u.root, "ready")
		got := filepath.Join(u.root, "got")
		u.rt.ctx.Service.Secrets = []runtimev1.SecretBinding{}
		u.rt.ctx.Service.Command.Args = []string{`trap 'echo usr1 >> ` + got + `' USR1; trap 'echo term >> ` + got + `; exit 3' TERM; touch ` + ready + `; while :; do sleep 0.05; done`}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- u.run(ctx, u.argv()) }()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the service never started")
			}
			time.Sleep(10 * time.Millisecond)
		}
		// A reload-style signal sent to the supervisor reaches the service unchanged.
		time.Sleep(100 * time.Millisecond)
		if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		cancel() // the stop: SIGTERM to the service, its grace, then its status
		select {
		case code := <-done:
			if code != 3 {
				t.Fatalf("exit %d, want the service's 3", code)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the supervisor did not stop with the service")
		}
		b, _ := os.ReadFile(got)
		if !bytes.Contains(b, []byte("usr1")) || !bytes.Contains(b, []byte("term")) {
			t.Fatalf("the service received %q", b)
		}
	})

	t.Run("a service that ignores the stop is killed after its grace", func(t *testing.T) {
		u := newSvcUnit(t, "")
		u.cfg.KillGrace = 300 * time.Millisecond
		ready := filepath.Join(u.root, "ready")
		u.rt.ctx.Service.Secrets = []runtimev1.SecretBinding{}
		u.rt.ctx.Service.Command.Args = []string{`trap '' TERM; touch ` + ready + `; while :; do sleep 0.05; done`}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- u.run(ctx, u.argv()) }()
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		select {
		case code := <-done:
			if code != 128+int(syscall.SIGKILL) {
				t.Fatalf("exit %d", code)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a stubborn service was not killed")
		}
	})

	t.Run("the supervisor's environment never reaches the service", func(t *testing.T) {
		env := serviceBaseEnv([]string{"PATH=/bin", "KODAMA_SERVER_URL=x", "KODAMA_ADMISSION_CREDENTIAL_FILE=/y", "HOME=/h", "KODAMA=1x"})
		if strings.Join(env, ",") != "PATH=/bin,HOME=/h,KODAMA=1x" {
			t.Fatalf("env %v", env)
		}
		merged := overlayEnv([]string{"A=1", "B=2"}, map[string]string{"A": "9", "C": "3"})
		if strings.Join(merged, ",") != "B=2,A=9,C=3" && strings.Join(merged, ",") != "B=2,C=3,A=9" {
			t.Fatalf("overlay %v", merged)
		}
	})
}

// Review finding 4: the resource bearer token must not stay in the supervisor's
// memory while the service runs in the same process namespace. Against the real
// HTTP client: authority is used for admission and secrets, then disposed of; a
// later call sends nothing.
func Test_unit_service_run_disposes_resource_authority(t *testing.T) {
	var mu sync.Mutex
	var seen []string // "METHOD path auth?"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+strconv.FormatBool(r.Header.Get("Authorization") != ""))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/admit"):
			ctx := newSvcUnit(t, "").rt.ctx
			ctx.Service.Command.Args = []string{"touch " + filepath.Join(t.TempDir(), "x")}
			_ = json.NewEncoder(w).Encode(runtimev2.ResourceAdmitResponse{Token: (&scriptRes{ctx: ctx}).token(), Context: ctx})
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"x","requestId":"r","details":{}}}`))
		}
	}))
	t.Cleanup(srv.Close)

	client, err := NewResourceClient(srv.URL, "run_01", "service.db", "", RetryPolicy{MaxAttempts: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := runtimev2.ResourceIdentity{WorkloadName: "s-ab12cd34ef567890-db", WorkloadUID: "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071"}
	if _, err := client.AdmitResource(ctx, runtimev2.ResourceAdmitRequest{AdmissionCredential: unitAdmission, Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0", Identity: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResourceContext(ctx); err == nil || apiCode(err) != domain.CodeForbidden {
		t.Fatalf("an authenticated call must reach the server: %v", err)
	}
	if client.c.bearer() == "" {
		t.Fatal("the client holds no token after admission")
	}
	client.Dispose()
	if client.c.bearer() != "" {
		t.Fatal("the bearer token survived disposal")
	}
	mu.Lock()
	before := len(seen)
	mu.Unlock()
	for name, call := range map[string]func() error{
		"context": func() error { _, err := client.ResourceContext(ctx); return err },
		"renew":   func() error { _, err := client.RenewResourceToken(ctx); return err },
		"resolve": func() error {
			_, err := client.ResolveSecret(ctx, runtimev1.SecretResolveRequest{Ref: pgRef, Version: 3})
			return err
		},
		"complete": func() error {
			_, err := client.CompletePreparation(ctx, runtimev2.PreparationReceiptRequest{Commit: runtimev1.GitCommit(strings.Repeat("1", 40)), TreeID: runtimev1.GitCommit(strings.Repeat("2", 40)), HooksDisabled: true, Submodules: "skipped", CredentialsRemoved: true, Attempts: 1})
			return err
		},
		"admit again": func() error {
			_, err := client.AdmitResource(ctx, runtimev2.ResourceAdmitRequest{AdmissionCredential: unitAdmission, Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0", Identity: id})
			return err
		},
	} {
		var pe *ProtocolError
		if err := call(); !errors.As(err, &pe) {
			t.Errorf("%s after disposal: %v (want a local refusal)", name, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != before {
		t.Fatalf("requests left after disposal: %v", seen[before:])
	}
	for _, line := range seen[1:before] {
		if !strings.Contains(line, "auth=true") {
			t.Fatalf("unauthenticated call before disposal: %s", line)
		}
	}
}

// The supervisor disposes of the authority before the child exists, and the
// client it used is empty while the service runs.
func Test_unit_service_run_holds_no_token_while_the_service_runs(t *testing.T) {
	u := newSvcUnit(t, "")
	var client *ResourceClient
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/admit") {
			_ = json.NewEncoder(w).Encode(runtimev2.ResourceAdmitResponse{Token: u.rt.token(), Context: u.rt.ctx})
			return
		}
		var req runtimev1.SecretResolveRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp, _ := u.rt.resolve(req)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	ready := filepath.Join(u.root, "ready")
	u.rt.ctx.Service.Command.Args = []string{"touch " + ready + "; while :; do sleep 0.05; done"}
	var err error
	if client, err = NewResourceClient(srv.URL, "run_01", "service.db", "", RetryPolicy{MaxAttempts: 1}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- newServiceRun(u.cfg, client, u.stdout, u.stderr).run(ctx, u.argv()) }()
	for i := 0; i < 500; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		cancel()
		t.Fatalf("the service never started\n%s", u.logs.String())
	}
	if client.c.bearer() != "" || !client.c.isDisposed() {
		t.Fatal("the supervisor still holds the resource token while the service runs")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor did not stop")
	}
}

// Review finding 5: a service's file secrets go only into its own private
// directory. A destination that overlaps the workspace or the credential
// directory, or that is (or sits beneath) a symlink, is refused before any secret
// is requested.
func Test_unit_service_run_refuses_unsafe_secret_destinations(t *testing.T) {
	type setup func(t *testing.T, u *svcUnit) string // returns the secrets dir to configure
	for name, mk := range map[string]setup{
		"inside the run workspace": func(t *testing.T, u *svcUnit) string {
			u.cfg.WorkspaceDir = filepath.Join(u.root, "workspace")
			return filepath.Join(u.cfg.WorkspaceDir, "secrets")
		},
		"the run workspace itself": func(t *testing.T, u *svcUnit) string {
			u.cfg.WorkspaceDir = filepath.Join(u.root, "workspace")
			return u.cfg.WorkspaceDir
		},
		"a parent of the run workspace": func(t *testing.T, u *svcUnit) string {
			u.cfg.WorkspaceDir = filepath.Join(u.root, "vol", "workspace")
			return filepath.Join(u.root, "vol")
		},
		"inside the identity directory": func(t *testing.T, u *svcUnit) string {
			return filepath.Join(u.root, "identity", "secrets")
		},
		"the identity directory itself": func(t *testing.T, u *svcUnit) string {
			return filepath.Join(u.root, "identity")
		},
		"a directory holding the checkout credential": func(t *testing.T, u *svcUnit) string {
			u.cfg.CheckoutCredentialFile = filepath.Join(u.root, "git", "credential")
			return filepath.Join(u.root, "git")
		},
		"a symlink to a private directory": func(t *testing.T, u *svcUnit) string {
			real := filepath.Join(u.root, "real-secrets")
			if err := os.MkdirAll(real, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(u.root, "secrets-link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"a symlink into the run workspace": func(t *testing.T, u *svcUnit) string {
			u.cfg.WorkspaceDir = filepath.Join(u.root, "workspace")
			if err := os.MkdirAll(filepath.Join(u.cfg.WorkspaceDir, "inner"), 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(u.root, "secrets-link")
			if err := os.Symlink(filepath.Join(u.cfg.WorkspaceDir, "inner"), link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"a symlinked parent component": func(t *testing.T, u *svcUnit) string {
			real := filepath.Join(u.root, "real-parent")
			if err := os.MkdirAll(real, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(u.root, "parent-link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(link, "secrets")
		},
		"a relative path": func(t *testing.T, u *svcUnit) string { return "secrets" },
		"an unclean path": func(t *testing.T, u *svcUnit) string {
			return u.root + "/a/../secrets"
		},
	} {
		t.Run(name, func(t *testing.T) {
			u := newSvcUnit(t, "")
			u.cfg.SecretsDir = mk(t, u)
			ran := filepath.Join(u.root, "ran")
			u.rt.ctx.Service.Command.Args = []string{"touch " + ran}
			code := u.run(context.Background(), u.argv())
			if code != ExitServiceFailed {
				t.Fatalf("exit %d\n%s", code, u.logs.String())
			}
			if u.rt.count("resolveSecret") != 0 {
				t.Fatal("a secret was requested for an unsafe destination")
			}
			if _, err := os.Stat(ran); err == nil {
				t.Fatal("the service started")
			}
			// Nothing was written through the link or into the other directory.
			for _, d := range []string{"real-secrets", "real-parent", "workspace/inner"} {
				if entries, _ := os.ReadDir(filepath.Join(u.root, d)); len(entries) != 0 {
					t.Fatalf("%s received %v", d, entries)
				}
			}
			if entries, _ := os.ReadDir(filepath.Join(u.root, "identity")); len(entries) != 1 {
				t.Fatalf("the identity directory holds %v", entries)
			}
		})
	}

	t.Run("a file secret planted as a symlink is never written through", func(t *testing.T) {
		u := newSvcUnit(t, "")
		if err := os.MkdirAll(u.cfg.SecretsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(u.root, "victim")
		writeFile(t, victim, "original")
		if err := os.Symlink(victim, filepath.Join(u.cfg.SecretsDir, "postgres-password")); err != nil {
			t.Fatal(err)
		}
		ran := filepath.Join(u.root, "ran")
		u.rt.ctx.Service.Command.Args = []string{"touch " + ran}
		if code := u.run(context.Background(), u.argv()); code != ExitServiceFailed {
			t.Fatalf("exit %d", code)
		}
		if b, _ := os.ReadFile(victim); string(b) != "original" {
			t.Fatalf("the secret was written through the link: %q", b)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Fatal("the service started")
		}
	})

	t.Run("a service with only env secrets needs no directory", func(t *testing.T) {
		u := newSvcUnit(t, "exit 0")
		u.cfg.SecretsDir = "relative-and-unused"
		u.rt.ctx.Service.Secrets = u.rt.ctx.Service.Secrets[1:] // the env binding only
		if code := u.run(context.Background(), u.argv()); code != 0 {
			t.Fatalf("exit %d\n%s", code, u.logs.String())
		}
	})

	t.Run("an own, disjoint, canonical directory is created private and works", func(t *testing.T) {
		u := newSvcUnit(t, "exit 0")
		u.cfg.SecretsDir = filepath.Join(u.root, "kodama", "secrets") // neither level exists yet
		if code := u.run(context.Background(), u.argv()); code != 0 {
			t.Fatalf("exit %d\n%s", code, u.logs.String())
		}
		if fi, err := os.Stat(u.cfg.SecretsDir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%v %v", fi, err)
		}
	})
}
