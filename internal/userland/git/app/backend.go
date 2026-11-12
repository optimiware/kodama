package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
)

// ErrMalformedBackendResponse reports a `git http-backend` response that is not
// a CGI response.
var ErrMalformedBackendResponse = errors.New("malformed git http-backend response")

// DefaultBackendMaxBodyBytes bounds one smart-HTTP request body when the
// backend configuration leaves the bound unset.
const DefaultBackendMaxBodyBytes int64 = 512 << 20

// DefaultBackendMaxResponseBytes bounds one `git http-backend` response when
// the configuration leaves the bound unset.
const DefaultBackendMaxResponseBytes int64 = 512 << 20

// backendMemoryThreshold is the in-memory cutoff for request bodies and CGI
// response pre-buffering before spooling to disk or streaming chunks.
const backendMemoryThreshold int64 = 1 << 20

// BackendConfig configures the native `git http-backend` adapter.
type BackendConfig struct {
	// GitPath is the native git binary. It defaults to "git".
	GitPath string
	// Env is the exact base child environment. A nil slice selects the
	// documented minimal environment.
	Env []string
	// MaxBodyBytes bounds one request body read before the CGI process starts.
	MaxBodyBytes int64
	// MaxResponseBytes bounds one buffered CGI response.
	MaxResponseBytes int64
	// Timeout bounds one backend exchange when non-zero.
	Timeout time.Duration
}

// HTTPBackend implements git.GitBackend over the native `git http-backend`
// CGI program. It never runs a shell and forwards only the validated request
// environment and headers.
type HTTPBackend struct {
	path    string
	env     []string
	maxBody int64
	maxResp int64
	timeout time.Duration
	command func(context.Context, string, ...string) *exec.Cmd
}

var _ git.GitBackend = (*HTTPBackend)(nil)

// NewHTTPBackend validates the configuration and returns the backend.
func NewHTTPBackend(config BackendConfig) (*HTTPBackend, error) {
	path := strings.TrimSpace(config.GitPath)
	if path == "" {
		path = "git"
	}
	if strings.ContainsAny(path, " \t\n;|&$`'\"<>") {
		return nil, fmt.Errorf("%w: git path %q is not a plain executable path", ErrUnsupportedCommand, path)
	}
	env := config.Env
	if env == nil {
		env = MinimalEnvironment()
	}
	maxBody := config.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultBackendMaxBodyBytes
	}
	maxResp := config.MaxResponseBytes
	if maxResp <= 0 {
		maxResp = DefaultBackendMaxResponseBytes
	}
	return &HTTPBackend{
		path:    path,
		env:     append(make([]string, 0, len(env)), env...),
		maxBody: maxBody,
		maxResp: maxResp,
		timeout: config.Timeout,
		command: exec.CommandContext,
	}, nil
}

