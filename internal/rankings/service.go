package rankings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"tgdrama/internal/storage"
)

const (
	cacheFileName   = "rankings.json"
	snapshotVersion = 1

	// 抓取节奏由 interval 控制（默认 12 小时）。这个下限只在手动强制刷新时生效，
	// 避免用户连点把外部站点打烦。
	minAttemptGap = 5 * time.Minute
)

// ErrTooSoon 距离上次抓取还不够久
var ErrTooSoon = errors.New("榜单刚更新过，稍后再试")

// Service 榜单抓取 + 匹配服务
type Service struct {
	store     *storage.DB
	cachePath string
	interval  time.Duration
	timeout   time.Duration
	client    *http.Client
	sources   []*source
	now       func() time.Time

	mu   sync.RWMutex
	snap Snapshot

	refreshMu sync.Mutex

	matchMu      sync.Mutex
	matchVersion string
	matchIndex   *showIndex
	matchCache   map[string][]MatchedItem
}

// New 创建服务。enabledKeys 为空表示启用全部来源。
func New(store *storage.DB, dataDir string, interval, timeout time.Duration, enabledKeys []string) *Service {
	if interval < time.Hour {
		interval = time.Hour
	}
	if interval > 168*time.Hour {
		interval = 168 * time.Hour
	}
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	srcs := defaultSources()
	if len(enabledKeys) > 0 {
		want := make(map[string]bool, len(enabledKeys))
		for _, k := range enabledKeys {
			want[strings.ToLower(strings.TrimSpace(k))] = true
		}
		filtered := make([]*source, 0, len(srcs))
		for _, s := range srcs {
			if want[strings.ToLower(s.Key)] {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) > 0 {
			srcs = filtered
		}
	}
	return &Service{
		store:     store,
		cachePath: filepath.Join(dataDir, cacheFileName),
		interval:  interval,
		timeout:   timeout,
		client:    &http.Client{Timeout: timeout},
		sources:   srcs,
		now:       time.Now,
		snap:      Snapshot{Version: snapshotVersion},
	}
}

// SetProxy 让榜单抓取走代理，通常与 Telegram 共用同一条链路。
// 支持 socks5:// 与 http:// / https:// 三种写法；留空表示保持默认（直连）。
// 必须在 Start / Refresh 之前调用（构造期写一次，之后只读）。
func (s *Service) SetProxy(proxyURL string) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		log.Printf("[rankings] 代理地址无法解析，改为直连: %q", proxyURL)
		return
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		log.Printf("[rankings] 无法克隆默认 Transport，改为直连")
		return
	}
	tr := base.Clone()
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, derr := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if derr != nil {
			log.Printf("[rankings] SOCKS5 代理不可用，改为直连: %v", derr)
			return
		}
		cd, ok := dialer.(proxy.ContextDialer)
		if !ok {
			log.Printf("[rankings] 代理不支持按上下文拨号，改为直连")
			return
		}
		// SOCKS5 由代理端解析域名，正好绕开被污染的本机 DNS
		tr.Proxy = nil
		tr.DialContext = cd.DialContext
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	default:
		log.Printf("[rankings] 暂不支持的代理协议 %q，改为直连", u.Scheme)
		return
	}
	s.client = &http.Client{Timeout: s.timeout, Transport: tr}
	log.Printf("[rankings] 榜单抓取走代理 %s", u.Scheme+"://"+u.Host)
}

// CachePath 落盘路径（测试/运维排查用）
func (s *Service) CachePath() string { return s.cachePath }

// Load 从本地 JSON 读回上次结果；文件不存在或损坏都不算错误。
func (s *Service) Load() error {
	raw, err := os.ReadFile(s.cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			s.ensureBoards()
			return nil
		}
		return err
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		log.Printf("[rankings] 本地榜单缓存损坏，忽略: %v", err)
		s.ensureBoards()
		return nil
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	s.ensureBoards()
	return nil
}

