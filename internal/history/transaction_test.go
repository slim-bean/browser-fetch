package history

import (
	"context"
	"database/sql"
	"testing"
)

// Exercise the real pure-Go SQLite/WAL boundary after libc syscall dependency
// changes: snapshots must never leak a writer's uncommitted history and must not
// retain stale rows after that writer commits or rolls back.
func TestSnapshotsRespectWALTransactions(t *testing.T) {
	root := t.TempDir()
	db := fixture(t, root, "Default", true, true)
	const now int64 = 1750000000123
	insert(t, db, "https://example.test/committed", "committed", now)
	reader := Reader{Root: root, Name: "assistant"}
	query := Query{Version: 2, SourceID: "assistant/Default"}
	assertTitles := func(want ...string) {
		t.Helper()
		result, err := reader.Query(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Rows) != len(want) {
			t.Fatalf("rows = %+v, want titles %q", result.Rows, want)
		}
		for i, title := range want {
			if result.Rows[i].Title != title {
				t.Fatalf("row %d = %+v, want title %q", i, result.Rows[i], title)
			}
		}
	}
	begin := func(title string, ms int64) *sql.Tx {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		if _, err := tx.Exec("INSERT INTO urls(url,title,visit_count,last_visit_time) VALUES(?,?,1,?)", "https://example.test/"+title, title, (ms+11644473600000)*1000); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	tx := begin("rolled-back", now+1000)
	assertTitles("committed")
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertTitles("committed")
	tx = begin("new-commit", now+2000)
	assertTitles("committed")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertTitles("new-commit", "committed")
	// A successful snapshot must leave the source writable and checkpointable.
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	assertTitles("new-commit", "committed")
}
