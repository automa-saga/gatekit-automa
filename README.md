# gatekit-automa

A durable, crash-resumable migration **runner** for
[`gatekit`](https://github.com/automa-saga/gatekit), backed by
[`automa`](https://github.com/automa-saga/automa).

A gatekit migration `Plan` is a saga: ordered steps, each with a forward action
(`Migration.Execute`) and a compensation (`Migration.Rollback`). `automa` runs
exactly that — sequential execution with automatic reverse-order rollback on
failure — and, with a journal, survives a process crash and resumes where it
stopped. `gatekit-automa` maps a `Plan` onto an `automa` workflow so `finalize`
is crash-safe.

## Why a separate module

`github.com/automa-saga/gatekit` is deliberately **zero-dependency**: anything it
imports lands in every consumer's module graph. `automa` is imported **here and
nowhere in the gatekit module**, so a plain gatekit consumer never inherits it.
Consumers that don't need durability use the dependency-free
`migration.StdRunner` in gatekit itself; consumers that want crash-safe finalize
use this `Runner`.

## Usage

```go
import (
    "github.com/automa-saga/gatekit/migration"
    gkautoma "github.com/automa-saga/gatekit-automa"
)

plan, _ := migration.NewPlanner(migs...).Plan(ctx, store.Current(), target)

// Durable: journals progress and resumes after a crash.
runner := gkautoma.NewRunner(gkautoma.WithJournal("/var/lib/app/finalize.journal"))

_ = runner.RunNow(ctx, plan)          // local steps, at install
_ = runner.Finalize(ctx, plan, store) // shared steps, then advance active — crash-safe

// Retreat: lower active, undoing reversible migrations — also crash-safe.
lowerPlan, _ := migration.NewPlanner(migs...).PlanLower(store.Current(), target, min)
_ = runner.Lower(ctx, lowerPlan, store)
```

`Runner` implements `migration.Runner`, so it is a drop-in replacement for
`migration.StdRunner`. For direct control, the workflow builders are exported:
`RunNowWorkflow(plan)`, `FinalizeWorkflow(plan, store)`, and
`LowerWorkflow(lowerPlan, store)` return an `*automa.WorkflowBuilder` you can run
with automa's own execution modes, notifications, and journal.

- **Compensation** — forward steps run in plan order; the first failure compensates the
  completed steps in reverse (automa's `RollbackOnError`).
- **Finalize is the one-way line** — `Finalize` runs the run-at-finalize (shared) steps,
  then advances the active version via `Store.SetActive` as the terminal step.
  The advance lands only if every run-at-finalize step succeeds; a failure compensates and
  leaves the active version unchanged.
- **Write floor** — `RunNow` and `Finalize` call `plan.CheckWriteFloor` first and
  refuse, executing nothing, when a step would write data the oldest supported version
  cannot read (returns a `*migration.WriteFloorError`).
- **Lower is the reverse** — `Lower` runs each step's `Rollback` in plan order, then lowers
  the active version, mirroring finalize (data first, active last). It runs in `StopOnError`
  mode with no compensation: a failure stops without re-applying a rolled-back migration, so
  re-running (or resuming) completes the retreat — the same contract as `StdRunner.Lower`.
- **Resume after a crash** — with `WithJournal`, automa write-aheads each step,
  so a re-run resumes: completed steps are skipped, an interrupted step re-runs. This
  applies to `RunNow`, `Finalize`, and `Lower`. `Migration.Execute`/`Rollback` MUST be
  idempotent (automa durability spec §7); gatekit's `Migration.Applies` already encourages
  ground-truth, re-runnable steps.

## Version pinning

This module tracks tagged `gatekit` releases; it currently requires `gatekit`
`v0.7.0`. The `migration.Runner` interface it implements — including `Lower` and the
write-floor preflight — has been stable since `v0.6.0`.

**Compatibility is the `require` above, not a version table.** `go get` resolves a working
gatekit + gatekit-automa pair and a mismatch is a compile error. Use the gatekit-automa
release built for your gatekit version. The version numbers of the two modules are
independent (each is cut by semantic-release from its own commits); a new gatekit-automa
release follows each gatekit release that changes `migration.Runner`.

## Development

```bash
task build        # compile
task test:unit    # race + coverage
task lint:check   # gofmt check
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
