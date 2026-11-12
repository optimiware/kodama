package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ChangeKind names one pipeline-management commit unit.
type ChangeKind string

const (
	// ChangeApply creates a pipeline or replaces an existing same-name
	// pipeline at its current version.
	ChangeApply ChangeKind = "apply"
	// ChangeUpdate replaces one selected pipeline at an expected version.
	ChangeUpdate ChangeKind = "update"
	// ChangeDelete removes one selected pipeline at an expected version.
	ChangeDelete ChangeKind = "delete"
)

// ChangeUnit is the complete atomic pipeline-management commit unit. Current
// authority reads, the receipt, audit, event publication intent and the exact
// authored bytes commit together; no registry or other network effect belongs
// in this unit. PushTriggers is the validated projection of the definition's
// `on.push` triggers (repository, service and exact refs) that the adapter
// persists to the pipelines.push_triggers column; adapters never parse YAML.
type ChangeUnit struct {
	Owner           tenancy.Scope
	Kind            ChangeKind
	PipelineID      PipelineID
	Name            string
	YAML            []byte
	ExpectedVersion uint64
	PushTriggers    []PushTrigger
	Receipt         receipt.Record
	Audit           []event.Audit
	Events          []event.Envelope
	Authority       []authorize.AuthorityRead
	PostCommit      []event.PostCommitEffect
}

// ChangeResult is the committed pipeline identity, current version and whether
// the receipt replayed an earlier identical request.
type ChangeResult struct {
	PipelineID PipelineID
	Version    uint64
	Replayed   bool
}

// ChangePort commits one complete pipeline-management unit. Implementations
// must check receipt identity before name/version constraints so an identical
// retry converges on the original result, and must enforce every versioned
// authority read inside the same atomic unit after that replay check.
type ChangePort interface {
	Change(context.Context, ChangeUnit) (ChangeResult, error)
}

// Definition is one stored pipeline definition at its current version. YAML is
// the exact authored byte sequence, never a canonicalized form. Push is the
// single validated on.push trigger when the definition declares one, and
// PushTriggers is the persisted projection of every validated on.push trigger
// (pipelines.push_triggers), in declaration order; a reader may populate either
// form and an adapter never parses YAML.
type Definition struct {
	ID           PipelineID
	Name         string
	YAML         []byte
	Version      uint64
	Push         *PushTrigger
	PushTriggers []PushTrigger
}

// Reader reads pipeline definitions scoped to one project.
type Reader interface {
	Get(context.Context, tenancy.Scope, PipelineID) (Definition, error)
	ByName(context.Context, tenancy.Scope, string) (Definition, error)
	List(context.Context, tenancy.Scope) ([]Definition, error)
}

// Action timeout bounds. Admission resolves every action duration inside the
// closed interval; queue time consumes none of it.
const (
	minActionTimeout = time.Minute
	maxActionTimeout = 30 * 24 * time.Hour
)

// Decode parses and validates one pipeline YAML document. The returned
// definition carries declaration order; an absent action timeout stays zero and
// is resolved from the installation default at admission.
func Decode(data []byte, limits Limits) (definition.Pipeline, error) {
	parsed, err := decodeDocument(data, limits)
	if err != nil {
		return definition.Pipeline{}, err
	}
	return parsed.pipeline, nil
}

// parsedDocument is the full decode result: the Kernel definition plus the
// admission-only declaration detail (typed input specifications and the
// optional push trigger).
type parsedDocument struct {
	pipeline definition.Pipeline
	inputs   []InputSpec
	push     *PushTrigger
}

func invalidInputf(format string, args ...any) error {
	return fault.New(fault.InvalidInput, fmt.Sprintf(format, args...))
}

func invalidInputWrap(message string, cause error) error {
	return fault.Wrap(fault.InvalidInput, message, cause)
}

