package history

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHotJournalRecoveryOnlyChangesDisposableCopy(t *testing.T) {
	root := t.TempDir()
	db := fixture(t, root, "Default", true, false)
	for i := 0; i < 100; i++ {
		insert(t, db, fmt.Sprintf("https://example.com/%03d", i), "committed "+strings.Repeat("x", 2000), 1750000000000+int64(i))
	}
	if _, err := db.Exec("PRAGMA cache_size=1; PRAGMA cache_spill=ON; BEGIN IMMEDIATE; UPDATE urls SET title='uncommitted ' || title;"); err != nil {
		t.Fatal(err)
	}
	defer db.Exec("ROLLBACK")
	hashes := map[string][32]byte{}
	for _, suffix := range []string{"", "-journal"} {
		data, err := os.ReadFile(filepath.Join(root, "Default", "History"+suffix))
		if err != nil {
			t.Fatal(err)
		}
		hashes[suffix] = sha256.Sum256(data)
	}
	result, err := (Reader{Root: root, Name: "assistant"}).Query(context.Background(), Query{Version: 2, SourceID: "assistant/Default", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || !strings.HasPrefix(result.Rows[0].Title, "committed ") {
		t.Fatalf("read uncommitted state: %+v", result.Rows)
	}
	for suffix, hash := range hashes {
		data, err := os.ReadFile(filepath.Join(root, "Default", "History"+suffix))
		if err != nil || sha256.Sum256(data) != hash {
			t.Fatalf("recovery changed live %s: %v", suffix, err)
		}
	}
}
