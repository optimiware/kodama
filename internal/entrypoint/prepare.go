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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// `kodama prepare` is the controller-selected, one-shot checkout of a run_shared
// run (docs/contracts/runtime-v2.md "Resource subjects"; ADR 0010). It runs in the
// policy's digest-pinned preparation image with the read-only checkout credential
// and no user code, once, before the first step:
//
//  1. admit as the run's `preparation` resource (token of the resource audience);
//  2. check out the pinned commit into the empty run workspace with every Git
//     feature that could run something disabled, retrying only INSIDE this one
//     workload (the policy's maxAttempts tries under one timeout);
//  3. verify HEAD is the pinned commit, compute the tree ID, make sure no
//     credential or credential configuration is left anywhere in the workspace;
//  4. record the receipt, the only thing the preparation may ever call besides its
//     own token renewal and context.
//
// There is no per-step clone afterwards: the workspace's readiness is the recorded
// receipt plus this workload being observed stopped.

// PrepareCommandName and ServiceRunCommandName are the subcommands the composition
// root registers.
const (
	PrepareCommandName    = "prepare"
	ServiceRunCommandName = "service-run"
)

// PrepareCommand returns the `kodama prepare` registration. The composition root
// (T-116) registers it beside StepCommand().
func PrepareCommand() Command {
	return Command{Name: PrepareCommandName, Summary: "Check out the run's pinned commit once into the shared workspace (run_shared preparation)", Run: RunPrepare}
}

// RunPrepare parses args (after "prepare") and the environment, runs the
// preparation and returns its exit code.
func RunPrepare(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	var output bytes.Buffer
	cfg, rest, err := parseResourceConfig(PrepareCommandName, args, getenv, &output)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = stdout.Write(output.Bytes())
			return ExitOK
		}
		if output.Len() > 0 {
			_, _ = stderr.Write(output.Bytes())
		}
		fmt.Fprintln(stderr, "kodama prepare:", err)
		return ExitUsage
	}
	if len(rest) != 0 {
		fmt.Fprintln(stderr, "kodama prepare: accepts flags only, not positional arguments")
		return ExitUsage
	}
	cfg.Logger = slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return ExecutePrepare(ctx, cfg)
}

// ExecutePrepare runs the preparation with the real HTTP adapter.
func ExecutePrepare(ctx context.Context, cfg ResourceConfig) int {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil || cfg.Key != string(runtimev2.ResourcePreparation) {
		if cfg.Logger != nil {
			cfg.Logger.Error("invalid configuration", "error", fmt.Sprint(err))
		}
		return ExitUsage
	}
	if cfg.RuntimeAPI == RuntimeAPIv3 {
		client, err := NewResourceClientV3(cfg.ServerURL, domain.RunID(cfg.RunID), cfg.Key, cfg.CAFile, cfg.Retry, cfg.Logger)
		if err != nil {
			if cfg.Logger != nil {
				cfg.Logger.Error("cannot create runtime client", "error", err.Error())
			}
			return ExitUsage
		}
		return newPreparationV3(cfg, client).run(ctx)
	}
	client, err := NewResourceClient(cfg.ServerURL, domain.RunID(cfg.RunID), cfg.Key, cfg.CAFile, cfg.Retry, cfg.Logger)
	if err != nil {
		if cfg.Logger != nil {
			cfg.Logger.Error("cannot create runtime client", "error", err.Error())
		}
		return ExitUsage
	}
	return newPreparation(cfg, client).run(ctx)
}

type preparation struct {
	cfg ResourceConfig
	rt  resourceRuntime
	log *slog.Logger
	git *git
}

