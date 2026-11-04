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

_ = runner.RunNow(ctx, plan)          // host-local steps, at install
_ = runner.Finalize(ctx, plan, store) // interop steps, then advance active — crash-safe
```

`Runner` implements `migration.Runner`, so it is a drop-in replacement for
`migration.StdRunner`. For direct control, the workflow builders are exported:
`RunNowWorkflow(plan)` and `FinalizeWorkflow(plan, store)` return an
`*automa.WorkflowBuilder` you can run with automa's own execution modes,
notifications, and journal.

- **Compensation** — steps run in plan order; the first failure compensates the
  completed steps in reverse (automa's `RollbackOnError`).
- **Finalize is the one-way line** — `Finalize` runs the held (interop) steps,
  then advances the active version via `Store.SetActive` as the terminal step.
  The advance lands only if every held step succeeds; a failure compensates and
  leaves the active version unchanged.
- **Resume after a crash** — with `WithJournal`, automa write-aheads each step,
  so a re-run resumes: completed steps are skipped, an interrupted step re-runs.
  `Migration.Execute`/`Rollback` MUST be idempotent (automa durability spec §7);
  gatekit's `Migration.Applies` already encourages ground-truth, re-runnable steps.

## Version pinning

Until `gatekit` cuts a release that includes the `migration.Runner` interface,
this module pins `gatekit` by a pseudo-version of the interface commit. Once that
release is tagged, bump the `require` to the release version.

## Development

```bash
task build        # compile
task test:unit    # race + coverage
task lint:check   # gofmt check
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
