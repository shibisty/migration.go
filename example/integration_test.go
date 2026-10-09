//go:build integration

package main

import (
	"context"
	"os"
	"reflect"
	"testing"

	"migration-example/migrations"

	ormstore "migration-orm"
	"orm"
)

// Full cycle against a real database: migrate → status → rollback → refresh → reset.
//
//	ORM_TEST_POSTGRES_DSN=postgres://... go test -tags integration .
//	ORM_TEST_MYSQL_DSN=root:@tcp(127.0.0.1:3306)/test go test -tags integration .
func TestFullCycle(t *testing.T) {
	for driver, env := range map[string]string{"postgres": "ORM_TEST_POSTGRES_DSN", "mysql": "ORM_TEST_MYSQL_DSN"} {
		t.Run(driver, func(t *testing.T) {
			dsn := os.Getenv(env)
			if dsn == "" {
				t.Skip(env + " is not set")
			}
			exec, err := open(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer exec.Close()
			ctx := context.Background()
			cleanup(ctx, t, exec)
			t.Cleanup(func() { cleanup(ctx, t, exec) })

			m, err := ormstore.New(exec, migrations.All()...)
			if err != nil {
				t.Fatal(err)
			}
			names := []string{"2024_01_01_000000_create_users_table", "2024_01_02_000000_create_comments_table"}

			ran, err := m.Migrate(ctx)
			if err != nil || !reflect.DeepEqual(ran, names) {
				t.Fatalf("Migrate = %v, %v", ran, err)
			}
			// The tables really exist: insert into comments referencing users.
			mustExec(ctx, t, exec, "INSERT INTO users (name, email, password_hash, created_at, updated_at) VALUES ('a', 'a@x', 'h', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)")

			if ran, err := m.Migrate(ctx); err != nil || ran != nil {
				t.Fatalf("second Migrate = %v, %v", ran, err)
			}
			st, err := m.Status(ctx)
			if err != nil || len(st) != 2 || !st[0].Applied || st[1].Batch != 1 {
				t.Fatalf("Status = %+v, %v", st, err)
			}

			back, err := m.Rollback(ctx, 1)
			if err != nil || !reflect.DeepEqual(back, []string{names[1], names[0]}) {
				t.Fatalf("Rollback = %v, %v", back, err)
			}
			if _, err := exec.ExecContext(ctx, "SELECT 1 FROM users"); err == nil {
				t.Fatal("users should be dropped after rollback")
			}

			if _, ran, err := m.Refresh(ctx); err != nil || len(ran) != 2 {
				t.Fatalf("Refresh = %v, %v", ran, err)
			}
			if back, err := m.Reset(ctx); err != nil || len(back) != 2 {
				t.Fatalf("Reset = %v, %v", back, err)
			}
		})
	}
}

func cleanup(ctx context.Context, t *testing.T, exec orm.SQLExecutor) {
	for _, tbl := range []string{"comments", "users", "migrations"} {
		_, _ = exec.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	}
}

func mustExec(ctx context.Context, t *testing.T, exec orm.SQLExecutor, q string) {
	t.Helper()
	if _, err := exec.ExecContext(ctx, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
