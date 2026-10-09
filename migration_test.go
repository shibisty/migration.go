package migration_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"migration"
)

var errBoom = errors.New("boom")

// db is what migrations receive in tests: a log of Up/Down calls.
type db struct {
	mu  sync.Mutex
	log []string
}

func (d *db) add(s string) { d.mu.Lock(); d.log = append(d.log, s); d.mu.Unlock() }

func (d *db) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}

func mig(name string) migration.Migration[*db] {
	return migration.Migration[*db]{
		Name: name,
		Up:   func(_ context.Context, d *db) error { d.add("up:" + name); return nil },
		Down: func(_ context.Context, d *db) error { d.add("down:" + name); return nil },
	}
}

func newMigrator(t *testing.T, store migration.Store[*db], migs ...migration.Migration[*db]) *migration.Migrator[*db] {
	t.Helper()
	m, err := migration.New(store, migs...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", what, got, want)
	}
}

var ctx = context.Background()

func TestName(t *testing.T) {
	got := migration.Name(time.Date(2024, 3, 5, 7, 8, 9, 0, time.UTC), "create_users_table")
	eq(t, "Name", got, "2024_03_05_070809_create_users_table")
}

func TestNewValidates(t *testing.T) {
	d := &db{}
	store := migration.NewMemoryStore(d)
	cases := []struct {
		name string
		migs []migration.Migration[*db]
		want error
	}{
		{"empty name", []migration.Migration[*db]{{Up: mig("a").Up}}, migration.ErrInvalid},
		{"no Up", []migration.Migration[*db]{{Name: "a"}}, migration.ErrInvalid},
		{"duplicate", []migration.Migration[*db]{mig("a"), mig("b"), mig("a")}, migration.ErrDuplicateName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := migration.New(store, tc.migs...); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := migration.New[*db](nil); !errors.Is(err, migration.ErrInvalid) {
		t.Fatalf("nil store: got %v", err)
	}
	if _, err := migration.New(store); err != nil {
		t.Fatalf("no migrations is valid, got %v", err)
	}
}

func TestMigrateAppliesInOrderAsOneBatch(t *testing.T) {
	d := &db{}
	store := migration.NewMemoryStore(d)
	m := newMigrator(t, store, mig("a"), mig("b"), mig("c"))

	ran, err := m.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "ran", ran, []string{"a", "b", "c"})
	eq(t, "calls", d.calls(), []string{"up:a", "up:b", "up:c"})
	eq(t, "journal", store.Records(), []migration.Record{{Name: "a", Batch: 1}, {Name: "b", Batch: 1}, {Name: "c", Batch: 1}})

	ran, err = m.Migrate(ctx)
	if err != nil || ran != nil {
		t.Fatalf("second Migrate should do nothing, got %v, %v", ran, err)
	}
}

func TestMigrateNewBatchForNewMigrations(t *testing.T) {
	d := &db{}
	store := migration.NewMemoryStore(d)
	if _, err := newMigrator(t, store, mig("a")).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ran, err := newMigrator(t, store, mig("a"), mig("b"), mig("c")).Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "ran", ran, []string{"b", "c"})
	eq(t, "journal", store.Records(), []migration.Record{{Name: "a", Batch: 1}, {Name: "b", Batch: 2}, {Name: "c", Batch: 2}})
}

func TestMigrateStopsOnErrorAndKeepsApplied(t *testing.T) {
	d := &db{}
	store := migration.NewMemoryStore(d)
	bad := migration.Migration[*db]{Name: "b", Up: func(context.Context, *db) error { return errBoom }}
	m := newMigrator(t, store, mig("a"), bad, mig("c"))

	ran, err := m.Migrate(ctx)
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "migration b: up") {
		t.Fatalf("want wrapped boom naming the migration, got %v", err)
	}
	eq(t, "ran", ran, []string{"a"})
	eq(t, "journal", store.Records(), []migration.Record{{Name: "a", Batch: 1}})
	eq(t, "calls", d.calls(), []string{"up:a"})
}

// A journal write error rolls back the step: the migration is not considered applied.
func TestStepErrorFromJournal(t *testing.T) {
	store := &failingStore{MemoryStore: migration.NewMemoryStore(&db{}), failAdd: true}
	ran, err := newMigrator(t, store, mig("a")).Migrate(ctx)
	if !errors.Is(err, errBoom) || ran != nil {
		t.Fatalf("got %v, %v", ran, err)
	}
	eq(t, "journal", store.Records(), []migration.Record(nil))
}

func TestRollbackLastBatchInReverseOrder(t *testing.T) {
	d := &db{}
	store := migration.NewMemoryStore(d)
	newMigrator(t, store, mig("a")).Migrate(ctx)
	m := newMigrator(t, store, mig("a"), mig("b"), mig("c"))
	m.Migrate(ctx)

	got, err := m.Rollback(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "rolled back", got, []string{"c", "b"})
	eq(t, "journal", store.Records(), []migration.Record{{Name: "a", Batch: 1}})
	eq(t, "last calls", d.calls()[3:], []string{"down:c", "down:b"})
}

