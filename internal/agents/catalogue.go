package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// CatalogueEntry is one tested image/wrapper/harness combination and the
// controls enforced for it.
type CatalogueEntry struct {
	Tuple
	Controls []Control
	// Evidence names the compatibility smoke result (for example a CI run or
	// artifact reference). It is mandatory: an entry without evidence is not
	// support.
	Evidence string
}

// Catalogue is the static, injected Compatibility. Only an exact match of the
// complete tuple qualifies, and an empty catalogue qualifies nothing.
type Catalogue struct{ entries []CatalogueEntry }

var _ Compatibility = (*Catalogue)(nil)

// NewCatalogue validates the entries. Duplicate tuples are refused so one
// tuple never has two sets of enforced controls.
func NewCatalogue(entries ...CatalogueEntry) (*Catalogue, error) {
	c := &Catalogue{}
	for i, e := range entries {
		if err := validateEntry(e); err != nil {
			return nil, fmt.Errorf("agents: catalogue entry %d: %w", i, err)
		}
		for _, o := range c.entries {
			if sameTuple(o.Tuple, e.Tuple) {
				return nil, fmt.Errorf("agents: catalogue entry %d: duplicate tuple", i)
			}
		}
		e.Command = slices.Clone(e.Command)
		e.Controls = slices.Clone(e.Controls)
		c.entries = append(c.entries, e)
	}
	return c, nil
}

func validateEntry(e CatalogueEntry) error {
	d := Draft{Image: e.Image, Wrapper: Wrapper{ID: e.WrapperID, Version: e.WrapperVersion, Command: e.Command,
		Harness: Harness{ID: e.HarnessID, Version: e.HarnessVersion}}}
	var dg diagnostics
	if len(d.Image) > maxImageBytes || !imageRE.MatchString(d.Image) || !identRE.MatchString(d.Wrapper.ID) || !versionRE.MatchString(d.Wrapper.Version) ||
		!identRE.MatchString(d.Wrapper.Harness.ID) || !versionRE.MatchString(d.Wrapper.Harness.Version) || len(e.Command) < 1 || len(e.Command) > MaxCommandItems {
		dg.add(CodeInvalidField, "tuple", "the tuple is not valid")
	}
	if e.Evidence == "" || len(e.Evidence) > 256 || !plainText(e.Evidence, false) {
		dg.add(CodeInvalidField, "evidence", "compatibility evidence is required")
	}
	for _, c := range e.Controls {
		if !c.Valid() {
			dg.add(CodeInvalidField, "controls", "unknown control")
		}
	}
	return dg.err()
}

func sameTuple(a, b Tuple) bool {
	return a.Image == b.Image && a.WrapperID == b.WrapperID && a.WrapperVersion == b.WrapperVersion &&
		a.HarnessID == b.HarnessID && a.HarnessVersion == b.HarnessVersion && slices.Equal(a.Command, b.Command)
}

// Qualify implements Compatibility.
func (c *Catalogue) Qualify(_ context.Context, t Tuple) (Qualification, error) {
	if c == nil {
		return Qualification{}, errors.New("agents: no compatibility catalogue")
	}
	for _, e := range c.entries {
		if sameTuple(e.Tuple, t) {
			return Qualification{Evidence: e.Evidence, Controls: slices.Clone(e.Controls)}, nil
		}
	}
	return Qualification{}, Unsupportedf("the wrapper, harness and image combination has no compatibility evidence")
}
