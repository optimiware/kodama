// Package kubernetes is the narrow HTTP adapter the Job executor uses to reach
// the Kubernetes API: get, list, create, conditional update and
// UID-preconditioned delete of the few resource kinds the executor manages or
// inspects. It carries no kubeconfig loader and no SDK; the caller injects an
// already authenticated *http.Client (T-027 supplies production credentials).
package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config configures a Client.
type Config struct {
	// Origin is the API server origin, for example https://10.0.0.1:6443. Plain
	// http is accepted only for loopback hosts (test servers).
	Origin string
	// HTTP is the authenticated client. Its transport owns TLS trust and
	// credentials; it must not follow redirects to other origins.
	HTTP *http.Client
	// RequestTimeout bounds every request, including its response body.
	RequestTimeout time.Duration
	// PageSize and MaxPages bound a listing.
	PageSize int
	MaxPages int
	// MaxBody bounds a response document.
	MaxBody int64
}

// Client talks to one API server.
type Client struct {
	origin *url.URL
	http   *http.Client
	cfg    Config
}

// NewClient validates the configuration.
func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Origin)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("kubernetes: the API origin must be an absolute origin without credentials, path, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if ip := net.ParseIP(u.Hostname()); !(u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
			return nil, errors.New("kubernetes: plain http is allowed only for loopback API servers")
		}
	default:
		return nil, errors.New("kubernetes: the API origin must use https")
	}
	if cfg.HTTP == nil {
		return nil, errors.New("kubernetes: an authenticated HTTP client is required")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 100
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 50
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 8 << 20
	}
	// Redirects would carry credentials to another place; refuse them.
	hc := *cfg.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{origin: &url.URL{Scheme: u.Scheme, Host: u.Host}, http: &hc, cfg: cfg}, nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

func validName(kind, s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("kubernetes: invalid %s name", kind)
	}
	return nil
}

// ---- request core ----

type status struct {
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Code    int    `json:"code"`
	Details struct {
		Causes []struct {
			Reason string `json:"reason"`
		} `json:"causes"`
	} `json:"details"`
}

// do performs one request. A mutation whose answer is lost is reported as
// ErrUnknownOutcome.
func (c *Client) do(ctx context.Context, op, method, path string, query url.Values, body any, out *[]byte) error {
	mutation := method != http.MethodGet
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &Error{Op: op, Kind: ErrInvalid, Cause: errors.New("unencodable object")}
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	u := *c.origin
	u.Path = path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return &Error{Op: op, Kind: ErrInvalid, Cause: errors.New("bad request")}
	}
	// The log subresource answers text/plain; every other operation JSON.
	req.Header.Set("Accept", "application/json, */*")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "kodama-executor")
	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(ctx, op, mutation, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBody+1))
	if err != nil {
		return transportError(ctx, op, mutation, err)
	}
	if int64(len(raw)) > c.cfg.MaxBody {
		return &Error{Op: op, Status: resp.StatusCode, Kind: ErrTooLarge}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil {
			*out = raw
		}
		return nil
	}
	return statusError(op, mutation, resp, raw)
}

func transportError(ctx context.Context, op string, mutation bool, err error) error {
	switch {
	case mutation:
		return &Error{Op: op, Kind: ErrUnknownOutcome, Cause: err}
	case errors.Is(ctx.Err(), context.Canceled):
		return &Error{Op: op, Kind: ErrCanceled, Cause: err}
	}
	return &Error{Op: op, Kind: ErrUnavailable, Cause: err}
}

func statusError(op string, mutation bool, resp *http.Response, raw []byte) error {
	var st status
	_ = json.Unmarshal(raw, &st)
	e := &Error{Op: op, Status: resp.StatusCode, Reason: safeReason(st.Reason)}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		e.Kind = ErrNotFound
	case resp.StatusCode == http.StatusConflict && st.Reason == "AlreadyExists":
		e.Kind = ErrAlreadyExists
	case resp.StatusCode == http.StatusConflict:
		e.Kind = ErrConflict
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Kind = ErrForbidden
	case resp.StatusCode == http.StatusGone:
		e.Kind = ErrExpired
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Kind = ErrThrottled
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 && s <= 3600 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusUnprocessableEntity,
		resp.StatusCode == http.StatusRequestEntityTooLarge, resp.StatusCode == http.StatusUnsupportedMediaType:
		e.Kind = ErrInvalid
	case resp.StatusCode >= 500 && mutation:
		e.Kind = ErrUnknownOutcome
	default:
		e.Kind = ErrUnavailable
	}
	return e
}

var reasonRe = regexp.MustCompile(`^[A-Za-z]{1,40}$`)

