package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mergeSeed 准备「重复剧 + 目标剧」：两剧各有一集、一条来源、一张图片，
// 重复剧另有收藏、标签、观看记录与封面。
func mergeSeed(t *testing.T) (path string, db *sql.DB) {
	t.Helper()
	return mergeSeedWithDDL(t, "")
}

func mergeSeedWithDDL(t *testing.T, ddl string) (path string, db *sql.DB) {
	t.Helper()
	path, db = nameRepairFixture(t, ddl)
	nameRepairExec(t, db, `INSERT OR IGNORE INTO chats(id,tg_chat_id,name,created_at) VALUES(1,101,'c1',1)`)
	nameRepairExec(t, db, `INSERT OR IGNORE INTO chats(id,tg_chat_id,name,created_at) VALUES(2,102,'c2',1)`)
	// 目标剧：show 1，第 1 集
	nameRepairExec(t, db, `INSERT INTO shows(id,name,norm_name,cover_path,created_at,updated_at,hidden) VALUES(1,'后悔有妻','后悔有妻','data/covers/show-1.jpg',10,10,0)`)
	nameRepairExec(t, db, `INSERT INTO episodes(id,show_id,episode_no,created_at,updated_at) VALUES(11,1,1,10,10)`)
	nameRepairExec(t, db, `INSERT INTO sources(id,episode_id,chat_id,message_id,segment_index,file_name,file_size,caption,created_at) VALUES(101,11,1,1000,0,'a.mp4',100,'《后悔有妻》第1集',10)`)
	// 重复剧：show 2，同样第 1 集，来源来自另一个频道
	nameRepairExec(t, db, `INSERT INTO shows(id,name,norm_name,cover_path,created_at,updated_at,hidden) VALUES(2,'后悔有妻 第','后悔有妻第','data/covers/show-2.jpg',10,10,0)`)
	nameRepairExec(t, db, `INSERT INTO episodes(id,show_id,episode_no,created_at,updated_at) VALUES(21,2,1,10,10)`)
	nameRepairExec(t, db, `INSERT INTO sources(id,episode_id,chat_id,message_id,segment_index,file_name,file_size,caption,created_at) VALUES(201,21,2,2000,0,'竖屏(1).mp4',200,'《后悔有妻  第1-2集》',10)`)
	nameRepairExec(t, db, `INSERT INTO show_photos(id,show_id,chat_id,message_id,doc_id,created_at) VALUES(31,2,2,2001,9001,10)`)
	nameRepairExec(t, db, `INSERT OR IGNORE INTO tags(id,name) VALUES(1,'共同标签'),(2,'独有标签')`)
	nameRepairExec(t, db, `INSERT INTO show_tags(show_id,tag_id) VALUES(1,1),(2,1),(2,2)`)
	nameRepairExec(t, db, `INSERT INTO favorites(show_id,created_at,base_ep) VALUES(2,10,1)`)
	nameRepairExec(t, db, `INSERT INTO history(episode_id,position,duration,finished,updated_at) VALUES(21,42.5,720,0,10)`)
	return path, db
}

func mergeBackupPath(t *testing.T, path string) string {
	t.Helper()
	return filepath.Join(filepath.Dir(path), "merge-backup.db")
}

