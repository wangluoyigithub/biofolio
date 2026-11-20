package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func newTestCloudLibrary(t *testing.T, withLogin bool) (*libraryPage, http.Handler, *libraryAccount) {
	t.Helper()
	dir := t.TempDir()
	db, err := openLibraryDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := prepareLibrary(db); err != nil {
		t.Fatal(err)
	}
	var account *libraryAccount
	if withLogin {
		account, err = openLibraryAccount(dir, "admin-pass-123")
		if err != nil {
			t.Fatal(err)
		}
		if err := account.updateWithID("", "li", "teacher-pass-1", "m1"); err != nil {
			t.Fatal(err)
		}
		if err := account.updateWithID("", "wang", "teacher-pass-2", "m2"); err != nil {
			t.Fatal(err)
		}
	}
	l := &libraryPage{
		db:      db,
		account: account,
		dir:     dir,
		page:    []byte("<html><head></head><body>ok</body></html>"),
		dirty:   make(chan struct{}, 1),
	}
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.serve(w, r)
	})
	if withLogin {
		h = libraryLoginWithIDLookup(account, l.reservedMemberID, h)
	}
	return l, h, account
}

func loginTestUser(t *testing.T, h http.Handler, user, pass string) *http.Cookie {
	t.Helper()
	form := url.Values{"username": {user}, "password": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/biology/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) == 0 {
		t.Fatalf("login failed for %s: code=%d", user, w.Code)
	}
	return w.Result().Cookies()[0]
}

