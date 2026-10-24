// probe exercises real dependency protocols from a Kubernetes Pod. It is not
// linked into the Kodama server and carries only development fixture authority.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var client = &http.Client{Timeout: 30 * time.Second}

const manifestTypes = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"

func secret(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join("/secrets", name))
	return strings.TrimSpace(string(data)), err
}

func main() {
	if err := checkAll(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func checkAll() error {
	// KODAMA_PROBE_CHECKS names a comma-separated subset; the cluster stack
	// runs everything, the services stack has no server Deployment to check.
	selected := map[string]bool{}
	for _, name := range strings.Split(os.Getenv("KODAMA_PROBE_CHECKS"), ",") {
		if name != "" {
			selected[name] = true
		}
	}
	enabled := func(name string) bool { return len(selected) == 0 || selected[name] }
	required := []string{"KODAMA_TEST_SCOPE", "KODAMA_NATS_URL", "KODAMA_S3_ENDPOINT", "KODAMA_S3_BUCKET", "KODAMA_S3_PREFIX",
		"KODAMA_GIT_URL", "KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_REGISTRY_URL", "KODAMA_REGISTRY_SOURCE"}
	if enabled("server") {
		required = append(required, "KODAMA_SERVER_URL")
	}
	for _, name := range required {
		if os.Getenv(name) == "" {
			return fmt.Errorf("%s is not set", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := make([]byte, 8)
	if _, err := rand.Read(runID); err != nil {
		return err
	}
	id := hex.EncodeToString(runID)
	checks := []struct {
		name string
		run  func() error
	}{
		{"server", func() error { return server(ctx) }},
		{"postgres", func() error { return postgres(ctx, id) }},
		{"jetstream", func() error { return streaming(ctx, id) }},
		{"s3", func() error { return objectStore(ctx, id) }},
		{"https_git", func() error { return git(ctx, id) }},
		{"oidc_pkce", func() error { return oidc(ctx, id) }},
		{"registry", func() error { return registry(ctx, id) }},
	}
	known := map[string]bool{}
	for _, check := range checks {
		known[check.name] = true
	}
	for name := range selected {
		if !known[name] {
			return fmt.Errorf("unknown probe check %q", name)
		}
	}
	for _, check := range checks {
		if !enabled(check.name) {
			continue
		}
		if err := eventually(ctx, check.run); err != nil {
			return fmt.Errorf("%s: %w", check.name, err)
		}
		fmt.Printf("PASS %s\n", check.name)
	}
	return nil
}

// eventually retries while dependencies are still starting. Every check cleans
// up after itself and asserts specific failures, so a retry cannot hide a defect.
func eventually(ctx context.Context, check func() error) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := check()
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(3 * time.Second)
	}
}

type statusError struct {
	method, path, body string
	code               int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.method, e.path, e.code, e.body)
}

func expectStatus(err error, want int, what string) error {
	var status *statusError
	if !errors.As(err, &status) || status.code != want {
		return fmt.Errorf("%s: want HTTP %d, got %v", what, want, err)
	}
	return nil
}

// request returns *statusError for non-2xx responses.
func request(ctx context.Context, method, address, contentType string, body []byte) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, &statusError{method, req.URL.Path, strings.TrimSpace(string(data[:min(len(data), 512)])), resp.StatusCode}
	}
	return data, resp.Header, nil
}

func server(ctx context.Context) error {
	for _, probe := range [][2]string{{"/healthz", "ok"}, {"/readyz", "ready"}} {
		data, _, err := request(ctx, "GET", os.Getenv("KODAMA_SERVER_URL")+probe[0], "", nil)
		if err != nil {
			return err
		}
		var result struct{ Status string }
		if err := json.Unmarshal(data, &result); err != nil {
			return err
		}
		if result.Status != probe[1] {
			return fmt.Errorf("%s status %q", probe[0], result.Status)
		}
	}
	return nil
}

