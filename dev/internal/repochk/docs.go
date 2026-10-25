package repochk

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

var (
	fenceLine = regexp.MustCompile("(?m)^```")
	heading   = regexp.MustCompile(`(?m)^#{1,6} (.+)$`)
	fence     = regexp.MustCompile("(?ms)^```(\\w*)\\n(.*?)^```")
	link      = regexp.MustCompile(`\[[^\]]*\]\(([^ )]+)(?: [^)]*)?\)`)
	scheme    = regexp.MustCompile(`^[a-zA-Z]+:`)
)

// Docs validates Markdown links, anchors and fenced examples.
func Docs(root string) error {
	files := []string{}
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range rootEntries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, filepath.Join(root, entry.Name()))
		}
	}
	for _, base := range []string{"docs", "contracts", filepath.Join(".opencode", "agents")} {
		found, err := SourceFiles(filepath.Join(root, base), ".md")
		if err != nil {
			return err
		}
		files = append(files, found...)
	}
	devFiles, err := os.ReadDir(filepath.Join(root, "dev"))
	if err != nil {
		return err
	}
	for _, entry := range devFiles {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, filepath.Join(root, "dev", entry.Name()))
		}
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		if len(fenceLine.FindAllString(text, -1))%2 != 0 {
			return fmt.Errorf("%s: unclosed fence", relative(root, path))
		}
		for _, match := range fence.FindAllStringSubmatch(text, -1) {
			language, body := match[1], match[2]
			switch language {
			case "json":
				var value any
				if err := json.Unmarshal([]byte(body), &value); err != nil {
					return fmt.Errorf("%s: fenced JSON: %w", relative(root, path), err)
				}
			case "bash", "sh", "shell":
				command := exec.Command("bash", "-n")
				command.Stdin = strings.NewReader(body)
				if output, err := command.CombinedOutput(); err != nil {
					return fmt.Errorf("%s: fenced shell: %w: %s", relative(root, path), err, output)
				}
			}
		}
		withoutCode := fence.ReplaceAllString(text, "")
		for _, match := range link.FindAllStringSubmatch(withoutCode, -1) {
			value := match[1]
			if scheme.MatchString(value) {
				continue
			}
			destination, fragment, _ := strings.Cut(value, "#")
			unescaped, err := url.PathUnescape(destination)
			if err != nil {
				unescaped = destination
			}
			target := path
			if unescaped != "" {
				target = filepath.Join(filepath.Dir(path), unescaped)
			}
			target = filepath.Clean(target)
			if _, err := os.Stat(target); err != nil {
				return fmt.Errorf("%s: broken link %s", relative(root, path), value)
			}
			if fragment != "" && strings.HasSuffix(target, ".md") {
				targetData, err := os.ReadFile(target)
				if err != nil {
					return err
				}
				if !Anchors(string(targetData))[fragment] {
					return fmt.Errorf("%s: broken anchor %s", relative(root, path), value)
				}
			}
		}
	}
	fmt.Printf("PASS documentation links, anchors, JSON and shell fences (%d files)\n", len(files))
	return nil
}

// Anchors returns the GitHub-style anchors of a Markdown document, including
// the -1, -2 suffixes duplicate headings receive.
func Anchors(text string) map[string]bool {
	result := map[string]bool{}
	counts := map[string]int{}
	for _, match := range heading.FindAllStringSubmatch(text, -1) {
		slug := slugify(match[1])
		count := counts[slug]
		counts[slug] = count + 1
		if count > 0 {
			slug = fmt.Sprintf("%s-%d", slug, count)
		}
		result[slug] = true
	}
	return result
}

func slugify(heading string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(heading) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == ' ' {
			builder.WriteRune(r)
		}
	}
	return strings.ReplaceAll(builder.String(), " ", "-")
}

func relative(root, path string) string {
	value, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return value
}
