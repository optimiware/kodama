package entrypoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

var (
	ErrInputPathUnsafe = errors.New("entrypoint: input destination path is unsafe")
	ErrInputExists     = errors.New("entrypoint: destination file already exists")
	ErrInputDigest     = errors.New("entrypoint: downloaded input digest or size mismatch")
)

// InputFetcher is the consumer-owned port for fetching an admitted artifact input
// and downloading its verified bytes.
type InputFetcher interface {
	FetchInput(ctx context.Context, req artifactsv1.InputFetchRequest) (artifactsv1.InputFetchResponse, error)
	DownloadInput(ctx context.Context, target runtimev1.Upload) (io.ReadCloser, error)
}

// FetchArtifactInput resolves an admitted file artifact input, streams its bytes
// into a private temporary file under destRoot, verifies the exact size and
// SHA-256 digest before making the file visible, and atomically places it at
// relPath inside destRoot. It never extracts archives or writes outside destRoot.
func FetchArtifactInput(ctx context.Context, fetcher InputFetcher, destRoot, relPath string, req artifactsv1.InputFetchRequest, overwrite bool) (artifactsv1.InputFetchResponse, error) {
	if fetcher == nil {
		return artifactsv1.InputFetchResponse{}, errors.New("entrypoint: an input fetcher is required")
	}
	req.Download = true
	if err := req.Validate(); err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	targetPath, err := safeInputDestination(destRoot, relPath)
	if err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	if !overwrite {
		if _, err := os.Lstat(targetPath); err == nil {
			return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: %s", ErrInputExists, relPath)
		}
	}

	meta, err := fetcher.FetchInput(ctx, req)
	if err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	if meta.ArtifactID != req.ArtifactID || meta.Digest != req.Digest || meta.Kind != runtimev1.KindFile || meta.Download == nil || meta.SizeBytes == nil {
		return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: invalid input metadata response", ErrInputDigest)
	}
	wantSize := *meta.SizeBytes
	if wantSize < 0 || wantSize > (1<<40) {
		return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: invalid input size %d", ErrInputDigest, wantSize)
	}

	parentDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	// Re-verify parentDir has no symlink components escaping destRoot.
	if err := verifyNoSymlinkEscape(destRoot, parentDir); err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}

	rc, err := fetcher.DownloadInput(ctx, *meta.Download)
	if err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(parentDir, ".kodama-input-*")
	if err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(rc, wantSize+1))
	if err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	if n != wantSize {
		return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: downloaded %d bytes, want %d", ErrInputDigest, n, wantSize)
	}
	gotDigest := runtimev1.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
	if gotDigest != req.Digest {
		return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: downloaded digest %s != %s", ErrInputDigest, gotDigest, req.Digest)
	}
	if err := tmp.Sync(); err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	if err := tmp.Close(); err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	if !overwrite {
		if _, err := os.Lstat(targetPath); err == nil {
			return artifactsv1.InputFetchResponse{}, fmt.Errorf("%w: %s", ErrInputExists, relPath)
		}
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return artifactsv1.InputFetchResponse{}, err
	}
	cleanup = false
	return meta, nil
}

func safeInputDestination(destRoot, relPath string) (string, error) {
	if destRoot == "" || !filepath.IsAbs(destRoot) || relPath == "" || filepath.IsAbs(relPath) {
		return "", ErrInputPathUnsafe
	}
	cleanRoot := filepath.Clean(destRoot)
	for _, forbidden := range []string{"/kodama/secrets", "/kodama/identity", "/kodama/bin"} {
		if cleanRoot == forbidden || strings.HasPrefix(cleanRoot, forbidden+"/") {
			return "", ErrInputPathUnsafe
		}
	}
	cleanRel := filepath.Clean(relPath)
	if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, "../") || strings.Contains(cleanRel, "/../") {
		return "", ErrInputPathUnsafe
	}
	joined := filepath.Join(cleanRoot, cleanRel)
	if joined == cleanRoot || !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", ErrInputPathUnsafe
	}
	return joined, nil
}

func verifyNoSymlinkEscape(root, dir string) error {
	cleanRoot := filepath.Clean(root)
	cur := cleanRoot
	rel, err := filepath.Rel(cleanRoot, filepath.Clean(dir))
	if err != nil || strings.HasPrefix(rel, "..") {
		return ErrInputPathUnsafe
	}
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return ErrInputPathUnsafe
		}
	}
	return nil
}

// HTTPInputDownload downloads from a presigned GET target using the provided HTTP client.
func HTTPInputDownload(ctx context.Context, client *http.Client, target runtimev1.Upload) (io.ReadCloser, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, target.Method, target.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	return resp.Body, nil
}