// 合并把来源并进目标同集、搬走图片/标签/收藏/观看记录，删除重复剧行，
// 目标剧 ID、名称与已有来源保持不变。
func TestMergeShowIntoMovesEverything(t *testing.T) {
	path, db := mergeSeed(t)
	res, err := MergeShowInto(context.Background(), path, 2, 1, mergeBackupPath(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if res.MovedSources != 1 || res.MergedEpisodes != 1 || res.MovedPhotos != 1 ||
		res.MovedTags != 1 || res.MovedFavorites != 1 || res.MovedHistories != 1 {
		t.Fatalf("merge result = %+v", res)
	}
	var name, norm string
	if err := db.QueryRow(`SELECT name, norm_name FROM shows WHERE id=1`).Scan(&name, &norm); err != nil || name != "后悔有妻" || norm != "后悔有妻" {
		t.Fatalf("target show = %q/%q, %v", name, norm, err)
	}
	var gone int
	_ = db.QueryRow(`SELECT COUNT(*) FROM shows WHERE id=2`).Scan(&gone)
	if gone != 0 {
		t.Fatal("duplicate show row not deleted")
	}
	var sources int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sources WHERE episode_id=11`).Scan(&sources)
	if sources != 2 {
		t.Fatalf("target episode sources = %d, want 2", sources)
	}
	var ep int
	_ = db.QueryRow(`SELECT COUNT(*) FROM episodes WHERE id=21`).Scan(&ep)
	if ep != 0 {
		t.Fatal("merged duplicate episode not deleted")
	}
	var histEp int64
	if err := db.QueryRow(`SELECT episode_id FROM history`).Scan(&histEp); err != nil || histEp != 11 {
		t.Fatalf("history episode = %d, %v", histEp, err)
	}
	var photoShow int64
	_ = db.QueryRow(`SELECT show_id FROM show_photos WHERE id=31`).Scan(&photoShow)
	if photoShow != 1 {
		t.Fatalf("photo show = %d", photoShow)
	}
	var fav, tags int
	_ = db.QueryRow(`SELECT COUNT(*) FROM favorites WHERE show_id=1`).Scan(&fav)
	_ = db.QueryRow(`SELECT COUNT(*) FROM show_tags WHERE show_id=1`).Scan(&tags)
	if fav != 1 || tags != 2 {
		t.Fatalf("favorites=%d tags=%d", fav, tags)
	}
	if res.Backup == "" {
		t.Fatal("merge must create a backup")
	}
	if _, err := os.Stat(res.Backup); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	// 再次合并同一对 ID：重复剧已不存在，必须报错且不改动任何数据。
	if _, err := MergeShowInto(context.Background(), path, 2, 1, filepath.Join(filepath.Dir(path), "second.db")); err == nil {
		t.Fatal("second merge should fail")
	}
}

// 目标剧没有同集号时整集搬到目标剧，ID 与集号不变。
func TestMergeShowIntoMovesMissingEpisode(t *testing.T) {
	path, db := mergeSeed(t)
	nameRepairExec(t, db, `UPDATE episodes SET episode_no=5 WHERE id=21`)
	res, err := MergeShowInto(context.Background(), path, 2, 1, mergeBackupPath(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if res.MovedEpisodes != 1 || res.MergedEpisodes != 0 {
		t.Fatalf("merge result = %+v", res)
	}
	var showID, no int64
	if err := db.QueryRow(`SELECT show_id, episode_no FROM episodes WHERE id=21`).Scan(&showID, &no); err != nil || showID != 1 || no != 5 {
		t.Fatalf("episode = %d/%d, %v", showID, no, err)
	}
}

// 两边同一集都有观看记录时无法判断保留哪条，必须整体拒绝。
func TestMergeShowIntoRefusesBothHistories(t *testing.T) {
	path, db := mergeSeed(t)
	nameRepairExec(t, db, `INSERT INTO history(episode_id,position,duration,finished,updated_at) VALUES(11,10,100,0,11)`)
	if _, err := MergeShowInto(context.Background(), path, 2, 1, mergeBackupPath(t, path)); err == nil {
		t.Fatal("merge should be refused")
	}
	var shows, hist int
	_ = db.QueryRow(`SELECT COUNT(*) FROM shows`).Scan(&shows)
	_ = db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&hist)
	if shows != 2 || hist != 2 {
		t.Fatalf("refused merge changed data: shows=%d history=%d", shows, hist)
	}
	if _, err := os.Stat(mergeBackupPath(t, path)); !os.IsNotExist(err) {
		t.Fatal("refused merge must not leave a backup")
	}
}

// 名称完全不相关的两部剧不允许合并。
func TestMergeShowIntoRefusesUnrelatedNames(t *testing.T) {
	path, db := mergeSeed(t)
	nameRepairExec(t, db, `UPDATE shows SET name='远山来信', norm_name='远山来信' WHERE id=2`)
	if _, err := MergeShowInto(context.Background(), path, 2, 1, mergeBackupPath(t, path)); err == nil || !strings.Contains(err.Error(), "unrelated") {
		t.Fatalf("err = %v", err)
	}
}

// 同一频道消息在两边重复（来源唯一键冲突）时拒绝合并，避免丢来源。
// 当前 schema 的唯一键不允许这种数据，这里去掉约束构造旧库场景，验证防御性预检。
func TestMergeShowIntoRefusesSourceKeyConflict(t *testing.T) {
	ddl := strings.Replace(schemaSQL, "UNIQUE (chat_id, message_id, segment_index),", "", 1)
	path, db := mergeSeedWithDDL(t, ddl)
	nameRepairExec(t, db, `INSERT INTO sources(id,episode_id,chat_id,message_id,segment_index,file_name,file_size,caption,created_at) VALUES(202,21,1,1000,0,'dup.mp4',300,'dup',10)`)
	if _, err := MergeShowInto(context.Background(), path, 2, 1, mergeBackupPath(t, path)); err == nil || !strings.Contains(err.Error(), "already exist") {
		t.Fatalf("err = %v", err)
	}
	var src int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&src)
	if src != 3 {
		t.Fatalf("sources = %d", src)
	}
}

// 备份路径已存在时必须拒绝，且不写库。
func TestMergeShowIntoRefusesExistingBackup(t *testing.T) {
	path, db := mergeSeed(t)
	backup := mergeBackupPath(t, path)
	if err := os.WriteFile(backup, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeShowInto(context.Background(), path, 2, 1, backup); err == nil {
		t.Fatal("merge should refuse an existing backup path")
	}
	var shows int
	_ = db.QueryRow(`SELECT COUNT(*) FROM shows`).Scan(&shows)
	if shows != 2 {
		t.Fatalf("shows = %d", shows)
	}
}
