package suite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readToken(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file %s has mode %o; want 0600", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the token file: %v", err)
	}
	return string(data)
}

// TestWriteTokenFileReplacesAtomically proves a token file is replaced as a
// private file, leaves no temporary file behind and never becomes empty.
func TestWriteTokenFileReplacesAtomically(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "admin-token")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("seed the token file: %v", err)
	}
	if err := WriteTokenFile(path, "second\n"); err != nil {
		t.Fatalf("WriteTokenFile: %v", err)
	}
	if got := readToken(t, path); got != "second" {
		t.Fatalf("token file = %q; want the trimmed new token", got)
	}
	if err := WriteTokenFile(path, " \n"); err == nil {
		t.Fatal("an empty token was written")
	}
	if got := readToken(t, path); got != "second" {
		t.Fatalf("a refused write changed the token file to %q", got)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("token directory holds %d entries (error %v); want only the token file", len(entries), err)
	}
}

// TestTokenRefresherKeepsEveryFileCurrent proves a round refreshes every
// persona it can, reports the ones it cannot and leaves their previous token
// in place, and that the loop keeps refreshing until its context ends.
func TestTokenRefresherKeepsEveryFileCurrent(t *testing.T) {
	directory := t.TempDir()
	files := map[string]string{
		"admin":    filepath.Join(directory, "admin-token"),
		"engineer": filepath.Join(directory, "engineer-token"),
	}
	for persona, path := range files {
		if err := WriteTokenFile(path, persona+"-initial"); err != nil {
			t.Fatalf("seed %s: %v", persona, err)
		}
	}
	var round atomic.Int32
	refresher := TokenRefresher{
		Files: files,
		Mint: func(_ context.Context, persona string) (string, error) {
			if persona == "engineer" && round.Load() == 0 {
				return "", errors.New("oauthsonas is unreachable")
			}
			return persona + "-refreshed", nil
		},
	}
	err := refresher.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "persona engineer") || strings.Contains(err.Error(), "persona admin") {
		t.Fatalf("Refresh error = %v; want one naming only the engineer persona", err)
	}
	if got := readToken(t, files["admin"]); got != "admin-refreshed" {
		t.Fatalf("admin token = %q; want it refreshed although the engineer failed", got)
	}
	if got := readToken(t, files["engineer"]); got != "engineer-initial" {
		t.Fatalf("engineer token = %q; want the previous token kept", got)
	}

	round.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		refresher.Run(ctx, 5*time.Millisecond, func(err error) { t.Errorf("a healthy round reported %v", err) })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for readToken(t, files["engineer"]) != "engineer-refreshed" {
		if time.Now().After(deadline) {
			t.Fatal("the refresh loop never refreshed the engineer token")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh loop did not stop with its context")
	}
}

// stubTokenScript makes the working directory a scratch repository root whose
// scripts/oauthsonas-token prints a token naming its persona and the OAuthSonas
// settings it received.
func stubTokenScript(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	script := `#!/usr/bin/env bash
[ "$1" = broken ] && exit 9
printf 'token:%s:%s:%s:%s\n' "$1" "$OAUTHSONAS_URL" "$OAUTHSONAS_RESOLVE" "$OAUTHSONAS_ACCESS_CODE_FILE"
`
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatalf("create the scratch scripts directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, tokenScript), []byte(script), 0o755); err != nil {
		t.Fatalf("write the token script stub: %v", err)
	}
	t.Chdir(root)
}

