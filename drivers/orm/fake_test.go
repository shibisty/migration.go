package ormstore_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"orm"
	"orm/schema"
)

// fakeDB is an in-memory SQLExecutor that understands exactly the SQL written
// by ormstore (the journal), plus any DDL/other Exec calls, which are just recorded.
// Transactions are real: changes become visible on Commit and are discarded on Rollback.
type fakeDB struct {
	mu      sync.Mutex
	tables  map[string]bool
	journal []row
	nextID  int
	exec    []string // all executed (committed) Exec calls, in order
	failOn  string   // SQL substring on which Exec returns an error
	failQ   bool     // QueryContext returns an error
	failTx  string   // "begin" | "commit" | "rollback"
}

type row struct {
	id    int
	name  string
	batch int
}

func newFakeDB() *fakeDB { return &fakeDB{tables: map[string]bool{}} }

var errFake = errors.New("fake failure")

func (f *fakeDB) Driver() string                                          { return "fake" }
func (f *fakeDB) Ping(context.Context) error                              { return nil }
func (f *fakeDB) Close() error                                            { return nil }
func (f *fakeDB) Dialect() orm.Dialect                                    { return fakeDialect{} }
func (f *fakeDB) QueryRowContext(context.Context, string, ...any) orm.Row { return nil }

func (f *fakeDB) ExecContext(ctx context.Context, q string, args ...any) (orm.Result, error) {
	t := &fakeTx{db: f, autocommit: true}
	res, err := t.ExecContext(ctx, q, args...)
	if err != nil {
		return res, err
	}
	return res, t.Commit()
}

func (f *fakeDB) QueryContext(ctx context.Context, q string, args ...any) (orm.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queryLocked(q, f.journal)
}

var selectRe = regexp.MustCompile(`^SELECT "migration", "batch" FROM "(\w+)" ORDER BY "id"$`)

func (f *fakeDB) queryLocked(q string, journal []row) (orm.Rows, error) {
	if f.failQ {
		return nil, errFake
	}
	m := selectRe.FindStringSubmatch(q)
	if m == nil {
		return nil, fmt.Errorf("fakeDB: unexpected query %q", q)
	}
	if !f.tables[m[1]] {
		return nil, fmt.Errorf("fakeDB: no table %q", m[1])
	}
	return &fakeRows{data: append([]row(nil), journal...)}, nil
}

func (f *fakeDB) BeginTx(context.Context) (orm.Tx, error) {
	if f.failTx == "begin" {
		return nil, errFake
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fakeTx{db: f, journal: append([]row(nil), f.journal...), nextID: f.nextID, tables: copyTables(f.tables)}, nil
}

func (f *fakeDB) executed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.exec...)
}

