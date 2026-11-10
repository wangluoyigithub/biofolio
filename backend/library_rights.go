package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// Teachers other than the administrator manage only what they uploaded:
// they may edit, delete and restore their own questions and papers, and
// suggest changes to anyone else's. Purging the recycle bin is reserved for
// the administrator. The administrator is the account with an empty personal id.

var errNotOwner = errors.New("只能修改、删除或恢复自己导入的题目和试卷；别人的题目请用「建议修改」。")
var errPurgeAdmin = errors.New("只有管理员可以彻底删除回收站中的内容。")

// Ownership is recorded by account id, never by username: usernames can be
// changed and reused, ids cannot. Records keep uploadedBy only for display.
const adminOwner = "admin"

// ownerKey turns a session's personal id into the id stored on records.
// The administrator has an empty personal id.
func ownerKey(personal string) string {
	if personal == "" {
		return adminOwner
	}
	return personal
}

// reservedQuestion reports whether id is one of the built-in sample questions
// or a hand-entered question kept in the shared state. Those always belong to
// the administrator, whatever records are uploaded later.
func (l *libraryPage) reservedQuestion(id string) (bool, error) {
	l.seedOnce.Do(func() {
		var seed struct {
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		}
		json.Unmarshal(l.seed(), &seed)
		l.seedIDs = map[string]bool{}
		for _, q := range seed.Questions {
			l.seedIDs[q.ID] = true
		}
	})
	if l.seedIDs[id] {
		return true, nil
	}
	var n int
	e := l.db.QueryRow(`select count(*) from library_state s, json_each(s.body,'$.extraQuestions') q
		where s.id=1 and json_extract(q.value,'$.id')=?`, id).Scan(&n)
	return n > 0, e
}

// questionOwner returns the account id that first uploaded a question.
// Records from before ids were stored, restored backups and built-in sample
// questions count as the administrator's until migrateOwners assigns them.
func (l *libraryPage) questionOwner(id string) (string, error) {
	if reserved, e := l.reservedQuestion(id); e != nil || reserved {
		return adminOwner, e
	}
	var who string
	e := l.db.QueryRow(`select coalesce(json_extract(r.body,'$.uploaderId'),'admin') from library_records r, json_each(r.body,'$.questions') q
		where json_extract(q.value,'$.id')=? order by r.rowid limit 1`, id).Scan(&who)
	if e == sql.ErrNoRows {
		return adminOwner, nil
	}
	return who, e
}

func (l *libraryPage) paperOwner(id string) (string, error) {
	var who string
	e := l.db.QueryRow("select coalesce(json_extract(body,'$.uploaderId'),'admin') from library_records where id=?", id).Scan(&who)
	if e == sql.ErrNoRows {
		return adminOwner, nil
	}
	return who, e
}

