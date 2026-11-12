package definition

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// Timeout bounds for every action. Admission resolves a duration inside the
// closed interval; queue time consumes none of it.
const (
	minActionTimeout = time.Minute
	maxActionTimeout = 30 * 24 * time.Hour
)

// Direct-lookup spellings accepted by ParseReference.
const (
	inputsPrefix = "inputs."
	outputsInfix = ".outputs."
)

// invalidInput builds the stable typed refusal every definition check returns.
func invalidInput(format string, args ...any) error {
	return fault.New(fault.InvalidInput, fmt.Sprintf(format, args...))
}

// Validate checks the structural semantics of an admitted pipeline declaration
// without reading a clock, the filesystem or any external service. It refuses
// with a typed fault.InvalidInput fault and never mutates the declaration.
//
// The supported shape is exactly S03 §1–2: a nonempty ordered action list, one
// kind per action, safe and unique declared names/keys, literal environment
// names that cannot shadow the reserved context, secret names that cannot
// shadow the execution credential, bounded timeouts and direct subject
// references to declared inputs or to outputs declared by an earlier action.
func Validate(p Pipeline) error {
	if len(p.Actions) == 0 {
		return invalidInput("pipeline declares no actions")
	}
	if err := validateInputKeys(p.Inputs); err != nil {
		return err
	}
	if err := validateEnv("pipeline env", p.Env); err != nil {
		return err
	}

	indices := make(map[id.ActionName]int, len(p.Actions))
	outputs := make(map[id.ActionName]map[id.OutputKey]bool, len(p.Actions))
	for index, action := range p.Actions {
		if err := validateAction(index, action); err != nil {
			return err
		}
		if _, duplicate := indices[action.Name]; duplicate {
			return invalidInput("duplicate action name %q", action.Name)
		}
		indices[action.Name] = index
		outputs[action.Name] = declaredOutputs(action)
	}

	for index, action := range p.Actions {
		if action.Request == nil {
			continue
		}
		if err := validateSubject(action.Request.Subject, p.Inputs, indices, outputs, index); err != nil {
			return err
		}
	}
	return nil
}

// validateInputKeys checks every admitted pipeline input key.
func validateInputKeys(keys []id.InputKey) error {
	seen := make(map[id.InputKey]bool, len(keys))
	for _, key := range keys {
		if _, err := id.ParseInputKey(string(key)); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("invalid pipeline input key %q", key), err)
		}
		if seen[key] {
			return invalidInput("duplicate pipeline input key %q", key)
		}
		seen[key] = true
	}
	return nil
}

// validateEnv checks one literal environment list: safe, non-reserved and
// unique names. Values stay opaque literals and are never interpolated.
func validateEnv(label string, entries []EnvVar) error {
	seen := make(map[id.EnvName]bool, len(entries))
	for _, entry := range entries {
		if _, err := id.ParseEnvName(string(entry.Name)); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("%s: invalid environment name %q", label, entry.Name), err)
		}
		if seen[entry.Name] {
			return invalidInput("%s: duplicate environment name %q", label, entry.Name)
		}
		seen[entry.Name] = true
	}
	return nil
}

// validateSecretNames checks declared secret names. Only names are declared;
// the reserved execution credential can never be shadowed.
func validateSecretNames(action id.ActionName, names []id.SecretName) error {
	seen := make(map[id.SecretName]bool, len(names))
	for _, name := range names {
		if _, err := id.ParseSecretName(string(name)); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("action %q: invalid secret name %q", action, name), err)
		}
		if seen[name] {
			return invalidInput("action %q: duplicate secret name %q", action, name)
		}
		seen[name] = true
	}
	return nil
}

// validateOutputKeys checks the declared output key set of one action.
func validateOutputKeys(action id.ActionName, keys []id.OutputKey) error {
	seen := make(map[id.OutputKey]bool, len(keys))
	for _, key := range keys {
		if _, err := id.ParseOutputKey(string(key)); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("action %q: invalid output key %q", action, key), err)
		}
		if seen[key] {
			return invalidInput("action %q: duplicate output key %q", action, key)
		}
		seen[key] = true
	}
	return nil
}

