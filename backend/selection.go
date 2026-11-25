// The teacher's selection UI uses its own versioned data, separate from the trial database.
package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type libraryPage struct {
	db      *sql.DB
	account *libraryAccount
	page    []byte
	dir     string
	dirty   chan struct{}
	online  *onlinePage
	// Record listings are identical until a record is added or restored, so
	// each variant is compacted and compressed once per generation.
	// Built-in sample question ids, read once from the page.
	seedOnce   sync.Once
	seedIDs    map[string]bool
	recordsMu  sync.Mutex
	recordsGen uint64
	recordsOut map[string]recordsReply
}
type recordsReply struct {
	gen   uint64
	asset pageAsset
}
type reviewDraftBackup struct {
	Account string          `json:"account"`
	Paper   string          `json:"paper"`
	Version int             `json:"version"`
	Body    json.RawMessage `json:"body"`
}
type libraryData struct {
	AccountID    string                     `json:"accountId,omitempty"`
	AIEndpoint   string                     `json:"aiEndpoint,omitempty"`
	AIModel      string                     `json:"aiModel,omitempty"`
	AIShared     bool                       `json:"aiShared,omitempty"`
	State        json.RawMessage            `json:"state"`
	Version      int                        `json:"version"`
	Records      []json.RawMessage          `json:"records"`
	Seed         json.RawMessage            `json:"seed,omitempty"`
	Favorites    map[string]json.RawMessage `json:"favorites,omitempty"`
	Personal     map[string]json.RawMessage `json:"personal,omitempty"`
	ReviewDrafts []reviewDraftBackup        `json:"reviewDrafts,omitempty"`
	MemberIDs    map[string]string          `json:"memberIds,omitempty"`
}

func safeJSONScript(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("<"), []byte(`\u003c`))
	b = bytes.ReplaceAll(b, []byte(">"), []byte(`\u003e`))
	b = bytes.ReplaceAll(b, []byte("\u2028"), []byte(`\u2028`))
	b = bytes.ReplaceAll(b, []byte("\u2029"), []byte(`\u2029`))
	return b
}

func (l *libraryPage) snapshot(records bool) (libraryData, error) {
	tx, e := l.db.Begin()
	if e != nil {
		return libraryData{}, e
	}
	defer tx.Rollback()
	var d libraryData
	var state string
	if e = tx.QueryRow("select body,version from library_state where id=1").Scan(&state, &d.Version); e != nil {
		return d, e
	}
	d.State = json.RawMessage(state)
	if records {
		d.Records = []json.RawMessage{}
		rows, e := tx.Query("select body from library_records order by rowid")
		if e != nil {
			return d, e
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			if e = rows.Scan(&body); e != nil {
				return d, e
			}
			d.Records = append(d.Records, json.RawMessage(body))
		}
		if e = rows.Err(); e != nil {
			return d, e
		}
	}
	return d, tx.Commit()
}
func (l *libraryPage) render(d libraryData, id string) ([]byte, error) {
	b, e := json.Marshal(d)
	if e != nil {
		return nil, e
	}
	b = safeJSONScript(b)
	tag := append([]byte("<script type=\"application/json\" id=\""+id+"\">"), b...)
	tag = append(tag, []byte("</script></head>")...)
	html := l.page
	if id == "library-server" && l.online != nil {
		html = l.online.html
	}
	idx := bytes.Index(html, []byte("</head>"))
	if idx < 0 {
		return append(html, tag...), nil
	}
	out := make([]byte, 0, len(html)+len(tag))
	out = append(out, html[:idx]...)
	out = append(out, tag...)
	out = append(out, html[idx+len("</head>"):]...)
	return out, nil
}
func (l *libraryPage) seed() json.RawMessage {
	start := []byte(`<script id="paper-data" type="application/json">`)
	_, tail, ok := bytes.Cut(l.page, start)
	if !ok {
		return nil
	}
	seed, _, ok := bytes.Cut(tail, []byte("</script>"))
	if !ok {
		return nil
	}
	return json.RawMessage(seed)
}
func (l *libraryPage) archive() error {
	d, e := l.snapshot(true)
	if e != nil {
		return e
	}
	if e = l.personal(&d, ""); e != nil {
		return e
	}
	page, e := l.render(d, "library-offline")
	if e != nil {
		return e
	}
	file := filepath.Join(l.dir, "离线题库.html")
	if e = os.WriteFile(file+".tmp", page, 0600); e != nil {
		return e
	}
	return os.Rename(file+".tmp", file)
}
func (l *libraryPage) changed() {
	select {
	case l.dirty <- struct{}{}:
	default:
	}
}

