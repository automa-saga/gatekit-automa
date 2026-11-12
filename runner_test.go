// SPDX-License-Identifier: Apache-2.0

package gatekitautoma

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/automa-saga/gatekit/migration"
)

// fakeMig is a test Migration. crashOn > 0 panics on that Execute call number (to
// simulate a process crash); execErr fails every Execute. rollErr fails every Rollback,
// and rollCrashOn > 0 panics on that Rollback call number. reversible and readableFrom back
// the optional Reversible/Readable interfaces.
type fakeMig struct {
	id           string
	version      int
	scope        migration.Scope
	execErr      error
	rollErr      error
	crashOn      int
	rollCrashOn  int
	reversible   bool
	readableFrom int
	execCount    int
	rollCount    int
}

func (m *fakeMig) ID() string                            { return m.id }
func (m *fakeMig) Description() string                   { return m.id }
func (m *fakeMig) Version() int                          { return m.version }
func (m *fakeMig) Scope() migration.Scope                { return m.scope }
func (m *fakeMig) Applies(context.Context) (bool, error) { return true, nil }
func (m *fakeMig) Reversible() bool                      { return m.reversible }
func (m *fakeMig) ReadableFrom() int                     { return m.readableFrom }
func (m *fakeMig) Execute(context.Context) error {
	m.execCount++
	if m.crashOn == m.execCount {
		panic("gatekit-automa test: simulated crash in " + m.id)
	}
	return m.execErr
}
func (m *fakeMig) Rollback(context.Context) error {
	m.rollCount++
	if m.rollCrashOn == m.rollCount {
		panic("gatekit-automa test: simulated crash rolling back " + m.id)
	}
	return m.rollErr
}

type memStore struct{ v int }

func (s *memStore) Current() int          { return s.v }
func (s *memStore) SetActive(v int) error { s.v = v; return nil }

func plan(t *testing.T, from, to int, migs ...migration.Migration) migration.Plan {
	t.Helper()
	p, err := migration.NewPlanner(migs...).Plan(context.Background(), from, to)
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}
	return p
}

func TestRunNowExecutesLocal(t *testing.T) {
	a := &fakeMig{id: "a", version: 5, scope: migration.Local}
	b := &fakeMig{id: "b", version: 6, scope: migration.Shared}
	if err := NewRunner().RunNow(context.Background(), plan(t, 4, 6, a, b)); err != nil {
		t.Fatalf("RunNow() error: %v", err)
	}
	if a.execCount != 1 {
		t.Errorf("local a executed %d times, want 1", a.execCount)
	}
	if b.execCount != 0 {
		t.Errorf("shared b must be run-at-finalize, not run now (executed %d times)", b.execCount)
	}
}

func TestRunNowCompensatesOnFailure(t *testing.T) {
	a := &fakeMig{id: "a", version: 5, scope: migration.Local}
	b := &fakeMig{id: "b", version: 5, scope: migration.Local, execErr: errors.New("boom")}
	err := NewRunner().RunNow(context.Background(), plan(t, 4, 5, a, b))
	if err == nil {
		t.Fatalf("RunNow() = nil, want failure")
	}
	if a.rollCount != 1 {
		t.Errorf("a should be compensated after b failed (rollCount=%d)", a.rollCount)
	}
}

func TestRunNowEmptyIsNoop(t *testing.T) {
	b := &fakeMig{id: "b", version: 6, scope: migration.Shared}
	if err := NewRunner().RunNow(context.Background(), plan(t, 4, 6, b)); err != nil {
		t.Errorf("RunNow() with no local steps = %v, want nil", err)
	}
}

func TestFinalizeRunsHeldAndAdvances(t *testing.T) {
	b := &fakeMig{id: "b", version: 6, scope: migration.Shared}
	store := &memStore{v: 5}
	if err := NewRunner().Finalize(context.Background(), plan(t, 5, 6, b), store); err != nil {
		t.Fatalf("Finalize() error: %v", err)
	}
	if b.execCount != 1 {
		t.Errorf("shared b should run at finalize (executed %d times)", b.execCount)
	}
	if store.Current() != 6 {
		t.Errorf("store = %d, want 6 after finalize", store.Current())
	}
}

