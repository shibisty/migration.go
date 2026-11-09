// Package cli is the command line of a migration program, Laravel-style:
//
//	migrate                    apply new migrations (one batch)
//	migrate:rollback [--step N] roll back the last batch (N batches)
//	migrate:reset              roll back everything
//	migrate:refresh            reset, then migrate
//	migrate:status             every migration: applied (batch) or pending
//	make:migration <name>      write migrations/<date>_<name>.go from a template
//
// The words may also be given without the prefix (rollback, status, make).
// A program in cmd/migrate calls Main, and gtr scripts call the program:
//
//	// cmd/migrate/main.go
//	func main() {
//	    os.Exit(cli.Main(os.Args[1:], cli.Options{Template: ormstore.Template}, func(ctx context.Context) (*migration.Migrator[*ormstore.DB], error) {
//	        conn, err := postgres.Open(os.Getenv("DATABASE_URL"))
//	        if err != nil {
//	            return nil, err
//	        }
//	        return ormstore.New(conn, migrations.All()...)
//	    }))
//	}
//
//	// gtr.json
//	"scripts": {
//	    "migrate": "go run ./cmd/migrate",
//	    "migrate:rollback": "go run ./cmd/migrate rollback",
//	    "migrate:status": "go run ./cmd/migrate status",
//	    "make:migration": "go run ./cmd/migrate make"
//	}
//
// gtr run loads .env, so DATABASE_URL may live there.
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"migration"
)

// Options configure Main and Run.
type Options struct {
	// Dir is where make:migration writes files (default "migrations").
	Dir string
	// Package is the Go package of the generated files (default: the base
	// name of Dir).
	Package string
	// Template is the text/template of a new migration file; a backend
	// provides one (migration-orm: ormstore.Template). It gets .Name (the
	// full name), .Description, .Package, .Table (users for
	// create_users_table, add_x_to_users_table) and .Create (create_*).
	Template string
	// Stdout and Stderr default to os.Stdout and os.Stderr.
	Stdout, Stderr io.Writer
	// Now is the clock of make:migration (default time.Now).
	Now func() time.Time
}

// Main runs the command in args and returns the exit code: 0, or 1 after
// printing the error. open connects only for commands that need the
// database, so make:migration works without one.
func Main[DB any](args []string, opts Options, open func(ctx context.Context) (*migration.Migrator[DB], error)) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	opts = opts.withDefaults()
	if err := Run(ctx, args, opts, open); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(opts.Stderr, "error:", err)
		}
		return 1
	}
	return 0
}

