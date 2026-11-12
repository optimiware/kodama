package api

// Resource binding wire vocabulary.
//
// A binding declares a stable resource identity, the enforcing service, the
// trusted origins and the separate resource-operation and report-type
// vocabularies it may exercise, plus bounded metadata. The public DTO never
// carries resource bytes, access rules or provisioning work.

// Binding metadata bounds. Metadata is bounded display/selector data, never
// resource bytes or access rules; the same limits apply to every read, write
// and query so an oversized registration is refused before any effect.
const (
	// MaxMetadataEntries is the largest accepted metadata entry count.
	MaxMetadataEntries = 32
	// MaxMetadataKeyBytes is the largest accepted metadata key length.
	MaxMetadataKeyBytes = 128
	// MaxMetadataValueBytes is the largest accepted metadata value length.
	MaxMetadataValueBytes = 1024
)

// ResourceBindingRegisterRequest is the resource.binding.register input. The
// project is carried explicitly for MCP callers and is optional for REST
// callers whose route supplies it.
type ResourceBindingRegisterRequest struct {
	ProjectID        string            `json:"project_id,omitempty" schema:"id"`
	BindingID        string            `json:"binding_id" schema:"id"`
	ResourceID       string            `json:"resource_id" schema:"id"`
	EnforcingService string            `json:"enforcing_service" schema:"id"`
	TrustedOrigins   []string          `json:"trusted_origins"`
	Operations       []string          `json:"operations"`
	ReportableEvents []string          `json:"reportable_events"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Validate checks the optional project identity, bounded identities, the
// resource-operation and report-type vocabularies and metadata bounds.
func (request ResourceBindingRegisterRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if !validID(request.BindingID) {
		return invalid("binding_id is invalid")
	}
	if !validID(request.ResourceID) {
		return invalid("resource_id is invalid")
	}
	if !validID(request.EnforcingService) {
		return invalid("enforcing_service is invalid")
	}
	if err := validateTrustedOrigins(request.TrustedOrigins); err != nil {
		return err
	}
	if err := validateResourceOperations(request.Operations); err != nil {
		return err
	}
	if err := validateReportableEvents(request.ReportableEvents); err != nil {
		return err
	}
	return validateBindingMetadata(request.Metadata)
}

// ResourceBindingRegisterResult is the resource.binding.register result.
type ResourceBindingRegisterResult struct {
	BindingID string `json:"binding_id"`
	Version   uint64 `json:"version" schema:"version"`
}

// Validate checks the binding identity and committed version.
func (result ResourceBindingRegisterResult) Validate() error {
	if !validID(result.BindingID) {
		return invalid("binding_id is invalid")
	}
	if result.Version == 0 {
		return invalid("binding version must be positive")
	}
	return nil
}

// ResourceBindingUpdateRequest is the resource.binding.update input. The
// binding is carried explicitly for MCP callers and is optional for REST
// callers whose route supplies it.
type ResourceBindingUpdateRequest struct {
	BindingID        string            `json:"binding_id,omitempty" schema:"id"`
	ResourceID       string            `json:"resource_id" schema:"id"`
	EnforcingService string            `json:"enforcing_service" schema:"id"`
	TrustedOrigins   []string          `json:"trusted_origins"`
	Operations       []string          `json:"operations"`
	ReportableEvents []string          `json:"reportable_events"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Validate checks the optional binding identity, bounded identities, declared
// vocabularies and metadata bounds.
func (request ResourceBindingUpdateRequest) Validate() error {
	if request.BindingID != "" && !validID(request.BindingID) {
		return invalid("binding_id is invalid")
	}
	if !validID(request.ResourceID) {
		return invalid("resource_id is invalid")
	}
	if !validID(request.EnforcingService) {
		return invalid("enforcing_service is invalid")
	}
	if err := validateTrustedOrigins(request.TrustedOrigins); err != nil {
		return err
	}
	if err := validateResourceOperations(request.Operations); err != nil {
		return err
	}
	if err := validateReportableEvents(request.ReportableEvents); err != nil {
		return err
	}
	return validateBindingMetadata(request.Metadata)
}

// ResourceBindingUpdateResult is the resource.binding.update result.
type ResourceBindingUpdateResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result ResourceBindingUpdateResult) Validate() error {
	if result.Version == 0 {
		return invalid("binding version must be positive")
	}
	return nil
}

