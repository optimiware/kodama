package gitservice

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Smart-HTTP endpoints, the only paths the Git handler serves:
//
//	GET  /<repositoryId>.git/info/refs?service=git-upload-pack|git-receive-pack
//	POST /<repositoryId>.git/git-upload-pack
//	POST /<repositoryId>.git/git-receive-pack
//
// Everything else (dumb HTTP, upload-archive, any other path or method) is 404.
// The control plane forwards an authorized request here with a forwarding
// assertion in AssertionHeader; the worker verifies it, takes the repository
// from the assertion (the path must name the same ID), and derives the on-disk
// path from that ID alone.
const (
	endpointInfoRefs   = "info/refs"
	endpointUploadPack = "git-upload-pack"
	endpointReceive    = "git-receive-pack"

	serviceUploadPack = "git-upload-pack"
	serviceReceive    = "git-receive-pack"
)

// protocolHeaderPattern is the Git-Protocol values passed through: protocol
// version 0, 1 or 2 with optional key=value capabilities.
var protocolHeaderPattern = regexp.MustCompile(`^version=[0-2](:[a-z0-9-]+=[A-Za-z0-9._-]+)*$`)

// GitHandler serves the smart-HTTP endpoints around `git http-backend`.
func (s *Service) GitHandler() http.Handler { return http.HandlerFunc(s.serveGit) }

type gitRoute struct {
	repositoryID string
	endpoint     string
	service      string
}

// parseGitRoute accepts exactly the three endpoints. The repository segment is
// a valid opaque ID plus ".git", so no separator, dot or traversal can appear.
func parseGitRoute(r *http.Request) (gitRoute, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/")
	if !ok {
		return gitRoute{}, false
	}
	idPart, endpoint, ok := strings.Cut(rest, "/")
	if !ok {
		return gitRoute{}, false
	}
	id, ok := strings.CutSuffix(idPart, ".git")
	if !ok || !repositoryIDPattern.MatchString(id) {
		return gitRoute{}, false
	}
	route := gitRoute{repositoryID: id, endpoint: endpoint}
	query := r.URL.Query()
	switch endpoint {
	case endpointInfoRefs:
		services := query["service"]
		if r.Method != http.MethodGet || len(query) != 1 || len(services) != 1 ||
			(services[0] != serviceUploadPack && services[0] != serviceReceive) {
			return gitRoute{}, false
		}
		route.service = services[0]
	case endpointUploadPack, endpointReceive:
		if r.Method != http.MethodPost || len(query) != 0 || r.URL.RawQuery != "" {
			return gitRoute{}, false
		}
		route.service = endpoint
	default:
		return gitRoute{}, false
	}
	return route, true
}

func (s *Service) serveGit(w http.ResponseWriter, r *http.Request) {
	route, ok := parseGitRoute(r)
	if !ok {
		writeContractError(w, contractError(domain.CodeNotFound, "not found"), "")
		return
	}
	token := r.Header.Get(AssertionHeader)
	if token == "" {
		writeContractError(w, contractError(domain.CodeInvalidCredential, "a forwarding assertion is required"), "")
		return
	}
	now := s.now()
	claims, err := s.cfg.Assertions.VerifyForwarding(r.Context(), token, s.cfg.TenantID, now)
	if err != nil {
		if ce, ok := asContract(err); ok && (ce.Code == domain.CodeForbidden || ce.Code == domain.CodeUnavailable) {
			writeContractError(w, contractError(ce.Code, "the forwarding assertion was refused"), "")
			return
		}
		writeContractError(w, contractError(domain.CodeInvalidCredential, "the forwarding assertion was refused"), "")
		return
	}
	requestID := claims.RequestID
	if err := gitv1.CheckForwarding(claims, string(s.cfg.TenantID), now); err != nil {
		s.writeError(w, err, requestID)
		return
	}
	if err := claims.CheckService(route.service); err != nil {
		s.writeError(w, err, requestID)
		return
	}
	// The assertion names the repository; the path must agree. A mismatch is
	// indistinguishable from an unknown repository.
	if claims.RepositoryID != route.repositoryID {
		writeContractError(w, contractError(domain.CodeNotFound, "the repository was not found"), requestID)
		return
	}
	dir, err := s.existingRepo(claims.RepositoryID)
	if err != nil {
		s.writeError(w, err, requestID)
		return
	}
	protocol := r.Header.Get("Git-Protocol")
	if protocol != "" && !protocolHeaderPattern.MatchString(protocol) {
		writeContractError(w, contractError(domain.CodeInvalidRequest, "unsupported Git-Protocol header"), requestID)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "5")
		writeContractError(w, contractError(domain.CodeRateLimited, "the Git worker is serving its maximum of %d concurrent requests", s.cfg.Limits.MaxConcurrent), requestID)
		return
	}
	s.serveBackend(w, r, route, claims, dir, protocol, requestID)
}

