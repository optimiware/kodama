package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/kms/production"
)

// KMSConfig contains only safe key coordinates; credentials are projected files, never arguments.
type KMSConfig struct{ A, B, Installation, Tenant string }

type freshInput struct {
	Config  production.Config
	Context secrets.WrapContext
	Wrap    secrets.WrappedKey
	Hash    [32]byte
}

func KMS(ctx context.Context) error {
	raw, err := os.ReadFile("/probe/config.json")
	if err != nil {
		return fmt.Errorf("kms: configuration missing")
	}
	var cfg KMSConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return fmt.Errorf("kms: configuration invalid")
	}
	token, err := os.ReadFile("/kms/secret-key")
	if err != nil {
		return fmt.Errorf("kms: workload credential missing")
	}
	denied, err := os.ReadFile("/denied/secret-key")
	if err != nil {
		return fmt.Errorf("kms: denied identity missing")
	}
	clients := production.Clients{ScalewayToken: strings.TrimSpace(string(token))}
	return runKMS(ctx, cfg, clients, strings.TrimSpace(string(denied)), func(in freshInput) error {
		input, _ := json.Marshal(in)
		defer clear(input)
		cmd := exec.CommandContext(ctx, os.Args[0], "-mode=kms-fresh")
		cmd.Stdin = bytes.NewReader(input)
		return cmd.Run()
	})
}

func runKMS(ctx context.Context, cfg KMSConfig, clients production.Clients, denied string, fresh func(freshInput) error) error {
	base := production.Config{Provider: production.Scaleway, KEKID: "t104-throwaway", Current: "v1", Resources: map[string]string{"v1": cfg.A, "v2": cfg.B}, ProbeTimeout: 10 * time.Second}
	p, err := production.New(ctx, base, clients)
	if err != nil {
		return fmt.Errorf("kms: workload startup failed class=%s", keyClass(err))
	}
	material := make([]byte, 32)
	defer clear(material)
	if _, err := rand.Read(material); err != nil {
		return fmt.Errorf("kms: entropy failed")
	}
	wc := secrets.WrapContext{InstallationID: cfg.Installation, TenantID: domain.TenantID(cfg.Tenant), Generation: 1, KeyID: "sk_t104"}
	wrapped, err := p.Wrap(ctx, wc, material)
	if err != nil {
		return fmt.Errorf("kms: wrap failed class=%s", keyClass(err))
	}
	got, err := p.Unwrap(ctx, wc, wrapped)
	if err != nil || !bytes.Equal(got, material) {
		return fmt.Errorf("kms: roundtrip failed")
	}
	clear(got)
	for _, mutate := range []func(*secrets.WrapContext){
		func(c *secrets.WrapContext) { c.InstallationID = "other" },
		func(c *secrets.WrapContext) { c.TenantID = "t_other" },
		func(c *secrets.WrapContext) { c.KeyID = "sk_other" },
		func(c *secrets.WrapContext) { c.Generation++ },
	} {
		bad := wc
		mutate(&bad)
		if _, err := p.Unwrap(ctx, bad, wrapped); keyClass(err) != secrets.KeyCorrupt {
			return fmt.Errorf("kms: context tamper was not corrupt")
		}
	}
	flipped := wrapped
	flipped.Bytes = bytes.Clone(wrapped.Bytes)
	flipped.Bytes[0] ^= 0xff
	if _, err := p.Unwrap(ctx, wc, flipped); keyClass(err) != secrets.KeyCorrupt {
		return fmt.Errorf("kms: ciphertext tamper was not corrupt")
	}
	relabelled := wrapped
	relabelled.KEK.Version = "v2"
	if _, err := p.Unwrap(ctx, wc, relabelled); keyClass(err) != secrets.KeyCorrupt {
		return fmt.Errorf("kms: configured version tamper was not corrupt")
	}
	base.Current = "v2"
	rotated, err := production.New(ctx, base, clients)
	if err != nil {
		return fmt.Errorf("kms: rotation startup failed class=%s", keyClass(err))
	}
	got, err = rotated.Unwrap(ctx, wc, wrapped)
	if err != nil || !bytes.Equal(got, material) {
		return fmt.Errorf("kms: retained v1 unwrap failed")
	}
	clear(got)
	newWrap, err := rotated.Wrap(ctx, wc, material)
	if err != nil || newWrap.KEK.Version != "v2" {
		return fmt.Errorf("kms: rotation did not record v2")
	}
	got, err = rotated.Unwrap(ctx, wc, newWrap)
	if err != nil || !bytes.Equal(got, material) {
		return fmt.Errorf("kms: rotated material changed")
	}
	clear(got)

	// Transfer the old envelope only through an anonymous pipe, never disk or evidence. A fresh
	// executable checks it under retained v1, then checks missing-v1 failure before networking.
	if err := fresh(freshInput{base, wc, wrapped, sha256.Sum256(material)}); err != nil {
		return fmt.Errorf("kms: fresh-process retained-envelope check failed")
	}

	denyClients := production.Clients{ScalewayToken: denied}
	_, err = production.New(ctx, base, denyClients)
	if !classified(err, secrets.KeyDenied, false) {
		return fmt.Errorf("kms: identity without decrypt was not non-retryable denied class=%s", keyClass(err))
	}
	// Prove the deployment credential has no key administration authority, on this test's key only.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.scaleway.com/key-manager/v1alpha1/regions/fr-par/keys/"+cfg.A+"/unprotect", strings.NewReader("{}"))
	req.Header.Set("X-Auth-Token", clients.ScalewayToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("kms: administration denial unavailable")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("kms: workload administration must be forbidden HTTP=%d", resp.StatusCode)
	}

	// Isolated probe process only: block actual cloud egress at its transport, exercise startup
	// and an already-constructed runtime client, then restore and use the unchanged old wrap.
	transport := http.DefaultTransport
	dead, _ := url.Parse("http://127.0.0.1:1")
	http.DefaultTransport = &http.Transport{Proxy: http.ProxyURL(dead)}
	defer func() { http.DefaultTransport = transport }()
	start := time.Now()
	_, err = production.New(ctx, base, clients)
	if !classified(err, secrets.KeyUnavailable, true) || time.Since(start) > base.ProbeTimeout+time.Second {
		return fmt.Errorf("kms: startup outage classification or bound failed")
	}
	if _, err := rotated.Unwrap(ctx, wc, wrapped); !classified(err, secrets.KeyUnavailable, true) {
		return fmt.Errorf("kms: runtime outage was not retryable unavailable")
	}
	http.DefaultTransport = transport
	got, err = rotated.Unwrap(ctx, wc, wrapped)
	if err != nil || !bytes.Equal(got, material) {
		return fmt.Errorf("kms: recovery changed or lost the retained material")
	}
	clear(got)
	fmt.Println("PASS kms workload_identity roundtrip tamper rotation fresh_process denied outage recovery")
	return nil
}

