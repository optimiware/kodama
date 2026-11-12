package factory

// Bounded, file-backed external checkpoint service for executable agent
// workloads.
//
// The service is the fixture half of the agent workload proof. Workload
// containers run with no durable local state and no workspace mount; their
// conversation checkpoints live in a host directory outside every execution
// workspace and outside the runtime root, and the workload reaches them over
// the same workload edge the composition API uses. The service imports the real
// Userland file checkpoint storage (app.CheckpointStorage), so compare-and-swap,
// atomic rename, fsync and owner-only permissions stay product behavior. The
// fixture adds only transport, authentication and an explicit size bound so a
// runaway transcript cannot fill the host.
//
// Restart replaces the server, listener and storage instance over the same
// durable directory. A workload attempt after a restart can therefore restore
// only from bytes that survived the reopen; no in-process map or cached handle
// is carried across.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent/app"
	"github.com/optimiware/kodama/internal/userland/session"
)

const (
	// AgentCheckpointMaxRecordBytes bounds one persisted session record. It is
	// twice the 256 KiB context window, so one complete transcript and
	// its bookkeeping fit without the fixture becoming the limit.
	AgentCheckpointMaxRecordBytes int64 = 512 << 10
	// AgentCheckpointMaxStoreBytes bounds the whole durable directory.
	AgentCheckpointMaxStoreBytes int64 = 8 << 20

	agentCheckpointServicePrefix = "/v1/checkpoints"
	agentCheckpointSavePath      = agentCheckpointServicePrefix + "/save"
	agentCheckpointReadPath      = agentCheckpointServicePrefix + "/read"
	agentCheckpointHealthPath    = agentCheckpointServicePrefix + "/health"

	// agentCheckpointWorkloadHost is the only alias a rootless pasta workload
	// reaches (see workloadListenHost).
	agentCheckpointWorkloadHost = "host.containers.internal"

	// agentCheckpointTokenBytes is the entropy of one per-service bearer
	// token. The token is generated with crypto/rand and is delivered to a
	// workload in a mounted file, never in argv, environment or evidence.
	agentCheckpointTokenBytes = 24

	// agentCheckpointEnvelopeSlack covers the JSON envelope around one record.
	agentCheckpointEnvelopeSlack int64 = 4096

	agentCheckpointCodeUnauthenticated = "unauthenticated"
	agentCheckpointCodeMethod          = "method_not_allowed"
	agentCheckpointCodeInvalidInput    = "invalid_input"
	agentCheckpointCodeRecordTooLarge  = "record_too_large"
	agentCheckpointCodeStoreFull       = "store_full"
	agentCheckpointCodeVersionConflict = "version_conflict"
	agentCheckpointCodeMissing         = "missing"
	agentCheckpointCodeUnavailable     = "storage_closed"
	agentCheckpointCodeInternal        = "internal_failure"
)

// errAgentCheckpointClosed reports a service whose durable storage is not open.
var errAgentCheckpointClosed = errors.New("factory: agent checkpoint storage is closed")

// AgentCheckpointConfig configures one bounded external checkpoint service.
type AgentCheckpointConfig struct {
	// T receives cleanup and fatal diagnostics. It defaults to the Start
	// receiver.
	T testing.TB
	// Root is the host durable directory. It must be outside every execution
	// workspace and outside the runtime root; the workload never mounts it.
	Root string
	// WorkloadHost is the concrete host address the workload edge binds. An
	// empty value creates no routable listener; unit tests use the loopback
	// endpoint directly.
	WorkloadHost string
	// MaxRecordBytes and MaxStoreBytes override the defaults when positive.
	MaxRecordBytes int64
	MaxStoreBytes  int64
	// Token overrides the generated bearer token. Tests use it only to prove
	// an unauthorized caller is refused.
	Token string
}

