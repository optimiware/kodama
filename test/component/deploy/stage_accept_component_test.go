//go:build component

// Hermetic tests for scripts/stage-accept.sh, the inline Vite acceptance
// driver. A real httptest TLS server stands in for the public API and a curl
// wrapper records argv, so no stage or network is touched.
package deploy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const acceptToken = "P28-SENTINEL-ACCEPT-TOKEN-4d1f"

const acceptRevision = "0123456789abcdef0123456789abcdef01234567"

const acceptRunID = "accept-run-1"

const acceptLog = "vite/8.3.6 linux-x64 node-v26.13.0\n" +
	"vite v8.3.6 building for production...\n" +
	"dist/index.html  0.42 kB\n" +
	"KODAMA_VITE_OK\n"

// acceptConfig is the part of the fake server's state a handler reads. The
// test writes it only through configure, which waits for every in-flight
// handler first; handlers copy it under the same mutex, so a test case can
// mutate the fake without racing the requests that drive the script.
type acceptConfig struct {
	submitStatus int
	replayed     bool
	state        string
	cleanup      string
	logChunks    []string
	wantYAML     []byte
}

type acceptServer struct {
	t *testing.T

	mu       sync.Mutex
	drained  *sync.Cond
	inFlight int
	config   acceptConfig

	submitted bool
	sawToken  bool
	keys      []string
}

// configure blocks until no handler is running and then applies change under
// the mutex, so the write is ordered after every handler read.
func (server *acceptServer) configure(t *testing.T, change func(*acceptConfig)) {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	for server.inFlight > 0 {
		server.drained.Wait()
	}
	change(&server.config)
}

// snapshot returns a copy of the configuration a handler serves one request
// from.
func (server *acceptServer) snapshot() acceptConfig {
	server.mu.Lock()
	defer server.mu.Unlock()
	config := server.config
	config.logChunks = append([]string(nil), server.config.logChunks...)
	return config
}

func (server *acceptServer) recordSubmit(key string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.submitted = true
	server.keys = append(server.keys, key)
}

func (server *acceptServer) recordToken() {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.sawToken = true
}

func (server *acceptServer) observations() (submitted, sawToken bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.submitted, server.sawToken
}

func (server *acceptServer) submittedKeys() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.keys...)
}

func (server *acceptServer) finishRequest() {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.inFlight--
	if server.inFlight == 0 {
		server.drained.Broadcast()
	}
}

func (server *acceptServer) handler() http.Handler {
	// The wrapper counts each request so configure never mutates server state
	// while a handler is reading it.
	mux := http.NewServeMux()
	server.routes(mux)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		server.mu.Lock()
		server.inFlight++
		server.mu.Unlock()
		defer server.finishRequest()
		mux.ServeHTTP(writer, request)
	})
}

