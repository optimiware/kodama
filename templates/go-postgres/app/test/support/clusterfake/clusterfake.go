// Package clusterfake is an in-memory Kubernetes double with the semantics the
// delivery helper depends on: namespace-scoped credentials, annotations, UIDs
// that survive a re-apply, structured readiness and injectable apply faults.
// Real-cluster behavior is qualified separately; this double never claims it.
package clusterfake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"example.invalid/inventory/internal/delivery"
)

type obj struct {
	annotations map[string]string
	uid         string
	spec        string
	gets        int
}

// Cluster implements delivery.Cluster.
type Cluster struct {
	mu sync.Mutex
	// Namespaces the credential covers. Empty means any.
	Allowed map[string]bool
	objects map[delivery.ObjectRef]*obj
	next    int

	// Fault injection.
	FailAfterEffect  int // the next n applies take effect, then return an error
	FailBeforeEffect int // the next n applies return an error without any effect
	ReadyAfterGets   int // objects become ready after this many Get calls (0 = immediately)
	NeverReady       bool
	FailedJobs       bool
	GetError         error

	// Events is a shared timeline; the factory fake appends to the same slice.
	Log *Timeline

	ApplyCalls int
	Effects    int
	last       []byte
}

// Timeline is an ordered, concurrency-safe event list shared between doubles.
type Timeline struct {
	mu     sync.Mutex
	events []string
}

// Add appends an event.
func (t *Timeline) Add(format string, a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, fmt.Sprintf(format, a...))
}

// Events returns a copy.
func (t *Timeline) Events() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.events...)
}

// New returns an empty cluster.
func New(log *Timeline) *Cluster {
	return &Cluster{objects: map[delivery.ObjectRef]*obj{}, Log: log}
}

// Preexist plants an object, for example one owned by someone else.
func (c *Cluster) Preexist(ref delivery.ObjectRef, annotations map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	c.objects[ref] = &obj{annotations: annotations, uid: uid(c.next)}
}

// Object returns the annotations and uid of an object.
func (c *Cluster) Object(ref delivery.ObjectRef) (annotations map[string]string, uidv string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.objects[ref]
	if !ok {
		return nil, "", false
	}
	return o.annotations, o.uid, true
}

// LastApplied returns the manifests of the most recent apply call.
func (c *Cluster) LastApplied() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.last != nil
}

// Count is the number of objects.
func (c *Cluster) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.objects)
}

func uid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

type item struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

// Apply implements delivery.Cluster.
func (c *Cluster) Apply(_ context.Context, namespace string, list []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ApplyCalls++
	c.last = append([]byte(nil), list...)
	var l struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(list, &l); err != nil {
		return errors.New("error: the manifests are not valid JSON")
	}
	if len(c.Allowed) > 0 && !c.Allowed[namespace] {
		c.Log.Add("cluster:apply-forbidden:%s", namespace)
		return fmt.Errorf("forbidden: the credential cannot access namespace %q", namespace)
	}
	var items []item
	for _, raw := range l.Items {
		var it item
		_ = json.Unmarshal(raw, &it)
		if it.Metadata.Namespace != namespace {
			return fmt.Errorf("error: the namespace %q of %s/%s does not match the namespace %q", it.Metadata.Namespace, it.Kind, it.Metadata.Name, namespace)
		}
		items = append(items, it)
	}
	if c.FailBeforeEffect > 0 {
		c.FailBeforeEffect--
		c.Log.Add("cluster:apply-failed-before-effect")
		return errors.New("error: the connection to the server was refused")
	}
	changed := false
	for i, it := range items {
		ref := delivery.ObjectRef{APIVersion: it.APIVersion, Kind: it.Kind, Namespace: namespace, Name: it.Metadata.Name}
		spec := string(l.Items[i])
		if cur, ok := c.objects[ref]; ok {
			if cur.spec != spec {
				cur.spec, cur.annotations = spec, it.Metadata.Annotations
				changed = true
			}
			continue
		}
		c.next++
		c.objects[ref] = &obj{annotations: it.Metadata.Annotations, uid: uid(c.next), spec: spec}
		changed = true
	}
	if changed {
		c.Effects++
	}
	c.Log.Add("cluster:apply:%d-objects", len(items))
	if c.FailAfterEffect > 0 {
		c.FailAfterEffect--
		c.Log.Add("cluster:apply-response-lost")
		return errors.New("error: the connection was reset before the response was read")
	}
	return nil
}

// Get implements delivery.Cluster.
func (c *Cluster) Get(_ context.Context, ref delivery.ObjectRef) (delivery.Observed, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.GetError != nil {
		return delivery.Observed{}, false, c.GetError
	}
	o, ok := c.objects[ref]
	if !ok {
		return delivery.Observed{}, false, nil
	}
	o.gets++
	ready := !c.NeverReady && o.gets > c.ReadyAfterGets
	obs := delivery.Observed{Ref: ref, UID: o.uid, Annotations: o.annotations, Ready: ready}
	if ref.Kind == "Job" && c.FailedJobs {
		obs.Ready, obs.Failed = false, true
	}
	return obs, true, nil
}
