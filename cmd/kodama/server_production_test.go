package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	shellapp "github.com/optimiware/kodama/internal/shell/app"
)

// productionServeFixture builds the TLS material the production server path
// serves. It returns the build input and the pool a test client trusts.
func productionServeFixture(t *testing.T) (productionBuild, *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	selfSigned(t, dir)
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatalf("load the test server certificate: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("read the test CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("the test CA is not usable")
	}
	return productionBuild{
		Config: shellapp.Config{Profile: shellapp.ProfileProduction},
		TLS:    &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}, pool
}

func productionServeOptions(t *testing.T) serverOptions {
	t.Helper()
	return serverOptions{
		Profile:        "production",
		Address:        "127.0.0.1:0",
		DataDir:        t.TempDir(),
		InstallationID: "kodama-production-test",
		PublicURL:      "https://kodama.kodama-system.svc.cluster.local:8443",
		MCPPath:        "/mcp",
	}
}

// servingApp serves the passed listener with a real http.Server and drains it
// when the serving context ends, like the Shell composition does.
func servingApp(handler http.Handler) *fakeServerApp {
	return &fakeServerApp{
		handler: handler,
		serve: func(ctx context.Context, listener net.Listener) error {
			server := &http.Server{Handler: handler}
			served := make(chan error, 1)
			go func() { served <- server.Serve(listener) }()
			select {
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = server.Shutdown(shutdownCtx)
				<-served
				return nil
			case err := <-served:
				if err == http.ErrServerClosed {
					return nil
				}
				return err
			}
		},
	}
}

// TestRunProductionServesTLSAndDrainsOnCancellation pins the served transport:
// runProduction must wrap the listener with the compiled TLS configuration, not
// serve the bare listener. A TLS client that trusts the test CA completes the
// handshake at TLS 1.2+ and sees the configured certificate; a plaintext client
// fails. Cancellation (the SIGTERM path in main) drains and exits 0.
func TestRunProductionServesTLSAndDrainsOnCancellation(t *testing.T) {
	build, pool := productionServeFixture(t)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
	})
	listeners := make(chan net.Listener, 1)
	deps := serverDeps{
		build: func(serverOptions) (productionBuild, error) { return build, nil },
		newApp: func(context.Context, shellapp.Config) (serverApp, error) {
			app := servingApp(handler)
			inner := app.serve
			app.serve = func(ctx context.Context, listener net.Listener) error {
				listeners <- listener
				return inner(ctx, listener)
			}
			return app, nil
		},
		listen: func(network, address string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() { done <- runProduction(ctx, productionServeOptions(t), &stderr, deps) }()

	var listener net.Listener
	select {
	case listener = <-listeners:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatalf("runProduction never served; stderr=%q", stderr.String())
	}
	address := listener.Addr().String()

	// A TLS 1.2-only client proves the listener speaks TLS 1.2 and the served
	// certificate is the configured one.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, ServerName: "kodama.test", MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
	}}}
	response, err := client.Get("https://" + address + "/")
	if err != nil {
		cancel()
		t.Fatalf("a TLS client could not use the production listener: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		cancel()
		t.Fatalf("response = %d %q; want 200 ok", response.StatusCode, body)
	}
	if response.TLS == nil || response.TLS.Version < tls.VersionTLS12 {
		cancel()
		t.Fatalf("negotiated TLS version = %v; want TLS 1.2 or newer", response.TLS)
	}
	if len(response.TLS.PeerCertificates) == 0 || response.TLS.PeerCertificates[0].Subject.CommonName != "kodama.test" {
		cancel()
		t.Fatalf("served certificate = %+v; want the configured kodama.test certificate", response.TLS.PeerCertificates)
	}

	// A plaintext client must never receive the served response: Go's TLS
	// server answers a plaintext request with an error (or a plain 400), while
	// serving the bare listener would answer it with the handler's 200.
	plaintext := &http.Client{Timeout: 2 * time.Second}
	plaintextResponse, plaintextErr := plaintext.Get("http://" + address + "/")
	if plaintextErr == nil {
		plaintextBody, _ := io.ReadAll(plaintextResponse.Body)
		plaintextResponse.Body.Close()
		if plaintextResponse.StatusCode == http.StatusOK || strings.Contains(string(plaintextBody), "ok") {
			cancel()
			t.Fatalf("the production listener served a plaintext HTTP client: %d %q", plaintextResponse.StatusCode, plaintextBody)
		}
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d after cancellation; stderr=%q", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runProduction did not drain and return after cancellation")
	}
}

