// Package ormstore is a migration backend on top of orm and an SQL driver
// (mysql, postgres, sqlite). gtr package: migration-orm.
//
// The journal of applied migrations is stored in the migrations table (the name
// is configurable via WithTable); each migration runs in a transaction
// together with its journal entry. A migration receives a *DB: the transaction and
// a schema builder bound to that same transaction.
//
//	conn, _ := orm.New(ctx, mysql.Driver(dsn))
//	m, err := ormstore.New(conn.(orm.SQLExecutor),
//		ormstore.Migration{
//			Name: "2024_01_01_000000_create_users_table",
//			Up: func(ctx context.Context, db *ormstore.DB) error {
//				return db.Schema.Create(ctx, "users", func(t *schema.Blueprint) {
//					t.ID()
//					t.String("email").Unique()
//					t.Timestamps()
//				})
//			},
//			Down: func(ctx context.Context, db *ormstore.DB) error {
//				return db.Schema.DropIfExists(ctx, "users")
//			},
//		},
//	)
//	ran, err := m.Migrate(ctx)
//
// Note: MySQL executes DDL with an implicit COMMIT, so on MySQL rolling back
// a transaction does not undo CREATE/ALTER statements already executed. Postgres and SQLite
// roll back DDL completely.
package ormstore

import (
	"context"
	"fmt"

	"migration"
	"orm"
	"orm/schema"
)

// DB is what a migration receives: the transaction of the current step and a Schema
// that executes DDL in that same transaction.
type DB struct {
	orm.SQLExecutor
	Schema *schema.Builder
}

// Migration is a migration for this backend.
type Migration = migration.Migration[*DB]

// Migrator is the migration runner for this backend.
type Migrator = migration.Migrator[*DB]

// DefaultTable is the default journal table name (as in Laravel).
const DefaultTable = "migrations"

// Store implements migration.Store[*DB] on top of orm.SQLExecutor.
type Store struct {
	exec    orm.SQLExecutor
	dialect schema.Dialect
	table   string
}

// NewStore creates a Store. The driver dialect must support DDL
// (implement schema.Dialect), which is true for mysql, postgres and sqlite.
func NewStore(exec orm.SQLExecutor) (*Store, error) {
	if exec == nil {
		return nil, fmt.Errorf("ormstore: exec is nil")
	}
	d, ok := exec.Dialect().(schema.Dialect)
	if !ok {
		return nil, fmt.Errorf("ormstore: driver %q does not support DDL (schema.Dialect)", exec.Driver())
	}
	return &Store{exec: exec, dialect: d, table: DefaultTable}, nil
}

// New is a shorthand for NewStore + migration.New.
func New(exec orm.SQLExecutor, migrations ...Migration) (*Migrator, error) {
	s, err := NewStore(exec)
	if err != nil {
		return nil, err
	}
	return migration.New[*DB](s, migrations...)
}

// WithTable sets the journal table name.
func (s *Store) WithTable(name string) *Store {
	s.table = name
	return s
}

// Table returns the journal table name.
func (s *Store) Table() string { return s.table }

// Prepare creates the journal table if it does not exist.
func (s *Store) Prepare(ctx context.Context) error {
	return schema.New(s.exec, s.dialect).CreateIfNotExists(ctx, s.table, func(t *schema.Blueprint) {
		t.ID()
		t.String("migration")
		t.Integer("batch")
		t.Timestamp("applied_at").DefaultRaw("CURRENT_TIMESTAMP")
	})
}

// Applied returns the journal in order of application.
func (s *Store) Applied(ctx context.Context) ([]migration.Record, error) {
	q := fmt.Sprintf("SELECT %s, %s FROM %s ORDER BY %s",
		s.dialect.Quote("migration"), s.dialect.Quote("batch"), s.dialect.Quote(s.table), s.dialect.Quote("id"))
	rows, err := s.exec.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []migration.Record
	for rows.Next() {
		var r migration.Record
		if err := rows.Scan(&r.Name, &r.Batch); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Step runs fn in a transaction and rolls back on error.
func (s *Store) Step(ctx context.Context, fn func(db *DB, journal migration.Journal) error) error {
	tx, err := s.exec.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("ormstore: begin: %w", err)
	}
	// A panic in a migration must not leave the transaction open (on SQLite
	// with a single connection this would lock the database).
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	db := &DB{SQLExecutor: tx, Schema: schema.New(tx, s.dialect)}
	if err := fn(db, &journal{exec: tx, s: s}); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ormstore: commit: %w", err)
	}
	return nil
}

type journal struct {
	exec orm.SQLExecutor
	s    *Store
}

func (j *journal) Add(ctx context.Context, name string, batch int) error {
	d := j.s.dialect
	q := fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES (%s, %s)",
		d.Quote(j.s.table), d.Quote("migration"), d.Quote("batch"), d.Placeholder(1), d.Placeholder(2))
	_, err := j.exec.ExecContext(ctx, q, name, batch)
	return err
}

func (j *journal) Remove(ctx context.Context, name string) error {
	d := j.s.dialect
	q := fmt.Sprintf("DELETE FROM %s WHERE %s = %s", d.Quote(j.s.table), d.Quote("migration"), d.Placeholder(1))
	_, err := j.exec.ExecContext(ctx, q, name)
	return err
}

var _ migration.Store[*DB] = (*Store)(nil)
