package harness

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Probe is one readiness attempt. It must return quickly and honor ctx.
type Probe func(ctx context.Context) error

// WaitOptions bounds a readiness barrier.
type WaitOptions struct {
	Timeout        time.Duration // total window; DefaultProbeTimeout when zero
	AttemptTimeout time.Duration // per-attempt bound; 5s when zero
	InitialBackoff time.Duration // 100ms when zero
	MaxBackoff     time.Duration // 2s when zero
}

// DefaultWaitOptions is the standard barrier configuration.
func DefaultWaitOptions() WaitOptions {
	return WaitOptions{
		Timeout:        DefaultProbeTimeout,
		AttemptTimeout: 5 * time.Second,
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
	}
}

// WaitReady runs probe until it succeeds, the total window expires or ctx is
// cancelled. Backoff is exponential and injectable through clock, so unit
// tests never sleep. A timeout returns an error matching ErrProbeTimeout.
func WaitReady(ctx context.Context, clock Clock, name string, probe Probe, opts WaitOptions) error {
	if clock == nil {
		clock = RealClock{}
	}
	if probe == nil {
		return errors.New("harness: nil readiness probe")
	}
	defaults := DefaultWaitOptions()
	if opts.Timeout <= 0 {
		opts.Timeout = defaults.Timeout
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = defaults.AttemptTimeout
	}
	if opts.InitialBackoff <= 0 {
		opts.InitialBackoff = defaults.InitialBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = defaults.MaxBackoff
	}
	if opts.InitialBackoff > opts.MaxBackoff {
		opts.InitialBackoff = opts.MaxBackoff
	}

	deadline := clock.Now().Add(opts.Timeout)
	backoff := opts.InitialBackoff
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %s: %v (last probe error: %v)", ErrProbeTimeout, name, err, last)
		}
		attemptCtx, cancel := context.WithTimeout(ctx, opts.AttemptTimeout)
		err := probe(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			return fmt.Errorf("%w: %s after %s: %v", ErrProbeTimeout, name, opts.Timeout, last)
		}
		delay := min(backoff, remaining)
		if sleepErr := clock.Sleep(ctx, delay); sleepErr != nil {
			return fmt.Errorf("%w: %s: %v (last probe error: %v)", ErrProbeTimeout, name, sleepErr, last)
		}
		backoff *= 2
		if backoff > opts.MaxBackoff {
			backoff = opts.MaxBackoff
		}
	}
}

// HTTPProbe succeeds on an accepted status. With no accepted status, any 2xx
// succeeds.
func HTTPProbe(rawURL string, accept ...int) Probe {
	return func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if len(accept) == 0 {
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
			return fmt.Errorf("harness: GET %s returned %d", rawURL, response.StatusCode)
		}
		if slices.Contains(accept, response.StatusCode) {
			return nil
		}
		return fmt.Errorf("harness: GET %s returned %d, want one of %v", rawURL, response.StatusCode, accept)
	}
}

// maxPostgresMessageBody bounds one backend protocol message body. Real
// startup messages are far smaller; a larger claimed length is malformed and
// must never be read into memory.
const maxPostgresMessageBody = 16 << 10

// PostgresProbe performs the PostgreSQL startup-message handshake over TCP: it
// sends a protocol 3.0 StartupMessage and succeeds only once the server proves
// it finished normal startup. It needs no driver and no shell inside the
// container.
//
// An Authentication message ('R') is the normal path. An ErrorResponse ('E')
// proves readiness only when its SQLSTATE (field 'C') is a startup-stage
// rejection (28xxx authentication, 3D000 unknown database): those are emitted
// by a fully started server. A starting, recovering or shutting-down server
// answers 57P03/57P01/57P02/53300, which must keep the barrier retrying, as
// must any malformed, truncated or oversized reply.
func PostgresProbe(addr, user string) Probe {
	return func(ctx context.Context) error {
		dialer := net.Dialer{}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		defer conn.Close()
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		if user == "" {
			user = "postgres"
		}
		payload := make([]byte, 0, 32)
		payload = append(payload, 0, 3, 0, 0) // protocol 3.0
		payload = append(payload, "user"...)
		payload = append(payload, 0)
		payload = append(payload, user...)
		payload = append(payload, 0)
		payload = append(payload, 0) // terminator
		message := make([]byte, 4+len(payload))
		binary.BigEndian.PutUint32(message, uint32(len(message)))
		copy(message[4:], payload)
		if _, err := conn.Write(message); err != nil {
			return err
		}
		return readPostgresStartupReply(conn)
	}
}

// readPostgresStartupReply reads the first backend message after a
// StartupMessage and decides whether it proves normal startup completion. It
// always consumes the full declared body (bounded) so no partial message is
// interpreted and no read can hang past the connection deadline.
func readPostgresStartupReply(conn net.Conn) error {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("harness: read postgres startup reply: %w", err)
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length < 4 || length > 4+maxPostgresMessageBody {
		return fmt.Errorf("harness: postgres startup reply has invalid length %d", length)
	}
	body := make([]byte, int(length)-4)
	if _, err := io.ReadFull(conn, body); err != nil {
		return fmt.Errorf("harness: read postgres startup reply body: %w", err)
	}
	switch header[0] {
	case 'R':
		if len(body) < 4 {
			return fmt.Errorf("harness: postgres authentication reply is %d bytes, want at least 4", len(body))
		}
		return nil
	case 'E':
		code := postgresErrorSQLState(body)
		if code == "" {
			return errors.New("harness: postgres startup error reply has no SQLSTATE")
		}
		if postgresStartupComplete(code) {
			return nil
		}
		return fmt.Errorf("harness: postgres startup not complete: SQLSTATE %s", code)
	default:
		return fmt.Errorf("harness: postgres startup reply has unexpected type %q", header[0])
	}
}

// postgresErrorSQLState extracts field 'C' from an ErrorResponse body. An
// absent, truncated or unterminated field list yields the empty string.
func postgresErrorSQLState(body []byte) string {
	for i := 0; i < len(body); {
		field := body[i]
		i++
		if field == 0 {
			return ""
		}
		end := bytes.IndexByte(body[i:], 0)
		if end < 0 {
			return ""
		}
		value := string(body[i : i+end])
		i += end + 1
		if field == 'C' {
			return value
		}
	}
	return ""
}

// postgresStartupComplete reports whether an ErrorResponse SQLSTATE was
// produced after the server finished starting: authentication failures and an
// unknown database are startup-stage rejections, while the 57P0x/53300 family
// means the server is not ready to serve yet.
func postgresStartupComplete(code string) bool {
	return strings.HasPrefix(code, "28") || code == "3D000"
}

// NATSProbe succeeds when the NATS monitoring endpoint reports healthy.
func NATSProbe(monitorBaseURL string) Probe {
	return HTTPProbe(strings.TrimRight(monitorBaseURL, "/")+"/healthz", http.StatusOK)
}

// RegistryProbe succeeds when the registry API answers. An unauthenticated
// registry returns 200 for /v2/; an auth-enabled one returns 401; both prove
// the registry process is serving.
func RegistryProbe(baseURL string) Probe {
	return HTTPProbe(strings.TrimRight(baseURL, "/")+"/v2/", http.StatusOK, http.StatusUnauthorized)
}
