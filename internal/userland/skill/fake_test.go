package skill_test

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/userland/skill"
)

// fakeSource is a deterministic knowledge source with a scripted enumeration
// order and call log. It performs no filesystem access.
type fakeSource struct {
	names   []skill.Name
	skills  map[skill.Name]skill.Skill
	listErr error
	readErr map[skill.Name]error
	reads   []skill.Name
	lists   int
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		skills:  make(map[skill.Name]skill.Skill),
		readErr: make(map[skill.Name]error),
	}
}

func (source *fakeSource) add(name skill.Name, body string) {
	source.names = append(source.names, name)
	source.skills[name] = skill.Skill{Name: name, Path: string(name) + ".md", Body: []byte(body)}
}

func (source *fakeSource) List(context.Context) ([]skill.Name, error) {
	source.lists++
	if source.listErr != nil {
		return nil, source.listErr
	}
	return append([]skill.Name(nil), source.names...), nil
}

func (source *fakeSource) Read(_ context.Context, name skill.Name) (skill.Skill, error) {
	source.reads = append(source.reads, name)
	if err := source.readErr[name]; err != nil {
		return skill.Skill{}, err
	}
	value, ok := source.skills[name]
	if !ok {
		return skill.Skill{}, fmt.Errorf("skill %q is missing", name)
	}
	return value, nil
}

var _ skill.Source = (*fakeSource)(nil)