func TestRollbackSeveralBatchesAndReset(t *testing.T) {
	store := migration.NewMemoryStore(&db{})
	all := []migration.Migration[*db]{mig("a"), mig("b"), mig("c")}
	for i := 1; i <= 3; i++ {
		newMigrator(t, store, all[:i]...).Migrate(ctx)
	}
	m := newMigrator(t, store, all...)

	got, err := m.Rollback(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "rollback 2", got, []string{"c", "b"})

	got, err = m.Rollback(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "rollback more than exists", got, []string{"a"})

	got, err = m.Rollback(ctx, 1)
	if err != nil || got != nil {
		t.Fatalf("rollback with empty journal: %v, %v", got, err)
	}

	m.Migrate(ctx)
	got, err = m.Reset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "reset", got, []string{"c", "b", "a"})
	eq(t, "journal", store.Records(), []migration.Record(nil))
}

func TestRefresh(t *testing.T) {
	d := &db{}
	store := migration.NewMemoryStore(d)
	m := newMigrator(t, store, mig("a"), mig("b"))
	m.Migrate(ctx)

	back, ran, err := m.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "rolled back", back, []string{"b", "a"})
	eq(t, "ran", ran, []string{"a", "b"})
	eq(t, "journal", store.Records(), []migration.Record{{Name: "a", Batch: 1}, {Name: "b", Batch: 1}})
}

func TestRefreshStopsOnResetError(t *testing.T) {
	store := migration.NewMemoryStore(&db{})
	noDown := migration.Migration[*db]{Name: "a", Up: mig("a").Up}
	m := newMigrator(t, store, noDown)
	m.Migrate(ctx)
	if _, ran, err := m.Refresh(ctx); !errors.Is(err, migration.ErrNoDown) || ran != nil {
		t.Fatalf("got %v, %v", ran, err)
	}
}

// Rollback validates all migrations first and rolls back nothing if any of them can't be rolled back.
func TestRollbackChecksBeforeRunning(t *testing.T) {
	t.Run("no Down", func(t *testing.T) {
		d := &db{}
		store := migration.NewMemoryStore(d)
		noDown := migration.Migration[*db]{Name: "b", Up: mig("b").Up}
		m := newMigrator(t, store, mig("a"), noDown, mig("c"))
		m.Migrate(ctx)

		_, err := m.Rollback(ctx, 1)
		if !errors.Is(err, migration.ErrNoDown) || !strings.Contains(err.Error(), "b") {
			t.Fatalf("got %v", err)
		}
		eq(t, "journal untouched", len(store.Records()), 3)
		for _, c := range d.calls() {
			if strings.HasPrefix(c, "down:") {
				t.Fatalf("nothing should be rolled back, got %v", d.calls())
			}
		}
	})

	t.Run("unknown applied migration", func(t *testing.T) {
		store := migration.NewMemoryStore(&db{})
		newMigrator(t, store, mig("a"), mig("gone")).Migrate(ctx)
		_, err := newMigrator(t, store, mig("a")).Rollback(ctx, 1)
		if !errors.Is(err, migration.ErrUnknown) || !strings.Contains(err.Error(), "gone") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRollbackDownError(t *testing.T) {
	store := migration.NewMemoryStore(&db{})
	failing := migration.Migration[*db]{
		Name: "b",
		Up:   mig("b").Up,
		Down: func(context.Context, *db) error { return errBoom },
	}
	m := newMigrator(t, store, mig("a"), failing, mig("c"))
	m.Migrate(ctx)

	got, err := m.Rollback(ctx, 1)
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "migration b: down") {
		t.Fatalf("got %v", err)
	}
	eq(t, "rolled back before failure", got, []string{"c"})
	eq(t, "journal", store.Records(), []migration.Record{{Name: "a", Batch: 1}, {Name: "b", Batch: 1}})
}

func TestStatusAndPending(t *testing.T) {
	store := migration.NewMemoryStore(&db{})
	newMigrator(t, store, mig("a"), mig("gone")).Migrate(ctx)
	m := newMigrator(t, store, mig("a"), mig("b"))

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "status", st, []migration.Status{
		{Name: "a", Applied: true, Batch: 1},
		{Name: "b"},
		{Name: "gone", Applied: true, Batch: 1, Missing: true},
	})

	p, err := m.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "pending", p, []string{"b"})
}

func TestStoreErrorsArePropagated(t *testing.T) {
	cases := []struct {
		name  string
		store *failingStore
	}{
		{"prepare", &failingStore{failPrepare: true}},
		{"applied", &failingStore{failApplied: true}},
		{"lock", &failingStore{failLock: true}},
		{"unlock", &failingStore{failUnlock: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.store.MemoryStore = migration.NewMemoryStore(&db{})
			m := newMigrator(t, tc.store, mig("a"))
			if _, err := m.Migrate(ctx); !errors.Is(err, errBoom) {
				t.Errorf("Migrate: got %v", err)
			}
			if _, err := m.Rollback(ctx, 1); !errors.Is(err, errBoom) {
				t.Errorf("Rollback: got %v", err)
			}
			if tc.name == "prepare" || tc.name == "applied" {
				if _, err := m.Status(ctx); !errors.Is(err, errBoom) {
					t.Errorf("Status: got %v", err)
				}
				if _, err := m.Pending(ctx); !errors.Is(err, errBoom) {
					t.Errorf("Pending: got %v", err)
				}
			}
		})
	}
}

