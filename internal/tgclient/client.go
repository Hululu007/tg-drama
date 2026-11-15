// Package tgclient 封装 Telegram 客户端：账号登录、历史回填、更新监听与媒体入库
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

// 登录阶段常量
const (
	phaseNone         = "none"
	phaseConnecting   = "connecting"
	phaseWaitingQR    = "waiting_qr"
	phaseNeedPhone    = "need_phone"    // 账号登录：等待输入手机号
	phaseSendingCode  = "sending_code"  // 账号登录：正在请求发送验证码
	phaseNeedCode     = "need_code"     // 账号登录：等待输入验证码
	phaseNeedPassword = "need_password" // 两步验证：等待输入密码
	phaseAuthorized   = "authorized"
	phaseError        = "error"
)

// 设备上报信息与 Telegram Desktop 对齐（凭证同为官方桌面应用）。
// 默认值会把 Go 运行时版本当成设备型号上报，反而比任何请求头都更容易被识别。
const (
	desktopDeviceModel = "Desktop"
	desktopSystemVer   = "Windows 10"
	desktopAppVer      = "5.16.3"
	desktopLangCode    = "zh-CN"
)

// WebSocket 握手伪装：主连接走 wss://*.web.telegram.org/apiws（官方 Web 端的
// 传输端点，443 域名不被封锁，直连 DC IP 在国内普遍不通），握手请求按浏览器
// 同款头部发送，避免出现 "Go-http-client" 这种一眼可辨的特征。
// 这只是传输管道的伪装，与登录凭证（api_id）无关。
const (
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	webOrigin  = "https://web.telegram.org"
	webReferer = "https://web.telegram.org/k/"
)

// browserHeaders 返回 WebSocket 握手用的浏览器请求头。
// coder/websocket 只会覆盖 Connection/Upgrade/Sec-WebSocket-Key 等协议必需项，
// 其余自定义头原样带到握手请求上。
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

// Service Telegram 客户端服务
type Service struct {
	cfg      *config.Config
	db       *storage.DB
	parser   *parser.Parser
	albums   *parser.AlbumResolver
	queue    *download.Queue
	client  *telegram.Client
	opts    telegram.Options // 连接参数，断线重连时用它重建客户端
	apiID   int              // 实际生效的 app id（配置提供，默认对齐 Telegram Desktop）
	apiHash string           // 实际生效的 app hash，扫码/手机号登录必须与之保持一致
	loggedIn qrlogin.LoggedIn // 手机端确认扫码后 TG 推送的 updateLoginToken 信号

	mu         sync.Mutex
	api        *tg.Client
	authorized bool

	authMu  sync.Mutex
	phase   string
	message string // 最近一次连接/登录失败原因，供前端展示
	qrToken qrlogin.Token
	pwChan  chan string

	// 登录方式与账号登录的输入通道：
	// loginMode 由前端按设备（PC 优先扫码 / 手机优先账号）设置；
	// authCancel 用于在中途切换登录方式时取消正在等待的流程。
	loginMode  string
	authCancel context.CancelFunc
	phoneChan  chan string
	codeChan   chan string
	syncing    int32

	// 退出登录：connCancel 取消当前连接让 Run 循环重建客户端；
	// restartReq 置 1 表示这次重建是退出登录触发的，跳过退避立即重连。
	connCancel context.CancelFunc
	restartReq int32
	// keyDead 置 1 表示当前 auth key 已被服务端注销（AUTH_KEY_UNREGISTERED），
	// 只在处理一次清理动作时用；登录成功后复位。
	keyDead int32
	// notice 是一条要一直显示在登录层上的提示（例如「会话已被 Telegram 注销」），
	// 登录成功后自动清空，避免用户只看到二维码却不知道上一会话为什么失效。
	notice string

	// 同步期内存缓存：剧名索引避免每条消息都查库，albumMu 保护相册集号状态
	// （多频道并发同步时 AlbumResolver 会被多协程同时访问）。
	normMu    sync.Mutex
	normNames map[string]string
	normIDs   map[string]int64
	albumMu   sync.Mutex
	// dialogsMu 防止「打开频道管理」连点或与启动同步同时拉对话列表，
	// 同一时刻只允许一次 MessagesGetDialogs 全量翻页。
	dialogsMu sync.Mutex

	// 后台媒体下载队列（封面/预览图），避免同步被逐张图片的网络往返串行拖慢
	mediaCh      chan mediaJob
	mediaMu      sync.Mutex
	coverPending map[int64]bool
	// 预览图预热：showWarm 是按剧的冷却表；warmSem 限制同时进行的预热批次，
	// floodUntil 是收到 FLOOD_WAIT 后的全局静默期（期间不再发任何预热请求）。
	showWarm   map[int64]time.Time
	warmSem    chan struct{}
	floodUntil time.Time
	// playbackStreams 当前活跃的播放流数（web 层计数）：>0 时后台封面/预览图
	// 下载全部退让，把带宽与限流额度让给播放取流。
	playbackStreams int

	// 进程级后台上下文：媒体协程池只在第一次连接成功后启动一次。
	// 早先是在 runOnce 里每次重连都 go 一批，断线重连几次就会有 8×N 个下载协程
	// 同时抢同一个队列，既浪费带宽也更容易被限流。
	bgCtx     context.Context
	bgCancel  context.CancelFunc
	mediaOnce sync.Once

	// 封面按需回填：coverTry 记每部剧上次尝试时间（失败也记，避免对没有缩略图
	// 的剧反复请求），coverSem 限制同时进行的封面拉取数。
	coverMu      sync.Mutex
	coverRunning bool
	coverTry     map[int64]time.Time
	coverSem     chan struct{}

	// 全局请求节流：所有非文件流的 RPC 共用 rpcGate（翻页/取消息），
	// 封面与预热各有自己的闸门，避免几条路径叠加后把账号打进 FLOOD_WAIT。
	// 播放取块（upload.getFile）不经过这里——它天然按播放进度匀速。
	rpcGate  pacer
	warmGate pacer
	dlGate   pacer

	// 推荐面板「频道是否已加入」的短缓存：那个接口每次点击都要问一次 TG，
	// 1 分钟内复用上一次结果，避免反复点推荐把请求打出去。
	joinedMu   sync.Mutex
	joinedAt   time.Time
	joinedVals map[int64]bool

	// 详情页发现选集不连贯时按 #剧名 全账号搜索补全，记录上次搜索时间做冷却
	fillMu sync.Mutex
	fillAt map[int64]time.Time

	// 频道管理面板启用/立即同步某个频道时，同一频道避免并发跑两份历史回填
	syncChatMu sync.Mutex
	syncChats  map[int64]bool
}

