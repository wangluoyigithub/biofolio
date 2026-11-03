package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestSelectionRestoreAndStaleWrite(t *testing.T) {
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	_, e = db.Exec("CREATE TABLE library_state(id integer primary key,version integer,body text);INSERT INTO library_state VALUES(1,0,'{}');CREATE TABLE library_records(id text primary key,body text)")
	if e != nil {
		t.Fatal(e)
	}
	l := libraryPage{db: db, dirty: make(chan struct{}, 1)}
	post := func(path, body string) int {
		t.Helper()
		r := httptest.NewRequest("POST", "/library/"+path, bytes.NewBufferString(body))
		r.Header.Set("X-Biology-Library", "1")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		l.serve(w, r)
		return w.Code
	}
	if post("restore", `{"state":{"good":["q"]},"records":[{"id":"q"},{"id":"q"}]}`) != 400 {
		t.Fatal("duplicate IDs should roll back restore")
	}
	restore := `{"state":{"good":["q"]},"records":[{"id":"q","questions":[{"id":"q","stem":"题干"}],"original":{"data":"data:text/plain;base64,YQ==","name":"原文.txt"}}]}`
	if post("restore", restore) != 204 || post("restore", restore) != 409 {
		t.Fatal("only empty library can be restored")
	}
	if post("state", `{"version":0,"state":{}}`) != 409 {
		t.Fatal("stale write accepted")
	}
	snapshot, e := l.snapshot(true)
	if e != nil || string(snapshot.State) != `{"good":["q"]}` || len(snapshot.Records) != 1 {
		t.Fatal(snapshot, e)
	}
	var rec map[string]any
	if json.Unmarshal(snapshot.Records[0], &rec) != nil || rec["original"].(map[string]any)["name"] != "原文.txt" {
		t.Fatal("attachment lost")
	}
}
