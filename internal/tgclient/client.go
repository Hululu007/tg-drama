package tgclient

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"
	xproxy "golang.org/x/net/proxy"

	"tgdrama/internal/config"
	"tgdrama/internal/download"
	"tgdrama/internal/parser"
	"tgdrama/internal/storage"
)

const (
	phaseNone         = "none"
	phaseConnecting   = "connecting"
	phaseWaitingQR    = "waiting_qr"
	phaseNeedPhone    = "need_phone"
	phaseSendingCode  = "sending_code"
	phaseNeedCode     = "need_code"
	phaseNeedPassword = "need_password"
	phaseAuthorized   = "authorized"
	phaseError        = "error"
)

const (
	desktopDeviceModel = "Desktop"
	desktopSystemVer   = "Windows 10"
	desktopAppVer      = "5.16.3"
	desktopLangCode    = "zh-CN"
)

const (
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	webOrigin  = "https://web.telegram.org"
	webReferer = "https://web.telegram.org/k/"
)

func browserHeaders() http.Header {
	return http.Header{
		"User-Agent":      {webUserAgent},
		"Origin":          {webOrigin},
		"Referer":         {webReferer},
		"Accept-Language": {"zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7"},
		"Accept-Encoding": {"gzip, deflate"},
		"Cache-Control":   {"no-cache"},
		"Pragma":          {"no-cache"},
	}
}

type Service struct {
	cfg      *config.Config
	db       *storage.DB
	parser   *parser.Parser
	albums   *parser.AlbumResolver
	queue    *download.Queue
	client   *telegram.Client
	opts     telegram.Options
	apiID    int
	apiHash  string
	loggedIn qrlogin.LoggedIn

	mu         sync.Mutex
	api        *tg.Client
	authorized bool

	authMu  sync.Mutex
	phase   string
	message string
	qrToken qrlogin.Token
	pwChan  chan string

	loginMode  string
	authCancel context.CancelFunc
	phoneChan  chan string
	codeChan   chan string
	syncing    int32

	connCancel context.CancelFunc
	restartReq int32

	keyDead int32

	notice string

	normMu    sync.Mutex
	normNames map[string]string
	normIDs   map[string]int64
	albumMu   sync.Mutex

	dialogsMu sync.Mutex

	mediaCh      chan mediaJob
	mediaMu      sync.Mutex
	coverPending map[int64]bool

	showWarm   map[int64]time.Time
	warmSem    chan struct{}
	floodUntil time.Time

	playbackStreams int

	bgCtx     context.Context
	bgCancel  context.CancelFunc
	mediaOnce sync.Once

	coverMu      sync.Mutex
	coverRunning bool
	coverTry     map[int64]time.Time
	coverSem     chan struct{}

	rpcGate  pacer
	warmGate pacer
	dlGate   pacer

	joinedMu   sync.Mutex
	joinedAt   time.Time
	joinedVals map[int64]bool

	fillMu sync.Mutex
	fillAt map[int64]time.Time

	syncChatMu sync.Mutex
	syncChats  map[int64]bool
}

