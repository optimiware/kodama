package entrypoint

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

// `kodama prepare` for a v3 run (docs/contracts/runtime-v3.md, "Resource subjects"):
// ONE preparation checks out EVERY source of the run's source set, each at its
// path under /workspace and at its pinned commit, inside one workload and one
// timeout, then records one receipt with a tree ID per source. Every guarantee of
// the v2 preparation holds for every source: no hooks, no submodule recursion, no
// attributes, filters or helpers from the repository, a sanitized Git
// configuration and `fsckObjects`; the only credential is the read-only
// preparation grant, delivered as a file and offered to Git by the scoped
// credential helper (gitcred.go) for the Git service's URLs only. It is never
// written to a remote, URL or configuration, and the repositories are scanned for
// it before the receipt.

type preparationV3 struct {
	cfg ResourceConfig
	rt  resourceRuntimeV3
	log *slog.Logger
	git *git
}

func newPreparationV3(cfg ResourceConfig, rt resourceRuntimeV3) *preparationV3 {
	cfg.applyDefaults()
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	}
	// The same hardened Git as the v2 preparation.
	p := &preparationV3{cfg: cfg, rt: rt, log: log, git: newPreparation(cfg, nil).git}
	return p
}

func (p *preparationV3) run(ctx context.Context) int {
	cfg, log := p.cfg, p.log
	credential, err := readTrimmed(cfg.AdmissionCredentialFile)
	if err != nil {
		log.Error("cannot read admission credential", "error", scrub(err))
		return ExitUsage
	}
	defer p.rt.Dispose()
	resp, err := p.admit(ctx, credential)
	credential = ""
	if err != nil {
		log.Error("admission failed", "error", redactErr(err))
		if isResourceRevoked(err) {
			return ExitRevoked
		}
		return ExitAdmission
	}
	prep := resp.Context.Preparation
	log.Info("admitted", "resource", cfg.Key, "sources", len(prep.Sources), "maxAttempts", prep.MaxAttempts, "timeoutSeconds", prep.TimeoutSeconds)

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		renewResourceLoop(runCtx, cancel, p.rt, resp.Token, time.Second, log)
	}()
	defer func() { cancel(nil); <-renewDone }()

	// The context the server committed is the assignment; a refetch must say the
	// same (a differing context is never acted on).
	fetched, err := p.rt.ResourceContextV3(runCtx)
	if err != nil {
		log.Error("context fetch failed", "error", redactErr(err))
		if isResourceRevoked(err) || errors.Is(context.Cause(runCtx), errAuthorityLost) {
			return ExitRevoked
		}
		return ExitAdmission
	}
	if fetched.Preparation == nil || !reflect.DeepEqual(fetched.Preparation, prep) {
		log.Error("the context returned by admit differs from GET /context; nothing was checked out")
		return ExitAdmission
	}
	cred, credFile, err := p.loadCredential(prep)
	if err != nil {
		log.Error("preparation credential", "error", err.Error())
		return ExitCheckout
	}
	p.git.redactors = cred.forms()
	if prep.Git.CAFile != nil {
		ca, err := mapGitFile(cfg.GitDir, *prep.Git.CAFile)
		if err != nil {
			log.Error("the CA file", "error", err.Error())
			return ExitCheckout
		}
		p.git.caFile = ca
	}

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

	trees, tries, err := p.checkout(tryCtx, prep, credFile)
	if err != nil {
		log.Error("checkout failed", "tries", tries, "error", p.git.redact(err.Error()))
		if errors.Is(context.Cause(runCtx), errAuthorityLost) {
			return ExitRevoked
		}
		return ExitCheckout
	}
	if !p.removeCredential(prep, cred, credFile) {
		log.Error("a credential could not be shown to be absent from the workspace; no receipt")
		return ExitCheckout
	}

	req := runtimev3.PreparationReceiptRequestV3{
		Sources: make([]runtimev3.PreparedSource, len(prep.Sources)), HooksDisabled: true, Submodules: string(runtimev3.SubmodulesSkipped),
		CredentialsRemoved: true, Attempts: int64(tries),
	}
	for i, s := range prep.Sources {
		req.Sources[i] = runtimev3.PreparedSource{Path: s.Path, Commit: s.Commit, TreeID: runtimev3.GitCommit(trees[i])}
	}
	if err := runtimev3.CheckReceipt(fetched, req); err != nil {
		log.Error("the receipt is not valid for the committed assignment", "error", err.Error())
		return ExitCheckout
	}
	recCtx, stopRec := context.WithTimeout(runCtx, 2*time.Minute)
	defer stopRec()
	ack, err := p.rt.CompletePreparationV3(recCtx, req)
	if err != nil {
		log.Error("the receipt was not acknowledged", "error", redactErr(err))
		switch {
		case isResourceRevoked(err) || errors.Is(context.Cause(runCtx), errAuthorityLost):
			return ExitRevoked
		case apiCode(err) == runtimev3.CodeReceiptConflict:
			return ExitCheckout
		}
		return ExitFinalization
	}
	log.Info("preparation complete", "sources", len(prep.Sources), "tries", tries, "receiptDigest", ack.ReceiptDigest)
	return ExitOK
}

