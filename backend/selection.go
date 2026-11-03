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
	"time"
)

type libraryPage struct {
	db    *sql.DB
	page  []byte
	dir   string
	dirty chan struct{}
}
type libraryData struct {
	AIEndpoint string            `json:"aiEndpoint,omitempty"`
	State      json.RawMessage   `json:"state"`
	Version    int               `json:"version"`
	Records    []json.RawMessage `json:"records"`
	Seed       json.RawMessage   `json:"seed,omitempty"`
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
	tag := append([]byte("<script type=\"application/json\" id=\""+id+"\">"), b...)
	tag = append(tag, []byte("</script></head>")...)
	return bytes.Replace(l.page, []byte("</head>"), tag, 1), nil
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
func (l *libraryPage) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == "GET" && r.URL.Path == "/" {
		d, e := l.snapshot(false)
		if e != nil {
			http.Error(w, "读取题库失败", 500)
			return
		}
		d.AIEndpoint = os.Getenv("BIOLOGY_AI_ENDPOINT")
		p, e := l.render(d, "library-server")
		if e != nil {
			http.Error(w, "读取页面失败", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(p)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/library/backup" {
		d, e := l.snapshot(true)
		if e != nil {
			http.Error(w, "备份失败", 500)
			return
		}
		d.Seed = l.seed()
		w.Header().Set("Content-Disposition", "attachment; filename=biology-library.json")
		jsonResponse(w, d)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/library/records" {
		rows, e := l.db.Query("select json_remove(body,'$.original','$.solution','$.photos','$.file') from library_records order by rowid")
		if e != nil {
			http.Error(w, "读取失败", 500)
			return
		}
		defer rows.Close()
		result := []json.RawMessage{}
		for rows.Next() {
			var b string
			if e = rows.Scan(&b); e != nil {
				http.Error(w, "读取失败", 500)
				return
			}
			result = append(result, json.RawMessage(b))
		}
		if rows.Err() != nil {
			http.Error(w, "读取失败", 500)
			return
		}
		jsonResponse(w, result)
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
		serveLibraryAI(w, r)
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
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<20))
	if e != nil || !json.Valid(body) {
		http.Error(w, "文件过大或数据格式无效", 400)
		return
	}
	switch r.URL.Path {
	case "/library/state":
		var d libraryData
		if json.Unmarshal(body, &d) != nil || len(d.State) == 0 || d.State[0] != '{' {
			http.Error(w, "格式无效", 400)
			return
		}
		res, e := l.db.Exec("update library_state set body=?,version=version+1 where id=1 and version=?", string(d.State), d.Version)
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
	case "/library/record":
		var rec struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &rec) != nil || rec.ID == "" {
			http.Error(w, "缺少记录编号", 400)
			return
		}
		res, e := l.db.Exec("insert into library_records(id,body) values(?,?) on conflict(id) do nothing", rec.ID, string(body))
		if e != nil {
			http.Error(w, "写入失败", 500)
			return
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			http.Error(w, "记录已经存在，请刷新后核对。", 409)
			return
		}
		l.changed()
		w.WriteHeader(204)
	case "/library/restore":
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
		res, e := tx.Exec("update library_state set body=?,version=1 where id=1 and version=0 and not exists(select 1 from library_records)", string(d.State))
		if e != nil {
			http.Error(w, "恢复失败", 500)
			return
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			http.Error(w, "只可恢复到空库", 409)
			return
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
		l.changed()
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}
func runSelection(path, dir, addr string) error {
	if dir == "" {
		return errors.New("需要指定独立的数据目录 --data")
	}
	page, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if !bytes.Contains(page, []byte("library-server")) {
		return errors.New("请使用新版题库选题预览.html")
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return e
	}
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
	db, e := sql.Open("sqlite", filepath.Join(dir, "selection.sqlite"))
	if e != nil {
		return e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS library_state(id integer primary key,version integer,body text); INSERT OR IGNORE INTO library_state VALUES(1,0,'{}'); CREATE TABLE IF NOT EXISTS library_records(id text primary key,body text not null)"); e != nil {
		return e
	}
	l := &libraryPage{db, page, dir, make(chan struct{}, 1)}
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
	app := libraryLogin(account, http.HandlerFunc(l.serve))
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
	srv := http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 8 * time.Second}
	return fmt.Errorf("题库服务: %w", srv.ListenAndServe())
}
