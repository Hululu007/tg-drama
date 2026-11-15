package storage

// This is deliberately independent of Open, migrations, rebuilds and Telegram.
// The only database mutation this tool performs is the final, guarded UPDATE.
import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"tgdrama/internal/parser"
)

const NameAuditVersion = 1
const nameAuditPolicy = "parser-name-only-v1"

// NameAuditPlan is both a redacted preview and the input to an explicitly
// selected apply. It contains no raw captions, file paths or Telegram secrets.
// Treat plans as local files: evidence hashes are freshness checks, not signatures.
type NameAuditPlan struct {
	Version           int                   `json:"version"`
	Policy            string                `json:"policy"`
	Database          string                `json:"database"`
	GeneratedAt       string                `json:"generated_at"`
	SchemaFingerprint string                `json:"schema_fingerprint"`
	ShowsScanned      int                   `json:"shows_scanned"`
	Candidates        []NameRepairCandidate `json:"candidates"`
}

type NameRepairCandidate struct {
	ShowID              int64               `json:"show_id"`
	OldName             string              `json:"old_name"`
	OldNorm             string              `json:"old_normalized"`
	NewName             string              `json:"new_name"`
	NewNorm             string              `json:"new_normalized"`
	Status              string              `json:"status"` // eligible, review, conflict
	Reasons             []string            `json:"reasons"`
	SourceIDs           []int64             `json:"source_ids"`
	PhotoIDs            []int64             `json:"photo_ids"`
	Evidence            []NameTitleEvidence `json:"evidence"`
	EvidenceFingerprint string              `json:"evidence_fingerprint"`
	ConflictingShowIDs  []int64             `json:"conflicting_show_ids,omitempty"`
}

// Title is a bounded parser result, never an excerpt of the full plot.
type NameTitleEvidence struct {
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	Field    string `json:"field"`
	Title    string `json:"title,omitempty"`
	Norm     string `json:"normalized,omitempty"`
	Explicit bool   `json:"explicit"`
	Issue    string `json:"issue,omitempty"`
}

type NameRepairResult struct {
	Backup  string           `json:"backup,omitempty"`
	Applied []int64          `json:"applied"`
	Skipped []NameRepairSkip `json:"skipped"`
}

type NameRepairSkip struct {
	ShowID int64  `json:"show_id"`
	Reason string `json:"reason"`
}

type nameQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type nameAuditSchema struct {
	columns     map[string]map[string]bool
	fingerprint string
	showTrigger bool
}

type nameInput struct {
	Kind                          string
	ID, EpisodeID, EpisodeNo      int64
	ChatID, MessageID, GroupedID  int64
	FileName, Caption, RawCaption string
	RawPresent                    bool
}

type nameShow struct {
	id          int64
	name, norm  string
	inputs      []nameInput
	fingerprint string
}

func nameDatabasePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("-db must name an existing SQLite file")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("open existing database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("database must be a regular file")
	}
	// Resolve symlinks so a plan cannot silently switch between different files.
	return filepath.EvalSymlinks(abs)
}

