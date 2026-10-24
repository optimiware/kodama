package secrets

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// VariableType is the immutable scalar type of a variable.
type VariableType string

const (
	TypeString  VariableType = "string"
	TypeBoolean VariableType = "boolean"
	TypeInteger VariableType = "integer"
)

// Valid reports whether t is a supported variable type. Artifact inputs exist
// only as pipeline input declarations; a variable never holds one.
func (t VariableType) Valid() bool {
	return t == TypeString || t == TypeBoolean || t == TypeInteger
}

// NamePattern is the syntax of a variable name: the runtime contract's Name.
const NamePattern = `^[a-z][a-z0-9_-]{0,62}$`

var nameRegexp = regexp.MustCompile(NamePattern)

// MaxDescriptionBytes bounds free-text descriptions.
const MaxDescriptionBytes = 1024

// ValidateVariableName checks a variable or input name.
func ValidateVariableName(name string) error {
	if !nameRegexp.MatchString(name) {
		return invalidf("variable name must match %s", NamePattern)
	}
	return nil
}

// FormatValue renders a scalar in its canonical stored text.
func FormatValue(v runtimev1.Value) (string, error) {
	if err := v.Validate(); err != nil {
		return "", invalidf("%v", err)
	}
	switch {
	case v.IsString():
		return v.String(), nil
	case v.IsBool():
		b, _ := v.MarshalJSON()
		return string(b), nil
	default:
		b, err := v.MarshalJSON()
		return string(b), err
	}
}

// ParseValue is the inverse of FormatValue for a declared type.
func ParseValue(t VariableType, text string) (runtimev1.Value, error) {
	var v runtimev1.Value
	switch t {
	case TypeString:
		v = runtimev1.StringValue(text)
	case TypeBoolean:
		switch text {
		case "true":
			v = runtimev1.BoolValue(true)
		case "false":
			v = runtimev1.BoolValue(false)
		default:
			return runtimev1.Value{}, invalidf("not a boolean")
		}
	case TypeInteger:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != text {
			return runtimev1.Value{}, invalidf("not a canonical integer")
		}
		v = runtimev1.IntValue(n)
	default:
		return runtimev1.Value{}, invalidf("unsupported variable type")
	}
	if err := v.Validate(); err != nil {
		return runtimev1.Value{}, invalidf("%v", err)
	}
	return v, nil
}

// TypeOf returns the variable type of a scalar value.
func TypeOf(v runtimev1.Value) (VariableType, bool) {
	switch {
	case v.IsString():
		return TypeString, true
	case v.IsBool():
		return TypeBoolean, true
	case v.IsInteger():
		return TypeInteger, true
	}
	return "", false
}

