package main

import "testing"

// Every connection keeps SQLite temporary storage in memory (see openLibraryDB).
func TestLibraryDBTempStoreInMemory(t *testing.T) {
	db, err := openLibraryDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	for i := 0; i < 2; i++ {
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var store int
		if err = conn.QueryRowContext(t.Context(), "PRAGMA temp_store").Scan(&store); err != nil || store != 2 {
			t.Fatalf("temp_store = %d, %v; want 2 (memory)", store, err)
		}
	}
}