func safeReason(s string) string {
	if reasonRe.MatchString(s) {
		return s
	}
	return ""
}

// ---- generic operations ----

func (c *Client) get(ctx context.Context, op, path string) ([]byte, error) {
	var raw []byte
	if err := c.do(ctx, op, http.MethodGet, path, nil, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func decode(op string, raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return &Error{Op: op, Kind: ErrUnavailable, Cause: errors.New("undecodable response")}
	}
	return nil
}

// list reads every page of a collection, bounded by MaxPages.
func (c *Client) list(ctx context.Context, op, path, selector string, item func(json.RawMessage) error) error {
	token := ""
	for page := 0; ; page++ {
		if page >= c.cfg.MaxPages {
			return &Error{Op: op, Kind: ErrTooLarge}
		}
		q := url.Values{"limit": {strconv.Itoa(c.cfg.PageSize)}}
		if selector != "" {
			q.Set("labelSelector", selector)
		}
		if token != "" {
			q.Set("continue", token)
		}
		var raw []byte
		if err := c.do(ctx, op, http.MethodGet, path, q, nil, &raw); err != nil {
			return err
		}
		var doc struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		if err := decode(op, raw, &doc); err != nil {
			return err
		}
		for _, it := range doc.Items {
			if err := item(it); err != nil {
				return err
			}
		}
		if doc.Metadata.Continue == "" {
			return nil
		}
		token = doc.Metadata.Continue
	}
}

func nsPath(group, ns, resource string) string {
	return group + "/namespaces/" + ns + "/" + resource
}

const (
	core  = "/api/v1"
	batch = "/apis/batch/v1"
	netv1 = "/apis/networking.k8s.io/v1"
)

// ---- Jobs ----

// GetJob reads a Job. The returned Job keeps the server's raw document.
func (c *Client) GetJob(ctx context.Context, ns, name string) (Job, error) {
	if err := firstErr(validName("namespace", ns), validName("job", name)); err != nil {
		return Job{}, err
	}
	raw, err := c.get(ctx, "get job", nsPath(batch, ns, "jobs")+"/"+name)
	if err != nil {
		return Job{}, err
	}
	return decodeJob("get job", raw)
}

func decodeJob(op string, raw []byte) (Job, error) {
	var j Job
	if err := decode(op, raw, &j); err != nil {
		return Job{}, err
	}
	j.Raw = raw
	return j, nil
}

// CreateJob creates a Job. ErrAlreadyExists means the name is taken; an
// ErrUnknownOutcome means the Job may exist.
func (c *Client) CreateJob(ctx context.Context, ns string, j Job) (Job, error) {
	if err := firstErr(validName("namespace", ns), validName("job", j.Metadata.Name)); err != nil {
		return Job{}, err
	}
	j.APIVersion, j.Kind = "batch/v1", "Job"
	j.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "create job", http.MethodPost, nsPath(batch, ns, "jobs"), nil, j, &raw); err != nil {
		return Job{}, err
	}
	return decodeJob("create job", raw)
}

// ListJobs lists Jobs matching a label selector.
func (c *Client) ListJobs(ctx context.Context, ns, selector string) ([]Job, error) {
	if err := validName("namespace", ns); err != nil {
		return nil, err
	}
	var out []Job
	err := c.list(ctx, "list jobs", nsPath(batch, ns, "jobs"), selector, func(m json.RawMessage) error {
		j, err := decodeJob("list jobs", m)
		out = append(out, j)
		return err
	})
	return out, err
}

// DeleteOptions are the preconditions and propagation of a delete.
type DeleteOptions struct {
	// UID is required: a delete of an object other than the one the caller
	// verified fails with ErrConflict.
	UID string
	// ResourceVersion, when set, is also required.
	ResourceVersion string
	// Propagation is "Foreground", "Background" or "Orphan" (default Foreground).
	Propagation string
}

func (o DeleteOptions) body() (map[string]any, error) {
	if o.UID == "" {
		return nil, &Error{Op: "delete", Kind: ErrInvalid, Cause: errors.New("a UID precondition is required")}
	}
	pre := map[string]any{"uid": o.UID}
	if o.ResourceVersion != "" {
		pre["resourceVersion"] = o.ResourceVersion
	}
	prop := o.Propagation
	if prop == "" {
		prop = "Foreground"
	}
	return map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": prop, "preconditions": pre}, nil
}

// DeleteJob deletes a Job under a UID precondition.
func (c *Client) DeleteJob(ctx context.Context, ns, name string, o DeleteOptions) error {
	return c.deleteNamed(ctx, "delete job", nsPath(batch, ns, "jobs"), ns, name, o)
}

