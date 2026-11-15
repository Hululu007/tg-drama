// Package rankings 抓取外部短剧榜单，并与本地媒体库做片名匹配。
//
// 设计要点：
//   - 榜单来自 5 个站点，任何一个站点失败都不影响其它站点；
//   - 抓取结果落盘成 JSON，进程重启后先读盘，只有超过 interval 才会再抓；
//   - 匹配在请求时按当前媒体库版本实时计算，库更新后榜单立刻反映新入库的剧；
//   - 只有「本地至少有一集可播放」的短剧才会出现在榜单结果里。
package rankings

import "tgdrama/internal/storage"

// Item 榜单里的一个条目（只保留展示与匹配需要的字段）
type Item struct {
	Rank  int    `json:"rank"`
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	Cover string `json:"cover,omitempty"`
	Heat  string `json:"heat,omitempty"`
}

// Board 单个榜单的抓取结果
type Board struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Site      string `json:"site"`
	URL       string `json:"url"`
	Items     []Item `json:"items"`
	FetchedAt int64  `json:"fetched_at"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// Snapshot 落盘快照。UpdatedAt 是最近一次「有榜单抓到内容」的时间，
// AttemptedAt 是最近一次尝试抓取的时间——抓取节奏按 AttemptedAt 算，
// 避免某个站点临时抽风时被反复重试。
type Snapshot struct {
	Version     int     `json:"version"`
	UpdatedAt   int64   `json:"updated_at"`
	AttemptedAt int64   `json:"attempted_at"`
	Boards      []Board `json:"boards"`
}

// MatchedItem 榜单条目 + 匹配到的本地短剧
type MatchedItem struct {
	Rank  int           `json:"rank"`
	Title string        `json:"title"`
	Heat  string        `json:"heat,omitempty"`
	URL   string        `json:"url,omitempty"`
	Show  *storage.Show `json:"show"`

	// score 仅用于调试与排序，不下发前端
	score float64
}

// BoardView 下发给前端的榜单视图
type BoardView struct {
	Key       string        `json:"key"`
	Name      string        `json:"name"`
	Site      string        `json:"site"`
	URL       string        `json:"url"`
	OK        bool          `json:"ok"`
	FetchedAt int64         `json:"fetched_at"`
	Error     string        `json:"error,omitempty"`
	Total     int           `json:"total"`   // 榜单抓到的条数
	Matched   int           `json:"matched"` // 其中在本地库里能匹配且可播的条数
	Items     []MatchedItem `json:"items"`
}

// Payload GET /api/rankings 的响应
type Payload struct {
	UpdatedAt     int64       `json:"updated_at"`
	AttemptedAt   int64       `json:"attempted_at"`
	NextUpdateAt  int64       `json:"next_update_at"`
	IntervalHours int         `json:"interval_hours"`
	Boards        []BoardView `json:"boards"`
}
