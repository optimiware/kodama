package skill_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/userland/skill"
)

func writeSkillFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func newDirSource(t *testing.T, root string) *skill.DirSource {
	t.Helper()
	source, err := skill.NewDirSource(root)
	if err != nil {
		t.Fatalf("NewDirSource(%q): %v", root, err)
	}
	return source
}

func sortedNames(names []skill.Name) []skill.Name {
	sorted := append([]skill.Name(nil), names...)
	slices.Sort(sorted)
	return sorted
}

// TestDirSourceListsMarkdownOnly pins the confined enumeration: regular .md
// files with a valid name are listed; text files, upper-case extensions,
// nested files and symlinks are not.
func TestDirSourceListsMarkdownOnly(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeSkillFile(t, outside, []byte("# outside\n"))
	writeSkillFile(t, filepath.Join(root, "b.md"), []byte("# b\n"))
	writeSkillFile(t, filepath.Join(root, "a.md"), []byte("# a\n"))
	writeSkillFile(t, filepath.Join(root, "empty.md"), nil)
	writeSkillFile(t, filepath.Join(root, "notes.txt"), []byte("# notes\n"))
	writeSkillFile(t, filepath.Join(root, "README.MD"), []byte("# readme\n"))
	writeSkillFile(t, filepath.Join(root, "nested", "inner.md"), []byte("# inner\n"))
	if err := os.Symlink(outside, filepath.Join(root, "link.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	source := newDirSource(t, root)
	names, err := source.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []skill.Name{mustName(t, "a"), mustName(t, "b"), mustName(t, "empty")}
	if !reflect.DeepEqual(sortedNames(names), want) {
		t.Fatalf("List = %v; want %v", sortedNames(names), want)
	}

	loaded, err := source.Read(context.Background(), mustName(t, "a"))
	if err != nil {
		t.Fatalf("Read(a): %v", err)
	}
	if loaded.Name != mustName(t, "a") || loaded.Path != "a.md" {
		t.Fatalf("Read(a) = %+v; want name a and path a.md", loaded)
	}
	if string(loaded.Body) != "# a\n" {
		t.Fatalf("Read(a).Body = %q", loaded.Body)
	}
	for _, name := range []skill.Name{mustName(t, "notes"), mustName(t, "link"), mustName(t, "nested")} {
		if value, err := source.Read(context.Background(), name); err == nil {
			t.Errorf("Read(%q) = %+v; want refusal", name, value)
		}
	}
}

// TestDirSourceRejectsSymlinksInEveryComponent pins the no-follow boundary: a
// symlinked root, a symlinked parent component and a symlinked leaf all refuse.
func TestDirSourceRejectsSymlinksInEveryComponent(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "real.md"), []byte("# real\n"))
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeSkillFile(t, outside, []byte("# outside\n"))
	if err := os.Symlink(outside, filepath.Join(root, "escape.md")); err != nil {
		t.Fatalf("Symlink leaf: %v", err)
	}

	source := newDirSource(t, root)
	if value, err := source.Read(context.Background(), mustName(t, "escape")); err == nil {
		t.Fatalf("Read followed a symlinked leaf: %+v", value)
	}

	linkRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, linkRoot); err != nil {
		t.Fatalf("Symlink root: %v", err)
	}
	if created, err := skill.NewDirSource(linkRoot); err == nil {
		t.Fatalf("NewDirSource followed a symlinked root: %+v", created)
	}

	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeSkillFile(t, filepath.Join(real, "sub", "keep.md"), []byte("# keep\n"))
	linkParent := filepath.Join(base, "link")
	if err := os.Symlink(real, linkParent); err != nil {
		t.Fatalf("Symlink parent: %v", err)
	}
	if created, err := skill.NewDirSource(filepath.Join(linkParent, "sub")); err == nil {
		t.Fatalf("NewDirSource followed a symlinked parent component: %+v", created)
	}
}

// TestDirSourceRejectsTraversalNames pins confinement by name: traversal,
// absolute, nested, dotted and extension-bearing names refuse before any read.
func TestDirSourceRejectsTraversalNames(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "inside.md"), []byte("# inside\n"))
	outside := filepath.Join(filepath.Dir(root), "escape.md")
	writeSkillFile(t, outside, []byte("# escape\n"))
	t.Cleanup(func() { _ = os.Remove(outside) })

	source := newDirSource(t, root)
	for _, name := range []skill.Name{"../escape", "..", ".", "a/b", "/inside", "inside.md", "inside ", "UPPER", ""} {
		if value, err := source.Read(context.Background(), name); err == nil {
			t.Errorf("Read(%q) = %+v; want refusal", name, value)
		}
	}
}