// ensureBoards 保证每个来源在快照里都有一个 board（顺序与来源一致）。
func (s *Service) ensureBoards() {
	s.mu.Lock()
	defer s.mu.Unlock()
	byKey := make(map[string]Board, len(s.snap.Boards))
	for _, b := range s.snap.Boards {
		byKey[b.Key] = b
	}
	boards := make([]Board, 0, len(s.sources))
	for _, src := range s.sources {
		b, ok := byKey[src.Key]
		if !ok {
			b = Board{Key: src.Key, Name: src.Name, Site: src.Site, URL: src.URL}
		}
		b.Name, b.Site, b.URL = src.Name, src.Site, src.URL
		if b.Items == nil {
			b.Items = []Item{}
		}
		boards = append(boards, b)
	}
	s.snap.Boards = boards
	if s.snap.Version == 0 {
		s.snap.Version = snapshotVersion
	}
}

// Start 启动后台刷新：先读本地缓存，过期才抓；之后每 10 分钟检查一次。
func (s *Service) Start(ctx context.Context) {
	if err := s.Load(); err != nil {
		log.Printf("[rankings] 读取本地榜单失败: %v", err)
	}
	if s.needsRefresh() {
		go func() {
			if err := s.Refresh(ctx, false); err != nil && !errors.Is(err, ErrTooSoon) {
				log.Printf("[rankings] 首次抓取失败: %v", err)
			}
		}()
	}
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.needsRefresh() {
				continue
			}
			if err := s.Refresh(ctx, false); err != nil && !errors.Is(err, ErrTooSoon) {
				log.Printf("[rankings] 定时抓取失败: %v", err)
			}
		}
	}
}

// NeedsRefresh 是否到了下一次抓取时间
func (s *Service) NeedsRefresh() bool { return s.needsRefresh() }

func (s *Service) needsRefresh() bool {
	s.mu.RLock()
	attempted := s.snap.AttemptedAt
	s.mu.RUnlock()
	if attempted <= 0 {
		return true
	}
	return s.now().Unix()-attempted >= int64(s.interval/time.Second)
}

// Refresh 抓取全部来源。force=false 时严格遵守 interval（默认 12 小时一次）。
func (s *Service) Refresh(ctx context.Context, force bool) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	now := s.now().Unix()
	s.mu.RLock()
	attempted := s.snap.AttemptedAt
	s.mu.RUnlock()

	if !force {
		if attempted > 0 && now-attempted < int64(s.interval/time.Second) {
			return ErrTooSoon
		}
	} else if attempted > 0 && now-attempted < int64(minAttemptGap/time.Second) {
		return ErrTooSoon
	}

	// 先记下尝试时间并落盘：进程中途被杀也不会在重启后立刻再抓一轮
	s.mu.Lock()
	s.snap.AttemptedAt = now
	s.mu.Unlock()
	if err := s.save(); err != nil {
		log.Printf("[rankings] 保存抓取时间失败: %v", err)
	}

	type result struct {
		key   string
		items []Item
		err   error
	}
	results := make(chan result, len(s.sources))
	sem := make(chan struct{}, 4)
	for _, src := range s.sources {
		go func(src *source) {
			sem <- struct{}{}
			defer func() { <-sem }()
			items, err := s.fetchWithRetry(ctx, src)
			results <- result{key: src.Key, items: items, err: err}
		}(src)
	}

	got := make(map[string]result, len(s.sources))
	success := 0
	for i := 0; i < len(s.sources); i++ {
		r := <-results
		got[r.key] = r
		if r.err == nil && len(r.items) > 0 {
			success++
		}
	}

	s.mu.Lock()
	oldBoards := make(map[string]Board, len(s.snap.Boards))
	for _, b := range s.snap.Boards {
		oldBoards[b.Key] = b
	}
	boards := make([]Board, 0, len(s.sources))
	for _, src := range s.sources {
		b := Board{Key: src.Key, Name: src.Name, Site: src.Site, URL: src.URL}
		old := oldBoards[src.Key]
		r := got[src.Key]
		if r.err != nil || len(r.items) == 0 {
			// 抓失败：保留上一次的条目（前端仍然能看），只标记异常
			b.Items = old.Items
			b.FetchedAt = old.FetchedAt
			b.OK = false
			if r.err != nil {
				b.Error = r.err.Error()
			} else {
				b.Error = "没有解析到条目"
			}
			if b.Items == nil {
				b.Items = []Item{}
			}
			log.Printf("[rankings] %s 抓取失败: %s", src.Name, b.Error)
		} else {
			b.Items = r.items
			b.FetchedAt = now
			b.OK = true
		}
		boards = append(boards, b)
	}
	s.snap.Boards = boards
	s.snap.Version = snapshotVersion
	if success > 0 {
		s.snap.UpdatedAt = now
	}
	s.mu.Unlock()

	// 榜单变了，匹配缓存作废
	s.matchMu.Lock()
	s.matchCache = nil
	s.matchMu.Unlock()

	if err := s.save(); err != nil {
		return fmt.Errorf("保存榜单: %w", err)
	}
	log.Printf("[rankings] 抓取完成：%d/%d 个榜单成功", success, len(s.sources))
	return nil
}

