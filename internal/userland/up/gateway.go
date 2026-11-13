package up

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const gatewayShutdownTimeout = 10 * time.Second

// GatewayConfig configures the single-origin HTTPS reverse-proxy gateway.
type GatewayConfig struct {
	// ServerUpstream is the Kodama control-plane upstream URL (required).
	ServerUpstream string
	// AuthUpstream is the OIDC identity provider upstream URL for /auth/*.
	// Either AuthUpstream or AuthHandler is required.
	AuthUpstream string
	// AuthHandler optionally serves /auth/* in-process when AuthUpstream is empty.
	AuthHandler http.Handler
	// StripAuthPrefix strips the leading /auth prefix before forwarding to AuthUpstream.
	StripAuthPrefix bool
	// GitUpstream is the optional git serve upstream URL for /git/*. When empty,
	// requests to /git/* return 503 Service Unavailable.
	GitUpstream string
	// KeepGitPrefix disables stripping the leading /git prefix when true. By
	// default (false), /git/repo.git/... is forwarded as /repo.git/... to git serve.
	KeepGitPrefix bool
	// UpstreamRootCAs trusts a custom CA pool (such as the local installation CA)
	// when dialing https:// upstreams.
	UpstreamRootCAs *x509.CertPool
	// Transport optionally overrides the HTTP RoundTripper used by every proxy.
	Transport http.RoundTripper
}

// Gateway routes a single public HTTPS origin by path prefix across Kodama's
// server, OIDC /auth/ issuer and /git/ smart-HTTP service without socat.
type Gateway struct {
	serverProxy *httputil.ReverseProxy
	authProxy   *httputil.ReverseProxy
	authHandler http.Handler
	gitProxy    *httputil.ReverseProxy
}

// NewGateway validates cfg and constructs the single-origin reverse proxy.
func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	serverURL, err := parseUpstreamURL("server upstream", cfg.ServerUpstream)
	if err != nil {
		return nil, err
	}
	var authURL *url.URL
	if strings.TrimSpace(cfg.AuthUpstream) != "" {
		authURL, err = parseUpstreamURL("auth upstream", cfg.AuthUpstream)
		if err != nil {
			return nil, err
		}
	} else if cfg.AuthHandler == nil {
		return nil, errors.New("gateway requires AuthUpstream or AuthHandler")
	}

	var gitURL *url.URL
	if strings.TrimSpace(cfg.GitUpstream) != "" {
		gitURL, err = parseUpstreamURL("git upstream", cfg.GitUpstream)
		if err != nil {
			return nil, err
		}
	}

	rt := cfg.Transport
	if rt == nil {
		baseTransport := http.DefaultTransport.(*http.Transport).Clone()
		baseTransport.ForceAttemptHTTP2 = true
		baseTransport.ResponseHeaderTimeout = 0 // allow long-lived SSE and git streams
		if cfg.UpstreamRootCAs != nil {
			baseTransport.TLSClientConfig = &tls.Config{
				RootCAs:    cfg.UpstreamRootCAs,
				MinVersion: tls.VersionTLS12,
			}
		}
		rt = baseTransport
	}

	gw := &Gateway{
		serverProxy: buildPrefixProxy(serverURL, "", rt),
		authHandler: cfg.AuthHandler,
	}
	if authURL != nil {
		strip := ""
		if cfg.StripAuthPrefix {
			strip = "/auth"
		}
		gw.authProxy = buildPrefixProxy(authURL, strip, rt)
	}
	if gitURL != nil {
		strip := "/git"
		if cfg.KeepGitPrefix {
			strip = ""
		}
		gw.gitProxy = buildPrefixProxy(gitURL, strip, rt)
	}
	return gw, nil
}

// ServeHTTP dispatches by path prefix:
//   - /git and /git/*  -> git serve upstream (stripping /git when configured)
//   - /auth and /auth/* -> OIDC issuer upstream
//   - everything else   -> kodama server upstream
func (gw *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/git" || strings.HasPrefix(path, "/git/"):
		if gw.gitProxy == nil {
			http.Error(w, `{"code":"dependency_unavailable","message":"git upstream is not configured"}`, http.StatusServiceUnavailable)
			return
		}
		if path == "/git" {
			target := "/git/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
			return
		}
		gw.gitProxy.ServeHTTP(w, r)

	case path == "/auth" || strings.HasPrefix(path, "/auth/"):
		if gw.authProxy != nil {
			gw.authProxy.ServeHTTP(w, r)
			return
		}
		gw.authHandler.ServeHTTP(w, r)

	default:
		gw.serverProxy.ServeHTTP(w, r)
	}
}

