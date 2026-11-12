package skill

import "context"

// Source is the knowledge boundary a Markdown loader reads through. A source
// is confined to its own storage and performs no authority checks of its own.
type Source interface {
	List(ctx context.Context) ([]Name, error)
	Read(ctx context.Context, name Name) (Skill, error)
}
