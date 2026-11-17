package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	pgdriver "github.com/optimiware/kodema/internal/shell/app/drivers/postgres"
)

// migrateEnv is one complete, explicit `kodema migrate` environment. The
// passwords are sentinels so every refusal can assert they never render.
func migrateEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"KODEMA_ADMIN_DATABASE_URL":      "postgres://kodema_admin:SENTINEL-ADMIN-PW@db:5432/postgres",
		"KODEMA_DATABASE_URL":            "postgres://kodema_runtime:SENTINEL-RUNTIME-PW@db:5432/kodema",
		"KODEMA_CONTROLLER_DATABASE_URL": "postgres://kodema_controller:SENTINEL-CONTROLLER-PW@db:5432/kodema",
		"KODEMA_PUBLISHER_DATABASE_URL":  "postgres://kodema_publisher:SENTINEL-PUBLISHER-PW@db:5432/kodema",
		"KODEMA_AUTH_DATABASE_URL":       "postgres://kodema_auth:SENTINEL-AUTH-PW@db:5432/kodema",
	}
}

func setMigrateEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for _, name := range []string{
		"KODEMA_ADMIN_DATABASE_URL", "KODEMA_DATABASE_URL", "KODEMA_CONTROLLER_DATABASE_URL",
		"KODEMA_PUBLISHER_DATABASE_URL", "KODEMA_AUTH_DATABASE_URL",
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
		{"admin URL missing", "KODEMA_ADMIN_DATABASE_URL", "", "", "KODEMA_ADMIN_DATABASE_URL", "must be a postgres:// URL"},
		{"admin URL has no user", "KODEMA_ADMIN_DATABASE_URL", "postgres://db:5432/postgres?password=SENTINEL-ADMIN-NOUSER", "SENTINEL-ADMIN-NOUSER", "KODEMA_ADMIN_DATABASE_URL", "must name a database user"},
		{"admin URL has an empty user", "KODEMA_ADMIN_DATABASE_URL", "postgres://:SENTINEL-ADMIN-EMPTY-USER@db:5432/postgres", "SENTINEL-ADMIN-EMPTY-USER", "KODEMA_ADMIN_DATABASE_URL", "must name a database user"},
		{"admin URL has no password", "KODEMA_ADMIN_DATABASE_URL", "postgres://admin@db:5432/postgres?password=SENTINEL-ADMIN-NO-PASSWORD", "SENTINEL-ADMIN-NO-PASSWORD", "KODEMA_ADMIN_DATABASE_URL", "must carry a database password"},
		{"admin URL has an empty password", "KODEMA_ADMIN_DATABASE_URL", "postgres://admin:@db:5432/postgres", "", "KODEMA_ADMIN_DATABASE_URL", "must carry a database password"},
		{"admin URL has no maintenance database", "KODEMA_ADMIN_DATABASE_URL", "postgres://admin:SENTINEL-ADMIN-NODB@db:5432/", "SENTINEL-ADMIN-NODB", "KODEMA_ADMIN_DATABASE_URL", "must name a maintenance database"},
		{"admin URL is not postgres", "KODEMA_ADMIN_DATABASE_URL", "mysql://admin:SENTINEL-ADMIN-SCHEME@db/postgres", "SENTINEL-ADMIN-SCHEME", "KODEMA_ADMIN_DATABASE_URL", "must be a postgres:// URL"},
		{"target URL missing", "KODEMA_DATABASE_URL", "", "", "KODEMA_DATABASE_URL", "must name the target database"},
		{"target URL has no database", "KODEMA_DATABASE_URL", "postgres://runtime:SENTINEL-TARGET-NODB@db:5432/", "SENTINEL-TARGET-NODB", "KODEMA_DATABASE_URL", "must name the target database"},
		{"runtime user missing", "KODEMA_DATABASE_URL", "postgres://db:5432/kodema?password=SENTINEL-RUNTIME-NOUSER", "SENTINEL-RUNTIME-NOUSER", "KODEMA_DATABASE_URL", "must name a database user"},
		{"runtime password missing", "KODEMA_DATABASE_URL", "postgres://kodema_runtime@db:5432/kodema?password=SENTINEL-RUNTIME-NOPW", "SENTINEL-RUNTIME-NOPW", "KODEMA_DATABASE_URL", "has no password"},
		{"controller user missing", "KODEMA_CONTROLLER_DATABASE_URL", "postgres://db:5432/kodema?password=SENTINEL-CONTROLLER-NOUSER", "SENTINEL-CONTROLLER-NOUSER", "KODEMA_CONTROLLER_DATABASE_URL", "must name a database user"},
		{"controller password missing", "KODEMA_CONTROLLER_DATABASE_URL", "postgres://kodema_controller@db:5432/kodema?password=SENTINEL-CONTROLLER-NOPW", "SENTINEL-CONTROLLER-NOPW", "KODEMA_CONTROLLER_DATABASE_URL", "has no password"},
		{"publisher user missing", "KODEMA_PUBLISHER_DATABASE_URL", "postgres://db:5432/kodema?password=SENTINEL-PUBLISHER-NOUSER", "SENTINEL-PUBLISHER-NOUSER", "KODEMA_PUBLISHER_DATABASE_URL", "must name a database user"},
		{"publisher password missing", "KODEMA_PUBLISHER_DATABASE_URL", "postgres://kodema_publisher@db:5432/kodema?password=SENTINEL-PUBLISHER-NOPW", "SENTINEL-PUBLISHER-NOPW", "KODEMA_PUBLISHER_DATABASE_URL", "has no password"},
		{"publisher URL is not postgres", "KODEMA_PUBLISHER_DATABASE_URL", "http://kodema_publisher:SENTINEL-PUBLISHER-SCHEME@db:5432/kodema", "SENTINEL-PUBLISHER-SCHEME", "KODEMA_PUBLISHER_DATABASE_URL", "must be a postgres:// URL"},
		{"auth user missing", "KODEMA_AUTH_DATABASE_URL", "postgres://db:5432/kodema?password=SENTINEL-AUTH-NOUSER", "SENTINEL-AUTH-NOUSER", "KODEMA_AUTH_DATABASE_URL", "must name a database user"},
		{"auth password missing", "KODEMA_AUTH_DATABASE_URL", "postgres://kodema_auth@db:5432/kodema?password=SENTINEL-AUTH-NOPW", "SENTINEL-AUTH-NOPW", "KODEMA_AUTH_DATABASE_URL", "has no password"},
		{"auth role is not a kodema role", "KODEMA_AUTH_DATABASE_URL", "postgres://postgres:SENTINEL-AUTH-ROLE@db:5432/kodema", "SENTINEL-AUTH-ROLE", "KODEMA_AUTH_DATABASE_URL", "kodema_ login role"},
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
	values["KODEMA_CONTROLLER_DATABASE_URL"] = "postgres://kodema_runtime:SENTINEL-OTHER-PW@db:5432/kodema"
	setMigrateEnv(t, values)
	var stdout, stderr lockedBuffer
	code := runMigrateEntry(context.Background(), nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2 (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "different passwords for role kodema_runtime") {
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
		{"valid", "postgres://kodema_runtime:p%27w@db:5432/kodema?sslmode=verify-full", "kodema_runtime", "p'w", "", true},
		{"no password", "postgres://kodema_runtime@db/kodema", "", "", "has no password", false},
		{"empty password", "postgres://kodema_runtime:@db/kodema", "", "", "has no password", false},
		{"not postgres", "http://kodema_runtime:x@db/kodema", "", "", "must be a postgres:// URL", false},
		{"no user", "postgres://db/kodema", "", "", "must name a database user", false},
		{"empty user", "postgres://:x@db/kodema", "", "", "must name a database user", false},
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
	values["KODEMA_ADMIN_DATABASE_URL"] = "postgres://kodema_admin:SENTINEL-MAINTENANCE-PW@db:5432/postgres"
	setMigrateEnv(t, values)

	previous := newMaintenanceDB
	newMaintenanceDB = func(pgdriver.Config) (*pgdriver.DB, error) {
		return nil, fmt.Errorf("dial %s failed: SENTINEL-MAINTENANCE-CAUSE", values["KODEMA_ADMIN_DATABASE_URL"])
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
