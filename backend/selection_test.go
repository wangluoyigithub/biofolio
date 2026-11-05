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

func TestPersonalFavoriteLegacyVisibility(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "favorites.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("create table library_favorites(account text primary key, marks text not null);insert into library_favorites values('member','[\"new\"]')"); err != nil {
		t.Fatal(err)
	}
	page := libraryPage{db: db}
	for _, tc := range []struct{ id, want string }{{"", "[\"old\"]"}, {"member", "[\"new\"]"}, {"another", ""}} {
		data := libraryData{State: json.RawMessage(`{"good":["old"],"bag":["shared"]}`)}
		if err := page.personal(&data, tc.id); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data.State, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["good"]) != tc.want || string(fields["bag"]) != `["shared"]` {
			t.Fatalf("unexpected state for %q: %s", tc.id, data.State)
		}
	}
}

func TestRecordsReplyCacheInvalidatedByWrite(t *testing.T) {
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "cache.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec("CREATE TABLE library_records(id text primary key,body text not null);INSERT INTO library_records VALUES('a','{\"id\":\"a\"}')"); e != nil {
		t.Fatal(e)
	}
	l := libraryPage{db: db, dirty: make(chan struct{}, 1)}
	first, e := l.recordsReply(false, false)
	if e != nil || !bytes.Contains(first.content, []byte(`"a"`)) || bytes.Contains(first.content, []byte(`"b"`)) {
		t.Fatal("initial listing wrong", string(first.content), e)
	}
	if _, e = db.Exec("INSERT INTO library_records VALUES('b','{\"id\":\"b\"}')"); e != nil {
		t.Fatal(e)
	}
	l.bumpRecords()
	fresh, e := l.recordsReply(false, false)
	if e != nil || !bytes.Contains(fresh.content, []byte(`"b"`)) {
		t.Fatal("write did not invalidate the cached listing", string(fresh.content))
	}
}
