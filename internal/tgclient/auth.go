package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// 登录方式常量。
// PC 端优先扫码（手机扫屏幕最省事）；移动端优先「账号登录」——手机上没法从相册
// 选取二维码给 Telegram 扫，只能输入手机号 + 验证码。
const (
	ModeQR    = "qr"
	ModePhone = "phone"

	// kvAuthMode 记住上次选择的登录方式：进程重启（如 Android 端被杀）后
	// 仍按用户偏好等待手机号，而不是又生成一个没人扫的二维码。
	kvAuthMode = "auth_mode"
)

// LoginMode 返回当前偏好的登录方式
func (s *Service) LoginMode() string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.loginMode == ModePhone {
		return ModePhone
	}
	return ModeQR
}

// SetLoginMode 切换登录方式。未登录且正在等另一种方式时，立即取消当前流程，
// authLoop 会用新的方式重新开始（不会触发断线重连）。
func (s *Service) SetLoginMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != ModeQR && mode != ModePhone {
		return errors.New("不支持的登录方式")
	}
	s.authMu.Lock()
	changed := s.loginMode != mode
	s.loginMode = mode
	cancel := s.authCancel
	if changed {
		// 切换方式后旧的错误提示没有意义，避免界面残留上一种方式的报错
		s.message = ""
		if mode == ModePhone {
			// 账号登录不需要二维码，清掉旧 token 防止前端误显示
			s.qrToken = qrlogin.Token{}
		}
	}
	s.authMu.Unlock()
	if changed && cancel != nil {
		cancel()
	}
	if changed && s.db != nil {
		_ = s.db.SetKV(kvAuthMode, mode)
	}
	return nil
}

// restoreLoginMode 启动时恢复上次选择的登录方式（读不到就保持默认扫码）
func (s *Service) restoreLoginMode() {
	if s.db == nil {
		return
	}
	if mode, ok, err := s.db.GetKV(kvAuthMode); err == nil && ok && mode == ModePhone {
		s.authMu.Lock()
		s.loginMode = ModePhone
		s.authMu.Unlock()
	}
}

// setAuthCancel 记录当前登录流程的取消函数（断线重连时用于取消等待输入的流程）
func (s *Service) setAuthCancel(cancel context.CancelFunc) {
	s.authMu.Lock()
	s.authCancel = cancel
	s.authMu.Unlock()
}

func (s *Service) clearAuthCancel() {
	s.authMu.Lock()
	s.authCancel = nil
	s.authMu.Unlock()
}

// setPhaseMessage 同时更新阶段与提示文案（用于「停留在当前步骤并说明原因」）
func (s *Service) setPhaseMessage(p, msg string) {
	s.authMu.Lock()
	s.phase = p
	s.message = msg
	s.authMu.Unlock()
}

// setNotice 记录一条持续显示的提示（例如「上一会话被注销」）。它不会像
// message 那样被阶段切换清掉，登录成功后由 setPhase(phaseAuthorized) 复位。
func (s *Service) setNotice(msg string) {
	s.authMu.Lock()
	s.notice = msg
	s.authMu.Unlock()
}

// setToken 记录当前二维码登录 token（过期时 qr.Auth 会自动重新导出并再次回调）
func (s *Service) setToken(t qrlogin.Token) {
	s.authMu.Lock()
	s.qrToken = t
	s.authMu.Unlock()
}

// QRInfo 返回二维码登录 URL 与过期时间
func (s *Service) QRInfo() (loginURL string, expires time.Time, ok bool) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.qrToken.Empty() {
		return "", time.Time{}, false
	}
	return s.qrToken.URL(), s.qrToken.Expires(), true
}

// authLoop 按当前偏好的方式执行登录；用户中途切换方式时换一种继续。
func (s *Service) authLoop(ctx context.Context, api *tg.Client) error {
	for {
		mode := s.LoginMode()
		actx, cancel := context.WithCancel(ctx)
		s.setAuthCancel(cancel)

		var err error
		if mode == ModePhone {
			s.setPhase(phaseNeedPhone)
			log.Printf("开始 Telegram 账号登录（手机号 + 验证码）")
			err = s.phoneLoginFlow(actx)
		} else {
			s.setPhase(phaseWaitingQR)
			err = s.qrLoginFlow(actx, api)
		}

		s.clearAuthCancel()
		cancel()

		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if actx.Err() != nil {
			// 当前流程是被 SetLoginMode 取消的，换一种方式重来
			log.Printf("登录方式已切换为 %s，重新开始登录", s.LoginMode())
			continue
		}
		return err
	}
}

