package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
)

// The operations below serve the run-resource adapter: claims, volumes (read
// only), volume attachments (read only), Services, Pods and NetworkPolicies.
// Like every other operation they never follow a mutation with an assumption:
// a lost answer is ErrUnknownOutcome and the caller reads the object back.

const (
	storagev1 = "/apis/storage.k8s.io/v1"
)

// ---- PersistentVolumeClaims ----

// GetPVC reads a claim. The returned claim keeps the server's raw document.
func (c *Client) GetPVC(ctx context.Context, ns, name string) (PersistentVolumeClaim, error) {
	if err := firstErr(validName("namespace", ns), validName("claim", name)); err != nil {
		return PersistentVolumeClaim{}, err
	}
	raw, err := c.get(ctx, "get pvc", nsPath(core, ns, "persistentvolumeclaims")+"/"+name)
	if err != nil {
		return PersistentVolumeClaim{}, err
	}
	return decodePVC("get pvc", raw)
}

func decodePVC(op string, raw []byte) (PersistentVolumeClaim, error) {
	var p PersistentVolumeClaim
	if err := decode(op, raw, &p); err != nil {
		return PersistentVolumeClaim{}, err
	}
	p.Raw = raw
	return p, nil
}

// CreatePVC creates a claim.
func (c *Client) CreatePVC(ctx context.Context, ns string, p PersistentVolumeClaim) (PersistentVolumeClaim, error) {
	if err := firstErr(validName("namespace", ns), validName("claim", p.Metadata.Name)); err != nil {
		return PersistentVolumeClaim{}, err
	}
	p.APIVersion, p.Kind = "v1", "PersistentVolumeClaim"
	p.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "create pvc", http.MethodPost, nsPath(core, ns, "persistentvolumeclaims"), nil, p, &raw); err != nil {
		return PersistentVolumeClaim{}, err
	}
	return decodePVC("create pvc", raw)
}

// DeletePVC deletes a claim under a UID precondition.
func (c *Client) DeletePVC(ctx context.Context, ns, name string, o DeleteOptions) error {
	if o.Propagation == "" {
		o.Propagation = "Background"
	}
	return c.deleteNamed(ctx, "delete pvc", nsPath(core, ns, "persistentvolumeclaims"), ns, name, o)
}

// ---- PersistentVolumes and VolumeAttachments (read only) ----

// GetPV reads a persistent volume (cluster scoped).
func (c *Client) GetPV(ctx context.Context, name string) (PersistentVolume, error) {
	if err := validName("volume", name); err != nil {
		return PersistentVolume{}, firstErr(err)
	}
	raw, err := c.get(ctx, "get pv", core+"/persistentvolumes/"+name)
	if err != nil {
		return PersistentVolume{}, err
	}
	var v PersistentVolume
	return v, decode("get pv", raw, &v)
}

