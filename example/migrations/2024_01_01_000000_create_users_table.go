package migrations

import (
	"context"

	ormstore "migration-orm"
	"orm/schema"
)

func init() {
	register(ormstore.Migration{
		Name: "2024_01_01_000000_create_users_table",
		Up: func(ctx context.Context, db *ormstore.DB) error {
			return db.Schema.Create(ctx, "users", func(t *schema.Blueprint) {
				t.ID()
				t.String("name")
				t.String("email").Unique()
				t.String("password_hash")
				t.Integer("age").Nullable()
				t.Boolean("is_active").Default(true)
				t.RememberToken()
				t.Timestamps()
				t.SoftDeletes()
			})
		},
		Down: func(ctx context.Context, db *ormstore.DB) error {
			return db.Schema.DropIfExists(ctx, "users")
		},
	})
}
