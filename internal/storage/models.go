package storage

type Chat struct {
	ID         int64
	TgChatID   int64
	AccessHash int64
	Name       string
	Username   string
	Type       string
	Enabled    bool
	LastOffset int64

	LastTopID int64
	SyncedAt  int64
	CreatedAt int64
}

type Show struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	NormName  string   `json:"-"`
	CoverPath string   `json:"cover_path"`
	Intro     string   `json:"intro"`
	Status    string   `json:"status"`
	Tags      []string `json:"tags"`
	EpCount   int      `json:"ep_count"`
	EpLatest  int      `json:"ep_latest"`

	WatchedEp int `json:"watched_ep"`

	Updates   int   `json:"updates"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

type Episode struct {
	ID        int64  `json:"id"`
	ShowID    int64  `json:"show_id"`
	EpisodeNo int    `json:"episode_no"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type Source struct {
	ID        int64  `json:"id"`
	EpisodeID int64  `json:"-"`
	ChatID    int64  `json:"chat_id"`
	ChatName  string `json:"chat_name"`

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

	Caption        string `json:"-"`
	DownloadStatus string `json:"download_status"`
	Error          string `json:"-"`
	CreatedAt      int64  `json:"created_at"`

	VariantsChecked bool `json:"-"`
}

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

	Codec string `json:"codec,omitempty"`
}

type PlayLine struct {
	ChatID   int64    `json:"chat_id"`
	ChatName string   `json:"chat_name"`
	Segments []Source `json:"segments"`
}

type TagCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type WatchProgress struct {
	EpisodeID int64   `json:"episode_id"`
	ShowID    int64   `json:"show_id"`
	EpisodeNo int     `json:"episode_no"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Finished  bool    `json:"finished"`
	Progress  float64 `json:"progress"`
	UpdatedAt int64   `json:"updated_at"`
}

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