func newPreparation(cfg ResourceConfig, rt resourceRuntime) *preparation {
	cfg.applyDefaults()
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	}
	p := &preparation{cfg: cfg, rt: rt, log: log}
	p.git = &git{bin: cfg.GitBinary, caFile: cfg.GitCAFile, home: cfg.ScratchDir, pathEnv: os.Getenv("PATH"), group: true}
	// Git's own hardening on top of the shared base: nothing from a repository's
	// content or configuration may run, and nothing leaves over a weaker protocol.
	p.git.cfg = []string{
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"-c", "core.attributesFile=/dev/null", "-c", "core.excludesFile=/dev/null",
		"-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true",
		"-c", "core.protectNTFS=true", "-c", "core.protectHFS=true", "-c", "core.symlinks=true",
		"-c", "maintenance.auto=false", "-c", "advice.detachedHead=false",
	}
	p.git.seal = []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0"}
	return p
}

func (p *preparation) run(ctx context.Context) int {
	cfg, log := p.cfg, p.log
	credential, err := readTrimmed(cfg.AdmissionCredentialFile)
	if err != nil {
		log.Error("cannot read admission credential", "error", scrub(err))
		return ExitUsage
	}
	defer p.rt.Dispose()
	resp, err := admitResource(ctx, p.rt, cfg, credential, log, runtimev2.ResourcePreparation)
	credential = ""
	if err != nil {
		log.Error("admission failed", "error", redactErr(err))
		if isResourceRevoked(err) {
			return ExitRevoked
		}
		return ExitAdmission
	}
	prep := resp.Context.Preparation
	log.Info("admitted", "resource", cfg.Key, "commit", prep.Commit, "maxAttempts", prep.MaxAttempts, "timeoutSeconds", prep.TimeoutSeconds)

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		renewResourceLoop(runCtx, cancel, p.rt, resp.Token, time.Second, log)
	}()
	defer func() { cancel(nil); <-renewDone }()

	// The context the server committed is the assignment; a refetch must say the same
	// (a differing context is never acted on).
	fetched, err := p.rt.ResourceContext(runCtx)
	if err != nil {
		log.Error("context fetch failed", "error", redactErr(err))
		if isResourceRevoked(err) || errors.Is(context.Cause(runCtx), errAuthorityLost) {
			return ExitRevoked
		}
		return ExitAdmission
	}
	if fetched.Preparation == nil || *fetched.Preparation != *prep {
		log.Error("the context returned by admit differs from GET /context; nothing was checked out")
		return ExitAdmission
	}
	if err := checkRepositoryURL(prep.RepositoryURL); err != nil {
		log.Error("refusing the repository", "error", err.Error())
		return ExitAdmission
	}

	cred, err := p.loadCredential()
	if err != nil {
		log.Error("checkout credential", "error", err.Error())
		return ExitCheckout
	}
	if cred != nil {
		p.git.redactors = cred.forms()
	}

	// One timeout covers every try, and the committed deadline ends them regardless.
	deadline, _ := prep.DeadlineAt.Time()
	budget := time.Duration(prep.TimeoutSeconds) * time.Second
	if until := time.Until(deadline); until < budget {
		budget = until
	}
	if budget <= 0 {
		log.Error("the preparation deadline has passed")
		return ExitCheckout
	}
	tryCtx, stopTries := context.WithTimeout(runCtx, budget)
	defer stopTries()

	treeID, tries, err := p.checkout(tryCtx, prep, cred)
	if err != nil {
		log.Error("checkout failed", "tries", tries, "error", p.git.redact(err.Error()))
		if errors.Is(context.Cause(runCtx), errAuthorityLost) {
			return ExitRevoked
		}
		return ExitCheckout
	}
	removed := p.removeCredential(cred)
	if !removed {
		log.Error("a credential could not be shown to be absent from the workspace; no receipt")
		return ExitCheckout
	}

	req := runtimev2.PreparationReceiptRequest{
		Commit: prep.Commit, TreeID: runtimev2.GitCommit(treeID), HooksDisabled: true, Submodules: runtimev2.SubmodulesSkipped,
		CredentialsRemoved: true, Attempts: int64(tries),
	}
	if err := runtimev2.CheckReceipt(*prep2ctx(prep, resp), req); err != nil {
		log.Error("the receipt is not valid for the committed assignment", "error", err.Error())
		return ExitCheckout
	}
	// Recording is idempotent: after an unknown outcome the identical repeat returns
	// the original, so it is retried within its own bound.
	recCtx, stopRec := context.WithTimeout(runCtx, 2*time.Minute)
	defer stopRec()
	ack, err := p.rt.CompletePreparation(recCtx, req)
	if err != nil {
		log.Error("the receipt was not acknowledged", "error", redactErr(err))
		switch {
		case isResourceRevoked(err) || errors.Is(context.Cause(runCtx), errAuthorityLost):
			return ExitRevoked
		case apiCode(err) == runtimev2.CodeReceiptConflict:
			return ExitCheckout
		}
		return ExitFinalization
	}
	log.Info("preparation complete", "commit", prep.Commit, "treeId", treeID, "tries", tries, "receiptDigest", ack.ReceiptDigest)
	return ExitOK
}

