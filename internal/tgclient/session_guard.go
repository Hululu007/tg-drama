package tgclient

import (
	"context"
	"log"
	"os"
	"sync/atomic"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

var sessionDeadCodes = []string{
	"AUTH_KEY_UNREGISTERED",
	"AUTH_KEY_INVALID",
	"SESSION_REVOKED",
	"SESSION_EXPIRED",
	"USER_DEACTIVATED",
	"USER_DEACTIVATED_BAN",
}

func isSessionDead(err error) bool {
	if err == nil {
		return false
	}
	for _, code := range sessionDeadCodes {
		if tgerr.Is(err, code) {
			return true
		}
	}
	return false
}

func (s *Service) sessionGuard(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		err := next.Invoke(ctx, input, output)

		if isSessionDead(err) && s.IsAuthorized() {
			s.handleSessionDead(err)
		}
		return err
	}
}

func (s *Service) handleSessionDead(cause error) {
	if !atomic.CompareAndSwapInt32(&s.keyDead, 0, 1) {
		return
	}
	log.Printf("登录会话已被 Telegram 注销（%v），清除本地会话并重新登录", cause)
	if err := os.Remove(s.cfg.SessionPath()); err != nil && !os.IsNotExist(err) {
		log.Printf("删除本地会话文件失败: %v", err)
	}
	s.mu.Lock()
	s.authorized = false
	s.api = nil
	cancel := s.connCancel
	s.mu.Unlock()
	s.setNotice("登录会话已被 Telegram 注销（可能在其它设备上退出了本应用，或触发了风控），请重新扫码/登录")
	s.setPhaseMessage(phaseError, "登录会话已被 Telegram 注销，请重新登录")
	atomic.StoreInt32(&s.restartReq, 1)
	if cancel != nil {
		cancel()
	}
}
