# migration

A Laravel-style migration runner for Go: batches, rollback, status. Not tied
to any database: where the journal is stored and what a migration operates on
is decided by the backend (`Store`). No external dependencies, Go 1.22+.

[![Patreon](https://c5.patreon.com/external/logo/become_a_patron_button.png)](https://www.patreon.com/cw/shibisty)

| Backend | Package |
|---|---|
| SQL via orm (MySQL, Postgres, SQLite) | [`migration-orm`](https://github.com/shibisty/migration.go-orm-driver) |
| In-memory, for tests | `migration.NewMemoryStore` (this package) |
| Custom (MongoDB, Redis, files, API) | implement `migration.Store[DB]` |

## Installation

```bash
gtr add github:shibisty/migration.go
```

## How it works

```go
type Migration[DB any] struct {
    Name string                                   // unique; by convention starts with a date
    Up   func(ctx context.Context, db DB) error
    Down func(ctx context.Context, db DB) error   // optional
}

type Store[DB any] interface {
    Prepare(ctx context.Context) error                      // create the journal if missing
    Applied(ctx context.Context) ([]Record, error)          // journal in order of application
    Step(ctx context.Context, fn func(DB, Journal) error) error // atomic step
}
```

`DB` is what each migration receives: for `migration-orm` it is a transaction and
a schema builder; for a document database, its client. The batch logic is the same for all.

If the `Store` implements `Locker`, `Migrate` and `Rollback` take a lock, so two
instances of the application never apply migrations concurrently.

## Usage

```go
m, err := migration.New(store, migs...) // error on an empty/duplicate name or missing Up

ran, err := m.Migrate(ctx)          // all new ones as a single batch
back, err := m.Rollback(ctx, 1)     // last batch, in reverse order
back, err = m.Reset(ctx)            // everything
back, ran, err = m.Refresh(ctx)     // Reset + Migrate
st, err := m.Status(ctx)            // applied or not, batch, Missing: in the journal but not in code
pending, err := m.Pending(ctx)
```

Behavior:

- Each migration is a separate `Store.Step`: the action and the journal entry
  are atomic. An error stops `Migrate`; migrations already applied stay applied.
- `Rollback` first validates every migration in the batch (whether it has a `Down`,
  whether it is registered) and rolls back nothing if any of them can't be rolled back.
- Errors: `ErrInvalid`, `ErrDuplicateName`, `ErrNoDown`, `ErrUnknown`
  (check with `errors.Is`); the migration name is always included in the error text.

Laravel-style names: `migration.Name(time.Now(), "create_users_table")` →
`2024_01_01_120000_create_users_table`. The application order is the order in which
migrations are passed to `New`; sort them by name.

## Testing your own migrations

```go
store := migration.NewMemoryStore(myFakeDB)
m, _ := migration.New(store, migs...)
m.Migrate(ctx)
store.Records() // journal
```

## Development

In a checkout, run `gtr install` once (it generates `go.mod`), then:

```bash
gtr run test -- -race -cover   # 100% coverage
```

## License

MIT

[![Patreon](https://c5.patreon.com/external/logo/become_a_patron_button.png)](https://www.patreon.com/cw/shibisty)

If this project helps you, consider supporting its development on Patreon ❤️