// Each teacher keeps their own basket and saved papers. Everything else in the
// state (fixes, aliases, deleted questions, purposes) is shared.
var personalFields = []string{"bag", "editions"}

// personalSeed copies the pre-split basket and papers to the original teacher.
const personalSeed = `json_object('bag',json(coalesce(json_extract(%s,'$.bag'),'[]')),'editions',json(coalesce(json_extract(%s,'$.editions'),'[]')))`

// Legacy good marks belonged to the original teacher. Replace them in each
// authenticated response with the current account's collection, basket and papers.
func (l *libraryPage) personal(d *libraryData, id string) error {
	d.AccountID = id
	var state map[string]json.RawMessage
	if err := json.Unmarshal(d.State, &state); err != nil {
		return err
	}
	var marks string
	err := l.db.QueryRow("select marks from library_favorites where account=?", id).Scan(&marks)
	if err == nil {
		state["good"] = json.RawMessage(marks)
	} else if err != sql.ErrNoRows {
		return err
	} else if id != "" {
		delete(state, "good")
	}
	own := map[string]json.RawMessage{}
	var body string
	err = l.db.QueryRow("select body from library_personal where account=?", id).Scan(&body)
	if err == nil {
		if err = json.Unmarshal([]byte(body), &own); err != nil {
			return err
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	for _, f := range personalFields {
		if v, ok := own[f]; ok {
			state[f] = v
		} else {
			state[f] = json.RawMessage("[]")
		}
	}
	d.State, err = json.Marshal(state)
	return err
}
func (l *libraryPage) serve(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(personalKey{}).(string)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == "GET" && l.online != nil {
		if strings.HasPrefix(r.URL.Path, "/"+picturePrefix) {
			l.online.pictures.serve(w, strings.TrimPrefix(r.URL.Path, "/"))
			return
		}
		if a, ok := l.online.files[r.URL.Path]; ok {
			sendAsset(w, r, a)
			return
		}
	}
	if r.Method == "GET" && r.URL.Path == "/" {
		d, e := l.snapshot(false)
		if e != nil {
			http.Error(w, "读取题库失败", 500)
			return
		}
		if e = l.personal(&d, id); e != nil {
			http.Error(w, "无法读取收藏", 500)
			return
		}
		d.AIEndpoint = os.Getenv("BIOLOGY_AI_ENDPOINT")
		if d.AIEndpoint != "" {
			shared := l.loadAIShared()
			d.AIModel, d.AIShared = shared.Model, shared.Key != ""
		}
		p, e := l.render(d, "library-server")
		if e != nil {
			http.Error(w, "读取页面失败", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		sendAsset(w, r, pageAsset{p, zipBytes(p), "text/html; charset=utf-8", false})
		return
	}
	if r.Method == "GET" && r.URL.Path == "/library/review-draft" {
		var body string
		err := l.db.QueryRow("select body from library_review_drafts where account=? and paper=?", id, r.URL.Query().Get("paper")).Scan(&body)
		if err == sql.ErrNoRows {
			jsonResponse(w, nil)
			return
		}
		if err != nil {
			http.Error(w, "读取核题草稿失败", 500)
			return
		}
		jsonResponse(w, json.RawMessage(body))
		return
	}
	if r.Method == "GET" && r.URL.Path == "/library/review-drafts" {
		type draftSummary struct {
			Paper     string `json:"paper"`
			Version   int    `json:"version"`
			Kind      string `json:"kind"`
			ItemCount int    `json:"itemCount"`
		}
		rows, err := l.db.Query(`select paper, version, coalesce(json_extract(body,'$.kind'),''), coalesce(json_array_length(body,'$.items'),0) from library_review_drafts where account=? order by paper`, id)
		if err != nil {
			http.Error(w, "读取草稿列表失败", 500)
			return
		}
		defer rows.Close()
		out := []draftSummary{}
		for rows.Next() {
			var s draftSummary
			if err := rows.Scan(&s.Paper, &s.Version, &s.Kind, &s.ItemCount); err != nil {
				http.Error(w, "读取草稿列表失败", 500)
				return
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "读取草稿列表失败", 500)
			return
		}
		jsonResponse(w, out)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/library/offline" {
		if id != "" {
			http.Error(w, "仅管理员可导出离线题库", 403)
			return
		}
		file := filepath.Join(l.dir, "离线题库.html")
		data, err := os.ReadFile(file)
		if err != nil {
			if e := l.archive(); e != nil {
				http.Error(w, "生成离线题库失败", 500)
				return
			}
			data, err = os.ReadFile(file)
			if err != nil {
				http.Error(w, "读取离线题库失败", 500)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=\"biofolio-offline.html\"; filename*=UTF-8''%E7%A6%BB%E7%BA%BF%E9%A2%98%E5%BA%93.html")
		w.Write(data)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/library/backup" {
		if id != "" {
			http.Error(w, "仅管理员可下载整库备份", 403)
			return
		}
		d, e := l.snapshot(true)
		if e != nil {
			http.Error(w, "备份失败", 500)
			return
		}
		if e = l.personal(&d, ""); e != nil {
			http.Error(w, "无法读取收藏", 500)
			return
		}
		d.Favorites = map[string]json.RawMessage{}
		if rows, err := l.db.Query("select account,marks from library_favorites"); err == nil {
			defer rows.Close()
			for rows.Next() {
				var acc, marks string
				if rows.Scan(&acc, &marks) == nil && json.Valid([]byte(marks)) {
					d.Favorites[acc] = json.RawMessage(marks)
				}
			}
		}
		if len(d.Favorites) == 0 {
			d.Favorites = nil
		}
		d.Personal = map[string]json.RawMessage{}
		if rows, err := l.db.Query("select account,body from library_personal"); err == nil {
			defer rows.Close()
			for rows.Next() {
				var acc, body string
				if rows.Scan(&acc, &body) == nil && json.Valid([]byte(body)) {
					d.Personal[acc] = json.RawMessage(body)
				}
			}
		}
		if len(d.Personal) == 0 {
			d.Personal = nil
		}
		if rows, err := l.db.Query("select account,paper,version,body from library_review_drafts"); err == nil {
			defer rows.Close()
			for rows.Next() {
				var row reviewDraftBackup
				var body string
				if rows.Scan(&row.Account, &row.Paper, &row.Version, &body) == nil && json.Valid([]byte(body)) {
					row.Body = json.RawMessage(body)
					d.ReviewDrafts = append(d.ReviewDrafts, row)
				}
			}
		}
		d.MemberIDs = l.exportMemberIDs()
		d.Seed = l.seed()
		w.Header().Set("Content-Disposition", "attachment; filename=biology-library.json")
		jsonResponse(w, d)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/library/records" {
		asset, e := l.recordsReply(r.URL.Query().Get("brief") == "1", r.URL.Query().Get("compact") == "1")
		if e != nil {
			http.Error(w, "读取失败", 500)
			return
		}
		sendAsset(w, r, asset)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/library/record" {
		var b string
		e := l.db.QueryRow("select body from library_records where id=?", r.URL.Query().Get("id")).Scan(&b)
		if errors.Is(e, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if e != nil {
			http.Error(w, "读取失败", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, b)
		return
	}
	if r.Method != "POST" || !strings.HasPrefix(r.URL.Path, "/library/") {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("X-Biology-Library") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "请求无效", 403)
		return
	}
	if r.URL.Path == "/library/ai" {
		serveLibraryAI(w, r, l.loadAIShared())
		return
	}
	if r.URL.Path == "/library/ai-settings" {
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if e != nil {
			http.Error(w, "请求过大", 400)
			return
		}
		l.serveAISettings(w, body, id == "")
		return
	}
	if r.URL.Path == "/library/digest" {
		var data []byte
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<20)).Decode(&data); err != nil {
			http.Error(w, "文件读取失败", 400)
			return
		}
		jsonResponse(w, fmt.Sprintf("%x", sha256.Sum256(data)))
		return
	}
	if r.URL.Path == "/library/convert-docx" {
		serveDocxToPdf(w, r, l.dir)
		return
	}
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<20))
	if e != nil || !json.Valid(body) {
		http.Error(w, "文件过大或数据格式无效", 400)
		return
	}
	switch r.URL.Path {
	case "/library/review-draft":
		var draft struct {
			Paper   string
			Version int
		}
		if len(body) > 4<<20 || json.Unmarshal(body, &draft) != nil || !strings.HasPrefix(draft.Paper, "paper-") || draft.Version < 1 {
			http.Error(w, "核题草稿无效或过大", 400)
			return
		}
		result, err := l.db.Exec(`insert into library_review_drafts(account,paper,version,body)
      select ?,?,?,? where ?=1 or exists(select 1 from library_review_drafts where account=? and paper=?)
      on conflict(account,paper) do update set version=excluded.version,body=excluded.body where library_review_drafts.version=excluded.version-1`,
			id, draft.Paper, draft.Version, string(body), draft.Version, id, draft.Paper)
		if err != nil {
			http.Error(w, "保存草稿失败", 500)
			return
		}
		if n, _ := result.RowsAffected(); n != 1 {
			http.Error(w, "另一标签页已更新进度，请在新标签页打开最新草稿，核对后再保存。", 409)
			return
		}
		w.WriteHeader(204)
	case "/library/favorite":
		var change struct {
			Question string
			Selected bool
		}
		if json.Unmarshal(body, &change) != nil || change.Question == "" {
			http.Error(w, "缺少题号", 400)
			return
		}
		tx, err := l.db.Begin()
		if err != nil {
			http.Error(w, "无法保存收藏", 500)
			return
		}
		defer tx.Rollback()
		_, err = tx.Exec("insert or ignore into library_favorites select ?,case when ?='' then coalesce(json_extract(body,'$.good'),'[]') else '[]' end from library_state where id=1", id, id)
		if err == nil {
			_, err = tx.Exec("update library_favorites set marks=(select json_group_array(value) from (select value from json_each(marks) where value!=? union all select ? where ?)) where account=?", change.Question, change.Question, change.Selected, id)
		}
		var marks string
		if err == nil {
			err = tx.QueryRow("select marks from library_favorites where account=?", id).Scan(&marks)
		}
		if err != nil || tx.Commit() != nil {
			http.Error(w, "收藏保存失败", 500)
			return
		}
		if id == "" {
			l.changed()
		} // The offline archive includes only the original account's favorites.
		jsonResponse(w, json.RawMessage(marks))
	case "/library/state":
		var d libraryData
		if json.Unmarshal(body, &d) != nil || len(d.State) == 0 || d.State[0] != '{' {
			http.Error(w, "格式无效", 400)
			return
		}
		if id != "" {
			var before []byte
			if l.db.QueryRow("select body from library_state where id=1 and version=?", d.Version).Scan(&before) != nil {
				http.Error(w, "另一台设备已经更新题库，请刷新后重新操作。", 409)
				return
			}
			if e := l.guardState(ownerKey(id), before, d.State); e != nil {
				http.Error(w, e.Error(), 403)
				return
			}
			if d.State, e = keepAdminFields(before, d.State); e != nil {
				http.Error(w, e.Error(), 400)
				return
			}
		}
		// The basket and saved papers are personal and saved through /library/personal.
		res, e := l.db.Exec("update library_state set body=json_remove(json_set(?, '$.good', json(coalesce(json_extract(body,'$.good'),'[]'))),'$.bag','$.editions'),version=version+1 where id=1 and version=?", string(d.State), d.Version)
		if e != nil {
			http.Error(w, "保存失败", 500)
			return
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			http.Error(w, "另一台设备已经更新题库，请刷新后重新操作。", 409)
			return
		}
		l.changed()
		jsonResponse(w, d.Version+1)
	case "/library/personal":
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			http.Error(w, "个人数据格式无效", 400)
			return
		}
		own := map[string]json.RawMessage{}
		for _, f := range personalFields {
			v := bytes.TrimSpace(fields[f])
			if len(v) == 0 || v[0] != '[' {
				http.Error(w, "个人数据格式无效", 400)
				return
			}
			own[f] = v
		}
		data, _ := json.Marshal(own)
		if _, e := l.db.Exec("insert into library_personal(account,body) values(?,?) on conflict(account) do update set body=excluded.body", id, string(data)); e != nil {
			http.Error(w, "保存失败", 500)
			return
		}
		if id == "" {
			l.changed()
		} // The offline archive includes only the original account's basket and papers.
		w.WriteHeader(204)
	case "/library/record":
		if l.online != nil {
			body, e = l.online.pictures.expand(body)
			if e != nil {
				http.Error(w, e.Error(), 409)
				return
			}
		}
		var rec struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &rec) != nil || rec.ID == "" {
			http.Error(w, "缺少记录编号", 400)
			return
		}
		if !completeIntake(body) {
			http.Error(w, "请先转写照片题面，并补齐每道题的答案和解析", 400)
			return
		}
		actor, _ := r.Context().Value(actorKey{}).(string)
		if _, _, e := checkRevision(body); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		if id != "" {
			if e := l.guardRevision(ownerKey(id), body); e != nil {
				http.Error(w, e.Error(), 403)
				return
			}
			if _, revision, _ := checkRevision(body); !revision {
				if e := l.guardUpload(ownerKey(id), body); e != nil {
					http.Error(w, e.Error(), 403)
					return
				}
			}
		}
		res, e := l.db.Exec("insert into library_records(id,body) values(?,json_set(?,'$.uploadedBy',?,'$.uploaderId',?)) on conflict(id) do nothing", rec.ID, string(body), actor, ownerKey(id))
		if e != nil {
			http.Error(w, "写入失败", 500)
			return
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			http.Error(w, "记录已经存在，请刷新后核对。", 409)
			return
		}
		l.bumpRecords()
		l.changed()
		jsonResponse(w, map[string]string{"uploadedBy": actor, "uploaderId": ownerKey(id)})
	case "/library/restore":
		if id != "" {
			http.Error(w, "仅管理员可恢复题库", 403)
			return
		}
		var d libraryData
		if json.Unmarshal(body, &d) != nil || len(d.State) == 0 || d.State[0] != '{' {
			http.Error(w, "备份格式无效", 400)
			return
		}
		if len(d.Seed) > 0 {
			var a, b struct{ StudentHash string }
			if json.Unmarshal(d.Seed, &a) != nil || json.Unmarshal(l.seed(), &b) != nil || a.StudentHash != b.StudentHash {
				http.Error(w, "内置样卷不一致，请使用导出备份时的题库页面恢复。", 400)
				return
			}
		}
		tx, e := l.db.Begin()
		if e != nil {
			http.Error(w, "打开失败", 500)
			return
		}
		defer tx.Rollback()
		res, e := tx.Exec("update library_state set body=json_remove(?,'$.bag','$.editions'),version=1 where id=1 and version=0 and not exists(select 1 from library_records)", string(d.State))
		if e != nil {
			http.Error(w, "恢复失败", 500)
			return
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			http.Error(w, "只可恢复到空库", 409)
			return
		}
		if len(d.Personal) > 0 {
			for acc, pBody := range d.Personal {
				if !json.Valid(pBody) {
					http.Error(w, "个人数据格式无效", 400)
					return
				}
				if _, e = tx.Exec("insert into library_personal(account,body) values(?,?) on conflict(account) do update set body=excluded.body", acc, string(pBody)); e != nil {
					http.Error(w, "恢复失败", 500)
					return
				}
			}
		} else {
			if _, e = tx.Exec("insert into library_personal(account,body) select '',"+fmt.Sprintf(personalSeed, "?1", "?1")+" where true on conflict(account) do update set body=excluded.body", string(d.State)); e != nil {
				http.Error(w, "恢复失败", 500)
				return
			}
		}
		for acc, marks := range d.Favorites {
			if !json.Valid(marks) {
				http.Error(w, "收藏数据格式无效", 400)
				return
			}
			if _, e = tx.Exec("insert into library_favorites(account,marks) values(?,?) on conflict(account) do update set marks=excluded.marks", acc, string(marks)); e != nil {
				http.Error(w, "恢复失败", 500)
				return
			}
		}
		for _, draft := range d.ReviewDrafts {
			if draft.Paper == "" || draft.Version < 1 || !json.Valid(draft.Body) {
				continue
			}
			if _, e = tx.Exec("insert into library_review_drafts(account,paper,version,body) values(?,?,?,?) on conflict(account,paper) do update set version=excluded.version,body=excluded.body", draft.Account, draft.Paper, draft.Version, string(draft.Body)); e != nil {
				http.Error(w, "恢复失败", 500)
				return
			}
		}
		for username, memberID := range d.MemberIDs {
			if username != "" && memberID != "" && memberID != "admin" {
				if _, e = tx.Exec("insert into library_member_ids(username,id) values(?,?) on conflict(username) do update set id=excluded.id", username, memberID); e != nil {
					http.Error(w, "恢复失败", 500)
					return
				}
			}
		}
		for _, rec := range d.Records {
			var row struct{ ID string }
			if json.Unmarshal(rec, &row) != nil || row.ID == "" {
				http.Error(w, "记录无效", 400)
				return
			}
			if _, e = tx.Exec("insert into library_records values(?,?)", row.ID, string(rec)); e != nil {
				http.Error(w, "备份记录重复或无效", 400)
				return
			}
		}
		if tx.Commit() != nil {
			http.Error(w, "恢复失败", 500)
			return
		}
		l.bumpRecords()
		l.changed()
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}
func (l *libraryPage) exportMemberIDs() map[string]string {
	out := map[string]string{}
	if rows, err := l.db.Query("select username,id from library_member_ids"); err == nil {
		defer rows.Close()
		for rows.Next() {
			var u, id string
			if rows.Scan(&u, &id) == nil && u != "" && id != "" {
				out[u] = id
			}
		}
	}
	for u, id := range l.account.memberIDMap() {
		out[u] = id
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (l *libraryPage) reservedMemberID(username string) string {
	var id string
	_ = l.db.QueryRow("select id from library_member_ids where username=?", username).Scan(&id)
	return id
}

func extractPaperDataTag(html []byte) []byte {
	const openTag = `<script id="paper-data" type="application/json">`
	const closeTag = `</script>`
	i := bytes.Index(html, []byte(openTag))
	if i < 0 {
		return nil
	}
	rest := html[i+len(openTag):]
	j := bytes.Index(rest, []byte(closeTag))
	if j < 0 {
		return nil
	}
	return html[i : i+len(openTag)+j+len(closeTag)]
}

func mergeSeedIntoAppPage(privatePage, appPage []byte) []byte {
	privSeed := extractPaperDataTag(privatePage)
	appSeed := extractPaperDataTag(appPage)
	if len(privSeed) == 0 || len(appSeed) == 0 {
		return nil
	}
	return bytes.Replace(appPage, appSeed, privSeed, 1)
}

func runSelection(path, dir, addr string) error {
	if dir == "" {
		return errors.New("需要指定独立的数据目录 --data")
	}
	page, e := os.ReadFile(path)
	if path == "/private/library.html" {
		if fallback, err := os.ReadFile("/app/library.html"); err == nil {
			if errors.Is(e, os.ErrNotExist) {
				page, e = fallback, nil
			} else if e == nil && !bytes.Contains(page, []byte("生物题册 "+releaseVersion)) {
				if merged := mergeSeedIntoAppPage(page, fallback); len(merged) > 0 {
					page = merged
				}
			}
		}
	}
	if e != nil {
		return e
	}
	if !bytes.Contains(page, []byte("library-server")) {
		return errors.New("请使用新版 library.html（可由 python3 scripts/build-page.py 生成）")
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	_ = os.MkdirAll(filepath.Join(dir, ".lo-tmp"), 0700)
	host, _, e := net.SplitHostPort(addr)
	if e != nil {
		return e
	}
	account, e := openLibraryAccount(dir, os.Getenv("BIOLOGY_LIBRARY_PASSWORD"))
	if e != nil {
		return e
	}
	loopback := host == "localhost" || net.ParseIP(host).IsLoopback()
	if !loopback && account == nil {
		return errors.New("局域网监听需要设置 BIOLOGY_LIBRARY_PASSWORD")
	}
	db, e := openLibraryDB(dir)
	if e != nil {
		return e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if e = prepareLibrary(db); e != nil {
		return e
	}
	if e = migrateOwners(db, account); e != nil {
		return e
	}
	online, err := splitOnlinePage(page)
	if err != nil {
		return err
	}
	online.pictures.loader = func(register func([]byte)) {
		rows, err := db.Query("select json_remove(body,'$.original','$.solution','$.photos','$.file') from library_records")
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var b []byte
			if rows.Scan(&b) == nil {
				register(b)
			}
		}
	}
	l := &libraryPage{db: db, account: account, page: page, dir: dir, dirty: make(chan struct{}, 1), online: online, recordsOut: map[string]recordsReply{}}
	if e = l.archive(); e != nil {
		return e
	}
	go func() {
		for range l.dirty {
			time.Sleep(time.Second)
			select {
			case <-l.dirty:
			default:
			}
			if e := l.archive(); e != nil {
				log.Print("更新离线档案失败: ", e)
			}
		}
	}()
	app := libraryLoginWithIDLookup(account, l.reservedMemberID, http.HandlerFunc(l.serve))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loopback {
			name, _, err := net.SplitHostPort(r.Host)
			if err != nil || (name != "localhost" && !net.ParseIP(name).IsLoopback()) {
				http.Error(w, "无效的主机名", 403)
				return
			}
		}
		app.ServeHTTP(w, r)
	})
	log.Printf("个人题库 http://%s", addr)
	srv := http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 8 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	return fmt.Errorf("题库服务: %w", srv.ListenAndServe())
}

var errRecordsRead = errors.New("读取失败")

// recordReply returns the listing for one brief/compact variant. A write bumps
// recordsGen, so cached replies are never served after a change.
func (l *libraryPage) recordsReply(brief, compact bool) (pageAsset, error) {
	key := fmt.Sprintf("%t|%t", brief, compact)
	l.recordsMu.Lock()
	if l.recordsOut == nil {
		l.recordsOut = map[string]recordsReply{}
	}
	gen := l.recordsGen
	if hit, ok := l.recordsOut[key]; ok && hit.gen == gen {
		l.recordsMu.Unlock()
		return hit.asset, nil
	}
	l.recordsMu.Unlock()
	rows, e := l.db.Query(`select case
		when json_extract(body,'$.paper')=1 and json_extract(body,'$.sourceKind') is null then
			json_set(
				json_remove(body,'$.original','$.solution','$.photos','$.file'),
				'$.sourceKind',
				case
					when json_array_length(body,'$.photos')>0 then 'photos'
					when lower(coalesce(json_extract(body,'$.original.name'),'')) like '%.pdf' then 'pdf'
					when json_extract(body,'$.original.name') is not null then 'docx'
					else ''
				end
			)
		else json_remove(body,'$.original','$.solution','$.photos','$.file')
	end from library_records order by rowid`)
	if e != nil {
		return pageAsset{}, errRecordsRead
	}
	defer rows.Close()
	result := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return pageAsset{}, errRecordsRead
		}
		if brief {
			b, e = recordSummary(b)
			if e != nil {
				return pageAsset{}, errRecordsRead
			}
		}
		if l.online != nil && compact {
			b = l.online.pictures.compact(b)
		}
		result = append(result, json.RawMessage(b))
	}
	if rows.Err() != nil {
		return pageAsset{}, errRecordsRead
	}
	data, e := json.Marshal(result)
	if e != nil {
		return pageAsset{}, errRecordsRead
	}
	asset := pageAsset{data, zipBytes(data), "application/json", false}
	l.recordsMu.Lock()
	if l.recordsGen == gen {
		l.recordsOut[key] = recordsReply{gen, asset}
	}
	l.recordsMu.Unlock()
	return asset, nil
}