// qrLoginFlow 执行扫码登录：导出二维码 token，等手机端确认后导入授权，支持 2FA。
// 注意 qrlogin.QR.Accept 是「已登录手机端」确认 token 用的接口，未登录客户端调用
// 会返回 401 AUTH_KEY_UNREGISTERED；新客户端必须走 QR.Auth（等待 updateLoginToken）。
//
// 登录过程中的任何失败（DC 迁移、网络抖动、凭证限流等）都不终止客户端：
// 记录原因后短暂等待，重新生成二维码继续等待扫码，直到登录成功或 ctx 取消。
func (s *Service) qrLoginFlow(ctx context.Context, api *tg.Client) error {
	qr := qrlogin.NewQR(api, s.apiID, s.apiHash, qrlogin.Options{
		Migrate: func(ctx context.Context, dcID int) error {
			return s.client.MigrateTo(ctx, dcID)
		},
	})

	log.Printf("开始 Telegram 二维码登录（app_id=%d）", s.apiID)
	backoff := 3 * time.Second
	for {
		err := s.qrAuthOnce(ctx, qr)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 手机上点「确认登录」那一两秒里二维码刚好到期，导入授权就会失败。
		// 这是扫码登录的正常竞态，不是故障：立刻换一张新码继续等用户扫，
		// 绝不能把 "400 AUTH_TOKEN_EXPIRED" 这类原始 RPC 文案怼到登录层上。
		if isStaleTokenErr(err) {
			log.Printf("扫码确认时二维码已过期（%v），立即换一张继续等待扫码", err)
			s.setPhaseMessage(phaseWaitingQR, "二维码已过期，已自动换上新的码，请再扫一次")
			backoff = 3 * time.Second
			if !sleepCtx(ctx, 300*time.Millisecond) {
				return ctx.Err()
			}
			continue
		}
		log.Printf("二维码登录流程异常: %v（%s 后重新生成二维码重试）", err, backoff)
		s.setError(err)
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, 30*time.Second)
		continue
	}
}

// staleTokenCodes 是「扫码确认阶段 token 过期/失效」这类可自愈的错误码。
// 遇到它们换一张二维码就行，不需要把状态打成 error 让用户看到报错横幅。
var staleTokenCodes = []string{
	"AUTH_TOKEN_EXPIRED",
	"AUTH_TOKEN_INVALID",
}

func isStaleTokenErr(err error) bool {
	for _, code := range staleTokenCodes {
		if tgerr.Is(err, code) {
			return true
		}
	}
	return false
}

// qrAuthOnce 完成一轮「生成二维码 → 等待扫码确认 → 导入授权」，支持 2FA。
func (s *Service) qrAuthOnce(ctx context.Context, qr qrlogin.QR) error {
	_, err := qr.Auth(ctx, s.loggedIn, func(_ context.Context, token qrlogin.Token) error {
		s.setToken(token) // 二维码过期时 Auth 会自动重新导出并再次回调
		s.setPhase(phaseWaitingQR)
		log.Printf("登录二维码已生成，有效期至 %s", token.Expires().Format("15:04:05"))
		return nil
	})
	if err == nil {
		log.Printf("Telegram 登录成功")
		s.setPhase(phaseAuthorized)
		return nil
	}
	if ctx.Err() != nil {
		// 用户切换了登录方式，这里是被主动取消的，不必当成异常记录
		return ctx.Err()
	}
	log.Printf("二维码登录流程异常: %v", err)

	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		return s.finishWithPassword(ctx)
	}
	// Telegram 要求先迁移到账号所在 DC 才能继续导出/导入登录 token
	// （qr.Auth 的 Export 阶段不会自动迁移，需要调用方处理）
	var mig *qrlogin.MigrationNeededError
	if errors.As(err, &mig) {
		log.Printf("Telegram 要求迁移到 DC%d，正在迁移…", mig.MigrateTo.DCID)
		if merr := s.client.MigrateTo(ctx, mig.MigrateTo.DCID); merr != nil {
			log.Printf("迁移到 DC%d 失败: %v", mig.MigrateTo.DCID, merr)
		}
	}
	return err
}

