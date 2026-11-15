package tgclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tgerr"

	"tgdrama/internal/config"
)

func TestIsSessionDead(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"验证码错误", tgerr.New(400, "PHONE_CODE_INVALID"), false},
		{"限流", tgerr.New(420, "FLOOD_WAIT_30"), false},
		{"普通错误", errors.New("connection reset"), false},
		{"auth key 被注销", tgerr.New(401, "AUTH_KEY_UNREGISTERED"), true},
		{"auth key 无效", tgerr.New(401, "AUTH_KEY_INVALID"), true},
		{"会话被撤销", tgerr.New(401, "SESSION_REVOKED"), true},
		{"账号被封", tgerr.New(401, "USER_DEACTIVATED_BAN"), true},
	}
	for _, c := range cases {
		if got := isSessionDead(c.err); got != c.want {
			t.Errorf("%s: isSessionDead(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// 会话被注销的自愈逻辑：登录中的 AUTH_KEY_UNREGISTERED 是正常状态（gotd 用它
// 表示「这个 auth key 还没登录」），不能清会话；已登录后同样的错误才代表
// 会话被 Telegram 注销，必须清掉本地会话并回到登录界面。
func TestSessionGuard(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "session.json")
	if err := os.WriteFile(sess, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: &config.Config{Data: config.DataConfig{Dir: dir}}}
	dead := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	next := telegram.InvokeFunc(func(context.Context, bin.Encoder, bin.Decoder) error {
		return dead
	})
	invoke := s.sessionGuard(next)

	// 未登录：视为正常，不删会话文件
	if err := invoke(context.Background(), nil, nil); err == nil {
		t.Fatal("错误应原样返回")
	}
	if _, err := os.Stat(sess); err != nil {
		t.Fatalf("未登录时不应删除会话文件: %v", err)
	}

	// 已登录：判定会话被注销，清会话 + 提示 + 重新连接
	s.authorized = true
	if err := invoke(context.Background(), nil, nil); err == nil {
		t.Fatal("错误应原样返回")
	}
	if _, err := os.Stat(sess); !os.IsNotExist(err) {
		t.Fatalf("已登录时应删除本地会话文件, stat err = %v", err)
	}
	if got := s.AuthPhase(); got != phaseError {
		t.Errorf("phase = %q, want %q", got, phaseError)
	}
	if msg := s.AuthMessage(); !strings.Contains(msg, "注销") {
		t.Errorf("AuthMessage = %q, 应提示会话被注销", msg)
	}
	if s.IsAuthorized() {
		t.Error("会话失效后 authorized 应为 false")
	}
}