func callLibraryAPI(h http.Handler, method, target string, cookie *http.Cookie, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	var rBody io.Reader
	if body != nil {
		rBody = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rBody)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if method == http.MethodPost {
		req.Header.Set("X-Biology-Library", "1")
		if headers["Content-Type"] == "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type mockWebDAVItem struct {
	isDir bool
	data  []byte
	mime  string
}

type mockWebDAVServer struct {
	mu    sync.Mutex
	user  string
	pass  string
	items map[string]*mockWebDAVItem
	srv   *httptest.Server
}

func newMockWebDAVServer(t *testing.T, user, pass string) *mockWebDAVServer {
	t.Helper()
	m := &mockWebDAVServer{
		user: user,
		pass: pass,
		items: map[string]*mockWebDAVItem{
			"/dav": {isDir: true},
		},
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockWebDAVServer) handle(w http.ResponseWriter, r *http.Request) {
	if m.user != "" || m.pass != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != m.user || p != m.pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	cleanPath := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	cleanPath = strings.TrimSuffix(cleanPath, "/")
	if cleanPath == "" {
		cleanPath = "/"
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	switch r.Method {
	case "PROPFIND":
		item, ok := m.items[cleanPath]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		depth := r.Header.Get("Depth")
		var buf strings.Builder
		buf.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
		writeResp := func(p string, it *mockWebDAVItem) {
			segs := strings.Split(strings.Trim(p, "/"), "/")
			for i := range segs {
				segs[i] = url.PathEscape(segs[i])
			}
			href := "/" + strings.Join(segs, "/")
			if it.isDir && !strings.HasSuffix(href, "/") {
				href += "/"
			}
			buf.WriteString(`<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>`)
			buf.WriteString(`<D:displayname>` + path.Base(p) + `</D:displayname>`)
			if it.isDir {
				buf.WriteString(`<D:resourcetype><D:collection/></D:resourcetype>`)
			} else {
				buf.WriteString(`<D:resourcetype/>`)
				buf.WriteString(fmt.Sprintf(`<D:getcontentlength>%d</D:getcontentlength>`, len(it.data)))
				buf.WriteString(`<D:getlastmodified>Mon, 20 Nov 2026 10:00:00 GMT</D:getlastmodified>`)
				if it.mime != "" {
					buf.WriteString(`<D:getcontenttype>` + it.mime + `</D:getcontenttype>`)
				}
			}
			buf.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		}
		writeResp(cleanPath, item)
		if depth == "1" && item.isDir {
			prefix := cleanPath + "/"
			for p, it := range m.items {
				if strings.HasPrefix(p, prefix) && !strings.Contains(strings.TrimPrefix(p, prefix), "/") {
					writeResp(p, it)
				}
			}
		}
		buf.WriteString(`</D:multistatus>`)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, buf.String())

	case "MKCOL":
		parent := path.Dir(cleanPath)
		if pItem, ok := m.items[parent]; !ok || !pItem.isDir {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if _, exists := m.items[cleanPath]; exists {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m.items[cleanPath] = &mockWebDAVItem{isDir: true}
		w.WriteHeader(http.StatusCreated)

	case http.MethodPut:
		parent := path.Dir(cleanPath)
		if pItem, ok := m.items[parent]; !ok || !pItem.isDir {
			w.WriteHeader(http.StatusConflict)
			return
		}
		data, _ := io.ReadAll(r.Body)
		m.items[cleanPath] = &mockWebDAVItem{isDir: false, data: data, mime: r.Header.Get("Content-Type")}
		w.WriteHeader(http.StatusCreated)

	case http.MethodGet:
		it, ok := m.items[cleanPath]
		if !ok || it.isDir {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if it.mime != "" {
			w.Header().Set("Content-Type", it.mime)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(it.data)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestCloudSecretEncryptionAndIsolation(t *testing.T) {
	prevLoopback := cloudAllowLoopbackForTest
	cloudAllowLoopbackForTest = true
	t.Cleanup(func() { cloudAllowLoopbackForTest = prevLoopback })

	l, h, _ := newTestCloudLibrary(t, true)
	// Verify AES-GCM roundtrip & tamper detection.
	key, err := l.getCloudKey()
	if err != nil || len(key) != 32 {
		t.Fatalf("getCloudKey failed: %v", err)
	}
	cipherBytes, err := encryptCloudSecret(key, []byte("super-secret-token"))
	if err != nil || bytes.Contains(cipherBytes, []byte("super-secret-token")) {
		t.Fatal("secret leaked in ciphertext")
	}
	plain, err := decryptCloudSecret(key, cipherBytes)
	if err != nil || string(plain) != "super-secret-token" {
		t.Fatalf("decrypt failed: %v", err)
	}
	cipherBytes[len(cipherBytes)-1] ^= 0xff
	if _, err := decryptCloudSecret(key, cipherBytes); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}

	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	liCookie := loginTestUser(t, h, "li", "teacher-pass-1")
	wangCookie := loginTestUser(t, h, "wang", "teacher-pass-2")

	dav1 := newMockWebDAVServer(t, "li-dav", "li-secret-pwd-999")
	dav2 := newMockWebDAVServer(t, "wang-dav", "wang-secret-pwd-888")

	// li binds dav1
	w := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/connections", liCookie, []byte(fmt.Sprintf(
		`{"action":"save_webdav","name":"李老师的115","baseUrl":%q,"username":"li-dav","password":"li-secret-pwd-999","rootDir":"/生物"}`,
		dav1.srv.URL+"/dav",
	)), nil)
	if w.Code != 200 {
		t.Fatalf("li save webdav failed: %d %s", w.Code, w.Body.String())
	}
	var liCfg struct {
		Connections []cloudConnectionView `json:"connections"`
	}
	if json.Unmarshal(w.Body.Bytes(), &liCfg) != nil || len(liCfg.Connections) != 1 {
		t.Fatalf("unexpected li response: %s", w.Body.String())
	}
	liConnID := liCfg.Connections[0].ID

	// wang binds dav2
	w = callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/connections", wangCookie, []byte(fmt.Sprintf(
		`{"action":"save_webdav","name":"王老师的阿里云盘","baseUrl":%q,"username":"wang-dav","password":"wang-secret-pwd-888"}`,
		dav2.srv.URL+"/dav",
	)), nil)
	if w.Code != 200 {
		t.Fatalf("wang save webdav failed: %d %s", w.Code, w.Body.String())
	}

	// Config endpoint never leaks secrets and isolates per teacher.
	cfgRes := callLibraryAPI(h, http.MethodGet, "/biology/library/cloud/config", liCookie, nil, nil)
	if cfgRes.Code != 200 {
		t.Fatalf("get config failed: %d", cfgRes.Code)
	}
	bodyStr := cfgRes.Body.String()
	if strings.Contains(bodyStr, "li-secret-pwd-999") || strings.Contains(bodyStr, "wang-secret-pwd-888") || strings.Contains(bodyStr, "王老师的阿里云盘") || strings.Contains(bodyStr, "adminConfig") {
		t.Fatalf("secret or cross-account leak in config: %s", bodyStr)
	}

	// wang cannot list, upload, test, or delete li's connection.
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/biology/library/cloud/list", fmt.Sprintf(`{"connectionId":%q,"dir":""}`, liConnID)},
		{"/biology/library/cloud/connections", fmt.Sprintf(`{"action":"test","id":%q}`, liConnID)},
		{"/biology/library/cloud/connections", fmt.Sprintf(`{"action":"delete","id":%q}`, liConnID)},
	} {
		resp := callLibraryAPI(h, http.MethodPost, tc.path, wangCookie, []byte(tc.body), nil)
		if resp.Code < 400 {
			t.Fatalf("wang accessed li's drive via %s: code=%d", tc.path, resp.Code)
		}
	}

	// Admin backup & offline archive must not contain cloud credentials.
	bak := callLibraryAPI(h, http.MethodGet, "/biology/library/backup", adminCookie, nil, nil)
	if bak.Code != 200 || strings.Contains(bak.Body.String(), "li-secret-pwd-999") || strings.Contains(bak.Body.String(), liConnID) {
		t.Fatalf("backup leaked cloud info: %d", bak.Code)
	}
	offBytes, err := l.renderOfflineBytes()
	if err != nil || bytes.Contains(offBytes, []byte("li-secret-pwd-999")) || bytes.Contains(offBytes, []byte(liConnID)) {
		t.Fatalf("offline archive leaked cloud info: %v", err)
	}
}