// phoneLoginFlow 手机号 + 验证码登录（可选两步验证）。
// 错误尽量在流程内部消化：验证码输错只是重新等输入，不会把客户端打回重连，
// 否则用户每错一次都要重连一次 Telegram。
func (s *Service) phoneLoginFlow(ctx context.Context) error {
	for {
		phone, err := s.waitPhone(ctx)
		if err != nil {
			return err
		}
		s.setPhase(phaseSendingCode)
		sent, err := s.client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("发送登录验证码失败: %v", err)
			s.setPhaseMessage(phaseNeedPhone, describeAuthErr(err))
			continue
		}
		var hash string
		switch v := sent.(type) {
		case *tg.AuthSentCode:
			hash = v.PhoneCodeHash
		case *tg.AuthSentCodeSuccess:
			// 服务端直接判定已授权（极少见），按成功处理
			log.Printf("Telegram 登录成功（免验证码）")
			s.setPhase(phaseAuthorized)
			return nil
		case *tg.AuthSentCodePaymentRequired:
			s.setPhaseMessage(phaseNeedPhone, "该手机号需要付费短信验证，暂不支持")
			continue
		default:
			s.setPhaseMessage(phaseNeedPhone, fmt.Sprintf("发送验证码失败（%T），请重试", sent))
			continue
		}

		log.Printf("验证码已发送到 %s", maskPhone(phone))
		s.setPhaseMessage(phaseNeedCode, "验证码已发送，请查看 Telegram 官方账号消息")

		// 验证码重试：输错只重新等待，过期/手机号变更才回到上一步
		resend := false
		for !resend {
			code, err := s.waitCode(ctx)
			if err != nil {
				return err
			}
			_, err = s.client.Auth().SignIn(ctx, phone, code, hash)
			switch {
			case err == nil:
				log.Printf("Telegram 登录成功（手机号验证码）")
				s.setPhase(phaseAuthorized)
				return nil
			case errors.Is(err, auth.ErrPasswordAuthNeeded):
				if err := s.loginWithPassword(ctx); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return err
				}
				log.Printf("Telegram 登录成功（手机号 + 两步验证）")
				s.setPhase(phaseAuthorized)
				return nil
			case tgerr.Is(err, "PHONE_CODE_INVALID"), tgerr.Is(err, "PHONE_CODE_EMPTY"):
				s.setPhaseMessage(phaseNeedCode, "验证码不正确，请重新输入")
			case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
				s.setPhaseMessage(phaseNeedPhone, "验证码已过期，请重新获取")
				resend = true
			case tgerr.Is(err, "FLOOD_WAIT"), tgerr.Is(err, "FLOOD_PREMIUM_WAIT"):
				s.setPhaseMessage(phaseNeedCode, describeAuthErr(err))
			case ctx.Err() != nil:
				return ctx.Err()
			default:
				s.setPhaseMessage(phaseNeedCode, describeAuthErr(err))
			}
		}
	}
}

// loginWithPassword 两步验证：等 Web 端提交密码，错了继续等，不会中断整个客户端。
func (s *Service) loginWithPassword(ctx context.Context) error {
	for {
		s.setPhase(phaseNeedPassword)
		pw, err := s.waitPassword(ctx)
		if err != nil {
			return err
		}
		if _, err := s.client.Auth().Password(ctx, pw); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, auth.ErrPasswordInvalid) || tgerr.Is(err, "PASSWORD_HASH_INVALID") {
				log.Printf("两步验证密码错误，等待重新输入")
				s.setPhaseMessage(phaseNeedPassword, "两步验证密码不正确，请重新输入")
				continue
			}
			log.Printf("两步验证失败: %v", err)
			return err
		}
		return nil
	}
}

// SubmitPhone 提交手机号（账号登录第一步）。
// 客户端可能还没连上 Telegram，这里只把号码放进缓冲通道，等流程启动后消费。
func (s *Service) SubmitPhone(phone string) error {
	phone = normalizePhone(phone)
	if phone == "" {
		return errors.New("请输入手机号")
	}
	if !validPhone(phone) {
		return errors.New("手机号要带国家码，例如 +8613800138000")
	}
	if s.IsAuthorized() {
		return errors.New("已经登录，无需再次操作")
	}
	// 切到账号登录：必须走 SetLoginMode，它会取消正在跑的扫码流程，
	// authLoop 随即以账号登录重来并消费下面塞进通道的手机号。
	// （只改 loginMode 字段不取消的话，二维码流程会一直占着登录循环，
	//   手机号躺在通道里没人读，用户永远等不到验证码。）
	if s.LoginMode() != ModePhone {
		if err := s.SetLoginMode(ModePhone); err != nil {
			return err
		}
	}
	select {
	case s.phoneChan <- phone:
		return nil
	default:
	}
	// 缓冲区里已有旧号码：丢掉换成最新的
	select {
	case <-s.phoneChan:
	default:
	}
	select {
	case s.phoneChan <- phone:
		return nil
	default:
		return errors.New("手机号已提交，请稍候")
	}
}

