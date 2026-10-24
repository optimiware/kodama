// Package config loads and validates typed process configuration at start-up.
//
// Configuration files hold only non-secret settings and references to
// protected secret files (a database URL file, ERP token files, a cursor key
// file). A secret value cannot be written inline: unknown fields are rejected
// and there is no field that accepts one. Every problem is reported together so
// a misconfigured deployment fails at start, not on the first request.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"example.invalid/inventory/internal/tenancy"
)

// Duration is a JSON string such as "5s".
type Duration time.Duration

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("must be a duration string such as \"5s\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Std converts to time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Database references the runtime (or migration) connection secret.
type Database struct {
	// URLFile contains the PostgreSQL URI. The API and worker point it at the
	// non-owner runtime role; only the migrate command points at the owner role.
	URLFile  string `json:"urlFile"`
	MaxConns int32  `json:"maxConns"`
}

// OIDC is the application's own identity provider, independent of any factory.
type OIDC struct {
	Issuer              string `json:"issuer"`
	Audience            string `json:"audience"`
	AllowInsecureIssuer bool   `json:"allowInsecureIssuer"`
}

// API configures `inventory serve`.
type API struct {
	HTTPAddr        string   `json:"httpAddr"`
	Database        Database `json:"database"`
	OIDC            OIDC     `json:"oidc"`
	CursorKeyFile   string   `json:"cursorKeyFile"`
	ShutdownTimeout Duration `json:"shutdownTimeout"`
}

// ERP configures the outgoing legacy integration.
type ERP struct {
	BaseURL        string   `json:"baseUrl"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	AllowInsecure  bool     `json:"allowInsecure"`
	Scopes         []string `json:"scopes"`
	// CredentialFiles maps application tenant IDs to protected token files.
	CredentialFiles map[string]string `json:"credentialFiles"`
}

// RequiredERPScopes are the ERP scopes the worker's credentials must carry.
var RequiredERPScopes = []string{"inventory:adjust", "inventory:read"}

// Worker configures `inventory worker`: one application tenant and one service identity.
type Worker struct {
	Database     Database `json:"database"`
	Tenant       string   `json:"tenant"`
	ServiceID    string   `json:"serviceId"`
	ERP          ERP      `json:"erp"`
	PollInterval Duration `json:"pollInterval"`
	Lease        Duration `json:"lease"`
	MaxAttempts  int      `json:"maxAttempts"`
	RetryBase    Duration `json:"retryBase"`
	RetryMax     Duration `json:"retryMax"`
}

// Migrate configures `inventory migrate` and `inventory seed`.
type Migrate struct {
	Database    Database `json:"database"`
	RuntimeRole string   `json:"runtimeRole"`
}

var roleRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

type problems []string

func (p *problems) addf(f string, a ...any) { *p = append(*p, fmt.Sprintf(f, a...)) }
func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	sort.Strings(p)
	return errors.New("invalid configuration:\n  - " + strings.Join(p, "\n  - "))
}

func decode(path string, dst any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("config: %s: trailing data", path)
	}
	return nil
}

// LoadAPI reads and validates the API configuration.
func LoadAPI(path string) (API, error) {
	c := API{HTTPAddr: ":8080", ShutdownTimeout: Duration(10 * time.Second), Database: Database{MaxConns: 10}}
	if err := decode(path, &c); err != nil {
		return API{}, err
	}
	var p problems
	c.Database.validate(&p)
	c.OIDC.validate(&p)
	if c.CursorKeyFile == "" {
		p.addf("cursorKeyFile is required")
	}
	if c.HTTPAddr == "" {
		p.addf("httpAddr is required")
	}
	if d := c.ShutdownTimeout.Std(); d <= 0 || d > time.Minute {
		p.addf("shutdownTimeout must be within (0, 1m]")
	}
	return c, p.err()
}

// LoadWorker reads and validates the worker configuration.
func LoadWorker(path string) (Worker, error) {
	c := Worker{PollInterval: Duration(2 * time.Second), Lease: Duration(30 * time.Second), MaxAttempts: 8,
		RetryBase: Duration(5 * time.Second), RetryMax: Duration(10 * time.Minute), Database: Database{MaxConns: 4}, ERP: ERP{TimeoutSeconds: 10}}
	if err := decode(path, &c); err != nil {
		return Worker{}, err
	}
	var p problems
	c.Database.validate(&p)
	if _, err := tenancy.ParseTenantID(c.Tenant); err != nil {
		p.addf("tenant must be an application tenant UUID")
	}
	if c.ServiceID == "" || len(c.ServiceID) > 256 {
		p.addf("serviceId is required (the worker's own attributable identity)")
	}
	c.ERP.validate(&p, c.Tenant)
	if c.PollInterval.Std() < 100*time.Millisecond {
		p.addf("pollInterval must be at least 100ms")
	}
	if c.Lease.Std() < time.Second {
		p.addf("lease must be at least 1s")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 100 {
		p.addf("maxAttempts must be within 1..100")
	}
	if c.RetryBase.Std() <= 0 || c.RetryMax.Std() < c.RetryBase.Std() {
		p.addf("retryBase must be positive and not exceed retryMax")
	}
	return c, p.err()
}

// LoadMigrate reads and validates the migration configuration.
func LoadMigrate(path string) (Migrate, error) {
	c := Migrate{Database: Database{MaxConns: 2}}
	if err := decode(path, &c); err != nil {
		return Migrate{}, err
	}
	var p problems
	c.Database.validate(&p)
	if !roleRE.MatchString(c.RuntimeRole) {
		p.addf("runtimeRole must be a plain PostgreSQL role name")
	}
	return c, p.err()
}

func (d Database) validate(p *problems) {
	if d.URLFile == "" {
		p.addf("database.urlFile is required")
	}
	if d.MaxConns < 1 || d.MaxConns > 100 {
		p.addf("database.maxConns must be within 1..100")
	}
}

func (o OIDC) validate(p *problems) {
	u, err := url.Parse(o.Issuer)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		p.addf("oidc.issuer must be an absolute http(s) URL")
	}
	if strings.TrimSpace(o.Audience) == "" {
		p.addf("oidc.audience is required")
	}
}

func (e ERP) validate(p *problems, workerTenant string) {
	u, err := url.Parse(e.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		p.addf("erp.baseUrl must be an absolute http(s) URL")
	}
	if e.TimeoutSeconds < 1 || e.TimeoutSeconds > 120 {
		p.addf("erp.timeoutSeconds must be within 1..120")
	}
	have := map[string]bool{}
	for _, s := range e.Scopes {
		have[s] = true
	}
	for _, s := range RequiredERPScopes {
		if !have[s] {
			p.addf("erp.scopes must include %s", s)
		}
	}
	if len(e.CredentialFiles) == 0 {
		p.addf("erp.credentialFiles must name a token file for the worker's tenant")
	}
	for t, f := range e.CredentialFiles {
		if _, err := tenancy.ParseTenantID(t); err != nil || f == "" {
			p.addf("erp.credentialFiles entries map a tenant UUID to a file")
		}
		if workerTenant != "" && t != workerTenant {
			p.addf("erp.credentialFiles must not carry another tenant's credential (%s)", t)
		}
	}
}

// ReadSecret reads a protected secret file and trims one trailing newline. The
// value never appears in an error.
func ReadSecret(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("secret file %s: %w", path, errors.Unwrap(err))
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("secret file %s is not a regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("secret file %s: %w", path, errors.Unwrap(err))
	}
	v := strings.TrimRight(string(raw), "\r\n")
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return v, nil
}
