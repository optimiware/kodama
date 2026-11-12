package natsdriver

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

type fakeDriverSubscription struct {
	mu           sync.Mutex
	drains       int
	unsubscribes int
	drainErr     error
}

func (sub *fakeDriverSubscription) Drain() error {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	sub.drains++
	return sub.drainErr
}

func (sub *fakeDriverSubscription) Unsubscribe() error {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	sub.unsubscribes++
	return nil
}

func (sub *fakeDriverSubscription) counts() (drains, unsubscribes int) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	return sub.drains, sub.unsubscribes
}

type published struct {
	subject string
	reply   string
	data    []byte
}

type fakeCore struct {
	mu           sync.Mutex
	published    []published
	handler      func(string, []byte)
	msgHandler   func(string, string, []byte)
	sub          *fakeDriverSubscription
	publishErr   error
	subscribeErr error
	drains       int
	closes       int
	drainErr     error
}

func (core *fakeCore) Publish(subject string, data []byte) error {
	core.mu.Lock()
	defer core.mu.Unlock()
	if core.publishErr != nil {
		return core.publishErr
	}
	core.published = append(core.published, published{subject: subject, data: append([]byte(nil), data...)})
	return nil
}

func (core *fakeCore) PublishMsg(subject, reply string, data []byte) error {
	core.mu.Lock()
	defer core.mu.Unlock()
	if core.publishErr != nil {
		return core.publishErr
	}
	core.published = append(core.published, published{subject: subject, reply: reply, data: append([]byte(nil), data...)})
	return nil
}

func (core *fakeCore) Subscribe(subject string, handler func(string, []byte)) (driverSubscription, error) {
	core.mu.Lock()
	defer core.mu.Unlock()
	if core.subscribeErr != nil {
		return nil, core.subscribeErr
	}
	core.handler = handler
	if core.sub == nil {
		core.sub = &fakeDriverSubscription{}
	}
	return core.sub, nil
}

func (core *fakeCore) SubscribeMsg(subject string, handler func(subject, reply string, data []byte)) (driverSubscription, error) {
	core.mu.Lock()
	defer core.mu.Unlock()
	if core.subscribeErr != nil {
		return nil, core.subscribeErr
	}
	core.msgHandler = handler
	if core.sub == nil {
		core.sub = &fakeDriverSubscription{}
	}
	return core.sub, nil
}

func (core *fakeCore) Drain() error {
	core.mu.Lock()
	defer core.mu.Unlock()
	core.drains++
	return core.drainErr
}

func (core *fakeCore) Close() {
	core.mu.Lock()
	defer core.mu.Unlock()
	core.closes++
}

func (core *fakeCore) deliverMsg(subject, reply string, payload []byte) {
	core.mu.Lock()
	handler := core.msgHandler
	core.mu.Unlock()
	if handler != nil {
		handler(subject, reply, payload)
	}
}

func (core *fakeCore) deliver(subject string, payload []byte) {
	core.mu.Lock()
	handler := core.handler
	core.mu.Unlock()
	if handler != nil {
		handler(subject, payload)
	}
}

type fakeJetStream struct {
	mu       sync.Mutex
	subject  string
	payload  []byte
	msgID    string
	ctxValue any
	err      error
}

func (stream *fakeJetStream) Publish(ctx context.Context, subject string, payload []byte, msgID string) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.err != nil {
		return stream.err
	}
	stream.subject = subject
	stream.payload = append([]byte(nil), payload...)
	stream.msgID = msgID
	stream.ctxValue = ctx.Value(testContextKey{})
	return nil
}

type testContextKey struct{}

