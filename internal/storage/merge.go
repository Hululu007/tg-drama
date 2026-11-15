package storage

import (
	"database/sql"
	"log"
	"regexp"
	"strconv"
	"strings"

	"tgdrama/internal/parser"
)

// reTrailZero 兜底历史 bug 的「剧名0」式残留名
var reTrailZero = regexp.MustCompile(`^(.*\p{Han})0{1,3}$`)

// reNameEpToken 剧名中的集数标记：第27集 / EP27 / E27 / 27集 / 尾部孤立EP / 尾部空格数字（含范围）
var reNameEpToken = regexp.MustCompile(`(?i)第\s*[0-9零一二三四五六七八九十百两]{1,4}\s*(?:[-~－～至到]\s*[0-9零一二三四五六七八九十百两]{1,4})?\s*[集期话回]|[0-9]{1,3}\s*[集期话回]|[Ee][Pp][ _]?0*[0-9]{1,3}|\b[Ee][ _]?0*[0-9]{1,3}\b|\s[Ee][Pp]$|\s[0-9]{1,3}$`)

// reGluedTrail 紧贴汉字的尾数：母上攻略12 → 剧名「母上攻略」集号 12
var reGluedTrail = regexp.MustCompile(`^(.*\p{Han})0*[1-9][0-9]{0,2}$`)

// splitNameEpisode 从误拆剧名中剥离集数标记，返回基准名。
// 与截断法不同，剥离只移除集数 token，保留「高三爱情故事 27集 兰兰篇」
// 这类名称中集号之后的副标题；gluedEp 为紧贴汉字的尾数集号（可能为 0）。
func splitNameEpisode(name string) (base string, gluedEp int, ok bool) {
	stripped := strings.TrimSpace(reNameEpToken.ReplaceAllString(name, " "))
	if stripped != "" && stripped != name {
		return strings.Join(strings.Fields(stripped), " "), 0, true
	}
	if m := reGluedTrail.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(name[len(m[1]):])
		return strings.TrimSpace(m[1]), n, true
	}
	return "", 0, false
}

