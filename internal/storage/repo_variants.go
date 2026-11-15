package storage

import (
	"database/sql"
	"errors"
)

// ErrVariantNotFound 清晰度变体或来源不存在。
var ErrVariantNotFound = errors.New("清晰度不存在")

// ListSourceVariants 取一条来源的其它清晰度版本（按短边从高到低）。
func (d *DB) ListSourceVariants(sourceID int64) ([]*SourceVariant, error) {
	rows, err := d.db.Query(`
SELECT id, source_id, doc_id, access_hash, file_reference, file_size, duration, width, height, mime, codec
FROM source_variants WHERE source_id = ?
ORDER BY CASE WHEN MIN(width, height) > 0 THEN MIN(width, height) ELSE 0 END DESC, id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SourceVariant
	for rows.Next() {
		v := &SourceVariant{}
		if err := rows.Scan(&v.ID, &v.SourceID, &v.DocID, &v.AccessHash, &v.FileReference,
			&v.FileSize, &v.Duration, &v.Width, &v.Height, &v.Mime, &v.Codec); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetSourceVariant 按变体行 ID 取一个清晰度。
func (d *DB) GetSourceVariant(id int64) (*SourceVariant, error) {
	row := d.db.QueryRow(`
SELECT id, source_id, doc_id, access_hash, file_reference, file_size, duration, width, height, mime, codec
FROM source_variants WHERE id = ?`, id)
	v := &SourceVariant{}
	if err := row.Scan(&v.ID, &v.SourceID, &v.DocID, &v.AccessHash, &v.FileReference,
			&v.FileSize, &v.Duration, &v.Width, &v.Height, &v.Mime, &v.Codec); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVariantNotFound
		}
		return nil, err
	}
	return v, nil
}

// ReplaceSourceVariants 用最新查到的副文档更新某来源的清晰度列表（在事务内调用）。
// 按 doc_id upsert，保留已有变体的行 ID（前端用变体 ID 切清晰度，刷新引用不能换号）；
// 这次没再出现的旧变体删除。没有副版本时清空旧记录。
func ReplaceSourceVariants(tx *sql.Tx, sourceID int64, variants []SourceVariant) error {
	keep := make([]any, 0, len(variants))
	for _, v := range variants {
		keep = append(keep, v.DocID)
	}
	// 删除本次未出现的旧变体
	del := `DELETE FROM source_variants WHERE source_id = ?`
	args := []any{sourceID}
	if len(keep) > 0 {
		del += ` AND doc_id NOT IN (` + placeholders(len(keep)) + `)`
		args = append(args, keep...)
	}
	if _, err := tx.Exec(del, args...); err != nil {
		return err
	}
	if len(variants) == 0 {
		return nil
	}
	ins, err := tx.Prepare(`
INSERT INTO source_variants (
    source_id, doc_id, access_hash, file_reference, file_size, duration, width, height, mime, codec, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(doc_id) DO UPDATE SET
    source_id = excluded.source_id,
    access_hash = excluded.access_hash,
    file_reference = excluded.file_reference,
    file_size = excluded.file_size,
    duration = excluded.duration,
    width = excluded.width,
    height = excluded.height,
    mime = excluded.mime,
    codec = excluded.codec`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, v := range variants {
		if _, err := ins.Exec(sourceID, v.DocID, v.AccessHash, v.FileReference, v.FileSize,
			v.Duration, v.Width, v.Height, v.Mime, v.Codec, nowUnix()); err != nil {
			return err
		}
	}
	return nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := "?"
	for i := 1; i < n; i++ {
		out += ",?"
	}
	return out
}

// ReplaceSourceVariantsByID 在独立事务里覆盖某来源的清晰度列表（刷新/按需补齐用）。
func (d *DB) ReplaceSourceVariantsByID(sourceID int64, variants []SourceVariant) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ReplaceSourceVariants(tx, sourceID, variants); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkVariantsChecked 记录「这条来源已经查过 alt_documents」，避免重复请求 Telegram。
func (d *DB) MarkVariantsChecked(sourceID int64) error {
	_, err := d.db.Exec(`UPDATE sources SET variants_checked = 1 WHERE id = ?`, sourceID)
	return err
}

// SourceVariantsChecked 该来源是否已经查过清晰度列表。
func (d *DB) SourceVariantsChecked(sourceID int64) (bool, error) {
	var checked int
	err := d.db.QueryRow(`SELECT variants_checked FROM sources WHERE id = ?`, sourceID).Scan(&checked)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrVariantNotFound
		}
		return false, err
	}
	return checked == 1, nil
}

// UpdateVariantLocation 刷新副文档的 file_reference / access_hash（引用会过期）。
func (d *DB) UpdateVariantLocation(id, docID, accessHash int64, ref []byte) error {
	_, err := d.db.Exec(`UPDATE source_variants SET doc_id = ?, access_hash = ?, file_reference = ? WHERE id = ?`,
		docID, accessHash, ref, id)
	return err
}
