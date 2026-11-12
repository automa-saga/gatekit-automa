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
	lowerWorkflowID    = "gatekit-lower"
	// advanceStepID is the terminal finalize step that advances the active version.
	// It is namespaced to avoid colliding with a migration ID.
	advanceStepID = "gatekit:advance-active"
	// lowerAdvanceStepID is the terminal lower step that lowers the active version.
	lowerAdvanceStepID = "gatekit:lower-active"
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

// RunNow executes the plan's run-now (local) steps as an automa saga, rolling
// back completed steps in reverse on failure. It does not advance the active
// version. With a journal it is crash-resumable. An empty run-now set is a no-op.
//
// It calls plan.CheckWriteFloor(true) first and returns, executing nothing, if a
// run-now step is blocked by the write floor.
func (r *Runner) RunNow(ctx context.Context, plan migration.Plan) error {
	if err := plan.CheckWriteFloor(true); err != nil {
		return err
	}
	if len(plan.RunNow()) == 0 {
		return nil
	}
	return r.run(ctx, RunNowWorkflow(plan))
}

// Finalize runs the plan's run-at-finalize (shared) steps as an automa saga, then advances
// the active version to plan.To via store as the terminal step. The advance lands
// only if every run-at-finalize step succeeds (the one-way line); a failure compensates the
// executed run-at-finalize steps and leaves the active version unchanged. With a journal it is
// crash-resumable.
//
// It calls plan.CheckWriteFloor(false) first and returns, executing nothing, if a
// run-at-finalize step is blocked by the write floor.
func (r *Runner) Finalize(ctx context.Context, plan migration.Plan, store migration.Store) error {
	if err := plan.CheckWriteFloor(false); err != nil {
		return err
	}
	return r.run(ctx, FinalizeWorkflow(plan, store))
}

// Lower runs a LowerPlan's rollbacks as an automa saga — each step's Rollback is the
// forward action, in plan order — then lowers the active version to plan.To via store as
// the terminal step. Data first, active last: the active version lands only after every
// rollback succeeds; a failure stops the workflow and leaves the active version unchanged.
//
// Unlike Finalize, the saga runs in StopOnError mode with no compensation: on a rollback
// failure it stops without re-applying a migration it just rolled back. Because Rollback is
// idempotent, re-running Lower (or resuming from a journal) completes the retreat — the same
// recovery contract as migration.StdRunner.Lower.
func (r *Runner) Lower(ctx context.Context, plan migration.LowerPlan, store migration.Store) error {
	return r.run(ctx, LowerWorkflow(plan, store))
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

// RunNowWorkflow builds the automa workflow for the plan's run-now (local)
// steps: each migration's Execute is the step action and Rollback its compensation,
// in plan order, with reverse-order rollback on failure. It is the building block
// the consumer may run directly (with automa's execution modes, notifications, and
// durability journal) instead of via Runner.
func RunNowWorkflow(plan migration.Plan) *automa.WorkflowBuilder {
	return workflowOf(runNowWorkflowID, automa.RollbackOnError, stepsOf(plan.RunNow()))
}

// FinalizeWorkflow builds the automa workflow for the plan's run-at-finalize (shared) steps
// followed by a terminal step that advances the active version to plan.To via
// store. If any run-at-finalize step fails, the workflow compensates and the advance never
// runs, so the active version is unchanged.
func FinalizeWorkflow(plan migration.Plan, store migration.Store) *automa.WorkflowBuilder {
	steps := stepsOf(plan.RunAtFinalize())
	steps = append(steps, advanceStep(plan, store))
	return workflowOf(finalizeWorkflowID, automa.RollbackOnError, steps)
}

// LowerWorkflow builds the automa workflow for a LowerPlan: each step's Rollback is the
// forward action, in plan order, followed by a terminal step that lowers the active version
// to plan.To via store. It runs in StopOnError mode with no compensation — on a rollback
// failure it stops and never re-applies a migration it just rolled back — so re-running (or
// resuming from a journal) is the recovery, matching migration.StdRunner.Lower. It is the
// building block a consumer may run directly instead of via Runner.Lower.
func LowerWorkflow(plan migration.LowerPlan, store migration.Store) *automa.WorkflowBuilder {
	steps := lowerStepsOf(plan.Steps)
	steps = append(steps, lowerAdvanceStep(plan, store))
	return workflowOf(lowerWorkflowID, automa.StopOnError, steps)
}

// workflowOf assembles a workflow with the given execution mode from ordered step builders.
func workflowOf(id string, mode automa.TypeMode, steps []*automa.StepBuilder) *automa.WorkflowBuilder {
	builders := make([]automa.Builder, len(steps))
	for i, s := range steps {
		builders[i] = s
	}
	return automa.NewWorkflowBuilder().
		WithId(id).
		WithExecutionMode(mode).
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
// already-run run-at-finalize steps are compensated by the workflow.
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

// lowerStepsOf maps a LowerPlan's migrations to automa step builders, preserving order.
func lowerStepsOf(migs []migration.Migration) []*automa.StepBuilder {
	out := make([]*automa.StepBuilder, 0, len(migs))
	for _, m := range migs {
		out = append(out, lowerStepFor(m))
	}
	return out
}

// lowerStepFor maps one migration to a lowering step: its Rollback is the forward action.
// It has no compensation — StopOnError never re-applies a completed rollback — so a failed
// lowering leaves earlier rollbacks in place and is recovered by re-running (Rollback is
// idempotent), not by undoing them.
func lowerStepFor(m migration.Migration) *automa.StepBuilder {
	return automa.NewStepBuilder().
		WithId(m.ID()).
		WithExecute(func(ctx context.Context, stp automa.Step) *automa.Report {
			if err := m.Rollback(ctx); err != nil {
				return automa.FailureReport(stp, automa.WithError(err))
			}
			return automa.SuccessReport(stp)
		})
}

// lowerAdvanceStep is the terminal lower step that persists the lowered active version. It
// runs only after every rollback step succeeds, so active lands last; it has no compensation.
func lowerAdvanceStep(plan migration.LowerPlan, store migration.Store) *automa.StepBuilder {
	return automa.NewStepBuilder().
		WithId(lowerAdvanceStepID).
		WithExecute(func(ctx context.Context, stp automa.Step) *automa.Report {
			if err := store.SetActive(plan.To); err != nil {
				return automa.FailureReport(stp, automa.WithError(err))
			}
			return automa.SuccessReport(stp)
		})
}
