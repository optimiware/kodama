package opsapp

import (
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// Bindings returns the complete immutable binding table: exactly one binding
// for every registry operation, in declaration order, with the public input
// and result codecs.
//
// Every dependency is required. A nil capability service is a construction
// refusal rather than an operation that would succeed without an effect. The
// returned slice is a fresh copy; the caller may not mutate a running router.
func Bindings(deps Dependencies) ([]ops.Binding, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	handlers := make(map[authorizeOperation]ops.Handler, len(authorize.Operations()))
	for _, family := range []func(Dependencies) map[authorizeOperation]ops.Handler{
		tenancyHandlers,
		resourceHandlers,
		pipelineHandlers,
		secretHandlers,
		serviceHandlers,
		taskHandlers,
		executionHandlers,
		verifyHandlers,
		eventHandlers,
		modelHandlers,
	} {
		for operation, handler := range family(deps) {
			if handler == nil {
				return nil, fault.New(fault.InternalFailure,
					fmt.Sprintf("operation %q has a nil handler", operation))
			}
			if _, duplicate := handlers[operation]; duplicate {
				return nil, fault.New(fault.InternalFailure,
					fmt.Sprintf("operation %q has more than one handler", operation))
			}
			handlers[operation] = handler
		}
	}

	bindings := make([]ops.Binding, 0, len(authorize.Operations()))
	for _, operation := range authorize.Operations() {
		codec, ok := codecs[operation.Name]
		if !ok {
			return nil, fault.New(fault.InternalFailure,
				fmt.Sprintf("operation %q has no public codec", operation.Name))
		}
		handler, ok := handlers[operation.Name]
		if !ok {
			return nil, fault.New(fault.InternalFailure,
				fmt.Sprintf("operation %q has no handler", operation.Name))
		}
		bindings = append(bindings, ops.Binding{
			Operation: operation.Name,
			Input:     inputCodec(codec),
			Result:    resultCodec(codec),
			Handler:   handler,
		})
	}
	if len(bindings) != len(authorize.Operations()) {
		return nil, fault.New(fault.InternalFailure, "binding table is incomplete")
	}
	return bindings, nil
}

// validate refuses a nil capability service, port or configuration value.
func (deps Dependencies) validate() error {
	required := []struct {
		name  string
		value any
	}{
		{"tenancy", deps.Tenancy},
		{"resource bindings", deps.Bindings},
		{"pipeline management", deps.Pipelines},
		{"pipeline admission", deps.Admission},
		{"execution operations", deps.Executions},
		{"receipt lookup", deps.Receipts},
		{"secret management", deps.Secrets},
		{"service administration", deps.Services},
		{"credential administration", deps.Credentials},
		{"task operations", deps.Tasks},
		{"inbox queries", deps.Inbox},
		{"request resolution", deps.Requests},
		{"retained logs", deps.Logs},
		{"verifier", deps.Verifier},
		{"actor action resolver", deps.ActorActions},
		{"reporter", deps.Reports},
		{"event publication", deps.Events},
		{"event read", deps.EventRead},
		{"event subscribe", deps.EventSubscribe},
		{"model invocation", deps.Models},
		{"tool invocation", deps.Tools},
		{"identity source", deps.IDs},
		{"tenant key source", deps.TenantKeys},
	}
	for _, dependency := range required {
		if dependency.value == nil {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("operation bindings require a %s dependency", dependency.name))
		}
	}
	if deps.ActionTimeout <= 0 {
		return fault.New(fault.InvalidInput, "operation bindings require a positive action timeout")
	}
	return nil
}
