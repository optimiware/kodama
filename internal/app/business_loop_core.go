package app

// The PostgreSQL-backed construction of the business loop (called by NewCore).
// It is the only place the real adapters of the dependency graph, the flow
// summaries, the QA mapping and ChatOps are chosen and the only place the
// identity authority is bound to them; the slice itself is AssembleBusinessLoop.
// Nothing here is exercised without a database (T-044 qualifies the joined
// transactions, notification delivery and replay); the pure parts it relies on
// are unit-tested in business_loop_test.go.

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity/consumer"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	chatopspg "github.com/optimiware/kodama/internal/integrations/chatops/postgres"
	"github.com/optimiware/kodama/internal/integrations/chatops/webhook"
	"github.com/optimiware/kodama/internal/metrics"
	metricsauth "github.com/optimiware/kodama/internal/metrics/auth"
	metricspg "github.com/optimiware/kodama/internal/metrics/postgres"
	"github.com/optimiware/kodama/internal/qa"
	qaadapters "github.com/optimiware/kodama/internal/qa/adapters"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
	dependenciespg "github.com/optimiware/kodama/internal/work/dependencies/postgres"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// chatopsDurable is the JetStream durable of the ChatOps notification consumer.
// The consumer name (chatops.ConsumerName) keys the inbox and never changes; the
// durable only names the broker's cursor.
const chatopsDurable = "kodama-chatops"

// txWorkPermissions binds the identity module's transaction-bound permission
// check to one mutating work transaction, so a revocation committed while a
// mutation waited for its locks is seen before anything is written.
func txWorkPermissions(tx pgx.Tx) work.PermissionChecker { return consumer.NewTxPermissions(tx) }

// dependencyGuards is the guard set the work store's binder contributes in the
// production composition: the M1 explicit "none" dependency selection is
// replaced by the transaction-bound business dependency graph, so a
// ready → active transition reads the current links and prerequisite states
// inside its own transaction. The writer and review-subject guards are bound by
// the callers around it.
func dependencyGuards(tx pgx.Tx) work.TxGuards {
	return work.TxGuards{Dependencies: dependenciespg.NewDependencyGuard(tx)}
}

// businessLoopDeps are the already-built modules the business-loop adapters
// bridge to.
type businessLoopDeps struct {
	cfg     BusinessLoopConfig
	env     Env
	log     *slog.Logger
	tuning  Tuning
	cursors *httpapi.CursorCodec

	pool          *storage.Pool
	events        *eventspg.Store
	workAuthority work.Authority
	executions    *execpg.Store
	deliveries    *deliverypg.Store
	artifacts     *artifactspg.Store
	objects       *arts3.Store
	tenants       execution.Tenants
}