// prep2ctx returns the committed context for CheckReceipt.
func prep2ctx(prep *runtimev2.PreparationContext, resp runtimev2.ResourceAdmitResponse) *runtimev2.ResourceContext {
	c := resp.Context
	c.Preparation = prep
	return &c
}

// checkRepositoryURL admits only an absolute https URL without credentials: the
// repository host is where the read-only credential may go, and nowhere else.
func checkRepositoryURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "":
		return errors.New("the repository URL must be an absolute https URL")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return errors.New("the repository URL must not carry credentials, a query or a fragment")
	}
	return nil
}

func (p *preparation) loadCredential() (*checkoutCredential, error) {
	if p.cfg.CheckoutCredentialFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(p.cfg.CheckoutCredentialFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil // an anonymous repository
	case err != nil:
		return nil, errors.New("the checkout credential cannot be read")
	}
	cred, err := parseCheckoutCredential(data)
	if err != nil {
		return nil, err
	}
	return cred, nil
}

// forms lists every representation of the credential an error message or a stored
// file could carry.
func (c *checkoutCredential) forms() []string {
	pair := c.username + ":" + c.password
	return []string{c.header(), pair, base64.StdEncoding.EncodeToString([]byte(pair)), c.password}
}

// checkout makes up to maxAttempts tries, all inside tryCtx. Each try starts from
// an empty workspace: whatever an earlier try left (the preparation's own partial
// tree) is removed, never anything it did not create. It returns the tree ID, the
// number of tries used and the last failure.
func (p *preparation) checkout(ctx context.Context, prep *runtimev2.PreparationContext, cred *checkoutCredential) (string, int, error) {
	var last error
	for try := 1; try <= int(prep.MaxAttempts); try++ {
		if try > 1 {
			if err := sleep(ctx, time.Duration(try-1)*500*time.Millisecond); err != nil {
				return "", try - 1, errors.Join(last, err)
			}
		}
		if err := p.emptyWorkspace(try > 1); err != nil {
			return "", try, err // not ours to clear: refuse, never repair
		}
		treeID, err := p.once(ctx, prep, cred)
		if err == nil {
			return treeID, try, nil
		}
		p.log.Warn("checkout try failed", "try", try, "of", prep.MaxAttempts, "error", p.git.redact(err.Error()))
		last = err
		if ctx.Err() != nil {
			return "", try, errors.Join(last, ctx.Err())
		}
	}
	return "", int(prep.MaxAttempts), last
}

