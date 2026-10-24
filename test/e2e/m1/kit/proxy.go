package kit

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// S3Forward lets the host-run server and a container reach one object-store
// origin by the same name. The server signs upload URLs with its configured
// S3 endpoint and hands them to the workload, so the endpoint must be the
// name a container resolves (the scope's LocalStack on the workload network);
// the host cannot resolve it, so the process sends its S3 requests through this
// forward proxy (HTTP_PROXY), which sends them to LocalStack's published
// port. The forward is hosted by the first test process (Main) and used by the
// child that runs the tests.
type S3Forward struct {
	// Origin is the endpoint as a workload resolves it.
	Origin string

	srv      *http.Server
	proxyURL string
}

var s3forward *S3Forward

// startS3Forward starts the forward proxy.
func startS3Forward() (*S3Forward, error) {
	dir := filepath.Dir(os.Getenv("KODAMA_SECRETS_DIR"))
	data, err := os.ReadFile(filepath.Join(dir, "endpoints.json"))
	if err != nil {
		return nil, fmt.Errorf("the scope's endpoints.json (workload URLs): %w", err)
	}
	var endpoints map[string]string
	if err := json.Unmarshal(data, &endpoints); err != nil {
		return nil, err
	}
	origin := endpoints["KODAMA_S3_ENDPOINT"]
	upstream, err := url.Parse(os.Getenv("KODAMA_S3_ENDPOINT"))
	if err != nil || origin == "" || upstream.Host == "" {
		return nil, fmt.Errorf("the scope names no object store (%q, %q)", origin, os.Getenv("KODAMA_S3_ENDPOINT"))
	}
	f := &S3Forward{Origin: origin}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			host := r.In.Host
			r.SetURL(&url.URL{Scheme: "http", Host: upstream.Host})
			r.Out.Host = host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	f.srv = &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
		}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() { _ = f.srv.Serve(ln) }()
	f.proxyURL = "http://" + ln.Addr().String()
	return f, nil
}
