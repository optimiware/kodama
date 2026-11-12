package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
)

type recorderResponse struct {
	header  http.Header
	status  int
	flushes int
	body    bytes.Buffer
}

func newRecorderResponse() *recorderResponse {
	return &recorderResponse{header: make(http.Header)}
}

func (response *recorderResponse) Header() http.Header { return response.header }
func (response *recorderResponse) WriteHeader(status int) {
	response.status = status
}
func (response *recorderResponse) Write(data []byte) (int, error) {
	return response.body.Write(data)
}
func (response *recorderResponse) Flush() {
	response.flushes++
}

func helperBackend(t *testing.T) *HTTPBackend {
	t.Helper()
	backend, err := NewHTTPBackend(BackendConfig{
		GitPath: os.Args[0],
		Env:     append(MinimalEnvironment(), "KODAMA_GIT_EXEC_HELPER=1"),
	})
	if err != nil {
		t.Fatalf("NewHTTPBackend: %v", err)
	}
	return backend
}

func TestHTTPBackendServesGetInfoRefs(t *testing.T) {
	backend := helperBackend(t)
	repository := filepath.Join(t.TempDir(), "repo")
	response := newRecorderResponse()
	err := backend.Serve(context.Background(), git.GitBackendRequest{
		Service:    "git-upload-pack",
		Repository: "repo",
		Path:       repository,
		Headers:    http.Header{},
	}, nil, response)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if response.status != http.StatusOK {
		t.Fatalf("status = %d", response.status)
	}
	if response.body.String() != "backend-body" {
		t.Fatalf("body = %q", response.body.String())
	}
	if got := response.header.Get("X-Request-Method"); got != http.MethodGet {
		t.Fatalf("REQUEST_METHOD = %q", got)
	}
	if got := response.header.Get("X-Path-Info"); got != "/repo/info/refs" {
		t.Fatalf("PATH_INFO = %q", got)
	}
	if got := response.header.Get("X-Query"); got != "service=git-upload-pack" {
		t.Fatalf("QUERY_STRING = %q", got)
	}
	if got := response.header.Get("X-Content-Length"); got != "0" {
		t.Fatalf("CONTENT_LENGTH = %q", got)
	}
}

func TestHTTPBackendServesPostService(t *testing.T) {
	backend := helperBackend(t)
	repository := filepath.Join(t.TempDir(), "repo")
	response := newRecorderResponse()
	err := backend.Serve(context.Background(), git.GitBackendRequest{
		Service:    "git-receive-pack",
		Repository: "repo",
		Path:       repository,
		Environment: []string{
			"KODAMA_ACTOR_REF=handle-1",
			"KODAMA_SOURCE_EVENT=event-1",
		},
		Headers: http.Header{"Content-Type": []string{"application/x-git-receive-pack-request"}},
	}, strings.NewReader("payload"), response)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if got := response.header.Get("X-Request-Method"); got != http.MethodPost {
		t.Fatalf("REQUEST_METHOD = %q", got)
	}
	if got := response.header.Get("X-Path-Info"); got != "/repo/git-receive-pack" {
		t.Fatalf("PATH_INFO = %q", got)
	}
	if got := response.header.Get("X-Content-Length"); got != "7" {
		t.Fatalf("CONTENT_LENGTH = %q", got)
	}
	if got := response.header.Get("X-Content-Type"); got != "application/x-git-receive-pack-request" {
		t.Fatalf("CONTENT_TYPE = %q", got)
	}
}

func TestHTTPBackendRefusesOversizedBody(t *testing.T) {
	backend, err := NewHTTPBackend(BackendConfig{
		GitPath:      os.Args[0],
		Env:          append(MinimalEnvironment(), "KODAMA_GIT_EXEC_HELPER=1"),
		MaxBodyBytes: 4,
	})
	if err != nil {
		t.Fatalf("NewHTTPBackend: %v", err)
	}
	err = backend.Serve(context.Background(), git.GitBackendRequest{
		Service: "git-receive-pack",
		Path:    filepath.Join(t.TempDir(), "repo"),
		Headers: http.Header{},
	}, strings.NewReader("too-large"), newRecorderResponse())
	if err == nil || !strings.Contains(err.Error(), "body") {
		t.Fatalf("Serve error = %v; want an oversized-body refusal", err)
	}
}

func TestHTTPBackendKeepsExplicitEmptyEnvironmentNonNil(t *testing.T) {
	backend, err := NewHTTPBackend(BackendConfig{GitPath: os.Args[0], Env: []string{}})
	if err != nil {
		t.Fatalf("NewHTTPBackend: %v", err)
	}
	if backend.env == nil {
		t.Fatal("NewHTTPBackend stored a nil base environment for an explicit empty Env")
	}
	if len(backend.env) != 0 {
		t.Fatalf("NewHTTPBackend base environment = %q; want empty", backend.env)
	}
}

// TestKeepRefHideEnvironmentHidesBothAdvertisementDirections pins the exact
// pinned Git configuration the served backend receives: upload-pack and
// transfer both hide the whole refs/kodama namespace, so a client can neither
// fetch retention bookkeeping nor discover it in a ref advertisement.
func TestKeepRefHideEnvironmentHidesBothAdvertisementDirections(t *testing.T) {
	environment := KeepRefHideEnvironment()
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", entry)
		}
		values[name] = value
	}
	if values["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("GIT_CONFIG_COUNT = %q; want 2", values["GIT_CONFIG_COUNT"])
	}
	for index, key := range []string{"uploadpack.hideRefs", "transfer.hideRefs"} {
		name := "GIT_CONFIG_KEY_" + strconv.Itoa(index)
		value := "GIT_CONFIG_VALUE_" + strconv.Itoa(index)
		if values[name] != key {
			t.Fatalf("%s = %q; want %q", name, values[name], key)
		}
		if values[value] != "refs/kodama" {
			t.Fatalf("%s = %q; want refs/kodama", value, values[value])
		}
	}
}