func (s *Service) fetchWithRetry(ctx context.Context, src *source) ([]Item, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(700 * time.Millisecond):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, s.timeout)
		items, err := src.Fetch(cctx, s.client)
		cancel()
		if err == nil && len(items) > 0 {
			return items, nil
		}
		if err == nil {
			err = errors.New("没有解析到条目")
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *Service) save() error {
	s.mu.RLock()
	raw, err := json.MarshalIndent(s.snap, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.cachePath), 0o755); err != nil {
		return err
	}
	tmp := s.cachePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.cachePath)
}

// Snapshot 返回快照副本（测试用）
func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.snap
	out.Boards = append([]Board(nil), s.snap.Boards...)
	return out
}

// Payload 组装下发给前端的数据：榜单 + 匹配到的本地可播短剧。
func (s *Service) Payload() Payload {
	snap := s.Snapshot()
	out := Payload{
		UpdatedAt:     snap.UpdatedAt,
		AttemptedAt:   snap.AttemptedAt,
		IntervalHours: int(s.interval / time.Hour),
		Boards:        make([]BoardView, 0, len(snap.Boards)),
	}
	if snap.AttemptedAt > 0 {
		out.NextUpdateAt = snap.AttemptedAt + int64(s.interval/time.Second)
	}

	idx := s.showIndex()
	// 同一版本 + 同一份榜单结果，匹配结果直接复用
	s.matchMu.Lock()
	if s.matchCache == nil {
		s.matchCache = make(map[string][]MatchedItem, len(snap.Boards))
	}
	s.matchMu.Unlock()

	for _, b := range snap.Boards {
		key := fmt.Sprintf("%s@%d", b.Key, b.FetchedAt)
		s.matchMu.Lock()
		items, ok := s.matchCache[key]
		s.matchMu.Unlock()
		if !ok {
			items = matchBoard(b.Items, idx)
			s.matchMu.Lock()
			s.matchCache[key] = items
			s.matchMu.Unlock()
		}
		if items == nil {
			items = []MatchedItem{}
		}
		out.Boards = append(out.Boards, BoardView{
			Key:       b.Key,
			Name:      b.Name,
			Site:      b.Site,
			URL:       b.URL,
			OK:        b.OK,
			FetchedAt: b.FetchedAt,
			Error:     b.Error,
			Total:     len(b.Items),
			Matched:   len(items),
			Items:     items,
		})
	}
	return out
}

// showIndex 按媒体库版本缓存匹配索引；库没变就不重复查库。
func (s *Service) showIndex() *showIndex {
	if s.store == nil {
		return buildShowIndex(nil)
	}
	version := s.store.VersionTag()
	s.matchMu.Lock()
	defer s.matchMu.Unlock()
	if s.matchVersion == version && s.matchIndex != nil {
		return s.matchIndex
	}
	shows, err := s.store.ListPlayableShows()
	if err != nil {
		log.Printf("[rankings] 读取本地片库失败: %v", err)
		return buildShowIndex(nil)
	}
	s.matchIndex = buildShowIndex(shows)
	s.matchVersion = version
	s.matchCache = nil
	return s.matchIndex
}
