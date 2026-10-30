package entrypoint

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// `kodama service-run -- argv...` is the supervisor of a run service that has
// pinned secrets (a service without bindings runs its declared image and argv
// directly, with no supervisor). It
//
//  1. admits as the run's `service.NAME` resource (token of the resource audience,
//     bound to the committed workload instance);
//  2. checks that the argv it was given is exactly the command the server committed
//     for this service;
//  3. resolves ONLY the service's own pinned bindings through the resource secrets
//     route and delivers them privately: env values into the child's environment,
//     files into the service's memory-backed secrets directory (never a shared
//     mount, never the run workspace);
//  4. drops its token, so nothing the service can reach holds authority, and execs
//     argv as a child, forwarding signals and mirroring its exit status.
//
// A service shares no authority with anything else: it cannot call source, result,
// artifacts, begin, the model/MCP/deployment proxies or the preparation routes, and
// no other service's bindings, whatever this process does (the server enforces it).

// ServiceRunCommand returns the `kodama service-run` registration. The composition
// root (T-116) registers it beside StepCommand().
func ServiceRunCommand() Command {
	return Command{Name: ServiceRunCommandName, Summary: "Deliver a run service's pinned secrets and run its command (run_shared services)", Run: RunServiceRun}
}

// RunServiceRun parses args (after "service-run": flags, then `--` and the argv)
// and the environment, runs the supervisor and returns the service's exit code.
func RunServiceRun(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	var output bytes.Buffer
	cfg, argv, err := parseResourceConfig(ServiceRunCommandName, args, getenv, &output)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = stdout.Write(output.Bytes())
			return ExitOK
		}
		if output.Len() > 0 {
			_, _ = stderr.Write(output.Bytes())
		}
		fmt.Fprintln(stderr, "kodama service-run:", err)
		return ExitUsage
	}
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "kodama service-run: the service command follows `--`")
		return ExitUsage
	}
	cfg.ChildEnv = serviceBaseEnv(os.Environ())
	cfg.Logger = slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	// SIGTERM/SIGINT stop the supervisor; once the service runs, they are passed on
	// with its grace and every other signal is forwarded as it arrives (exec).
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return ExecuteServiceRun(ctx, cfg, argv, stdout, stderr)
}

// serviceBaseEnv is the process environment minus every KODAMA_ variable: the
// supervisor's identity, server address and admission files are not the service's.
func serviceBaseEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "KODAMA_") {
			out = append(out, kv)
		}
	}
	return out
}

// ExecuteServiceRun runs the supervisor with the real HTTP adapter.
func ExecuteServiceRun(ctx context.Context, cfg ResourceConfig, argv []string, stdout, stderr io.Writer) int {
	cfg.applyDefaults()
	kind, ok := resourceKindOfKey(cfg.Key)
	if err := cfg.Validate(); err != nil || !ok || kind != runtimev2.ResourceService {
		if cfg.Logger != nil {
			cfg.Logger.Error("invalid configuration", "error", fmt.Sprint(err))
		}
		return ExitUsage
	}
	client, err := NewResourceClient(cfg.ServerURL, domain.RunID(cfg.RunID), cfg.Key, cfg.CAFile, cfg.Retry, cfg.Logger)
	if err != nil {
		if cfg.Logger != nil {
			cfg.Logger.Error("cannot create runtime client", "error", err.Error())
		}
		return ExitUsage
	}
	return newServiceRun(cfg, client, stdout, stderr).run(ctx, argv)
}

type serviceRun struct {
	cfg            ResourceConfig
	rt             resourceRuntime
	log            *slog.Logger
	stdout, stderr io.Writer
}

func newServiceRun(cfg ResourceConfig, rt resourceRuntime, stdout, stderr io.Writer) *serviceRun {
	cfg.applyDefaults()
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	}
	return &serviceRun{cfg: cfg, rt: rt, log: log, stdout: stdout, stderr: stderr}
}

