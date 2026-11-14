package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tgdrama/internal/parser"
)

func nameRepairFixture(t *testing.T, ddl string) (string, *sql.DB) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "names # evidence.db")
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if ddl == "" {
		ddl = schemaSQL
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func nameRepairExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("fixture SQL: %v", err)
	}
}

func nameRepairSeed(t *testing.T, db *sql.DB, id int64, old, caption, filename string) {
	t.Helper()
	nameRepairExec(t, db, `INSERT OR IGNORE INTO chats(id,tg_chat_id,name,created_at) VALUES(1,101,'test',10)`)
	nameRepairExec(t, db, `INSERT INTO shows(id,name,norm_name,cover_path,intro,status,created_at,updated_at,hidden) VALUES(?,?,?,'unchanged-cover','unchanged-intro','已完结',11,12,1)`, id, old, parser.Normalize(old))
	nameRepairExec(t, db, `INSERT INTO episodes(id,show_id,episode_no,title,created_at,updated_at) VALUES(?,?,7,'unchanged-episode',13,14)`, id, id)
	nameRepairExec(t, db, `INSERT INTO sources(id,episode_id,chat_id,message_id,grouped_id,file_name,caption,created_at,file_path,file_size,duration,doc_id,access_hash,file_reference,download_status) VALUES(?,?,1,?,99,?,?,15,'cached-video',2048,123.4,456,789,X'010203','done')`, id, id, id, filename, caption)
	nameRepairExec(t, db, `INSERT INTO show_photos(id,show_id,chat_id,message_id,doc_id,caption,created_at,grouped_id,cache_path,access_hash,file_reference) VALUES(?,?,1,?,?,?,16,99,'cached-photo',888,X'040506')`, id, id, 1000+id, 1000+id, caption)
	nameRepairExec(t, db, `INSERT INTO history(episode_id,position,duration,finished,updated_at) VALUES(?,62.5,123.4,0,17)`, id)
	nameRepairExec(t, db, `INSERT INTO favorites(show_id,created_at,base_ep) VALUES(?,18,7)`, id)
	nameRepairExec(t, db, `INSERT OR IGNORE INTO tags(id,name) VALUES(1,'test-tag')`)
	nameRepairExec(t, db, `INSERT INTO show_tags(show_id,tag_id) VALUES(?,1)`, id)
	nameRepairExec(t, db, `INSERT OR IGNORE INTO kv(k,v) VALUES('rebuild_version','unchanged-version')`)
	nameRepairExec(t, db, `INSERT OR IGNORE INTO pending_msgs(chat_id,message_id,caption,info_json,created_at) VALUES(1,90000,'unrelated','opaque-do-not-read',19)`)
}