func TestCloudSSRFAndAllowlist(t *testing.T) {
	prevLoopback := cloudAllowLoopbackForTest
	cloudAllowLoopbackForTest = false
	t.Cleanup(func() { cloudAllowLoopbackForTest = prevLoopback })

	_, h, _ := newTestCloudLibrary(t, true)
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	liCookie := loginTestUser(t, h, "li", "teacher-pass-1")

	dav := newMockWebDAVServer(t, "u", "p")

	// Loopback and private IPs are rejected for non-admin by default.
	for _, badURL := range []string{
		dav.srv.URL + "/dav",
		"http://localhost:5244/dav",
		"http://192.168.1.10:5244/dav",
		"http://user:pass@example.com/dav",
	} {
		res := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/connections", liCookie, []byte(fmt.Sprintf(
			`{"action":"save_webdav","name":"Bad","baseUrl":%q,"username":"u","password":"p"}`, badURL,
		)), nil)
		if res.Code < 400 {
			t.Fatalf("disallowed URL %s accepted for teacher: code=%d", badURL, res.Code)
		}
	}

	// Admin enables allowPrivateWebdav + adds exact dav.srv.URL+"/dav" to whitelist.
	admRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/admin-settings", adminCookie, []byte(fmt.Sprintf(
		`{"allowPrivateWebdav":true,"allowedWebdavPrefixes":[%q],"baiduAppDir":"/apps/Biofolio"}`,
		dav.srv.URL+"/dav",
	)), nil)
	if admRes.Code != 200 {
		t.Fatalf("admin settings failed: %d %s", admRes.Code, admRes.Body.String())
	}

	// validateWebDAVTargetURL now permits private RFC1918 for teacher, still rejects metadata 169.254.169.254.
	cfg := cloudAdminSettings{}
	allow := true
	cfg.AllowPrivateWebDAV = &allow
	if _, _, err := validateWebDAVTargetURL("http://192.168.1.50:5244/dav", cfg, false); err != nil {
		t.Fatalf("expected private LAN URL allowed: %v", err)
	}
	if _, _, err := validateWebDAVTargetURL("http://169.254.169.254/latest/meta-data", cfg, true); err == nil {
		t.Fatal("link-local metadata IP must always be blocked")
	}

	// Whitelisted loopback mock server now succeeds even with cloudAllowLoopbackForTest=false!
	okRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/connections", liCookie, []byte(fmt.Sprintf(
		`{"action":"save_webdav","name":"NAS OpenList","baseUrl":%q,"username":"u","password":"p"}`,
		dav.srv.URL+"/dav",
	)), nil)
	if okRes.Code != 200 {
		t.Fatalf("whitelisted WebDAV failed: %d %s", okRes.Code, okRes.Body.String())
	}
}

func TestCloudWebDAVEndToEnd(t *testing.T) {
	prevLoopback := cloudAllowLoopbackForTest
	cloudAllowLoopbackForTest = true
	t.Cleanup(func() { cloudAllowLoopbackForTest = prevLoopback })

	_, h, _ := newTestCloudLibrary(t, false)
	dav := newMockWebDAVServer(t, "teacher", "app-pass")

	res := callLibraryAPI(h, http.MethodPost, "/library/cloud/connections", nil, []byte(fmt.Sprintf(
		`{"action":"save_webdav","name":"坚果云","baseUrl":%q,"username":"teacher","password":"app-pass","rootDir":"/Biofolio题库"}`,
		dav.srv.URL+"/dav",
	)), nil)
	if res.Code != 200 {
		t.Fatalf("save_webdav failed: %d %s", res.Code, res.Body.String())
	}
	var saved struct {
		Connections []cloudConnectionView `json:"connections"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &saved)
	connID := saved.Connections[0].ID

	// Create folder inside rootDir (which doesn't exist yet on mock server - auto created!).
	mk := callLibraryAPI(h, http.MethodPost, "/library/cloud/mkdir", nil, []byte(fmt.Sprintf(
		`{"connectionId":%q,"parentDir":"","name":"高三一模"}`, connID,
	)), nil)
	if mk.Code != 200 {
		t.Fatalf("mkdir failed: %d %s", mk.Code, mk.Body.String())
	}

	// Upload Chinese filename twice with overwrite=false -> second becomes " ... (1).docx"
	filename := "2026高三期中（解析版）.docx"
	meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"connectionId":%q,"dir":"/高三一模","filename":%q,"mimeType":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","overwrite":false}`,
		connID, filename,
	)))
	up1 := callLibraryAPI(h, http.MethodPost, "/library/cloud/upload", nil, []byte("DOCX_CONTENT_V1"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": meta,
	})
	if up1.Code != 200 {
		t.Fatalf("upload 1 failed: %d %s", up1.Code, up1.Body.String())
	}
	up2 := callLibraryAPI(h, http.MethodPost, "/library/cloud/upload", nil, []byte("DOCX_CONTENT_V2"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": meta,
	})
	if up2.Code != 200 || !strings.Contains(up2.Body.String(), "2026高三期中（解析版） (1).docx") {
		t.Fatalf("upload 2 dedupe failed: %d %s", up2.Code, up2.Body.String())
	}

	// List and download
	ls := callLibraryAPI(h, http.MethodPost, "/library/cloud/list", nil, []byte(fmt.Sprintf(
		`{"connectionId":%q,"dir":"/高三一模"}`, connID,
	)), nil)
	if ls.Code != 200 {
		t.Fatalf("list failed: %d %s", ls.Code, ls.Body.String())
	}
	var listing cloudListResult
	_ = json.Unmarshal(ls.Body.Bytes(), &listing)
	if len(listing.Entries) != 2 {
		t.Fatalf("expected 2 files, got %d: %s", len(listing.Entries), ls.Body.String())
	}

	dl := callLibraryAPI(h, http.MethodPost, "/library/cloud/download", nil, []byte(fmt.Sprintf(
		`{"connectionId":%q,"fileId":%q}`, connID, "/高三一模/"+filename,
	)), nil)
	if dl.Code != 200 || dl.Body.String() != "DOCX_CONTENT_V1" {
		t.Fatalf("download mismatch: code=%d body=%q", dl.Code, dl.Body.String())
	}
	decName, _ := url.PathUnescape(dl.Header().Get("X-Cloud-Filename"))
	if decName != filename {
		t.Fatalf("filename header mismatch: %q", decName)
	}
}

