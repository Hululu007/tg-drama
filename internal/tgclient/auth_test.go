package tgclient

import (
	"errors"
	"testing"

	"github.com/gotd/td/tgerr"
)

// TestNormalizePhone 手机号来自输入框/通讯录复制，需统一成 +国家码 再去提交
func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"+86 138-0013-8000": "+8613800138000",
		" +8613800138000 ":  "+8613800138000",
		"008613800138000":   "+8613800138000",
		"(+86)13800138000":  "+8613800138000",
		"":                  "",
		"   ":               "",
	}
	for in, want := range cases {
		if got := normalizePhone(in); got != want {
			t.Errorf("normalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	got := maskPhone("+8613800138000")
	if got == "+8613800138000" {
		t.Fatalf("日志里不应出现完整手机号: %s", got)
	}
	if len(got) <= 4 || got[len(got)-4:] != "8000" {
		t.Fatalf("应保留后四位: %s", got)
	}
}

// TestSetLoginMode 登录方式切换：默认扫码，切账号后状态可读，非法值拒绝
func TestSetLoginMode(t *testing.T) {
	s := &Service{loginMode: ModeQR, phoneChan: make(chan string, 1), codeChan: make(chan string, 1)}
	if got := s.LoginMode(); got != ModeQR {
		t.Fatalf("默认应为扫码，实际 %s", got)
	}
	if err := s.SetLoginMode(ModePhone); err != nil {
		t.Fatal(err)
	}
	if got := s.LoginMode(); got != ModePhone {
		t.Fatalf("切换后应为账号登录，实际 %s", got)
	}
	if err := s.SetLoginMode("sms"); err == nil {
		t.Fatal("非法登录方式应报错")
	}
	if got := s.LoginMode(); got != ModePhone {
		t.Fatalf("非法值不应改动当前方式，实际 %s", got)
	}
}

// TestSubmitPhoneCode 提交手机号 / 验证码进入缓冲通道；格式错误直接拒绝
func TestSubmitPhoneCode(t *testing.T) {
	s := &Service{loginMode: ModeQR, phoneChan: make(chan string, 1), codeChan: make(chan string, 1)}

	if err := s.SubmitPhone(""); err == nil {
		t.Fatal("空手机号应报错")
	}
	if err := s.SubmitPhone("+86 138-0013-8000"); err != nil {
		t.Fatal(err)
	}
	// 提交手机号会自动切到账号登录
	if s.LoginMode() != ModePhone {
		t.Fatalf("提交手机号后应切到账号登录，实际 %s", s.LoginMode())
	}
	select {
	case got := <-s.phoneChan:
		if got != "+8613800138000" {
			t.Fatalf("手机号未规范化: %s", got)
		}
	default:
		t.Fatal("手机号未进入等待队列")
	}

	if err := s.SubmitCode("  "); err == nil {
		t.Fatal("空验证码应报错")
	}
	if err := s.SubmitCode("1 2 3 4 5"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-s.codeChan:
		if got != "12345" {
			t.Fatalf("验证码未去掉空格: %q", got)
		}
	default:
		t.Fatal("验证码未进入等待队列")
	}
}

// TestRestoreLoginModeWithoutDB 没有数据库时（单测/临时服务）恢复逻辑不能 panic
func TestRestoreLoginModeWithoutDB(t *testing.T) {
	s := &Service{loginMode: ModeQR}
	s.restoreLoginMode()
	if got := s.LoginMode(); got != ModeQR {
		t.Fatalf("无数据库时应保持默认扫码，实际 %s", got)
	}
}

// TestIsStaleTokenErr 扫码确认阶段 token 过期属于正常竞态：必须能识别出来，
// 靠换一张二维码自愈，而不是把 400 AUTH_TOKEN_EXPIRED 当成故障显示给用户。
func TestIsStaleTokenErr(t *testing.T) {
	if isStaleTokenErr(nil) {
		t.Fatal("nil 不应算过期")
	}
	for _, code := range staleTokenCodes {
		if !isStaleTokenErr(tgerr.New(400, code)) {
			t.Fatalf("%s 应判定为 token 过期", code)
		}
	}
	for _, code := range []string{"PHONE_CODE_INVALID", "FLOOD_WAIT", "AUTH_KEY_UNREGISTERED", "SESSION_PASSWORD_NEEDED"} {
		if isStaleTokenErr(tgerr.New(400, code)) {
			t.Fatalf("%s 不应判定为 token 过期", code)
		}
	}
	if isStaleTokenErr(errors.New("网络不可用")) {
		t.Fatal("普通错误不应判定为 token 过期")
	}
}
