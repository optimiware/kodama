package gitservice

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The hooks are subprocesses of Git and must reach the control plane, which only
// the worker is allowed to call. They talk to the worker over a loopback
// listener, authenticated by a random per-receive token that only that
// receive's process tree has in its environment; the token is void when the
// receive ends. The worker forwards to the ControlPlane port under its own
// identity, so a hook never holds a control-plane credential.

func (s *Service) startHookServer() error {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.hookLn, s.hookAddr = ln, ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /authorize", s.hookHandler(s.hookAuthorize))
	mux.HandleFunc("POST /commit-intent", s.hookHandler(s.hookCommitIntent))
	mux.HandleFunc("POST /outcome", s.hookHandler(s.hookOutcome))
	s.hookSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.hookSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.cfg.Logger.Error("git worker hook listener stopped", "error", err)
		}
	}()
	return nil
}

type hookFunc func(ctx context.Context, rc *receive, body []byte) (any, error)

// hookHandler authenticates the per-receive token, bounds the body and writes
// the contract error shape on failure.
func (s *Service) hookHandler(fn hookFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		rc, found := s.receiveByToken(token)
		if !ok || !found || subtle.ConstantTimeCompare([]byte(rc.token), []byte(token)) != 1 {
			writeContractError(w, contractError(domain.CodeInvalidCredential, "unknown receive"), "")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
		if err != nil {
			writeContractError(w, contractError(domain.CodeInvalidRequest, "the request body is unreadable or too large"), rc.id)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Limits.ControlTimeout+5*time.Second)
		defer cancel()
		out, err := fn(ctx, rc, body)
		if err != nil {
			writeContractError(w, s.controlError(err), rc.id)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// controlError maps a control-plane failure to the contract error the hook
// reports: a contract error keeps its code, anything else is unavailable (the
// push is refused, nothing changes).
func (s *Service) controlError(err error) *gitv1.ContractError {
	if ce, ok := asContract(err); ok {
		return ce
	}
	s.cfg.Logger.Warn("control plane call failed", "error", err)
	return contractError(domain.CodeUnavailable, "the control plane could not be reached")
}

func decodeHook[T any](body []byte) (T, error) {
	var v T
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, contractError(domain.CodeInvalidRequest, "malformed hook request")
	}
	return v, nil
}

// hookAuthorize is pre-receive's call: the worker adds the receive's identity
// (repository, grant, receive ID) itself, so a hook cannot name another one.
func (s *Service) hookAuthorize(ctx context.Context, rc *receive, body []byte) (any, error) {
	in, err := decodeHook[hookAuthorizeRequest](body)
	if err != nil {
		return nil, err
	}
	if in.Updates == nil || in.PushOptions == nil || in.GitFacts == nil {
		return nil, contractError(domain.CodeInvalidRequest, "malformed hook request")
	}
	if err := gitv1.CheckUpdateBound(len(in.Updates), s.cfg.Limits.MaxRefUpdates); err != nil {
		return nil, err
	}
	req := gitv1.PushAuthorizeRequest{
		RepositoryID: rc.repositoryID, GrantID: rc.grantID, ReceiveID: rc.id,
		Updates: in.Updates, PushOptions: in.PushOptions, GitFacts: in.GitFacts,
	}
	if err := req.Validate(); err != nil {
		return nil, contractError(domain.CodeInvalidRequest, "%v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.ControlTimeout)
	defer cancel()
	resp, err := s.cfg.ControlPlane.Authorize(cctx, req)
	if err != nil {
		return nil, err
	}
	if err := resp.Validate(); err != nil {
		return nil, contractError(domain.CodeUnavailable, "the control plane's answer is invalid")
	}
	if err := gitv1.CheckAuthorizeResponse(req, resp); err != nil {
		return nil, contractError(domain.CodeUnavailable, "the control plane's answer does not match the request")
	}
	deadline, _ := resp.Deadline.Time()
	now := s.now()
	if max := now.Add(gitv1.MaxReceiveDeadlineLifetime); deadline.After(max) {
		deadline = max
	}
	if !deadline.After(now) {
		return nil, contractError(gitv1.CodeReceiveExpired, "the receive deadline has passed")
	}
	rc.arm(resp.PushID, in.Updates, deadline)
	return resp, nil
}

// hookCommitIntent is the prepared hook's call, made with the refs locked and
// the prepared journal entry durable.
func (s *Service) hookCommitIntent(ctx context.Context, rc *receive, body []byte) (any, error) {
	in, err := decodeHook[hookCommitRequest](body)
	if err != nil {
		return nil, err
	}
	pushID, deadline, authorized := rc.snapshot()
	if pushID == "" {
		return nil, contractError(domain.CodeForbidden, "the receive was not authorized")
	}
	if !s.now().Before(deadline) {
		return nil, contractError(gitv1.CodeReceiveExpired, "the receive deadline has passed")
	}
	req := gitv1.CommitIntentRequest{ReceiveID: rc.id, Updates: in.Updates}
	if err := req.Validate(); err != nil {
		return nil, contractError(domain.CodeInvalidRequest, "%v", err)
	}
	for _, u := range in.Updates {
		if a, ok := authorized[u.Ref]; !ok || a != u {
			return nil, contractError(domain.CodeForbidden, "ref %s was not authorized with this update", u.Ref)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.ControlTimeout)
	defer cancel()
	resp, err := s.cfg.ControlPlane.CommitIntent(cctx, pushID, req)
	if err != nil {
		return nil, err
	}
	if resp.Validate() != nil {
		return nil, contractError(domain.CodeUnavailable, "the control plane did not grant commit intent")
	}
	return resp, nil
}

// hookOutcome is post-receive's call. The report is forwarded once
// synchronously; a failure is retried in the background with the same body so
// the pusher is not held, and reconciliation settles from the journal if it
// never arrives.
func (s *Service) hookOutcome(ctx context.Context, rc *receive, body []byte) (any, error) {
	in, err := decodeHook[hookOutcomeRequest](body)
	if err != nil {
		return nil, err
	}
	pushID, _, _ := rc.snapshot()
	if pushID == "" {
		return nil, contractError(domain.CodeForbidden, "the receive was not authorized")
	}
	req := gitv1.OutcomeRequest{ReceiveID: rc.id, Applied: in.Applied}
	// The journal sequence rides along only when nothing else in the journal
	// window is unreported (reconcile.go); otherwise the sweep acknowledges later.
	req.JournalSequence = s.ackSequence(rc)
	req.JournalPosition = receivePosition(rc.dir, rc.id)
	if err := req.Validate(); err != nil {
		return nil, contractError(domain.CodeInvalidRequest, "%v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.ControlTimeout)
	defer cancel()
	resp := hookOutcomeResponse{}
	if _, err := s.cfg.ControlPlane.Outcome(cctx, pushID, req); err != nil {
		if ce, ok := asContract(err); ok && ce.Code != domain.CodeUnavailable {
			// A refusal is final: retrying the same body cannot help. The push
			// record exists all the same (the control plane reconciles it).
			resp.Warning = ce.Message
			s.markReported(rc.repositoryID, rc.id)
		} else {
			s.retryOutcome(rc.repositoryID, pushID, req)
		}
	} else {
		s.markReported(rc.repositoryID, rc.id)
		if req.JournalSequence != nil {
			s.ackAdvanced(rc.repositoryID, *req.JournalSequence)
		}
	}
	// The push ID is handed to the hook for the client only for a receive that
	// applied something, and only once.
	if len(in.Applied) > 0 && rc.takePushIDForClient() {
		resp.PushID = pushID
	}
	return resp, nil
}

// Retry schedule of an unconfirmed outcome.
const (
	outcomeRetryFirst  = time.Second
	outcomeRetryMax    = time.Minute
	outcomeRetryWindow = 30 * time.Minute
)

func (s *Service) retryOutcome(repositoryID, pushID string, req gitv1.OutcomeRequest) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		delay, stop := outcomeRetryFirst, time.NewTimer(outcomeRetryWindow)
		defer stop.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-stop.C:
				s.cfg.Logger.Warn("outcome never confirmed; reconciliation settles it", "pushId", pushID, "receiveId", req.ReceiveID)
				return
			case <-time.After(delay):
			}
			ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Limits.ControlTimeout)
			_, err := s.cfg.ControlPlane.Outcome(ctx, pushID, req)
			cancel()
			if err == nil {
				s.markReported(repositoryID, req.ReceiveID)
				if req.JournalSequence != nil {
					s.ackAdvanced(repositoryID, *req.JournalSequence)
				}
				return
			}
			if ce, ok := asContract(err); ok && ce.Code != domain.CodeUnavailable {
				s.markReported(repositoryID, req.ReceiveID)
				return
			}
			if delay *= 2; delay > outcomeRetryMax {
				delay = outcomeRetryMax
			}
		}
	}()
}
