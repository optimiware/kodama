package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/installation"
	"github.com/optimiware/kodema/internal/shell/app/bridges"
	opsapp "github.com/optimiware/kodema/internal/shell/app/ops"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/identity"
	natsbroker "github.com/optimiware/kodema/internal/shell/integration/nats"
	"github.com/optimiware/kodema/internal/shell/integration/oidc"
	"github.com/optimiware/kodema/internal/shell/service"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/store/memory"
	identitystore "github.com/optimiware/kodema/internal/shell/store/units/identity"
	"github.com/optimiware/kodema/internal/shell/store/units/platform"
	"github.com/optimiware/kodema/internal/shell/store/units/readers"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/token"
	transportops "github.com/optimiware/kodema/internal/shell/transport/ops"
)

type sseFrame struct {
	ID    string
	Event string
	Data  string
}

func readSSEFrames(t *testing.T, body io.Reader, want int, timeout time.Duration) ([]sseFrame, []string) {
	t.Helper()
	type result struct {
		frames   []sseFrame
		comments []string
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 8192), 512<<10)
		var (
			frames   []sseFrame
			comments []string
			cur      sseFrame
			dataBuf  []string
		)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if len(dataBuf) > 0 {
					cur.Data = strings.Join(dataBuf, "\n")
					frames = append(frames, cur)
					cur = sseFrame{}
					dataBuf = nil
					if want > 0 && len(frames) >= want {
						ch <- result{frames: frames, comments: comments}
						return
					}
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				comments = append(comments, line)
				continue
			}
			field, val, _ := strings.Cut(line, ":")
			val = strings.TrimPrefix(val, " ")
			switch field {
			case "id":
				cur.ID = val
			case "event":
				cur.Event = val
			case "data":
				dataBuf = append(dataBuf, val)
			}
		}
		ch <- result{frames: frames, comments: comments, err: scanner.Err()}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("readSSEFrames scanner err: %v", res.err)
		}
		return res.frames, res.comments
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for %d SSE frame(s)", timeout, want)
		return nil, nil
	}
}

type staticEventStreamCredSource struct {
	mu    sync.Mutex
	token string
	exp   time.Time
	kind  client.CredentialKind
}

func (s *staticEventStreamCredSource) Credential(context.Context) (client.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return client.Credential{Kind: s.kind, Token: s.token, ExpiresAt: s.exp}, nil
}

func (s *staticEventStreamCredSource) set(tok string, exp time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = tok
	s.exp = exp
}

