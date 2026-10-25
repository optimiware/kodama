package stage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/optimiware/kodama/test/support/stage/probe"
)

type CreateFault struct {
	i      *Installation
	client *http.Client
	url    string
}

// InstallCreateFault instruments only this subcase. TLS is verified on both hops: the server
// trusts an ephemeral loopback certificate, and the helper trusts the real cluster CA. The
// original Pod template is restored even if installing or exercising the fault fails.
func (i *Installation) InstallCreateFault(ctx context.Context) *CreateFault {
	i.T.Helper()
	image := i.ProbeImage()
	var deploy struct {
		Spec struct {
			Template json.RawMessage `json:"template"`
		} `json:"spec"`
	}
	if err := i.Kube.JSON(ctx, &deploy, "-n", i.S.Namespace, "get", "deploy", i.Deployment()); err != nil {
		i.T.Fatal(err)
	}
	original := bytes.Clone(deploy.Spec.Template)
	var template map[string]any
	if json.Unmarshal(original, &template) != nil {
		i.T.Fatal("server Pod template invalid")
	}
	secret := "kq-api-tls-" + random(4)
	i.RemoveFixture("secret", secret)
	cert, key, err := LoopbackCertificate()
	if err != nil {
		i.T.Fatal("generate fault TLS identity failed")
	}
	i.ApplyFixture(ctx, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": secret, "namespace": i.S.Namespace}, "data": map[string][]byte{"tls.crt": cert, "tls.key": key, "ca.crt": cert}})
	// Register restoration before modifying the workload; LIFO deletes the TLS Secret afterwards.
	i.T.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
		defer cancel()
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": json.RawMessage(original)}})
		if _, err := i.Kube.Run(c, "-n", i.S.Namespace, "patch", "deploy", i.Deployment(), "--type=merge", "-p", string(patch)); err != nil {
			i.T.Errorf("restore server template: %v", err)
			return
		}
		if _, err := i.Kube.Run(c, "-n", i.S.Namespace, "rollout", "status", "deploy/"+i.Deployment(), "--timeout=6m"); err != nil {
			i.T.Errorf("restore server rollout: %v", err)
		}
	})
	spec := template["spec"].(map[string]any)
	containers := spec["containers"].([]any)
	for _, c := range containers {
		c := c.(map[string]any)
		if c["name"] != "server" {
			continue
		}
		env := c["env"].([]any)
		for _, e := range env {
			name := e.(map[string]any)["name"]
			if name == "KODAMA_KUBE_API" || name == "KODAMA_KUBE_CA_FILE" {
				i.T.Fatal("server already has API fault overrides")
			}
		}
		c["env"] = append(env, map[string]any{"name": "KODAMA_KUBE_API", "value": "https://127.0.0.1:6444"}, map[string]any{"name": "KODAMA_KUBE_CA_FILE", "value": "/fault/ca.crt"})
		c["volumeMounts"] = append(c["volumeMounts"].([]any), map[string]any{"name": "fault-tls", "mountPath": "/fault", "readOnly": true})
	}
	spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "fault-tls", "secret": map[string]any{"secretName": secret, "defaultMode": 0440}})
	spec["containers"] = append(containers, map[string]any{
		"name": "api-fault", "image": image, "args": []string{"-mode=proxy"},
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
		"resources":       map[string]any{"limits": map[string]string{"memory": "128Mi", "cpu": "250m"}},
		"volumeMounts":    []any{map[string]any{"name": "fault-tls", "mountPath": "/fault", "readOnly": true}},
	})
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": template}})
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "patch", "deploy", i.Deployment(), "--type=merge", "-p", string(patch)); err != nil {
		i.T.Fatal(err)
	}
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "rollout", "status", "deploy/"+i.Deployment(), "--timeout=6m"); err != nil {
		i.T.Fatal(err)
	}
	i.WaitReady(ctx)
	pf, err := i.Kube.Forward(ctx, i.S.Namespace, "deploy/"+i.Deployment(), 6444)
	if err != nil {
		i.T.Fatal(err)
	}
	i.T.Cleanup(pf.Close)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	i.T.Cleanup(transport.CloseIdleConnections)
	f := &CreateFault{i: i, client: &http.Client{Timeout: 10 * time.Second, Transport: transport}, url: fmt.Sprintf("https://127.0.0.1:%d", pf.Port)}
	if f.Status().Lost {
		i.T.Fatal("fault proxy started armed")
	}
	i.T.Log("FAULT_CONFIG PX-A02 HTTPS loopback proxy; both TLS hops verified; test-owned only")
	return f
}

func (f *CreateFault) Arm(namespace, name string) {
	f.i.T.Helper()
	data, _ := json.Marshal(probe.Target{Namespace: namespace, Name: name})
	r, err := f.client.Post(f.url+"/__fault/arm", "application/json", bytes.NewReader(data))
	if err != nil {
		f.i.T.Fatal("arm fault proxy: transport failed")
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		f.i.T.Fatalf("arm fault proxy: HTTP %d", r.StatusCode)
	}
}

func (f *CreateFault) Status() probe.FaultStatus {
	f.i.T.Helper()
	r, err := f.client.Get(f.url + "/__fault/status")
	if err != nil {
		f.i.T.Fatal("fault status: transport failed")
	}
	defer r.Body.Close()
	var s probe.FaultStatus
	if r.StatusCode != http.StatusOK || json.NewDecoder(r.Body).Decode(&s) != nil {
		f.i.T.Fatal("fault status invalid")
	}
	return s
}

// LoopbackCertificate returns an ephemeral self-signed CA/leaf usable only for the loopback API.
// Its private key is kept in memory and in a test-owned Kubernetes Secret, never in evidence.
func LoopbackCertificate() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "kodama-qualification-loopback"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(2 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), nil
}
