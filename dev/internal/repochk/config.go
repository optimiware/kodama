package repochk

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/dev/internal/devenv"
)

var (
	composeImage = regexp.MustCompile(`(?m)^\s*image:\s*(\S+)`)
	pinnedImage  = regexp.MustCompile(`.+:[^@]+@sha256:[a-f0-9]{64}`)
)

// Config validates pinned configuration: JSON documents, immutable images and
// the Go toolchain pin.
func Config(root string) error {
	jsonFiles := []string{filepath.Join(root, "openforge.json")}
	opencode, err := SourceFiles(filepath.Join(root, ".opencode"), ".json")
	if err != nil {
		return err
	}
	jsonFiles = append(jsonFiles, opencode...)
	jsonFiles = append(jsonFiles, filepath.Join(root, "opencode.json"))
	contracts, err := SourceFiles(filepath.Join(root, "contracts"), ".json")
	if err != nil {
		return err
	}
	jsonFiles = append(jsonFiles, contracts...)
	devContracts, err := SourceFiles(filepath.Join(root, "dev", "contracts"), ".json")
	if err != nil {
		return err
	}
	jsonFiles = append(jsonFiles, devContracts...)
	for _, path := range jsonFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	lock, err := devenv.Versions()
	if err != nil {
		return err
	}
	images := []string{}
	compose, err := os.ReadFile(devenv.ComposeFile())
	if err != nil {
		return err
	}
	for _, match := range composeImage.FindAllStringSubmatch(string(compose), -1) {
		images = append(images, match[1])
	}
	for _, name := range []string{filepath.Join("dev", "images", "Containerfile"), "Containerfile"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return err
		}
		images = append(images, containerfileImages(string(data))...)
	}
	images = append(images, lock.Images.Node)
	for _, image := range images {
		if strings.HasPrefix(image, "${") {
			continue
		}
		if !pinnedImage.MatchString(image) {
			return fmt.Errorf("unpinned image %s", image)
		}
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	goLine := regexp.MustCompile(`(?m)^go ` + regexp.QuoteMeta(lock.Go) + `$`)
	if !goLine.Match(module) {
		return fmt.Errorf("Go toolchain/lock mismatch")
	}
	containerfile, err := os.ReadFile(filepath.Join(root, "Containerfile"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(containerfile), "golang:"+lock.Go) {
		return fmt.Errorf("Containerfile Go image does not match the Go lock")
	}
	fmt.Printf("PASS configuration and lock validation (%d images)\n", len(images))
	return nil
}

// containerfileImages returns the image references on FROM lines, skipping
// BuildKit platform flags.
func containerfileImages(text string) []string {
	images := []string{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "FROM" {
			continue
		}
		index := 1
		for index < len(fields) && strings.HasPrefix(fields[index], "--") {
			index++
		}
		if index < len(fields) {
			images = append(images, fields[index])
		}
	}
	return images
}