func decodeDocument(data []byte, limits Limits) (parsedDocument, error) {
	if err := limits.CheckYAMLBytes(len(data)); err != nil {
		return parsedDocument{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if err == io.EOF {
			return parsedDocument{}, invalidInputf("pipeline yaml is empty")
		}
		return parsedDocument{}, invalidInputWrap("pipeline yaml is malformed", err)
	}
	// The snapshot stores and hashes the whole byte sequence, so a second
	// document would be persisted without ever being inspected. Exactly one
	// document is admitted; any trailing document, empty or malformed, is
	// refused rather than silently ignored.
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return parsedDocument{}, invalidInputWrap("pipeline yaml has a malformed trailing document", err)
		}
		return parsedDocument{}, invalidInputf("pipeline yaml must contain exactly one document")
	}
	root := rootNode(&document)
	if root == nil {
		return parsedDocument{}, invalidInputf("pipeline yaml is empty")
	}
	if err := inspectYAML(root, limits, 1); err != nil {
		return parsedDocument{}, err
	}
	return parsePipeline(root)
}

func rootNode(document *yaml.Node) *yaml.Node {
	if document == nil {
		return nil
	}
	switch document.Kind {
	case yaml.DocumentNode:
		if len(document.Content) != 1 {
			return nil
		}
		return document.Content[0]
	case 0:
		return nil
	default:
		return document
	}
}

// inspectYAML refuses every unsupported YAML construct before parsing: aliases,
// anchors, explicit tags, merge keys and documents deeper than the bound.
func inspectYAML(node *yaml.Node, limits Limits, depth int) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode {
		return invalidInputf("yaml aliases are not supported")
	}
	if node.Anchor != "" {
		return invalidInputf("yaml anchors are not supported")
	}
	if node.Style&yaml.TaggedStyle != 0 {
		return invalidInputf("yaml tags are not supported")
	}
	switch node.Kind {
	case yaml.MappingNode:
		if err := limits.CheckDepth(depth); err != nil {
			return err
		}
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			value := node.Content[index+1]
			if key.Kind == yaml.ScalarNode && key.Tag == "!!merge" {
				return invalidInputf("yaml merge keys are not supported")
			}
			if err := inspectYAML(key, limits, depth+1); err != nil {
				return err
			}
			if err := inspectYAML(value, limits, depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if err := limits.CheckDepth(depth); err != nil {
			return err
		}
		for _, child := range node.Content {
			if err := inspectYAML(child, limits, depth+1); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		return nil
	default:
		return invalidInputf("yaml node kind %d is not supported", node.Kind)
	}
	return nil
}

type mappingEntry struct {
	key   string
	value *yaml.Node
}

func mappingEntries(node *yaml.Node, label string) ([]mappingEntry, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, invalidInputf("%s must be a mapping", label)
	}
	seen := make(map[string]bool, len(node.Content)/2)
	entries := make([]mappingEntry, 0, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		value := node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return nil, invalidInputf("%s has a non-string key", label)
		}
		if seen[key.Value] {
			return nil, invalidInputf("%s has duplicate key %q", label, key.Value)
		}
		seen[key.Value] = true
		entries = append(entries, mappingEntry{key: key.Value, value: value})
	}
	return entries, nil
}

func scalarValue(node *yaml.Node, label string) (string, error) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return "", invalidInputf("%s must be a scalar value", label)
	}
	if node.Tag != "!!str" {
		return "", invalidInputf("%s must be a string value", label)
	}
	return node.Value, nil
}

func boolValue(node *yaml.Node, label string) (bool, error) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return false, invalidInputf("%s must be a boolean", label)
	}
	var value bool
	if err := node.Decode(&value); err != nil {
		return false, invalidInputWrap(label+" must be a boolean", err)
	}
	return value, nil
}

func sequenceNodes(node *yaml.Node, label string) ([]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil, invalidInputf("%s must be a sequence", label)
	}
	return node.Content, nil
}