// New 创建客户端服务。接入方式为标准 MTProto：TCP 直连 DC（可经代理），
// 凭证由配置提供（默认对齐 Telegram Desktop 官方应用，见 config.applyDefaults）。
// 官方凭证的登录验证码走 Telegram App 内服务消息投递，第三方凭证只能走短信，
// 国内手机号短信投递不可靠——「验证码收不到」优先检查凭证。
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
		// 会话被服务端注销（AUTH_KEY_UNREGISTERED 等）时立刻自愈：
		// 否则客户端会停在 authorized=true，所有 RPC 都 401，界面只显示「刷新失败」。
		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(s.sessionGuard)},
		// 网络不可达时必须让连接尽快失败，否则整个登录流程会静默卡在“连接中”
		DialTimeout:     15 * time.Second,
		ExchangeTimeout: 15 * time.Second,
		// 设备信息与 Telegram Desktop 保持一致：默认值会把 Go 运行时版本当成
		// 设备型号上报，反而比任何请求头都更容易被识别出来。
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

// newClient 按保存的连接参数新建客户端：gotd 的 Client 关闭后不可复用，
// 所以每次重连都要重建一个。
func (s *Service) newClient() *telegram.Client {
	return telegram.NewClient(s.apiID, s.apiHash, s.opts)
}

// Queue 暴露下载队列
func (s *Service) Queue() *download.Queue { return s.queue }

// API 返回当前 RPC 客户端，未登录时为 nil
func (s *Service) API() *tg.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.api
}

// SetAPIForTest 注入假 API 客户端（仅测试使用）：未连接 Telegram 时
// 让刷新 file_reference / 播放取块等路径可以走假 Invoker 完成闭环测试。
func (s *Service) SetAPIForTest(api *tg.Client) {
	s.mu.Lock()
	s.api = api
	s.authorized = true
	s.mu.Unlock()
}

// IsAuthorized 是否已登录
func (s *Service) IsAuthorized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorized
}

// AuthPhase 当前登录阶段
func (s *Service) AuthPhase() string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.phase
}

func (s *Service) setPhase(p string) {
	s.authMu.Lock()
	s.phase = p
	// 连接中保留上一次的失败原因，避免界面在“连接中”和错误之间闪跳；
	// 只有真正进入等待用户输入/已登录状态才认为错误已消除。
	if p == phaseWaitingQR || p == phaseNeedPhone || p == phaseNeedCode ||
		p == phaseSendingCode || p == phaseAuthorized || p == phaseNone {
		s.message = ""
	}
	if p == phaseAuthorized {
		// 登录成功：上一会话被注销的提示到此为止
		s.notice = ""
	}
	s.authMu.Unlock()
}

// AuthMessage 返回最近一次连接失败的原因，正常时为空字符串。
// notice（会话被注销等）优先：它比短暂的连接错误更值得让用户看到。
func (s *Service) AuthMessage() string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.notice != "" {
		return s.notice
	}
	return s.message
}

// fail 记录失败原因，把状态切到 error，前端会展示 message 并继续重试
func (s *Service) setError(err error) {
	if err == nil {
		return
	}
	s.authMu.Lock()
	s.phase = phaseError
	s.message = err.Error()
	s.authMu.Unlock()
}

