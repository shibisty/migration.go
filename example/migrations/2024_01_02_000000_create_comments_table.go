package migrations

import (
	"context"

	ormstore "migration-orm"
	"orm/schema"
)

func init() {
	register(ormstore.Migration{
		Name: "2024_01_02_000000_create_comments_table",
		Up: func(ctx context.Context, db *ormstore.DB) error {
			return db.Schema.Create(ctx, "comments", func(t *schema.Blueprint) {
				t.ID()
				t.Text("body")
				t.ForeignID("user_id").Constrained("users").OnDelete("cascade")
				// Polymorphic relation: commentable_type + commentable_id + index.
				t.Morphs("commentable")
				t.Timestamps()
			})
		},
		Down: func(ctx context.Context, db *ormstore.DB) error {
			return db.Schema.DropIfExists(ctx, "comments")
		},
	})
}