func TestCloudGoogleDriveOAuthAndFiles(t *testing.T) {
	prevLoopback := cloudAllowLoopbackForTest
	prevAuth, prevTok, prevAPI, prevUp := googleAuthURL, googleTokenURL, googleDriveAPIBase, googleDriveUploadBase
	cloudAllowLoopbackForTest = true
	t.Cleanup(func() {
		cloudAllowLoopbackForTest = prevLoopback
		googleAuthURL, googleTokenURL, googleDriveAPIBase, googleDriveUploadBase = prevAuth, prevTok, prevAPI, prevUp
	})

	var mu sync.Mutex
	refreshCount := 0
	type gItem struct {
		id, name, mime, parent string
		data                   []byte
	}
	items := map[string]*gItem{
		"gdoc-1": {id: "gdoc-1", name: "生物细胞膜学案", mime: googleDocMime, parent: "root", data: []byte("EXPORTED_DOCX_BYTES")},
	}
	nextID := 100

	gSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			grant := r.Form.Get("grant_type")
			if grant == "authorization_code" {
				jsonResponse(w, map[string]any{
					"access_token":  "g-acc-initial",
					"refresh_token": "g-ref-1",
					"expires_in":    3600,
				})
				return
			}
			if grant == "refresh_token" {
				refreshCount++
				jsonResponse(w, map[string]any{
					"access_token": "g-acc-refreshed",
					"expires_in":   3600,
				})
				return
			}
		case r.URL.Path == "/drive/v3/about":
			jsonResponse(w, map[string]any{
				"user": map[string]string{"emailAddress": "teacher@school.edu"},
			})
		case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodGet:
			q := r.URL.Query().Get("q")
			var out []map[string]any
			for _, it := range items {
				if strings.Contains(q, "'"+it.parent+"' in parents") {
					if strings.Contains(q, googleFolderMime) && it.mime != googleFolderMime {
						continue
					}
					out = append(out, map[string]any{
						"id":       it.id,
						"name":     it.name,
						"mimeType": it.mime,
						"size":     strconv.Itoa(len(it.data)),
						"parents":  []string{it.parent},
					})
				}
			}
			jsonResponse(w, map[string]any{"files": out})
		case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodPost:
			var meta struct {
				Name     string   `json:"name"`
				MimeType string   `json:"mimeType"`
				Parents  []string `json:"parents"`
			}
			_ = json.NewDecoder(r.Body).Decode(&meta)
			nextID++
			id := fmt.Sprintf("gf-%d", nextID)
			parent := "root"
			if len(meta.Parents) > 0 {
				parent = meta.Parents[0]
			}
			items[id] = &gItem{id: id, name: meta.Name, mime: meta.MimeType, parent: parent}
			jsonResponse(w, map[string]any{"id": id, "name": meta.Name, "mimeType": meta.MimeType})
		case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && strings.HasSuffix(r.URL.Path, "/export"):
			fid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/drive/v3/files/"), "/export")
			it := items[fid]
			if it == nil {
				w.WriteHeader(404)
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write(it.data)
		case strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
			fid := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
			it := items[fid]
			if it == nil {
				w.WriteHeader(404)
				return
			}
			jsonResponse(w, map[string]any{
				"id":       it.id,
				"name":     it.name,
				"mimeType": it.mime,
				"size":     strconv.Itoa(len(it.data)),
				"parents":  []string{it.parent},
			})
		case r.URL.Path == "/upload/drive/v3/files":
			mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
				w.WriteHeader(400)
				return
			}
			mr := multipart.NewReader(r.Body, params["boundary"])
			var meta struct {
				Name    string   `json:"name"`
				Parents []string `json:"parents"`
			}
			var payload []byte
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					w.WriteHeader(400)
					return
				}
				b, _ := io.ReadAll(part)
				if strings.Contains(part.Header.Get("Content-Type"), "application/json") {
					_ = json.Unmarshal(b, &meta)
				} else {
					payload = b
				}
			}
			nextID++
			id := fmt.Sprintf("gf-%d", nextID)
			parent := "root"
			if len(meta.Parents) > 0 {
				parent = meta.Parents[0]
			}
			items[id] = &gItem{id: id, name: meta.Name, mime: docxMimeType, parent: parent, data: payload}
			jsonResponse(w, map[string]any{"id": id, "name": meta.Name, "mimeType": docxMimeType})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(gSrv.Close)

	googleAuthURL = gSrv.URL + "/auth"
	googleTokenURL = gSrv.URL + "/token"
	googleDriveAPIBase = gSrv.URL + "/drive/v3"
	googleDriveUploadBase = gSrv.URL + "/upload/drive/v3"

	l, h, _ := newTestCloudLibrary(t, true)
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	liCookie := loginTestUser(t, h, "li", "teacher-pass-1")
	wangCookie := loginTestUser(t, h, "wang", "teacher-pass-2")

	// Admin saves shared Google OAuth credentials.
	if r := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/admin-settings", adminCookie, []byte(
		`{"googleClientId":"shared-cid","googleClientSecret":"shared-csec"}`,
	), nil); r.Code != 200 {
		t.Fatalf("admin google config failed: %d %s", r.Code, r.Body.String())
	}

	// 1. Web popup callback without session cookie (simulating SameSite=Strict cross-site redirect).
	startRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/google/start", liCookie, []byte(
		`{"mode":"web","name":"李老师的谷歌盘","rootDir":""}`,
	), nil)
	if startRes.Code != 200 {
		t.Fatalf("google start failed: %d %s", startRes.Code, startRes.Body.String())
	}
	var startData struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	_ = json.Unmarshal(startRes.Body.Bytes(), &startData)
	cbURL := "/biology/library/cloud/google/callback?state=" + url.QueryEscape(startData.State) + "&code=web-code-1"
	cbRes := callLibraryAPI(h, http.MethodGet, cbURL, nil, nil, nil) // NO cookie!
	if cbRes.Code != 200 || !strings.Contains(cbRes.Body.String(), "biofolio-cloud-oauth") || !strings.Contains(cbRes.Body.String(), `"ok":true`) {
		t.Fatalf("cross-site callback failed: %d %s", cbRes.Code, cbRes.Body.String())
	}

	// Check li now sees Google Drive connection.
	liCfgRes := callLibraryAPI(h, http.MethodGet, "/biology/library/cloud/config", liCookie, nil, nil)
	var liCfg struct {
		Connections []cloudConnectionView `json:"connections"`
	}
	_ = json.Unmarshal(liCfgRes.Body.Bytes(), &liCfg)
	if len(liCfg.Connections) != 1 || liCfg.Connections[0].AccountLabel != "teacher@school.edu" {
		t.Fatalf("li google connection missing: %s", liCfgRes.Body.String())
	}

	// Force token expiry and test auto-refresh + Google Docs export.
	liConnID := liCfg.Connections[0].ID
	row, plain, err := l.getCloudConnection("m1", liConnID)
	if err != nil {
		t.Fatal(err)
	}
	var sec gdriveSecret
	_ = json.Unmarshal(plain, &sec)
	sec.ExpiryUnix = 1 // expired!
	secBytes, _ := json.Marshal(sec)
	_ = l.updateCloudConnectionSecret(row.Account, row.ID, secBytes, sec.Email)

	dlRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/download", liCookie, []byte(fmt.Sprintf(
		`{"connectionId":%q,"fileId":"gdoc-1"}`, liConnID,
	)), nil)
	if dlRes.Code != 200 || dlRes.Body.String() != "EXPORTED_DOCX_BYTES" {
		t.Fatalf("google doc export download failed: %d %s", dlRes.Code, dlRes.Body.String())
	}
	decName, _ := url.PathUnescape(dlRes.Header().Get("X-Cloud-Filename"))
	if decName != "生物细胞膜学案.docx" || refreshCount == 0 {
		t.Fatalf("expected .docx export and token refresh, got name=%q refreshCount=%d", decName, refreshCount)
	}

	// 2. Manual paste mode for wang (LAN IP).
	wStart := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/google/start", wangCookie, []byte(
		`{"mode":"manual","name":"王老师谷歌盘","rootDir":"BioDir"}`,
	), nil)
	var wData struct {
		State       string `json:"state"`
		RedirectURI string `json:"redirectUri"`
	}
	_ = json.Unmarshal(wStart.Body.Bytes(), &wData)
	if wData.RedirectURI != googleManualRedirect {
		t.Fatalf("expected manual redirect uri, got %q", wData.RedirectURI)
	}
	compRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/google/complete", wangCookie, []byte(fmt.Sprintf(
		`{"state":%q,"input":%q}`, wData.State, "http://localhost:1/?state="+wData.State+"&code=manual-code-2",
	)), nil)
	if compRes.Code != 200 {
		t.Fatalf("manual complete failed: %d %s", compRes.Code, compRes.Body.String())
	}
	var wDone struct {
		Connection cloudConnectionView `json:"connection"`
	}
	_ = json.Unmarshal(compRes.Body.Bytes(), &wDone)

	// Upload into wang's configured rootDir ("BioDir" -> auto-created!).
	meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"connectionId":%q,"dir":"","filename":"周练.docx","mimeType":%q,"overwrite":false}`,
		wDone.Connection.ID, docxMimeType,
	)))
	upRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/upload", wangCookie, []byte("GDRIVE_UPLOAD_DATA"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": meta,
	})
	if upRes.Code != 200 {
		t.Fatalf("gdrive upload failed: %d %s", upRes.Code, upRes.Body.String())
	}
	lsRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/list", wangCookie, []byte(fmt.Sprintf(
		`{"connectionId":%q,"dir":""}`, wDone.Connection.ID,
	)), nil)
	if lsRes.Code != 200 || !strings.Contains(lsRes.Body.String(), "周练.docx") {
		t.Fatalf("uploaded file missing from BioDir listing: %d %s", lsRes.Code, lsRes.Body.String())
	}
}

func TestCloudBaiduDeviceCodeAndChunkUpload(t *testing.T) {
	prevLoopback := cloudAllowLoopbackForTest
	prevOAuth, prevPan, prevPCS := baiduOAuthBase, baiduPanBase, baiduPCSBase
	cloudAllowLoopbackForTest = true
	t.Cleanup(func() {
		cloudAllowLoopbackForTest = prevLoopback
		baiduOAuthBase, baiduPanBase, baiduPCSBase = prevOAuth, prevPan, prevPCS
	})

	var mu sync.Mutex
	pollNum := 0
	uploadedChunks := map[int][]byte{}
	var finalFile []byte
	var finalPath string
	var createdFsID uint64 = 987654321

	var srvURL string
	bSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/oauth/device/code":
			jsonResponse(w, map[string]any{
				"device_code":      "dev-code-1",
				"user_code":        "1234-5678",
				"verification_url": srvURL + "/verify",
				"qrcode_url":       srvURL + "/qr.png",
				"expires_in":       300,
				"interval":         1,
			})
		case "/qr.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case "/oauth/token":
			pollNum++
			if pollNum == 1 {
				w.WriteHeader(400)
				jsonResponse(w, map[string]any{"error": "authorization_pending"})
				return
			}
			jsonResponse(w, map[string]any{
				"access_token":  "bd-acc-1",
				"refresh_token": "bd-ref-1",
				"expires_in":    2592000,
			})
		case "/xpan/nas":
			jsonResponse(w, map[string]any{"netdisk_name": "生物王老师"})
		case "/xpan/file":
			method := r.URL.Query().Get("method")
			switch method {
			case "precreate":
				if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
					t.Errorf("precreate must be form-urlencoded, got %s", r.Header.Get("Content-Type"))
				}
				_ = r.ParseForm()
				var bl []string
				_ = json.Unmarshal([]byte(r.Form.Get("block_list")), &bl)
				if len(bl) != 2 {
					t.Errorf("expected 2 chunks in block_list, got %d", len(bl))
				}
				jsonResponse(w, map[string]any{
					"errno":       0,
					"uploadid":    "up-id-1",
					"return_type": 1,
					"block_list":  []int{0, 1},
				})
			case "create":
				if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
					t.Errorf("create must be form-urlencoded")
				}
				_ = r.ParseForm()
				finalPath = r.Form.Get("path")
				finalFile = append(append([]byte(nil), uploadedChunks[0]...), uploadedChunks[1]...)
				jsonResponse(w, map[string]any{
					"errno": 0,
					"fs_id": createdFsID,
					"path":  finalPath,
					"size":  len(finalFile),
				})
			case "list":
				jsonResponse(w, map[string]any{
					"errno": 0,
					"list": []map[string]any{
						{
							"fs_id":           createdFsID,
							"path":            finalPath,
							"server_filename": path.Base(finalPath),
							"isdir":           0,
							"size":            len(finalFile),
							"server_mtime":    1763600000,
						},
					},
				})
			}
		case "/pcs/superfile2":
			if r.Header.Get("User-Agent") != baiduUserAgent {
				t.Errorf("superfile2 UA mismatch: %q", r.Header.Get("User-Agent"))
			}
			seq, _ := strconv.Atoi(r.URL.Query().Get("partseq"))
			f, _, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(400)
				return
			}
			b, _ := io.ReadAll(f)
			f.Close()
			uploadedChunks[seq] = b
			jsonResponse(w, map[string]any{"md5": fmt.Sprintf("%x", md5.Sum(b))})
		case "/xpan/multimedia":
			if r.Header.Get("User-Agent") != baiduUserAgent {
				w.WriteHeader(403)
				jsonResponse(w, map[string]any{"errno": 31362})
				return
			}
			jsonResponse(w, map[string]any{
				"errno": 0,
				"list": []map[string]any{
					{
						"fs_id":    createdFsID,
						"filename": path.Base(finalPath),
						"isdir":    0,
						"dlink":    srvURL + "/pcs-dlink?file=1",
					},
				},
			})
		case "/pcs-dlink":
			if r.Header.Get("User-Agent") != baiduUserAgent {
				w.WriteHeader(403)
				return
			}
			http.Redirect(w, r, srvURL+"/pcs-cdn/data", http.StatusFound)
		case "/pcs-cdn/data":
			if r.Header.Get("User-Agent") != baiduUserAgent {
				w.WriteHeader(403)
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write(finalFile)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(bSrv.Close)
	srvURL = bSrv.URL

	baiduOAuthBase = srvURL + "/oauth"
	baiduPanBase = srvURL + "/xpan"
	baiduPCSBase = srvURL + "/pcs"

	_, h, _ := newTestCloudLibrary(t, true)
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	liCookie := loginTestUser(t, h, "li", "teacher-pass-1")

	if r := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/admin-settings", adminCookie, []byte(
		`{"baiduAppKey":"bd-key","baiduSecretKey":"bd-sec","baiduAppDir":"/apps/Biofolio"}`,
	), nil); r.Code != 200 {
		t.Fatalf("admin baidu config failed: %d %s", r.Code, r.Body.String())
	}

	startRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/baidu/start", liCookie, []byte(
		`{"name":"百度盘","rootDir":"/生物试卷"}`,
	), nil)
	if startRes.Code != 200 {
		t.Fatalf("baidu start failed: %d %s", startRes.Code, startRes.Body.String())
	}
	var bStart struct {
		SessionID string `json:"sessionId"`
		QRDataURL string `json:"qrDataUrl"`
	}
	_ = json.Unmarshal(startRes.Body.Bytes(), &bStart)
	if !strings.HasPrefix(bStart.QRDataURL, "data:image/png;base64,") {
		t.Fatalf("missing QR data url: %q", bStart.QRDataURL)
	}

	p1 := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/baidu/poll", liCookie, []byte(fmt.Sprintf(`{"sessionId":%q}`, bStart.SessionID)), nil)
	if p1.Code != 200 || !strings.Contains(p1.Body.String(), `"status":"pending"`) {
		t.Fatalf("expected pending on first poll: %d %s", p1.Code, p1.Body.String())
	}
	p2 := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/baidu/poll", liCookie, []byte(fmt.Sprintf(`{"sessionId":%q}`, bStart.SessionID)), nil)
	if p2.Code != 200 || !strings.Contains(p2.Body.String(), `"status":"connected"`) {
		t.Fatalf("expected connected on second poll: %d %s", p2.Code, p2.Body.String())
	}
	var p2Data struct {
		Connections []cloudConnectionView `json:"connections"`
	}
	_ = json.Unmarshal(p2.Body.Bytes(), &p2Data)
	connID := p2Data.Connections[0].ID

	// Upload > 4MB file (5MB + 123B) -> 2 chunks.
	bigData := bytes.Repeat([]byte("BIOFOLIO12345678"), (5*1024*1024+123)/16)
	sum0 := md5.Sum(bigData[:baiduChunkSize])
	_ = hex.EncodeToString(sum0[:])
	meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"connectionId":%q,"dir":"","filename":"大体积PPT讲评.pptx","mimeType":"application/octet-stream","overwrite":true}`,
		connID,
	)))
	upRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/upload", liCookie, bigData, map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": meta,
	})
	if upRes.Code != 200 {
		t.Fatalf("baidu chunk upload failed: %d %s", upRes.Code, upRes.Body.String())
	}
	if finalPath != "/apps/Biofolio/生物试卷/大体积PPT讲评.pptx" || !bytes.Equal(finalFile, bigData) {
		t.Fatalf("baidu assembled path/bytes mismatch: path=%q len=%d want=%d", finalPath, len(finalFile), len(bigData))
	}

	lsRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/list", liCookie, []byte(fmt.Sprintf(`{"connectionId":%q,"dir":""}`, connID)), nil)
	var lsData cloudListResult
	_ = json.Unmarshal(lsRes.Body.Bytes(), &lsData)
	if len(lsData.Entries) != 1 {
		t.Fatalf("baidu list failed: %d %s", lsRes.Code, lsRes.Body.String())
	}
	dlRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/download", liCookie, []byte(fmt.Sprintf(
		`{"connectionId":%q,"fileId":%q}`, connID, lsData.Entries[0].ID,
	)), nil)
	if dlRes.Code != 200 || !bytes.Equal(dlRes.Body.Bytes(), bigData) {
		t.Fatalf("baidu download failed: code=%d len=%d", dlRes.Code, dlRes.Body.Len())
	}
}

