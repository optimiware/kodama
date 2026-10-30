package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// The operations below serve the tenant Git hosting adapter (T-129): the
// Kodama-managed service namespace, its token-less ServiceAccount, the worker
// Deployment and the NetworkPolicies. Like every other operation they never
// follow a mutation with an assumption: a lost answer is ErrUnknownOutcome and
// the caller reads the object back. Every replace and delete carries a UID
// precondition, so an object that was swapped for another one is never changed.

const appsv1 = "/apis/apps/v1"

// ---- Namespaces ----

type namespaceWire struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
}

// CreateNamespace creates a Namespace (cluster scoped). ErrAlreadyExists means
// the name is taken; ErrUnknownOutcome means the Namespace may exist.
func (c *Client) CreateNamespace(ctx context.Context, n Namespace) (Namespace, error) {
	if err := validName("namespace", n.Metadata.Name); err != nil {
		return Namespace{}, firstErr(err)
	}
	n.Metadata.Namespace = ""
	var raw []byte
	body := namespaceWire{APIVersion: "v1", Kind: "Namespace", Metadata: n.Metadata}
	if err := c.do(ctx, "create namespace", http.MethodPost, core+"/namespaces", nil, body, &raw); err != nil {
		return Namespace{}, err
	}
	var out Namespace
	return out, decode("create namespace", raw, &out)
}

// DeleteNamespace deletes a Namespace and everything in it under a UID
// precondition. It is the one operation that removes a tenant's volume claim and
// is called only by the explicit offboarding path.
func (c *Client) DeleteNamespace(ctx context.Context, name string, o DeleteOptions) error {
	if err := validName("namespace", name); err != nil {
		return firstErr(err)
	}
	body, err := o.body()
	if err != nil {
		return err
	}
	if o.Propagation == "" {
		body["propagationPolicy"] = "Background"
	}
	return c.do(ctx, "delete namespace", http.MethodDelete, core+"/namespaces/"+name, nil, body, nil)
}

// ---- ServiceAccounts ----

type serviceAccountWire struct {
	APIVersion                   string     `json:"apiVersion"`
	Kind                         string     `json:"kind"`
	Metadata                     ObjectMeta `json:"metadata"`
	AutomountServiceAccountToken *bool      `json:"automountServiceAccountToken,omitempty"`
}

// CreateServiceAccount creates a ServiceAccount. The caller sets
// automountServiceAccountToken explicitly.
func (c *Client) CreateServiceAccount(ctx context.Context, ns string, s ServiceAccount) (ServiceAccount, error) {
	if err := firstErr(validName("namespace", ns), validName("serviceaccount", s.Metadata.Name)); err != nil {
		return ServiceAccount{}, err
	}
	s.Metadata.Namespace = ns
	body := serviceAccountWire{APIVersion: "v1", Kind: "ServiceAccount", Metadata: s.Metadata, AutomountServiceAccountToken: s.AutomountServiceAccountToken}
	var raw []byte
	if err := c.do(ctx, "create serviceaccount", http.MethodPost, nsPath(core, ns, "serviceaccounts"), nil, body, &raw); err != nil {
		return ServiceAccount{}, err
	}
	var out ServiceAccount
	return out, decode("create serviceaccount", raw, &out)
}

// ---- Deployments ----

// GetDeployment reads a Deployment. The result keeps the server's raw document
// and its metadata.generation.
func (c *Client) GetDeployment(ctx context.Context, ns, name string) (Deployment, error) {
	if err := firstErr(validName("namespace", ns), validName("deployment", name)); err != nil {
		return Deployment{}, err
	}
	raw, err := c.get(ctx, "get deployment", nsPath(appsv1, ns, "deployments")+"/"+name)
	if err != nil {
		return Deployment{}, err
	}
	return decodeDeployment("get deployment", raw)
}

func decodeDeployment(op string, raw []byte) (Deployment, error) {
	var d Deployment
	if err := decode(op, raw, &d); err != nil {
		return Deployment{}, err
	}
	var gen struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
	}
	if err := decode(op, raw, &gen); err != nil {
		return Deployment{}, err
	}
	d.Generation = gen.Metadata.Generation
	d.Raw = raw
	return d, nil
}