// ResourceBindingDeleteRequest is the resource.binding.delete input. The
// binding is carried explicitly for MCP callers and is optional for REST
// callers whose route supplies it.
type ResourceBindingDeleteRequest struct {
	BindingID string `json:"binding_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded binding identity.
func (request ResourceBindingDeleteRequest) Validate() error {
	if request.BindingID != "" && !validID(request.BindingID) {
		return invalid("binding_id is invalid")
	}
	return nil
}

// ResourceBindingDeleteResult is the resource.binding.delete result.
type ResourceBindingDeleteResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result ResourceBindingDeleteResult) Validate() error {
	if result.Version == 0 {
		return invalid("binding version must be positive")
	}
	return nil
}

// ResourceBindingReadRequest is the resource.binding.read input. The binding is
// carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type ResourceBindingReadRequest struct {
	BindingID string `json:"binding_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded binding identity.
func (request ResourceBindingReadRequest) Validate() error {
	if request.BindingID != "" && !validID(request.BindingID) {
		return invalid("binding_id is invalid")
	}
	return nil
}

// ResourceBindingReadResult is the resource.binding.read result.
type ResourceBindingReadResult struct {
	BindingID        string            `json:"binding_id"`
	ResourceID       string            `json:"resource_id"`
	EnforcingService string            `json:"enforcing_service"`
	TrustedOrigins   []string          `json:"trusted_origins"`
	Operations       []string          `json:"operations"`
	ReportableEvents []string          `json:"reportable_events"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Version          uint64            `json:"version" schema:"version"`
}

// Validate checks every observed binding member, including metadata bounds.
func (result ResourceBindingReadResult) Validate() error {
	if !validID(result.BindingID) {
		return invalid("binding_id is invalid")
	}
	if !validID(result.ResourceID) {
		return invalid("resource_id is invalid")
	}
	if !validID(result.EnforcingService) {
		return invalid("enforcing_service is invalid")
	}
	if err := validateTrustedOrigins(result.TrustedOrigins); err != nil {
		return err
	}
	if err := validateResourceOperations(result.Operations); err != nil {
		return err
	}
	if err := validateReportableEvents(result.ReportableEvents); err != nil {
		return err
	}
	if err := validateBindingMetadata(result.Metadata); err != nil {
		return err
	}
	if result.Version == 0 {
		return invalid("binding version must be positive")
	}
	return nil
}

// validateTrustedOrigins checks the bounded trusted origin list. The list must
// be present but may be empty: Binding.Validate accepts a binding with no
// trusted origins.
func validateTrustedOrigins(origins []string) error {
	if origins == nil {
		return invalid("trusted_origins is missing")
	}
	for _, origin := range origins {
		if origin == "" {
			return invalid("trusted origin is empty")
		}
		if len(origin) > MaxSubjectValueBytes {
			return invalid("trusted origin is too long")
		}
	}
	return nil
}

// validateResourceOperations checks the separate resource-operation
// vocabulary. The list must be present but may be empty: Binding.Validate
// accepts a binding that declares no verifiable operations. A public router
// operation name is never a resource operation.
func validateResourceOperations(operations []string) error {
	if operations == nil {
		return invalid("operations is missing")
	}
	for _, operation := range operations {
		if !ResourceOperation(operation).Valid() {
			return invalid("operation %q is not a resource operation", operation)
		}
	}
	return nil
}

// validateReportableEvents checks the binding-declared report types. The list
// must be present but may be empty: Binding.Validate accepts a binding that
// reports no event types. A lifecycle type is never a report type.
func validateReportableEvents(events []string) error {
	if events == nil {
		return invalid("reportable_events is missing")
	}
	for _, eventType := range events {
		if !validReportType(eventType) {
			return invalid("event_type %q is not a reportable type", eventType)
		}
	}
	return nil
}

// validateBindingMetadata checks the binding metadata bounds: at most
// MaxMetadataEntries entries, a non-empty key of at most MaxMetadataKeyBytes and
// a value of at most MaxMetadataValueBytes.
func validateBindingMetadata(metadata map[string]string) error {
	if len(metadata) > MaxMetadataEntries {
		return invalid("metadata has %d entries; at most %d are allowed", len(metadata), MaxMetadataEntries)
	}
	for key, value := range metadata {
		if key == "" {
			return invalid("metadata key is empty")
		}
		if len(key) > MaxMetadataKeyBytes {
			return invalid("metadata key is %d bytes; at most %d are allowed", len(key), MaxMetadataKeyBytes)
		}
		if len(value) > MaxMetadataValueBytes {
			return invalid("metadata value for %q is %d bytes; at most %d are allowed", key, len(value), MaxMetadataValueBytes)
		}
	}
	return nil
}
