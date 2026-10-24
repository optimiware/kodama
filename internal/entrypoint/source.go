package entrypoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

// maxSettleBackoff caps the delay between re-proposals of a pending publication.
const maxSettleBackoff = 5 * time.Second

// publish creates the source commit of a successful writer and has the control
// plane publish it. It returns the commit to report as sourceCommit, or a
// failed verdict, or an abort.
//
// Commit rules: the workspace HEAD is the candidate. If non-ignored changes
// (including untracked files) exist they are committed on top of HEAD with the
// fixed identity "Kodama <step@kodama.invalid>", so commits made by the command
// itself are kept and uncommitted work is added after them. The candidate must
// descend from the pinned commit. If it equals the expected tip nothing changed
// and nothing is proposed; the pinned commit is reported. Otherwise a bundle of
// pinned..candidate is uploaded as a bundle artifact and proposed with the
// context's expected tip. The proposal is repeated, identical, until accepted;
// a pending publication must settle.
func (a *attempt) publish() (runtimev1.GitCommit, *verdict, *abortError) {
	cfg, src := a.s.cfg, a.c.Workspace.Source
	dir, ctx := cfg.WorkspaceDir, a.finalCtx()
	reject := func(reason, format string, args ...any) (runtimev1.GitCommit, *verdict, *abortError) {
		a.note(format, args...)
		v := failed(reason, nil)
		return "", &v, nil
	}
	pinned, tip := string(src.Commit), string(*src.ExpectedTip)

	candidate, err := a.s.git.head(ctx, dir)
	if err != nil {
		if ab := a.classify(ctx, err); ab != nil {
			return "", nil, ab
		}
		return reject(ReasonSourceFailed, "cannot read workspace HEAD: %v", err)
	}
	dirty, err := a.s.git.dirty(ctx, dir)
	if err != nil {
		return reject(ReasonSourceFailed, "cannot inspect workspace: %v", err)
	}
	if dirty {
		msg := fmt.Sprintf("kodama: %s (attempt %s)", a.c.StepID, a.c.AttemptID)
		if candidate, err = a.s.git.commitAll(ctx, dir, msg); err != nil {
			return reject(ReasonSourceFailed, "cannot commit workspace changes: %v", err)
		}
	}
	if candidate != pinned {
		ok, err := a.s.git.isAncestor(ctx, dir, pinned, candidate)
		if err != nil || !ok {
			return reject(ReasonSourceFailed, "the resulting commit %s does not descend from the pinned commit %s", candidate, pinned)
		}
		if path, err := a.s.git.scanForSecrets(ctx, dir, pinned, candidate, a.secretNeedles()); err != nil {
			return reject(ReasonSourceFailed, "cannot scan source for injected secrets: %v", err)
		} else if path != "" {
			return reject(ReasonSecretInSource, "path %q in the source change contains an injected secret value; nothing was published", path)
		}
	}
	if candidate == tip {
		return runtimev1.GitCommit(candidate), nil, nil
	}
	if candidate == pinned {
		return reject(ReasonSourceFailed, "the workspace is unchanged but the pinned commit %s is not the expected tip %s", pinned, tip)
	}

	tmp, err := os.MkdirTemp(cfg.ScratchDir, "kodama-bundle-")
	if err != nil {
		return reject(ReasonSourceFailed, "cannot create bundle directory: %v", err)
	}
	defer os.RemoveAll(tmp)
	bundle := filepath.Join(tmp, "source.bundle")
	if err := a.s.git.bundle(ctx, dir, pinned, candidate, bundle); err != nil {
		return reject(ReasonSourceFailed, "cannot create bundle: %v", err)
	}
	f, err := os.Open(bundle)
	if err != nil {
		return reject(ReasonSourceFailed, "cannot read bundle: %v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return reject(ReasonSourceFailed, "cannot read bundle: %v", err)
	}
	if fi.Size() > a.c.Limits.MaxOutputBytes {
		return reject(ReasonBundleTooLarge, "the source bundle is %d bytes, over the %d byte limit", fi.Size(), a.c.Limits.MaxOutputBytes)
	}
	digest, err := hashFile(f, fi.Size())
	if err != nil {
		return reject(ReasonSourceFailed, "cannot hash bundle: %v", err)
	}
	mt, sz := "application/x-git-bundle", fi.Size()
	id, err := a.uploadFile(ctx, f, runtimev1.ArtifactReserveRequest{
		Name: "source.bundle", Kind: runtimev1.KindFile, Role: runtimev1.RoleBundle, Digest: digest, MediaType: &mt, SizeBytes: &sz,
	})
	if err != nil {
		if ab := a.classify(ctx, err); ab != nil {
			return "", nil, ab
		}
		return reject(ReasonPublicationFailed, "bundle upload failed: %v", redactErr(err))
	}

	req := runtimev1.SourceProposalRequest{Commit: runtimev1.GitCommit(candidate), ExpectedTip: *src.ExpectedTip, BundleArtifactID: id}
	return a.settle(ctx, req)
}

// settle repeats the identical proposal until the publication is accepted.
// Pending responses and transport failures (unknown outcome) are retried with
// bounded backoff; a lost expected-tip race is a failed result, never retried.
func (a *attempt) settle(ctx context.Context, req runtimev1.SourceProposalRequest) (runtimev1.GitCommit, *verdict, *abortError) {
	cfg := a.s.cfg
	sctx, cancel := context.WithTimeout(ctx, cfg.SettleTimeout)
	defer cancel()
	reject := func(reason, format string, args ...any) (runtimev1.GitCommit, *verdict, *abortError) {
		a.note(format, args...)
		v := failed(reason, nil)
		return "", &v, nil
	}
	backoff := cfg.SettleBackoff
	for {
		resp, err := a.s.rt.ProposeSource(sctx, req)
		switch {
		case err == nil && resp.Commit != req.Commit:
			return reject(ReasonPublicationFailed, "the publisher answered for commit %s, not %s", resp.Commit, req.Commit)
		case err == nil && resp.Status == runtimev1.PublishAccepted:
			a.s.log.Info("source accepted", "commit", resp.Commit)
			return req.Commit, nil, nil
		case err == nil:
			a.s.log.Info("source publication pending; will re-propose", "commit", req.Commit)
		case isRevoked(err) || errors.Is(context.Cause(a.runCtx), errAuthorityLost):
			return "", nil, &abortError{ExitRevoked, err}
		case apiCode(err) == runtimev1.CodePublicationConflict:
			var tip any
			var api *APIError
			if errors.As(err, &api) {
				tip = api.Details["observedTip"]
			}
			return reject(ReasonPublicationConflict, "the branch moved: expected tip %s, observed tip %v; the proposal was not published and is not retried", req.ExpectedTip, tip)
		case isTransport(err) || sctx.Err() != nil:
			a.s.log.Warn("source proposal outcome unknown; will re-propose", "error", redactErr(err))
		default:
			return reject(ReasonPublicationFailed, "the source proposal was rejected: %v", err)
		}
		if err := sleep(sctx, backoff); err != nil {
			if ctx.Err() != nil {
				return "", nil, &abortError{ExitFinalization, ctx.Err()}
			}
			return reject(ReasonPublicationUnsettled, "the publication did not settle within %s; it may still complete remotely", cfg.SettleTimeout)
		}
		backoff = min(backoff*2, maxSettleBackoff)
	}
}
