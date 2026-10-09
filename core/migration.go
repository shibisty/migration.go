// Package migration is a migration runner that is not tied to any particular database.
//
// The package only knows about migration order, batches and rollback. Where the journal
// of applied migrations is stored, and what a migration operates on, is decided by a Store,
// a separate implementation for a specific backend:
//
//   - migration-orm: SQL databases via orm and its driver (mysql, postgres, sqlite);
//   - MemoryStore from this package: for tests;
//   - any custom implementation (MongoDB, Redis, files, an external API).
//
// The DB type is what each migration receives in Up/Down: a transaction and
// a schema builder for SQL, a MongoDB client for a document database, and so on.
// This way the same batch logic works for any storage:
//
//	m, err := migration.New(store,
//		migration.Migration[*ormstore.DB]{
//			Name: "2024_01_01_000000_create_users_table",
//			Up:   func(ctx context.Context, db *ormstore.DB) error { ... },
//			Down: func(ctx context.Context, db *ormstore.DB) error { ... },
//		},
//	)
//	ran, err := m.Migrate(ctx)
//
// As in Laravel: Migrate applies all new migrations as a single batch,
// Rollback rolls back the latest batches, and Status reports the state.
package migration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Migration is a single migration. Name is written to the journal and must be
// unique; by convention it starts with a date so that file order
// matches application order (see Name).
type Migration[DB any] struct {
	Name string
	Up   func(ctx context.Context, db DB) error
	// Down is optional: without it the migration cannot be rolled back.
	Down func(ctx context.Context, db DB) error
}

// Name builds a Laravel-style name: "2006_01_02_150405_description".
func Name(t time.Time, description string) string {
	return t.Format("2006_01_02_150405") + "_" + description
}

// Record is a journal entry for an applied migration.
type Record struct {
	Name  string
	Batch int
}

// Journal is used to write to the journal within a step. Journal changes and the
// migration's actions run in the same Store.Step, so if the migration
// fails, no journal entry is written.
type Journal interface {
	Add(ctx context.Context, name string, batch int) error
	Remove(ctx context.Context, name string) error
}

// Store is a migration backend: it stores the journal and executes steps.
type Store[DB any] interface {
	// Prepare creates the journal storage if it does not exist yet. It is called before
	// every operation and must be idempotent.
	Prepare(ctx context.Context) error
	// Applied returns the applied migrations in order of application.
	Applied(ctx context.Context) ([]Record, error)
	// Step runs fn as a single unit: in a transaction, if the backend supports
	// them. If fn returns an error, the changes made by fn and to the journal are reverted
	// (as far as the backend allows; for example, MySQL does not roll back DDL).
	Step(ctx context.Context, fn func(db DB, journal Journal) error) error
}

// Locker is an optional extension of Store. If a Store implements it,
// Migrator holds the lock for the duration of Migrate/Rollback so that two instances
// of the application do not apply migrations concurrently.
type Locker interface {
	Lock(ctx context.Context) (unlock func(context.Context) error, err error)
}

var (
	// ErrDuplicateName means two migrations share the same name.
	ErrDuplicateName = errors.New("migration: duplicate migration name")
	// ErrInvalid means a migration has no name or no Up.
	ErrInvalid = errors.New("migration: invalid migration")
	// ErrUnknown means a migration is in the journal but is not registered.
	ErrUnknown = errors.New("migration: applied migration is not registered")
	// ErrNoDown means a migration has no Down and cannot be rolled back.
	ErrNoDown = errors.New("migration: migration has no Down")
)

// Status is the state of a single migration.
type Status struct {
	Name    string
	Applied bool
	Batch   int // 0 if not applied
	// Missing means the migration is in the journal but not registered in the Migrator
	// (for example, the migration file was deleted).
	Missing bool
}

// Migrator applies and rolls back migrations through a Store.
type Migrator[DB any] struct {
	store      Store[DB]
	migrations []Migration[DB]
	byName     map[string]Migration[DB]
	index      map[string]int
}

// New creates a Migrator. Migrations are applied in the given order;
// sort them by name if names start with a date.
// It returns an error if a name is empty or duplicated, or if Up is missing.
func New[DB any](store Store[DB], migrations ...Migration[DB]) (*Migrator[DB], error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is nil", ErrInvalid)
	}
	byName := make(map[string]Migration[DB], len(migrations))
	index := make(map[string]int, len(migrations))
	for i, mig := range migrations {
		if mig.Name == "" {
			return nil, fmt.Errorf("%w: migration #%d has no name", ErrInvalid, i)
		}
		if mig.Up == nil {
			return nil, fmt.Errorf("%w: %s has no Up", ErrInvalid, mig.Name)
		}
		if _, dup := byName[mig.Name]; dup {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateName, mig.Name)
		}
		byName[mig.Name] = mig
		index[mig.Name] = i
	}
	return &Migrator[DB]{store: store, migrations: migrations, byName: byName, index: index}, nil
}