// validateAction checks one ordered declaration independently of its peers.
func validateAction(index int, action Action) error {
	if _, err := id.ParseActionName(string(action.Name)); err != nil {
		return fault.Wrap(fault.InvalidInput, fmt.Sprintf("action %d: invalid action name %q", index, action.Name), err)
	}
	if action.Timeout < minActionTimeout || action.Timeout > maxActionTimeout {
		return invalidInput("action %q: timeout %s is outside [%s, %s]", action.Name, action.Timeout, minActionTimeout, maxActionTimeout)
	}
	switch {
	case action.Container != nil && action.Request != nil:
		return invalidInput("action %q declares both container and request kinds", action.Name)
	case action.Container != nil:
		return validateContainer(action)
	case action.Request != nil:
		return validateRequest(action)
	default:
		return invalidInput("action %q declares no kind", action.Name)
	}
}

// validateContainer checks the supported container fields.
func validateContainer(action Action) error {
	container := action.Container
	if container.Image == "" {
		return invalidInput("action %q: container image is empty", action.Name)
	}
	if len(container.Commands) == 0 {
		return invalidInput("action %q: container declares no commands", action.Name)
	}
	for index, command := range container.Commands {
		if strings.ContainsRune(command, 0) {
			return invalidInput("action %q: command %d contains a NUL byte", action.Name, index)
		}
	}
	if err := validateEnv(fmt.Sprintf("action %q container env", action.Name), container.Env); err != nil {
		return err
	}
	if err := validateSecretNames(action.Name, container.Secrets); err != nil {
		return err
	}
	return validateOutputKeys(action.Name, container.Outputs)
}

// validateRequest checks the supported request fields. Assignment stays an
// opaque, nonempty validated value the Kernel never interprets.
func validateRequest(action Action) error {
	request := action.Request
	if strings.TrimSpace(request.Assignment) == "" {
		return invalidInput("action %q: request assignment is empty", action.Name)
	}
	return validateOutputKeys(action.Name, request.Outputs)
}

// declaredOutputs returns the output key set an action declares.
func declaredOutputs(action Action) map[id.OutputKey]bool {
	var keys []id.OutputKey
	switch {
	case action.Container != nil:
		keys = action.Container.Outputs
	case action.Request != nil:
		keys = action.Request.Outputs
	}
	declared := make(map[id.OutputKey]bool, len(keys))
	for _, key := range keys {
		declared[key] = true
	}
	return declared
}

// validateSubject checks every subject value structurally and then verifies
// that each direct reference names a declared input or an output declared by an
// earlier action. Self and forward references are refused.
func validateSubject(subject Subject, inputs []id.InputKey, indices map[id.ActionName]int, outputs map[id.ActionName]map[id.OutputKey]bool, self int) error {
	declaredInputs := make(map[string]bool, len(inputs))
	for _, key := range inputs {
		declaredInputs[string(key)] = true
	}

	fields := subject.Fields()
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		value := fields[name]
		if err := value.Validate(); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("subject field %q is malformed", name), err)
		}
		if value.Kind != SubjectReference {
			continue
		}
		reference := value.Reference
		if reference.Input {
			if _, err := id.ParseInputKey(reference.Key); err != nil {
				return fault.Wrap(fault.InvalidInput, fmt.Sprintf("subject field %q: invalid input reference key %q", name, reference.Key), err)
			}
			if !declaredInputs[reference.Key] {
				return invalidInput("subject field %q references undeclared input %q", name, reference.Key)
			}
			continue
		}

		if _, err := id.ParseActionName(reference.Action); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("subject field %q: invalid reference action %q", name, reference.Action), err)
		}
		if _, err := id.ParseOutputKey(reference.Key); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("subject field %q: invalid reference output key %q", name, reference.Key), err)
		}
		index, known := indices[id.ActionName(reference.Action)]
		if !known {
			return invalidInput("subject field %q references unknown action %q", name, reference.Action)
		}
		if index >= self {
			return invalidInput("subject field %q references action %q that is not strictly earlier", name, reference.Action)
		}
		if !outputs[id.ActionName(reference.Action)][id.OutputKey(reference.Key)] {
			return invalidInput("subject field %q references output %q not declared by action %q", name, reference.Key, reference.Action)
		}
	}
	return nil
}