func (server *acceptServer) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/projects/tenant-a/executions", func(writer http.ResponseWriter, request *http.Request) {
		config := server.snapshot()
		if request.Method != http.MethodPost {
			server.t.Errorf("submit method = %s; want POST", request.Method)
			http.Error(writer, "method", http.StatusMethodNotAllowed)
			return
		}
		if got := request.Header.Get("Authorization"); got == "Bearer "+acceptToken {
			server.recordToken()
		} else {
			server.t.Errorf("submit Authorization header = %q; want the bearer token from the file", got)
		}
		var envelope struct {
			Input struct {
				ProjectID string `json:"project_id"`
				YAML      string `json:"yaml"`
			} `json:"input"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&envelope); err != nil {
			server.t.Errorf("decode submit envelope: %v", err)
			http.Error(writer, "bad envelope", http.StatusBadRequest)
			return
		}
		if envelope.Input.ProjectID != "tenant-a" {
			server.t.Errorf("submit project_id = %q; want tenant-a", envelope.Input.ProjectID)
		}
		if envelope.IdempotencyKey == "" {
			server.t.Errorf("submit has no idempotency_key")
		}
		if len(envelope.IdempotencyKey) > 128 {
			server.t.Errorf("submit idempotency_key is %d bytes; want at most 128", len(envelope.IdempotencyKey))
		}
		decoded, err := base64.StdEncoding.DecodeString(envelope.Input.YAML)
		if err != nil {
			server.t.Errorf("decode inline yaml: %v", err)
		}
		if !bytes.Equal(decoded, config.wantYAML) {
			server.t.Errorf("submitted yaml differs from the checked-in bytes:\n%q\n%q", decoded, config.wantYAML)
		}
		server.recordSubmit(envelope.IdempotencyKey)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(config.submitStatus)
		if config.submitStatus != http.StatusOK {
			fmt.Fprint(writer, `{"code":"dependency_unavailable","message":"submit refused"}`)
			return
		}
		if config.replayed {
			fmt.Fprint(writer, `{"operation":"pipeline.start","replayed":true,"body":{"execution_id":"exec-1","version":1}}`)
			return
		}
		fmt.Fprint(writer, `{"operation":"pipeline.start","body":{"execution_id":"exec-1","version":1}}`)
	})
	mux.HandleFunc("/api/v1/executions/exec-1", func(writer http.ResponseWriter, request *http.Request) {
		config := server.snapshot()
		if request.Method != http.MethodGet {
			server.t.Errorf("inspect method = %s; want GET", request.Method)
		}
		var envelope struct {
			Input map[string]any `json:"input"`
		}
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&envelope); err != nil {
			server.t.Errorf("inspect body is not the public {input:{...}} envelope: %v", err)
			http.Error(writer, "bad envelope", http.StatusBadRequest)
			return
		}
		if len(envelope.Input) != 0 {
			server.t.Errorf("inspect input = %v; want an empty input object", envelope.Input)
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"operation":"execution.inspect","body":{"execution_id":"exec-1","state":%q,"version":1,"current_action":0,"actions":[{"action_id":"action-1","index":0,"name":"vite-build","kind":"container","state":"succeeded","version":1}],"cleanup":{"state":%q}}}`, config.state, config.cleanup)
	})
	mux.HandleFunc("/api/v1/executions/exec-1/logs", func(writer http.ResponseWriter, request *http.Request) {
		config := server.snapshot()
		if request.Method != http.MethodGet {
			server.t.Errorf("logs method = %s; want GET", request.Method)
		}
		var envelope struct {
			Input struct {
				ActionID string `json:"action_id"`
				Stream   string `json:"stream"`
				Page     struct {
					Limit  int    `json:"limit"`
					Cursor string `json:"cursor"`
				} `json:"page"`
			} `json:"input"`
		}
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&envelope); err != nil {
			server.t.Errorf("logs body is not the public {input:{...}} envelope: %v", err)
			http.Error(writer, "bad envelope", http.StatusBadRequest)
			return
		}
		body := envelope.Input
		if body.ActionID != "action-1" || body.Stream != "stdout" {
			server.t.Errorf("logs selection = %q/%q; want action-1/stdout", body.ActionID, body.Stream)
		}
		page := 0
		if body.Page.Cursor == "page-2" {
			page = 1
		}
		if page >= len(config.logChunks) {
			server.t.Errorf("logs requested an unknown page cursor %q", body.Page.Cursor)
		}
		chunk := config.logChunks[page]
		offset := 0
		for _, earlier := range config.logChunks[:page] {
			offset += len(earlier)
		}
		next := ""
		if page+1 < len(config.logChunks) {
			next = "page-2"
		}
		fmt.Fprintf(writer, `{"operation":"execution.logs.read","body":{"chunks":[{"action_id":"action-1","stream":"stdout","offset":%d,"sequence":%d,"data":%q}],"next_cursor":%q,"truncated":false}}`,
			offset, page+1, base64.StdEncoding.EncodeToString([]byte(chunk)), next)
	})
}

type acceptSandbox struct {
	root     string
	bin      string
	evidence string
	token    string
	ca       string
	curlLog  string
	script   string
	pipeline string
	server   *httptest.Server
	env      []string
	extraEnv []string
}

