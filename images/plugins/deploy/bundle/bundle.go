// Package bundle reads the bounded, immutable artifact archive consumed by
// deployment drivers. It never executes or follows archive links.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const (
	MaxBytes   = 16 << 20
	MaxEntries = 128
	MaxFile    = 8 << 20
)

// Files is the normalized archive content.
type Files map[string][]byte

// Read extracts a tar or tar.gz archive into memory.
func Read(filename string) (Files, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = io.LimitReader(f, MaxBytes+1)
	head := make([]byte, 2)
	n, _ := io.ReadFull(r, head)
	if n == 2 && head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(io.MultiReader(strings.NewReader(string(head)), r))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	} else {
		r = io.MultiReader(strings.NewReader(string(head[:n])), r)
	}
	tr := tar.NewReader(r)
	out := Files{}
	entries := 0
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		entries++
		if entries > MaxEntries {
			return nil, fmt.Errorf("bundle: too many entries")
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
			return nil, fmt.Errorf("bundle: unsafe path %q", hdr.Name)
		}
		if hdr.Size < 0 || hdr.Size > MaxFile {
			return nil, fmt.Errorf("bundle: file %q is too large", name)
		}
		if total+hdr.Size > MaxBytes {
			return nil, fmt.Errorf("bundle: archive is too large")
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxFile+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > MaxFile {
			return nil, fmt.Errorf("bundle: file %q is too large", name)
		}
		total += int64(len(data))
		out[name] = data
	}
	return out, nil
}

// Manifest returns the required Kubernetes/plugin manifest.
func (f Files) Manifest() ([]byte, bool) {
	for _, name := range []string{"manifest.yaml", "manifest.yml"} {
		if data, ok := f[name]; ok {
			return data, true
		}
	}
	return nil, false
}
