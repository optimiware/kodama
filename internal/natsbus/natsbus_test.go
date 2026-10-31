package natsbus_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/natsbus"
	"github.com/optimiware/kodama/internal/shell/bus"
)

func TestDeployedProfilesRequireNATS(t *testing.T) {
	dialed := 0
	failing := func(string, ...nats.Option) (*nats.Conn, error) {
		dialed++
		return nil, errors.New("connection refused")
	}
	for _, p := range []natsbus.Profile{natsbus.Test, natsbus.Production} {
		// No broker configured: a startup failure, never the in-process bus.
		b, err := natsbus.Open(t.Context(), natsbus.Options{Profile: p})
		if !errors.Is(err, natsbus.ErrBrokerRequired) || b != nil {
			t.Errorf("%s without NATS = %v, %v; want ErrBrokerRequired", p, b, err)
		}
		// A configured but unreachable broker is a startup failure too.
		b, err = natsbus.Open(t.Context(), natsbus.Options{Profile: p, URL: "nats://127.0.0.1:1", Dial: failing})
		if !errors.Is(err, natsbus.ErrBrokerUnavailable) || b != nil {
			t.Errorf("%s with an unreachable NATS = %v, %v; want ErrBrokerUnavailable", p, b, err)
		}
	}
	if dialed != 2 {
		t.Fatalf("dialed %d times", dialed)
	}
	// Development with an unreachable configured NATS is also an error: only
	// "nothing configured" selects the in-process bus.
	if _, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Development, URL: "nats://127.0.0.1:1", Dial: failing}); !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("development with a dead NATS = %v", err)
	}
	for _, bad := range []natsbus.Profile{"", "prod", "Production", "staging"} {
		if _, err := natsbus.Open(t.Context(), natsbus.Options{Profile: bad}); !errors.Is(err, natsbus.ErrProfileUnknown) {
			t.Errorf("profile %q = %v", bad, err)
		}
	}
}

