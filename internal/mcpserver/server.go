package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimiware/kodama/api"
	identityoidc "github.com/optimiware/kodama/internal/identity/oidc"
)

const (
	// ProtocolRevision is the single MCP revision this server advertises, the
	// same revision the in-product MCP proxy pins.
	ProtocolRevision = "2025-11-25"
	// ImplementationVersion is the MCP implementation name's version.
	ImplementationVersion = "0.1.0"
)

// Server is the external MCP endpoint over the Kodama public API.
type Server struct {
	cfg        Config
	operations []Operation
	executor   *Executor
	auth       *Authenticator
	sdk        *mcp.Server
}

// New builds the server, its tool registry and its OAuth resource-server
// authenticator. It reads the embedded public OpenAPI document; a route added
// there without a generated tool is exposed automatically.
func New(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	apiCA, err := ReadCA(cfg.APICAFile)
	if err != nil {
		return nil, err
	}
	oidcCA, err := ReadCA(cfg.OIDCCAFile)
	if err != nil {
		return nil, err
	}
	exchangeCA, err := ReadCA(cfg.ExchangeCAFile)
	if err != nil {
		return nil, err
	}
	secret, err := ReadSecret(cfg.ExchangeClientSecretFile)
	if err != nil {
		return nil, err
	}
	apiClient, err := httpClient(cfg.HTTPTimeout, oidcCA)
	if err != nil {
		return nil, err
	}
	apiVerifier, err := identityoidc.NewVerifier(identityoidc.Config{
		Issuer: cfg.APIIssuer, Audience: cfg.APIAudience, AllowInsecureHTTP: cfg.AllowInsecureHTTP,
		HTTPClient: apiClient,
	})
	if err != nil {
		return nil, fmt.Errorf("mcpserver: API OIDC verifier: %w", err)
	}
	exchangeClient, err := httpClient(cfg.HTTPTimeout, exchangeCA)
	if err != nil {
		return nil, err
	}
	provider, err := NewExchangeProvider(cfg, exchangeClient, secret, apiVerifier)
	if err != nil {
		return nil, err
	}
	executor, err := NewExecutor(cfg.APIURL, cfg.HTTPTimeout, cfg.MaxResponseBytes, apiCA, provider, cfg.DefaultTenant, cfg.DefaultProject)
	if err != nil {
		return nil, err
	}
	authenticator, err := NewAuthenticator(cfg, oidcCA)
	if err != nil {
		return nil, err
	}
	operations, err := BuildOperations(api.OpenAPI)
	if err != nil {
		return nil, err
	}
	operations, err = filterOperations(operations, cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, operations: operations, executor: executor, auth: authenticator}
	s.sdk = mcp.NewServer(&mcp.Implementation{Name: "kodama-mcp", Version: ImplementationVersion}, &mcp.ServerOptions{
		SupportedProtocolVersions: []string{ProtocolRevision},
		Capabilities:              &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	for _, op := range operations {
		s.addTool(op)
	}
	return s, nil
}

func filterOperations(operations []Operation, cfg Config) ([]Operation, error) {
	allowed := map[string]bool{}
	for _, name := range cfg.Toolsets {
		allowed[name] = true
	}
	out := make([]Operation, 0, len(operations))
	for _, op := range operations {
		if cfg.ReadOnly && !op.ReadOnly {
			continue
		}
		if op.Sensitive && !cfg.EnableSecretWrites {
			continue
		}
		if op.Credential && !cfg.EnableCredentialTools {
			continue
		}
		if len(allowed) > 0 && !allowed[op.Toolset] {
			continue
		}
		out = append(out, op)
	}
	if len(out) == 0 {
		return nil, errors.New("mcpserver: the selected toolset exposes no tools")
	}
	return out, nil
}

func (s *Server) addTool(op Operation) {
	tool := &mcp.Tool{
		Name: op.Tool, Title: titleFor(op), Description: descriptionFor(op),
		InputSchema: op.InputSchema,
		Annotations: &mcp.ToolAnnotations{
			Title:           titleFor(op),
			ReadOnlyHint:    op.ReadOnly,
			DestructiveHint: boolPtr(op.Destructive),
			IdempotentHint:  op.Idempotent,
			OpenWorldHint:   boolPtr(false),
		},
	}
	s.sdk.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return toolError("The tool arguments are not a JSON object.", "invalid_arguments"), nil
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		result, err := s.executor.Call(ctx, op, args)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return toolError(err.Error(), "mcp_error"), nil
		}
		out := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: result.Text}},
			IsError: result.IsError,
		}
		if len(result.Meta) > 0 {
			out.Meta = mcp.Meta(result.Meta)
		}
		if result.Structured != nil && !result.IsError {
			out.StructuredContent = result.Structured
		}
		return out, nil
	})
}

func toolError(text, code string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		Meta:    mcp.Meta{"kodama/code": code},
	}
}

func titleFor(op Operation) string {
	if op.Summary != "" {
		return op.Summary
	}
	return op.Tool
}

func descriptionFor(op Operation) string {
	var b strings.Builder
	if op.Summary != "" {
		b.WriteString(op.Summary)
		b.WriteString(". ")
	}
	if op.Description != "" {
		b.WriteString(op.Description)
		b.WriteString(" ")
	}
	fmt.Fprintf(&b, "Kodama API operation %s (%s %s).", op.ID, op.Method, op.Path)
	if op.Streaming {
		b.WriteString(" This tool returns a bounded snapshot; follow/streaming is not supported over MCP.")
	}
	if op.Sensitive {
		b.WriteString(" This operation carries secret material in its arguments; the server must be started with --enable-secret-writes.")
	}
	return b.String()
}

func boolPtr(v bool) *bool { return &v }

// Handler returns the HTTP surface: OAuth discovery, the MCP endpoint and
// process probes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, path := range s.auth.MetadataPaths() {
		mux.Handle(path, s.auth.MetadataHandler())
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.sdk }, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: s.cfg.MaxResponseBytes,
	})
	mux.Handle("/mcp", s.auth.Middleware(mcpHandler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	return mux
}

// Run listens until the context is cancelled, then drains the HTTP server.
func (s *Server) Run(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("mcpserver: listen: %w", err)
	}
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("mcp server listening", "address", listener.Addr().String(), "resource", s.cfg.Resource, "tools", len(s.operations))
		errCh <- server.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Operations returns the exposed operations for tests and diagnostics.
func (s *Server) Operations() []Operation { return append([]Operation(nil), s.operations...) }
