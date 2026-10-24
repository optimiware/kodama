package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// BrowserOpener launches the user's browser at an authorization URL. It is a
// consumer-owned port so tests and the composition root can substitute it.
type BrowserOpener interface {
	Open(ctx context.Context, rawURL string) error
}

// BrowserFunc adapts a function to BrowserOpener.
type BrowserFunc func(ctx context.Context, rawURL string) error

// Open calls f.
func (f BrowserFunc) Open(ctx context.Context, rawURL string) error { return f(ctx, rawURL) }

// SystemBrowser opens URLs with the platform's default handler. Only http and
// https URLs are accepted so a crafted value can never name a local file or
// another scheme handler.
type SystemBrowser struct{}

var _ BrowserOpener = SystemBrowser{}

// Open starts the platform opener without waiting for the browser to exit.
func (SystemBrowser) Open(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("refusing to open a non-http(s) URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", rawURL)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", rawURL)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the browser opener: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
