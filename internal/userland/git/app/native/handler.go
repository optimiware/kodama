package native

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/userland/git"
)

// RejectionMessage is the single sanitized diagnostic every hook refusal
// prints. Every refusal, including an unknown, forged or expired handle, is
// identical so the callback is not an oracle.
const RejectionMessage = "kodama git: update rejected"

// Handler handles one decoded hook callback request.
type Handler interface {
	Handle(ctx context.Context, request Request) Response
}

// PolicyValidator checks the policy content a candidate commit would install.
// The hook handler calls it during pre-receive, before receive-pack moves any
// ref, so an invalid proposal is refused while nothing has changed.
type PolicyValidator interface {
	ValidateCommit(ctx context.Context, commit string) error
}

// HookHandlerConfig is the complete callback dependency set.
type HookHandlerConfig struct {
	// Service is the tenant-bound Git service the git.Hook delegates to.
	Service *git.Service
	// Actors resolves the opaque handle. The raw credential is used only by the
	// immediate in-process verification and is never returned to the hook.
	Actors git.ActorSource
	// MaxLineBytes and MaxBatch bound the delegated stdin parse. Zero values
	// select the adapter defaults.
	MaxLineBytes int
	MaxBatch     int
	// Logger records sanitized refusal causes. It never receives a credential.
	Logger *slog.Logger
	// RepositoryPath resolves the tenant-scoped repository directory. It is
	// required when object environment entries are forwarded: every forwarded
	// path must stay under the repository's object directory.
	RepositoryPath func(repository string) (string, error)
	// Policy is the committed policy source. Every phase reads it before the
	// delegated adapter acts, so a post-receive delivery or a duplicate
	// journal delivery can never settle an entry after the committed policy
	// became unreadable or stopped declaring the pushed repository.
	Policy git.PolicySource
	// PolicyRepository is the tenant-relative repository whose committed
	// refs/heads/main is the trusted policy. When it is set, PolicyValidator is
	// required and every pre-receive batch that moves that ref is validated.
	PolicyRepository string
	// PolicyValidator validates the policy blob of a proposed commit.
	PolicyValidator PolicyValidator
}

// HookHandler delegates one callback to the git.Hook adapter. A fresh
// adapter is built per request so the environment is exactly the request's
// non-secret fields; the process environment is never mutated.
type HookHandler struct {
	service         *git.Service
	actors          git.ActorSource
	policy          git.PolicySource
	maxLine         int
	maxBatch        int
	logger          *slog.Logger
	repositoryPath  func(string) (string, error)
	policyRepo      string
	policyValidator PolicyValidator
}

var _ Handler = (*HookHandler)(nil)

// NewHookHandler validates the boundaries and returns the handler.
func NewHookHandler(config HookHandlerConfig) (*HookHandler, error) {
	if config.Service == nil {
		return nil, fmt.Errorf("hook callback service is missing")
	}
	if config.Actors == nil {
		return nil, fmt.Errorf("hook callback actor source is missing")
	}
	if config.Policy == nil {
		return nil, fmt.Errorf("hook callback policy source is missing")
	}
	policyRepository := strings.TrimSpace(config.PolicyRepository)
	if policyRepository != "" && config.PolicyValidator == nil {
		return nil, fmt.Errorf("hook callback policy validator is missing for repository %q", policyRepository)
	}
	return &HookHandler{
		service:         config.Service,
		actors:          config.Actors,
		policy:          config.Policy,
		maxLine:         config.MaxLineBytes,
		maxBatch:        config.MaxBatch,
		logger:          config.Logger,
		repositoryPath:  config.RepositoryPath,
		policyRepo:      policyRepository,
		policyValidator: config.PolicyValidator,
	}, nil
}

// Handle resolves the handle first, so a forged or expired handle is refused
// uniformly for every phase (the post-receive adapter would otherwise
// not resolve it), then delegates to the exact hook adapter.
func (handler *HookHandler) Handle(ctx context.Context, request Request) Response {
	if handler == nil || handler.service == nil || handler.actors == nil {
		return rejectResponse()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		handler.logRejection(request, err)
		return rejectResponse()
	}
	if _, err := handler.actors.ResolveActor(ctx, request.ActorRef); err != nil {
		handler.logRejection(request, err)
		return rejectResponse()
	}
	// Every phase, including post-receive and a duplicate delivery of an
	// already accepted entry, is gated by the committed policy. The source is
	// read again per delivery and never cached, so a policy that became
	// unreadable or stopped declaring the repository refuses before the
	// delegated adapter can settle anything.
	policy, err := handler.policy.Current(ctx)
	if err != nil {
		handler.logRejection(request, err)
		return rejectResponse()
	}
	if _, ok := policy.Policy(request.Repository); !ok {
		handler.logRejection(request, fmt.Errorf("repository %q is not declared by the committed policy", request.Repository))
		return rejectResponse()
	}
	if err := handler.validateObjectEnvironment(request); err != nil {
		handler.logRejection(request, err)
		return rejectResponse()
	}
	ctx = WithCommandEnvironment(ctx, request.ObjectEnvironment)
	if request.Phase == PhasePreReceive {
		// The trusted policy repository validates a proposed policy from the
		// candidate commit here, while the objects are still quarantined and
		// receive-pack has not moved any ref.
		if err := handler.validatePolicyProposal(ctx, request); err != nil {
			handler.logRejection(request, err)
			return rejectResponse()
		}
	}
	environment := func(key string) (string, bool) {
		switch key {
		case git.EnvActorRef:
			return request.ActorRef, true
		case git.EnvRepository:
			return request.Repository, true
		case git.EnvResourceID:
			return request.ResourceID, true
		case git.EnvSourceEvent:
			return request.SourceEvent, true
		default:
			return "", false
		}
	}
	hook, err := git.NewHook(git.HookConfig{
		Service:      handler.service,
		Actors:       handler.actors,
		Environment:  environment,
		MaxLineBytes: handler.maxLine,
		MaxBatch:     handler.maxBatch,
	})
	if err != nil {
		return rejectResponse()
	}
	var result git.HookResult
	switch request.Phase {
	case PhaseUpdate:
		result, err = hook.RunUpdate(ctx, request.Ref, request.Old, request.New)
	case PhasePreReceive:
		result, err = hook.RunPreReceive(ctx, request.LinesReader())
	case PhasePostReceive:
		result, err = hook.RunPostReceive(ctx, request.LinesReader())
	default:
		handler.logRejection(request, fmt.Errorf("unknown phase %q", request.Phase))
		return rejectResponse()
	}
	if err != nil {
		handler.logRejection(request, err)
		return rejectResponse()
	}
	if !result.Accepted() {
		handler.logRejection(request, fmt.Errorf("decision %q exit %d", result.Decision, result.ExitCode))
		return rejectResponse()
	}
	return Response{ExitCode: 0}
}

