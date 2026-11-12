package skill

import (
	"context"
	"fmt"
	"slices"
)

// MarkdownLoader is the validating Markdown knowledge loader over a Source. It
// reads no filesystem itself and refuses supplied knowledge that carries a
// credential.
type MarkdownLoader struct {
	source Source
}

// NewLoader builds a loader over an explicit source. It refuses a nil source.
func NewLoader(source Source) (*MarkdownLoader, error) {
	if source == nil {
		return nil, fmt.Errorf("skill loader requires a source")
	}
	return &MarkdownLoader{source: source}, nil
}

// Load validates the requested name before touching the source, refuses a
// mismatched result, applies Skill.Validate and refuses secret-bearing bodies.
func (loader *MarkdownLoader) Load(ctx context.Context, name Name) (Skill, error) {
	if _, err := ParseName(string(name)); err != nil {
		return Skill{}, err
	}
	loaded, err := loader.source.Read(ctx, name)
	if err != nil {
		return Skill{}, err
	}
	if loaded.Name != name {
		return Skill{}, fmt.Errorf("skill source returned %q for requested %q", loaded.Name, name)
	}
	if err := loaded.Validate(); err != nil {
		return Skill{}, err
	}
	if ContainsSecret(loaded.Body) {
		return Skill{}, fmt.Errorf("skill %q contains credential material", name)
	}
	return loaded, nil
}

// List returns the source names sorted ascending and de-duplicated, independent
// of enumeration order. An invalid source name refuses the whole listing.
func (loader *MarkdownLoader) List(ctx context.Context) ([]Name, error) {
	names, err := loader.source.List(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[Name]bool, len(names))
	unique := make([]Name, 0, len(names))
	for _, name := range names {
		if _, err := ParseName(string(name)); err != nil {
			return nil, fmt.Errorf("skill source listed invalid name %q: %w", name, err)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		unique = append(unique, name)
	}
	slices.Sort(unique)
	return unique, nil
}

var _ Loader = (*MarkdownLoader)(nil)