// migrateOwners gives every record without an owner id one, matching the
// stored username against the current teacher accounts; anything else is the
// administrator's. Suggestions get their author's id the same way. It runs at
// start-up, so later renames and reused names no longer change ownership.
func migrateOwners(db *sql.DB, account *libraryAccount) error {
	idOf := map[string]string{}
	if account != nil {
		for _, m := range account.Members {
			idOf[m.Username] = m.ID
		}
	}
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for name, id := range idOf {
		if _, e = tx.Exec("update library_records set body=json_set(body,'$.uploaderId',?) where json_extract(body,'$.uploaderId') is null and json_extract(body,'$.uploadedBy')=?", id, name); e != nil {
			return e
		}
	}
	if _, e = tx.Exec("update library_records set body=json_set(body,'$.uploaderId','admin') where json_extract(body,'$.uploaderId') is null"); e != nil {
		return e
	}
	var state []byte
	if e = tx.QueryRow("select body from library_state where id=1").Scan(&state); e != nil {
		return e
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(state, &fields) == nil && fields["suggestions"] != nil {
		var list []map[string]any
		if json.Unmarshal(fields["suggestions"], &list) == nil {
			touched := false
			for _, g := range list {
				if _, ok := g["byId"]; ok {
					continue
				}
				name, _ := g["by"].(string)
				g["byId"] = adminOwner
				if id, ok := idOf[name]; ok {
					g["byId"] = id
				}
				touched = true
			}
			if touched {
				fields["suggestions"], _ = json.Marshal(list)
				body, _ := json.Marshal(fields)
				if _, e = tx.Exec("update library_state set body=?,version=version+1 where id=1", string(body)); e != nil {
					return e
				}
			}
		}
	}
	return tx.Commit()
}

type sharedRights struct {
	DeletedQuestions []string `json:"deletedQuestions"`
	PurgedQuestions  []string `json:"purgedQuestions"`
	PurgedPapers     []string `json:"purgedPapers"`
	DeletedPapers    []struct {
		ID string `json:"id"`
	} `json:"deletedPapers"`
	Suggestions []suggestionRef            `json:"suggestions"`
	Fixes       map[string]json.RawMessage `json:"fixes"`
}

type suggestionRef struct {
	ID   string `json:"id"`
	Q    string `json:"q"`
	ByID string `json:"byId"`
}

// changed lists the values present in exactly one of a and b.
func changed(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	for _, v := range b {
		if !slices.Contains(a, v) {
			out = append(out, v)
		}
	}
	return out
}

// guardState checks a teacher's shared-state save against the stored state.
func (l *libraryPage) guardState(owner string, before, after []byte) error {
	var old, next sharedRights
	json.Unmarshal(before, &old)
	if json.Unmarshal(after, &next) != nil {
		return errors.New("格式无效")
	}
	if len(changed(old.PurgedQuestions, next.PurgedQuestions)) > 0 || len(changed(old.PurgedPapers, next.PurgedPapers)) > 0 {
		return errPurgeAdmin
	}
	for _, id := range changed(old.DeletedQuestions, next.DeletedQuestions) {
		who, e := l.questionOwner(id)
		if e != nil {
			return e
		}
		if who != owner {
			return errNotOwner
		}
	}
	paperIDs := func(s sharedRights) (ids []string) {
		for _, d := range s.DeletedPapers {
			ids = append(ids, d.ID)
		}
		return
	}
	for _, id := range changed(paperIDs(old), paperIDs(next)) {
		who, e := l.paperOwner(id)
		if e != nil {
			return e
		}
		if who != owner {
			return errNotOwner
		}
	}
	// Answers, analyses and sources filled in by hand belong to the question too.
	if next.Fixes == nil {
		next.Fixes = map[string]json.RawMessage{}
	}
	same := func(a, b json.RawMessage) bool {
		var x, y any
		json.Unmarshal(a, &x)
		json.Unmarshal(b, &y)
		p, _ := json.Marshal(x)
		q, _ := json.Marshal(y)
		return string(p) == string(q)
	}
	for id := range old.Fixes {
		if _, ok := next.Fixes[id]; !ok {
			next.Fixes[id] = nil
		}
	}
	for id, v := range next.Fixes {
		if same(old.Fixes[id], v) {
			continue
		}
		who, e := l.questionOwner(id)
		if e != nil {
			return e
		}
		if who != owner {
			return errNotOwner
		}
	}
	// Paper names, notes and categories belong to the paper's uploader. The
	// page fills in a missing name from the record itself, which is allowed.
	for _, field := range []string{"aliases", "paperNotes", "paperTags"} {
		var was, now map[string]json.RawMessage
		var o, n map[string]json.RawMessage
		json.Unmarshal(before, &o)
		json.Unmarshal(after, &n)
		json.Unmarshal(o[field], &was)
		json.Unmarshal(n[field], &now)
		keys := map[string]bool{}
		for k := range was {
			keys[k] = true
		}
		for k := range now {
			keys[k] = true
		}
		for id := range keys {
			if same(was[id], now[id]) {
				continue
			}
			who, e := l.paperOwner(id)
			if e != nil {
				return e
			}
			if who == owner {
				continue
			}
			if field == "aliases" && was[id] == nil {
				var name, given string
				l.db.QueryRow("select coalesce(json_extract(body,'$.name'),'') from library_records where id=?", id).Scan(&name)
				if json.Unmarshal(now[id], &given) == nil && given == name {
					continue
				}
			}
			return errNotOwner
		}
	}
	// Suggestions: anyone may add or edit their own, only under their own id;
	// the question's owner may also close someone else's.
	var oldRaw, nextRaw struct {
		Suggestions []json.RawMessage `json:"suggestions"`
	}
	json.Unmarshal(before, &oldRaw)
	json.Unmarshal(after, &nextRaw)
	entries := func(list []json.RawMessage) map[string]json.RawMessage {
		out := map[string]json.RawMessage{}
		for _, raw := range list {
			var g suggestionRef
			json.Unmarshal(raw, &g)
			out[g.ID] = raw
		}
		return out
	}
	was, now := entries(oldRaw.Suggestions), entries(nextRaw.Suggestions)
	for id := range was {
		if _, ok := now[id]; !ok {
			now[id] = nil
		}
	}
	for id, raw := range now {
		if same(was[id], raw) {
			continue
		}
		var prev, cur suggestionRef
		json.Unmarshal(was[id], &prev)
		json.Unmarshal(raw, &cur)
		if raw != nil {
			if cur.ByID != owner || (was[id] != nil && prev.ByID != owner) {
				return errNotOwner
			}
			continue
		}
		if prev.ByID == owner {
			continue
		}
		who, e := l.questionOwner(prev.Q)
		if e != nil {
			return e
		}
		if who != owner {
			return errNotOwner
		}
	}
	return nil
}

// errRevisionShape rejects a revision whose new version is not the same
// question as the version it replaces. Clients replace the question named by
// question.id, so checking only before.id would let a teacher overwrite any
// question by pairing it with one of their own.
var errRevisionShape = errors.New("修改记录无效：修改前后必须是同一道题。")

// checkRevision validates any record that carries a revision. It applies to
// every account, the administrator included.
func checkRevision(body []byte) (id string, revision bool, err error) {
	var rec struct {
		Before *struct {
			ID string `json:"id"`
		} `json:"before"`
		Question *struct {
			ID string `json:"id"`
		} `json:"question"`
	}
	if json.Unmarshal(body, &rec) != nil {
		return "", false, errRevisionShape
	}
	if rec.Before == nil && rec.Question == nil {
		return "", false, nil
	}
	if rec.Before == nil || rec.Question == nil || rec.Before.ID == "" || rec.Before.ID != rec.Question.ID {
		return "", true, errRevisionShape
	}
	return rec.Question.ID, true, nil
}

// guardRevision lets a teacher save a new version only of their own question.
func (l *libraryPage) guardRevision(owner string, body []byte) error {
	id, revision, err := checkRevision(body)
	if err != nil || !revision {
		return err
	}
	who, e := l.questionOwner(id)
	if e != nil {
		return e
	}
	if who != owner {
		return errNotOwner
	}
	return nil
}

var errQuestionTaken = errors.New("题号已被其他题目使用，请刷新后重新导入。")

// guardUpload stops a teacher from claiming a question id that is not theirs:
// a sample or hand-entered question, or one already in another teacher's
// record. The first record holding an id decides its owner, so without this
// an upload could take over any question that had no record yet.
func (l *libraryPage) guardUpload(owner string, body []byte) error {
	var rec struct {
		Questions []struct {
			ID string `json:"id"`
		} `json:"questions"`
		Question *struct {
			ID string `json:"id"`
		} `json:"question"`
		Links []struct {
			Q struct {
				ID string `json:"id"`
			} `json:"q"`
		} `json:"links"`
	}
	if json.Unmarshal(body, &rec) != nil {
		return errors.New("格式无效")
	}
	var ids []string
	for _, q := range rec.Questions {
		ids = append(ids, q.ID)
	}
	if rec.Question != nil {
		ids = append(ids, rec.Question.ID)
	}
	for _, link := range rec.Links {
		ids = append(ids, link.Q.ID)
	}
	for _, id := range ids {
		if id == "" {
			return errors.New("题目缺少编号")
		}
		reserved, e := l.reservedQuestion(id)
		if e != nil {
			return e
		}
		if reserved {
			return errQuestionTaken
		}
		var who string
		e = l.db.QueryRow(`select coalesce(json_extract(r.body,'$.uploaderId'),'admin') from library_records r
			where exists(select 1 from json_each(r.body,'$.questions') q where json_extract(q.value,'$.id')=?)
			   or exists(select 1 from json_each(r.body,'$.links') k where json_extract(k.value,'$.q.id')=?)
			order by r.rowid limit 1`, id, id).Scan(&who)
		if e == sql.ErrNoRows {
			continue
		}
		if e != nil {
			return e
		}
		if who != owner {
			return errQuestionTaken
		}
	}
	return nil
}

// keepAdminFields returns the teacher's state with the administrator-only
// fields put back as stored: hand-entered questions (extraQuestions) can only
// be changed by the administrator. Restoring them, rather than refusing the
// save, keeps a teacher's page that holds an older copy from failing.
func keepAdminFields(before, after []byte) ([]byte, error) {
	var old, next map[string]json.RawMessage
	json.Unmarshal(before, &old)
	if json.Unmarshal(after, &next) != nil {
		return nil, errors.New("格式无效")
	}
	for _, field := range []string{"extraQuestions"} {
		if v, ok := old[field]; ok {
			next[field] = v
		} else {
			delete(next, field)
		}
	}
	return json.Marshal(next)
}
