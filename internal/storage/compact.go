package storage

import (
	"log"
)

const (
	// compactIndexVersion 索引压缩版本：改写规则升级后 +1 会重新压一遍
	compactIndexVersion = "2"
	// maxCaption 每条来源保留的文案长度上限（字符）。
	// 剧名、集号、标签都在开头，尾部多是刷屏的表情与广告，留着只是占空间。
	maxCaption = 600
)

// CompactIndex 把数据库收敛成「只做索引」的形态，纯本地操作，不访问 Telegram：
//
//  1. 过长的 caption 裁到 maxCaption 字符（文案是唯一需要留给「重建」的原文）；
//  2. VACUUM 回收释放出来的页。
//
// 幂等：通过 kv 记版本号，只跑一次。
func (d *DB) CompactIndex() (int, error) { return d.compactIndex(false) }

// ForceCompactIndex 无视版本标记强制压一遍（命令行 -compact 用）
func (d *DB) ForceCompactIndex() (int, error) { return d.compactIndex(true) }

func (d *DB) compactIndex(force bool) (int, error) {
	if !force {
		if done, _, err := d.GetKV("compact_index_v" + compactIndexVersion); err == nil && done == "1" {
			return 0, nil
		}
	}

	trimmed := 0
	// 正文裁剪：substr 按字符计，中文不会被截半
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
	// 回收空间：VACUUM 不能在事务里，放在所有写入之后
	if _, err := d.db.Exec(`VACUUM`); err != nil {
		log.Printf("VACUUM 失败（不影响使用）: %v", err)
	}
	if err := d.SetKV("compact_index_v"+compactIndexVersion, "1"); err != nil {
		return trimmed, err
	}
	return trimmed, nil
}
