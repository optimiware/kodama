//go:build unix

package messaging

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFixtureContextModesAndTimestampsIgnoreAmbientUmask proves the
// materialized build context carries exactly the fixed modes and the fixed
// epoch timestamp, even when the process umask would otherwise strip permission
// bits and the parent directories were created with an ambient mtime. Removing
// the tree stamp pass leaves the nested directory at the wall-clock time;
// removing a file Chmod leaves the umask-truncated mode. Both mutants must fail
// this test.
func TestFixtureContextModesAndTimestampsIgnoreAmbientUmask(t *testing.T) {
	// A non-022 umask (0077) makes every plain os.WriteFile create 0600 bytes,
	// so an exact 0644/0444/0555 assertion can only pass through the explicit
	// Chmod.
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)

	inputs, err := NormalizeBuildInputs(FixtureInputs{
		BaseReference: "docker.io/library/buildpack-deps@" + fixtureTestBaseDigest,
		Marker:        "fixture-modes\n",
		ExtraFiles: []FixtureFile{
			// A zero mode is the documented default 0644.
			{Path: "data/nested.txt", Content: []byte("nested\n")},
			{Path: "readonly.txt", Content: []byte("readonly\n"), Mode: 0o444},
		},
	})
	if err != nil {
		t.Fatalf("normalize fixture inputs: %v", err)
	}

	root := filepath.Join(t.TempDir(), "context")
	if err := inputs.WriteContext(root); err != nil {
		t.Fatalf("write fixture context: %v", err)
	}

	epoch := time.Unix(FixtureEpochUnix, 0)
	modes := map[string]os.FileMode{
		"Containerfile":   0o644,
		"fixture.sh":      0o555,
		"marker":          0o444,
		"data":            0o755,
		"data/nested.txt": 0o644,
		"readonly.txt":    0o444,
	}
	for name, want := range modes {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %04o; want %04o (umask 0077 must not change the fixed context modes)", name, got, want)
		}
	}

	// The stamp pass must cover the root and every nested directory, not only
	// the files it wrote directly.
	for _, directory := range []string{"", "data"} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(directory)))
		if err != nil {
			t.Fatalf("stat directory %q: %v", directory, err)
		}
		if !info.ModTime().Equal(epoch) {
			t.Fatalf("directory %q mtime = %s; want the fixed epoch %s (the tree stamp pass must run after nested writes)",
				directory, info.ModTime(), epoch)
		}
	}
	for _, name := range []string{"Containerfile", "fixture.sh", "marker", "data/nested.txt", "readonly.txt"} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if !info.ModTime().Equal(epoch) {
			t.Fatalf("%s mtime = %s; want the fixed epoch %s", name, info.ModTime(), epoch)
		}
	}
}

// TestFixtureContainerfileCopiesOnlyPinnedAssets proves ExtraFiles are a
// descriptor-only input: the image recipe copies exactly fixture.sh and the
// marker, and the descriptor changes when extra files change while every image
// input (base, Containerfile, script, marker) stays byte-identical. Copying an
// extra file into the image would change the image digest for a descriptor
// difference the build controls do not account for, so this pin keeps the two
// identities distinct.
func TestFixtureContainerfileCopiesOnlyPinnedAssets(t *testing.T) {
	var copies []string
	for line := range strings.SplitSeq(string(fixtureContainerfile), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "COPY ") || strings.HasPrefix(trimmed, "ADD ") {
			copies = append(copies, trimmed)
		}
	}
	wantCopies := []string{
		"COPY fixture.sh /fixture.sh",
		"COPY marker /fixture/marker",
	}
	if strings.Join(copies, "\n") != strings.Join(wantCopies, "\n") {
		t.Fatalf("Containerfile copies %v; want exactly %v so ExtraFiles never reach the image", copies, wantCopies)
	}

	base := FixtureInputs{
		BaseReference: "docker.io/library/buildpack-deps@" + fixtureTestBaseDigest,
		Marker:        "fixture-extra\n",
	}
	plain, err := NormalizeBuildInputs(base)
	if err != nil {
		t.Fatalf("normalize inputs without extra files: %v", err)
	}
	withExtra := base
	withExtra.ExtraFiles = []FixtureFile{
		{Path: "extra/data.txt", Content: []byte("extra\n"), Mode: 0o644},
	}
	extra, err := NormalizeBuildInputs(withExtra)
	if err != nil {
		t.Fatalf("normalize inputs with extra files: %v", err)
	}

	if plain.Descriptor == extra.Descriptor {
		t.Fatal("ExtraFiles did not change the normalized build descriptor")
	}
	if plain.BaseReference != extra.BaseReference ||
		plain.ContainerfileDigest != extra.ContainerfileDigest ||
		plain.ScriptDigest != extra.ScriptDigest ||
		plain.MarkerDigest != extra.MarkerDigest {
		t.Fatalf("ExtraFiles changed an image input: base %q/%q containerfile %q/%q script %q/%q marker %q/%q",
			plain.BaseReference, extra.BaseReference,
			plain.ContainerfileDigest, extra.ContainerfileDigest,
			plain.ScriptDigest, extra.ScriptDigest,
			plain.MarkerDigest, extra.MarkerDigest)
	}
	if len(extra.Files) != 1 || extra.Files[0].Path != "extra/data.txt" {
		t.Fatalf("extra-file descriptor = %+v; want the extra/data.txt entry", extra.Files)
	}
}
