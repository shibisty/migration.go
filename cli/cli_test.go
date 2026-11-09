package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"migration"
	"migration/cli"
)

type fakeDB struct{ tables map[string]bool }

func migs(db *fakeDB) []migration.Migration[*fakeDB] {
	mk := func(name, table string) migration.Migration[*fakeDB] {
		return migration.Migration[*fakeDB]{
			Name: name,
			Up:   func(context.Context, *fakeDB) error { db.tables[table] = true; return nil },
			Down: func(context.Context, *fakeDB) error { delete(db.tables, table); return nil },
		}
	}
	return []migration.Migration[*fakeDB]{
		mk("2024_01_01_000000_create_users_table", "users"),
		mk("2024_01_02_000000_create_posts_table", "posts"),
	}
}

const tmpl = `package {{.Package}}

// {{.Name}} {{.Description}} table={{.Table}} create={{.Create}}
var _ = 1
`

func setup(t *testing.T) (*fakeDB, func(args ...string) (string, error)) {
	db := &fakeDB{tables: map[string]bool{}}
	store := migration.NewMemoryStore(db)
	all := migs(db)
	opened := 0
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		opts := cli.Options{Stdout: &out, Stderr: &out, Template: tmpl, Dir: filepath.Join(t.TempDir(), "migrations"),
			Now: func() time.Time { return time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) }}
		err := cli.Run(context.Background(), args, opts, func(context.Context) (*migration.Migrator[*fakeDB], error) {
			opened++
			return migration.New(store, all...)
		})
		return out.String(), err
	}
	return db, run
}

func TestCommands(t *testing.T) {
	db, run := setup(t)
	if out, err := run("migrate:status"); err != nil || !strings.Contains(out, "2024_01_01_000000_create_users_table  pending") {
		t.Fatalf("status: %q %v", out, err)
	}
	if out, err := run(); err != nil || !strings.Contains(out, "Migrated 2024_01_02_000000_create_posts_table") || !db.tables["posts"] {
		t.Fatalf("migrate: %q %v", out, err)
	}
	if out, _ := run("migrate"); !strings.Contains(out, "Nothing to migrate") {
		t.Fatalf("second migrate: %q", out)
	}
	if out, _ := run("status"); !strings.Contains(out, "batch 1") {
		t.Fatalf("status after migrate: %q", out)
	}
	if out, err := run("migrate:rollback", "--step", "1"); err != nil || !strings.Contains(out, "Rolled back 2024_01_02") || db.tables["users"] {
		t.Fatalf("rollback: %q %v %v", out, err, db.tables)
	}
	if out, _ := run("rollback"); !strings.Contains(out, "Nothing to roll back") {
		t.Fatalf("empty rollback: %q", out)
	}
	if out, err := run("refresh"); err != nil || !strings.Contains(out, "Migrated") {
		t.Fatalf("refresh: %q %v", out, err)
	}
	if out, err := run("migrate:reset"); err != nil || len(db.tables) != 0 || !strings.Contains(out, "Rolled back") {
		t.Fatalf("reset: %q %v", out, err)
	}
	if _, err := run("rollback", "--step", "0"); err == nil {
		t.Fatal("--step 0 accepted")
	}
	if _, err := run("fly"); err == nil {
		t.Fatal("unknown command accepted")
	}
	if _, err := run("status", "extra"); err == nil {
		t.Fatal("extra argument accepted")
	}
	if out, err := run("help"); err != nil || !strings.Contains(out, "make:migration") {
		t.Fatalf("help: %q %v", out, err)
	}
	if _, err := run("--nope"); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestMake(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db", "migrations")
	opts := cli.Options{Dir: dir, Template: tmpl, Now: func() time.Time { return time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) }}
	path, err := cli.Make(opts, "CreatePostsTable")
	if err != nil || filepath.Base(path) != "2026_11_09_100000_create_posts_table.go" {
		t.Fatalf("Make: %s %v", path, err)
	}
	src, _ := os.ReadFile(path)
	if !strings.Contains(string(src), "package migrations") || !strings.Contains(string(src), "table=posts create=true") {
		t.Fatalf("generated:\n%s", src)
	}
	if _, err := cli.Make(opts, "create_posts_table"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate: %v", err)
	}
	p2, _ := cli.Make(opts, "add-avatar-to-users-table")
	if src, _ := os.ReadFile(p2); !strings.Contains(string(src), "table=users create=false") {
		t.Fatalf("alter: %s", src)
	}
	for _, bad := range []string{"", "1abc", "drop;table"} {
		if _, err := cli.Make(opts, bad); err == nil {
			t.Errorf("bad name %q accepted", bad)
		}
	}
	if _, err := cli.Make(cli.Options{Dir: filepath.Join(dir, "1x"), Template: tmpl}, "x"); err == nil || !strings.Contains(err.Error(), "Options.Package") {
		t.Fatalf("bad package: %v", err)
	}
	if _, err := cli.Make(cli.Options{Dir: dir}, "x"); err == nil {
		t.Fatal("no template accepted")
	}
	if _, err := cli.Make(cli.Options{Dir: dir, Template: "{{.Nope"}, "y"); err == nil {
		t.Fatal("broken template accepted")
	}
	if _, err := cli.Make(cli.Options{Dir: dir, Template: "{{.Nope}}"}, "z"); err == nil {
		t.Fatal("template error ignored")
	}
	if _, err := cli.Make(cli.Options{Dir: dir, Template: "not go"}, "w"); err == nil {
		t.Fatal("non-Go output accepted")
	}

	// make:migration through Run never opens the database.
	var out bytes.Buffer
	err = cli.Run(context.Background(), []string{"make:migration", "create_tags_table"}, cli.Options{Dir: dir, Template: tmpl, Stdout: &out},
		func(context.Context) (*migration.Migrator[*fakeDB], error) { return nil, errors.New("no database") })
	if err != nil || !strings.Contains(out.String(), "Created") {
		t.Fatalf("make via Run: %q %v", out.String(), err)
	}
	if err := cli.Run(context.Background(), []string{"make"}, cli.Options{Dir: dir, Template: tmpl, Stdout: &out},
		func(context.Context) (*migration.Migrator[*fakeDB], error) { return nil, nil }); err == nil {
		t.Fatal("make without a name")
	}
}

func TestMainExitCodes(t *testing.T) {
	var out bytes.Buffer
	open := func(context.Context) (*migration.Migrator[*fakeDB], error) { return nil, errors.New("cannot connect") }
	if code := cli.Main([]string{"status"}, cli.Options{Stdout: &out, Stderr: &out}, open); code != 1 || !strings.Contains(out.String(), "cannot connect") {
		t.Fatalf("Main = %d, %q", code, out.String())
	}
	out.Reset()
	if code := cli.Main([]string{"help"}, cli.Options{Stdout: &out, Stderr: &out}, open); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	out.Reset()
	if code := cli.Main([]string{"-h"}, cli.Options{Stdout: &out, Stderr: &out}, open); code != 1 || strings.Contains(out.String(), "error:") {
		t.Fatalf("-h: %d %q", code, out.String())
	}
}