func newAcceptSandbox(t *testing.T, server *acceptServer) *acceptSandbox {
	t.Helper()
	root := t.TempDir()
	box := &acceptSandbox{
		root:     root,
		bin:      filepath.Join(root, "bin"),
		evidence: filepath.Join(root, "evidence"),
		token:    filepath.Join(root, "admin-token"),
		ca:       filepath.Join(root, "ca.pem"),
		curlLog:  filepath.Join(root, "curl.argv"),
		script:   filepath.Join(repoRoot(t), "scripts", "stage-accept.sh"),
		pipeline: filepath.Join(repoRoot(t), "deploy", "stage", "vite-acceptance.yaml"),
	}
	if err := os.MkdirAll(box.bin, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	yamlBytes, err := os.ReadFile(box.pipeline)
	if err != nil {
		t.Fatalf("read vite acceptance pipeline: %v", err)
	}
	server.configure(t, func(config *acceptConfig) { config.wantYAML = yamlBytes })

	box.server = httptest.NewTLSServer(server.handler())
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: box.server.Certificate().Raw})
	if certPEM == nil {
		t.Fatal("encode test server certificate")
	}
	writeFile(t, box.ca, string(certPEM))
	writeFile(t, box.token, acceptToken+"\n")

	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatalf("curl is required for the acceptance test: %v", err)
	}
	writeExecutable(t, filepath.Join(box.bin, "curl"), fmt.Sprintf(`#!/usr/bin/env bash
set -u
{
  printf 'curl'
  for arg in "$@"; do printf '\t%%s' "$arg"; done
  printf '\n'
} >> "$CURL_ARGV_LOG"
exec %q "$@"
`, realCurl))

	box.env = append(os.Environ(),
		"PATH="+box.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KODAMA_STAGE_API_URL="+box.server.URL,
		"KODAMA_STAGE_CA_FILE="+box.ca,
		"KODAMA_STAGE_ADMIN_TOKEN_FILE="+box.token,
		"KODAMA_STAGE_PROJECT_ID=tenant-a",
		"KODAMA_STAGE_PIPELINE_FILE="+box.pipeline,
		"KODAMA_STAGE_EVIDENCE_DIR="+box.evidence,
		"KODAMA_STAGE_RUN_ID="+acceptRunID,
		"Q01_DEPLOYED_REVISION="+acceptRevision,
		"CURL_ARGV_LOG="+box.curlLog,
	)
	return box
}

func (b *acceptSandbox) withEnv(entries ...string) *acceptSandbox {
	b.extraEnv = append(b.extraEnv, entries...)
	return b
}

func (b *acceptSandbox) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	command := exec.Command("bash", append([]string{b.script}, args...)...)
	command.Env = append(append([]string(nil), b.env...), b.extraEnv...)
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run scripts/stage-accept.sh: %v\n%s", err, output)
		}
		code = exitError.ExitCode()
	}
	return code, string(output)
}

func (b *acceptSandbox) curlCalls(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(b.curlLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read curl log: %v", err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func newAcceptTestServer(t *testing.T) *acceptServer {
	server := &acceptServer{
		t: t,
		config: acceptConfig{
			submitStatus: http.StatusOK,
			state:        "succeeded",
			cleanup:      "complete",
			logChunks:    []string{acceptLog},
		},
	}
	server.drained = sync.NewCond(&server.mu)
	return server
}

// TestStageAcceptServerSerializesReconfiguration is the regression for the race
// the kind component suite found: a case mutated the fake server while an
// in-flight handler was reading it. Readers poll the inspect endpoint while the
// test alternates the advertised state; without the mutex and the in-flight
// barrier, -race reports the concurrent access (or a handler observes a state
// the test never set).
func TestStageAcceptServerSerializesReconfiguration(t *testing.T) {
	server := newAcceptTestServer(t)
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	allowed := map[string]bool{"succeeded": true, "failed": true, "cancelled": true}
	const readers = 4
	const rounds = 25
	var wait sync.WaitGroup
	for range readers {
		wait.Go(func() {
			for range rounds {
				request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/executions/exec-1", strings.NewReader(`{"input":{}}`))
				if err != nil {
					t.Errorf("build inspect request: %v", err)
					return
				}
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Errorf("inspect request: %v", err)
					return
				}
				var envelope struct {
					Body struct {
						State   string `json:"state"`
						Cleanup struct {
							State string `json:"state"`
						} `json:"cleanup"`
					} `json:"body"`
				}
				decodeErr := json.NewDecoder(response.Body).Decode(&envelope)
				response.Body.Close()
				if decodeErr != nil {
					t.Errorf("decode inspect response: %v", decodeErr)
					return
				}
				if !allowed[envelope.Body.State] || envelope.Body.Cleanup.State != "complete" {
					t.Errorf("inspect reported state %q and cleanup %q; want a configured state and complete cleanup",
						envelope.Body.State, envelope.Body.Cleanup.State)
					return
				}
			}
		})
	}
	states := []string{"failed", "cancelled", "succeeded"}
	for round := range rounds {
		state := states[round%len(states)]
		server.configure(t, func(config *acceptConfig) { config.state = state })
	}
	wait.Wait()
}