// AgentCheckpointService is one bounded external checkpoint service.
type AgentCheckpointService struct {
	t              testing.TB
	root           string
	token          string
	workloadHost   string
	maxRecordBytes int64
	maxStoreBytes  int64
	closeOnce      sync.Once
	saveMu         sync.Mutex
	mu             sync.Mutex
	closed         bool
	generation     int
	storage        *app.CheckpointStorage
	listener       net.Listener
	server         *http.Server
	relay          *edgeForwarder
	endpoint       string
	workloadURL    string
	serveDone      chan struct{}
}

// agentCheckpointSaveRequest is the save envelope.
type agentCheckpointSaveRequest struct {
	ExpectedRevision uint64         `json:"expected_revision"`
	Record           session.Record `json:"record"`
}

// agentCheckpointSaveResponse is one committed reference.
type agentCheckpointSaveResponse struct {
	Ref string `json:"ref"`
}

// agentCheckpointReadRequest is the read envelope.
type agentCheckpointReadRequest struct {
	Ref string `json:"ref"`
}

// agentCheckpointReadResponse is one immutable record.
type agentCheckpointReadResponse struct {
	Record session.Record `json:"record"`
}

// agentCheckpointHealthResponse reports the durable state without content.
type agentCheckpointHealthResponse struct {
	Generation int   `json:"generation"`
	Refs       int   `json:"refs"`
	Bytes      int64 `json:"bytes"`
}

// agentCheckpointError is one typed refusal.
type agentCheckpointError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StartAgentCheckpointService opens the durable directory and serves it on
// loopback plus, when configured, the workload edge. Cleanup is registered
// before the fallible listener work.
func StartAgentCheckpointService(t testing.TB, config AgentCheckpointConfig) *AgentCheckpointService {
	t.Helper()
	if config.T != nil {
		t = config.T
	}
	root := strings.TrimSpace(config.Root)
	if root == "" {
		root = t.TempDir()
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("factory: resolve agent checkpoint root %q: %v", root, err)
	}
	if config.MaxRecordBytes <= 0 {
		config.MaxRecordBytes = AgentCheckpointMaxRecordBytes
	}
	if config.MaxStoreBytes <= 0 {
		config.MaxStoreBytes = AgentCheckpointMaxStoreBytes
	}
	if config.MaxRecordBytes > config.MaxStoreBytes {
		t.Fatalf("factory: agent checkpoint record bound %d exceeds the store bound %d",
			config.MaxRecordBytes, config.MaxStoreBytes)
	}
	token := strings.TrimSpace(config.Token)
	if token == "" {
		token = newAgentCheckpointToken(t)
	}
	service := &AgentCheckpointService{
		t:              t,
		root:           filepath.Clean(absolute),
		token:          token,
		workloadHost:   strings.TrimSpace(config.WorkloadHost),
		maxRecordBytes: config.MaxRecordBytes,
		maxStoreBytes:  config.MaxStoreBytes,
	}
	t.Cleanup(service.Close)
	if err := service.open(); err != nil {
		t.Fatalf("factory: start agent checkpoint service: %v", err)
	}
	return service
}

// newAgentCheckpointToken mints one per-service bearer token.
func newAgentCheckpointToken(t testing.TB) string {
	t.Helper()
	raw := make([]byte, agentCheckpointTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("factory: generate agent checkpoint token: %v", err)
	}
	return hex.EncodeToString(raw)
}

