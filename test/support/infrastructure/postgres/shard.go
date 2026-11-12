// Package pgtest starts one isolated PostgreSQL server per component test and
// exposes the product pgx wrapper for real conformance runs.
//
// It is test support code under test/, never product code. The server is the
// pinned official image from the infrastructure harness, authenticated by trust
// on the shard's private network only. Migrations and every conformance query
// travel through the product wrapper (kodama/internal/shell/app/drivers/postgres);
// only the administrator bootstrap (role creation, CREATE DATABASE, the
// migration ledger, catalog inspection and fixture seeding) uses a raw
// superuser session because those operations are not part of the application
// runtime.
package pgtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// DatabaseName is the single application database created per shard. Roles are
// cluster-scoped in PostgreSQL, so one fresh server per shard is what keeps the
// role bootstrap deterministic.
const DatabaseName = "kodama"

// EvidenceDirEnv names the PARENT directory the harness and this package write
// evidence into. Each run resolves one unique child directory under it, so
// concurrent runs sharing a parent never overwrite each other. When unset, the
// parent is the ignored local scratch path <module root>/.harness/evidence/I02b
// so no tracked source path is written.
const EvidenceDirEnv = "KODAMA_EVIDENCE_DIR"

const (
	evidenceRunPrefix    = "run-"
	serverEvidencePrefix = "postgres-server-"
)

// Shard is one isolated PostgreSQL server plus the wrapper handles conformance
// tests use.
type Shard struct {
	T             testing.TB
	Harness       *harness.Shard
	Container     *harness.Container
	Addr          string
	ImageTag      string
	ImageDigest   string
	ServerVersion string
	Database      string

	admin *pgdriver.DB
	owner *pgdriver.DB
}

// Start pulls the pinned image, starts one server, creates the application
// database and records the image digest/version as evidence. All cleanup is
// registered with t.
func Start(t testing.TB) *Shard {
	t.Helper()
	// Resolve and publish the run-unique evidence directory before the harness
	// builds its Podman client, so image digests are recorded in this run too.
	EvidenceDir()
	hs := harness.NewShard(t)
	digest := hs.RequireImage(hs.Context(), harness.ImagePostgres)
	container, err := hs.Run(hs.Context(), harness.RunSpec{
		Image: harness.ImagePostgres,
		Ports: []harness.Port{{Container: 5432}},
		Env: map[string]string{
			"POSTGRES_HOST_AUTH_METHOD": "trust",
			"POSTGRES_USER":             "postgres",
			"POSTGRES_DB":               "postgres",
		},
	})
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	ctx := hs.Context()
	addr, err := container.Addr(ctx, 5432)
	if err != nil {
		t.Fatalf("postgres address: %v", err)
	}
	if err := hs.WaitReady(ctx, "postgres", harness.PostgresProbe(addr, "postgres")); err != nil {
		logs, _ := container.Logs(ctx)
		t.Fatalf("postgres readiness: %v\n%s", err, logs)
	}

	shard := &Shard{
		T:           t,
		Harness:     hs,
		Container:   container,
		Addr:        addr,
		ImageTag:    harness.ImagePostgres,
		ImageDigest: digest,
		Database:    DatabaseName,
	}
	shard.createDatabase(ctx)
	shard.admin = shard.mustDB(ctx, "postgres", 4)
	shard.owner = shard.mustDB(ctx, "postgres", 4)
	shard.ServerVersion = shard.scalarString(ctx, shard.admin, "SHOW server_version")
	shard.recordEvidence()
	t.Cleanup(func() { shard.close() })
	return shard
}

