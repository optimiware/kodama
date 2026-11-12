package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/services"
)

type fakeTransport struct {
	mu       sync.Mutex
	calls    []api.Invocation
	events   api.Result
	inbox    api.Result
	invoked  chan struct{}
	closeOne sync.Once
}

func (transport *fakeTransport) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	transport.mu.Lock()
	transport.calls = append(transport.calls, invocation)
	transport.mu.Unlock()
	if transport.invoked != nil {
		transport.closeOne.Do(func() { close(transport.invoked) })
	}
	switch invocation.Operation {
	case "event.read":
		return transport.events, nil
	case "task.inbox":
		return transport.inbox, nil
	default:
		return api.Result{}, errors.New("unexpected operation " + invocation.Operation)
	}
}

func eventReadResult(t *testing.T, hasMore bool) api.Result {
	t.Helper()
	next, err := api.EncodeCursor("1")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	body, err := json.Marshal(api.EventReadResult{
		Events: []api.Event{{
			ID:        "event-1",
			Type:      "action.requested",
			Source:    api.EventSource{Kind: "shell", PrincipalID: "principal-1"},
			At:        time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
			Scope:     api.EventScope{TenantID: "tenant-1", ProjectID: "project-1"},
			SubjectID: "subject-1",
		}},
		Next:    next,
		HasMore: hasMore,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return api.Result{Operation: "event.read", Body: body}
}

func inboxResult(t *testing.T) api.Result {
	t.Helper()
	// The served task.inbox result carries typed items, never the legacy
	// "assignments" projection the adapter refuses.
	body, err := json.Marshal(api.TaskInboxResult{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return api.Result{Operation: "task.inbox", Body: body}
}

func TestRunRefusesWithoutServiceConfiguration(t *testing.T) {
	if err := Run(context.Background(), Config{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Run error = %v; want ErrDisabled", err)
	}
}

func TestRunRunsConfiguredAdapterUntilCancellation(t *testing.T) {
	transport := &fakeTransport{
		events:  eventReadResult(t, false),
		inbox:   inboxResult(t),
		invoked: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Kind:           services.KindResponder,
			ServiceID:      "service-1",
			TenantID:       "tenant-1",
			ProjectID:      "project-1",
			Transport:      transport,
			RescanInterval: 10 * time.Millisecond,
		})
	}()
	select {
	case <-transport.invoked:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the adapter never invoked the public edge")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not join after cancellation")
	}
}

func TestPublicEventSourceMapsRetainedPage(t *testing.T) {
	transport := &fakeTransport{events: eventReadResult(t, false)}
	source, err := NewPublicEventSource(transport, "project-1")
	if err != nil {
		t.Fatalf("NewPublicEventSource: %v", err)
	}
	page, err := source.Poll(context.Background(), "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(page.Notifications) != 1 {
		t.Fatalf("notifications = %+v", page.Notifications)
	}
	notification := page.Notifications[0]
	if notification.EventID != "event-1" || notification.TenantID != "tenant-1" || notification.ProjectID != "project-1" {
		t.Fatalf("notification = %+v", notification)
	}
	if page.Next.NextCursor != "" {
		t.Fatalf("next cursor = %q; want empty when has_more is false", page.Next.NextCursor)
	}
}

func TestPublicEventSourceReturnsCursorOnlyWhenMore(t *testing.T) {
	transport := &fakeTransport{events: eventReadResult(t, true)}
	source, err := NewPublicEventSource(transport, "project-1")
	if err != nil {
		t.Fatalf("NewPublicEventSource: %v", err)
	}
	page, err := source.Poll(context.Background(), "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if page.Next.NextCursor == "" {
		t.Fatal("Poll dropped the continuation cursor")
	}
}

func TestPublicEventSourceRefusesMismatchedOperation(t *testing.T) {
	transport := &fakeTransport{events: api.Result{Operation: "task.inbox", Body: []byte(`{}`)}}
	source, err := NewPublicEventSource(transport, "project-1")
	if err != nil {
		t.Fatalf("NewPublicEventSource: %v", err)
	}
	if _, err := source.Poll(context.Background(), ""); err == nil {
		t.Fatal("Poll accepted a result for a different operation")
	}
}