// Serve runs one validated smart-HTTP exchange through `git http-backend`.
//
// GitBackendRequest does not carry the HTTP method. The smart-HTTP
// handler supplies a non-nil body only for a POST, so a nil body selects the
// GET `info/refs` exchange and a present body selects the POST service
// exchange. This is the complete information the existing port defines.
func (backend *HTTPBackend) Serve(ctx context.Context, request git.GitBackendRequest, body io.Reader, response git.GitResponseWriter) error {
	if backend == nil {
		return fmt.Errorf("%w: backend is nil", ErrMalformedBackendResponse)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if response == nil {
		return fmt.Errorf("%w: response sink is nil", ErrMalformedBackendResponse)
	}
	if request.Service != "git-upload-pack" && request.Service != "git-receive-pack" {
		return fmt.Errorf("%w: service %q", ErrMalformedBackendResponse, request.Service)
	}
	if !filepath.IsAbs(request.Path) {
		return fmt.Errorf("%w: repository path %q is not absolute", ErrMalformedBackendResponse, request.Path)
	}

	method := http.MethodGet
	pathInfo := "/" + filepath.Base(request.Path) + "/info/refs"
	query := "service=" + request.Service
	var stdin io.Reader
	var contentLength int64
	if body != nil {
		var cleanup func()
		var err error
		stdin, contentLength, cleanup, err = spoolBackendBody(body, backend.maxBody)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return err
		}
		method = http.MethodPost
		pathInfo = "/" + filepath.Base(request.Path) + "/" + request.Service
		query = ""
	}

	environment := append([]string(nil), backend.env...)
	environment = append(environment, request.Environment...)
	environment = append(environment,
		"GIT_PROJECT_ROOT="+filepath.Dir(request.Path),
		"PATH_INFO="+pathInfo,
		"REQUEST_METHOD="+method,
		"QUERY_STRING="+query,
		"GIT_HTTP_EXPORT_ALL=1",
		"CONTENT_TYPE="+request.Headers.Get("Content-Type"),
		"CONTENT_LENGTH="+strconv.FormatInt(contentLength, 10),
	)
	for key, values := range request.Headers {
		canonical := http.CanonicalHeaderKey(key)
		if canonical == "Content-Type" || canonical == "Content-Length" {
			continue
		}
		name := "HTTP_" + strings.ToUpper(strings.ReplaceAll(canonical, "-", "_"))
		for _, value := range values {
			environment = append(environment, name+"="+value)
		}
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if backend.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, backend.timeout)
		defer cancel()
	}
	cmdCtx, cmdCancel := context.WithCancel(runCtx)
	defer cmdCancel()

	child := backend.command(cmdCtx, backend.path, "http-backend")
	child.Env = environment
	child.Stdin = stdin
	stdoutPipe, err := child.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := newBoundedBuffer(64 << 10)
	child.Stderr = stderr

	if err := child.Start(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if runErr := runCtx.Err(); runErr != nil {
			return runErr
		}
		return err
	}
	abortChild := func() {
		cmdCancel()
		_ = stdoutPipe.Close()
		_ = child.Wait()
	}

	preBufferLimit := backendMemoryThreshold
	if backend.maxResp <= backendMemoryThreshold {
		preBufferLimit = backend.maxResp + 1
	}
	initial, readErr := io.ReadAll(io.LimitReader(stdoutPipe, preBufferLimit))
	if readErr != nil {
		abortChild()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if runErr := runCtx.Err(); runErr != nil {
			return runErr
		}
		return readErr
	}
	if int64(len(initial)) > backend.maxResp {
		abortChild()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if runErr := runCtx.Err(); runErr != nil {
			return runErr
		}
		return fmt.Errorf("%w: git http-backend response", ErrOutputTooLarge)
	}

	// When the entire CGI output fit within preBufferLimit, stdout has already
	// reached EOF, so we can reap the child before committing response headers.
	if int64(len(initial)) < preBufferLimit {
		waitErr := child.Wait()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if runErr := runCtx.Err(); runErr != nil {
			return runErr
		}
		if waitErr != nil {
			if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
				return fmt.Errorf("git http-backend exited %d", exitErr.ExitCode())
			}
			return waitErr
		}
		if err := writeCGIResponse(response, initial); err != nil {
			return err
		}
		flushGitResponse(response)
		return nil
	}

	headerBlock, bodyPrefix := splitCGIResponse(initial)
	if headerBlock == nil {
		abortChild()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if runErr := runCtx.Err(); runErr != nil {
			return runErr
		}
		return fmt.Errorf("%w: no header terminator", ErrMalformedBackendResponse)
	}
	status, parsedHeaders, err := parseCGIHeaderBlock(headerBlock)
	if err != nil {
		abortChild()
		return err
	}
	applyCGIHeaders(response, parsedHeaders, status)
	if len(bodyPrefix) > 0 {
		if _, writeErr := response.Write(bodyPrefix); writeErr != nil {
			abortChild()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return writeErr
		}
	}
	flushGitResponse(response)

	totalBytes := int64(len(initial))
	chunk := make([]byte, 32<<10)
	for {
		n, streamErr := stdoutPipe.Read(chunk)
		if n > 0 {
			totalBytes += int64(n)
			if totalBytes > backend.maxResp {
				abortChild()
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if runErr := runCtx.Err(); runErr != nil {
					return runErr
				}
				return fmt.Errorf("%w: git http-backend response", ErrOutputTooLarge)
			}
			if _, writeErr := response.Write(chunk[:n]); writeErr != nil {
				abortChild()
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return writeErr
			}
			flushGitResponse(response)
		}
		if streamErr != nil {
			if errors.Is(streamErr, io.EOF) {
				break
			}
			abortChild()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if runErr := runCtx.Err(); runErr != nil {
				return runErr
			}
			return streamErr
		}
	}

	waitErr := child.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if runErr := runCtx.Err(); runErr != nil {
		return runErr
	}
	if waitErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			return fmt.Errorf("git http-backend exited %d", exitErr.ExitCode())
		}
		return waitErr
	}
	return nil
}

