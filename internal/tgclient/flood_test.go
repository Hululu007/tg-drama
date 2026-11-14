package tgclient

import (
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
)

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

	s.noteFlood(tgerr.New(420, "FLOOD_WAIT_60"))
	if left := time.Until(s.floodUntil); left < 59*time.Second {
		t.Fatalf("更长的 FLOOD_WAIT 未生效：%s", left)
	}
	s.noteFlood(tgerr.New(420, "FLOOD_WAIT_5"))
	if left := time.Until(s.floodUntil); left < 59*time.Second {
		t.Fatalf("较短的 FLOOD_WAIT 缩短了已有的静默期：%s", left)
	}
}

func TestRequeuePhotoFallsBackToShowWarm(t *testing.T) {
	s := &Service{}
	if api := s.API(); api != nil {
		t.Fatal("未连接时不应有 API")
	}
	s.RequeuePhoto(0)
	s.RequeuePhoto(12345)
}