// CreateDeployment creates a Deployment. ErrAlreadyExists means the name is
// taken; ErrUnknownOutcome means the Deployment may exist.
func (c *Client) CreateDeployment(ctx context.Context, ns string, d Deployment) (Deployment, error) {
	if err := firstErr(validName("namespace", ns), validName("deployment", d.Metadata.Name)); err != nil {
		return Deployment{}, err
	}
	d.APIVersion, d.Kind = "apps/v1", "Deployment"
	d.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "create deployment", http.MethodPost, nsPath(appsv1, ns, "deployments"), nil, d, &raw); err != nil {
		return Deployment{}, err
	}
	return decodeDeployment("create deployment", raw)
}

// ReplaceDeployment replaces a Deployment. The object's UID and resourceVersion
// are both required and both are preconditions: a Deployment replaced by another
// one, or changed since it was read, fails with ErrConflict.
func (c *Client) ReplaceDeployment(ctx context.Context, ns string, d Deployment) (Deployment, error) {
	if err := firstErr(validName("namespace", ns), validName("deployment", d.Metadata.Name)); err != nil {
		return Deployment{}, err
	}
	if d.Metadata.UID == "" || d.Metadata.ResourceVersion == "" {
		return Deployment{}, &Error{Op: "replace deployment", Kind: ErrInvalid, Cause: errors.New("a UID and resourceVersion precondition is required")}
	}
	d.APIVersion, d.Kind = "apps/v1", "Deployment"
	d.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "replace deployment", http.MethodPut, nsPath(appsv1, ns, "deployments")+"/"+d.Metadata.Name, nil, d, &raw); err != nil {
		return Deployment{}, err
	}
	return decodeDeployment("replace deployment", raw)
}

// ListReplicaSets lists the ReplicaSets of a namespace matching a label selector.
func (c *Client) ListReplicaSets(ctx context.Context, ns, selector string) ([]ReplicaSet, error) {
	if err := validName("namespace", ns); err != nil {
		return nil, firstErr(err)
	}
	var out []ReplicaSet
	err := c.list(ctx, "list replicasets", nsPath(appsv1, ns, "replicasets"), selector, func(m json.RawMessage) error {
		var r ReplicaSet
		if err := decode("list replicasets", m, &r); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// ---- NetworkPolicies (replace) ----

// ReplaceNetworkPolicy replaces a NetworkPolicy. The object's UID and
// resourceVersion are both required and both are preconditions.
func (c *Client) ReplaceNetworkPolicy(ctx context.Context, ns string, p NetworkPolicy) (NetworkPolicy, error) {
	if err := firstErr(validName("namespace", ns), validName("networkpolicy", p.Metadata.Name)); err != nil {
		return NetworkPolicy{}, err
	}
	if p.Metadata.UID == "" || p.Metadata.ResourceVersion == "" {
		return NetworkPolicy{}, &Error{Op: "replace networkpolicy", Kind: ErrInvalid, Cause: errors.New("a UID and resourceVersion precondition is required")}
	}
	p.APIVersion, p.Kind = "networking.k8s.io/v1", "NetworkPolicy"
	p.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "replace networkpolicy", http.MethodPut, nsPath(netv1, ns, "networkpolicies")+"/"+p.Metadata.Name, nil, p, &raw); err != nil {
		return NetworkPolicy{}, err
	}
	return decodePolicy("replace networkpolicy", raw)
}

// ReplaceNamespace replaces a Namespace's metadata (labels and annotations). The
// object's UID and resourceVersion are both required and both are preconditions.
// The server keeps spec.finalizers, which only the finalize operation changes.
func (c *Client) ReplaceNamespace(ctx context.Context, n Namespace) (Namespace, error) {
	if err := validName("namespace", n.Metadata.Name); err != nil {
		return Namespace{}, firstErr(err)
	}
	if n.Metadata.UID == "" || n.Metadata.ResourceVersion == "" {
		return Namespace{}, &Error{Op: "replace namespace", Kind: ErrInvalid, Cause: errors.New("a UID and resourceVersion precondition is required")}
	}
	n.Metadata.Namespace = ""
	body := namespaceWire{APIVersion: "v1", Kind: "Namespace", Metadata: n.Metadata}
	var raw []byte
	if err := c.do(ctx, "replace namespace", http.MethodPut, core+"/namespaces/"+n.Metadata.Name, nil, body, &raw); err != nil {
		return Namespace{}, err
	}
	var out Namespace
	return out, decode("replace namespace", raw, &out)
}