func TestHTTPBackendRefusesMalformedResponse(t *testing.T) {
	if err := writeCGIResponse(newRecorderResponse(), []byte("no headers")); err == nil {
		t.Fatal("writeCGIResponse accepted a response without a header terminator")
	}
}

func TestHTTPBackendDefaultBoundsAre512MiB(t *testing.T) {
	const want int64 = 512 << 20
	if git.DefaultSmartHTTPMaxBodyBytes != want {
		t.Fatalf("DefaultSmartHTTPMaxBodyBytes = %d; want %d", git.DefaultSmartHTTPMaxBodyBytes, want)
	}
	if DefaultBackendMaxBodyBytes != want {
		t.Fatalf("DefaultBackendMaxBodyBytes = %d; want %d", DefaultBackendMaxBodyBytes, want)
	}
	if DefaultBackendMaxResponseBytes != want {
		t.Fatalf("DefaultBackendMaxResponseBytes = %d; want %d", DefaultBackendMaxResponseBytes, want)
	}
}

func TestHTTPBackendSpoolsLargeRequestBodyAndBoundsBeforeChild(t *testing.T) {
	const bodySize = (1 << 20) + 8192
	backend, err := NewHTTPBackend(BackendConfig{
		GitPath:      os.Args[0],
		Env:          append(MinimalEnvironment(), "KODAMA_GIT_EXEC_HELPER=1", "KODAMA_HELPER_READ_STDIN=1"),
		MaxBodyBytes: bodySize,
	})
	if err != nil {
		t.Fatalf("NewHTTPBackend: %v", err)
	}
	payload := bytes.Repeat([]byte("z"), bodySize)
	response := newRecorderResponse()
	err = backend.Serve(context.Background(), git.GitBackendRequest{
		Service:    "git-receive-pack",
		Repository: "repo",
		Path:       filepath.Join(t.TempDir(), "repo"),
		Headers:    http.Header{"Content-Type": []string{"application/x-git-receive-pack-request"}},
	}, bytes.NewReader(payload), response)
	if err != nil {
		t.Fatalf("Serve large body: %v", err)
	}
	if got := response.header.Get("X-Content-Length"); got != strconv.Itoa(bodySize) {
		t.Fatalf("CONTENT_LENGTH = %q; want %d", got, bodySize)
	}
	if got := response.header.Get("X-Stdin-Bytes"); got != strconv.Itoa(bodySize) {
		t.Fatalf("X-Stdin-Bytes = %q; want %d", got, bodySize)
	}

	oversized := bytes.Repeat([]byte("z"), bodySize+1)
	overResponse := newRecorderResponse()
	err = backend.Serve(context.Background(), git.GitBackendRequest{
		Service:    "git-receive-pack",
		Repository: "repo",
		Path:       filepath.Join(t.TempDir(), "repo"),
		Headers:    http.Header{},
	}, bytes.NewReader(oversized), overResponse)
	if !errors.Is(err, git.ErrBodyTooLarge) {
		t.Fatalf("Serve oversized spooled body error = %v; want git.ErrBodyTooLarge", err)
	}
	if overResponse.status != 0 {
		t.Fatalf("oversized body wrote status %d", overResponse.status)
	}
}

func TestHTTPBackendStreamsLargeResponseAndEnforcesMaxResponseBytes(t *testing.T) {
	t.Run("small bound refuses before WriteHeader", func(t *testing.T) {
		backend, err := NewHTTPBackend(BackendConfig{
			GitPath:          os.Args[0],
			Env:              append(MinimalEnvironment(), "KODAMA_GIT_EXEC_HELPER=1"),
			MaxResponseBytes: 32,
		})
		if err != nil {
			t.Fatalf("NewHTTPBackend: %v", err)
		}
		response := newRecorderResponse()
		err = backend.Serve(context.Background(), git.GitBackendRequest{
			Service:    "git-upload-pack",
			Repository: "repo",
			Path:       filepath.Join(t.TempDir(), "repo"),
			Headers:    http.Header{},
		}, nil, response)
		if !errors.Is(err, ErrOutputTooLarge) {
			t.Fatalf("Serve error = %v; want ErrOutputTooLarge", err)
		}
		if response.status != 0 {
			t.Fatalf("small bound wrote status %d before refusing", response.status)
		}
	})

	t.Run("large response streams and flushes", func(t *testing.T) {
		const streamBody = (1 << 20) + 16384
		backend, err := NewHTTPBackend(BackendConfig{
			GitPath: os.Args[0],
			Env: append(MinimalEnvironment(),
				"KODAMA_GIT_EXEC_HELPER=1",
				"KODAMA_HELPER_BODY_SIZE="+strconv.Itoa(streamBody),
			),
			MaxResponseBytes: streamBody + (64 << 10),
		})
		if err != nil {
			t.Fatalf("NewHTTPBackend: %v", err)
		}
		response := newRecorderResponse()
		err = backend.Serve(context.Background(), git.GitBackendRequest{
			Service:    "git-upload-pack",
			Repository: "repo",
			Path:       filepath.Join(t.TempDir(), "repo"),
			Headers:    http.Header{},
		}, nil, response)
		if err != nil {
			t.Fatalf("Serve streamed response: %v", err)
		}
		if response.status != http.StatusOK {
			t.Fatalf("status = %d; want 200", response.status)
		}
		if response.body.Len() != streamBody {
			t.Fatalf("streamed body bytes = %d; want %d", response.body.Len(), streamBody)
		}
		if response.flushes == 0 {
			t.Fatal("streamed response never flushed")
		}
	})
}