func nameRepairAudit(t *testing.T, path string) *NameAuditPlan {
	t.Helper()
	p, err := AuditNames(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func nameRepairCandidate(t *testing.T, p *NameAuditPlan, id int64) NameRepairCandidate {
	t.Helper()
	for _, c := range p.Candidates {
		if c.ShowID == id {
			return c
		}
	}
	t.Fatalf("show %d missing from candidates: %+v", id, p.Candidates)
	return NameRepairCandidate{}
}

func nameRepairStatus(t *testing.T, p *NameAuditPlan, id int64, status string) NameRepairCandidate {
	t.Helper()
	c := nameRepairCandidate(t, p, id)
	if c.Status != status {
		t.Fatalf("show %d status=%s want=%s reasons=%v evidence=%+v", id, c.Status, status, c.Reasons, c.Evidence)
	}
	return c
}

func nameRepairState(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	tables := []string{"chats", "shows", "episodes", "sources", "tags", "show_tags", "show_photos", "kv", "pending_msgs", "history", "favorites", "sqlite_sequence"}
	state := map[string]string{}
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + table + `" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var values [][]any
		for rows.Next() {
			row := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range row {
				ptrs[i] = &row[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, col := range cols {
				if table == "shows" && (col == "name" || col == "norm_name") {
					row[i] = "<allowed-name-field>"
				}
			}
			values = append(values, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		state[table] = string(b)
	}
	return state
}

func TestNameRepairReadOnlyAndMissingDatabase(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集\n剧情简介："+strings.Repeat("私密剧情不得输出", 100), "长夜微光 第2集.mp4")
	before := nameRepairState(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	bytesBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, _ := os.Stat(path)
	entriesBefore, _ := os.ReadDir(filepath.Dir(path))
	p := nameRepairAudit(t, path)
	c := nameRepairStatus(t, p, 1, "eligible")
	if c.NewName != "长夜微光" || !reflect.DeepEqual(c.SourceIDs, []int64{1}) || !reflect.DeepEqual(c.PhotoIDs, []int64{1}) {
		t.Fatalf("wrong preview: %+v", c)
	}
	out, _ := json.Marshal(p)
	if bytes.Contains(out, []byte("私密剧情")) || bytes.Contains(out, []byte("access_hash")) || bytes.Contains(out, []byte("opaque-do-not-read")) {
		t.Fatalf("preview leaked non-title content: %s", out)
	}
	bytesAfter, _ := os.ReadFile(path)
	infoAfter, _ := os.Stat(path)
	entriesAfter, _ := os.ReadDir(filepath.Dir(path))
	if !bytes.Equal(bytesBefore, bytesAfter) || !infoBefore.ModTime().Equal(infoAfter.ModTime()) || !reflect.DeepEqual(entriesBefore, entriesAfter) {
		t.Fatal("read-only audit modified database or created sidecars")
	}
	ro, err := openNameDatabase(context.Background(), path, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var qo int
	if err := ro.QueryRow(`PRAGMA query_only`).Scan(&qo); err != nil || qo != 1 {
		t.Fatalf("query_only=%d err=%v", qo, err)
	}
	if _, err := ro.Exec(`UPDATE shows SET name='not allowed'`); err == nil {
		t.Fatal("read-only connection allowed writes")
	}
	if got := nameRepairState(t, ro); !reflect.DeepEqual(got, before) {
		t.Fatal("read-only audit changed logical data")
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := AuditNames(context.Background(), missing); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing database was created: %v", err)
	}
}

func TestNameRepairUnsupportedSchemas(t *testing.T) {
	cases := []struct{ name, ddl, want string }{
		{"missing_tables", "CREATE TABLE unrelated(id INTEGER)", "unsupported schema"},
		{"missing_photo_caption", strings.Replace(schemaSQL, "caption        TEXT NOT NULL DEFAULT ''", "other_caption  TEXT NOT NULL DEFAULT ''", 1), "show_photos.caption"},
		{"missing_filename", strings.Replace(schemaSQL, "file_name       TEXT", "other_name      TEXT", 1), "sources.file_name"},
		{"generated_column", strings.Replace(schemaSQL, "intro       TEXT NOT NULL DEFAULT ''", "intro       TEXT GENERATED ALWAYS AS (name) VIRTUAL", 1), "generated show columns"},
		{"view_not_table", strings.Replace(schemaSQL, "CREATE TABLE IF NOT EXISTS shows (", "CREATE TABLE IF NOT EXISTS old_shows (", 1) + `CREATE VIEW shows AS SELECT * FROM old_shows;`, "table shows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {

			ddl := strings.Replace(tc.ddl, "CREATE INDEX IF NOT EXISTS idx_shows_updated    ON shows (updated_at);", "", 1)
			path, _ := nameRepairFixture(t, ddl)
			_, err := AuditNames(context.Background(), path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want explicit %q", err, tc.want)
			}
		})
	}
}

func TestNameRepairSameIDPreservesEverythingAndIdempotent(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
	before := nameRepairState(t, db)
	cache := filepath.Join(filepath.Dir(path), "cache-sentinel.bin")
	if err := os.WriteFile(cache, []byte("cache-unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	p := nameRepairAudit(t, path)
	nameRepairStatus(t, p, 1, "eligible")
	backup := filepath.Join(filepath.Dir(path), "backup.db")
	r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, backup)
	if err != nil || !reflect.DeepEqual(r.Applied, []int64{1}) || len(r.Skipped) != 0 {
		t.Fatalf("apply=%+v err=%v", r, err)
	}
	var id int64
	var name, norm string
	if err := db.QueryRow(`SELECT id,name,norm_name FROM shows`).Scan(&id, &name, &norm); err != nil || id != 1 || name != "长夜微光" || norm != "长夜微光" {
		t.Fatalf("wrong in-place repair: %d %q %q %v", id, name, norm, err)
	}
	if after := nameRepairState(t, db); !reflect.DeepEqual(before, after) {
		for table := range before {
			if before[table] != after[table] {
				t.Errorf("table %s changed: before=%s after=%s", table, before[table], after[table])
			}
		}
	}
	if b, _ := os.ReadFile(cache); string(b) != "cache-unchanged" {
		t.Fatal("cache file modified")
	}
	br, err := openNameDatabase(context.Background(), backup, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	if err := br.QueryRow(`SELECT name FROM shows WHERE id=1`).Scan(&name); err != nil || name != "长夜微光 第2集" {
		t.Fatalf("backup missing pre-apply name: %q %v", name, err)
	}
	if after := nameRepairState(t, br); !reflect.DeepEqual(before, after) {
		t.Fatal("backup did not preserve all original data")
	}
	second := filepath.Join(filepath.Dir(path), "unneeded-second-backup.db")
	r, err = ApplyNameRepairs(context.Background(), path, p, []int64{1}, second)
	if err != nil || len(r.Applied) != 0 || len(r.Skipped) != 1 || r.Skipped[0].Reason != "already_applied" {
		t.Fatalf("non-idempotent second apply=%+v err=%v", r, err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("idempotent apply created another backup")
	}
}

func TestNameRepairExistingTargetAndSharedCandidateConflicts(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "后悔有妻 第2集", "《后悔有妻》第2集", "竖屏(1).mp4")
	nameRepairSeed(t, db, 2, "后悔有妻", "《后悔有妻》第3集", "后悔有妻 第3集.mp4")
	nameRepairSeed(t, db, 3, "远山来信 第2集", "《远山来信》第2集", "远山来信 第2集.mp4")
	nameRepairSeed(t, db, 4, "远山来信 第3集", "《远山来信》第3集", "远山来信 第3集.mp4")
	p := nameRepairAudit(t, path)
	for _, id := range []int64{1, 3, 4} {
		c := nameRepairStatus(t, p, id, "conflict")
		if len(c.ConflictingShowIDs) == 0 {
			t.Fatalf("no conflict IDs for %d", id)
		}
	}
	before := nameRepairState(t, db)
	r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1, 3, 4}, filepath.Join(filepath.Dir(path), "not-needed.db"))
	if err != nil || len(r.Applied) != 0 || len(r.Skipped) != 3 {
		t.Fatalf("conflicts were not skipped: %+v %v", r, err)
	}
	if !reflect.DeepEqual(before, nameRepairState(t, db)) {
		t.Fatal("conflict apply altered data")
	}
}

func TestNameRepairEvidenceSafety(t *testing.T) {
	cases := []struct {
		name, old, caption, file, photo, status string
	}{
		{"explicit_range_legacy_marker", "乡下小子的姐姐 第", "《乡下小子的姐姐  第1-2集》", "竖屏(1).mp4", "《乡下小子的姐姐  第1-2集》", "eligible"},
		{"literal_suffix_not_trimmed", "书香门第", "《书香门》第1-2集", "竖屏(1).mp4", "《书香门》第1-2集", "review"},
		{"orphan_marker_without_range", "远山来信 第", "《远山来信》第1集", "竖屏(1).mp4", "《远山来信》第1集", "review"},
		{"unrelated_title", "长夜微光 第2集", "《远山来信》第2集", "远山来信 第2集.mp4", "《远山来信》第2集", "review"},
		{"filename_disagreement", "长夜微光 第2集", "《长夜微光》第2集", "远山来信 第2集.mp4", "《长夜微光》第2集", "review"},
		{"photo_disagreement", "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4", "《远山来信》第2集", "review"},
		{"missing_photo_caption", "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4", "", "review"},
		{"two_title_caption", "长夜微光 第2集", "《长夜微光》第2集 《远山来信》第3集", "长夜微光 第2集.mp4", "《长夜微光》第2集", "review"},
		{"multi_title_caption", "长夜微光 第2集", "《长夜微光》第2集 《远山来信》第3集 《南城旧事》第4集", "长夜微光 第2集.mp4", "《长夜微光》第2集", "review"},
		{"parse_failed_caption", "长夜微光 第2集", "#制服", "长夜微光 第2集.mp4", "《长夜微光》第2集", "review"},
		{"no_explicit_title", "长夜微光 第2集", "长夜微光", "", "长夜微光", "review"},
		{"title_tag_disagreement", "长夜微光 第2集", "《长夜微光》第2集 #远山来信", "长夜微光 第2集.mp4", "《长夜微光》第2集", "review"},
		{"explicit_title_with_footer", "乡下小子的姐姐 第", "《乡下小子的姐姐 第1-2集》\n点此开通 立刻进群追更", "竖屏(1).mp4", "《乡下小子的姐姐 第1-2集》\n点此开通 立刻进群追更", "eligible"},
		{"promotion_without_container", "长夜微光 第2集", "长夜微光 第2集\n点此开通 立刻进群追更", "长夜微光 第2集.mp4", "《长夜微光》第2集", "review"},
		{"filename_only_not_authorization", "长夜微光 第2集", "", "长夜微光 第2集.mp4", "长夜微光 第2集", "review"},
		{"tag_only_not_authorization", "长夜微光 第2集", "#长夜微光 第2集", "长夜微光 第2集.mp4", "#长夜微光 第2集", "review"},
		{"short_category_not_counterevidence", "乡下小子的姐姐 第", "《乡下小子的姐姐 第1-2集》\n#ai #色 #AI短剧 #乡下小子的姐姐\n点此开通", "竖屏(1).mp4", "《乡下小子的姐姐 第1-2集》\n#ai #色 #AI短剧 #乡下小子的姐姐\n点此开通", "eligible"},
		{"repeated_same_title_and_category", "后悔有妻 第", "《后悔有妻 第1-2集》\n#AI短剧 #群p #后悔有妻\n《后悔有妻》\n点此开通", "竖屏(1).mp4", "《后悔有妻 第1-2集》\n#AI短剧 #群p #后悔有妻\n点此开通", "eligible"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, db := nameRepairFixture(t, "")
			nameRepairSeed(t, db, 1, tc.old, tc.caption, tc.file)
			nameRepairExec(t, db, `UPDATE show_photos SET caption=?`, tc.photo)
			p := nameRepairAudit(t, path)
			nameRepairStatus(t, p, 1, tc.status)
		})
	}
}

func TestNameRepairWhitespaceOnlyIsReview(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "浪头女儿国我最大", "《浪头 女儿国我最大》第2集", "浪头 女儿国我最大 第2集.mp4")
	p := nameRepairAudit(t, path)
	c := nameRepairStatus(t, p, 1, "review")
	if c.OldNorm != c.NewNorm || !strings.Contains(strings.Join(c.Reasons, ","), "display_whitespace_only") {
		t.Fatalf("whitespace-only repair not labeled: %+v", c)
	}
	r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, filepath.Join(filepath.Dir(path), "unused.db"))
	if err != nil || len(r.Applied) != 0 || len(r.Skipped) != 1 {
		t.Fatalf("whitespace-only repair applied: %+v %v", r, err)
	}
}

func TestNameRepairNoopCandidateOmitted(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "大吊姐驾到 统统闪开", "", "大吊姐驾到 统统闪开 第2集.mp4")
	nameRepairExec(t, db, `INSERT INTO sources(id,episode_id,chat_id,message_id,caption,file_name,created_at) VALUES(2,1,1,2,'《大吊姐驾到统统闪开》第2集','1.mp4',20)`)
	for _, c := range nameRepairAudit(t, path).Candidates {
		if c.ShowID == 1 {
			t.Fatalf("no-op candidate should be omitted: %+v", c)
		}
	}
}

func TestNameRepairAllSourcesMatter(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			path, db := nameRepairFixture(t, "")
			nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
			caption := "《远山来信》第2集"
			if empty {
				caption = ""
			}
			nameRepairExec(t, db, `INSERT INTO sources(id,episode_id,chat_id,message_id,caption,file_name,created_at) VALUES(2,1,1,2,?,'1.mp4',20)`, caption)
			c := nameRepairStatus(t, nameRepairAudit(t, path), 1, "review")
			if !reflect.DeepEqual(c.SourceIDs, []int64{1, 2}) {
				t.Fatalf("ignored a source: %+v", c)
			}
		})
	}
}

func TestNameRepairLegacyRawMessages(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
	nameRepairExec(t, db, `CREATE TABLE raw_messages(chat_id INTEGER NOT NULL,message_id INTEGER NOT NULL,grouped_id INTEGER NOT NULL DEFAULT 0,text TEXT NOT NULL,info_json TEXT NOT NULL DEFAULT '',PRIMARY KEY(chat_id,message_id))`)
	nameRepairExec(t, db, `INSERT INTO raw_messages(chat_id,message_id,text) VALUES(1,1,'《长夜微光》第2集'),(1,1001,'《长夜微光》第2集')`)
	nameRepairExec(t, db, `ALTER TABLE sources DROP COLUMN caption`)
	nameRepairExec(t, db, `ALTER TABLE show_photos DROP COLUMN caption`)
	nameRepairExec(t, db, `ALTER TABLE show_photos DROP COLUMN grouped_id`)
	p := nameRepairAudit(t, path)
	c := nameRepairStatus(t, p, 1, "eligible")
	found := false
	for _, e := range c.Evidence {
		found = found || e.Field == "raw_messages.text"
	}
	if !found {
		t.Fatal("legacy raw evidence omitted")
	}
	nameRepairExec(t, db, `UPDATE raw_messages SET text='《远山来信》第2集' WHERE message_id=1001`)
	r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, filepath.Join(filepath.Dir(path), "unused.db"))
	if err != nil || len(r.Skipped) != 1 || r.Skipped[0].Reason != "stale_evidence" {
		t.Fatalf("changed legacy evidence not rejected: %+v %v", r, err)
	}
}

func TestNameRepairStaleEvidenceAndValues(t *testing.T) {
	for _, tc := range []struct{ name, sql, reason string }{
		{"caption", `UPDATE sources SET caption='《长夜微光》第2集 新信息'`, "stale_evidence"},
		{"photo", `UPDATE show_photos SET caption='《长夜微光》第2集 新信息'`, "stale_evidence"},
		{"filename", `UPDATE sources SET file_name='长夜微光 第3集.mp4'`, "stale_evidence"},
		{"source_added", `INSERT INTO sources(episode_id,chat_id,message_id,file_name,caption,created_at) VALUES(1,1,3,'长夜微光.mp4','《长夜微光》第3集',20)`, "stale_evidence"},
		{"photo_removed", `DELETE FROM show_photos`, "stale_evidence"},
		{"episode", `UPDATE episodes SET episode_no=8`, "stale_evidence"},
		{"old_value", `UPDATE shows SET name='changed-by-someone-else'`, "stale_old_values"},
		{"new_conflict", `INSERT INTO shows(name,norm_name,created_at,updated_at) VALUES('长夜微光','长夜微光',20,20)`, "current_evidence_or_conflict_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, db := nameRepairFixture(t, "")
			nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
			p := nameRepairAudit(t, path)
			nameRepairExec(t, db, tc.sql)
			backup := filepath.Join(filepath.Dir(path), "unused.db")
			r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, backup)
			if err != nil || len(r.Applied) != 0 || len(r.Skipped) != 1 || r.Skipped[0].Reason != tc.reason {
				t.Fatalf("stale plan not rejected: %+v %v", r, err)
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Fatal("stale-only apply created backup")
			}
		})
	}
}

func TestNameRepairSelectionAndMixedStaleSkips(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	for i, name := range []string{"长夜微光", "远山来信", "南城旧事"} {
		nameRepairSeed(t, db, int64(i+1), name+" 第2集", "《"+name+"》第2集", name+" 第2集.mp4")
	}
	p := nameRepairAudit(t, path)
	nameRepairExec(t, db, `UPDATE sources SET caption='《远山来信》第3集' WHERE id=2`)
	r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1, 2}, filepath.Join(filepath.Dir(path), "selected.db"))
	if err != nil || !reflect.DeepEqual(r.Applied, []int64{1}) || len(r.Skipped) != 1 || r.Skipped[0].ShowID != 2 || r.Skipped[0].Reason != "stale_evidence" {
		t.Fatalf("wrong selective apply: %+v %v", r, err)
	}
	for id, want := range map[int64]string{1: "长夜微光", 2: "远山来信 第2集", 3: "南城旧事 第2集"} {
		var got string
		if err := db.QueryRow(`SELECT name FROM shows WHERE id=?`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("show %d=%q want=%q err=%v", id, got, want, err)
		}
	}
}

func TestNameRepairRollbackWholeBatch(t *testing.T) {

	ddl := strings.Replace(schemaSQL, "name        TEXT NOT NULL,", "name        TEXT NOT NULL CHECK (name <> '远山来信'),", 1)
	path, db := nameRepairFixture(t, ddl)
	nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
	nameRepairSeed(t, db, 2, "远山来信 第2集", "《远山来信》第2集", "远山来信 第2集.mp4")
	p := nameRepairAudit(t, path)
	nameRepairStatus(t, p, 1, "eligible")
	nameRepairStatus(t, p, 2, "eligible")
	before := nameRepairState(t, db)
	r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1, 2}, filepath.Join(filepath.Dir(path), "backup.db"))
	if err == nil || len(r.Applied) != 0 || len(r.Skipped) != 2 || r.Backup == "" {
		t.Fatalf("expected actual transaction failure: %+v %v", r, err)
	}
	for _, skip := range r.Skipped {
		if skip.Reason != "transaction_rolled_back" {
			t.Fatal(skip)
		}
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM shows WHERE id=1`).Scan(&name); err != nil || name != "长夜微光 第2集" {
		t.Fatalf("first update was not rolled back: %q %v", name, err)
	}
	if !reflect.DeepEqual(before, nameRepairState(t, db)) {
		t.Fatal("rollback changed unrelated data")
	}
}

func TestNameRepairBackupIncludesWALAndRefusesExistingPath(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairExec(t, db, `PRAGMA journal_mode=WAL`)
	nameRepairExec(t, db, `PRAGMA wal_autocheckpoint=0`)
	nameRepairExec(t, db, `PRAGMA wal_checkpoint(TRUNCATE)`)
	mainBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
	nameRepairExec(t, db, `INSERT INTO kv(k,v) VALUES('wal-only','must-be-in-backup')`)
	mainAfter, _ := os.ReadFile(path)
	if !bytes.Equal(mainBefore, mainAfter) {
		t.Fatal("fixture should have committed only to WAL, not the main file")
	}
	walInfo, err := os.Stat(path + "-wal")
	if err != nil || walInfo.Size() <= 32 {
		t.Fatalf("fixture has no populated WAL: %v", err)
	}
	walBeforeAudit, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	p := nameRepairAudit(t, path)
	walAfterAudit, _ := os.ReadFile(path + "-wal")
	mainAfterAudit, _ := os.ReadFile(path)
	if !bytes.Equal(walBeforeAudit, walAfterAudit) || !bytes.Equal(mainAfter, mainAfterAudit) {
		t.Fatal("read-only audit wrote database or WAL")
	}
	backup := filepath.Join(filepath.Dir(path), "existing.db")
	for _, contents := range []string{"", "do-not-overwrite"} {
		if err := os.WriteFile(backup, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, backup)
		if err == nil || len(r.Applied) != 0 {
			t.Fatalf("existing backup target accepted: %+v %v", r, err)
		}
		if b, _ := os.ReadFile(backup); string(b) != contents {
			t.Fatal("existing backup target overwritten")
		}
		var name string
		if err := db.QueryRow(`SELECT name FROM shows WHERE id=1`).Scan(&name); err != nil || name != "长夜微光 第2集" {
			t.Fatal("backup failure did not prevent update")
		}
	}
	backup = filepath.Join(filepath.Dir(path), "wal-backup.db")
	r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, backup)
	if err != nil || len(r.Applied) != 1 {
		t.Fatalf("apply with WAL backup: %+v %v", r, err)
	}
	br, err := openNameDatabase(context.Background(), backup, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	var value, name string
	if err := br.QueryRow(`SELECT v FROM kv WHERE k='wal-only'`).Scan(&value); err != nil || value != "must-be-in-backup" {
		t.Fatalf("backup excluded WAL data: %q %v", value, err)
	}
	if err := br.QueryRow(`SELECT name FROM shows WHERE id=1`).Scan(&name); err != nil || name != "长夜微光 第2集" {
		t.Fatalf("backup does not contain pre-apply show: %q %v", name, err)
	}
}

func TestNameRepairLegacyAndCurrentEvidenceAreBothChecked(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
	nameRepairExec(t, db, `CREATE TABLE raw_messages(chat_id INTEGER,message_id INTEGER,text TEXT)`)
	nameRepairExec(t, db, `INSERT INTO raw_messages VALUES(1,1,'《远山来信》第2集')`)
	c := nameRepairStatus(t, nameRepairAudit(t, path), 1, "review")
	foundCurrent, foundRaw := false, false
	for _, e := range c.Evidence {
		foundCurrent = foundCurrent || e.Field == "caption"
		foundRaw = foundRaw || e.Field == "raw_messages.text"
	}
	if !foundCurrent || !foundRaw {
		t.Fatal("mixed schema evidence was silently discarded")
	}
	nameRepairExec(t, db, `INSERT INTO raw_messages VALUES(1,1,'《长夜微光》第2集')`)
	if _, err := AuditNames(context.Background(), path); err == nil || !strings.Contains(err.Error(), "duplicate raw_messages") {
		t.Fatalf("ambiguous raw message keys accepted: %v", err)
	}
}

func TestNameRepairUnsafeBackupPathsAndNameForeignKeys(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
	p := nameRepairAudit(t, path)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, path+suffix)
		if err == nil || len(r.Applied) != 0 || len(r.Skipped) != 1 || r.Skipped[0].Reason != "backup_failed" {
			t.Fatalf("source sidecar accepted as backup: %+v %v", r, err)
		}
	}
	backup := filepath.Join(filepath.Dir(path), "new.db")
	if err := os.WriteFile(backup+"-wal", []byte("existing-sidecar"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, backup); err == nil {
		t.Fatal("destination with existing sidecar accepted")
	}
	if b, _ := os.ReadFile(backup + "-wal"); string(b) != "existing-sidecar" {
		t.Fatal("backup overwrote existing sidecar")
	}
	nameRepairExec(t, db, `CREATE TABLE custom_name_reference(name TEXT REFERENCES shows(norm_name) ON UPDATE CASCADE)`)
	p = nameRepairAudit(t, path)
	if _, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, filepath.Join(filepath.Dir(path), "fk.db")); err == nil || !strings.Contains(err.Error(), "foreign keys") {
		t.Fatalf("mutable name foreign-key schema accepted: %v", err)
	}
}

