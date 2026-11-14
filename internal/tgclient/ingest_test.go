package tgclient

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"

	"tgdrama/internal/config"
	"tgdrama/internal/storage"
)

func TestIngestPage(t *testing.T) {
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

	chatID, err := db.UpsertChat(&storage.Chat{TgChatID: 100, AccessHash: 1, Name: "测试短剧频道", Type: "channel", Enabled: true})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	chats, err := db.ListEnabledChats()
	if err != nil || len(chats) != 1 {
		t.Fatalf("list chats: %v %d", err, len(chats))
	}
	c := chats[0]

	msgs := []*tg.Message{
		videoMessage(11, "母上攻略 第1集 #母上攻略", 1001),
		videoMessage(12, "母上攻略 第2集 #母上攻略", 1002),
		videoMessage(13, "母上攻略 第1集 #母上攻略", 1003),
	}
	n, err := svc.ingestPage(context.Background(), c, msgs, nil)
	if err != nil {
		t.Fatalf("ingest page: %v", err)
	}
	if n != 3 {
		t.Fatalf("inserted = %d, want 3", n)
	}

	shows, err := db.ListShows("", "", 10, 0)
	if err != nil || len(shows) != 1 {
		t.Fatalf("list shows: %v %d", err, len(shows))
	}
	if shows[0].Name != "母上攻略" || shows[0].EpCount != 2 {
		t.Fatalf("show = %+v, want 母上攻略 with 2 episodes", shows[0])
	}

	n, err = svc.ingestPage(context.Background(), c, msgs, nil)
	if err != nil {
		t.Fatalf("ingest again: %v", err)
	}
	if n != 0 {
		t.Fatalf("second ingest inserted = %d, want 0", n)
	}
	_ = chatID
}

func videoMessage(id int, caption string, docID int64) *tg.Message {
	return &tg.Message{
		ID:      id,
		Date:    1700000000 + id,
		Message: caption,
		Media: &tg.MessageMediaDocument{
			Video: true,
			Document: &tg.Document{
				ID:         docID,
				AccessHash: 7,
				Size:       1024,
				MimeType:   "video/mp4",
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeVideo{Duration: 60, W: 720, H: 1280},
					&tg.DocumentAttributeFilename{FileName: "母上攻略.mp4"},
				},
			},
		},
	}
}

func TestIngestSkipsDisabledChat(t *testing.T) {
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

	if _, err := db.UpsertChat(&storage.Chat{
		TgChatID: 200, AccessHash: 1, Name: "已关闭频道", Type: "channel",
	}); err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	c, err := db.GetChatByTgID(200)
	if err != nil || c.Enabled {
		t.Fatalf("chat 应为关闭状态: %+v %v", c, err)
	}

	n, err := svc.ingestPage(context.Background(), c,
		[]*tg.Message{videoMessage(21, "某剧 第1集 #某剧", 2001)}, nil)
	if err != nil {
		t.Fatalf("ingest page: %v", err)
	}
	if n != 0 {
		t.Fatalf("关闭频道入库 %d 条，期望 0", n)
	}
	if _, ok, _ := db.FindShowByNorm("某剧"); ok {
		t.Fatal("关闭频道的剧不该出现在媒体库里")
	}
}

func TestIngestStoresAltDocuments(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := config.Default()
	cfg.Data.Dir = dir
	svc, err := New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertChat(&storage.Chat{TgChatID: 200, AccessHash: 1, Name: "频道", Type: "channel", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	chats, _ := db.ListEnabledChats()
	c := chats[0]

	msg := videoMessage(21, "远山来信 第1集 #远山来信", 5001)
	media := msg.Media.(*tg.MessageMediaDocument)
	media.AltDocuments = []tg.DocumentClass{
		&tg.Document{ID: 5002, AccessHash: 8, Size: 500, MimeType: "video/mp4",
			FileReference: []byte("ref-720"),
			Attributes:    []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{Duration: 60, W: 720, H: 1280}}},
		&tg.Document{ID: 5003, AccessHash: 9, Size: 200, MimeType: "video/mp4",
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{Duration: 60, W: 480, H: 854}}},

		&tg.Document{ID: 5004, AccessHash: 10, Size: 50, MimeType: "application/x-mpegurl"},

		&tg.Document{ID: 5001, AccessHash: 7, Size: 1024, MimeType: "video/mp4"},
	}
	n, err := svc.ingestPage(context.Background(), c, []*tg.Message{msg}, nil)
	if err != nil || n != 1 {
		t.Fatalf("ingest = %d, %v", n, err)
	}

	res, _ := db.SearchShows("远山来信", 10)
	if len(res) != 1 {
		t.Fatalf("shows = %d", len(res))
	}
	eps, _ := db.ListEpisodes(res[0].ID)
	if len(eps) != 1 {
		t.Fatalf("episodes = %d", len(eps))
	}
	lines, _ := db.ListEpisodeLines(eps[0].ID)
	if len(lines) != 1 || len(lines[0].Segments) != 1 {
		t.Fatalf("lines = %+v", lines)
	}
	src := lines[0].Segments[0]
	vars, err := db.ListSourceVariants(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 {
		t.Fatalf("variants = %d, want 2: %+v", len(vars), vars)
	}
	if vars[0].Height != 1280 || vars[1].Height != 854 {
		t.Fatalf("variant order/heights = %+v", vars)
	}
	if checked, err := db.SourceVariantsChecked(src.ID); err != nil || !checked {
		t.Fatalf("checked = %v, %v", checked, err)
	}
}
