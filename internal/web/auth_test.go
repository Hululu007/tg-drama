package web

import (
	"net/http"
	"testing"
)

func TestAuthModeAPI(t *testing.T) {
	h, _ := newTestServer(t)

	code, out := doJSON(t, h, http.MethodGet, "/api/account/state", nil)
	if code != http.StatusOK {
		t.Fatalf("status 返回 %d", code)
	}
	if out["mode"] != "qr" {
		t.Fatalf("默认登录方式应为 qr，实际 %v", out["mode"])
	}

	code, out = doJSON(t, h, http.MethodPost, "/api/account/mode", map[string]string{"mode": "phone"})
	if code != http.StatusOK || out["mode"] != "phone" {
		t.Fatalf("切换账号登录失败: %d %v", code, out)
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/account/state", nil)
	if out["mode"] != "phone" {
		t.Fatalf("状态未同步为 phone: %v", out["mode"])
	}

	if code, _ := doJSON(t, h, http.MethodPost, "/api/account/mode", map[string]string{"mode": "sms"}); code != http.StatusBadRequest {
		t.Fatalf("非法登录方式应返回 400，实际 %d", code)
	}

	if code, _ := doJSON(t, h, http.MethodGet, "/api/account/qr", nil); code != http.StatusNotFound {
		t.Fatalf("账号登录下 /api/account/qr 应返回 404，实际 %d", code)
	}
}

func TestAuthPhoneValidation(t *testing.T) {
	h, _ := newTestServer(t)

	if code, _ := doJSON(t, h, http.MethodPost, "/api/account/phone", map[string]string{"phone": ""}); code != http.StatusBadRequest {
		t.Fatalf("空手机号应返回 400，实际 %d", code)
	}
	if code, _ := doJSON(t, h, http.MethodPost, "/api/account/phone", map[string]string{"phone": "13800138000"}); code != http.StatusBadRequest {
		t.Fatalf("缺少国家码应返回 400，实际 %d", code)
	}
	if code, out := doJSON(t, h, http.MethodPost, "/api/account/phone", map[string]string{"phone": "+86 138-0013-8000"}); code != http.StatusOK {
		t.Fatalf("合法手机号应被接受: %d %v", code, out)
	}
	if code, _ := doJSON(t, h, http.MethodPost, "/api/account/code", map[string]string{"code": "  "}); code != http.StatusBadRequest {
		t.Fatalf("空验证码应返回 400，实际 %d", code)
	}
}
