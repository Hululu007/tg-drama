package rankings

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"
)

func fakeSource(key string, fn func() ([]Item, error)) *source {
	return &source{
		Key: key, Name: key, Site: key, URL: "https://example.com/" + key,
		Fetch: func(context.Context, *http.Client) ([]Item, error) { return fn() },
	}
}

func TestServiceRefreshAndTTL(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	svc := New(nil, dir, 12*time.Hour, 5*time.Second, nil)
	svc.now = func() time.Time { return now }
	svc.sources = []*source{
		fakeSource("a", func() ([]Item, error) {
			return []Item{{Rank: 1, Title: "甲"}, {Rank: 2, Title: "乙"}}, nil
		}),
		fakeSource("b", func() ([]Item, error) {
			return nil, errors.New("boom")
		}),
	}
	svc.ensureBoards()

	if !svc.NeedsRefresh() {
		t.Fatal("首次启动应该需要抓取")
	}
	if err := svc.Refresh(context.Background(), false); err != nil {
		t.Fatalf("首次抓取失败: %v", err)
	}
	snap := svc.Snapshot()
	if len(snap.Boards) != 2 {
		t.Fatalf("应有 2 个榜单，实际 %d", len(snap.Boards))
	}
	if !snap.Boards[0].OK || len(snap.Boards[0].Items) != 2 {
		t.Fatalf("A 榜应抓取成功: %+v", snap.Boards[0])
	}
	if snap.Boards[1].OK {
		t.Fatalf("B 榜应标记失败: %+v", snap.Boards[1])
	}
	if snap.UpdatedAt != now.Unix() || snap.AttemptedAt != now.Unix() {
		t.Fatalf("时间戳异常: %+v", snap)
	}

	// 12 小时内再调用必须被拒绝
	if err := svc.Refresh(context.Background(), false); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("12 小时内重复抓取应返回 ErrTooSoon，实际 %v", err)
	}
	if svc.NeedsRefresh() {
		t.Fatal("12 小时内不应需要刷新")
	}

	// 缓存要真的落盘，重启后能读回来
	raw, err := os.ReadFile(svc.CachePath())
	if err != nil || len(raw) == 0 {
		t.Fatalf("缓存文件未写入: %v", err)
	}
	svc2 := New(nil, dir, 12*time.Hour, 5*time.Second, nil)
	svc2.sources = svc.sources
	svc2.now = func() time.Time { return now.Add(time.Hour) }
	if err := svc2.Load(); err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if got := svc2.Snapshot(); len(got.Boards) != 2 || len(got.Boards[0].Items) != 2 {
		t.Fatalf("重启后榜单数据丢失: %+v", got)
	}
	if svc2.NeedsRefresh() {
		t.Fatal("重启后 12 小时内不应重新抓取")
	}

	// 过了 12 小时，抓取失败时要保留上一轮条目
	now = now.Add(13 * time.Hour)
	if !svc.NeedsRefresh() {
		t.Fatal("超过 12 小时应需要刷新")
	}
	svc.sources[0].Fetch = func(context.Context, *http.Client) ([]Item, error) {
		return nil, errors.New("network down")
	}
	if err := svc.Refresh(context.Background(), false); err != nil {
		t.Fatalf("抓取失败不应让整体报错: %v", err)
	}
	snap = svc.Snapshot()
	if snap.Boards[0].OK {
		t.Fatal("抓取失败应标记 OK=false")
	}
	if len(snap.Boards[0].Items) != 2 {
		t.Fatalf("抓取失败应保留上一轮条目，实际 %+v", snap.Boards[0])
	}
	if snap.UpdatedAt == now.Unix() {
		t.Fatal("全部失败时不应刷新 UpdatedAt")
	}
}

func TestServiceForceCooldown(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	svc := New(nil, dir, 12*time.Hour, 5*time.Second, nil)
	svc.now = func() time.Time { return now }
	svc.sources = []*source{fakeSource("a", func() ([]Item, error) {
		return []Item{{Rank: 1, Title: "甲"}}, nil
	})}
	svc.ensureBoards()
	if err := svc.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := svc.Refresh(context.Background(), true); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("强制刷新也要有 5 分钟冷却，实际 %v", err)
	}
	now = now.Add(6 * time.Minute)
	if err := svc.Refresh(context.Background(), true); err != nil {
		t.Fatalf("冷却结束后应允许强制刷新: %v", err)
	}
}

func TestServicePayloadShape(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	svc := New(nil, dir, 12*time.Hour, 5*time.Second, []string{"yeguo"})
	svc.now = func() time.Time { return now }
	if len(svc.sources) != 1 || svc.sources[0].Key != "yeguo" {
		t.Fatalf("来源过滤异常: %+v", svc.sources)
	}
	svc.sources = []*source{fakeSource("yeguo", func() ([]Item, error) {
		return []Item{{Rank: 1, Title: "甲"}}, nil
	})}
	svc.ensureBoards()
	if err := svc.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	p := svc.Payload()
	if p.IntervalHours != 12 {
		t.Fatalf("interval_hours 应为 12，实际 %d", p.IntervalHours)
	}
	if p.NextUpdateAt != now.Unix()+12*3600 {
		t.Fatalf("下次更新时间异常: %d", p.NextUpdateAt)
	}
	if len(p.Boards) != 1 || p.Boards[0].Total != 1 {
		t.Fatalf("payload 榜单异常: %+v", p.Boards)
	}
	if p.Boards[0].Items == nil {
		t.Fatal("items 必须是空数组而不是 null，前端才能直接遍历")
	}
}