// TestDirSourceBoundsBody pins the 1 MiB read bound at the exact boundary.
func TestDirSourceBoundsBody(t *testing.T) {
	root := t.TempDir()
	exact := make([]byte, skill.MaxBodyBytes)
	for index := range exact {
		exact[index] = 'a'
	}
	writeSkillFile(t, filepath.Join(root, "exact.md"), exact)
	writeSkillFile(t, filepath.Join(root, "over.md"), append(append([]byte(nil), exact...), 'b'))

	source := newDirSource(t, root)
	loaded, err := source.Read(context.Background(), mustName(t, "exact"))
	if err != nil {
		t.Fatalf("Read(exact): %v", err)
	}
	if len(loaded.Body) != skill.MaxBodyBytes {
		t.Fatalf("Read(exact) returned %d bytes; want %d", len(loaded.Body), skill.MaxBodyBytes)
	}
	if value, err := source.Read(context.Background(), mustName(t, "over")); err == nil {
		t.Fatalf("Read(over) returned %d bytes; want refusal", len(value.Body))
	}
}

// TestDirSourceRejectsInvalidRoots pins root validation.
func TestDirSourceRejectsInvalidRoots(t *testing.T) {
	if source, err := skill.NewDirSource(""); err == nil {
		t.Fatalf("NewDirSource(\"\") = %+v; want refusal", source)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if source, err := skill.NewDirSource(missing); err == nil {
		t.Fatalf("NewDirSource(missing) = %+v; want refusal", source)
	}
	file := filepath.Join(t.TempDir(), "file")
	writeSkillFile(t, file, []byte("x"))
	if source, err := skill.NewDirSource(file); err == nil {
		t.Fatalf("NewDirSource(file) = %+v; want refusal", source)
	}
}

// TestDirSourceConfinesReadsAfterCheck pins the no-TOCTOU boundary: a symlink
// leaf is refused even inside the root, a leaf swapped after listing cannot
// redirect a read, and replacing the root path after construction keeps reads
// confined to the pinned directory.
func TestDirSourceConfinesReadsAfterCheck(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outsideDir := filepath.Join(base, "outside")
	writeSkillFile(t, filepath.Join(root, "inside.md"), []byte("# inside\n"))
	writeSkillFile(t, filepath.Join(root, "swap.md"), []byte("# swap\n"))
	writeSkillFile(t, filepath.Join(root, "internal.md"), []byte("# internal\n"))
	writeSkillFile(t, filepath.Join(outsideDir, "escape.md"), []byte("# outside\n"))
	writeSkillFile(t, filepath.Join(outsideDir, "swap.md"), []byte("# outside swap\n"))

	source := newDirSource(t, root)
	t.Cleanup(func() { _ = source.Close() })

	// A symlink leaf is refused even when its target stays inside the root.
	if err := os.Remove(filepath.Join(root, "internal.md")); err != nil {
		t.Fatalf("Remove(internal.md): %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "inside.md"), filepath.Join(root, "internal.md")); err != nil {
		t.Fatalf("Symlink internal leaf: %v", err)
	}
	if value, err := source.Read(context.Background(), mustName(t, "internal")); err == nil {
		t.Fatalf("Read followed an internal symlink leaf: %+v", value)
	}

	// Swap the leaf after listing; the read refuses instead of following.
	if _, err := source.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "swap.md")); err != nil {
		t.Fatalf("Remove(swap.md): %v", err)
	}
	if err := os.Symlink(filepath.Join(outsideDir, "swap.md"), filepath.Join(root, "swap.md")); err != nil {
		t.Fatalf("Symlink swap leaf: %v", err)
	}
	if value, err := source.Read(context.Background(), mustName(t, "swap")); err == nil {
		t.Fatalf("Read escaped through a swapped leaf: %+v", value)
	}

	// Replace the whole root path after construction; the pinned handle keeps
	// reads confined to the original directory.
	if err := os.Rename(root, filepath.Join(base, "root-original")); err != nil {
		t.Fatalf("Rename root: %v", err)
	}
	if err := os.Symlink(outsideDir, root); err != nil {
		t.Fatalf("Symlink replaced root: %v", err)
	}
	if value, err := source.Read(context.Background(), mustName(t, "escape")); err == nil {
		t.Fatalf("Read escaped through a replaced root: %+v", value)
	}
	if loaded, err := source.Read(context.Background(), mustName(t, "inside")); err != nil || string(loaded.Body) != "# inside\n" {
		t.Fatalf("Read from the pinned root = %+v, %v; want the original inside.md", loaded, err)
	}
}

// TestDirSourceRejectsNonRegularLeaf pins that a directory named like a skill
// file is not a Markdown skill.
func TestDirSourceRejectsNonRegularLeaf(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir.md"), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	source := newDirSource(t, root)
	if value, err := source.Read(context.Background(), mustName(t, "dir")); err == nil {
		t.Fatalf("Read(dir) = %+v; want refusal", value)
	}
}