func connect(ctx context.Context, name string) (*pgx.Conn, error) {
	address, err := secret(name)
	if err != nil {
		return nil, err
	}
	return pgx.Connect(ctx, address)
}

func postgres(ctx context.Context, id string) error {
	owner, err := connect(ctx, "database-migration-url")
	if err != nil {
		return err
	}
	defer owner.Close(context.Background())
	runtimeConn, err := connect(ctx, "database-runtime-url")
	if err != nil {
		return err
	}
	defer runtimeConn.Close(context.Background())
	table := pgx.Identifier{"smoke_" + id}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE TABLE "+table+" (value text NOT NULL)"); err != nil {
		return err
	}
	defer owner.Exec(context.Background(), "DROP TABLE "+table)
	if _, err := owner.Exec(ctx, "GRANT SELECT, INSERT ON "+table+" TO kodama_runtime"); err != nil {
		return err
	}
	if _, err := runtimeConn.Exec(ctx, "INSERT INTO "+table+" VALUES ($1)", id); err != nil {
		return err
	}
	var value string
	if err := runtimeConn.QueryRow(ctx, "SELECT value FROM "+table).Scan(&value); err != nil {
		return err
	}
	if value != id {
		return fmt.Errorf("database round trip mismatch")
	}
	var ownerUser, runtimeUser string
	var privileged bool
	if err := owner.QueryRow(ctx, "SELECT current_user").Scan(&ownerUser); err != nil {
		return err
	}
	if err := runtimeConn.QueryRow(ctx, "SELECT current_user, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&runtimeUser, &privileged); err != nil {
		return err
	}
	if ownerUser == runtimeUser || privileged {
		return fmt.Errorf("runtime identity %q must differ from migration identity %q and be unprivileged", runtimeUser, ownerUser)
	}
	_, err = runtimeConn.Exec(ctx, "CREATE TABLE "+pgx.Identifier{"smoke_" + id + "_denied"}.Sanitize()+" ()")
	var denied *pgconn.PgError
	if !errors.As(err, &denied) || denied.Code != "42501" {
		return fmt.Errorf("runtime role must not create tables: %v", err)
	}
	return nil
}

func streaming(ctx context.Context, id string) error {
	token, err := secret("nats-token")
	if err != nil {
		return err
	}
	nc, err := nats.Connect(os.Getenv("KODAMA_NATS_URL"), nats.Token(token), nats.Timeout(10*time.Second), nats.NoReconnect())
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	name := os.Getenv("KODAMA_TEST_SCOPE") + "-" + id
	subject := "smoke." + id
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: []string{subject}, Storage: jetstream.FileStorage, MaxAge: time.Hour})
	if err != nil {
		return err
	}
	defer js.DeleteStream(context.Background(), name)
	ack, err := js.Publish(ctx, subject, []byte(id))
	if err != nil {
		return err
	}
	if ack.Stream != name {
		return fmt.Errorf("published to stream %q, want %q", ack.Stream, name)
	}
	consumer, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Durable: "durable-" + id, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		return err
	}
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		return err
	}
	var messages []jetstream.Msg
	for message := range batch.Messages() {
		messages = append(messages, message)
	}
	if err := batch.Error(); err != nil {
		return err
	}
	if len(messages) != 1 || string(messages[0].Data()) != id {
		return fmt.Errorf("JetStream payload mismatch")
	}
	return messages[0].DoubleAck(ctx)
}