func TestDevelopmentFallbackIsFlaggedInProcess(t *testing.T) {
	b, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Development})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !b.InProcess() || !b.DevelopmentOnly() || b.Profile() != natsbus.Development {
		t.Fatalf("broker = in-process %v, development-only %v", b.InProcess(), b.DevelopmentOnly())
	}
	var got []string
	if _, err := b.Subscribe(t.Context(), "kodama-test-x", "kodama.v1.>", func(_ context.Context, m bus.Message) error { got = append(got, m.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context(), "kodama.v1.a.b.x.y", bus.Message{ID: "evt_1", TenantID: "t_a", Type: "x.y"}); err != nil || len(got) != 1 {
		t.Fatalf("in-process publish = %v, %v", got, err)
	}
}

func TestParseProfile(t *testing.T) {
	for _, s := range []string{"development", "test", "production"} {
		if p, err := natsbus.ParseProfile(s); err != nil || string(p) != s {
			t.Errorf("ParseProfile(%q) = %q, %v", s, p, err)
		}
	}
	for _, s := range []string{"", "Test", " test", "prod"} {
		if _, err := natsbus.ParseProfile(s); !errors.Is(err, natsbus.ErrProfileUnknown) {
			t.Errorf("ParseProfile(%q) = %v", s, err)
		}
	}
	if natsbus.Development.RequiresBroker() || !natsbus.Test.RequiresBroker() || !natsbus.Production.RequiresBroker() {
		t.Fatal("broker requirement per profile")
	}
}

func TestConsumerName(t *testing.T) {
	got, err := natsbus.ConsumerName("shell", "run-admission")
	if err != nil || got != "kodama_shell_run-admission" {
		t.Fatalf("= %q, %v", got, err)
	}
	if s, p, err := natsbus.ParseConsumerName(got); err != nil || s != "shell" || p != "run-admission" {
		t.Fatalf("parse = %q %q %v", s, p, err)
	}
	for _, bad := range [][2]string{{"", "x"}, {"shell", ""}, {"Shell", "x"}, {"shell", "a.b"}, {"shell", "a*"}, {"shell", "a>"}, {"shell", "a b"}, {"shell", "-x"},
		{"shell", "x--y"}, {"shell", "x-"}, {"shell", "a_b"}, {"sh_ell", "x"}, {"1shell", "x"},
		{strings.Repeat("a", natsbus.MaxServiceLen+1), "x"}, {"shell", strings.Repeat("a", natsbus.MaxPurposeLen+1)}} {
		if n, err := natsbus.ConsumerName(bad[0], bad[1]); err == nil {
			t.Errorf("ConsumerName(%q,%q) = %q", bad[0], bad[1], n)
		}
	}
}

// The name is an injective encoding of the pair: the tuples that a hyphen join
// would conflate give different names, and every name parses back.
func TestConsumerNameIsUnambiguous(t *testing.T) {
	pairs := [][2]string{
		{"shell", "run-admission"}, {"shell-run", "admission"}, {"shell-run-admission", "x"}, {"shell", "run"}, {"shell-run", "x"},
		{"a", "b-c"}, {"a-b", "c"}, {"a", "b"}, {"kodama", "shell"}, {"shell", "kodama"},
	}
	seen := map[string][2]string{}
	for _, p := range pairs {
		n, err := natsbus.ConsumerName(p[0], p[1])
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[n]; dup {
			t.Fatalf("%v and %v both produce %q", prev, p, n)
		}
		seen[n] = p
		if s, pu, err := natsbus.ParseConsumerName(n); err != nil || s != p[0] || pu != p[1] {
			t.Fatalf("%q parses to %q %q %v", n, s, pu, err)
		}
	}
	for _, bad := range []string{"", "kodama_shell", "kodama__x", "kodama_a_b_c", "kodama-shell-x", "other_shell_x", "kodama_shell_run--x"} {
		if _, _, err := natsbus.ParseConsumerName(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// Every name ConsumerName can generate is a name Subscribe accepts, up to the
// maximum lengths; this needs no broker because Subscribe checks the name before
// it uses the stream.
func TestEveryGeneratedConsumerNameIsSubscribable(t *testing.T) {
	longest, err := natsbus.ConsumerName(strings.Repeat("a", natsbus.MaxServiceLen), strings.Repeat("b", natsbus.MaxPurposeLen))
	if err != nil || len(longest) != natsbus.MaxConsumerNameLen || natsbus.MaxConsumerNameLen != 64 {
		t.Fatalf("longest = %q (%d), %v", longest, len(longest), err)
	}
	words := []string{"a", "a1", "shell", "run-admission", "a-b-c", "x9", strings.Repeat("a", natsbus.MaxServiceLen), strings.Repeat("b", natsbus.MaxPurposeLen),
		strings.Repeat("a-", natsbus.MaxPurposeLen/2-1) + "a"}
	for _, s := range words {
		for _, p := range words {
			name, err := natsbus.ConsumerName(s, p)
			if err != nil {
				continue // beyond a component bound
			}
			if err := natsbus.CheckConsumerName(name); err != nil {
				t.Errorf("ConsumerName(%q,%q) = %q is rejected by Subscribe: %v", s, p, name, err)
			}
		}
	}
	// Names Subscribe must keep accepting for existing durable consumers.
	if err := natsbus.CheckConsumerName("kernel-git-dispatch"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"bad.name", "Bad", "a*", "x y", "a>", "-a", "a-", strings.Repeat("a", 65), "a"} {
		if err := natsbus.CheckConsumerName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestJetStreamBus runs against a NATS server with JetStream enabled when
// KODAMA_NATS_URL names one (skipped otherwise): a publication is acknowledged
// by the stream, the event ID deduplicates a repeated publication inside the
// broker's window, a handler error redelivers, a settled delivery is not
// delivered again, and a durable consumer resumes after a stop without a loss
// or a repeat.
func TestJetStreamBus(t *testing.T) {
	url := os.Getenv("KODAMA_NATS_URL")
	if url == "" {
		t.Skip("set KODAMA_NATS_URL to a NATS server with JetStream to run the JetStream adapter test")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	const stream = "KODAMA_NATSBUS_TEST"
	js, err := natsjs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	_ = js.DeleteStream(ctx, stream)
	defer func() { _ = js.DeleteStream(context.Background(), stream) }()

	b, err := natsbus.Open(ctx, natsbus.Options{Profile: natsbus.Test, Conn: nc, JetStream: natsbus.JetStreamConfig{Stream: stream, Subjects: "kodama.v1.natsbustest.>", RetryDelay: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if b.InProcess() {
		t.Fatal("a test profile with NATS composed the in-process bus")
	}
	var mu sync.Mutex
	calls := map[string]int{}
	handler := func(_ context.Context, m bus.Message) error {
		mu.Lock()
		defer mu.Unlock()
		calls[m.ID]++
		if m.ID == "evt_retry" && calls[m.ID] == 1 {
			return errors.New("not yet")
		}
		return nil
	}
	name, err := natsbus.ConsumerName("test", "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	stop, err := b.Subscribe(ctx, name, "kodama.v1.natsbustest.*.git.refs_updated", handler)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Subscribe(ctx, "bad.name", "kodama.v1.natsbustest.>", handler); err == nil {
		t.Fatal("a consumer name with a dot was accepted")
	}
	msg := func(id string) bus.Message {
		return bus.Message{ID: id, TenantID: "t_acme", ProjectID: "p_erp", Type: "git.refs_updated", ResourceType: "git_push", ResourceID: "rcv_1",
			Actor: bus.Actor{Type: "user", ID: "u_1"}, OccurredAt: time.Now().UTC(), Data: []byte(`{"a":1}`)}
	}
	subject := "kodama.v1.natsbustest.erp.git.refs_updated"
	for _, id := range []string{"evt_once", "evt_once", "evt_retry"} {
		if err := b.Publish(ctx, subject, msg(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Publish(ctx, "kodama.v1.natsbustest._.repository.created", msg("evt_other")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls["evt_once"] == 1 && calls["evt_retry"] >= 2 })
	stop()
	// Published while the consumer is stopped; the durable consumer resumes at it
	// and does not repeat the settled ones.
	if err := b.Publish(ctx, subject, msg("evt_while_stopped")); err != nil {
		t.Fatal(err)
	}
	stop2, err := b.Subscribe(ctx, name, "kodama.v1.natsbustest.*.git.refs_updated", handler)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls["evt_while_stopped"] == 1 })
	time.Sleep(500 * time.Millisecond) // a wrongly repeated delivery would show now
	mu.Lock()
	defer mu.Unlock()
	if calls["evt_once"] != 1 || calls["evt_retry"] != 2 || calls["evt_other"] != 0 || calls["evt_while_stopped"] != 1 {
		t.Fatalf("deliveries = %v", calls)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestOpenAgainstUnreachableURLFailsAtStartup(t *testing.T) {
	// A real dial of a closed port: the deployed profile stops, it does not fall
	// back to memory.
	_, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Production, URL: "nats://127.0.0.1:1"})
	if !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamSubjectsMustLieUnderTheRoot(t *testing.T) {
	// The configuration is checked before the connection is used.
	_, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Test, Conn: &nats.Conn{}, JetStream: natsbus.JetStreamConfig{Subjects: "other.>"}})
	if !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("a stream outside kodama.v1 was accepted: %v", err)
	}
}

// noJetStreamServer is a reachable NATS server that does not run JetStream: it
// answers every request with "no responders".
func noJetStreamServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = fmt.Fprint(c, "INFO {\"server_id\":\"x\",\"version\":\"2.10.0\",\"proto\":1,\"headers\":true,\"max_payload\":1048576}\r\n")
				r := bufio.NewReader(c)
				var sid string
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					f := strings.Fields(line)
					switch {
					case len(f) >= 3 && f[0] == "SUB":
						sid = f[len(f)-1]
					case len(f) >= 1 && f[0] == "PING":
						_, _ = fmt.Fprint(c, "PONG\r\n")
					case len(f) >= 4 && (f[0] == "PUB" || f[0] == "HPUB"):
						var n int
						_, _ = fmt.Sscan(f[len(f)-1], &n)
						_, _ = r.Discard(n + 2)
						if (f[0] == "PUB" && len(f) == 4) || (f[0] == "HPUB" && len(f) == 5) {
							_, _ = fmt.Fprintf(c, "HMSG %s %s 16 16\r\nNATS/1.0 503\r\n\r\n\r\n", f[2], sid)
						}
					}
				}
			}()
		}
	}()
	return "nats://" + ln.Addr().String()
}

func TestOpenAgainstAServerWithoutJetStreamFailsAtStartup(t *testing.T) {
	url := noJetStreamServer(t)
	for _, p := range []natsbus.Profile{natsbus.Development, natsbus.Test, natsbus.Production} {
		b, err := natsbus.Open(t.Context(), natsbus.Options{Profile: p, URL: url})
		if !errors.Is(err, natsbus.ErrBrokerUnavailable) || b != nil {
			t.Errorf("%s = %v, %v; want ErrBrokerUnavailable", p, b, err)
		}
	}
}

type devFlagged struct{ bus.Bus }

func (devFlagged) DevelopmentOnly() bool { return true }

func TestVerifyDeployedRefusesWhatIsNotAVerifiedAdapter(t *testing.T) {
	dev, err := natsbus.Open(t.Context(), natsbus.Options{Profile: natsbus.Development})
	if err != nil {
		t.Fatal(err)
	}
	var typedNilJS *natsbus.JetStream
	var typedNilBroker *natsbus.Broker
	for name, b := range map[string]bus.Bus{
		"nil":              nil,
		"memory":           bus.NewMemory(),
		"development":      dev,
		"flagged":          devFlagged{bus.NewMemory()},
		"typed nil js":     typedNilJS,
		"typed nil broker": typedNilBroker,
		"zero js":          &natsbus.JetStream{},
		"zero broker":      &natsbus.Broker{},
		"broker over zero": &natsbus.Broker{Bus: &natsbus.JetStream{}},
		"broker over mem":  &natsbus.Broker{Bus: bus.NewMemory()},
	} {
		err := natsbus.VerifyDeployed(t.Context(), b)
		if err == nil || (!errors.Is(err, natsbus.ErrBrokerRequired) && !errors.Is(err, natsbus.ErrBrokerUnavailable)) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

// With a real broker, an adapter built by Open or NewJetStream verifies, and it
// stops verifying when its stream or its connection is gone.
func TestVerifyDeployedWithABroker(t *testing.T) {
	url := os.Getenv("KODAMA_NATS_URL")
	if url == "" {
		t.Skip("set KODAMA_NATS_URL to a NATS server with JetStream to run the verification test")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const stream = "KODAMA_NATSBUS_VERIFY"
	raw, _ := natsjs.New(nc)
	_ = raw.DeleteStream(ctx, stream)
	defer func() { _ = raw.DeleteStream(context.Background(), stream) }()
	cfg := natsbus.JetStreamConfig{Stream: stream, Subjects: "kodama.v1.verifytest.>"}
	b, err := natsbus.Open(ctx, natsbus.Options{Profile: natsbus.Production, Conn: nc, JetStream: cfg})
	if err != nil {
		t.Fatal(err)
	}
	js, err := natsbus.NewJetStream(ctx, nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]bus.Bus{"broker": b, "adapter": js, "adapter inside broker": &natsbus.Broker{Bus: js}} {
		if err := natsbus.VerifyDeployed(ctx, v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := raw.DeleteStream(ctx, stream); err != nil {
		t.Fatal(err)
	}
	if err := natsbus.VerifyDeployed(ctx, js); !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("an adapter whose stream is gone verified: %v", err)
	}
	nc.Close()
	if err := natsbus.VerifyDeployed(ctx, b); !errors.Is(err, natsbus.ErrBrokerUnavailable) {
		t.Fatalf("an adapter whose connection is closed verified: %v", err)
	}
}
