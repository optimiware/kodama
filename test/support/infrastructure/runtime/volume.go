package runtimesupport

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// WorkspaceLabel is the workspace ownership label.
const WorkspaceLabel = "kodama.dev/workspace"

// RunPodman runs one Podman CLI command with the explicit harness environment
// and no shell.
func RunPodman(ctx context.Context, args ...string) (harness.Result, error) {
	return harness.ExecRunner{}.Run(ctx, harness.Command{
		Name: "podman",
		Args: args,
		Env:  harness.BaseEnv(),
	})
}

// CreateWorkspaceVolume creates the labelled named volume the Shell would
// provision before activation. The kodama.dev/workspace label is what the
// adapter checks; the kodama.test/owner labels scope every cleanup to this run.
func CreateWorkspaceVolume(ctx context.Context, name, workspaceID, shardID string) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(workspaceID) == "" {
		return errors.New("runtimesupport: workspace volume needs a name and workspace id")
	}
	args := []string{
		"volume", "create",
		"--label", WorkspaceLabel + "=" + workspaceID,
		"--label", harness.LabelTest + "=1",
		"--label", harness.LabelOwnerPID + "=" + RunOwnerPID(),
	}
	if shardID != "" {
		args = append(args, "--label", harness.LabelShard+"="+shardID)
	}
	args = append(args, name)
	if _, err := RunPodman(ctx, args...); err != nil {
		return fmt.Errorf("runtimesupport: create workspace volume %s: %w", name, err)
	}
	return nil
}

// RemoveWorkspaceVolume removes one volume this run created. An absent volume
// is clean. A volume not carrying this run's test/owner labels is refused
// untouched, so a foreign volume whose name happens to collide is never
// deleted. Removal is not forced: an in-use volume is reported, not taken away
// together with the container using it.
func RemoveWorkspaceVolume(ctx context.Context, name string) error {
	labels, err := VolumeLabels(ctx, name)
	if err != nil {
		if missingVolume(err) {
			return nil
		}
		return err
	}
	if !OwnedByRun(labels) {
		return fmt.Errorf("runtimesupport: refusing to remove volume %q: not owned by this run", name)
	}
	_, err = RunPodman(ctx, "volume", "rm", name)
	if err != nil && missingVolume(err) {
		return nil
	}
	return err
}

// VolumeLabels returns one volume's labels. A missing volume is an error whose
// text matches missingVolume.
func VolumeLabels(ctx context.Context, name string) (map[string]string, error) {
	result, err := RunPodman(ctx, "volume", "inspect", "--format", "{{json .Labels}}", name)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	text := strings.TrimSpace(string(result.Stdout))
	if text == "" || text == "null" {
		return labels, nil
	}
	if err := json.Unmarshal([]byte(text), &labels); err != nil {
		return nil, fmt.Errorf("runtimesupport: decode volume %s labels: %w", name, err)
	}
	return labels, nil
}

// VolumeExists reports whether the named volume exists.
func VolumeExists(ctx context.Context, name string) bool {
	_, err := RunPodman(ctx, "volume", "exists", name)
	return err == nil
}

// ReadVolumeFile reads one regular file from a named volume by exporting the
// volume to a tar and extracting only the requested entry. It starts no
// container and never follows a tar symlink.
func ReadVolumeFile(ctx context.Context, t testing.TB, volume, logical string) ([]byte, bool) {
	t.Helper()
	dir := t.TempDir()
	archive := filepath.Join(dir, "volume.tar")
	if _, err := RunPodman(ctx, "volume", "export", volume, "--output", archive); err != nil {
		t.Fatalf("export volume %s: %v", volume, err)
	}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open volume archive: %v", err)
	}
	defer func() { _ = file.Close() }()
	want := strings.TrimPrefix(filepath.ToSlash(logical), "/")
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, false
		}
		if err != nil {
			t.Fatalf("read volume archive: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if strings.TrimPrefix(filepath.ToSlash(header.Name), "./") != want {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil {
			t.Fatalf("read volume file %s: %v", want, err)
		}
		return data, true
	}
}

// ContainerNamesByLabel lists container names matching one label.
func ContainerNamesByLabel(ctx context.Context, key, value string) ([]string, error) {
	result, err := RunPodman(ctx, "ps", "-a",
		"--filter", "label="+key+"="+value,
		"--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("runtimesupport: list containers by %s: %w", key, err)
	}
	var names []string
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// PodmanVersion returns the server/client version for evidence.
func PodmanVersion(ctx context.Context) string {
	result, err := RunPodman(ctx, "version", "--format", "{{.Client.Version}}")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(result.Stdout))
}

func missingVolume(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such volume") || strings.Contains(text, "does not exist")
}

func missingContainer(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such container") ||
		strings.Contains(text, "no container with") ||
		strings.Contains(text, "does not exist")
}