// TestRunProductionFailsClosedOnConstructionListenAndServeErrors pins the exit
// codes and cleanup of every failure branch: a composition, listen or Serve
// failure exits 1 with a secret-free message, and a listen failure closes the
// constructed app instead of leaking its loops.
func TestRunProductionFailsClosedOnConstructionListenAndServeErrors(t *testing.T) {
	build, _ := productionServeFixture(t)
	const environmentSecret = "SENTINEL-PRODUCTION-DB-PASSWORD"
	// A hostile dependency quotes a credential, DSN or token in its private
	// diagnostic. The command must render a fixed step message, never the
	// wrapped dependency text.
	const dependencySecret = "SENTINEL-DEPENDENCY-CREDENTIAL"
	t.Setenv("KODAMA_DATABASE_URL", "postgres://kodama_runtime:"+environmentSecret+"@db/kodama")

	t.Run("composition failure exits 2", func(t *testing.T) {
		listened := false
		deps := serverDeps{
			build: func(serverOptions) (productionBuild, error) {
				return productionBuild{}, fmt.Errorf("app construction failed")
			},
			newApp: func(context.Context, shellapp.Config) (serverApp, error) {
				t.Fatal("a failed composition was constructed")
				return nil, nil
			},
			listen: func(string, string) (net.Listener, error) {
				listened = true
				return nil, nil
			},
		}
		var stderr lockedBuffer
		if code := runProduction(context.Background(), productionServeOptions(t), &stderr, deps); code != 2 {
			t.Fatalf("exit code = %d; want 2 for a configuration refusal", code)
		}
		if listened {
			t.Fatal("a failed composition listened")
		}
		if strings.Contains(stderr.String(), environmentSecret) {
			t.Fatalf("stderr rendered the database password: %q", stderr.String())
		}
	})

	t.Run("newApp failure exits 1", func(t *testing.T) {
		listened := false
		deps := serverDeps{
			build: func(serverOptions) (productionBuild, error) { return build, nil },
			newApp: func(context.Context, shellapp.Config) (serverApp, error) {
				return nil, fmt.Errorf("dial postgres://kodama_runtime:%s@db/kodama: composition could not start", dependencySecret)
			},
			listen: func(string, string) (net.Listener, error) {
				listened = true
				return nil, nil
			},
		}
		var stderr lockedBuffer
		if code := runProduction(context.Background(), productionServeOptions(t), &stderr, deps); code != 1 {
			t.Fatalf("exit code = %d; want 1", code)
		}
		if listened {
			t.Fatal("a composition that failed to start listened")
		}
		if strings.Contains(stderr.String(), environmentSecret) || strings.Contains(stderr.String(), dependencySecret) {
			t.Fatalf("stderr rendered a dependency credential: %q", stderr.String())
		}
		if !strings.Contains(stderr.String(), "composition failed to start") {
			t.Fatalf("stderr = %q; want the fixed composition diagnostic", stderr.String())
		}
	})

	t.Run("listen failure exits 1 and closes the app", func(t *testing.T) {
		app := &fakeServerApp{handler: http.NewServeMux(), serve: func(context.Context, net.Listener) error {
			t.Fatal("a failed listen served")
			return nil
		}}
		options := productionServeOptions(t)
		var listenedAddress string
		deps := serverDeps{
			build: func(serverOptions) (productionBuild, error) { return build, nil },
			newApp: func(context.Context, shellapp.Config) (serverApp, error) {
				return app, nil
			},
			listen: func(_, address string) (net.Listener, error) {
				listenedAddress = address
				return nil, fmt.Errorf("address already in use: credential %s", dependencySecret)
			},
		}
		var stderr lockedBuffer
		if code := runProduction(context.Background(), options, &stderr, deps); code != 1 {
			t.Fatalf("exit code = %d; want 1", code)
		}
		if app.closed != 1 {
			t.Fatalf("app.Close calls = %d; want 1 after a listen failure", app.closed)
		}
		if listenedAddress != options.Address {
			t.Fatalf("listen address = %q; want the --address %q", listenedAddress, options.Address)
		}
		if !strings.Contains(stderr.String(), "listen") || strings.Contains(stderr.String(), environmentSecret) || strings.Contains(stderr.String(), dependencySecret) {
			t.Fatalf("stderr = %q; want a secret-free listen diagnostic", stderr.String())
		}
	})

	t.Run("serve failure exits 1", func(t *testing.T) {
		deps := serverDeps{
			build: func(serverOptions) (productionBuild, error) { return build, nil },
			newApp: func(context.Context, shellapp.Config) (serverApp, error) {
				return &fakeServerApp{
					handler: http.NewServeMux(),
					serve: func(context.Context, net.Listener) error {
						return errors.New("serving failed with token " + dependencySecret)
					},
				}, nil
			},
			listen: func(network, address string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") },
		}
		var stderr lockedBuffer
		if code := runProduction(context.Background(), productionServeOptions(t), &stderr, deps); code != 1 {
			t.Fatalf("exit code = %d; want 1", code)
		}
		if strings.Contains(stderr.String(), environmentSecret) || strings.Contains(stderr.String(), dependencySecret) {
			t.Fatalf("stderr rendered a dependency credential: %q", stderr.String())
		}
		if !strings.Contains(stderr.String(), "server failed") {
			t.Fatalf("stderr = %q; want the fixed serve diagnostic", stderr.String())
		}
	})
}

// TestRunProductionRefusesAnEmptyProfileWithoutDevelopmentFallback pins that an
// empty profile is a configuration refusal (exit 2) and never builds the
// development composition.
func TestRunProductionRefusesAnEmptyProfileWithoutDevelopmentFallback(t *testing.T) {
	t.Setenv("KODAMA_PROFILE", "")
	constructed := atomic.Bool{}
	deps := serverDeps{
		newApp: func(context.Context, shellapp.Config) (serverApp, error) {
			constructed.Store(true)
			return nil, nil
		},
		listen: func(string, string) (net.Listener, error) {
			constructed.Store(true)
			return nil, nil
		},
	}
	var stdout, stderr lockedBuffer
	code := runServer(context.Background(), nil, &stdout, &stderr, deps)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if constructed.Load() {
		t.Fatal("an empty profile built a composition")
	}
	if !strings.Contains(stderr.String(), "explicit --profile") {
		t.Fatalf("stderr = %q; want an explicit-profile refusal", stderr.String())
	}
}
