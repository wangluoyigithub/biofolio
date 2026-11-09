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

// questionOwner returns who first uploaded a question; built-in sample
// questions have no uploader and belong to the administrator.
func (l *libraryPage) questionOwner(id string) (string, error) {
	var who sql.NullString
	e := l.db.QueryRow(`select json_extract(r.body,'$.uploadedBy') from library_records r, json_each(r.body,'$.questions') q
		where json_extract(q.value,'$.id')=? order by r.rowid limit 1`, id).Scan(&who)
	if e == sql.ErrNoRows {
		return "", nil
	}
	return who.String, e
}

func (l *libraryPage) paperOwner(id string) (string, error) {
	var who sql.NullString
	e := l.db.QueryRow("select json_extract(body,'$.uploadedBy') from library_records where id=?", id).Scan(&who)
	if e == sql.ErrNoRows {
		return "", nil
	}
	return who.String, e
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
	ID string `json:"id"`
	Q  string `json:"q"`
	By string `json:"by"`
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
func (l *libraryPage) guardState(actor string, before, after []byte) error {
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
		if who == "" || who != actor {
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
		if who == "" || who != actor {
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
		if who == "" || who != actor {
			return errNotOwner
		}
	}
	key := func(s sharedRights) (ids []string) {
		for _, g := range s.Suggestions {
			ids = append(ids, g.ID)
		}
		return
	}
	for _, id := range changed(key(old), key(next)) {
		for _, g := range append(old.Suggestions, next.Suggestions...) {
			if g.ID != id {
				continue
			}
			if g.By == actor {
				break
			}
			// Only the question's owner may close someone else's suggestion,
			// and nobody may write one under another teacher's name.
			who, e := l.questionOwner(g.Q)
			if e != nil {
				return e
			}
			if who == "" || who != actor || slices.ContainsFunc(next.Suggestions, func(n suggestionRef) bool { return n.ID == id }) {
				return errNotOwner
			}
			break
		}
	}
	return nil
}

// guardRevision lets a teacher save a new version only of their own question.
func (l *libraryPage) guardRevision(actor string, body []byte) error {
	var rec struct {
		Before *struct {
			ID string `json:"id"`
		} `json:"before"`
	}
	if json.Unmarshal(body, &rec) != nil || rec.Before == nil {
		return nil
	}
	who, e := l.questionOwner(rec.Before.ID)
	if e != nil {
		return e
	}
	if who == "" || who != actor {
		return errNotOwner
	}
	return nil
}
