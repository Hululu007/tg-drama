package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgdrama/internal/storage"
)

func cliNameFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "library.db")
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
CREATE TABLE shows(id INTEGER PRIMARY KEY, name TEXT NOT NULL, norm_name TEXT NOT NULL UNIQUE, hidden INTEGER);
CREATE TABLE episodes(id INTEGER PRIMARY KEY, show_id INTEGER NOT NULL, episode_no INTEGER NOT NULL);
CREATE TABLE sources(id INTEGER PRIMARY KEY, episode_id INTEGER NOT NULL, chat_id INTEGER, message_id INTEGER, grouped_id INTEGER, file_name TEXT, caption TEXT);
CREATE TABLE show_photos(id INTEGER PRIMARY KEY, show_id INTEGER NOT NULL, chat_id INTEGER, message_id INTEGER, caption TEXT);
INSERT INTO shows VALUES(1,'长夜微光 第2集','长夜微光第2集',1);
INSERT INTO episodes VALUES(1,1,2);
INSERT INTO sources VALUES(1,1,1,1,1,'长夜微光 第2集.mp4','《长夜微光》第2集');
INSERT INTO show_photos VALUES(1,1,1,2,'《长夜微光》第2集');
`)
	if err != nil {
		t.Fatal(err)
	}
	return path, db
}

func cliRunName(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := run(context.Background(), args, &out, &stderr)
	return out.Bytes(), err
}

func TestNameAuditDefaultPreviewAndOutput(t *testing.T) {
	path, db := cliNameFixture(t)
	out, err := cliRunName(t, "-db", path)
	if err != nil {
		t.Fatal(err)
	}
	var p storage.NameAuditPlan
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("invalid JSON stdout: %v %s", err, out)
	}
	if len(p.Candidates) != 1 || p.Candidates[0].Status != "eligible" || p.Candidates[0].NewName != "长夜微光" {
		t.Fatalf("incorrect preview: %+v", p)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM shows WHERE id=1`).Scan(&name); err != nil || name != "长夜微光 第2集" {
		t.Fatalf("default run wrote database: %q %v", name, err)
	}
	plan := filepath.Join(filepath.Dir(path), "plan.json")
	if out, err := cliRunName(t, "-db", path, "-output", plan); err != nil || len(out) != 0 {
		t.Fatalf("explicit output failed: %s %v", out, err)
	}
	b, err := os.ReadFile(plan)
	if err != nil || !json.Valid(b) {
		t.Fatalf("invalid output file: %v", err)
	}
	if _, err := cliRunName(t, "-db", path, "-output", plan); err == nil {
		t.Fatal("existing output was overwritten")
	}
	after, _ := os.ReadFile(plan)
	if !bytes.Equal(b, after) {
		t.Fatal("existing output content changed")
	}
}