func TestFinalizeFailureDoesNotAdvance(t *testing.T) {
	a := &fakeMig{id: "a", version: 6, scope: migration.Shared}
	b := &fakeMig{id: "b", version: 6, scope: migration.Shared, execErr: errors.New("boom")}
	store := &memStore{v: 5}
	err := NewRunner().Finalize(context.Background(), plan(t, 5, 6, a, b), store)
	if err == nil {
		t.Fatalf("Finalize() = nil, want failure")
	}
	if store.Current() != 5 {
		t.Errorf("store = %d, want 5 (unchanged) after failed finalize", store.Current())
	}
	if a.rollCount != 1 {
		t.Errorf("a should be compensated after b failed (rollCount=%d)", a.rollCount)
	}
}

// TestFinalizeResumesAfterCrash simulates a crash: on the first Finalize, run-at-finalize step
// "b" panics after "a" has committed (automa write-aheads "a" as completed). A
// second Finalize with the same plan and journal resumes: "a" is skipped (already
// done), "b" re-runs and succeeds, the remaining steps run, and the active version
// advances.
func TestFinalizeResumesAfterCrash(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "finalize.journal")
	a := &fakeMig{id: "a", version: 6, scope: migration.Shared}
	b := &fakeMig{id: "b", version: 7, scope: migration.Shared, crashOn: 1} // crash on first run
	c := &fakeMig{id: "c", version: 8, scope: migration.Shared}
	store := &memStore{v: 5}
	runner := NewRunner(WithJournal(journal))

	// Attempt 1: b panics (the "crash"). Recover it like a process boundary would.
	func() {
		defer func() { _ = recover() }()
		_ = runner.Finalize(context.Background(), plan(t, 5, 8, a, b, c), store)
	}()
	if store.Current() != 5 {
		t.Fatalf("store advanced to %d before the crash completed finalize; want 5", store.Current())
	}

	// Attempt 2: resume from the journal. b no longer crashes (execCount now 2).
	if err := runner.Finalize(context.Background(), plan(t, 5, 8, a, b, c), store); err != nil {
		t.Fatalf("resumed Finalize() error: %v", err)
	}
	if store.Current() != 8 {
		t.Errorf("store = %d, want 8 after resumed finalize", store.Current())
	}
	if a.execCount != 1 {
		t.Errorf("a executed %d times; want 1 (resume must skip the completed step)", a.execCount)
	}
	if c.execCount != 1 {
		t.Errorf("c executed %d times; want 1 (should run on resume)", c.execCount)
	}
	if b.execCount < 2 {
		t.Errorf("b executed %d times; want >=2 (crashed, then re-run on resume)", b.execCount)
	}
}

func lowerPlan(t *testing.T, from, to, min int, migs ...migration.Migration) migration.LowerPlan {
	t.Helper()
	lp, err := migration.NewPlanner(migs...).PlanLower(from, to, min)
	if err != nil {
		t.Fatalf("PlanLower() error: %v", err)
	}
	return lp
}

func TestLowerRollsBackAndLowersActive(t *testing.T) {
	a := &fakeMig{id: "a", version: 6, scope: migration.Shared, reversible: true}
	store := &memStore{v: 6}
	if err := NewRunner().Lower(context.Background(), lowerPlan(t, 6, 5, 0, a), store); err != nil {
		t.Fatalf("Lower() error: %v", err)
	}
	if a.rollCount != 1 {
		t.Errorf("a rolled back %d times, want 1", a.rollCount)
	}
	if store.Current() != 5 {
		t.Errorf("store = %d, want 5 after lower", store.Current())
	}
}