func TestNameRepairTamperedPlansAndTriggers(t *testing.T) {
	path, db := nameRepairFixture(t, "")
	nameRepairSeed(t, db, 1, "长夜微光 第2集", "《长夜微光》第2集", "长夜微光 第2集.mp4")
	p := nameRepairAudit(t, path)
	p.Candidates[0].NewName, p.Candidates[0].NewNorm = "任意不相关名称", "任意不相关名称"
	r, err := ApplyNameRepairs(context.Background(), path, p, []int64{1}, filepath.Join(filepath.Dir(path), "unused.db"))
	if err != nil || len(r.Applied) != 0 || len(r.Skipped) != 1 {
		t.Fatalf("tampered target accepted: %+v %v", r, err)
	}
	nameRepairExec(t, db, `CREATE TRIGGER unrelated_effect AFTER UPDATE ON shows BEGIN UPDATE favorites SET base_ep=999; END`)
	p = nameRepairAudit(t, path)
	_, err = ApplyNameRepairs(context.Background(), path, p, []int64{1}, filepath.Join(filepath.Dir(path), "unused.db"))
	if err == nil || !strings.Contains(err.Error(), "triggers") {
		t.Fatalf("unsafe trigger schema accepted: %v", err)
	}
	var base int
	if err := db.QueryRow(`SELECT base_ep FROM favorites`).Scan(&base); err != nil || base != 7 {
		t.Fatalf("trigger mutated favorites: %d %v", base, err)
	}
}
