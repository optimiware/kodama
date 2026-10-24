package auth

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// Test_keyring_adapter_roundtrip exercises the real OS keyring. It is opt-in
// (KODAMA_TEST_OS_KEYRING=1) because headless CI has no unlocked keyring; its
// result is recorded separately from the headless acceptance tests. It uses a
// unique namespace and removes everything it writes.
func Test_keyring_adapter_roundtrip(t *testing.T) {
	if os.Getenv("KODAMA_TEST_OS_KEYRING") != "1" {
		t.Skip("set KODAMA_TEST_OS_KEYRING=1 to run against the real OS keyring")
	}
	service := fmt.Sprintf("t017-kodama-test-%d", time.Now().UnixNano())
	store := NewKeyringStore(SystemKeyring(), service)
	const key = "roundtrip"
	t.Cleanup(func() { _ = store.Delete(ctxbg(), key) })

	if _, err := store.Load(ctxbg(), key); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("fresh namespace: %v", err)
	}
	if err := store.Save(ctxbg(), key, testCred); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctxbg(), key)
	if err != nil || got.AccessToken != testCred.AccessToken {
		t.Fatalf("%+v %v", got, err)
	}
	if err := store.Delete(ctxbg(), key); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctxbg(), key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := store.Load(ctxbg(), key); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
