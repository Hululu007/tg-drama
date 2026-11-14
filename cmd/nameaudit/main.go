package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tgdrama/internal/storage"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "nameaudit:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("nameaudit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "existing SQLite file (required; default operation is read-only)")
	output := fs.String("output", "", "write JSON to a NEW file instead of stdout; never overwrite")
	apply := fs.Bool("apply", false, "apply selected eligible name-only repairs, offline, after backup")
	idsFlag := fs.String("ids", "", "comma-separated show IDs to apply (required with -apply)")
	planPath := fs.String("plan", "", "JSON preview produced by this command (required with -apply)")
	backup := fs.String("backup", "", "NEW consistent SQLite backup file (required with -apply or merge)")
	mergeFrom := fs.Int64("merge-from", 0, "duplicate show ID to merge away (requires -merge-into and -backup)")
	mergeInto := fs.Int64("merge-into", 0, "target show ID that keeps its identity")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	merging := *mergeFrom != 0 || *mergeInto != 0
	if fs.NArg() != 0 || strings.TrimSpace(*db) == "" {
		return errors.New("usage: nameaudit -db <existing.db> [-output <new-plan.json>]; apply requires -apply -ids <IDs> -plan <plan.json> -backup <new.db>; merge requires -merge-from <ID> -merge-into <ID> -backup <new.db>")
	}
	if merging && *apply {
		return errors.New("-merge-from/-merge-into cannot be combined with -apply")
	}
	if merging {
		if *mergeFrom <= 0 || *mergeInto <= 0 || *mergeFrom == *mergeInto || *backup == "" {
			return errors.New("merge requires -merge-from, -merge-into and -backup; the two IDs must differ")
		}
		if *idsFlag != "" || *planPath != "" {
			return errors.New("-ids and -plan are only valid with -apply")
		}
	} else if !*apply && (*idsFlag != "" || *planPath != "" || *backup != "") {
		return errors.New("-ids, -plan and -backup are only valid with explicit -apply or merge")
	}
	var plan storage.NameAuditPlan
	var ids []int64
	if *apply {
		if *planPath == "" || *backup == "" || *idsFlag == "" {
			return errors.New("-apply requires -ids, -plan and -backup; no implicit apply-all")
		}
		seen := map[int64]bool{}
		for _, value := range strings.Split(*idsFlag, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || id <= 0 || seen[id] {
				return errors.New("-ids must contain unique positive show IDs")
			}
			seen[id] = true
			ids = append(ids, id)
		}
		f, err := os.Open(*planPath)
		if err != nil {
			return fmt.Errorf("read preview: %w", err)
		}
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
			f.Close()
			return errors.New("preview must be a regular JSON file no larger than 64 MiB")
		}
		dec := json.NewDecoder(io.LimitReader(f, 64<<20))
		dec.DisallowUnknownFields()
		err = dec.Decode(&plan)
		if err == nil {
			var extra any
			if next := dec.Decode(&extra); next != io.EOF {
				err = errors.New("preview must contain exactly one JSON object")
			}
		}
		f.Close()
		if err != nil {
			return fmt.Errorf("invalid preview: %w", err)
		}
	}
	var out *os.File
	if *output != "" {
		var err error
		out, err = newOutput(*output, *db, *planPath, *backup)
		if err != nil {
			return err
		}
		defer out.Close()
		stdout = out
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	writeJSON := func(value any) error {
		if err := enc.Encode(value); err != nil {
			return err
		}
		if out != nil {
			return out.Sync()
		}
		return nil
	}
	if merging {
		result, err := storage.MergeShowInto(ctx, *db, *mergeFrom, *mergeInto, *backup)
		if result != nil {
			if writeErr := writeJSON(result); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		return err
	}
	if *apply {
		result, err := storage.ApplyNameRepairs(ctx, *db, &plan, ids, *backup)
		if result != nil {
			if writeErr := writeJSON(result); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		return err
	}
	preview, err := storage.AuditNames(ctx, *db)
	if err != nil {
		return err
	}
	return writeJSON(preview)
}

func newOutput(path string, protected ...string) (*os.File, error) {
	absolute := func(p string) (string, error) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return "", err
		}
		return filepath.Join(parent, filepath.Base(abs)), nil
	}
	dest, err := absolute(path)
	if err != nil {
		return nil, err
	}
	for _, p := range protected {
		if p == "" {
			continue
		}
		abs, err := absolute(p)
		if err != nil {
			return nil, err
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if strings.EqualFold(dest, abs+suffix) {
				return nil, errors.New("output must not use an input, backup, or SQLite sidecar path")
			}
		}
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("create new output (existing paths are refused): %w", err)
	}
	return f, nil
}
