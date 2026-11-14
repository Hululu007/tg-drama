package tgclient

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"

	"tgdrama/internal/config"
	"tgdrama/internal/parser"
	"tgdrama/internal/storage"
)

func TestAlbumCaptionHealsDroppedMember(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cfg := config.Default()
	cfg.Data.Dir = dir
	svc, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if _, err := db.UpsertChat(&storage.Chat{TgChatID: 200, AccessHash: 1, Name: "AI短剧频道", Type: "channel", Enabled: true}); err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	chats, err := db.ListEnabledChats()
	if err != nil || len(chats) != 1 {
		t.Fatalf("list chats: %v %d", err, len(chats))
	}
	c := chats[0]

	m7 := albumVideo(71, "", 7001, "doc-5143642006537048112.mp4", 900)
	if n, err := svc.ingestPage(context.Background(), c, []*tg.Message{m7}, nil); err != nil || n != 0 {
		t.Fatalf("ingest dropped member: n=%d err=%v, want 0", n, err)
	}
	if !db.HasPendingMessage(c.ID, 71) {
		t.Fatalf("待归属索引丢了第 7 集视频，重建时无法复活")
	}
	if shows, _ := db.ListShows("", "", 10, 0); len(shows) != 0 {
		t.Fatalf("被丢弃的垃圾视频不应建剧，got %d", len(shows))
	}

	m8 := albumVideo(72, "《淫魔事务所》第7-8集 连载中 全系列完整版 #淫魔事务所", 7002, "doc-72.mp4", 900)
	if n, err := svc.ingestPage(context.Background(), c, []*tg.Message{m8}, nil); err != nil || n != 1 {
		t.Fatalf("ingest caption member: n=%d err=%v, want 1", n, err)
	}

	if err := db.RebuildFromRaw(parser.New()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	shows, err := db.ListShows("", "", 10, 0)
	if err != nil || len(shows) != 1 {
		t.Fatalf("list shows: %v %d", err, len(shows))
	}
	if shows[0].Name != "淫魔事务所" {
		t.Fatalf("show name = %q", shows[0].Name)
	}
	eps, err := db.ListEpisodes(shows[0].ID)
	if err != nil {
		t.Fatalf("list episodes: %v", err)
	}
	got := make([]int, 0, len(eps))
	for _, e := range eps {
		got = append(got, e.EpisodeNo)
	}
	if len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Fatalf("episodes = %v, want [7 8]", got)
	}
}

func TestAlbumPartialFillKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cfg := config.Default()
	cfg.Data.Dir = dir
	svc, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if _, err := db.UpsertChat(&storage.Chat{TgChatID: 201, AccessHash: 1, Name: "AI短剧频道", Type: "channel", Enabled: true}); err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	chats, _ := db.ListEnabledChats()
	c := chats[0]

	m8 := albumVideo(82, "《淫魔事务所》第8集 #淫魔事务所", 8002, "ep8.mp4", 901)
	if n, err := svc.ingestPage(context.Background(), c, []*tg.Message{m8}, nil); err != nil || n != 1 {
		t.Fatalf("ingest ep8: n=%d err=%v", n, err)
	}

	m7 := albumVideo(81, "", 8001, "doc-8001.mp4", 901)
	if n, err := svc.ingestPage(context.Background(), c, []*tg.Message{m7}, map[int64]string{901: "《淫魔事务所》第7-8集 #淫魔事务所"}); err != nil || n != 1 {
		t.Fatalf("ingest ep7: n=%d err=%v", n, err)
	}

	shows, _ := db.ListShows("", "", 10, 0)
	if len(shows) != 1 {
		t.Fatalf("shows = %d", len(shows))
	}
	eps, err := db.ListEpisodes(shows[0].ID)
	if err != nil {
		t.Fatalf("list episodes: %v", err)
	}
	got := make([]int, 0, len(eps))
	for _, e := range eps {
		got = append(got, e.EpisodeNo)
	}
	if len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Fatalf("episodes = %v, want [7 8]", got)
	}
}

func albumVideo(id int, caption string, docID int64, fileName string, groupedID int64) *tg.Message {
	m := videoMessage(id, caption, docID)
	m.GroupedID = groupedID
	if doc, ok := m.Media.(*tg.MessageMediaDocument); ok {
		if d, ok := doc.Document.(*tg.Document); ok {
			d.Attributes = []tg.DocumentAttributeClass{
				&tg.DocumentAttributeVideo{Duration: 60, W: 720, H: 1280},
				&tg.DocumentAttributeFilename{FileName: fileName},
			}
		}
	}
	return m
}