func objectStore(ctx context.Context, id string) error {
	access, err := secret("s3-access-key")
	if err != nil {
		return err
	}
	key, err := secret("s3-secret-key")
	if err != nil {
		return err
	}
	c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(os.Getenv("KODAMA_S3_ENDPOINT")), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider(access, key, "")})
	bucket := os.Getenv("KODAMA_S3_BUCKET")
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); err != nil {
		var missing *types.NotFound
		if !errors.As(err, &missing) {
			return err
		}
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
			return err
		}
	}
	object := os.Getenv("KODAMA_S3_PREFIX") + id
	payload := []byte("scaffold " + id)
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &object, Body: bytes.NewReader(payload)}); err != nil {
		return err
	}
	defer c.DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: &bucket, Key: &object})
	result, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &object})
	if err != nil {
		return err
	}
	defer result.Body.Close()
	actual, err := io.ReadAll(result.Body)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, actual) {
		return fmt.Errorf("S3 content mismatch")
	}
	return nil
}

func git(ctx context.Context, id string) error {
	readToken, err := secret("git-read-token")
	if err != nil {
		return err
	}
	writeToken, err := secret("git-write-token")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "git-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	run := func(ctx context.Context, token string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSL_CAINFO=/secrets/ca.crt", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("fixture:"+token)))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return string(out), nil
	}
	must := func(token string, args ...string) error {
		_, err := run(ctx, token, args...)
		return err
	}
	repo := os.Getenv("KODAMA_GIT_URL")
	branch := "smoke/" + id
	if err := must(writeToken, "clone", repo, "writer"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "writer", "smoke.txt"), []byte(id), 0o600); err != nil {
		return err
	}
	// Incompressible data above Git's 1 MiB http.postBuffer forces a chunked
	// push, which the fixture must support for real repositories.
	large := make([]byte, 2<<20)
	if _, err := rand.Read(large); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "writer", "large.bin"), large, 0o600); err != nil {
		return err
	}
	if err := must(writeToken, "-C", "writer", "add", "smoke.txt", "large.bin"); err != nil {
		return err
	}
	if err := must(writeToken, "-C", "writer", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "smoke"); err != nil {
		return err
	}
	if err := must(writeToken, "-C", "writer", "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return err
	}
	defer run(context.Background(), writeToken, "-C", "writer", "push", "origin", ":refs/heads/"+branch)
	if err := must(readToken, "clone", "--branch", branch, repo, "reader"); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "reader", "smoke.txt"))
	if err != nil {
		return err
	}
	if string(data) != id {
		return fmt.Errorf("Git clone content mismatch")
	}
	if clone, err := os.ReadFile(filepath.Join(dir, "reader", "large.bin")); err != nil || !bytes.Equal(clone, large) {
		return fmt.Errorf("Git large object mismatch: %v", err)
	}
	out, err := run(ctx, readToken, "-C", "reader", "push", "origin", "HEAD:refs/heads/denied-"+id)
	if err == nil || !strings.Contains(out, "403") {
		return fmt.Errorf("read-only credential push must fail with HTTP 403, got: %v", err)
	}
	out, err = run(ctx, "invalid-token", "ls-remote", repo)
	if err == nil || !(strings.Contains(out, "401") || strings.Contains(out, "Authentication failed") || strings.Contains(out, "terminal prompts disabled")) {
		return fmt.Errorf("invalid token must be rejected as unauthenticated, got: %v", err)
	}
	return nil
}