func (c *Client) deleteNamed(ctx context.Context, op, collection, ns, name string, o DeleteOptions) error {
	if err := firstErr(validName("namespace", ns), validName("object", name)); err != nil {
		return err
	}
	body, err := o.body()
	if err != nil {
		return err
	}
	return c.do(ctx, op, http.MethodDelete, collection+"/"+name, nil, body, nil)
}

// ---- Pods ----

// ListPods lists Pods matching a label selector.
func (c *Client) ListPods(ctx context.Context, ns, selector string) ([]Pod, error) {
	if err := validName("namespace", ns); err != nil {
		return nil, err
	}
	var out []Pod
	err := c.list(ctx, "list pods", nsPath(core, ns, "pods"), selector, func(m json.RawMessage) error {
		var p Pod
		if err := decode("list pods", m, &p); err != nil {
			return err
		}
		p.Raw = m
		out = append(out, p)
		return nil
	})
	return out, err
}

// GetPod reads one Pod.
func (c *Client) GetPod(ctx context.Context, ns, name string) (Pod, error) {
	if err := firstErr(validName("namespace", ns), validName("pod", name)); err != nil {
		return Pod{}, err
	}
	raw, err := c.get(ctx, "get pod", nsPath(core, ns, "pods")+"/"+name)
	if err != nil {
		return Pod{}, err
	}
	var p Pod
	if err := decode("get pod", raw, &p); err != nil {
		return Pod{}, err
	}
	p.Raw = raw
	return p, nil
}