func TestCloudAdminBackupAndRestore(t *testing.T) {
	prevLoopback := cloudAllowLoopbackForTest
	cloudAllowLoopbackForTest = true
	t.Cleanup(func() { cloudAllowLoopbackForTest = prevLoopback })

	dav := newMockWebDAVServer(t, "adm", "adm-pass")

	// Source server with 1 question.
	srcLib, srcHandler, _ := newTestCloudLibrary(t, true)
	srcCookie := loginTestUser(t, srcHandler, "teacher", "admin-pass-123")
	if r := callLibraryAPI(srcHandler, http.MethodPost, "/biology/library/record", srcCookie, []byte(
		`{"id":"rec-cloud-1","questions":[{"id":"q-cloud-1","stem":"云盘备份测试题干","answer":"A","a":"解析内容"}]}`,
	), nil); r.Code != 200 {
		t.Fatalf("add record failed: %d %s", r.Code, r.Body.String())
	}
	connRes := callLibraryAPI(srcHandler, http.MethodPost, "/biology/library/cloud/connections", srcCookie, []byte(fmt.Sprintf(
		`{"action":"save_webdav","name":"备份盘","baseUrl":%q,"username":"adm","password":"adm-pass"}`,
		dav.srv.URL+"/dav",
	)), nil)
	var srcConns struct {
		Connections []cloudConnectionView `json:"connections"`
	}
	_ = json.Unmarshal(connRes.Body.Bytes(), &srcConns)
	srcConnID := srcConns.Connections[0].ID

	// Export JSON backup & offline archive directly to cloud.
	expJSON := callLibraryAPI(srcHandler, http.MethodPost, "/biology/library/cloud/backup-export", srcCookie, []byte(fmt.Sprintf(
		`{"connectionId":%q,"dir":"","kind":"json","overwrite":true}`, srcConnID,
	)), nil)
	if expJSON.Code != 200 {
		t.Fatalf("cloud backup-export json failed: %d %s", expJSON.Code, expJSON.Body.String())
	}
	var expData struct {
		Entry cloudEntry `json:"entry"`
	}
	_ = json.Unmarshal(expJSON.Body.Bytes(), &expData)

	expOff := callLibraryAPI(srcHandler, http.MethodPost, "/biology/library/cloud/backup-export", srcCookie, []byte(fmt.Sprintf(
		`{"connectionId":%q,"dir":"","kind":"offline","overwrite":true}`, srcConnID,
	)), nil)
	if expOff.Code != 200 {
		t.Fatalf("cloud backup-export offline failed: %d %s", expOff.Code, expOff.Body.String())
	}
	_ = srcLib

	// Restore onto a brand-new empty library instance from the same cloud drive.
	dstLib, dstHandler, _ := newTestCloudLibrary(t, true)
	dstCookie := loginTestUser(t, dstHandler, "teacher", "admin-pass-123")
	dstConnRes := callLibraryAPI(dstHandler, http.MethodPost, "/biology/library/cloud/connections", dstCookie, []byte(fmt.Sprintf(
		`{"action":"save_webdav","name":"恢复盘","baseUrl":%q,"username":"adm","password":"adm-pass"}`,
		dav.srv.URL+"/dav",
	)), nil)
	var dstConns struct {
		Connections []cloudConnectionView `json:"connections"`
	}
	_ = json.Unmarshal(dstConnRes.Body.Bytes(), &dstConns)
	dstConnID := dstConns.Connections[0].ID

	restRes := callLibraryAPI(dstHandler, http.MethodPost, "/biology/library/cloud/backup-restore", dstCookie, []byte(fmt.Sprintf(
		`{"connectionId":%q,"fileId":%q}`, dstConnID, expData.Entry.ID,
	)), nil)
	if restRes.Code != http.StatusNoContent {
		t.Fatalf("cloud backup-restore failed: %d %s", restRes.Code, restRes.Body.String())
	}
	snap, err := dstLib.snapshot(true)
	if err != nil || len(snap.Records) != 1 || !bytes.Contains(snap.Records[0], []byte("云盘备份测试题干")) {
		t.Fatalf("restored snapshot mismatch: err=%v records=%d", err, len(snap.Records))
	}

	// Verify second restore on non-empty library is rejected with 409.
	restAgain := callLibraryAPI(dstHandler, http.MethodPost, "/biology/library/cloud/backup-restore", dstCookie, []byte(fmt.Sprintf(
		`{"connectionId":%q,"fileId":%q}`, dstConnID, expData.Entry.ID,
	)), nil)
	if restAgain.Code != http.StatusConflict {
		t.Fatalf("expected 409 on non-empty restore, got %d", restAgain.Code)
	}
}