func New(cfg *config.Config, db *storage.DB) (*Service, error) {
	if cfg.Telegram.APIID <= 0 || cfg.Telegram.APIHash == "" {
		return nil, fmt.Errorf("缺少 Telegram 凭证：请在 config.yaml 配置 telegram.api_id / telegram.api_hash" +
			"（my.telegram.org 申请，或使用默认内置凭证）")
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	s := &Service{
		cfg:     cfg,
		db:      db,
		parser:  parser.New(),
		albums:  parser.NewAlbumResolver(),
		phase:   phaseNone,
		apiID:   cfg.Telegram.APIID,
		apiHash: cfg.Telegram.APIHash,

		phoneChan: make(chan string, 1),
		codeChan:  make(chan string, 1),

		loginMode:    ModeQR,
		mediaCh:      make(chan mediaJob, mediaQueueSize),
		coverPending: map[int64]bool{},
		showWarm:     map[int64]time.Time{},
		warmSem:      make(chan struct{}, warmConcurrency),
		fillAt:       map[int64]time.Time{},
		syncChats:    map[int64]bool{},
		bgCtx:        bgCtx,
		bgCancel:     bgCancel,
		coverTry:     map[int64]time.Time{},
		coverSem:     make(chan struct{}, coverConcurrency),
		joinedVals:   map[int64]bool{},
	}
	s.queue = download.NewQueue(db, cfg)
	s.restoreLoginMode()

	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewMessage(s.handleNewMessage)
	dispatcher.OnNewChannelMessage(s.handleNewChannelMessage)
	s.loggedIn = qrlogin.OnLoginToken(&dispatcher)

	opts := telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionPath()},
		UpdateHandler:  &dispatcher,

		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(s.sessionGuard)},

		DialTimeout:     15 * time.Second,
		ExchangeTimeout: 15 * time.Second,

		Device: telegram.DeviceConfig{
			DeviceModel:    desktopDeviceModel,
			SystemVersion:  desktopSystemVer,
			AppVersion:     desktopAppVer,
			SystemLangCode: desktopLangCode,
			LangCode:       desktopLangCode,
		},
	}
	resolver, err := buildWebResolver(cfg.Telegram.Proxy, cfg.Telegram.Transport)
	if err != nil {
		return nil, err
	}
	opts.Resolver = resolver
	s.opts = opts
	s.client = s.newClient()
	return s, nil
}

func (s *Service) newClient() *telegram.Client {
	return telegram.NewClient(s.apiID, s.apiHash, s.opts)
}

func (s *Service) Queue() *download.Queue { return s.queue }

func (s *Service) API() *tg.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.api
}

func (s *Service) SetAPIForTest(api *tg.Client) {
	s.mu.Lock()
	s.api = api
	s.authorized = true
	s.mu.Unlock()
}

func (s *Service) IsAuthorized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorized
}

func (s *Service) AuthPhase() string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.phase
}

func (s *Service) setPhase(p string) {
	s.authMu.Lock()
	s.phase = p

	if p == phaseWaitingQR || p == phaseNeedPhone || p == phaseNeedCode ||
		p == phaseSendingCode || p == phaseAuthorized || p == phaseNone {
		s.message = ""
	}
	if p == phaseAuthorized {

		s.notice = ""
	}
	s.authMu.Unlock()
}

func (s *Service) AuthMessage() string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.notice != "" {
		return s.notice
	}
	return s.message
}

func (s *Service) setError(err error) {
	if err == nil {
		return
	}
	s.authMu.Lock()
	s.phase = phaseError
	s.message = err.Error()
	s.authMu.Unlock()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *Service) setConnCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	s.connCancel = cancel
	s.mu.Unlock()
}