func openNameDatabase(ctx context.Context, path string, writable, queryOnly bool) (*sql.DB, error) {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // file:///C:/... on Windows; URL escapes ?, # and spaces.
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}
	if writable {
		q.Set("mode", "rw") // never create a missing database
		q.Set("_txlock", "immediate")
	}
	if queryOnly {
		q.Add("_pragma", "query_only(1)")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// AuditNames opens only an existing database, with BOTH mode=ro and query_only.
// A read transaction supplies a consistent view, including committed WAL data.
func AuditNames(ctx context.Context, path string) (*NameAuditPlan, error) {
	abs, err := nameDatabasePath(path)
	if err != nil {
		return nil, err
	}
	db, err := openNameDatabase(ctx, abs, false, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	plan, _, _, err := auditNameSnapshot(ctx, tx, abs)
	return plan, err
}

func inspectNameSchema(ctx context.Context, q nameQuery) (*nameAuditSchema, error) {
	s := &nameAuditSchema{columns: map[string]map[string]bool{}}
	rows, err := q.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("unsupported schema: %w", err)
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	tables := map[string]string{}
	for rows.Next() {
		var kind, name, table, ddl string
		if err := rows.Scan(&kind, &name, &table, &ddl); err != nil {
			rows.Close()
			return nil, err
		}
		_ = enc.Encode([]string{kind, name, table, ddl})
		if kind == "table" {
			tables[name] = ddl
		}
		if kind == "trigger" && table == "shows" {
			s.showTrigger = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	s.fingerprint = hex.EncodeToString(h.Sum(nil))
	required := map[string][]string{
		"shows":       {"id", "name", "norm_name"},
		"episodes":    {"id", "show_id", "episode_no"},
		"sources":     {"id", "episode_id", "chat_id", "message_id", "file_name", "grouped_id"},
		"show_photos": {"id", "show_id", "chat_id", "message_id"},
	}
	if _, ok := tables["raw_messages"]; ok {
		required["raw_messages"] = []string{"chat_id", "message_id", "text"}
	}
	for _, table := range []string{"shows", "episodes", "sources", "show_photos", "raw_messages"} {
		cols, wanted := required[table]
		if !wanted {
			continue
		}
		ddl, exists := tables[table]
		if !exists || strings.Contains(strings.ToUpper(ddl), "CREATE VIRTUAL TABLE") {
			return nil, fmt.Errorf("unsupported schema: missing ordinary table %s", table)
		}
		r, err := q.QueryContext(ctx, `PRAGMA table_xinfo("`+table+`")`)
		if err != nil {
			return nil, err
		}
		found := map[string]bool{}
		idPK, pkCount := false, 0
		for r.Next() {
			var cid, notNull, pk, hidden int
			var name, typ string
			var def any
			if err := r.Scan(&cid, &name, &typ, &notNull, &def, &pk, &hidden); err != nil {
				r.Close()
				return nil, err
			}
			if table == "shows" && hidden != 0 {
				r.Close()
				return nil, errors.New("unsupported schema: generated show columns may change during a rename")
			}
			found[name] = hidden == 0
			if pk > 0 {
				pkCount++
			}
			if name == "id" && pk == 1 && strings.EqualFold(typ, "INTEGER") {
				idPK = true
			}
		}
		err = r.Err()
		r.Close()
		if err != nil {
			return nil, err
		}
		for _, col := range cols {
			if !found[col] {
				return nil, fmt.Errorf("unsupported schema: missing ordinary column %s.%s", table, col)
			}
		}
		if table != "raw_messages" && (!idPK || pkCount != 1) {
			return nil, fmt.Errorf("unsupported schema: %s.id must be an INTEGER primary key", table)
		}
		s.columns[table] = found
	}
	for _, table := range []string{"sources", "show_photos"} {
		if !s.columns[table]["caption"] && s.columns["raw_messages"] == nil {
			return nil, fmt.Errorf("unsupported schema: %s.caption and raw_messages evidence are both absent", table)
		}
	}
	if s.columns["raw_messages"] != nil {
		var duplicate int
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM raw_messages GROUP BY chat_id, message_id HAVING COUNT(*) > 1)`).Scan(&duplicate)
		if err != nil {
			return nil, err
		}
		if duplicate != 0 {
			return nil, errors.New("unsupported schema: ambiguous duplicate raw_messages keys")
		}
	}
	return s, nil
}

func auditNameSnapshot(ctx context.Context, q nameQuery, path string) (*NameAuditPlan, map[int64]*nameShow, *nameAuditSchema, error) {
	schema, err := inspectNameSchema(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	shows := map[int64]*nameShow{}
	var ids []int64
	rows, err := q.QueryContext(ctx, `SELECT id, name, norm_name FROM shows ORDER BY id`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		s := &nameShow{}
		if err := rows.Scan(&s.id, &s.name, &s.norm); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		shows[s.id] = s
		ids = append(ids, s.id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	// Empty episodes are retained in the evidence hash and block auto-repair.
	rows, err = q.QueryContext(ctx, `SELECT e.show_id, e.id, e.episode_no FROM episodes e WHERE NOT EXISTS (SELECT 1 FROM sources s WHERE s.episode_id=e.id) ORDER BY e.id`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var showID int64
		i := nameInput{Kind: "episode"}
		if err := rows.Scan(&showID, &i.ID, &i.EpisodeNo); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		if s := shows[showID]; s != nil {
			s.inputs = append(s.inputs, i)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, table := range []string{"sources", "show_photos"} {
		caption, grouped := "''", "0"
		if schema.columns[table]["caption"] {
			caption = "t.caption"
		}
		if schema.columns[table]["grouped_id"] {
			grouped = "t.grouped_id"
		}
		rawText, rawPresent, join := "''", "0", ""
		if schema.columns["raw_messages"] != nil {
			rawText, rawPresent = "COALESCE(r.text, '')", "r.message_id IS NOT NULL"
			join = ` LEFT JOIN raw_messages r ON r.chat_id=t.chat_id AND r.message_id=t.message_id`
		}
		from, show, epID, epNo, file, kind := "show_photos t", "t.show_id", "0", "0", "''", "photo"
		if table == "sources" {
			from, show, epID, epNo, file, kind = "sources t JOIN episodes e ON e.id=t.episode_id", "e.show_id", "e.id", "e.episode_no", "t.file_name", "source"
		}
		query := `SELECT ` + show + `, t.id, ` + epID + `, ` + epNo + `, t.chat_id, t.message_id, ` + grouped + `, ` + file + `, ` + caption + `, ` + rawText + `, ` + rawPresent + ` FROM ` + from + join + ` ORDER BY t.id`
		rows, err := q.QueryContext(ctx, query)
		if err != nil {
			return nil, nil, nil, err
		}
		for rows.Next() {
			var showID int64
			i := nameInput{Kind: kind}
			if err := rows.Scan(&showID, &i.ID, &i.EpisodeID, &i.EpisodeNo, &i.ChatID, &i.MessageID, &i.GroupedID, &i.FileName, &i.Caption, &i.RawCaption, &i.RawPresent); err != nil {
				rows.Close()
				return nil, nil, nil, fmt.Errorf("unsupported evidence values in %s: %w", table, err)
			}
			if s := shows[showID]; s != nil {
				s.inputs = append(s.inputs, i)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	plan := &NameAuditPlan{Version: NameAuditVersion, Policy: nameAuditPolicy, Database: path,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339), SchemaFingerprint: schema.fingerprint,
		ShowsScanned: len(shows), Candidates: []NameRepairCandidate{}}
	for _, id := range ids {
		s := shows[id]
		b, _ := json.Marshal(struct {
			ShowID int64
			Inputs []nameInput
		}{s.id, s.inputs})
		h := sha256.Sum256(b)
		s.fingerprint = hex.EncodeToString(h[:])
		if c := evaluateNameShow(s); c != nil {
			plan.Candidates = append(plan.Candidates, *c)
		}
	}
	markNameConflicts(plan, shows)
	return plan, shows, schema, nil
}

var nameAuditContainers = regexp.MustCompile(`《[^》]{1,80}》|【[^】]{1,80}】`)
var nameAuditRange = regexp.MustCompile(`第\s*[0-9零一二三四五六七八九十百千两]+\s*[-~－～至到]\s*[0-9零一二三四五六七八九十百千两]+\s*[集话回]`)
var nameAuditOrphan = regexp.MustCompile(`^(.+?)\s+第$`)

func briefNameTitle(s string, max int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return string(r)
}

func evaluateNameShow(s *nameShow) *NameRepairCandidate {
	c := &NameRepairCandidate{ShowID: s.id, OldName: s.name, OldNorm: s.norm, Status: "eligible",
		SourceIDs: []int64{}, PhotoIDs: []int64{}, Evidence: []NameTitleEvidence{}, Reasons: []string{}, EvidenceFingerprint: s.fingerprint}
	p := parser.New()
	name, norm := "", ""
	changed, explicit, rangeTitle := false, false, false
	reason := func(r string) {
		for _, existing := range c.Reasons {
			if existing == r {
				return
			}
		}
		c.Reasons = append(c.Reasons, r)
		c.Status = "review"
	}
	addTitle := func(i nameInput, field, text string, file bool) bool {
		e := NameTitleEvidence{Kind: i.Kind, ID: i.ID, Field: field}
		var parsed *parser.Parsed
		var err error
		if file {
			fn := parser.NameFromFileName(text)
			if fn == "" { // A hash/portrait filename is not a competing title.
				e.Issue = "filename_not_title"
				c.Evidence = append(c.Evidence, e)
				return false
			}
			parsed, err = p.Parse("", text)
			if err == nil && parsed != nil && parsed.NormName != parser.Normalize(fn) {
				reason("ambiguous_filename")
				e.Issue = "ambiguous_filename"
			}
		} else {
			parsed, err = p.Parse(text, "")
		}
		if err != nil || parsed == nil || parsed.ShowName == "" || parsed.NormName == "" {
			e.Issue = "parse_failed"
			c.Evidence = append(c.Evidence, e)
			reason("parse_failed")
			return false
		}
		e.Title, e.Norm = briefNameTitle(parsed.ShowName, 48), briefNameTitle(parsed.NormName, 80)
		e.Explicit = file || parsed.HasTitle || (parsed.EpisodeNo > 0 && parser.Normalize(parsed.TitleName) == parsed.NormName)
		if len(parsed.Multi) > 1 {
			e.Issue = "multi_title"
		}
		// Parse intentionally accepts some two-title posts; repairs must be stricter.
		containerNorms := map[string]bool{}
		for _, title := range nameAuditContainers.FindAllString(text, -1) {
			v, err := p.Parse(title, "")
			if err == nil && v != nil && v.NormName != "" && !parser.IsJunkShowName(v.ShowName) {
				containerNorms[v.NormName] = true
			}
		}
		if len(containerNorms) > 1 {
			e.Issue = "multi_title"
		}
		if parsed.FromTag && parsed.TitleName != "" && parser.Normalize(parsed.TitleName) != parsed.NormName {
			e.Issue = "ambiguous_title_tag"
		}
		// A short, unselected tag beside a real container (e.g. 色 / 群p)
		// is category metadata, not independently trustworthy title evidence.
		// Adopted tags and longer competing title tags remain conservative.
		trustTag := parsed.FromTag || !parsed.Container || utf8.RuneCountInString(parsed.TagName) >= 3
		if trustTag && parsed.TagName != "" && !parser.IsJunkShowName(parsed.TagName) && parser.Normalize(parser.TrimNameNoise(parsed.TagName, false)) != parsed.NormName {
			e.Issue = "ambiguous_title_tag"
		}
		// Promotional footers do not invalidate an unambiguous explicit title:
		// this tool repairs names, not source/playback eligibility. Without an
		// actual title container, promotional/prose inference remains review-only.
		if !parsed.Container && (parser.IsAdCaption(text) || parser.IsNonDramaCaption(text)) {
			e.Issue = "non_drama_or_promotion"
		}
		if utf8.RuneCountInString(parsed.ShowName) > 80 || parser.IsJunkName(parsed.ShowName) {
			e.Issue = "unreliable_title"
		}
		if e.Issue != "" {
			reason(e.Issue)
		}
		c.Evidence = append(c.Evidence, e)
		if name == "" {
			name, norm = parsed.ShowName, parsed.NormName
		} else if norm != parsed.NormName {
			reason("evidence_disagrees")
		}
		if parsed.ShowName != s.name || parsed.NormName != s.norm {
			changed = true
		}
		// Filename/tag inference can corroborate or contradict, but cannot by
		// itself authorize a repair: at least one explicit caption container is
		// required. This deliberately prefers review over widening eligibility.
		explicit = explicit || (!file && parsed.Container && len(containerNorms) == 1 && e.Issue == "")
		if !file && parsed.Container && len(containerNorms) == 1 && e.Issue == "" && parsed.EpisodeNo > 0 && parsed.EpEnd > parsed.EpisodeNo && nameAuditRange.MatchString(text) {
			rangeTitle = true
		}
		return true
	}
	for _, i := range s.inputs {
		switch i.Kind {
		case "source":
			c.SourceIDs = append(c.SourceIDs, i.ID)
		case "photo":
			c.PhotoIDs = append(c.PhotoIDs, i.ID)
		default:
			c.Evidence = append(c.Evidence, NameTitleEvidence{Kind: i.Kind, ID: i.ID, Field: "source", Issue: "missing_source"})
			reason("missing_source")
			continue
		}
		has := false
		if strings.TrimSpace(i.Caption) != "" {
			has = addTitle(i, "caption", i.Caption, false) || has
		}
		if strings.TrimSpace(i.RawCaption) != "" {
			has = addTitle(i, "raw_messages.text", i.RawCaption, false) || has
		}
		if i.Kind == "source" && strings.TrimSpace(i.FileName) != "" {
			has = addTitle(i, "file_name", i.FileName, true) || has
		}
		if !has {
			reason("missing_title_evidence")
			c.Evidence = append(c.Evidence, NameTitleEvidence{Kind: i.Kind, ID: i.ID, Field: "title", Issue: "missing_title_evidence"})
		}
	}
	cleanedOld := parser.TrimNameNoise(s.name, false)
	if !changed && cleanedOld == s.name && !nameAuditOrphan.MatchString(s.name) {
		return nil
	}
	c.NewName, c.NewNorm = name, norm
	// A candidate whose target equals the current values is a no-op: the evidence
	// merely varies spacing, and there is nothing to repair.
	if c.NewName == s.name && c.NewNorm == s.norm {
		return nil
	}
	if !explicit || name == "" {
		reason("no_explicit_title")
	}
	related := norm != "" && parser.Normalize(cleanedOld) == norm
	// The legacy range bug left an isolated marker, separated by whitespace.
	// Never strip a literal suffix (e.g. 门第), and never infer from the old name
	// alone: a successfully parsed explicit 第N-M集 title is required as well.
	if m := nameAuditOrphan.FindStringSubmatch(s.name); !related && m != nil && rangeTitle {
		related = parser.Normalize(m[1]) == norm
	}
	if !related {
		reason("not_a_limited_noise_correction")
	}
	if norm == s.norm && name != s.name && strings.Join(strings.Fields(name), "") == strings.Join(strings.Fields(s.name), "") {
		reason("display_whitespace_only")
	}
	if utf8.RuneCountInString(s.name) > 80 || utf8.RuneCountInString(s.norm) > 80 || utf8.RuneCountInString(name) > 80 {
		reason("overlong_name_redacted")
		c.OldName, c.OldNorm = briefNameTitle(s.name, 80), briefNameTitle(s.norm, 80)
		c.NewName, c.NewNorm = briefNameTitle(name, 80), briefNameTitle(norm, 80)
	}
	if c.Status == "eligible" {
		c.Reasons = append(c.Reasons, "consistent_explicit_title_and_limited_noise")
	}
	return c
}

func markNameConflicts(plan *NameAuditPlan, shows map[int64]*nameShow) {
	owners, targets := map[string][]int64{}, map[string][]int64{}
	for _, s := range shows {
		owners[s.norm] = append(owners[s.norm], s.id)
	}
	for _, c := range plan.Candidates {
		if c.NewNorm != "" {
			targets[c.NewNorm] = append(targets[c.NewNorm], c.ShowID)
		}
	}
	for i := range plan.Candidates {
		c := &plan.Candidates[i]
		if c.NewNorm == "" {
			continue
		}
		conflicts := map[int64]bool{}
		for _, id := range append(append([]int64{}, owners[c.NewNorm]...), targets[c.NewNorm]...) {
			if id != c.ShowID {
				conflicts[id] = true
			}
		}
		for id := range conflicts {
			c.ConflictingShowIDs = append(c.ConflictingShowIDs, id)
		}
		if len(conflicts) > 0 {
			sort.Slice(c.ConflictingShowIDs, func(i, j int) bool { return c.ConflictingShowIDs[i] < c.ConflictingShowIDs[j] })
			c.Status = "conflict"
			c.Reasons = append(c.Reasons, "target_norm_exists_or_is_shared")
		}
	}
}

// ApplyNameRepairs requires a preview and an explicit nonempty selection. Rows
// that are stale, ambiguous or conflicting are skipped, never merged. SQL
// errors roll back the entire selected batch. Already-applied plans are no-ops.
// Call only offline: the write lock covers revalidation, backup and all updates.
func ApplyNameRepairs(ctx context.Context, path string, plan *NameAuditPlan, ids []int64, backupPath string) (*NameRepairResult, error) {
	result := &NameRepairResult{Applied: []int64{}, Skipped: []NameRepairSkip{}}
	if plan == nil || plan.Version != NameAuditVersion || plan.Policy != nameAuditPolicy || len(ids) == 0 || backupPath == "" {
		return result, errors.New("apply requires a supported preview plan, selected show IDs and a new backup path")
	}
	abs, err := nameDatabasePath(path)
	if err != nil {
		return result, err
	}
	if !sameNamePath(abs, plan.Database) {
		return result, errors.New("preview database path does not match -db")
	}
	selected := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || selected[id] {
			return result, errors.New("selected show IDs must be positive and unique")
		}
		selected[id] = true
	}
	preview := map[int64]NameRepairCandidate{}
	for _, c := range plan.Candidates {
		if _, exists := preview[c.ShowID]; exists {
			return result, errors.New("preview contains duplicate show IDs")
		}
		preview[c.ShowID] = c
	}
	db, err := openNameDatabase(ctx, abs, true, false)
	if err != nil {
		return result, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil) // _txlock=immediate: no racing writer
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	current, shows, schema, err := auditNameSnapshot(ctx, tx, abs)
	if err != nil {
		return result, err
	}
	if schema.fingerprint != plan.SchemaFingerprint {
		return result, errors.New("stale preview: database schema changed; audit again")
	}
	if schema.showTrigger {
		return result, errors.New("unsupported apply schema: shows triggers could modify unrelated data")
	}
	if err := checkNameForeignKeys(ctx, tx); err != nil {
		return result, err
	}
	fresh := map[int64]NameRepairCandidate{}
	for _, c := range current.Candidates {
		fresh[c.ShowID] = c
	}
	var updates []NameRepairCandidate
	for _, id := range ids {
		c, exists := preview[id]
		s := shows[id]
		skip := ""
		switch {
		case !exists:
			skip = "not_in_preview"
		case c.Status != "eligible":
			skip = "preview_" + c.Status
		case s == nil:
			skip = "show_missing"
		case s.fingerprint != c.EvidenceFingerprint:
			skip = "stale_evidence"
		case s.name == c.NewName && s.norm == c.NewNorm:
			skip = "already_applied"
		case s.name != c.OldName || s.norm != c.OldNorm:
			skip = "stale_old_values"
		default:
			now, ok := fresh[id]
			if !ok || now.Status != "eligible" || now.NewName != c.NewName || now.NewNorm != c.NewNorm {
				skip = "current_evidence_or_conflict_rejected"
			}
		}
		if skip != "" {
			result.Skipped = append(result.Skipped, NameRepairSkip{id, skip})
		} else {
			updates = append(updates, c)
		}
	}
	if len(updates) == 0 {
		return result, nil // No writes, no unnecessary backup on a repeated apply.
	}
	backup, err := backupNameDatabase(ctx, abs, backupPath)
	if err != nil {
		for _, u := range updates {
			result.Skipped = append(result.Skipped, NameRepairSkip{u.ShowID, "backup_failed"})
		}
		return result, err
	}
	result.Backup = backup
	var applied []int64
	for _, c := range updates {
		r, err := tx.ExecContext(ctx, `UPDATE OR ABORT shows SET name=?, norm_name=? WHERE id=? AND name=? AND norm_name=?`, c.NewName, c.NewNorm, c.ShowID, c.OldName, c.OldNorm)
		if err == nil {
			var n int64
			n, err = r.RowsAffected()
			if err == nil && n != 1 {
				err = errors.New("guarded update affected an unexpected number of rows")
			}
		}
		if err != nil {
			for _, u := range updates {
				result.Skipped = append(result.Skipped, NameRepairSkip{u.ShowID, "transaction_rolled_back"})
			}
			return result, fmt.Errorf("apply rolled back at show %d: %w", c.ShowID, err)
		}
		applied = append(applied, c.ShowID)
	}
	if err := tx.Commit(); err != nil {
		for _, u := range updates {
			result.Skipped = append(result.Skipped, NameRepairSkip{u.ShowID, "transaction_rolled_back"})
		}
		return result, fmt.Errorf("commit name repairs: %w", err)
	}
	result.Applied = applied
	return result, nil
}

// Reject custom schemas that use mutable names as foreign keys. Turning foreign
// keys off would break those references; turning them on could cascade writes.
// The application's schema references only shows.id, which is never changed.
func checkNameForeignKeys(ctx context.Context, q nameQuery) error {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, table)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, table := range tables {
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		rows, err := q.QueryContext(ctx, `PRAGMA foreign_key_list(`+quoted+`)`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, seq int
			var parent, from, update, deleteAction, match string
			var to sql.NullString
			if err := rows.Scan(&id, &seq, &parent, &from, &to, &update, &deleteAction, &match); err != nil {
				rows.Close()
				return err
			}
			mutable := func(s string) bool { return strings.EqualFold(s, "name") || strings.EqualFold(s, "norm_name") }
			if (strings.EqualFold(parent, "shows") && mutable(to.String)) || (strings.EqualFold(table, "shows") && mutable(from)) {
				rows.Close()
				return errors.New("unsupported apply schema: mutable show names participate in foreign keys")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func sameNamePath(a, b string) bool {
	// Windows is the primary host; EqualFold also safely rejects more aliases on
	// case-sensitive hosts when checking dangerous backup sidecar destinations.
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// VACUUM INTO reads the live SQLite snapshot, not just the main file, so WAL
// commits are included. The caller holds BEGIN IMMEDIATE on another connection:
// no writer can change that snapshot before the guarded updates finish.
func backupNameDatabase(ctx context.Context, source, destination string) (string, error) {
	dest, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil {
		return "", fmt.Errorf("backup directory: %w", err)
	}
	dest = filepath.Join(parent, filepath.Base(dest))
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if sameNamePath(dest, source+suffix) {
			return "", errors.New("backup must not use the database or its sidecar paths")
		}
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(dest + suffix); err == nil || !os.IsNotExist(err) {
			return "", errors.New("backup target has an existing or inaccessible SQLite sidecar")
		}
	}
	// SQLite accepts an existing empty target. O_EXCL reserves it without ever
	// truncating an existing file, including a pre-existing zero-byte file.
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", fmt.Errorf("reserve new backup (existing targets are refused): %w", err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// query_only would also prohibit writing the VACUUM destination. mode=ro
	// still enforces that the SOURCE cannot be changed by this connection.
	db, err := openNameDatabase(ctx, source, false, false)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `VACUUM main INTO ?`, dest); err != nil {
		return "", fmt.Errorf("backup failed; no changes applied (incomplete backup retained): %w", err)
	}
	f, err = os.OpenFile(dest, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	err = f.Sync()
	cerr := f.Close()
	if err != nil {
		return "", err
	}
	if cerr != nil {
		return "", cerr
	}
	check, err := openNameDatabase(ctx, dest, false, true)
	if err != nil {
		return "", err
	}
	defer check.Close()
	var integrity string
	if err := check.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil {
		return "", fmt.Errorf("verify backup: %w", err)
	}
	if integrity != "ok" {
		return "", errors.New("backup integrity check failed; no changes applied")
	}
	return dest, nil
}
