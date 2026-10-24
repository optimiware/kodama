package protocol

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimiware/kodama/internal/mcpproxy"
)

// RemoteConfig configures the SDK-backed remote leg.
type RemoteConfig struct {
	Destinations *Destinations
	// RootCAs are the trust roots for remote servers (nil: system roots).
	RootCAs *x509.CertPool
	// ConnectTimeout bounds the initialization handshake.
	ConnectTimeout time.Duration
	// MaxResultBytes bounds one remote reply (also the SDK's single-event cap).
	MaxResultBytes int
}

// Remote implements mcpproxy.RemoteTools with the pinned SDK over Streamable
// HTTP.
type Remote struct {
	cfg       RemoteConfig
	transport *http.Transport
}

var _ mcpproxy.RemoteTools = (*Remote)(nil)

// NewRemote validates the configuration.
func NewRemote(cfg RemoteConfig) (*Remote, error) {
	if cfg.Destinations == nil || cfg.ConnectTimeout <= 0 || cfg.MaxResultBytes <= 0 {
		return nil, errors.New("protocol: destinations, a connect timeout and a result bound are required")
	}
	if err := VerifyCompatibility(); err != nil {
		return nil, err
	}
	return &Remote{cfg: cfg, transport: NewTransport(cfg.Destinations, cfg.RootCAs)}, nil
}

// Open connects to the approved endpoint, applies the credential to that origin
// only and verifies the pinned protocol revision. Redirects are not followed and
// the connection never retries, reconnects or re-initializes by itself, so a
// tools/call can never be replayed by the transport.
func (r *Remote) Open(ctx context.Context, t mcpproxy.RemoteTarget) (mcpproxy.RemoteConn, error) {
	if err := r.cfg.Destinations.Approve(t.Endpoint); err != nil {
		return nil, err
	}
	_, u, err := mcpproxy.NormalizeEndpoint(t.Endpoint)
	if err != nil {
		return nil, mcpproxy.ErrDestination
	}
	if t.Recipient != mcpproxy.Origin(u) || len(t.Bearer) == 0 {
		return nil, mcpproxy.ErrCredential
	}
	rt := &bearerTransport{base: r.transport, origin: mcpproxy.Origin(u), token: append([]byte(nil), t.Bearer...)}
	httpClient := &http.Client{Transport: rt, CheckRedirect: noRedirects}
	client := mcp.NewClient(&mcp.Implementation{Name: "kodama-mcp-proxy", Version: "1"}, &mcp.ClientOptions{})
	transport := &mcp.StreamableClientTransport{
		Endpoint:             t.Endpoint,
		HTTPClient:           httpClient,
		MaxRetries:           -1, // no reconnect-driven resends
		DisableStandaloneSSE: true,
		MaxEventSize:         r.cfg.MaxResultBytes,
	}
	cctx, cancel := context.WithTimeout(ctx, r.cfg.ConnectTimeout)
	defer cancel()
	cs, err := client.Connect(cctx, transport, nil)
	if err != nil {
		rt.clear()
		if errors.Is(err, mcpproxy.ErrDestination) {
			return nil, mcpproxy.ErrDestination
		}
		return nil, err
	}
	res := cs.InitializeResult()
	if res == nil || res.ProtocolVersion != ProtocolRevision {
		_ = cs.Close()
		rt.clear()
		return nil, mcpproxy.ErrProtocol
	}
	return &conn{cs: cs, rt: rt, max: r.cfg.MaxResultBytes, protocol: res.ProtocolVersion}, nil
}

type conn struct {
	cs       *mcp.ClientSession
	rt       *bearerTransport
	max      int
	protocol string
	once     sync.Once
}

func (c *conn) Protocol() string { return c.protocol }

// ListTools lists one remote page.
func (c *conn) ListTools(ctx context.Context, cursor string) (mcpproxy.RemotePage, error) {
	res, err := c.cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
	if err != nil {
		return mcpproxy.RemotePage{}, err
	}
	page := mcpproxy.RemotePage{NextCursor: res.NextCursor}
	for _, t := range res.Tools {
		if t == nil {
			continue
		}
		rt := mcpproxy.RemoteTool{Name: t.Name, Title: t.Title, Description: t.Description}
		if t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil {
				rt.InputSchema = b
			}
		}
		if t.OutputSchema != nil {
			if b, err := json.Marshal(t.OutputSchema); err == nil {
				rt.OutputSchema = b
			}
		}
		page.Tools = append(page.Tools, rt)
	}
	return page, nil
}

// CallTool sends exactly one tools/call. A JSON-RPC error reply is a definitive
// failure; every other error (transport loss, deadline, cancellation) leaves the
// effect unknown. Cancelling ctx makes the SDK send notifications/cancelled to
// the remote, which stops remote work where it supports cancellation; that is
// not a claim that the remote effect did not happen.
func (c *conn) CallTool(ctx context.Context, name string, args json.RawMessage) (mcpproxy.RemoteResult, error) {
	var arguments any
	if len(args) > 0 {
		arguments = args
	}
	res, err := c.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return mcpproxy.RemoteResult{}, classifyCall(err)
	}
	raw, merr := json.Marshal(res)
	if merr != nil {
		return mcpproxy.RemoteResult{}, &mcpproxy.RemoteFailure{Code: "unreadable_reply"}
	}
	if len(raw) > c.max {
		return mcpproxy.RemoteResult{}, &mcpproxy.RemoteFailure{Code: "result_too_large"}
	}
	return mcpproxy.RemoteResult{JSON: raw, IsError: res.IsError}, nil
}

func classifyCall(err error) error {
	var je *jsonrpc.Error
	if errors.As(err, &je) {
		// Only the protocol's own error replies are definitive. The SDK reports
		// local transport failures (rejected by transport, connection closing)
		// with codes of the same type, and those leave the remote effect unknown.
		if code, ok := replyCode(je.Code); ok {
			return &mcpproxy.RemoteFailure{Reply: true, Code: code}
		}
		return &mcpproxy.RemoteFailure{Code: "transport"}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return &mcpproxy.RemoteFailure{Code: "cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		return &mcpproxy.RemoteFailure{Code: "deadline"}
	}
	return &mcpproxy.RemoteFailure{Code: "transport"}
}

func replyCode(code int64) (string, bool) {
	switch code {
	case jsonrpc.CodeInvalidRequest:
		return "invalid_request", true
	case jsonrpc.CodeInvalidParams:
		return "invalid_params", true
	case jsonrpc.CodeMethodNotFound:
		return "method_not_found", true
	case jsonrpc.CodeInternalError:
		return "remote_internal", true
	case jsonrpc.CodeParseError:
		return "parse_error", true
	}
	return "", false
}

// Revoke erases the credential copy without any network request, so Close can
// then only end the session locally.
func (c *conn) Revoke() { c.rt.clear() }

// Close terminates the remote session and erases the credential copy.
func (c *conn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.cs.Close()
		c.rt.clear()
	})
	return err
}
