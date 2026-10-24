package devenv

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FixtureNames are the credentials generated once per scope. They stay stable
// across repeated `dev-up` calls so dependency volumes remain usable.
var FixtureNames = []string{
	"postgres-password", "migration-password", "runtime-password", "nats-token",
	"git-read-token", "git-write-token", "s3-secret-key",
}

// Fixtures writes the scope's fixture files and returns their directory.
//
// The scope directory is mode 0700 on the host; this subdirectory is mounted
// read-only into containers that run as other UIDs.
func (e *Environment) Fixtures() (string, error) {
	directory := filepath.Join(e.Dir, "fixtures")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	stable := map[string]string{}
	for _, name := range FixtureNames {
		path := filepath.Join(directory, name)
		if !exists(path) {
			value, err := tokenHex(24)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
				return "", err
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		stable[name] = string(data)
	}
	if !exists(filepath.Join(directory, "master-key")) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(directory, "master-key"), key, 0o644); err != nil {
			return "", err
		}
	}
	if !exists(filepath.Join(directory, "tls.crt")) {
		if err := certGenFn(directory, e.WorkloadHost("git")); err != nil {
			return "", err
		}
	}
	database := e.Database()
	urls := []struct{ name, user, password string }{
		{"database-migration-url", "kodama_migration", stable["migration-password"]},
		{"database-runtime-url", "kodama_runtime", stable["runtime-password"]},
	}
	for _, item := range urls {
		value := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable",
			item.user, item.password, e.WorkloadHost("postgres"), database)
		if err := os.WriteFile(filepath.Join(directory, item.name), []byte(value), 0o644); err != nil {
			return "", err
		}
	}
	files := map[string]string{
		"s3-access-key": "test",
		"init.sql": fmt.Sprintf(`CREATE ROLE kodama_migration LOGIN PASSWORD '%s';
CREATE ROLE kodama_runtime LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS;
CREATE DATABASE %s OWNER kodama_migration;
\connect %s
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO kodama_runtime;
`, stable["migration-password"], stable["runtime-password"], database, database),
		"nats.conf": fmt.Sprintf("port: 4222\njetstream { store_dir: /data/jetstream }\nauthorization { token: \"%s\" }\n", stable["nats-token"]),
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o644); err != nil {
			return "", err
		}
	}
	environment := ProbeEnv(e.Scope, e.WorkloadHost)
	oidc := map[string]any{
		"issuer":                 environment["KODAMA_OIDC_ISSUER"],
		"api_audience":           environment["KODAMA_OIDC_AUDIENCE"],
		"token_ttl":              "15m",
		"authorization_code_ttl": "5m",
		"refresh_token_ttl":      "8h",
		"clients": []map[string]any{{
			"id": "kodama-cli", "name": "Kodama development CLI", "public": true,
			"redirect_uris":             []string{"http://127.0.0.1:18999/callback"},
			"post_logout_redirect_uris": []string{"http://127.0.0.1:18999/"},
			"allowed_origins":           []string{"http://127.0.0.1:18999"},
		}},
		"personas": []map[string]any{{
			"id": "engineer", "subject": "oauthsonas|engineer", "email": "engineer@example.test",
			"name": "Fixture Engineer", "roles": []string{"engineer"},
		}},
	}
	encoded, err := json.MarshalIndent(oidc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(directory, "oauthsonas.yaml"), append(encoded, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Chmod(filepath.Join(directory, entry.Name()), 0o644); err != nil {
			return "", err
		}
	}
	return directory, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func tokenHex(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
