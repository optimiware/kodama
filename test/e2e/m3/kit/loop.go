package kit

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// AllEvents are the event types a destination can select.
var AllEvents = []string{"work_item.transitioned", "gate.requested", "gate.decided", "gate.invalidated", "run.state_changed", "deployment.state_changed"}

// Destination configures the receiver as a project destination, as the
// administrator (project:settings), selecting the events (default: all).
func (e *Env) Destination(name string, events ...string) client.Destination {
	t := e.T
	t.Helper()
	if len(events) == 0 {
		events = AllEvents
	}
	evs := make([]string, len(events))
	copy(evs, events)
	b := e.MustDo("admin", "POST", e.ProjectPath()+"/chatops-destinations", map[string]any{"name": name, "endpoint": e.Receiver.URL(), "events": evs, "enabled": true}, nil, http.StatusCreated)
	var d client.Destination
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Deliveries lists the project's notification deliveries as the persona.
func (e *Env) Deliveries(persona string, query string) []client.Delivery {
	t := e.T
	t.Helper()
	path := e.ProjectPath() + "/chatops-deliveries?limit=200"
	if query != "" {
		path += "&" + query
	}
	var out []client.Delivery
	var page struct {
		Items      []client.Delivery
		NextCursor *string
	}
	if err := json.Unmarshal(e.MustDo(persona, "GET", path, nil, nil, http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	out = append(out, page.Items...)
	return out
}

// DeliveryDetail reads one delivery with its complete attempt history.
func (e *Env) DeliveryDetail(persona, id string) client.DeliveryDetail {
	t := e.T
	t.Helper()
	var d client.DeliveryDetail
	if err := json.Unmarshal(e.MustDo(persona, "GET", e.ProjectPath()+"/chatops-deliveries/"+id, nil, nil, http.StatusOK), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Replay asks for the manual replay of a failed delivery as the persona.
func (e *Env) Replay(persona string, d client.Delivery, reason string) (int, []byte) {
	return e.Do(persona, "POST", e.ProjectPath()+"/chatops-deliveries/"+d.Id+"/replay", map[string]any{"reason": reason},
		map[string]string{"If-Match": `"` + itoa(int64(d.Version)) + `"`})
}

// Payload is the part of the outbound webhook payload the scenarios read.
type Payload struct {
	SchemaVersion  int    `json:"schemaVersion"`
	NotificationID string `json:"notificationId"`
	Event          struct {
		ID, Type, OccurredAt, CorrelationID string
	} `json:"event"`
	TenantID, ProjectID string
	Resource            struct {
		Type, ID string
		Version  int64
	}
	Summary    string
	Attributes map[string]string
	URL        string `json:"url"`
}

// Received decodes the notifications the receiver saw, in arrival order.
func (e *Env) Received() []Payload {
	e.T.Helper()
	var out []Payload
	for _, r := range e.Receiver.Requests() {
		var p Payload
		if err := json.Unmarshal(r.Body, &p); err != nil {
			e.T.Fatalf("a notification that is not the documented payload: %v: %s", err, r.Body)
		}
		out = append(out, p)
	}
	return out
}

// ReceivedFor returns the distinct notifications about a resource of a type, by
// notification id (a retry or replay repeats the id).
func (e *Env) ReceivedFor(eventType, resource string) []Payload {
	e.T.Helper()
	seen := map[string]bool{}
	var out []Payload
	for _, p := range e.Received() {
		if p.Event.Type == eventType && p.Resource.ID == resource && !seen[p.NotificationID] {
			seen[p.NotificationID] = true
			out = append(out, p)
		}
	}
	return out
}

// WaitReceived waits until a distinct notification of the type about the
// resource reached the receiver and returns the first.
func (e *Env) WaitReceived(eventType, resource string, within time.Duration) Payload {
	e.T.Helper()
	var p Payload
	m2.WaitFor(e.T, "a "+eventType+" notification about "+resource, within, func() bool {
		got := e.ReceivedFor(eventType, resource)
		if len(got) == 0 {
			return false
		}
		p = got[0]
		return true
	})
	return p
}

// WaitDelivered waits until every delivery of the project is delivered or
// failed (nothing pending or in flight) and returns them.
func (e *Env) WaitSettled(within time.Duration) []client.Delivery {
	e.T.Helper()
	var ds []client.Delivery
	m2.WaitFor(e.T, "the notification deliveries to settle", within, func() bool {
		ds = e.Deliveries("admin", "")
		for _, d := range ds {
			if d.State != "delivered" && d.State != "failed" {
				return false
			}
		}
		return len(ds) > 0
	})
	return ds
}

// DeliveryOf returns the delivery of the event type about the resource (the
// payload's notification id is the delivery id).
func (e *Env) DeliveryByID(id string) (client.Delivery, bool) {
	for _, d := range e.Deliveries("admin", "") {
		if d.Id == id {
			return d, true
		}
	}
	return client.Delivery{}, false
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return strings.TrimSpace(string(b))
}

// LoseFirstPerKey is a receiver that accepts every notification but loses the
// response of the first request carrying each Idempotency-Key: the receiver has
// applied it and Kodama cannot know.
func LoseFirstPerKey() chatopstest.Behavior {
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(n int, w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		mu.Lock()
		first := !seen[key]
		seen[key] = true
		mu.Unlock()
		if first {
			chatopstest.LoseResponse()(n, w, r)
			return
		}
		chatopstest.Accept("rcpt-"+key[len(key)-6:])(n, w, r)
	}
}

// DestinationList lists the project's destinations as the administrator.
func (e *Env) DestinationList() []client.Destination {
	t := e.T
	t.Helper()
	var page struct{ Items []client.Destination }
	if err := json.Unmarshal(e.MustDo("admin", "GET", e.ProjectPath()+"/chatops-destinations?limit=200", nil, nil, http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// Disable turns the destination off as the administrator.
func (e *Env) Disable(d client.Destination) {
	e.T.Helper()
	e.MustDo("admin", "PATCH", e.ProjectPath()+"/chatops-destinations/"+d.Id, map[string]any{"enabled": false},
		map[string]string{"If-Match": `"` + itoa(int64(d.Version)) + `"`}, http.StatusOK)
}
