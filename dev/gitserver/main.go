// gitserver is a development-only HTTPS/token fixture around Git's real smart
// HTTP backend. It deliberately has no product/provider management API.
package main

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"time"
)

const repository = "/fixture.git"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func token(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	data = bytes.TrimSpace(data)
	if err == nil && len(data) < 16 {
		err = fmt.Errorf("%s: fixture token is missing or too short", path)
	}
	return data, err
}

func run() error {
	read, err := token("/secrets/git-read-token")
	if err != nil {
		return err
	}
	write, err := token("/secrets/git-write-token")
	if err != nil {
		return err
	}
	if bytes.Equal(read, write) {
		return errors.New("read and write tokens must differ")
	}
	if _, err := os.Stat("/data" + repository + "/HEAD"); os.IsNotExist(err) {
		if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", "/data"+repository).CombinedOutput(); err != nil {
			return fmt.Errorf("init: %w: %s", err, out)
		}
	}
	backend := &cgi.Handler{Path: "/usr/libexec/git-core/git-http-backend", Env: []string{"GIT_PROJECT_ROOT=/data", "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=fixture"}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, password, ok := r.BasicAuth()
		canWrite := ok && subtle.ConstantTimeCompare([]byte(password), write) == 1
		if !canWrite && !(ok && subtle.ConstantTimeCompare([]byte(password), read) == 1) {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Default deny: only the three smart-HTTP endpoints, with exactly one
		// service value so this gate and the backend cannot disagree about it.
		needsWrite := false
		switch r.URL.Path {
		case repository + "/info/refs":
			service := r.URL.Query()["service"]
			if len(service) != 1 || (service[0] != "git-upload-pack" && service[0] != "git-receive-pack") {
				http.Error(w, "unsupported service", http.StatusBadRequest)
				return
			}
			needsWrite = service[0] == "git-receive-pack"
		case repository + "/git-upload-pack":
		case repository + "/git-receive-pack":
			needsWrite = true
		default:
			http.NotFound(w, r)
			return
		}
		if needsWrite && !canWrite {
			http.Error(w, "read-only token", http.StatusForbidden)
			return
		}
		// Git sends packs above http.postBuffer chunked, which net/http/cgi rejects.
		if len(r.TransferEncoding) > 0 {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
			if err != nil {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		}
		r.Header.Del("Authorization") // do not expose the token to the CGI process
		backend.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: ":8443", Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute, IdleTimeout: time.Minute}
	return server.ListenAndServeTLS("/secrets/tls.crt", "/secrets/tls.key")
}