// ParseReference parses exactly the two direct lookup forms of S03 §2:
// inputs.<declared-key> and <earlier-action>.outputs.<declared-key>. Nothing
// else is a reference: no interpolation, template, path expression or extra
// segment is accepted, and a refusal always returns the zero Reference.
func ParseReference(from string) (Reference, error) {
	if before, after, ok := strings.Cut(from, outputsInfix); ok {
		action := before
		key := after
		if _, err := id.ParseActionName(action); err == nil {
			if _, err := id.ParseOutputKey(key); err == nil {
				return Reference{Action: action, Key: key}, nil
			}
		}
	}
	if strings.HasPrefix(from, inputsPrefix) {
		key := from[len(inputsPrefix):]
		if _, err := id.ParseInputKey(key); err == nil {
			return Reference{Input: true, Key: key}, nil
		}
	}
	return Reference{}, invalidInput("subject reference %q is not a canonical direct lookup", from)
}

// ResolveSubject resolves one immutable subject from the supplied admitted
// input values and the supplied map of outputs of actions that already
// succeeded. Failed, pending and staged outputs are structurally unreachable:
// they simply have no entry in the supplied map. Resolution never mutates the
// admitted subject or the supplied maps, and a missing value is refused rather
// than coerced from an empty one.
func ResolveSubject(subject Subject, inputs map[string]string, succeeded map[id.ActionName]map[id.OutputKey]string) (Subject, error) {
	fields := subject.Fields()
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	resolved := make(map[string]SubjectValue, len(fields))
	for _, name := range names {
		value := fields[name]
		if err := value.Validate(); err != nil {
			return Subject{}, fault.Wrap(fault.InvalidInput, fmt.Sprintf("subject field %q is malformed", name), err)
		}
		if value.Kind == SubjectLiteral {
			resolved[name] = value
			continue
		}
		literal, err := resolveReference(name, value.Reference, inputs, succeeded)
		if err != nil {
			return Subject{}, err
		}
		resolved[name] = SubjectValue{Kind: SubjectLiteral, Literal: literal}
	}
	return NewSubject(resolved), nil
}

// resolveReference resolves one direct lookup against the supplied values.
func resolveReference(name string, reference Reference, inputs map[string]string, succeeded map[id.ActionName]map[id.OutputKey]string) (string, error) {
	if reference.Input {
		if _, err := id.ParseInputKey(reference.Key); err != nil {
			return "", fault.Wrap(fault.InvalidInput, fmt.Sprintf("subject field %q: invalid input reference key %q", name, reference.Key), err)
		}
		value, ok := inputs[reference.Key]
		if !ok {
			return "", invalidInput("subject field %q references unavailable input %q", name, reference.Key)
		}
		return value, nil
	}

	if _, err := id.ParseActionName(reference.Action); err != nil {
		return "", fault.Wrap(fault.InvalidInput, fmt.Sprintf("subject field %q: invalid reference action %q", name, reference.Action), err)
	}
	if _, err := id.ParseOutputKey(reference.Key); err != nil {
		return "", fault.Wrap(fault.InvalidInput, fmt.Sprintf("subject field %q: invalid reference output key %q", name, reference.Key), err)
	}
	outputs, ok := succeeded[id.ActionName(reference.Action)]
	if !ok {
		return "", invalidInput("subject field %q references action %q without succeeded outputs", name, reference.Action)
	}
	value, ok := outputs[id.OutputKey(reference.Key)]
	if !ok {
		return "", invalidInput("subject field %q references unavailable output %q of action %q", name, reference.Key, reference.Action)
	}
	return value, nil
}