// open builds the storage, the loopback listener and, when configured, the
// routable relay. It increments the generation so a restart is observable.
func (service *AgentCheckpointService) open() error {
	storage, err := app.NewCheckpointStorage(app.StorageConfig{Root: service.root})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	workloadURL := ""
	var relay *edgeForwarder
	if service.workloadHost != "" {
		var port int
		var edgeErr error
		relay, port, edgeErr = newWorkloadEdge(service.workloadHost, listener.Addr().String())
		if edgeErr != nil {
			_ = listener.Close()
			return fmt.Errorf("workload edge: %w", edgeErr)
		}
		workloadURL = fmt.Sprintf("http://%s:%d", agentCheckpointWorkloadHost, port)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(service.handle),
		ReadHeaderTimeout: 5 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()

	service.mu.Lock()
	service.storage = storage
	service.listener = listener
	service.server = server
	service.relay = relay
	service.endpoint = "http://" + listener.Addr().String()
	service.workloadURL = workloadURL
	service.serveDone = done
	service.generation++
	service.mu.Unlock()
	return nil
}

// Root returns the durable host directory.
func (service *AgentCheckpointService) Root() string { return service.root }

// Token returns the per-service bearer token. It is test-only material and is
// written into a mounted file by the workload runner.
func (service *AgentCheckpointService) Token() string { return service.token }

// Endpoint returns the loopback URL unit tests use.
func (service *AgentCheckpointService) Endpoint() string {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.endpoint
}

// WorkloadURL returns the container-reachable URL, or "" when no workload host
// was configured.
func (service *AgentCheckpointService) WorkloadURL() string {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.workloadURL
}

// Generation reports how many storage instances have served this directory.
func (service *AgentCheckpointService) Generation() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.generation
}

// Restart closes the current server, listener and storage instance and reopens
// the same durable directory behind a new one. Only bytes on disk survive.
func (service *AgentCheckpointService) Restart() {
	service.t.Helper()
	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		service.t.Fatalf("factory: agent checkpoint service is closed")
		return
	}
	server, relay, done := service.server, service.relay, service.serveDone
	service.storage = nil
	service.listener = nil
	service.server = nil
	service.relay = nil
	service.endpoint = ""
	service.workloadURL = ""
	service.serveDone = nil
	service.mu.Unlock()

	if server != nil {
		_ = server.Close()
	}
	if relay != nil {
		relay.Close()
	}
	if done != nil {
		<-done
	}
	if err := service.open(); err != nil {
		service.t.Fatalf("factory: reopen agent checkpoint service: %v", err)
	}
}

// Close stops the service and joins its goroutines. It is idempotent and keeps
// the durable directory untouched.
func (service *AgentCheckpointService) Close() {
	service.closeOnce.Do(func() {
		service.mu.Lock()
		service.closed = true
		server, relay, done := service.server, service.relay, service.serveDone
		service.storage = nil
		service.listener = nil
		service.server = nil
		service.relay = nil
		service.serveDone = nil
		// The endpoint and workload URL stay published: a closed service must
		// be reachable enough to refuse, so a workload run proves the container
		// cannot fall back to a local copy.
		service.mu.Unlock()

		if server != nil {
			_ = server.Close()
		}
		if relay != nil {
			relay.Close()
		}
		if done != nil {
			<-done
		}
	})
}

// currentStorage returns the open durable store.
func (service *AgentCheckpointService) currentStorage() (*app.CheckpointStorage, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.closed {
		return nil, errAgentCheckpointClosed
	}
	if service.storage == nil {
		return nil, errAgentCheckpointClosed
	}
	return service.storage, nil
}

// Refs returns every committed reference, sorted.
func (service *AgentCheckpointService) Refs() ([]string, error) {
	entries, err := os.ReadDir(service.root)
	if err != nil {
		return nil, fmt.Errorf("read agent checkpoint root: %w", err)
	}
	refs := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "head-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(service.root, name))
		if err != nil {
			return nil, fmt.Errorf("read agent checkpoint head %s: %w", name, err)
		}
		var head struct {
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			return nil, fmt.Errorf("decode agent checkpoint head %s: %w", name, err)
		}
		if head.Ref != "" {
			refs = append(refs, head.Ref)
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// StoredBytes reports the durable directory size.
func (service *AgentCheckpointService) StoredBytes() (int64, error) {
	return directoryBytes(service.root)
}

// Read returns one committed record through the real storage. It is the
// host-side observation of the same bytes a workload restores.
func (service *AgentCheckpointService) Read(ref string) (session.Record, error) {
	storage, err := service.currentStorage()
	if err != nil {
		return session.Record{}, err
	}
	record, err := storage.Read(context.Background(), session.Ref(ref))
	if err != nil {
		return session.Record{}, err
	}
	return record, nil
}

// directoryBytes sums the regular files below root.
func directoryBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure agent checkpoint root: %w", err)
	}
	return total, nil
}