func scalarStrings(node *yaml.Node, label string) ([]string, error) {
	items, err := sequenceNodes(node, label)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(items))
	for index, item := range items {
		value, err := scalarValue(item, fmt.Sprintf("%s item %d", label, index))
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func parsePipeline(root *yaml.Node) (parsedDocument, error) {
	entries, err := mappingEntries(root, "pipeline")
	if err != nil {
		return parsedDocument{}, err
	}
	var inputsNode, envNode, actionsNode, onNode, repositoryNode, serviceNode *yaml.Node
	for _, entry := range entries {
		switch entry.key {
		case "inputs":
			inputsNode = entry.value
		case "env":
			envNode = entry.value
		case "actions":
			actionsNode = entry.value
		case "repository":
			repositoryNode = entry.value
		case "service":
			serviceNode = entry.value
		case "on":
			onNode = entry.value
		default:
			return parsedDocument{}, invalidInputf("pipeline has unknown field %q", entry.key)
		}
	}
	if actionsNode == nil {
		return parsedDocument{}, invalidInputf("pipeline declares no actions")
	}

	inputs, specs, err := parseInputs(inputsNode)
	if err != nil {
		return parsedDocument{}, err
	}
	env, err := parseEnv(envNode, "pipeline env")
	if err != nil {
		return parsedDocument{}, err
	}
	actions, err := parseActions(actionsNode)
	if err != nil {
		return parsedDocument{}, err
	}

	var repositoryID resource.ResourceID
	if repositoryNode != nil {
		value, err := scalarValue(repositoryNode, "repository")
		if err != nil {
			return parsedDocument{}, err
		}
		repositoryID, err = resource.ParseResourceID(value)
		if err != nil {
			return parsedDocument{}, invalidInputWrap("pipeline repository is invalid", err)
		}
	}
	var serviceID service.ServiceID
	if serviceNode != nil {
		value, err := scalarValue(serviceNode, "service")
		if err != nil {
			return parsedDocument{}, err
		}
		serviceID, err = service.ParseServiceID(value)
		if err != nil {
			return parsedDocument{}, invalidInputWrap("pipeline service is invalid", err)
		}
	}
	var push *PushTrigger
	if onNode != nil {
		push, err = parseOn(onNode)
		if err != nil {
			return parsedDocument{}, err
		}
		if repositoryNode == nil || serviceNode == nil {
			return parsedDocument{}, invalidInputf("on.push requires repository and service")
		}
		push.Repository = repositoryID
		push.Service = serviceID
		allowed := map[id.InputKey]bool{
			"repository":      true,
			"ref":             true,
			"commit":          true,
			"previous_commit": true,
		}
		for _, spec := range specs {
			if !allowed[spec.Key] {
				return parsedDocument{}, invalidInputf("push pipeline declares unsupported input %q", spec.Key)
			}
		}
	}

	parsed := parsedDocument{
		pipeline: definition.Pipeline{Inputs: inputs, Env: env, Actions: actions},
		inputs:   specs,
		push:     push,
	}
	if err := validateDefinition(parsed.pipeline); err != nil {
		return parsedDocument{}, err
	}
	return parsed, nil
}

func parseInputs(node *yaml.Node) ([]id.InputKey, []InputSpec, error) {
	if node == nil {
		return nil, nil, nil
	}
	items, err := sequenceNodes(node, "inputs")
	if err != nil {
		return nil, nil, err
	}
	keys := make([]id.InputKey, 0, len(items))
	specs := make([]InputSpec, 0, len(items))
	seen := make(map[id.InputKey]bool, len(items))
	for index, item := range items {
		var spec InputSpec
		switch item.Kind {
		case yaml.ScalarNode:
			text, err := scalarValue(item, fmt.Sprintf("input %d", index))
			if err != nil {
				return nil, nil, err
			}
			key, err := id.ParseInputKey(text)
			if err != nil {
				return nil, nil, invalidInputWrap(fmt.Sprintf("input %d is invalid", index), err)
			}
			spec = InputSpec{Key: key, Kind: InputString, Required: true}
		case yaml.MappingNode:
			entries, err := mappingEntries(item, fmt.Sprintf("input %d", index))
			if err != nil {
				return nil, nil, err
			}
			var keyNode *yaml.Node
			spec = InputSpec{Kind: InputString, Required: false}
			for _, entry := range entries {
				switch entry.key {
				case "key":
					keyNode = entry.value
				case "kind":
					value, err := scalarValue(entry.value, "input kind")
					if err != nil {
						return nil, nil, err
					}
					spec.Kind = InputKind(value)
					if !spec.Kind.Valid() {
						return nil, nil, invalidInputf("input %d has unknown kind %q", index, value)
					}
				case "required":
					value, err := boolValue(entry.value, "input required")
					if err != nil {
						return nil, nil, err
					}
					spec.Required = value
				default:
					return nil, nil, invalidInputf("input %d has unknown field %q", index, entry.key)
				}
			}
			if keyNode == nil {
				return nil, nil, invalidInputf("input %d has no key", index)
			}
			value, err := scalarValue(keyNode, "input key")
			if err != nil {
				return nil, nil, err
			}
			key, err := id.ParseInputKey(value)
			if err != nil {
				return nil, nil, invalidInputWrap(fmt.Sprintf("input %d key is invalid", index), err)
			}
			spec.Key = key
		default:
			return nil, nil, invalidInputf("input %d must be a string or a mapping", index)
		}
		if seen[spec.Key] {
			return nil, nil, invalidInputf("duplicate input key %q", spec.Key)
		}
		seen[spec.Key] = true
		keys = append(keys, spec.Key)
		specs = append(specs, spec)
	}
	return keys, specs, nil
}

func parseEnv(node *yaml.Node, label string) ([]definition.EnvVar, error) {
	if node == nil {
		return nil, nil
	}
	entries, err := mappingEntries(node, label)
	if err != nil {
		return nil, err
	}
	env := make([]definition.EnvVar, 0, len(entries))
	for _, entry := range entries {
		name, err := id.ParseEnvName(entry.key)
		if err != nil {
			return nil, invalidInputWrap(fmt.Sprintf("%s name %q is invalid", label, entry.key), err)
		}
		value, err := scalarValue(entry.value, fmt.Sprintf("%s value %q", label, entry.key))
		if err != nil {
			return nil, err
		}
		env = append(env, definition.EnvVar{Name: name, Value: value})
	}
	return env, nil
}

func parseActions(node *yaml.Node) ([]definition.Action, error) {
	items, err := sequenceNodes(node, "actions")
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, invalidInputf("pipeline declares no actions")
	}
	actions := make([]definition.Action, 0, len(items))
	for index, item := range items {
		action, err := parseAction(index, item)
		if err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func parseAction(index int, node *yaml.Node) (definition.Action, error) {
	label := fmt.Sprintf("action %d", index)
	entries, err := mappingEntries(node, label)
	if err != nil {
		return definition.Action{}, err
	}
	var nameNode, imageNode, requestNode, subjectNode, commandsNode, envNode, secretsNode, timeoutNode, outputsNode *yaml.Node
	for _, entry := range entries {
		switch entry.key {
		case "name":
			nameNode = entry.value
		case "image":
			imageNode = entry.value
		case "request":
			requestNode = entry.value
		case "subject":
			subjectNode = entry.value
		case "commands":
			commandsNode = entry.value
		case "env":
			envNode = entry.value
		case "secrets":
			secretsNode = entry.value
		case "timeout":
			timeoutNode = entry.value
		case "outputs":
			outputsNode = entry.value
		default:
			return definition.Action{}, invalidInputf("%s has unknown field %q", label, entry.key)
		}
	}
	if nameNode == nil {
		return definition.Action{}, invalidInputf("%s has no name", label)
	}
	nameText, err := scalarValue(nameNode, label+" name")
	if err != nil {
		return definition.Action{}, err
	}
	name, err := id.ParseActionName(nameText)
	if err != nil {
		return definition.Action{}, invalidInputWrap(label+" name is invalid", err)
	}
	action := definition.Action{Name: name}
	if imageNode != nil && requestNode != nil {
		return definition.Action{}, invalidInputf("%s declares both container and request kinds", label)
	}
	if imageNode == nil && requestNode == nil {
		return definition.Action{}, invalidInputf("%s declares no action kind", label)
	}
	if imageNode != nil {
		if subjectNode != nil {
			return definition.Action{}, invalidInputf("%s container action declares subject", label)
		}
		image, err := scalarValue(imageNode, label+" image")
		if err != nil {
			return definition.Action{}, err
		}
		if image == "" {
			return definition.Action{}, invalidInputf("%s image is empty", label)
		}
		var commands []string
		if commandsNode != nil {
			commands, err = scalarStrings(commandsNode, label+" commands")
			if err != nil {
				return definition.Action{}, err
			}
		}
		var env []definition.EnvVar
		if envNode != nil {
			env, err = parseEnv(envNode, label+" env")
			if err != nil {
				return definition.Action{}, err
			}
		}
		var secrets []id.SecretName
		if secretsNode != nil {
			secrets, err = parseSecrets(secretsNode, label)
			if err != nil {
				return definition.Action{}, err
			}
		}
		var outputs []id.OutputKey
		if outputsNode != nil {
			outputs, err = parseOutputs(outputsNode, label)
			if err != nil {
				return definition.Action{}, err
			}
		}
		action.Container = &definition.Container{
			Image:    image,
			Commands: commands,
			Env:      env,
			Secrets:  secrets,
			Outputs:  outputs,
		}
	} else {
		if commandsNode != nil || envNode != nil || secretsNode != nil {
			return definition.Action{}, invalidInputf("%s request action declares container fields", label)
		}
		assignment, err := scalarValue(requestNode, label+" request")
		if err != nil {
			return definition.Action{}, err
		}
		if assignment == "" {
			return definition.Action{}, invalidInputf("%s request assignment is empty", label)
		}
		var subject definition.Subject
		if subjectNode != nil {
			subject, err = parseSubject(subjectNode, label)
			if err != nil {
				return definition.Action{}, err
			}
		}
		var outputs []id.OutputKey
		if outputsNode != nil {
			outputs, err = parseOutputs(outputsNode, label)
			if err != nil {
				return definition.Action{}, err
			}
		}
		action.Request = &definition.Request{Assignment: assignment, Subject: subject, Outputs: outputs}
	}
	if timeoutNode != nil {
		text, err := scalarValue(timeoutNode, label+" timeout")
		if err != nil {
			return definition.Action{}, err
		}
		duration, err := time.ParseDuration(text)
		if err != nil {
			return definition.Action{}, invalidInputWrap(label+" timeout is invalid", err)
		}
		if duration < minActionTimeout || duration > maxActionTimeout {
			return definition.Action{}, invalidInputf("%s timeout %s is outside [%s, %s]", label, duration, minActionTimeout, maxActionTimeout)
		}
		action.Timeout = duration
	}
	return action, nil
}

func parseSecrets(node *yaml.Node, label string) ([]id.SecretName, error) {
	items, err := sequenceNodes(node, label+" secrets")
	if err != nil {
		return nil, err
	}
	names := make([]id.SecretName, 0, len(items))
	for index, item := range items {
		text, err := scalarValue(item, fmt.Sprintf("%s secret %d", label, index))
		if err != nil {
			return nil, err
		}
		name, err := id.ParseSecretName(text)
		if err != nil {
			return nil, invalidInputWrap(label+" secret name is invalid", err)
		}
		names = append(names, name)
	}
	return names, nil
}

func parseOutputs(node *yaml.Node, label string) ([]id.OutputKey, error) {
	items, err := sequenceNodes(node, label+" outputs")
	if err != nil {
		return nil, err
	}
	keys := make([]id.OutputKey, 0, len(items))
	for index, item := range items {
		text, err := scalarValue(item, fmt.Sprintf("%s output %d", label, index))
		if err != nil {
			return nil, err
		}
		key, err := id.ParseOutputKey(text)
		if err != nil {
			return nil, invalidInputWrap(label+" output key is invalid", err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func parseSubject(node *yaml.Node, label string) (definition.Subject, error) {
	entries, err := mappingEntries(node, label+" subject")
	if err != nil {
		return definition.Subject{}, err
	}
	fields := make(map[string]definition.SubjectValue, len(entries))
	for _, entry := range entries {
		if _, err := id.ParseInputKey(entry.key); err != nil {
			return definition.Subject{}, invalidInputWrap(fmt.Sprintf("%s subject field %q is invalid", label, entry.key), err)
		}
		switch entry.value.Kind {
		case yaml.ScalarNode:
			fields[entry.key] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: entry.value.Value}
		case yaml.MappingNode:
			valueEntries, err := mappingEntries(entry.value, fmt.Sprintf("%s subject field %q", label, entry.key))
			if err != nil {
				return definition.Subject{}, err
			}
			if len(valueEntries) != 1 || valueEntries[0].key != "from" {
				return definition.Subject{}, invalidInputf("%s subject field %q must be a literal or exactly {from}", label, entry.key)
			}
			from, err := scalarValue(valueEntries[0].value, fmt.Sprintf("%s subject field %q from", label, entry.key))
			if err != nil {
				return definition.Subject{}, err
			}
			reference, err := definition.ParseReference(from)
			if err != nil {
				return definition.Subject{}, invalidInputWrap(fmt.Sprintf("%s subject field %q is not a canonical direct lookup", label, entry.key), err)
			}
			fields[entry.key] = definition.SubjectValue{Kind: definition.SubjectReference, Reference: reference}
		default:
			return definition.Subject{}, invalidInputf("%s subject field %q must be a literal or exactly {from}", label, entry.key)
		}
	}
	return definition.NewSubject(fields), nil
}

func parseOn(node *yaml.Node) (*PushTrigger, error) {
	entries, err := mappingEntries(node, "on")
	if err != nil {
		return nil, err
	}
	var push *PushTrigger
	for _, entry := range entries {
		if entry.key != "push" {
			return nil, invalidInputf("on has unknown field %q", entry.key)
		}
		items, err := sequenceNodes(entry.value, "on.push")
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, invalidInputf("on.push declares no refs")
		}
		refs := make([]string, 0, len(items))
		for index, item := range items {
			ref, err := scalarValue(item, fmt.Sprintf("on.push ref %d", index))
			if err != nil {
				return nil, err
			}
			if !validExactRef(ref) {
				return nil, invalidInputf("on.push ref %q is not an exact ref", ref)
			}
			refs = append(refs, ref)
		}
		push = &PushTrigger{Refs: refs}
	}
	if push == nil {
		return nil, invalidInputf("on declares no push trigger")
	}
	return push, nil
}

func validExactRef(value string) bool {
	if value == "" || len(value) > id.MaxIDLength*4 {
		return false
	}
	if len(value) < len("refs/") || value[:len("refs/")] != "refs/" {
		return false
	}
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '*', '?', '[', ']', '\\', ' ', '\t', '\r', '\n':
			return false
		default:
			if value[index] < 0x20 {
				return false
			}
		}
	}
	return true
}

func validateDefinition(p definition.Pipeline) error {
	validation := p
	validation.Actions = make([]definition.Action, len(p.Actions))
	for index, action := range p.Actions {
		validation.Actions[index] = action
		if action.Timeout == 0 {
			validation.Actions[index].Timeout = minActionTimeout
		}
	}
	if err := definition.Validate(validation); err != nil {
		if _, ok := fault.ClassOf(err); ok {
			return err
		}
		return invalidInputWrap("pipeline definition is invalid", err)
	}
	return nil
}