// PodLog reads the log of one container of a Pod, at most MaxBody bytes.
func (c *Client) PodLog(ctx context.Context, ns, name, container string) ([]byte, error) {
	if err := firstErr(validName("namespace", ns), validName("pod", name), validName("container", container)); err != nil {
		return nil, err
	}
	q := url.Values{"container": {container}, "limitBytes": {strconv.FormatInt(c.cfg.MaxBody, 10)}}
	var raw []byte
	if err := c.do(ctx, "read pod log", http.MethodGet, nsPath(core, ns, "pods")+"/"+name+"/log", q, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// DeletePod deletes a Pod under a UID precondition.
func (c *Client) DeletePod(ctx context.Context, ns, name string, o DeleteOptions) error {
	return c.deleteNamed(ctx, "delete pod", nsPath(core, ns, "pods"), ns, name, o)
}

// ---- Secrets ----

type secretWire struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Immutable  *bool             `json:"immutable,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
}

// GetSecret reads a Secret.
func (c *Client) GetSecret(ctx context.Context, ns, name string) (Secret, error) {
	if err := firstErr(validName("namespace", ns), validName("secret", name)); err != nil {
		return Secret{}, err
	}
	raw, err := c.get(ctx, "get secret", nsPath(core, ns, "secrets")+"/"+name)
	if err != nil {
		return Secret{}, err
	}
	var s Secret
	if err := decode("get secret", raw, &s); err != nil {
		return Secret{}, err
	}
	return s, nil
}

// CreateSecret creates a Secret.
func (c *Client) CreateSecret(ctx context.Context, ns string, s Secret) (Secret, error) {
	if err := firstErr(validName("namespace", ns), validName("secret", s.Metadata.Name)); err != nil {
		return Secret{}, err
	}
	s.APIVersion, s.Kind = "v1", "Secret"
	s.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "create secret", http.MethodPost, nsPath(core, ns, "secrets"), nil, s, &raw); err != nil {
		return Secret{}, err
	}
	var out Secret
	return out, decode("create secret", raw, &out)
}

// UpdateSecret replaces a Secret. The object's resourceVersion is the
// precondition: a concurrent update fails with ErrConflict.
func (c *Client) UpdateSecret(ctx context.Context, ns string, s Secret) (Secret, error) {
	if err := firstErr(validName("namespace", ns), validName("secret", s.Metadata.Name)); err != nil {
		return Secret{}, err
	}
	if s.Metadata.ResourceVersion == "" {
		return Secret{}, &Error{Op: "update secret", Kind: ErrInvalid, Cause: errors.New("a resourceVersion precondition is required")}
	}
	s.APIVersion, s.Kind = "v1", "Secret"
	s.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "update secret", http.MethodPut, nsPath(core, ns, "secrets")+"/"+s.Metadata.Name, nil, s, &raw); err != nil {
		return Secret{}, err
	}
	var out Secret
	return out, decode("update secret", raw, &out)
}

// DeleteSecret deletes a Secret under a UID precondition.
func (c *Client) DeleteSecret(ctx context.Context, ns, name string, o DeleteOptions) error {
	if o.Propagation == "" {
		o.Propagation = "Background"
	}
	return c.deleteNamed(ctx, "delete secret", nsPath(core, ns, "secrets"), ns, name, o)
}

// ListSecrets lists Secrets matching a label selector.
func (c *Client) ListSecrets(ctx context.Context, ns, selector string) ([]Secret, error) {
	if err := validName("namespace", ns); err != nil {
		return nil, err
	}
	var out []Secret
	err := c.list(ctx, "list secrets", nsPath(core, ns, "secrets"), selector, func(m json.RawMessage) error {
		var s Secret
		if err := decode("list secrets", m, &s); err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	return out, err
}

// ---- read-only policy inspection ----

// GetNamespace reads a Namespace.
func (c *Client) GetNamespace(ctx context.Context, name string) (Namespace, error) {
	if err := validName("namespace", name); err != nil {
		return Namespace{}, err
	}
	raw, err := c.get(ctx, "get namespace", core+"/namespaces/"+name)
	if err != nil {
		return Namespace{}, err
	}
	var n Namespace
	return n, decode("get namespace", raw, &n)
}

// GetServiceAccount reads a ServiceAccount.
func (c *Client) GetServiceAccount(ctx context.Context, ns, name string) (ServiceAccount, error) {
	if err := firstErr(validName("namespace", ns), validName("serviceaccount", name)); err != nil {
		return ServiceAccount{}, err
	}
	raw, err := c.get(ctx, "get serviceaccount", nsPath(core, ns, "serviceaccounts")+"/"+name)
	if err != nil {
		return ServiceAccount{}, err
	}
	var s ServiceAccount
	return s, decode("get serviceaccount", raw, &s)
}

// GetConfigMap reads a ConfigMap.
func (c *Client) GetConfigMap(ctx context.Context, ns, name string) (ConfigMap, error) {
	if err := firstErr(validName("namespace", ns), validName("configmap", name)); err != nil {
		return ConfigMap{}, err
	}
	raw, err := c.get(ctx, "get configmap", nsPath(core, ns, "configmaps")+"/"+name)
	if err != nil {
		return ConfigMap{}, err
	}
	var m ConfigMap
	return m, decode("get configmap", raw, &m)
}

// ListResourceQuotas lists the namespace's ResourceQuotas.
func (c *Client) ListResourceQuotas(ctx context.Context, ns string) ([]ResourceQuota, error) {
	if err := validName("namespace", ns); err != nil {
		return nil, err
	}
	var out []ResourceQuota
	err := c.list(ctx, "list resourcequotas", nsPath(core, ns, "resourcequotas"), "", func(m json.RawMessage) error {
		var q ResourceQuota
		if err := decode("list resourcequotas", m, &q); err != nil {
			return err
		}
		out = append(out, q)
		return nil
	})
	return out, err
}

// ListNetworkPolicies lists the namespace's NetworkPolicies.
func (c *Client) ListNetworkPolicies(ctx context.Context, ns string) ([]NetworkPolicy, error) {
	if err := validName("namespace", ns); err != nil {
		return nil, err
	}
	var out []NetworkPolicy
	err := c.list(ctx, "list networkpolicies", nsPath(netv1, ns, "networkpolicies"), "", func(m json.RawMessage) error {
		var p NetworkPolicy
		if err := decode("list networkpolicies", m, &p); err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	return out, err
}

// Allowed asks the API server whether the user (a ServiceAccount subject, for
// example system:serviceaccount:<ns>:<name>) may perform the verb. It creates a
// SubjectAccessReview, which records nothing and grants nothing.
func (c *Client) Allowed(ctx context.Context, user string, groups []string, attrs ResourceAttributes) (bool, error) {
	if strings.TrimSpace(user) == "" {
		return false, &Error{Op: "access review", Kind: ErrInvalid, Cause: errors.New("a user is required")}
	}
	var r AccessReview
	r.APIVersion, r.Kind = "authorization.k8s.io/v1", "SubjectAccessReview"
	r.Spec.User, r.Spec.Groups, r.Spec.ResourceAttributes = user, groups, &attrs
	var raw []byte
	// A review is not a mutation of cluster state: a lost answer is simply an
	// inconclusive read.
	if err := c.do(ctx, "access review", http.MethodPost, "/apis/authorization.k8s.io/v1/subjectaccessreviews", nil, r, &raw); err != nil {
		if errors.Is(err, ErrUnknownOutcome) {
			return false, &Error{Op: "access review", Kind: ErrUnavailable}
		}
		return false, err
	}
	var out AccessReview
	if err := decode("access review", raw, &out); err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return &Error{Op: "validate", Kind: ErrInvalid, Cause: e}
		}
	}
	return nil
}