// TestProjectEventStream_DevCompositionEndToEnd exercises the real composed App
// HTTP SSE endpoint end-to-end: auth refusal, cross-project isolation,
// subscribe-before-backfill with zero gaps/duplicates, base64 JSON payload
// decoding, type filtering, Last-Event-ID resume, and client.HTTPTransport
// streaming with reconnect deduplication.
func TestProjectEventStream_DevCompositionEndToEnd(t *testing.T) {
	ctx := context.Background()
	tenantID := id.TenantID("tenant-stream")
	projA := id.ProjectID("proj-a")
	projB := id.ProjectID("proj-b")
	scopeA := tenancy.Scope{TenantID: tenantID, ProjectID: projA}
	scopeB := tenancy.Scope{TenantID: tenantID, ProjectID: projB}

	const (
		adminToken  = "dev-tenant-admin-stream"
		projAToken  = "dev-proj-a-viewer-stream"
		projA2Token = "dev-proj-a2-engineer-stream"
		projBToken  = "dev-proj-b-viewer-stream"
		execToken   = "dev-exec-stream"
	)

	cfg := Config{
		Profile: ProfileDevelopment,
		Installation: installation.Config{
			Environment: installation.EnvironmentDevelopment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyEphemeral,
			PublicURL:   "http://127.0.0.1:8080",
		},
		InstallationID: "inst-stream-test",
		DataDir:        t.TempDir(),
		Dev: &DevelopmentConfig{
			Bootstrap: &Bootstrap{
				TenantID:       tenantID,
				TenantName:     "Stream Tenant",
				ProjectID:      projA,
				ProjectName:    "Project A",
				AdminPrincipal: "human-admin",
			},
			Tokens: []DevToken{
				{
					Credential: adminToken,
					Principal:  identity.Principal{ID: "human-admin", Kind: identity.KindHuman, Scope: tenancy.Scope{TenantID: tenantID}, Roles: []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer}, Enabled: true, Version: 1},
					ExpiresAt:  time.Now().Add(2 * time.Hour),
				},
				{
					Credential: projAToken,
					Principal:  identity.Principal{ID: "human-a", Kind: identity.KindHuman, Scope: scopeA, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1},
					ExpiresAt:  time.Now().Add(2 * time.Hour),
				},
				{
					Credential: projA2Token,
					Principal:  identity.Principal{ID: "human-a2", Kind: identity.KindHuman, Scope: scopeA, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1},
					ExpiresAt:  time.Now().Add(2 * time.Hour),
				},
				{
					Credential: projBToken,
					Principal:  identity.Principal{ID: "human-b", Kind: identity.KindHuman, Scope: scopeB, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1},
					ExpiresAt:  time.Now().Add(2 * time.Hour),
				},
				{
					Credential: execToken,
					Principal:  identity.Principal{ID: "exec-principal-1", Kind: identity.KindExecution, Scope: scopeA, Enabled: true, Version: 1, ExecutionID: "exec-1"},
					ExpiresAt:  time.Now().Add(2 * time.Hour),
				},
			},
		},
	}

	application, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = application.Close(context.Background()) })

	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	adminClient := &client.HTTPTransport{
		BaseURL:      server.URL,
		RoundTripper: server.Client().Transport,
		Source:       &staticEventStreamCredSource{kind: client.KindSession, token: adminToken, exp: time.Now().Add(time.Hour)},
		Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
	projAClient := &client.HTTPTransport{
		BaseURL:      server.URL,
		RoundTripper: server.Client().Transport,
		Source:       &staticEventStreamCredSource{kind: client.KindSession, token: projAToken, exp: time.Now().Add(time.Hour)},
		Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
	projA2Client := &client.HTTPTransport{
		BaseURL:      server.URL,
		RoundTripper: server.Client().Transport,
		Source:       &staticEventStreamCredSource{kind: client.KindSession, token: projA2Token, exp: time.Now().Add(time.Hour)},
		Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
	projBClient := &client.HTTPTransport{
		BaseURL:      server.URL,
		RoundTripper: server.Client().Transport,
		Source:       &staticEventStreamCredSource{kind: client.KindSession, token: projBToken, exp: time.Now().Add(time.Hour)},
		Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}

	// Provision Project B and project memberships for human-a, human-a2, and human-b.
	invokeMust := func(tb testing.TB, tr client.Transport, op string, body any, key string) {
		tb.Helper()
		raw, _ := json.Marshal(body)
		if _, err := tr.Invoke(ctx, api.Invocation{
			Operation: op,
			Request:   api.Request{Input: raw, IdempotencyKey: key},
		}); err != nil {
			tb.Fatalf("invoke %s: %v", op, err)
		}
	}
	invokeMust(t, adminClient, "project.create", map[string]any{"tenant_id": string(tenantID), "project_id": string(projB), "name": "Project B"}, "k-proj-b")
	invokeMust(t, adminClient, "membership.grant", map[string]any{"project_id": string(projA), "principal_id": "human-a", "roles": []string{"engineer"}}, "k-mem-a")
	invokeMust(t, adminClient, "membership.grant", map[string]any{"project_id": string(projA), "principal_id": "human-a2", "roles": []string{"engineer"}}, "k-mem-a2")
	invokeMust(t, adminClient, "membership.grant", map[string]any{"project_id": string(projB), "principal_id": "human-b", "roles": []string{"engineer"}}, "k-mem-b")

	// 1. Unauthenticated request is refused with 401.
	t.Run("unauthenticated_refused", func(t *testing.T) {
		resp, err := server.Client().Get(server.URL + "/api/v1/projects/proj-a/events/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d; want 401", resp.StatusCode)
		}
	})

	// 2. Cross-project request from proj-a principal targeting proj-b is refused with 404.
	t.Run("cross_project_refused", func(t *testing.T) {
		_, err := projAClient.StreamEvents(ctx, client.EventStreamRequest{ProjectID: string(projB)})
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("cross-project StreamEvents err = %v (class=%v); want not_found", err, class)
		}
	})

	// 2b. Execution principal is refused with 403 forbidden.
	t.Run("execution_principal_refused", func(t *testing.T) {
		execClient := &client.HTTPTransport{
			BaseURL:      server.URL,
			RoundTripper: server.Client().Transport,
			Source:       &staticEventStreamCredSource{kind: client.KindExecution, token: execToken, exp: time.Now().Add(time.Hour)},
			Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		}
		_, err := execClient.StreamEvents(ctx, client.EventStreamRequest{ProjectID: string(projA)})
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("execution StreamEvents err = %v (class=%v); want forbidden", err, class)
		}
	})

	// 3. Method not allowed / unknown query param / unknown event type refused.
	t.Run("method_and_query_validation", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/projects/proj-a/events/stream", nil)
		req.Header.Set("Authorization", "Bearer "+projAToken)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST status = %d; want 405", resp.StatusCode)
		}

		req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/projects/proj-a/events/stream?bogus=1", nil)
		req2.Header.Set("Authorization", "Bearer "+projAToken)
		resp2, err := server.Client().Do(req2)
		if err != nil {
			t.Fatalf("GET bogus: %v", err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET ?bogus=1 status = %d; want 400", resp2.StatusCode)
		}

		req3, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/projects/proj-a/events/stream?type=not.a.real.type", nil)
		req3.Header.Set("Authorization", "Bearer "+projAToken)
		resp3, err := server.Client().Do(req3)
		if err != nil {
			t.Fatalf("GET bad type: %v", err)
		}
		resp3.Body.Close()
		if resp3.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET bad type status = %d; want 400", resp3.StatusCode)
		}
	})

	// 4. Backfill from cursor(0) + live event streaming + cross-project isolation + decoded JSON data + Last-Event-ID resume.
	t.Run("backfill_to_live_decoded_json_and_resume", func(t *testing.T) {
		// Create task 1 in proj-a and a custom observation with JSON data in proj-a, plus a task in proj-b (must NEVER leak!).
		invokeMust(t, projAClient, "task.create", map[string]any{"project_id": string(projA), "title": "Task A1"}, "k-task-a1")
		invokeMust(t, projAClient, "event.publish", map[string]any{
			"project_id": string(projA),
			"type":       "observation.custom",
			"subject_id": "obs-subject-1",
			"data":       []byte(`{"step":"init","count":7}`),
		}, "k-obs-1")
		invokeMust(t, projBClient, "task.create", map[string]any{"project_id": string(projB), "title": "Secret Project B Task"}, "k-task-b1")

		cursorZero, err := api.EncodeEventCursor(0)
		if err != nil {
			t.Fatalf("EncodeEventCursor(0): %v", err)
		}

		// Filter to task.created and observation.custom from cursor(0).
		streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		stream, err := projAClient.StreamEvents(streamCtx, client.EventStreamRequest{
			ProjectID: string(projA),
			Since:     cursorZero,
			Types:     []string{"task.created", "observation.custom"},
		})
		if err != nil {
			t.Fatalf("StreamEvents: %v", err)
		}
		defer stream.Close()

		evt1, err := stream.Next(streamCtx)
		if err != nil {
			t.Fatalf("Next(1): %v", err)
		}
		evt2, err := stream.Next(streamCtx)
		if err != nil {
			t.Fatalf("Next(2): %v", err)
		}

		if evt1.Type != "task.created" || evt1.Scope.ProjectID != string(projA) {
			t.Fatalf("evt1 = %+v; want task.created in proj-a", evt1)
		}
		if evt2.Type != "observation.custom" || evt2.SubjectID != "obs-subject-1" {
			t.Fatalf("evt2 = %+v; want observation.custom obs-subject-1", evt2)
		}
		// Verify base64 data was decoded into a real JSON object!
		var decodedData map[string]any
		if err := json.Unmarshal(evt2.Data, &decodedData); err != nil {
			t.Fatalf("evt2.Data is not decoded JSON object (%s): %v", string(evt2.Data), err)
		}
		if decodedData["step"] != "init" || decodedData["count"] != float64(7) {
			t.Fatalf("evt2.Data = %v; want {step:init, count:7}", decodedData)
		}
		if evt2.Sequence <= evt1.Sequence {
			t.Fatalf("sequences not strictly monotonic: %d <= %d", evt2.Sequence, evt1.Sequence)
		}

		// Emit a live event in proj-b (must NOT appear) followed by a live event in proj-a (must appear immediately via wakeup!).
		invokeMust(t, projBClient, "task.create", map[string]any{"project_id": string(projB), "title": "Secret Project B Task 2"}, "k-task-b2")
		invokeMust(t, projAClient, "task.create", map[string]any{"project_id": string(projA), "title": "Task A2 Live"}, "k-task-a2")

		evt3, err := stream.Next(streamCtx)
		if err != nil {
			t.Fatalf("Next(3): %v", err)
		}
		if evt3.Type != "task.created" || evt3.Scope.ProjectID != string(projA) || evt3.Sequence <= evt2.Sequence {
			t.Fatalf("evt3 = %+v; want live task.created in proj-a after seq %d", evt3, evt2.Sequence)
		}

		// Verify Last-Event-ID header takes precedence over ?since=cursor(0) and resumes without duplicates.
		req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet,
			server.URL+"/api/v1/projects/proj-a/events/stream?since="+cursorZero+"&type=task.created&type=observation.custom", nil)
		req.Header.Set("Authorization", "Bearer "+projAToken)
		req.Header.Set("Last-Event-ID", evt2.Cursor)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("GET Last-Event-ID: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, api.EventStreamMediaType) {
			t.Fatalf("Content-Type = %q; want %s", ct, api.EventStreamMediaType)
		}
		frames, _ := readSSEFrames(t, resp.Body, 1, 3*time.Second)
		if len(frames) != 1 {
			t.Fatalf("frames = %d; want 1", len(frames))
		}
		if frames[0].ID != evt3.Cursor || frames[0].Event != "task.created" {
			t.Fatalf("resumed frame = %+v; want id=%s event=task.created", frames[0], evt3.Cursor)
		}
	})

	// 5. Revoking a principal's membership terminates an open stream cleanly
	// without emitting any subsequent events committed after revocation.
	t.Run("membership_revocation_terminates_stream_cleanly", func(t *testing.T) {
		streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet,
			server.URL+"/api/v1/projects/proj-a/events/stream", nil)
		req.Header.Set("Authorization", "Bearer "+projAToken)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("GET stream before revoke: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; want 200", resp.StatusCode)
		}

		// Read until EOF in background.
		doneCh := make(chan []sseFrame, 1)
		go func() {
			frames, _ := readSSEFrames(t, resp.Body, 0, 4*time.Second)
			doneCh <- frames
		}()

		// Revoke human-a's membership in proj-a (version 1).
		revokeRaw, _ := json.Marshal(map[string]any{
			"project_id":   string(projA),
			"principal_id": "human-a",
		})
		expectedVer := uint64(1)
		if _, err := adminClient.Invoke(ctx, api.Invocation{
			Operation: "membership.revoke",
			Request:   api.Request{Input: revokeRaw, ExpectedVersion: &expectedVer, IdempotencyKey: "k-revoke-a"},
		}); err != nil {
			t.Fatalf("membership.revoke: %v", err)
		}

		// Emit a post-revocation event from another still-authorized member;
		// human-a's stream must close without delivering it.
		invokeMust(t, projA2Client, "task.create", map[string]any{
			"project_id": string(projA),
			"title":      "Post-Revoke Secret Task",
		}, "k-task-post-revoke")

		select {
		case frames := <-doneCh:
			for _, f := range frames {
				if strings.Contains(f.Data, "Post-Revoke Secret Task") {
					t.Fatalf("stream leaked post-revocation event: %s", f.Data)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("SSE stream did not terminate after membership revocation")
		}
	})
}

