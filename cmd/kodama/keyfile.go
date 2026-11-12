package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// testKeys is the validated local key material the test-profile server and the
// one-shot first-admin bootstrap share. The keyfile provider serves only
// provider-key version 1.
type testKeys struct {
	provider keys.Provider
	ref      secret.KeyRef
	path     string
	version  secret.KeyVersion
}

// testKeySettings validates the explicit keyfile selection. Nothing is
// inferred: the adapter, the exact `keyfile:<name>` reference and the absolute
// file path are all required, and a provider-key version other than 1 is
// refused because the keyfile adapter serves exactly one version.
func testKeySettings(getenv productionEnv) (secret.KeyRef, string, error) {
	if adapter := strings.TrimSpace(getenv("KODAMA_KEY_ADAPTER")); adapter != string(installation.KeyKeyfile) {
		return "", "", fmt.Errorf("test profile requires KODAMA_KEY_ADAPTER=%s", installation.KeyKeyfile)
	}
	reference := getenv("KODAMA_KEY_REF")
	name, ok := strings.CutPrefix(reference, installation.KeyRefKeyfilePrefix)
	if !ok || name == "" || strings.IndexFunc(name, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) >= 0 {
		return "", "", fmt.Errorf("test profile requires KODAMA_KEY_REF of the form %s<name>", installation.KeyRefKeyfilePrefix)
	}
	path := strings.TrimSpace(getenv("KODAMA_KEYFILE"))
	if path == "" || !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("test profile requires KODAMA_KEYFILE as an absolute path")
	}
	if version := strings.TrimSpace(getenv("KODAMA_KEY_CURRENT_VERSION")); version != "" && version != "1" {
		return "", "", fmt.Errorf("KODAMA_KEY_CURRENT_VERSION must be 1 for the keyfile adapter")
	}
	return secret.KeyRef(reference), filepath.Clean(path), nil
}

// loadTestKeys reads an existing keyfile. It never creates one: a missing key
// is a refusal, never a fresh installation.
func loadTestKeys(getenv productionEnv) (testKeys, error) {
	reference, path, err := testKeySettings(getenv)
	if err != nil {
		return testKeys{}, err
	}
	provider, err := keys.NewKeyfile(reference, path, rand.Reader)
	if err != nil {
		return testKeys{}, fmt.Errorf("test profile keyfile is unusable: %w", err)
	}
	return testKeys{provider: provider, ref: reference, path: path, version: secret.MinKeyVersion}, nil
}

// initializeTestKeyfile creates a new keyfile of exactly secret.DataKeyBytes
// random bytes. A missing parent directory is created owner-only; the key is
// written to an owner-only temporary sibling, synced and published with a hard
// link, which fails when the destination already exists. An existing key (or
// any existing path, including a symlink) is never replaced, so concurrent
// initializations have exactly one winner. Only the temporary file this call
// created is removed; a published key is never deleted.
func initializeTestKeyfile(path string, random io.Reader) error {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return fault.New(fault.InvalidInput, "keyfile path must be absolute")
	}
	if random == nil {
		return fault.New(fault.InvalidInput, "keyfile initialization requires a random source")
	}
	path = filepath.Clean(path)
	if _, err := os.Lstat(path); err == nil {
		return keyfileExists()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fault.New(fault.InvalidInput, "keyfile destination cannot be inspected")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fault.New(fault.DependencyUnavailable, "keyfile directory cannot be created")
	}
	temporary, err := os.CreateTemp(parent, ".kodama-keyfile-*")
	if err != nil {
		return fault.New(fault.DependencyUnavailable, "keyfile temporary file cannot be created")
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	key := make([]byte, secret.DataKeyBytes)
	defer clear(key)
	if _, err := io.ReadFull(random, key); err != nil {
		_ = temporary.Close()
		return fault.New(fault.DependencyUnavailable, "keyfile random source failed")
	}
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fault.New(fault.DependencyUnavailable, "keyfile temporary file cannot be restricted")
	}
	if _, err := temporary.Write(key); err != nil {
		_ = temporary.Close()
		return fault.New(fault.DependencyUnavailable, "keyfile cannot be written")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fault.New(fault.DependencyUnavailable, "keyfile cannot be synced")
	}
	if err := temporary.Close(); err != nil {
		return fault.New(fault.DependencyUnavailable, "keyfile cannot be closed")
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return keyfileExists()
		}
		return fault.New(fault.DependencyUnavailable, "keyfile cannot be published")
	}
	if directory, err := os.Open(parent); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func keyfileExists() error {
	return fault.New(fault.VersionConflict, "keyfile already exists; it is never replaced")
}