func testSubject(t *testing.T) broker.Subject {
	t.Helper()
	subject, err := broker.EventSubject(tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-a")})
	if err != nil {
		t.Fatalf("EventSubject: %v", err)
	}
	return subject
}

func newTestClient(t *testing.T) (*Client, *fakeCore, *fakeJetStream) {
	t.Helper()
	core := &fakeCore{}
	stream := &fakeJetStream{}
	client := newClient(core, stream, Config{})
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client, core, stream
}

func TestPublishJetStreamUsesDedupeIdentity(t *testing.T) {
	client, _, stream := newTestClient(t)
	ctx := context.WithValue(context.Background(), testContextKey{}, "propagated")
	subject := testSubject(t)
	if err := client.PublishJetStream(ctx, subject, id.EventID("event-1"), []byte("payload")); err != nil {
		t.Fatalf("PublishJetStream: %v", err)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.msgID != "event-1" {
		t.Fatalf("dedupe identity = %q; want event-1", stream.msgID)
	}
	if stream.subject != string(subject) || string(stream.payload) != "payload" {
		t.Fatalf("publication = %q/%q", stream.subject, stream.payload)
	}
	if stream.ctxValue != "propagated" {
		t.Fatalf("context was not propagated: %#v", stream.ctxValue)
	}
}

func TestPublishCoreCarriesNoIdentity(t *testing.T) {
	client, core, stream := newTestClient(t)
	subject := testSubject(t)
	if err := client.PublishCore(context.Background(), subject, []byte("chunk")); err != nil {
		t.Fatalf("PublishCore: %v", err)
	}
	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.published) != 1 {
		t.Fatalf("core publications = %d; want 1", len(core.published))
	}
	if core.published[0].subject != string(subject) || string(core.published[0].data) != "chunk" {
		t.Fatalf("core publication = %q/%q", core.published[0].subject, core.published[0].data)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.msgID != "" {
		t.Fatalf("core publication used JetStream identity %q", stream.msgID)
	}
}

func TestPublishRefusesInvalidInput(t *testing.T) {
	client, _, _ := newTestClient(t)
	if err := client.PublishJetStream(context.Background(), "not-a-subject", "event-1", nil); err == nil {
		t.Fatal("PublishJetStream accepted a malformed subject")
	}
	if err := client.PublishJetStream(context.Background(), testSubject(t), "", nil); err == nil {
		t.Fatal("PublishJetStream accepted an empty identity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.PublishCore(ctx, testSubject(t), nil); err == nil {
		t.Fatal("PublishCore accepted a canceled context")
	} else if class, _ := fault.ClassOf(err); class != fault.DeadlineExpired {
		t.Fatalf("fault class = %q; want deadline_expired", class)
	}
}

func TestSubscribeDeliversAndReportsHandlerErrors(t *testing.T) {
	core := &fakeCore{}
	stream := &fakeJetStream{}
	var observedSubject string
	var observedErr error
	client := newClient(core, stream, Config{HandlerErrors: func(subject string, err error) {
		observedSubject = subject
		observedErr = err
	}})
	subject := testSubject(t)
	var received []byte
	var handlerCtx context.Context
	sub, err := client.Subscribe(context.Background(), subject, func(ctx context.Context, payload []byte) error {
		handlerCtx = ctx
		received = append([]byte(nil), payload...)
		return errors.New("handler failed")
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	core.deliver(string(subject), []byte("payload"))
	if string(received) != "payload" {
		t.Fatalf("delivered payload = %q; want payload", received)
	}
	if observedErr == nil || observedSubject != string(subject) {
		t.Fatalf("handler error was not observed: subject=%q err=%v", observedSubject, observedErr)
	}
	if err := sub.Close(context.Background()); err != nil {
		t.Fatalf("subscription Close: %v", err)
	}
	if handlerCtx.Err() == nil {
		t.Fatal("subscription Close did not cancel the handler context")
	}
	if err := sub.Close(context.Background()); err != nil {
		t.Fatalf("second subscription Close: %v", err)
	}
	drains, _ := core.sub.counts()
	if drains != 1 {
		t.Fatalf("subscription drains = %d; want 1", drains)
	}
}

func TestSubscribeRefusesInvalidInput(t *testing.T) {
	client, _, _ := newTestClient(t)
	if _, err := client.Subscribe(context.Background(), "bad", func(context.Context, []byte) error { return nil }); err == nil {
		t.Fatal("Subscribe accepted a malformed subject")
	}
	if _, err := client.Subscribe(context.Background(), testSubject(t), nil); err == nil {
		t.Fatal("Subscribe accepted a nil handler")
	}
	core := &fakeCore{subscribeErr: errors.New("subscription failed")}
	failing := newClient(core, &fakeJetStream{}, Config{})
	if _, err := failing.Subscribe(context.Background(), testSubject(t), func(context.Context, []byte) error { return nil }); err == nil {
		t.Fatal("Subscribe ignored the driver error")
	}
}

func TestCloseDrainsAndRefusesFurtherUse(t *testing.T) {
	core := &fakeCore{}
	stream := &fakeJetStream{}
	client := newClient(core, stream, Config{})
	if _, err := client.Subscribe(context.Background(), testSubject(t), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	core.mu.Lock()
	drains, closes := core.drains, core.closes
	core.mu.Unlock()
	if drains != 1 || closes != 1 {
		t.Fatalf("connection drain/close = %d/%d; want 1/1", drains, closes)
	}
	if err := client.PublishCore(context.Background(), testSubject(t), nil); err == nil {
		t.Fatal("closed client published")
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSubscribeInvalidationsDecodesAndChecksScope(t *testing.T) {
	core := &fakeCore{}
	client := newClient(core, &fakeJetStream{}, Config{})
	scope := tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-a")}
	invalidation := token.Invalidation{
		Scope:       scope,
		PrincipalID: id.PrincipalID("human-1"),
		ExecutionID: id.ExecutionID("execution-1"),
		Reason:      token.InvalidationRevoked,
		At:          time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(invalidation)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var received token.Invalidation
	sub, err := client.SubscribeInvalidations(context.Background(), scope, func(_ context.Context, invalidation token.Invalidation) error {
		received = invalidation
		return nil
	})
	if err != nil {
		t.Fatalf("SubscribeInvalidations: %v", err)
	}
	subject, err := broker.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	core.deliver(string(subject), payload)
	if received != invalidation {
		t.Fatalf("received invalidation = %+v; want %+v", received, invalidation)
	}

	otherScope := tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-b")}
	otherPayload, err := json.Marshal(token.Invalidation{
		Scope:       otherScope,
		PrincipalID: id.PrincipalID("human-2"),
		ExecutionID: id.ExecutionID("execution-2"),
		Reason:      token.InvalidationRevoked,
		At:          invalidation.At,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var observed error
	client.handlerError = func(_ string, err error) { observed = err }
	core.deliver(string(subject), otherPayload)
	if observed == nil {
		t.Fatal("a foreign-scope invalidation was not refused")
	}
	core.deliver(string(subject), []byte("not-json"))
	if observed == nil {
		t.Fatal("an invalid payload was not refused")
	}
	if err := sub.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSubscribeInvalidationsValidatesInput(t *testing.T) {
	client, _, _ := newTestClient(t)
	if _, err := client.SubscribeInvalidations(context.Background(), tenancy.Scope{}, func(context.Context, token.Invalidation) error {
		return nil
	}); err == nil {
		t.Fatal("SubscribeInvalidations accepted an invalid scope")
	}
	if _, err := client.SubscribeInvalidations(context.Background(), tenancy.Scope{TenantID: id.TenantID("tenant-a")}, nil); err == nil {
		t.Fatal("SubscribeInvalidations accepted a nil handler")
	}
}

func TestConfigValidationAndSecretFreeErrors(t *testing.T) {
	cases := []Config{
		{},
		{URL: "   "},
		{URL: "nats://localhost:4222", Token: "token", User: "user"},
		{URL: "nats://localhost:4222", Password: "password"},
		{URL: "nats://localhost:4222", ConnectTimeout: -1},
		{URL: "nats://localhost:4222", ReconnectWait: -1},
		{URL: "nats://localhost:4222", DrainTimeout: -1},
		{URL: "nats://localhost:4222", MaxReconnects: -2},
	}
	for index, config := range cases {
		if _, err := Dial(context.Background(), config); err == nil {
			t.Fatalf("case %d: Dial accepted an invalid configuration", index)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dial(ctx, Config{URL: "nats://localhost:4222"}); err == nil {
		t.Fatal("Dial accepted a canceled context")
	}

	_, err := Dial(context.Background(), Config{URL: "nats://user:supersecret@[::1"})
	if err == nil {
		t.Fatal("Dial accepted a malformed URL")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("connection error leaked the password: %v", err)
	}

	core := &fakeCore{publishErr: errors.New("publish failed with token=supersecret")}
	client := newClient(core, &fakeJetStream{}, Config{})
	if err := client.PublishCore(context.Background(), testSubject(t), nil); err == nil {
		t.Fatal("PublishCore succeeded with an injected failure")
	} else if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("publication error leaked the token: %v", err)
	}

	stream := &fakeJetStream{err: errors.New("jetstream failed with token=supersecret")}
	client = newClient(&fakeCore{}, stream, Config{})
	if err := client.PublishJetStream(context.Background(), testSubject(t), id.EventID("event-1"), nil); err == nil {
		t.Fatal("PublishJetStream succeeded with an injected failure")
	} else if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("publication error leaked the token: %v", err)
	}
}

// fakeNATSServer is a minimal in-process NATS protocol peer. It exists so the
// driver's dial, trust and reconnect behavior is proven against the real
// nats.go client without a real broker: nats.go performs the same INFO, TLS
// upgrade, CONNECT and PING/PONG exchange as it does against nats-server. A nil
// tlsConfig serves plaintext.
//
// The server records the CONNECT payload and the subject of every publication,
// and signals both on buffered channels, so tests synchronize on protocol
// events instead of sleeping.
type fakeNATSServer struct {
	listener  net.Listener
	tlsConfig *tls.Config
	// tlsAvailable advertises TLS support without requiring it. The peer still
	// speaks plaintext, so only a client that upgrades on its own configuration
	// reaches a handshake (and it fails against this peer).
	tlsAvailable bool

	connectCh chan string
	publishCh chan string

	mu        sync.Mutex
	conns     []net.Conn
	connects  []string
	published []string

	wg        sync.WaitGroup
	closed    bool
	closeOnce sync.Once
}

// startFakeNATSServer listens on one loopback port and serves until cleanup.
func startFakeNATSServer(t *testing.T, tlsConfig *tls.Config) *fakeNATSServer {
	return startFakeNATSServerOptions(t, tlsConfig, false)
}

// startFakeNATSServerAdvertisingTLS serves plaintext while telling the client
// that TLS is available but not required (tls_available, no tls_required).
func startFakeNATSServerAdvertisingTLS(t *testing.T) *fakeNATSServer {
	return startFakeNATSServerOptions(t, nil, true)
}

func startFakeNATSServerOptions(t *testing.T, tlsConfig *tls.Config, tlsAvailable bool) *fakeNATSServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake nats: %v", err)
	}
	server := &fakeNATSServer{
		listener:     listener,
		tlsConfig:    tlsConfig,
		tlsAvailable: tlsAvailable,
		connectCh:    make(chan string, 16),
		publishCh:    make(chan string, 16),
	}
	server.wg.Add(1)
	go server.serve()
	t.Cleanup(server.Close)
	return server
}

// URL is the plaintext nats:// URL new clients pass to Dial. TLS is negotiated
// from the INFO the server advertises, exactly like a tls_required server.
func (server *fakeNATSServer) URL() string {
	return "nats://" + server.listener.Addr().String()
}

func (server *fakeNATSServer) serve() {
	defer server.wg.Done()
	for {
		raw, err := server.listener.Accept()
		if err != nil {
			return
		}
		server.mu.Lock()
		if server.closed {
			server.mu.Unlock()
			_ = raw.Close()
			return
		}
		server.conns = append(server.conns, raw)
		server.wg.Add(1)
		server.mu.Unlock()
		go server.serveConn(raw)
	}
}

func (server *fakeNATSServer) serveConn(raw net.Conn) {
	defer server.wg.Done()
	defer raw.Close()
	// The deadline only bounds a stuck test peer; it is never used as a
	// synchronization mechanism.
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))

	info := `INFO {"server_id":"fake-nats","version":"2.12.0","proto":1,"max_payload":1048576}`
	switch {
	case server.tlsConfig != nil:
		info = `INFO {"server_id":"fake-nats","version":"2.12.0","proto":1,"max_payload":1048576,"tls_required":true}`
	case server.tlsAvailable:
		info = `INFO {"server_id":"fake-nats","version":"2.12.0","proto":1,"max_payload":1048576,"tls_available":true}`
	}
	if _, err := io.WriteString(raw, info+"\r\n"); err != nil {
		return
	}
	conn := raw
	if server.tlsConfig != nil {
		tlsConn := tls.Server(raw, server.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
	}

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case strings.HasPrefix(line, "CONNECT "):
			server.recordConnect(strings.TrimPrefix(line, "CONNECT "))
		case line == "PING":
			if _, err := io.WriteString(conn, "PONG\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(line, "PUB "):
			fields := strings.Fields(line)
			if len(fields) < 3 {
				return
			}
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil || size < 0 {
				return
			}
			body := make([]byte, size+2)
			if _, err := io.ReadFull(reader, body); err != nil {
				return
			}
			server.recordPublish(fields[1])
		case strings.HasPrefix(line, "SUB "), strings.HasPrefix(line, "UNSUB "), line == "PONG":
			// No server-side routing is needed by these scenarios.
		}
	}
}

func (server *fakeNATSServer) recordConnect(payload string) {
	server.mu.Lock()
	server.connects = append(server.connects, payload)
	server.mu.Unlock()
	select {
	case server.connectCh <- payload:
	default:
	}
}

func (server *fakeNATSServer) recordPublish(subject string) {
	server.mu.Lock()
	server.published = append(server.published, subject)
	server.mu.Unlock()
	select {
	case server.publishCh <- subject:
	default:
	}
}

// DropConnections closes every live connection, simulating a server restart
// while the listener keeps accepting the client's reconnect attempts.
func (server *fakeNATSServer) DropConnections() {
	server.mu.Lock()
	conns := append([]net.Conn(nil), server.conns...)
	server.conns = nil
	server.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// Connects returns the recorded CONNECT payloads in order.
func (server *fakeNATSServer) Connects() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.connects...)
}

// Published returns the recorded publication subjects in order.
func (server *fakeNATSServer) Published() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.published...)
}

// Close stops accepting, closes live connections and joins the server
// goroutines.
func (server *fakeNATSServer) Close() {
	server.closeOnce.Do(func() {
		_ = server.listener.Close()
		server.mu.Lock()
		server.closed = true
		conns := append([]net.Conn(nil), server.conns...)
		server.conns = nil
		server.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	done := make(chan struct{})
	go func() {
		server.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// waitFakeEvent waits for one protocol event on ch under a bounded timeout.
func waitFakeEvent[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// flushCore flushes the buffered nats.go connection behind a driver client.
// PublishCore buffers like nats.go Publish, so a test that observes the wire
// must flush. The driver exposes no Flush operation of its own.
func flushCore(t *testing.T, client *Client) {
	t.Helper()
	core, ok := client.core.(*natsCore)
	if !ok {
		t.Fatalf("driver core is %T; want the nats.go adapter", client.core)
	}
	if err := core.conn.Flush(); err != nil {
		t.Fatalf("flush nats connection: %v", err)
	}
}

// requireFaultClass asserts the seam's fault class for a non-nil error.
func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

// TestNATSDialKeepsPlaintextComposition proves a composition that sets no TLS
// configuration dials a plaintext broker, keeps its connection name and
// publishes. Private-CA TLS support is additive and never forces encryption
// onto a plaintext test composition.
func TestNATSDialKeepsPlaintextComposition(t *testing.T) {
	server := startFakeNATSServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := Dial(ctx, Config{
		URL:            server.URL(),
		Name:           "plaintext-composition",
		ConnectTimeout: 2 * time.Second,
		DrainTimeout:   time.Second,
	})
	if err != nil {
		t.Fatalf("Dial plaintext: %v", err)
	}
	subject := testSubject(t)
	if err := client.PublishCore(ctx, subject, []byte("plaintext")); err != nil {
		t.Fatalf("PublishCore plaintext: %v", err)
	}
	flushCore(t, client)
	waitFakeEvent(t, server.publishCh, "plaintext publication")
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close plaintext: %v", err)
	}
	published := server.Published()
	if len(published) != 1 || published[0] != string(subject) {
		t.Fatalf("plaintext publications = %v; want [%s]", published, subject)
	}
	connects := server.Connects()
	if len(connects) != 1 {
		t.Fatalf("plaintext CONNECT count = %d; want 1", len(connects))
	}
	if !strings.Contains(connects[0], `"name":"plaintext-composition"`) {
		t.Fatalf("plaintext CONNECT = %s; want the configured connection name", connects[0])
	}
}

// TestSubscribeCalloutAndReply pins the raw authorization-callout path: the
// handler receives the broker reply inbox and the service publishes the
// response to that inbox, not back to the protected system subject.
func TestSubscribeCalloutAndReply(t *testing.T) {
	client, core, _ := newTestClient(t)
	requests := make(chan broker.CalloutRequest, 1)
	subscription, err := client.SubscribeCallout(context.Background(), broker.CalloutSubject, func(_ context.Context, request broker.CalloutRequest) error {
		requests <- request
		return nil
	})
	if err != nil {
		t.Fatalf("SubscribeCallout: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Close(context.Background()) })
	core.deliverMsg(broker.CalloutSubject, "_INBOX.reply", []byte("request"))
	request := <-requests
	if request.Subject != broker.CalloutSubject || request.Reply != "_INBOX.reply" || string(request.Data) != "request" {
		t.Fatalf("callout request = %+v", request)
	}
	if err := client.PublishCallout(context.Background(), request.Reply, []byte("response")); err != nil {
		t.Fatalf("PublishCallout: %v", err)
	}
	core.mu.Lock()
	published := core.published[len(core.published)-1]
	core.mu.Unlock()
	if published.subject != "_INBOX.reply" || string(published.data) != "response" {
		t.Fatalf("callout reply = %+v", published)
	}
}

type blockingDrainSubscription struct {
	mu           sync.Mutex
	drains       int
	unsubscribes int
	unsubCh      chan struct{}
	once         sync.Once
}

func newBlockingDrainSubscription() *blockingDrainSubscription {
	return &blockingDrainSubscription{unsubCh: make(chan struct{})}
}

func (sub *blockingDrainSubscription) Drain() error {
	sub.mu.Lock()
	sub.drains++
	sub.mu.Unlock()
	<-sub.unsubCh
	return nil
}

func (sub *blockingDrainSubscription) Unsubscribe() error {
	sub.mu.Lock()
	sub.unsubscribes++
	sub.mu.Unlock()
	sub.once.Do(func() { close(sub.unsubCh) })
	return nil
}

func (sub *blockingDrainSubscription) counts() (drains, unsubscribes int) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	return sub.drains, sub.unsubscribes
}

type blockingDrainCore struct {
	mu         sync.Mutex
	subs       []driverSubscription
	stallDrain bool
	drains     int
	closes     int
	closeCh    chan struct{}
	closeOnce  sync.Once
}

func newBlockingDrainCore(stallDrain bool, subs ...driverSubscription) *blockingDrainCore {
	return &blockingDrainCore{
		subs:       subs,
		stallDrain: stallDrain,
		closeCh:    make(chan struct{}),
	}
}

func (core *blockingDrainCore) Publish(string, []byte) error            { return nil }
func (core *blockingDrainCore) PublishMsg(string, string, []byte) error { return nil }

func (core *blockingDrainCore) Subscribe(_ string, _ func(string, []byte)) (driverSubscription, error) {
	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.subs) == 0 {
		return &fakeDriverSubscription{}, nil
	}
	sub := core.subs[0]
	core.subs = core.subs[1:]
	return sub, nil
}

func (core *blockingDrainCore) SubscribeMsg(_ string, _ func(string, string, []byte)) (driverSubscription, error) {
	return core.Subscribe("", nil)
}

func (core *blockingDrainCore) Drain() error {
	core.mu.Lock()
	core.drains++
	stall := core.stallDrain
	core.mu.Unlock()
	if stall {
		<-core.closeCh
	}
	return nil
}

func (core *blockingDrainCore) Close() {
	core.mu.Lock()
	core.closes++
	core.mu.Unlock()
	core.closeOnce.Do(func() { close(core.closeCh) })
}

func (core *blockingDrainCore) counts() (drains, closes int) {
	core.mu.Lock()
	defer core.mu.Unlock()
	return core.drains, core.closes
}

func TestCloseBoundsStalledDrainWithDrainTimeout(t *testing.T) {
	t.Run("stalled connection drain with background context", func(t *testing.T) {
		core := newBlockingDrainCore(true)
		client := newClient(core, &fakeJetStream{}, Config{DrainTimeout: 25 * time.Millisecond})

		started := time.Now()
		err := client.Close(context.Background())
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("Close took %s; want bounded by DrainTimeout", elapsed)
		}
		requireFaultClass(t, err, fault.DeadlineExpired)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close error cause = %v; want context.DeadlineExceeded", err)
		}
		if strings.Contains(err.Error(), "was canceled was canceled") {
			t.Fatalf("Close error duplicated cancellation suffix: %v", err)
		}
		drains, closes := core.counts()
		if drains != 1 || closes != 1 {
			t.Fatalf("core drains/closes = %d/%d; want 1/1", drains, closes)
		}
	})

	t.Run("stalled subscription drain unsubscribes and still drains remaining subscriptions", func(t *testing.T) {
		stalled := newBlockingDrainSubscription()
		healthy := &fakeDriverSubscription{}
		core := newBlockingDrainCore(false, stalled, healthy)
		client := newClient(core, &fakeJetStream{}, Config{DrainTimeout: 25 * time.Millisecond})

		if _, err := client.Subscribe(context.Background(), testSubject(t), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatalf("Subscribe stalled: %v", err)
		}
		if _, err := client.Subscribe(context.Background(), testSubject(t), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatalf("Subscribe healthy: %v", err)
		}

		started := time.Now()
		err := client.Close(context.Background())
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("Close took %s; want bounded by DrainTimeout", elapsed)
		}
		requireFaultClass(t, err, fault.DeadlineExpired)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close error cause = %v; want context.DeadlineExceeded", err)
		}
		stalledDrains, stalledUnsubs := stalled.counts()
		if stalledDrains != 1 || stalledUnsubs != 1 {
			t.Fatalf("stalled sub drains/unsubs = %d/%d; want 1/1", stalledDrains, stalledUnsubs)
		}
		healthyDrains, healthyUnsubs := healthy.counts()
		if healthyDrains != 1 || healthyUnsubs != 0 {
			t.Fatalf("healthy sub drains/unsubs = %d/%d; want 1/0", healthyDrains, healthyUnsubs)
		}
		drains, closes := core.counts()
		if drains != 1 || closes != 1 {
			t.Fatalf("core drains/closes = %d/%d; want 1/1", drains, closes)
		}
	})

	t.Run("canceled context avoids duplicated was canceled suffix", func(t *testing.T) {
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()

		_, dialErr := Dial(canceledCtx, Config{URL: "nats://127.0.0.1:4222"})
		requireFaultClass(t, dialErr, fault.DeadlineExpired)
		if strings.Contains(dialErr.Error(), "was canceled was canceled") {
			t.Fatalf("Dial error duplicated suffix: %v", dialErr)
		}

		stalled := newBlockingDrainSubscription()
		core := newBlockingDrainCore(true, stalled)
		client := newClient(core, &fakeJetStream{}, Config{DrainTimeout: time.Second})

		pubErr := client.PublishCore(canceledCtx, testSubject(t), nil)
		requireFaultClass(t, pubErr, fault.DeadlineExpired)
		if strings.Contains(pubErr.Error(), "was canceled was canceled") {
			t.Fatalf("PublishCore error duplicated suffix: %v", pubErr)
		}

		sub, err := client.Subscribe(context.Background(), testSubject(t), func(context.Context, []byte) error { return nil })
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		subErr := sub.Close(canceledCtx)
		requireFaultClass(t, subErr, fault.DeadlineExpired)
		if strings.Contains(subErr.Error(), "was canceled was canceled") {
			t.Fatalf("subscription Close error duplicated suffix: %v", subErr)
		}

		closeErr := client.Close(canceledCtx)
		requireFaultClass(t, closeErr, fault.DeadlineExpired)
		if strings.Contains(closeErr.Error(), "was canceled was canceled") {
			t.Fatalf("Client.Close error duplicated suffix: %v", closeErr)
		}
	})

	t.Run("Dial passes DrainTimeout to nats.go options", func(t *testing.T) {
		server := startFakeNATSServer(t, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client, err := Dial(ctx, Config{
			URL:            server.URL(),
			ConnectTimeout: 2 * time.Second,
			DrainTimeout:   321 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = client.Close(context.Background()) }()
		core, ok := client.core.(*natsCore)
		if !ok {
			t.Fatalf("client.core = %T; want *natsCore", client.core)
		}
		if core.conn.Opts.DrainTimeout != 321*time.Millisecond {
			t.Fatalf("nats.go DrainTimeout = %s; want 321ms", core.conn.Opts.DrainTimeout)
		}
	})
}
