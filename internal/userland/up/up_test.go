package up

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
)

func TestDetectRuntimeDeterministicOrderAndOverrides(t *testing.T) {
	t.Run("explicit --socket wins over env and candidates", func(t *testing.T) {
		home := "/Users/alice"
		got, err := DetectRuntime(context.Background(), DetectOptions{
			ExplicitSocket: "unix:///custom/orb.sock",
			GOOS:           "darwin",
			HomeDir:        home,
			Getenv: func(k string) string {
				if k == "DOCKER_HOST" {
					return "unix:///env/docker.sock"
				}
				return ""
			},
			StatSocket: func(p string) bool { return p == "/custom/orb.sock" },
			PingSocket: func(_ context.Context, p string) (RuntimeKind, error) {
				if p == "/custom/orb.sock" {
					return KindDocker, nil
				}
				return "", fmt.Errorf("unexpected %s", p)
			},
		})
		if err != nil {
			t.Fatalf("DetectRuntime failed: %v", err)
		}
		if got.SocketPath != "/custom/orb.sock" || got.Kind != KindDocker || got.HostAlias != "host.docker.internal" {
			t.Fatalf("unexpected runtime: %+v", got)
		}
	})

	t.Run("CONTAINER_HOST and DOCKER_HOST precedence", func(t *testing.T) {
		got, err := DetectRuntime(context.Background(), DetectOptions{
			GOOS:    "linux",
			HomeDir: "/home/dev",
			Getenv: func(k string) string {
				switch k {
				case "CONTAINER_HOST":
					return "unix:///run/user/1000/podman/podman.sock"
				case "DOCKER_HOST":
					return "unix:///var/run/docker.sock"
				default:
					return ""
				}
			},
			StatSocket: func(string) bool { return true },
			PingSocket: func(_ context.Context, p string) (RuntimeKind, error) {
				if strings.Contains(p, "podman") {
					return KindPodman, nil
				}
				return KindDocker, nil
			},
		})
		if err != nil {
			t.Fatalf("DetectRuntime failed: %v", err)
		}
		if got.SocketPath != "/run/user/1000/podman/podman.sock" || got.Kind != KindPodman || got.HostAlias != "host.containers.internal" {
			t.Fatalf("unexpected runtime: %+v", got)
		}
	})

	t.Run("macOS candidate order covers Podman Machine subdirs, OrbStack, Docker Desktop, Colima", func(t *testing.T) {
		home := t.TempDir()
		machineDir := filepath.Join(home, ".local", "share", "containers", "podman", "machine")
		if err := os.MkdirAll(filepath.Join(machineDir, "applehv"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(machineDir, "libkrun", "podman-machine-default"), 0o755); err != nil {
			t.Fatal(err)
		}

		paths := CandidateSocketPaths(DetectOptions{
			GOOS:        "darwin",
			HomeDir:     home,
			Getenv:      func(string) string { return "" },
			UserTempDir: func() string { return "" },
			ReadDir:     onlyUnder(home),
		})
		wantOrder := []string{
			filepath.Join(machineDir, "podman.sock"),
			filepath.Join(machineDir, "applehv", "podman.sock"),
			filepath.Join(machineDir, "libkrun", "podman.sock"),
			filepath.Join(machineDir, "libkrun", "podman-machine-default", "podman.sock"),
			filepath.Join(home, ".orbstack", "run", "docker.sock"),
			filepath.Join(home, ".docker", "run", "docker.sock"),
			filepath.Join(home, ".colima", "default", "docker.sock"),
			"/run/podman/podman.sock",
			"/var/run/docker.sock",
		}
		if len(paths) != len(wantOrder) {
			t.Fatalf("candidate count = %d, want %d\ngot:  %v\nwant: %v", len(paths), len(wantOrder), paths, wantOrder)
		}
		for i := range wantOrder {
			if paths[i] != wantOrder[i] {
				t.Fatalf("candidate[%d] = %q, want %q", i, paths[i], wantOrder[i])
			}
		}

		// When only OrbStack is running, OrbStack is selected.
		orbSock := filepath.Join(home, ".orbstack", "run", "docker.sock")
		got, err := DetectRuntime(context.Background(), DetectOptions{
			GOOS:        "darwin",
			HomeDir:     home,
			Getenv:      func(string) string { return "" },
			UserTempDir: func() string { return "" },
			ReadDir:     onlyUnder(home),
			StatSocket:  func(p string) bool { return p == orbSock || p == "/var/run/docker.sock" },
			PingSocket: func(_ context.Context, p string) (RuntimeKind, error) {
				if p == orbSock {
					return KindDocker, nil
				}
				return "", fmt.Errorf("not running")
			},
		})
		if err != nil {
			t.Fatalf("DetectRuntime OrbStack failed: %v", err)
		}
		if got.Provider != "orbstack" || got.SocketPath != orbSock || got.Kind != KindDocker {
			t.Fatalf("got %+v, want orbstack", got)
		}
	})

	t.Run("Podman Machine API sockets under the temporary directories follow the machine directory", func(t *testing.T) {
		home := t.TempDir()
		// The user's TMPDIR, and a stand-in for /tmp/podman, where Podman puts
		// the socket when it runs without TMPDIR.
		tmp := t.TempDir()
		podmanDir := filepath.Join(tmp, "podman")
		slashTmpPodman := t.TempDir()
		for dir, names := range map[string][]string{
			podmanDir:      {"podman-machine-default-api.sock", "podman-machine-default-gvproxy.sock", "other-api.sock", "gvproxy.log"},
			slashTmpPodman: {"podman-machine-default-api.sock"},
		} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		readDir := func(dir string) ([]os.DirEntry, error) {
			if dir == "/tmp/podman" {
				return os.ReadDir(slashTmpPodman)
			}
			return os.ReadDir(dir)
		}
		candidates := func(goos, tmpDir string) []string {
			return CandidateSocketPaths(DetectOptions{
				GOOS:        goos,
				HomeDir:     home,
				ReadDir:     readDir,
				UserTempDir: func() string { return tmp + "/" },
				Getenv: func(k string) string {
					if k == "TMPDIR" {
						return tmpDir
					}
					return ""
				},
			})
		}
		machineSocket := filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock")
		orbStack := filepath.Join(home, ".orbstack", "run", "docker.sock")
		for _, tc := range []struct {
			name, goos, tmpDir string
			want               []string
		}{
			{"with TMPDIR", "darwin", tmp + "/", []string{
				machineSocket,
				filepath.Join(podmanDir, "other-api.sock"),
				filepath.Join(podmanDir, "podman-machine-default-api.sock"),
				"/tmp/podman/podman-machine-default-api.sock",
				orbStack,
			}},
			{"without TMPDIR, from the per-user temporary directory", "darwin", "", []string{
				machineSocket,
				filepath.Join(podmanDir, "other-api.sock"),
				filepath.Join(podmanDir, "podman-machine-default-api.sock"),
				"/tmp/podman/podman-machine-default-api.sock",
				orbStack,
			}},
			{"on Linux, where a machine socket is not there", "linux", tmp, []string{
				machineSocket,
				orbStack,
			}},
		} {
			if got := candidates(tc.goos, tc.tmpDir); len(got) < len(tc.want) || !slices.Equal(got[:len(tc.want)], tc.want) {
				t.Errorf("%s: candidates = %v, want them to start with %v", tc.name, got, tc.want)
			}
		}
	})

	t.Run("the recorded socket follows the explicit overrides and precedes the candidates", func(t *testing.T) {
		recorded := DetectedRuntime{Kind: KindPodman, Provider: "--socket", SocketPath: "/recorded/podman.sock"}
		detect := func(explicit string, reachable ...string) (DetectedRuntime, error) {
			return DetectRuntime(context.Background(), DetectOptions{
				ExplicitSocket: explicit,
				Recorded:       recorded,
				GOOS:           "linux",
				HomeDir:        "/home/dev",
				Getenv: func(k string) string {
					if k == "XDG_RUNTIME_DIR" {
						return "/run/user/1000"
					}
					return ""
				},
				ReadDir:    func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist },
				StatSocket: func(p string) bool { return slices.Contains(reachable, p) },
				PingSocket: func(context.Context, string) (RuntimeKind, error) { return KindPodman, nil },
			})
		}
		candidate := "/run/user/1000/podman/podman.sock"

		got, err := detect("", recorded.SocketPath, candidate)
		if err != nil || got != (DetectedRuntime{Kind: KindPodman, Provider: "--socket", SocketPath: recorded.SocketPath, HostAlias: "host.containers.internal"}) {
			t.Fatalf("with a reachable recorded socket: %+v, %v", got, err)
		}
		got, err = detect("/explicit/podman.sock", "/explicit/podman.sock", recorded.SocketPath)
		if err != nil || got.SocketPath != "/explicit/podman.sock" {
			t.Fatalf("with --socket: %+v, %v", got, err)
		}
		got, err = detect("", candidate)
		if err != nil || got.SocketPath != candidate || got.Provider != "podman-rootless" {
			t.Fatalf("with a recorded socket that is gone: %+v, %v", got, err)
		}
	})

	t.Run("Linux XDG_RUNTIME_DIR rootless Podman precedes system sockets", func(t *testing.T) {
		paths := CandidateSocketPaths(DetectOptions{
			GOOS:    "linux",
			HomeDir: "/home/dev",
			Getenv: func(k string) string {
				if k == "XDG_RUNTIME_DIR" {
					return "/run/user/1001"
				}
				return ""
			},
			ReadDir: func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist },
		})
		if len(paths) == 0 || paths[0] != "/run/user/1001/podman/podman.sock" {
			t.Fatalf("first candidate = %v, want /run/user/1001/podman/podman.sock", paths)
		}
	})

	t.Run("live Unix socket ping distinguishes Podman vs Docker", func(t *testing.T) {
		// Short path under /tmp so macOS/Linux sun_path stays well under 104 bytes.
		sockDir, err := os.MkdirTemp("/tmp", "kdm-sock-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

		podmanSock := filepath.Join(sockDir, "podman.sock")
		ln, err := net.Listen("unix", podmanSock)
		if err != nil {
			t.Fatalf("listen unix: %v", err)
		}
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_ping" {
					w.Header().Set("Libpod-Api-Version", "5.4.0")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("OK"))
					return
				}
				http.NotFound(w, r)
			}),
		}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })

		got, err := DetectRuntime(context.Background(), DetectOptions{
			ExplicitSocket: podmanSock,
			GOOS:           "linux",
		})
		if err != nil {
			t.Fatalf("live unix DetectRuntime failed: %v", err)
		}
		if got.Kind != KindPodman || got.HostAlias != "host.containers.internal" {
			t.Fatalf("expected Podman from Libpod-Api-Version header, got %+v", got)
		}
	})

	t.Run("platform-specific actionable remediation when no socket is reachable", func(t *testing.T) {
		_, darwinErr := DetectRuntime(context.Background(), DetectOptions{
			GOOS:       "darwin",
			HomeDir:    "/Users/alice",
			Getenv:     func(string) string { return "" },
			ReadDir:    func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist },
			StatSocket: func(string) bool { return false },
		})
		if darwinErr == nil || !strings.Contains(darwinErr.Error(), "orb start") || !strings.Contains(darwinErr.Error(), "podman machine start") {
			t.Fatalf("darwin error missing macOS remediation: %v", darwinErr)
		}

		_, linuxErr := DetectRuntime(context.Background(), DetectOptions{
			GOOS:       "linux",
			HomeDir:    "/home/alice",
			Getenv:     func(string) string { return "" },
			ReadDir:    func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist },
			StatSocket: func(string) bool { return false },
		})
		if linuxErr == nil || !strings.Contains(linuxErr.Error(), "systemctl --user enable --now podman.socket") {
			t.Fatalf("linux error missing systemd remediation: %v", linuxErr)
		}
	})
}

