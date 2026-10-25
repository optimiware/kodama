// Package probe contains test-only programs run beside the installed server or as isolated Jobs.
// It is never wired into Kodama's product binary or release image.
package probe

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

type Target struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type FaultStatus struct {
	Target  Target `json:"target"`
	UID     string `json:"uid"`
	Lost    bool   `json:"lost"`
	Creates int    `json:"creates"`
}

// Proxy forwards the real Kubernetes API. Only an explicitly armed Job create is intercepted: the
// named Job, or, when the name is empty, the first Job created in the armed namespace (the
// controller's first dispatch happens before its Job name could be known to the test).
// Its successful upstream response is withheld, then read-back is refused until the Pod is
// replaced. State is intentionally in memory: a replacement sidecar is transparent again.
type Proxy struct {
	Origin *url.URL
	Client *http.Client
	mu     sync.Mutex
	status FaultStatus
}

var objectName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`)

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/__fault/") {
		p.admin(w, r)
		return
	}
	p.mu.Lock()
	s := p.status
	p.mu.Unlock()
	jobs := "/apis/batch/v1/namespaces/" + s.Target.Namespace + "/jobs"
	pods := "/api/v1/namespaces/" + s.Target.Namespace + "/pods"
	if s.Lost && (r.URL.Path == jobs || strings.HasPrefix(r.URL.Path, jobs+"/") || r.URL.Path == pods || strings.HasPrefix(r.URL.Path, pods+"/")) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	u := *p.Origin
	u.Path, u.RawPath, u.RawQuery = r.URL.Path, r.URL.RawPath, r.URL.RawQuery
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	req.Header = r.Header.Clone()
	stripHop(req.Header)
	resp, err := p.Client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if s.Target.Namespace != "" && !s.Lost && r.Method == http.MethodPost && r.URL.Path == jobs && resp.StatusCode == http.StatusCreated {
		// Inspect safe metadata only. Do not print request/response bodies or credentials.
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		var job struct{ Metadata struct{ Name, UID string } }
		if err != nil || json.Unmarshal(body, &job) != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if s.Target.Name == "" || job.Metadata.Name == s.Target.Name {
			p.mu.Lock()
			p.status.Target.Name, p.status.UID, p.status.Lost = job.Metadata.Name, job.Metadata.UID, true
			p.status.Creates++
			p.mu.Unlock()
			// No HTTP response reaches the original controller, even though create committed.
			if h, ok := w.(http.Hijacker); ok {
				if c, _, err := h.Hijack(); err == nil {
					c.Close()
					return
				}
			}
			// HTTP/2 has no Hijacker. Abort that stream without sending an acknowledgment;
			// net/http suppresses a stack trace specifically for this sentinel panic.
			panic(http.ErrAbortHandler)
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		stripHop(w.Header())
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	stripHop(w.Header())
	w.WriteHeader(resp.StatusCode)
	// Flush streaming log reads without buffering until EOF.
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func stripHop(h http.Header) {
	for _, name := range strings.Split(h.Get("Connection"), ",") {
		h.Del(strings.TrimSpace(name))
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(name)
	}
}

func (p *Proxy) admin(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.URL.Path == "/__fault/arm" && r.Method == http.MethodPost:
		var target Target
		dec := json.NewDecoder(io.LimitReader(r.Body, 2048))
		dec.DisallowUnknownFields()
		if dec.Decode(&target) != nil || (target.Name != "" && !objectName.MatchString(target.Name)) || !objectName.MatchString(target.Namespace) || !strings.HasPrefix(target.Namespace, "kodama-q-") || p.status.Target.Namespace != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.status = FaultStatus{Target: target}
	case r.URL.Path == "/__fault/status" && r.Method == http.MethodGet:
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p.status)
}