func copyTables(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// fakeTx accumulates changes and applies them to fakeDB on Commit.
type fakeTx struct {
	db      *fakeDB
	journal []row
	nextID  int
	tables  map[string]bool
	exec    []string
	started bool
	// autocommit: Exec outside a transaction; Commit/Rollback failures do not apply to it.
	autocommit bool
}

var (
	insertRe = regexp.MustCompile(`^INSERT INTO "(\w+)" \("migration", "batch"\) VALUES \(\$1, \$2\)$`)
	deleteRe = regexp.MustCompile(`^DELETE FROM "(\w+)" WHERE "migration" = \$1$`)
	createRe = regexp.MustCompile(`^CREATE TABLE (?:IF NOT EXISTS )?"(\w+)"`)
)

func (t *fakeTx) init() {
	if t.started {
		return
	}
	t.started = true
	if t.tables == nil {
		t.db.mu.Lock()
		t.journal = append([]row(nil), t.db.journal...)
		t.nextID = t.db.nextID
		t.tables = copyTables(t.db.tables)
		t.db.mu.Unlock()
	}
}

func (t *fakeTx) Driver() string                                          { return "fake" }
func (t *fakeTx) Ping(context.Context) error                              { return nil }
func (t *fakeTx) Close() error                                            { return nil }
func (t *fakeTx) Dialect() orm.Dialect                                    { return fakeDialect{} }
func (t *fakeTx) QueryRowContext(context.Context, string, ...any) orm.Row { return nil }
func (t *fakeTx) BeginTx(context.Context) (orm.Tx, error) {
	return nil, errors.New("nested transactions are not supported")
}

func (t *fakeTx) ExecContext(_ context.Context, q string, args ...any) (orm.Result, error) {
	t.init()
	if t.db.failOn != "" && strings.Contains(q, t.db.failOn) {
		return orm.Result{}, errFake
	}
	t.exec = append(t.exec, q)
	switch {
	case createRe.MatchString(q):
		t.tables[createRe.FindStringSubmatch(q)[1]] = true
	case insertRe.MatchString(q):
		t.nextID++
		t.journal = append(t.journal, row{id: t.nextID, name: args[0].(string), batch: args[1].(int)})
		return orm.Result{LastInsertID: int64(t.nextID), RowsAffected: 1}, nil
	case deleteRe.MatchString(q):
		out := t.journal[:0:0]
		for _, r := range t.journal {
			if r.name != args[0].(string) {
				out = append(out, r)
			}
		}
		t.journal = out
	}
	return orm.Result{}, nil
}

func (t *fakeTx) QueryContext(_ context.Context, q string, _ ...any) (orm.Rows, error) {
	t.init()
	return t.db.queryLocked(q, t.journal)
}

func (t *fakeTx) Commit() error {
	if t.db.failTx == "commit" && !t.autocommit {
		return errFake
	}
	t.init()
	t.db.mu.Lock()
	defer t.db.mu.Unlock()
	t.db.journal, t.db.nextID, t.db.tables = t.journal, t.nextID, t.tables
	t.db.exec = append(t.db.exec, t.exec...)
	return nil
}

func (t *fakeTx) Rollback() error {
	if t.db.failTx == "rollback" {
		return errFake
	}
	return nil
}

type fakeRows struct {
	data []row
	pos  int
}

func (r *fakeRows) Next() bool { r.pos++; return r.pos <= len(r.data) }
func (r *fakeRows) Scan(dest ...any) error {
	d := r.data[r.pos-1]
	*dest[0].(*string) = d.name
	*dest[1].(*int) = d.batch
	return nil
}
func (r *fakeRows) Err() error   { return nil }
func (r *fakeRows) Close() error { return nil }

// fakeDialect is a Postgres-like dialect: "quotes" and $N.
type fakeDialect struct{}

func (fakeDialect) Name() string                           { return "fake" }
func (fakeDialect) Quote(id string) string                 { return `"` + id + `"` }
func (fakeDialect) Placeholder(n int) string               { return fmt.Sprintf("$%d", n) }
func (fakeDialect) BuildSelect(*orm.Query) (string, []any) { return "", nil }
func (fakeDialect) BuildInsert(*orm.Query) (string, []any) { return "", nil }
func (fakeDialect) BuildUpdate(*orm.Query) (string, []any) { return "", nil }
func (fakeDialect) BuildDelete(*orm.Query) (string, []any) { return "", nil }
func (fakeDialect) ColumnSQL(c *schema.Column) (string, error) {
	return fmt.Sprintf(`"%s" %s`, c.Name, strings.ToUpper(string(c.Type))), nil
}
func (fakeDialect) AlterColumnSQL(string, *schema.Column) (string, error) { return "", nil }
func (fakeDialect) TableSuffix() string                                   { return "" }
func (fakeDialect) CreateIndexSQL(string, *schema.Index) string           { return "" }
func (fakeDialect) DropIndexSQL(string, string) string                    { return "" }
func (fakeDialect) DropForeignKeySQL(string, string) (string, error)      { return "", nil }
func (fakeDialect) ForeignKeyClause(*schema.ForeignKey) string            { return "" }

// noDDLDialect is a dialect without DDL (like a driver that does not implement schema.Dialect).
type noDDLDialect struct{}

func (noDDLDialect) Name() string                           { return "noddl" }
func (noDDLDialect) Quote(id string) string                 { return id }
func (noDDLDialect) Placeholder(int) string                 { return "?" }
func (noDDLDialect) BuildSelect(*orm.Query) (string, []any) { return "", nil }
func (noDDLDialect) BuildInsert(*orm.Query) (string, []any) { return "", nil }
func (noDDLDialect) BuildUpdate(*orm.Query) (string, []any) { return "", nil }
func (noDDLDialect) BuildDelete(*orm.Query) (string, []any) { return "", nil }

type noDDLDB struct{ *fakeDB }

func (noDDLDB) Dialect() orm.Dialect { return noDDLDialect{} }