func (s *Shard) createDatabase(ctx context.Context) {
	s.T.Helper()
	conn, err := pgx.Connect(ctx, s.dsnDatabase("postgres", "postgres"))
	if err != nil {
		s.T.Fatalf("admin connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+quoteIdent(s.Database)); err != nil {
		s.T.Fatalf("create database %s: %v", s.Database, err)
	}
}

func (s *Shard) mustDB(ctx context.Context, role string, maxConns int32) *pgdriver.DB {
	s.T.Helper()
	db, err := s.NewDB(role, maxConns)
	if err != nil {
		s.T.Fatalf("connect as %s: %v", role, err)
	}
	return db
}

// NewDB opens the pgx wrapper for one role. It is the only way conformance
// tests reach the database.
func (s *Shard) NewDB(role string, maxConns int32) (*pgdriver.DB, error) {
	return pgdriver.New(pgdriver.Config{
		ConnString:     s.dsn(role),
		MaxConns:       maxConns,
		MinConns:       0,
		ConnectTimeout: 10 * time.Second,
	})
}

func (s *Shard) dsn(role string) string {
	return s.dsnDatabase(role, s.Database)
}

func (s *Shard) dsnDatabase(role, database string) string {
	host, port, err := net.SplitHostPort(s.Addr)
	if err != nil {
		host, port = "127.0.0.1", "5432"
	}
	return fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=disable&connect_timeout=10",
		role, host, port, database)
}

// Admin is the superuser wrapper. It is for bootstrap, catalog inspection and
// fixture seeding only; application reads and writes use role-scoped wrappers.
func (s *Shard) Admin() *pgdriver.DB { return s.admin }

// Owner is a wrapper connected as the administrator that migrations assume the
// owner role with. It is the migration connection.
func (s *Shard) Owner() *pgdriver.DB { return s.owner }

// Runtime returns a wrapper bound to the non-owner runtime role.
func (s *Shard) Runtime(maxConns int32) *pgdriver.DB {
	s.T.Helper()
	return s.mustDB(s.Harness.Context(), "kodama_runtime", maxConns)
}

// Role returns a wrapper bound to one declared non-owner role.
func (s *Shard) Role(name string, maxConns int32) *pgdriver.DB {
	s.T.Helper()
	return s.mustDB(s.Harness.Context(), name, maxConns)
}

// RawSession opens one direct pgx session for a role. It is for test-owned lock
// holders, deadlock participants and pg_stat_activity observers only; every
// application read and write still travels through the pgx wrapper. The caller
// closes the session.
func (s *Shard) RawSession(ctx context.Context, role string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, s.dsn(role))
}

func (s *Shard) close() {
	if s.admin != nil {
		s.admin.Close()
	}
	if s.owner != nil {
		s.owner.Close()
	}
}

func (s *Shard) scalarString(ctx context.Context, db *pgdriver.DB, query string, args ...any) string {
	s.T.Helper()
	values, err := QueryStrings(ctx, db, query, args...)
	if err != nil {
		s.T.Fatalf("query %q: %v", query, err)
	}
	if len(values) != 1 || len(values[0]) != 1 {
		s.T.Fatalf("query %q returned %d rows", query, len(values))
	}
	return values[0][0]
}

func quoteIdent(name string) string {
	return `"` + name + `"`
}

// serverEvidence is one shard's record of the real server that served it.
// Shard is the harness shard identity, so every shard keeps its own server
// identity even when many shards run in one process.
type serverEvidence struct {
	Shard         string    `json:"shard"`
	Image         string    `json:"image"`
	Digest        string    `json:"digest"`
	ServerVersion string    `json:"server_version"`
	Database      string    `json:"database"`
	RecordedAt    time.Time `json:"recorded_at"`
}

func (s *Shard) recordEvidence() {
	if err := s.writeServerEvidence(EvidenceDir()); err != nil {
		s.T.Logf("postgres evidence: %v", err)
	}
}

// writeServerEvidence records this shard's server identity in a file named for
// that shard, so shards never overwrite each other's evidence.
func (s *Shard) writeServerEvidence(dir string) error {
	identity := ""
	if s.Harness != nil {
		identity = s.Harness.ID
	}
	record := serverEvidence{
		Shard:         identity,
		Image:         s.ImageTag,
		Digest:        s.ImageDigest,
		ServerVersion: s.ServerVersion,
		Database:      s.Database,
		RecordedAt:    time.Now().UTC(),
	}
	return WriteEvidence(dir, serverEvidenceName(identity), record)
}

