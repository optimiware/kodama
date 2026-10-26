package runprofile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RegClient is a minimal OCI Distribution client used only to seed the base
// image and to verify what a builder pushed. It trusts only the run's CA.
type RegClient struct {
	Base     string // https://127.0.0.1:port
	User     string
	Password string
	HTTP     *http.Client
}

// NewRegClient builds a client that pins the run's CA.
func NewRegClient(base, user, password string, caPEM []byte) (*RegClient, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("registry CA is not valid PEM")
	}
	return &RegClient{Base: base, User: user, Password: password, HTTP: &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}}, nil
}

func (r *RegClient) do(method, url, contentType string, body []byte, accept string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.SetBasicAuth(r.User, r.Password)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	return resp, data, err
}

// Digest returns the sha256 reference of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PutBlob uploads a blob.
func (r *RegClient) PutBlob(repo string, data []byte) (string, error) {
	resp, _, err := r.do("POST", fmt.Sprintf("%s/v2/%s/blobs/uploads/", r.Base, repo), "", nil, "")
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("blob upload start: %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	if strings.HasPrefix(loc, "/") {
		loc = r.Base + loc
	}
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	d := Digest(data)
	resp, _, err = r.do("PUT", loc+sep+"digest="+d, "application/octet-stream", data, "")
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("blob upload finish: %s", resp.Status)
	}
	return d, nil
}

// PutManifest uploads a manifest under a tag.
func (r *RegClient) PutManifest(repo, tag, mediaType string, data []byte) error {
	resp, body, err := r.do("PUT", fmt.Sprintf("%s/v2/%s/manifests/%s", r.Base, repo, tag), mediaType, data, "")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("manifest put: %s %s", resp.Status, tail(string(body), 200))
	}
	return nil
}

// SeedEmptyImage pushes a single-platform image with no layers.
func (r *RegClient) SeedEmptyImage(repo, tag, arch string) error {
	cfg, _ := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{}}})
	cd, err := r.PutBlob(repo, cfg)
	if err != nil {
		return err
	}
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(cfg)}, "layers": []any{}})
	return r.PutManifest(repo, tag, "application/vnd.oci.image.manifest.v1+json", man)
}

// Manifest fetches an image manifest, resolving an index to the first
// linux/amd64 child.
func (r *RegClient) Manifest(repo, ref string) (map[string]any, string, error) {
	accept := "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json"
	resp, data, err := r.do("GET", fmt.Sprintf("%s/v2/%s/manifests/%s", r.Base, repo, ref), "", nil, accept)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("manifest %s:%s: %s", repo, ref, resp.Status)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, "", err
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if kids, ok := m["manifests"].([]any); ok && len(kids) > 0 {
		for _, k := range kids {
			km, _ := k.(map[string]any)
			plat, _ := km["platform"].(map[string]any)
			if plat["architecture"] == "amd64" && plat["os"] == "linux" {
				d, _ := km["digest"].(string)
				return r.Manifest(repo, d)
			}
		}
		return nil, "", fmt.Errorf("index %s:%s has no linux/amd64 manifest", repo, ref)
	}
	return m, digest, nil
}

// TarEntry is one file of a pushed layer with its recorded numeric owner.
type TarEntry struct {
	Name string
	UID  int
	GID  int
	Mode int64
	Size int64
}

// LayerEntries returns every tar entry across an image's layers.
func (r *RegClient) LayerEntries(repo string, manifest map[string]any) ([]TarEntry, error) {
	var out []TarEntry
	layers, _ := manifest["layers"].([]any)
	for _, l := range layers {
		lm, _ := l.(map[string]any)
		d, _ := lm["digest"].(string)
		resp, data, err := r.do("GET", fmt.Sprintf("%s/v2/%s/blobs/%s", r.Base, repo, d), "", nil, "")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("blob %s: %s", d, resp.Status)
		}
		var rd io.Reader = bytes.NewReader(data)
		if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
			gz, err := gzip.NewReader(rd)
			if err != nil {
				return nil, err
			}
			rd = gz
		}
		tr := tar.NewReader(rd)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			out = append(out, TarEntry{Name: strings.TrimPrefix(h.Name, "./"), UID: h.Uid, GID: h.Gid, Mode: h.Mode, Size: h.Size})
		}
	}
	return out, nil
}

// ImageConfig returns the image config blob's decoded form.
func (r *RegClient) ImageConfig(repo string, manifest map[string]any) (map[string]any, error) {
	cm, _ := manifest["config"].(map[string]any)
	d, _ := cm["digest"].(string)
	resp, data, err := r.do("GET", fmt.Sprintf("%s/v2/%s/blobs/%s", r.Base, repo, d), "", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config blob: %s", resp.Status)
	}
	var m map[string]any
	return m, json.Unmarshal(data, &m)
}

// Unauthenticated reports the status of an anonymous catalog request, the
// negative control for registry authentication.
func (r *RegClient) Unauthenticated() (int, error) {
	req, _ := http.NewRequest("GET", r.Base+"/v2/", nil)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
