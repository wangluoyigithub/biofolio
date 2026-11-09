package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestTeacherRights(t *testing.T) {
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "rights.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = prepareLibrary(db); e != nil {
		t.Fatal(e)
	}
	l := libraryPage{db: db, dirty: make(chan struct{}, 1)}
	post := func(id, actor, path, body string) int {
		t.Helper()
		r := httptest.NewRequest("POST", "/library/"+path, bytes.NewBufferString(body))
		r.Header.Set("X-Biology-Library", "1")
		r.Header.Set("Content-Type", "application/json")
		ctx := context.WithValue(r.Context(), personalKey{}, id)
		r = r.WithContext(context.WithValue(ctx, actorKey{}, actor))
		w := httptest.NewRecorder()
		l.serve(w, r)
		return w.Code
	}
	q := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"stem":"题干","answer":"A","a":"解析"}`, id)
	}
	if post("", "admin", "record", `{"id":"r1","questions":[`+q("mine")+`]}`) != 200 {
		t.Fatal("admin upload")
	}
	db.Exec("update library_records set body=json_set(body,'$.uploadedBy','li') where id='r1'")
	if post("m2", "wang", "record", `{"id":"p2","paper":true,"questions":[`+q("theirs")+`]}`) != 200 {
		t.Fatal("teacher upload")
	}
	version := 0
	state := func(id, actor, body string) int {
		t.Helper()
		code := post(id, actor, "state", fmt.Sprintf(`{"version":%d,"state":%s}`, version, body))
		if code == 200 {
			version++
		}
		return code
	}
	// Editing: own question yes, someone else's or a sample question no.
	if post("m1", "li", "record", `{"id":"revision:mine:1","question":`+q("mine")+`,"before":`+q("mine")+`}`) != 200 {
		t.Fatal("owner edit refused")
	}
	if post("m1", "li", "record", `{"id":"revision:theirs:1","question":`+q("theirs")+`,"before":`+q("theirs")+`}`) != 403 {
		t.Fatal("edit of another teacher's question accepted")
	}
	if post("m1", "li", "record", `{"id":"revision:cell-1:1","question":`+q("cell-1")+`,"before":`+q("cell-1")+`}`) != 403 {
		t.Fatal("teacher edited a sample question")
	}
	if post("", "admin", "record", `{"id":"revision:theirs:1","question":`+q("theirs")+`,"before":`+q("theirs")+`}`) != 200 {
		t.Fatal("admin edit refused")
	}
	// Deleting and restoring.
	if state("m1", "li", `{"deletedQuestions":["theirs"]}`) != 403 {
		t.Fatal("deleted another teacher's question")
	}
	if state("m1", "li", `{"deletedQuestions":["mine"]}`) != 200 {
		t.Fatal("owner delete refused")
	}
	if state("m2", "wang", `{"deletedQuestions":[]}`) != 403 {
		t.Fatal("restored another teacher's question")
	}
	if state("m1", "li", `{"deletedQuestions":["mine"],"deletedPapers":[{"id":"p2"}]}`) != 403 {
		t.Fatal("deleted another teacher's paper")
	}
	if state("m2", "wang", `{"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2","questions":["theirs"]}]}`) != 200 {
		t.Fatal("owner paper delete refused")
	}
	if state("m1", "li", `{"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2","questions":["theirs"]}],"fixes":{"theirs":{"answer":"B"}}}`) != 403 {
		t.Fatal("filled in another teacher's answer")
	}
	if state("", "admin", `{"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2","questions":["theirs"]}],"fixes":{"theirs":{"answer":"B","source":"x"}}}`) != 200 {
		t.Fatal("admin fix refused")
	}
	if state("m1", "li", `{"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2","questions":["theirs"]}],"fixes":{"theirs":{"source":"x","answer":"B"}}}`) != 200 {
		t.Fatal("unchanged fix with reordered keys refused")
	}
	if state("m1", "li", `{"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2","questions":["theirs"]}]}`) != 403 {
		t.Fatal("removed another teacher's fix")
	}
	// Purging is for the administrator only.
	if state("m2", "wang", `{"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2"}],"fixes":{"theirs":{"answer":"B","source":"x"}},"purgedQuestions":["theirs"]}`) != 403 {
		t.Fatal("teacher purged")
	}
	if state("", "admin", `{"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2"}],"fixes":{"theirs":{"answer":"B","source":"x"}},"purgedQuestions":["theirs"],"purgedPapers":["p2"]}`) != 200 {
		t.Fatal("admin purge refused")
	}
	// Suggestions: anyone may add under their own name; the owner closes them.
	base := `"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2"}],"fixes":{"theirs":{"answer":"B","source":"x"}},"purgedQuestions":["theirs"],"purgedPapers":["p2"]`
	if state("m2", "wang", `{`+base+`,"suggestions":[{"id":"s1","q":"mine","by":"li"}]}`) != 403 {
		t.Fatal("suggestion under another name accepted")
	}
	if state("m2", "wang", `{`+base+`,"suggestions":[{"id":"s1","q":"mine","by":"wang"}]}`) != 200 {
		t.Fatal("suggestion refused")
	}
	if state("m3", "zhao", `{`+base+`,"suggestions":[]}`) != 403 {
		t.Fatal("third teacher closed a suggestion")
	}
	if state("m1", "li", `{`+base+`,"suggestions":[]}`) != 200 {
		t.Fatal("owner could not close a suggestion")
	}
}
