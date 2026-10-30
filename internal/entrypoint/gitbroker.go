package entrypoint

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

// The grant broker: how a command obtains a runtime-issued Git grant without the
// credential ever being exposed before the entrypoint knows it.
//
// The entrypoint, not the command, makes the seed and integration route calls
// with the attempt's token. For every answer it registers the credential (and its
// header forms) in the disclosure set first, which is what the log capture, the
// live container-log mirror and the pre-push scan all follow, and only then writes
// the 0600 credential file and keeps the claims. Every credential it ever issued
// stays registered for the whole attempt, so a repeated request that replaces the
// file never lets an earlier value escape redaction or scanning. The command talks
// to it over a Unix socket in a private directory with a per-attempt key, and the
// answer carries no secret.

// grantState is a runtime-issued grant the entrypoint holds: the recorded claims
// and, for a seed, the repository the command created and the directory to push.
type grantState struct {
	Kind          gitv1.GrantKind
	RepositoryID  string
	RepositoryURL string
	Dir           string
	Grant         gitv1.GrantClaims
	CredFile      string
}

type grantBroker struct {
	a      *attempt
	socket string
	key    string
	dir    string
	ln     net.Listener
	wg     sync.WaitGroup
	// ctx is the broker's own lifetime: the command's (it ends at its timeout, its
	// exit, a cancellation or the loss of authority) and cancelled at once by stop.
	// Every request, queued or in flight, and every control-plane call is bound to it.
	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{} // one request at a time; waiting for it is bounded by ctx

	connMu sync.Mutex
	conns  map[net.Conn]struct{}
	once   sync.Once
}

// brokerStopWait bounds how long stop waits for handlers after cancelling them.
const brokerStopWait = 2 * time.Second

// startBroker serves the grant socket when the context advertises a runtime grant
// and the control-plane port can issue one. It runs until stopBroker.
func (a *attempt) startBroker(parent context.Context) *abortError {
	g := a.c3Git()
	if g == nil || (g.Runtime.Seed == nil && g.Runtime.Integration == nil) || a.s.grants == nil {
		return nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return &abortError{ExitError, err}
	}
	base := a.s.cfg.ScratchDir
	if len(base) > 60 { // the socket path must fit sockaddr_un
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "kg-")
	if err != nil {
		return &abortError{ExitError, fmt.Errorf("cannot create the grant socket directory: %w", err)}
	}
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		return &abortError{ExitError, fmt.Errorf("cannot serve the grant socket: %w", err)}
	}
	_ = os.Chmod(sock, 0o600)
	ctx, cancel := context.WithCancel(parent)
	b := &grantBroker{a: a, socket: sock, key: base64.RawURLEncoding.EncodeToString(raw), dir: dir, ln: ln,
		ctx: ctx, cancel: cancel, sem: make(chan struct{}, 1), conns: map[net.Conn]struct{}{}}
	a.s.remember(b.key)
	a.broker = b
	b.wg.Add(1)
	go b.serve()
	return nil
}

// stopBroker ends the broker: it is idempotent, bounded and safe to call on every
// exit path. It cancels the broker's lifetime (so a request in flight, including its
// control-plane call, is cancelled and a request queued behind it issues nothing),
// closes the listener and every accepted connection, waits a short time for the
// handlers and removes the private socket directory. After it returns a credential
// the command obtained is registered, and none can be issued any more.
func (a *attempt) stopBroker() {
	if b := a.broker; b != nil {
		b.stop()
	}
}

func (b *grantBroker) stop() {
	b.once.Do(func() {
		b.cancel()
		_ = b.ln.Close()
		b.connMu.Lock()
		for c := range b.conns {
			_ = c.Close()
		}
		b.connMu.Unlock()
		done := make(chan struct{})
		go func() { b.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(brokerStopWait):
			b.a.s.log.Warn("grant broker handlers did not stop in time")
		}
		_ = os.RemoveAll(b.dir)
	})
}

func (b *grantBroker) track(c net.Conn) bool {
	b.connMu.Lock()
	defer b.connMu.Unlock()
	if b.ctx.Err() != nil {
		return false
	}
	b.conns[c] = struct{}{}
	return true
}

func (b *grantBroker) untrack(c net.Conn) {
	b.connMu.Lock()
	delete(b.conns, c)
	b.connMu.Unlock()
}

func (b *grantBroker) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		if !b.track(conn) {
			_ = conn.Close()
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer b.untrack(conn)
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Minute))
			var req grantRequest
			reply := grantReply{Error: "malformed request"}
			if err := json.NewDecoder(io.LimitReader(conn, 16<<10)).Decode(&req); err == nil {
				reply = b.handle(req)
			}
			_ = json.NewEncoder(conn).Encode(reply)
		}()
	}
}

