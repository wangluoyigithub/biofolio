package main

import (
	"bytes"
	"crypto/sha256"
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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncMockItem struct {
	id       string
	name     string
	mime     string
	parent   string
	data     []byte
	modified string
	trashed  bool
}

type syncMockResumable struct {
	parent      string
	overwriteID string
	name        string
	mime        string
	buf         bytes.Buffer
}

type mockSyncGDriveServer struct {
	mu           sync.Mutex
	srv          *httptest.Server
	items        map[string]*syncMockItem
	resumable    map[string]*syncMockResumable
	nextID       int
	uploadCounts map[string]int
	uploadOrder  []string
	uploadDelay  time.Duration
	uploadStatus int
	onUpload     func(relPath string)
}

func newMockSyncGDriveServer(t *testing.T) *mockSyncGDriveServer {
	t.Helper()
	m := &mockSyncGDriveServer{
		items:        make(map[string]*syncMockItem),
		resumable:    make(map[string]*syncMockResumable),
		nextID:       100,
		uploadCounts: make(map[string]int),
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockSyncGDriveServer) bindForTest(t *testing.T) {
	t.Helper()
	prevLoopback := cloudAllowLoopbackForTest
	prevAuth, prevTok, prevAPI, prevUp := googleAuthURL, googleTokenURL, googleDriveAPIBase, googleDriveUploadBase
	cloudAllowLoopbackForTest = true
	googleAuthURL = m.srv.URL + "/auth"
	googleTokenURL = m.srv.URL + "/token"
	googleDriveAPIBase = m.srv.URL + "/drive/v3"
	googleDriveUploadBase = m.srv.URL + "/upload/drive/v3"
	t.Cleanup(func() {
		cloudAllowLoopbackForTest = prevLoopback
		googleAuthURL, googleTokenURL, googleDriveAPIBase, googleDriveUploadBase = prevAuth, prevTok, prevAPI, prevUp
	})
}

func (m *mockSyncGDriveServer) fullPathLocked(it *syncMockItem) string {
	var segs []string
	cur := it
	for cur != nil {
		segs = append(segs, cur.name)
		if cur.parent == "" || cur.parent == "root" {
			break
		}
		cur = m.items[cur.parent]
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return strings.Join(segs, "/")
}

func (m *mockSyncGDriveServer) recordUploadLocked(it *syncMockItem) {
	p := m.fullPathLocked(it)
	m.uploadCounts[p]++
	m.uploadOrder = append(m.uploadOrder, p)
	cb := m.onUpload
	if cb != nil {
		go cb(p)
	}
}

func (m *mockSyncGDriveServer) resetStats() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploadCounts = make(map[string]int)
	m.uploadOrder = nil
}

func (m *mockSyncGDriveServer) countFor(fullPath string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.uploadCounts[fullPath]
}

func (m *mockSyncGDriveServer) lastUploadedPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.uploadOrder) == 0 {
		return ""
	}
	return m.uploadOrder[len(m.uploadOrder)-1]
}

func (m *mockSyncGDriveServer) orderCopy() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.uploadOrder...)
}

func (m *mockSyncGDriveServer) readFile(fullPath string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	segs := strings.Split(strings.Trim(fullPath, "/"), "/")
	cur := "root"
	var matched *syncMockItem
	for _, seg := range segs {
		matched = nil
		for _, it := range m.items {
			if !it.trashed && it.parent == cur && it.name == seg {
				matched = it
				break
			}
		}
		if matched == nil {
			return nil, false
		}
		cur = matched.id
	}
	if matched == nil || matched.mime == googleFolderMime {
		return nil, false
	}
	return append([]byte(nil), matched.data...), true
}

func (m *mockSyncGDriveServer) putFile(fullPath, mimeType string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	segs := strings.Split(strings.Trim(fullPath, "/"), "/")
	cur := "root"
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < len(segs)-1; i++ {
		seg := segs[i]
		var dir *syncMockItem
		for _, it := range m.items {
			if !it.trashed && it.parent == cur && it.name == seg && it.mime == googleFolderMime {
				dir = it
				break
			}
		}
		if dir == nil {
			m.nextID++
			id := fmt.Sprintf("sgf-%d", m.nextID)
			dir = &syncMockItem{id: id, name: seg, mime: googleFolderMime, parent: cur, modified: now}
			m.items[id] = dir
		}
		cur = dir.id
	}
	name := segs[len(segs)-1]
	for _, it := range m.items {
		if !it.trashed && it.parent == cur && it.name == name && it.mime != googleFolderMime {
			it.data = append([]byte(nil), data...)
			it.mime = mimeType
			it.modified = now
			return
		}
	}
	m.nextID++
	id := fmt.Sprintf("sgf-%d", m.nextID)
	m.items[id] = &syncMockItem{
		id: id, name: name, mime: mimeType, parent: cur, data: append([]byte(nil), data...), modified: now,
	}
}

func (m *mockSyncGDriveServer) mkdirAll(fullPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	segs := strings.Split(strings.Trim(fullPath, "/"), "/")
	cur := "root"
	now := time.Now().UTC().Format(time.RFC3339)
	for _, seg := range segs {
		var dir *syncMockItem
		for _, it := range m.items {
			if !it.trashed && it.parent == cur && it.name == seg && it.mime == googleFolderMime {
				dir = it
				break
			}
		}
		if dir == nil {
			m.nextID++
			id := fmt.Sprintf("sgf-%d", m.nextID)
			dir = &syncMockItem{id: id, name: seg, mime: googleFolderMime, parent: cur, modified: now}
			m.items[id] = dir
		}
		cur = dir.id
	}
}

func (m *mockSyncGDriveServer) deleteFile(fullPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	segs := strings.Split(strings.Trim(fullPath, "/"), "/")
	cur := "root"
	var matched *syncMockItem
	for _, seg := range segs {
		matched = nil
		for _, it := range m.items {
			if !it.trashed && it.parent == cur && it.name == seg {
				matched = it
				break
			}
		}
		if matched == nil {
			return
		}
		cur = matched.id
	}
	if matched != nil {
		matched.trashed = true
	}
}

