package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// fakeFS is the injected filesystem for kodactl tests. It mirrors the
// documented cli.FileSystem seam: WriteFile writes in place (creating no
// parents), while ReplaceFile creates missing parents 0700, replaces the target
// atomically and leaves it untouched on failure.
type fakeFS struct {
	mu         sync.Mutex
	dirs       map[string]fs.FileMode
	files      map[string][]byte
	entries    map[string][]cli.DirEntry
	readErr    map[string]error
	dirErr     map[string]error
	writeErr   map[string]error
	replaceErr map[string]error
	writes     map[string][]byte
	replaces   map[string][]byte
	modes      map[string]fs.FileMode
	tempModes  []fs.FileMode
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		dirs:       map[string]fs.FileMode{"/": 0o755, ".": 0o755},
		files:      make(map[string][]byte),
		entries:    make(map[string][]cli.DirEntry),
		readErr:    make(map[string]error),
		dirErr:     make(map[string]error),
		writeErr:   make(map[string]error),
		replaceErr: make(map[string]error),
		writes:     make(map[string][]byte),
		replaces:   make(map[string][]byte),
		modes:      make(map[string]fs.FileMode),
	}
}

func (filesystem *fakeFS) ReadFile(name string) ([]byte, error) {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	if err := filesystem.readErr[name]; err != nil {
		return nil, err
	}
	data, ok := filesystem.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (filesystem *fakeFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	if err := filesystem.writeErr[name]; err != nil {
		return err
	}
	// os.WriteFile fails when the parent is missing and only applies perm when
	// it creates the file; an existing file keeps its mode.
	if _, ok := filesystem.dirs[path.Dir(name)]; !ok {
		return fs.ErrNotExist
	}
	if _, exists := filesystem.files[name]; !exists {
		filesystem.modes[name] = perm
	}
	filesystem.writes[name] = append([]byte(nil), data...)
	filesystem.files[name] = append([]byte(nil), data...)
	return nil
}

// ReplaceFile mirrors the declared seam: a directory target is refused, missing
// parents are created 0700 without changing an existing directory, the bytes
// go through an owner-only temporary file, and an injected failure removes the
// temporary file and keeps the previous target's bytes and mode. It records
// successful replaces separately from in-place writes.
func (filesystem *fakeFS) ReplaceFile(name string, data []byte, perm fs.FileMode) error {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	if _, isDir := filesystem.dirs[name]; isDir {
		return errors.New("replace: target is a directory")
	}
	filesystem.createParents(name)
	filesystem.tempModes = append(filesystem.tempModes, 0o600)
	if err := filesystem.replaceErr[name]; err != nil {
		return err
	}
	filesystem.replaces[name] = append([]byte(nil), data...)
	filesystem.files[name] = append([]byte(nil), data...)
	filesystem.modes[name] = perm
	return nil
}

// createParents records every missing ancestor of name as 0700 and never
// changes the mode of an existing directory.
func (filesystem *fakeFS) createParents(name string) {
	var missing []string
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		if _, ok := filesystem.dirs[dir]; ok {
			break
		}
		missing = append(missing, dir)
		if dir == "/" || dir == "." {
			break
		}
	}
	for _, m := range slices.Backward(missing) {
		filesystem.dirs[m] = 0o700
	}
}

// Remove mirrors the injected deletion seam: a directory is refused, a
// missing file reports fs.ErrNotExist, and a regular file is deleted.
func (filesystem *fakeFS) Remove(name string) error {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	if _, isDir := filesystem.dirs[name]; isDir {
		return errors.New("remove: target is a directory")
	}
	if _, ok := filesystem.files[name]; !ok {
		return fs.ErrNotExist
	}
	delete(filesystem.files, name)
	delete(filesystem.modes, name)
	delete(filesystem.writes, name)
	delete(filesystem.replaces, name)
	return nil
}

func (filesystem *fakeFS) ReadDir(name string) ([]cli.DirEntry, error) {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	if err := filesystem.dirErr[name]; err != nil {
		return nil, err
	}
	entries, ok := filesystem.entries[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]cli.DirEntry(nil), entries...), nil
}

// scriptedTransport records invocations and delegates each response.
type scriptedTransport struct {
	mu          sync.Mutex
	invocations []api.Invocation
	respond     func(ctx context.Context, index int, invocation api.Invocation) (api.Result, error)
}

func (transport *scriptedTransport) Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error) {
	transport.mu.Lock()
	index := len(transport.invocations)
	transport.invocations = append(transport.invocations, invocation)
	respond := transport.respond
	transport.mu.Unlock()
	if respond == nil {
		return api.Result{Operation: invocation.Operation, Body: json.RawMessage(`{}`)}, nil
	}
	return respond(ctx, index, invocation)
}

func (transport *scriptedTransport) recorded() []api.Invocation {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]api.Invocation(nil), transport.invocations...)
}

// scriptedSource is a scripted credential source.
type scriptedSource struct {
	mu          sync.Mutex
	credentials []client.Credential
	err         error
	calls       int
}

func (source *scriptedSource) Credential(context.Context) (client.Credential, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	if source.err != nil {
		return client.Credential{}, source.err
	}
	if len(source.credentials) == 0 {
		return client.Credential{}, nil
	}
	index := source.calls - 1
	if index >= len(source.credentials) {
		index = len(source.credentials) - 1
	}
	return source.credentials[index], nil
}

func (source *scriptedSource) callCount() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls
}

// fakeSessionStore records persisted credentials.
type fakeSessionStore struct {
	mu      sync.Mutex
	saved   []client.Credential
	saveErr error
	load    client.Credential
	loadErr error
}

func (store *fakeSessionStore) Load(context.Context) (client.Credential, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadErr != nil {
		return client.Credential{}, store.loadErr
	}
	return store.load, nil
}

func (store *fakeSessionStore) Save(_ context.Context, credential client.Credential) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.saveErr != nil {
		return store.saveErr
	}
	store.saved = append(store.saved, credential)
	return nil
}

func (store *fakeSessionStore) savedCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.saved)
}

// testNow is the authoritative CLI test clock.
func testNow() time.Time {
	return time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
}

// testConfig is a valid kodactl configuration.
func testConfig() cli.Config {
	return cli.Config{
		ServerURL:      "https://kodama.example.com",
		Project:        "shop",
		Output:         cli.OutputJSON,
		TrustedOrigins: []string{"https://git.example.com/shop"},
	}
}

// testPorts builds ports with buffers and a deterministic clock.
func testPorts(transport client.Transport) (cli.Ports, *bytes.Buffer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	return cli.Ports{
		Client: transport,
		Files:  newFakeFS(),
		Out:    out,
		Err:    errOut,
		Now:    testNow,
		Sleep:  func(time.Duration) {},
	}, out, errOut
}

// humanCredential is a current human session credential.
func humanCredential() client.Credential {
	return client.Credential{
		Kind:      client.KindSession,
		Token:     "kodex.human-1.signature",
		ExpiresAt: testNow().Add(time.Hour),
	}
}
