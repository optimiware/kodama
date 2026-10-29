package runprofile

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

type registryFixture struct {
	host     string // in-cluster address, registry.<ns>.svc.cluster.local:5000
	user     string
	password string
	caPEM    []byte
	client   *RegClient
	stop     func()
	deps     string // in-cluster base URL of the dependency server
}

// freshClient re-opens the port-forward: a long-idle kubectl forward does
// not survive a multi-minute build, so every verification reconnects.
func (q *Qualifier) freshClient(ctx context.Context) (*RegClient, error) {
	f := q.reg
	if f.stop != nil {
		f.stop()
		f.stop = nil
	}
	addr, stop, err := q.c.Forward(ctx, q.nsMain, "registry", 5000)
	if err != nil {
		return nil, err
	}
	f.stop = stop
	f.client, err = NewRegClient("https://"+addr, f.user, f.password, f.caPEM)
	return f.client, err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// issueCerts creates a throwaway CA and a server certificate for the registry.
func issueCerts(ns string) (caPEM, certPEM, keyPEM []byte, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "krp-t111 run CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "registry"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"registry", "registry." + ns, "registry." + ns + ".svc", "registry." + ns + ".svc.cluster.local", "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

func (q *Qualifier) secret(ns, name string, data map[string]string) map[string]any {
	sd := map[string]any{}
	for k, v := range data {
		sd[k] = v
	}
	o := q.c.obj("v1", "Secret", ns, name, nil, nil)
	o["type"] = "Opaque"
	o["stringData"] = sd
	return o
}

// publicEgressExcept are the ranges public egress never reaches: private,
// link-local (cloud metadata), CGNAT and loopback. Cluster ranges sit inside
// them on the supported networks.
var publicEgressExcept = []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "127.0.0.0/8"}

// tenantPolicies renders the O-30 tenant network model (seam S8) for one
// namespace: default deny, DNS, any Pod of the tenant namespace may reach the
// service ports of any other (a run label is not a boundary), and public
// HTTP/HTTPS egress except private, link-local, CGNAT and cluster ranges.
func (q *Qualifier) tenantPolicies(ns string, ports []int) []map[string]any {
	sel := func(kv map[string]any) map[string]any { return map[string]any{"matchLabels": kv} }
	var tp []any
	for _, p := range ports {
		tp = append(tp, map[string]any{"protocol": "TCP", "port": p})
	}
	pol := func(name string, spec map[string]any) map[string]any {
		return q.c.obj("networking.k8s.io/v1", "NetworkPolicy", ns, name, nil, spec)
	}
	all := map[string]any{}
	return []map[string]any{
		pol("default-deny", map[string]any{"podSelector": all, "policyTypes": []any{"Ingress", "Egress"}}),
		pol("dns-egress", map[string]any{"podSelector": all, "policyTypes": []any{"Egress"},
			"egress": []any{map[string]any{
				"to": []any{map[string]any{
					"namespaceSelector": sel(map[string]any{"kubernetes.io/metadata.name": "kube-system"}),
					"podSelector":       sel(map[string]any{"k8s-app": "kube-dns"})}},
				"ports": []any{map[string]any{"protocol": "UDP", "port": 53}, map[string]any{"protocol": "TCP", "port": 53}}}}}),
		pol("tenant-egress", map[string]any{"podSelector": all, "policyTypes": []any{"Egress"},
			"egress": []any{map[string]any{"to": []any{map[string]any{"podSelector": all}}, "ports": tp}}}),
		pol("tenant-ingress", map[string]any{"podSelector": all, "policyTypes": []any{"Ingress"},
			"ingress": []any{map[string]any{"from": []any{map[string]any{"podSelector": all}}, "ports": tp}}}),
		pol("public-egress", map[string]any{"podSelector": all, "policyTypes": []any{"Egress"},
			"egress": []any{map[string]any{"to": []any{map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0", "except": publicEgressExcept}}},
				"ports": []any{map[string]any{"protocol": "TCP", "port": 80}, map[string]any{"protocol": "TCP", "port": 443}}}}}),
	}
}

// networkPolicies applies the main namespace's tenant model.
func (q *Qualifier) networkPolicies(ctx context.Context) error {
	return q.c.ApplyAll(ctx, q.tenantPolicies(q.nsMain, []int{5000, 8080, 5432})...)
}

func (q *Qualifier) service(ns, name string, port int) map[string]any {
	return q.c.obj("v1", "Service", ns, name, nil, map[string]any{
		"type": "ClusterIP", "selector": map[string]any{"krp/svc": name},
		"ports": []any{map[string]any{"name": name, "port": port, "targetPort": port, "protocol": "TCP"}}})
}

// DependencyTree is the controlled dependency server content: a text payload
// for build-time fetches and a one-module GOPROXY.
func DependencyTree(payload string) ([]byte, error) {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for name, body := range map[string]string{
		"example.com/dep@v1.0.0/go.mod": "module example.com/dep\n\ngo 1.22\n",
		"example.com/dep@v1.0.0/dep.go": "package dep\n\n// Payload is fetched through the run's dependency egress.\nconst Payload = \"" + payload + "\"\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(body)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"data.txt":                               []byte(payload + "\n"),
		"goproxy/example.com/dep/@v/list":        []byte("v1.0.0\n"),
		"goproxy/example.com/dep/@v/v1.0.0.info": []byte(`{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}` + "\n"),
		"goproxy/example.com/dep/@v/v1.0.0.mod":  []byte("module example.com/dep\n\ngo 1.22\n"),
		"goproxy/example.com/dep/@v/v1.0.0.zip":  zbuf.Bytes(),
	}
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0), Format: tar.FormatUSTAR}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

const depPayloadPrefix = "dependency-payload-"

// phaseRegistry brings up the fixtures every build needs: policies, the
// authenticated TLS registry, the dependency server, and a seeded base image.
func (q *Qualifier) phaseRegistry(ctx context.Context) error {
	ns := q.nsMain
	if err := q.networkPolicies(ctx); err != nil {
		return err
	}
	caPEM, certPEM, keyPEM, err := issueCerts(ns)
	if err != nil {
		return err
	}
	f := &registryFixture{host: "registry." + ns + ".svc.cluster.local:5000", user: "kodama", password: randomHex(16), caPEM: caPEM,
		deps: "http://deps." + ns + ".svc.cluster.local:8080"}
	q.reg = f
	q.c.K.Rec.AddSecret(f.password)
	q.c.K.Rec.AddSecret(base64.StdEncoding.EncodeToString([]byte(f.user + ":" + f.password)))

	// htpasswd hash, generated in-cluster; the password reaches the Pod only
	// through a Secret reference.
	if err := q.c.ApplyAll(ctx, q.secret(ns, "registry-cred-src", map[string]string{"password": f.password})); err != nil {
		return err
	}
	h := q.base("htpasswd", q.img("httpd"), 10001)
	h.Env = []any{secretEnv("PW", "registry-cred-src", "password")}
	q.c.K.Rec.SetQuiet(true)
	hashOut, code, err := q.oneShot(ctx, ns, h, `htpasswd -nbB kodama "$PW"`, 5*minute)
	q.c.K.Rec.SetQuiet(false)
	if err != nil || code != 0 {
		return fmt.Errorf("htpasswd generation failed (exit %d): %v", code, err)
	}
	hash := strings.TrimSpace(hashOut)
	q.c.K.Rec.AddSecret(hash)
	dockerCfg := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, f.host, base64.StdEncoding.EncodeToString([]byte(f.user+":"+f.password)))
	if err := q.c.ApplyAll(ctx,
		q.secret(ns, "registry-tls", map[string]string{"tls.crt": string(certPEM), "tls.key": string(keyPEM)}),
		q.secret(ns, "registry-auth", map[string]string{"htpasswd": hash}),
		q.secret(ns, "registry-ca", map[string]string{"ca.crt": string(caPEM)}),
		q.secret(ns, "builder-docker-config", map[string]string{"config.json": dockerCfg}),
	); err != nil {
		return err
	}

	reg := q.base("registry", q.img("registry"), 10001)
	reg.Labels = map[string]any{"krp/svc": "registry"}
	reg.FSGroup = 10001
	reg.Command = nil
	reg.Env = []any{plainEnv("REGISTRY_HTTP_ADDR", "0.0.0.0:5000"), plainEnv("REGISTRY_HTTP_TLS_CERTIFICATE", "/tls/tls.crt"),
		plainEnv("REGISTRY_HTTP_TLS_KEY", "/tls/tls.key"), plainEnv("REGISTRY_AUTH", "htpasswd"),
		plainEnv("REGISTRY_AUTH_HTPASSWD_REALM", "run-profile"), plainEnv("REGISTRY_AUTH_HTPASSWD_PATH", "/auth/htpasswd"),
		plainEnv("REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY", "/data"), plainEnv("REGISTRY_STORAGE_DELETE_ENABLED", "true")}
	reg.Volumes = []any{secretVolume("tls", "registry-tls", 0o440), secretVolume("auth", "registry-auth", 0o440), emptyDir("data", "512Mi", false)}
	reg.Mounts = []any{mount("tls", "/tls", true), mount("auth", "/auth", true), mount("data", "/data", false)}
	reg.Readiness = map[string]any{"tcpSocket": map[string]any{"port": 5000}, "periodSeconds": 3}
	reg.Restart = "Never"

	deps := q.base("deps", q.img("httpd"), 10001)
	deps.Labels = map[string]any{"krp/svc": "deps"}
	deps.Command = []string{"sh", "-ec", `while [ ! -f /www/.ready ]; do sleep 1; done; ` + serveScript("/www")}
	deps.Volumes = []any{emptyDir("www", "16Mi", false)}
	deps.Mounts = []any{mount("www", "/www", false)}
	deps.Readiness = map[string]any{"tcpSocket": map[string]any{"port": 8080}, "periodSeconds": 3}

	if err := q.c.ApplyAll(ctx, q.c.PodObject(reg), q.c.PodObject(deps), q.service(ns, "registry", 5000), q.service(ns, "deps", 8080)); err != nil {
		return err
	}
	tree, err := DependencyTree(depPayloadPrefix + q.c.RunID)
	if err != nil {
		return err
	}
	if err := Wait(ctx, 6*minute, 3*time.Second, func() (bool, error) {
		p, err := q.c.GetPod(ctx, ns, "deps")
		if err != nil {
			return false, err
		}
		return p.Status.Phase == "Running", fmt.Errorf("deps phase %s", p.Status.Phase)
	}); err != nil {
		return err
	}
	if _, err := q.c.Exec(ctx, ns, "deps", bytes.NewReader(tree), "tar -C /www -xf - && touch /www/.ready"); err != nil {
		return err
	}
	if err := q.c.WaitReady(ctx, ns, "registry", 6*minute); err != nil {
		return err
	}
	if err := q.c.WaitReady(ctx, ns, "deps", 3*minute); err != nil {
		return err
	}
	addr, stop, err := q.c.Forward(ctx, ns, "registry", 5000)
	if err != nil {
		return err
	}
	f.stop = stop
	f.client, err = NewRegClient("https://"+addr, f.user, f.password, caPEM)
	if err != nil {
		return err
	}
	anon, err := f.client.Unauthenticated()
	if err != nil {
		return err
	}
	if anon != 401 {
		return fmt.Errorf("registry accepted an unauthenticated request (status %d)", anon)
	}
	if err := f.client.SeedEmptyImage("base", "empty", "amd64"); err != nil {
		return fmt.Errorf("seeding base image: %w", err)
	}
	q.add("fixture.registry", "authenticated TLS registry, dependency server and base image", true, Pass,
		"registry (TLS, htpasswd) returned 401 anonymously, accepted the run credential, and holds the empty base image; dependency server serves payload and a GOPROXY module",
		[]string{"registry=" + q.pins.Images["registry"].Ref, "endpoint=" + f.host, "deps=" + f.deps}, "")
	return nil
}
