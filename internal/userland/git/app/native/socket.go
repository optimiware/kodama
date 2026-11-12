package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/id"
)

// ErrSocketUnavailable reports a hook callback channel that cannot be created
// or reached. It is a fail-closed error: a push must be rejected, never
// accepted, when the channel is absent.
var ErrSocketUnavailable = errors.New("hook callback channel is unavailable")

// ErrForeignSocket reports an existing path that is not our own socket. The
// server never clobbers an unknown file.
var ErrForeignSocket = errors.New("hook socket path already exists")

// SocketConfig configures one private hook callback server.
type SocketConfig struct {
	// Path is the absolute Unix socket path. It must live in a private (0700)
	// directory that this server may create.
	Path string
	// Handler handles one decoded request.
	Handler Handler
	// Timeout bounds one connection: the request read, the delegated decision
	// and the response write.
	Timeout time.Duration
	// MaxRequestBytes bounds one encoded request. Zero selects
	// DefaultMaxRequestBytes.
	MaxRequestBytes int
	// PeerUID resolves the connecting peer's uid. A nil value selects the
	// platform implementation (SO_PEERCRED on Linux). It exists so the
	// peer-credential refusal is unit-testable without a second uid.
	PeerUID func(*net.UnixConn) (uint32, error)
	// RemoveDirectory removes the socket's private directory on Close. The app
	// sets it for the temp directory it owns; an injected directory is left.
	RemoveDirectory bool
}

// SocketServer is the private Unix-domain-socket hook callback server. It is
// safe for concurrent use and starts no goroutine until Serve.
type SocketServer struct {
	path            string
	handler         Handler
	timeout         time.Duration
	maxRequest      int
	peerUID         func(*net.UnixConn) (uint32, error)
	removeDirectory bool
	expectedUID     uint32

	mutex     sync.Mutex
	listener  *net.UnixListener
	closed    bool
	serving   bool
	serveDone chan struct{}
	closeOne  sync.Once
	conns     sync.WaitGroup
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, Request) Response

var _ Handler = HandlerFunc(nil)

// Handle implements Handler.
func (handler HandlerFunc) Handle(ctx context.Context, request Request) Response {
	return handler(ctx, request)
}

// NewSocketServer validates the configuration and returns the server. It
// performs no filesystem effect.
func NewSocketServer(config SocketConfig) (*SocketServer, error) {
	if config.Handler == nil {
		return nil, fmt.Errorf("hook socket handler is missing")
	}
	path, err := filepath.Abs(config.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve hook socket path: %w", err)
	}
	path = filepath.Clean(path)
	if len(path) > MaxSocketPathBytes {
		return nil, fmt.Errorf("%w: socket path %q is too long", ErrSocketUnavailable, path)
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxRequest := config.MaxRequestBytes
	if maxRequest <= 0 {
		maxRequest = DefaultMaxRequestBytes
	}
	peerUID := config.PeerUID
	if peerUID == nil {
		peerUID = systemPeerUID
	}
	return &SocketServer{
		path:            path,
		handler:         config.Handler,
		timeout:         timeout,
		maxRequest:      maxRequest,
		peerUID:         peerUID,
		removeDirectory: config.RemoveDirectory,
		expectedUID:     uint32(os.Getuid()),
	}, nil
}

// Path returns the socket path.
func (server *SocketServer) Path() string { return server.path }

// SocketPath returns a unique, unguessable socket path inside baseDirectory.
// It performs no filesystem effect; Listen creates the private directory.
func SocketPath(baseDirectory string, tenant id.TenantID) (string, error) {
	base := strings.TrimSpace(baseDirectory)
	if base == "" {
		base = os.TempDir()
	}
	absolute, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve hook socket base directory: %w", err)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate hook socket name: %w", err)
	}
	name := "kodama-git-hook-" + string(tenant) + "-" + hex.EncodeToString(raw[:])
	return filepath.Join(absolute, name, "hook.sock"), nil
}

