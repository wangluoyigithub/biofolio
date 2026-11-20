package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockGDriveItem struct {
	id       string
	name     string
	mime     string
	parent   string
	data     []byte
	modified string
	trashed  bool
}

type mockGDriveResumable struct {
	parent      string
	overwriteID string
	name        string
	mime        string
	buf         bytes.Buffer
}

type mockGDriveServer struct {
	mu                     sync.Mutex
	srv                    *httptest.Server
	items                  map[string]*mockGDriveItem
	resumable              map[string]*mockGDriveResumable
	nextID                 int
	folderCreateDelay      time.Duration
	folderCreateCount      int
	duplicateFolders       []string
	tokenRefreshCount      int
	realDriveTrashMode     bool
	getFileMetaCount       int
	trashOnFirstChunk308ID string
	onRequest              func(w http.ResponseWriter, r *http.Request) bool
}

var (
	mockQueryParentRe = regexp.MustCompile(`'([^']*)' in parents`)
	mockQueryNameRe   = regexp.MustCompile(`name='((?:\\.|[^'\\])*)'`)
	mockQueryMimeRe   = regexp.MustCompile(`mimeType='([^']*)'`)
)

func unescapeMockDriveQuery(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func newMockGDriveServer(t *testing.T) *mockGDriveServer {
	t.Helper()
	m := &mockGDriveServer{
		items:     make(map[string]*mockGDriveItem),
		resumable: make(map[string]*mockGDriveResumable),
		nextID:    100,
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockGDriveServer) useForTest(t *testing.T) {
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

func (m *mockGDriveServer) findByPath(segments ...string) (*mockGDriveItem, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := "root"
	var last *mockGDriveItem
	for _, seg := range segments {
		var matched *mockGDriveItem
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
		last = matched
	}
	return last, last != nil
}

func (m *mockGDriveServer) listChildren(parentID string) []*mockGDriveItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*mockGDriveItem
	for _, it := range m.items {
		if !it.trashed && it.parent == parentID {
			cp := *it
			cp.data = append([]byte(nil), it.data...)
			out = append(out, &cp)
		}
	}
	return out
}

func (m *mockGDriveServer) trashByID(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it := m.items[id]; it != nil {
		it.trashed = true
	}
}

func (m *mockGDriveServer) assertNoDuplicateFolders(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.duplicateFolders) > 0 {
		t.Fatalf("expected 0 duplicate folders, got: %v", m.duplicateFolders)
	}
	seen := make(map[string]string)
	for _, it := range m.items {
		if it.trashed || it.mime != googleFolderMime {
			continue
		}
		key := it.parent + "\x00" + it.name
		if prevID, exists := seen[key]; exists {
			t.Fatalf("duplicate folder found for (%s, %s): %s and %s", it.parent, it.name, prevID, it.id)
		}
		seen[key] = it.id
	}
}

func (m *mockGDriveServer) parentMissingLocked(parentID string) bool {
	if parentID == "" || parentID == "root" {
		return false
	}
	it := m.items[parentID]
	return it == nil || it.trashed
}

func (m *mockGDriveServer) isTrashedRecursiveLocked(it *mockGDriveItem) bool {
	for cur := it; cur != nil; {
		if cur.trashed {
			return true
		}
		if cur.parent == "" || cur.parent == "root" {
			return false
		}
		cur = m.items[cur.parent]
	}
	return false
}

func (m *mockGDriveServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	hook := m.onRequest
	m.mu.Unlock()
	if hook != nil && hook(w, r) {
		return
	}
	switch {
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		grant := r.Form.Get("grant_type")
		if grant == "refresh_token" {
			m.mu.Lock()
			m.tokenRefreshCount++
			n := m.tokenRefreshCount
			m.mu.Unlock()
			jsonResponse(w, map[string]any{
				"access_token": fmt.Sprintf("g-refreshed-%d", n),
				"expires_in":   3600,
			})
			return
		}
		jsonResponse(w, map[string]any{
			"access_token":  "g-acc-initial",
			"refresh_token": "g-ref-1",
			"expires_in":    3600,
		})

	case r.URL.Path == "/drive/v3/about" && r.Method == http.MethodGet:
		jsonResponse(w, map[string]any{
			"user": map[string]string{"emailAddress": "teacher@school.edu"},
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
		wantNotTrashed := strings.Contains(q, "trashed=false")

		m.mu.Lock()
		defer m.mu.Unlock()
		if wantParent != "" && !m.realDriveTrashMode && m.parentMissingLocked(wantParent) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"parent not found"}}`))
			return
		}
		var out []map[string]any
		for _, it := range m.items {
			if m.realDriveTrashMode && wantParent != "" && wantParent != "root" {
				if pItem := m.items[wantParent]; pItem == nil || m.isTrashedRecursiveLocked(pItem) {
					continue
				}
			}
			if wantNotTrashed && (it.trashed || (m.realDriveTrashMode && m.isTrashedRecursiveLocked(it))) {
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
		m.mu.Lock()
		delay := m.folderCreateDelay
		m.mu.Unlock()
		if meta.MimeType == googleFolderMime && delay > 0 {
			time.Sleep(delay)
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		parent := "root"
		if len(meta.Parents) > 0 && meta.Parents[0] != "" {
			parent = meta.Parents[0]
		}
		if !m.realDriveTrashMode && m.parentMissingLocked(parent) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"parent not found"}}`))
			return
		}
		if meta.MimeType == googleFolderMime {
			m.folderCreateCount++
			for _, existing := range m.items {
				if !existing.trashed && existing.mime == googleFolderMime && existing.parent == parent && existing.name == meta.Name {
					m.duplicateFolders = append(m.duplicateFolders, parent+"/"+meta.Name)
					break
				}
			}
		}
		m.nextID++
		id := fmt.Sprintf("gf-%d", m.nextID)
		now := time.Now().UTC().Format(time.RFC3339)
		m.items[id] = &mockGDriveItem{
			id:       id,
			name:     meta.Name,
			mime:     meta.MimeType,
			parent:   parent,
			modified: now,
		}
		jsonResponse(w, map[string]any{
			"id":           id,
			"name":         meta.Name,
			"mimeType":     meta.MimeType,
			"modifiedTime": now,
		})

	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && strings.HasSuffix(r.URL.Path, "/export") && r.Method == http.MethodGet:
		fid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/drive/v3/files/"), "/export")
		m.mu.Lock()
		it := m.items[fid]
		if it == nil || it.trashed {
			m.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data := append([]byte(nil), it.data...)
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)

	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && r.Method == http.MethodGet:
		fid := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		m.mu.Lock()
		it := m.items[fid]
		if it == nil || (!m.realDriveTrashMode && it.trashed) {
			m.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		isTrashed := m.isTrashedRecursiveLocked(it)
		if r.URL.Query().Get("alt") != "media" {
			m.getFileMetaCount++
		}
		cp := *it
		cp.data = append([]byte(nil), it.data...)
		m.mu.Unlock()

		if r.URL.Query().Get("alt") == "media" {
			if cp.mime == googleFolderMime {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if cp.mime != "" {
				w.Header().Set("Content-Type", cp.mime)
			}
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
			"trashed":      isTrashed,
		})

	case r.URL.Path == "/upload/drive/v3/files" && r.Method == http.MethodPost:
		upType := r.URL.Query().Get("uploadType")
		if upType == "resumable" {
			var meta struct {
				Name    string   `json:"name"`
				Parents []string `json:"parents"`
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
			if !m.realDriveTrashMode && m.parentMissingLocked(parent) {
				m.mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				return
			}
			m.nextID++
			sessID := fmt.Sprintf("sess-%d", m.nextID)
			m.resumable[sessID] = &mockGDriveResumable{
				parent: parent,
				name:   meta.Name,
				mime:   r.Header.Get("X-Upload-Content-Type"),
			}
			m.mu.Unlock()
			w.Header().Set("Location", m.srv.URL+"/upload/drive/v3/session/"+sessID)
			w.WriteHeader(http.StatusOK)
			return
		}

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		var meta struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		var payload []byte
		partMime := "application/octet-stream"
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
			if strings.Contains(ct, "application/json") && meta.Name == "" && len(meta.Parents) == 0 {
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
		if !m.realDriveTrashMode && m.parentMissingLocked(parent) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.nextID++
		id := fmt.Sprintf("gf-%d", m.nextID)
		now := time.Now().UTC().Format(time.RFC3339)
		m.items[id] = &mockGDriveItem{
			id:       id,
			name:     meta.Name,
			mime:     partMime,
			parent:   parent,
			data:     payload,
			modified: now,
		}
		jsonResponse(w, map[string]any{
			"id":           id,
			"name":         meta.Name,
			"mimeType":     partMime,
			"size":         strconv.Itoa(len(payload)),
			"modifiedTime": now,
		})

	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/files/") && r.Method == http.MethodPatch:
		fid := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files/")
		upType := r.URL.Query().Get("uploadType")
		if upType == "resumable" {
			m.mu.Lock()
			it := m.items[fid]
			if it == nil || it.trashed {
				m.mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				return
			}
			m.nextID++
			sessID := fmt.Sprintf("sess-%d", m.nextID)
			m.resumable[sessID] = &mockGDriveResumable{
				overwriteID: fid,
				name:        it.name,
				mime:        r.Header.Get("X-Upload-Content-Type"),
			}
			m.mu.Unlock()
			w.Header().Set("Location", m.srv.URL+"/upload/drive/v3/session/"+sessID)
			w.WriteHeader(http.StatusOK)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		it := m.items[fid]
		if it == nil || it.trashed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		it.data = payload
		if ct := r.Header.Get("Content-Type"); ct != "" {
			it.mime = ct
		}
		it.modified = time.Now().UTC().Format(time.RFC3339)
		jsonResponse(w, map[string]any{
			"id":           it.id,
			"name":         it.name,
			"mimeType":     it.mime,
			"size":         strconv.Itoa(len(it.data)),
			"modifiedTime": it.modified,
		})

	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/session/") && r.Method == http.MethodPut:
		sessID := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/session/")
		chunk, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		contentRange := r.Header.Get("Content-Range")
		var start, end, total int
		if _, err := fmt.Sscanf(contentRange, "bytes %d-%d/%d", &start, &end, &total); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		sess := m.resumable[sessID]
		if sess == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if sess.buf.Len() != start {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		sess.buf.Write(chunk)
		if end+1 < total {
			if m.trashOnFirstChunk308ID != "" {
				if target := m.items[m.trashOnFirstChunk308ID]; target != nil {
					target.trashed = true
				}
				m.trashOnFirstChunk308ID = ""
			}
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		delete(m.resumable, sessID)
		finalBytes := append([]byte(nil), sess.buf.Bytes()...)
		now := time.Now().UTC().Format(time.RFC3339)
		if sess.overwriteID != "" {
			it := m.items[sess.overwriteID]
			if it == nil || it.trashed {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			it.data = finalBytes
			if sess.mime != "" {
				it.mime = sess.mime
			}
			it.modified = now
			jsonResponse(w, map[string]any{
				"id":           it.id,
				"name":         it.name,
				"mimeType":     it.mime,
				"size":         strconv.Itoa(len(finalBytes)),
				"modifiedTime": now,
			})
			return
		}
		m.nextID++
		id := fmt.Sprintf("gf-%d", m.nextID)
		m.items[id] = &mockGDriveItem{
			id:       id,
			name:     sess.name,
			mime:     sess.mime,
			parent:   sess.parent,
			data:     finalBytes,
			modified: now,
		}
		jsonResponse(w, map[string]any{
			"id":           id,
			"name":         sess.name,
			"mimeType":     sess.mime,
			"size":         strconv.Itoa(len(finalBytes)),
			"modifiedTime": now,
		})

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestCloudRelDirHelpers(t *testing.T) {
	validCases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/", ""},
		{"rel:", ""},
		{"rel:/", ""},
		{"rel:   ", ""},
		{"rel:/已导入试卷/试卷A/", "已导入试卷/试卷A"},
		{"已组试卷", "已组试卷"},
		{"/同步库/papers/", "同步库/papers"},
		{"rel:a//b", "a/b"},
	}
	for _, tc := range validCases {
		got, err := cleanCloudRelDir(tc.in)
		if err != nil {
			t.Fatalf("cleanCloudRelDir(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("cleanCloudRelDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	invalidCases := []string{
		"rel:..",
		"rel:.",
		"rel:../secret",
		"rel:a/../b",
		"rel:a/./b",
		"rel:a\\b",
		"a\\b",
		"rel:a\nb",
		"\trel:a",
		"rel:a\n",
		"rel:a\x00b",
		"\x00",
	}
	for _, raw := range invalidCases {
		if got, err := cleanCloudRelDir(raw); err == nil {
			t.Fatalf("cleanCloudRelDir(%q) expected error, got %q", raw, got)
		}
	}

	if rel, isRel, err := parseCloudRelPath("folder123"); err != nil || isRel || rel != "" {
		t.Fatalf("parseCloudRelPath(folder123) = (%q, %v, %v)", rel, isRel, err)
	}
	if rel, isRel, err := parseCloudRelPath("rel:/同步库/papers/"); err != nil || !isRel || rel != "同步库/papers" {
		t.Fatalf("parseCloudRelPath(rel) = (%q, %v, %v)", rel, isRel, err)
	}
	if dir, name, isRel, err := parseCloudRelFilePath("rel:同步库/papers/p1.json"); err != nil || !isRel || dir != "同步库/papers" || name != "p1.json" {
		t.Fatalf("parseCloudRelFilePath = (%q, %q, %v, %v)", dir, name, isRel, err)
	}
	if dir, name, isRel, err := parseCloudRelFilePath("rel:manifest.json"); err != nil || !isRel || dir != "" || name != "manifest.json" {
		t.Fatalf("parseCloudRelFilePath(root file) = (%q, %q, %v, %v)", dir, name, isRel, err)
	}
	if _, _, _, err := parseCloudRelFilePath("rel:"); err == nil {
		t.Fatalf("expected error for empty rel file path")
	}
	if got := cloudRelJoin("已导入试卷", "试卷A", "试卷A.json"); got != "rel:已导入试卷/试卷A/试卷A.json" {
		t.Fatalf("cloudRelJoin = %q", got)
	}
}

func TestGDriveRelPathAndBackwardCompat(t *testing.T) {
	mock := newMockGDriveServer(t)
	mock.useForTest(t)

	l, _, _ := newTestCloudLibrary(t, true)
	p, err := newGDriveProvider(l, cloudAdminSettings{}, cloudConnectionRow{
		ID:      "conn-rel-1",
		Account: "teacher",
		Kind:    "gdrive",
		RootDir: "",
	}, gdriveSecret{
		AccessToken: "tok-valid",
		ExpiryUnix:  time.Now().Add(time.Hour).Unix(),
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. Read-only List & Download on missing folders create 0 folders.
	lsMissing, err := p.List(ctx, "rel:同步库")
	if err != nil {
		t.Fatalf("List missing rel dir returned error: %v", err)
	}
	if len(lsMissing.Entries) != 0 || lsMissing.Dir != "rel:同步库" || lsMissing.ParentDir != "rel:" {
		t.Fatalf("unexpected missing list result: %+v", lsMissing)
	}
	_, dlErr := p.Download(ctx, "rel:同步库/manifest.json")
	if !errors.Is(dlErr, errCloudNotFound) || !errors.Is(dlErr, os.ErrNotExist) {
		t.Fatalf("expected errCloudNotFound & os.ErrNotExist, got %v", dlErr)
	}
	if mock.folderCreateCount != 0 {
		t.Fatalf("read-only List/Download created %d folders, want 0", mock.folderCreateCount)
	}

	// 2. Upload nested rel paths & verify contents.
	up1, err := p.Upload(ctx, "rel:已导入试卷/试卷A", "试卷A.json", []byte(`{"id":"paper-a"}`), "application/json", true)
	if err != nil {
		t.Fatalf("Upload json failed: %v", err)
	}
	if up1.Path != "rel:已导入试卷/试卷A/试卷A.json" {
		t.Fatalf("unexpected entry path: %q", up1.Path)
	}
	if _, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A", "试卷A.json"); !ok {
		t.Fatalf("file not found under Biofolio/已导入试卷/试卷A/试卷A.json")
	}

	// Overwrite in place preserves ID.
	up2, err := p.Upload(ctx, "rel:已导入试卷/试卷A", "试卷A.json", []byte(`{"id":"paper-a","v":2}`), "application/json", true)
	if err != nil {
		t.Fatalf("Overwrite upload failed: %v", err)
	}
	if up2.ID != up1.ID {
		t.Fatalf("expected in-place overwrite ID %q, got %q", up1.ID, up2.ID)
	}
	dl, err := p.Download(ctx, "rel:已导入试卷/试卷A/试卷A.json")
	if err != nil || string(dl.Data) != `{"id":"paper-a","v":2}` {
		t.Fatalf("Download rel mismatch: data=%q err=%v", string(dl.Data), err)
	}

	// Reject downloading folder.
	if _, err := p.Download(ctx, "rel:已导入试卷/试卷A"); err == nil {
		t.Fatalf("expected error downloading folder")
	}

	// 3. Idempotent Mkdir in rel: mode.
	mk1, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷B")
	if err != nil {
		t.Fatal(err)
	}
	mk2, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷B")
	if err != nil {
		t.Fatal(err)
	}
	if mk1.ID != mk2.ID {
		t.Fatalf("expected idempotent Mkdir ID %q, got %q", mk1.ID, mk2.ID)
	}

	// 4. Resumable upload > 4 MiB via rel:.
	bigPayload := bytes.Repeat([]byte("Z"), (4<<20)+1024)
	upBig, err := p.Upload(ctx, "rel:同步库/papers", "big.bin", bigPayload, "application/octet-stream", true)
	if err != nil {
		t.Fatalf("resumable rel upload failed: %v", err)
	}
	dlBig, err := p.Download(ctx, upBig.Path)
	if err != nil || !bytes.Equal(dlBig.Data, bigPayload) {
		t.Fatalf("resumable rel download mismatch: len=%d err=%v", len(dlBig.Data), err)
	}

	// 5. External deletion: List sees deletion immediately; Upload retries after invalidating cache.
	paperFolder, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A")
	if !ok {
		t.Fatal("missing 试卷A folder")
	}
	mock.trashByID(paperFolder.id)
	lsAfterTrash, err := p.List(ctx, "rel:已导入试卷/试卷A")
	if err != nil || len(lsAfterTrash.Entries) != 0 {
		t.Fatalf("expected empty list after external folder trash, got %+v err=%v", lsAfterTrash, err)
	}
	if _, err := p.Upload(ctx, "rel:已导入试卷/试卷A", "recovered.json", []byte(`{"ok":1}`), "application/json", true); err != nil {
		t.Fatalf("Upload after external folder deletion failed: %v", err)
	}
	if _, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A", "recovered.json"); !ok {
		t.Fatalf("recovered.json missing after cache invalidation retry")
	}

	// 6. Legacy folder-ID backward compatibility.
	legacyDir, err := p.Mkdir(ctx, "", "LegacyRootChild")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mock.findByPath("LegacyRootChild"); !ok {
		t.Fatalf("expected LegacyRootChild directly under root when RootDir==\"\" and parentDir==\"\"")
	}
	legacyFile, err := p.Upload(ctx, legacyDir.ID, "legacy.txt", []byte("legacy-ok"), "text/plain", false)
	if err != nil {
		t.Fatal(err)
	}
	lsLegacy, err := p.List(ctx, legacyDir.ID)
	if err != nil || len(lsLegacy.Entries) != 1 || lsLegacy.Entries[0].ID != legacyFile.ID {
		t.Fatalf("legacy List by ID failed: %+v err=%v", lsLegacy, err)
	}
	dlLegacy, err := p.Download(ctx, legacyFile.ID)
	if err != nil || string(dlLegacy.Data) != "legacy-ok" {
		t.Fatalf("legacy Download by ID failed: %q err=%v", string(dlLegacy.Data), err)
	}
	if _, err := p.Download(ctx, "nonexistent-id-123"); !errors.Is(err, errCloudNotFound) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected errCloudNotFound on missing ID, got %v", err)
	}

	mock.assertNoDuplicateFolders(t)
}

func TestGDriveDefaultRootDir(t *testing.T) {
	mock := newMockGDriveServer(t)
	mock.useForTest(t)

	l, _, _ := newTestCloudLibrary(t, true)
	ctx := context.Background()

	// Configured rootDir is preserved.
	pCustom, err := newGDriveProvider(l, cloudAdminSettings{}, cloudConnectionRow{
		ID:      "conn-custom",
		Account: "teacher",
		Kind:    "gdrive",
		RootDir: "/生物题册/",
	}, gdriveSecret{
		AccessToken: "tok",
		ExpiryUnix:  time.Now().Add(time.Hour).Unix(),
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pCustom.Upload(ctx, "rel:已组试卷", "exam.docx", []byte("DOCX"), docxMimeType, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := mock.findByPath("生物题册", "已组试卷", "exam.docx"); !ok {
		t.Fatalf("expected file under configured root 生物题册/已组试卷/exam.docx")
	}
	if _, ok := mock.findByPath("Biofolio"); ok {
		t.Fatalf("did not expect Biofolio folder when rootDir is configured")
	}

	// Updating root_dir invalidates folder cache cleanly.
	secBytes, _ := json.Marshal(gdriveSecret{AccessToken: "tok", ExpiryUnix: time.Now().Add(time.Hour).Unix()})
	if err := l.saveCloudConnection(cloudConnectionRow{
		ID:      "conn-meta-cache",
		Account: "teacher",
		Kind:    "gdrive",
		Name:    "Drive",
		RootDir: "RootA",
	}, secBytes); err != nil {
		t.Fatal(err)
	}
	_, provA, err := l.openCloudProvider("teacher", "conn-meta-cache", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provA.Upload(ctx, "rel:已组试卷", "a.json", []byte("{}"), "application/json", true); err != nil {
		t.Fatal(err)
	}
	if err := l.updateCloudConnectionMeta("teacher", "conn-meta-cache", "Drive", "RootB", true, true); err != nil {
		t.Fatal(err)
	}
	_, provB, err := l.openCloudProvider("teacher", "conn-meta-cache", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provB.Upload(ctx, "rel:已组试卷", "b.json", []byte("{}"), "application/json", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := mock.findByPath("RootB", "已组试卷", "b.json"); !ok {
		t.Fatalf("expected file under updated root RootB/已组试卷/b.json")
	}
}

func TestGDriveConcurrentRelPathRace(t *testing.T) {
	mock := newMockGDriveServer(t)
	mock.useForTest(t)
	mock.folderCreateDelay = 8 * time.Millisecond

	l, _, _ := newTestCloudLibrary(t, true)
	secBytes, _ := json.Marshal(gdriveSecret{
		RefreshToken:       "ref-race-1",
		AccessToken:        "expired",
		ExpiryUnix:         1, // expired to exercise folderLock -> tokenRefreshLock -> secMu
		CustomClientID:     "cid",
		CustomClientSecret: "csec",
	})
	row := cloudConnectionRow{
		ID:      "conn-race-1",
		Account: "teacher",
		Kind:    "gdrive",
		Name:    "RaceDrive",
		RootDir: "",
	}
	if err := l.saveCloudConnection(row, secBytes); err != nil {
		t.Fatal(err)
	}
	var sec gdriveSecret
	_ = json.Unmarshal(secBytes, &sec)
	p, err := newGDriveProvider(l, cloudAdminSettings{}, row, sec, true)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 16
	start := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			ctx := context.Background()
			if idx%4 == 0 {
				if _, err := p.Mkdir(ctx, "rel:已导入试卷", "并发试卷"); err != nil {
					errCh <- err
					return
				}
			}
			_, err := p.Upload(ctx, "rel:已导入试卷/并发试卷", fmt.Sprintf("item-%02d.json", idx), []byte(fmt.Sprintf(`{"i":%d}`, idx)), "application/json", true)
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent rel upload/mkdir error: %v", err)
	}

	mock.assertNoDuplicateFolders(t)
	folder, ok := mock.findByPath("Biofolio", "已导入试卷", "并发试卷")
	if !ok {
		t.Fatalf("expected Biofolio/已导入试卷/并发试卷 folder")
	}
	if children := mock.listChildren(folder.id); len(children) != workers {
		t.Fatalf("expected %d uploaded files, got %d", workers, len(children))
	}
	if mock.tokenRefreshCount != 1 {
		t.Fatalf("expected single coalesced token refresh, got %d", mock.tokenRefreshCount)
	}
}

type zeroLimitReader struct {
	remaining int64
}

func (z *zeroLimitReader) Read(p []byte) (int, error) {
	if z.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > z.remaining {
		n = int(z.remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	z.remaining -= int64(n)
	return n, nil
}

func encodeMetaHeader(v any) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}

func TestCloudAutoUploadEndpoint(t *testing.T) {
	mock := newMockGDriveServer(t)
	mock.useForTest(t)

	l, h, _ := newTestCloudLibrary(t, true)
	adminCookie := loginTestUser(t, h, "teacher", "admin-pass-123")
	liCookie := loginTestUser(t, h, "li", "teacher-pass-1")
	wangCookie := loginTestUser(t, h, "wang", "teacher-pass-2")

	// 1. Auth & CSRF checks.
	unauth := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", nil, []byte("{}"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{"relDir": "已组试卷", "filename": "a.docx"}),
	})
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthenticated, got %d", unauth.Code)
	}

	reqNoCSRF := httptest.NewRequest(http.MethodPost, "/biology/library/cloud/auto-upload", bytes.NewReader([]byte("x")))
	reqNoCSRF.AddCookie(liCookie)
	reqNoCSRF.Header.Set("Content-Type", "application/octet-stream")
	reqNoCSRF.Header.Set("X-Cloud-Meta", encodeMetaHeader(map[string]any{"relDir": "已组试卷", "filename": "a.docx"}))
	wNoCSRF := httptest.NewRecorder()
	h.ServeHTTP(wNoCSRF, reqNoCSRF)
	if wNoCSRF.Code != http.StatusForbidden {
		t.Fatalf("expected 403 missing CSRF header, got %d", wNoCSRF.Code)
	}

	// 2. Invalid metadata validation fails fast with 400 even before connection lookup.
	badMetas := []string{
		"",
		strings.Repeat("A", maxCloudMetaHeaderBytes+4),
		"not-base64!!!",
		encodeMetaHeader("not-object"),
		encodeMetaHeader(map[string]any{"relDir": "../secret", "filename": "a.json"}),
		encodeMetaHeader(map[string]any{"relDir": "a\\b", "filename": "a.json"}),
		encodeMetaHeader(map[string]any{"relDir": "a\x00b", "filename": "a.json"}),
		encodeMetaHeader(map[string]any{"relDir": "a\nb", "filename": "a.json"}),
		encodeMetaHeader(map[string]any{"relDir": "已组试卷", "filename": "bad/name.json"}),
		encodeMetaHeader(map[string]any{"relDir": "已组试卷", "filename": "ok.json", "providerKind": "dropbox"}),
	}
	for i, mh := range badMetas {
		headers := map[string]string{"Content-Type": "application/octet-stream"}
		if mh != "" {
			headers["X-Cloud-Meta"] = mh
		}
		res := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte("data"), headers)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("case %d: expected 400, got %d (%s)", i, res.Code, res.Body.String())
		}
	}

	// 3. Admin has a gdrive connection, li has only a default WebDAV connection -> li gets 200 skipped:true.
	adminGDriveSec, _ := json.Marshal(gdriveSecret{AccessToken: "tok-admin", ExpiryUnix: time.Now().Add(time.Hour).Unix()})
	if err := l.saveCloudConnection(cloudConnectionRow{
		ID:        "admin-gdrive-1",
		Account:   adminOwner,
		Kind:      "gdrive",
		Name:      "AdminDrive",
		RootDir:   "",
		IsDefault: true,
	}, adminGDriveSec); err != nil {
		t.Fatal(err)
	}
	liWebDAVSec, _ := json.Marshal(webdavSecret{BaseURL: "https://dav.example.com", Username: "u", Password: "p"})
	if err := l.saveCloudConnection(cloudConnectionRow{
		ID:        "m1-webdav-1",
		Account:   "m1",
		Kind:      "webdav",
		Name:      "LiWebDAV",
		IsDefault: true,
	}, liWebDAVSec); err != nil {
		t.Fatal(err)
	}

	skipRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte("data"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{"relDir": "已组试卷", "filename": "exam.docx"}),
	})
	if skipRes.Code != http.StatusOK {
		t.Fatalf("expected 200 skipped, got %d %s", skipRes.Code, skipRes.Body.String())
	}
	var skipBody struct {
		OK      bool   `json:"ok"`
		Skipped bool   `json:"skipped"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(skipRes.Body.Bytes(), &skipBody)
	if !skipBody.OK || !skipBody.Skipped || skipBody.Reason != "no_gdrive_connection" {
		t.Fatalf("unexpected skipped body: %+v", skipBody)
	}

	// 4. Foreign / nonexistent explicit connectionId -> 404; mismatched providerKind -> 400.
	foreignRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte("data"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{"connectionId": "admin-gdrive-1", "relDir": "已组试卷", "filename": "exam.docx"}),
	})
	if foreignRes.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for foreign connectionId, got %d", foreignRes.Code)
	}
	mismatchRes := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte("data"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{"connectionId": "m1-webdav-1", "providerKind": "gdrive", "relDir": "已组试卷", "filename": "exam.docx"}),
	})
	if mismatchRes.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for kind mismatch, got %d", mismatchRes.Code)
	}

	// 5. Add non-default gdrive connection for li (while webdav remains is_default=1); auto-upload selects gdrive!
	liGDriveSec, _ := json.Marshal(gdriveSecret{AccessToken: "tok-li", ExpiryUnix: time.Now().Add(time.Hour).Unix()})
	if err := l.saveCloudConnection(cloudConnectionRow{
		ID:        "m1-gdrive-1",
		Account:   "m1",
		Kind:      "gdrive",
		Name:      "LiDrive",
		RootDir:   "",
		IsDefault: false,
	}, liGDriveSec); err != nil {
		t.Fatal(err)
	}

	// Paper import auto-upload (.json + .docx + .pdf) -> Biofolio/已导入试卷/高三生物一模/
	upImportJSON := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte(`{"paper":"高三生物一模"}`), map[string]string{
		"Content-Type": "application/json",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{
			"relDir":   "已导入试卷/高三生物一模",
			"filename": "高三生物一模.json",
			"mimeType": "application/json",
		}),
	})
	if upImportJSON.Code != http.StatusOK {
		t.Fatalf("import json auto-upload failed: %d %s", upImportJSON.Code, upImportJSON.Body.String())
	}
	upImportDocx := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte("DOCX_IMPORT_BYTES"), map[string]string{
		"Content-Type": docxMimeType,
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{
			"relDir":   "已导入试卷/高三生物一模",
			"filename": "高三生物一模.docx",
			"mimeType": docxMimeType,
		}),
	})
	if upImportDocx.Code != http.StatusOK {
		t.Fatalf("import docx auto-upload failed: %d %s", upImportDocx.Code, upImportDocx.Body.String())
	}
	upImportPDF := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte("PDF_IMPORT_BYTES"), map[string]string{
		"Content-Type": pdfMimeType,
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{
			"relDir":   "已导入试卷/高三生物一模",
			"filename": "高三生物一模.pdf",
			"mimeType": pdfMimeType,
		}),
	})
	if upImportPDF.Code != http.StatusOK {
		t.Fatalf("import pdf auto-upload failed: %d %s", upImportPDF.Code, upImportPDF.Body.String())
	}

	// Exam assembly auto-upload (.docx + .json) -> Biofolio/已组试卷/
	upExamDocx := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte("EXAM_DOCX_V1"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{
			"relDir":   "已组试卷",
			"filename": "期中组卷.docx",
			"mimeType": docxMimeType,
		}),
	})
	if upExamDocx.Code != http.StatusOK {
		t.Fatalf("exam docx auto-upload failed: %d %s", upExamDocx.Code, upExamDocx.Body.String())
	}
	// Default overwrite=true replaces in-place when overwrite is omitted.
	upExamDocxV2 := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte("EXAM_DOCX_V2"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{
			"relDir":   "已组试卷",
			"filename": "期中组卷.docx",
			"mimeType": docxMimeType,
		}),
	})
	if upExamDocxV2.Code != http.StatusOK {
		t.Fatalf("exam docx overwrite failed: %d %s", upExamDocxV2.Code, upExamDocxV2.Body.String())
	}
	upExamJSON := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", liCookie, []byte(`{"edition":"期中组卷"}`), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{
			"relDir":   "已组试卷",
			"filename": "期中组卷.json",
			"mimeType": "application/json",
		}),
	})
	if upExamJSON.Code != http.StatusOK {
		t.Fatalf("exam json auto-upload failed: %d %s", upExamJSON.Code, upExamJSON.Body.String())
	}

	var respV1, respV2 struct {
		OK           bool       `json:"ok"`
		Skipped      bool       `json:"skipped"`
		ConnectionID string     `json:"connectionId"`
		RelDir       string     `json:"relDir"`
		Entry        cloudEntry `json:"entry"`
	}
	_ = json.Unmarshal(upExamDocx.Body.Bytes(), &respV1)
	_ = json.Unmarshal(upExamDocxV2.Body.Bytes(), &respV2)
	if !respV1.OK || respV1.Skipped || respV1.ConnectionID != "m1-gdrive-1" || respV1.RelDir != "已组试卷" {
		t.Fatalf("unexpected auto-upload response: %+v", respV1)
	}
	if respV1.Entry.ID != respV2.Entry.ID {
		t.Fatalf("expected overwrite=true default to preserve ID %q, got %q", respV1.Entry.ID, respV2.Entry.ID)
	}

	if item, ok := mock.findByPath("Biofolio", "已导入试卷", "高三生物一模", "高三生物一模.json"); !ok || string(item.data) != `{"paper":"高三生物一模"}` {
		t.Fatalf("missing or mismatched imported json: %+v ok=%v", item, ok)
	}
	if item, ok := mock.findByPath("Biofolio", "已导入试卷", "高三生物一模", "高三生物一模.docx"); !ok || string(item.data) != "DOCX_IMPORT_BYTES" {
		t.Fatalf("missing or mismatched imported docx: %+v ok=%v", item, ok)
	}
	if item, ok := mock.findByPath("Biofolio", "已导入试卷", "高三生物一模", "高三生物一模.pdf"); !ok || string(item.data) != "PDF_IMPORT_BYTES" {
		t.Fatalf("missing or mismatched imported pdf: %+v ok=%v", item, ok)
	}
	if item, ok := mock.findByPath("Biofolio", "已组试卷", "期中组卷.docx"); !ok || string(item.data) != "EXAM_DOCX_V2" {
		t.Fatalf("missing or mismatched exam docx: %+v ok=%v", item, ok)
	}
	if item, ok := mock.findByPath("Biofolio", "已组试卷", "期中组卷.json"); !ok || string(item.data) != `{"edition":"期中组卷"}` {
		t.Fatalf("missing or mismatched exam json: %+v ok=%v", item, ok)
	}

	// 6. Wang has no connections at all -> skipped: true.
	wangSkip := callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", wangCookie, []byte("x"), map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Cloud-Meta": encodeMetaHeader(map[string]any{"relDir": "已组试卷", "filename": "a.docx"}),
	})
	if wangSkip.Code != http.StatusOK || !strings.Contains(wangSkip.Body.String(), `"skipped":true`) {
		t.Fatalf("expected skipped:true for wang, got %d %s", wangSkip.Code, wangSkip.Body.String())
	}

	// 7. Body exceeding maxCloudFileBytes (128 MiB + 1) -> 413 Request Entity Too Large.
	bigReq := httptest.NewRequest(http.MethodPost, "/biology/library/cloud/auto-upload", &zeroLimitReader{remaining: maxCloudFileBytes + 1})
	bigReq.AddCookie(adminCookie)
	bigReq.Header.Set("X-Biology-Library", "1")
	bigReq.Header.Set("Content-Type", "application/octet-stream")
	bigReq.Header.Set("X-Cloud-Meta", encodeMetaHeader(map[string]any{"relDir": "已组试卷", "filename": "huge.bin"}))
	bigW := httptest.NewRecorder()
	h.ServeHTTP(bigW, bigReq)
	if bigW.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for >128MiB body, got %d (%s)", bigW.Code, bigW.Body.String())
	}
}

func newTestGDriveProviderForRecovery(t *testing.T, connID, rootDir string) *gdriveProvider {
	t.Helper()
	l, _, _ := newTestCloudLibrary(t, true)
	p, err := newGDriveProvider(l, cloudAdminSettings{}, cloudConnectionRow{
		ID:      connID,
		Account: "teacher",
		Kind:    "gdrive",
		RootDir: rootDir,
	}, gdriveSecret{
		AccessToken: "tok-valid",
		ExpiryUnix:  time.Now().Add(time.Hour).Unix(),
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGDriveStaleCacheRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("Mkdir_trashed_parent_within_TTL", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		p := newTestGDriveProviderForRecovery(t, "conn-stale-mkdir-parent", "")

		mkA, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷A")
		if err != nil {
			t.Fatalf("warm Mkdir(试卷A) failed: %v", err)
		}
		papersFolder, ok := mock.findByPath("Biofolio", "已导入试卷")
		if !ok {
			t.Fatal("missing Biofolio/已导入试卷 after warm Mkdir")
		}
		mock.trashByID(papersFolder.id)

		// Do NOT call p.List before Mkdir: stale parent ID must recover within 30s TTL.
		mkB, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷B")
		if err != nil {
			t.Fatalf("Mkdir(rel:已导入试卷, 试卷B) after parent trash failed: %v", err)
		}
		if !mkB.IsDir || mkB.Name != "试卷B" || mkB.Path != "rel:已导入试卷/试卷B" || mkB.ID == "" {
			t.Fatalf("unexpected mkB entry: %+v", mkB)
		}
		newPapers, ok := mock.findByPath("Biofolio", "已导入试卷")
		if !ok || newPapers.id == papersFolder.id {
			t.Fatalf("expected recreated 已导入试卷 folder != %q, got ok=%v item=%+v", papersFolder.id, ok, newPapers)
		}
		liveB, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷B")
		if !ok || liveB.id != mkB.ID {
			t.Fatalf("expected live 试卷B ID %q, got ok=%v item=%+v", mkB.ID, ok, liveB)
		}

		// Subsequent Mkdir calls prove cache is healed, not still poisoned.
		mkB2, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷B")
		if err != nil || mkB2.ID != mkB.ID {
			t.Fatalf("idempotent Mkdir(试卷B) mismatch: mkB2=%+v err=%v", mkB2, err)
		}
		if _, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷C"); err != nil {
			t.Fatalf("Mkdir(试卷C) failed: %v", err)
		}
		if _, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷C"); !ok {
			t.Fatal("missing Biofolio/已导入试卷/试卷C")
		}
		mkARecreated, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷A")
		if err != nil || mkARecreated.ID == "" || mkARecreated.ID == mkA.ID {
			t.Fatalf("expected recreated live 试卷A != %q, got %+v err=%v", mkA.ID, mkARecreated, err)
		}
		if liveA, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A"); !ok || liveA.parent != newPapers.id {
			t.Fatalf("expected recreated 试卷A under new parent %q, got ok=%v item=%+v", newPapers.id, ok, liveA)
		}
		mock.assertNoDuplicateFolders(t)
	})

	t.Run("Mkdir_trashed_target_within_TTL", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		p := newTestGDriveProviderForRecovery(t, "conn-stale-mkdir-leaf", "")

		mkA1, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷A")
		if err != nil || mkA1.ID == "" {
			t.Fatalf("warm Mkdir(试卷A) failed: %+v err=%v", mkA1, err)
		}
		mock.trashByID(mkA1.ID)

		// Do NOT call p.List before Mkdir.
		mkA2, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷A")
		if err != nil {
			t.Fatalf("Mkdir(rel:已导入试卷, 试卷A) after leaf trash failed: %v", err)
		}
		if mkA2.ID == "" || mkA2.ID == mkA1.ID || mkA2.Path != "rel:已导入试卷/试卷A" {
			t.Fatalf("expected new live folder ID != %q, got %+v", mkA1.ID, mkA2)
		}
		liveA, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A")
		if !ok || liveA.trashed || liveA.id != mkA2.ID {
			t.Fatalf("expected live 试卷A matching %q, got ok=%v item=%+v", mkA2.ID, ok, liveA)
		}

		mkA3, err := p.Mkdir(ctx, "rel:已导入试卷", "试卷A")
		if err != nil || mkA3.ID != mkA2.ID {
			t.Fatalf("idempotent Mkdir(试卷A) mismatch: mkA3=%+v err=%v", mkA3, err)
		}
		if _, err := p.Upload(ctx, "rel:已导入试卷/试卷A", "check.json", []byte(`{"live":true}`), "application/json", true); err != nil {
			t.Fatalf("Upload into recreated leaf failed: %v", err)
		}
		if _, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A", "check.json"); !ok {
			t.Fatal("missing check.json under recreated 试卷A")
		}
		mock.assertNoDuplicateFolders(t)
	})

	t.Run("Upload_trashed_intermediate_parent_within_TTL", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		p := newTestGDriveProviderForRecovery(t, "conn-stale-upload-parent", "")

		if _, err := p.Upload(ctx, "rel:已导入试卷/试卷A", "a.json", []byte(`{"a":1}`), "application/json", true); err != nil {
			t.Fatalf("warm Upload failed: %v", err)
		}
		papersFolder, ok := mock.findByPath("Biofolio", "已导入试卷")
		if !ok {
			t.Fatal("missing Biofolio/已导入试卷")
		}
		mock.trashByID(papersFolder.id)

		// Do NOT call p.List before Upload.
		upB, err := p.Upload(ctx, "rel:已导入试卷/试卷B", "b.json", []byte(`{"b":2}`), "application/json", true)
		if err != nil {
			t.Fatalf("Upload(rel:已导入试卷/试卷B) after intermediate parent trash failed: %v", err)
		}
		if upB.Path != "rel:已导入试卷/试卷B/b.json" {
			t.Fatalf("unexpected upB.Path = %q", upB.Path)
		}
		newPapers, ok := mock.findByPath("Biofolio", "已导入试卷")
		if !ok || newPapers.id == papersFolder.id {
			t.Fatalf("expected recreated 已导入试卷 != %q, got ok=%v item=%+v", papersFolder.id, ok, newPapers)
		}
		itemB, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷B", "b.json")
		if !ok || string(itemB.data) != `{"b":2}` {
			t.Fatalf("missing or mismatched b.json: ok=%v item=%+v", ok, itemB)
		}
		dl, err := p.Download(ctx, "rel:已导入试卷/试卷B/b.json")
		if err != nil || string(dl.Data) != `{"b":2}` {
			t.Fatalf("Download(b.json) mismatch: data=%q err=%v", string(dl.Data), err)
		}

		// Also trash top-level Biofolio root and upload immediately without List.
		bioFolder, ok := mock.findByPath("Biofolio")
		if !ok {
			t.Fatal("missing Biofolio root")
		}
		mock.trashByID(bioFolder.id)
		if _, err := p.Upload(ctx, "rel:已组试卷", "exam.json", []byte(`{"e":1}`), "application/json", true); err != nil {
			t.Fatalf("Upload after Biofolio root trash failed: %v", err)
		}
		newBio, ok := mock.findByPath("Biofolio")
		if !ok || newBio.id == bioFolder.id {
			t.Fatalf("expected recreated Biofolio != %q, got ok=%v item=%+v", bioFolder.id, ok, newBio)
		}
		if _, ok := mock.findByPath("Biofolio", "已组试卷", "exam.json"); !ok {
			t.Fatal("missing Biofolio/已组试卷/exam.json")
		}
		mock.assertNoDuplicateFolders(t)
	})

	t.Run("Upload_trashed_leaf_without_List_within_TTL", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		p := newTestGDriveProviderForRecovery(t, "conn-stale-upload-leaf", "")

		if _, err := p.Upload(ctx, "rel:已导入试卷/试卷A", "v1.json", []byte(`{"v":1}`), "application/json", true); err != nil {
			t.Fatalf("warm Upload failed: %v", err)
		}
		leafA, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A")
		if !ok {
			t.Fatal("missing Biofolio/已导入试卷/试卷A")
		}
		mock.trashByID(leafA.id)

		// Do NOT call p.List before Upload.
		if _, err := p.Upload(ctx, "rel:已导入试卷/试卷A", "v2.json", []byte(`{"v":2}`), "application/json", true); err != nil {
			t.Fatalf("Upload after leaf trash (without List) failed: %v", err)
		}
		newLeafA, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A")
		if !ok || newLeafA.id == leafA.id {
			t.Fatalf("expected recreated leaf 试卷A != %q, got ok=%v item=%+v", leafA.id, ok, newLeafA)
		}
		if item, ok := mock.findByPath("Biofolio", "已导入试卷", "试卷A", "v2.json"); !ok || string(item.data) != `{"v":2}` {
			t.Fatalf("missing or mismatched v2.json: ok=%v item=%+v", ok, item)
		}
		mock.assertNoDuplicateFolders(t)
	})

	t.Run("Legacy_RootDir_trashed_within_TTL", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		p := newTestGDriveProviderForRecovery(t, "conn-stale-legacy-root", "/生物题册/")

		preDir, err := p.Mkdir(ctx, "", "预热")
		if err != nil || preDir.ID == "" {
			t.Fatalf("warm legacy Mkdir failed: %+v err=%v", preDir, err)
		}
		rootFolder, ok := mock.findByPath("生物题册")
		if !ok {
			t.Fatal("missing configured RootDir 生物题册")
		}
		mock.trashByID(rootFolder.id)

		// Legacy Mkdir("", ...) recovers from trashed RootDir cache within TTL.
		subDir, err := p.Mkdir(ctx, "", "子目录")
		if err != nil || subDir.ID == "" {
			t.Fatalf("legacy Mkdir(\"\", 子目录) after RootDir trash failed: %+v err=%v", subDir, err)
		}
		newRoot1, ok := mock.findByPath("生物题册")
		if !ok || newRoot1.id == rootFolder.id {
			t.Fatalf("expected recreated 生物题册 != %q, got ok=%v item=%+v", rootFolder.id, ok, newRoot1)
		}
		liveSub, ok := mock.findByPath("生物题册", "子目录")
		if !ok || liveSub.id != subDir.ID {
			t.Fatalf("expected live 子目录 %q, got ok=%v item=%+v", subDir.ID, ok, liveSub)
		}
		if _, err := p.Mkdir(ctx, "", "子目录2"); err != nil {
			t.Fatalf("subsequent legacy Mkdir(\"\", 子目录2) failed: %v", err)
		}
		if _, ok := mock.findByPath("生物题册", "子目录2"); !ok {
			t.Fatal("missing 生物题册/子目录2")
		}

		// Legacy Upload("", ...) recovers from trashed RootDir cache within TTL.
		mock.trashByID(newRoot1.id)
		upLeg, err := p.Upload(ctx, "", "legacy.json", []byte(`{"legacy":true}`), "application/json", true)
		if err != nil {
			t.Fatalf("legacy Upload(\"\", legacy.json) after RootDir trash failed: %v", err)
		}
		newRoot2, ok := mock.findByPath("生物题册")
		if !ok || newRoot2.id == newRoot1.id {
			t.Fatalf("expected recreated 生物题册 != %q, got ok=%v item=%+v", newRoot1.id, ok, newRoot2)
		}
		itLeg, ok := mock.findByPath("生物题册", "legacy.json")
		if !ok || string(itLeg.data) != `{"legacy":true}` || itLeg.id != upLeg.ID {
			t.Fatalf("missing or mismatched legacy.json: ok=%v item=%+v up=%+v", ok, itLeg, upLeg)
		}
		dlLeg, err := p.Download(ctx, upLeg.ID)
		if err != nil || string(dlLeg.Data) != `{"legacy":true}` {
			t.Fatalf("Download(legacy.json) mismatch: data=%q err=%v", string(dlLeg.Data), err)
		}

		// Control check: explicit trashed Google Drive folder ID must still fail and not be recreated.
		if _, err := p.Mkdir(ctx, newRoot1.id, "should-fail"); err == nil {
			t.Fatal("expected Mkdir on explicit trashed folder ID to fail")
		} else if gdriveErrorStatus(err) != http.StatusNotFound {
			t.Fatalf("expected HTTP 404 status on mkdirByParentID error, got %d (%v)", gdriveErrorStatus(err), err)
		}
		mock.assertNoDuplicateFolders(t)
	})

	t.Run("RealDrive_trashed_true_and_resumable_chunk_PUT_404", func(t *testing.T) {
		// Part A: Real Google Drive v3 trashed=true (GET /files?q=... returns 200 [], GET /files/{id} returns 200 trashed:true).
		mock := newMockGDriveServer(t)
		mock.realDriveTrashMode = true
		mock.useForTest(t)
		p := newTestGDriveProviderForRecovery(t, "conn-real-drive-trash", "")

		up1, err := p.Upload(ctx, "rel:同步库", "state.json", []byte(`{"v":1}`), "application/json", true)
		if err != nil {
			t.Fatalf("initial upload failed: %v", err)
		}

		// Steady-state overwrite when len(files) > 0 must issue 0 extra getFileMeta calls.
		mock.mu.Lock()
		metaBefore := mock.getFileMetaCount
		mock.mu.Unlock()
		if _, err := p.Upload(ctx, "rel:同步库", "state.json", []byte(`{"v":2}`), "application/json", true); err != nil {
			t.Fatalf("steady-state overwrite failed: %v", err)
		}
		mock.mu.Lock()
		metaAfter := mock.getFileMetaCount
		mock.mu.Unlock()
		if metaAfter != metaBefore {
			t.Fatalf("expected 0 extra getFileMeta calls on len(files)>0 overwrite, got %d", metaAfter-metaBefore)
		}

		// Trash 同步库 in realDriveTrashMode (no 404 from list/POST) and upload a new file.
		syncFolder, ok := mock.findByPath("Biofolio", "同步库")
		if !ok {
			t.Fatal("missing Biofolio/同步库")
		}
		mock.trashByID(syncFolder.id)
		if _, err := p.Upload(ctx, "rel:同步库", "new-shard.json", []byte(`{"shard":1}`), "application/json", true); err != nil {
			t.Fatalf("Upload after realDriveTrashMode folder trash failed: %v", err)
		}
		newSync, ok := mock.findByPath("Biofolio", "同步库")
		if !ok || newSync.id == syncFolder.id {
			t.Fatalf("expected recreated live 同步库 != %q, got ok=%v item=%+v", syncFolder.id, ok, newSync)
		}
		if _, ok := mock.findByPath("Biofolio", "同步库", "new-shard.json"); !ok {
			t.Fatal("expected new-shard.json under live Biofolio/同步库 (must not be buried in Trash)")
		}

		// Trash newSync and call Mkdir on it under realDriveTrashMode.
		mock.trashByID(newSync.id)
		mkSync, err := p.Mkdir(ctx, "rel:", "同步库")
		if err != nil || mkSync.ID == "" || mkSync.ID == newSync.id {
			t.Fatalf("Mkdir after realDriveTrashMode trash failed: mkSync=%+v err=%v", mkSync, err)
		}
		if liveSync, ok := mock.findByPath("Biofolio", "同步库"); !ok || liveSync.id != mkSync.ID {
			t.Fatalf("expected live 同步库 matching %q, got ok=%v item=%+v", mkSync.ID, ok, liveSync)
		}

		// Download by explicit ID on trashed file in realDriveTrashMode must return errCloudNotFound.
		if _, err := p.Download(ctx, up1.ID); !errors.Is(err, errCloudNotFound) {
			t.Fatalf("expected errCloudNotFound downloading trashed ID in realDriveTrashMode, got %v", err)
		}
		mock.assertNoDuplicateFolders(t)

		// Part B: uploadResumable (> 4 MiB, 2 chunks > 8 MiB) overwrite chunk-PUT 404 recovery.
		mock2 := newMockGDriveServer(t)
		mock2.useForTest(t)
		p2 := newTestGDriveProviderForRecovery(t, "conn-resumable-chunk-404", "")

		payload1 := bytes.Repeat([]byte("A"), (8<<20)+1024)
		upOrig, err := p2.Upload(ctx, "rel:同步库/papers", "big-overwrite.bin", payload1, "application/octet-stream", true)
		if err != nil {
			t.Fatalf("initial >8MiB upload failed: %v", err)
		}

		mock2.mu.Lock()
		mock2.trashOnFirstChunk308ID = upOrig.ID
		mock2.mu.Unlock()

		payload2 := bytes.Repeat([]byte("B"), (8<<20)+1024)
		upRecovered, err := p2.Upload(ctx, "rel:同步库/papers", "big-overwrite.bin", payload2, "application/octet-stream", true)
		if err != nil {
			t.Fatalf("resumable overwrite after chunk-PUT 404 failed: %v", err)
		}
		if upRecovered.ID == "" || upRecovered.ID == upOrig.ID {
			t.Fatalf("expected new file ID after trashed overwrite recovery, got %+v (orig=%q)", upRecovered, upOrig.ID)
		}
		dlBig, err := p2.Download(ctx, upRecovered.Path)
		if err != nil || !bytes.Equal(dlBig.Data, payload2) {
			t.Fatalf("recovered >8MiB download mismatch: len=%d err=%v", len(dlBig.Data), err)
		}
		mock2.assertNoDuplicateFolders(t)
	})
}

func TestGDriveConcurrentStaleCacheRecoveryRace(t *testing.T) {
	mock := newMockGDriveServer(t)
	mock.useForTest(t)
	mock.folderCreateDelay = 8 * time.Millisecond

	l, _, _ := newTestCloudLibrary(t, true)
	secBytes, _ := json.Marshal(gdriveSecret{
		RefreshToken:       "ref-stale-race-1",
		AccessToken:        "expired",
		ExpiryUnix:         1, // expired to contend on tokenRefreshLocks + secMu + folderState.mu
		CustomClientID:     "cid",
		CustomClientSecret: "csec",
	})
	row := cloudConnectionRow{
		ID:      "conn-stale-race-1",
		Account: "teacher",
		Kind:    "gdrive",
		Name:    "StaleRaceDrive",
		RootDir: "",
	}
	if err := l.saveCloudConnection(row, secBytes); err != nil {
		t.Fatal(err)
	}
	var sec gdriveSecret
	_ = json.Unmarshal(secBytes, &sec)
	p, err := newGDriveProvider(l, cloudAdminSettings{}, row, sec, true)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := p.Mkdir(ctx, "rel:已导入试卷", "预热"); err != nil {
		t.Fatalf("warm Mkdir failed: %v", err)
	}
	papersFolder, ok := mock.findByPath("Biofolio", "已导入试卷")
	if !ok {
		t.Fatal("missing Biofolio/已导入试卷")
	}
	mock.trashByID(papersFolder.id)

	const workers = 24
	start := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			if idx%2 == 0 {
				if _, err := p.Mkdir(ctx, "rel:已导入试卷", "并发试卷"); err != nil {
					errCh <- fmt.Errorf("worker %d mkdir: %w", idx, err)
					return
				}
			} else if idx%5 == 1 {
				if _, err := p.Mkdir(ctx, "rel:已导入试卷", fmt.Sprintf("分卷-%d", idx%3)); err != nil {
					errCh <- fmt.Errorf("worker %d sub-mkdir: %w", idx, err)
					return
				}
			}
			_, err := p.Upload(ctx, "rel:已导入试卷/并发试卷", fmt.Sprintf("item-%02d.json", idx), []byte(fmt.Sprintf(`{"i":%d}`, idx)), "application/json", true)
			if err != nil {
				errCh <- fmt.Errorf("worker %d upload: %w", idx, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent stale-cache recovery error: %v", err)
	}

	mock.assertNoDuplicateFolders(t)
	livePapers, ok := mock.findByPath("Biofolio", "已导入试卷")
	if !ok || livePapers.id == papersFolder.id {
		t.Fatalf("expected recreated live 已导入试卷 != %q, got ok=%v item=%+v", papersFolder.id, ok, livePapers)
	}
	liveTarget, ok := mock.findByPath("Biofolio", "已导入试卷", "并发试卷")
	if !ok {
		t.Fatal("expected Biofolio/已导入试卷/并发试卷")
	}
	if children := mock.listChildren(liveTarget.id); len(children) != workers {
		t.Fatalf("expected %d uploaded files, got %d", workers, len(children))
	}
	if mock.tokenRefreshCount != 1 {
		t.Fatalf("expected single coalesced token refresh, got %d", mock.tokenRefreshCount)
	}
}

func TestGDriveTransient429And503And401Retry(t *testing.T) {
	prevBaseDelay := gdriveRetryBaseDelay
	gdriveRetryBaseDelay = 10 * time.Millisecond
	t.Cleanup(func() { gdriveRetryBaseDelay = prevBaseDelay })

	ctx := context.Background()

	t.Run("429_and_503_retry_on_list_and_upload", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		var mu sync.Mutex
		listAttempts := 0
		uploadAttempts := 0
		mock.onRequest = func(w http.ResponseWriter, r *http.Request) bool {
			mu.Lock()
			defer mu.Unlock()
			if r.URL.Path == "/drive/v3/files" && r.Method == http.MethodGet {
				listAttempts++
				if listAttempts == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"code":429,"message":"rateLimitExceeded"}}`))
					return true
				}
			}
			if r.URL.Path == "/upload/drive/v3/files" && r.Method == http.MethodPost {
				uploadAttempts++
				if uploadAttempts == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":{"code":503,"message":"backendError"}}`))
					return true
				}
			}
			return false
		}

		p := newTestGDriveProviderForRecovery(t, "conn-retry-429-503", "")
		up, err := p.Upload(ctx, "rel:同步库", "state.json", []byte(`{"ok":true}`), "application/json", true)
		if err != nil {
			t.Fatalf("Upload with 429+503 retry failed: %v", err)
		}
		if up.ID == "" {
			t.Fatalf("expected uploaded entry ID, got %+v", up)
		}
		dl, err := p.Download(ctx, "rel:同步库/state.json")
		if err != nil || string(dl.Data) != `{"ok":true}` {
			t.Fatalf("Download after retry mismatch: data=%q err=%v", string(dl.Data), err)
		}
	})

	t.Run("unexpired_but_revoked_401_forces_single_coalesced_refresh", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		mock.onRequest = func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasPrefix(r.URL.Path, "/drive/v3/") || strings.HasPrefix(r.URL.Path, "/upload/drive/v3/") {
				if r.Header.Get("Authorization") == "Bearer unexpired-revoked-tok" {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid Credentials"}}`))
					return true
				}
			}
			return false
		}

		l, _, _ := newTestCloudLibrary(t, true)
		secBytes, _ := json.Marshal(gdriveSecret{
			RefreshToken:       "ref-401-1",
			AccessToken:        "unexpired-revoked-tok",
			ExpiryUnix:         time.Now().Add(time.Hour).Unix(), // still > now+60
			CustomClientID:     "cid",
			CustomClientSecret: "csec",
		})
		row := cloudConnectionRow{
			ID:      "conn-401-revoked",
			Account: "teacher",
			Kind:    "gdrive",
			Name:    "Revoked401Drive",
			RootDir: "",
		}
		if err := l.saveCloudConnection(row, secBytes); err != nil {
			t.Fatal(err)
		}
		var sec gdriveSecret
		_ = json.Unmarshal(secBytes, &sec)
		p, err := newGDriveProvider(l, cloudAdminSettings{}, row, sec, true)
		if err != nil {
			t.Fatal(err)
		}

		const workers = 8
		start := make(chan struct{})
		errCh := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				if _, err := p.List(ctx, "rel:同步库"); err != nil {
					errCh <- fmt.Errorf("worker %d list: %w", idx, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
		if mock.tokenRefreshCount != 1 {
			t.Fatalf("expected 1 coalesced forced refresh on 401, got %d", mock.tokenRefreshCount)
		}
		if _, err := p.Upload(ctx, "rel:同步库", "after-401.json", []byte(`{"refreshed":true}`), "application/json", true); err != nil {
			t.Fatalf("Upload after 401 refresh failed: %v", err)
		}
		if mock.tokenRefreshCount != 1 {
			t.Fatalf("expected refreshed token persisted without extra refresh, got %d", mock.tokenRefreshCount)
		}
	})

	t.Run("resumable_chunk_PUT_503_retries_only_failed_chunk", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		var mu sync.Mutex
		putCount := 0
		failedChunk2Once := false
		mock.onRequest = func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasPrefix(r.URL.Path, "/upload/drive/v3/session/") && r.Method == http.MethodPut {
				mu.Lock()
				putCount++
				cur := putCount
				shouldFail := cur == 2 && !failedChunk2Once
				if shouldFail {
					failedChunk2Once = true
				}
				mu.Unlock()
				if shouldFail {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":{"code":503,"message":"chunk transient failure"}}`))
					return true
				}
			}
			return false
		}

		p := newTestGDriveProviderForRecovery(t, "conn-resumable-503", "")
		payload := bytes.Repeat([]byte("R"), (8<<20)+4096)
		up, err := p.Upload(ctx, "rel:同步库/papers", "heavy-retry.bin", payload, "application/octet-stream", true)
		if err != nil {
			t.Fatalf("resumable upload with chunk 2 503 retry failed: %v", err)
		}
		mu.Lock()
		gotPuts := putCount
		mu.Unlock()
		if gotPuts != 3 {
			t.Fatalf("expected exactly 3 chunk PUTs (chunk1 + failed chunk2 + retried chunk2), got %d", gotPuts)
		}
		dl, err := p.Download(ctx, up.Path)
		if err != nil || !bytes.Equal(dl.Data, payload) {
			t.Fatalf("downloaded >8MiB payload mismatch after chunk retry: len=%d err=%v", len(dl.Data), err)
		}
	})

	t.Run("retry_respects_context_cancellation", func(t *testing.T) {
		mock := newMockGDriveServer(t)
		mock.useForTest(t)
		mock.onRequest = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/drive/v3/files" && r.Method == http.MethodGet {
				w.Header().Set("Retry-After", "4")
				w.WriteHeader(http.StatusTooManyRequests)
				return true
			}
			return false
		}
		p := newTestGDriveProviderForRecovery(t, "conn-retry-cancel", "")
		cctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
		defer cancel()
		_, err := p.List(cctx, "rel:同步库")
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context deadline/cancel error during Retry-After sleep, got %v", err)
		}
	})
}