// Verify normalizeBaiduAppDir & context helpers cleanly reject unsafe inputs.
func TestCloudPathNormalization(t *testing.T) {
	if got, err := normalizeBaiduAppDir("Biofolio"); err != nil || got != "/apps/Biofolio" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := normalizeBaiduAppDir("/apps/a/b"); err == nil {
		t.Fatal("nested /apps path should fail")
	}
	if _, err := normalizeRelativeCloudDir("/foo/../bar"); err == nil {
		t.Fatal("path traversal must fail")
	}
	if _, err := cleanCloudFilename("bad/name.docx"); err == nil {
		t.Fatal("slash in filename must fail")
	}
	_ = context.Background()
	_ = sql.ErrNoRows
	_ = filepath.Join("", "")
}

func TestCloudBuiltPageSplit(t *testing.T) {
	script := filepath.Join("..", "scripts", "build-page.py")
	if _, err := os.Stat(script); err != nil {
		t.Skip("build-page.py not present in standalone backend context")
	}
	outPath := filepath.Join(t.TempDir(), "library.html")
	cmd := exec.Command("python3", script, "--out", outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build-page.py failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	online, err := splitOnlinePage(raw)
	if err != nil {
		t.Fatalf("splitOnlinePage failed on built page: %v", err)
	}
	if !bytes.Contains(online.html, []byte("const cloudDrive")) {
		t.Fatal("cloudDrive missing from online html")
	}
	if !bytes.Contains(online.html, []byte(`ref="cloudDialog"`)) {
		t.Fatal("cloudDialog missing from online html")
	}
}
