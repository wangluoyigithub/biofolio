package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func rightsLibrary(t *testing.T) (*libraryPage, func(id, actor, path, body string) int) {
	t.Helper()
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "rights.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	if e = prepareLibrary(db); e != nil {
		t.Fatal(e)
	}
	l := &libraryPage{db: db, dirty: make(chan struct{}, 1)}
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
	return l, post
}

func rq(id string) string {
	return fmt.Sprintf(`{"id":%q,"stem":"题干","answer":"A","a":"解析"}`, id)
}

func TestTeacherRights(t *testing.T) {
	_, post := rightsLibrary(t)
	// m1 is teacher li, m2 is teacher wang; "" is the administrator.
	if post("m1", "li", "record", `{"id":"r1","questions":[`+rq("mine")+`]}`) != 200 ||
		post("m2", "wang", "record", `{"id":"p2","paper":true,"questions":[`+rq("theirs")+`]}`) != 200 {
		t.Fatal("upload refused")
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
	if post("m1", "li", "record", `{"id":"revision:mine:1","question":`+rq("mine")+`,"before":`+rq("mine")+`}`) != 200 {
		t.Fatal("owner edit refused")
	}
	if post("m1", "li", "record", `{"id":"revision:theirs:1","question":`+rq("theirs")+`,"before":`+rq("theirs")+`}`) != 403 {
		t.Fatal("edit of another teacher's question accepted")
	}
	if post("m1", "li", "record", `{"id":"revision:cell-1:1","question":`+rq("cell-1")+`,"before":`+rq("cell-1")+`}`) != 403 {
		t.Fatal("teacher edited a sample question")
	}
	if post("", "admin", "record", `{"id":"revision:theirs:1","question":`+rq("theirs")+`,"before":`+rq("theirs")+`}`) != 200 {
		t.Fatal("admin edit refused")
	}
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
	base := `"deletedQuestions":["mine","theirs"],"deletedPapers":[{"id":"p2","questions":["theirs"]}],"fixes":{"theirs":{"answer":"B","source":"x"}}`
	if state("m2", "wang", `{`+base+`,"purgedQuestions":["theirs"]}`) != 403 {
		t.Fatal("teacher purged")
	}
	base += `,"purgedQuestions":["theirs"],"purgedPapers":["p2"]`
	if state("", "admin", `{`+base+`}`) != 200 {
		t.Fatal("admin purge refused")
	}
	// Suggestions are signed with the author's account id.
	if state("m2", "wang", `{`+base+`,"suggestions":[{"id":"s1","q":"mine","by":"wang","byId":"m1"}]}`) != 403 {
		t.Fatal("suggestion under another id accepted")
	}
	if state("m2", "wang", `{`+base+`,"suggestions":[{"id":"s1","q":"mine","by":"wang","byId":"m2","text":"原文"}]}`) != 200 {
		t.Fatal("suggestion refused")
	}
	if state("m3", "zhao", `{`+base+`,"suggestions":[{"id":"s1","q":"mine","by":"wang","byId":"m2","text":"改掉"}]}`) != 403 {
		t.Fatal("third teacher rewrote a suggestion")
	}
	if state("m3", "zhao", `{`+base+`,"suggestions":[]}`) != 403 {
		t.Fatal("third teacher closed a suggestion")
	}
	if state("m1", "li", `{`+base+`,"suggestions":[]}`) != 200 {
		t.Fatal("owner could not close a suggestion")
	}
}

// Paper names, notes and categories can be changed only by the paper's uploader.
func TestPaperDetailsBelongToUploader(t *testing.T) {
	_, post := rightsLibrary(t)
	post("m2", "wang", "record", `{"id":"p2","paper":true,"name":"期中卷","questions":[]}`)
	version := 0
	state := func(id, body string) int {
		t.Helper()
		code := post(id, "x", "state", fmt.Sprintf(`{"version":%d,"state":%s}`, version, body))
		if code == 200 {
			version++
		}
		return code
	}
	if state("m1", `{"aliases":{"p2":"期中卷"}}`) != 200 {
		t.Fatal("filling in the stored name was refused")
	}
	for _, body := range []string{`{"aliases":{"p2":"改名"}}`, `{"aliases":{"p2":"期中卷"},"paperNotes":{"p2":"x"}}`, `{"aliases":{"p2":"期中卷"},"paperTags":{"p2":{"year":"2026"}}}`, `{}`} {
		if state("m1", body) != 403 {
			t.Fatal("another teacher changed paper details:", body)
		}
	}
	if state("m2", `{"aliases":{"p2":"改名"},"paperNotes":{"p2":"x"},"paperTags":{"p2":{"year":"2026"}}}`) != 200 {
		t.Fatal("uploader could not change paper details")
	}
}

// A revision must replace the question it was made from; pairing your own
// question as "before" with someone else's as "question" is refused.
func TestRevisionMustKeepQuestion(t *testing.T) {
	l, post := rightsLibrary(t)
	post("", "admin", "record", `{"id":"r0","questions":[`+rq("adminq")+`]}`)
	post("m1", "bob", "record", `{"id":"r1","questions":[`+rq("bobq")+`]}`)
	if c := post("m1", "bob", "record", `{"id":"rev-1","before":`+rq("bobq")+`,"question":`+rq("adminq")+`}`); c != 400 {
		t.Fatalf("swapped revision: want 400, got %d", c)
	}
	if c := post("", "admin", "record", `{"id":"rev-2","before":`+rq("bobq")+`,"question":`+rq("adminq")+`}`); c != 400 {
		t.Fatalf("swapped revision by admin: want 400, got %d", c)
	}
	if c := post("m1", "bob", "record", `{"id":"rev-3","question":`+rq("adminq")+`}`); c != 400 {
		t.Fatalf("revision without before: want 400, got %d", c)
	}
	var n int
	l.db.QueryRow("select count(*) from library_records where id like 'rev-%'").Scan(&n)
	if n != 0 {
		t.Fatal("rejected revisions were stored")
	}
}

// Ownership follows the account id: renaming keeps it, and a new teacher
// who takes over an old name gets nothing.
func TestOwnershipSurvivesRename(t *testing.T) {
	l, post := rightsLibrary(t)
	post("", "teacher", "record", `{"id":"r0","questions":[`+rq("adminq")+`]}`)
	post("m1", "bob", "record", `{"id":"r1","questions":[`+rq("bobq")+`]}`)
	// The administrator was renamed to boss; bob then renamed himself teacher.
	if post("m1", "teacher", "state", `{"version":0,"state":{"deletedQuestions":["adminq"]}}`) != 403 {
		t.Fatal("renamed teacher took over the administrator's question")
	}
	if post("m1", "teacher", "state", `{"version":0,"state":{"deletedQuestions":["bobq"]}}`) != 200 {
		t.Fatal("renamed teacher lost his own question")
	}
	var who string
	l.db.QueryRow("select json_extract(body,'$.uploaderId') from library_records where id='r0'").Scan(&who)
	if who != adminOwner {
		t.Fatal("administrator upload stored as", who)
	}
}

// Records from before ids were stored get them from the current accounts:
// a teacher's username maps to that teacher, everything else to the administrator.
func TestMigrateOwners(t *testing.T) {
	l, _ := rightsLibrary(t)
	for _, row := range [][2]string{
		{"a", `{"id":"a","uploadedBy":"teacher","questions":[]}`},
		{"b", `{"id":"b","uploadedBy":"xxf","questions":[]}`},
		{"c", `{"id":"c","questions":[]}`},
		{"d", `{"id":"d","uploadedBy":"xxf","uploaderId":"keep","questions":[]}`},
	} {
		l.db.Exec("insert into library_records values(?,?)", row[0], row[1])
	}
	l.db.Exec(`update library_state set body='{"suggestions":[{"id":"s1","q":"a","by":"xxf"},{"id":"s2","q":"a","by":"wjy"}]}' where id=1`)
	account := &libraryAccount{Username: "wjy", Members: []libraryAccount{{Username: "xxf", ID: "M1"}}}
	for range 2 { // running again changes nothing
		if e := migrateOwners(l.db, account); e != nil {
			t.Fatal(e)
		}
	}
	want := map[string]string{"a": "admin", "b": "M1", "c": "admin", "d": "keep"}
	for id, owner := range want {
		var got string
		l.db.QueryRow("select json_extract(body,'$.uploaderId') from library_records where id=?", id).Scan(&got)
		if got != owner {
			t.Fatalf("record %s: owner %q, want %q", id, got, owner)
		}
	}
	var body []byte
	var version int
	l.db.QueryRow("select body,version from library_state where id=1").Scan(&body, &version)
	var s struct{ Suggestions []suggestionRef }
	json.Unmarshal(body, &s)
	if len(s.Suggestions) != 2 || s.Suggestions[0].ByID != "M1" || s.Suggestions[1].ByID != "admin" || version != 1 {
		t.Fatal("suggestions not migrated", string(body), version)
	}
}

// A teacher cannot claim a question id that has no record yet: sample and
// hand-entered questions stay the administrator's, and ids already in another
// teacher's record cannot be uploaded again.
func TestQuestionIDsCannotBeClaimed(t *testing.T) {
	l, post := rightsLibrary(t)
	l.page = []byte(`<script id="paper-data" type="application/json">{"questions":[{"id":"seed-x"}]}</script>`)
	l.db.Exec(`update library_state set body='{"extraQuestions":[{"id":"hand-1","stem":"手工题"}]}',version=0 where id=1`)
	post("m2", "wang", "record", `{"id":"r2","questions":[`+rq("wang-q")+`]}`)
	for _, body := range []string{
		`{"id":"paper-C","paper":true,"name":"C卷","questions":[` + rq("seed-x") + `]}`,
		`{"id":"paper-D","paper":true,"questions":[` + rq("hand-1") + `]}`,
		`{"id":"paper-E","paper":true,"questions":[` + rq("wang-q") + `]}`,
		`{"id":"link-F","questions":[],"links":[{"q":` + rq("seed-x") + `,"target":"x"}]}`,
		`{"id":"paper-G","questions":[{"id":"","stem":"x","answer":"A","a":"b"}]}`,
	} {
		if c := post("m1", "bob", "record", body); c != 403 {
			t.Fatalf("claim accepted (%d): %s", c, body)
		}
	}
	if post("m1", "bob", "record", `{"id":"r1","questions":[`+rq("bob-q")+`]}`) != 200 {
		t.Fatal("ordinary upload refused")
	}
	if post("m2", "wang", "record", `{"id":"r3","questions":[`+rq("wang-q")+`]}`) != 200 {
		t.Fatal("teacher could not re-upload their own question id")
	}
	// Every path that asks who owns a question sees the administrator.
	for _, state := range []string{
		`{"deletedQuestions":["seed-x"],"extraQuestions":[{"id":"hand-1","stem":"手工题"}]}`,
		`{"deletedQuestions":["hand-1"],"extraQuestions":[{"id":"hand-1","stem":"手工题"}]}`,
		`{"fixes":{"seed-x":{"answer":"B"}},"extraQuestions":[{"id":"hand-1","stem":"手工题"}]}`,
	} {
		if c := post("m1", "bob", "state", `{"version":0,"state":`+state+`}`); c != 403 {
			t.Fatalf("teacher changed a reserved question (%d): %s", c, state)
		}
	}
	if post("m1", "bob", "record", `{"id":"revision:seed-x:1","question":`+rq("seed-x")+`,"before":`+rq("seed-x")+`}`) != 403 {
		t.Fatal("teacher revised a sample question")
	}
}

// Teachers cannot change or clear hand-entered questions through the state;
// the stored list is kept and the rest of the save still goes through.
func TestTeacherCannotRewriteExtraQuestions(t *testing.T) {
	l, post := rightsLibrary(t)
	l.db.Exec(`update library_state set body='{"extraQuestions":[{"id":"hand-1","stem":"手工题"}]}',version=0 where id=1`)
	if c := post("m1", "bob", "state", `{"version":0,"state":{"extraQuestions":[],"wrong":["x"]}}`); c != 200 {
		t.Fatal("save refused", c)
	}
	var stem, wrong string
	l.db.QueryRow("select json_extract(body,'$.extraQuestions[0].stem'), json_extract(body,'$.wrong[0]') from library_state").Scan(&stem, &wrong)
	if stem != "手工题" || wrong != "x" {
		t.Fatal("hand-entered questions not kept or save lost:", stem, wrong)
	}
	if c := post("", "admin", "state", `{"version":1,"state":{"extraQuestions":[]}}`); c != 200 {
		t.Fatal("admin save refused", c)
	}
	var n int
	l.db.QueryRow("select json_array_length(body,'$.extraQuestions') from library_state").Scan(&n)
	if n != 0 {
		t.Fatal("admin could not change hand-entered questions")
	}
}
