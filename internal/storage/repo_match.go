package storage

// ListPlayableShows 返回全部「至少有一集可播放」的短剧。
// 排行榜匹配只认这些条目：榜单上抓到了、但本地没有可播视频的剧不会出现在结果里。
func (d *DB) ListPlayableShows() ([]*Show, error) {
	rows, err := d.db.Query(showSelectSQL + playableShowWhere)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Show, 0, 256)
	for rows.Next() {
		s, err := scanShow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