func (b *grantBroker) handle(req grantRequest) grantReply {
	a := b.a
	if subtle.ConstantTimeCompare([]byte(req.Key), []byte(b.key)) != 1 {
		return grantReply{Error: "the request is not authenticated"}
	}
	// One request at a time. A request that was queued when the command ended gets no
	// grant: the lifetime is checked after the wait, not only before it.
	select {
	case b.sem <- struct{}{}:
		defer func() { <-b.sem }()
	case <-b.ctx.Done():
		return grantReply{Error: "the command has ended; no grant is issued"}
	}
	ctx := b.ctx
	if ctx.Err() != nil {
		return grantReply{Error: "the command has ended; no grant is issued"}
	}
	var st *grantState
	var err error
	switch req.Kind {
	case "seed":
		st, err = b.seed(ctx, req)
	case "integration":
		st, err = b.integration(ctx, req)
	default:
		err = errors.New("unknown grant kind")
	}
	var done *alreadyIntegrated
	if errors.As(err, &done) {
		a.recordIntegration(req.RepositoryID, integrationApplied)
		return grantReply{OK: true, Summary: fmt.Sprintf("integration already-integrated %s: the default branch is at the approved candidate, nothing to push", done.tip)}
	}
	if err != nil {
		if req.Kind == "integration" {
			if publicationConflict(err) {
				a.recordIntegration(req.RepositoryID, integrationRefused)
			}
		}
		var api *APIError
		if errors.As(err, &api) {
			return grantReply{Error: fmt.Sprintf("the control plane refused: HTTP %d %s", api.Status, api.Code)}
		}
		return grantReply{Error: err.Error()}
	}
	if st.Kind == gitv1.GrantIntegration {
		a.recordIntegration(req.RepositoryID, integrationGranted)
	}
	a.grantMu.Lock()
	if a.grants == nil {
		a.grants = map[gitv1.GrantKind]*grantState{}
	}
	a.grants[st.Kind] = st
	a.issued = append(a.issued, st.Grant)
	a.grantMu.Unlock()
	return grantReply{OK: true, Summary: fmt.Sprintf("%s %s %s", st.Kind, st.Grant.GrantID, strings.Join(st.Grant.Repositories[0].Update, ","))}
}

func (b *grantBroker) seed(ctx context.Context, req grantRequest) (*grantState, error) {
	a, g := b.a, b.a.c3Git()
	if g.Runtime.Seed == nil {
		return nil, errors.New("this attempt has no seed capability (only a provisioning run is seeded)")
	}
	if _, err := a.s.cfg.serviceBase(g.BaseURL, req.URL); err != nil {
		return nil, fmt.Errorf("--url: %w", err)
	}
	cfg := a.s.cfg
	abs, err := filepath.Abs(req.Dir)
	if err != nil || !within(cfg.WorkspaceDir, abs) {
		return nil, errors.New("--dir must be inside the run workspace")
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, errors.New("--dir is not a directory")
	}
	resp, err := a.s.grants.SeedGrant(ctx, runtimev3.GitSeedRequest{RepositoryID: req.RepositoryID})
	if err != nil {
		return nil, err
	}
	// The credential is registered the moment it is known, before any check of the
	// answer and before it is written or shown to anyone.
	a.registerGitCredential(resp.Credential)
	if ctx.Err() != nil {
		return nil, errors.New("the command ended while the grant was requested; it was not issued")
	}
	if resp.Grant.Kind != gitv1.GrantSeed || len(resp.Grant.Repositories) != 1 || resp.Grant.Repositories[0].ID != req.RepositoryID || !resp.Grant.Repositories[0].Seed {
		return nil, errors.New("the control plane answered a grant that is not the seed grant of the repository")
	}
	file, err := b.store(g.Runtime.Seed.CredentialFile, resp)
	if err != nil {
		return nil, err
	}
	return &grantState{Kind: gitv1.GrantSeed, RepositoryID: req.RepositoryID, RepositoryURL: req.URL, Dir: abs, Grant: resp.Grant, CredFile: file}, nil
}