// authorized checks the per-service bearer token in constant time.
func (service *AgentCheckpointService) authorized(request *http.Request) bool {
	const prefix = "Bearer "
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(provided), []byte(service.token)) == 1
}

// handle dispatches one authenticated request.
func (service *AgentCheckpointService) handle(writer http.ResponseWriter, request *http.Request) {
	if !service.authorized(request) {
		writeAgentCheckpointError(writer, http.StatusUnauthorized, agentCheckpointCodeUnauthenticated, "checkpoint service token is required")
		return
	}
	switch request.URL.Path {
	case agentCheckpointSavePath:
		if request.Method != http.MethodPost {
			writeAgentCheckpointError(writer, http.StatusMethodNotAllowed, agentCheckpointCodeMethod, "save requires POST")
			return
		}
		service.handleSave(writer, request)
	case agentCheckpointReadPath:
		if request.Method != http.MethodPost {
			writeAgentCheckpointError(writer, http.StatusMethodNotAllowed, agentCheckpointCodeMethod, "read requires POST")
			return
		}
		service.handleRead(writer, request)
	case agentCheckpointHealthPath:
		if request.Method != http.MethodGet {
			writeAgentCheckpointError(writer, http.StatusMethodNotAllowed, agentCheckpointCodeMethod, "health requires GET")
			return
		}
		service.handleHealth(writer)
	default:
		writeAgentCheckpointError(writer, http.StatusNotFound, agentCheckpointCodeMissing, "unknown checkpoint endpoint")
	}
}

// handleSave enforces the bounds, then commits through the real storage's
// compare-and-swap.
func (service *AgentCheckpointService) handleSave(writer http.ResponseWriter, request *http.Request) {
	limited := http.MaxBytesReader(writer, request.Body, service.maxRecordBytes+agentCheckpointEnvelopeSlack)
	data, err := io.ReadAll(limited)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeAgentCheckpointError(writer, http.StatusRequestEntityTooLarge, agentCheckpointCodeRecordTooLarge, "record exceeds the service bound")
			return
		}
		writeAgentCheckpointError(writer, http.StatusBadRequest, agentCheckpointCodeInvalidInput, "record body is unreadable")
		return
	}
	var envelope agentCheckpointSaveRequest
	if err := json.Unmarshal(data, &envelope); err != nil {
		writeAgentCheckpointError(writer, http.StatusBadRequest, agentCheckpointCodeInvalidInput, "record body is not a save envelope")
		return
	}
	if err := envelope.Record.Validate(); err != nil {
		writeAgentCheckpointError(writer, http.StatusBadRequest, agentCheckpointCodeInvalidInput, "record is invalid")
		return
	}
	encoded, err := json.Marshal(envelope.Record)
	if err != nil {
		writeAgentCheckpointError(writer, http.StatusInternalServerError, agentCheckpointCodeInternal, "record cannot be encoded")
		return
	}
	if int64(len(encoded)) > service.maxRecordBytes {
		writeAgentCheckpointError(writer, http.StatusRequestEntityTooLarge, agentCheckpointCodeRecordTooLarge, "record exceeds the service bound")
		return
	}

	service.saveMu.Lock()
	defer service.saveMu.Unlock()
	storage, err := service.currentStorage()
	if err != nil {
		writeAgentCheckpointError(writer, http.StatusServiceUnavailable, agentCheckpointCodeUnavailable, "checkpoint storage is closed")
		return
	}
	stored, err := directoryBytes(service.root)
	if err != nil {
		writeAgentCheckpointError(writer, http.StatusInternalServerError, agentCheckpointCodeInternal, "checkpoint store cannot be measured")
		return
	}
	if stored+int64(len(encoded)) > service.maxStoreBytes {
		writeAgentCheckpointError(writer, http.StatusRequestEntityTooLarge, agentCheckpointCodeStoreFull, "checkpoint store is full")
		return
	}
	ref, err := storage.CompareAndSwap(request.Context(), envelope.Record, envelope.ExpectedRevision)
	if err != nil {
		if class, classified := fault.ClassOf(err); classified && class == fault.VersionConflict {
			writeAgentCheckpointError(writer, http.StatusConflict, agentCheckpointCodeVersionConflict, "checkpoint revision is stale")
			return
		}
		writeAgentCheckpointError(writer, http.StatusInternalServerError, agentCheckpointCodeInternal, "checkpoint commit failed")
		return
	}
	writeAgentCheckpointJSON(writer, http.StatusOK, agentCheckpointSaveResponse{Ref: string(ref)})
}