// Migrate applies all pending migrations as one new batch.
// Each migration is a separate Store.Step. It stops at the first
// error; migrations already applied in this run stay applied.
// It returns the names of the applied migrations.
func (m *Migrator[DB]) Migrate(ctx context.Context) (ran []string, err error) {
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unlock(context.WithoutCancel(ctx))) }()

	applied, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	done := make(map[string]bool, len(applied))
	batch := 0
	for _, r := range applied {
		done[r.Name] = true
		if r.Batch > batch {
			batch = r.Batch
		}
	}
	batch++

	for _, mig := range m.migrations {
		if done[mig.Name] {
			continue
		}
		err := m.store.Step(ctx, func(db DB, j Journal) error {
			if err := mig.Up(ctx, db); err != nil {
				return err
			}
			return j.Add(ctx, mig.Name, batch)
		})
		if err != nil {
			return ran, fmt.Errorf("migration %s: up: %w", mig.Name, err)
		}
		ran = append(ran, mig.Name)
	}
	return ran, nil
}

// Rollback rolls back the last steps batches (steps <= 0 means one batch),
// calling Down in reverse order of application. It returns the names of the rolled-back migrations.
func (m *Migrator[DB]) Rollback(ctx context.Context, steps int) ([]string, error) {
	if steps <= 0 {
		steps = 1
	}
	return m.rollback(ctx, func(applied []Record) int {
		maxBatch := 0
		for _, r := range applied {
			if r.Batch > maxBatch {
				maxBatch = r.Batch
			}
		}
		return maxBatch - steps + 1
	})
}

// Reset rolls back all applied migrations.
func (m *Migrator[DB]) Reset(ctx context.Context) ([]string, error) {
	return m.rollback(ctx, func([]Record) int { return 0 })
}

// Refresh = Reset + Migrate: rebuilds the schema from scratch.
func (m *Migrator[DB]) Refresh(ctx context.Context) (rolledBack, ran []string, err error) {
	rolledBack, err = m.Reset(ctx)
	if err != nil {
		return rolledBack, nil, err
	}
	ran, err = m.Migrate(ctx)
	return rolledBack, ran, err
}

// rollback rolls back migrations from batches >= minBatch(applied).
func (m *Migrator[DB]) rollback(ctx context.Context, minBatch func([]Record) int) (rolledBack []string, err error) {
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	// The lock is released even if ctx is already cancelled (often that is exactly why everything failed).
	defer func() { err = errors.Join(err, unlock(context.WithoutCancel(ctx))) }()

	applied, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	from := minBatch(applied)

	// Validate everything first, so we do not roll back half and then fail midway
	// because of a missing Down or an unregistered migration.
	type item struct {
		mig   Migration[DB]
		batch int
		index int // registration position = application order within a batch
	}
	var todo []item
	for _, r := range applied {
		if r.Batch < from {
			continue
		}
		mig, ok := m.byName[r.Name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknown, r.Name)
		}
		if mig.Down == nil {
			return nil, fmt.Errorf("%w: %s", ErrNoDown, r.Name)
		}
		todo = append(todo, item{mig: mig, batch: r.Batch, index: m.index[r.Name]})
	}
	// Roll back strictly in reverse order of application: the highest batch first, and within
	// a batch, starting from the last registered. Do not rely on the order
	// in which the Store returned the journal.
	sort.SliceStable(todo, func(i, j int) bool {
		if todo[i].batch != todo[j].batch {
			return todo[i].batch > todo[j].batch
		}
		return todo[i].index > todo[j].index
	})

	for _, it := range todo {
		mig := it.mig
		err := m.store.Step(ctx, func(db DB, j Journal) error {
			if err := mig.Down(ctx, db); err != nil {
				return err
			}
			return j.Remove(ctx, mig.Name)
		})
		if err != nil {
			return rolledBack, fmt.Errorf("migration %s: down: %w", mig.Name, err)
		}
		rolledBack = append(rolledBack, mig.Name)
	}
	return rolledBack, nil
}

// Status returns the state of all registered migrations in registration
// order, followed by migrations from the journal that are not in the Migrator
// (Missing = true).
func (m *Migrator[DB]) Status(ctx context.Context) ([]Status, error) {
	applied, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	batchOf := make(map[string]int, len(applied))
	for _, r := range applied {
		batchOf[r.Name] = r.Batch
	}

	out := make([]Status, 0, len(m.migrations))
	for _, mig := range m.migrations {
		b, ok := batchOf[mig.Name]
		out = append(out, Status{Name: mig.Name, Applied: ok, Batch: b})
	}
	for _, r := range applied {
		if _, ok := m.byName[r.Name]; !ok {
			out = append(out, Status{Name: r.Name, Applied: true, Batch: r.Batch, Missing: true})
		}
	}
	return out, nil
}

// Pending returns the names of migrations that have not been applied yet.
func (m *Migrator[DB]) Pending(ctx context.Context) ([]string, error) {
	st, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range st {
		if !s.Applied {
			out = append(out, s.Name)
		}
	}
	return out, nil
}

func (m *Migrator[DB]) applied(ctx context.Context) ([]Record, error) {
	if err := m.store.Prepare(ctx); err != nil {
		return nil, fmt.Errorf("migration: prepare store: %w", err)
	}
	recs, err := m.store.Applied(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration: read journal: %w", err)
	}
	return recs, nil
}

func (m *Migrator[DB]) lock(ctx context.Context) (func(context.Context) error, error) {
	l, ok := any(m.store).(Locker)
	if !ok {
		return func(context.Context) error { return nil }, nil
	}
	unlock, err := l.Lock(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration: lock: %w", err)
	}
	return unlock, nil
}