// logRejection records one sanitized cause. It never receives the request's
// actor reference value or any credential.
func (handler *HookHandler) logRejection(request Request, cause error) {
	if handler.logger == nil {
		return
	}
	handler.logger.Warn("git hook callback refused",
		"phase", string(request.Phase),
		"repository", request.Repository,
		"ref", request.Ref,
		"cause", cause.Error(),
	)
}

// validatePolicyProposal checks every proposed update of the trusted policy
// ref against the candidate commit before receive-pack applies anything. A
// delete is left to the adapter's uniform refusal, and a push to any
// other ref (a work branch) is not policy and is not validated.
func (handler *HookHandler) validatePolicyProposal(ctx context.Context, request Request) error {
	if handler.policyRepo == "" || handler.policyValidator == nil || request.Repository != handler.policyRepo {
		return nil
	}
	for _, line := range request.Lines {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return fmt.Errorf("%w: policy batch line", ErrMalformedRequest)
		}
		ref, candidate := fields[2], fields[1]
		if ref != CommittedPolicyRef {
			continue
		}
		if strings.Trim(candidate, "0") == "" {
			continue
		}
		if err := handler.policyValidator.ValidateCommit(ctx, candidate); err != nil {
			return fmt.Errorf("proposed policy on %s: %w", ref, err)
		}
	}
	return nil
}

// validateObjectEnvironment proves every forwarded git object path stays under
// the pushed repository's own object directory. A hostile hook caller cannot
// point the service's ancestry check at another repository's objects.
func (handler *HookHandler) validateObjectEnvironment(request Request) error {
	if len(request.ObjectEnvironment) == 0 {
		return nil
	}
	if handler.repositoryPath == nil {
		return fmt.Errorf("%w: object environment without a repository resolver", ErrMalformedRequest)
	}
	repositoryPath, err := handler.repositoryPath(request.Repository)
	if err != nil {
		return fmt.Errorf("%w: resolve repository: %v", ErrMalformedRequest, err)
	}
	return validateObjectEnvironment(repositoryPath, request.ObjectEnvironment)
}

// validateObjectEnvironment checks the allow-listed receive-pack object
// variables against the repository object directory.
func validateObjectEnvironment(repositoryPath string, environment []string) error {
	objectsDirectory := filepath.Join(repositoryPath, "objects")
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			return fmt.Errorf("%w: malformed object environment", ErrMalformedRequest)
		}
		switch key {
		case EnvObjectDirectory, EnvQuarantinePath:
			if err := withinDirectory(objectsDirectory, value); err != nil {
				return err
			}
		case EnvAlternateObjectDirectories:
			parts := strings.Split(value, string(os.PathListSeparator))
			if len(parts) == 0 {
				return fmt.Errorf("%w: empty alternate object directories", ErrMalformedRequest)
			}
			for _, part := range parts {
				if err := exactDirectory(objectsDirectory, part); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%w: object environment key %q is not allowed", ErrMalformedRequest, key)
		}
	}
	return nil
}

// withinDirectory reports whether value is an absolute, parent-free path
// strictly below base.
func withinDirectory(base, value string) error {
	cleaned, err := cleanAbsolute(value)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Clean(base), cleaned)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: object path %q escapes the repository objects directory", ErrMalformedRequest, value)
	}
	return nil
}

// exactDirectory reports whether value cleans to exactly base.
func exactDirectory(base, value string) error {
	cleaned, err := cleanAbsolute(value)
	if err != nil {
		return err
	}
	if cleaned != filepath.Clean(base) {
		return fmt.Errorf("%w: object path %q is not the repository objects directory", ErrMalformedRequest, value)
	}
	return nil
}

// cleanAbsolute rejects relative paths and any parent segment before cleaning.
func cleanAbsolute(value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%w: object path %q is not absolute", ErrMalformedRequest, value)
	}
	for _, segment := range strings.Split(filepath.ToSlash(value), "/") {
		if segment == ".." {
			return "", fmt.Errorf("%w: object path %q contains a parent segment", ErrMalformedRequest, value)
		}
	}
	return filepath.Clean(value), nil
}

func rejectResponse() Response {
	return Response{ExitCode: 1, Message: RejectionMessage}
}