// TestStageAcceptRefusesUnsafeRuns proves the guard set: transport, token
// custody, API outcome, terminal states and every content assertion fail
// closed.
func TestStageAcceptRefusesUnsafeRuns(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, box *acceptSandbox, server *acceptServer)
		want   string
	}{
		{
			name: "non-https URL",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				box.withEnv("KODAMA_STAGE_API_URL=http://127.0.0.1:1")
			},
			want: "https",
		},
		{
			name: "missing token file",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				box.withEnv("KODAMA_STAGE_ADMIN_TOKEN_FILE=" + filepath.Join(box.root, "absent-token"))
			},
			want: "token",
		},
		{
			name: "token file is not 0600",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				if err := os.Chmod(box.token, 0o644); err != nil {
					t.Fatalf("chmod token: %v", err)
				}
			},
			want: "0600",
		},
		{
			name: "unreadable CA file",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				box.withEnv("KODAMA_STAGE_CA_FILE=" + filepath.Join(box.root, "absent-ca"))
			},
			want: "CA file",
		},
		{
			name: "relative evidence directory",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				box.withEnv("KODAMA_STAGE_EVIDENCE_DIR=evidence")
			},
			want: "absolute",
		},
		{
			name: "non-2xx submit",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) { config.submitStatus = http.StatusBadGateway })
			},
			want: "HTTP",
		},
		{
			name: "failed execution",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) { config.state = "failed" })
			},
			want: "failed",
		},
		{
			name: "cancelled execution",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) { config.state = "cancelled" })
			},
			want: "cancelled",
		},
		{
			name: "cleanup not complete",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) { config.cleanup = "pending" })
			},
			want: "timed out",
		},
		{
			name: "kodex credential marker",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) {
					config.logChunks = []string{acceptLog + "lease kodex.abc123\n"}
				})
			},
			want: "kodex.",
		},
		{
			name: "kodsvc credential marker",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) {
					config.logChunks = []string{acceptLog + "token kodsvc.def456\n"}
				})
			},
			want: "kodsvc.",
		},
		{
			name: "missing build marker",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) {
					config.logChunks = []string{strings.ReplaceAll(acceptLog, "KODAMA_VITE_OK\n", "")}
				})
			},
			want: "KODAMA_VITE_OK",
		},
		{
			name: "missing dist artifact line",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) {
					config.logChunks = []string{strings.ReplaceAll(acceptLog, "dist/index.html  0.42 kB\n", "")}
				})
			},
			want: "dist/",
		},
		{
			name: "missing Vite version",
			mutate: func(t *testing.T, box *acceptSandbox, server *acceptServer) {
				server.configure(t, func(config *acceptConfig) {
					config.logChunks = []string{strings.ReplaceAll(acceptLog, "8.3.6", "9.0.0")}
				})
			},
			want: "8.3.6",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := newAcceptTestServer(t)
			box := newAcceptSandbox(t, server)
			defer box.server.Close()
			test.mutate(t, box, server)
			// A short poll bound keeps the cleanup-not-complete case hermetic.
			box.withEnv("KODAMA_STAGE_ACCEPT_TIMEOUT=2")
			box.withEnv("KODAMA_STAGE_ACCEPT_POLL_SECONDS=1")
			code, output := box.run(t)
			if code == 0 {
				t.Fatalf("stage-accept accepted an unsafe run:\n%s", output)
			}
			if !strings.Contains(output, test.want) {
				t.Errorf("refusal does not mention %q:\n%s", test.want, output)
			}
			for _, call := range box.curlCalls(t) {
				if strings.Contains(call, acceptToken) || strings.Contains(call, "Bearer") {
					t.Errorf("the token or a raw bearer header reached curl argv: %s", call)
				}
			}
		})
	}
}

