package webhook_test

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	"github.com/optimiware/kodama/internal/integrations/chatops/webhook"
)

// TestWebhookBoundsAndClassification: slow responses, oversized headers and
// bodies, receipt bounds, retry classification and uncertain acceptance. Local
// HTTP/TLS fixtures only.
func TestWebhookBoundsAndClassification(t *testing.T) {
	t.Run("delivered statuses record the status and an optional bounded receipt", func(t *testing.T) {
		long := strings.Repeat("r", chatops.MaxReceiptBytes+1)
		for name, tc := range map[string]struct {
			behavior chatopstest.Behavior
			status   int
			receipt  string
		}{
			"200 with a receipt":    {chatopstest.Accept("rcpt-77"), 200, "rcpt-77"},
			"200 without a receipt": {chatopstest.Accept(""), 200, ""},
			"204":                   {chatopstest.Status(204), 204, ""},
			"oversized receipt":     {chatopstest.Accept(long), 200, ""},
			"receipt with a space":  {chatopstest.Accept("two words"), 200, ""},
			"receipt with a control": {func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Receipt-Id", "a\tb")
				w.WriteHeader(200)
			}, 200, ""},
			"two receipt headers": {func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Add("X-Receipt-Id", "a")
				w.Header().Add("X-Receipt-Id", "b")
				w.WriteHeader(200)
			}, 200, ""},
			"another header is no receipt": {func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Other", "x")
				w.WriteHeader(200)
			}, 200, ""},
		} {
			rc := chatopstest.NewReceiver(t, tc.behavior)
			out := send(t, client(t, webhook.Config{}, rc), request(scopeA, rc.URL(), ""), 3*time.Second)
			if !out.Delivered || out.HTTPStatus != tc.status || out.Receipt != tc.receipt || out.Class != "" || out.Uncertain {
				t.Errorf("%s: %+v", name, out)
			}
		}
		// The receipt header name is configurable.
		rc := chatopstest.NewReceiver(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Msg-Ts", "1700.5")
			w.WriteHeader(200)
		})
		if out := send(t, client(t, webhook.Config{ReceiptHeader: "X-Msg-Ts"}, rc), request(scopeA, rc.URL(), ""), 3*time.Second); out.Receipt != "1700.5" {
			t.Fatalf("configured receipt header: %+v", out)
		}
		if _, err := webhook.NewClient(webhook.Config{Policy: mustPolicy(t), ReceiptHeader: "bad header"}); err == nil {
			t.Fatal("an invalid receipt header name was accepted")
		}
	})

	t.Run("statuses are classified for retry", func(t *testing.T) {
		for _, tc := range []struct {
			status    int
			class     chatops.FailureClass
			permanent bool
		}{
			{500, chatops.ClassReceiverError, false}, {502, chatops.ClassReceiverError, false}, {503, chatops.ClassReceiverError, false}, {504, chatops.ClassReceiverError, false},
			{408, chatops.ClassReceiverError, false}, {425, chatops.ClassReceiverError, false}, {429, chatops.ClassRateLimited, false},
			{400, chatops.ClassRejected, true}, {401, chatops.ClassRejected, true}, {403, chatops.ClassRejected, true}, {404, chatops.ClassRejected, true},
			{410, chatops.ClassRejected, true}, {413, chatops.ClassRejected, true}, {422, chatops.ClassRejected, true},
			{301, chatops.ClassRedirectRefused, true}, {302, chatops.ClassRedirectRefused, true}, {307, chatops.ClassRedirectRefused, true}, {308, chatops.ClassRedirectRefused, true},
		} {
			rc := chatopstest.NewReceiver(t, chatopstest.Status(tc.status))
			out := send(t, client(t, webhook.Config{}, rc), request(scopeA, rc.URL(), ""), 3*time.Second)
			if out.Delivered || out.Class != tc.class || out.Class.Permanent() != tc.permanent || out.HTTPStatus != tc.status {
				t.Errorf("%d: %+v", tc.status, out)
			}
			if out.Receipt != "" {
				t.Errorf("%d: a failure carried a receipt", tc.status)
			}
			// The receiver's diagnostic body never reaches the outcome.
			if s := fmt.Sprintf("%+v", out); strings.Contains(s, "diagnostic") || strings.Contains(s, rc.AddrPort().String()) {
				t.Errorf("%d: the outcome leaks transport detail: %s", tc.status, s)
			}
		}
	})

	t.Run("Retry-After is honored in seconds and bounded", func(t *testing.T) {
		for header, want := range map[string]time.Duration{"7": 7 * time.Second, "0": 0, "999999": time.Hour, "9223372036": time.Hour, "9223372036854775807": time.Hour, "99999999999999999999999": 0, "-5": 0, "soon": 0, "Wed, 21 Oct 2026 07:28:00 GMT": 0, "": 0} {
			rc := chatopstest.NewReceiver(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				if header != "" {
					w.Header().Set("Retry-After", header)
				}
				w.WriteHeader(429)
			})
			out := send(t, client(t, webhook.Config{}, rc), request(scopeA, rc.URL(), ""), 3*time.Second)
			if out.Class != chatops.ClassRateLimited || out.RetryAfter != want {
				t.Errorf("Retry-After %q: %+v, want %s", header, out, want)
			}
		}
	})

	t.Run("a slow receiver is a timeout and an unknown outcome", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, chatopstest.Hang())
		start := time.Now()
		out := send(t, client(t, webhook.Config{}, rc), request(scopeA, rc.URL(), ""), 300*time.Millisecond)
		if out.Delivered || out.Class != chatops.ClassTimeout || !out.Uncertain || out.Class.Permanent() {
			t.Fatalf("outcome = %+v", out)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("the deadline was not enforced: %s", time.Since(start))
		}
		if rc.Count() != 1 {
			t.Fatalf("the receiver saw %d requests", rc.Count())
		}
	})

	t.Run("acceptance followed by a lost response is a connection loss and an unknown outcome", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, chatopstest.LoseResponse())
		out := send(t, client(t, webhook.Config{}, rc), request(scopeA, rc.URL(), ""), 3*time.Second)
		if out.Delivered || out.Class != chatops.ClassConnectionLost || !out.Uncertain || out.Class.Permanent() {
			t.Fatalf("outcome = %+v", out)
		}
		if rc.Count() != 1 || rc.Effects() != 1 {
			t.Fatalf("the receiver processed it %d times (effects %d); the outcome must be unknown, not failed-without-effect", rc.Count(), rc.Effects())
		}
	})

	t.Run("a refused connection never reached the receiver and is not uncertain", func(t *testing.T) {
		addr := closedAddr(t)
		p, err := webhook.NewPolicy(webhook.PolicyConfig{Ports: []int{int(addr.Port())}, PrivateEndpoints: []webhook.PrivateEndpoint{{Scope: scopeA, Addr: addr}}})
		if err != nil {
			t.Fatal(err)
		}
		out := send(t, client(t, webhook.Config{Policy: p}), request(scopeA, "https://"+addr.String()+"/hook", ""), 3*time.Second)
		if out.Delivered || out.Class != chatops.ClassUnreachable || out.Uncertain || out.Class.Permanent() {
			t.Fatalf("outcome = %+v", out)
		}
	})

	t.Run("a certificate that does not verify fails before any request", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, nil)
		p := mustPolicy(t, rc)
		c, err := webhook.NewClient(webhook.Config{Policy: p}) // no trust anchor for the fixture's certificate
		if err != nil {
			t.Fatal(err)
		}
		out := send(t, c, request(scopeA, rc.URL(), ""), 3*time.Second)
		if out.Delivered || out.Class != chatops.ClassUnreachable || out.Uncertain {
			t.Fatalf("outcome = %+v", out)
		}
		if rc.Count() != 0 {
			t.Fatalf("a request was sent over an unverified connection: %d", rc.Count())
		}
	})

	t.Run("oversized response headers are a bounded bad response, not delivery", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Big", strings.Repeat("h", 64<<10))
			w.WriteHeader(200)
		})
		out := send(t, client(t, webhook.Config{MaxResponseHeaderBytes: 4 << 10}, rc), request(scopeA, rc.URL(), ""), 3*time.Second)
		if out.Delivered || out.Class != chatops.ClassBadResponse || !out.Uncertain {
			t.Fatalf("outcome = %+v", out)
		}
	})

	t.Run("an oversized body is read only to its bound and never retained", func(t *testing.T) {
		huge := strings.Repeat("b", 8<<20)
		rc := chatopstest.NewReceiver(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Receipt-Id", "big-1")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(huge))
		})
		out := send(t, client(t, webhook.Config{MaxResponseBodyBytes: 1024}, rc), request(scopeA, rc.URL(), ""), 5*time.Second)
		if !out.Delivered || out.Receipt != "big-1" {
			t.Fatalf("outcome = %+v", out)
		}
		if strings.Contains(fmt.Sprintf("%+v", out), "bbbb") {
			t.Fatal("response content reached the outcome")
		}
	})

	t.Run("the request carries only the documented headers and the exact payload", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, nil)
		r := request(scopeA, rc.URL(), "")
		r.Attempt = 4
		send(t, client(t, webhook.Config{UserAgent: "kodama-test/1"}, rc), r, 3*time.Second)
		reqs := rc.Requests()
		if len(reqs) != 1 {
			t.Fatalf("requests = %d", len(reqs))
		}
		got := reqs[0]
		if string(got.Body) != string(r.Payload) {
			t.Fatalf("body = %q", got.Body)
		}
		want := map[string]string{
			"Content-Type": "application/json", "Idempotency-Key": "cdl_t041_1", "Kodama-Delivery-Id": "cdl_t041_1", "Kodama-Event-Id": "evt_1",
			"Kodama-Event-Type": "gate.requested", "Kodama-Attempt": "4", "User-Agent": "kodama-test/1",
		}
		for k, v := range want {
			if got.Header.Get(k) != v {
				t.Errorf("header %s = %q, want %q", k, got.Header.Get(k), v)
			}
		}
		var names []string
		for k := range got.Header {
			names = append(names, k)
		}
		sort.Strings(names)
		allowed := map[string]bool{"Accept": true, "Connection": true, "Accept-Encoding": true, "Content-Length": true, "Content-Type": true, "Idempotency-Key": true, "Kodama-Attempt": true,
			"Kodama-Delivery-Id": true, "Kodama-Event-Id": true, "Kodama-Event-Type": true, "User-Agent": true}
		for _, n := range names {
			if !allowed[n] {
				t.Errorf("unexpected header %s", n)
			}
		}
		for _, forbidden := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Forwarded-For"} {
			if got.Header.Get(forbidden) != "" {
				t.Errorf("a credential-free send carried %s", forbidden)
			}
		}
	})

	t.Run("an identical payload and key are sent on every attempt", func(t *testing.T) {
		rc := chatopstest.NewReceiver(t, chatopstest.Then(2, chatopstest.Status(503), chatopstest.Accept("ok-1")))
		c := client(t, webhook.Config{}, rc)
		r := request(scopeA, rc.URL(), "")
		for a := 1; a <= 3; a++ {
			r.Attempt = a
			send(t, c, r, 3*time.Second)
		}
		reqs := rc.Requests()
		if len(reqs) != 3 {
			t.Fatalf("requests = %d", len(reqs))
		}
		for _, q := range reqs {
			if string(q.Body) != string(r.Payload) || q.Header.Get("Idempotency-Key") != "cdl_t041_1" {
				t.Fatalf("attempt %d changed content or key", q.N)
			}
		}
		if rc.Effects() != 1 {
			t.Fatalf("a deduplicating receiver applied %d effects", rc.Effects())
		}
	})
}