// sleepCtx 可中断的等待：false 表示 ctx 已取消
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

// setConnCancel 记录当前连接的取消函数（退出登录时用它中断连接）
func (s *Service) setConnCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	s.connCancel = cancel
	s.mu.Unlock()
}

// Logout 退出登录：注销服务端授权、删除本地会话文件，然后以未登录状态重连。
// 已登录的会话文件被清除，重连后进入登录等待流程（扫码或手机号）。
func (s *Service) Logout(ctx context.Context) error {
	if api := s.API(); api != nil && s.IsAuthorized() {
		// 注销服务端授权：尽力而为，失败（如已断线）也继续清理本地状态
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
	// 断开当前连接：Run 循环看到 restartReq 后跳过退避，立即以未登录状态重连
	atomic.StoreInt32(&s.restartReq, 1)
	s.mu.Lock()
	cancel := s.connCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.setPhase(phaseNone)
	s.setNotice("") // 主动退出登录不需要保留「上一会话被注销」的提示
	log.Printf("已退出登录，正在重新连接")
	return nil
}

// Run 启动客户端并阻塞直到 ctx 结束；连接失败时自动退避重连，
// 这样 Android 等网络切换场景无需重启宿主。
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
				// gotd 在连不上服务器时会一直静默重连，回调永远不会触发，
				// 状态就会一直停在“连接中”。这里主动升级成可见错误，
				// 前端据此提示用户检查网络/代理，同时后台继续尝试。
				if s.AuthPhase() == phaseConnecting {
					s.setError(fmt.Errorf("连接 Telegram 超时：请检查网络，或确认代理/VPN 可用"))
				}
			}
		}
		connCancel()
		if ctx.Err() != nil {
			return nil
		}
		// 退出登录触发的重建：跳过退避立即重连，并把退避基线复位
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
		// 稳定运行过一段时间后重置退避，避免长时间在线后一次断线就等 30 秒
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

// runOnce 执行一次完整的连接与登录流程
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
			// 扫码 / 账号登录按前端设置的偏好自动选择；中途切换会在这里续上
			if err := s.authLoop(ctx, api); err != nil {
				s.setError(err)
				return err
			}
		}
		s.mu.Lock()
		s.authorized = true
		s.mu.Unlock()
		// 新会话已建立：复位「会话被注销」标记，下次再被注销时能重新自愈
		atomic.StoreInt32(&s.keyDead, 0)
		s.setPhase(phaseAuthorized)

		// 媒体协程池与下载队列只在第一次连上时启动：它们自己按任务调用
		// s.API() 取当前连接，断线重连后无需重建，也就不会攒出多批协程。
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

// proxyDialer 根据代理 URL 构造 SOCKS5 等拨号器
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

// plainResolver 原生 TCP DC Resolver，可经代理。
// DC 地址使用 gotd 内置的 IP 列表，Android 纯 Go 二进制没有 cgo 解析器、
// 系统 DNS 不可用也不会影响连接（不依赖域名解析）。
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

// webResolver 主 DC 优先走 Telegram Web 的 WebSocket 传输（443 端口，
// web.telegram.org 域名直连不被封锁；DC 裸 IP 在国内普遍不通），
// 失败时回退到 DC IP 的普通 TCP 传输。Android 的纯 Go 二进制没有 cgo
// 解析器，系统 DNS（netd）不可用，WebSocket 的域名解析必然失败，
// 因此 Android 上直接优先用 IP 直连。
// 媒体/CDN DC 只能走 TCP：dcs.Websocket 本身不支持这两类连接。
type webResolver struct {
	ws          dcs.Resolver
	plain       dcs.Resolver
	preferPlain bool
	// wsOnly 只走 WebSocket，不回退 TCP（transport: ws）
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

// buildWebResolver 构造主连接链路：默认优先 wss://*.web.telegram.org/apiws，
// 失败回退 DC IP 的 TCP 直连；媒体/CDN 恒走 TCP。
// transport 见 config.TelegramConfig.Transport：tcp 时改为优先 IP 直连
// （有代理的机器上直连可达，握手不再带浏览器指纹）；ws 时只用 WebSocket。
func buildWebResolver(proxyURL, transport string) (dcs.Resolver, error) {
	dialOpts := &websocket.DialOptions{
		Subprotocols: []string{"binary"},
		// 握手请求按浏览器发：UA/Origin/Referer 都指向 web.telegram.org，
		// 避免出现 "Go-http-client" 这种一眼可辨的客户端特征
		HTTPHeader: browserHeaders(),
	}
	// 默认也给 WebSocket 拨号加上超时：网络被墙时 TCP/TLS 会静默挂起，
	// 没有超时的话登录状态查询永远不返回，前端只能看到一直“转圈”。
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

// markSyncing 防止历史同步重复执行
func (s *Service) markSyncing() bool {
	return atomic.CompareAndSwapInt32(&s.syncing, 0, 1)
}