func (m *mockSyncGDriveServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		jsonResponse(w, map[string]any{
			"access_token":  "sync-acc-tok",
			"refresh_token": "sync-ref-tok",
			"expires_in":    3600,
		})

	case r.URL.Path == "/drive/v3/about" && r.Method == http.MethodGet:
		jsonResponse(w, map[string]any{
			"user": map[string]string{"emailAddress": "sync@school.edu"},
		})

	case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodGet:
		q := r.URL.Query().Get("q")
		wantParent := ""
		if pm := mockQueryParentRe.FindStringSubmatch(q); len(pm) == 2 {
			wantParent = unescapeMockDriveQuery(pm[1])
		}
		hasName := false
		wantName := ""
		if nm := mockQueryNameRe.FindStringSubmatch(q); len(nm) == 2 {
			hasName = true
			wantName = unescapeMockDriveQuery(nm[1])
		}
		wantMime := ""
		if mm := mockQueryMimeRe.FindStringSubmatch(q); len(mm) == 2 {
			wantMime = mm[1]
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []map[string]any
		for _, it := range m.items {
			if it.trashed {
				continue
			}
			if wantParent != "" && it.parent != wantParent {
				continue
			}
			if hasName && it.name != wantName {
				continue
			}
			if wantMime != "" && it.mime != wantMime {
				continue
			}
			out = append(out, map[string]any{
				"id":           it.id,
				"name":         it.name,
				"mimeType":     it.mime,
				"size":         strconv.Itoa(len(it.data)),
				"modifiedTime": it.modified,
				"parents":      []string{it.parent},
			})
		}
		jsonResponse(w, map[string]any{"files": out})

	case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodPost:
		var meta struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		if json.NewDecoder(r.Body).Decode(&meta) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		parent := "root"
		if len(meta.Parents) > 0 && meta.Parents[0] != "" {
			parent = meta.Parents[0]
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.nextID++
		id := fmt.Sprintf("sgf-%d", m.nextID)
		now := time.Now().UTC().Format(time.RFC3339)
		m.items[id] = &syncMockItem{id: id, name: meta.Name, mime: meta.MimeType, parent: parent, modified: now}
		jsonResponse(w, map[string]any{"id": id, "name": meta.Name, "mimeType": meta.MimeType, "modifiedTime": now})

	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && r.Method == http.MethodGet:
		fid := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		m.mu.Lock()
		it := m.items[fid]
		if it == nil || it.trashed {
			m.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		cp := *it
		cp.data = append([]byte(nil), it.data...)
		m.mu.Unlock()
		if r.URL.Query().Get("alt") == "media" {
			w.Header().Set("Content-Type", cp.mime)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cp.data)
			return
		}
		jsonResponse(w, map[string]any{
			"id":           cp.id,
			"name":         cp.name,
			"mimeType":     cp.mime,
			"size":         strconv.Itoa(len(cp.data)),
			"modifiedTime": cp.modified,
			"parents":      []string{cp.parent},
			"trashed":      cp.trashed,
		})

	case r.URL.Path == "/upload/drive/v3/files" && r.Method == http.MethodPost:
		m.mu.Lock()
		delay := m.uploadDelay
		status := m.uploadStatus
		m.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if r.URL.Query().Get("uploadType") == "resumable" {
			var meta struct {
				Name    string   `json:"name"`
				Parents []string `json:"parents"`
			}
			_ = json.NewDecoder(r.Body).Decode(&meta)
			parent := "root"
			if len(meta.Parents) > 0 && meta.Parents[0] != "" {
				parent = meta.Parents[0]
			}
			m.mu.Lock()
			m.nextID++
			sessID := fmt.Sprintf("sess-%d", m.nextID)
			m.resumable[sessID] = &syncMockResumable{parent: parent, name: meta.Name, mime: r.Header.Get("X-Upload-Content-Type")}
			m.mu.Unlock()
			w.Header().Set("Location", m.srv.URL+"/upload/drive/v3/session/"+sessID)
			w.WriteHeader(http.StatusOK)
			return
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		var meta struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		var payload []byte
		partMime := "application/json"
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			b, _ := io.ReadAll(part)
			ct := part.Header.Get("Content-Type")
			if strings.Contains(ct, "application/json") && meta.Name == "" {
				_ = json.Unmarshal(b, &meta)
			} else {
				payload = b
				if ct != "" {
					partMime = ct
				}
			}
		}
		parent := "root"
		if len(meta.Parents) > 0 && meta.Parents[0] != "" {
			parent = meta.Parents[0]
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.nextID++
		id := fmt.Sprintf("sgf-%d", m.nextID)
		now := time.Now().UTC().Format(time.RFC3339)
		it := &syncMockItem{id: id, name: meta.Name, mime: partMime, parent: parent, data: payload, modified: now}
		m.items[id] = it
		m.recordUploadLocked(it)
		jsonResponse(w, map[string]any{"id": id, "name": it.name, "mimeType": it.mime, "size": strconv.Itoa(len(payload)), "modifiedTime": now})

	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/files/") && r.Method == http.MethodPatch:
		m.mu.Lock()
		delay := m.uploadDelay
		status := m.uploadStatus
		m.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		fid := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files/")
		if r.URL.Query().Get("uploadType") == "resumable" {
			m.mu.Lock()
			it := m.items[fid]
			if it == nil || it.trashed {
				m.mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				return
			}
			m.nextID++
			sessID := fmt.Sprintf("sess-%d", m.nextID)
			m.resumable[sessID] = &syncMockResumable{overwriteID: fid, name: it.name, mime: r.Header.Get("X-Upload-Content-Type")}
			m.mu.Unlock()
			w.Header().Set("Location", m.srv.URL+"/upload/drive/v3/session/"+sessID)
			w.WriteHeader(http.StatusOK)
			return
		}
		payload, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		defer m.mu.Unlock()
		it := m.items[fid]
		if it == nil || it.trashed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		it.data = payload
		it.modified = time.Now().UTC().Format(time.RFC3339)
		m.recordUploadLocked(it)
		jsonResponse(w, map[string]any{"id": it.id, "name": it.name, "mimeType": it.mime, "size": strconv.Itoa(len(it.data)), "modifiedTime": it.modified})

	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/session/") && r.Method == http.MethodPut:
		sessID := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/session/")
		chunk, _ := io.ReadAll(r.Body)
		var start, end, total int
		_, _ = fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
		m.mu.Lock()
		defer m.mu.Unlock()
		sess := m.resumable[sessID]
		if sess == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sess.buf.Write(chunk)
		if end+1 < total {
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		delete(m.resumable, sessID)
		finalBytes := append([]byte(nil), sess.buf.Bytes()...)
		now := time.Now().UTC().Format(time.RFC3339)
		var it *syncMockItem
		if sess.overwriteID != "" {
			it = m.items[sess.overwriteID]
			it.data = finalBytes
			it.modified = now
		} else {
			m.nextID++
			id := fmt.Sprintf("sgf-%d", m.nextID)
			it = &syncMockItem{id: id, name: sess.name, mime: sess.mime, parent: sess.parent, data: finalBytes, modified: now}
			m.items[id] = it
		}
		m.recordUploadLocked(it)
		jsonResponse(w, map[string]any{"id": it.id, "name": it.name, "mimeType": it.mime, "size": strconv.Itoa(len(finalBytes)), "modifiedTime": now})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func bindAdminGDriveForSyncTest(t *testing.T, l *libraryPage, rootDir string) string {
	t.Helper()
	secBytes, _ := json.Marshal(gdriveSecret{
		RefreshToken: "ref-sync",
		AccessToken:  "acc-sync",
		ExpiryUnix:   time.Now().Add(2 * time.Hour).Unix(),
		Email:        "sync@school.edu",
	})
	row := cloudConnectionRow{
		ID:           "sync-gd-" + newCloudID(),
		Account:      adminOwner,
		Kind:         "gdrive",
		Name:         "SyncDrive",
		AccountLabel: "sync@school.edu",
		RootDir:      rootDir,
		IsDefault:    true,
		UseProxy:     false,
	}
	if err := l.saveCloudConnection(row, secBytes); err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func syncTestPostJSON(t *testing.T, h http.Handler, path string, cookie *http.Cookie, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if payload == nil {
		body = []byte("{}")
	} else {
		body, _ = json.Marshal(payload)
	}
	return callLibraryAPI(h, http.MethodPost, path, cookie, body, nil)
}

func syncTestMustPost(t *testing.T, h http.Handler, path string, cookie *http.Cookie, payload any) *httptest.ResponseRecorder {
	t.Helper()
	r := syncTestPostJSON(t, h, path, cookie, payload)
	if r.Code < 200 || r.Code >= 300 {
		t.Fatalf("POST %s failed: %d %s", path, r.Code, r.Body.String())
	}
	return r
}

func syncTestQuestion(id, stem string) map[string]any {
	return map[string]any{
		"id": id,
		"questions": []map[string]any{
			{
				"id":          id,
				"stem":        stem,
				"contentHtml": "<p>" + stem + "</p>",
				"answer":      "A",
				"a":           "解析",
				"type":        "单选题",
			},
		},
	}
}

func TestSyncShardSplitAndPerPathUploadCounts(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	l, h, _ := newTestCloudLibrary(t, true)
	l.db.SetMaxOpenConns(1)
	l.syncDebounce = 5 * time.Second
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	liCookie := loginTestUser(t, h, "li", "teacher-pass-1")
	bindAdminGDriveForSyncTest(t, l, "Biofolio")

	heavyDocx := bytes.Repeat([]byte("H"), (4<<20)+1024)
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, map[string]any{
		"id": "paper-heavy", "paper": true, "sourceKind": "docx", "name": "重型原卷",
		"questions": []any{}, "links": []any{},
		"original": map[string]any{
			"name": "重型.docx",
			"type": docxMimeType,
			"data": "data:" + docxMimeType + ";base64," + base64.StdEncoding.EncodeToString(heavyDocx),
		},
	})
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-1", "题干1"))
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-2", "题干2"))

	syncTestMustPost(t, h, "/biology/library/cloud/sync/push", adminCookie, nil)

	if c := gd.countFor("Biofolio/同步库/papers/paper-heavy.json"); c != 1 {
		t.Fatalf("expected paper-heavy.json upload count 1, got %d", c)
	}
	if last := gd.lastUploadedPath(); last != "Biofolio/同步库/manifest.json" {
		t.Fatalf("expected manifest.json uploaded last, got %q", last)
	}

	// Verify empty buckets are NOT uploaded (only buckets with records exist).
	var bucketManifest cloudSyncManifest
	mBytes, ok := gd.readFile("Biofolio/同步库/manifest.json")
	if !ok || json.Unmarshal(mBytes, &bucketManifest) != nil {
		t.Fatalf("manifest.json missing or invalid: ok=%v", ok)
	}
	bucketShardCount := 0
	for k := range bucketManifest.Shards {
		if strings.HasPrefix(k, "records/bucket_") {
			bucketShardCount++
		}
	}
	if bucketShardCount == 0 || bucketShardCount > 2 {
		t.Fatalf("expected 1-2 non-empty bucket shards for 2 questions, got %d", bucketShardCount)
	}

	// Incremental mutations: add question, revision, delete, favorites (admin + m1), teachingNote, review-draft, personal.
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-new", "新增题"))
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, map[string]any{
		"id":       "revision:q-1:1",
		"before":   map[string]any{"id": "q-1", "stem": "题干1"},
		"question": map[string]any{"id": "q-1", "stem": "题干1改", "answer": "B", "a": "新析"},
	})
	syncTestMustPost(t, h, "/biology/library/state", adminCookie, map[string]any{
		"version": 0,
		"state": map[string]any{
			"deletedQuestions": []string{"q-2"},
			"fixes":            map[string]any{"q-1": map[string]any{"teachingNote": "讲评重点"}},
		},
	})
	syncTestMustPost(t, h, "/biology/library/favorite", adminCookie, map[string]any{"question": "q-1", "selected": true})
	syncTestMustPost(t, h, "/biology/library/favorite", liCookie, map[string]any{"question": "q-1", "selected": true})
	syncTestMustPost(t, h, "/biology/library/review-draft", adminCookie, map[string]any{"paper": "paper-heavy", "version": 1, "items": []any{}})
	syncTestMustPost(t, h, "/biology/library/personal", adminCookie, map[string]any{
		"bag":      []string{"q-1"},
		"editions": []map[string]any{{"id": "ed-1", "name": "周测1"}},
	})

	syncTestMustPost(t, h, "/biology/library/cloud/sync/push", adminCookie, nil)

	// Heavy paper MUST remain at upload count 1!
	if c := gd.countFor("Biofolio/同步库/papers/paper-heavy.json"); c != 1 {
		t.Fatalf("paper-heavy.json was re-uploaded! count=%d", c)
	}
	for _, mustHave := range []string{
		"Biofolio/同步库/state.json",
		"Biofolio/同步库/favorites/__admin__.json",
		"Biofolio/同步库/favorites/m1.json",
		"Biofolio/同步库/personal/__admin__.json",
		"Biofolio/同步库/manifest.json",
	} {
		if gd.countFor(mustHave) < 2 && strings.HasSuffix(mustHave, "state.json") {
			t.Fatalf("expected %s uploaded at least twice, got %d", mustHave, gd.countFor(mustHave))
		}
		if _, ok := gd.readFile(mustHave); !ok {
			t.Fatalf("missing remote shard %s", mustHave)
		}
	}
	draftPath := "Biofolio/同步库/drafts/" + draftShardFilename("", "paper-heavy")
	if _, ok := gd.readFile(draftPath); !ok {
		t.Fatalf("missing flat draft shard %s", draftPath)
	}
	if last := gd.lastUploadedPath(); last != "Biofolio/同步库/manifest.json" {
		t.Fatalf("expected manifest.json uploaded last after incremental push, got %q", last)
	}
}

func TestSyncDebounceCoalescingAndLostWriteSafety(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	l, h, _ := newTestCloudLibrary(t, true)
	l.db.SetMaxOpenConns(1)
	l.syncDebounce = 120 * time.Millisecond
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, l, "Biofolio")

	// 1. Coalesce 5 rapid mutations into 1 manifest upload.
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-coal", "合并题"))
	syncTestMustPost(t, h, "/biology/library/favorite", adminCookie, map[string]any{"question": "q-coal", "selected": true})
	syncTestMustPost(t, h, "/biology/library/state", adminCookie, map[string]any{"version": 0, "state": map[string]any{"aliases": map[string]string{"a": "b"}}})
	syncTestMustPost(t, h, "/biology/library/personal", adminCookie, map[string]any{"bag": []string{"q-coal"}, "editions": []any{}})
	syncTestMustPost(t, h, "/biology/library/review-draft", adminCookie, map[string]any{"paper": "paper-coal", "version": 1})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && gd.countFor("Biofolio/同步库/manifest.json") == 0 {
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if c := gd.countFor("Biofolio/同步库/manifest.json"); c != 1 {
		t.Fatalf("expected 1 coalesced manifest upload, got %d", c)
	}

	// 2. Mid-flight mutation during upload must NOT be lost!
	gd.resetStats()
	uploadStarted := make(chan struct{}, 1)
	gd.mu.Lock()
	gd.uploadDelay = 250 * time.Millisecond
	gd.onUpload = func(relPath string) {
		if strings.HasSuffix(relPath, "state.json") {
			select {
			case uploadStarted <- struct{}{}:
			default:
			}
		}
	}
	gd.mu.Unlock()

	syncTestMustPost(t, h, "/biology/library/state", adminCookie, map[string]any{"version": 1, "state": map[string]any{"aliases": map[string]string{"a": "c"}}})
	select {
	case <-uploadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first upload to begin")
	}
	// Mutate mid-flight while manifest/other uploads are delayed.
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-midflight", "飞行中新增"))
	gd.mu.Lock()
	gd.uploadDelay = 0
	gd.onUpload = nil
	gd.mu.Unlock()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, ok := gd.readFile("Biofolio/同步库/manifest.json"); ok && strings.Contains(string(b), "records/bucket_") {
			// Also verify q-midflight reached its bucket on Drive.
			var m cloudSyncManifest
			if json.Unmarshal(b, &m) == nil && m.RecordCount == 2 {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	b, ok := gd.readFile("Biofolio/同步库/manifest.json")
	var m cloudSyncManifest
	if !ok || json.Unmarshal(b, &m) != nil || m.RecordCount != 2 {
		t.Fatalf("mid-flight mutation was lost! manifest=%s", string(b))
	}
}

func TestSyncStartupCheckAndStatusMatrix(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	l, h, _ := newTestCloudLibrary(t, true)
	l.db.SetMaxOpenConns(1)
	l.syncDebounce = 10 * time.Second
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	liCookie := loginTestUser(t, h, "li", "teacher-pass-1")

	readStatus := func(cookie *http.Cookie) cloudSyncStatusResponse {
		r := callLibraryAPI(h, http.MethodGet, "/biology/library/cloud/sync/status", cookie, nil, nil)
		if r.Code != 200 {
			t.Fatalf("GET sync/status failed: %d %s", r.Code, r.Body.String())
		}
		var out cloudSyncStatusResponse
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 1. Unconfigured
	if st := readStatus(adminCookie); st.Configured || st.Status != "unconfigured" {
		t.Fatalf("expected unconfigured, got %+v", st)
	}
	if stLi := readStatus(liCookie); stLi.Configured || stLi.Status != "unconfigured" {
		t.Fatalf("expected unconfigured for teacher read, got %+v", stLi)
	}

	// Non-admin cannot call check/push/restore.
	for _, ep := range []string{"/biology/library/cloud/sync/check", "/biology/library/cloud/sync/push", "/biology/library/cloud/sync/restore"} {
		if r := syncTestPostJSON(t, h, ep, liCookie, nil); r.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-admin on %s, got %d", ep, r.Code)
		}
	}

	// 2. Idle (configured, empty local + empty remote)
	bindAdminGDriveForSyncTest(t, l, "Biofolio")
	l.StartCloudSync()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := readStatus(adminCookie); st.Remote.Checked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := readStatus(adminCookie); !st.Configured || st.Status != "idle" {
		t.Fatalf("expected idle on empty local + empty remote, got %+v", st)
	}

	// 3. remote_empty_local_populated (with empty 同步库/ folder too)
	gd.mkdirAll("Biofolio/同步库")
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-st-1", "状态题"))
	chk := syncTestMustPost(t, h, "/biology/library/cloud/sync/check", adminCookie, nil)
	var chkResp cloudSyncStatusResponse
	_ = json.Unmarshal(chk.Body.Bytes(), &chkResp)
	if chkResp.Status != "remote_empty_local_populated" || !chkResp.Remote.Empty || chkResp.Local.RecordCount != 1 {
		t.Fatalf("expected remote_empty_local_populated, got %+v", chkResp)
	}

	// 4. in_sync
	syncTestMustPost(t, h, "/biology/library/cloud/sync/push", adminCookie, nil)
	if st := readStatus(adminCookie); st.Status != "in_sync" || st.Dirty {
		t.Fatalf("expected in_sync, got %+v", st)
	}

	// 5. local_ahead
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-st-2", "本地领先"))
	if st := readStatus(adminCookie); st.Status != "local_ahead" || !st.Dirty {
		t.Fatalf("expected local_ahead, got %+v", st)
	}

	// 6. diverged (remote bumped while local dirty)
	mBytes, _ := gd.readFile("Biofolio/同步库/manifest.json")
	var m cloudSyncManifest
	_ = json.Unmarshal(mBytes, &m)
	m.SyncVersion += 5
	bumped, _ := json.Marshal(m)
	gd.putFile("Biofolio/同步库/manifest.json", "application/json", bumped)

	syncTestMustPost(t, h, "/biology/library/cloud/sync/check", adminCookie, nil)
	if st := readStatus(adminCookie); st.Status != "diverged" {
		t.Fatalf("expected diverged, got %+v", st)
	}

	// 7. remote_ahead (push/re-sync, then bump remote while local clean)
	syncTestMustPost(t, h, "/biology/library/cloud/sync/push", adminCookie, nil)
	mBytes, _ = gd.readFile("Biofolio/同步库/manifest.json")
	_ = json.Unmarshal(mBytes, &m)
	m.SyncVersion += 3
	bumped, _ = json.Marshal(m)
	gd.putFile("Biofolio/同步库/manifest.json", "application/json", bumped)
	syncTestMustPost(t, h, "/biology/library/cloud/sync/check", adminCookie, nil)
	if st := readStatus(adminCookie); st.Status != "remote_ahead" {
		t.Fatalf("expected remote_ahead, got %+v", st)
	}

	// 8. error (corrupt manifest.json)
	gd.putFile("Biofolio/同步库/manifest.json", "application/json", []byte("{not-json"))
	syncTestMustPost(t, h, "/biology/library/cloud/sync/check", adminCookie, nil)
	if st := readStatus(adminCookie); st.Status != "error" || st.Ok {
		t.Fatalf("expected status=error & ok=false on corrupt manifest, got %+v", st)
	}

	// Verify SQLite records were never touched by startup check or status checks.
	var recCnt int
	_ = l.db.QueryRow(`SELECT count(*) FROM library_records`).Scan(&recCnt)
	if recCnt != 2 {
		t.Fatalf("expected 2 local records untouched, got %d", recCnt)
	}
}

func TestSyncEmptyRemoteGuard409(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	l, h, _ := newTestCloudLibrary(t, true)
	l.db.SetMaxOpenConns(1)
	l.syncDebounce = 10 * time.Second
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, l, "Biofolio")

	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-guard", "保护"))
	syncTestMustPost(t, h, "/biology/library/favorite", adminCookie, map[string]any{"question": "q-guard", "selected": true})
	syncTestMustPost(t, h, "/biology/library/personal", adminCookie, map[string]any{"bag": []string{"q-guard"}, "editions": []any{}})
	syncTestMustPost(t, h, "/biology/library/review-draft", adminCookie, map[string]any{"paper": "paper-guard", "version": 1})

	// Case A: 同步库/ absent; Case B: 同步库/ empty dir; Case C: manifest with empty shards.
	for step := 0; step < 3; step++ {
		if step == 1 {
			gd.mkdirAll("Biofolio/同步库")
		} else if step == 2 {
			gd.putFile("Biofolio/同步库/manifest.json", "application/json", []byte(`{"schemaVersion":1,"syncVersion":1,"shards":{}}`))
		}
		for _, mode := range []string{"merge", "overwrite"} {
			r := syncTestPostJSON(t, h, "/biology/library/cloud/sync/restore", adminCookie, map[string]any{"mode": mode})
			if r.Code != http.StatusConflict {
				t.Fatalf("step=%d mode=%s expected 409, got %d %s", step, mode, r.Code, r.Body.String())
			}
		}
	}

	var cnt int
	_ = l.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-guard'`).Scan(&cnt)
	if cnt != 1 {
		t.Fatalf("local record lost despite 409 guard")
	}
}

func TestSyncCrossMachineRestoreMergeAndOverwrite(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	// NAS-A populates full dataset and pushes.
	lA, hA, _ := newTestCloudLibrary(t, true)
	lA.db.SetMaxOpenConns(1)
	lA.syncDebounce = 10 * time.Second
	lA.page = []byte(`<html><head><script id="paper-data" type="application/json">{"studentHash":"hash-A","questions":[]}</script></head><body></body></html>`)
	adminA := loginTestUser(t, hA, "teacher", "admin-pass-123")
	liA := loginTestUser(t, hA, "li", "teacher-pass-1")
	bindAdminGDriveForSyncTest(t, lA, "Biofolio")

	syncTestMustPost(t, hA, "/biology/library/record", adminA, map[string]any{
		"id": "paper-cm", "paper": true, "sourceKind": "docx", "name": "跨机卷", "questions": []any{}, "links": []any{},
	})
	syncTestMustPost(t, hA, "/biology/library/record", adminA, syncTestQuestion("q-cm-1", "远端题1"))
	syncTestMustPost(t, hA, "/biology/library/state", adminA, map[string]any{
		"version": 0,
		"state": map[string]any{
			"fixes":            map[string]any{"q-cm-1": map[string]any{"teachingNote": "注意审题"}},
			"deletedQuestions": []string{"q-del"},
		},
	})
	syncTestMustPost(t, hA, "/biology/library/favorite", adminA, map[string]any{"question": "q-cm-1", "selected": true})
	syncTestMustPost(t, hA, "/biology/library/favorite", liA, map[string]any{"question": "q-cm-1", "selected": true})
	syncTestMustPost(t, hA, "/biology/library/personal", adminA, map[string]any{
		"bag":      []string{"q-cm-1"},
		"editions": []map[string]any{{"id": "ed-cm", "name": "跨机卷子"}},
	})
	syncTestMustPost(t, hA, "/biology/library/review-draft", adminA, map[string]any{"paper": "paper-cm", "version": 1, "items": []any{}})
	syncTestMustPost(t, hA, "/biology/library/cloud/sync/push", adminA, nil)

	// Mismatch seed -> 400
	lBad, hBad, _ := newTestCloudLibrary(t, true)
	lBad.db.SetMaxOpenConns(1)
	lBad.page = []byte(`<html><head><script id="paper-data" type="application/json">{"studentHash":"hash-OTHER","questions":[]}</script></head><body></body></html>`)
	adminBad := loginTestUser(t, hBad, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lBad, "Biofolio")
	if r := syncTestPostJSON(t, hBad, "/biology/library/cloud/sync/restore", adminBad, map[string]any{"mode": "overwrite"}); r.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on seed studentHash mismatch, got %d %s", r.Code, r.Body.String())
	}

	// (1) Empty NAS-B merge restore
	lB, hB, _ := newTestCloudLibrary(t, true)
	lB.db.SetMaxOpenConns(1)
	lB.syncDebounce = 10 * time.Second
	lB.page = []byte(`<html><head><script id="paper-data" type="application/json">{"studentHash":"hash-A","questions":[]}</script></head><body></body></html>`)
	adminB := loginTestUser(t, hB, "teacher", "admin-pass-123")
	liB := loginTestUser(t, hB, "li", "teacher-pass-1")
	bindAdminGDriveForSyncTest(t, lB, "Biofolio")

	// Snapshot credentials/settings before restore.
	snapFiles := map[string][]byte{}
	for _, fn := range []string{"library-account.json", "cloud-secret.key"} {
		b, err := os.ReadFile(filepath.Join(lB.dir, fn))
		if err != nil {
			t.Fatalf("missing %s: %v", fn, err)
		}
		snapFiles[fn] = b
	}

	rest1 := syncTestMustPost(t, hB, "/biology/library/cloud/sync/restore", adminB, map[string]any{"mode": "merge"})
	var restResp map[string]any
	_ = json.Unmarshal(rest1.Body.Bytes(), &restResp)
	if restResp["status"] != "in_sync" || restResp["dirty"] != false || restResp["lastSyncError"] != "" {
		t.Fatalf("unexpected post-restore status: %s", rest1.Body.String())
	}

	// Verify files and connections untouched.
	for fn, want := range snapFiles {
		got, err := os.ReadFile(filepath.Join(lB.dir, fn))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s modified during restore", fn)
		}
	}
	var connCnt int
	_ = lB.db.QueryRow(`SELECT count(*) FROM library_cloud_connections`).Scan(&connCnt)
	if connCnt != 1 {
		t.Fatalf("expected 1 preserved connection, got %d", connCnt)
	}
	// Non-admin forbidden
	if r := syncTestPostJSON(t, hB, "/biology/library/cloud/sync/restore", liB, map[string]any{"mode": "merge"}); r.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin restore, got %d", r.Code)
	}

	// Post-restore incremental edit + push must NOT re-upload paper-cm.json!
	gd.resetStats()
	syncTestMustPost(t, hB, "/biology/library/record", adminB, syncTestQuestion("q-cm-2", "恢复后新增"))
	syncTestMustPost(t, hB, "/biology/library/cloud/sync/push", adminB, nil)
	if c := gd.countFor("Biofolio/同步库/papers/paper-cm.json"); c != 0 {
		t.Fatalf("paper-cm.json needlessly re-uploaded after restore: count=%d", c)
	}

	// (2) Non-empty NAS-C merge preserves q-local + merges q-cm-1
	lC, hC, _ := newTestCloudLibrary(t, true)
	lC.db.SetMaxOpenConns(1)
	lC.syncDebounce = 10 * time.Second
	adminC := loginTestUser(t, hC, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lC, "Biofolio")
	syncTestMustPost(t, hC, "/biology/library/record", adminC, syncTestQuestion("q-local", "本地独有"))
	syncTestMustPost(t, hC, "/biology/library/personal", adminC, map[string]any{
		"bag":      []string{"q-local"},
		"editions": []map[string]any{{"id": "ed-local", "name": "本地卷"}},
	})
	syncTestMustPost(t, hC, "/biology/library/cloud/sync/restore", adminC, map[string]any{"mode": "merge"})
	var mergedCnt int
	_ = lC.db.QueryRow(`SELECT count(*) FROM library_records WHERE id IN ('q-local','q-cm-1','q-cm-2','paper-cm')`).Scan(&mergedCnt)
	if mergedCnt != 4 {
		var ids []string
		if rows, err := lC.db.Query(`SELECT id FROM library_records ORDER BY id`); err == nil {
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
		}
		mb, _ := gd.readFile("Biofolio/同步库/manifest.json")
		t.Fatalf("merge restore expected 4 records, got %d (ids=%v, manifest=%s)", mergedCnt, ids, string(mb))
	}
	var perBody string
	_ = lC.db.QueryRow(`SELECT body FROM library_personal WHERE account=''`).Scan(&perBody)
	if !strings.Contains(perBody, "ed-local") || !strings.Contains(perBody, "ed-cm") {
		t.Fatalf("merge restore missing local or remote edition: %s", perBody)
	}

	// (3) Overwrite replaces q-local with remote set
	syncTestMustPost(t, hC, "/biology/library/cloud/sync/restore", adminC, map[string]any{"mode": "overwrite"})
	var staleCnt int
	_ = lC.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-local'`).Scan(&staleCnt)
	if staleCnt != 0 {
		t.Fatalf("overwrite restore did not remove q-local")
	}
}

func TestSyncCorruptOrMissingShardAbortsBeforeSQLite(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	lA, hA, _ := newTestCloudLibrary(t, true)
	lA.db.SetMaxOpenConns(1)
	adminA := loginTestUser(t, hA, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lA, "Biofolio")
	syncTestMustPost(t, hA, "/biology/library/record", adminA, syncTestQuestion("q-remote-ok", "远端题"))
	syncTestMustPost(t, hA, "/biology/library/cloud/sync/push", adminA, nil)

	lB, hB, _ := newTestCloudLibrary(t, true)
	lB.db.SetMaxOpenConns(1)
	adminB := loginTestUser(t, hB, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lB, "Biofolio")
	syncTestMustPost(t, hB, "/biology/library/record", adminB, syncTestQuestion("q-keep", "保留题"))

	// Save valid bucket bytes, then tamper bucket hash.
	bktName := fmt.Sprintf("Biofolio/同步库/records/bucket_%02d.json", syncRecordBucket("q-remote-ok"))
	origBkt, ok := gd.readFile(bktName)
	if !ok {
		t.Fatalf("missing %s", bktName)
	}
	gd.putFile(bktName, "application/json", []byte(`{"bucket":0,"records":[{"id":"tampered"}]}`))

	for _, mode := range []string{"overwrite", "merge"} {
		r := syncTestPostJSON(t, hB, "/biology/library/cloud/sync/restore", adminB, map[string]any{"mode": mode})
		if r.Code == http.StatusOK {
			t.Fatalf("mode %s succeeded despite tampered shard!", mode)
		}
	}
	var keepCnt, remCnt int
	_ = lB.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-keep'`).Scan(&keepCnt)
	_ = lB.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-remote-ok'`).Scan(&remCnt)
	if keepCnt != 1 || remCnt != 0 {
		t.Fatalf("tampered shard mutated SQLite: keep=%d rem=%d", keepCnt, remCnt)
	}

	// Restore bucket, delete state.json -> must also abort before touching SQLite.
	gd.putFile(bktName, "application/json", origBkt)
	gd.deleteFile("Biofolio/同步库/state.json")
	if r := syncTestPostJSON(t, hB, "/biology/library/cloud/sync/restore", adminB, map[string]any{"mode": "overwrite"}); r.Code == http.StatusOK {
		t.Fatalf("overwrite succeeded despite missing state.json")
	}
	_ = lB.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-keep'`).Scan(&keepCnt)
	_ = lB.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-remote-ok'`).Scan(&remCnt)
	if keepCnt != 1 || remCnt != 0 {
		t.Fatalf("missing shard mutated SQLite: keep=%d rem=%d", keepCnt, remCnt)
	}
}

func TestSyncV094SchemaUpgradeCompatibility(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	dir := t.TempDir()
	db0, err := sql.Open("sqlite", filepath.Join(dir, "selection.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// Exact v0.9.4 DDL without any sync tables.
	v094DDL := `
	PRAGMA journal_mode=WAL;
	CREATE TABLE library_member_ids(username text primary key, id text not null);
	CREATE TABLE library_review_drafts(account text not null, paper text not null, version integer not null, body text not null, primary key(account, paper));
	CREATE TABLE library_favorites(account text primary key, marks text not null);
	CREATE TABLE library_state(id integer primary key, version integer, body text);
	CREATE TABLE library_records(id text primary key, body text not null);
	CREATE TABLE library_personal(account text primary key, body text not null);
	CREATE TABLE library_cloud_connections(id text primary key, account text not null, kind text not null, name text not null, account_label text not null default '', root_dir text not null default '', is_default integer not null default 0, use_proxy integer not null default 0, secret blob not null, updated_at text not null);
	`
	if _, err := db0.Exec(v094DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`INSERT INTO library_state VALUES(1, 5, '{"good":["q-v094"],"aliases":{"p-v094":"旧卷"}}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`INSERT INTO library_records VALUES('q-v094', '{"id":"q-v094","questions":[{"id":"q-v094","stem":"旧版题"}]}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`INSERT INTO library_favorites VALUES('m1', '["q-v094"]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`INSERT INTO library_personal VALUES('', '{"bag":["q-v094"],"editions":[{"id":"ed-v094","name":"旧组卷"}]}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`INSERT INTO library_review_drafts VALUES('', 'paper-v094', 2, '{"paper":"paper-v094","version":2}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`INSERT INTO library_member_ids VALUES('li', 'm1')`); err != nil {
		t.Fatal(err)
	}
	_ = db0.Close()

	// Re-open with v0.9.5 openLibraryDB + prepareLibrary + migrateOwners.
	db, err := openLibraryDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if err := prepareLibrary(db); err != nil {
		t.Fatal(err)
	}
	acc, err := openLibraryAccount(dir, "admin-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateOwners(db, acc); err != nil {
		t.Fatal(err)
	}

	l := &libraryPage{
		db:           db,
		account:      acc,
		dir:          dir,
		page:         []byte("<html><head></head><body>ok</body></html>"),
		dirty:        make(chan struct{}, 1),
		syncDebounce: 10 * time.Second,
	}
	t.Cleanup(l.Close)
	h := libraryLoginWithHooks(acc, l.reservedMemberID, l.markSyncDirty, http.HandlerFunc(l.serve))
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, l, "生物题库")

	var integrity string
	_ = l.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	if integrity != "ok" {
		t.Fatalf("integrity_check failed: %q", integrity)
	}

	chk := syncTestMustPost(t, h, "/biology/library/cloud/sync/check", adminCookie, nil)
	var st cloudSyncStatusResponse
	_ = json.Unmarshal(chk.Body.Bytes(), &st)
	if st.Status != "remote_empty_local_populated" || st.RootDir != "生物题库" {
		t.Fatalf("unexpected v0.9.4 upgrade status: %+v", st)
	}
	if r := syncTestPostJSON(t, h, "/biology/library/cloud/sync/restore", adminCookie, map[string]any{"mode": "overwrite"}); r.Code != http.StatusConflict {
		t.Fatalf("expected 409 on empty remote, got %d", r.Code)
	}

	syncTestMustPost(t, h, "/biology/library/cloud/sync/push", adminCookie, nil)
	if _, ok := gd.readFile("生物题库/同步库/manifest.json"); !ok {
		t.Fatal("expected manifest.json under custom root_dir 生物题库/同步库/")
	}
}

func parseSyncTestHomeState(t *testing.T, h http.Handler, cookie *http.Cookie) map[string]any {
	t.Helper()
	res := callLibraryAPI(h, http.MethodGet, "/biology/", cookie, nil, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /biology/ failed: %d %s", res.Code, res.Body.String())
	}
	_, tail, ok := bytes.Cut(res.Body.Bytes(), []byte(`<script type="application/json" id="library-server">`))
	if !ok {
		t.Fatalf("missing library-server script in GET /biology/: %s", res.Body.String())
	}
	raw, _, ok := bytes.Cut(tail, []byte("</script>"))
	if !ok {
		t.Fatalf("missing </script> in GET /biology/")
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("invalid library-server JSON: %v", err)
	}
	st, _ := payload["state"].(map[string]any)
	if st == nil {
		t.Fatalf("missing state object in library-server payload: %s", string(raw))
	}
	return st
}

func TestSyncV094LegacyGoodFavoritesMergeAndOverwrite(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	// 1. Build exact v0.9.4 DB on NAS-A with $.good=["q-v094"] and NO library_favorites row for "".
	dirA := t.TempDir()
	db0, err := sql.Open("sqlite", filepath.Join(dirA, "selection.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	v094DDL := `
	PRAGMA journal_mode=WAL;
	CREATE TABLE library_member_ids(username text primary key, id text not null);
	CREATE TABLE library_review_drafts(account text not null, paper text not null, version integer not null, body text not null, primary key(account, paper));
	CREATE TABLE library_favorites(account text primary key, marks text not null);
	CREATE TABLE library_state(id integer primary key, version integer, body text);
	CREATE TABLE library_records(id text primary key, body text not null);
	CREATE TABLE library_personal(account text primary key, body text not null);
	CREATE TABLE library_cloud_connections(id text primary key, account text not null, kind text not null, name text not null, account_label text not null default '', root_dir text not null default '', is_default integer not null default 0, use_proxy integer not null default 0, secret blob not null, updated_at text not null);
	`
	if _, err := db0.Exec(v094DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`INSERT INTO library_state VALUES(1, 5, '{"good":["q-v094"],"aliases":{"p-v094":"旧卷"}}')`); err != nil {
		t.Fatal(err)
	}
	qBytes, _ := json.Marshal(syncTestQuestion("q-v094", "旧版收藏题"))
	if _, err := db0.Exec(`INSERT INTO library_records VALUES('q-v094', ?)`, string(qBytes)); err != nil {
		t.Fatal(err)
	}
	_ = db0.Close()

	dbA, err := openLibraryDB(dirA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbA.Close() })
	dbA.SetMaxOpenConns(1)
	if err := prepareLibrary(dbA); err != nil {
		t.Fatal(err)
	}
	accA, err := openLibraryAccount(dirA, "admin-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateOwners(dbA, accA); err != nil {
		t.Fatal(err)
	}
	lA := &libraryPage{
		db:           dbA,
		account:      accA,
		dir:          dirA,
		page:         []byte("<html><head></head><body>ok</body></html>"),
		dirty:        make(chan struct{}, 1),
		syncDebounce: 10 * time.Second,
	}
	t.Cleanup(lA.Close)
	hA := libraryLoginWithHooks(accA, lA.reservedMemberID, lA.markSyncDirty, http.HandlerFunc(lA.serve))
	adminA := loginTestUser(t, hA, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lA, "Biofolio")
	syncTestMustPost(t, hA, "/biology/library/cloud/sync/push", adminA, nil)

	// 2. NAS-B already has a library_favorites row for "" -> ["q-nasb"]; merge restore must preserve BOTH.
	lB, hB, _ := newTestCloudLibrary(t, true)
	lB.db.SetMaxOpenConns(1)
	lB.syncDebounce = 10 * time.Second
	adminB := loginTestUser(t, hB, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lB, "Biofolio")
	syncTestMustPost(t, hB, "/biology/library/record", adminB, syncTestQuestion("q-nasb", "NAS-B收藏题"))
	syncTestMustPost(t, hB, "/biology/library/favorite", adminB, map[string]any{"question": "q-nasb", "selected": true})

	syncTestMustPost(t, hB, "/biology/library/cloud/sync/restore", adminB, map[string]any{"mode": "merge"})
	stB := parseSyncTestHomeState(t, hB, adminB)
	goodB := fmt.Sprint(stB["good"])
	if !strings.Contains(goodB, "q-nasb") || !strings.Contains(goodB, "q-v094") {
		t.Fatalf("GET /biology/ on NAS-B missing merged favorites: good=%v", stB["good"])
	}

	// 3. Also verify legacy remote bundle WITHOUT favorites/__admin__.json merges into existing library_favorites[""] AND overwrites cleanly.
	mRaw, ok := gd.readFile("Biofolio/同步库/manifest.json")
	if !ok {
		t.Fatal("missing remote manifest.json")
	}
	var m cloudSyncManifest
	if err := json.Unmarshal(mRaw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m.Shards, "favorites/__admin__.json")
	m.ShardCount = len(m.Shards)
	m.SyncVersion++
	gd.deleteFile("Biofolio/同步库/favorites/__admin__.json")
	mUpdated, _ := json.Marshal(m)
	gd.putFile("Biofolio/同步库/manifest.json", "application/json", mUpdated)

	lC, hC, _ := newTestCloudLibrary(t, true)
	lC.db.SetMaxOpenConns(1)
	lC.syncDebounce = 10 * time.Second
	adminC := loginTestUser(t, hC, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lC, "Biofolio")
	syncTestMustPost(t, hC, "/biology/library/record", adminC, syncTestQuestion("q-nasc", "NAS-C收藏题"))
	syncTestMustPost(t, hC, "/biology/library/favorite", adminC, map[string]any{"question": "q-nasc", "selected": true})

	syncTestMustPost(t, hC, "/biology/library/cloud/sync/restore", adminC, map[string]any{"mode": "merge"})
	stC := parseSyncTestHomeState(t, hC, adminC)
	goodC := fmt.Sprint(stC["good"])
	if !strings.Contains(goodC, "q-nasc") || !strings.Contains(goodC, "q-v094") {
		t.Fatalf("GET /biology/ on NAS-C missing legacy $.good merge without __admin__.json: good=%v", stC["good"])
	}

	syncTestMustPost(t, hC, "/biology/library/cloud/sync/restore", adminC, map[string]any{"mode": "overwrite"})
	stCOver := parseSyncTestHomeState(t, hC, adminC)
	goodCOver := fmt.Sprint(stCOver["good"])
	if !strings.Contains(goodCOver, "q-v094") || strings.Contains(goodCOver, "q-nasc") {
		t.Fatalf("overwrite restore with legacy $.good failed: good=%v", stCOver["good"])
	}

	// 4. When favorites/__admin__.json IS present (admin unfavorited q-v094), stale $.good must NOT be resurrected.
	syncTestMustPost(t, hA, "/biology/library/favorite", adminA, map[string]any{"question": "q-v094", "selected": false})
	syncTestMustPost(t, hA, "/biology/library/cloud/sync/push", adminA, nil)

	lD, hD, _ := newTestCloudLibrary(t, true)
	lD.db.SetMaxOpenConns(1)
	lD.syncDebounce = 10 * time.Second
	adminD := loginTestUser(t, hD, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lD, "Biofolio")
	syncTestMustPost(t, hD, "/biology/library/record", adminD, syncTestQuestion("q-nasd", "NAS-D收藏题"))
	syncTestMustPost(t, hD, "/biology/library/favorite", adminD, map[string]any{"question": "q-nasd", "selected": true})
	syncTestMustPost(t, hD, "/biology/library/cloud/sync/restore", adminD, map[string]any{"mode": "merge"})
	stD := parseSyncTestHomeState(t, hD, adminD)
	goodD := fmt.Sprint(stD["good"])
	if !strings.Contains(goodD, "q-nasd") || strings.Contains(goodD, "q-v094") {
		t.Fatalf("stale $.good resurrected despite authoritative favorites/__admin__.json: good=%v", stD["good"])
	}
}

func TestSyncConcurrencyAndCloseRaceSafety(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	l, h, _ := newTestCloudLibrary(t, true)
	l.db.SetMaxOpenConns(1)
	l.syncDebounce = 40 * time.Millisecond
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, l, "Biofolio")
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-conc-0", "并发基准"))

	gd.mu.Lock()
	gd.uploadDelay = 150 * time.Millisecond
	gd.uploadStatus = http.StatusServiceUnavailable
	gd.mu.Unlock()

	l.StartCloudSync()

	var wg sync.WaitGroup
	errs := make(chan string, 64)

	// Concurrent local reads must NEVER block on slow/failing Drive (<300ms).
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p := []string{"/biology/", "/biology/library/records?brief=1&compact=1", "/biology/library/cloud/sync/status"}[idx%3]
			start := time.Now()
			r := callLibraryAPI(h, http.MethodGet, p, adminCookie, nil, nil)
			dur := time.Since(start)
			if r.Code != http.StatusOK || dur > 300*time.Millisecond {
				errs <- fmt.Sprintf("read %s slow/failed: code=%d dur=%v", p, r.Code, dur)
			}
		}(i)
	}

	// Concurrent mutations + push + check.
	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("q-conc-%d", idx)
			r := syncTestPostJSON(t, h, "/biology/library/record", adminCookie, syncTestQuestion(id, "并发题"))
			if r.Code != http.StatusOK {
				errs <- fmt.Sprintf("record %s failed: %d %s", id, r.Code, r.Body.String())
			}
			_ = syncTestPostJSON(t, h, "/biology/library/favorite", adminCookie, map[string]any{"question": id, "selected": true})
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = syncTestPostJSON(t, h, "/biology/library/cloud/sync/push", adminCookie, nil)
		_ = syncTestPostJSON(t, h, "/biology/library/cloud/sync/check", adminCookie, nil)
	}()

	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}

	// Recover Drive and verify clean Close().
	gd.mu.Lock()
	gd.uploadDelay = 0
	gd.uploadStatus = 0
	gd.mu.Unlock()
	_ = hex.EncodeToString(sha256.New().Sum(nil))
	l.Close()
}

func readSyncStatusForTest(t *testing.T, h http.Handler, cookie *http.Cookie) cloudSyncStatusResponse {
	t.Helper()
	r := callLibraryAPI(h, http.MethodGet, "/biology/library/cloud/sync/status", cookie, nil, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("GET sync/status failed: %d %s", r.Code, r.Body.String())
	}
	var st cloudSyncStatusResponse
	if err := json.Unmarshal(r.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal sync/status: %v", err)
	}
	return st
}

func readRemoteManifestForTest(t *testing.T, gd *mockSyncGDriveServer, rootDir string) cloudSyncManifest {
	t.Helper()
	b, ok := gd.readFile(rootDir + "/同步库/manifest.json")
	if !ok {
		t.Fatalf("missing remote %s/同步库/manifest.json", rootDir)
	}
	var m cloudSyncManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid remote manifest.json: %v", err)
	}
	return m
}

func TestSyncStartCloudSyncProtectsRemoteAheadAndEmptyRemote(t *testing.T) {
	t.Run("CaseB_PopulatedSecondNASDoesNotClobberRemote", func(t *testing.T) {
		gd := newMockSyncGDriveServer(t)
		gd.bindForTest(t)

		lA, hA, _ := newTestCloudLibrary(t, true)
		lA.db.SetMaxOpenConns(1)
		lA.syncDebounce = 10 * time.Second
		adminA := loginTestUser(t, hA, "teacher", "admin-pass-123")
		bindAdminGDriveForSyncTest(t, lA, "Biofolio")
		for i := 1; i <= 5; i++ {
			id := fmt.Sprintf("q-a-%d", i)
			syncTestMustPost(t, hA, "/biology/library/record", adminA, syncTestQuestion(id, "NAS-A题"+strconv.Itoa(i)))
		}
		syncTestMustPost(t, hA, "/biology/library/cloud/sync/push", adminA, nil)
		mA := readRemoteManifestForTest(t, gd, "Biofolio")
		if mA.SyncVersion != 1 || mA.RecordCount != 5 {
			t.Fatalf("unexpected NAS-A manifest: %+v", mA)
		}

		gd.resetStats()

		// NAS-B boots with preexisting local record (LastSyncedSyncVersion == 0) against syncVersion=1 remote.
		lB, hB, _ := newTestCloudLibrary(t, true)
		lB.db.SetMaxOpenConns(1)
		lB.syncDebounce = 60 * time.Millisecond
		qBBytes, _ := json.Marshal(syncTestQuestion("q-b-local", "NAS-B本地题"))
		if _, err := lB.db.Exec(`INSERT INTO library_records(id, body) VALUES('q-b-local', ?)`, string(qBBytes)); err != nil {
			t.Fatal(err)
		}
		adminB := loginTestUser(t, hB, "teacher", "admin-pass-123")
		bindAdminGDriveForSyncTest(t, lB, "Biofolio")
		lB.StartCloudSync()

		deadline := time.Now().Add(3 * time.Second)
		var st cloudSyncStatusResponse
		for time.Now().Before(deadline) {
			st = readSyncStatusForTest(t, hB, adminB)
			if st.Remote.Checked {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !st.Remote.Checked {
			t.Fatalf("startup check did not finish in time: %+v", st)
		}
		time.Sleep(300 * time.Millisecond)

		if uploads := gd.orderCopy(); len(uploads) != 0 {
			t.Fatalf("expected 0 uploads on diverged StartCloudSync, got %v", uploads)
		}
		mAfter := readRemoteManifestForTest(t, gd, "Biofolio")
		if mAfter.SyncVersion != 1 || mAfter.RecordCount != 5 {
			t.Fatalf("remote manifest was clobbered on startup! %+v", mAfter)
		}
		st = readSyncStatusForTest(t, hB, adminB)
		if st.Status != "diverged" || !st.Dirty || !st.Remote.Exists || st.Remote.SyncVersion != 1 || st.Remote.RecordCount != 5 {
			t.Fatalf("unexpected NAS-B status after startup check: %+v", st)
		}
		var localCnt int
		if err := lB.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-b-local'`).Scan(&localCnt); err != nil || localCnt != 1 {
			t.Fatalf("local q-b-local was altered: cnt=%d err=%v", localCnt, err)
		}
	})

	t.Run("CaseA_FreshlyPopulatedV094UpgradeEmptyRemoteStaysProtected", func(t *testing.T) {
		gd := newMockSyncGDriveServer(t)
		gd.bindForTest(t)

		lC, hC, _ := newTestCloudLibrary(t, true)
		lC.db.SetMaxOpenConns(1)
		lC.syncDebounce = 60 * time.Millisecond
		qCBytes, _ := json.Marshal(syncTestQuestion("q-c-local", "升级本地题"))
		if _, err := lC.db.Exec(`INSERT INTO library_records(id, body) VALUES('q-c-local', ?)`, string(qCBytes)); err != nil {
			t.Fatal(err)
		}
		adminC := loginTestUser(t, hC, "teacher", "admin-pass-123")
		bindAdminGDriveForSyncTest(t, lC, "Biofolio")
		lC.StartCloudSync()

		deadline := time.Now().Add(3 * time.Second)
		var st cloudSyncStatusResponse
		for time.Now().Before(deadline) {
			st = readSyncStatusForTest(t, hC, adminC)
			if st.Remote.Checked {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !st.Remote.Checked {
			t.Fatalf("startup check did not finish in time: %+v", st)
		}
		time.Sleep(300 * time.Millisecond)

		if uploads := gd.orderCopy(); len(uploads) != 0 {
			t.Fatalf("expected 0 boot uploads on empty remote, got %v", uploads)
		}
		st = readSyncStatusForTest(t, hC, adminC)
		if st.Status != "remote_empty_local_populated" || !st.Dirty {
			t.Fatalf("expected remote_empty_local_populated + dirty=true, got %+v", st)
		}
		var localCnt int
		if err := lC.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-c-local'`).Scan(&localCnt); err != nil || localCnt != 1 {
			t.Fatalf("local q-c-local was altered: cnt=%d err=%v", localCnt, err)
		}
	})
}

func TestSyncBackgroundEditAbortsWhenRemoteAheadAndRecovers(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	// NAS-A pushes v1 (1 question).
	lA, hA, _ := newTestCloudLibrary(t, true)
	lA.db.SetMaxOpenConns(1)
	lA.syncDebounce = 10 * time.Second
	adminA := loginTestUser(t, hA, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lA, "Biofolio")
	syncTestMustPost(t, hA, "/biology/library/record", adminA, syncTestQuestion("q-1", "基准题1"))
	syncTestMustPost(t, hA, "/biology/library/cloud/sync/push", adminA, nil)

	// NAS-B restores v1 -> in_sync, then starts background sync worker.
	lB, hB, _ := newTestCloudLibrary(t, true)
	lB.db.SetMaxOpenConns(1)
	lB.syncDebounce = 60 * time.Millisecond
	adminB := loginTestUser(t, hB, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lB, "Biofolio")
	syncTestMustPost(t, hB, "/biology/library/cloud/sync/restore", adminB, map[string]any{"mode": "merge"})
	if st := readSyncStatusForTest(t, hB, adminB); st.Status != "in_sync" || st.Local.SyncVersion != 1 {
		t.Fatalf("expected in_sync v1 on NAS-B, got %+v", st)
	}
	lB.StartCloudSync()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := readSyncStatusForTest(t, hB, adminB); st.Remote.Checked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// NAS-A advances remote to v2 (3 questions).
	syncTestMustPost(t, hA, "/biology/library/record", adminA, syncTestQuestion("q-2", "远端新增2"))
	syncTestMustPost(t, hA, "/biology/library/record", adminA, syncTestQuestion("q-3", "远端新增3"))
	syncTestMustPost(t, hA, "/biology/library/cloud/sync/push", adminA, nil)
	mV2 := readRemoteManifestForTest(t, gd, "Biofolio")
	if mV2.SyncVersion != 2 || mV2.RecordCount != 3 {
		t.Fatalf("expected remote v2 / 3 records, got %+v", mV2)
	}

	gd.resetStats()

	// NAS-B mutates favorite + adds local question -> debounced background push MUST abort without clobbering Drive.
	syncTestMustPost(t, hB, "/biology/library/favorite", adminB, map[string]any{"question": "q-1", "selected": true})
	syncTestMustPost(t, hB, "/biology/library/record", adminB, syncTestQuestion("q-b-edit", "本地并发编辑"))

	deadline = time.Now().Add(3 * time.Second)
	var st cloudSyncStatusResponse
	for time.Now().Before(deadline) {
		st = readSyncStatusForTest(t, hB, adminB)
		if st.LastSyncError != "" && !st.Syncing {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if st.LastSyncError == "" {
		t.Fatalf("expected background push conflict error, got %+v", st)
	}
	if uploads := gd.orderCopy(); len(uploads) != 0 {
		t.Fatalf("expected 0 shard/manifest uploads on aborted background push, got %v", uploads)
	}
	mStillV2 := readRemoteManifestForTest(t, gd, "Biofolio")
	if mStillV2.SyncVersion != 2 || mStillV2.RecordCount != 3 {
		t.Fatalf("remote manifest was overwritten! %+v", mStillV2)
	}
	if st.Status != "diverged" || !st.Dirty || !st.Remote.Checked || !st.Remote.Exists || st.Remote.SyncVersion != 2 || st.Remote.RecordCount != 3 || st.Remote.Error != "" {
		t.Fatalf("unexpected diverged status after aborted background push: %+v", st)
	}

	// Ensure workerLoop does not tight-loop Drive after aborting.
	checkedAt := st.Remote.CheckedAt
	time.Sleep(300 * time.Millisecond)
	st2 := readSyncStatusForTest(t, hB, adminB)
	if st2.Remote.CheckedAt != checkedAt {
		t.Fatalf("workerLoop re-checked/tight-looped Drive unexpectedly: %q -> %q", checkedAt, st2.Remote.CheckedAt)
	}
	if uploads := gd.orderCopy(); len(uploads) != 0 {
		t.Fatalf("expected 0 uploads during cooldown, got %v", uploads)
	}

	// Recovery via merge restore + push -> v3 with all 4 questions preserved.
	syncTestMustPost(t, hB, "/biology/library/cloud/sync/restore", adminB, map[string]any{"mode": "merge"})
	var totalCnt int
	if err := lB.db.QueryRow(`SELECT count(*) FROM library_records`).Scan(&totalCnt); err != nil || totalCnt != 4 {
		t.Fatalf("expected 4 records after merge restore, got %d (err=%v)", totalCnt, err)
	}
	stMerged := readSyncStatusForTest(t, hB, adminB)
	if stMerged.LastSyncError != "" || stMerged.Status != "local_ahead" {
		t.Fatalf("expected local_ahead with cleared LastSyncError after merge, got %+v", stMerged)
	}
	syncTestMustPost(t, hB, "/biology/library/cloud/sync/push", adminB, nil)
	stFinal := readSyncStatusForTest(t, hB, adminB)
	if stFinal.Status != "in_sync" || stFinal.Dirty || stFinal.LastSyncError != "" {
		t.Fatalf("expected clean in_sync after manual push, got %+v", stFinal)
	}
	mV3 := readRemoteManifestForTest(t, gd, "Biofolio")
	if mV3.SyncVersion != 3 || mV3.RecordCount != 4 {
		t.Fatalf("expected remote v3 / 4 records, got %+v", mV3)
	}

	// Case D: explicit manual POST /sync/push resolves diverged status authoritatively.
	mV3.SyncVersion = 8
	bV8, _ := json.Marshal(mV3)
	gd.putFile("Biofolio/同步库/manifest.json", "application/json", bV8)
	syncTestMustPost(t, hB, "/biology/library/record", adminB, syncTestQuestion("q-b-force", "手动覆盖提交"))
	syncTestMustPost(t, hB, "/biology/library/cloud/sync/check", adminB, nil)
	if stDiv := readSyncStatusForTest(t, hB, adminB); stDiv.Status != "diverged" {
		t.Fatalf("expected diverged before manual push, got %+v", stDiv)
	}
	syncTestMustPost(t, hB, "/biology/library/cloud/sync/push", adminB, nil)
	stCaseD := readSyncStatusForTest(t, hB, adminB)
	if stCaseD.Status != "in_sync" || stCaseD.Dirty || stCaseD.LastSyncError != "" || stCaseD.Remote.SyncVersion != 9 {
		t.Fatalf("expected Case D manual push to resolve divergence to v9 in_sync, got %+v", stCaseD)
	}
}

func TestSyncConcurrentMemberMutationsAndStatusPushRace(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	l, h, acc := newTestCloudLibrary(t, true)
	l.db.SetMaxOpenConns(1)
	l.syncDebounce = 40 * time.Millisecond
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, l, "Biofolio")
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-member-race-0", "成员并发基准题"))
	l.StartCloudSync()

	var wg sync.WaitGroup
	errs := make(chan string, 128)
	stop := make(chan struct{})

	// 1. Member lifecycle workers (create -> reset -> GET account -> delete).
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for cycle := 0; cycle < 3; cycle++ {
				u := fmt.Sprintf("race-u-%d-%d", workerID, cycle)
				if r := syncTestPostJSON(t, h, "/biology/members", adminCookie, map[string]any{
					"username": u, "password": "pass-word-1", "confirm": "pass-word-1",
				}); r.Code != http.StatusNoContent {
					errs <- fmt.Sprintf("create %s failed: %d %s", u, r.Code, r.Body.String())
					return
				}
				if r := syncTestPostJSON(t, h, "/biology/member", adminCookie, map[string]any{
					"action": "reset", "username": u, "password": "pass-word-2", "confirm": "pass-word-2",
				}); r.Code != http.StatusNoContent {
					errs <- fmt.Sprintf("reset %s failed: %d %s", u, r.Code, r.Body.String())
					return
				}
				if r := callLibraryAPI(h, http.MethodGet, "/biology/account", adminCookie, nil, nil); r.Code != http.StatusOK {
					errs <- fmt.Sprintf("GET account failed: %d %s", r.Code, r.Body.String())
					return
				}
				if r := syncTestPostJSON(t, h, "/biology/member", adminCookie, map[string]any{
					"action": "delete", "username": u,
				}); r.Code != http.StatusNoContent {
					errs <- fmt.Sprintf("delete %s failed: %d %s", u, r.Code, r.Body.String())
					return
				}
			}
		}(w)
	}

	// 2. Concurrent status readers.
	var statusWg sync.WaitGroup
	for s := 0; s < 2; s++ {
		statusWg.Add(1)
		go func() {
			defer statusWg.Done()
			for i := 0; i < 40; i++ {
				select {
				case <-stop:
					return
				default:
				}
				r := callLibraryAPI(h, http.MethodGet, "/biology/library/cloud/sync/status", adminCookie, nil, nil)
				if r.Code != http.StatusOK {
					errs <- fmt.Sprintf("GET sync/status failed: %d %s", r.Code, r.Body.String())
					return
				}
				var st cloudSyncStatusResponse
				if err := json.Unmarshal(r.Body.Bytes(), &st); err != nil {
					errs <- fmt.Sprintf("invalid sync/status JSON: %v", err)
					return
				}
			}
		}()
	}

	// 3. Concurrent sync/push & library/backup (exercises buildSyncSnapshot + exportMemberIDs).
	var pushWg sync.WaitGroup
	pushWg.Add(1)
	go func() {
		defer pushWg.Done()
		for i := 0; i < 4; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if r := syncTestPostJSON(t, h, "/biology/library/cloud/sync/push", adminCookie, nil); r.Code != http.StatusOK {
				errs <- fmt.Sprintf("POST sync/push failed: %d %s", r.Code, r.Body.String())
				return
			}
			if r := callLibraryAPI(h, http.MethodGet, "/biology/library/backup", adminCookie, nil, nil); r.Code != http.StatusOK {
				errs <- fmt.Sprintf("GET backup failed: %d %s", r.Code, r.Body.String())
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	statusWg.Wait()
	pushWg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}

	// Final push ensures member_ids.json on Drive is valid JSON and original members li/wang remain intact.
	syncTestMustPost(t, h, "/biology/library/cloud/sync/push", adminCookie, nil)
	l.Close()
	memRaw, ok := gd.readFile("Biofolio/同步库/member_ids.json")
	if !ok {
		t.Fatal("missing Biofolio/同步库/member_ids.json")
	}
	var memMap map[string]string
	if err := json.Unmarshal(memRaw, &memMap); err != nil {
		t.Fatalf("invalid member_ids.json: %v", err)
	}
	idMap := acc.memberIDMap()
	if idMap["li"] == "" || idMap["wang"] == "" || memMap["li"] == "" || memMap["wang"] == "" {
		t.Fatalf("expected li and wang intact in memberIDMap=%v memMap=%v", idMap, memMap)
	}
}

// Regression test for GAP-1: No infinite workerLoop spin & no unsolicited push when GDrive is unconfigured.
func TestSyncNoSpinLoopWhenGDriveUnconfigured(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	l, h, _ := newTestCloudLibrary(t, true)
	l.db.SetMaxOpenConns(1)
	l.syncDebounce = 25 * time.Millisecond
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")

	l.StartCloudSync()
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-nospin", "无网盘编辑"))

	e := l.sync()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		inMemDirty := e.dirty
		pendingAt := e.pendingAt
		e.mu.Unlock()
		if !inMemDirty && pendingAt == "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	e.mu.Lock()
	inMemDirty := e.dirty
	pendingAt := e.pendingAt
	e.mu.Unlock()
	if inMemDirty || pendingAt != "" {
		t.Fatalf("expected background worker to clear in-memory dirty/pendingAt when unconfigured, got dirty=%v pendingAt=%q", inMemDirty, pendingAt)
	}

	// Unconfigured status still reports local populated dirty=true, and manual sync/push still returns HTTP 400.
	stUnconf := readSyncStatusForTest(t, h, adminCookie)
	if stUnconf.Configured || stUnconf.Status != "unconfigured" || !stUnconf.Dirty {
		t.Fatalf("unexpected unconfigured sync/status: %+v", stUnconf)
	}
	if r := syncTestPostJSON(t, h, "/biology/library/cloud/sync/push", adminCookie, nil); r.Code != http.StatusBadRequest {
		t.Fatalf("expected manual sync/push 400 when unconfigured, got %d %s", r.Code, r.Body.String())
	}

	// Binding GDrive afterward must NOT trigger an unsolicited auto-push without a new mutation/manual push.
	bindAdminGDriveForSyncTest(t, l, "Biofolio")
	time.Sleep(150 * time.Millisecond)
	if uploads := gd.orderCopy(); len(uploads) != 0 {
		t.Fatalf("expected 0 unsolicited uploads after binding GDrive, got %v", uploads)
	}

	// A genuine new mutation after binding DOES trigger background sync.
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-after-bind", "绑定后编辑"))
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if gd.countFor("Biofolio/同步库/manifest.json") > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gd.countFor("Biofolio/同步库/manifest.json") == 0 {
		t.Fatalf("expected background push after new mutation once GDrive bound")
	}
}

// Regression test for GAP-2: Reject null/non-array marks/bag/editions shards before touching SQLite.
func TestSyncRestoreRejectsNullFavoritesAndPersonalShards(t *testing.T) {
	cases := []struct {
		name     string
		favJSON  string
		perJSON  string
		wantCode int
	}{
		{
			name:     "null_marks_and_null_bag_editions",
			favJSON:  `{"account":"","marks":null}`,
			perJSON:  `{"account":"","body":{"bag":null,"editions":null}}`,
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name:     "null_bag_only",
			favJSON:  `{"account":"","marks":[]}`,
			perJSON:  `{"account":"","body":{"bag":null,"editions":[]}}`,
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name:     "non_array_editions",
			favJSON:  `{"account":"","marks":[]}`,
			perJSON:  `{"account":"","body":{"bag":[],"editions":{}}}`,
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name:     "non_array_marks",
			favJSON:  `{"account":"","marks":"bad"}`,
			perJSON:  `{"account":"","body":{"bag":[],"editions":[]}}`,
			wantCode: http.StatusUnprocessableEntity,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gd := newMockSyncGDriveServer(t)
			gd.bindForTest(t)

			stateBytes := []byte(`{"version":1,"body":{"aliases":{}}}`)
			favBytes := []byte(tc.favJSON)
			perBytes := []byte(tc.perJSON)
			gd.putFile("Biofolio/同步库/state.json", "application/json", stateBytes)
			gd.putFile("Biofolio/同步库/favorites/__admin__.json", "application/json", favBytes)
			gd.putFile("Biofolio/同步库/personal/__admin__.json", "application/json", perBytes)
			mBytes, _ := json.Marshal(cloudSyncManifest{
				SchemaVersion: 1, SyncVersion: 1, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
				StateVersion: 1, ShardCount: 3,
				Shards: map[string]syncShardMeta{
					"state.json":               {Hash: syncSHA256Hex(stateBytes), Size: int64(len(stateBytes)), Kind: "state"},
					"favorites/__admin__.json": {Hash: syncSHA256Hex(favBytes), Size: int64(len(favBytes)), Kind: "favorites"},
					"personal/__admin__.json":  {Hash: syncSHA256Hex(perBytes), Size: int64(len(perBytes)), Kind: "personal"},
				},
			})
			gd.putFile("Biofolio/同步库/manifest.json", "application/json", mBytes)

			l, h, _ := newTestCloudLibrary(t, true)
			l.db.SetMaxOpenConns(1)
			adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
			bindAdminGDriveForSyncTest(t, l, "Biofolio")
			syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-keep-null", "保留题"))

			r := syncTestPostJSON(t, h, "/biology/library/cloud/sync/restore", adminCookie, map[string]any{"mode": "overwrite"})
			if r.Code != tc.wantCode {
				t.Fatalf("expected %d rejecting invalid shard, got %d %s", tc.wantCode, r.Code, r.Body.String())
			}
			var cnt int
			_ = l.db.QueryRow(`SELECT count(*) FROM library_records WHERE id='q-keep-null'`).Scan(&cnt)
			if cnt != 1 {
				t.Fatalf("local SQLite mutated despite invalid null shard")
			}
		})
	}
}

// Regression test for GAP-4: Post-merge push must NOT re-upload unchanged heavy paper shards,
// while genuine local edits to another paper/question ARE still uploaded.
func TestSyncPostMergeSkipsUnchangedHeavyPaperShard(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	// NAS-A pushes heavy unchanged paper + shared paper + q-a.
	lA, hA, _ := newTestCloudLibrary(t, true)
	lA.db.SetMaxOpenConns(1)
	lA.syncDebounce = 10 * time.Second
	adminA := loginTestUser(t, hA, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lA, "Biofolio")

	heavyPayload := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("P"), 80*1024))
	syncTestMustPost(t, hA, "/biology/library/record", adminA, map[string]any{
		"id": "paper-heavy-cm", "paper": true, "sourceKind": "docx", "name": "重型原卷CM",
		"questions": []any{}, "links": []any{},
		"original": map[string]any{"name": "heavy.docx", "type": docxMimeType, "data": "data:" + docxMimeType + ";base64," + heavyPayload},
	})
	syncTestMustPost(t, hA, "/biology/library/record", adminA, map[string]any{
		"id": "paper-shared-edit", "paper": true, "sourceKind": "docx", "name": "远端版试卷",
		"questions": []any{}, "links": []any{},
	})
	syncTestMustPost(t, hA, "/biology/library/record", adminA, syncTestQuestion("q-nas-a", "NAS-A题目"))
	syncTestMustPost(t, hA, "/biology/library/cloud/sync/push", adminA, nil)

	// NAS-B already has local-only q-nas-b AND its own local edit of paper-shared-edit.
	lB, hB, _ := newTestCloudLibrary(t, true)
	lB.db.SetMaxOpenConns(1)
	lB.syncDebounce = 10 * time.Second
	adminB := loginTestUser(t, hB, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, lB, "Biofolio")
	syncTestMustPost(t, hB, "/biology/library/record", adminB, map[string]any{
		"id": "paper-shared-edit", "paper": true, "sourceKind": "docx", "name": "NAS-B本地修改版",
		"questions": []any{}, "links": []any{},
	})
	syncTestMustPost(t, hB, "/biology/library/record", adminB, syncTestQuestion("q-nas-b", "NAS-B本地题"))

	syncTestMustPost(t, hB, "/biology/library/cloud/sync/restore", adminB, map[string]any{"mode": "merge"})
	stMerged := readSyncStatusForTest(t, hB, adminB)
	if stMerged.Status != "local_ahead" || !stMerged.Dirty {
		t.Fatalf("expected local_ahead & dirty after merge with local data, got %+v", stMerged)
	}

	gd.resetStats()
	syncTestMustPost(t, hB, "/biology/library/cloud/sync/push", adminB, nil)

	if got := gd.countFor("Biofolio/同步库/papers/paper-heavy-cm.json"); got != 0 {
		t.Fatalf("expected 0 re-uploads for unchanged paper-heavy-cm.json after merge, got %d (orders=%v)", got, gd.orderCopy())
	}
	if got := gd.countFor("Biofolio/同步库/papers/paper-shared-edit.json"); got != 1 {
		t.Fatalf("expected genuine local edit paper-shared-edit.json to upload once, got %d (orders=%v)", got, gd.orderCopy())
	}
	if got := gd.countFor("Biofolio/同步库/manifest.json"); got != 1 {
		t.Fatalf("expected manifest.json upload once, got %d", got)
	}
	stAfter := readSyncStatusForTest(t, hB, adminB)
	if stAfter.Status != "in_sync" || stAfter.Dirty || stAfter.Remote.RecordCount != 4 {
		t.Fatalf("expected in_sync with 4 records after post-merge push, got %+v", stAfter)
	}
}

// Regression test for GAP-5: Creating/deleting teacher members triggers background sync of member_ids.json.
func TestSyncMemberCreateAndDeleteTriggersBackgroundSync(t *testing.T) {
	gd := newMockSyncGDriveServer(t)
	gd.bindForTest(t)

	l, h, _ := newTestCloudLibrary(t, true)
	l.db.SetMaxOpenConns(1)
	l.syncDebounce = 30 * time.Millisecond
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	bindAdminGDriveForSyncTest(t, l, "Biofolio")
	syncTestMustPost(t, h, "/biology/library/record", adminCookie, syncTestQuestion("q-mem-base", "成员同步基准题"))
	syncTestMustPost(t, h, "/biology/library/cloud/sync/push", adminCookie, nil)
	l.StartCloudSync()
	gd.resetStats()

	// 1. Create teacher member -> member_ids.json auto-uploads within debounce window.
	rCreate := syncTestPostJSON(t, h, "/biology/members", adminCookie, map[string]any{
		"username": "zhaoliu", "password": "pass-word-1", "confirm": "pass-word-1",
	})
	if rCreate.Code != http.StatusNoContent {
		t.Fatalf("create member failed: %d %s", rCreate.Code, rCreate.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if gd.countFor("Biofolio/同步库/member_ids.json") > 0 && gd.countFor("Biofolio/同步库/manifest.json") > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gd.countFor("Biofolio/同步库/member_ids.json") == 0 {
		t.Fatalf("expected member_ids.json auto-upload after member creation, orders=%v", gd.orderCopy())
	}
	rawMem, ok := gd.readFile("Biofolio/同步库/member_ids.json")
	if !ok {
		t.Fatal("missing Biofolio/同步库/member_ids.json after create")
	}
	var memMap map[string]string
	if err := json.Unmarshal(rawMem, &memMap); err != nil || memMap["zhaoliu"] == "" {
		t.Fatalf("expected zhaoliu in synced member_ids.json, got %s (err=%v)", string(rawMem), err)
	}

	// 2. Delete teacher member -> member_ids.json auto-uploads again without zhaoliu.
	gd.resetStats()
	rDel := syncTestPostJSON(t, h, "/biology/member", adminCookie, map[string]any{
		"action": "delete", "username": "zhaoliu",
	})
	if rDel.Code != http.StatusNoContent {
		t.Fatalf("delete member failed: %d %s", rDel.Code, rDel.Body.String())
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if gd.countFor("Biofolio/同步库/member_ids.json") > 0 && gd.countFor("Biofolio/同步库/manifest.json") > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gd.countFor("Biofolio/同步库/member_ids.json") == 0 {
		t.Fatalf("expected member_ids.json auto-upload after member deletion, orders=%v", gd.orderCopy())
	}
	rawMem2, ok := gd.readFile("Biofolio/同步库/member_ids.json")
	if !ok {
		t.Fatal("missing Biofolio/同步库/member_ids.json after delete")
	}
	var memMap2 map[string]string
	if err := json.Unmarshal(rawMem2, &memMap2); err != nil {
		t.Fatalf("invalid member_ids.json after delete: %v", err)
	}
	if _, stillThere := memMap2["zhaoliu"]; stillThere {
		t.Fatalf("expected zhaoliu removed from synced member_ids.json, got %s", string(rawMem2))
	}
}
