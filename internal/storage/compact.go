package storage

import (
	"log"
)

const (
	compactIndexVersion = "2"

	maxCaption = 600
)

func (d *DB) CompactIndex() (int, error) { return d.compactIndex(false) }

func (d *DB) ForceCompactIndex() (int, error) { return d.compactIndex(true) }

func (d *DB) compactIndex(force bool) (int, error) {
	if !force {
		if done, _, err := d.GetKV("compact_index_v" + compactIndexVersion); err == nil && done == "1" {
			return 0, nil
		}
	}

	trimmed := 0

	for _, q := range []string{
		`UPDATE sources SET caption = substr(caption, 1, ?) WHERE length(caption) > ?`,
		`UPDATE show_photos SET caption = substr(caption, 1, ?) WHERE length(caption) > ?`,
	} {
		res, err := d.db.Exec(q, maxCaption, maxCaption)
		if err != nil {
			return trimmed, err
		}
		if n, err := res.RowsAffected(); err == nil {
			trimmed += int(n)
		}
	}

	if _, err := d.db.Exec(`VACUUM`); err != nil {
		log.Printf("VACUUM 失败（不影响使用）: %v", err)
	}
	if err := d.SetKV("compact_index_v"+compactIndexVersion, "1"); err != nil {
		return trimmed, err
	}
	return trimmed, nil
}