func (b *grantBroker) integration(ctx context.Context, req grantRequest) (*grantState, error) {
	a, g := b.a, b.a.c3Git()
	if g.Runtime.Integration == nil {
		return nil, errors.New("this attempt has no integration capability (its source set has no write source)")
	}
	resp, err := a.s.grants.IntegrationGrant(ctx, runtimev3.GitIntegrationRequest{RepositoryID: req.RepositoryID, GateID: req.GateID})
	if err != nil {
		// 409 publication_conflict with reason already_integrated and a well-formed
		// observedTip: the control plane's own statement that the default branch is at
		// the gate's approved candidate. No grant exists and none is needed. Only those
		// two fields are read and nothing of the answer is written anywhere; any other
		// 409 stays the refusal it is.
		if tip, ok := alreadyIntegratedTip(err); ok {
			return nil, &alreadyIntegrated{tip: tip}
		}
		return nil, err
	}
	a.registerGitCredential(resp.Credential)
	if ctx.Err() != nil {
		return nil, errors.New("the command ended while the grant was requested; it was not issued")
	}
	if resp.Grant.Kind != gitv1.GrantIntegration || len(resp.Grant.Repositories) != 1 || resp.Grant.Repositories[0].ID != req.RepositoryID ||
		resp.Grant.Repositories[0].Integration == nil || resp.Grant.Repositories[0].Integration.GateID != req.GateID {
		return nil, errors.New("the control plane answered a grant that is not the integration grant of the gate and repository")
	}
	file, err := b.store(g.Runtime.Integration.CredentialFile, resp)
	if err != nil {
		return nil, err
	}
	return &grantState{Kind: gitv1.GrantIntegration, RepositoryID: req.RepositoryID, Grant: resp.Grant, CredFile: file}, nil
}

// store checks the answer against the advertised file and writes the credential
// (0600, atomically). The value is already registered.
func (b *grantBroker) store(advertised string, resp runtimev3.GitGrantResponse) (string, error) {
	if err := resp.Validate(); err != nil {
		return "", errors.New("the grant response is not valid")
	}
	if resp.CredentialFile != advertised {
		return "", errors.New("the grant response names another credential file than the context advertised")
	}
	file, err := b.a.s.cfg.gitFile(advertised)
	if err != nil {
		return "", err
	}
	if err := writePrivateFile(file, []byte(resp.Credential+"\n")); err != nil {
		return "", errors.New("the credential cannot be written")
	}
	return file, nil
}

// alreadyIntegrated is the integration route's "the default branch is already at the
// approved candidate" answer: success without a grant.
type alreadyIntegrated struct{ tip string }

func (e *alreadyIntegrated) Error() string { return "the approved candidate is already integrated" }

func publicationConflict(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusConflict && api.Code == runtimev1.CodePublicationConflict
}

// alreadyIntegratedTip reports whether err is the already_integrated answer: HTTP 409,
// code publication_conflict, reason already_integrated and a well-formed observedTip,
// which it returns. The control plane is the authority (it answers this only when the
// default branch is at the gate's approved candidate); the command's workspace is not
// consulted.
func alreadyIntegratedTip(err error) (string, bool) {
	var api *APIError
	if !errors.As(err, &api) || !publicationConflict(err) {
		return "", false
	}
	reason, _ := api.Details["reason"].(string)
	tip, _ := api.Details["observedTip"].(string)
	if reason != "already_integrated" || !hexCommit.MatchString(tip) {
		return "", false
	}
	return tip, true
}

// integrationOutcome is what an integration request of a repository told the entrypoint.
type integrationOutcome int

const (
	integrationFailed  integrationOutcome = iota // some other failure (a transient error): nothing learned
	integrationRefused                           // 409 publication_conflict that is not already_integrated
	integrationGranted                           // a grant was issued
	integrationApplied                           // already_integrated: nothing to push
)

// recordIntegration keeps what the integration requests of a repository established.
// A refusal for a moved base (the pipeline recomputes the candidate) is sticky: only a
// grant or the already_integrated answer clears it, never a transient failure. An
// already_integrated answer also supersedes a grant an earlier request of this attempt
// obtained (nothing is pushed for it; its credential stays registered).
func (a *attempt) recordIntegration(repo string, o integrationOutcome) {
	a.grantMu.Lock()
	defer a.grantMu.Unlock()
	if a.integConflict == nil {
		a.integConflict = map[string]bool{}
	}
	switch o {
	case integrationRefused:
		a.integConflict[repo] = true
	case integrationGranted:
		a.integConflict[repo] = false
	case integrationApplied:
		a.integConflict[repo] = false
		if st := a.grants[gitv1.GrantIntegration]; st != nil && st.RepositoryID == repo {
			delete(a.grants, gitv1.GrantIntegration)
		}
	}
}

// integrationConflicted reports whether the latest integration request of any
// repository was refused as a publication conflict.
func (a *attempt) integrationConflicted() bool {
	a.grantMu.Lock()
	defer a.grantMu.Unlock()
	for _, c := range a.integConflict {
		if c {
			return true
		}
	}
	return false
}