// ListPVs lists every persistent volume of the cluster (cluster scoped). A caller
// filters by claimRef: a volume bound to a claim is found by the claim's identity,
// whatever its name.
func (c *Client) ListPVs(ctx context.Context) ([]PersistentVolume, error) {
	var out []PersistentVolume
	err := c.list(ctx, "list persistentvolumes", core+"/persistentvolumes", "", func(m json.RawMessage) error {
		var v PersistentVolume
		if err := decode("list persistentvolumes", m, &v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	return out, err
}

// GetStorageClass reads a storage class (cluster scoped).
func (c *Client) GetStorageClass(ctx context.Context, name string) (StorageClass, error) {
	if err := validName("storageclass", name); err != nil {
		return StorageClass{}, firstErr(err)
	}
	raw, err := c.get(ctx, "get storageclass", storagev1+"/storageclasses/"+name)
	if err != nil {
		return StorageClass{}, err
	}
	var sc StorageClass
	return sc, decode("get storageclass", raw, &sc)
}

// ListVolumeAttachments lists every volume attachment of the cluster. A caller
// filters by volume name: an attachment of a volume means a node may still use it.
func (c *Client) ListVolumeAttachments(ctx context.Context) ([]VolumeAttachment, error) {
	var out []VolumeAttachment
	err := c.list(ctx, "list volumeattachments", storagev1+"/volumeattachments", "", func(m json.RawMessage) error {
		var v VolumeAttachment
		if err := decode("list volumeattachments", m, &v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	return out, err
}

// ---- Services ----

// GetService reads a Service.
func (c *Client) GetService(ctx context.Context, ns, name string) (Service, error) {
	if err := firstErr(validName("namespace", ns), validName("service", name)); err != nil {
		return Service{}, err
	}
	raw, err := c.get(ctx, "get service", nsPath(core, ns, "services")+"/"+name)
	if err != nil {
		return Service{}, err
	}
	return decodeService("get service", raw)
}

func decodeService(op string, raw []byte) (Service, error) {
	var s Service
	if err := decode(op, raw, &s); err != nil {
		return Service{}, err
	}
	s.Raw = raw
	return s, nil
}

// CreateService creates a Service.
func (c *Client) CreateService(ctx context.Context, ns string, s Service) (Service, error) {
	if err := firstErr(validName("namespace", ns), validName("service", s.Metadata.Name)); err != nil {
		return Service{}, err
	}
	s.APIVersion, s.Kind = "v1", "Service"
	s.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "create service", http.MethodPost, nsPath(core, ns, "services"), nil, s, &raw); err != nil {
		return Service{}, err
	}
	return decodeService("create service", raw)
}

// DeleteService deletes a Service under a UID precondition.
func (c *Client) DeleteService(ctx context.Context, ns, name string, o DeleteOptions) error {
	if o.Propagation == "" {
		o.Propagation = "Background"
	}
	return c.deleteNamed(ctx, "delete service", nsPath(core, ns, "services"), ns, name, o)
}

// ---- Pods (create) ----

// CreatePod creates a Pod. ErrAlreadyExists means the name is taken; an
// ErrUnknownOutcome means the Pod may exist.
func (c *Client) CreatePod(ctx context.Context, ns string, p Pod) (Pod, error) {
	if err := firstErr(validName("namespace", ns), validName("pod", p.Metadata.Name)); err != nil {
		return Pod{}, err
	}
	p.APIVersion, p.Kind = "v1", "Pod"
	p.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "create pod", http.MethodPost, nsPath(core, ns, "pods"), nil, p, &raw); err != nil {
		return Pod{}, err
	}
	var out Pod
	if err := decode("create pod", raw, &out); err != nil {
		return Pod{}, err
	}
	out.Raw = raw
	return out, nil
}

// ---- NetworkPolicies ----

// GetNetworkPolicy reads one NetworkPolicy; the result keeps its raw document.
func (c *Client) GetNetworkPolicy(ctx context.Context, ns, name string) (NetworkPolicy, error) {
	if err := firstErr(validName("namespace", ns), validName("networkpolicy", name)); err != nil {
		return NetworkPolicy{}, err
	}
	raw, err := c.get(ctx, "get networkpolicy", nsPath(netv1, ns, "networkpolicies")+"/"+name)
	if err != nil {
		return NetworkPolicy{}, err
	}
	return decodePolicy("get networkpolicy", raw)
}

func decodePolicy(op string, raw []byte) (NetworkPolicy, error) {
	var p NetworkPolicy
	if err := decode(op, raw, &p); err != nil {
		return NetworkPolicy{}, err
	}
	p.Raw = raw
	return p, nil
}

// SelectNetworkPolicies lists the namespace's NetworkPolicies matching a label
// selector; each keeps its raw document.
func (c *Client) SelectNetworkPolicies(ctx context.Context, ns, selector string) ([]NetworkPolicy, error) {
	if err := validName("namespace", ns); err != nil {
		return nil, err
	}
	var out []NetworkPolicy
	err := c.list(ctx, "list networkpolicies", nsPath(netv1, ns, "networkpolicies"), selector, func(m json.RawMessage) error {
		p, err := decodePolicy("list networkpolicies", m)
		out = append(out, p)
		return err
	})
	return out, err
}

// CreateNetworkPolicy creates a NetworkPolicy.
func (c *Client) CreateNetworkPolicy(ctx context.Context, ns string, p NetworkPolicy) (NetworkPolicy, error) {
	if err := firstErr(validName("namespace", ns), validName("networkpolicy", p.Metadata.Name)); err != nil {
		return NetworkPolicy{}, err
	}
	p.APIVersion, p.Kind = "networking.k8s.io/v1", "NetworkPolicy"
	p.Metadata.Namespace = ns
	var raw []byte
	if err := c.do(ctx, "create networkpolicy", http.MethodPost, nsPath(netv1, ns, "networkpolicies"), nil, p, &raw); err != nil {
		return NetworkPolicy{}, err
	}
	return decodePolicy("create networkpolicy", raw)
}

// DeleteNetworkPolicy deletes a NetworkPolicy under a UID precondition.
func (c *Client) DeleteNetworkPolicy(ctx context.Context, ns, name string, o DeleteOptions) error {
	if o.Propagation == "" {
		o.Propagation = "Background"
	}
	return c.deleteNamed(ctx, "delete networkpolicy", nsPath(netv1, ns, "networkpolicies"), ns, name, o)
}