// MergeDuplicateShows 将因「剧名带集号/水印后缀」而被误拆的短剧
// 合并回基准剧。启动时执行，幂等：合并后的剧名不含集号，不会重复处理。
func (d *DB) MergeDuplicateShows() error {
	type showRow struct {
		id   int64
		name string
		norm string
	}
	rows, err := d.db.Query(`SELECT id, name, norm_name FROM shows`)
	if err != nil {
		return err
	}
	// 连接池只有 1 个连接：必须先读完结果集再做后续查询
	var list []showRow
	for rows.Next() {
		var r showRow
		if err := rows.Scan(&r.id, &r.name, &r.norm); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	p := parser.New()
	merged := 0
	for _, r := range list {
		// 只剥离集数标记得到基准名；集号优先取紧贴尾数，其次重新解析
		base, gluedEp, ok := splitNameEpisode(r.name)
		if !ok {
			if m := reTrailZero.FindStringSubmatch(r.name); m != nil {
				base, gluedEp, ok = strings.TrimSpace(m[1]), 1, true
			}
		}
		if !ok || base == "" {
			continue
		}
		ep := gluedEp
		if ep <= 0 {
			if parsed, perr := p.Parse(r.name, ""); perr == nil {
				ep = parsed.EpisodeNo
			}
		}
		if ep <= 0 || base == "" {
			continue
		}
		norm := parser.Normalize(base)
		if norm == "" || norm == r.norm {
			continue
		}
		// 只有基准剧真实存在且已有可播集时才合并：
		// 避免把「情趣318」这类本身带数字的正规剧名误拆成「情趣」+ 第318集。
		var targetID int64
		var playable int
		if qerr := d.db.QueryRow(`
SELECT sh.id, (SELECT COUNT(*) FROM episodes e JOIN sources s ON s.episode_id = e.id
               WHERE e.show_id = sh.id)
FROM shows sh WHERE sh.norm_name = ?`, norm).Scan(&targetID, &playable); qerr != nil {
			continue
		}
		if playable == 0 || targetID == r.id {
			continue
		}
		// 目标剧已经有这个集号：再合并就成了「同一集挂两段来源」，而这种名字
		// 更可能是正规续作（秘密教学2）或独立的数字标题，不是误拆的集名。
		// 宁可留着让它单独成剧，也不要把两部剧揉在一起。
		var dup int
		if qerr := d.db.QueryRow(`SELECT COUNT(*) FROM episodes WHERE show_id = ? AND episode_no = ?`,
			targetID, ep).Scan(&dup); qerr != nil || dup > 0 {
			continue
		}
		if err := d.moveShow(r.id, targetID, ep); err != nil {
			log.Printf("合并短剧 %q -> %q 失败: %v", r.name, base, err)
			continue
		}
		merged++
	}
	if merged > 0 {
		log.Printf("已合并 %d 部误拆短剧", merged)
	}
	return nil
}

// DeleteEmptyEpisodes 删除没有任何来源的剧集（合并/重建可能留下空集，导致集数虚高）
func (d *DB) DeleteEmptyEpisodes() error {
	res, err := d.db.Exec(`DELETE FROM episodes WHERE id NOT IN (SELECT DISTINCT episode_id FROM sources)`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("已清理 %d 个空剧集", n)
	}
	return nil
}

// CleanJunkTags 清理无分类意义的标签及其关联，并删除孤儿标签
func (d *DB) CleanJunkTags() error {
	res, err := d.db.Exec(`
DELETE FROM show_tags WHERE tag_id IN (
    SELECT id FROM tags WHERE
        name LIKE '%短剧%' OR name LIKE '%ai%' OR name LIKE '%AI%'
        OR name LIKE '%黄豆%' OR name LIKE '%黄果%' OR name LIKE '%魔改%'
        OR name LIKE '%传媒%' OR name LIKE '%原创%' OR name LIKE '%连载%'
        OR name LIKE '%内群%' OR name LIKE '%watermark%' OR name LIKE '%\_ep%' ESCAPE '\'
        OR name LIKE '\_%' ESCAPE '\'
        OR name IN ('9pm', 'black128x', '犬九', 'joioio_犬九', 'icicis', 'pupuwaifu',
            '噗噗', '老虎菜', 'kenny', 'kennys548534', '糖心', '单体作品', '原档无水',
            '花少母带', '泰剧', '欧美版', '吃瓜', '写实', '落地', 'asmr', '采耳', 'pmv', '出差'))`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("已清理无意义标签关联 %d 条", n)
	}
	// 删除不再被任何短剧引用的标签
	if _, err := d.db.Exec(`
DELETE FROM tags WHERE id NOT IN (SELECT DISTINCT tag_id FROM show_tags)`); err != nil {
		return err
	}
	return nil
}

// CleanupEmptyShows 删除既无剧集也无预览图的空壳短剧（历史合并/重建残留）
func (d *DB) CleanupEmptyShows() error {
	if _, err := d.db.Exec(`
DELETE FROM show_tags WHERE show_id NOT IN (
SELECT DISTINCT show_id FROM episodes
UNION SELECT DISTINCT show_id FROM show_photos)`); err != nil {
		return err
	}
	res, err := d.db.Exec(`
DELETE FROM shows WHERE id NOT IN (
SELECT DISTINCT show_id FROM episodes
UNION SELECT DISTINCT show_id FROM show_photos)`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("已清理 %d 部空壳短剧", n)
	}
	return nil
}

// moveShow 把 fromID 剧的所有集与来源迁移到 toID 剧，迁移完删除空壳。
// nameEp 是从剧名中解析出的集号，用于修正误建的 episode_no=1 的集。
func (d *DB) moveShow(fromID, toID int64, nameEp int) error {
	eps, err := d.ListEpisodes(fromID)
	if err != nil {
		return err
	}
	for _, e := range eps {
		newNo := e.EpisodeNo
		if newNo <= 1 && nameEp > 1 {
			newNo = nameEp
		}
		var destID int64
		err := d.db.QueryRow(
			`SELECT id FROM episodes WHERE show_id = ? AND episode_no = ?`,
			toID, newNo).Scan(&destID)
		switch {
		case err == nil:
			// 目标集已存在：把来源挂过去后删除空集
			if _, err := d.db.Exec(`UPDATE sources SET episode_id = ? WHERE episode_id = ?`,
				destID, e.ID); err != nil {
				return err
			}
			if _, err := d.db.Exec(`DELETE FROM episodes WHERE id = ?`, e.ID); err != nil {
				return err
			}
		case err == sql.ErrNoRows:
			if _, err := d.db.Exec(`
UPDATE episodes SET show_id = ?, episode_no = ?, updated_at = ? WHERE id = ?`,
				toID, newNo, nowUnix(), e.ID); err != nil {
				return err
			}
		default:
			return err
		}
	}
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM episodes WHERE show_id = ?`, fromID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := d.db.Exec(`DELETE FROM show_tags WHERE show_id = ?`, fromID); err != nil {
			return err
		}
		// 预览图与收藏都没有 ON DELETE CASCADE，必须先转移/清掉，
		// 否则删除 shows 会报 FOREIGN KEY constraint failed (787)。
		if _, err := d.db.Exec(`UPDATE OR IGNORE show_photos SET show_id = ? WHERE show_id = ?`,
			toID, fromID); err != nil {
			return err
		}
		if _, err := d.db.Exec(`DELETE FROM show_photos WHERE show_id = ?`, fromID); err != nil {
			return err
		}
		if _, err := d.db.Exec(`INSERT OR IGNORE INTO favorites (show_id, created_at, base_ep)
SELECT ?, created_at, base_ep FROM favorites WHERE show_id = ?`, toID, fromID); err != nil {
			return err
		}
		if _, err := d.db.Exec(`DELETE FROM favorites WHERE show_id = ?`, fromID); err != nil {
			return err
		}
		if _, err := d.db.Exec(`DELETE FROM shows WHERE id = ?`, fromID); err != nil {
			return err
		}
	}
	return nil
}

// HasCover 短剧是否已有封面
func (d *DB) HasCover(showID int64) (bool, error) {
	var p string
	err := d.db.QueryRow(`SELECT cover_path FROM shows WHERE id = ?`, showID).Scan(&p)
	if err != nil {
		return false, err
	}
	return p != "", nil
}

// ListShowsWithoutCover 列出尚无封面、但还有来源消息可以重取缩略图的短剧 ID。
//
// 只列「启用频道里还有视频、且没有被判为非短剧」的剧，并按最近更新排序：
// 回填是有预算的（见 BackfillCovers），先补用户打开列表最可能看到的那批；
// 把关闭频道/隐藏条目的剧混进来只会白白发出取不到消息的请求。
func (d *DB) ListShowsWithoutCover(limit int) ([]int64, error) {
	rows, err := d.db.Query(`
SELECT e.show_id FROM shows sh
JOIN episodes e ON e.show_id = sh.id
JOIN sources s ON s.episode_id = e.id
JOIN chats c ON c.id = s.chat_id AND c.enabled = 1
WHERE sh.cover_path = '' AND sh.hidden = 0 AND s.doc_id <> 0
GROUP BY e.show_id
ORDER BY MAX(sh.updated_at) DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