// A Store without Locker works (locking is optional).
func TestStoreWithoutLocker(t *testing.T) {
	store := noLockStore{migration.NewMemoryStore(&db{})}
	ran, err := newMigrator(t, migration.Store[*db](store), mig("a")).Migrate(ctx)
	if err != nil || len(ran) != 1 {
		t.Fatalf("got %v, %v", ran, err)
	}
}

// Concurrent Migrate calls on one Store with Locker apply each migration once.
func TestConcurrentMigrateWithLocker(t *testing.T) {
	d := &db{}
	store := migration.NewMemoryStore(d)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, _ := migration.New(store, mig("a"), mig("b"))
			if _, err := m.Migrate(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	eq(t, "calls", d.calls(), []string{"up:a", "up:b"})
}

// ---- helper Stores ----

type failingStore struct {
	*migration.MemoryStore[*db]
	failPrepare, failApplied, failAdd, failLock, failUnlock bool
}

func (s *failingStore) Prepare(ctx context.Context) error {
	if s.failPrepare {
		return errBoom
	}
	return s.MemoryStore.Prepare(ctx)
}

func (s *failingStore) Applied(ctx context.Context) ([]migration.Record, error) {
	if s.failApplied {
		return nil, errBoom
	}
	return s.MemoryStore.Applied(ctx)
}

func (s *failingStore) Step(ctx context.Context, fn func(*db, migration.Journal) error) error {
	return s.MemoryStore.Step(ctx, func(d *db, j migration.Journal) error {
		if s.failAdd {
			return fn(d, failingJournal{j})
		}
		return fn(d, j)
	})
}

func (s *failingStore) Lock(ctx context.Context) (func(context.Context) error, error) {
	if s.failLock {
		return nil, errBoom
	}
	if s.failUnlock {
		return func(context.Context) error { return errBoom }, nil
	}
	return s.MemoryStore.Lock(ctx)
}

type failingJournal struct{ migration.Journal }

func (failingJournal) Add(context.Context, string, int) error { return errBoom }

// noLockStore hides Lock from MemoryStore.
type noLockStore struct{ s *migration.MemoryStore[*db] }

func (n noLockStore) Prepare(ctx context.Context) error { return n.s.Prepare(ctx) }
func (n noLockStore) Applied(ctx context.Context) ([]migration.Record, error) {
	return n.s.Applied(ctx)
}
func (n noLockStore) Step(ctx context.Context, fn func(*db, migration.Journal) error) error {
	return n.s.Step(ctx, fn)
}

// shuffledStore returns the journal in reverse order; Rollback must not rely on the order.
type shuffledStore struct{ *migration.MemoryStore[*db] }

func (s shuffledStore) Applied(ctx context.Context) ([]migration.Record, error) {
	recs, err := s.MemoryStore.Applied(ctx)
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	return recs, err
}

func TestRollbackOrderDoesNotDependOnStoreOrder(t *testing.T) {
	d := &db{}
	inner := migration.NewMemoryStore(d)
	store := shuffledStore{inner}
	newMigrator(t, store, mig("a"), mig("b")).Migrate(ctx)
	m := newMigrator(t, store, mig("a"), mig("b"), mig("c"), mig("d"))
	m.Migrate(ctx)

	got, err := m.Reset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "reset order", got, []string{"d", "c", "b", "a"})
}

func TestLockRespectsContext(t *testing.T) {
	store := migration.NewMemoryStore(&db{})
	unlock, err := store.Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Lock(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Lock on a held lock with cancelled ctx: got %v", err)
	}
	m := newMigrator(t, store, mig("a"))
	if _, err := m.Migrate(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Migrate should fail to lock: got %v", err)
	}
	if err := unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(ctx); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

// Unlock receives a context without cancellation, even if the caller's ctx is cancelled mid-run.
func TestUnlockGetsUncancelledContext(t *testing.T) {
	cctx, cancel := context.WithCancel(ctx)
	store := &ctxLockStore{MemoryStore: migration.NewMemoryStore(&db{})}
	cancelling := migration.Migration[*db]{Name: "a", Up: func(context.Context, *db) error { cancel(); return nil }}
	m := newMigrator(t, store, cancelling)
	if _, err := m.Migrate(cctx); err != nil {
		t.Fatal(err)
	}
	if store.unlockErr != nil {
		t.Fatalf("unlock got a cancelled context: %v", store.unlockErr)
	}
}

type ctxLockStore struct {
	*migration.MemoryStore[*db]
	unlockErr error
}

func (s *ctxLockStore) Lock(context.Context) (func(context.Context) error, error) {
	return func(ctx context.Context) error { s.unlockErr = ctx.Err(); return nil }, nil
}
