// Package history exposes bounded Chromium history records, not agent-specific
// query syntax, ranking or formatted text. It never opens a live database writable.
package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	Version          = 2
	MaxCandidates    = 20000
	MaxResponseBytes = 8 << 20
	MaxSnapshotBytes = 256 << 20
	MaxFieldChars    = 16384
	maxDateMs        = int64(8640000000000000)
)

var ErrSourceNotFound = errors.New("unknown history source")

type Source struct {
	ID      string `json:"id"`
	Browser string `json:"browser"`
	Profile string `json:"profile"`
	Label   string `json:"label"`
	dir     string
}

type Query struct {
	Version  int      `json:"version"`
	SourceID string   `json:"sourceId"`
	Terms    []string `json:"terms,omitempty"`
	Excluded []string `json:"excluded,omitempty"`
	Hosts    []string `json:"hosts,omitempty"`
	SinceMs  *int64   `json:"sinceMs,omitempty"`
	UntilMs  *int64   `json:"untilMs,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

func (q Query) Validate() error {
	if q.Version != Version {
		return errors.New("history protocol version 2 is required")
	}
	if q.SourceID == "" || len(q.SourceID) > 256 {
		return errors.New("sourceId is required (use /history/sources)")
	}
	for _, values := range [][]string{q.Terms, q.Excluded, q.Hosts} {
		if len(values) > 64 {
			return errors.New("at most 64 filters per field")
		}
		for _, value := range values {
			if value == "" || len(value) > 4096 {
				return errors.New("filters must contain 1–4096 bytes")
			}
		}
	}
	if q.Limit < 0 || q.Limit > MaxCandidates {
		return fmt.Errorf("limit must be between 1 and %d (or omitted)", MaxCandidates)
	}
	for _, t := range []*int64{q.SinceMs, q.UntilMs} {
		if t != nil && (*t < -maxDateMs || *t > maxDateMs) {
			return errors.New("timestamp outside supported range")
		}
	}
	return nil
}

type Row struct {
	URL    string `json:"url"`
	Title  string `json:"title"`
	Visits int64  `json:"visits"`
	Ms     int64  `json:"ms"`
}

type Result struct {
	Version   int    `json:"version"`
	Source    Source `json:"source"`
	Rows      []Row  `json:"rows"`
	Truncated bool   `json:"truncated"`
}

type Reader struct{ Root, Name string }

// Sources are immediate, real profile directories with a regular History file.
// IDs are stable directory identities, not request-supplied paths. Source labels
// deliberately don't depend on Chrome's unrelated Local State configuration.
func (r Reader) sources(root *os.Root) ([]Source, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	sources := []Source{}
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) > 128 {
			continue
		}
		info, err := root.Lstat(filepath.Join(entry.Name(), "History"))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		label := r.Name + "/" + entry.Name()
		sources = append(sources, Source{ID: label, Browser: r.Name, Profile: entry.Name(), Label: label, dir: entry.Name()})
		if len(sources) > 64 {
			return nil, errors.New("too many history profiles (maximum 64)")
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	return sources, nil
}

func (r Reader) Sources() ([]Source, error) {
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return r.sources(root)
}

// Query copies only History and its SQLite recovery sidecars into a private
// temporary directory. Reads and hot-journal recovery happen on that disposable
// copy. os.Root confines file access even if a symlink changes while copying.
func (r Reader) Query(ctx context.Context, q Query) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	sources, err := r.sources(root)
	if err != nil {
		return Result{}, err
	}
	var source *Source
	for i := range sources {
		if sources[i].ID == q.SourceID {
			source = &sources[i]
			break
		}
	}
	if source == nil {
		return Result{}, ErrSourceNotFound
	}
	// A live writer may race the filesystem copy. Retry boundedly; never treat an
	// unreadable/inconsistent copy as an empty history or read the live DB writable.
	for attempt := 0; attempt < 3; attempt++ {
		if err = ctx.Err(); err != nil {
			break
		}
		var snapshot string
		var cleanup func()
		snapshot, cleanup, err = copySnapshot(ctx, root, source.dir)
		if err == nil {
			var result Result
			result, err = readSnapshot(ctx, snapshot, *source, q)
			cleanup()
			if err == nil {
				return result, nil
			}
		}
	}
	return Result{}, err
}

func copySnapshot(ctx context.Context, root *os.Root, profile string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "browser-fetch-history-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fail := func(err error) (string, func(), error) { cleanup(); return "", nil, err }
	type copied struct {
		name string
		info fs.FileInfo
	}
	before := []copied{}
	var total int64
	// The WAL index (-shm) is intentionally rebuilt, never copied from a writer.
	for _, suffix := range []string{"", "-wal", "-journal"} {
		name := filepath.Join(profile, "History"+suffix)
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) && suffix != "" {
			before = append(before, copied{name, nil})
			continue
		}
		if err != nil {
			return fail(err)
		}
		if !info.Mode().IsRegular() {
			return fail(errors.New("history source must be a regular file"))
		}
		src, err := root.Open(name)
		if err != nil {
			return fail(err)
		}
		actual, err := src.Stat()
		if err != nil || !os.SameFile(info, actual) {
			src.Close()
			return fail(errors.New("history changed while opening snapshot"))
		}
		dst, err := os.OpenFile(filepath.Join(dir, "History"+suffix), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			src.Close()
			return fail(err)
		}
		var n int64
		n, err = copyBounded(ctx, dst, src, MaxSnapshotBytes-total)
		src.Close()
		closeErr := dst.Close()
		total += n
		if err != nil {
			return fail(err)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
		before = append(before, copied{name, actual})
	}
	for _, item := range before {
		after, err := root.Lstat(item.name)
		if item.info == nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fail(errors.New("history changed while copying"))
		}
		if err != nil || !os.SameFile(item.info, after) || item.info.Size() != after.Size() || !item.info.ModTime().Equal(after.ModTime()) {
			return fail(errors.New("history changed while copying"))
		}
	}
	return filepath.Join(dir, "History"), cleanup, nil
}

func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	var total int64
	buf := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if total+int64(n) > limit {
			return total, errors.New("history snapshot exceeds 256 MiB limit")
		}
		if n > 0 {
			written, err := dst.Write(buf[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func pattern(s string) string {
	return "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s) + "%"
}

func readSnapshot(ctx context.Context, path string, source Source, q Query) (Result, error) {
	u := url.URL{Scheme: "file", Path: path}
	// Recovery may write to this private copy; the live source is never opened by SQLite.
	u.RawQuery = "mode=rw&_pragma=busy_timeout(1000)"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return Result{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var integrity string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&integrity); err != nil {
		return Result{}, err
	}
	if integrity != "ok" {
		return Result{}, errors.New("history snapshot failed SQLite integrity check")
	}
	if _, err = db.ExecContext(ctx, "PRAGMA query_only=ON; PRAGMA trusted_schema=OFF"); err != nil {
		return Result{}, err
	}
	var hasHidden bool
	cols, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info('urls')")
	if err != nil {
		return Result{}, err
	}
	for cols.Next() {
		var name string
		if err = cols.Scan(&name); err != nil {
			break
		}
		if name == "hidden" {
			hasHidden = true
		}
	}
	if err == nil {
		err = cols.Err()
	}
	cols.Close()
	if err != nil {
		return Result{}, err
	}
	hidden := ""
	if hasHidden {
		hidden = " AND hidden = 0"
	}
	base := `SELECT url, IFNULL(title,'') AS title, visit_count AS visits,
  CAST(last_visit_time / 1000 - 11644473600000 AS INTEGER) AS ms FROM urls WHERE last_visit_time > 0` + hidden
	conditions := []string{}
	args := []any{}
	if q.SinceMs != nil {
		conditions = append(conditions, "ms >= ?")
		args = append(args, *q.SinceMs)
	}
	if q.UntilMs != nil {
		conditions = append(conditions, "ms <= ?")
		args = append(args, *q.UntilMs)
	}
	for _, term := range q.Terms {
		conditions = append(conditions, `(url LIKE ? ESCAPE '\' OR title LIKE ? ESCAPE '\')`)
		args = append(args, pattern(term), pattern(term))
	}
	for _, term := range q.Excluded {
		conditions = append(conditions, `(url NOT LIKE ? ESCAPE '\' AND title NOT LIKE ? ESCAPE '\')`)
		args = append(args, pattern(term), pattern(term))
	}
	if len(q.Hosts) > 0 {
		clauses := []string{}
		for _, host := range q.Hosts {
			clauses = append(clauses, `url LIKE ? ESCAPE '\'`)
			args = append(args, pattern(host))
		}
		conditions = append(conditions, "("+strings.Join(clauses, " OR ")+")")
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	limit := q.Limit
	if limit == 0 {
		limit = MaxCandidates
	}
	args = append(args, limit+1)
	statement := fmt.Sprintf("SELECT substr(url,1,%d), substr(title,1,%d), IFNULL(visits,0), ms FROM (%s)%s ORDER BY ms DESC, url ASC LIMIT ?", MaxFieldChars+1, MaxFieldChars+1, base, where)
	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	result := Result{Version: Version, Source: source, Rows: []Row{}}
	// Reserve room for envelope/source metadata; measure JSON-encoded row bytes,
	// including escaping, instead of assuming character count equals wire size.
	size := 4096
	count := 0
	for rows.Next() {
		var row Row
		if err = rows.Scan(&row.URL, &row.Title, &row.Visits, &row.Ms); err != nil {
			return Result{}, err
		}
		count++
		if count > limit {
			result.Truncated = true
			break
		}
		if len(row.URL) > MaxFieldChars || len(row.Title) > MaxFieldChars || row.Ms < -maxDateMs || row.Ms > maxDateMs || row.Visits < 0 || row.Visits > 9007199254740991 {
			result.Truncated = true
			continue
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return Result{}, err
		}
		size += len(encoded) + 1
		if size > MaxResponseBytes {
			result.Truncated = true
			break
		}
		result.Rows = append(result.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	return result, nil
}
