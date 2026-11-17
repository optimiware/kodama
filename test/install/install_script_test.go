// Package install runs install.sh, at the root of the repository, against a
// local release.
package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installRelease is a release as install.sh downloads it: the two archives of
// this platform and SHA256SUMS, served like github.com/<repo>/releases.
type installRelease struct {
	version string
	files   map[string][]byte
	latest  int
}

// newInstallRelease builds the archives scripts/dist would name for this
// platform. Each binary is a shell script that prints its name and version.
func newInstallRelease(t *testing.T, version string) *installRelease {
	t.Helper()
	release := &installRelease{version: version, files: map[string][]byte{}}
	var sums strings.Builder
	for _, binary := range []string{"kodema", "kodactl"} {
		name := fmt.Sprintf("%s_%s_%s_%s", binary, version, runtime.GOOS, runtime.GOARCH)
		script := []byte("#!/bin/sh\necho " + binary + " " + version + "\n")
		var archive bytes.Buffer
		compressed := gzip.NewWriter(&archive)
		writer := tar.NewWriter(compressed)
		if err := writer.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteHeader(&tar.Header{Name: name + "/" + binary, Mode: 0o755, Size: int64(len(script))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(script); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
		release.files[name+".tar.gz"] = archive.Bytes()
		digest := sha256.Sum256(archive.Bytes())
		sums.WriteString(hex.EncodeToString(digest[:]) + "  " + name + ".tar.gz\n")
	}
	release.files["SHA256SUMS"] = []byte(sums.String())
	return release
}

func (release *installRelease) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tag := "v" + release.version
	switch path := request.URL.Path; {
	case path == "/releases/latest":
		release.latest++
		http.Redirect(writer, request, "/releases/tag/"+tag, http.StatusFound)
	case path == "/releases/tag/"+tag:
		_, _ = writer.Write([]byte("release page"))
	case strings.HasPrefix(path, "/releases/download/"+tag+"/"):
		body, ok := release.files[strings.TrimPrefix(path, "/releases/download/"+tag+"/")]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(body)
	default:
		http.NotFound(writer, request)
	}
}

// TestInstallScript runs install.sh against a local release: it installs both
// binaries of this platform into ~/.local/bin, and installs nothing when an
// archive is not the one SHA256SUMS names.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh supports linux and darwin")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("install.sh supports amd64 and arm64")
	}
	for _, tool := range []string{"sh", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}

	// install runs the script with an empty home and returns the directory it
	// installs into by default, its output and whether it succeeded.
	install := func(t *testing.T, release *installRelease, env ...string) (string, string, bool) {
		t.Helper()
		server := httptest.NewServer(release)
		t.Cleanup(server.Close)
		home := t.TempDir()
		command := exec.Command("sh", script)
		command.Env = append([]string{
			"HOME=" + home,
			"PATH=" + os.Getenv("PATH"),
			"KODEMA_RELEASES_URL=" + server.URL + "/releases",
		}, env...)
		output, err := command.CombinedOutput()
		return filepath.Join(home, ".local", "bin"), string(output), err == nil
	}
	installed := func(directory string) []string {
		entries, _ := os.ReadDir(directory)
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}

	t.Run("installs the latest release", func(t *testing.T) {
		release := newInstallRelease(t, "9.8.7")
		directory, output, ok := install(t, release)
		if !ok {
			t.Fatalf("install.sh failed: %s", output)
		}
		for _, binary := range []string{"kodema", "kodactl"} {
			printed, err := exec.Command(filepath.Join(directory, binary)).Output()
			if err != nil || string(printed) != binary+" 9.8.7\n" {
				t.Errorf("%s printed %q, %v; want the installed binary to run", binary, printed, err)
			}
		}
		if names := installed(directory); len(names) != 2 {
			t.Errorf("%s holds %v; want kodema and kodactl only", directory, names)
		}
		// The empty home's directory is not on PATH, and the script says so.
		if !strings.Contains(output, "is not on your PATH") {
			t.Errorf("output does not mention PATH: %s", output)
		}
	})

	t.Run("installs a named release into a named directory", func(t *testing.T) {
		release := newInstallRelease(t, "1.2.3-rc.1")
		target := filepath.Join(t.TempDir(), "bin")
		_, output, ok := install(t, release, "KODEMA_VERSION=v1.2.3-rc.1", "KODEMA_INSTALL_DIR="+target)
		if !ok {
			t.Fatalf("install.sh failed: %s", output)
		}
		if release.latest != 0 {
			t.Errorf("a named version asked for the latest release %d time(s)", release.latest)
		}
		if names := installed(target); len(names) != 2 {
			t.Errorf("%s holds %v; want kodema and kodactl", target, names)
		}
	})

	for _, refusal := range []struct {
		name    string
		break_  func(release *installRelease, archive string)
		message string
	}{
		{"an archive that does not match its digest", func(release *installRelease, archive string) {
			release.files[archive] = append(release.files[archive], 0)
		}, "does not match"},
		{"an archive SHA256SUMS does not list", func(release *installRelease, archive string) {
			var kept []string
			for _, line := range strings.Split(strings.TrimSpace(string(release.files["SHA256SUMS"])), "\n") {
				if !strings.HasSuffix(line, archive) {
					kept = append(kept, line)
				}
			}
			release.files["SHA256SUMS"] = []byte(strings.Join(kept, "\n") + "\n")
		}, "does not list"},
		{"a release without the archive", func(release *installRelease, archive string) {
			delete(release.files, archive)
		}, "has no kodactl"},
	} {
		t.Run("refuses "+refusal.name, func(t *testing.T) {
			release := newInstallRelease(t, "9.8.7")
			// kodactl is the second archive: kodema has passed by then.
			refusal.break_(release, fmt.Sprintf("kodactl_9.8.7_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH))
			directory, output, ok := install(t, release)
			if ok || !strings.Contains(output, refusal.message) {
				t.Fatalf("install.sh succeeded=%v with %q; want a refusal that says %q", ok, output, refusal.message)
			}
			if names := installed(directory); len(names) != 0 {
				t.Errorf("a refused install left %v in %s", names, directory)
			}
		})
	}

	t.Run("refuses a release that does not exist", func(t *testing.T) {
		release := newInstallRelease(t, "9.8.7")
		directory, output, ok := install(t, release, "KODEMA_VERSION=9.9.9")
		if ok || !strings.Contains(output, "release 9.9.9 was not found") {
			t.Fatalf("install.sh succeeded=%v with %q; want a refusal that names the release", ok, output)
		}
		if names := installed(directory); len(names) != 0 {
			t.Errorf("a refused install left %v in %s", names, directory)
		}
	})
}
