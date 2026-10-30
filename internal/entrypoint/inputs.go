package entrypoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// materializeArtifactFiles resolves every declared artifactFiles entry before
// the command begins. Bytes are written under the private inputs root, never
// into shared scratch, and the digest is checked before the file is published
// to its final name.
func (a *attempt) materializeArtifactFiles() (*verdict, *abortError) {
	if len(a.c.ArtifactFiles) == 0 {
		return nil, nil
	}
	downloader, ok := a.s.rt.(ArtifactDownloader)
	if !ok {
		v := failed(ReasonArtifactUnavailable, nil)
		a.note("runtime does not expose artifact downloads")
		return &v, nil
	}
	if err := os.RemoveAll(a.s.cfg.InputsDir); err != nil {
		v := failed(ReasonArtifactUnavailable, nil)
		a.note("cannot clear inputs directory: %v", err)
		return &v, nil
	}
	if err := os.MkdirAll(a.s.cfg.InputsDir, 0o755); err != nil {
		v := failed(ReasonArtifactUnavailable, nil)
		a.note("cannot prepare inputs directory: %v", err)
		return &v, nil
	}
	for _, input := range a.c.ArtifactFiles {
		if err := input.Validate(); err != nil {
			v := failed(ReasonArtifactUnavailable, nil)
			a.note("invalid artifact input descriptor: %v", err)
			return &v, nil
		}
		resp, err := downloader.DownloadArtifact(a.runCtx, domainArtifact(input.ArtifactID))
		if err != nil {
			if a.authorityLost() {
				return nil, &abortError{ExitRevoked, context.Cause(a.runCtx)}
			}
			v := failed(ReasonArtifactUnavailable, nil)
			a.note("artifact input %s is unavailable: %v", input.Name, redactErr(err))
			return &v, nil
		}
		if err := a.downloadInput(input, resp); err != nil {
			v := failed(ReasonArtifactUnavailable, nil)
			a.note("artifact input %s failed verification: %v", input.Name, err)
			return &v, nil
		}
	}
	return nil, nil
}

func (a *attempt) downloadInput(input runtimev1.ArtifactInput, resp runtimev1.ArtifactReadResponse) error {
	if err := resp.Validate(); err != nil {
		return err
	}
	if resp.ArtifactID != domainArtifact(input.ArtifactID) || resp.Digest != input.Digest {
		return fmt.Errorf("server returned a different artifact identity")
	}
	ctx, cancel := context.WithTimeout(a.runCtx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, resp.Download.Method, resp.Download.URL, nil)
	if err != nil {
		return err
	}
	for k, v := range resp.Download.Headers {
		req.Header.Set(k, v)
	}
	respHTTP, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer respHTTP.Body.Close()
	if respHTTP.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", respHTTP.StatusCode)
	}
	target := filepath.Join(a.s.cfg.InputsDir, input.Name)
	tmp, err := os.CreateTemp(a.s.cfg.InputsDir, ".input-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(respHTTP.Body, runtimev1MaxInputBytes+1))
	if err != nil {
		tmp.Close()
		return err
	}
	if written > runtimev1MaxInputBytes {
		tmp.Close()
		return fmt.Errorf("artifact exceeds the input size ceiling")
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != string(input.Digest) {
		return fmt.Errorf("digest mismatch")
	}
	return os.Rename(tmpName, target)
}

// The artifact contract allows up to 1 TiB, but a container input is bounded
// well below that; local disk and the attempt deadline are the real limits.
const runtimev1MaxInputBytes int64 = 1 << 30

func domainArtifact(id string) (out domain.ArtifactID) { return domain.ArtifactID(id) }

// writeContextFile publishes the immutable context to the private identity
// directory for trusted plugins. It is not placed in shared scratch.
func (a *attempt) writeContextFile() error {
	raw, err := json.Marshal(a.c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.s.cfg.ContextFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.s.cfg.ContextFile, append(raw, '\n'), 0o600)
}