// SubmitCode 提交收到的登录验证码
func (s *Service) SubmitCode(code string) error {
	code = strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(code))
	if code == "" {
		return errors.New("请输入验证码")
	}
	if s.IsAuthorized() {
		return errors.New("已经登录，无需再次操作")
	}
	select {
	case s.codeChan <- code:
		return nil
	default:
	}
	select {
	case <-s.codeChan:
	default:
	}
	select {
	case s.codeChan <- code:
		return nil
	default:
		return errors.New("验证码已提交，请稍候")
	}
}

func (s *Service) waitPhone(ctx context.Context) (string, error) {
	select {
	case v := <-s.phoneChan:
		return v, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Service) waitCode(ctx context.Context) (string, error) {
	select {
	case v := <-s.codeChan:
		return v, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// normalizePhone 去掉用户从通讯录/网页复制来的空格与分隔符，统一成 +国家码形式。
func normalizePhone(raw string) string {
	p := strings.NewReplacer(" ", "", "\t", "", "\n", "", "-", "", "(", "", ")", "", "（", "", "）", "").Replace(strings.TrimSpace(raw))
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "00") {
		p = "+" + p[2:]
	}
	return p
}

// maskPhone 日志里只保留国家码与后四位，避免完整手机号落盘
func maskPhone(p string) string {
	if len(p) <= 6 {
		return p
	}
	return p[:len(p)-8] + "****" + p[len(p)-4:]
}

// validPhone 只接受国际格式（+ 国家码 + 号码），避免把本地号码直接发给 Telegram
func validPhone(p string) bool {
	if len(p) < 7 || len(p) > 21 || p[0] != '+' {
		return false
	}
	for _, r := range p[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// describeAuthErr 把 Telegram 的 RPC 错误翻译成用户能看懂的提示
func describeAuthErr(err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		sec := int(d.Seconds())
		if sec <= 0 {
			sec = 1
		}
		return fmt.Sprintf("操作过于频繁，请 %d 秒后再试", sec)
	}
	switch {
	case tgerr.Is(err, "PHONE_NUMBER_INVALID"):
		return "手机号格式不正确，请带上国家码，例如 +8613800138000"
	case tgerr.Is(err, "PHONE_NUMBER_BANNED"):
		return "该手机号已被 Telegram 封禁"
	case tgerr.Is(err, "PHONE_NUMBER_FLOOD"):
		return "该手机号获取验证码过于频繁，请稍后再试"
	case tgerr.Is(err, "PHONE_PASSWORD_FLOOD"):
		return "两步验证密码尝试过于频繁，请稍后再试"
	case tgerr.Is(err, "AUTH_RESTART"):
		return "登录流程已重置，请重新获取验证码"
	case tgerr.Is(err, "API_ID_PUBLISHED_FLOOD"):
		return "当前公共 API 凭证被限流，请稍后再试"
	case tgerr.Is(err, "SESSION_PASSWORD_NEEDED"):
		return "该账号开启了两步验证，请先输入验证码"
	}
	return err.Error()
}

// finishWithPassword 处理两步验证：等待 Web 端提交密码完成登录
func (s *Service) finishWithPassword(ctx context.Context) error {
	s.setPhase(phaseNeedPassword)
	pw, perr := s.waitPassword(ctx)
	if perr != nil {
		return perr
	}
	if _, perr := s.client.Auth().Password(ctx, pw); perr != nil {
		log.Printf("两步验证密码校验失败: %v", perr)
		return perr
	}
	log.Printf("Telegram 登录成功（两步验证）")
	s.setPhase(phaseAuthorized)
	return nil
}

func (s *Service) waitPassword(ctx context.Context) (string, error) {
	s.authMu.Lock()
	s.pwChan = make(chan string, 1)
	ch := s.pwChan
	s.authMu.Unlock()

	select {
	case pw := <-ch:
		return pw, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// SubmitPassword Web 端提交两步验证密码
func (s *Service) SubmitPassword(pw string) error {
	s.authMu.Lock()
	ch := s.pwChan
	s.authMu.Unlock()
	if ch == nil {
		return errors.New("当前不需要两步验证密码")
	}
	select {
	case ch <- pw:
		return nil
	default:
		return errors.New("密码已提交，请勿重复操作")
	}
}