// handleRead returns one immutable record or a typed missing result.
func (service *AgentCheckpointService) handleRead(writer http.ResponseWriter, request *http.Request) {
	limited := http.MaxBytesReader(writer, request.Body, 2*session.MaxRefBytes+agentCheckpointEnvelopeSlack)
	data, err := io.ReadAll(limited)
	if err != nil {
		writeAgentCheckpointError(writer, http.StatusBadRequest, agentCheckpointCodeInvalidInput, "read body is unreadable")
		return
	}
	var envelope agentCheckpointReadRequest
	if err := json.Unmarshal(data, &envelope); err != nil {
		writeAgentCheckpointError(writer, http.StatusBadRequest, agentCheckpointCodeInvalidInput, "read body is not a read envelope")
		return
	}
	if _, err := session.ParseRef(envelope.Ref); err != nil {
		writeAgentCheckpointError(writer, http.StatusBadRequest, agentCheckpointCodeInvalidInput, "reference is invalid")
		return
	}
	storage, err := service.currentStorage()
	if err != nil {
		writeAgentCheckpointError(writer, http.StatusServiceUnavailable, agentCheckpointCodeUnavailable, "checkpoint storage is closed")
		return
	}
	record, err := storage.Read(request.Context(), session.Ref(envelope.Ref))
	if err != nil {
		switch {
		case errors.Is(err, session.ErrCheckpointMissing):
			writeAgentCheckpointError(writer, http.StatusNotFound, agentCheckpointCodeMissing, "checkpoint was not found")
		case errors.Is(err, app.ErrCorruptCheckpoint):
			writeAgentCheckpointError(writer, http.StatusInternalServerError, agentCheckpointCodeInternal, "checkpoint is corrupt")
		default:
			writeAgentCheckpointError(writer, http.StatusInternalServerError, agentCheckpointCodeInternal, "checkpoint read failed")
		}
		return
	}
	writeAgentCheckpointJSON(writer, http.StatusOK, agentCheckpointReadResponse{Record: record})
}

// handleHealth reports the durable state without any transcript content.
func (service *AgentCheckpointService) handleHealth(writer http.ResponseWriter) {
	refs, err := service.Refs()
	if err != nil {
		writeAgentCheckpointError(writer, http.StatusInternalServerError, agentCheckpointCodeInternal, "checkpoint store cannot be enumerated")
		return
	}
	stored, err := service.StoredBytes()
	if err != nil {
		writeAgentCheckpointError(writer, http.StatusInternalServerError, agentCheckpointCodeInternal, "checkpoint store cannot be measured")
		return
	}
	writeAgentCheckpointJSON(writer, http.StatusOK, agentCheckpointHealthResponse{
		Generation: service.Generation(),
		Refs:       len(refs),
		Bytes:      stored,
	})
}

// writeAgentCheckpointJSON writes one JSON response.
func writeAgentCheckpointJSON(writer http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, "encode response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(data)
}

// writeAgentCheckpointError writes one typed refusal.
func writeAgentCheckpointError(writer http.ResponseWriter, status int, code, message string) {
	writeAgentCheckpointJSON(writer, status, agentCheckpointError{Code: code, Message: message})
}
