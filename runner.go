// SPDX-License-Identifier: Apache-2.0

// Package gatekitautoma provides a durable, crash-resumable implementation of the
// gatekit migration.Runner, backed by github.com/automa-saga/automa.
//
// A migration Plan is a saga: ordered steps, each with a forward action
// (Migration.Execute) and a compensation (Migration.Rollback). automa runs exactly
// that — sequential execution with reverse-order rollback on failure — and, with a
// journal, survives a process crash and resumes where it stopped. This package maps
// a Plan onto an automa workflow so finalize is crash-safe.
//
// automa is imported here and nowhere in the gatekit module, so it never enters a
// plain gatekit consumer's module graph. Consumers that do not need durability use
// the dependency-free migration.StdRunner instead.
package gatekitautoma

import (
	"context"
	"fmt"

	"github.com/automa-saga/automa"
	"github.com/automa-saga/gatekit/migration"
)

const (
	runNowWorkflowID   = "gatekit-run-now"
	finalizeWorkflowID = "gatekit-finalize"
	// advanceStepID is the terminal finalize step that advances the active version.
	// It is namespaced to avoid colliding with a migration ID.
	advanceStepID = "gatekit:advance-active"
)

// Runner is an automa-backed migration.Runner. It executes a Plan as an automa
// saga: steps run in plan order and a failure compensates the completed steps in
// reverse. With a journal (see WithJournal) a run also survives a crash and resumes
// from where it stopped. Runner implements migration.Runner.
type Runner struct {
	journalPath string
}

// Option configures a Runner.
type Option func(*Runner)

// WithJournal enables durability: the workflow journals its progress to path and a
// later run resumes from it after a crash (forward, or completing an interrupted
// rollback). Without it, execution is in-memory and writes nothing to disk.
//
// Durability shifts obligations onto the Migration author (automa's durability
// spec §7): Execute and Rollback MUST be idempotent, because a step recorded
// started-but-not-completed before a crash is re-executed on resume. gatekit's
// Migration.Applies already encourages idempotent, ground-truth-checked steps.
func WithJournal(path string) Option {
	return func(r *Runner) { r.journalPath = path }
}

// NewRunner returns an automa-backed Runner.
func NewRunner(opts ...Option) *Runner {
	r := &Runner{}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Runner satisfies migration.Runner.
var _ migration.Runner = (*Runner)(nil)

// RunNow executes the plan's run-now (host-local) steps as an automa saga, rolling
// back completed steps in reverse on failure. It does not advance the active
// version. With a journal it is crash-resumable. An empty run-now set is a no-op.
func (r *Runner) RunNow(ctx context.Context, plan migration.Plan) error {
	if len(plan.RunNow()) == 0 {
		return nil
	}
	return r.run(ctx, RunNowWorkflow(plan))
}

// Finalize runs the plan's held (interop) steps as an automa saga, then advances
// the active version to plan.To via store as the terminal step. The advance lands
// only if every held step succeeds (the one-way line); a failure compensates the
// executed held steps and leaves the active version unchanged. With a journal it is
// crash-resumable.
func (r *Runner) Finalize(ctx context.Context, plan migration.Plan, store migration.Store) error {
	return r.run(ctx, FinalizeWorkflow(plan, store))
}

// run executes a workflow, resuming from the journal when one is configured.
func (r *Runner) run(ctx context.Context, wb *automa.WorkflowBuilder) error {
	var report *automa.Report
	if r.journalPath != "" {
		report = automa.ResumeWorkflow(ctx, wb.WithJournal(r.journalPath), r.journalPath)
	} else {
		report = automa.RunWorkflow(ctx, wb)
	}
	if report.IsFailed() {
		if report.Error != nil {
			return fmt.Errorf("gatekit-automa: workflow %q failed: %w", wb.Id(), report.Error)
		}
		return fmt.Errorf("gatekit-automa: workflow %q failed", wb.Id())
	}
	return nil
}

// RunNowWorkflow builds the automa workflow for the plan's run-now (host-local)
// steps: each migration's Execute is the step action and Rollback its compensation,
// in plan order, with reverse-order rollback on failure. It is the building block
// the consumer may run directly (with automa's execution modes, notifications, and
// durability journal) instead of via Runner.
func RunNowWorkflow(plan migration.Plan) *automa.WorkflowBuilder {
	return workflowOf(runNowWorkflowID, stepsOf(plan.RunNow()))
}

// FinalizeWorkflow builds the automa workflow for the plan's held (interop) steps
// followed by a terminal step that advances the active version to plan.To via
// store. If any held step fails, the workflow compensates and the advance never
// runs, so the active version is unchanged.
func FinalizeWorkflow(plan migration.Plan, store migration.Store) *automa.WorkflowBuilder {
	steps := stepsOf(plan.Held())
	steps = append(steps, advanceStep(plan, store))
	return workflowOf(finalizeWorkflowID, steps)
}

// workflowOf assembles a RollbackOnError workflow from ordered step builders.
func workflowOf(id string, steps []*automa.StepBuilder) *automa.WorkflowBuilder {
	builders := make([]automa.Builder, len(steps))
	for i, s := range steps {
		builders[i] = s
	}
	return automa.NewWorkflowBuilder().
		WithId(id).
		WithExecutionMode(automa.RollbackOnError).
		Steps(builders...)
}

// stepsOf maps plan steps to automa step builders, preserving order.
func stepsOf(planSteps []migration.Step) []*automa.StepBuilder {
	out := make([]*automa.StepBuilder, 0, len(planSteps))
	for _, s := range planSteps {
		out = append(out, stepFor(s.Migration))
	}
	return out
}

// stepFor maps one migration to an automa step: Execute → forward action,
// Rollback → compensation.
func stepFor(m migration.Migration) *automa.StepBuilder {
	return automa.NewStepBuilder().
		WithId(m.ID()).
		WithExecute(func(ctx context.Context, stp automa.Step) *automa.Report {
			if err := m.Execute(ctx); err != nil {
				return automa.FailureReport(stp, automa.WithError(err))
			}
			return automa.SuccessReport(stp)
		}).
		WithRollback(func(ctx context.Context, stp automa.Step) *automa.Report {
			if err := m.Rollback(ctx); err != nil {
				return automa.FailureReport(stp, automa.WithError(err))
			}
			return automa.SuccessReport(stp)
		})
}

// advanceStep is the terminal finalize step that persists the new active version.
// It has no compensation: advancing is the one-way line, and if it fails the
// already-run held steps are compensated by the workflow.
func advanceStep(plan migration.Plan, store migration.Store) *automa.StepBuilder {
	return automa.NewStepBuilder().
		WithId(advanceStepID).
		WithExecute(func(ctx context.Context, stp automa.Step) *automa.Report {
			if err := store.SetActive(plan.To); err != nil {
				return automa.FailureReport(stp, automa.WithError(err))
			}
			return automa.SuccessReport(stp)
		})
}
