package archtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func checkDocs(root string) ([]Finding, error) {
	var findings []Finding

	markdownFiles, err := collectDocsFiles(root, ".md")
	if err != nil {
		return nil, err
	}
	jsonFiles, err := collectDocsFiles(root, ".json")
	if err != nil {
		return nil, err
	}

	for _, path := range jsonFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", relativePath(root, path), err)
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			findings = append(findings, Finding{
				Path:    relativePath(root, path),
				Message: "invalid JSON: " + oneLine(err.Error()),
			})
		}
	}

	pending := append([]string(nil), markdownFiles...)
	seen := make(map[string]bool)
	for len(pending) > 0 {
		path := pending[0]
		pending = pending[1:]
		if seen[path] {
			continue
		}
		seen[path] = true

		links, err := markdownLinks(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", relativePath(root, path), err)
		}
		for _, link := range links {
			target := link.Target
			if target == "" || isExternalLink(target) {
				continue
			}
			pathPart, anchor := splitLinkTarget(target)
			var targetPath string
			if pathPart == "" {
				targetPath = path
			} else {
				decodedPath := decodeURLComponent(pathPart)
				if after, ok := strings.CutPrefix(decodedPath, "/"); ok {
					targetPath = filepath.Join(root, filepath.FromSlash(after))
				} else {
					targetPath = filepath.Join(filepath.Dir(path), filepath.FromSlash(decodedPath))
				}
			}
			targetPath = filepath.Clean(targetPath)

			info, statErr := os.Stat(targetPath)
			if statErr != nil {
				findings = append(findings, Finding{
					Path:    relativePath(root, path),
					Message: fmt.Sprintf("broken link %q: target does not exist", target),
				})
				continue
			}
			if anchor != "" && !info.IsDir() && isMarkdownPath(targetPath) {
				anchors, err := markdownAnchors(targetPath)
				if err != nil {
					return nil, fmt.Errorf("read %s: %w", relativePath(root, targetPath), err)
				}
				decodedAnchor := decodeURLComponent(anchor)
				if !anchors[decodedAnchor] && !anchors[strings.ToLower(decodedAnchor)] {
					findings = append(findings, Finding{
						Path:    relativePath(root, path),
						Message: fmt.Sprintf("missing anchor %q in %q", anchor, relativePath(root, targetPath)),
					})
				}
			}
			if !info.IsDir() && isMarkdownPath(targetPath) && !seen[targetPath] && pathWithinRoot(root, targetPath) {
				pending = append(pending, targetPath)
			}
		}
	}

	return findings, nil
}

func collectDocsFiles(root, extension string) ([]string, error) {
	docs := filepath.Join(root, "docs")
	var files []string
	err := filepath.WalkDir(docs, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == docs {
				return filepath.SkipDir
			}
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != docs && (name == ".git" || name == ".work" || name == ".harness" || name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), extension) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return files, nil
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
