# migration-orm

A backend for [`migration`](https://github.com/shibisty/migration.go) on top of
[`orm`](https://github.com/shibisty/orm.go) and an SQL driver: MySQL, Postgres, SQLite.
The Go package is `ormstore`.

- The journal is the `migrations` table (`id`, `migration`, `batch`, `applied_at`);
  the name can be changed via `WithTable`.
- Each migration runs in a transaction together with its journal entry.
- A migration receives an `*ormstore.DB`: the step's transaction and a `Schema` that
  executes DDL in that same transaction.

## Installation

```bash
gtr add github:shibisty/migration.go github:shibisty/orm.go \
        github:shibisty/orm.go-postgres-driver github:shibisty/migration.go-orm-driver
```

`migration` and `orm` are peer dependencies (`^0.1`); add the orm driver of your database.

## Usage

```go
import (
    "orm"
    postgres "orm-postgres"
    ormstore "migration-orm"
    "orm/schema"
)

conn, _ := postgres.Open(dsn)

m, err := ormstore.New(conn,
    ormstore.Migration{
        Name: "2024_01_01_000000_create_users_table",
        Up: func(ctx context.Context, db *ormstore.DB) error {
            return db.Schema.Create(ctx, "users", func(t *schema.Blueprint) {
                t.ID()
                t.String("email").Unique()
                t.Timestamps()
            })
        },
        Down: func(ctx context.Context, db *ormstore.DB) error {
            return db.Schema.DropIfExists(ctx, "users")
        },
    },
)
ran, err := m.Migrate(ctx)
```

Plain SQL is available inside a migration as well: `db.ExecContext(ctx, "UPDATE …")`.

A full example with the `migrate | rollback | reset | refresh | status` commands is in
`migration.go/example` in the local workspace.

## Limitations

- **MySQL executes DDL with an implicit COMMIT.** If a migration fails after
  `CREATE TABLE`, the table remains even though the journal entry is rolled back. Postgres
  and SQLite roll back DDL completely.
- Locking against concurrent runs (`migration.Locker`) is not implemented yet:
  run migrations from a single process (for example, as a deploy step).

## Tests

In a checkout, run `gtr install` once, then:

```bash
gtr run test -- -race -cover   # fake database with real transactions, 98% coverage
```

The full cycle against a real database is covered by the integration test in `migration.go/example`:

```bash
cd migration.go/example
ORM_TEST_POSTGRES_DSN="postgres://…" gtr run test:integration
ORM_TEST_MYSQL_DSN="root:@tcp(127.0.0.1:3306)/test" gtr run test:integration
```

## License

MIT
