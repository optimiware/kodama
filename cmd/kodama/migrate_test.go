package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
)

// migrateEnv is one complete, explicit `kodama migrate` environment. The
// passwords are sentinels so every refusal can assert they never render.
func migrateEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"KODAMA_ADMIN_DATABASE_URL":      "postgres://kodama_admin:SENTINEL-ADMIN-PW@db:5432/postgres",
		"KODAMA_DATABASE_URL":            "postgres://kodama_runtime:SENTINEL-RUNTIME-PW@db:5432/kodama",
		"KODAMA_CONTROLLER_DATABASE_URL": "postgres://kodama_controller:SENTINEL-CONTROLLER-PW@db:5432/kodama",
		"KODAMA_PUBLISHER_DATABASE_URL":  "postgres://kodama_publisher:SENTINEL-PUBLISHER-PW@db:5432/kodama",
		"KODAMA_AUTH_DATABASE_URL":       "postgres://kodama_auth:SENTINEL-AUTH-PW@db:5432/kodama",
	}
}

func setMigrateEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for _, name := range []string{
		"KODAMA_ADMIN_DATABASE_URL", "KODAMA_DATABASE_URL", "KODAMA_CONTROLLER_DATABASE_URL",
		"KODAMA_PUBLISHER_DATABASE_URL", "KODAMA_AUTH_DATABASE_URL",
	} {
		t.Setenv(name, values[name])
	}
}

