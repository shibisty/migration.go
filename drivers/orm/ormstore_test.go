package ormstore_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"migration"
	ormstore "migration-orm"
	"orm/schema"
)

var ctx = context.Background()

func createTable(name string) ormstore.Migration {
	return ormstore.Migration{
		Name: "create_" + name,
		Up: func(ctx context.Context, db *ormstore.DB) error {
			return db.Schema.Create(ctx, name, func(t *schema.Blueprint) {
				t.ID()
				t.String("title")
			})
		},
		Down: func(ctx context.Context, db *ormstore.DB) error {
			return db.Schema.DropIfExists(ctx, name)
		},
	}
}

func journal(t *testing.T, s *ormstore.Store) []migration.Record {
	t.Helper()
	recs, err := s.Applied(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestMigrateAndRollbackThroughORM(t *testing.T) {
	db := newFakeDB()
	m, err := ormstore.New(db, createTable("users"), createTable("posts"))
	if err != nil {
		t.Fatal(err)
	}

	ran, err := m.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, []string{"create_users", "create_posts"}) {
		t.Fatalf("ran = %v", ran)
	}

	exec := strings.Join(db.executed(), "\n")
	for _, want := range []string{
		`CREATE TABLE IF NOT EXISTS "migrations"`,
		`CREATE TABLE "users"`,
		`CREATE TABLE "posts"`,
		`INSERT INTO "migrations" ("migration", "batch") VALUES ($1, $2)`,
	} {
		if !strings.Contains(exec, want) {
			t.Errorf("executed SQL has no %q:\n%s", want, exec)
		}
	}

	st, _ := ormstore.NewStore(db)
	if got := journal(t, st); !reflect.DeepEqual(got, []migration.Record{{Name: "create_users", Batch: 1}, {Name: "create_posts", Batch: 1}}) {
		t.Fatalf("journal = %v", got)
	}

	back, err := m.Rollback(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, []string{"create_posts", "create_users"}) {
		t.Fatalf("rolled back = %v", back)
	}
	if got := journal(t, st); len(got) != 0 {
		t.Fatalf("journal after rollback = %v", got)
	}
	if !strings.Contains(strings.Join(db.executed(), "\n"), `DROP TABLE IF EXISTS "posts"`) {
		t.Error("Down should drop the table through db.Schema")
	}
}

// An error in Up rolls back the transaction: neither the DDL nor the journal entry is applied.
func TestFailedUpRollsBackStep(t *testing.T) {
	db := newFakeDB()
	db.failOn = `CREATE TABLE "posts"`
	m, _ := ormstore.New(db, createTable("users"), createTable("posts"))

	ran, err := m.Migrate(ctx)
	if !errors.Is(err, errFake) {
		t.Fatalf("want fake failure, got %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"create_users"}) {
		t.Fatalf("ran = %v", ran)
	}
	st, _ := ormstore.NewStore(db)
	if got := journal(t, st); !reflect.DeepEqual(got, []migration.Record{{Name: "create_users", Batch: 1}}) {
		t.Fatalf("journal = %v", got)
	}
	for _, q := range db.executed() {
		if strings.Contains(q, `"posts"`) {
			t.Fatalf("SQL of the failed step must not be committed: %q", q)
		}
	}
}

// A migration receives the step's transaction, not the original connection.
func TestMigrationGetsTransaction(t *testing.T) {
	db := newFakeDB()
	var gotTx bool
	m, _ := ormstore.New(db, ormstore.Migration{
		Name: "raw",
		Up: func(ctx context.Context, d *ormstore.DB) error {
			_, isTx := d.SQLExecutor.(interface{ Commit() error })
			gotTx = isTx && d.Schema != nil
			_, err := d.ExecContext(ctx, "UPDATE users SET active = 1")
			return err
		},
	})
	if _, err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if !gotTx {
		t.Fatal("migration should receive the step transaction and a Schema builder")
	}
	if !strings.Contains(strings.Join(db.executed(), "\n"), "UPDATE users SET active = 1") {
		t.Fatal("raw SQL in a migration should be committed")
	}
}

func TestCustomTable(t *testing.T) {
	db := newFakeDB()
	st, err := ormstore.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	st.WithTable("schema_versions")
	if st.Table() != "schema_versions" {
		t.Fatalf("Table() = %q", st.Table())
	}
	m, _ := migration.New[*ormstore.DB](st, createTable("users"))
	if _, err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	exec := strings.Join(db.executed(), "\n")
	if !strings.Contains(exec, `CREATE TABLE IF NOT EXISTS "schema_versions"`) ||
		!strings.Contains(exec, `INSERT INTO "schema_versions"`) {
		t.Fatalf("custom table not used:\n%s", exec)
	}
}

func TestNewStoreErrors(t *testing.T) {
	if _, err := ormstore.NewStore(nil); err == nil {
		t.Error("nil exec should be an error")
	}
	if _, err := ormstore.NewStore(noDDLDB{newFakeDB()}); err == nil || !strings.Contains(err.Error(), "DDL") {
		t.Errorf("driver without schema.Dialect should be rejected, got %v", err)
	}
	if _, err := ormstore.New(noDDLDB{newFakeDB()}); err == nil {
		t.Error("New should propagate the NewStore error")
	}
}

func TestTransactionErrors(t *testing.T) {
	cases := []struct {
		failTx string
		failOn string
		want   string
	}{
		{failTx: "begin", want: "begin"},
		{failTx: "commit", want: "commit"},
		{failTx: "rollback", failOn: `CREATE TABLE "users"`, want: "rollback"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			db := newFakeDB()
			if _, err := ormstore.NewStore(db); err != nil {
				t.Fatal(err)
			}
			// Prepare runs outside a transaction, so transaction errors do not affect it.
			m, _ := ormstore.New(db, createTable("users"))
			db.failTx, db.failOn = tc.failTx, tc.failOn
			_, err := m.Migrate(ctx)
			if !errors.Is(err, errFake) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestAppliedQueryError(t *testing.T) {
	db := newFakeDB()
	db.failQ = true
	st, _ := ormstore.NewStore(db)
	if _, err := st.Applied(ctx); !errors.Is(err, errFake) {
		t.Fatalf("got %v", err)
	}
}

// A panic in a migration rolls back the transaction and is re-raised.
func TestPanicRollsBack(t *testing.T) {
	db := newFakeDB()
	m, _ := ormstore.New(db, ormstore.Migration{
		Name: "boom",
		Up: func(ctx context.Context, d *ormstore.DB) error {
			_, _ = d.ExecContext(ctx, "UPDATE x SET y = 1")
			panic("boom")
		},
	})
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("panic should propagate, got %v", r)
			}
		}()
		_, _ = m.Migrate(ctx)
	}()
	for _, q := range db.executed() {
		if strings.Contains(q, "UPDATE x") {
			t.Fatal("work of a panicking migration must not be committed")
		}
	}
}