// buildBusinessLoopPG builds the slice and the event-bus bindings of its
// consumers. ChatOps and the QA mapping are built only when the installation
// selected them; the dependency graph and the flow summaries always are.
func buildBusinessLoopPG(d businessLoopDeps) (*BusinessLoopSlice, []eventspg.Binding, error) {
	// ---- business dependency graph (T-045): links share the work module's
	// transactions and locks; the guard is bound in the work store itself ----
	depStore, err := dependenciespg.NewStore(d.pool, d.events, txWorkPermissions)
	if err != nil {
		return nil, nil, err
	}
	depSvc, err := dependencies.NewService(dependencies.Config{Repository: depStore, Authority: d.workAuthority})
	if err != nil {
		return nil, nil, err
	}

	// ---- SQL flow summaries (T-042): a read model over the immutable work
	// history, authorized by the work module's own read permission ----
	flowStore, err := metricspg.NewStore(d.pool)
	if err != nil {
		return nil, nil, err
	}
	flowAuth, err := metricsauth.NewWorkReadAuthorizer(d.workAuthority)
	if err != nil {
		return nil, nil, err
	}
	flowSvc, err := metrics.NewService(metrics.Config{Reader: flowStore, Authorizer: flowAuth})
	if err != nil {
		return nil, nil, err
	}

	in := BusinessLoopInputs{
		Env: d.env, Log: d.log, Cursors: d.cursors, Dependencies: depSvc, Flow: flowSvc,
		Adapters: []any{depStore, flowStore, flowAuth},
	}
	var bindings []eventspg.Binding

	// ---- QA evidence-to-defect mapping (T-040): a second work store carries the
	// narrowing guard, so the mapping can read, create and link evidence and can
	// move nothing ----
	if q := d.cfg.QA; q != nil {
		qaWork, err := workpg.NewStore(d.pool, d.events, qaadapters.WorkGuards(d.artifacts, txWorkPermissions))
		if err != nil {
			return nil, nil, err
		}
		attempts, err := qaadapters.NewAttempts(d.executions)
		if err != nil {
			return nil, nil, err
		}
		deployments, err := qaadapters.NewDeployments(d.deliveries)
		if err != nil {
			return nil, nil, err
		}
		evidence, err := qaadapters.NewArtifacts(d.artifacts, d.objects, nil)
		if err != nil {
			return nil, nil, err
		}
		defects, err := qaadapters.NewWork(qaWork, d.workAuthority, q.MappingPrincipal, nil)
		if err != nil {
			return nil, nil, err
		}
		qaSvc, err := qa.NewService(qa.Config{
			Attempts: attempts, Deployments: deployments, Artifacts: evidence, Defects: defects,
			ReportOutput: q.ReportOutput, DeploymentInput: q.DeploymentInput,
		})
		if err != nil {
			return nil, nil, err
		}
		in.QA = qaSvc
		in.Adapters = append(in.Adapters, qaWork, attempts, deployments, evidence, defects)
	}

	// ---- ChatOps (T-041): the planner joins the event bus's inbox transaction,
	// the worker sends only durable intents, the management routes are
	// authorized by the standalone identity authority ----
	if c := d.cfg.ChatOps; c != nil {
		parts, binding, adapters, err := buildChatOpsPG(d, *c)
		if err != nil {
			return nil, nil, fmt.Errorf("chatops: %w", err)
		}
		in.ChatOps = parts
		in.Adapters = append(in.Adapters, adapters...)
		bindings = append(bindings, binding)
	}
	slice, err := AssembleBusinessLoop(in)
	if err != nil {
		return nil, nil, err
	}
	return slice, bindings, nil
}

func buildChatOpsPG(d businessLoopDeps, c ChatOpsConfig) (*ChatOpsParts, eventspg.Binding, []any, error) {
	if d.tuning.ChatOps.Validate() != nil {
		return nil, eventspg.Binding{}, nil, errors.New("the ChatOps worker tuning is not valid")
	}
	links, err := c.linkBuilder()
	if err != nil {
		return nil, eventspg.Binding{}, nil, err
	}
	planner, err := chatops.NewPlanner(links, nil)
	if err != nil {
		return nil, eventspg.Binding{}, nil, err
	}
	binding, err := chatopspg.NewBinding(planner)
	if err != nil {
		return nil, eventspg.Binding{}, nil, err
	}
	store, err := chatopspg.NewStore(chatopspg.Config{DB: d.pool, Authorizer: chatopspg.IdentityAuthorizer{}})
	if err != nil {
		return nil, eventspg.Binding{}, nil, err
	}
	policy, err := c.policy(d.env)
	if err != nil {
		return nil, eventspg.Binding{}, nil, err
	}
	roots, err := certPool(c.CAFile)
	if err != nil {
		return nil, eventspg.Binding{}, nil, fmt.Errorf("CA file: %w", err)
	}
	// No credential source and no operator authority are selected: an
	// authenticated destination and the restore fence are refused by the module
	// instead of defaulting (T-061 qualifies hosted custody).
	client, err := webhook.NewClient(webhook.Config{Policy: policy, RootCAs: roots})
	if err != nil {
		return nil, eventspg.Binding{}, nil, err
	}
	t := d.tuning.ChatOps
	svc, err := chatops.NewService(chatops.Config{
		Store: store, Policy: policy, Fences: store, MaxDestinationsPerProject: t.MaxDestinationsPerProject, Logger: d.log,
	})
	if err != nil {
		return nil, eventspg.Binding{}, nil, err
	}
	worker, err := chatops.NewWorker(chatops.WorkerConfig{
		Work: store, Tenants: d.tenants, Sender: client, Retry: t.Retry, Interval: t.Interval, Batch: t.Batch,
		Lease: t.Lease, AttemptTimeout: t.AttemptTimeout, Concurrency: t.Concurrency, Logger: d.log,
	})
	if err != nil {
		return nil, eventspg.Binding{}, nil, err
	}
	return &ChatOpsParts{Service: svc, Worker: worker, Consumer: binding.Spec, Durable: chatopsDurable}, binding,
		[]any{store, policy, client, chatopspg.IdentityAuthorizer{}}, nil
}