func FreshKMS(ctx context.Context, input io.Reader) error {
	var in freshInput
	if json.NewDecoder(io.LimitReader(input, 32<<10)).Decode(&in) != nil {
		return fmt.Errorf("kms: fresh input invalid")
	}
	token, err := os.ReadFile("/kms/secret-key")
	if err != nil {
		return fmt.Errorf("kms: fresh credential missing")
	}
	c := production.Clients{ScalewayToken: strings.TrimSpace(string(token))}
	return checkFresh(ctx, in, c)
}

func checkFresh(ctx context.Context, in freshInput, c production.Clients) error {
	p, err := production.New(ctx, in.Config, c)
	if err != nil {
		return fmt.Errorf("kms: fresh startup failed")
	}
	got, err := p.Unwrap(ctx, in.Context, in.Wrap)
	if err != nil || sha256.Sum256(got) != in.Hash {
		return fmt.Errorf("kms: restored old envelope failed")
	}
	clear(got)
	in.Config.Resources = map[string]string{"v2": in.Config.Resources["v2"]}
	p, err = production.New(ctx, in.Config, c)
	if err != nil {
		return fmt.Errorf("kms: fresh current startup failed")
	}
	if _, err := p.Unwrap(ctx, in.Context, in.Wrap); keyClass(err) != secrets.KeyNotFound {
		return fmt.Errorf("kms: missing retained version was not not_found")
	}
	return nil
}

func keyClass(err error) secrets.KeyErrorClass {
	var ke *secrets.KeyError
	if errors.As(err, &ke) {
		return ke.Class
	}
	return "unclassified"
}

func classified(err error, class secrets.KeyErrorClass, retryable bool) bool {
	var ke *secrets.KeyError
	return errors.As(err, &ke) && ke.Class == class && ke.Retryable() == retryable
}
