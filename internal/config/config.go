package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram TelegramConfig `yaml:"telegram"`
	Server   ServerConfig   `yaml:"server"`
	Data     DataConfig     `yaml:"data"`
	Stream   StreamConfig   `yaml:"stream"`
}

type TelegramConfig struct {
	APIID int `yaml:"api_id"`

	APIHash string `yaml:"api_hash"`

	Proxy string `yaml:"proxy"`

	Transport string `yaml:"transport"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
}

type DataConfig struct {
	Dir string `yaml:"dir"`
}

type StreamConfig struct {
	ChunkKB int `yaml:"chunk_kb"`

	CacheMB int `yaml:"cache_mb"`

	DiskCacheMB int `yaml:"disk_cache_mb"`
}

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

	if c.Telegram.APIID == 0 {
		c.Telegram.APIID = 2040
	}
	if c.Telegram.APIHash == "" {
		c.Telegram.APIHash = "b18441a1ff607e10a989891a5462e627"
	}

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
}

func (c *Config) DBPath() string { return filepath.Join(c.Data.Dir, "tgdrama.db") }

func (c *Config) SessionPath() string { return filepath.Join(c.Data.Dir, "session.json") }

func (c *Config) MediaDir() string { return filepath.Join(c.Data.Dir, "media") }

func (c *Config) CoverDir() string { return filepath.Join(c.Data.Dir, "covers") }

func (c *Config) PhotoDir() string { return filepath.Join(c.Data.Dir, "photos") }

func (c *Config) CacheDir() string { return filepath.Join(c.Data.Dir, "cache") }

func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.Data.Dir, c.MediaDir(), c.CoverDir(), c.PhotoDir(), c.CacheDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", d, err)
		}
	}
	return nil
}
