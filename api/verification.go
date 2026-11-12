package api

// VerifyRequest is the service-plus-actor verification input. The forwarded
// actor credential is the only credential field.
type VerifyRequest struct {
	ActorCredential string  `json:"actor_credential"`
	ResourceID      string  `json:"resource_id"`
	Operation       string  `json:"operation"`
	Subject         Subject `json:"subject"`
	Approval        string  `json:"approval,omitempty"`
}

// Validate checks the bounded credential, resource, resource operation, typed
// subject and optional role approval.
func (request VerifyRequest) Validate() error {
	if request.ActorCredential == "" {
		return invalid("actor_credential is empty")
	}
	if len(request.ActorCredential) > MaxCredentialBytes {
		return invalid("actor_credential is %d bytes; at most %d are allowed", len(request.ActorCredential), MaxCredentialBytes)
	}
	if !validID(request.ResourceID) {
		return invalid("resource_id is invalid")
	}
	if !ResourceOperation(request.Operation).Valid() {
		return invalid("operation %q is not a resource operation", request.Operation)
	}
	if request.Operation == string(ResourceOperationGitRead) {
		// A repository read is repository-level: verification carries no
		// subject and requests no role approval.
		if len(request.Subject.Fields) != 0 {
			return invalid("git.read verification requires an empty subject")
		}
		if request.Approval != "" {
			return invalid("git.read verification does not accept an approval")
		}
	}
	if err := request.Subject.Validate(); err != nil {
		return err
	}
	if !validApproval(request.Approval) {
		return invalid("approval %q must be role/<role>", request.Approval)
	}
	return nil
}

// DecodeVerifyRequest decodes one bounded verification input.
func DecodeVerifyRequest(data []byte) (VerifyRequest, error) {
	if len(data) > MaxRequestBytes {
		return VerifyRequest{}, invalid("verify request is %d bytes; at most %d are allowed", len(data), MaxRequestBytes)
	}
	var request VerifyRequest
	if err := decodeStrict(data, &request); err != nil {
		return VerifyRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return VerifyRequest{}, err
	}
	return request, nil
}

// VerifiedPrincipal is the authenticated actor of one verification attempt.
type VerifiedPrincipal struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// principalKinds is the verified-principal vocabulary.
var principalKinds = []string{"human", "service", "execution"}

// validPrincipalKind reports whether kind is a declared principal kind.
func validPrincipalKind(kind string) bool {
	for _, candidate := range principalKinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// VerifyResponse is the verification fact response. It never echoes the
// forwarded credential.
type VerifyResponse struct {
	Principal      VerifiedPrincipal `json:"principal"`
	Roles          []string          `json:"roles,omitempty"`
	Approval       bool              `json:"approval,omitempty"`
	VerificationID string            `json:"verification_id"`
}

// Validate checks the verified principal, current roles and verification ID.
func (response VerifyResponse) Validate() error {
	if !validID(response.Principal.ID) {
		return invalid("verified principal id is invalid")
	}
	if !validPrincipalKind(response.Principal.Kind) {
		return invalid("verified principal kind %q is unknown", response.Principal.Kind)
	}
	for _, role := range response.Roles {
		if !validHumanRole(role) {
			return invalid("verified role %q is unknown", role)
		}
	}
	if !validID(response.VerificationID) {
		return invalid("verification_id is invalid")
	}
	return nil
}

// ReportRequest is the verified resource report input. Actor and execution
// causation come from the verification record, never from this body, so the
// schema has no principal, approval or credential field.
type ReportRequest struct {
	ResourceID     string  `json:"resource_id"`
	SourceEventID  string  `json:"source_event_id"`
	EventType      string  `json:"event_type"`
	Operation      string  `json:"operation"`
	VerificationID string  `json:"verification_id"`
	Subject        Subject `json:"subject"`
}

// Validate checks the resource, source event, declared report type, resource
// operation, verification and typed subject.
func (request ReportRequest) Validate() error {
	if !validID(request.ResourceID) {
		return invalid("resource_id is invalid")
	}
	if !validID(request.SourceEventID) {
		return invalid("source_event_id is invalid")
	}
	if !validReportType(request.EventType) {
		return invalid("event_type %q is not a reportable type", request.EventType)
	}
	if !ResourceOperation(request.Operation).Valid() {
		return invalid("operation %q is not a resource operation", request.Operation)
	}
	if request.Operation == string(ResourceOperationGitRead) {
		// A repository read check is verification-only: it asserts no effect,
		// so it can never stand behind a verified resource report.
		return invalid("git.read is not a reportable operation")
	}
	if !validID(request.VerificationID) {
		return invalid("verification_id is invalid")
	}
	return request.Subject.Validate()
}

// DecodeReportRequest decodes one bounded verified-report input.
func DecodeReportRequest(data []byte) (ReportRequest, error) {
	if len(data) > MaxRequestBytes {
		return ReportRequest{}, invalid("report request is %d bytes; at most %d are allowed", len(data), MaxRequestBytes)
	}
	var request ReportRequest
	if err := decodeStrict(data, &request); err != nil {
		return ReportRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return ReportRequest{}, err
	}
	return request, nil
}

// ReportResponse is the committed report result.
type ReportResponse struct {
	VerificationID string `json:"verification_id"`
	EventID        string `json:"event_id"`
	Replayed       bool   `json:"replayed,omitempty"`
}

// Validate checks the verification and accepted event identity.
func (response ReportResponse) Validate() error {
	if !validID(response.VerificationID) {
		return invalid("verification_id is invalid")
	}
	if !validID(response.EventID) {
		return invalid("event_id is invalid")
	}
	return nil
}