func (s *serviceRun) run(ctx context.Context, argv []string) int {
	cfg, log := s.cfg, s.log
	credential, err := readTrimmed(cfg.AdmissionCredentialFile)
	if err != nil {
		log.Error("cannot read admission credential", "error", scrub(err))
		return ExitUsage
	}
	// Whatever happens, the process leaves this function holding no authority; the
	// service itself starts only after the disposal below.
	defer s.rt.Dispose()
	resp, err := admitResource(ctx, s.rt, cfg, credential, log, runtimev2.ResourceService)
	credential = "" // the one-time credential is spent: this process keeps no copy
	if err != nil {
		log.Error("admission failed", "error", redactErr(err))
		if isResourceRevoked(err) {
			return ExitRevoked
		}
		return ExitAdmission
	}
	svc := resp.Context.Service
	log.Info("admitted", "resource", cfg.Key, "bindings", len(svc.Secrets))

	// The argv on the command line is the adapter's rendering of the committed
	// command; anything else is not this service.
	want := append(append([]string{}, svc.Command.Command...), svc.Command.Args...)
	if !slices.Equal(argv, want) || len(want) == 0 {
		log.Error("the command line is not the committed service command")
		return ExitAdmission
	}

	set := &serviceSecrets{env: map[string]string{}}
	defer set.cleanup()
	if err := s.openSecretsDir(svc, set); err != nil {
		log.Error("the secrets directory is refused", "error", err.Error())
		return ExitServiceFailed
	}
	err = s.deliver(ctx, svc, set)
	if err != nil {
		log.Error("secret delivery failed", "error", redactErr(err))
		switch {
		case isResourceRevoked(err):
			return ExitRevoked
		case ctx.Err() != nil:
			return ExitRevoked
		}
		return ExitServiceFailed
	}
	// Nothing more is called: authority is dropped before the service runs, so the
	// service's own process (which shares this one's memory and namespace) holds
	// none: the bearer token is cleared from the client and from the admission
	// response, and the client refuses every later call.
	s.rt.Dispose()
	resp = runtimev2.ResourceAdmitResponse{}
	if ctx.Err() != nil {
		return ExitRevoked
	}
	return s.exec(ctx, argv, svc, set)
}

// openSecretsDir applies the destination rules before any secret is requested: a
// service that has file secrets writes them only into its own secrets directory,
// which must be disjoint (by real path) from the run workspace and from the
// directory holding the admission or checkout credentials, and is opened without
// following a symlink at any component.
func (s *serviceRun) openSecretsDir(svc *runtimev2.ServiceContext, set *serviceSecrets) error {
	files := false
	for _, b := range svc.Secrets {
		files = files || b.Delivery == runtimev1.DeliveryFile
	}
	if !files {
		return nil
	}
	if err := s.cfg.checkSecretsRoot(); err != nil {
		return err
	}
	root, err := openSecretsRoot(s.cfg.SecretsDir)
	if err != nil {
		return err
	}
	set.root = root
	return nil
}

// deliver resolves exactly the committed bindings. A binding the server does not
// echo exactly is a failure; a value is never logged.
func (s *serviceRun) deliver(ctx context.Context, svc *runtimev2.ServiceContext, set *serviceSecrets) error {
	for _, b := range svc.Secrets {
		resp, err := s.rt.ResolveSecret(ctx, runtimev1.SecretResolveRequest{Ref: b.Ref, Version: b.Version})
		if err != nil {
			return err
		}
		if resp.Ref != b.Ref || resp.Version != b.Version || resp.Name != b.Name || resp.Delivery != b.Delivery {
			return fmt.Errorf("the secret response does not match binding %q", b.Name)
		}
		value, err := base64.StdEncoding.DecodeString(resp.ValueBase64)
		if err != nil {
			return fmt.Errorf("secret %q: invalid base64", b.Name)
		}
		set.values = append(set.values, value)
		switch b.Delivery {
		case runtimev1.DeliveryFile:
			if err := set.writeFile(s.cfg.SecretsDir, b.Name, value); err != nil {
				return err
			}
		case runtimev1.DeliveryEnv:
			name := envNameFor(b.Name)
			if _, dup := set.env[name]; dup {
				return fmt.Errorf("secret %q maps to a duplicate environment variable", b.Name)
			}
			set.env[name] = string(value)
		}
	}
	return nil
}

// serviceSecrets is the delivered secret material of one service.
type serviceSecrets struct {
	root   *os.Root
	files  []string
	env    map[string]string
	values [][]byte
}