// spoolBackendBody buffers small request bodies in memory and spools bodies
// larger than 1 MiB to an owner-only temporary file bounded by limit.
func spoolBackendBody(reader io.Reader, limit int64) (io.Reader, int64, func(), error) {
	memCap := min(limit, backendMemoryThreshold)
	initial, err := io.ReadAll(io.LimitReader(reader, memCap+1))
	if err != nil {
		return nil, 0, nil, err
	}
	if int64(len(initial)) > limit {
		return nil, 0, nil, fmt.Errorf("%w: request body exceeds %d bytes", git.ErrBodyTooLarge, limit)
	}
	if int64(len(initial)) <= memCap {
		return bytes.NewReader(initial), int64(len(initial)), nil, nil
	}

	spool, err := os.CreateTemp("", "kodama-git-body-*.tmp")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("create request body spool: %w", err)
	}
	spoolName := spool.Name()
	closed := false
	cleanup := func() {
		if !closed {
			closed = true
			_ = spool.Close()
		}
		_ = os.Remove(spoolName)
	}
	if err := spool.Chmod(0o600); err != nil {
		cleanup()
		return nil, 0, nil, fmt.Errorf("restrict request body spool permissions: %w", err)
	}
	if _, err := spool.Write(initial); err != nil {
		cleanup()
		return nil, 0, nil, fmt.Errorf("write request body spool: %w", err)
	}
	remainingLimit := limit - int64(len(initial))
	written, err := io.Copy(spool, io.LimitReader(reader, remainingLimit+1))
	if err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	total := int64(len(initial)) + written
	if total > limit {
		cleanup()
		return nil, 0, nil, fmt.Errorf("%w: request body exceeds %d bytes", git.ErrBodyTooLarge, limit)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, 0, nil, fmt.Errorf("rewind request body spool: %w", err)
	}
	return spool, total, cleanup, nil
}

type cgiHeader struct {
	name  string
	value string
}

func parseCGIHeaderBlock(headerBlock []byte) (int, []cgiHeader, error) {
	status := http.StatusOK
	var headers []cgiHeader
	lines := strings.SplitSeq(string(headerBlock), "\n")
	for line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return 0, nil, fmt.Errorf("%w: header line %q", ErrMalformedBackendResponse, line)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if strings.EqualFold(name, "Status") {
			code := strings.Fields(value)
			if len(code) == 0 {
				return 0, nil, fmt.Errorf("%w: empty status", ErrMalformedBackendResponse)
			}
			parsed, err := strconv.Atoi(code[0])
			if err != nil || parsed < 100 || parsed > 999 {
				return 0, nil, fmt.Errorf("%w: status %q", ErrMalformedBackendResponse, value)
			}
			status = parsed
			continue
		}
		headers = append(headers, cgiHeader{name: name, value: value})
	}
	return status, headers, nil
}

func applyCGIHeaders(response git.GitResponseWriter, headers []cgiHeader, status int) {
	for _, header := range headers {
		response.Header().Add(header.name, header.value)
	}
	response.WriteHeader(status)
}

func flushGitResponse(response git.GitResponseWriter) {
	if flusher, ok := response.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeCGIResponse parses one CGI response and forwards it to the sink.
func writeCGIResponse(response git.GitResponseWriter, data []byte) error {
	headerBlock, body := splitCGIResponse(data)
	if headerBlock == nil {
		return fmt.Errorf("%w: no header terminator", ErrMalformedBackendResponse)
	}
	status, headers, err := parseCGIHeaderBlock(headerBlock)
	if err != nil {
		return err
	}
	applyCGIHeaders(response, headers, status)
	if len(body) > 0 {
		if _, err := response.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// splitCGIResponse splits a CGI response into its header block and body. A nil
// header block reports a missing terminator.
func splitCGIResponse(data []byte) ([]byte, []byte) {
	if before, after, ok := bytes.Cut(data, []byte("\r\n\r\n")); ok {
		return before, after
	}
	if before, after, ok := bytes.Cut(data, []byte("\n\n")); ok {
		return before, after
	}
	return nil, nil
}