// TestStageAcceptKeysSubmissionsPerRevision proves the submit idempotency key
// is scoped to the run/revision identity, not only the YAML bytes: the same
// checked-in pipeline submitted for a different run or a different deployed
// revision must never replay the earlier execution.
func TestStageAcceptKeysSubmissionsPerRevision(t *testing.T) {
	server := newAcceptTestServer(t)
	revisionA := strings.Repeat("a", 40)
	revisionB := strings.Repeat("b", 40)

	type attempt struct {
		name string
		run  string
		rev  string
	}
	attempts := []attempt{
		{name: "run-a revision-a", run: "run-a", rev: revisionA},
		{name: "run-b revision-a", run: "run-b", rev: revisionA},
		{name: "run-a revision-b", run: "run-a", rev: revisionB},
	}
	for _, attempt := range attempts {
		box := newAcceptSandbox(t, server)
		func() {
			defer box.server.Close()
			box.withEnv(
				"KODAMA_STAGE_RUN_ID="+attempt.run,
				"Q01_DEPLOYED_REVISION="+attempt.rev,
			)
			if code, output := box.run(t); code != 0 {
				t.Fatalf("%s: stage-accept failed: exit %d\n%s", attempt.name, code, output)
			}
		}()
	}

	keys := server.submittedKeys()
	if len(keys) != len(attempts) {
		t.Fatalf("submit keys = %v; want %d submissions", keys, len(attempts))
	}
	seen := map[string]string{}
	for index, key := range keys {
		if len(key) > 128 {
			t.Errorf("submit key %q is %d bytes; want at most 128", key, len(key))
		}
		if earlier, ok := seen[key]; ok {
			t.Fatalf("submissions for %q and %q reused the idempotency key %q; want a per-run/revision key",
				earlier, attempts[index].name, key)
		}
		seen[key] = attempts[index].name
	}
}

// TestStageAcceptRefusesReplayedSubmission proves a submit response carrying
// replayed=true fails closed before polling and prints only the execution id
// and the flag, so a replayed execution can never become acceptance evidence.
func TestStageAcceptRefusesReplayedSubmission(t *testing.T) {
	server := newAcceptTestServer(t)
	server.configure(t, func(config *acceptConfig) { config.replayed = true })
	box := newAcceptSandbox(t, server)
	defer box.server.Close()

	code, output := box.run(t)
	if code == 0 {
		t.Fatalf("stage-accept accepted a replayed execution:\n%s", output)
	}
	if !strings.Contains(output, "exec-1") {
		t.Errorf("replay refusal does not name the execution id:\n%s", output)
	}
	if !strings.Contains(output, "replayed") {
		t.Errorf("replay refusal does not report the replay flag:\n%s", output)
	}
	if strings.Contains(output, `"version":1`) || strings.Contains(output, "pipeline.start\"") {
		t.Errorf("replay refusal dumped the submit response:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(box.evidence, "stage-accept-summary.json")); !os.IsNotExist(err) {
		t.Errorf("replay refusal wrote a summary (stat err: %v); want no evidence for a replayed execution", err)
	}
	if calls := box.curlCalls(t); len(calls) != 1 {
		t.Errorf("curl calls = %d; want only the submit call before the replay refusal", len(calls))
	}
	if submitted, _ := server.observations(); !submitted {
		t.Error("the driver never submitted the pipeline")
	}
}

// TestStageAcceptAcceptsReplayOnlyOnOptIn proves the explicit escape hatch:
// KODAMA_STAGE_ALLOW_REPLAY=1 accepts the replayed execution and records the
// flag in the summary.
func TestStageAcceptAcceptsReplayOnlyOnOptIn(t *testing.T) {
	server := newAcceptTestServer(t)
	server.configure(t, func(config *acceptConfig) { config.replayed = true })
	box := newAcceptSandbox(t, server)
	defer box.server.Close()
	box.withEnv("KODAMA_STAGE_ALLOW_REPLAY=1")

	code, output := box.run(t)
	if code != 0 {
		t.Fatalf("stage-accept failed with the replay opt-in: exit %d\n%s", code, output)
	}
	body, err := os.ReadFile(filepath.Join(box.evidence, "stage-accept-summary.json"))
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var summary map[string]any
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatalf("decode summary: %v\n%s", err, body)
	}
	if replayed, _ := summary["replayed"].(bool); !replayed {
		t.Errorf("summary replayed = %v; want true with the opt-in", summary["replayed"])
	}
}