// TestProjectEventStream_HumanOIDCAuthExpiryAndKeepalive wires the real OIDC
// verifier, persistentCredentialValidity, event.Subscriber, event.Reader, and
// projectEventStreamHandler to prove:
//   - Human OIDC tokens (from #38) authenticate and stream project events.
//   - Keepalive comments are emitted while idle.
//   - The stream terminates cleanly at VerifiedExpiry (ExpiresAt) and emits NO
//     events committed at or after ExpiresAt.
//   - Execution principals are refused with 403 forbidden.
func TestProjectEventStream_HumanOIDCAuthExpiryAndKeepalive(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	clock := &fixedValidityClock{now: now}
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}

	oidcFixture := newOIDCTestFixture(t)
	verifier := oidcFixture.newVerifier(t, clock, 5*time.Second)

	tenantAdminID, err := oidc.PrincipalIDFor(oidcFixture.issuer, "auth0|admin-1")
	if err != nil {
		t.Fatalf("PrincipalIDFor admin: %v", err)
	}
	projectHumanID, err := oidc.PrincipalIDFor(oidcFixture.issuer, "auth0|engineer-1")
	if err != nil {
		t.Fatalf("PrincipalIDFor engineer: %v", err)
	}

	serviceID := service.ServiceID("svc-sse-1")
	serviceNonce := make([]byte, token.MinNonceBytes)
	if _, err := rand.Read(serviceNonce); err != nil {
		t.Fatalf("rand nonce: %v", err)
	}
	mem := memory.NewWithClock(clock.Now)
	derivationKey := seedPersistentSubscribeFixture(t, clock, mem, scope, tenantAdminID, projectHumanID, serviceID, serviceNonce, now.Add(time.Hour))

	identityAdapter, err := identitystore.New(identitystore.Config{
		Executor: mem, Scanner: mem, Clock: clock, IDs: bridges.NewSecureIDs(),
	})
	if err != nil {
		t.Fatalf("identitystore.New: %v", err)
	}
	receiptLookup, _ := readers.NewReceiptLookup(mem)
	manager, err := token.NewManager(token.ManagerConfig{
		Keys: identityAdapter, Resolver: staticDerivationKey{key: derivationKey},
		Credentials: identityAdapter, Principals: identityAdapter, Services: identityAdapter,
		Mutations: identityAdapter, Receipts: receiptLookup, Deliverer: noopDeliverer{},
		Invalidations: noopInvalidator{}, Clock: clock, Random: rand.Reader,
	})
	if err != nil {
		t.Fatalf("token.NewManager: %v", err)
	}
	resolver, err := token.NewResolver(token.ResolverConfig{
		Lookup: mem, Credentials: manager, OIDC: verifier, Principals: identityAdapter, Clock: clock,
	})
	if err != nil {
		t.Fatalf("token.NewResolver: %v", err)
	}
	storeValidity, _ := readers.NewCredentialValidity(mem)
	validity, _ := newPersistentCredentialValidity(storeValidity, clock)
	cursorReader, _ := readers.NewCursorReader(mem)
	evReaderPort, _ := platform.NewEventReader(mem)
	subscriber, _ := event.NewSubscriber(event.SubscribeConfig{
		Authorizer: authorize.NewMatrix(), Authority: identityAdapter, Principals: identityAdapter,
		Cursor: cursorReader, Validity: validity, Clock: clock,
	})
	reader, _ := event.NewReader(event.ReadConfig{
		Authorizer: authorize.NewMatrix(), Authority: identityAdapter, Principals: identityAdapter,
		Port: evReaderPort,
	})
	factsPermission, _ := bridges.NewPermissionSource(authorize.ExecOwnTaskContent, clock)
	executionFacts, _ := platform.NewExecutionReader(mem.Controller(), factsPermission)
	ownership, _ := opsapp.NewOwnership(executionFacts, opsapp.RegistryPermissions{})
	hub := newEventWakeupHub()

	sseHandler, err := newProjectEventStreamHandler(projectEventStreamConfig{
		Principals:        resolver,
		Scopes:            ownership,
		Subscriber:        subscriber,
		Reader:            reader,
		Hub:               hub,
		Clock:             clock,
		PollInterval:      15 * time.Millisecond,
		KeepaliveInterval: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newProjectEventStreamHandler: %v", err)
	}
	t.Cleanup(func() { _ = sseHandler.Close(context.Background()) })

	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if projectID, ok := api.ParseProjectEventStreamPath(r.URL.Path); ok {
			sseHandler.ServeProjectStream(w, r, projectID)
			return
		}
		http.NotFound(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	// Human OIDC token valid for 30s on the fake clock.
	exp := now.Add(30 * time.Second)
	jwt := oidcFixture.signToken(t, "auth0|engineer-1", scope, now, exp)
	cursorZero, _ := api.EncodeEventCursor(0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+"/api/v1/projects/project-1/events/stream?since="+cursorZero, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET OIDC stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("OIDC stream status = %d body=%s; want 200", resp.StatusCode, string(bodyBytes))
	}

	// Read in background until EOF (which must occur when clock reaches exp).
	var (
		mu         sync.Mutex
		frames     []sseFrame
		comments   []string
		streamDone = make(chan struct{})
	)
	go func() {
		defer close(streamDone)
		scanner := bufio.NewScanner(resp.Body)
		var cur sseFrame
		var dataLines []string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if len(dataLines) > 0 {
					cur.Data = strings.Join(dataLines, "\n")
					mu.Lock()
					frames = append(frames, cur)
					mu.Unlock()
					cur = sseFrame{}
					dataLines = nil
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				mu.Lock()
				comments = append(comments, line)
				mu.Unlock()
				continue
			}
			field, val, _ := strings.Cut(line, ":")
			val = strings.TrimPrefix(val, " ")
			switch field {
			case "id":
				cur.ID = val
			case "event":
				cur.Event = val
			case "data":
				dataLines = append(dataLines, val)
			}
		}
	}()

	// Wait until initial seeded event + at least one keepalive arrive.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		gotFrames := len(frames)
		gotComments := len(comments)
		mu.Unlock()
		if gotFrames >= 1 && gotComments >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected >=1 frame and >=1 keepalive comment; got frames=%d comments=%d", gotFrames, gotComments)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Advance fake clock past ExpiresAt AND simultaneously commit a new event:
	// the stream MUST terminate cleanly and emit NO event past ExpiresAt!
	clock.mu.Lock()
	clock.now = exp
	clock.mu.Unlock()
	if _, err := mem.Execute(ctx, scope, store.Unit{
		Events: []event.Envelope{{
			ID:        "evt-after-expiry",
			Type:      event.TypeTaskCreated,
			Source:    event.Source{Kind: event.SourceShell, PrincipalID: projectHumanID},
			At:        exp,
			Scope:     scope,
			SubjectID: "task-after-expiry",
		}},
	}); err != nil {
		t.Fatalf("commit post-expiry event: %v", err)
	}
	hub.notify(scope)

	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream did not terminate cleanly when human OIDC credential expired")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 1 {
		t.Fatalf("frames received = %d (%+v); want exactly 1 (no event past ExpiresAt)", len(frames), frames)
	}
	if strings.Contains(frames[0].Data, "evt-after-expiry") {
		t.Fatalf("stream leaked post-expiry event: %s", frames[0].Data)
	}
}

