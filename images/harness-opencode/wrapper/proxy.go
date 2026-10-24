package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
)

// proxy is the wrapper's loopback, credential-injecting reverse proxy. OpenCode
// is configured with plain-HTTP loopback endpoints and no credential; the proxy
// forwards exactly two route families to the platform over TLS, replacing any
// inbound Authorization with the attempt's *current* runtime token, re-read
// from the token file on every request so renewal needs no OpenCode restart
// and the token never appears in OpenCode's configuration, environment or
// transcript. It follows no redirects and uses no environment proxy.
//
//	POST {ModelPath}/chat/completions -> {platform}/gateway/v1/attempts/{attempt}/chat/completions
//	*    {MCPPath}{registration}      -> {platform}/internal/mcp/v1/attempts/{attempt}/servers/{registration}
//
// Only the registrations the pinned profile allows are routable.
type proxy struct {
	platform  *url.URL
	attempt   string
	tokenFile string
	allowed   map[string]bool
	onToken   func(string)
	rp        *httputil.ReverseProxy
	ln        net.Listener
	srv       *http.Server

	mu       sync.Mutex
	requests map[string]int
}

func newProxy(platform, attempt, tokenFile, caFile string, regs []string, onToken func(string)) (*proxy, error) {
	u, err := url.Parse(platform)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("the platform URL must be an https origin")
	}
	pool := x509.NewCertPool()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("the platform CA file is unreadable or holds no certificate")
		}
	} else if pool, err = x509.SystemCertPool(); err != nil {
		return nil, err
	}
	p := &proxy{platform: u, attempt: attempt, tokenFile: tokenFile, allowed: map[string]bool{}, onToken: onToken, requests: map[string]int{}}
	for _, r := range regs {
		p.allowed[r] = true
	}
	p.rp = &httputil.ReverseProxy{
		Director:      func(*http.Request) {},
		FlushInterval: -1,
		Transport: &http.Transport{
			Proxy:                 nil,
			TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			ResponseHeaderTimeout: 10 * time.Minute,
			DisableCompression:    true,
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	return p, nil
}

func (p *proxy) token() (string, error) {
	b, err := os.ReadFile(p.tokenFile)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("empty token file")
	}
	if p.onToken != nil {
		p.onToken(t)
	}
	return t, nil
}

func (p *proxy) count(route string) {
	p.mu.Lock()
	p.requests[route]++
	p.mu.Unlock()
}

// Requests returns how many requests each route family served.
func (p *proxy) Requests() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for k, v := range p.requests {
		out[k] = v
	}
	return out
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var target string
	var route string
	switch {
	case r.URL.Path == spec.ModelPath+"/chat/completions":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		target, route = "/gateway/v1/attempts/"+p.attempt+"/chat/completions", "model"
	case strings.HasPrefix(r.URL.Path, spec.MCPPath):
		reg := strings.TrimPrefix(r.URL.Path, spec.MCPPath)
		if reg == "" || strings.Contains(reg, "/") || !p.allowed[reg] {
			http.NotFound(w, r)
			return
		}
		target, route = "/internal/mcp/v1/attempts/"+p.attempt+"/servers/"+reg, "mcp"
	default:
		http.NotFound(w, r)
		return
	}
	tok, err := p.token()
	if err != nil {
		http.Error(w, "credential unavailable", http.StatusServiceUnavailable)
		return
	}
	body := r.Body
	length := r.ContentLength
	if route == "model" {
		// Measured mismatch (U-06): OpenCode 2.1.2 declares every tool with
		// "strict": false, a member the gateway's frozen
		// chat-completions-stream/1 subset does not accept. The wrapper drops
		// exactly that no-op (strict:false is the default) and nothing else; any
		// other body, including strict:true, is forwarded unchanged and judged
		// by the gateway.
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxModelBody+1))
		if err != nil || len(raw) > maxModelBody {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		raw = dropStrictFalse(raw)
		body, length = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	}
	out := r.Clone(r.Context())
	out.Body, out.ContentLength = body, length
	out.URL = &url.URL{Scheme: "https", Host: p.platform.Host, Path: target}
	out.Host = p.platform.Host
	out.RequestURI = ""
	// Only what the protocol needs travels on; everything credential-like or
	// hop-specific from the loopback client is dropped.
	hdr := http.Header{}
	for _, k := range []string{"Content-Type", "Accept", "Content-Length", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id"} {
		if v := r.Header.Values(k); len(v) > 0 {
			hdr[k] = v
		}
	}
	hdr.Set("Authorization", "Bearer "+tok)
	hdr.Set("User-Agent", "kodama-opencode-wrapper/"+spec.WrapperVersion)
	out.Header = hdr
	p.count(route)
	p.rp.ServeHTTP(w, out)
}

func (p *proxy) start() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", spec.ProxyHost, spec.ProxyPort))
	if err != nil {
		return fmt.Errorf("the loopback proxy address is unavailable: %w", err)
	}
	p.ln = ln
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

func (p *proxy) stop() {
	if p.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = p.srv.Shutdown(ctx)
	_ = p.srv.Close()
}

const maxModelBody = 4 << 20

// dropStrictFalse removes "strict": false from every tools[].function object.
// A body that does not parse, or has no such member, is returned unchanged.
func dropStrictFalse(raw []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return raw
	}
	toolsRaw, ok := top["tools"]
	if !ok {
		return raw
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(toolsRaw, &tools) != nil {
		return raw
	}
	changed := false
	for _, t := range tools {
		var fn map[string]json.RawMessage
		fr, ok := t["function"]
		if !ok || json.Unmarshal(fr, &fn) != nil {
			continue
		}
		if v, ok := fn["strict"]; ok && string(bytes.TrimSpace(v)) == "false" {
			delete(fn, "strict")
			nb, err := json.Marshal(fn)
			if err != nil {
				return raw
			}
			t["function"] = nb
			changed = true
		}
	}
	if !changed {
		return raw
	}
	nt, err := json.Marshal(tools)
	if err != nil {
		return raw
	}
	top["tools"] = nt
	out, err := json.Marshal(top)
	if err != nil {
		return raw
	}
	return out
}
