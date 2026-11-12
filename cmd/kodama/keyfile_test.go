package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

func keyfileEnv(path string, overrides map[string]string) productionEnv {
	values := map[string]string{
		"KODAMA_KEY_ADAPTER": "keyfile",
		"KODAMA_KEY_REF":     "keyfile:kek-v1",
		"KODAMA_KEYFILE":     path,
	}
	maps.Copy(values, overrides)
	return func(name string) string { return values[name] }
}

// failingReader fails after delivering n bytes.
type failingReader struct{ n int }

func (reader *failingReader) Read(buffer []byte) (int, error) {
	if reader.n <= 0 {
		return 0, errors.New("entropy source failed")
	}
	count := min(len(buffer), reader.n)
	reader.n -= count
	return count, nil
}

func directoryEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestTestKeyfileInitializationAndLoading(t *testing.T) {
	t.Run("initializes once with owner-only permissions and reloads stably", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "keys")
		path := filepath.Join(parent, "kek-v1.key")
		if err := initializeTestKeyfile(path, rand.Reader); err != nil {
			t.Fatalf("initialize: %v", err)
		}
		dirInfo, err := os.Stat(parent)
		if err != nil || dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("new key directory mode = %v (%v); want 0700", dirInfo.Mode().Perm(), err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != secret.DataKeyBytes {
			t.Fatalf("keyfile = %v %v size %d; want a regular 0600 file of %d bytes", err, info.Mode(), info.Size(), secret.DataKeyBytes)
		}
		if names := directoryEntries(t, parent); len(names) != 1 {
			t.Fatalf("key directory = %q; want only the published key", names)
		}
		original, _ := os.ReadFile(path)
		if bytes.Equal(original, make([]byte, secret.DataKeyBytes)) {
			t.Fatal("the generated key is all zero")
		}

		first, err := loadTestKeys(keyfileEnv(path, nil))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if first.ref != "keyfile:kek-v1" || first.version != secret.MinKeyVersion || first.path != path {
			t.Fatalf("loaded keys = %q v%d %q", first.ref, first.version, first.path)
		}
		cipher, _ := keys.NewCipher(first.provider, rand.Reader)
		associated := secret.AssociatedData{InstallationID: "i", TenantID: "tenant-1", SecretID: "s", Name: "N", KeyVersion: 1}
		envelope, err := cipher.Seal(context.Background(), associated, []byte("payload"))
		if err != nil {
			t.Fatal(err)
		}
		second, err := loadTestKeys(keyfileEnv(path, map[string]string{"KODAMA_KEY_CURRENT_VERSION": "1"}))
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		reopened, _ := keys.NewCipher(second.provider, rand.Reader)
		if plaintext, err := reopened.Open(context.Background(), associated, envelope); err != nil || string(plaintext) != "payload" {
			t.Fatalf("reload open = %q, %v; want the sealed payload", plaintext, err)
		}

		if err := initializeTestKeyfile(path, rand.Reader); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("second initialization = %v; want an already-exists refusal", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(original, after) {
			t.Fatal("a second initialization replaced the existing key")
		}
		if names := directoryEntries(t, parent); len(names) != 1 {
			t.Fatalf("key directory after a refused initialization = %q", names)
		}
	})

	t.Run("an existing parent directory keeps its mode", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := initializeTestKeyfile(filepath.Join(parent, "k.key"), rand.Reader); err != nil {
			t.Fatal(err)
		}
		if info, _ := os.Stat(parent); info.Mode().Perm() != 0o750 {
			t.Fatalf("existing directory mode changed to %v", info.Mode().Perm())
		}
	})

	t.Run("never replaces a symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "kek.key")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := initializeTestKeyfile(link, rand.Reader); err == nil {
			t.Fatal("initialization replaced a symlink")
		}
		if data, _ := os.ReadFile(target); string(data) != "target" {
			t.Fatal("initialization wrote through a symlink")
		}
		if _, err := loadTestKeys(keyfileEnv(link, nil)); err == nil {
			t.Fatal("a symlinked keyfile was loaded")
		}
	})

	t.Run("concurrent initialization has one winner", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "kek.key")
		const racers = 8
		var wg sync.WaitGroup
		results := make([]error, racers)
		for index := range racers {
			wg.Go(func() {
				results[index] = initializeTestKeyfile(path, rand.Reader)
			})
		}
		wg.Wait()
		winners := 0
		for _, err := range results {
			if err == nil {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("winners = %d (%v); want exactly one", winners, results)
		}
		if names := directoryEntries(t, dir); len(names) != 1 || names[0] != "kek.key" {
			t.Fatalf("directory = %q; want only the published key and no temporary files", names)
		}
		if _, err := loadTestKeys(keyfileEnv(path, nil)); err != nil {
			t.Fatalf("load after the race: %v", err)
		}
	})

	t.Run("an entropy failure publishes nothing and cleans up", func(t *testing.T) {
		for _, reader := range []io.Reader{&failingReader{n: 0}, &failingReader{n: 7}} {
			dir := t.TempDir()
			path := filepath.Join(dir, "kek.key")
			if err := initializeTestKeyfile(path, reader); err == nil {
				t.Fatal("initialization succeeded without entropy")
			}
			if names := directoryEntries(t, dir); len(names) != 0 {
				t.Fatalf("directory after a failed initialization = %q; want empty", names)
			}
		}
	})

	t.Run("loading never initializes and refuses unusable keys", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "missing.key")
		if _, err := loadTestKeys(keyfileEnv(missing, nil)); err == nil {
			t.Fatal("a missing keyfile was accepted")
		}
		if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("loading created a keyfile")
		}
		short := filepath.Join(dir, "short.key")
		_ = os.WriteFile(short, []byte("short"), 0o600)
		permissive := filepath.Join(dir, "open.key")
		_ = os.WriteFile(permissive, make([]byte, secret.DataKeyBytes), 0o644)
		_ = os.Chmod(permissive, 0o644)
		for _, path := range []string{short, permissive} {
			if _, err := loadTestKeys(keyfileEnv(path, nil)); err == nil {
				t.Fatalf("an unusable keyfile %s was accepted", filepath.Base(path))
			}
		}
	})

	t.Run("settings are explicit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kek.key")
		if err := initializeTestKeyfile(path, rand.Reader); err != nil {
			t.Fatal(err)
		}
		for _, testCase := range []struct {
			overrides map[string]string
			want      string
		}{
			{map[string]string{"KODAMA_KEY_ADAPTER": ""}, "KODAMA_KEY_ADAPTER"},
			{map[string]string{"KODAMA_KEY_ADAPTER": "scw-kms"}, "KODAMA_KEY_ADAPTER"},
			{map[string]string{"KODAMA_KEY_REF": ""}, "KODAMA_KEY_REF"},
			{map[string]string{"KODAMA_KEY_REF": "scw-kms:k"}, "KODAMA_KEY_REF"},
			{map[string]string{"KODAMA_KEY_REF": "keyfile: k"}, "KODAMA_KEY_REF"},
			{map[string]string{"KODAMA_KEYFILE": ""}, "KODAMA_KEYFILE"},
			{map[string]string{"KODAMA_KEYFILE": "relative.key"}, "KODAMA_KEYFILE"},
			{map[string]string{"KODAMA_KEY_CURRENT_VERSION": "2"}, "KODAMA_KEY_CURRENT_VERSION"},
			{map[string]string{"KODAMA_KEY_CURRENT_VERSION": "one"}, "KODAMA_KEY_CURRENT_VERSION"},
		} {
			if _, err := loadTestKeys(keyfileEnv(path, testCase.overrides)); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("settings %v = %v; want a refusal naming %s", testCase.overrides, err, testCase.want)
			}
		}
	})
}