// bumpRecords must run after a record write commits.
func (l *libraryPage) bumpRecords() {
	l.recordsMu.Lock()
	l.recordsGen++
	clear(l.recordsOut)
	l.recordsMu.Unlock()
}

// prepareLibrary creates the tables and moves pre-split baskets to the original teacher.
func prepareLibrary(db *sql.DB) error {
	if _, e := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS library_member_ids(username text primary key,id text not null); CREATE TABLE IF NOT EXISTS library_review_drafts(account text not null,paper text not null,version integer not null,body text not null,primary key(account,paper));CREATE TABLE IF NOT EXISTS library_favorites(account text primary key,marks text not null); CREATE TABLE IF NOT EXISTS library_state(id integer primary key,version integer,body text); INSERT OR IGNORE INTO library_state VALUES(1,0,'{}'); CREATE TABLE IF NOT EXISTS library_records(id text primary key,body text not null); CREATE TABLE IF NOT EXISTS library_personal(account text primary key,body text not null)"); e != nil {
		return e
	}
	// Before baskets became personal they lived in the shared state; give them to the original teacher once.
	_, e := db.Exec("INSERT OR IGNORE INTO library_personal(account,body) SELECT ''," + fmt.Sprintf(personalSeed, "body", "body") + " FROM library_state WHERE id=1")
	return e
}

// openLibraryDB opens the library database. The container has a read-only
// root and no /tmp, so SQLite must keep its temporary files (statement
// journals, sorts, temporary tables) in memory; otherwise a multi-row update
// inside a transaction fails with SQLITE_IOERR_GETTEMPPATH (6410).
func openLibraryDB(dir string) (*sql.DB, error) {
	return sql.Open("sqlite", filepath.Join(dir, "selection.sqlite")+"?_pragma=temp_store(memory)")
}
