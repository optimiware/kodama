//go:build integration

package identity_test

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// secretRule describes one credential value and where it may legitimately
// appear. An empty allowed path means the value must appear nowhere.
type secretRule struct {
	label   string
	value   string
	allowed string
}

// findPersistedSecrets walks every regular file under root and reports each
// rule violation. It is deliberately independent of the fixtures' own safe
// records so a product that writes credential material to disk is detected.
func findPersistedSecrets(root string, rules []secretRule) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, rule := range rules {
			if rule.value == "" || !bytes.Contains(data, []byte(rule.value)) {
				continue
			}
			if rule.allowed != "" && sameFile(path, rule.allowed) {
				continue
			}
			violations = append(violations, fmt.Sprintf("%s appears in %s", rule.label, path))
		}
		return nil
	})
	return violations, err
}

func requirePersistedSecretsOnly(t *testing.T, root string, rules ...secretRule) {
	t.Helper()
	violations, err := findPersistedSecrets(root, rules)
	if err != nil {
		t.Fatalf("scan persisted secrets: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("secret persistence violations: %s", strings.Join(violations, "; "))
	}
}

// findLeakedSecrets reports every diagnostic text that renders a rule value.
// It never prints the value itself.
func findLeakedSecrets(texts []string, rules []secretRule) []string {
	var violations []string
	for index, text := range texts {
		for _, rule := range rules {
			if rule.value == "" || !strings.Contains(text, rule.value) {
				continue
			}
			violations = append(violations, fmt.Sprintf("%s rendered in diagnostic %d", rule.label, index))
		}
	}
	return violations
}

func requireNoLeakedSecrets(t *testing.T, label string, texts []string, rules ...secretRule) {
	t.Helper()
	if violations := findLeakedSecrets(texts, rules); len(violations) > 0 {
		t.Fatalf("%s leaked credential material: %s", label, strings.Join(violations, "; "))
	}
}

// sameFile reports whether both paths name the same existing file.
func sameFile(left, right string) bool {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

// leakyFiles is a deliberately leaky stand-in for the kodactl filesystem port:
// it persists an extra sidecar copy of every session write. It exists only to
// prove the persistence scanner can fail.
type leakyFiles struct {
	cli.FileSystem
}

func (files leakyFiles) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if err := files.FileSystem.WriteFile(name, data, perm); err != nil {
		return err
	}
	return files.FileSystem.WriteFile(name+".diagnostic", data, perm)
}

// ReplaceFile leaks the same sidecar through the durable-replace path the
// session store uses, so the persistence scanner stays non-vacuous.
func (files leakyFiles) ReplaceFile(name string, data []byte, perm fs.FileMode) error {
	if err := files.FileSystem.ReplaceFile(name, data, perm); err != nil {
		return err
	}
	return files.FileSystem.WriteFile(name+".diagnostic", data, perm)
}

// leakyTransport is a deliberately leaky stand-in for the public transport: it
// renders the credential it was given. It exists only to prove the diagnostic
// scanner can fail.
type leakyTransport struct {
	secret string
}

func (transport leakyTransport) Invoke(context.Context, api.Invocation) (api.Result, error) {
	return api.Result{}, fmt.Errorf("request failed while authenticating %s", transport.secret)
}
