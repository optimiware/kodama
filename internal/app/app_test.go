package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfiguration(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"KODAMA_HTTP_ADDR", "missing-port"}, {"KODAMA_HTTP_ADDR", ":65536"},
		{"KODAMA_LOG_LEVEL", "verbose"}, {"KODAMA_SHUTDOWN_TIMEOUT", "0s"},
		{"KODAMA_SHUTDOWN_TIMEOUT", "2m"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			_, err := LoadConfig(func(k string) string {
				if k == tc.key {
					return tc.value
				}
				return ""
			})
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCommandExitCodes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{nil, 2}, {[]string{"step"}, 2}, {[]string{"server", "extra"}, 2}, {[]string{"--help"}, 0},
		{[]string{"server", "--help"}, 0}, {[]string{"-h"}, 0}, {[]string{"help"}, 0},
		{[]string{"mcp", "--help"}, 0}, {[]string{"mcp"}, 2},
		{[]string{"--unknown"}, 2}, {[]string{"server", "--unknown"}, 2},
		{[]string{"server", "--shutdown-timeout", "bad"}, 2},
		{[]string{"server", "--http-addr"}, 2}, {[]string{"server", "--http-addr="}, 2},
	} {
		var output bytes.Buffer
		if code := Main(context.Background(), tc.args, func(string) string { return "" }, &output, &output); code != tc.code {
			t.Fatalf("%v: code %d, want %d", tc.args, code, tc.code)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	code := Main(context.Background(), []string{"server"}, func(k string) string {
		if k == "KODAMA_HTTP_ADDR" {
			return listener.Addr().String()
		}
		return ""
	}, io.Discard, io.Discard)
	if code != 1 {
		t.Fatalf("occupied address: exit %d", code)
	}
}

func TestServerFlagPrecedence(t *testing.T) {
	environment := map[string]string{"KODAMA_HTTP_ADDR": "127.0.0.1:9090", "KODAMA_LOG_LEVEL": "warn", "KODAMA_SHUTDOWN_TIMEOUT": "3s"}
	getenv := func(name string) string { return environment[name] }
	config, err := parseServerConfig(nil, getenv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if config.HTTPAddr != "127.0.0.1:9090" || config.LogLevel != slog.LevelWarn || config.ShutdownTimeout != 3*time.Second {
		t.Fatalf("environment defaults not applied: %+v", config)
	}
	config, err = parseServerConfig([]string{"--http-addr=127.0.0.1:0", "--log-level", "debug", "--shutdown-timeout", "1s"}, getenv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if config.HTTPAddr != "127.0.0.1:0" || config.LogLevel != slog.LevelDebug || config.ShutdownTimeout != time.Second {
		t.Fatalf("flags did not override environment: %+v", config)
	}
	environment["KODAMA_HTTP_ADDR"] = "invalid"
	var help bytes.Buffer
	if _, err := parseServerConfig([]string{"--help"}, getenv, &help); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help should work even with invalid environment: %v", err)
	}
	if !strings.Contains(help.String(), "-http-addr") || !strings.Contains(help.String(), "-shutdown-timeout") {
		t.Fatalf("incomplete flag help: %s", help.String())
	}
}

func TestHealthContract(t *testing.T) {
	var ready atomic.Bool
	handler := healthHandler(&ready)
	for _, tc := range []struct {
		method, path, body string
		ready              bool
		status             int
	}{
		{"GET", "/healthz", `{"status":"ok"}`, false, 200},
		{"GET", "/readyz", `{"status":"not_ready"}`, false, 503},
		{"GET", "/readyz", `{"status":"ready"}`, true, 200},
		{"POST", "/healthz", "", true, 405},
		{"GET", "/api/v1/runs", "", true, 404},
	} {
		ready.Store(tc.ready)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
		if tc.body != "" && (strings.TrimSpace(w.Body.String()) != tc.body || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store") {
			t.Fatalf("invalid health contract: %v %s", w.Header(), w.Body.String())
		}
	}
}

// Exercise each published response, so edits to the machine-readable probe
// contract are checked against the actual handler rather than a second fixture.
func TestPublishedHealthContract(t *testing.T) {
	data, err := os.ReadFile("../../dev/contracts/health.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Properties map[string]struct{ Const string }
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	if len(contract.Paths) != 2 {
		t.Fatal("expected both process probe paths")
	}
	for path, operation := range contract.Paths {
		for status, response := range operation.Get.Responses {
			code, err := strconv.Atoi(status)
			if err != nil {
				t.Fatal(err)
			}
			var ready atomic.Bool
			ready.Store(code == http.StatusOK)
			w := httptest.NewRecorder()
			healthHandler(&ready).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			expected := response.Content["application/json"].Schema.Properties["status"].Const
			if expected == "" || w.Code != code || len(body) != 1 || body["status"] != expected {
				t.Fatalf("%s response differs from OpenAPI: %d %s", path, w.Code, w.Body.String())
			}
		}
	}
}

func TestServerCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, Config{ShutdownTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + listener.Addr().String() + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("listener remained open")
	}
}
