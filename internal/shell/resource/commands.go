package resource

import (
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"maps"
)

// RegisterBindingCommand asks to register one new binding at version 1.
type RegisterBindingCommand struct {
	Actor            authorize.Principal `json:"actor"`
	Scope            tenancy.Scope       `json:"scope"`
	BindingID        BindingID           `json:"binding_id"`
	ResourceID       ResourceID          `json:"resource_id"`
	EnforcingService service.ServiceID   `json:"enforcing_service"`
	TrustedOrigins   []string            `json:"trusted_origins,omitempty"`
	Operations       []Operation         `json:"operations,omitempty"`
	ReportableEvents []string            `json:"reportable_events,omitempty"`
	Metadata         map[string]string   `json:"metadata,omitempty"`
	CallerKey        string              `json:"caller_key"`
	Content          receipt.Content     `json:"content"`
}

// UpdateBindingCommand asks to replace one binding value at an expected
// version.
type UpdateBindingCommand struct {
	Actor            authorize.Principal `json:"actor"`
	Scope            tenancy.Scope       `json:"scope"`
	BindingID        BindingID           `json:"binding_id"`
	ResourceID       ResourceID          `json:"resource_id"`
	EnforcingService service.ServiceID   `json:"enforcing_service"`
	TrustedOrigins   []string            `json:"trusted_origins,omitempty"`
	Operations       []Operation         `json:"operations,omitempty"`
	ReportableEvents []string            `json:"reportable_events,omitempty"`
	Metadata         map[string]string   `json:"metadata,omitempty"`
	ExpectedVersion  uint64              `json:"expected_version"`
	CallerKey        string              `json:"caller_key"`
	Content          receipt.Content     `json:"content"`
}

// DeleteBindingCommand asks to remove one binding at an expected version.
type DeleteBindingCommand struct {
	Actor           authorize.Principal `json:"actor"`
	Scope           tenancy.Scope       `json:"scope"`
	BindingID       BindingID           `json:"binding_id"`
	ExpectedVersion uint64              `json:"expected_version"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// ReadBindingCommand asks for one binding by its stable identity inside the
// caller's project scope.
type ReadBindingCommand struct {
	Actor     authorize.Principal `json:"actor"`
	Scope     tenancy.Scope       `json:"scope"`
	BindingID BindingID           `json:"binding_id"`
}

// QueryBindingsCommand asks for one scoped binding page.
type QueryBindingsCommand struct {
	Actor            authorize.Principal `json:"actor"`
	Scope            tenancy.Scope       `json:"scope"`
	EnforcingService service.ServiceID   `json:"enforcing_service,omitempty"`
	ResourceID       ResourceID          `json:"resource_id,omitempty"`
	Cursor           string              `json:"cursor,omitempty"`
	Limit            int                 `json:"limit"`
}

// cloneMetadata returns an independent copy of bounded binding metadata.
func cloneMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]string, len(metadata))
	maps.Copy(cloned, metadata)
	return cloned
}

// cloneBinding returns an independent copy of one binding value so a caller
// cannot mutate port-owned rows.
func cloneBinding(binding Binding) Binding {
	binding.TrustedOrigins = append([]string(nil), binding.TrustedOrigins...)
	binding.Operations = append([]Operation(nil), binding.Operations...)
	binding.ReportableEvents = append([]string(nil), binding.ReportableEvents...)
	binding.Metadata = cloneMetadata(binding.Metadata)
	return binding
}

// cloneBindingPage returns an independent copy of a binding page.
func cloneBindingPage(page BindingPage) BindingPage {
	cloned := BindingPage{
		Bindings:   make([]Binding, len(page.Bindings)),
		NextCursor: page.NextCursor,
	}
	for index, binding := range page.Bindings {
		cloned.Bindings[index] = cloneBinding(binding)
	}
	return cloned
}