// serveBackend spools the request, runs `git http-backend` as a CGI in its own
// process group and streams its answer.
func (s *Service) serveBackend(w http.ResponseWriter, r *http.Request, route gitRoute, claims gitv1.ForwardingAssertion, dir, protocol, requestID string) {
	rc := http.NewResponseController(w)
	isReceive := route.endpoint == endpointReceive

	var (
		body     *os.File
		bodySize int64
	)
	if r.Method == http.MethodPost {
		limit := s.cfg.Limits.MaxRequestBytes
		if isReceive {
			limit = s.cfg.Limits.MaxPushBytes
		}
		var status int
		var ce *gitv1.ContractError
		body, bodySize, status, ce = s.spool(w, r, rc, limit)
		if ce != nil {
			writeStatusError(w, status, ce, requestID)
			return
		}
		defer body.Close()
	}

	ctx := r.Context()
	var recv *receive
	if isReceive {
		// A receive runs to its own end whatever the client does: the whole pack
		// is already on disk, and a half-finished receive is the worst outcome.
		ctx = context.WithoutCancel(ctx)
		gateCtx, cancel := context.WithTimeout(ctx, s.cfg.Limits.GateWait)
		release, err := s.gates.of(claims.RepositoryID).acquireShared(gateCtx)
		cancel()
		if err != nil {
			w.Header().Set("Retry-After", "5")
			writeContractError(w, contractError(domain.CodeUnavailable, "the repository is under maintenance; retry shortly"), requestID)
			return
		}
		defer release()
		recv = s.startReceive(claims.RepositoryID, claims.GrantID, dir)
		defer recv.finish()
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.MaxDuration)
	defer cancel()

	env := s.gitEnv(true,
		"GIT_PROJECT_ROOT="+s.layout.repositories,
		"GIT_HTTP_EXPORT_ALL=1",
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_PROTOCOL=HTTP/1.1",
		"REQUEST_METHOD="+r.Method,
		"PATH_INFO=/"+claims.RepositoryID+".git/"+route.endpoint,
		"REMOTE_ADDR=127.0.0.1",
	)
	if r.Method == http.MethodGet {
		env = append(env, "QUERY_STRING=service="+route.service)
	} else {
		env = append(env, "CONTENT_TYPE="+cgiValue(r.Header.Get("Content-Type")), "CONTENT_LENGTH="+strconv.FormatInt(bodySize, 10))
	}
	if route.service == serviceReceive {
		// REMOTE_USER is what enables receive-pack in http-backend; the assertion
		// already permitted it. The grant ID is the reflog identity.
		env = append(env, "REMOTE_USER="+claims.GrantID)
	}
	if protocol != "" {
		env = append(env, "GIT_PROTOCOL="+protocol)
	}
	if recv != nil {
		env = append(env,
			envHookAddr+"="+s.hookAddr, envHookToken+"="+recv.token, envHookKind+"="+kindReceive,
			envReceiveID+"="+recv.id, envRepoID+"="+claims.RepositoryID, envRepoDir+"="+dir,
			envGrantID+"="+claims.GrantID, envTenantID+"="+string(s.cfg.TenantID),
			envGitBinary+"="+s.cfg.GitBinary,
			envHookTimeout+"="+strconv.Itoa(int((s.cfg.Limits.ControlTimeout+10*time.Second)/time.Second)),
		)
	}

	cmd := exec.CommandContext(ctx, s.cfg.GitBinary, "http-backend")
	cmd.Dir = s.layout.repositories
	cmd.Env = env
	if body != nil {
		cmd.Stdin = body
	}
	var stderr strings.Builder
	cmd.Stderr = &capWriter{b: &stderr, remaining: 32 << 10}
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	cmd.ExtraFiles = []*os.File{s.lock} // the generation lock: held by the whole process tree
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.writeError(w, err, requestID)
		return
	}
	if err := cmd.Start(); err != nil {
		s.writeError(w, fmt.Errorf("starting git http-backend: %w", err), requestID)
		return
	}
	if recv != nil {
		recv.setKill(func() {
			select {
			case <-recv.done:
			default:
				_ = killProcessGroup(cmd)
			}
		})
	}
	_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.Limits.MaxDuration))

	streamed := s.streamCGI(w, rc, stdout)
	waitErr := cmd.Wait()
	if recv != nil {
		reason := recv.terminatedFor()
		if reason == "" && ctx.Err() != nil {
			reason = "request duration exceeded"
		}
		if reason != "" {
			s.cfg.Logger.Warn("receive terminated", "reason", reason, "repositoryId", claims.RepositoryID, "receiveId", recv.id)
		}
		// Kill what still holds the group and verify it gone before anything
		// repairs the repository; a group that does not disappear fences it.
		s.groupExited(claims.RepositoryID, dir, cmd, recv.id, reason != "")
	} else {
		_ = killProcessGroup(cmd) // anything still holding the group dies with the request
	}
	if waitErr != nil || !streamed {
		s.cfg.Logger.Warn("git http-backend did not complete", "error", waitErr, "stderr", stderr.String(),
			"repositoryId", claims.RepositoryID, "endpoint", route.endpoint, "requestId", requestID, "headersSent", streamed)
	}
	if !streamed {
		writeContractError(w, contractError(domain.CodeUnavailable, "the Git backend failed"), requestID)
	}
}

