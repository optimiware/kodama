package app

// The one-shot email intake composition entry.
//
// RunEmailIntake is composition only: it validates the trusted configuration,
// selects the injected public handler (or the public transport) and delegates
// the bounded message read and the single create-and-start to the services
// intake. It never selects scope from the message and starts no long-running
// process.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

// EmailIntakeConfig is the complete email intake composition input. The
// message source and the public boundary are required, as are the trusted
// tenant, project and pipeline.
type EmailIntakeConfig struct {
	TenantID   id.TenantID
	ProjectID  id.ProjectID
	PipelineID string

	// Message is the one bounded RFC822 message source.
	Message io.Reader
	// Transport is the public client boundary. It is required unless Handler is
	// injected.
	Transport client.Transport
	// Handler is an optional injected public operation handler.
	Handler api.Handler
}

// RunEmailIntake reads one bounded message and creates and starts one task with
// the trusted scope and pipeline. It propagates every intake refusal.
func RunEmailIntake(ctx context.Context, config EmailIntakeConfig) (services.EmailIntakeResult, error) {
	if config.Message == nil {
		return services.EmailIntakeResult{}, fmt.Errorf("email intake requires a message source")
	}
	if strings.TrimSpace(string(config.TenantID)) == "" {
		return services.EmailIntakeResult{}, fmt.Errorf("email intake requires a tenant")
	}
	if strings.TrimSpace(string(config.ProjectID)) == "" {
		return services.EmailIntakeResult{}, fmt.Errorf("email intake requires a project")
	}
	if strings.TrimSpace(config.PipelineID) == "" {
		return services.EmailIntakeResult{}, fmt.Errorf("email intake requires a pipeline")
	}
	handler := config.Handler
	if handler == nil {
		if config.Transport == nil {
			return services.EmailIntakeResult{}, fmt.Errorf("email intake requires a public transport or handler")
		}
		handler = config.Transport
	}
	return services.IntakeEmail(ctx, services.EmailIntakeConfig{
		TenantID:   config.TenantID,
		ProjectID:  config.ProjectID,
		PipelineID: config.PipelineID,
		Handler:    handler,
	}, config.Message)
}
