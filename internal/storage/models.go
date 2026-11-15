package storage

// Chat 频道/群组
type Chat struct {
	ID         int64
	TgChatID   int64
	AccessHash int64
	Name       string
	Username   string
	Type       string
	Enabled    bool
	LastOffset int64
	// LastTopID 已同步过的最新消息 ID，用于只拉取比它更新的消息
	LastTopID int64
	SyncedAt  int64
	CreatedAt int64
}

// Show 短剧（一部剧）
type Show struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	NormName  string   `json:"-"`
	CoverPath string   `json:"cover_path"`
	Intro     string   `json:"intro"`
	Status    string   `json:"status"`
	Tags      []string `json:"tags"`
	EpCount   int      `json:"ep_count"`  // 有来源的集数（可播集数）
	EpLatest  int      `json:"ep_latest"` // 已更新到第几集（最大集号）
	// WatchedEp 已经看到第几集（0 表示还没看过），只在收藏列表里计算
	WatchedEp int `json:"watched_ep"`
	// Updates 收藏剧的新增集数（看到之后才发布的集），只有「追到最新一集」时才计数
	Updates   int   `json:"updates"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Episode 剧集
type Episode struct {
	ID        int64  `json:"id"`
	ShowID    int64  `json:"show_id"`
	EpisodeNo int    `json:"episode_no"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Source 线路来源（某频道中的某一段视频文件）
type Source struct {
	ID            int64   `json:"id"`
	EpisodeID     int64   `json:"-"`
	ChatID        int64   `json:"chat_id"`
	ChatName      string  `json:"chat_name"`
	// ChatUsername 频道公开用户名（公开频道才有）：网页版直链播放用它
	ChatUsername  string  `json:"chat_username,omitempty"`
	MessageID     int     `json:"message_id"`
	GroupedID     int64   `json:"-"`
	SegmentIndex  int     `json:"segment_index"`
	FileName      string  `json:"file_name"`
	FilePath      string  `json:"-"`
	FileSize      int64   `json:"file_size"`
	Duration      float64 `json:"duration"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	Mime          string  `json:"mime"`
	DocID         int64   `json:"-"`
	AccessHash    int64   `json:"-"`
	FileReference []byte  `json:"-"`
	// Caption 这条消息的原始文案，重建媒体库映射时用它重新解析剧名/集号
	Caption        string `json:"-"`
	DownloadStatus string `json:"download_status"`
	Error          string `json:"-"`
	CreatedAt      int64  `json:"created_at"`
	// VariantsChecked 是否已向 Telegram 查过这条来源的其它清晰度（alt_documents）。
	// 查过就不再重复请求；没有副版本时列表为空，前端不显示清晰度入口。
	VariantsChecked bool `json:"-"`
}

// SourceVariant 同一视频的其它清晰度版本（Telegram alt_documents）。
// 每个变体是独立文档，取流必须用它自己的 DocID/AccessHash/FileReference。
type SourceVariant struct {
	ID            int64   `json:"id"`
	SourceID      int64   `json:"-"`
	DocID         int64   `json:"-"`
	AccessHash    int64   `json:"-"`
	FileReference []byte  `json:"-"`
	FileSize      int64   `json:"file_size"`
	Duration      float64 `json:"duration"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	Mime          string  `json:"mime"`
	// Codec 编码（h264/h265/av1），同清晰度优先给浏览器能直接解码的版本
	Codec string `json:"codec,omitempty"`
}

// PlayLine 播放线路：同一频道来源的一集，可能包含多个分段
type PlayLine struct {
	ChatID   int64    `json:"chat_id"`
	ChatName string   `json:"chat_name"`
	Segments []Source `json:"segments"`
}

// TagCount 标签及其下短剧数量
type TagCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// WatchProgress 某集的观看进度（history 表一行）
type WatchProgress struct {
	EpisodeID int64   `json:"episode_id"`
	ShowID    int64   `json:"show_id"`
	EpisodeNo int     `json:"episode_no"`
	Position  float64 `json:"position"` // 已看到第几秒
	Duration  float64 `json:"duration"` // 总时长（秒），未知为 0
	Finished  bool    `json:"finished"`
	Progress  float64 `json:"progress"` // 0-1，前端画进度条
	UpdatedAt int64   `json:"updated_at"`
}

// HistoryItem 观看历史（按剧聚合后的「继续观看」条目）
type HistoryItem struct {
	EpisodeID int64   `json:"episode_id"`
	ShowID    int64   `json:"show_id"`
	ShowName  string  `json:"show_name"`
	CoverPath string  `json:"cover_path"`
	EpisodeNo int     `json:"episode_no"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Finished  bool    `json:"finished"`
	Progress  float64 `json:"progress"`
	UpdatedAt int64   `json:"updated_at"`
}