// emptyWorkspace requires the run workspace to hold nothing but what a fresh volume
// holds. On a retry it first removes the partial tree its own earlier try created
// (the directory's children, never the mount itself).
func (p *preparation) emptyWorkspace(retry bool) error {
	dir := p.cfg.WorkspaceDir
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("the run workspace %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("the run workspace cannot be read: %v", scrub(err))
	}
	for _, e := range entries {
		if !retry {
			// A fresh volume may carry the filesystem's own lost+found and nothing else.
			if e.Name() == "lost+found" {
				continue
			}
			return fmt.Errorf("the run workspace is not empty (%s): a preparation never runs over existing content", e.Name())
		}
		if e.Name() == "lost+found" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("the partial tree of an earlier try cannot be removed: %v", scrub(err))
		}
	}
	return nil
}

// once is one try: init, fetch the pinned commit, detach onto it, verify. The
// credential travels only as an extra HTTP header in the environment of the one
// fetch; it is never written to the repository's configuration, a remote, a URL or
// the command line.
func (p *preparation) once(ctx context.Context, prep *runtimev2.PreparationContext, cred *checkoutCredential) (string, error) {
	dir, commit := p.cfg.WorkspaceDir, string(prep.Commit)
	initArgs := []string{"init", "--quiet", "--template="}
	if len(commit) == 64 {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	if _, err := p.git.run(ctx, dir, nil, initArgs...); err != nil {
		return "", err
	}
	var extra []string
	if cred != nil {
		u, err := url.Parse(prep.RepositoryURL)
		if err != nil {
			return "", err
		}
		origin := u.Scheme + "://" + u.Host
		extra = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http." + origin + ".extraHeader", "GIT_CONFIG_VALUE_0=" + cred.header()}
	}
	// The URL is named on the command line, not stored as a remote: the repository's
	// configuration keeps nothing about where it came from.
	if _, err := p.git.run(ctx, dir, extra, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", prep.RepositoryURL, commit); err != nil {
		return "", err
	}
	if _, err := p.git.run(ctx, dir, nil, "checkout", "--quiet", "--detach", "--no-recurse-submodules", commit); err != nil {
		return "", err
	}
	head, err := p.git.head(ctx, dir)
	if err != nil {
		return "", err
	}
	if head != commit {
		return "", fmt.Errorf("checked out %s, expected %s", head, commit)
	}
	out, err := p.git.run(ctx, dir, nil, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(out))
	if !hexCommit.MatchString(tree) {
		return "", errors.New("the tree ID is not an object name")
	}
	return tree, nil
}

// removeCredential leaves no credential material the workspace could carry and
// reports whether that was shown: the repository holds no credential header or
// helper configuration and none of the credential's forms in any file under .git
// (configuration, FETCH_HEAD, packed refs, hooks); the credential file itself is
// removed when it is removable (a read-only Secret mount is not, and is not the
// workspace). The in-memory copy is dropped.
func (p *preparation) removeCredential(cred *checkoutCredential) bool {
	ok := true
	gitDir := filepath.Join(p.cfg.WorkspaceDir, ".git")
	cfgData, _ := os.ReadFile(filepath.Join(gitDir, "config"))
	lc := strings.ToLower(string(cfgData))
	for _, bad := range []string{"extraheader", "[credential", "helper", "[url ", "insteadof", "authorization"} {
		if strings.Contains(lc, bad) {
			p.log.Error("the repository configuration carries credential or rewrite settings", "setting", bad)
			ok = false
		}
	}
	if cred != nil {
		forms := cred.forms()
		_ = filepath.WalkDir(gitDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, ierr := d.Info(); ierr != nil || info.Size() > 8<<20 || !info.Mode().IsRegular() {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, f := range forms {
				if f != "" && bytes.Contains(data, []byte(f)) {
					rel, _ := filepath.Rel(gitDir, path)
					p.log.Error("the credential reached a repository file", "file", rel)
					ok = false
					return nil
				}
			}
			return nil
		})
		if p.cfg.CheckoutCredentialFile != "" {
			if err := os.Remove(p.cfg.CheckoutCredentialFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				p.log.Info("the credential file is on a read-only mount and goes away with the workload")
			}
		}
		cred.password, cred.username = "", ""
	}
	return ok
}
