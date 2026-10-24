// Package kubernetes is the log-reader adapter over the Kubernetes API: it
// verifies that the Job and Pod belong to the attempt before opening the
// selected container's log subresource, and bounds what it reads.
//
// The reader takes an injected, already authenticated *http.Client and the API
// endpoint; it never reads a kubeconfig or an ambient credential. Namespace and
// container come from trusted composition and the Job identity from persisted
// execution state. Nothing about the workload is taken from a caller: any
// selector used to discover Pods is built here from the Job's own selector
// labels, and ownership is verified before a byte is read.
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/logs"
)

// DefaultMaxObjectBytes bounds a Job or Pod list response.
const DefaultMaxObjectBytes = 4 << 20

// Config configures a Reader.
type Config struct {
	// Endpoint is the API server base URL, for example https://10.0.0.1:6443.
	Endpoint string
	// Client is authenticated by its transport (TLS client certificate, token
	// round tripper, CA pool). Its Timeout must not be set for follow reads: the
	// lifetime of a read is the request context.
	Client *http.Client
	// AllowInsecure permits an http:// endpoint (local fixtures only).
	AllowInsecure bool
	// MaxObjectBytes bounds API object responses (default DefaultMaxObjectBytes).
	MaxObjectBytes int64
	// DiscoveryTimeout bounds the Job and Pod lookups that precede the log read
	// (default 15s).
	DiscoveryTimeout time.Duration
}

// Reader implements logs.LiveReader.
type Reader struct {
	base   *url.URL
	client *http.Client
	max    int64
	disco  time.Duration
}

var _ logs.LiveReader = (*Reader)(nil)

// NewReader validates cfg.
func NewReader(cfg Config) (*Reader, error) {
	if cfg.Client == nil {
		return nil, errors.New("logs/kubernetes: an authenticated HTTP client is required")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("logs/kubernetes: the endpoint must be a bare http(s) API server URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && cfg.AllowInsecure:
	default:
		return nil, errors.New("logs/kubernetes: the endpoint must be https")
	}
	// The API server never needs to redirect a request the reader built from
	// validated identifiers; following one could forward the client's credential
	// to another location. Work on a copy so the caller's client is untouched.
	client := *cfg.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r := &Reader{base: u, client: &client, max: cfg.MaxObjectBytes, disco: cfg.DiscoveryTimeout}
	if r.max <= 0 {
		r.max = DefaultMaxObjectBytes
	}
	if r.disco <= 0 {
		r.disco = 15 * time.Second
	}
	return r, nil
}

type objectMeta struct {
	Name              string `json:"name"`
	Namespace         string `json:"namespace"`
	UID               string `json:"uid"`
	CreationTimestamp string `json:"creationTimestamp"`
	DeletionTimestamp string `json:"deletionTimestamp"`
	OwnerReferences   []struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		UID        string `json:"uid"`
		Controller *bool  `json:"controller"`
	} `json:"ownerReferences"`
}

type job struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
	} `json:"spec"`
}

type pod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
}

type podList struct {
	Items []pod `json:"items"`
}

