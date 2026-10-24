package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.invalid/inventory/internal/config"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAPI(t *testing.T) {
	ok := `{"database":{"urlFile":"/run/secrets/db"},"oidc":{"issuer":"https://idp.example.invalid","audience":"inv"},"cursorKeyFile":"/run/secrets/cursor"}`
	c, err := config.LoadAPI(write(t, ok))
	if err != nil || c.HTTPAddr != ":8080" || c.Database.MaxConns != 10 {
		t.Fatalf("%v %+v", err, c)
	}
	for name, body := range map[string]string{
		"inline secret field": `{"database":{"url":"postgres://u:p@h/db"},"oidc":{"issuer":"https://i","audience":"a"},"cursorKeyFile":"x"}`,
		"missing everything":  `{}`,
		"bad duration":        `{"database":{"urlFile":"x"},"oidc":{"issuer":"https://i.example","audience":"a"},"cursorKeyFile":"x","shutdownTimeout":"forever"}`,
		"trailing data":       ok + `{}`,
	} {
		if _, err := config.LoadAPI(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	_, err = config.LoadAPI(write(t, `{}`))
	for _, want := range []string{"database.urlFile", "oidc.issuer", "oidc.audience", "cursorKeyFile"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("all problems are reported together; missing %q in %v", want, err)
		}
	}
}

func TestLoadWorker(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	ok := `{"database":{"urlFile":"/db"},"tenant":"` + tenant + `","serviceId":"svc-worker",
		"erp":{"baseUrl":"https://erp.example.invalid","scopes":["inventory:adjust","inventory:read"],"credentialFiles":{"` + tenant + `":"/tok"}}}`
	if _, err := config.LoadWorker(write(t, ok)); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"missing scope":  strings.Replace(ok, `"inventory:read"`, `"inventory:other"`, 1),
		"foreign tenant": strings.Replace(ok, `"credentialFiles":{"`+tenant, `"credentialFiles":{"22222222-2222-4222-8222-222222222222":"/x","`+tenant, 1),
		"no service id":  strings.Replace(ok, `"svc-worker"`, `""`, 1),
		"bad tenant":     strings.Replace(ok, `"tenant":"`+tenant+`"`, `"tenant":"acme"`, 1),
	} {
		if _, err := config.LoadWorker(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadMigrateAndSecrets(t *testing.T) {
	if _, err := config.LoadMigrate(write(t, `{"database":{"urlFile":"/db"},"runtimeRole":"x\"; drop"}`)); err == nil {
		t.Error("an unsafe role name must be refused")
	}
	if _, err := config.LoadMigrate(write(t, `{"database":{"urlFile":"/db"},"runtimeRole":"inventory_runtime"}`)); err != nil {
		t.Error(err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "s")
	_ = os.WriteFile(good, []byte("value\n"), 0o600)
	if v, err := config.ReadSecret(good); err != nil || v != "value" {
		t.Fatalf("%q %v", v, err)
	}
	empty := filepath.Join(dir, "e")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	if _, err := config.ReadSecret(empty); err == nil {
		t.Error("an empty secret must fail")
	}
	if _, err := config.ReadSecret(dir); err == nil {
		t.Error("a directory is not a secret file")
	}
	if _, err := config.ReadSecret(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing secret must fail")
	}
}

// The shipped examples must stay loadable: they are the documented contract.
func TestShippedExamplesAreValid(t *testing.T) {
	if _, err := config.LoadAPI("../../config/api.example.json"); err != nil {
		t.Error(err)
	}
	if _, err := config.LoadWorker("../../config/worker.example.json"); err != nil {
		t.Error(err)
	}
	if _, err := config.LoadMigrate("../../config/migrate.example.json"); err != nil {
		t.Error(err)
	}
}