func TestNameAuditApplyRequiresExplicitSelectionAndPlan(t *testing.T) {
	path, db := cliNameFixture(t)
	cases := [][]string{
		{}, {"-apply"}, {"-apply", "-db", path},
		{"-db", path, "-ids", "1"},
		{"-db", path, "-plan", "plan.json"},
		{"-db", path, "-backup", "backup.db"},
		{"-db", path, "-apply", "-ids", "1"},
		{"-db", path, "-apply", "-ids", "1,1", "-plan", "missing", "-backup", "missing"},
		{"-db", path, "-apply", "-ids", "0", "-plan", "missing", "-backup", "missing"},
		{"-db", path, "-apply", "-ids", "1", "-plan", "missing", "-backup", "missing"},
		{"-db", path, "unexpected-argument"},
	}
	for _, args := range cases {
		if _, err := cliRunName(t, args...); err == nil {
			t.Errorf("invalid args accepted: %q", args)
		}
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM shows WHERE id=1`).Scan(&name); err != nil || name != "长夜微光 第2集" {
		t.Fatalf("invalid args changed database: %q %v", name, err)
	}
}

func TestNameAuditCLIApplyAndRepeat(t *testing.T) {
	path, db := cliNameFixture(t)
	plan := filepath.Join(filepath.Dir(path), "plan.json")
	backup := filepath.Join(filepath.Dir(path), "before.db")
	if _, err := cliRunName(t, "-db", path, "-output", plan); err != nil {
		t.Fatal(err)
	}
	out, err := cliRunName(t, "-db", path, "-apply", "-ids", "1", "-plan", plan, "-backup", backup)
	var result storage.NameRepairResult
	if err != nil || json.Unmarshal(out, &result) != nil || len(result.Applied) != 1 || result.Applied[0] != 1 {
		t.Fatalf("apply output=%s err=%v", out, err)
	}
	var name string
	var hidden int
	if err := db.QueryRow(`SELECT name,hidden FROM shows WHERE id=1`).Scan(&name, &hidden); err != nil || name != "长夜微光" || hidden != 1 {
		t.Fatalf("wrong repaired values: %q %d %v", name, hidden, err)
	}
	out, err = cliRunName(t, "-db", path, "-apply", "-ids", "1", "-plan", plan, "-backup", backup)
	if err != nil || json.Unmarshal(out, &result) != nil || len(result.Applied) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != "already_applied" {
		t.Fatalf("repeat output=%s err=%v", out, err)
	}
}

func TestNameAuditMissingDBAndUnsafeOutputPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := cliRunName(t, "-db", missing); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing database was created: %v", err)
	}
	path, _ := cliNameFixture(t)
	for _, output := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if _, err := cliRunName(t, "-db", path, "-output", output); err == nil {
			t.Fatalf("unsafe output accepted: %s", output)
		}
	}
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Fatal("output path created a database sidecar")
	}
	if _, err := cliRunName(t, "-db", missing, "-output", missing); err == nil {
		t.Fatal("output matching missing input accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("output created missing input database")
	}
}

func TestNameAuditBadPlanRejectedBeforeApply(t *testing.T) {
	path, _ := cliNameFixture(t)
	plan := filepath.Join(filepath.Dir(path), "bad-plan.json")
	for _, contents := range []string{`{`, `{"version":1,"unexpected":"reject"}`, `{} {}`, strings.Repeat(" ", 10)} {
		if err := os.WriteFile(plan, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := cliRunName(t, "-db", path, "-apply", "-ids", "1", "-plan", plan, "-backup", filepath.Join(filepath.Dir(path), "backup.db"))
		if err == nil {
			t.Fatalf("invalid preview accepted: %q", contents)
		}
	}
}

func cliMergeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "merge.db")
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE shows(id INTEGER PRIMARY KEY, name TEXT NOT NULL, norm_name TEXT NOT NULL UNIQUE, cover_path TEXT NOT NULL DEFAULT '', hidden INTEGER NOT NULL DEFAULT 0);
CREATE TABLE episodes(id INTEGER PRIMARY KEY, show_id INTEGER NOT NULL, episode_no INTEGER NOT NULL, UNIQUE(show_id,episode_no));
CREATE TABLE sources(id INTEGER PRIMARY KEY, episode_id INTEGER NOT NULL, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, segment_index INTEGER NOT NULL DEFAULT 0, file_name TEXT NOT NULL DEFAULT '', caption TEXT NOT NULL DEFAULT '', UNIQUE(chat_id,message_id,segment_index));
CREATE TABLE show_photos(id INTEGER PRIMARY KEY, show_id INTEGER NOT NULL, chat_id INTEGER, message_id INTEGER, doc_id INTEGER, caption TEXT);
CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE show_tags(show_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, PRIMARY KEY(show_id,tag_id));
CREATE TABLE favorites(show_id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL DEFAULT 0, base_ep INTEGER NOT NULL DEFAULT 0);
CREATE TABLE history(episode_id INTEGER PRIMARY KEY, position REAL NOT NULL DEFAULT 0, duration REAL NOT NULL DEFAULT 0, finished INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0);
INSERT INTO shows VALUES(1,'后悔有妻','后悔有妻','',0);
INSERT INTO shows VALUES(2,'后悔有妻 第','后悔有妻第','',0);
INSERT INTO episodes VALUES(11,1,1);
INSERT INTO episodes VALUES(21,2,1);
INSERT INTO sources VALUES(101,11,1,1000,0,'a.mp4','《后悔有妻》第1集');
INSERT INTO sources VALUES(201,21,2,2000,0,'竖屏(1).mp4','《后悔有妻  第1-2集》');
INSERT INTO show_photos VALUES(31,2,2,2001,9001,'');
INSERT INTO tags VALUES(1,'tag');
INSERT INTO show_tags VALUES(2,1);
INSERT INTO favorites VALUES(2,10,1);
`)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNameAuditMergeFlagsAndExecution(t *testing.T) {
	path := cliMergeFixture(t)
	if _, err := cliRunName(t, "-db", path, "-merge-from", "2", "-merge-into", "1"); err == nil {
		t.Fatal("merge without -backup must fail")
	}
	if _, err := cliRunName(t, "-db", path, "-merge-from", "2", "-merge-into", "2", "-backup", filepath.Join(filepath.Dir(path), "b.db")); err == nil {
		t.Fatal("merge with identical IDs must fail")
	}
	if _, err := cliRunName(t, "-db", path, "-merge-from", "2", "-merge-into", "1", "-backup", filepath.Join(filepath.Dir(path), "b.db"), "-apply"); err == nil {
		t.Fatal("merge combined with -apply must fail")
	}
	backup := filepath.Join(filepath.Dir(path), "merge-backup.db")
	out, err := cliRunName(t, "-db", path, "-merge-from", "2", "-merge-into", "1", "-backup", backup)
	if err != nil {
		t.Fatalf("merge failed: %v (%s)", err, out)
	}
	var res storage.MergeShowResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid JSON: %v %s", err, out)
	}
	if res.MovedSources != 1 || res.MergedEpisodes != 1 || res.Backup != backup {
		t.Fatalf("result = %+v", res)
	}
}