// TestStageAcceptRefusesMissingRunIdentity proves the driver fails before
// submit when neither a run id nor a deployed revision is supplied, rather
// than silently deriving the YAML-only key that replays the previous run.
func TestStageAcceptRefusesMissingRunIdentity(t *testing.T) {
	server := newAcceptTestServer(t)
	box := newAcceptSandbox(t, server)
	defer box.server.Close()
	box.withEnv(
		"KODAMA_STAGE_RUN_ID=",
		"Q01_DEPLOYED_REVISION=",
		"KODAMA_STAGE_REVISION=",
	)

	code, output := box.run(t)
	if code == 0 {
		t.Fatalf("stage-accept submitted without a run identity:\n%s", output)
	}
	if !strings.Contains(output, "KODAMA_STAGE_RUN_ID") {
		t.Errorf("refusal does not name the required run identity:\n%s", output)
	}
	if submitted, _ := server.observations(); submitted {
		t.Error("the driver submitted the pipeline before the missing-identity refusal")
	}
	if calls := box.curlCalls(t); len(calls) != 0 {
		t.Errorf("curl calls = %d; want none before the missing-identity refusal", len(calls))
	}
}

// TestStageAcceptSucceedsAndKeepsTheTokenOffArgv proves the happy path: exact
// YAML bytes are submitted once through the inline binding, polling waits for
// both success and complete cleanup, paged logs are reassembled and asserted,
// and the 0600 summary records the non-secret identity.
func TestStageAcceptSucceedsAndKeepsTheTokenOffArgv(t *testing.T) {
	server := newAcceptTestServer(t)
	split := len(acceptLog) / 2
	server.configure(t, func(config *acceptConfig) {
		config.logChunks = []string{acceptLog[:split], acceptLog[split:]}
	})
	box := newAcceptSandbox(t, server)
	defer box.server.Close()

	code, output := box.run(t)
	if code != 0 {
		t.Fatalf("stage-accept failed: exit %d\n%s", code, output)
	}
	submitted, sawToken := server.observations()
	if !submitted {
		t.Fatal("the driver never submitted the pipeline")
	}
	if !sawToken {
		t.Fatal("the server never saw the token from the private file")
	}
	for _, call := range box.curlCalls(t) {
		if strings.Contains(call, acceptToken) {
			t.Errorf("token appeared in curl argv: %s", call)
		}
		if strings.Contains(call, "Bearer") {
			t.Errorf("raw Authorization header appeared in curl argv: %s", call)
		}
	}
	if strings.Contains(output, acceptToken) {
		t.Errorf("stage-accept printed the token:\n%s", output)
	}

	summaryPath := filepath.Join(box.evidence, "stage-accept-summary.json")
	info, err := os.Stat(summaryPath)
	if err != nil {
		t.Fatalf("stat summary: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("summary mode = %04o; want 0600", info.Mode().Perm())
	}
	body, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var summary map[string]any
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatalf("decode summary: %v\n%s", err, body)
	}
	for key, want := range map[string]string{
		"execution_id":  "exec-1",
		"action_id":     "action-1",
		"cleanup_state": "complete",
		"state":         "succeeded",
		"project_id":    "tenant-a",
		"run_id":        acceptRunID,
		"revision":      acceptRevision,
	} {
		if got, _ := summary[key].(string); got != want {
			t.Errorf("summary %s = %q; want %q", key, got, want)
		}
	}
	if replayed, ok := summary["replayed"].(bool); !ok || replayed {
		t.Errorf("summary replayed = %v; want false", summary["replayed"])
	}
	if digest, _ := summary["yaml_sha256"].(string); !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Errorf("summary yaml_sha256 = %q; want a sha256 identity", digest)
	}
	if excerpt, _ := summary["sanitized_excerpt"].(string); !strings.Contains(excerpt, "KODAMA_VITE_OK") {
		t.Errorf("summary excerpt does not carry the build marker: %q", excerpt)
	}
}