func TestMigrateRefusesIncompleteConfigurationSecretFree(t *testing.T) {
	// Every failing value carries its own sentinel credential, so a refusal
	// that echoes the invalid value (not just the other, untouched URLs)
	// cannot pass. Each case also names the exact reason its refusal must give:
	// a query-string password must not stand in for missing userinfo, and a
	// wrong scheme must not stand in for a missing user. The two "missing"
	// cases have no value to echo; they still scan the complete environment's
	// sentinels.
	for _, tc := range []struct {
		name, key, value, sentinel, wantKey, wantReason string
	}{
		{"admin URL missing", "KODAMA_ADMIN_DATABASE_URL", "", "", "KODAMA_ADMIN_DATABASE_URL", "must be a postgres:// URL"},
		{"admin URL has no user", "KODAMA_ADMIN_DATABASE_URL", "postgres://db:5432/postgres?password=SENTINEL-ADMIN-NOUSER", "SENTINEL-ADMIN-NOUSER", "KODAMA_ADMIN_DATABASE_URL", "must name a database user"},
		{"admin URL has an empty user", "KODAMA_ADMIN_DATABASE_URL", "postgres://:SENTINEL-ADMIN-EMPTY-USER@db:5432/postgres", "SENTINEL-ADMIN-EMPTY-USER", "KODAMA_ADMIN_DATABASE_URL", "must name a database user"},
		{"admin URL has no password", "KODAMA_ADMIN_DATABASE_URL", "postgres://admin@db:5432/postgres?password=SENTINEL-ADMIN-NO-PASSWORD", "SENTINEL-ADMIN-NO-PASSWORD", "KODAMA_ADMIN_DATABASE_URL", "must carry a database password"},
		{"admin URL has an empty password", "KODAMA_ADMIN_DATABASE_URL", "postgres://admin:@db:5432/postgres", "", "KODAMA_ADMIN_DATABASE_URL", "must carry a database password"},
		{"admin URL has no maintenance database", "KODAMA_ADMIN_DATABASE_URL", "postgres://admin:SENTINEL-ADMIN-NODB@db:5432/", "SENTINEL-ADMIN-NODB", "KODAMA_ADMIN_DATABASE_URL", "must name a maintenance database"},
		{"admin URL is not postgres", "KODAMA_ADMIN_DATABASE_URL", "mysql://admin:SENTINEL-ADMIN-SCHEME@db/postgres", "SENTINEL-ADMIN-SCHEME", "KODAMA_ADMIN_DATABASE_URL", "must be a postgres:// URL"},
		{"target URL missing", "KODAMA_DATABASE_URL", "", "", "KODAMA_DATABASE_URL", "must name the target database"},
		{"target URL has no database", "KODAMA_DATABASE_URL", "postgres://runtime:SENTINEL-TARGET-NODB@db:5432/", "SENTINEL-TARGET-NODB", "KODAMA_DATABASE_URL", "must name the target database"},
		{"runtime user missing", "KODAMA_DATABASE_URL", "postgres://db:5432/kodama?password=SENTINEL-RUNTIME-NOUSER", "SENTINEL-RUNTIME-NOUSER", "KODAMA_DATABASE_URL", "must name a database user"},
		{"runtime password missing", "KODAMA_DATABASE_URL", "postgres://kodama_runtime@db:5432/kodama?password=SENTINEL-RUNTIME-NOPW", "SENTINEL-RUNTIME-NOPW", "KODAMA_DATABASE_URL", "has no password"},
		{"controller user missing", "KODAMA_CONTROLLER_DATABASE_URL", "postgres://db:5432/kodama?password=SENTINEL-CONTROLLER-NOUSER", "SENTINEL-CONTROLLER-NOUSER", "KODAMA_CONTROLLER_DATABASE_URL", "must name a database user"},
		{"controller password missing", "KODAMA_CONTROLLER_DATABASE_URL", "postgres://kodama_controller@db:5432/kodama?password=SENTINEL-CONTROLLER-NOPW", "SENTINEL-CONTROLLER-NOPW", "KODAMA_CONTROLLER_DATABASE_URL", "has no password"},
		{"publisher user missing", "KODAMA_PUBLISHER_DATABASE_URL", "postgres://db:5432/kodama?password=SENTINEL-PUBLISHER-NOUSER", "SENTINEL-PUBLISHER-NOUSER", "KODAMA_PUBLISHER_DATABASE_URL", "must name a database user"},
		{"publisher password missing", "KODAMA_PUBLISHER_DATABASE_URL", "postgres://kodama_publisher@db:5432/kodama?password=SENTINEL-PUBLISHER-NOPW", "SENTINEL-PUBLISHER-NOPW", "KODAMA_PUBLISHER_DATABASE_URL", "has no password"},
		{"publisher URL is not postgres", "KODAMA_PUBLISHER_DATABASE_URL", "http://kodama_publisher:SENTINEL-PUBLISHER-SCHEME@db:5432/kodama", "SENTINEL-PUBLISHER-SCHEME", "KODAMA_PUBLISHER_DATABASE_URL", "must be a postgres:// URL"},
		{"auth user missing", "KODAMA_AUTH_DATABASE_URL", "postgres://db:5432/kodama?password=SENTINEL-AUTH-NOUSER", "SENTINEL-AUTH-NOUSER", "KODAMA_AUTH_DATABASE_URL", "must name a database user"},
		{"auth password missing", "KODAMA_AUTH_DATABASE_URL", "postgres://kodama_auth@db:5432/kodama?password=SENTINEL-AUTH-NOPW", "SENTINEL-AUTH-NOPW", "KODAMA_AUTH_DATABASE_URL", "has no password"},
		{"auth role is not a kodama role", "KODAMA_AUTH_DATABASE_URL", "postgres://postgres:SENTINEL-AUTH-ROLE@db:5432/kodama", "SENTINEL-AUTH-ROLE", "KODAMA_AUTH_DATABASE_URL", "kodama_ login role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := migrateEnv(t)
			values[tc.key] = tc.value
			setMigrateEnv(t, values)
			var stdout, stderr lockedBuffer
			code := runMigrateEntry(context.Background(), nil, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d; want 2 (stderr=%q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantKey) {
				t.Fatalf("stderr = %q; want it to name %q", stderr.String(), tc.wantKey)
			}
			if !strings.Contains(stderr.String(), tc.wantReason) {
				t.Fatalf("stderr = %q; want the refusal reason %q, not an unrelated one", stderr.String(), tc.wantReason)
			}
			secrets := []string{"SENTINEL-ADMIN-PW", "SENTINEL-RUNTIME-PW", "SENTINEL-CONTROLLER-PW", "SENTINEL-PUBLISHER-PW", "SENTINEL-AUTH-PW"}
			if tc.sentinel != "" {
				secrets = append(secrets, tc.sentinel)
			}
			for _, secret := range secrets {
				if strings.Contains(stderr.String(), secret) || strings.Contains(stdout.String(), secret) {
					t.Fatalf("migration output rendered a password: %q", stderr.String())
				}
			}
			if tc.value != "" && strings.Contains(stdout.String()+stderr.String(), tc.value) {
				t.Fatalf("migration output rendered the rejected value: %q", stdout.String()+stderr.String())
			}
			if stdout.String() != "" {
				t.Fatalf("a refused migration wrote to stdout: %q", stdout.String())
			}
		})
	}
}