func oidc(ctx context.Context, id string) error {
	issuer, audience := os.Getenv("KODAMA_OIDC_ISSUER"), os.Getenv("KODAMA_OIDC_AUDIENCE")
	data, _, err := request(ctx, "GET", issuer+"/.well-known/openid-configuration", "", nil)
	if err != nil {
		return err
	}
	var discovery struct {
		Issuer        string
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
		JWKS          string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(data, &discovery); err != nil {
		return err
	}
	if discovery.Issuer != issuer {
		return fmt.Errorf("issuer mismatch")
	}
	// All fixture endpoints must stay at the configured issuer origin.
	for _, endpoint := range []string{discovery.Authorization, discovery.Token, discovery.JWKS} {
		if !strings.HasPrefix(endpoint, issuer+"/") {
			return fmt.Errorf("unexpected discovery endpoint")
		}
	}
	verifier := strings.Repeat(id, 4)
	challenge := sha256.Sum256([]byte(verifier))
	redirect := "http://127.0.0.1:18999/callback"
	query := url.Values{"client_id": {"kodama-cli"}, "response_type": {"code"}, "scope": {"openid profile email"}, "redirect_uri": {redirect}, "state": {id}, "nonce": {id}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "login_hint": {"engineer"}}
	noRedirect := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authorize := func() (string, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", discovery.Authorization+"?"+query.Encode(), nil)
		if err != nil {
			return "", err
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		location, err := resp.Location()
		if err != nil {
			return "", fmt.Errorf("authorization returned HTTP %d without a redirect: %w", resp.StatusCode, err)
		}
		callback := location.Scheme + "://" + location.Host + location.Path
		code := location.Query().Get("code")
		if (resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther) || callback != redirect || location.Query().Get("state") != id || code == "" {
			return "", fmt.Errorf("authorization callback mismatch (HTTP %d, error=%s, description=%s)", resp.StatusCode, location.Query().Get("error"), location.Query().Get("error_description"))
		}
		return code, nil
	}
	exchange := func(code, codeVerifier string) ([]byte, error) {
		form := url.Values{"grant_type": {"authorization_code"}, "client_id": {"kodama-cli"}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {codeVerifier}}
		data, _, err := request(ctx, "POST", discovery.Token, "application/x-www-form-urlencoded", []byte(form.Encode()))
		return data, err
	}
	code, err := authorize()
	if err != nil {
		return err
	}
	data, err = exchange(code, verifier)
	if err != nil {
		return err
	}
	var tokens struct {
		Access string `json:"access_token"`
		ID     string `json:"id_token"`
	}
	if err := json.Unmarshal(data, &tokens); err != nil {
		return err
	}
	if tokens.Access == "" || tokens.ID == "" {
		return fmt.Errorf("token response lacks access_token or id_token")
	}
	data, _, err = request(ctx, "GET", discovery.JWKS, "", nil)
	if err != nil {
		return err
	}
	var jwks struct {
		Keys []struct{ Kty, Kid, N, E string }
	}
	if err := json.Unmarshal(data, &jwks); err != nil {
		return err
	}
	keys := func(t *jwt.Token) (any, error) {
		for _, key := range jwks.Keys {
			if key.Kty != "RSA" || key.Kid != t.Header["kid"] {
				continue
			}
			n, err := base64.RawURLEncoding.DecodeString(key.N)
			if err != nil {
				return nil, err
			}
			e, err := base64.RawURLEncoding.DecodeString(key.E)
			if err != nil {
				return nil, err
			}
			return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
		}
		return nil, fmt.Errorf("signing key not found")
	}
	common := []jwt.ParserOption{jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(issuer), jwt.WithExpirationRequired()}
	token, err := jwt.Parse(tokens.Access, keys, append(common[:3:3], jwt.WithAudience(audience))...)
	if err != nil {
		return err
	}
	if subject, err := token.Claims.GetSubject(); err != nil || subject != "oauthsonas|engineer" {
		return fmt.Errorf("access token subject mismatch")
	}
	// The ID token is for the client, not the API: it must verify for the
	// client audience and carry our nonce, and be rejected for the API audience.
	if _, err := jwt.Parse(tokens.ID, keys, append(common[:3:3], jwt.WithAudience(audience))...); !errors.Is(err, jwt.ErrTokenInvalidAudience) {
		return fmt.Errorf("ID token must be rejected for the API audience: %v", err)
	}
	idToken, err := jwt.Parse(tokens.ID, keys, append(common[:3:3], jwt.WithAudience("kodama-cli"))...)
	if err != nil {
		return err
	}
	if claims, ok := idToken.Claims.(jwt.MapClaims); !ok || claims["nonce"] != id {
		return fmt.Errorf("ID token nonce mismatch")
	}
	_, err = exchange(code, verifier)
	if err := expectStatus(err, http.StatusBadRequest, "authorization code replay"); err != nil {
		return err
	}
	// A fresh code proves the server binds it to the PKCE challenge.
	code, err = authorize()
	if err != nil {
		return err
	}
	_, err = exchange(code, strings.Repeat("x", 64))
	return expectStatus(err, http.StatusBadRequest, "wrong PKCE verifier")
}

func getManifest(ctx context.Context, base, repository, reference string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v2/"+repository+"/manifests/"+reference, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", manifestTypes)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("manifest %s:%s: %s", repository, reference, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return data, resp.Header.Get("Content-Type"), err
}

// Copy an image through the registry's HTTP API from this Pod, then report its
// digest so the host can prove that kubelet pulls the Pod-published image.
func registry(ctx context.Context, id string) error {
	base, source := os.Getenv("KODAMA_REGISTRY_URL"), os.Getenv("KODAMA_REGISTRY_SOURCE")
	dest := os.Getenv("KODAMA_TEST_SCOPE") + "/pod-probe"
	manifest, mediaType, err := getManifest(ctx, base, source, "dev")
	if err != nil {
		return err
	}
	var index struct {
		Manifests []struct {
			Digest   string
			Platform struct{ OS, Architecture string }
		}
	}
	if err := json.Unmarshal(manifest, &index); err != nil {
		return err
	}
	if len(index.Manifests) > 0 { // an index: copy this Pod's platform
		selected := ""
		for _, m := range index.Manifests {
			if m.Platform.OS == runtime.GOOS && m.Platform.Architecture == runtime.GOARCH {
				selected = m.Digest
			}
		}
		if selected == "" {
			return fmt.Errorf("image index has no %s/%s manifest", runtime.GOOS, runtime.GOARCH)
		}
		if manifest, mediaType, err = getManifest(ctx, base, source, selected); err != nil {
			return err
		}
	}
	var image struct {
		Config struct{ Digest string }
		Layers []struct{ Digest string }
	}
	if err := json.Unmarshal(manifest, &image); err != nil {
		return err
	}
	if image.Config.Digest == "" {
		return fmt.Errorf("manifest has no config blob")
	}
	digests := []string{image.Config.Digest}
	for _, layer := range image.Layers {
		digests = append(digests, layer.Digest)
	}
	origin, err := url.Parse(base)
	if err != nil {
		return err
	}
	for _, digest := range digests {
		blob, _, err := request(ctx, "GET", base+"/v2/"+source+"/blobs/"+digest, "", nil)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(blob)
		if "sha256:"+hex.EncodeToString(hash[:]) != digest {
			return fmt.Errorf("registry blob digest mismatch (%d bytes)", len(blob))
		}
		_, headers, err := request(ctx, "POST", base+"/v2/"+dest+"/blobs/uploads/", "", nil)
		if err != nil {
			return err
		}
		location, err := url.Parse(headers.Get("Location"))
		if err != nil {
			return err
		}
		location = origin.ResolveReference(location)
		if location.Host != origin.Host {
			return fmt.Errorf("unexpected registry upload origin")
		}
		q := location.Query()
		q.Set("digest", digest)
		location.RawQuery = q.Encode()
		if _, _, err := request(ctx, "PUT", location.String(), "application/octet-stream", blob); err != nil {
			return err
		}
	}
	_, headers, err := request(ctx, "PUT", base+"/v2/"+dest+"/manifests/"+id, mediaType, manifest)
	if err != nil {
		return err
	}
	digest := headers.Get("Docker-Content-Digest")
	hash := sha256.Sum256(manifest)
	if digest != "sha256:"+hex.EncodeToString(hash[:]) {
		return fmt.Errorf("registry manifest digest mismatch")
	}
	fmt.Printf("PULL_IMAGE %s@%s\n", dest, digest)
	return nil
}