// admit is admitResource on the v3 routes: an unbound resource and an unknown
// outcome are retried with the same nonce, every other refusal is final.
func (p *preparationV3) admit(ctx context.Context, credential string) (runtimev3.ResourceAdmitResponseV3, error) {
	cfg := p.cfg
	nonce, err := newNonce()
	if err != nil {
		return runtimev3.ResourceAdmitResponseV3{}, err
	}
	req := runtimev3.ResourceAdmitRequest{AdmissionCredential: credential, Nonce: nonce, ClientVersion: cfg.ClientVersion, Identity: cfg.Identity}
	wait, cancel := context.WithTimeout(ctx, cfg.AdmissionWait)
	defer cancel()
	backoff := 250 * time.Millisecond
	for {
		resp, err := p.rt.AdmitResourceV3(wait, req)
		switch {
		case err == nil:
			c := resp.Context
			if err := resp.Validate(); err != nil {
				return runtimev3.ResourceAdmitResponseV3{}, &ProtocolError{Op: "admitResource", Err: err}
			}
			if c.ResourceKind != runtimev3.ResourcePreparation || c.ResourceKey != cfg.Key || string(c.RunID) != cfg.RunID {
				return runtimev3.ResourceAdmitResponseV3{}, &ProtocolError{Op: "admitResource", Err: errors.New("the committed context is another resource's")}
			}
			return resp, nil
		case isUnbound(err) || isTransport(err):
			p.log.Warn("admission not yet possible; will retry", "error", redactErr(err))
		default:
			return runtimev3.ResourceAdmitResponseV3{}, err
		}
		if err := sleep(wait, backoff); err != nil {
			return runtimev3.ResourceAdmitResponseV3{}, fmt.Errorf("admission did not succeed within %s: %w", cfg.AdmissionWait, err)
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// preparationCredential is the read-only preparation grant's credential: one
// opaque value, the user name being GitUsername.
type preparationCredential struct{ value string }

func (c preparationCredential) forms() []string {
	if c.value == "" {
		return nil
	}
	pair := GitUsername + ":" + c.value
	basic := base64.StdEncoding.EncodeToString([]byte(pair))
	return []string{"Authorization: Basic " + basic, pair, basic, c.value}
}

// loadCredential reads the credential file the context names. A preparation
// always has one: it is the grant that lets it read an internally hosted
// repository.
func (p *preparationV3) loadCredential(prep *runtimev3.PreparationContextV3) (preparationCredential, string, error) {
	file, err := mapGitFile(p.cfg.GitDir, prep.Git.CredentialFile)
	if err != nil {
		return preparationCredential{}, "", err
	}
	v, err := readCredentialFile(file)
	if err != nil {
		return preparationCredential{}, "", err
	}
	return preparationCredential{value: v}, file, nil
}

// checkout makes up to maxAttempts tries over the whole source set, all inside
// tryCtx. Each try starts from an empty workspace: only what an earlier try of this
// workload created is removed. It returns the tree ID of every source, in order.
func (p *preparationV3) checkout(ctx context.Context, prep *runtimev3.PreparationContextV3, credFile string) ([]string, int, error) {
	empty := &preparation{cfg: p.cfg}
	var last error
	for try := 1; try <= int(prep.MaxAttempts); try++ {
		if try > 1 {
			if err := sleep(ctx, time.Duration(try-1)*500*time.Millisecond); err != nil {
				return nil, try - 1, errors.Join(last, err)
			}
		}
		if err := empty.emptyWorkspace(try > 1); err != nil {
			return nil, try, err // not ours to clear: refuse, never repair
		}
		trees, err := p.once(ctx, prep, credFile)
		if err == nil {
			return trees, try, nil
		}
		p.log.Warn("checkout try failed", "try", try, "of", prep.MaxAttempts, "error", p.git.redact(err.Error()))
		last = err
		if ctx.Err() != nil {
			return nil, try, errors.Join(last, ctx.Err())
		}
	}
	return nil, int(prep.MaxAttempts), last
}

// once is one try over every source: init, fetch the pinned commit, detach onto it,
// verify HEAD and compute the tree.
func (p *preparationV3) once(ctx context.Context, prep *runtimev3.PreparationContextV3, credFile string) ([]string, error) {
	trees := make([]string, 0, len(prep.Sources))
	for _, src := range prep.Sources {
		tree, err := p.checkoutSource(ctx, prep, src, credFile)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", src.Path, err)
		}
		trees = append(trees, tree)
	}
	return trees, nil
}

func (p *preparationV3) checkoutSource(ctx context.Context, prep *runtimev3.PreparationContextV3, src runtimev3.PreparationSource, credFile string) (string, error) {
	if err := checkRepositoryURL(src.RepositoryURL); err != nil {
		return "", err
	}
	base, err := Config{}.serviceBase(prep.Git.BaseURL, src.RepositoryURL)
	if err != nil {
		return "", err
	}
	// The credential is offered to this one repository's URL and nowhere else.
	credArgs, err := credentialConfig(p.cfg.GitHelper, base, src.RepositoryURL, credFile)
	if err != nil {
		return "", err
	}
	dir := Config{WorkspaceDir: p.cfg.WorkspaceDir}.sourceDir(src.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create the source directory: %v", scrub(err))
	}
	commit := string(src.Commit)
	initArgs := []string{"init", "--quiet", "--template="}
	if len(commit) == 64 {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	if _, err := p.git.run(ctx, dir, nil, initArgs...); err != nil {
		return "", err
	}
	fg := *p.git
	fg.cfg = append(append([]string{}, p.git.cfg...), credArgs...)
	// The URL is named on the command line, not stored as a remote: the repository's
	// configuration keeps nothing about where it came from.
	if _, err := fg.run(ctx, dir, nil, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", src.RepositoryURL, commit); err != nil {
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

// removeCredential leaves no credential material in any source and reports whether
// that was shown: no repository configuration carries a credential header, helper
// or rewrite, and none of the credential's forms is in any file under any .git. The
// credential file is removed when removable (a read-only Secret mount is not, and
// is not the workspace).
func (p *preparationV3) removeCredential(prep *runtimev3.PreparationContextV3, cred preparationCredential, credFile string) bool {
	ok := true
	forms := cred.forms()
	for _, src := range prep.Sources {
		gitDir := filepath.Join(Config{WorkspaceDir: p.cfg.WorkspaceDir}.sourceDir(src.Path), ".git")
		cfgData, _ := os.ReadFile(filepath.Join(gitDir, "config"))
		lc := strings.ToLower(string(cfgData))
		for _, bad := range []string{"extraheader", "[credential", "helper", "[url ", "insteadof", "authorization", "[remote"} {
			if strings.Contains(lc, bad) {
				p.log.Error("a repository configuration carries credential, remote or rewrite settings", "source", src.Path, "setting", bad)
				ok = false
			}
		}
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
					p.log.Error("the credential reached a repository file", "source", src.Path, "file", rel)
					ok = false
					return nil
				}
			}
			return nil
		})
	}
	if err := os.Remove(credFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.log.Info("the credential file is on a read-only mount and goes away with the workload")
	}
	return ok
}
