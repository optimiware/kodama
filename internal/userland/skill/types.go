// Package skill declares Markdown knowledge skills and their loader port.
//
// A skill is a named Markdown document loaded from an explicit relative path.
// The declarations read no filesystem.
package skill

import (
	"context"
	"fmt"
	"strings"
)

// MaxBodyBytes bounds one skill body (1 MiB).
const MaxBodyBytes = 1 << 20

// maxNameLength bounds one skill name.
const maxNameLength = 64

// Name is one validated skill name.
type Name string

// ParseName validates and converts a skill name. Names are bounded lowercase
// slugs with single hyphens.
func ParseName(value string) (Name, error) {
	if !validName(value) {
		return "", fmt.Errorf("invalid skill name %q", value)
	}
	return Name(value), nil
}

// validName reports whether value is a bounded lowercase skill slug.
func validName(value string) bool {
	if value == "" || len(value) > maxNameLength {
		return false
	}
	previousHyphen := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
			previousHyphen = false
		case character >= '0' && character <= '9':
			if index == 0 {
				return false
			}
			previousHyphen = false
		case character == '-':
			if index == 0 || previousHyphen {
				return false
			}
			previousHyphen = true
		default:
			return false
		}
	}
	return !previousHyphen
}

// Skill is one loaded Markdown knowledge document.
type Skill struct {
	Name Name   `json:"name"`
	Path string `json:"path"`
	Body []byte `json:"body"`
}

// Validate checks the name, the relative Markdown path and the bounded body.
func (skill Skill) Validate() error {
	if !validName(string(skill.Name)) {
		return fmt.Errorf("skill name %q is invalid", skill.Name)
	}
	if !validPath(skill.Path) {
		return fmt.Errorf("skill path %q must be a relative .md path", skill.Path)
	}
	if len(skill.Body) == 0 {
		return fmt.Errorf("skill body is empty")
	}
	if len(skill.Body) > MaxBodyBytes {
		return fmt.Errorf("skill body is %d bytes; at most %d are allowed", len(skill.Body), MaxBodyBytes)
	}
	return nil
}

// validPath reports whether value is a relative Markdown path without a parent
// segment.
func validPath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || !strings.HasSuffix(value, ".md") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// Loader loads and lists Markdown skills.
type Loader interface {
	Load(ctx context.Context, name Name) (Skill, error)
	List(ctx context.Context) ([]Name, error)
}
