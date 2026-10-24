package openai

import (
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/gateway"
)

// ModelAccounting is the explicit, conservative token accounting of one model.
//
// The input bound is ceil(textBytes / BytesPerToken) plus fixed framing
// overheads. With BytesPerToken = 1 the bound is a true upper bound for any
// tokenizer in which a token covers at least one byte of the UTF-8 request,
// which holds for byte-level and character-level vocabularies. A larger
// BytesPerToken is only as safe as the installation's evidence for that model;
// the bound is published through the run's limits and is never silently
// loosened.
type ModelAccounting struct {
	BytesPerToken   int64
	RequestOverhead int64
	MessageOverhead int64
	ToolOverhead    int64
}

// ConservativeAccounting is the safe default: one token per byte plus generous
// framing.
var ConservativeAccounting = ModelAccounting{BytesPerToken: 1, RequestOverhead: 16, MessageOverhead: 8, ToolOverhead: 16}

// Estimator implements gateway.Estimator over a per-model accounting table. A
// model without an entry cannot be estimated and is refused.
type Estimator struct{ models map[string]ModelAccounting }

var _ gateway.Estimator = (*Estimator)(nil)

// NewEstimator validates the table.
func NewEstimator(models map[string]ModelAccounting) (*Estimator, error) {
	if len(models) == 0 {
		return nil, errors.New("openai: at least one model accounting entry is required")
	}
	e := &Estimator{models: map[string]ModelAccounting{}}
	for name, a := range models {
		if !gateway.ValidModelName(name) {
			return nil, fmt.Errorf("openai: %q is not a valid model name", name)
		}
		if a.BytesPerToken < 1 || a.RequestOverhead < 0 || a.MessageOverhead < 0 || a.ToolOverhead < 0 ||
			a.RequestOverhead > 1<<20 || a.MessageOverhead > 1<<16 || a.ToolOverhead > 1<<16 {
			return nil, fmt.Errorf("openai: the accounting of %q is out of range", name)
		}
		e.models[name] = a
	}
	return e, nil
}

// InputBound implements gateway.Estimator.
func (e *Estimator) InputBound(c gateway.Completion) (int64, error) {
	a, ok := e.models[c.Model]
	if !ok {
		return 0, gateway.Failf(gateway.ErrModelNotAllowed, "the model has no token accounting")
	}
	var n int64
	for _, m := range c.Messages {
		n += int64(len(m.Role) + len(m.Name) + len(m.ToolCallID))
		if m.Content != nil {
			n += int64(len(*m.Content))
		}
		for _, tc := range m.ToolCalls {
			n += int64(len(tc.ID) + len(tc.Name) + len(tc.Arguments))
		}
	}
	for _, t := range c.Tools {
		n += int64(len(t.Name) + len(t.Description) + len(t.Parameters))
	}
	for _, s := range c.Stop {
		n += int64(len(s))
	}
	n += int64(len(c.ToolChoice.Function))
	tokens := (n + a.BytesPerToken - 1) / a.BytesPerToken
	tokens += a.RequestOverhead + a.MessageOverhead*int64(len(c.Messages)) + a.ToolOverhead*int64(len(c.Tools))
	return tokens, nil
}