func TestResolveStateDirAndPurgeSafety(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Users", "alice")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	gotDefault, err := ResolveStateDir(StateDirOptions{
		GOOS:         "darwin",
		HomeDir:      home,
		InstanceName: "dev-stack",
	})
	if err != nil {
		t.Fatalf("ResolveStateDir default failed: %v", err)
	}
	wantDefault := filepath.Join(evalExistingAncestors(home), ".kodema", "instances", "dev-stack")
	if gotDefault != wantDefault {
		t.Fatalf("default state dir = %q, want %q", gotDefault, wantDefault)
	}

	// Refuse /tmp, /var/tmp, and non-$HOME paths on darwin unless AllowUnsharedStateDir is set.
	for _, unshared := range []string{"/tmp/kodema-test", "/var/tmp/kodema-test", "/opt/kodema"} {
		_, err := ResolveStateDir(StateDirOptions{
			GOOS:             "darwin",
			HomeDir:          home,
			InstanceName:     "default",
			ExplicitStateDir: unshared,
		})
		if err == nil || !strings.Contains(err.Error(), "--allow-unshared-state-dir") {
			t.Fatalf("expected darwin virtiofs refusal for %q, got %v", unshared, err)
		}

		allowed, err := ResolveStateDir(StateDirOptions{
			GOOS:                  "darwin",
			HomeDir:               home,
			InstanceName:          "default",
			ExplicitStateDir:      unshared,
			AllowUnsharedStateDir: true,
		})
		if err != nil || allowed == "" {
			t.Fatalf("expected --allow-unshared-state-dir to permit %q, got %v", unshared, err)
		}
	}

	// Refuse degenerate paths on all platforms.
	for _, bad := range []string{"/", "relative/path", home, filepath.Dir(home)} {
		if _, err := ResolveStateDir(StateDirOptions{
			GOOS:             "linux",
			HomeDir:          home,
			InstanceName:     "default",
			ExplicitStateDir: bad,
		}); err == nil {
			t.Fatalf("expected degenerate state dir %q to be refused", bad)
		}
	}

	// Marker and safe purge behavior: unknown files keep marker intact so a retry works.
	stateDir := filepath.Join(home, ".kodema", "instances", "alpha")
	if err := EnsureInstanceStateDir(stateDir, "alpha"); err != nil {
		t.Fatalf("EnsureInstanceStateDir: %v", err)
	}
	if err := EnsureInstanceStateDir(stateDir, "beta"); err == nil {
		t.Fatal("expected foreign instance marker refusal")
	}
	userFile := filepath.Join(stateDir, "notes.txt")
	if err := os.WriteFile(userFile, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PurgeInstanceStateDir(stateDir, "alpha"); err == nil {
		t.Fatal("PurgeInstanceStateDir must refuse unknown user files")
	}
	if err := os.Remove(userFile); err != nil {
		t.Fatal(err)
	}
	// The quickstart has the reader store a Git credential in the instance.
	if err := os.WriteFile(filepath.Join(stateDir, "git-credentials"), []byte("https://kodema:token@127.0.0.1%3a8443\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PurgeInstanceStateDir(stateDir, "alpha"); err != nil {
		t.Fatalf("second PurgeInstanceStateDir after removing unknown file failed: %v", err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("stateDir still exists after purge: %v", err)
	}
}

func TestEnsureTLSStrictRFC5280AndReuse(t *testing.T) {
	tlsDir := filepath.Join(t.TempDir(), "tls")
	first, err := EnsureTLS(TLSConfig{
		Dir:   tlsDir,
		Hosts: []string{"127.0.0.1:8443", "kodema.local"},
	})
	if err != nil {
		t.Fatalf("EnsureTLS first call failed: %v", err)
	}
	if first.ReusedCA || first.ReusedLeaf {
		t.Fatalf("first generation reported reuse: %+v", first)
	}

	// Verify 0600 private key permissions.
	for _, keyPath := range []string{first.CAKeyPath, first.LeafKeyPath} {
		info, statErr := os.Stat(keyPath)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %04o, want 0600", keyPath, info.Mode().Perm())
		}
	}

	// Parse CA and check strict RFC 5280 extensions: BasicConstraints CA:TRUE + critical KeyUsage(keyCertSign|cRLSign).
	caBlock, _ := pem.Decode(first.CACertPEM)
	if caBlock == nil {
		t.Fatal("CA PEM failed to decode")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	if !caCert.IsCA || !caCert.BasicConstraintsValid {
		t.Fatal("CA certificate missing BasicConstraints CA:TRUE")
	}
	wantUsage := x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	if caCert.KeyUsage&wantUsage != wantUsage {
		t.Fatalf("CA KeyUsage = %v, want keyCertSign|cRLSign", caCert.KeyUsage)
	}
	keyUsageOID := asn1.ObjectIdentifier{2, 5, 29, 15}
	foundCriticalKU := false
	for _, ext := range caCert.Extensions {
		if ext.Id.Equal(keyUsageOID) && ext.Critical {
			foundCriticalKU = true
		}
	}
	if !foundCriticalKU {
		t.Fatal("CA KeyUsage extension is not marked critical (required by strict RFC 5280 verifiers)")
	}
	if len(caCert.SubjectKeyId) == 0 {
		t.Fatal("CA certificate has no SubjectKeyIdentifier")
	}

	// Verify leaf chain and SANs.
	leafBytes, err := os.ReadFile(first.LeafCertPath)
	if err != nil {
		t.Fatal(err)
	}
	leafBlock, _ := pem.Decode(leafBytes)
	leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := first.CertPool()
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "host.containers.internal", "host.docker.internal", "kodema.local"} {
		if _, err := leafCert.Verify(x509.VerifyOptions{
			DNSName:   host,
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Fatalf("leaf failed verification for %q: %v", host, err)
		}
	}

	// Second call with identical SANs must reuse both CA and leaf.
	second, err := EnsureTLS(TLSConfig{
		Dir:   tlsDir,
		Hosts: []string{"127.0.0.1:8443", "kodema.local"},
	})
	if err != nil {
		t.Fatalf("EnsureTLS second call failed: %v", err)
	}
	if !second.ReusedCA || !second.ReusedLeaf {
		t.Fatalf("expected CA and leaf reuse on restart, got %+v", second)
	}
}

func TestSingleOriginGatewayRoutingHeadersAndSSE(t *testing.T) {
	type observedReq struct {
		upstream string
		method   string
		path     string
		rawQuery string
		auth     string
		ctype    string
		xff      string
		xfh      string
		xfp      string
		xfPrefix string
	}
	var (
		mu       sync.Mutex
		observed []observedReq
	)
	record := func(name string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, observedReq{
			upstream: name,
			method:   r.Method,
			path:     r.URL.Path,
			rawQuery: r.URL.RawQuery,
			auth:     r.Header.Get("Authorization"),
			ctype:    r.Header.Get("Content-Type"),
			xff:      r.Header.Get("X-Forwarded-For"),
			xfh:      r.Header.Get("X-Forwarded-Host"),
			xfp:      r.Header.Get("X-Forwarded-Proto"),
			xfPrefix: r.Header.Get("X-Forwarded-Prefix"),
		})
	}

	sseChunkSent := make(chan struct{})
	releaseSSE := make(chan struct{})

	serverUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("server", r)
		if r.URL.Path == "/api/v1/events/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: first-event\n\n")
			flusher.Flush()
			close(sseChunkSent)
			<-releaseSSE
			_, _ = io.WriteString(w, "data: second-event\n\n")
			flusher.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"upstream":"server","path":%q}`, r.URL.Path)
	}))
	defer serverUpstream.Close()

	authUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("auth", r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"upstream":"auth","path":%q}`, r.URL.Path)
	}))
	defer authUpstream.Close()

	gitUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("git", r)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = fmt.Fprintf(w, "git:%s?%s", r.URL.Path, r.URL.RawQuery)
	}))
	defer gitUpstream.Close()

	tlsBundle, err := EnsureTLS(TLSConfig{Dir: filepath.Join(t.TempDir(), "tls")})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := tlsBundle.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := tlsBundle.CertPool()
	if err != nil {
		t.Fatal(err)
	}

	gw, err := NewGateway(GatewayConfig{
		ServerUpstream: serverUpstream.URL,
		AuthHandler:    authUpstream.Config.Handler,
		GitUpstream:    gitUpstream.URL,
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- gw.ServeTLS(ctx, ln, tlsCfg)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	baseURL := "https://" + ln.Addr().String()
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
		Timeout: 10 * time.Second,
	}

	// 1. Route /git/* -> git upstream with /git stripped and Authorization preserved.
	// The gateway is the edge: X-Forwarded-* sent by the client is dropped.
	gitReq, _ := http.NewRequest(http.MethodGet, baseURL+"/git/demo.git/info/refs?service=git-upload-pack", nil)
	gitReq.Header.Set("Authorization", "Basic Z2l0OnRva2Vu")
	gitReq.Header.Set("X-Forwarded-For", "203.0.113.9")
	gitReq.Header.Set("X-Forwarded-Host", "spoofed.example")
	gitReq.Header.Set("X-Forwarded-Proto", "http")
	gitReq.Header.Set("X-Forwarded-Prefix", "/spoofed")
	gitResp, err := client.Do(gitReq)
	if err != nil {
		t.Fatalf("GET /git/... failed: %v", err)
	}
	gitBody, _ := io.ReadAll(gitResp.Body)
	_ = gitResp.Body.Close()
	if string(gitBody) != "git:/demo.git/info/refs?service=git-upload-pack" {
		t.Fatalf("unexpected git response: %q", string(gitBody))
	}

	// 2. Route /auth/* -> auth upstream.
	authResp, err := client.Get(baseURL + "/auth/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET /auth/... failed: %v", err)
	}
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("auth status = %d", authResp.StatusCode)
	}

	// 3. Route /api/v1/*, /mcp, /llm/*, /.well-known/*, /healthz, /readyz -> server upstream.
	for _, p := range []string{
		"/api/v1/projects/p1/executions",
		"/mcp",
		"/llm/v1/chat/completions",
		"/.well-known/kodema-configuration",
		"/healthz",
		"/readyz",
	} {
		req, _ := http.NewRequest(http.MethodPost, baseURL+p, strings.NewReader(`{"ok":true}`))
		req.Header.Set("Authorization", "Bearer test-jwt")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %s failed: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %s status = %d", p, resp.StatusCode)
		}
	}

	// 4. Verify immediate SSE streaming before upstream closes the response.
	sseResp, err := client.Get(baseURL + "/api/v1/events/stream")
	if err != nil {
		t.Fatalf("SSE GET failed: %v", err)
	}
	<-sseChunkSent
	reader := bufio.NewReader(sseResp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first SSE line while stream still open: %v", err)
	}
	if strings.TrimSpace(line) != "data: first-event" {
		t.Fatalf("first streamed SSE line = %q, want data: first-event", line)
	}
	close(releaseSSE)
	_, _ = io.ReadAll(reader)
	_ = sseResp.Body.Close()

	// 5. A path that is not clean is refused; a trailing slash is fine.
	for p, want := range map[string]int{
		"/git/../auth/token": http.StatusBadRequest,
		"/auth//token":       http.StatusBadRequest,
		"/git/":              http.StatusOK,
	} {
		resp, err := client.Get(baseURL + p)
		if err != nil {
			t.Fatalf("GET %s failed: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("GET %s status = %d, want %d", p, resp.StatusCode, want)
		}
	}

	// Verify recorded upstream headers.
	mu.Lock()
	defer mu.Unlock()
	if len(observed) < 8 {
		t.Fatalf("expected >= 8 upstream requests, got %d", len(observed))
	}
	firstGit := observed[0]
	if firstGit.upstream != "git" || firstGit.path != "/demo.git/info/refs" || firstGit.xfPrefix != "/git" {
		t.Fatalf("unexpected git upstream observation: %+v", firstGit)
	}
	if firstGit.auth != "Basic Z2l0OnRva2Vu" || firstGit.xff != "127.0.0.1" || firstGit.xfh != ln.Addr().String() || firstGit.xfp != "https" {
		t.Fatalf("git forwarded headers are not the gateway's own: %+v", firstGit)
	}
	if observed[1].upstream != "auth" || observed[1].path != "/auth/.well-known/openid-configuration" {
		t.Fatalf("unexpected auth upstream observation: %+v", observed[1])
	}
	if observed[2].upstream != "server" || observed[2].auth != "Bearer test-jwt" || observed[2].ctype != "application/json" {
		t.Fatalf("unexpected server upstream observation: %+v", observed[2])
	}
}

