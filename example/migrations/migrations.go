// Package migrations contains the example migrations. One migration per file;
// the file name matches Migration.Name, and each file adds itself to All.
package migrations

import (
	"sort"

	ormstore "migration-orm"
)

var all []ormstore.Migration

// All returns the migrations sorted by name (names start with a date).
func All() []ormstore.Migration {
	out := append([]ormstore.Migration(nil), all...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func register(m ormstore.Migration) { all = append(all, m) }
