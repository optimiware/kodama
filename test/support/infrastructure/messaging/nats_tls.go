package messaging

// This helper starts the pinned NATS image configured for mutual TLS from a
// private certificate authority generated in the test process. It mirrors the
// plaintext StartNATS helper for the driver TLS proof: the same shard, image
// digest evidence and readiness barrier apply, but the client port requires a
// verified TLS handshake and a client certificate signed by the generated CA.
//
// TLS material is written to a short-lived host directory bind-mounted read-only
// into the container. Certificates and keys are ephemeral test material and are
// never accessed after the test.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// natsTLSDir is where the generated material is mounted in the container.
const natsTLSDir = "/etc/kodama-tls"

// NATSTLS is one real JetStream-enabled broker that requires verified mutual
// TLS from a generated private CA.
type NATSTLS struct {
	*NATS

	// CAFile is the generated private CA bundle the client must trust.
	CAFile string
	// ClientCertFile and ClientKeyFile are the generated client certificate
	// pair the broker requires.
	ClientCertFile string
	ClientKeyFile  string
	// UntrustedCAFile is a second generated CA that did not sign the server;
	// a client trusting only it must fail.
	UntrustedCAFile string
}

// StartNATSTLS starts the pinned NATS image with JetStream, the monitoring
// endpoint and mutual TLS, waits for the readiness barrier and records image
// evidence.
func StartNATSTLS(t *testing.T) *NATSTLS {
	t.Helper()
	ca := generateTestCA(t, "kodama component CA")
	untrusted := generateTestCA(t, "kodama untrusted component CA")
	server := ca.issue(t, 100, "nats", []string{"nats", "localhost"},
		[]net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	client := ca.issue(t, 101, "kodama-client", nil, nil, x509.ExtKeyUsageClientAuth)

	dir := t.TempDir()
	writeTLSFile(t, dir, "ca.pem", ca.pem)
	writeTLSFile(t, dir, "server-cert.pem", server.certPEM)
	writeTLSFile(t, dir, "server-key.pem", server.keyPEM)
	writeTLSFile(t, dir, "client-cert.pem", client.certPEM)
	writeTLSFile(t, dir, "client-key.pem", client.keyPEM)

	shard := harness.NewShard(t)
	ctx := shard.Context()
	digest := shard.RequireImage(ctx, harness.ImageNATS)
	container, err := shard.Run(ctx, harness.RunSpec{
		Image: harness.ImageNATS,
		Ports: []harness.Port{{Container: NATSClientPort}, {Container: NATSMonitorPort}},
		// A host path volume is a bind mount: the container reads exactly the
		// material generated above and writes nothing back.
		Volumes: []harness.Volume{{Name: dir, Target: natsTLSDir}},
		Cmd: []string{
			"-js", "-m", strconv.Itoa(NATSMonitorPort),
			"--tls",
			"--tlscert", filepath.ToSlash(filepath.Join(natsTLSDir, "server-cert.pem")),
			"--tlskey", filepath.ToSlash(filepath.Join(natsTLSDir, "server-key.pem")),
			"--tlscacert", filepath.ToSlash(filepath.Join(natsTLSDir, "ca.pem")),
			"--tlsverify",
		},
	})
	if err != nil {
		t.Fatalf("start nats with tls: %v", err)
	}
	clientAddr, err := container.Addr(ctx, NATSClientPort)
	if err != nil {
		t.Fatalf("nats client address: %v", err)
	}
	monitorAddr, err := container.Addr(ctx, NATSMonitorPort)
	if err != nil {
		t.Fatalf("nats monitor address: %v", err)
	}
	instance := &NATSTLS{
		NATS: &NATS{
			Shard:      shard,
			Container:  container,
			URL:        "nats://" + clientAddr,
			MonitorURL: "http://" + monitorAddr,
			Digest:     digest,
		},
		CAFile:          filepath.Join(dir, "ca.pem"),
		ClientCertFile:  filepath.Join(dir, "client-cert.pem"),
		ClientKeyFile:   filepath.Join(dir, "client-key.pem"),
		UntrustedCAFile: filepath.Join(dir, "untrusted-ca.pem"),
	}
	writeTLSFile(t, dir, "untrusted-ca.pem", untrusted.pem)
	instance.WaitReady(t)
	instance.Version = instance.ServerVersion(t)
	RecordImage(harness.ImageNATS, digest, "nats-server "+instance.Version+" (mutual TLS, generated private CA)")
	return instance
}

// tlsConfig returns the verified client configuration: the generated CA as the
// only root, and the generated client certificate.
func (instance *NATSTLS) tlsConfig(t *testing.T) *tls.Config {
	t.Helper()
	pemBytes, err := os.ReadFile(instance.CAFile)
	if err != nil {
		t.Fatalf("read generated CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("generated CA is not a usable certificate")
	}
	certificate, err := tls.LoadX509KeyPair(instance.ClientCertFile, instance.ClientKeyFile)
	if err != nil {
		t.Fatalf("load generated client certificate: %v", err)
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}
}

// Raw opens one verified mutual-TLS nats.go connection used only to inspect or
// seed the broker. It is closed at test cleanup.
func (instance *NATSTLS) Raw(t *testing.T) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(instance.URL,
		natsgo.Name("i01c-tls-raw-"+t.Name()),
		natsgo.Secure(instance.tlsConfig(t)),
		natsgo.Timeout(5*time.Second),
	)
	if err != nil {
		t.Fatalf("raw nats tls connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// JetStream returns a JetStream context over a verified TLS connection.
func (instance *NATSTLS) JetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(instance.Raw(t))
	if err != nil {
		t.Fatalf("jetstream over tls: %v", err)
	}
	return js
}

// EnsureStreams creates the events and invalidations streams over verified
// mutual TLS.
func (instance *NATSTLS) EnsureStreams(t *testing.T) {
	t.Helper()
	js := instance.JetStream(t)
	streams := []jetstream.StreamConfig{
		{Name: EventsStream, Subjects: []string{EventsStreamSubjects}, Storage: jetstream.FileStorage},
		{Name: InvalidationsStream, Subjects: []string{InvalidationsStreamSubjects}, Storage: jetstream.FileStorage},
	}
	ctx := instance.Shard.Context()
	for _, config := range streams {
		if _, err := js.CreateOrUpdateStream(ctx, config); err != nil {
			t.Fatalf("create stream %s over tls: %v", config.Name, err)
		}
	}
}

// StreamInfo reads one stream's current state over verified mutual TLS.
func (instance *NATSTLS) StreamInfo(t *testing.T, stream string) *jetstream.StreamInfo {
	t.Helper()
	ctx := instance.Shard.Context()
	handle, err := instance.JetStream(t).Stream(ctx, stream)
	if err != nil {
		t.Fatalf("stream %s over tls: %v", stream, err)
	}
	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatalf("stream %s info over tls: %v", stream, err)
	}
	return info
}

// WaitForStreamMessages is a bounded barrier on the stored message count over
// verified mutual TLS.
func (instance *NATSTLS) WaitForStreamMessages(t *testing.T, stream string, want uint64) *jetstream.StreamInfo {
	t.Helper()
	var last *jetstream.StreamInfo
	probe := func(context.Context) error {
		last = instance.StreamInfo(t, stream)
		if last.State.Msgs != want {
			return fmt.Errorf("stream %s has %d messages; want %d", stream, last.State.Msgs, want)
		}
		return nil
	}
	if err := harness.WaitReady(instance.Shard.Context(), harness.RealClock{}, "stream "+stream, probe, fastWaitOptions()); err != nil {
		t.Fatalf("stream message barrier over tls: %v", err)
	}
	return last
}

// testCA is one generated private certificate authority for the component
// harness.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

type testIdentity struct {
	certPEM []byte
	keyPEM  []byte
}

// generateTestCA creates a private CA valid for the duration of the test run.
func generateTestCA(t *testing.T, commonName string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign |
			x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return &testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue signs one certificate for the given names and extended key usage.
func (ca *testCA) issue(t *testing.T, serial int64, commonName string, dnsNames []string, ips []net.IP, usage x509.ExtKeyUsage) *testIdentity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return &testIdentity{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}

// writeTLSFile writes one generated file read-only to every container user.
// The directory is world-readable so the rootless Podman user mapping can read
// the mount; the material is ephemeral test data.
func writeTLSFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("make tls material directory readable: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
