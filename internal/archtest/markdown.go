package archtest

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode"
)

var (
	headingPattern = regexp.MustCompile(`^(#{1,6})[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	linkPattern    = regexp.MustCompile(`!?\[[^\]]*\]\(([^)]+)\)`)
	codeSpan       = regexp.MustCompile("`([^`]*)`")
	markdownLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	htmlTag        = regexp.MustCompile(`<[^>]*>`)
	schemePattern  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

type markdownLinkRef struct {
	Target string
	Line   int
}

// markdownAnchors returns the GitHub-style heading anchors of a Markdown file.
func markdownAnchors(path string) (map[string]bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	anchors := make(map[string]bool)
	used := make(map[string]int)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	inFence := false
	fenceMarker := ""
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if marker, ok := fenceMarkerOf(trimmed); ok {
			if !inFence {
				inFence = true
				fenceMarker = marker
			} else if strings.HasPrefix(trimmed, fenceMarker) {
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		match := headingPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		base := slugify(match[2])
		if base == "" {
			continue
		}
		count := used[base]
		used[base] = count + 1
		if count == 0 {
			anchors[base] = true
		} else {
			anchors[fmt.Sprintf("%s-%d", base, count)] = true
		}
	}
	return anchors, scanner.Err()
}

func markdownLinks(path string) ([]markdownLinkRef, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var links []markdownLinkRef
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	inFence := false
	fenceMarker := ""
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if marker, ok := fenceMarkerOf(trimmed); ok {
			if !inFence {
				inFence = true
				fenceMarker = marker
			} else if strings.HasPrefix(trimmed, fenceMarker) {
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		line = stripInlineCode(line)
		for _, match := range linkPattern.FindAllStringSubmatch(line, -1) {
			target, ok := parseLinkTarget(match[1])
			if !ok {
				continue
			}
			links = append(links, markdownLinkRef{Target: target, Line: lineNumber})
		}
	}
	return links, scanner.Err()
}

func fenceMarkerOf(trimmed string) (string, bool) {
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			return marker, true
		}
	}
	return "", false
}

func parseLinkTarget(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if strings.HasPrefix(raw, "<") {
		end := strings.Index(raw, ">")
		if end <= 0 {
			return "", false
		}
		raw = raw[1:end]
	} else if index := strings.IndexAny(raw, " \t"); index >= 0 {
		raw = raw[:index]
	}
	raw = strings.Trim(raw, "\"'")
	return raw, raw != ""
}

func stripInlineCode(line string) string {
	var b strings.Builder
	inCode := false
	for _, r := range line {
		if r == '`' {
			inCode = !inCode
			continue
		}
		if !inCode {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stripInlineMarkdown(text string) string {
	text = codeSpan.ReplaceAllString(text, "$1")
	text = markdownLink.ReplaceAllString(text, "$1")
	text = htmlTag.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "*", "")
	text = strings.ReplaceAll(text, "~", "")
	return strings.TrimSpace(text)
}

func slugify(text string) string {
	text = stripInlineMarkdown(text)
	text = strings.ToLower(text)
	var b strings.Builder
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte('-')
		}
	}
	return b.String()
}

func isExternalLink(target string) bool {
	return schemePattern.MatchString(target) || strings.HasPrefix(target, "//")
}

func isMarkdownPath(path string) bool {
	return strings.EqualFold(strings.ToLower(filepathExt(path)), ".md")
}

func filepathExt(path string) string {
	index := strings.LastIndex(path, ".")
	if index < 0 {
		return ""
	}
	return path[index:]
}

func splitLinkTarget(target string) (string, string) {
	before, after, ok := strings.Cut(target, "#")
	if !ok {
		return target, ""
	}
	return before, after
}

func decodeURLComponent(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}