// serverEvidenceName is the per-shard evidence file name. The readable shard
// identity is suffix-safe, and the hash keeps two identities that sanitize to
// the same readable form distinct.
func serverEvidenceName(shardID string) string {
	sum := sha256.Sum256([]byte(shardID))
	return serverEvidencePrefix + sanitizeEvidenceName(shardID) + "-" +
		hex.EncodeToString(sum[:4]) + ".json"
}

func sanitizeEvidenceName(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		out = "shard"
	}
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "-")
	}
	return out
}

// evidenceStore resolves one process's run-unique evidence directory. The
// getenv/setenv seams keep the resolution testable and let the exported
// functions publish the actual run path to the harness.
type evidenceStore struct {
	getenv func(string) string
	setenv func(string, string) error

	mu     sync.Mutex
	parent string
	dir    string
}

func (s *evidenceStore) parentDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parent == "" {
		s.parent = evidenceParent(s.getenv)
	}
	return s.parent
}

// runDir returns the run-unique evidence directory, creating it on first use.
// The environment holds the parent, so publishing the resolved child is what
// makes the harness record image evidence into this run.
func (s *evidenceStore) runDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		return s.dir
	}
	if s.parent == "" {
		s.parent = evidenceParent(s.getenv)
	}
	s.dir = newEvidenceRunDir(s.parent)
	if s.setenv != nil {
		_ = s.setenv(EvidenceDirEnv, s.dir)
	}
	return s.dir
}

var processEvidence = &evidenceStore{getenv: os.Getenv, setenv: os.Setenv}

// evidenceParent returns the supplied evidence parent: KODAMA_EVIDENCE_DIR
// when set, otherwise ignored local scratch <module root>/.harness/evidence/I02b.
func evidenceParent(getenv func(string) string) string {
	if dir := getenv(EvidenceDirEnv); dir != "" {
		return filepath.Clean(dir)
	}
	return fallbackEvidenceParent()
}

func fallbackEvidenceParent() string {
	dir, err := os.Getwd()
	if err == nil {
		for {
			if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
				return filepath.Join(dir, ".harness", "evidence", "I02b")
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return filepath.Join(os.TempDir(), "kodama-i02b-evidence")
}

// newEvidenceRunDir creates a directory that is unique to this run and process.
// The random token keeps two sequential runs of the same process apart; the pid
// keeps concurrent processes apart. Creation is best effort: WriteEvidence
// creates the directory too and reports a real failure.
func newEvidenceRunDir(parent string) string {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		suffix = []byte(strconv.FormatInt(time.Now().UnixNano(), 16))
	}
	dir := filepath.Join(parent, evidenceRunPrefix+strconv.Itoa(os.Getpid())+"-"+hex.EncodeToString(suffix))
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// EvidenceDir returns this process's run-unique evidence directory, creating it
// on first use and publishing it through KODAMA_EVIDENCE_DIR so the harness
// records into the same run. Every call in one process returns the same path.
func EvidenceDir() string { return processEvidence.runDir() }

// WriteEvidence writes one JSON document atomically into dir. Each write stages
// its bytes in a unique temporary file next to the target and renames that file
// into place, so concurrent writers never share a staging path and a reader
// never observes a partial document.
func WriteEvidence(dir, name string, value any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create evidence dir: %w", err)
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode evidence: %w", err)
	}
	staged, err := os.CreateTemp(dir, "."+filepath.Base(name)+".tmp-*")
	if err != nil {
		return fmt.Errorf("stage evidence: %w", err)
	}
	temporary := staged.Name()
	defer os.Remove(temporary)
	if _, err := staged.Write(append(encoded, '\n')); err != nil {
		staged.Close()
		return fmt.Errorf("write evidence: %w", err)
	}
	if err := staged.Chmod(0o644); err != nil {
		staged.Close()
		return fmt.Errorf("mode evidence: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("close evidence: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("publish evidence: %w", err)
	}
	return nil
}
