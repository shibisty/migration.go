package migration

import (
	"context"
	"sync"
)

// MemoryStore is a Store that keeps the journal in memory. It is suitable for testing
// your own migrations and for backends without a persistent journal.
// If fn in Step returns an error, journal changes made in that step are reverted;
// changes fn made to db are the responsibility of db itself.
type MemoryStore[DB any] struct {
	db      DB
	mu      sync.Mutex    // journal
	lock    chan struct{} // Locker: buffer of 1 means busy/free
	records []Record
}

// NewMemoryStore creates a MemoryStore that passes db to every migration.
func NewMemoryStore[DB any](db DB) *MemoryStore[DB] {
	return &MemoryStore[DB]{db: db, lock: make(chan struct{}, 1)}
}

// Prepare does nothing: the in-memory journal always exists.
func (s *MemoryStore[DB]) Prepare(context.Context) error { return nil }

// Applied returns a copy of the journal.
func (s *MemoryStore[DB]) Applied(context.Context) ([]Record, error) {
	return s.Records(), nil
}

// Records returns a copy of the journal in order of application.
func (s *MemoryStore[DB]) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

// Step runs fn; on error the journal is restored to its state before the step.
func (s *MemoryStore[DB]) Step(ctx context.Context, fn func(db DB, journal Journal) error) error {
	j := &memoryJournal{base: s.Records()}
	if err := fn(s.db, j); err != nil {
		return err
	}
	s.mu.Lock()
	s.records = j.base
	s.mu.Unlock()
	return nil
}

// Lock implements Locker: only one Migrate/Rollback runs at a time.
// Waiting is interrupted by cancelling ctx.
func (s *MemoryStore[DB]) Lock(ctx context.Context) (func(context.Context) error, error) {
	select {
	case s.lock <- struct{}{}:
		return func(context.Context) error { <-s.lock; return nil }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type memoryJournal struct{ base []Record }

func (j *memoryJournal) Add(_ context.Context, name string, batch int) error {
	j.base = append(j.base, Record{Name: name, Batch: batch})
	return nil
}

func (j *memoryJournal) Remove(_ context.Context, name string) error {
	out := j.base[:0:0]
	for _, r := range j.base {
		if r.Name != name {
			out = append(out, r)
		}
	}
	j.base = out
	return nil
}

var (
	_ Store[int] = (*MemoryStore[int])(nil)
	_ Locker     = (*MemoryStore[int])(nil)
)
