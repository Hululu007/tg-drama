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

// sessionDeadCodes 是「这条会话已经被 Telegram 注销」的错误码：本地重试
// 没有任何意义，只有换 auth key 重新登录才能恢复。常见触发场景：
// 账号在其它设备上点了退出登录、Telegram 侧风控强制下线，
// 或两端共用同一个 session.json（同一 auth key 从两个 IP 使用会被判重注销）。
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

// sessionGuard 是 gotd 的 RPC 中间件：一旦某个请求返回「会话已注销」，
// 立刻清掉本地会话并重新走登录流程。没有这层保护时，客户端会一直停在
// authorized=true，页面上所有请求都 401，用户只看到「刷新失败」，
// 却不知道会话早就失效、需要重新登录。
func (s *Service) sessionGuard(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		err := next.Invoke(ctx, input, output)
		// 只在「自认为已登录」时才判定会话被注销：登录流程里的
		// auth.Status 就是靠 AUTH_KEY_UNREGISTERED 表示「这个 auth key 还没登录」，
		// 那种情况是正常状态，不能当成会话失效去清文件重连。
		if isSessionDead(err) && s.IsAuthorized() {
			s.handleSessionDead(err)
		}
		return err
	}
}

// handleSessionDead 清理被注销的会话并让 Run 循环立即以未登录状态重连。
// 幂等：并发的多个 401 只会触发一次清理。
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
