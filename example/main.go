// Example: migrations via migration + migration-orm on MySQL or Postgres.
//
//	go run . -driver postgres -dsn "postgres://user:pass@localhost:5432/app?sslmode=disable" migrate
//	go run . -driver mysql    -dsn "root:@tcp(localhost:3306)/app" status
//
// Commands: migrate (default), rollback [N], reset, refresh, status.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

	"migration-example/migrations"

	ormstore "migration-orm"
	"orm"
	mysql "orm-mysql"
	postgres "orm-postgres"
)

func main() {
	driver := flag.String("driver", "postgres", "mysql | postgres")
	dsn := flag.String("dsn", os.Getenv("DATABASE_DSN"), "connection string (or DATABASE_DSN)")
	flag.Parse()

	ctx := context.Background()
	exec, err := open(*driver, *dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer exec.Close()

	m, err := ormstore.New(exec, migrations.All()...)
	if err != nil {
		log.Fatal(err)
	}

	cmd := flag.Arg(0)
	switch cmd {
	case "", "migrate":
		ran, err := m.Migrate(ctx)
		report("applied", ran, err)
	case "rollback":
		steps, _ := strconv.Atoi(flag.Arg(1))
		back, err := m.Rollback(ctx, steps)
		report("rolled back", back, err)
	case "reset":
		back, err := m.Reset(ctx)
		report("rolled back", back, err)
	case "refresh":
		back, ran, err := m.Refresh(ctx)
		report("rolled back", back, nil)
		report("applied", ran, err)
	case "status":
		st, err := m.Status(ctx)
		if err != nil {
			log.Fatal(err)
		}
		for _, s := range st {
			mark := "  pending"
			switch {
			case s.Missing:
				mark = fmt.Sprintf("? applied in batch %d, but the file is missing", s.Batch)
			case s.Applied:
				mark = fmt.Sprintf("✓ batch %d", s.Batch)
			}
			fmt.Printf(" %-45s %s\n", s.Name, mark)
		}
	default:
		log.Fatalf("unknown command %q", cmd)
	}
}

func open(driver, dsn string) (orm.SQLExecutor, error) {
	if dsn == "" {
		return nil, fmt.Errorf("specify -dsn or DATABASE_DSN")
	}
	switch driver {
	case "mysql":
		return mysql.Open(dsn)
	case "postgres":
		return postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("unknown driver %q", driver)
	}
}

func report(what string, names []string, err error) {
	for _, n := range names {
		fmt.Printf(" %s: %s\n", what, n)
	}
	if err != nil {
		log.Fatal(err)
	}
}
