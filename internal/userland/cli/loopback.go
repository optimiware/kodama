package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// loopbackHost is the only host the callback listener binds.
const loopbackHost = "127.0.0.1"

// loopbackReadTimeout bounds one callback request read.
const loopbackReadTimeout = 10 * time.Second

// loopbackListener binds one 127.0.0.1:0 callback listener.
type loopbackListener struct{}

// NewLoopbackListener returns the production loopback callback listener.
func NewLoopbackListener() LoopbackListener {
	return loopbackListener{}
}

// Listen binds 127.0.0.1:0 and returns the callback address, a bounded wait and
// an idempotent close. Exactly one request to the fixed callback path with the
// expected Host header is accepted; every other request and every later
// request is refused with a fixed page and never delivered.
func (loopbackListener) Listen(ctx context.Context) (string, func(context.Context) (CallbackParams, error), func() error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", nil, nil, err
	}
	socket, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	if err != nil {
		return "", nil, nil, fmt.Errorf("bind loopback callback: %w", err)
	}
	address := socket.Addr().String()
	session := &loopbackSession{
		listener: socket,
		address:  address,
		results:  make(chan CallbackParams, 1),
		closed:   make(chan struct{}),
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(session.handle),
		ReadHeaderTimeout: loopbackReadTimeout,
		ReadTimeout:       loopbackReadTimeout,
		WriteTimeout:      loopbackReadTimeout,
		IdleTimeout:       loopbackReadTimeout,
	}
	session.server = server
	go func() {
		_ = server.Serve(socket)
	}()
	return address, session.wait, session.close, nil
}

// loopbackSession is one bound callback listener.
type loopbackSession struct {
	listener  net.Listener
	server    *http.Server
	address   string
	results   chan CallbackParams
	closed    chan struct{}
	once      sync.Once
	delivered atomic.Bool
}

// handle answers one callback request. Only the fixed path, the GET method and
// the exact configured Host header are the callback; a first match is
// delivered once, and every later request is refused.
func (session *loopbackSession) handle(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.URL.Path != OAuthCallbackPath || request.Host != session.address {
		writeCallbackPage(writer, http.StatusBadRequest, "This is not the Kodama login callback.")
		return
	}
	if !session.delivered.CompareAndSwap(false, true) {
		writeCallbackPage(writer, http.StatusGone, "This login callback was already used.")
		return
	}
	query := request.URL.Query()
	params := CallbackParams{
		Code:             query.Get("code"),
		State:            query.Get("state"),
		Error:            query.Get("error"),
		ErrorDescription: query.Get("error_description"),
	}
	writeCallbackPage(writer, http.StatusOK, "Kodama login received. You may close this window.")
	// Stop accepting new connections; the current one may still serve the
	// refusal page for a replayed callback.
	_ = session.listener.Close()
	session.results <- params
}

// wait blocks for the one callback, listener closure or context cancellation.
func (session *loopbackSession) wait(ctx context.Context) (CallbackParams, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case params := <-session.results:
		return params, nil
	case <-session.closed:
		select {
		case params := <-session.results:
			return params, nil
		default:
			return CallbackParams{}, errors.New("the loopback callback listener is closed")
		}
	case <-ctx.Done():
		select {
		case params := <-session.results:
			return params, nil
		default:
			return CallbackParams{}, ctx.Err()
		}
	}
}

// close stops the listener and unblocks a pending wait. It is idempotent.
func (session *loopbackSession) close() error {
	var closeErr error
	session.once.Do(func() {
		close(session.closed)
		if err := session.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = err
		}
	})
	return closeErr
}

// writeCallbackPage writes one fixed human page.
func writeCallbackPage(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, message+"\n")
}
