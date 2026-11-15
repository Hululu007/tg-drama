package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config 全局配置
type Config struct {
	Telegram TelegramConfig `yaml:"telegram"`
	Server   ServerConfig   `yaml:"server"`
	Data     DataConfig     `yaml:"data"`
	Stream   StreamConfig   `yaml:"stream"`
	Rankings RankingConfig  `yaml:"rankings"`
}

// RankingConfig 外部榜单抓取配置。
// 默认开启（每 12 小时抓一轮，结果落盘到数据目录的 rankings.json）；
// 不想要就在 config.yaml 里写 rankings.disabled: true。
type RankingConfig struct {
	Disabled bool `yaml:"disabled"`
	// IntervalHours 抓取间隔（小时），默认 12
	IntervalHours int `yaml:"interval_hours"`
	// TimeoutSec 单个站点的抓取超时（秒），默认 25
	TimeoutSec int `yaml:"timeout_sec"`
	// Sources 只启用指定榜单（key：yeguo/dj51/huangguo/huangtao/huangguoai），留空表示全部
	Sources []string `yaml:"sources"`
}

// Enabled 榜单功能是否开启
func (c RankingConfig) Enabled() bool { return !c.Disabled }

// TelegramConfig Telegram 客户端配置。
// 接入方式为标准 MTProto（TCP 直连 DC，可经代理），凭证必须由配置提供——
// 共享/公开的 api_id 会被官方风控，表现为登录验证码收不到（AUTH 系列失败）。
type TelegramConfig struct {
	// APIID my.telegram.org 申请的应用 ID，见 api_hash
	APIID int `yaml:"api_id"`
	// APIHash 与 api_id 配对的应用密钥（32 位十六进制）。
	// 两者缺一不可：全部分发实例共享同一凭证时风控压力集中在凭证上，
	// 验证码投递异常首先检查这里是否被限。
	APIHash string `yaml:"api_hash"`
	// Proxy 形如 socks5://127.0.0.1:1080，留空表示直连
	Proxy string `yaml:"proxy"`
	// Transport 主 DC 的传输方式：
	//   auto（默认）优先 wss://*.web.telegram.org/apiws，失败回退 DC IP 直连；
	//   tcp 优先 DC IP 直连（有代理时推荐：握手不带浏览器指纹，
	//       与 api_id 上报的设备信息更一致，不易被判成冒充官方客户端）；
	//   ws 只用 WebSocket（DC IP 被封锁、又没法用代理时用）。
	Transport string `yaml:"transport"`
}

// ServerConfig Web 服务配置
type ServerConfig struct {
	Listen string `yaml:"listen"`
}

// DataConfig 数据目录配置
type DataConfig struct {
	Dir string `yaml:"dir"`
}

// StreamConfig 在线播放（/api/stream）的读取参数。
// 播放固定单线程顺序读取：Telegram Web 端并发读块不会提速，反而容易触发风控限流。
type StreamConfig struct {
	// ChunkKB 单次 upload.getFile 读取的块大小（64~1024KB，必须是 4KB 的倍数；
	// 官方上限 1MB，默认 1024 让请求数减半，配合预读收益最大）
	ChunkKB int `yaml:"chunk_kb"`
	// CacheMB 已读视频块的内存缓存上限，用于拖动/重播时直接命中
	CacheMB int `yaml:"cache_mb"`
	// DiskCacheMB 视频块磁盘缓存上限（边播边落盘，重启有效；0=关闭）
	DiskCacheMB int `yaml:"disk_cache_mb"`
}

// Load 读取并解析配置文件
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	c.applyDefaults()
	return &c, nil
}

// Default 返回内置默认配置：插件环境没有 config.yaml 时使用（Web 端链路无需任何凭证）。
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":5278"
	}
	if c.Data.Dir == "" {
		c.Data.Dir = "./data"
	}
	// 凭证默认取 Telegram Desktop 官方应用（与参考插件分发配置一致）：
	// 官方凭证登录时验证码通过 Telegram App 内服务消息投递，第三方自申请凭证
	// 只能走短信，国内手机号短信投递不可靠（表现为「验证码收不到」）。
	// config.yaml 里的 api_id/api_hash 可覆盖默认值。
	if c.Telegram.APIID == 0 {
		c.Telegram.APIID = 2040
	}
	if c.Telegram.APIHash == "" {
		c.Telegram.APIHash = "b18441a1ff607e10a989891a5462e627"
	}
	// transport 只认 tcp / ws，其余（含未配置）都按 auto 处理
	switch c.Telegram.Transport {
	case "tcp", "ws":
	default:
		c.Telegram.Transport = "auto"
	}
	if c.Stream.ChunkKB <= 0 {
		c.Stream.ChunkKB = 1024
	}
	if c.Stream.ChunkKB < 64 {
		c.Stream.ChunkKB = 64
	}
	if c.Stream.ChunkKB > 1024 {
		c.Stream.ChunkKB = 1024
	}
	if c.Stream.CacheMB <= 0 {
		c.Stream.CacheMB = 128
	}
	if c.Stream.CacheMB > 1024 {
		c.Stream.CacheMB = 1024
	}
	if c.Stream.DiskCacheMB < 0 {
		c.Stream.DiskCacheMB = 0
	}
	if c.Stream.DiskCacheMB > 51200 {
		c.Stream.DiskCacheMB = 51200
	}
	// 排行榜抓取：默认 12 小时一轮、单站点 25 秒超时
	if c.Rankings.IntervalHours <= 0 {
		c.Rankings.IntervalHours = 12
	}
	if c.Rankings.IntervalHours < 1 {
		c.Rankings.IntervalHours = 1
	}
	if c.Rankings.IntervalHours > 168 {
		c.Rankings.IntervalHours = 168
	}
	if c.Rankings.TimeoutSec <= 0 {
		c.Rankings.TimeoutSec = 25
	}
	if c.Rankings.TimeoutSec < 5 {
		c.Rankings.TimeoutSec = 5
	}
	if c.Rankings.TimeoutSec > 60 {
		c.Rankings.TimeoutSec = 60
	}
}

// DBPath 数据库文件路径
func (c *Config) DBPath() string { return filepath.Join(c.Data.Dir, "tgdrama.db") }

// SessionPath TG 会话文件路径
func (c *Config) SessionPath() string { return filepath.Join(c.Data.Dir, "session.json") }

// MediaDir 视频文件目录
func (c *Config) MediaDir() string { return filepath.Join(c.Data.Dir, "media") }

// CoverDir 封面目录
func (c *Config) CoverDir() string { return filepath.Join(c.Data.Dir, "covers") }

// PhotoDir 剧集预览图目录
func (c *Config) PhotoDir() string { return filepath.Join(c.Data.Dir, "photos") }

// CacheDir 图片缓存目录（没有真实封面时生成的占位图）
func (c *Config) CacheDir() string { return filepath.Join(c.Data.Dir, "cache") }

// RankingsFile 榜单快照文件（JSON），抓取结果落盘、重启后先读它
func (c *Config) RankingsFile() string { return filepath.Join(c.Data.Dir, "rankings.json") }

// EnsureDirs 创建运行所需的全部目录
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.Data.Dir, c.MediaDir(), c.CoverDir(), c.PhotoDir(), c.CacheDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", d, err)
		}
	}
	return nil
}