// errSecretsRoot refuses a destination that is not the service's own private
// directory.
var errSecretsRoot = errors.New("the secrets directory is not private to the service")

// openSecretsRoot opens the secrets directory without following a symlink at any
// component of its path: each component is looked at with lstat, a missing one is
// created (0700), and anything that is not a plain directory refuses the delivery.
// The path must be canonical (an operator configures the real path, so a link
// planted in or beneath it is never silently followed). The returned Root confines
// every later operation to the directory itself.
func openSecretsRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%w: %q is not a clean absolute path", errSecretsRoot, path)
	}
	cur := string(filepath.Separator)
	for _, comp := range strings.Split(strings.TrimPrefix(path, cur), cur) {
		if comp == "" {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(cur, 0o700); err == nil {
				fi, err = os.Lstat(cur)
			}
		}
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w: %v", errSecretsRoot, scrub(err))
		case fi.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("%w: %s is a symlink", errSecretsRoot, cur)
		case !fi.IsDir():
			return nil, fmt.Errorf("%w: %s is not a directory", errSecretsRoot, cur)
		}
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSecretsRoot, scrub(err))
	}
	return root, nil
}

// writeFile delivers one file secret at <root>/<name>: owner-only, created
// exclusively inside the confined root (a leftover path or a symlink of that name
// fails the delivery rather than being followed).
func (set *serviceSecrets) writeFile(rootPath, name string, value []byte) error {
	if filepath.Base(name) != name || strings.HasPrefix(name, ".") {
		return fmt.Errorf("secret name %q is not a plain file name", name)
	}
	if set.root == nil {
		root, err := openSecretsRoot(rootPath)
		if err != nil {
			return err
		}
		set.root = root
	}
	f, err := set.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("secret %q: %w", name, err)
	}
	set.files = append(set.files, name)
	_, werr := f.Write(value)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("secret %q: %w", name, werr)
	}
	return nil
}

func (set *serviceSecrets) cleanup() {
	if set == nil {
		return
	}
	if set.root != nil {
		for _, f := range set.files {
			_ = set.root.Remove(f)
		}
		_ = set.root.Close()
		set.root = nil
	}
	set.files = nil
	for _, v := range set.values {
		clear(v)
	}
	set.env = nil
}

// exec starts the service as a child in its own process group, forwards every
// signal this process receives and returns its exit status (128+n when a signal
// ended it). Its stdout and stderr are the container's own.
func (s *serviceRun) exec(ctx context.Context, argv []string, svc *runtimev2.ServiceContext, set *serviceSecrets) int {
	env := append([]string{}, s.cfg.ChildEnv...)
	env = overlayEnv(env, svc.Command.Env)
	env = overlayEnv(env, set.env)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = s.stdout, s.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		s.log.Error("the service could not be started", "error", scrub(err))
		return ExitServiceFailed
	}
	pid := cmd.Process.Pid
	s.log.Info("service started", "pid", pid)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	// Every other signal reaches the service unchanged (a reload, a dump).
	forward := make(chan os.Signal, 8)
	signal.Notify(forward, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(forward)

	for {
		select {
		case sig := <-forward:
			if ss, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(pid, ss)
			}
		case err := <-waited:
			// Whatever the service left running in its group goes with it.
			killGroup(pid, syscall.SIGKILL)
			return exitStatus(err)
		case <-ctx.Done():
			// The supervisor was told to stop: pass the stop on and give the service its
			// grace; a service that outlives it is killed with its group.
			killGroup(pid, syscall.SIGTERM)
			select {
			case err := <-waited:
				killGroup(pid, syscall.SIGKILL)
				return exitStatus(err)
			case <-time.After(s.cfg.KillGrace):
				killGroup(pid, syscall.SIGKILL)
				return exitStatus(<-waited)
			}
		}
	}
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return ExitServiceFailed
}

// overlayEnv sets each key of kv in env (replacing an existing one).
func overlayEnv(env []string, kv map[string]string) []string {
	for k, v := range kv {
		prefix := k + "="
		env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, prefix) })
		env = append(env, prefix+v)
	}
	return env
}