func TestMigrateRefusesConflictingPasswordsForOneRole(t *testing.T) {
	values := migrateEnv(t)
	values["KODAMA_CONTROLLER_DATABASE_URL"] = "postgres://kodama_runtime:SENTINEL-OTHER-PW@db:5432/kodama"
	setMigrateEnv(t, values)
	var stdout, stderr lockedBuffer
	code := runMigrateEntry(context.Background(), nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2 (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "different passwords for role kodama_runtime") {
		t.Fatalf("stderr = %q; want a precise role conflict", stderr.String())
	}
	if strings.Contains(stderr.String(), "SENTINEL-OTHER-PW") || strings.Contains(stderr.String(), "SENTINEL-RUNTIME-PW") {
		t.Fatalf("stderr rendered a password: %q", stderr.String())
	}
}

func TestMigrateTakesNoArguments(t *testing.T) {
	setMigrateEnv(t, migrateEnv(t))
	var stdout, stderr lockedBuffer
	if code := runMigrateEntry(context.Background(), []string{"--force"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "migrate takes no arguments") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRoleCredential(t *testing.T) {
	for _, tc := range []struct {
		name, raw, role, password string
		wantReason                string
		ok                        bool
	}{
		{"valid", "postgres://kodama_runtime:p%27w@db:5432/kodama?sslmode=verify-full", "kodama_runtime", "p'w", "", true},
		{"no password", "postgres://kodama_runtime@db/kodama", "", "", "has no password", false},
		{"empty password", "postgres://kodama_runtime:@db/kodama", "", "", "has no password", false},
		{"not postgres", "http://kodama_runtime:x@db/kodama", "", "", "must be a postgres:// URL", false},
		{"no user", "postgres://db/kodama", "", "", "must name a database user", false},
		{"empty user", "postgres://:x@db/kodama", "", "", "must name a database user", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role, password, err := roleCredential(tc.raw)
			if tc.ok {
				if err != nil || role != tc.role || password != tc.password {
					t.Fatalf("roleCredential = (%q, %q, %v); want (%q, %q, nil)", role, password, err, tc.role, tc.password)
				}
				return
			}
			if err == nil {
				t.Fatalf("roleCredential accepted %q", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("roleCredential(%q) = %v; want the refusal reason %q", tc.raw, err, tc.wantReason)
			}
		})
	}
}

// TestMigrateMaintenanceUnreachableIsSecretFree pins the unit-level half of the
// maintenance-database diagnostic: a pool that cannot be built for the
// maintenance connection refuses with the fixed, secret-free message and never
// renders the administrator URL, the role passwords or the rejected setting.
// The component suite proves the live-server path; this proves the command's
// own message without one.
func TestMigrateMaintenanceUnreachableIsSecretFree(t *testing.T) {
	values := migrateEnv(t)
	// The maintenance database differs from the target, so the command takes
	// the lock path and builds this pool first. The connector is replaced with
	// one whose private cause quotes the whole maintenance URL: the command
	// must still render only its fixed diagnostic.
	values["KODAMA_ADMIN_DATABASE_URL"] = "postgres://kodama_admin:SENTINEL-MAINTENANCE-PW@db:5432/postgres"
	setMigrateEnv(t, values)

	previous := newMaintenanceDB
	newMaintenanceDB = func(pgdriver.Config) (*pgdriver.DB, error) {
		return nil, fmt.Errorf("dial %s failed: SENTINEL-MAINTENANCE-CAUSE", values["KODAMA_ADMIN_DATABASE_URL"])
	}
	t.Cleanup(func() { newMaintenanceDB = previous })

	var stdout, stderr lockedBuffer
	code := runMigrateEntry(context.Background(), nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d; want 1 (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "maintenance database is unreachable") {
		t.Fatalf("stderr = %q; want the fixed maintenance-database diagnostic", stderr.String())
	}
	for _, secret := range []string{
		"SENTINEL-ADMIN-PW", "SENTINEL-RUNTIME-PW", "SENTINEL-CONTROLLER-PW",
		"SENTINEL-PUBLISHER-PW", "SENTINEL-AUTH-PW", "SENTINEL-MAINTENANCE-PW",
		"SENTINEL-MAINTENANCE-CAUSE",
	} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatalf("maintenance-database refusal rendered %q: %q", secret, stdout.String()+stderr.String())
		}
	}
}