// TestOpenStageTokensOwnsTheSecondTenantToken proves a stage run refreshes the
// persona files its runner acquired, acquires the second tenant's token into a
// private file of its own, and removes only that file when it closes.
func TestOpenStageTokensOwnsTheSecondTenantToken(t *testing.T) {
	stubTokenScript(t)
	installation := writeDescriptor(t, nil)
	adminFile := filepath.Join(t.TempDir(), "admin-token")
	if err := WriteTokenFile(adminFile, "runner-acquired"); err != nil {
		t.Fatalf("seed the admin token: %v", err)
	}
	values := map[string]string{
		"Q01_ADMIN_TOKEN_FILE":     adminFile,
		"Q02_SECOND_ADMIN_PERSONA": "tenant-b-admin",
	}
	tokens, err := OpenStageTokens(context.Background(), installation, environment(values))
	if err != nil {
		t.Fatalf("OpenStageTokens: %v", err)
	}
	const settings = ":https://oauthsonas.kodama-system.svc.cluster.local:8443:oauthsonas.kodama-system.svc.cluster.local:8443:127.0.0.1:/run/stage/access-code"
	if got := readToken(t, tokens.SecondAdminFile); got != "token:tenant-b-admin"+settings {
		t.Fatalf("second tenant token = %q; want one acquired with the descriptor's OAuthSonas settings", got)
	}
	if info, err := os.Stat(filepath.Dir(tokens.SecondAdminFile)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the owned token directory is not private: %v", err)
	}
	if got := readToken(t, adminFile); got != "runner-acquired" {
		t.Fatalf("opening the stage tokens rewrote the admin token to %q", got)
	}

	refresher, ok := tokens.Refresher()
	if !ok || len(refresher.Files) != 2 {
		t.Fatalf("refresher = %v files, usable %v; want the admin and second tenant files", len(refresher.Files), ok)
	}
	if err := refresher.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := readToken(t, adminFile); got != "token:admin"+settings {
		t.Fatalf("refreshed admin token = %q", got)
	}

	tokens.Close()
	if _, err := os.Stat(tokens.SecondAdminFile); !os.IsNotExist(err) {
		t.Fatalf("Close left the owned second tenant token behind: %v", err)
	}
	if _, err := os.Stat(adminFile); err != nil {
		t.Fatalf("Close removed the runner's admin token: %v", err)
	}
}

// TestOpenStageTokensLeavesExportedCredentialsAlone proves no token is
// acquired when the caller exported the second tenant's credential or named no
// second-tenant persona, that a failed acquisition is an error which renders no
// script output, and that a descriptor without OAuthSonas settings turns the
// refresh off.
func TestOpenStageTokensLeavesExportedCredentialsAlone(t *testing.T) {
	stubTokenScript(t)
	installation := writeDescriptor(t, nil)
	for _, values := range []map[string]string{
		{},
		{"Q02_SECOND_ADMIN_PERSONA": "tenant-b-admin", "Q02_SECOND_ADMIN_TOKEN": "exported-value"},
		{"Q02_SECOND_ADMIN_PERSONA": "tenant-b-admin", "Q02_SECOND_ADMIN_TOKEN_FILE": "/exported/second-admin-token"},
	} {
		tokens, err := OpenStageTokens(context.Background(), installation, environment(values))
		if err != nil || tokens.SecondAdminFile != "" {
			t.Errorf("environment %v: second tenant file %q, error %v; want none acquired", values, tokens.SecondAdminFile, err)
		}
		if _, ok := tokens.Refresher(); ok {
			t.Errorf("environment %v: a run that owns no persona file can refresh", values)
		}
	}

	_, err := OpenStageTokens(context.Background(), installation, environment(map[string]string{"Q02_SECOND_ADMIN_PERSONA": "broken"}))
	if err == nil || !strings.Contains(err.Error(), "persona broken") {
		t.Fatalf("a failed acquisition returned %v; want an error naming the persona", err)
	}
	_, err = OpenStageTokens(context.Background(), installation, environment(map[string]string{
		"Q01_ADMIN_TOKEN_FILE": "/run/stage/admin-token", "Q02_SECOND_ADMIN_PERSONA": "admin",
	}))
	if err == nil {
		t.Fatal("a second-tenant persona equal to a first-tenant persona was accepted")
	}

	withoutOAuth := writeDescriptor(t, func(document map[string]any) { delete(document, "oauthsonas") })
	tokens, err := OpenStageTokens(context.Background(), withoutOAuth, environment(map[string]string{"Q01_ADMIN_TOKEN_FILE": "/run/stage/admin-token"}))
	if err != nil {
		t.Fatalf("OpenStageTokens without OAuthSonas settings: %v", err)
	}
	if _, ok := tokens.Refresher(); ok {
		t.Fatal("a descriptor without OAuthSonas settings can refresh tokens")
	}
}
