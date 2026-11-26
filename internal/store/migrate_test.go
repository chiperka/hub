package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A database created before cloud_servers.price_unknown existed gets the column.
func TestOpenAddsNewColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cloud_servers (id TEXT PRIMARY KEY, provider TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cloud_servers(id,provider,status,created_at) VALUES('ci-old','aws','deleted',1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ { // the second open must not add it again
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		var unknown bool
		if err := s.db.QueryRowContext(context.Background(), `SELECT price_unknown FROM cloud_servers WHERE id='ci-old'`).Scan(&unknown); err != nil || unknown {
			t.Fatalf("open %d: price_unknown %v, %v", i, unknown, err)
		}
		s.Close()
	}
}