func TestLowerFailureLeavesActiveUnchangedAndDoesNotCompensate(t *testing.T) {
	// Plan order is newest-first: newer rolls back, then older's rollback fails. StopOnError
	// halts without re-applying newer (no compensation) and the advance step never runs.
	newer := &fakeMig{id: "newer", version: 7, scope: migration.Shared, reversible: true}
	older := &fakeMig{id: "older", version: 6, scope: migration.Shared, reversible: true, rollErr: errors.New("boom")}
	store := &memStore{v: 7}
	err := NewRunner().Lower(context.Background(), lowerPlan(t, 7, 5, 0, newer, older), store)
	if err == nil {
		t.Fatal("Lower() = nil, want failure")
	}
	if newer.rollCount != 1 {
		t.Errorf("newer rolled back %d times, want 1", newer.rollCount)
	}
	if newer.execCount != 0 {
		t.Errorf("newer re-executed %d times; StopOnError must not compensate a completed rollback", newer.execCount)
	}
	if store.Current() != 7 {
		t.Errorf("store = %d, want 7 (unchanged) after failed lower", store.Current())
	}
}

// TestLowerResumesAfterCrash mirrors the finalize resume test for the reverse direction:
// "older" panics mid-rollback after "newer" has rolled back; a second Lower with the same
// plan and journal resumes, skips the completed rollback, finishes, and lowers active.
func TestLowerResumesAfterCrash(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "lower.journal")
	newer := &fakeMig{id: "newer", version: 7, scope: migration.Shared, reversible: true}
	older := &fakeMig{id: "older", version: 6, scope: migration.Shared, reversible: true, rollCrashOn: 1}
	store := &memStore{v: 7}
	runner := NewRunner(WithJournal(journal))

	func() {
		defer func() { _ = recover() }()
		_ = runner.Lower(context.Background(), lowerPlan(t, 7, 5, 0, newer, older), store)
	}()
	if store.Current() != 7 {
		t.Fatalf("store lowered to %d before the crash completed lower; want 7", store.Current())
	}

	if err := runner.Lower(context.Background(), lowerPlan(t, 7, 5, 0, newer, older), store); err != nil {
		t.Fatalf("resumed Lower() error: %v", err)
	}
	if store.Current() != 5 {
		t.Errorf("store = %d, want 5 after resumed lower", store.Current())
	}
	if newer.rollCount != 1 {
		t.Errorf("newer rolled back %d times; want 1 (resume must skip the completed step)", newer.rollCount)
	}
	if older.rollCount < 2 {
		t.Errorf("older rolled back %d times; want >=2 (crashed, then re-run on resume)", older.rollCount)
	}
}

func TestRunNowRefusesWriteFloorBlocked(t *testing.T) {
	a := &fakeMig{id: "a", version: 6, scope: migration.Local, readableFrom: 6}
	p, err := migration.NewPlanner(a).WithMin(5).Plan(context.Background(), 5, 6)
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}
	err = NewRunner().RunNow(context.Background(), p)
	var wf *migration.WriteFloorError
	if !errors.As(err, &wf) {
		t.Fatalf("RunNow() = %v, want *WriteFloorError", err)
	}
	if a.execCount != 0 {
		t.Errorf("blocked step executed %d times; RunNow must refuse before executing", a.execCount)
	}
}

func TestFinalizeRefusesWriteFloorBlocked(t *testing.T) {
	a := &fakeMig{id: "a", version: 6, scope: migration.Shared, readableFrom: 6}
	store := &memStore{v: 5}
	p, err := migration.NewPlanner(a).WithMin(5).Plan(context.Background(), 5, 6)
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}
	err = NewRunner().Finalize(context.Background(), p, store)
	var wf *migration.WriteFloorError
	if !errors.As(err, &wf) {
		t.Fatalf("Finalize() = %v, want *WriteFloorError", err)
	}
	if a.execCount != 0 || store.Current() != 5 {
		t.Errorf("blocked finalize ran (execCount=%d, store=%d); must refuse before executing", a.execCount, store.Current())
	}
}
