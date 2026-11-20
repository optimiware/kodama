package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
)

type sseRoundTripperFunc func(*http.Request) (*http.Response, error)

func (fn sseRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func TestHTTPTransportStreamEventsReconnectAndDeduplicate(t *testing.T) {
	cur1, _ := api.EncodeEventCursor(1)
	cur2, _ := api.EncodeEventCursor(2)
	cur3, _ := api.EncodeEventCursor(3)

	evtJSON := func(id, cur string, seq uint64, typ string) string {
		raw, _ := json.Marshal(api.StreamEvent{
			ID:        id,
			Cursor:    cur,
			Sequence:  seq,
			Type:      typ,
			At:        fixedNow(),
			Scope:     api.EventScope{TenantID: "t1", ProjectID: "proj-1"},
			SubjectID: "sub-" + id,
			Data:      json.RawMessage(`{"ok":true}`),
		})
		return string(raw)
	}

	var (
		mu              sync.Mutex
		calls           int
		seenLastEventID []string
		seenAuth        []string
	)
	rt := sseRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		call := calls
		seenLastEventID = append(seenLastEventID, req.Header.Get("Last-Event-ID"))
		seenAuth = append(seenAuth, req.Header.Get("Authorization"))
		mu.Unlock()

		if req.Header.Get("Accept") != api.EventStreamMediaType {
			t.Errorf("Accept = %q; want %s", req.Header.Get("Accept"), api.EventStreamMediaType)
		}
		switch call {
		case 1:
			// Emit keepalive, event 1, event 2, then close (EOF simulating server expiry/disconnect).
			payload := ": keepalive\n\n" +
				"id: " + cur1 + "\nevent: task.created\ndata: " + evtJSON("e1", cur1, 1, "task.created") + "\n\n" +
				"id: " + cur2 + "\nevent: execution.queued\ndata: " + evtJSON("e2", cur2, 2, "execution.queued") + "\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(payload)),
				Request:    req,
			}, nil
		case 2:
			// Replay overlapping event 2 (must be deduplicated!) then emit event 3.
			payload := "id: " + cur2 + "\nevent: execution.queued\ndata: " + evtJSON("e2", cur2, 2, "execution.queued") + "\n\n" +
				"id: " + cur3 + "\nevent: execution.succeeded\ndata: " + evtJSON("e3", cur3, 3, "execution.succeeded") + "\n\n"
			pr, pw := io.Pipe()
			go func() {
				_, _ = io.WriteString(pw, payload)
				<-req.Context().Done()
				_ = pw.Close()
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       pr,
				Request:    req,
			}, nil
		default:
			return nil, errors.New("unexpected extra call")
		}
	})

	source := &fakeSource{credentials: []client.Credential{
		{Kind: client.KindSession, Token: "tok-1", ExpiresAt: fixedNow().Add(time.Hour)},
		{Kind: client.KindSession, Token: "tok-2-refreshed", ExpiresAt: fixedNow().Add(2 * time.Hour)},
	}}
	tr := &client.HTTPTransport{
		BaseURL:      "https://api.kodema.example",
		RoundTripper: rt,
		Source:       source,
		Retry:        client.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		Now:          fixedNow,
		Sleep:        func(time.Duration) {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := tr.StreamEvents(ctx, client.EventStreamRequest{
		ProjectID: "proj-1",
		Types:     []string{"task.created", "execution.queued", "execution.succeeded"},
	})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	defer stream.Close()

	var gotIDs []string
	for range 3 {
		evt, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		gotIDs = append(gotIDs, evt.ID)
	}
	if strings.Join(gotIDs, ",") != "e1,e2,e3" {
		t.Fatalf("received IDs = %v; want [e1 e2 e3] with zero duplicates", gotIDs)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenLastEventID) != 2 || seenLastEventID[0] != "" || seenLastEventID[1] != cur2 {
		t.Fatalf("Last-Event-ID headers = %v; want [\"\", %q]", seenLastEventID, cur2)
	}
	if len(seenAuth) != 2 || seenAuth[0] != "Bearer tok-1" || seenAuth[1] != "Bearer tok-2-refreshed" {
		t.Fatalf("Authorization headers = %v; want refreshed token on reconnect", seenAuth)
	}
}

func TestHTTPTransportStreamEventsRefusesPermanentErrors(t *testing.T) {
	rt := sseRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(api.Error{Code: api.CodeForbidden, Message: "denied"})
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": []string{api.MediaType}},
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Request:    req,
		}, nil
	})
	tr := &client.HTTPTransport{
		BaseURL:      "https://api.kodema.example",
		RoundTripper: rt,
		Source: &fakeSource{credentials: []client.Credential{
			{Kind: client.KindSession, Token: "tok-1", ExpiresAt: fixedNow().Add(time.Hour)},
		}},
		Retry: client.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		Now:   fixedNow,
		Sleep: func(time.Duration) {},
	}

	_, err := tr.StreamEvents(context.Background(), client.EventStreamRequest{ProjectID: "proj-1"})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("err = %v (class=%v); want forbidden", err, class)
	}
}