// ServeTLS serves the gateway over TLS on listener until ctx is canceled.
func (gw *Gateway) ServeTLS(ctx context.Context, listener net.Listener, tlsCfg *tls.Config) error {
	if listener == nil {
		return errors.New("gateway listener is nil")
	}
	if tlsCfg == nil {
		return errors.New("gateway TLS config is nil")
	}
	srv := &http.Server{
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
	}
	tlsListener := tls.NewListener(listener, tlsCfg)
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(tlsListener)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), gatewayShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func parseUpstreamURL(label, raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%s is empty", label)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return nil, fmt.Errorf("%s %q must be an absolute http:// or https:// URL", label, raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%s %q must use http or https", label, raw)
	}
	return parsed, nil
}

func buildPrefixProxy(target *url.URL, stripPrefix string, rt http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:     rt,
		FlushInterval: -1, // flush immediately for SSE and streaming git smart-HTTP
		Rewrite: func(pr *httputil.ProxyRequest) {
			inPath := pr.In.URL.Path
			inRawPath := pr.In.URL.RawPath
			if stripPrefix != "" {
				inPath, inRawPath = stripURLPrefix(inPath, inRawPath, stripPrefix)
			}

			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			basePath := strings.TrimRight(target.Path, "/")
			if basePath == "" {
				pr.Out.URL.Path = inPath
				pr.Out.URL.RawPath = inRawPath
			} else {
				pr.Out.URL.Path = basePath + inPath
				if inRawPath != "" {
					pr.Out.URL.RawPath = strings.TrimRight(target.EscapedPath(), "/") + inRawPath
				} else {
					pr.Out.URL.RawPath = ""
				}
			}
			pr.Out.Host = target.Host

			preserveAndExtendForwardedHeaders(pr, stripPrefix)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":"dependency_unavailable","message":"upstream service is unavailable"}`))
		},
	}
}

func stripURLPrefix(path, rawPath, prefix string) (string, string) {
	strippedPath := strings.TrimPrefix(path, prefix)
	if strippedPath == "" {
		strippedPath = "/"
	} else if !strings.HasPrefix(strippedPath, "/") {
		strippedPath = "/" + strippedPath
	}

	strippedRaw := ""
	if rawPath != "" && strings.HasPrefix(rawPath, prefix) {
		strippedRaw = strings.TrimPrefix(rawPath, prefix)
		if strippedRaw == "" {
			strippedRaw = "/"
		} else if !strings.HasPrefix(strippedRaw, "/") {
			strippedRaw = "/" + strippedRaw
		}
	}
	return strippedPath, strippedRaw
}

func preserveAndExtendForwardedHeaders(pr *httputil.ProxyRequest, strippedPrefix string) {
	// ReverseProxy clears X-Forwarded-* on pr.Out before calling Rewrite; copy
	// incoming X-Forwarded-* headers first so upstream callers never lose them.
	for name, values := range pr.In.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
			copied := make([]string, len(values))
			copy(copied, values)
			pr.Out.Header[name] = copied
		}
	}

	clientIP := pr.In.RemoteAddr
	if host, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil && host != "" {
		clientIP = host
	}
	if clientIP != "" {
		prior := pr.In.Header.Values("X-Forwarded-For")
		if len(prior) > 0 {
			pr.Out.Header.Set("X-Forwarded-For", strings.Join(prior, ", ")+", "+clientIP)
		} else {
			pr.Out.Header.Set("X-Forwarded-For", clientIP)
		}
	}

	if pr.Out.Header.Get("X-Forwarded-Host") == "" && pr.In.Host != "" {
		pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
	}
	if pr.Out.Header.Get("X-Forwarded-Proto") == "" {
		if pr.In.TLS != nil {
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		} else {
			pr.Out.Header.Set("X-Forwarded-Proto", "http")
		}
	}
	if strippedPrefix != "" && pr.Out.Header.Get("X-Forwarded-Prefix") == "" {
		pr.Out.Header.Set("X-Forwarded-Prefix", strippedPrefix)
	}
}