type stubPrincipalResolver struct {
	principal identity.Principal
}

func (s stubPrincipalResolver) Resolve(context.Context, string) (identity.Principal, error) {
	return s.principal, nil
}

type panickingScopeResolver struct{}

func (panickingScopeResolver) Resolve(context.Context, identity.Principal, transportops.Call) (transportops.Resolution, error) {
	panic("scopes.Resolve must not be called for execution principals on event stream")
}

type fakeBrokerWakeupSub struct {
	mu     sync.Mutex
	closed bool
	onDone chan struct{}
}

func (s *fakeBrokerWakeupSub) Close(context.Context) error {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if !already && s.onDone != nil {
		close(s.onDone)
	}
	return nil
}

type fakeBrokerWakeup struct {
	mu     sync.Mutex
	subCnt int
	last   *fakeBrokerWakeupSub
}

func (f *fakeBrokerWakeup) SubscribeEventWakeup(context.Context, tenancy.Scope, func()) (natsbroker.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subCnt++
	sub := &fakeBrokerWakeupSub{onDone: make(chan struct{})}
	f.last = sub
	return sub, nil
}

func TestProjectEventStream_ExecutionPrincipalRejectedBeforeScopeResolveAndSharedWakeup(t *testing.T) {
	execPrincipal := identity.Principal{
		ID:          "exec-principal-1",
		Kind:        identity.KindExecution,
		ExecutionID: "exec-1",
		Scope:       tenancy.Scope{TenantID: "tenant-1", ProjectID: "proj-1"},
		Enabled:     true,
	}
	broker := &fakeBrokerWakeup{}
	handler := &projectEventStreamHandler{
		principals:   stubPrincipalResolver{principal: execPrincipal},
		scopes:       panickingScopeResolver{},
		hub:          newEventWakeupHub(),
		brokerWakeup: broker,
		brokerSubs:   make(map[tenancy.Scope]*sharedBrokerWakeup),
		done:         make(chan struct{}),
	}
	defer func() { _ = handler.Close(context.Background()) }()

	// Own project -> 403 forbidden without calling scopes.Resolve.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-1/events/stream", nil)
	req.Header.Set("Authorization", "Bearer exec-token")
	handler.ServeProjectStream(rec, req, "proj-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("own project status = %d body=%s; want 403", rec.Code, rec.Body.String())
	}

	// Foreign project -> 404 not_found without calling scopes.Resolve.
	recOther := httptest.NewRecorder()
	reqOther := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-2/events/stream", nil)
	reqOther.Header.Set("Authorization", "Bearer exec-token")
	handler.ServeProjectStream(recOther, reqOther, "proj-2")
	if recOther.Code != http.StatusNotFound {
		t.Fatalf("foreign project status = %d body=%s; want 404", recOther.Code, recOther.Body.String())
	}

	// Shared ref-counted NATS wakeup: 2 concurrent streams share 1 subscription,
	// closed only after both release.
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "proj-1"}
	rel1 := handler.acquireBrokerWakeup(context.Background(), scope)
	rel2 := handler.acquireBrokerWakeup(context.Background(), scope)
	broker.mu.Lock()
	subCnt := broker.subCnt
	sub := broker.last
	broker.mu.Unlock()
	if subCnt != 1 {
		t.Fatalf("broker wakeup subscriptions = %d; want 1 shared per scope", subCnt)
	}
	rel1()
	select {
	case <-sub.onDone:
		t.Fatal("shared broker wakeup closed while second subscriber still active")
	case <-time.After(25 * time.Millisecond):
	}
	rel2()
	select {
	case <-sub.onDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shared broker wakeup did not close after last subscriber released")
	}
}
