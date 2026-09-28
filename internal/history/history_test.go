package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, root, profile string, hidden, wal bool) *sql.DB {
	t.Helper()
	dir := filepath.Join(root, profile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "History"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if wal {
		if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;"); err != nil {
			t.Fatal(err)
		}
	}
	schema := "CREATE TABLE urls(url TEXT,title TEXT,visit_count INTEGER,last_visit_time INTEGER"
	if hidden {
		schema += ",hidden INTEGER DEFAULT 0"
	}
	schema += ")"
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func insert(t *testing.T, db *sql.DB, url, title string, ms int64) {
	t.Helper()
	_, err := db.Exec("INSERT INTO urls(url,title,visit_count,last_visit_time) VALUES(?,?,2,?)", url, title, (ms+11644473600000)*1000)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeHistoryWALAndReadOnlySource(t *testing.T) {
	root := t.TempDir()
	db := fixture(t, root, "Default", true, true)
	const now int64 = 1750000000123
	insert(t, db, "https://example.com/100_percent?q=x", "100%_literal", now)
	insert(t, db, "https://example.com/skip", "excluded title", now-1000)
	insert(t, db, "https://other.example/x", "hidden", now-2000)
	if _, err := db.Exec("UPDATE urls SET hidden=1 WHERE title='hidden'"); err != nil {
		t.Fatal(err)
	}
	before := map[string][32]byte{}
	for _, s := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(root, "Default", "History"+s))
		if err != nil {
			t.Fatal(err)
		}
		before[s] = sha256.Sum256(data)
	}
	reader := Reader{Root: root, Name: "assistant"}
	sources, err := reader.Sources()
	if err != nil || len(sources) != 1 {
		t.Fatal(sources, err)
	}
	wire, _ := json.Marshal(sources)
	if strings.Contains(string(wire), root) {
		t.Fatal("leaked filesystem path")
	}
	q := Query{Version: 2, SourceID: sources[0].ID, Terms: []string{"%_"}, SinceMs: ptr(now), UntilMs: ptr(now)}
	result, err := reader.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0].Title != "100%_literal" || result.Rows[0].Ms != now {
		t.Fatalf("bad rows: %+v", result)
	}
	q.Terms = nil
	q.SinceMs = nil
	q.UntilMs = nil
	q.Excluded = []string{"excluded"}
	result, err = reader.Query(context.Background(), q)
	if err != nil || len(result.Rows) != 1 {
		t.Fatal(result, err)
	}
	q.Excluded = nil
	q.Limit = 1
	result, err = reader.Query(context.Background(), q)
	if err != nil || len(result.Rows) != 1 || !result.Truncated {
		t.Fatal(result, err)
	}
	for s, hash := range before {
		data, err := os.ReadFile(filepath.Join(root, "Default", "History"+s))
		if err != nil || sha256.Sum256(data) != hash {
			t.Fatalf("live %s was changed: %v", s, err)
		}
	}
}

func ptr(n int64) *int64 { return &n }

func TestLockedDatabaseAndOldSchema(t *testing.T) {
	root := t.TempDir()
	db := fixture(t, root, "Profile 1", false, false)
	insert(t, db, "https://example.com/a", "old schema", 1750000000000)
	if _, err := db.Exec("PRAGMA locking_mode=EXCLUSIVE; BEGIN EXCLUSIVE; COMMIT;"); err != nil {
		t.Fatal(err)
	}
	result, err := (Reader{Root: root, Name: "assistant"}).Query(context.Background(), Query{Version: 2, SourceID: "assistant/Profile 1"})
	if err != nil || len(result.Rows) != 1 {
		t.Fatal(result, err)
	}
}

func TestSnapshotFailuresAndConfinement(t *testing.T) {
	root := t.TempDir()
	db := fixture(t, root, "Default", true, false)
	insert(t, db, "https://example.com/a", "safe", 1750000000000)
	db.Close()
	outside := t.TempDir()
	other := fixture(t, outside, "Secret", true, false)
	other.Close()
	if err := os.Symlink(filepath.Join(outside, "Secret"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(root, "fake"), 0700)
	os.Symlink(filepath.Join(outside, "Secret", "History"), filepath.Join(root, "fake", "History"))
	reader := Reader{Root: root, Name: "assistant"}
	sources, err := reader.Sources()
	if err != nil || len(sources) != 1 {
		t.Fatal(sources, err)
	}
	if _, err = reader.Query(context.Background(), Query{Version: 2, SourceID: "../../Secret"}); !errors.Is(err, ErrSourceNotFound) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = reader.Query(ctx, Query{Version: 2, SourceID: sources[0].ID}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "Default", "History"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = reader.Query(context.Background(), Query{Version: 2, SourceID: sources[0].ID}); err == nil {
		t.Fatal("corrupt DB reported success")
	}
}

func TestBoundsAndValidation(t *testing.T) {
	for _, q := range []Query{{SourceID: "x"}, {Version: 2}, {Version: 2, SourceID: "x", Limit: MaxCandidates + 1}, {Version: 2, SourceID: "x", Terms: make([]string, 65)}} {
		if q.Validate() == nil {
			t.Fatal("invalid query accepted", q)
		}
	}
	var dst strings.Builder
	if _, err := copyBounded(context.Background(), &dst, strings.NewReader("12345"), 4); err == nil {
		t.Fatal("unbounded snapshot")
	}
	root := t.TempDir()
	db := fixture(t, root, "Default", true, false)
	insert(t, db, "https://example.com/huge", strings.Repeat("x", MaxFieldChars+1), 1750000000000)
	db.Close()
	result, err := (Reader{Root: root, Name: "assistant"}).Query(context.Background(), Query{Version: 2, SourceID: "assistant/Default"})
	if err != nil || !result.Truncated || len(result.Rows) != 0 {
		t.Fatal(result, err)
	}
}