// cgiValue keeps a header value on one line with no control characters.
func cgiValue(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
}

// capWriter keeps the first remaining bytes written.
type capWriter struct {
	b         *strings.Builder
	remaining int
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if c.remaining > 0 {
		if len(p) > c.remaining {
			p = p[:c.remaining]
		}
		c.b.Write(p)
		c.remaining -= len(p)
	}
	return n, nil
}

// streamCGI parses the CGI response headers and streams the body, flushing each
// chunk. It reports whether a response was started. After a client write
// error it keeps draining the backend so the process ends by itself.
func (s *Service) streamCGI(w http.ResponseWriter, rc *http.ResponseController, stdout io.Reader) bool {
	br := bufio.NewReaderSize(stdout, 64<<10)
	header, err := textproto.NewReader(br).ReadMIMEHeader()
	if err != nil && len(header) == 0 {
		_, _ = io.Copy(io.Discard, br)
		return false
	}
	status := http.StatusOK
	if st := header.Get("Status"); st != "" {
		if code, err := strconv.Atoi(strings.Fields(st)[0]); err == nil && code >= 200 && code <= 599 {
			status = code
		}
	}
	for name, values := range header {
		switch textproto.CanonicalMIMEHeaderKey(name) {
		case "Status", "Connection", "Transfer-Encoding", "Content-Length", "Set-Cookie", "Server":
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	buf := make([]byte, 32<<10)
	clientGone := false
	for {
		n, err := br.Read(buf)
		if n > 0 && !clientGone {
			if _, werr := w.Write(buf[:n]); werr != nil {
				clientGone = true
			} else if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				clientGone = true
			}
		}
		if err != nil {
			return true
		}
	}
}

// writeStatusError writes a contract error under an explicit HTTP status (the
// contract has no code for a too-large request).
func writeStatusError(w http.ResponseWriter, status int, ce *gitv1.ContractError, requestID string) {
	if requestID == "" {
		requestID = "unknown"
	}
	writeJSON(w, status, gitv1.ErrorResponse{Error: gitv1.ErrorBody{
		Code: ce.Code, Message: truncateText(ce.Message, 2000), RequestID: requestID, Details: map[string]any{},
	}})
}

// spool copies the request body to an unlinked temporary file under the worker's
// spool directory, bounded in size and time. Git sends a push larger than
// http.postBuffer chunked, which net/http/cgi (and http-backend's CONTENT_LENGTH
// contract) cannot take streamed; the spool gives the backend an exact length
// without holding the pack in memory. A gzip-encoded body is decompressed here,
// so the limit bounds the decompressed size.
func (s *Service) spool(w http.ResponseWriter, r *http.Request, rc *http.ResponseController, limit int64) (*os.File, int64, int, *gitv1.ContractError) {
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity", "gzip", "x-gzip":
	default:
		return nil, 0, http.StatusUnsupportedMediaType, contractError(domain.CodeInvalidRequest, "unsupported Content-Encoding")
	}
	if r.ContentLength > limit && !isGzip(r) {
		return nil, 0, http.StatusRequestEntityTooLarge, contractError(domain.CodeInvalidRequest, "the request body exceeds the limit of %d bytes", limit)
	}
	_ = rc.SetReadDeadline(time.Now().Add(s.cfg.Limits.BodyTimeout))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()

	var src io.Reader = http.MaxBytesReader(w, r.Body, limit)
	if isGzip(r) {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return nil, 0, http.StatusBadRequest, contractError(domain.CodeInvalidRequest, "the request body is not valid gzip")
		}
		defer gz.Close()
		src = gz
	}
	f, err := os.CreateTemp(s.layout.spool, "req-")
	if err != nil {
		return nil, 0, http.StatusServiceUnavailable, contractError(domain.CodeUnavailable, "the request could not be spooled")
	}
	_ = os.Remove(f.Name()) // the open descriptor is all that is needed; nothing is left after a crash
	n, err := io.Copy(f, io.LimitReader(src, limit+1))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge) || (err == nil && n > limit):
		f.Close()
		return nil, 0, http.StatusRequestEntityTooLarge, contractError(domain.CodeInvalidRequest, "the request body exceeds the limit of %d bytes", limit)
	case err != nil:
		f.Close()
		status := http.StatusBadRequest
		var netErr interface{ Timeout() bool }
		if errors.As(err, &netErr) && netErr.Timeout() {
			status = http.StatusRequestTimeout
		}
		return nil, 0, status, contractError(domain.CodeInvalidRequest, "the request body was not received completely")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, 0, http.StatusServiceUnavailable, contractError(domain.CodeUnavailable, "the request could not be spooled")
	}
	return f, n, 0, nil
}

func isGzip(r *http.Request) bool {
	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	return enc == "gzip" || enc == "x-gzip"
}