// Listen creates the private directory and binds the socket. It is idempotent.
func (server *SocketServer) Listen() error {
	if server == nil {
		return fmt.Errorf("%w: server is nil", ErrSocketUnavailable)
	}
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.closed {
		return fmt.Errorf("%w: server is closed", ErrSocketUnavailable)
	}
	if server.listener != nil {
		return nil
	}
	directory := filepath.Dir(server.path)
	info, err := os.Stat(directory)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%w: socket parent %q is not a directory", ErrSocketUnavailable, directory)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%w: socket parent %q is not private (mode %04o)", ErrSocketUnavailable, directory, info.Mode().Perm())
		}
	case os.IsNotExist(err):
		if err := os.Mkdir(directory, 0o700); err != nil {
			return fmt.Errorf("%w: create socket directory: %v", ErrSocketUnavailable, err)
		}
	default:
		return fmt.Errorf("%w: stat socket directory: %v", ErrSocketUnavailable, err)
	}
	if _, err := os.Lstat(server.path); err == nil {
		return fmt.Errorf("%w: %q", ErrForeignSocket, server.path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("%w: stat socket path: %v", ErrSocketUnavailable, err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: server.path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("%w: listen: %v", ErrSocketUnavailable, err)
	}
	if err := os.Chmod(server.path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(server.path)
		return fmt.Errorf("%w: set socket mode: %v", ErrSocketUnavailable, err)
	}
	server.listener = listener
	return nil
}

// Serve accepts connections until ctx ends or Close is called. It joins every
// in-flight connection before returning.
func (server *SocketServer) Serve(ctx context.Context) error {
	if server == nil {
		return fmt.Errorf("%w: server is nil", ErrSocketUnavailable)
	}
	if err := server.Listen(); err != nil {
		return err
	}
	server.mutex.Lock()
	listener := server.listener
	if !server.serving {
		server.serving = true
		server.serveDone = make(chan struct{})
	}
	done := server.serveDone
	server.mutex.Unlock()
	if listener == nil {
		return fmt.Errorf("%w: not listening", ErrSocketUnavailable)
	}
	defer func() {
		server.conns.Wait()
		close(done)
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if server.isClosed() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("%w: accept: %v", ErrSocketUnavailable, err)
		}
		server.conns.Add(1)
		go func() {
			defer server.conns.Done()
			server.handle(connection)
		}()
	}
}

// Close stops accepting, joins in-flight connections and removes the socket
// (and its private directory when configured). It is idempotent.
func (server *SocketServer) Close() error {
	if server == nil {
		return nil
	}
	var closeErr error
	server.closeOne.Do(func() {
		server.mutex.Lock()
		server.closed = true
		listener := server.listener
		server.listener = nil
		serving := server.serving
		done := server.serveDone
		server.mutex.Unlock()
		if listener != nil {
			closeErr = listener.Close()
		}
		if serving && done != nil {
			<-done
		} else {
			server.conns.Wait()
		}
		_ = os.Remove(server.path)
		if server.removeDirectory {
			_ = os.Remove(filepath.Dir(server.path))
		}
	})
	return closeErr
}

func (server *SocketServer) isClosed() bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return server.closed
}

// handle serves exactly one bounded request/response exchange.
func (server *SocketServer) handle(connection *net.UnixConn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(server.timeout))
	if server.peerUID != nil {
		uid, err := server.peerUID(connection)
		if err != nil || uid != server.expectedUID {
			// An outside process is refused without a response.
			return
		}
	}
	data, err := io.ReadAll(io.LimitReader(connection, int64(server.maxRequest)+1))
	if err != nil || len(data) > server.maxRequest {
		server.writeResponse(connection, rejectResponse())
		return
	}
	request, err := decodeRequest(data)
	if err != nil {
		server.writeResponse(connection, rejectResponse())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), server.timeout)
	defer cancel()
	response := server.handler.Handle(ctx, request)
	if ctx.Err() != nil || response.ExitCode != 0 {
		response = rejectResponse()
	}
	server.writeResponse(connection, response)
}

func (server *SocketServer) writeResponse(connection *net.UnixConn, response Response) {
	data, err := encodeResponse(response)
	if err != nil {
		return
	}
	_ = connection.SetWriteDeadline(time.Now().Add(server.timeout))
	_, _ = connection.Write(data)
}

// Call sends one hook request to the private socket and returns the decision.
// Every transport, decoding or timeout failure is an error; the caller must
// treat it as a rejection.
func Call(ctx context.Context, socketPath string, request Request, timeout time.Duration) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		return Response{}, err
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	data, err := encodeRequest(request)
	if err != nil {
		return Response{}, err
	}
	dialer := net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return Response{}, fmt.Errorf("%w: dial: %v", ErrSocketUnavailable, err)
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		connection.Close()
		return Response{}, fmt.Errorf("%w: not a Unix connection", ErrSocketUnavailable)
	}
	defer unixConnection.Close()
	deadline := time.Now().Add(timeout)
	_ = unixConnection.SetDeadline(deadline)
	if _, err := unixConnection.Write(data); err != nil {
		return Response{}, fmt.Errorf("%w: write: %v", ErrSocketUnavailable, err)
	}
	if err := unixConnection.CloseWrite(); err != nil {
		return Response{}, fmt.Errorf("%w: close write: %v", ErrSocketUnavailable, err)
	}
	raw, err := io.ReadAll(io.LimitReader(unixConnection, int64(DefaultMaxRequestBytes)+1))
	if err != nil {
		return Response{}, fmt.Errorf("%w: read: %v", ErrSocketUnavailable, err)
	}
	if len(raw) > DefaultMaxRequestBytes {
		return Response{}, fmt.Errorf("%w: response over-bound", ErrSocketUnavailable)
	}
	return decodeResponse(raw)
}