// VariableRecord is the plain persisted state of a Variable. Adapters read and
// write it; RestoreVariable re-establishes the entity's invariants.
type VariableRecord struct {
	Scope        domain.Scope
	ID           string
	Name         string
	Type         VariableType
	ValueText    string
	ValueVersion int64
	Description  string
	Version      domain.Version
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// Variable is a typed, nonsecret project value. Its name and type are
// immutable. Editing the value creates a new value version; the resource
// version (the ETag) additionally counts description edits and deletion. A
// deleted variable keeps its name forever.
type Variable struct {
	rec   VariableRecord
	value runtimev1.Value
}

// NewVariable validates and creates a variable at value version 1, resource
// version 1.
func NewVariable(scope domain.Scope, id, name string, value runtimev1.Value, description string, now time.Time) (Variable, error) {
	typ, ok := TypeOf(value)
	if !ok {
		return Variable{}, invalidf("variable value must be a string, boolean or integer")
	}
	text, err := FormatValue(value)
	if err != nil {
		return Variable{}, err
	}
	return RestoreVariable(VariableRecord{
		Scope: scope, ID: id, Name: name, Type: typ, ValueText: text, ValueVersion: 1,
		Description: description, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	})
}

// RestoreVariable rebuilds a variable from persisted state and validates it.
func RestoreVariable(r VariableRecord) (Variable, error) {
	if err := r.Scope.Validate(); err != nil {
		return Variable{}, err
	}
	if _, err := validID(r.ID); err != nil {
		return Variable{}, err
	}
	if err := ValidateVariableName(r.Name); err != nil {
		return Variable{}, err
	}
	if !r.Type.Valid() {
		return Variable{}, invalidf("unsupported variable type")
	}
	if len(r.Description) > MaxDescriptionBytes {
		return Variable{}, invalidf("description longer than %d bytes", MaxDescriptionBytes)
	}
	if err := r.Version.Validate(); err != nil {
		return Variable{}, err
	}
	if err := domain.Version(r.ValueVersion).Validate(); err != nil {
		return Variable{}, err
	}
	v, err := ParseValue(r.Type, r.ValueText)
	if err != nil {
		return Variable{}, err
	}
	return Variable{rec: r, value: v}, nil
}

func validID(id string) (string, error) {
	if err := domain.ArtifactID(id).Validate(); err != nil { // shared opaque ID syntax
		return "", err
	}
	return id, nil
}

// Record returns a copy of the persisted state.
func (v Variable) Record() VariableRecord { return v.rec }

func (v Variable) Scope() domain.Scope     { return v.rec.Scope }
func (v Variable) ID() string              { return v.rec.ID }
func (v Variable) Name() string            { return v.rec.Name }
func (v Variable) Type() VariableType      { return v.rec.Type }
func (v Variable) Value() runtimev1.Value  { return v.value }
func (v Variable) ValueVersion() int64     { return v.rec.ValueVersion }
func (v Variable) Description() string     { return v.rec.Description }
func (v Variable) Version() domain.Version { return v.rec.Version }
func (v Variable) CreatedAt() time.Time    { return v.rec.CreatedAt }
func (v Variable) UpdatedAt() time.Time    { return v.rec.UpdatedAt }
func (v Variable) Deleted() bool           { return v.rec.DeletedAt != nil }
func (v Variable) DeletedAt() *time.Time   { return cloneTime(v.rec.DeletedAt) }
func (v Variable) String() string          { return fmt.Sprintf("variable %s/%s", v.rec.Scope, v.rec.Name) }
func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// VariableEdit carries the optional changes of an edit. A nil member is left
// unchanged.
type VariableEdit struct {
	Value       *runtimev1.Value
	Description *string
}

// Edit applies the edit if expected is the current resource version. It
// returns whether the value changed (a new value version was created) and
// whether anything changed at all. An edit that changes nothing does not bump
// the version.
func (v *Variable) Edit(expected domain.Version, e VariableEdit, now time.Time) (valueChanged, changed bool, err error) {
	if v.Deleted() {
		return false, false, ErrDeleted
	}
	if expected != v.rec.Version {
		return false, false, &ConflictError{Resource: "variable", Expected: int64(expected), Current: int64(v.rec.Version)}
	}
	if e.Value == nil && e.Description == nil {
		return false, false, invalidf("an edit must change the value or the description")
	}
	next := v.rec
	if e.Value != nil {
		typ, ok := TypeOf(*e.Value)
		if !ok || typ != v.rec.Type {
			return false, false, invalidf("value type must remain %s", v.rec.Type)
		}
		text, err := FormatValue(*e.Value)
		if err != nil {
			return false, false, err
		}
		if text != v.rec.ValueText {
			next.ValueText = text
			next.ValueVersion++
			valueChanged = true
		}
	}
	if e.Description != nil {
		if len(*e.Description) > MaxDescriptionBytes {
			return false, false, invalidf("description longer than %d bytes", MaxDescriptionBytes)
		}
		next.Description = *e.Description
	}
	if !valueChanged && next.Description == v.rec.Description {
		return false, false, nil
	}
	next.Version++
	next.UpdatedAt = now.UTC()
	restored, err := RestoreVariable(next)
	if err != nil {
		return false, false, err
	}
	*v = restored
	return valueChanged, true, nil
}

// Delete logically deletes the variable if expected is the current resource
// version. Deleting a deleted variable is a no-op that reports changed=false.
func (v *Variable) Delete(expected domain.Version, now time.Time) (changed bool, err error) {
	if expected != v.rec.Version {
		return false, &ConflictError{Resource: "variable", Expected: int64(expected), Current: int64(v.rec.Version)}
	}
	if v.Deleted() {
		return false, nil
	}
	t := now.UTC()
	v.rec.DeletedAt = &t
	v.rec.Version++
	v.rec.UpdatedAt = t
	return true, nil
}