func (s *Service) Logout(ctx context.Context) error {
	if api := s.API(); api != nil && s.IsAuthorized() {

		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := api.AuthLogOut(lctx); err != nil {
			log.Printf("注销服务端授权失败（继续清理本地会话）: %v", err)
		}
	}
	if err := os.Remove(s.cfg.SessionPath()); err != nil && !os.IsNotExist(err) {
		log.Printf("删除本地会话文件失败: %v", err)
	}
	s.mu.Lock()
	s.authorized = false
	s.api = nil
	s.mu.Unlock()

	atomic.StoreInt32(&s.restartReq, 1)
	s.mu.Lock()
	cancel := s.connCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.setPhase(phaseNone)
	s.setNotice("")
	log.Printf("已退出登录，正在重新连接")
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	backoff := 3 * time.Second
	for {
		s.setPhase(phaseConnecting)
		connCtx, connCancel := context.WithCancel(ctx)
		s.setConnCancel(connCancel)
		started := time.Now()
		done := make(chan error, 1)
		go func() {
			done <- s.runOnce(connCtx)
		}()
		var err error
	wait:
		for {
			select {
			case <-ctx.Done():
				connCancel()
				return nil
			case err = <-done:
				break wait
			case <-time.After(25 * time.Second):

				if s.AuthPhase() == phaseConnecting {
					s.setError(fmt.Errorf("连接 Telegram 超时：请检查网络，或确认代理/VPN 可用"))
				}
			}
		}
		connCancel()
		if ctx.Err() != nil {
			return nil
		}

		if atomic.SwapInt32(&s.restartReq, 0) == 1 {
			log.Printf("以未登录状态重新连接 Telegram")
			backoff = 3 * time.Second
			s.client = s.newClient()
			continue
		}
		if err != nil {
			log.Printf("Telegram 连接中断，%s 后重试: %v", backoff, err)
		} else {
			log.Printf("Telegram 客户端已退出，%s 后重连", backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		if time.Since(started) > 30*time.Second {
			backoff = 3 * time.Second
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		s.client = s.newClient()
	}
}

func (s *Service) runOnce(ctx context.Context) error {
	return s.client.Run(ctx, func(ctx context.Context) error {
		api := s.client.API()
		s.mu.Lock()
		s.api = api
		s.mu.Unlock()

		status, err := s.client.Auth().Status(ctx)
		if err != nil {
			err = fmt.Errorf("连接 Telegram 失败: %w", err)
			s.setError(err)
			return err
		}
		if !status.Authorized {

			if err := s.authLoop(ctx, api); err != nil {
				s.setError(err)
				return err
			}
		}
		s.mu.Lock()
		s.authorized = true
		s.mu.Unlock()

		atomic.StoreInt32(&s.keyDead, 0)
		s.setPhase(phaseAuthorized)

		s.mediaOnce.Do(func() {
			go s.queue.Run(s.bgCtx)
			go s.startMediaWorkers(s.bgCtx)
		})
		go s.startHistorySync(ctx, api)
		go s.BackfillCovers(ctx, api)

		<-ctx.Done()
		return ctx.Err()
	})
}

func proxyDialer(proxyURL string) (xproxy.Dialer, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("解析代理地址失败: %w", err)
	}
	dialer, err := xproxy.FromURL(u, xproxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("创建代理拨号器失败: %w", err)
	}
	return dialer, nil
}

func plainResolver(proxyURL string) (dcs.Resolver, error) {
	opts := dcs.PlainOptions{}
	if proxyURL != "" {
		dialer, err := proxyDialer(proxyURL)
		if err != nil {
			return nil, err
		}
		opts.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	}
	return dcs.Plain(opts), nil
}

type webResolver struct {
	ws          dcs.Resolver
	plain       dcs.Resolver
	preferPlain bool

	wsOnly bool
}

var _ dcs.Resolver = webResolver{}

func (r webResolver) Primary(ctx context.Context, dc int, list dcs.List) (transport.Conn, error) {
	first, second := r.ws, r.plain
	if r.preferPlain {
		first, second = r.plain, r.ws
	}
	conn, err := first.Primary(ctx, dc, list)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil || r.wsOnly {
		return nil, err
	}
	conn2, err2 := second.Primary(ctx, dc, list)
	if err2 != nil {
		return nil, fmt.Errorf("WebSocket 直连失败: %v；TCP 直连失败: %w", err, err2)
	}
	return conn2, nil
}

func (r webResolver) MediaOnly(ctx context.Context, dc int, list dcs.List) (transport.Conn, error) {
	return r.plain.MediaOnly(ctx, dc, list)
}

func (r webResolver) CDN(ctx context.Context, dc int, list dcs.List) (transport.Conn, error) {
	return r.plain.CDN(ctx, dc, list)
}

func buildWebResolver(proxyURL, transport string) (dcs.Resolver, error) {
	dialOpts := &websocket.DialOptions{
		Subprotocols: []string{"binary"},

		HTTPHeader: browserHeaders(),
	}

	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 12 * time.Second,
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析代理地址失败: %w", err)
		}
		switch u.Scheme {
		case "http", "https":
			tr.Proxy = http.ProxyURL(u)
		default:
			dialer, err := proxyDialer(proxyURL)
			if err != nil {
				return nil, err
			}
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	}
	dialOpts.HTTPClient = &http.Client{Transport: tr, Timeout: 20 * time.Second}

	plain, err := plainResolver(proxyURL)
	if err != nil {
		return nil, err
	}
	preferPlain := transport == "tcp" || runtime.GOOS == "android" || runtime.GOOS == "ios"
	return webResolver{
		ws:          dcs.Websocket(dcs.WebsocketOptions{DialOptions: dialOpts}),
		plain:       plain,
		preferPlain: preferPlain,
		wsOnly:      transport == "ws",
	}, nil
}

func (s *Service) markSyncing() bool {
	return atomic.CompareAndSwapInt32(&s.syncing, 0, 1)
}
