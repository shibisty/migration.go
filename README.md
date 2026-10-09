# migration.go — local workspace

Not a repository: each subdirectory below is a separate repository (ADR-0006).

| Directory | Repository | gtr package |
|---|---|---|
| `core` | `shibisty/migration.go` | `migration` |
| `drivers/orm` | `shibisty/migration.go-orm-driver` | `migration-orm` |
| `example` | — | example application |

`gtr.json` here (not committed) makes the folder a gtr workspace (ADR-0009): the packages
are used in place, while each package's own `gtr.json` keeps its real sources for its CI.
`migration-orm` and the example need orm: the workspace takes `orm`, `orm-mysql` and
`orm-postgres` from the sibling folder `orm.go`, so `orm.go` must sit alongside.

```bash
gtr install          # one gtr.lock and gtr_modules/ here
gtr run -r test      # every package's tests
```
