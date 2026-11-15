package tgclient

import (
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
)

// 收到 FLOOD_WAIT 后必须进入静默期，期间不再发预热请求；
// 普通网络错误不能误判成限流，否则图片会长时间不加载。
func TestNoteFloodPausesWarm(t *testing.T) {
	s := &Service{}
	if s.PlaybackDeferred() {
		t.Fatal("新服务不应处于限流静默期")
	}
	if s.noteFlood(errors.New("connection reset")) {
		t.Fatal("普通错误被误判为 FLOOD_WAIT")
	}
	if s.PlaybackDeferred() {
		t.Fatal("普通错误不应触发静默")
	}

	if !s.noteFlood(tgerr.New(420, "FLOOD_WAIT_30")) {
		t.Fatal("FLOOD_WAIT 未被识别")
	}
	if !s.PlaybackDeferred() {
		t.Fatal("FLOOD_WAIT 之后应进入静默期")
	}
	if left := time.Until(s.floodUntil); left < 29*time.Second {
		t.Fatalf("静默期过短：%s", left)
	}

	// 更长的等待要覆盖较短的，反过来不能把到期时间改早
	s.noteFlood(tgerr.New(420, "FLOOD_WAIT_60"))
	if left := time.Until(s.floodUntil); left < 59*time.Second {
		t.Fatalf("更长的 FLOOD_WAIT 未生效：%s", left)
	}
	s.noteFlood(tgerr.New(420, "FLOOD_WAIT_5"))
	if left := time.Until(s.floodUntil); left < 59*time.Second {
		t.Fatalf("较短的 FLOOD_WAIT 缩短了已有的静默期：%s", left)
	}
}

// 单张预览图缺失时不能再各发一次 getMessages（一页十几张会被风控），
// 必须收敛成按剧批量预热；没有访问令牌（未登录）时也不能 panic。
func TestRequeuePhotoFallsBackToShowWarm(t *testing.T) {
	s := &Service{}
	if api := s.API(); api != nil {
		t.Fatal("未连接时不应有 API")
	}
	s.RequeuePhoto(0)
	s.RequeuePhoto(12345) // 无库、无登录：只应安静返回
}
