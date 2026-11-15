package main

import (
	"bytes"
	"context"
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
	if e = prepareLibrary(db); e != nil {
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
	if err = prepareLibrary(db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`update library_state set body='{"good":["old"],"bag":["mine"],"editions":[{"id":"e1"}]}' where id=1;delete from library_personal;insert into library_favorites values('member','["new"]')`); err != nil {
		t.Fatal(err)
	}
	if err = prepareLibrary(db); err != nil { // migrate the legacy basket to the original teacher
		t.Fatal(err)
	}
	page := libraryPage{db: db}
	for _, tc := range []struct{ id, want, bag string }{{"", "[\"old\"]", `["mine"]`}, {"member", "[\"new\"]", "[]"}, {"another", "", "[]"}} {
		data := libraryData{State: json.RawMessage(`{"good":["old"],"bag":["mine"],"editions":[{"id":"e1"}]}`)}
		if err := page.personal(&data, tc.id); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data.State, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["good"]) != tc.want || string(fields["bag"]) != tc.bag {
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

func TestPersonalBasketAndPapers(t *testing.T) {
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "personal.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = prepareLibrary(db); e != nil {
		t.Fatal(e)
	}
	l := libraryPage{db: db, dirty: make(chan struct{}, 1)}
	post := func(id, path, body string) int {
		t.Helper()
		r := httptest.NewRequest("POST", "/library/"+path, bytes.NewBufferString(body))
		r.Header.Set("X-Biology-Library", "1")
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(context.WithValue(r.Context(), personalKey{}, id))
		w := httptest.NewRecorder()
		l.serve(w, r)
		return w.Code
	}
	view := func(id string) map[string]json.RawMessage {
		t.Helper()
		d, e := l.snapshot(false)
		if e != nil || l.personal(&d, id) != nil {
			t.Fatal(e)
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(d.State, &fields)
		return fields
	}
	if post("", "personal", `{"bag":["a"]}`) != 400 || post("", "personal", `{"bag":"a","editions":[]}`) != 400 {
		t.Fatal("malformed personal data accepted")
	}
	if post("", "personal", `{"bag":["a"],"editions":[{"id":"p1"}]}`) != 204 || post("m1", "personal", `{"bag":["b"],"editions":[]}`) != 204 {
		t.Fatal("personal save failed")
	}
	// A shared save from either teacher never carries or erases a basket.
	if post("m1", "state", `{"version":0,"state":{"fixes":{},"bag":["leak"],"editions":[{"id":"leak"}]}}`) != 200 {
		t.Fatal("shared save failed")
	}
	admin, member, other := view(""), view("m1"), view("m2")
	if string(admin["bag"]) != `["a"]` || string(admin["editions"]) != `[{"id":"p1"}]` {
		t.Fatalf("admin basket wrong: %s %s", admin["bag"], admin["editions"])
	}
	if string(member["bag"]) != `["b"]` || string(member["editions"]) != `[]` || string(other["bag"]) != `[]` {
		t.Fatalf("member baskets leaked: %s %s", member["bag"], other["bag"])
	}
	if string(member["fixes"]) != `{}` {
		t.Fatal("shared state lost")
	}
}

func TestRenderEscapesRawMessageScriptCloseAndColdPictureReload(t *testing.T) {
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "xss.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = prepareLibrary(db); e != nil {
		t.Fatal(e)
	}
	l := libraryPage{
		db:   db,
		page: []byte(`<!doctype html><html><head><title>x</title></head><body>ok</body></html>`),
	}
	payload := libraryData{
		State:   json.RawMessage(`{"aliases":{"p1":"</script><script>alert(1)</script>"}}`),
		Records: []json.RawMessage{},
	}
	rendered, err := l.render(payload, "library-server")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rendered, []byte("<script>alert(1)")) || bytes.Count(rendered, []byte("</script>")) != 1 {
		t.Fatalf("script breakout not escaped: %s", rendered)
	}

	// Cold pictureStore reload from SQLite after restart
	imgRecord := `{"id":"r-img","questions":[{"id":"q1","contentHtml":"<img src=\"data:image/png;base64,YWJj\">"}]}`
	if _, e = db.Exec("INSERT INTO library_records VALUES('r-img',?)", imgRecord); e != nil {
		t.Fatal(e)
	}
	var warm, cold pictureStore
	compacted := warm.compact([]byte(imgRecord))
	cold.loader = func(register func([]byte)) {
		var b []byte
		if db.QueryRow("select body from library_records where id='r-img'").Scan(&b) == nil {
			register(b)
		}
	}
	expanded, err := cold.expand(compacted)
	if err != nil || !bytes.Contains(expanded, []byte("data:image/png;base64,YWJj")) {
		t.Fatalf("cold expand failed: %v %s", err, expanded)
	}
}

func TestMultiTeacherBackupAndAdminGuard(t *testing.T) {
	dir := t.TempDir()
	db, e := sql.Open("sqlite", filepath.Join(dir, "backup.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = prepareLibrary(db); e != nil {
		t.Fatal(e)
	}
	acc, e := openLibraryAccount(dir, "admin-pass-123")
	if e != nil || acc.updateWithID("", "li", "teacher-pass-1", "M-LI") != nil {
		t.Fatal(e)
	}
	l := libraryPage{db: db, account: acc, dir: dir, page: []byte(`<!doctype html><head></head>`), dirty: make(chan struct{}, 1)}
	db.Exec(`INSERT INTO library_favorites VALUES('M-LI','["q1"]'); INSERT INTO library_personal VALUES('M-LI','{"bag":["q1"],"editions":[{"id":"e1"}]}'); INSERT INTO library_records VALUES('r1','{"id":"r1","uploaderId":"M-LI","questions":[{"id":"q1","stem":"x"}]}')`)

	// Non-admin cannot download backup
	rMember := httptest.NewRequest("GET", "/library/backup", nil)
	rMember = rMember.WithContext(context.WithValue(rMember.Context(), personalKey{}, "M-LI"))
	wMember := httptest.NewRecorder()
	l.serve(wMember, rMember)
	if wMember.Code != 403 {
		t.Fatalf("member backup should be 403, got %d", wMember.Code)
	}

	// Admin backup captures member favorites, personal basket/editions, and memberIds
	rAdmin := httptest.NewRequest("GET", "/library/backup", nil)
	wAdmin := httptest.NewRecorder()
	l.serve(wAdmin, rAdmin)
	if wAdmin.Code != 200 {
		t.Fatalf("admin backup failed: %d", wAdmin.Code)
	}

	// Restore onto a brand-new database and verify M-LI data & ID reservation survive
	db2, _ := sql.Open("sqlite", filepath.Join(t.TempDir(), "restored.sqlite"))
	defer db2.Close()
	_ = prepareLibrary(db2)
	l2 := libraryPage{db: db2, dir: t.TempDir(), page: []byte(`<!doctype html><head></head>`), dirty: make(chan struct{}, 1)}
	rRestore := httptest.NewRequest("POST", "/library/restore", bytes.NewReader(wAdmin.Body.Bytes()))
	rRestore.Header.Set("X-Biology-Library", "1")
	rRestore.Header.Set("Content-Type", "application/json")
	wRestore := httptest.NewRecorder()
	l2.serve(wRestore, rRestore)
	if wRestore.Code != 204 {
		t.Fatalf("restore failed: %d %s", wRestore.Code, wRestore.Body.String())
	}
	if got := l2.reservedMemberID("li"); got != "M-LI" {
		t.Fatalf("member id mapping lost: got %q", got)
	}
	dView, _ := l2.snapshot(false)
	_ = l2.personal(&dView, "M-LI")
	var stateMap map[string]json.RawMessage
	_ = json.Unmarshal(dView.State, &stateMap)
	if string(stateMap["good"]) != `["q1"]` || string(stateMap["bag"]) != `["q1"]` {
		t.Fatalf("member personal/favorites lost after restore: %s", dView.State)
	}
}
