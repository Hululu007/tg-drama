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

const (
	ModeQR    = "qr"
	ModePhone = "phone"

	kvAuthMode = "auth_mode"
)

func (s *Service) LoginMode() string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.loginMode == ModePhone {
		return ModePhone
	}
	return ModeQR
}

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

		s.message = ""
		if mode == ModePhone {

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

func (s *Service) setPhaseMessage(p, msg string) {
	s.authMu.Lock()
	s.phase = p
	s.message = msg
	s.authMu.Unlock()
}

func (s *Service) setNotice(msg string) {
	s.authMu.Lock()
	s.notice = msg
	s.authMu.Unlock()
}

func (s *Service) setToken(t qrlogin.Token) {
	s.authMu.Lock()
	s.qrToken = t
	s.authMu.Unlock()
}

func (s *Service) QRInfo() (loginURL string, expires time.Time, ok bool) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.qrToken.Empty() {
		return "", time.Time{}, false
	}
	return s.qrToken.URL(), s.qrToken.Expires(), true
}

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

			log.Printf("登录方式已切换为 %s，重新开始登录", s.LoginMode())
			continue
		}
		return err
	}
}

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

func (s *Service) qrAuthOnce(ctx context.Context, qr qrlogin.QR) error {
	_, err := qr.Auth(ctx, s.loggedIn, func(_ context.Context, token qrlogin.Token) error {
		s.setToken(token)
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

		return ctx.Err()
	}
	log.Printf("二维码登录流程异常: %v", err)

	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		return s.finishWithPassword(ctx)
	}

	var mig *qrlogin.MigrationNeededError
	if errors.As(err, &mig) {
		log.Printf("Telegram 要求迁移到 DC%d，正在迁移…", mig.MigrateTo.DCID)
		if merr := s.client.MigrateTo(ctx, mig.MigrateTo.DCID); merr != nil {
			log.Printf("迁移到 DC%d 失败: %v", mig.MigrateTo.DCID, merr)
		}
	}
	return err
}

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

func maskPhone(p string) string {
	if len(p) <= 6 {
		return p
	}
	return p[:len(p)-8] + "****" + p[len(p)-4:]
}

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