func (o Options) withDefaults() Options {
	if o.Dir == "" {
		o.Dir = "migrations"
	}
	if o.Package == "" {
		o.Package = filepath.Base(o.Dir)
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

const usage = `usage: migrate [command]
  migrate                     apply new migrations (default)
  migrate:rollback [--step N] roll back the last N batches (1)
  migrate:reset               roll back every migration
  migrate:refresh             reset, then migrate
  migrate:status              list migrations and their batches
  make:migration <name>       create <dir>/<date>_<name>.go
`

// Run is Main without the exit code.
func Run[DB any](ctx context.Context, args []string, opts Options, open func(ctx context.Context) (*migration.Migrator[DB], error)) error {
	opts = opts.withDefaults()
	cmd := "migrate"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	cmd = strings.TrimPrefix(cmd, "migrate:")
	if cmd == "make:migration" {
		cmd = "make"
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	fs.Usage = func() { fmt.Fprint(opts.Stderr, usage) }
	step := fs.Int("step", 1, "batches to roll back")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out := opts.Stdout
	if cmd == "make" {
		if fs.NArg() != 1 {
			return errors.New("make:migration needs a name, e.g. create_posts_table")
		}
		path, err := Make(opts, fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Created %s\n", path)
		return nil
	}
	switch cmd {
	case "migrate", "rollback", "reset", "refresh", "status":
	case "help":
		fmt.Fprint(out, usage)
		return nil
	default:
		fmt.Fprint(opts.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	m, err := open(ctx)
	if err != nil {
		return err
	}
	switch cmd {
	case "migrate":
		ran, err := m.Migrate(ctx)
		list(out, "Migrated", ran, "Nothing to migrate")
		return err
	case "rollback":
		if *step < 1 {
			return fmt.Errorf("--step must be at least 1, got %d", *step)
		}
		back, err := m.Rollback(ctx, *step)
		list(out, "Rolled back", back, "Nothing to roll back")
		return err
	case "reset":
		back, err := m.Reset(ctx)
		list(out, "Rolled back", back, "Nothing to roll back")
		return err
	case "refresh":
		back, ran, err := m.Refresh(ctx)
		list(out, "Rolled back", back, "Nothing to roll back")
		list(out, "Migrated", ran, "Nothing to migrate")
		return err
	default: // status
		st, err := m.Status(ctx)
		if err != nil {
			return err
		}
		if len(st) == 0 {
			fmt.Fprintln(out, "No migrations")
		}
		width := 0
		for _, s := range st {
			width = max(width, len(s.Name))
		}
		for _, s := range st {
			mark := "pending"
			switch {
			case s.Missing:
				mark = fmt.Sprintf("batch %d, file missing", s.Batch)
			case s.Applied:
				mark = fmt.Sprintf("batch %d", s.Batch)
			}
			fmt.Fprintf(out, "  %-*s  %s\n", width, s.Name, mark)
		}
		return nil
	}
}

func list(out io.Writer, verb string, names []string, none string) {
	if len(names) == 0 {
		fmt.Fprintln(out, none)
	}
	for _, n := range names {
		fmt.Fprintf(out, "%s %s\n", verb, n)
	}
}

var (
	descRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	identRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	createRe = regexp.MustCompile(`^create_(\w+?)_table$`)
	tableRe  = regexp.MustCompile(`_(?:to|from|in)_(\w+?)_table$`)
)

// Make writes a new migration file and returns its path; the name is
// snake_case (CreatePostsTable is accepted as create_posts_table).
func Make(opts Options, description string) (string, error) {
	opts = opts.withDefaults()
	if opts.Template == "" {
		return "", errors.New("no migration template: set cli.Options.Template (migration-orm: ormstore.Template)")
	}
	if !identRe.MatchString(opts.Package) {
		return "", fmt.Errorf("package name %q (from %s) is not a Go identifier; set cli.Options.Package", opts.Package, opts.Dir)
	}
	desc := snake(description)
	if !descRe.MatchString(desc) {
		return "", fmt.Errorf("invalid migration name %q: use letters, digits and _, e.g. create_posts_table", description)
	}
	data := struct {
		Name, Description, Package, Table string
		Create                            bool
	}{Name: migration.Name(opts.Now(), desc), Description: desc, Package: opts.Package}
	if m := createRe.FindStringSubmatch(desc); m != nil {
		data.Table, data.Create = m[1], true
	} else if m := tableRe.FindStringSubmatch(desc); m != nil {
		data.Table = m[1]
	}
	tmpl, err := template.New("migration").Parse(opts.Template)
	if err != nil {
		return "", fmt.Errorf("migration template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("migration template: %w", err)
	}
	src, err := format.Source(buf.Bytes())
	if err != nil {
		return "", fmt.Errorf("the migration template does not produce Go code: %w", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(opts.Dir, "*_"+desc+".go")); len(matches) > 0 {
		return "", fmt.Errorf("a migration %s already exists: %s", desc, matches[0])
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(opts.Dir, data.Name+".go")
	return path, os.WriteFile(path, src, 0o644)
}

// snake turns CreatePostsTable or create-posts-table into create_posts_table.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
			b.WriteRune(r + 'a' - 'A')
		case r == '-' || r == ' ':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
