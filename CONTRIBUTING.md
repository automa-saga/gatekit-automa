# Contributing to gatekit-automa

Thanks for your interest in contributing! Bug reports, documentation fixes, and
code contributions are all welcome.

`gatekit-automa` is the durable, crash-resumable migration runner for
[`gatekit`](https://github.com/automa-saga/gatekit), backed by
[`automa`](https://github.com/automa-saga/automa). It is a **separate module** so
that `automa` never enters a plain gatekit consumer's module graph — keep that
boundary in mind when contributing (nothing here should be needed by gatekit
itself).

## Prerequisites

- **Go 1.27+** (see [`go.mod`](go.mod)).
- **[Task](https://taskfile.dev/installation/)** — the task runner used for all
  common commands ([`Taskfile.yaml`](Taskfile.yaml)).
- This module `require`s `github.com/automa-saga/gatekit` and
  `github.com/automa-saga/automa`. While gatekit is a private repository, set
  `GOPRIVATE=github.com/automa-saga/*` (and have Git credentials for it) so the
  modules resolve.

## Getting started

```sh
git clone https://github.com/<your-username>/gatekit-automa.git
cd gatekit-automa
export GOPRIVATE=github.com/automa-saga/*

task build
task lint:check
task test:unit
```

Common tasks:

| Command | What it does |
| --- | --- |
| `task build` | `go build -v ./...` |
| `task lint` | Format the code (`go fmt ./...`) |
| `task lint:check` | Fail if any file is not gofmt-clean (CI runs this) |
| `task test:unit` | Run the unit tests (`-race -cover`) and write `unit-test-report.md` |
| `task test:coverage` | Run tests with coverage gating ([`.testcoverage.yml`](.testcoverage.yml)) |
| `task clean` | Remove test artifacts and clear the Go build/test cache |

## Making a change

1. Create a branch off `main`.
2. Keep commits focused, and keep `automa` usage confined to this module.
3. Add or update tests. Durability behavior (resume/compensation) should be
   covered — see the crash-then-resume test in `runner_test.go` for the pattern.
4. Run `task lint:check` and `task test:unit` locally — CI runs the same checks.
5. Update the README/GoDoc when the public API changes.

### Definition of done

- [ ] `task build` succeeds.
- [ ] `task lint:check` is clean.
- [ ] `task test:unit` passes, including new/updated tests.
- [ ] `task test:coverage` meets the thresholds.
- [ ] The automa dependency stays confined to this module.

## Versioning note

Until gatekit cuts a release that includes the `migration.Runner` interface, this
module pins gatekit by a pseudo-version of the interface commit. Once that release
is tagged, the `require` is bumped to the release version.

## Commits & sign-off

- **DCO sign-off is required.** Every commit must be signed off, certifying the
  [Developer Certificate of Origin](https://developercertificate.org/). Add the
  `Signed-off-by` trailer with `git commit -s`.
- **Commit messages** follow the
  [Conventional Commits](https://www.conventionalcommits.org/) format.

## Pull requests

- Open your PR against `main`.
- PRs are **squash-merged**, and the **PR title becomes the squashed commit
  message** — so the PR title **must** follow Conventional Commits. This drives
  automated release versioning.
- Keep PRs focused, and ensure CI is green before requesting review.

## License

By contributing, you agree that your contributions will be licensed under the
project's [Apache License 2.0](LICENSE).