func TestDevIssuerPKCEAndDeviceFlow(t *testing.T) {
	var srv *httptest.Server
	var issuer *DevIssuer
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer.ServeHTTP(w, r)
	}))
	defer srv.Close()

	keyPath := filepath.Join(t.TempDir(), "oidc.key")
	var err error
	issuer, err = NewDevIssuer(DevIssuerConfig{
		Issuer:    srv.URL + "/auth",
		Audience:  srv.URL,
		APIURL:    srv.URL,
		ClientID:  "kodactl",
		KeyPath:   keyPath,
		TenantID:  "tenant-local",
		ProjectID: "project-local",
	})
	if err != nil {
		t.Fatalf("NewDevIssuer: %v", err)
	}

	noRedirectClient := &http.Client{
		Transport: srv.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// PKCE S256 + dynamic loopback port (e.g. 127.0.0.1:59123/callback).
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirectURI := "http://127.0.0.1:59123/callback"

	authURL, _ := url.Parse(srv.URL + "/auth/authorize")
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", "kodactl")
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", "xyz-state")
	q.Set("nonce", "abc-nonce")
	authURL.RawQuery = q.Encode()

	authResp, err := noRedirectClient.Get(authURL.String())
	if err != nil {
		t.Fatalf("authorize GET: %v", err)
	}
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", authResp.StatusCode)
	}
	loc, err := url.Parse(authResp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Query().Get("state") != "xyz-state" || loc.Query().Get("code") == "" {
		t.Fatalf("unexpected callback location: %s", loc)
	}
	code := loc.Query().Get("code")

	tokenResp, err := srv.Client().PostForm(srv.URL+"/auth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"kodactl"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatalf("token POST: %v", err)
	}
	var tokPayload struct {
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokPayload); err != nil {
		t.Fatal(err)
	}
	_ = tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK || tokPayload.AccessToken == "" || tokPayload.IDToken == "" || tokPayload.RefreshToken == "" {
		t.Fatalf("token exchange failed: status=%d payload=%+v", tokenResp.StatusCode, tokPayload)
	}

	// An oversized authorize form is refused, not truncated and accepted.
	oversized := q.Encode() + "&pad=" + strings.Repeat("a", maxOIDCFormBytes)
	bigResp, err := noRedirectClient.Post(srv.URL+"/auth/authorize", "application/x-www-form-urlencoded", strings.NewReader(oversized))
	if err != nil {
		t.Fatal(err)
	}
	_ = bigResp.Body.Close()
	if bigResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized authorize POST status = %d, want 400", bigResp.StatusCode)
	}

	// Replaying the single-use authorization code must fail.
	replayResp, err := srv.Client().PostForm(srv.URL+"/auth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"kodactl"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = replayResp.Body.Close()
	if replayResp.StatusCode == http.StatusOK {
		t.Fatal("replayed authorization code was accepted")
	}

	// Device flow: pending -> approve -> token issued.
	devResp, err := srv.Client().PostForm(srv.URL+"/auth/device", url.Values{
		"client_id": {"kodactl"},
		"scope":     {"openid profile"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var devStart struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	_ = json.NewDecoder(devResp.Body).Decode(&devStart)
	_ = devResp.Body.Close()
	if devStart.DeviceCode == "" || devStart.UserCode == "" {
		t.Fatalf("device start incomplete: %+v", devStart)
	}

	pendingResp, err := srv.Client().PostForm(srv.URL+"/auth/token", url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {"kodactl"},
		"device_code": {devStart.DeviceCode},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = pendingResp.Body.Close()
	if pendingResp.StatusCode == http.StatusOK {
		t.Fatal("unapproved device_code returned a token")
	}

	approveResp, err := srv.Client().PostForm(srv.URL+"/auth/device", url.Values{
		"user_code": {devStart.UserCode},
		"persona":   {"engineer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("device approve failed: %d", approveResp.StatusCode)
	}

	approvedTokResp, err := srv.Client().PostForm(srv.URL+"/auth/token", url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {"kodactl"},
		"device_code": {devStart.DeviceCode},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = approvedTokResp.Body.Close()
	if approvedTokResp.StatusCode != http.StatusOK {
		t.Fatalf("approved device token exchange failed: %d", approvedTokResp.StatusCode)
	}
}

func TestStackUpStatusAndDownWithMockEngine(t *testing.T) {
	home := t.TempDir()
	engine := newMockEngine(t)
	rewritingClient := engine.client

	detectOpts := &DetectOptions{
		ExplicitSocket: "/mock/podman.sock",
		GOOS:           "darwin",
		HomeDir:        home,
		StatSocket:     func(string) bool { return true },
		PingSocket:     func(context.Context, string) (RuntimeKind, error) { return KindPodman, nil },
	}

	testPort, err := allocateLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	testListen := fmt.Sprintf("127.0.0.1:%d", testPort)
	var out, errOut strings.Builder
	gatewayCtx, stopGateway := context.WithCancel(context.Background())
	defer stopGateway()
	desc, err := Up(context.Background(), UpConfig{
		Instance:   "test",
		ListenAddr: testListen,
		GOOS:       "darwin",
		HomeDir:    home,
		DetectOpts: detectOpts,
		EngineHTTP: rewritingClient,
		Stderr:     &errOut,
		// Up waits for the gateway it spawned, so the test runs a real one.
		SpawnGateway: func(stateDir string) (int, error) {
			go func() { _ = ServeStackGateway(gatewayCtx, stateDir) }()
			return os.Getpid(), nil
		},
		// No server runs behind the mock engine.
		Provision: func(context.Context, provisionConfig) (string, error) { return "kodsvc.test", nil },
	}, &out)
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	for _, want := range []string{
		"using podman",
		"checking container images...",
		"starting postgres (kodema-test-postgres)...",
		"waiting for postgres readiness...",
		"starting nats (kodema-test-nats)...",
		"running database migrations...",
		"starting TLS gateway on " + testListen + "...",
		"bootstrapping tenant tenant-local...",
		"starting kodema server (kodema-test-server)...",
		"waiting for kodema server readiness (https://" + testListen + "/readyz)...",
		"starting git service (kodema-test-git)...",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("Up stderr missing %q:\n%s", want, errOut.String())
		}
	}
	if strings.Contains(out.String(), "starting postgres") || strings.Contains(out.String(), "checking container images") {
		t.Fatalf("Up progress leaked onto stdout:\n%s", out.String())
	}
	if desc.PublicURL != "https://"+testListen {
		t.Fatalf("PublicURL = %q", desc.PublicURL)
	}
	envSh, err := os.ReadFile(filepath.Join(desc.StateDir, "env.sh"))
	if err != nil {
		t.Fatalf("read env.sh: %v", err)
	}
	for _, want := range []string{"KODEMA_URL=", "KODEMA_CA_FILE=", "KODEMA_TOKEN=", "KODEMA_PROJECT="} {
		if !strings.Contains(string(envSh), want) {
			t.Fatalf("env.sh missing %q:\n%s", want, envSh)
		}
	}
	wantLoginHint := fmt.Sprintf("CLI login:  kodema --server %s --ca-file %s login",
		shellSingleQuote(desc.PublicURL), shellSingleQuote(desc.CACertPath))
	if !strings.Contains(out.String(), wantLoginHint) {
		t.Fatalf("Up stdout missing %q:\n%s", wantLoginHint, out.String())
	}

	// The server mounts the socket Podman reports for itself, which on a
	// rootless Podman machine is not /run/podman/podman.sock.
	serverBinds := fmt.Sprint(engine.hostConfig(t, "kodema-test-server")["Binds"])
	if !strings.Contains(serverBinds, "/run/user/501/podman/podman.sock:/var/run/kodema-container.sock") {
		t.Fatalf("server binds = %s, want the engine's own socket", serverBinds)
	}
	// A fresh `kodema up` configures project-scoped workload secret rules so
	// project secrets written via secret.write can materialize in workloads.
	secretRulesPath := filepath.Join(desc.StateDir, secretRulesFileName)
	serverEnv := engine.containerEnv(t, "kodema-test-server")
	if !slices.Contains(serverEnv, "KODEMA_SECRET_RULES_FILE="+secretRulesPath) {
		t.Fatalf("server env missing KODEMA_SECRET_RULES_FILE=%s: %v", secretRulesPath, serverEnv)
	}
	rulesData, err := os.ReadFile(secretRulesPath)
	if err != nil {
		t.Fatalf("read secret-rules.json: %v", err)
	}
	var parsedRules localSecretRulesFile
	if err := json.Unmarshal(rulesData, &parsedRules); err != nil {
		t.Fatalf("decode secret-rules.json: %v", err)
	}
	if len(parsedRules.SecretRules) != 1 ||
		parsedRules.SecretRules[0].TenantID != DefaultTenantID ||
		parsedRules.SecretRules[0].ProjectID != DefaultProjectID ||
		!parsedRules.SecretRules[0].AllowAll ||
		len(parsedRules.SecretRules[0].Names) != 0 {
		t.Fatalf("default secret-rules.json = %+v; want project-scoped allow_all", parsedRules)
	}
	// Every container that mounts a host path runs unconfined by SELinux; the
	// broker mounts a named volume only and stays confined.
	for _, name := range []string{"kodema-test-postgres", "kodema-test-bootstrap", "kodema-test-server", "kodema-test-git-init", "kodema-test-git"} {
		if got := fmt.Sprint(engine.hostConfig(t, name)["SecurityOpt"]); got != "[label=disable]" {
			t.Errorf("container %s SecurityOpt = %s, want [label=disable]", name, got)
		}
	}
	if got, set := engine.hostConfig(t, "kodema-test-nats")["SecurityOpt"]; set {
		t.Errorf("container kodema-test-nats SecurityOpt = %v, want none", got)
	}

	// Without --socket, the other commands find the engine the stack recorded.
	recordedOpts := &DetectOptions{
		GOOS:       "darwin",
		HomeDir:    home,
		Getenv:     func(string) string { return "" },
		ReadDir:    func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist },
		StatSocket: func(path string) bool { return path == "/mock/podman.sock" },
		PingSocket: func(context.Context, string) (RuntimeKind, error) { return KindPodman, nil },
	}
	if recorded, err := Status(context.Background(), StatusConfig{
		Instance:   "test",
		GOOS:       "darwin",
		HomeDir:    home,
		DetectOpts: recordedOpts,
		EngineHTTP: rewritingClient,
	}); err != nil || recorded.Runtime.SocketPath != "/mock/podman.sock" {
		t.Fatalf("Status without --socket = %+v, %v; want the recorded socket", recorded.Runtime, err)
	}

	report, err := Status(context.Background(), StatusConfig{
		Instance:   "test",
		GOOS:       "darwin",
		HomeDir:    home,
		DetectOpts: detectOpts,
		EngineHTTP: rewritingClient,
	})
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	for _, comp := range []string{"postgres", "nats", "server", "git"} {
		if !report.Containers[comp].Running {
			t.Fatalf("container %s not running in Status: %+v", comp, report.Containers[comp])
		}
	}

	// A stale gateway.pid that now names another process is not signaled.
	bystander := exec.Command("sleep", "60")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(desc.StateDir, "gateway.pid"), []byte(strconv.Itoa(bystander.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
		if bystander.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGTERM {
			t.Error("Down signaled a process that is not the gateway")
		}
	}()

	if err := Down(context.Background(), DownConfig{
		Instance:   "test",
		Purge:      true,
		GOOS:       "darwin",
		HomeDir:    home,
		DetectOpts: detectOpts,
		EngineHTTP: rewritingClient,
	}, &out); err != nil {
		t.Fatalf("Down --purge failed: %v", err)
	}
	if _, err := os.Stat(desc.StateDir); !os.IsNotExist(err) {
		t.Fatalf("state dir still exists after Down --purge: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

// TestStackServesItsOwnMachineOnly pins the two peer rules: the gateway
// answers this machine and its containers, and the built-in sign-in, which
// asks for no credential, answers loopback only.
func TestStackServesItsOwnMachineOnly(t *testing.T) {
	gw, err := NewGateway(GatewayConfig{
		ServerUpstream: "http://127.0.0.1:1",
		AuthHandler:    http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewDevIssuer(DevIssuerConfig{
		Issuer:   "https://127.0.0.1:8443/auth",
		Audience: "https://127.0.0.1:8443",
		APIURL:   "https://127.0.0.1:8443",
		ClientID: "kodactl",
		KeyPath:  filepath.Join(t.TempDir(), "oidc-signing.key"),
		TenantID: "tenant-local",
	})
	if err != nil {
		t.Fatal(err)
	}
	status := func(handler http.Handler, method, path, remoteAddr string) int {
		request := httptest.NewRequest(method, path, strings.NewReader("grant_type=client_credentials&client_id=kodactl"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.RemoteAddr = remoteAddr
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if got := status(gw, http.MethodGet, "/auth/jwks.json", "192.0.2.1:4000"); got != http.StatusForbidden {
		t.Fatalf("gateway answered another machine with %d, want 403", got)
	}
	if got := status(gw, http.MethodGet, "/auth/jwks.json", "127.0.0.1:4000"); got != http.StatusNotFound {
		t.Fatalf("gateway answered loopback with %d, want the upstream's 404", got)
	}
	// A container's connection arrives from one of the host's own addresses.
	for _, remoteAddr := range []string{"192.0.2.1:4000", "10.88.0.2:4000"} {
		if got := status(issuer, http.MethodPost, "/auth/token", remoteAddr); got != http.StatusForbidden {
			t.Fatalf("sign-in answered %s with %d, want 403", remoteAddr, got)
		}
		if got := status(issuer, http.MethodGet, "/auth/jwks.json", remoteAddr); got != http.StatusOK {
			t.Fatalf("keys answered %s with %d, want 200", remoteAddr, got)
		}
	}
	if got := status(issuer, http.MethodPost, "/auth/token", "127.0.0.1:4000"); got != http.StatusOK {
		t.Fatalf("sign-in answered loopback with %d, want 200", got)
	}
}

func TestUpNeedsPodman(t *testing.T) {
	_, err := Up(context.Background(), UpConfig{
		HomeDir: t.TempDir(),
		DetectOpts: &DetectOptions{
			ExplicitSocket: "/mock/docker.sock",
			StatSocket:     func(string) bool { return true },
			PingSocket:     func(context.Context, string) (RuntimeKind, error) { return KindDocker, nil },
		},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "needs Podman") {
		t.Fatalf("Up on a Docker socket = %v, want a refusal that names Podman", err)
	}
}

func TestUpSecretRulesConfiguration(t *testing.T) {
	runMockUp := func(t *testing.T, home string, cfg UpConfig) (StackDescriptor, *mockEngine, error) {
		t.Helper()
		engine := newMockEngine(t)
		port, err := allocateLoopbackPort()
		if err != nil {
			t.Fatal(err)
		}
		gwCtx, stopGW := context.WithCancel(context.Background())
		t.Cleanup(stopGW)
		cfg.Instance = "sec"
		cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", port)
		cfg.GOOS = "darwin"
		cfg.HomeDir = home
		cfg.DetectOpts = &DetectOptions{
			ExplicitSocket: "/mock/podman.sock",
			GOOS:           "darwin",
			HomeDir:        home,
			StatSocket:     func(string) bool { return true },
			PingSocket:     func(context.Context, string) (RuntimeKind, error) { return KindPodman, nil },
		}
		cfg.EngineHTTP = engine.client
		cfg.SpawnGateway = func(stateDir string) (int, error) {
			go func() { _ = ServeStackGateway(gwCtx, stateDir) }()
			return os.Getpid(), nil
		}
		cfg.Provision = func(context.Context, provisionConfig) (string, error) { return "kodsvc.test", nil }
		desc, err := Up(context.Background(), cfg, io.Discard)
		return desc, engine, err
	}

	t.Run("exact AllowSecrets writes names list and deduplicates", func(t *testing.T) {
		home := t.TempDir()
		desc, engine, err := runMockUp(t, home, UpConfig{
			TenantID:     "tenant-dev",
			ProjectID:    "project-dev",
			AllowSecrets: []string{"MY_SECRET", "API_KEY", "MY_SECRET"},
		})
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		path := filepath.Join(desc.StateDir, secretRulesFileName)
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("secret-rules.json stat/mode = %v, %v; want 0600", info, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var parsed localSecretRulesFile
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatal(err)
		}
		if len(parsed.SecretRules) != 1 || parsed.SecretRules[0].AllowAll ||
			parsed.SecretRules[0].TenantID != "tenant-dev" || parsed.SecretRules[0].ProjectID != "project-dev" ||
			!slices.Equal(parsed.SecretRules[0].Names, []string{"MY_SECRET", "API_KEY"}) {
			t.Fatalf("parsed secret-rules.json = %+v", parsed)
		}
		env := engine.containerEnv(t, "kodema-sec-server")
		if !slices.Contains(env, "KODEMA_SECRET_RULES_FILE="+path) {
			t.Fatalf("server env missing KODEMA_SECRET_RULES_FILE=%s", path)
		}

		// Re-running with DenyProjectSecrets removes the stale file and omits the env var.
		_, denyEngine, err := runMockUp(t, home, UpConfig{
			TenantID:           "tenant-dev",
			ProjectID:          "project-dev",
			DenyProjectSecrets: true,
		})
		if err != nil {
			t.Fatalf("Up(DenyProjectSecrets): %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale secret-rules.json still present: %v", err)
		}
		for _, entry := range denyEngine.containerEnv(t, "kodema-sec-server") {
			if strings.HasPrefix(entry, "KODEMA_SECRET_RULES_FILE=") {
				t.Fatalf("DenyProjectSecrets server env unexpectedly set %s", entry)
			}
		}
	})

	t.Run("rejects reserved KODEMA_TOKEN, invalid names and conflicting options", func(t *testing.T) {
		for _, cfg := range []UpConfig{
			{AllowSecrets: []string{"KODEMA_TOKEN"}},
			{AllowSecrets: []string{"bad_lowercase"}},
			{DenyProjectSecrets: true, AllowSecrets: []string{"MY_SECRET"}},
		} {
			if _, _, err := runMockUp(t, t.TempDir(), cfg); err == nil {
				t.Fatalf("Up(%+v) succeeded; want error", cfg)
			}
		}
	})
}

// TestProvisionCreatesWhatIsMissingAndRenewsTheGitCredential drives provision
// against a public API that already holds the project (a repeated `kodema up`)
// and has no Git credential yet.
func TestProvisionCreatesWhatIsMissingAndRenewsTheGitCredential(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			return
		}
		route, _, ok := api.Match(api.Method(r.Method), r.URL.Path)
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		calls = append(calls, route.Operation+" "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		w.Header().Set("Content-Type", api.MediaType)
		body := `{}`
		switch route.Operation {
		case "project.create":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"version_conflict","message":"project exists"}`))
			return
		case "service.list":
			body = `{"services":[{"service_id":"git","state":"enabled","roles":["viewer"],"version":3}]}`
		case "service.credential.rotate":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"no current service credential to rotate"}`))
			return
		case "service.credential.issue":
			body = `{"state":"current","version":4,"token":"kodsvc.issued"}`
		}
		_, _ = fmt.Fprintf(w, `{"operation":%q,"body":%s}`, route.Operation, body)
	}))
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())

	token, err := provision(context.Background(), provisionConfig{
		PublicURL:    server.URL,
		RootCAs:      pool,
		Issuer:       "https://127.0.0.1:8443/auth",
		TenantID:     "tenant-local",
		ProjectID:    "project-local",
		TenantToken:  "tenant-token",
		ProjectToken: "project-token",
		GitOrigins:   []string{"https://127.0.0.1:8443"},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if token != "kodsvc.issued" {
		t.Fatalf("Git credential = %q", token)
	}
	slices.Sort(calls)
	want := []string{
		"membership.grant tenant-token", "membership.grant tenant-token",
		"membership.grant tenant-token", "membership.grant tenant-token",
		"project.create tenant-token",
		"resource.binding.register project-token",
		"service.create project-token",
		"service.credential.issue project-token",
		"service.credential.rotate project-token",
		"service.list project-token",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("operations = %q, want %q", calls, want)
	}
}

// mockEngine is an engine API that accepts every request `kodema up` makes
// and keeps what each container was created with.
type mockEngine struct {
	mu      sync.Mutex
	created map[string]map[string]any
	// missingImages reports every GET /images/<ref>/json as 404 until pulled.
	missingImages bool
	pulledImages  []string
	// logs is the output every container reports.
	logs   string
	client *http.Client
}

// hostConfig returns the HostConfig the named container was created with.
func (engine *mockEngine) hostConfig(t *testing.T, name string) map[string]any {
	t.Helper()
	engine.mu.Lock()
	defer engine.mu.Unlock()
	hostConfig, ok := engine.created[name]["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("container %q was not created with a HostConfig: %v", name, engine.created[name])
	}
	return hostConfig
}

// containerEnv returns the Env slice the named container was created with.
func (engine *mockEngine) containerEnv(t *testing.T, name string) []string {
	t.Helper()
	engine.mu.Lock()
	defer engine.mu.Unlock()
	raw, ok := engine.created[name]["Env"].([]any)
	if !ok {
		t.Fatalf("container %q was not created with an Env slice: %v", name, engine.created[name])
	}
	env := make([]string, 0, len(raw))
	for _, entry := range raw {
		if s, ok := entry.(string); ok {
			env = append(env, s)
		}
	}
	return env
}

func newMockEngine(t *testing.T) *mockEngine {
	t.Helper()
	engine := &mockEngine{created: map[string]map[string]any{}}
	containers := map[string]bool{}
	engineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		path := r.URL.Path
		switch {
		case path == "/_ping":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		case path == "/v4.0.0/libpod/info":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"host":{"remoteSocket":{"path":"unix:///run/user/501/podman/podman.sock","exists":true}}}`))
		case strings.HasPrefix(path, "/v1.41/networks/"):
			if path == "/v1.41/networks/create" {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"Id":"net-1"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Name":"net"}`))
		case strings.HasPrefix(path, "/v1.41/volumes/"):
			if path == "/v1.41/volumes/create" {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"Name":"vol"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case path == "/v1.41/images/create" && r.Method == http.MethodPost:
			engine.pulledImages = append(engine.pulledImages, r.URL.Query().Get("fromImage"))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"Status: Downloaded newer image"}` + "\n"))
		case strings.HasPrefix(path, "/v1.41/images/"):
			if engine.missingImages {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"no such image"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Id":"sha256:abc"}`))
		case path == "/v1.41/containers/create":
			name := r.URL.Query().Get("name")
			containers[name] = true
			var created map[string]any
			_ = json.NewDecoder(r.Body).Decode(&created)
			engine.created[name] = created
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"Id":%q}`, name)
		case strings.HasSuffix(path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/wait"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"StatusCode":0}`))
		case strings.HasSuffix(path, "/logs"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(engine.logs))
		case strings.HasSuffix(path, "/exec"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"exec-1"}`))
		case path == "/v1.41/exec/exec-1/start":
			w.WriteHeader(http.StatusOK)
		case path == "/v1.41/exec/exec-1/json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ExitCode":0}`))
		case path == "/v1.41/containers/json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1.41/containers/"):
			name := strings.TrimPrefix(path, "/v1.41/containers/")
			delete(containers, name)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1.41/containers/") && strings.HasSuffix(path, "/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/v1.41/containers/"), "/json")
			if !containers[name] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{
				"Id": %q,
				"Name": "/%s",
				"State": {"Status": "running", "Running": true, "ExitCode": 0},
				"Config": {"Labels": {"kodema.up.instance": "test"}},
				"NetworkSettings": {"Ports": {"8443/tcp": [{"HostIp":"127.0.0.1","HostPort":"28443"}], "8081/tcp": [{"HostIp":"127.0.0.1","HostPort":"28081"}]}}
			}`, name, name)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(engineSrv.Close)
	// Rewrite http://d/... requests to the httptest engine server.
	baseTransport := engineSrv.Client().Transport
	engine.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			u, _ := url.Parse(engineSrv.URL)
			cloned := req.Clone(req.Context())
			cloned.URL.Scheme = u.Scheme
			cloned.URL.Host = u.Host
			return baseTransport.RoundTrip(cloned)
		}),
	}
	return engine
}

// TestUpThatFailsStopsItsGatewayAndShowsTheServerLog pins what a failed
// `kodema up` leaves: no gateway on the listen port, and an error that carries
// the end of the server's log.
func TestUpThatFailsStopsItsGatewayAndShowsTheServerLog(t *testing.T) {
	home := t.TempDir()
	engine := newMockEngine(t)
	engine.logs = "2026/01/02 03:04:05 WARN readiness check failed check=runtime\n" +
		"2026/01/02 03:04:06 WARN readiness check failed check=runtime\n"

	// A process `ps` shows with --gateway-only, as the real gateway is.
	gateway := exec.Command("sh", "-c", "while :; do sleep 1; done", "--gateway-only")
	testPort, err := allocateLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Up(context.Background(), UpConfig{
		Instance:   "test",
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", testPort),
		GOOS:       "darwin",
		HomeDir:    home,
		DetectOpts: &DetectOptions{
			ExplicitSocket: "/mock/podman.sock",
			StatSocket:     func(string) bool { return true },
			PingSocket:     func(context.Context, string) (RuntimeKind, error) { return KindPodman, nil },
		},
		EngineHTTP: engine.client,
		SpawnGateway: func(string) (int, error) {
			if err := gateway.Start(); err != nil {
				return 0, err
			}
			return gateway.Process.Pid, nil
		},
		Provision: func(context.Context, provisionConfig) (string, error) {
			return "", fmt.Errorf("the server was not ready within 90s")
		},
	}, nil)
	if gateway.Process == nil {
		t.Fatalf("Up did not start the gateway: %v", err)
	}
	defer func() { _ = gateway.Process.Kill() }()
	if err == nil {
		t.Fatal("Up succeeded although provisioning failed")
	}
	want := "the server was not ready within 90s; the log of container kodema-test-server ends with:\n" +
		"2026/01/02 03:04:06 WARN readiness check failed check=runtime"
	if err.Error() != want {
		t.Fatalf("Up error = %q, want %q", err, want)
	}

	exited := make(chan error, 1)
	go func() { exited <- gateway.Wait() }()
	select {
	case <-exited:
		if gateway.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Fatalf("gateway ended with %v, want SIGTERM from Up", gateway.ProcessState)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway of a failed Up is still running")
	}
	if _, err := os.Stat(filepath.Join(home, ".kodema", "instances", "test", "gateway.pid")); !os.IsNotExist(err) {
		t.Fatalf("gateway.pid after a failed Up: %v", err)
	}
}

func TestContainerSocketMountSource(t *testing.T) {
	podman := DetectedRuntime{Kind: KindPodman, SocketPath: "/host/podman.sock"}
	for _, tc := range []struct {
		name, goos, remote, want string
		detected                 DetectedRuntime
	}{
		{"rootless Podman machine", "darwin", "/run/user/501/podman/podman.sock", "/run/user/501/podman/podman.sock", podman},
		{"rootful Podman machine", "darwin", "/run/podman/podman.sock", "/run/podman/podman.sock", podman},
		{"Podman machine that does not say", "darwin", "", "/run/podman/podman.sock", podman},
		{"Linux mounts the socket it detected", "linux", "/run/podman/podman.sock", "/host/podman.sock", podman},
	} {
		if got := containerSocketMountSource(tc.goos, tc.detected, tc.remote); got != tc.want {
			t.Errorf("%s: mount source = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLastDistinctLines(t *testing.T) {
	text := "2026/01/02 03:04:01 WARN test profile\n" +
		"2026/01/02 03:04:02 WARN readiness check failed check=runtime\n" +
		"\n" +
		"2026/01/02 03:04:03 WARN readiness check failed check=broker\n" +
		"2026/01/02 03:04:04 WARN readiness check failed check=runtime\n"
	want := "2026/01/02 03:04:03 WARN readiness check failed check=broker\n" +
		"2026/01/02 03:04:04 WARN readiness check failed check=runtime"
	if got := lastDistinctLines(text, 2); got != want {
		t.Fatalf("lastDistinctLines = %q, want %q", got, want)
	}
	if got := lastDistinctLines("", 5); got != "" {
		t.Fatalf("lastDistinctLines of no output = %q", got)
	}
}

// TestUpReportsImagePullProgressOnStderr verifies that a cold image cache emits
// per-image pull progress on stderr while keeping stdout limited to the final
// summary.
func TestUpReportsImagePullProgressOnStderr(t *testing.T) {
	home := t.TempDir()
	engine := newMockEngine(t)
	engine.missingImages = true

	testPort, err := allocateLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	testListen := fmt.Sprintf("127.0.0.1:%d", testPort)
	var out, errOut strings.Builder
	gatewayCtx, stopGateway := context.WithCancel(context.Background())
	defer stopGateway()
	_, err = Up(context.Background(), UpConfig{
		Instance:      "cold",
		ListenAddr:    testListen,
		KodemaImage:   "ghcr.io/optimiware/kodema/kodema:v0.4.0",
		PostgresImage: "docker.io/library/postgres:18-alpine",
		NATSImage:     "docker.io/library/nats:2.15-alpine",
		GOOS:          "darwin",
		HomeDir:       home,
		Stderr:        &errOut,
		DetectOpts: &DetectOptions{
			ExplicitSocket: "/mock/podman.sock",
			GOOS:           "darwin",
			HomeDir:        home,
			StatSocket:     func(string) bool { return true },
			PingSocket:     func(context.Context, string) (RuntimeKind, error) { return KindPodman, nil },
		},
		EngineHTTP: engine.client,
		SpawnGateway: func(stateDir string) (int, error) {
			go func() { _ = ServeStackGateway(gatewayCtx, stateDir) }()
			return os.Getpid(), nil
		},
		Provision: func(context.Context, provisionConfig) (string, error) { return "kodsvc.cold", nil },
	}, &out)
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	wantImages := []string{
		"docker.io/library/postgres:18-alpine",
		"docker.io/library/nats:2.15-alpine",
		"ghcr.io/optimiware/kodema/kodema:v0.4.0",
	}
	engine.mu.Lock()
	pulled := slices.Clone(engine.pulledImages)
	engine.mu.Unlock()
	if !slices.Equal(pulled, wantImages) {
		t.Fatalf("pulled images = %v, want %v", pulled, wantImages)
	}
	for _, img := range wantImages {
		for _, want := range []string{"pulling image " + img + "...\n", "pulled image " + img + "\n"} {
			if !strings.Contains(errOut.String(), want) {
				t.Fatalf("stderr missing %q:\n%s", want, errOut.String())
			}
		}
	}
	if strings.Contains(out.String(), "pulling image") {
		t.Fatalf("pull progress leaked onto stdout:\n%s", out.String())
	}
}

// onlyUnder reads directories under root and reports every other one absent,
// so socket discovery does not see the machine the test runs on.
func onlyUnder(root string) func(string) ([]os.DirEntry, error) {
	return func(dir string) ([]os.DirEntry, error) {
		if dir != root && !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return nil, os.ErrNotExist
		}
		return os.ReadDir(dir)
	}
}

// TestDiscoverDefaultInstance pins local default-instance discovery:
// alive via gateway.pid, alive via foreground loopback listener, and absent
// when stopped or missing.
func TestDiscoverDefaultInstance(t *testing.T) {
	t.Run("missing state returns false", func(t *testing.T) {
		if _, ok := DiscoverDefaultInstance(t.TempDir()); ok {
			t.Fatal("DiscoverDefaultInstance succeeded without stack.json")
		}
	})

	t.Run("alive via gateway.pid returns descriptor", func(t *testing.T) {
		home := t.TempDir()
		stateDir, err := DefaultStateDir(home, DefaultInstanceName)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		want := StackDescriptor{
			Instance:   DefaultInstanceName,
			StateDir:   stateDir,
			PublicURL:  "https://127.0.0.1:8443",
			TenantID:   DefaultTenantID,
			ProjectID:  DefaultProjectID,
			CACertPath: filepath.Join(stateDir, "tls", "ca.crt"),
			GatewayPID: os.Getpid(),
		}
		if err := saveStackDescriptor(stateDir, want); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "gateway.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := DiscoverDefaultInstance(home)
		if !ok {
			t.Fatal("DiscoverDefaultInstance returned false for live gateway.pid")
		}
		if got.PublicURL != want.PublicURL || got.CACertPath != want.CACertPath || got.ProjectID != want.ProjectID {
			t.Fatalf("discovered = %+v; want %+v", got, want)
		}
	})

	t.Run("alive via foreground loopback listener without gateway.pid", func(t *testing.T) {
		home := t.TempDir()
		stateDir, err := DefaultStateDir(home, DefaultInstanceName)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		want := StackDescriptor{
			Instance:   DefaultInstanceName,
			StateDir:   stateDir,
			PublicURL:  "https://" + ln.Addr().String(),
			ProjectID:  "proj-fg",
			CACertPath: filepath.Join(stateDir, "tls", "ca.crt"),
		}
		if err := saveStackDescriptor(stateDir, want); err != nil {
			t.Fatal(err)
		}
		got, ok := DiscoverDefaultInstance(home)
		if !ok || got.PublicURL != want.PublicURL || got.ProjectID != want.ProjectID {
			t.Fatalf("DiscoverDefaultInstance(foreground) = %+v, %v; want %+v, true", got, ok, want)
		}
	})

	t.Run("stopped instance without gateway.pid or listener returns false", func(t *testing.T) {
		home := t.TempDir()
		stateDir, err := DefaultStateDir(home, DefaultInstanceName)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedAddr := ln.Addr().String()
		_ = ln.Close()

		if err := saveStackDescriptor(stateDir, StackDescriptor{
			Instance:  DefaultInstanceName,
			StateDir:  stateDir,
			PublicURL: "https://" + closedAddr,
			ProjectID: DefaultProjectID,
		}); err != nil {
			t.Fatal(err)
		}
		if _, ok := DiscoverDefaultInstance(home); ok {
			t.Fatal("DiscoverDefaultInstance returned true for stopped instance")
		}
	})
}