// Open implements logs.LiveReader. Failures are classified: ErrWorkloadMissing
// for an absent Job, Pod or not-yet-started container, ErrWorkloadMismatch for
// an object that is not the attempt's, ErrUnavailable for everything else.
func (r *Reader) Open(ctx context.Context, t logs.Target, q logs.LiveQuery) (io.ReadCloser, error) {
	if err := validTarget(t); err != nil {
		return nil, err
	}
	if q.TailLines < 0 || q.LimitBytes < 0 {
		return nil, fmt.Errorf("%w: negative bound", logs.ErrInvalid)
	}
	dctx, cancel := context.WithTimeout(ctx, r.disco)
	defer cancel()

	var j job
	if err := r.getJSON(dctx, r.jobURL(t), &j); err != nil {
		return nil, err
	}
	if j.Metadata.UID != t.JobUID || j.Metadata.Name != t.JobName || (j.Metadata.Namespace != "" && j.Metadata.Namespace != t.Namespace) {
		return nil, logs.ErrWorkloadMismatch
	}
	listURL, err := r.podsURL(t, j.Spec.Selector.MatchLabels)
	if err != nil {
		return nil, err
	}
	var pods podList
	if err := r.getJSON(dctx, listURL, &pods); err != nil {
		return nil, err
	}
	p, err := pick(pods.Items, t)
	if err != nil {
		return nil, err
	}

	// The log read is bound to the caller's context, not the discovery timeout.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.logURL(t, p.Metadata.Name, q), nil)
	if err != nil {
		return nil, logs.Unavailablef(nil, "log request")
	}
	// kubectl sends "text/plain, */*": a real API server answers 406 to the
	// log subresource when only text/plain is acceptable (found against a
	// real cluster; the fake could not show it).
	req.Header.Set("Accept", "text/plain, */*")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, logs.Unavailablef(nil, "log request")
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusBadRequest:
		// 400 is how the API reports a container that is waiting to start.
		drain(resp.Body)
		return nil, logs.ErrWorkloadMissing
	default:
		drain(resp.Body)
		return nil, logs.Unavailablef(nil, fmt.Sprintf("log read status %d", resp.StatusCode))
	}
	var body io.Reader = resp.Body
	if q.LimitBytes > 0 {
		// An independent local bound, whatever the server does with limitBytes.
		body = io.LimitReader(resp.Body, q.LimitBytes)
	}
	return &stream{Reader: body, closer: resp.Body}, nil
}

type stream struct {
	io.Reader
	closer io.Closer
}

func (s *stream) Close() error { return s.closer.Close() }

func drain(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4<<10))
	_ = rc.Close()
}

// getJSON reads one API object with a bounded body.
func (r *Reader) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return logs.Unavailablef(nil, "api request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return logs.Unavailablef(nil, "api request")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		drain(resp.Body)
		return logs.ErrWorkloadMissing
	default:
		drain(resp.Body)
		return logs.Unavailablef(nil, fmt.Sprintf("api status %d", resp.StatusCode))
	}
	lr := &io.LimitedReader{R: resp.Body, N: r.max + 1}
	body, err := io.ReadAll(lr)
	if err != nil || int64(len(body)) > r.max {
		return logs.Unavailablef(nil, "api response")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return logs.Unavailablef(nil, "api response")
	}
	return nil
}

// pick selects the attempt's Pod: only a Pod in the namespace controlled by
// exactly this Job (kind, name and UID) is a candidate, the selected container
// must exist in it, and the most recently created candidate wins. A Pod that
// merely matches a label never qualifies.
func pick(items []pod, t logs.Target) (pod, error) {
	var owned []pod
	for _, p := range items {
		if p.Metadata.Namespace != "" && p.Metadata.Namespace != t.Namespace {
			continue
		}
		if !controlledBy(p, t) || !nameRe.MatchString(p.Metadata.Name) {
			continue
		}
		owned = append(owned, p)
	}
	if len(owned) == 0 {
		return pod{}, logs.ErrWorkloadMissing
	}
	sort.SliceStable(owned, func(i, j int) bool {
		a, b := owned[i].Metadata, owned[j].Metadata
		if a.CreationTimestamp != b.CreationTimestamp {
			return a.CreationTimestamp > b.CreationTimestamp // RFC 3339 UTC sorts lexically
		}
		return a.Name > b.Name
	})
	p := owned[0]
	for _, c := range p.Spec.Containers {
		if c.Name == t.Container {
			return p, nil
		}
	}
	return pod{}, logs.ErrWorkloadMismatch
}

func controlledBy(p pod, t logs.Target) bool {
	for _, o := range p.Metadata.OwnerReferences {
		if o.Kind == "Job" && o.Name == t.JobName && o.UID == t.JobUID && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}
