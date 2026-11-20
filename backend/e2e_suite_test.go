//go:build e2e

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// Stateful Fake Google Drive v3 + OAuth2 Server
// -----------------------------------------------------------------------------

type fakeDriveNode struct {
	ID           string
	Name         string
	MimeType     string
	Parents      []string
	Data         []byte
	ModifiedTime string
	Trashed      bool
}

type fakeUploadSession struct {
	ParentID    string
	OverwriteID string
	Name        string
	MimeType    string
	TotalSize   int
	Buf         []byte
	Received    int
}

type fakeDriveUploadEvent struct {
	Path      string
	FileID    string
	ParentID  string
	Name      string
	Method    string
	Mode      string
	Size      int
	Timestamp time.Time
}

type fakeDriveFaultConfig struct {
	UploadStatus        int
	FailNextUploadCount int
	UploadDelay         time.Duration
	ListStatus          int
	AboutStatus         int
	DownloadStatus      int
	ForcePageSize       int
	EnforceBearerToken  bool
}

type fakeGoogleDrive struct {
	mu                sync.Mutex
	srv               *httptest.Server
	nextID            int
	nextSess          int
	items             map[string]*fakeDriveNode
	sessions          map[string]*fakeUploadSession
	uploads           []fakeDriveUploadEvent
	folderCreateCount int
	listCalls         int
	refreshCount      int
	validTokens       map[string]bool
	userEmail         string
	faults            fakeDriveFaultConfig
}

func newFakeGoogleDrive(t *testing.T, email string) *fakeGoogleDrive {
	t.Helper()
	if email == "" {
		email = "e2e-teacher@school.edu"
	}
	gd := &fakeGoogleDrive{
		items: map[string]*fakeDriveNode{
			"root": {
				ID:           "root",
				Name:         "My Drive",
				MimeType:     googleFolderMime,
				ModifiedTime: "2026-01-01T00:00:00Z",
			},
		},
		sessions:    map[string]*fakeUploadSession{},
		validTokens: map[string]bool{"g-acc-initial": true},
		userEmail:   email,
	}
	gd.srv = httptest.NewServer(http.HandlerFunc(gd.serveHTTP))
	t.Cleanup(gd.srv.Close)
	return gd
}

func (g *fakeGoogleDrive) allocID(prefix string) string {
	g.nextID++
	return fmt.Sprintf("%s-%d", prefix, g.nextID)
}

func (g *fakeGoogleDrive) nodePathLocked(id string) string {
	var segs []string
	cur := id
	seen := map[string]bool{}
	for cur != "" && cur != "root" && !seen[cur] {
		seen[cur] = true
		n := g.items[cur]
		if n == nil {
			break
		}
		segs = append(segs, n.Name)
		if len(n.Parents) == 0 {
			break
		}
		cur = n.Parents[0]
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return "/" + strings.Join(segs, "/")
}

func (g *fakeGoogleDrive) checkAuthLocked(r *http.Request) bool {
	if !g.faults.EnforceBearerToken {
		return true
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	tok := strings.TrimPrefix(auth, "Bearer ")
	return g.validTokens[tok]
}

func (g *fakeGoogleDrive) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		grant := r.Form.Get("grant_type")
		g.mu.Lock()
		defer g.mu.Unlock()
		switch grant {
		case "authorization_code":
			tok := g.allocID("g-acc")
			g.validTokens[tok] = true
			jsonResponse(w, map[string]any{
				"access_token":  tok,
				"refresh_token": "g-ref-" + g.allocID("tok"),
				"expires_in":    3600,
				"token_type":    "Bearer",
			})
		case "refresh_token":
			g.refreshCount++
			tok := fmt.Sprintf("g-acc-ref-%d", g.refreshCount)
			g.validTokens[tok] = true
			jsonResponse(w, map[string]any{
				"access_token": tok,
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		}
		return

	case r.URL.Path == "/drive/v3/about" && r.Method == http.MethodGet:
		g.mu.Lock()
		status := g.faults.AboutStatus
		email := g.userEmail
		authOK := g.checkAuthLocked(r)
		g.mu.Unlock()
		if !authOK {
			http.Error(w, `{"error":{"code":401,"message":"Invalid Credentials"}}`, http.StatusUnauthorized)
			return
		}
		if status != 0 {
			http.Error(w, "about fault", status)
			return
		}
		jsonResponse(w, map[string]any{
			"user": map[string]string{
				"emailAddress": email,
				"displayName":  "E2E Teacher",
			},
		})
		return

	case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodGet:
		g.mu.Lock()
		defer g.mu.Unlock()
		g.listCalls++
		if !g.checkAuthLocked(r) {
			http.Error(w, `{"error":{"code":401,"message":"Invalid Credentials"}}`, http.StatusUnauthorized)
			return
		}
		if g.faults.ListStatus != 0 {
			http.Error(w, "list fault", g.faults.ListStatus)
			return
		}
		q := r.URL.Query().Get("q")
		preds := parseDriveQuery(q)
		var matched []*fakeDriveNode
		for id, n := range g.items {
			if id == "root" {
				continue
			}
			if preds.match(n) {
				matched = append(matched, n)
			}
		}
		sort.Slice(matched, func(i, j int) bool {
			ai, aj := matched[i].MimeType == googleFolderMime, matched[j].MimeType == googleFolderMime
			if ai != aj {
				return ai
			}
			if matched[i].ModifiedTime != matched[j].ModifiedTime {
				return matched[i].ModifiedTime > matched[j].ModifiedTime
			}
			return matched[i].Name < matched[j].Name
		})
		pageSize := 200
		if ps, err := strconv.Atoi(r.URL.Query().Get("pageSize")); err == nil && ps > 0 {
			pageSize = ps
		}
		if g.faults.ForcePageSize > 0 && g.faults.ForcePageSize < pageSize {
			pageSize = g.faults.ForcePageSize
		}
		start := 0
		if pt := r.URL.Query().Get("pageToken"); pt != "" {
			if v, err := strconv.Atoi(pt); err == nil && v >= 0 {
				start = v
			}
		}
		if start > len(matched) {
			start = len(matched)
		}
		end := start + pageSize
		nextToken := ""
		if end < len(matched) {
			nextToken = strconv.Itoa(end)
		} else {
			end = len(matched)
		}
		files := make([]map[string]any, 0, end-start)
		for _, it := range matched[start:end] {
			files = append(files, map[string]any{
				"id":           it.ID,
				"name":         it.Name,
				"mimeType":     it.MimeType,
				"size":         strconv.Itoa(len(it.Data)),
				"modifiedTime": it.ModifiedTime,
				"parents":      it.Parents,
			})
		}
		resp := map[string]any{"files": files}
		if nextToken != "" {
			resp["nextPageToken"] = nextToken
		}
		jsonResponse(w, resp)
		return

	case r.URL.Path == "/drive/v3/files" && r.Method == http.MethodPost:
		g.mu.Lock()
		defer g.mu.Unlock()
		if !g.checkAuthLocked(r) {
			http.Error(w, `{"error":{"code":401,"message":"Invalid Credentials"}}`, http.StatusUnauthorized)
			return
		}
		var meta struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if meta.MimeType == "" {
			meta.MimeType = googleFolderMime
		}
		parents := meta.Parents
		if len(parents) == 0 {
			parents = []string{"root"}
		}
		id := g.allocID("gf")
		now := time.Now().UTC().Format(time.RFC3339Nano)
		node := &fakeDriveNode{
			ID:           id,
			Name:         meta.Name,
			MimeType:     meta.MimeType,
			Parents:      parents,
			ModifiedTime: now,
		}
		// Deliberately allow duplicate folders so concurrency tests detect races.
		g.items[id] = node
		if meta.MimeType == googleFolderMime {
			g.folderCreateCount++
		}
		jsonResponse(w, map[string]any{
			"id":           node.ID,
			"name":         node.Name,
			"mimeType":     node.MimeType,
			"modifiedTime": node.ModifiedTime,
		})
		return

	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && strings.HasSuffix(r.URL.Path, "/export") && r.Method == http.MethodGet:
		fid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/drive/v3/files/"), "/export")
		g.mu.Lock()
		authOK := g.checkAuthLocked(r)
		it := g.items[fid]
		var data []byte
		if it != nil && !it.Trashed {
			data = append([]byte(nil), it.Data...)
		} else {
			it = nil
		}
		g.mu.Unlock()
		if !authOK {
			http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
			return
		}
		if it == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return

	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && r.Method == http.MethodGet:
		fid := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		g.mu.Lock()
		authOK := g.checkAuthLocked(r)
		dlFault := g.faults.DownloadStatus
		it := g.items[fid]
		var copyNode fakeDriveNode
		if it != nil && !it.Trashed {
			copyNode = *it
			copyNode.Data = append([]byte(nil), it.Data...)
		} else {
			it = nil
		}
		g.mu.Unlock()
		if !authOK {
			http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
			return
		}
		if it == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			if dlFault != 0 {
				http.Error(w, "download fault", dlFault)
				return
			}
			if copyNode.MimeType != "" {
				w.Header().Set("Content-Type", copyNode.MimeType)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(copyNode.Data)
			return
		}
		jsonResponse(w, map[string]any{
			"id":           copyNode.ID,
			"name":         copyNode.Name,
			"mimeType":     copyNode.MimeType,
			"size":         strconv.Itoa(len(copyNode.Data)),
			"modifiedTime": copyNode.ModifiedTime,
			"parents":      copyNode.Parents,
		})
		return

	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/") && r.Method == http.MethodDelete:
		fid := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		g.mu.Lock()
		defer g.mu.Unlock()
		if !g.checkAuthLocked(r) {
			http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
			return
		}
		if it := g.items[fid]; it != nil {
			it.Trashed = true
		}
		w.WriteHeader(http.StatusNoContent)
		return

	case r.URL.Path == "/upload/drive/v3/files" && r.Method == http.MethodPost:
		g.handleUploadPost(w, r)
		return

	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/files/") && r.Method == http.MethodPatch:
		fid := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files/")
		g.handleUploadPatch(w, r, fid)
		return

	case strings.HasPrefix(r.URL.Path, "/upload/session/") && r.Method == http.MethodPut:
		sid := strings.TrimPrefix(r.URL.Path, "/upload/session/")
		g.handleUploadSessionPut(w, r, sid)
		return

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *fakeGoogleDrive) checkUploadFault() (int, string) {
	g.mu.Lock()
	delay := g.faults.UploadDelay
	status := g.faults.UploadStatus
	failOnce := g.faults.FailNextUploadCount
	if failOnce > 0 {
		g.faults.FailNextUploadCount--
	}
	g.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if failOnce > 0 {
		return http.StatusServiceUnavailable, "transient upload failure"
	}
	if status != 0 {
		return status, "injected upload fault"
	}
	return 0, ""
}

func (g *fakeGoogleDrive) handleUploadPost(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	authOK := g.checkAuthLocked(r)
	g.mu.Unlock()
	if !authOK {
		http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
		return
	}
	if st, msg := g.checkUploadFault(); st != 0 {
		http.Error(w, msg, st)
		return
	}

	uploadType := r.URL.Query().Get("uploadType")
	switch uploadType {
	case "resumable":
		var meta struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, "bad resumable init json", http.StatusBadRequest)
			return
		}
		totalLen, _ := strconv.Atoi(r.Header.Get("X-Upload-Content-Length"))
		mimeType := r.Header.Get("X-Upload-Content-Type")
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		parent := "root"
		if len(meta.Parents) > 0 && meta.Parents[0] != "" {
			parent = meta.Parents[0]
		}
		g.mu.Lock()
		g.nextSess++
		sid := fmt.Sprintf("sess-%d", g.nextSess)
		g.sessions[sid] = &fakeUploadSession{
			ParentID:  parent,
			Name:      meta.Name,
			MimeType:  mimeType,
			TotalSize: totalLen,
			Buf:       make([]byte, totalLen),
		}
		g.mu.Unlock()
		w.Header().Set("Location", g.srv.URL+"/upload/session/"+sid)
		w.WriteHeader(http.StatusOK)

	case "multipart":
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
			http.Error(w, "invalid multipart", http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		var meta struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		var payload []byte
		partMime := "application/octet-stream"
		for partIdx := 0; ; partIdx++ {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "multipart read err", http.StatusBadRequest)
				return
			}
			b, _ := io.ReadAll(part)
			ct := part.Header.Get("Content-Type")
			if partIdx == 0 && strings.Contains(ct, "application/json") {
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
		g.mu.Lock()
		id := g.allocID("gf")
		now := time.Now().UTC().Format(time.RFC3339Nano)
		node := &fakeDriveNode{
			ID:           id,
			Name:         meta.Name,
			MimeType:     partMime,
			Parents:      []string{parent},
			Data:         payload,
			ModifiedTime: now,
		}
		g.items[id] = node
		p := g.nodePathLocked(id)
		g.uploads = append(g.uploads, fakeDriveUploadEvent{
			Path:      p,
			FileID:    id,
			ParentID:  parent,
			Name:      meta.Name,
			Method:    http.MethodPost,
			Mode:      "multipart",
			Size:      len(payload),
			Timestamp: time.Now(),
		})
		g.mu.Unlock()
		jsonResponse(w, map[string]any{
			"id":           node.ID,
			"name":         node.Name,
			"mimeType":     node.MimeType,
			"size":         strconv.Itoa(len(payload)),
			"modifiedTime": node.ModifiedTime,
		})

	default:
		http.Error(w, "unsupported uploadType", http.StatusBadRequest)
	}
}

func (g *fakeGoogleDrive) handleUploadPatch(w http.ResponseWriter, r *http.Request, fid string) {
	g.mu.Lock()
	authOK := g.checkAuthLocked(r)
	g.mu.Unlock()
	if !authOK {
		http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
		return
	}
	if st, msg := g.checkUploadFault(); st != 0 {
		http.Error(w, msg, st)
		return
	}

	uploadType := r.URL.Query().Get("uploadType")
	switch uploadType {
	case "media":
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read err", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		it := g.items[fid]
		if it == nil || it.Trashed {
			g.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		it.Data = payload
		if ct := r.Header.Get("Content-Type"); ct != "" {
			it.MimeType = ct
		}
		it.ModifiedTime = time.Now().UTC().Format(time.RFC3339Nano)
		parent := ""
		if len(it.Parents) > 0 {
			parent = it.Parents[0]
		}
		p := g.nodePathLocked(it.ID)
		g.uploads = append(g.uploads, fakeDriveUploadEvent{
			Path:      p,
			FileID:    it.ID,
			ParentID:  parent,
			Name:      it.Name,
			Method:    http.MethodPatch,
			Mode:      "media",
			Size:      len(payload),
			Timestamp: time.Now(),
		})
		out := map[string]any{
			"id":           it.ID,
			"name":         it.Name,
			"mimeType":     it.MimeType,
			"size":         strconv.Itoa(len(payload)),
			"modifiedTime": it.ModifiedTime,
		}
		g.mu.Unlock()
		jsonResponse(w, out)

	case "resumable":
		totalLen, _ := strconv.Atoi(r.Header.Get("X-Upload-Content-Length"))
		mimeType := r.Header.Get("X-Upload-Content-Type")
		g.mu.Lock()
		it := g.items[fid]
		if it == nil || it.Trashed {
			g.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if mimeType == "" {
			mimeType = it.MimeType
		}
		parent := "root"
		if len(it.Parents) > 0 {
			parent = it.Parents[0]
		}
		g.nextSess++
		sid := fmt.Sprintf("sess-%d", g.nextSess)
		g.sessions[sid] = &fakeUploadSession{
			ParentID:    parent,
			OverwriteID: fid,
			Name:        it.Name,
			MimeType:    mimeType,
			TotalSize:   totalLen,
			Buf:         make([]byte, totalLen),
		}
		g.mu.Unlock()
		w.Header().Set("Location", g.srv.URL+"/upload/session/"+sid)
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "unsupported patch uploadType", http.StatusBadRequest)
	}
}

func (g *fakeGoogleDrive) handleUploadSessionPut(w http.ResponseWriter, r *http.Request, sid string) {
	chunk, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read err", http.StatusBadRequest)
		return
	}
	cr := r.Header.Get("Content-Range")
	// Format: bytes <start>-<end>/<total>
	var start, end, total int
	if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err != nil {
		http.Error(w, "invalid Content-Range", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	sess := g.sessions[sid]
	if sess == nil {
		g.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if start < 0 || end >= sess.TotalSize || end-start+1 != len(chunk) {
		g.mu.Unlock()
		http.Error(w, "range mismatch", http.StatusBadRequest)
		return
	}
	copy(sess.Buf[start:end+1], chunk)
	sess.Received = end + 1
	if end+1 < total {
		g.mu.Unlock()
		// CRITICAL: Google Drive returns 308 Permanent Redirect WITHOUT Location header on partial chunks!
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	delete(g.sessions, sid)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var node *fakeDriveNode
	method := http.MethodPost
	if sess.OverwriteID != "" {
		method = http.MethodPatch
		node = g.items[sess.OverwriteID]
		if node != nil {
			node.Data = sess.Buf
			node.MimeType = sess.MimeType
			node.ModifiedTime = now
		}
	}
	if node == nil {
		id := g.allocID("gf")
		node = &fakeDriveNode{
			ID:           id,
			Name:         sess.Name,
			MimeType:     sess.MimeType,
			Parents:      []string{sess.ParentID},
			Data:         sess.Buf,
			ModifiedTime: now,
		}
		g.items[id] = node
	}
	p := g.nodePathLocked(node.ID)
	g.uploads = append(g.uploads, fakeDriveUploadEvent{
		Path:      p,
		FileID:    node.ID,
		ParentID:  sess.ParentID,
		Name:      node.Name,
		Method:    method,
		Mode:      "resumable",
		Size:      len(node.Data),
		Timestamp: time.Now(),
	})
	out := map[string]any{
		"id":           node.ID,
		"name":         node.Name,
		"mimeType":     node.MimeType,
		"size":         strconv.Itoa(len(node.Data)),
		"modifiedTime": node.ModifiedTime,
	}
	g.mu.Unlock()
	jsonResponse(w, out)
}

// Drive `q` parser supporting: 'id' in parents, name='...', mimeType='...', trashed=false|true.
type driveQueryPreds struct {
	hasParent bool
	parentID  string
	hasName   bool
	name      string
	hasMime   bool
	mimeType  string
	hasTrash  bool
	trashed   bool
}

func (p driveQueryPreds) match(n *fakeDriveNode) bool {
	if p.hasTrash && n.Trashed != p.trashed {
		return false
	}
	if p.hasParent {
		found := false
		for _, par := range n.Parents {
			if par == p.parentID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if p.hasName && n.Name != p.name {
		return false
	}
	if p.hasMime && n.MimeType != p.mimeType {
		return false
	}
	return true
}

func unescapeDriveQuote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func splitDriveAndClauses(q string) []string {
	var clauses []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\\' && inQuote && i+1 < len(q) {
			cur.WriteByte(c)
			cur.WriteByte(q[i+1])
			i++
			continue
		}
		if c == '\'' {
			inQuote = !inQuote
			cur.WriteByte(c)
			continue
		}
		if !inQuote && i+5 <= len(q) && strings.EqualFold(q[i:i+5], " and ") {
			clauses = append(clauses, strings.TrimSpace(cur.String()))
			cur.Reset()
			i += 4
			continue
		}
		cur.WriteByte(c)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		clauses = append(clauses, s)
	}
	return clauses
}

func parseDriveQuery(q string) driveQueryPreds {
	var p driveQueryPreds
	for _, cl := range splitDriveAndClauses(q) {
		low := strings.ToLower(strings.TrimSpace(cl))
		switch {
		case strings.HasSuffix(low, " in parents"):
			idx := strings.Index(strings.ToLower(cl), " in parents")
			raw := strings.TrimSpace(cl[:idx])
			if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
				p.hasParent = true
				p.parentID = unescapeDriveQuote(raw[1 : len(raw)-1])
			}
		case strings.HasPrefix(low, "name="):
			raw := strings.TrimSpace(cl[5:])
			if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
				p.hasName = true
				p.name = unescapeDriveQuote(raw[1 : len(raw)-1])
			}
		case strings.HasPrefix(low, "mimetype="):
			raw := strings.TrimSpace(cl[9:])
			if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
				p.hasMime = true
				p.mimeType = unescapeDriveQuote(raw[1 : len(raw)-1])
			}
		case low == "trashed=false":
			p.hasTrash = true
			p.trashed = false
		case low == "trashed=true":
			p.hasTrash = true
			p.trashed = true
		}
	}
	return p
}

// --- Inspection & Manipulation Helpers on fakeGoogleDrive ---

func (g *fakeGoogleDrive) ResolvePath(relPath string) (*fakeDriveNode, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	clean := strings.Trim(relPath, "/")
	if clean == "" {
		return g.items["root"], true
	}
	cur := "root"
	var curNode *fakeDriveNode
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" {
			continue
		}
		var found *fakeDriveNode
		for _, n := range g.items {
			if n.Trashed || n.ID == "root" {
				continue
			}
			for _, p := range n.Parents {
				if p == cur && n.Name == seg {
					found = n
					break
				}
			}
			if found != nil {
				break
			}
		}
		if found == nil {
			return nil, false
		}
		cur = found.ID
		curNode = found
	}
	if curNode == nil {
		return nil, false
	}
	cp := *curNode
	cp.Data = append([]byte(nil), curNode.Data...)
	return &cp, true
}

func (g *fakeGoogleDrive) ReadPath(relPath string) ([]byte, bool) {
	n, ok := g.ResolvePath(relPath)
	if !ok {
		return nil, false
	}
	return n.Data, true
}

func (g *fakeGoogleDrive) ListDir(relPath string) []*fakeDriveNode {
	g.mu.Lock()
	defer g.mu.Unlock()
	parentID := "root"
	clean := strings.Trim(relPath, "/")
	if clean != "" {
		for _, seg := range strings.Split(clean, "/") {
			if seg == "" {
				continue
			}
			next := ""
			for _, n := range g.items {
				if n.Trashed || n.ID == "root" {
					continue
				}
				for _, p := range n.Parents {
					if p == parentID && n.Name == seg {
						next = n.ID
						break
					}
				}
				if next != "" {
					break
				}
			}
			if next == "" {
				return nil
			}
			parentID = next
		}
	}
	var out []*fakeDriveNode
	for _, n := range g.items {
		if n.Trashed || n.ID == "root" {
			continue
		}
		for _, p := range n.Parents {
			if p == parentID {
				cp := *n
				cp.Data = append([]byte(nil), n.Data...)
				out = append(out, &cp)
				break
			}
		}
	}
	return out
}

func (g *fakeGoogleDrive) CountFolders(parentID, name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := 0
	for _, n := range g.items {
		if n.Trashed || n.MimeType != googleFolderMime || n.Name != name {
			continue
		}
		for _, p := range n.Parents {
			if p == parentID {
				count++
				break
			}
		}
	}
	return count
}

func (g *fakeGoogleDrive) DuplicateFolders() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	type key struct{ parent, name string }
	counts := map[key]int{}
	for _, n := range g.items {
		if n.Trashed || n.MimeType != googleFolderMime || n.ID == "root" {
			continue
		}
		for _, p := range n.Parents {
			counts[key{p, n.Name}]++
		}
	}
	var dups []string
	for k, c := range counts {
		if c > 1 {
			dups = append(dups, fmt.Sprintf("%s/%s (x%d)", k.parent, k.name, c))
		}
	}
	sort.Strings(dups)
	return dups
}

func (g *fakeGoogleDrive) UploadCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.uploads)
}

func (g *fakeGoogleDrive) LastUpload() (fakeDriveUploadEvent, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.uploads) == 0 {
		return fakeDriveUploadEvent{}, false
	}
	return g.uploads[len(g.uploads)-1], true
}

func (g *fakeGoogleDrive) TotalUploadedBytes() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var total int64
	for _, ev := range g.uploads {
		total += int64(ev.Size)
	}
	return total
}

func (g *fakeGoogleDrive) UploadsFor(name string) []fakeDriveUploadEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []fakeDriveUploadEvent
	for _, ev := range g.uploads {
		if ev.Name == name {
			out = append(out, ev)
		}
	}
	return out
}

func (g *fakeGoogleDrive) UploadsMatchingPrefix(relPrefix string) []fakeDriveUploadEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []fakeDriveUploadEvent
	for _, ev := range g.uploads {
		if strings.HasPrefix(ev.Path, relPrefix) {
			out = append(out, ev)
		}
	}
	return out
}

func (g *fakeGoogleDrive) ResetUploadStats() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.uploads = nil
	g.listCalls = 0
}

func (g *fakeGoogleDrive) RefreshCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refreshCount
}

func (g *fakeGoogleDrive) ListCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.listCalls
}

func (g *fakeGoogleDrive) PutFileAtPath(relPath, mimeType string, data []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	clean := strings.Trim(relPath, "/")
	parts := strings.Split(clean, "/")
	cur := "root"
	for i := 0; i < len(parts)-1; i++ {
		seg := parts[i]
		if seg == "" {
			continue
		}
		foundID := ""
		for _, n := range g.items {
			if n.Trashed || n.MimeType != googleFolderMime {
				continue
			}
			for _, p := range n.Parents {
				if p == cur && n.Name == seg {
					foundID = n.ID
					break
				}
			}
			if foundID != "" {
				break
			}
		}
		if foundID == "" {
			foundID = g.allocID("gf")
			g.items[foundID] = &fakeDriveNode{
				ID:           foundID,
				Name:         seg,
				MimeType:     googleFolderMime,
				Parents:      []string{cur},
				ModifiedTime: time.Now().UTC().Format(time.RFC3339Nano),
			}
		}
		cur = foundID
	}
	filename := parts[len(parts)-1]
	for _, n := range g.items {
		if n.Trashed || n.MimeType == googleFolderMime {
			continue
		}
		for _, p := range n.Parents {
			if p == cur && n.Name == filename {
				n.Data = append([]byte(nil), data...)
				n.MimeType = mimeType
				n.ModifiedTime = time.Now().UTC().Format(time.RFC3339Nano)
				return
			}
		}
	}
	id := g.allocID("gf")
	g.items[id] = &fakeDriveNode{
		ID:           id,
		Name:         filename,
		MimeType:     mimeType,
		Parents:      []string{cur},
		Data:         append([]byte(nil), data...),
		ModifiedTime: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func (g *fakeGoogleDrive) DeletePath(relPath string) {
	n, ok := g.ResolvePath(relPath)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if real := g.items[n.ID]; real != nil {
		real.Trashed = true
	}
}

func (g *fakeGoogleDrive) ClearDir(relPath string) {
	n, ok := g.ResolvePath(relPath)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var markChildren func(pid string)
	markChildren = func(pid string) {
		for _, it := range g.items {
			if it.Trashed || it.ID == "root" {
				continue
			}
			for _, p := range it.Parents {
				if p == pid {
					it.Trashed = true
					if it.MimeType == googleFolderMime {
						markChildren(it.ID)
					}
					break
				}
			}
		}
	}
	markChildren(n.ID)
}

// -----------------------------------------------------------------------------
// E2E Test Harness Helpers (Opaque-Box HTTP + SQLite/File Inspection)
// -----------------------------------------------------------------------------

type e2eNAS struct {
	l           *libraryPage
	h           http.Handler
	acc         *libraryAccount
	adminCookie *http.Cookie
	liCookie    *http.Cookie
	wangCookie  *http.Cookie
}

func bindFakeGoogleDrive(t *testing.T, gd *fakeGoogleDrive) {
	t.Helper()
	prevLoopback := cloudAllowLoopbackForTest
	prevAuth, prevTok, prevAPI, prevUp := googleAuthURL, googleTokenURL, googleDriveAPIBase, googleDriveUploadBase
	cloudAllowLoopbackForTest = true
	t.Cleanup(func() {
		cloudAllowLoopbackForTest = prevLoopback
		googleAuthURL, googleTokenURL, googleDriveAPIBase, googleDriveUploadBase = prevAuth, prevTok, prevAPI, prevUp
	})
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("all_proxy", "")

	googleAuthURL = gd.srv.URL + "/auth"
	googleTokenURL = gd.srv.URL + "/token"
	googleDriveAPIBase = gd.srv.URL + "/drive/v3"
	googleDriveUploadBase = gd.srv.URL + "/upload/drive/v3"
}

func newE2ENAS(t *testing.T, withLogin bool, seedHTML string) *e2eNAS {
	t.Helper()
	l, h, acc := newTestCloudLibrary(t, withLogin)
	l.db.SetMaxOpenConns(1)
	if seedHTML == "" {
		seedHTML = `<html><head><script id="paper-data" type="application/json">{"studentHash":"e2e-seed","questions":[]}</script></head><body>ok</body></html>`
	}
	l.page = []byte(seedHTML)
	nas := &e2eNAS{l: l, h: h, acc: acc}
	if withLogin {
		nas.adminCookie = loginTestUser(t, h, "teacher", "admin-pass-123")
		nas.liCookie = loginTestUser(t, h, "li", "teacher-pass-1")
		nas.wangCookie = loginTestUser(t, h, "wang", "teacher-pass-2")
	}
	return nas
}

func newE2EEnv(t *testing.T) (*fakeGoogleDrive, *e2eNAS) {
	t.Helper()
	gd := newFakeGoogleDrive(t, "teacher@school.edu")
	bindFakeGoogleDrive(t, gd)
	nas := newE2ENAS(t, true, "")
	return gd, nas
}

func callJSON(h http.Handler, method, target string, cookie *http.Cookie, payload any) *httptest.ResponseRecorder {
	var body []byte
	if method == http.MethodPost {
		if payload == nil {
			body = []byte("{}")
		} else {
			switch v := payload.(type) {
			case []byte:
				body = v
			case string:
				body = []byte(v)
			default:
				body, _ = json.Marshal(v)
			}
		}
	}
	return callLibraryAPI(h, method, target, cookie, body, nil)
}

func setupAdminGoogleCreds(t *testing.T, h http.Handler, adminCookie *http.Cookie) {
	t.Helper()
	r := callJSON(h, http.MethodPost, "/biology/library/cloud/admin-settings", adminCookie, map[string]any{
		"googleClientId":     "e2e-client-id",
		"googleClientSecret": "e2e-client-secret",
		"allowPrivateWebdav": true,
	})
	if r.Code != http.StatusOK {
		t.Fatalf("admin-settings failed: %d %s", r.Code, r.Body.String())
	}
}

func connectGoogleDrive(t *testing.T, h http.Handler, cookie *http.Cookie, name, rootDir string, isDefault bool) string {
	t.Helper()
	start := callJSON(h, http.MethodPost, "/biology/library/cloud/google/start", cookie, map[string]any{
		"mode":    "web",
		"name":    name,
		"rootDir": rootDir,
	})
	if start.Code != http.StatusOK {
		t.Fatalf("google/start failed: %d %s", start.Code, start.Body.String())
	}
	var st struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &st); err != nil || st.State == "" {
		t.Fatalf("invalid google/start body: %s", start.Body.String())
	}
	cb := callLibraryAPI(h, http.MethodGet, "/biology/library/cloud/google/callback?state="+url.QueryEscape(st.State)+"&code=auth-code-"+st.State, nil, nil, nil)
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), `"ok":true`) {
		t.Fatalf("google/callback failed: %d %s", cb.Code, cb.Body.String())
	}
	cfg := callLibraryAPI(h, http.MethodGet, "/biology/library/cloud/config", cookie, nil, nil)
	if cfg.Code != http.StatusOK {
		t.Fatalf("cloud/config failed: %d %s", cfg.Code, cfg.Body.String())
	}
	var out struct {
		Connections []cloudConnectionView `json:"connections"`
	}
	_ = json.Unmarshal(cfg.Body.Bytes(), &out)
	var conn cloudConnectionView
	for _, c := range out.Connections {
		if c.Kind == "gdrive" && c.Name == name {
			conn = c
			break
		}
	}
	if conn.ID == "" {
		t.Fatalf("connected gdrive %q not found in config: %s", name, cfg.Body.String())
	}
	if isDefault {
		up := callJSON(h, http.MethodPost, "/biology/library/cloud/connections", cookie, map[string]any{
			"action":    "update_meta",
			"id":        conn.ID,
			"name":      conn.Name,
			"rootDir":   conn.RootDir,
			"isDefault": true,
			"useProxy":  true,
		})
		if up.Code != http.StatusOK {
			t.Fatalf("set default gdrive failed: %d %s", up.Code, up.Body.String())
		}
	}
	return conn.ID
}

func postAutoUpload(h http.Handler, cookie *http.Cookie, meta map[string]any, data []byte) *httptest.ResponseRecorder {
	rawMeta, _ := json.Marshal(meta)
	b64 := base64.StdEncoding.EncodeToString(rawMeta)
	mimeType, _ := meta["mimeType"].(string)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return callLibraryAPI(h, http.MethodPost, "/biology/library/cloud/auto-upload", cookie, data, map[string]string{
		"Content-Type": mimeType,
		"X-Cloud-Meta": b64,
	})
}

func makeValidQuestionRecord(id, stem, answer, analysis string) map[string]any {
	return map[string]any{
		"id": id,
		"questions": []map[string]any{
			{
				"id":           id,
				"stem":         stem,
				"body":         stem,
				"contentHtml":  "<p>" + stem + "</p>",
				"answer":       answer,
				"answerHtml":   "<p>" + answer + "</p>",
				"a":            analysis,
				"analysisHtml": "<p>" + analysis + "</p>",
				"type":         "单选题",
				"date":         "2026-11-20",
				"collectedAt":  "2026-11-20T10:00:00",
			},
		},
	}
}

func makeValidPaperRecord(id, name, origName string, origBytes []byte, solName string, solBytes []byte) map[string]any {
	rec := map[string]any{
		"id":            id,
		"paper":         true,
		"name":          name,
		"questionCount": 2,
		"questions":     []any{},
		"links":         []any{},
		"original": map[string]any{
			"name": origName,
			"type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"data": "data:application/vnd.openxmlformats-officedocument.wordprocessingml.document;base64," + base64.StdEncoding.EncodeToString(origBytes),
		},
	}
	if solName != "" {
		rec["solution"] = map[string]any{
			"name": solName,
			"type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"data": "data:application/vnd.openxmlformats-officedocument.wordprocessingml.document;base64," + base64.StdEncoding.EncodeToString(solBytes),
		}
	}
	return rec
}

func mustPostJSON(t *testing.T, h http.Handler, path string, cookie *http.Cookie, payload any) *httptest.ResponseRecorder {
	t.Helper()
	r := callJSON(h, http.MethodPost, path, cookie, payload)
	if r.Code < 200 || r.Code >= 300 {
		t.Fatalf("POST %s failed: %d %s", path, r.Code, r.Body.String())
	}
	return r
}

func parseLibraryServerJSON(t *testing.T, htmlBytes []byte) map[string]any {
	t.Helper()
	startTag := []byte(`<script type="application/json" id="library-server">`)
	startTagAlt := []byte(`<script id="library-server" type="application/json">`)
	var tail []byte
	var ok bool
	if _, tail, ok = bytes.Cut(htmlBytes, startTag); !ok {
		if _, tail, ok = bytes.Cut(htmlBytes, startTagAlt); !ok {
			t.Fatalf("library-server script tag missing in html: %s", string(htmlBytes))
		}
	}
	raw, _, ok := bytes.Cut(tail, []byte("</script>"))
	if !ok {
		t.Fatalf("closing </script> missing")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invalid library-server json: %v\n%s", err, string(raw))
	}
	return out
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func waitUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(80 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, desc)
}

func decodeSyncJSON(t *testing.T, r *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid sync JSON (code=%d): %v\n%s", r.Code, err, r.Body.String())
	}
	return m
}

func syncStatusString(m map[string]any) string {
	s, _ := m["status"].(string)
	return s
}

func sqliteCount(t *testing.T, l *libraryPage, query string, args ...any) int {
	t.Helper()
	var n int
	if err := l.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q failed: %v", query, err)
	}
	return n
}

func mustReadSyncManifest(t *testing.T, gd *fakeGoogleDrive, rootDir string) map[string]any {
	t.Helper()
	b, ok := gd.ReadPath(filepath.ToSlash(filepath.Join(rootDir, "同步库", "manifest.json")))
	if !ok {
		t.Fatalf("manifest.json missing under %s/同步库/", rootDir)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid manifest.json: %v", err)
	}
	return m
}

// -----------------------------------------------------------------------------
// Tier 1: Feature Coverage Matrix (F1 - F8)
// -----------------------------------------------------------------------------

func TestE2ETier1_F1(t *testing.T) {
	t.Run("T1-F1-01_HomeHydration", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		req := httptest.NewRequest(http.MethodGet, "/biology/", nil)
		req.AddCookie(nas.adminCookie)
		w := httptest.NewRecorder()
		nas.h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /biology/ failed: %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("unexpected content-type: %q", ct)
		}
		srv := parseLibraryServerJSON(t, w.Body.Bytes())
		if accID, _ := srv["accountId"].(string); accID != "" {
			t.Fatalf("expected empty accountId for admin, got %q", accID)
		}
		if ver, _ := srv["version"].(float64); ver != 0 {
			t.Fatalf("expected version 0, got %v", ver)
		}
		if srv["records"] != nil {
			t.Fatalf("expected records:null in home hydration, got %v", srv["records"])
		}
		st, ok := srv["state"].(map[string]any)
		if !ok {
			t.Fatalf("missing state object in library-server: %v", srv)
		}
		if _, ok := st["bag"].([]any); !ok {
			t.Fatalf("expected bag array in state: %v", st)
		}
		if _, ok := st["editions"].([]any); !ok {
			t.Fatalf("expected editions array in state: %v", st)
		}
	})

	t.Run("T1-F1-02_BriefCompactGzipAndCache", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-001", "2026一模", "2026一模.docx", []byte("PK-ORIG"), "2026一模解析.docx", []byte("PK-SOL")))
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-001", "ATP是直接能源物质", "A", "ATP水解供能"))

		req := httptest.NewRequest(http.MethodGet, "/biology/library/records?brief=1&compact=1", nil)
		req.AddCookie(nas.adminCookie)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		nas.h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("expected 200 gzip, got %d encoding=%q", w.Code, w.Header().Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			t.Fatal(err)
		}
		var recs []map[string]any
		if err := json.Unmarshal(plain, &recs); err != nil {
			t.Fatalf("invalid records json: %v", err)
		}
		if len(recs) != 2 {
			t.Fatalf("expected 2 records, got %d", len(recs))
		}
		var sawPaper, sawQ bool
		for _, r := range recs {
			for _, stripped := range []string{"original", "solution", "photos", "file"} {
				if _, exists := r[stripped]; exists {
					t.Fatalf("stripped field %q leaked in /records", stripped)
				}
			}
			if r["id"] == "paper-001" {
				sawPaper = true
				if r["sourceKind"] != "docx" {
					t.Fatalf("expected projected sourceKind=docx, got %v", r["sourceKind"])
				}
			}
			if r["id"] == "q-001" {
				sawQ = true
				qs, _ := r["questions"].([]any)
				if len(qs) != 1 {
					t.Fatalf("expected 1 brief question, got %v", r["questions"])
				}
				qm := qs[0].(map[string]any)
				if qm["answer"] != "1" || qm["a"] != "1" {
					t.Fatalf("expected brief summary flags answer=1 a=1, got %v", qm)
				}
			}
		}
		if !sawPaper || !sawQ {
			t.Fatalf("missing expected records: paper=%v q=%v", sawPaper, sawQ)
		}
		// Second request hits recordsOut cache.
		nas.l.recordsMu.Lock()
		_, cached := nas.l.recordsOut["true|true"]
		nas.l.recordsMu.Unlock()
		if !cached {
			t.Fatal("expected recordsOut cache populated for brief=1&compact=1")
		}
		req2 := httptest.NewRequest(http.MethodGet, "/biology/library/records?brief=1&compact=1", nil)
		req2.AddCookie(nas.adminCookie)
		req2.Header.Set("Accept-Encoding", "gzip")
		w2 := httptest.NewRecorder()
		nas.h.ServeHTTP(w2, req2)
		if w2.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), w2.Body.Bytes()) {
			t.Fatalf("cached gzip response mismatch: code=%d", w2.Code)
		}
	})

	t.Run("T1-F1-03_CompactPicturesServeImmutable", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		onlineHTML := []byte(`<html><head><script data-lib="vue">window.Vue={};</script><script id="paper-data" type="application/json">{"studentHash":"e2e-seed","questions":[]}</script></head><body>function libraryPicture(x){return x}</body></html>`)
		online, err := splitOnlinePage(onlineHTML)
		if err != nil {
			t.Fatal(err)
		}
		nas.l.online = online
		nas.l.page = online.html

		pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 1}
		b64 := base64.StdEncoding.EncodeToString(pngBytes)
		rec := map[string]any{
			"id": "q-pic-1",
			"questions": []map[string]any{
				{
					"id":          "q-pic-1",
					"stem":        "如图细胞膜结构",
					"contentHtml": `<p><img src="data:image/png;base64,` + b64 + `"></p>`,
					"answer":      "B",
					"a":           "磷脂双分子层",
				},
			},
		}
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, rec)

		r := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?compact=1", nas.adminCookie, nil, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("records?compact=1 failed: %d", r.Code)
		}
		re := regexp.MustCompile(`library/picture/([0-9a-f]{64})`)
		m := re.FindStringSubmatch(r.Body.String())
		if len(m) != 2 {
			t.Fatalf("expected library/picture/<64-hex> in compact response, got: %s", r.Body.String())
		}
		if strings.Contains(r.Body.String(), "data:image/png;base64") {
			t.Fatalf("raw base64 image leaked into compact=1 response")
		}
		picRes := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/picture/"+m[1], nas.adminCookie, nil, nil)
		if picRes.Code != http.StatusOK {
			t.Fatalf("GET picture failed: %d", picRes.Code)
		}
		if picRes.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("unexpected image mime: %q", picRes.Header().Get("Content-Type"))
		}
		if !strings.Contains(picRes.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("expected immutable Cache-Control, got %q", picRes.Header().Get("Cache-Control"))
		}
		if !bytes.Equal(picRes.Body.Bytes(), pngBytes) {
			t.Fatalf("picture bytes mismatch: got %x want %x", picRes.Body.Bytes(), pngBytes)
		}
	})

	t.Run("T1-F1-04_FullRecordIncludesOriginalAndSolution", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		orig := []byte("PK-ORIGINAL-DOCX-DATA")
		sol := []byte("PK-SOLUTION-DOCX-DATA")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-001", "期中卷", "期中卷.docx", orig, "期中卷解析.docx", sol))

		r := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/record?id=paper-001", nas.adminCookie, nil, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("GET record?id=paper-001 failed: %d", r.Code)
		}
		var got map[string]any
		if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		oMap, _ := got["original"].(map[string]any)
		sMap, _ := got["solution"].(map[string]any)
		if oMap == nil || oMap["name"] != "期中卷.docx" || !strings.Contains(oMap["data"].(string), base64.StdEncoding.EncodeToString(orig)) {
			t.Fatalf("original attachment missing or corrupted: %v", got["original"])
		}
		if sMap == nil || sMap["name"] != "期中卷解析.docx" || !strings.Contains(sMap["data"].(string), base64.StdEncoding.EncodeToString(sol)) {
			t.Fatalf("solution attachment missing or corrupted: %v", got["solution"])
		}
	})

	t.Run("T1-F1-05_RecordInsertBumpsCache", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		// Prime recordsOut cache before inserting q-new.
		r0 := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?brief=1", nas.adminCookie, nil, nil)
		if r0.Code != http.StatusOK || strings.Contains(r0.Body.String(), "q-new") {
			t.Fatalf("unexpected initial records: %d %s", r0.Code, r0.Body.String())
		}
		ins := mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-new", "光合作用暗反应场所", "C", "叶绿体基质"))
		var who map[string]string
		_ = json.Unmarshal(ins.Body.Bytes(), &who)
		if who["uploadedBy"] != "teacher" || who["uploaderId"] != "admin" {
			t.Fatalf("unexpected uploader metadata: %v", who)
		}
		r1 := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?brief=1", nas.adminCookie, nil, nil)
		if r1.Code != http.StatusOK || !strings.Contains(r1.Body.String(), `"q-new"`) {
			t.Fatalf("expected q-new immediately visible after insert, got %d %s", r1.Code, r1.Body.String())
		}
	})

	t.Run("T1-F1-06_LocalReadsZeroDriveCalls", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "主Drive", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-local-1", "有丝分裂中期", "B", "着丝粒排列在赤道板上"))

		gd.mu.Lock()
		gd.faults.ListStatus = http.StatusInternalServerError
		gd.faults.AboutStatus = http.StatusInternalServerError
		gd.faults.UploadStatus = http.StatusInternalServerError
		gd.mu.Unlock()
		gd.ResetUploadStats()

		start := time.Now()
		for _, p := range []string{"/biology/", "/biology/library/records?brief=1", "/biology/library/record?id=q-local-1"} {
			r := callLibraryAPI(nas.h, http.MethodGet, p, nas.adminCookie, nil, nil)
			if r.Code != http.StatusOK {
				t.Fatalf("local read %s failed while Drive down: %d", p, r.Code)
			}
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("local reads took too long (%v)", elapsed)
		}
		if gd.ListCalls() != 0 || gd.UploadCount() != 0 {
			t.Fatalf("local reads hit Drive unexpectedly: list=%d upload=%d", gd.ListCalls(), gd.UploadCount())
		}
	})
}

func TestE2ETier1_F2(t *testing.T) {
	t.Run("T1-F2-01_DefaultRootDirBiofolioAndRelDir", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "默认Root为空", "", true)

		res := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"connectionId": "",
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/2026一模",
			"filename":     "paper.json",
			"mimeType":     "application/json",
			"overwrite":    true,
		}, []byte(`{"id":"paper-1"}`))
		if res.Code != http.StatusOK {
			t.Fatalf("auto-upload failed: %d %s", res.Code, res.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(res.Body.Bytes(), &out)
		if out["ok"] != true || out["skipped"] != false {
			t.Fatalf("unexpected auto-upload response: %s", res.Body.String())
		}
		entry, _ := out["entry"].(map[string]any)
		if entry == nil || entry["name"] != "paper.json" {
			t.Fatalf("missing entry in response: %s", res.Body.String())
		}
		data, ok := gd.ReadPath("Biofolio/已导入试卷/2026一模/paper.json")
		if !ok || string(data) != `{"id":"paper-1"}` {
			t.Fatalf("file missing at Biofolio/已导入试卷/2026一模/paper.json: ok=%v data=%q", ok, string(data))
		}
	})

	t.Run("T1-F2-02_CustomNestedRootDirPreserved", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connID := connectGoogleDrive(t, nas.h, nas.adminCookie, "自定义目录盘", "教学资料/Biofolio", true)

		res := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"connectionId": connID,
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "周练.docx",
			"mimeType":     docxMimeType,
			"overwrite":    true,
		}, []byte("PK-EXAM-DOCX"))
		if res.Code != http.StatusOK {
			t.Fatalf("auto-upload failed: %d %s", res.Code, res.Body.String())
		}
		data, ok := gd.ReadPath("教学资料/Biofolio/已组试卷/周练.docx")
		if !ok || string(data) != "PK-EXAM-DOCX" {
			t.Fatalf("expected file under 教学资料/Biofolio/已组试卷/周练.docx")
		}
		if _, wrong := gd.ResolvePath("Biofolio/已组试卷"); wrong {
			t.Fatal("must not hardcode top-level Biofolio when rootDir is 教学资料/Biofolio")
		}
	})

	t.Run("T1-F2-03_ReuseExistingFoldersNoDuplicates", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "复用目录盘", "Biofolio", true)

		for _, f := range []struct {
			name string
			mime string
			data string
		}{
			{"p.json", "application/json", `{"name":"高三一模"}`},
			{"p.docx", docxMimeType, "PK-DOCX"},
		} {
			res := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"connectionId": "",
				"providerKind": "gdrive",
				"relDir":       "已导入试卷/高三一模",
				"filename":     f.name,
				"mimeType":     f.mime,
				"overwrite":    true,
			}, []byte(f.data))
			if res.Code != http.StatusOK {
				t.Fatalf("upload %s failed: %d %s", f.name, res.Code, res.Body.String())
			}
		}
		if dups := gd.DuplicateFolders(); len(dups) > 0 {
			t.Fatalf("duplicate folders created: %v", dups)
		}
		if bioNode, ok := gd.ResolvePath("Biofolio"); !ok || gd.CountFolders("root", "Biofolio") != 1 {
			t.Fatalf("expected exactly 1 Biofolio folder, ok=%v", ok)
		} else if impNode, ok := gd.ResolvePath("Biofolio/已导入试卷"); !ok || gd.CountFolders(bioNode.ID, "已导入试卷") != 1 {
			t.Fatalf("expected exactly 1 已导入试卷 folder, ok=%v", ok)
		} else if gd.CountFolders(impNode.ID, "高三一模") != 1 {
			t.Fatalf("expected exactly 1 高三一模 folder")
		}
	})

	t.Run("T1-F2-04_LegacyFolderIDUploadBackwardCompat", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connID := connectGoogleDrive(t, nas.h, nas.adminCookie, "LegacyDrive", "Biofolio", true)

		mk := mustPostJSON(t, nas.h, "/biology/library/cloud/mkdir", nas.adminCookie, map[string]any{
			"connectionId": connID,
			"parentDir":    "",
			"name":         "手工目录",
		})
		var mkOut struct {
			Entry cloudEntry `json:"entry"`
		}
		_ = json.Unmarshal(mk.Body.Bytes(), &mkOut)
		if mkOut.Entry.ID == "" {
			t.Fatalf("mkdir missing entry.id: %s", mk.Body.String())
		}
		meta, _ := json.Marshal(map[string]any{
			"connectionId": connID,
			"dir":          mkOut.Entry.ID,
			"filename":     "note.txt",
			"mimeType":     "text/plain",
			"overwrite":    false,
		})
		up := callLibraryAPI(nas.h, http.MethodPost, "/biology/library/cloud/upload", nas.adminCookie, []byte("hello legacy"), map[string]string{
			"Content-Type": "text/plain",
			"X-Cloud-Meta": base64.StdEncoding.EncodeToString(meta),
		})
		if up.Code != http.StatusOK {
			t.Fatalf("legacy cloud/upload failed: %d %s", up.Code, up.Body.String())
		}
		data, ok := gd.ReadPath("Biofolio/手工目录/note.txt")
		if !ok || string(data) != "hello legacy" {
			t.Fatalf("expected file inside folder ID %s, got ok=%v", mkOut.Entry.ID, ok)
		}
	})

	t.Run("T1-F2-05_BrowseListRootAndSubfolder", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connID := connectGoogleDrive(t, nas.h, nas.adminCookie, "BrowseDrive", "Biofolio", true)

		gd.PutFileAtPath("Biofolio/已导入试卷/卷1/卷1.json", "application/json", []byte(`{}`))
		gd.PutFileAtPath("Biofolio/已组试卷/周练.docx", docxMimeType, []byte("PK"))
		gd.PutFileAtPath("Biofolio/同步库/manifest.json", "application/json", []byte(`{}`))

		lsRoot := mustPostJSON(t, nas.h, "/biology/library/cloud/list", nas.adminCookie, map[string]any{
			"connectionId": connID,
			"dir":          "",
		})
		var rootOut cloudListResult
		_ = json.Unmarshal(lsRoot.Body.Bytes(), &rootOut)
		var importedID string
		names := map[string]bool{}
		for _, e := range rootOut.Entries {
			names[e.Name] = e.IsDir
			if e.Name == "已导入试卷" {
				importedID = e.ID
			}
		}
		for _, want := range []string{"已导入试卷", "已组试卷", "同步库"} {
			if !names[want] {
				t.Fatalf("missing folder %q in root listing: %+v", want, rootOut.Entries)
			}
		}
		lsSub := mustPostJSON(t, nas.h, "/biology/library/cloud/list", nas.adminCookie, map[string]any{
			"connectionId": connID,
			"dir":          importedID,
		})
		var subOut cloudListResult
		_ = json.Unmarshal(lsSub.Body.Bytes(), &subOut)
		if subOut.ParentDir != "" || len(subOut.Entries) != 1 || subOut.Entries[0].Name != "卷1" {
			t.Fatalf("unexpected subfolder listing: %+v", subOut)
		}
	})
}

func TestE2ETier1_F3(t *testing.T) {
	t.Run("T1-F3-01_AutoUploadDefaultGDriveConnection", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connID := connectGoogleDrive(t, nas.h, nas.adminCookie, "DefaultGD", "Biofolio", true)

		res := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"connectionId": "",
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "月考.json",
			"mimeType":     "application/json",
			"overwrite":    true,
		}, []byte(`{"id":"e1"}`))
		if res.Code != http.StatusOK {
			t.Fatalf("auto-upload failed: %d %s", res.Code, res.Body.String())
		}
		var out struct {
			OK           bool       `json:"ok"`
			Skipped      bool       `json:"skipped"`
			ConnectionID string     `json:"connectionId"`
			RelDir       string     `json:"relDir"`
			Entry        cloudEntry `json:"entry"`
		}
		_ = json.Unmarshal(res.Body.Bytes(), &out)
		if !out.OK || out.Skipped || out.ConnectionID != connID || out.Entry.Name != "月考.json" || out.Entry.Size != 11 {
			t.Fatalf("unexpected response: %+v (%s)", out, res.Body.String())
		}
		if b, ok := gd.ReadPath("Biofolio/已组试卷/月考.json"); !ok || string(b) != `{"id":"e1"}` {
			t.Fatalf("uploaded content mismatch: ok=%v", ok)
		}
	})

	t.Run("T1-F3-02_AutoUploadOverwritePatch", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "OverwriteGD", "Biofolio", true)

		r1 := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "月考.json",
			"mimeType":     "application/json",
			"overwrite":    true,
		}, []byte(`{"id":"e1","v":1}`))
		var o1 struct {
			Entry cloudEntry `json:"entry"`
		}
		_ = json.Unmarshal(r1.Body.Bytes(), &o1)

		r2 := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "月考.json",
			"mimeType":     "application/json",
			"overwrite":    true,
		}, []byte(`{"id":"e1","v":2}`))
		var o2 struct {
			Entry cloudEntry `json:"entry"`
		}
		_ = json.Unmarshal(r2.Body.Bytes(), &o2)

		if o1.Entry.ID == "" || o2.Entry.ID != o1.Entry.ID {
			t.Fatalf("expected overwrite to reuse ID %q, got %q", o1.Entry.ID, o2.Entry.ID)
		}
		evs := gd.UploadsFor("月考.json")
		if len(evs) != 2 || evs[1].Method != http.MethodPatch || evs[1].Mode != "media" {
			t.Fatalf("expected second upload to use PATCH media, got %+v", evs)
		}
		if b, ok := gd.ReadPath("Biofolio/已组试卷/月考.json"); !ok || string(b) != `{"id":"e1","v":2}` {
			t.Fatalf("overwritten content mismatch: %q", string(b))
		}
	})

	t.Run("T1-F3-03_AutoUploadNoOverwriteDedupe", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "DedupeGD", "Biofolio", true)

		postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "月考.json",
			"mimeType":     "application/json",
			"overwrite":    false,
		}, []byte("v1"))
		r2 := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "月考.json",
			"mimeType":     "application/json",
			"overwrite":    false,
		}, []byte("v2"))
		var o2 struct {
			Entry cloudEntry `json:"entry"`
		}
		_ = json.Unmarshal(r2.Body.Bytes(), &o2)
		if o2.Entry.Name != "月考 (1).json" {
			t.Fatalf("expected deduped filename 月考 (1).json, got %q", o2.Entry.Name)
		}
		if b1, _ := gd.ReadPath("Biofolio/已组试卷/月考.json"); string(b1) != "v1" {
			t.Fatalf("original file overwritten unexpectedly: %q", string(b1))
		}
		if b2, _ := gd.ReadPath("Biofolio/已组试卷/月考 (1).json"); string(b2) != "v2" {
			t.Fatalf("deduped file missing: %q", string(b2))
		}
	})

	t.Run("T1-F3-04_AutoUploadIgnoresWebDAVDefaultSelectsGDrive", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		dav := newMockWebDAVServer(t, "u", "p")
		mustPostJSON(t, nas.h, "/biology/library/cloud/connections", nas.adminCookie, map[string]any{
			"action":    "save_webdav",
			"name":      "默认WebDAV",
			"baseUrl":   dav.srv.URL + "/dav",
			"username":  "u",
			"password":  "p",
			"isDefault": true,
		})
		gdConnID := connectGoogleDrive(t, nas.h, nas.adminCookie, "非默认GDrive", "Biofolio", false)

		res := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"connectionId": "",
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "选GDrive.json",
			"mimeType":     "application/json",
			"overwrite":    true,
		}, []byte(`{"ok":1}`))
		if res.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d %s", res.Code, res.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(res.Body.Bytes(), &out)
		if out["skipped"] != false || out["connectionId"] != gdConnID {
			t.Fatalf("expected gdrive connection %q selected, got %s", gdConnID, res.Body.String())
		}
		if _, ok := gd.ReadPath("Biofolio/已组试卷/选GDrive.json"); !ok {
			t.Fatal("file missing from Google Drive")
		}
	})

	t.Run("T1-F3-05_AutoUploadExplicitConnectionID", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		_ = connectGoogleDrive(t, nas.h, nas.adminCookie, "GD-1", "Root1", true)
		conn2 := connectGoogleDrive(t, nas.h, nas.adminCookie, "GD-2", "Root2", false)

		res := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"connectionId": conn2,
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/卷A",
			"filename":     "卷A.docx",
			"mimeType":     docxMimeType,
			"overwrite":    true,
		}, []byte("DOCX-A"))
		if res.Code != http.StatusOK {
			t.Fatalf("explicit connection upload failed: %d %s", res.Code, res.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(res.Body.Bytes(), &out)
		if out["connectionId"] != conn2 {
			t.Fatalf("expected connectionId %q, got %v", conn2, out["connectionId"])
		}
		if _, ok := gd.ReadPath("Root2/已导入试卷/卷A/卷A.docx"); !ok {
			t.Fatal("expected file in Root2")
		}
		if _, wrong := gd.ResolvePath("Root1/已导入试卷"); wrong {
			t.Fatal("must not upload to Root1 when conn2 explicitly requested")
		}
	})
}

func TestE2ETier1_F4(t *testing.T) {
	t.Run("T1-F4-04_PaperImportWithCompanionSolutionAndDatasetJSON", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "ImportGD", "Biofolio", true)

		orig := []byte("PK\x03\x04ORIGINAL-DOCX")
		sol := []byte("PK\x03\x04SOLUTION-DOCX")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-imp-1", "2026高三生物一模", "2026高三生物一模.docx", orig, "2026高三生物一模解析.docx", sol))
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("paper-imp-1-1", "酶的作用机理", "D", "降低活化能"))

		datasetJSON, _ := json.Marshal(map[string]any{
			"id":         "paper-imp-1",
			"name":       "2026高三生物一模",
			"sourceKind": "docx",
			"questions": []map[string]any{
				{"id": "paper-imp-1-1", "stem": "酶的作用机理", "answer": "D", "a": "降低活化能"},
			},
		})
		for _, f := range []struct {
			name string
			mime string
			data []byte
		}{
			{"2026高三生物一模.json", "application/json", datasetJSON},
			{"2026高三生物一模.docx", docxMimeType, orig},
			{"2026高三生物一模解析.docx", docxMimeType, sol},
		} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已导入试卷/2026高三生物一模",
				"filename":     f.name,
				"mimeType":     f.mime,
				"overwrite":    true,
			}, f.data)
			if r.Code != http.StatusOK {
				t.Fatalf("auto-upload %s failed: %d %s", f.name, r.Code, r.Body.String())
			}
		}

		gotJSON, ok := gd.ReadPath("Biofolio/已导入试卷/2026高三生物一模/2026高三生物一模.json")
		if !ok {
			t.Fatal("missing dataset json in Drive")
		}
		var pkg map[string]any
		if err := json.Unmarshal(gotJSON, &pkg); err != nil {
			t.Fatal(err)
		}
		if pkg["id"] != "paper-imp-1" || pkg["sourceKind"] != "docx" {
			t.Fatalf("unexpected dataset json: %v", pkg)
		}
		if gotOrig, ok := gd.ReadPath("Biofolio/已导入试卷/2026高三生物一模/2026高三生物一模.docx"); !ok || !bytes.Equal(gotOrig, orig) {
			t.Fatal("original docx mismatch in Drive")
		}
		if gotSol, ok := gd.ReadPath("Biofolio/已导入试卷/2026高三生物一模/2026高三生物一模解析.docx"); !ok || !bytes.Equal(gotSol, sol) {
			t.Fatal("companion solution docx mismatch in Drive")
		}
	})

	t.Run("T1-F4-02_PDFPaperImportSourceKindAndAutoUpload", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "ImportPDFGD", "Biofolio", true)

		pdfBytes := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
		// Intentionally omit "sourceKind" to verify SQL projection infers "pdf" from original.name.
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, map[string]any{
			"id":            "paper-pdf-1",
			"paper":         true,
			"name":          "2026高三生物二模",
			"questionCount": 1,
			"questions":     []any{},
			"links":         []any{},
			"original": map[string]any{
				"name": "2026高三生物二模.pdf",
				"type": pdfMimeType,
				"data": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdfBytes),
			},
		})

		briefRes := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?brief=1&compact=1", nas.adminCookie, nil, nil)
		if briefRes.Code != http.StatusOK {
			t.Fatalf("records?brief=1&compact=1 failed: %d %s", briefRes.Code, briefRes.Body.String())
		}
		var recs []map[string]any
		if err := json.Unmarshal(briefRes.Body.Bytes(), &recs); err != nil {
			t.Fatal(err)
		}
		var foundBrief map[string]any
		for _, r := range recs {
			if r["id"] == "paper-pdf-1" {
				foundBrief = r
				break
			}
		}
		if foundBrief == nil || foundBrief["sourceKind"] != "pdf" {
			t.Fatalf("expected projected sourceKind=pdf in brief list, got %+v", foundBrief)
		}
		if _, leaked := foundBrief["original"]; leaked {
			t.Fatalf("original attachment leaked into brief list: %+v", foundBrief)
		}

		fullRes := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/record?id=paper-pdf-1", nas.adminCookie, nil, nil)
		if fullRes.Code != http.StatusOK {
			t.Fatalf("GET record failed: %d", fullRes.Code)
		}
		var fullRec map[string]any
		if err := json.Unmarshal(fullRes.Body.Bytes(), &fullRec); err != nil {
			t.Fatal(err)
		}
		origObj, _ := fullRec["original"].(map[string]any)
		if origObj == nil || origObj["name"] != "2026高三生物二模.pdf" || !strings.Contains(fmt.Sprint(origObj["data"]), base64.StdEncoding.EncodeToString(pdfBytes)) {
			t.Fatalf("full paper record missing PDF original: %+v", fullRec["original"])
		}

		datasetJSON, _ := json.Marshal(map[string]any{
			"id":         "paper-pdf-1",
			"name":       "2026高三生物二模",
			"sourceKind": "pdf",
			"questions":  []any{},
		})
		for _, f := range []struct {
			name string
			mime string
			data []byte
		}{
			{"2026高三生物二模.json", "application/json", datasetJSON},
			{"2026高三生物二模.pdf", pdfMimeType, pdfBytes},
		} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已导入试卷/2026高三生物二模",
				"filename":     f.name,
				"mimeType":     f.mime,
				"overwrite":    true,
			}, f.data)
			if r.Code != http.StatusOK {
				t.Fatalf("auto-upload %s failed: %d %s", f.name, r.Code, r.Body.String())
			}
		}
		if gotJSON, ok := gd.ReadPath("Biofolio/已导入试卷/2026高三生物二模/2026高三生物二模.json"); !ok || !bytes.Equal(gotJSON, datasetJSON) {
			t.Fatalf("PDF paper dataset json mismatch, ok=%v", ok)
		}
		if gotPDF, ok := gd.ReadPath("Biofolio/已导入试卷/2026高三生物二模/2026高三生物二模.pdf"); !ok || !bytes.Equal(gotPDF, pdfBytes) {
			t.Fatalf("PDF original mismatch, ok=%v", ok)
		}
		if len(gd.ListDir("Biofolio/已导入试卷/2026高三生物二模")) != 2 {
			t.Fatalf("expected 2 files in PDF paper dir, got %d", len(gd.ListDir("Biofolio/已导入试卷/2026高三生物二模")))
		}
		if dups := gd.DuplicateFolders(); len(dups) > 0 {
			t.Fatalf("duplicate folders detected: %v", dups)
		}
	})

	t.Run("T1-F4-03_JSONDatasetPackageImportRoundTrip", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connID := connectGoogleDrive(t, nas.h, nas.adminCookie, "PkgGD", "Biofolio", true)

		pkgV1, _ := json.Marshal(map[string]any{
			"id":         "paper-pkg-1",
			"name":       "2026数据包卷",
			"sourceKind": "docx",
			"questions": []map[string]any{
				{"id": "paper-pkg-1-1", "stem": "包内第1题", "answer": "A", "a": "解析1"},
				{"id": "paper-pkg-1-2", "stem": "包内第2题", "answer": "C", "a": "解析2"},
			},
		})
		up1 := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/2026数据包卷",
			"filename":     "2026数据包卷.json",
			"mimeType":     "application/json",
			"overwrite":    true,
		}, pkgV1)
		if up1.Code != http.StatusOK {
			t.Fatalf("initial dataset json upload failed: %d %s", up1.Code, up1.Body.String())
		}

		listRes := callJSON(nas.h, http.MethodPost, "/biology/library/cloud/list", nas.adminCookie, map[string]any{
			"connectionId": connID,
			"dir":          "rel:已导入试卷/2026数据包卷",
		})
		if listRes.Code != http.StatusOK {
			t.Fatalf("cloud/list failed: %d %s", listRes.Code, listRes.Body.String())
		}
		var listOut cloudListResult
		if err := json.Unmarshal(listRes.Body.Bytes(), &listOut); err != nil {
			t.Fatal(err)
		}
		var jsonFileID string
		for _, e := range listOut.Entries {
			if e.Name == "2026数据包卷.json" && !e.IsDir {
				jsonFileID = e.ID
				break
			}
		}
		if jsonFileID == "" {
			t.Fatalf("2026数据包卷.json missing from cloud/list: %+v", listOut.Entries)
		}

		dlRes := callJSON(nas.h, http.MethodPost, "/biology/library/cloud/download", nas.adminCookie, map[string]any{
			"connectionId": connID,
			"fileId":       jsonFileID,
		})
		if dlRes.Code != http.StatusOK {
			t.Fatalf("cloud/download failed: %d %s", dlRes.Code, dlRes.Body.String())
		}
		var downloadedPkg struct {
			ID         string           `json:"id"`
			Name       string           `json:"name"`
			SourceKind string           `json:"sourceKind"`
			Questions  []map[string]any `json:"questions"`
		}
		if err := json.Unmarshal(dlRes.Body.Bytes(), &downloadedPkg); err != nil {
			t.Fatalf("invalid downloaded package json: %v", err)
		}
		if downloadedPkg.ID != "paper-pkg-1" || len(downloadedPkg.Questions) != 2 {
			t.Fatalf("unexpected downloaded package: %+v", downloadedPkg)
		}

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, map[string]any{
			"id":            downloadedPkg.ID,
			"paper":         true,
			"sourceKind":    downloadedPkg.SourceKind,
			"name":          downloadedPkg.Name,
			"questionCount": len(downloadedPkg.Questions),
			"questions":     []any{},
			"links":         []any{},
		})
		for _, q := range downloadedPkg.Questions {
			qid, _ := q["id"].(string)
			mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord(qid, fmt.Sprint(q["stem"]), fmt.Sprint(q["answer"]), fmt.Sprint(q["a"])))
		}
		if cnt := sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records"); cnt != 3 {
			t.Fatalf("expected 3 records after package import, got %d", cnt)
		}
		briefRes := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?brief=1", nas.adminCookie, nil, nil)
		for _, wantID := range []string{"paper-pkg-1", "paper-pkg-1-1", "paper-pkg-1-2"} {
			if !strings.Contains(briefRes.Body.String(), wantID) {
				t.Fatalf("records?brief=1 missing %s", wantID)
			}
		}

		pkgV2, _ := json.Marshal(map[string]any{"id": "paper-pkg-1", "name": "2026数据包卷", "version": 2, "questions": downloadedPkg.Questions})
		up2 := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/2026数据包卷",
			"filename":     "2026数据包卷.json",
			"mimeType":     "application/json",
			"overwrite":    true,
		}, pkgV2)
		if up2.Code != http.StatusOK {
			t.Fatalf("overwrite dataset json failed: %d %s", up2.Code, up2.Body.String())
		}
		evs := gd.UploadsFor("2026数据包卷.json")
		if len(evs) != 2 || evs[1].Method != http.MethodPatch {
			t.Fatalf("expected 2nd upload to PATCH existing file, got %+v", evs)
		}
		if got, ok := gd.ReadPath("Biofolio/已导入试卷/2026数据包卷/2026数据包卷.json"); !ok || !bytes.Equal(got, pkgV2) {
			t.Fatalf("expected updated pkgV2 in Drive, ok=%v", ok)
		}
	})

	t.Run("T1-F4-05_PaperShellThenIncrementalQuestionConfirmAndAutoUpload", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "IncGD", "Biofolio", true)

		orig := []byte("PK\x03\x04INC-DOCX")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-inc-1", "试卷增量", "试卷增量.docx", orig, "", nil))
		q1 := makeValidQuestionRecord("paper-inc-1-1", "增量第1题", "A", "析1")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, q1)

		// Re-posting already confirmed row 1 must return 409 Conflict without overwriting.
		dup := callJSON(nas.h, http.MethodPost, "/biology/library/record", nas.adminCookie, q1)
		if dup.Code != http.StatusConflict {
			t.Fatalf("expected 409 on duplicate row confirm, got %d %s", dup.Code, dup.Body.String())
		}

		q2 := makeValidQuestionRecord("paper-inc-1-2", "增量第2题", "B", "析2")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, q2)

		pkgV1, _ := json.Marshal(map[string]any{"id": "paper-inc-1", "confirmed": []string{"paper-inc-1-1"}})
		pkgV2, _ := json.Marshal(map[string]any{"id": "paper-inc-1", "confirmed": []string{"paper-inc-1-1", "paper-inc-1-2"}})
		for _, f := range []struct {
			name string
			mime string
			data []byte
		}{
			{"试卷增量.json", "application/json", pkgV1},
			{"试卷增量.docx", docxMimeType, orig},
			{"试卷增量.json", "application/json", pkgV2},
		} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已导入试卷/试卷增量",
				"filename":     f.name,
				"mimeType":     f.mime,
				"overwrite":    true,
			}, f.data)
			if r.Code != http.StatusOK {
				t.Fatalf("auto-upload %s failed: %d %s", f.name, r.Code, r.Body.String())
			}
		}

		if cnt := sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records"); cnt != 3 {
			t.Fatalf("expected 3 records in SQLite, got %d", cnt)
		}
		briefRes := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?brief=1", nas.adminCookie, nil, nil)
		for _, wantID := range []string{"paper-inc-1", "paper-inc-1-1", "paper-inc-1-2"} {
			if !strings.Contains(briefRes.Body.String(), wantID) {
				t.Fatalf("records?brief=1 missing %s", wantID)
			}
		}
		if len(gd.ListDir("Biofolio/已导入试卷/试卷增量")) != 2 {
			t.Fatalf("expected 2 files in 已导入试卷/试卷增量, got %d", len(gd.ListDir("Biofolio/已导入试卷/试卷增量")))
		}
		if got, ok := gd.ReadPath("Biofolio/已导入试卷/试卷增量/试卷增量.json"); !ok || !bytes.Equal(got, pkgV2) {
			t.Fatalf("expected pkgV2 in Drive, ok=%v", ok)
		}
	})

	t.Run("T1-F4-06_PhotosPaperImportAndAutoUpload", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "PhotosGD", "Biofolio", true)

		png1 := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 1, 1}
		png2 := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 1, 2}
		// Omit "sourceKind" to verify SQL projection infers "photos" from json_array_length($.photos)>0.
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, map[string]any{
			"id":            "paper-photo-1",
			"paper":         true,
			"name":          "照片导入卷",
			"questionCount": 2,
			"original":      nil,
			"photos": []map[string]any{
				{"name": "p1.png", "type": "image/png", "data": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png1)},
				{"name": "p2.png", "type": "image/png", "data": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png2)},
			},
			"questions": []any{},
			"links":     []any{},
		})

		briefRes := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?brief=1&compact=1", nas.adminCookie, nil, nil)
		var recs []map[string]any
		if err := json.Unmarshal(briefRes.Body.Bytes(), &recs); err != nil {
			t.Fatal(err)
		}
		var foundBrief map[string]any
		for _, r := range recs {
			if r["id"] == "paper-photo-1" {
				foundBrief = r
				break
			}
		}
		if foundBrief == nil || foundBrief["sourceKind"] != "photos" {
			t.Fatalf("expected projected sourceKind=photos, got %+v", foundBrief)
		}
		if _, leaked := foundBrief["photos"]; leaked {
			t.Fatalf("photos array leaked into brief list: %+v", foundBrief)
		}

		fullRes := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/record?id=paper-photo-1", nas.adminCookie, nil, nil)
		var fullRec map[string]any
		if err := json.Unmarshal(fullRes.Body.Bytes(), &fullRec); err != nil {
			t.Fatal(err)
		}
		photos, _ := fullRec["photos"].([]any)
		if len(photos) != 2 {
			t.Fatalf("expected 2 photos in full record, got %v", fullRec["photos"])
		}

		pkgBytes, _ := json.Marshal(map[string]any{"id": "paper-photo-1", "name": "照片导入卷", "sourceKind": "photos", "questions": []any{}})
		for _, f := range []struct {
			name string
			mime string
			data []byte
		}{
			{"照片导入卷.json", "application/json", pkgBytes},
			{"p1.png", "image/png", png1},
		} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已导入试卷/照片导入卷",
				"filename":     f.name,
				"mimeType":     f.mime,
				"overwrite":    true,
			}, f.data)
			if r.Code != http.StatusOK {
				t.Fatalf("auto-upload %s failed: %d %s", f.name, r.Code, r.Body.String())
			}
		}
		if len(gd.ListDir("Biofolio/已导入试卷/照片导入卷")) != 2 {
			t.Fatalf("expected 2 files in 照片导入卷 dir")
		}
		if got, ok := gd.ReadPath("Biofolio/已导入试卷/照片导入卷/p1.png"); !ok || !bytes.Equal(got, png1) {
			t.Fatalf("p1.png bytes mismatch, ok=%v", ok)
		}
	})
}

func TestE2ETier1_F5(t *testing.T) {
	t.Run("T1-F5-04_ExamAssemblyDocxAndConfigAutoUpload", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "ExamGD", "Biofolio", true)

		edition := map[string]any{
			"id":      "ed-midterm",
			"isExam":  true,
			"purpose": "期中考试",
			"name":    "期中考试",
			"ids":     []string{"q1", "q2"},
		}
		mustPostJSON(t, nas.h, "/biology/library/personal", nas.adminCookie, map[string]any{
			"bag":      []string{},
			"editions": []any{edition},
		})
		cfgBytes, _ := json.Marshal(edition)
		docxV1 := []byte("PK\x03\x04DOCX-V1")
		docxV2 := []byte("PK\x03\x04DOCX-V2")

		for _, up := range []struct {
			name string
			mime string
			data []byte
		}{
			{"期中考试（学生版）.docx", docxMimeType, docxV1},
			{"期中考试.json", "application/json", cfgBytes},
			{"期中考试（学生版）.docx", docxMimeType, docxV2},
		} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已组试卷",
				"filename":     up.name,
				"mimeType":     up.mime,
				"overwrite":    true,
			}, up.data)
			if r.Code != http.StatusOK {
				t.Fatalf("auto-upload %s failed: %d %s", up.name, r.Code, r.Body.String())
			}
		}
		if b, ok := gd.ReadPath("Biofolio/已组试卷/期中考试（学生版）.docx"); !ok || !bytes.Equal(b, docxV2) {
			t.Fatalf("expected overwritten docxV2, ok=%v", ok)
		}
		if b, ok := gd.ReadPath("Biofolio/已组试卷/期中考试.json"); !ok || !bytes.Equal(b, cfgBytes) {
			t.Fatalf("expected exam config json, ok=%v", ok)
		}
		if len(gd.ListDir("Biofolio/已组试卷")) != 2 {
			t.Fatalf("expected exactly 2 files in 已组试卷/, got %d", len(gd.ListDir("Biofolio/已组试卷")))
		}
	})
}

func TestE2ETier1_F6(t *testing.T) {
	t.Run("T1-F6-01_SingleQuestionAddDebouncedSplitSync", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		// Initial baseline push including a paper record so we can verify papers/* isn't re-uploaded.
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-base", "基础卷", "基础.docx", []byte("PK-BASE"), "", nil))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		gd.ResetUploadStats()

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-sync-1", "ATP分子结构", "A", "腺苷三磷酸"))
		waitUntil(t, 15*time.Second, "debounced split sync manifest update", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		if len(gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")) > 0 {
			t.Fatalf("single question add unexpectedly re-uploaded papers/* shards: %+v", gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/"))
		}
		recUploads := gd.UploadsMatchingPrefix("/Biofolio/同步库/records/")
		if len(recUploads) != 1 {
			t.Fatalf("expected exactly 1 bucket upload for single question add, got %d: %+v", len(recUploads), recUploads)
		}
		lastUp, ok := gd.LastUpload()
		if !ok || lastUp.Name != "manifest.json" {
			t.Fatalf("manifest.json must be uploaded last, got ok=%v last=%+v", ok, lastUp)
		}
	})

	t.Run("T1-F6-02_RevisionAndStateFixesCoalescedSync", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		q := makeValidQuestionRecord("q-sync-1", "原题干", "A", "原解析")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, q)
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		gd.ResetUploadStats()

		qObj := q["questions"].([]map[string]any)[0]
		qNew := map[string]any{"id": "q-sync-1", "stem": "修订题干", "answer": "B", "a": "修订解析"}
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, map[string]any{
			"id":       "revision:q-sync-1:1",
			"before":   qObj,
			"question": qNew,
		})
		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{
			"version": 0,
			"state":   map[string]any{"fixes": map[string]any{"q-sync-1": map[string]any{"answer": "B"}}},
		})
		waitUntil(t, 15*time.Second, "coalesced revision+state sync", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		if n := len(gd.UploadsFor("manifest.json")); n != 1 {
			t.Fatalf("expected 1 coalesced sync cycle (1 manifest upload), got %d", n)
		}
		if len(gd.UploadsFor("state.json")) != 1 {
			t.Fatalf("expected state.json uploaded")
		}
	})

	t.Run("T1-F6-03_StateDeleteAndPurgeSync", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-sync-1", "删题测试", "A", "解析"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		m1 := mustReadSyncManifest(t, gd, "Biofolio")
		v1, _ := m1["syncVersion"].(float64)

		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{
			"version": 0,
			"state":   map[string]any{"deletedQuestions": []string{"q-sync-1"}},
		})
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		m2 := mustReadSyncManifest(t, gd, "Biofolio")
		v2, _ := m2["syncVersion"].(float64)
		if v2 <= v1 {
			t.Fatalf("expected monotonic syncVersion > %v, got %v", v1, v2)
		}
		stBytes, ok := gd.ReadPath("Biofolio/同步库/state.json")
		if !ok || !strings.Contains(string(stBytes), "q-sync-1") {
			t.Fatalf("state.json missing deletedQuestions: ok=%v", ok)
		}
	})

	t.Run("T1-F6-04_FavoritesSyncBothAdminAndTeacher", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-sync-1", "收藏测试", "A", "解析"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		gd.ResetUploadStats()

		// Admin favorite
		mustPostJSON(t, nas.h, "/biology/library/favorite", nas.adminCookie, map[string]any{"question": "q-sync-1", "selected": true})
		waitUntil(t, 15*time.Second, "admin favorite sync", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		if _, ok := gd.ReadPath("Biofolio/同步库/favorites/__admin__.json"); !ok {
			t.Fatal("missing Biofolio/同步库/favorites/__admin__.json")
		}
		gd.ResetUploadStats()

		// Teacher m1 favorite (must also trigger sync, lifting v0.9.4 id=="" gate!)
		mustPostJSON(t, nas.h, "/biology/library/favorite", nas.liCookie, map[string]any{"question": "q-sync-1", "selected": true})
		waitUntil(t, 15*time.Second, "teacher m1 favorite sync", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		if _, ok := gd.ReadPath("Biofolio/同步库/favorites/m1.json"); !ok {
			t.Fatal("missing Biofolio/同步库/favorites/m1.json")
		}
	})

	t.Run("T1-F6-05_TeachingNoteAndReviewDraftSync", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{
			"version": 0,
			"state": map[string]any{
				"fixes": map[string]any{
					"q-1": map[string]any{"teachingNote": "讲评要点：强调细胞呼吸第一阶段场所"},
				},
			},
		})
		mustPostJSON(t, nas.h, "/biology/library/review-draft", nas.adminCookie, map[string]any{
			"paper":   "paper-1",
			"version": 1,
			"kind":    "import",
			"items":   []map[string]any{{"n": 1, "stem": "草稿题"}},
		})
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		st, ok := gd.ReadPath("Biofolio/同步库/state.json")
		if !ok || !strings.Contains(string(st), "讲评要点") {
			t.Fatalf("state.json missing teachingNote: ok=%v", ok)
		}
		drafts := gd.ListDir("Biofolio/同步库/drafts")
		if len(drafts) != 1 || !strings.Contains(string(drafts[0].Data), "paper-1") {
			t.Fatalf("expected 1 draft shard in 同步库/drafts/, got %+v", drafts)
		}
	})

	t.Run("T1-F6-06_PersonalBagEditionsSync", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-p1", "原卷", "原卷.docx", []byte("PK-HEAVY"), "", nil))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		gd.ResetUploadStats()

		mustPostJSON(t, nas.h, "/biology/library/personal", nas.adminCookie, map[string]any{
			"bag":      []string{"q-sync-1"},
			"editions": []map[string]any{{"id": "ed-1", "name": "测试卷", "ids": []string{"q-sync-1"}}},
		})
		waitUntil(t, 15*time.Second, "personal bag/editions sync", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		if b, ok := gd.ReadPath("Biofolio/同步库/personal/__admin__.json"); !ok || !strings.Contains(string(b), "ed-1") {
			t.Fatalf("personal/__admin__.json missing ed-1: ok=%v", ok)
		}
		if len(gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")) > 0 || len(gd.UploadsMatchingPrefix("/Biofolio/同步库/records/")) > 0 {
			t.Fatal("personal change must not re-upload papers/* or records/* shards")
		}
	})
}

func TestE2ETier1_F7(t *testing.T) {
	t.Run("T1-F7-01_StartupCheckAndInSyncStatus", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-71", "题1", "A", "析1"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		m := decodeSyncJSON(t, chk)
		if m["configured"] != true || syncStatusString(m) != "in_sync" {
			t.Fatalf("expected configured=true status=in_sync, got %s", chk.Body.String())
		}
		rem, _ := m["remote"].(map[string]any)
		if rem["checked"] != true || rem["exists"] != true || rem["empty"] != false {
			t.Fatalf("unexpected remote metadata: %v", rem)
		}
		st := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/cloud/sync/status", nas.adminCookie, nil, nil)
		if st.Code != http.StatusOK || syncStatusString(decodeSyncJSON(t, st)) != "in_sync" {
			t.Fatalf("GET sync/status mismatch: %d %s", st.Code, st.Body.String())
		}
	})

	t.Run("T1-F7-02_RemoteAheadDetected", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-72", "题2", "B", "析2"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		m := mustReadSyncManifest(t, gd, "Biofolio")
		m["syncVersion"] = float64(99)
		b, _ := json.Marshal(m)
		gd.PutFileAtPath("Biofolio/同步库/manifest.json", "application/json", b)

		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		out := decodeSyncJSON(t, chk)
		if syncStatusString(out) != "remote_ahead" {
			t.Fatalf("expected remote_ahead, got %s", chk.Body.String())
		}
	})

	t.Run("T1-F7-03_LocalAheadDetected", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-73a", "题3a", "A", "析"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		// Perform local edit without pushing yet.
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-73b", "题3b", "B", "析"))
		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		out := decodeSyncJSON(t, chk)
		if syncStatusString(out) != "local_ahead" {
			t.Fatalf("expected local_ahead, got %s", chk.Body.String())
		}
	})

	t.Run("T1-F7-04_EmptyRemotePopulatedLocalSafety", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-74", "保护题", "C", "析"))

		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		out := decodeSyncJSON(t, chk)
		rem, _ := out["remote"].(map[string]any)
		if syncStatusString(out) != "remote_empty_local_populated" || rem["empty"] != true || rem["exists"] != false {
			t.Fatalf("expected remote_empty_local_populated, got %s", chk.Body.String())
		}
		if sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records") != 1 {
			t.Fatal("local SQLite record was unexpectedly wiped!")
		}
	})

	t.Run("T1-F7-05_SyncPushInitializesEmptyRemote", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-75", "初始上传题", "D", "析"))

		push := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		if push.Code != http.StatusOK {
			t.Fatalf("sync/push failed: %d", push.Code)
		}
		_ = mustReadSyncManifest(t, gd, "Biofolio")
		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chk)) != "in_sync" {
			t.Fatalf("expected in_sync after push, got %s", chk.Body.String())
		}
	})
}

func TestE2ETier1_F8(t *testing.T) {
	t.Run("T1-F8-01_RestoreOntoFreshEmptyDB", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "SharedGD", "Biofolio", true)

		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidPaperRecord("paper-81", "恢复测试卷", "恢复.docx", []byte("PK-81"), "", nil))
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-81", "基因分离定律", "A", "等位基因分离"))
		mustPostJSON(t, nasA.h, "/biology/library/state", nasA.adminCookie, map[string]any{
			"version": 0,
			"state":   map[string]any{"fixes": map[string]any{"q-81": map[string]any{"teachingNote": "重点"}}},
		})
		mustPostJSON(t, nasA.h, "/biology/library/favorite", nasA.adminCookie, map[string]any{"question": "q-81", "selected": true})
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.adminCookie, map[string]any{
			"bag":      []string{"q-81"},
			"editions": []map[string]any{{"id": "ed-81", "name": "恢复组卷", "ids": []string{"q-81"}}},
		})
		mustPostJSON(t, nasA.h, "/biology/library/review-draft", nasA.adminCookie, map[string]any{
			"paper":   "paper-81",
			"version": 1,
			"items":   []any{},
		})
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		// Boot fresh NAS-B pointing to the same fakeGoogleDrive.
		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "SharedGD", "Biofolio", true)

		rest := mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})
		if rest.Code != http.StatusOK {
			t.Fatalf("restore failed: %d %s", rest.Code, rest.Body.String())
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records") != 2 {
			t.Fatalf("expected 2 restored records, got %d", sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records"))
		}
		recRes := callLibraryAPI(nasB.h, http.MethodGet, "/biology/library/records?brief=1", nasB.adminCookie, nil, nil)
		if !strings.Contains(recRes.Body.String(), "q-81") || !strings.Contains(recRes.Body.String(), "paper-81") {
			t.Fatalf("records cache not refreshed on NAS-B: %s", recRes.Body.String())
		}
		home := callLibraryAPI(nasB.h, http.MethodGet, "/biology/", nasB.adminCookie, nil, nil)
		srv := parseLibraryServerJSON(t, home.Body.Bytes())
		st := srv["state"].(map[string]any)
		if !strings.Contains(fmt.Sprint(st["good"]), "q-81") || !strings.Contains(fmt.Sprint(st["editions"]), "ed-81") {
			t.Fatalf("state/favorites/editions not restored on NAS-B: %v", st)
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_review_drafts WHERE paper='paper-81'") != 1 {
			t.Fatal("review_drafts not restored")
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_member_ids") == 0 {
			t.Fatal("member_ids not restored")
		}
		_ = gd
	})

	t.Run("T1-F8-02_RestoreMergeOntoNonEmptyDB", func(t *testing.T) {
		_, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "SharedGD", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-remote", "远端题", "A", "解析"))
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.adminCookie, map[string]any{
			"bag":      []string{},
			"editions": []map[string]any{{"id": "ed-remote", "name": "远端卷", "ids": []string{"q-remote"}}},
		})
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "SharedGD", "Biofolio", true)
		mustPostJSON(t, nasB.h, "/biology/library/record", nasB.adminCookie, makeValidQuestionRecord("q-local", "本地题", "B", "解析"))

		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records WHERE id IN ('q-local','q-remote')") != 2 {
			t.Fatal("merge restore lost q-local or failed to import q-remote")
		}
		var pBody string
		_ = nasB.l.db.QueryRow("SELECT body FROM library_personal WHERE account=''").Scan(&pBody)
		if !strings.Contains(pBody, "ed-remote") {
			t.Fatalf("merge restore missing ed-remote: %s", pBody)
		}
	})

	t.Run("T1-F8-03_RestoreOverwriteOntoNonEmptyDB", func(t *testing.T) {
		_, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "SharedGD", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-remote", "远端新题", "A", "解析"))
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "SharedGD", "Biofolio", true)
		mustPostJSON(t, nasB.h, "/biology/library/record", nasB.adminCookie, makeValidQuestionRecord("q-stale", "旧题", "C", "旧析"))

		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "overwrite"})
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records WHERE id='q-stale'") != 0 {
			t.Fatal("overwrite restore did not replace stale record")
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records WHERE id='q-remote'") != 1 {
			t.Fatal("overwrite restore missing q-remote")
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_cloud_connections") != 1 {
			t.Fatal("overwrite restore wiped library_cloud_connections!")
		}
	})

	t.Run("T1-F8-04_RestorePreservesCloudConnectionsAndCredentials", func(t *testing.T) {
		_, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "SharedGD", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-84", "凭据保留题", "A", "析"))
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "NASB-GD", "Biofolio", true)
		dav := newMockWebDAVServer(t, "u", "p")
		mustPostJSON(t, nasB.h, "/biology/library/cloud/connections", nasB.adminCookie, map[string]any{
			"action":   "save_webdav",
			"name":     "NASB-DAV",
			"baseUrl":  dav.srv.URL + "/dav",
			"username": "u",
			"password": "p",
		})

		snapFiles := map[string]string{}
		for _, fn := range []string{"library-account.json", "cloud-secret.key", "cloud-settings.json"} {
			b, err := os.ReadFile(filepath.Join(nasB.l.dir, fn))
			if err != nil {
				t.Fatalf("missing %s before restore: %v", fn, err)
			}
			snapFiles[fn] = sha256Hex(b)
		}
		type connRowSnap struct {
			id, account, kind, name, rootDir, secretHex string
		}
		readConns := func() []connRowSnap {
			rows, err := nasB.l.db.Query("SELECT id, account, kind, name, root_dir, hex(secret) FROM library_cloud_connections ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []connRowSnap
			for rows.Next() {
				var r connRowSnap
				_ = rows.Scan(&r.id, &r.account, &r.kind, &r.name, &r.rootDir, &r.secretHex)
				out = append(out, r)
			}
			return out
		}
		beforeConns := readConns()
		if len(beforeConns) != 2 {
			t.Fatalf("expected 2 connections before restore, got %d", len(beforeConns))
		}

		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "overwrite"})

		for fn, wantHash := range snapFiles {
			b, err := os.ReadFile(filepath.Join(nasB.l.dir, fn))
			if err != nil || sha256Hex(b) != wantHash {
				t.Fatalf("%s modified or lost after restore: err=%v", fn, err)
			}
		}
		afterConns := readConns()
		if len(afterConns) != len(beforeConns) {
			t.Fatalf("connection count changed: before=%d after=%d", len(beforeConns), len(afterConns))
		}
		for i := range beforeConns {
			if beforeConns[i] != afterConns[i] {
				t.Fatalf("connection row %d altered after restore: before=%+v after=%+v", i, beforeConns[i], afterConns[i])
			}
		}
	})

	t.Run("T1-F8-05_PostRestoreStatusInSyncAndIncrementalPushWorks", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "SharedGD", "Biofolio", true)

		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidPaperRecord("paper-85", "恢复增量卷", "恢复增量.docx", []byte("PK-85-HEAVY"), "", nil))
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-85a", "恢复后基线题", "A", "析a"))
		mustPostJSON(t, nasA.h, "/biology/library/favorite", nasA.adminCookie, map[string]any{"question": "q-85a", "selected": true})
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.adminCookie, map[string]any{
			"bag":      []string{"q-85a"},
			"editions": []map[string]any{{"id": "ed-85", "name": "恢复组卷", "ids": []string{"q-85a"}}},
		})
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "SharedGD", "Biofolio", true)

		chkBefore := mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/check", nasB.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chkBefore)) != "remote_ahead" {
			t.Fatalf("expected remote_ahead on NAS-B before restore, got %s", chkBefore.Body.String())
		}
		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})

		for _, ep := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/biology/library/cloud/sync/status"},
			{http.MethodPost, "/biology/library/cloud/sync/check"},
		} {
			res := callJSON(nasB.h, ep.method, ep.path, nasB.adminCookie, nil)
			if res.Code != http.StatusOK {
				t.Fatalf("%s %s failed: %d %s", ep.method, ep.path, res.Code, res.Body.String())
			}
			m := decodeSyncJSON(t, res)
			loc, _ := m["local"].(map[string]any)
			rem, _ := m["remote"].(map[string]any)
			lastErr, errIsStr := m["lastSyncError"].(string)
			if m["configured"] != true || m["dirty"] != false || syncStatusString(m) != "in_sync" ||
				!errIsStr || strings.TrimSpace(lastErr) != "" ||
				loc["recordCount"].(float64) != 2 || rem["recordCount"].(float64) != 2 {
				t.Fatalf("unexpected post-restore status from %s %s: %s", ep.method, ep.path, res.Body.String())
			}
		}

		// Incremental edit + push on restored NAS-B must skip unchanged papers/* shard and sync cleanly to NAS-A.
		mustPostJSON(t, nasB.h, "/biology/library/record", nasB.adminCookie, makeValidQuestionRecord("q-85b", "恢复后新增题", "B", "析b"))
		mustPostJSON(t, nasB.h, "/biology/library/favorite", nasB.adminCookie, map[string]any{"question": "q-85b", "selected": true})
		gd.ResetUploadStats()
		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/push", nasB.adminCookie, nil)

		if len(gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")) != 0 {
			t.Fatalf("post-restore incremental push needlessly re-uploaded papers/* shard")
		}
		chkB := mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/check", nasB.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chkB)) != "in_sync" {
			t.Fatalf("expected NAS-B in_sync after incremental push, got %s", chkB.Body.String())
		}
		chkA := mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/check", nasA.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chkA)) != "remote_ahead" {
			t.Fatalf("expected NAS-A remote_ahead after NAS-B push, got %s", chkA.Body.String())
		}
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/restore", nasA.adminCookie, map[string]any{"mode": "merge"})
		if cnt := sqliteCount(t, nasA.l, "SELECT COUNT(*) FROM library_records"); cnt != 3 {
			t.Fatalf("expected 3 records on NAS-A after merge restore, got %d", cnt)
		}
	})
}

// -----------------------------------------------------------------------------
// Tier 2: Boundary & Corner Cases (F1 - F8)
// -----------------------------------------------------------------------------

func TestE2ETier2_F1(t *testing.T) {
	t.Run("T2-F1-01_UnauthenticatedReturns401", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		for _, path := range []string{"/biology/library/records", "/biology/library/record?id=x", "/biology/library/cloud/config"} {
			r := callLibraryAPI(nas.h, http.MethodGet, path, nil, nil, nil)
			if r.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 for %s, got %d", path, r.Code)
			}
		}
	})

	t.Run("T2-F1-02_MissingHeaderOrNonJSONReturns403", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		// Missing X-Biology-Library: 1
		req := httptest.NewRequest(http.MethodPost, "/biology/library/state", strings.NewReader(`{"version":0,"state":{}}`))
		req.AddCookie(nas.adminCookie)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		nas.h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 without X-Biology-Library, got %d", w.Code)
		}
		// Non-JSON Content-Type
		r2 := callLibraryAPI(nas.h, http.MethodPost, "/biology/library/state", nas.adminCookie, []byte(`{"version":0,"state":{}}`), map[string]string{
			"Content-Type": "text/plain",
		})
		if r2.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for text/plain Content-Type, got %d", r2.Code)
		}
	})

	t.Run("T2-F1-03_MissingRecordAndInvalidPictureReturn404", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		if r := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/record?id=does-not-exist", nas.adminCookie, nil, nil); r.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for missing record, got %d", r.Code)
		}
		if r := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/picture/0000", nas.adminCookie, nil, nil); r.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for invalid picture, got %d", r.Code)
		}
	})

	t.Run("T2-F1-04_CompleteIntakeRejectsIncompleteQuestion", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		r1 := callJSON(nas.h, http.MethodPost, "/biology/library/record", nas.adminCookie, map[string]any{
			"id":        "q-bad-1",
			"questions": []map[string]any{{"id": "q-bad-1", "stem": "题干", "answer": "待补答案", "a": ""}},
		})
		if r1.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for placeholder answer, got %d %s", r1.Code, r1.Body.String())
		}
		r2 := callJSON(nas.h, http.MethodPost, "/biology/library/record", nas.adminCookie, map[string]any{
			"id":        "q-bad-2",
			"questions": []map[string]any{{"id": "q-bad-2", "stem": "photo.jpg", "contentHtml": "<img src='x'>", "answer": "A", "a": "解析"}},
		})
		if r2.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for untranscribed photo stem, got %d", r2.Code)
		}
	})

	t.Run("T2-F1-05_StaleVersionReturns409", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{"version": 0, "state": map[string]any{"aliases": map[string]string{"a": "b"}}})
		staleState := callJSON(nas.h, http.MethodPost, "/biology/library/state", nas.adminCookie, map[string]any{"version": 0, "state": map[string]any{}})
		if staleState.Code != http.StatusConflict {
			t.Fatalf("expected 409 on stale state version, got %d", staleState.Code)
		}
		mustPostJSON(t, nas.h, "/biology/library/review-draft", nas.adminCookie, map[string]any{"paper": "paper-v1", "version": 1})
		staleDraft := callJSON(nas.h, http.MethodPost, "/biology/library/review-draft", nas.adminCookie, map[string]any{"paper": "paper-v1", "version": 1})
		if staleDraft.Code != http.StatusConflict {
			t.Fatalf("expected 409 on duplicate draft version, got %d", staleDraft.Code)
		}
	})

	t.Run("T2-F1-06_NonAdminForbiddenOnAdminEndpoints", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		if r := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/backup", nas.liCookie, nil, nil); r.Code != http.StatusForbidden {
			t.Fatalf("expected 403 on backup for non-admin, got %d", r.Code)
		}
		if r := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/offline", nas.liCookie, nil, nil); r.Code != http.StatusForbidden {
			t.Fatalf("expected 403 on offline for non-admin, got %d", r.Code)
		}
		if r := callJSON(nas.h, http.MethodPost, "/biology/library/restore", nas.liCookie, nil); r.Code != http.StatusForbidden {
			t.Fatalf("expected 403 on restore for non-admin, got %d", r.Code)
		}
	})
}

func TestE2ETier2_F2(t *testing.T) {
	t.Run("T2-F2-01_PathTraversalRejected", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		for _, bad := range []string{"已导入试卷/../../etc", "../同步库"} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       bad,
				"filename":     "x.json",
				"mimeType":     "application/json",
			}, []byte(`{}`))
			if r.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for traversal relDir %q, got %d %s", bad, r.Code, r.Body.String())
			}
		}
		if gd.UploadCount() != 0 || len(gd.ListDir("")) != 0 {
			t.Fatal("path traversal created files/folders on Drive")
		}
	})

	t.Run("T2-F2-02_ControlCharsAndNullBytesRejected", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		for _, tc := range []struct {
			relDir, filename string
		}{
			{"已导入试卷/卷\x00A", "a.json"},
			{"已导入试卷/卷\nB", "a.json"},
			{"已导入试卷/卷C", "a\rb.json"},
		} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       tc.relDir,
				"filename":     tc.filename,
				"mimeType":     "application/json",
			}, []byte(`{}`))
			if r.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for control chars (%q, %q), got %d", tc.relDir, tc.filename, r.Code)
			}
		}
	})

	t.Run("T2-F2-03_SpecialCharsEscapedInDriveQuery", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		seg := "2026年'生物'专题_A_B_C_D_E_F_G"
		for _, fn := range []string{"1.json", "2.json"} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已导入试卷/" + seg,
				"filename":     fn,
				"mimeType":     "application/json",
				"overwrite":    true,
			}, []byte(`{}`))
			if r.Code != http.StatusOK {
				t.Fatalf("special-char relDir upload failed: %d %s", r.Code, r.Body.String())
			}
		}
		if dups := gd.DuplicateFolders(); len(dups) > 0 {
			t.Fatalf("duplicate folder created on special-char segment: %v", dups)
		}
		if _, ok := gd.ReadPath("Biofolio/已导入试卷/" + seg + "/2.json"); !ok {
			t.Fatal("special-char folder upload missing")
		}
	})

	t.Run("T2-F2-04_LongUnicodeEmojiSegment", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		longSeg := strings.Repeat("生物一模🧬", 25)
		r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/" + longSeg,
			"filename":     "emoji.json",
			"mimeType":     "application/json",
		}, []byte(`{"ok":true}`))
		if r.Code != http.StatusOK && r.Code != http.StatusBadRequest {
			t.Fatalf("unexpected status %d for long Unicode segment: %s", r.Code, r.Body.String())
		}
	})

	t.Run("T2-F2-05_ConcurrentAutoUploadsZeroDuplicateFolders", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "ConcurrentGD", "Biofolio", true)

		var wg sync.WaitGroup
		errs := make(chan string, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
					"providerKind": "gdrive",
					"relDir":       "已导入试卷/并发试卷",
					"filename":     fmt.Sprintf("f-%d.json", idx),
					"mimeType":     "application/json",
					"overwrite":    true,
				}, []byte(fmt.Sprintf(`{"i":%d}`, idx)))
				if r.Code != http.StatusOK {
					errs <- fmt.Sprintf("goroutine %d failed: %d %s", idx, r.Code, r.Body.String())
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
		if dups := gd.DuplicateFolders(); len(dups) > 0 {
			t.Fatalf("concurrent uploads created duplicate folders: %v", dups)
		}
		if len(gd.ListDir("Biofolio/已导入试卷/并发试卷")) != 8 {
			t.Fatalf("expected 8 files in 并发试卷, got %d", len(gd.ListDir("Biofolio/已导入试卷/并发试卷")))
		}
	})

	t.Run("T2-F2-06_SlashesAndBackslashesNormalized", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       `///已导入试卷//卷A//`,
			"filename":     "norm.json",
			"mimeType":     "application/json",
		}, []byte(`{}`))
		if r.Code != http.StatusOK {
			t.Fatalf("expected 200 on normalized relDir, got %d %s", r.Code, r.Body.String())
		}
		if _, ok := gd.ReadPath("Biofolio/已导入试卷/卷A/norm.json"); !ok {
			t.Fatal("normalized path missing in Drive")
		}
		// Backslashes in relDir are rejected cleanly with 400.
		rBad := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       `已导入试卷\卷A`,
			"filename":     "bad.json",
			"mimeType":     "application/json",
		}, []byte(`{}`))
		if rBad.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on backslash relDir, got %d %s", rBad.Code, rBad.Body.String())
		}
	})
}

func TestE2ETier2_F3(t *testing.T) {
	t.Run("T2-F3-01_UnconfiguredGDriveGracefulSkip", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		// li has 0 gdrive connections.
		r := postAutoUpload(nas.h, nas.liCookie, map[string]any{
			"connectionId": "",
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/卷1",
			"filename":     "卷1.json",
			"mimeType":     "application/json",
		}, []byte(`{}`))
		if r.Code != http.StatusOK {
			t.Fatalf("expected 200 graceful skip, got %d %s", r.Code, r.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(r.Body.Bytes(), &out)
		if out["ok"] != true || out["skipped"] != true || out["reason"] != "no_gdrive_connection" {
			t.Fatalf("unexpected skip response: %s", r.Body.String())
		}
	})

	t.Run("T2-F3-02_InvalidHeadersOrMeta", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		// Missing X-Cloud-Meta -> 400
		if r := callLibraryAPI(nas.h, http.MethodPost, "/biology/library/cloud/auto-upload", nas.adminCookie, []byte("x"), nil); r.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 without X-Cloud-Meta, got %d", r.Code)
		}
		// Malformed base64 -> 400
		if r := callLibraryAPI(nas.h, http.MethodPost, "/biology/library/cloud/auto-upload", nas.adminCookie, []byte("x"), map[string]string{"X-Cloud-Meta": "!!!"}); r.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid base64, got %d", r.Code)
		}
		// Missing filename -> 400
		if r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{"relDir": "已组试卷", "filename": ""}, []byte("x")); r.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for empty filename, got %d", r.Code)
		}
		// Missing X-Biology-Library: 1 -> 403
		req := httptest.NewRequest(http.MethodPost, "/biology/library/cloud/auto-upload", strings.NewReader("x"))
		req.AddCookie(nas.adminCookie)
		w := httptest.NewRecorder()
		nas.h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 without X-Biology-Library, got %d", w.Code)
		}
	})

	t.Run("T2-F3-03_ResumableUploadOver4MiBAnd9MiB", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		for _, tc := range []struct {
			name string
			size int
		}{
			{"over4m.bin", (4 << 20) + 1},
			{"multi9m.bin", 9 << 20},
		} {
			payload := bytes.Repeat([]byte("AB12"), tc.size/4+1)[:tc.size]
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已导入试卷/大文件",
				"filename":     tc.name,
				"mimeType":     "application/octet-stream",
				"overwrite":    true,
			}, payload)
			if r.Code != http.StatusOK {
				t.Fatalf("resumable upload %s failed: %d %s", tc.name, r.Code, r.Body.String())
			}
			evs := gd.UploadsFor(tc.name)
			if len(evs) != 1 || evs[0].Mode != "resumable" {
				t.Fatalf("expected resumable mode for %s, got %+v", tc.name, evs)
			}
			got, ok := gd.ReadPath("Biofolio/已导入试卷/大文件/" + tc.name)
			if !ok || sha256Hex(got) != sha256Hex(payload) {
				t.Fatalf("resumable payload checksum mismatch for %s (ok=%v len=%d)", tc.name, ok, len(got))
			}
		}
	})

	t.Run("T2-F3-04_Exceeding128MiBReturns413", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connID := connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		rawMeta, _ := json.Marshal(map[string]any{
			"connectionId": connID,
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/超大",
			"filename":     "huge.bin",
			"mimeType":     "application/octet-stream",
		})
		// Stream a 128 MiB + 1 byte reader without allocating 128 MiB twice.
		hugeReader := io.LimitReader(zeroReader{}, maxCloudFileBytes+1)
		req := httptest.NewRequest(http.MethodPost, "/biology/library/cloud/auto-upload", hugeReader)
		req.AddCookie(nas.adminCookie)
		req.Header.Set("X-Biology-Library", "1")
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Cloud-Meta", base64.StdEncoding.EncodeToString(rawMeta))
		w := httptest.NewRecorder()
		nas.h.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413 for >128 MiB body, got %d %s", w.Code, w.Body.String())
		}
		if gd.UploadCount() != 0 {
			t.Fatal("oversized payload must not upload to Drive")
		}
	})

	t.Run("T2-F3-05_CrossAccountConnectionIDRejected", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		adminConnID := connectGoogleDrive(t, nas.h, nas.adminCookie, "AdminGD", "Biofolio", true)

		r := postAutoUpload(nas.h, nas.liCookie, map[string]any{
			"connectionId": adminConnID,
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "hijack.json",
			"mimeType":     "application/json",
		}, []byte(`{}`))
		if r.Code != http.StatusNotFound && r.Code != http.StatusBadRequest {
			t.Fatalf("expected 404/400 when teacher uses admin connectionId, got %d %s", r.Code, r.Body.String())
		}
	})

	t.Run("T2-F3-06_ExpiredOAuthTokenAutoRefreshed", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connID := connectGoogleDrive(t, nas.h, nas.adminCookie, "ExpiryGD", "Biofolio", true)

		row, plain, err := nas.l.getCloudConnection("admin", connID)
		if err != nil {
			t.Fatal(err)
		}
		var sec gdriveSecret
		_ = json.Unmarshal(plain, &sec)
		sec.ExpiryUnix = 1
		secBytes, _ := json.Marshal(sec)
		if err := nas.l.updateCloudConnectionSecret(row.Account, row.ID, secBytes, sec.Email); err != nil {
			t.Fatal(err)
		}

		r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"connectionId": connID,
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "refreshed.json",
			"mimeType":     "application/json",
		}, []byte(`{"ok":1}`))
		if r.Code != http.StatusOK || gd.RefreshCount() == 0 {
			t.Fatalf("expected token refresh + 200 OK, got code=%d refreshes=%d body=%s", r.Code, gd.RefreshCount(), r.Body.String())
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestE2ETier2_F4(t *testing.T) {
	t.Run("T2-F4-01_PaperImportMissingOriginalJSONOnly", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, map[string]any{
			"id":         "paper-json-only",
			"paper":      true,
			"sourceKind": "photos",
			"name":       "纯图导入卷",
			"questions":  []any{},
			"links":      []any{},
		})
		r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/纯图导入卷",
			"filename":     "纯图导入卷.json",
			"mimeType":     "application/json",
		}, []byte(`{"id":"paper-json-only","questions":[]}`))
		if r.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", r.Code)
		}
		if len(gd.ListDir("Biofolio/已导入试卷/纯图导入卷")) != 1 {
			t.Fatal("expected only .json when no original docx/pdf exists")
		}
	})

	t.Run("T2-F4-02_ZeroByteUploadHandledSafely", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/空文件",
			"filename":     "empty.txt",
			"mimeType":     "text/plain",
		}, []byte{})
		if r.Code != http.StatusOK {
			t.Fatalf("expected 0-byte upload to succeed cleanly without panic, got %d %s", r.Code, r.Body.String())
		}
	})

	t.Run("T2-F4-03_SpecialCharsInPaperNameRelDir", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		sanitized := "高三(1)班：细胞呼吸_光合'专题'"
		r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/" + sanitized,
			"filename":     sanitized + ".json",
			"mimeType":     "application/json",
		}, []byte(`{"name":"ok"}`))
		if r.Code != http.StatusOK {
			t.Fatalf("expected 200 for special char paper name, got %d %s", r.Code, r.Body.String())
		}
		if _, ok := gd.ReadPath("Biofolio/已导入试卷/" + sanitized + "/" + sanitized + ".json"); !ok {
			t.Fatal("special-char paper file missing in Drive")
		}
	})

	t.Run("T2-F4-06_DuplicatePaperRecord409AndOverwriteAutoUpload", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		paper := makeValidPaperRecord("paper-dup", "重复卷", "重复.docx", []byte("PK"), "", nil)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, paper)
		dup := callJSON(nas.h, http.MethodPost, "/biology/library/record", nas.adminCookie, paper)
		if dup.Code != http.StatusConflict {
			t.Fatalf("expected 409 on duplicate paper id, got %d", dup.Code)
		}
		postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive", "relDir": "已导入试卷/重复卷", "filename": "重复卷.json", "mimeType": "application/json", "overwrite": true,
		}, []byte(`{"v":1}`))
		postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive", "relDir": "已导入试卷/重复卷", "filename": "重复卷.json", "mimeType": "application/json", "overwrite": true,
		}, []byte(`{"v":2}`))
		if len(gd.ListDir("Biofolio/已导入试卷/重复卷")) != 1 {
			t.Fatal("expected overwrite to keep single file")
		}
	})

	t.Run("T2-F4-04_DriveOutageDuringPaperSavePreservesLocalSQLite", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		gd.mu.Lock()
		gd.faults.UploadStatus = http.StatusServiceUnavailable
		gd.mu.Unlock()
		t.Cleanup(func() {
			gd.mu.Lock()
			gd.faults.UploadStatus = 0
			gd.mu.Unlock()
		})

		orig := []byte("PK\x03\x04OUTAGE-DOCX")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-outage-1", "断网卷", "断网卷.docx", orig, "", nil))
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("paper-outage-1-1", "断网保存题", "A", "解析"))

		for _, f := range []struct {
			name string
			mime string
			data []byte
		}{
			{"断网卷.json", "application/json", []byte(`{"id":"paper-outage-1"}`)},
			{"断网卷.docx", docxMimeType, orig},
		} {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已导入试卷/断网卷",
				"filename":     f.name,
				"mimeType":     f.mime,
				"overwrite":    true,
			}, f.data)
			if r.Code != http.StatusBadGateway {
				t.Fatalf("expected 502 Bad Gateway during Drive 503 outage for %s, got %d %s", f.name, r.Code, r.Body.String())
			}
		}

		if gd.UploadCount() != 0 {
			t.Fatalf("expected 0 completed uploads during 503 outage, got %d", gd.UploadCount())
		}
		if cnt := sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records WHERE id IN ('paper-outage-1','paper-outage-1-1')"); cnt != 2 {
			t.Fatalf("expected local SQLite records preserved on Drive outage, got %d", cnt)
		}
		if getPaper := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/record?id=paper-outage-1", nas.adminCookie, nil, nil); getPaper.Code != http.StatusOK {
			t.Fatalf("local GET record failed after Drive outage: %d", getPaper.Code)
		}

		gd.mu.Lock()
		gd.faults.UploadStatus = 0
		gd.mu.Unlock()
		retry := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已导入试卷/断网卷",
			"filename":     "断网卷.json",
			"mimeType":     "application/json",
			"overwrite":    true,
		}, []byte(`{"id":"paper-outage-1"}`))
		if retry.Code != http.StatusOK {
			t.Fatalf("retry auto-upload after Drive recovery failed: %d %s", retry.Code, retry.Body.String())
		}
		if _, ok := gd.ReadPath("Biofolio/已导入试卷/断网卷/断网卷.json"); !ok {
			t.Fatal("missing 断网卷.json after Drive recovery")
		}
	})

	t.Run("T2-F4-05_MalformedDatasetJSONRejectedBeforeRecordWrite", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		badPayloads := []string{
			"{not-json",
			"{}",
			`{"id":"q-bad-ds","questions":[{"id":"q-bad-ds","stem":"题干","answer":"待补答案","a":""}]}`,
			`{"id":"rev-bad","before":{"id":"q1"},"question":{"id":"q2"}}`,
		}
		for _, body := range badPayloads {
			r := callJSON(nas.h, http.MethodPost, "/biology/library/record", nas.adminCookie, body)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request for malformed record %s, got %d %s", body, r.Code, r.Body.String())
			}
		}
		if cnt := sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records"); cnt != 0 {
			t.Fatalf("expected 0 records in SQLite after rejected payloads, got %d", cnt)
		}
		if gd.UploadCount() != 0 {
			t.Fatalf("expected 0 Drive uploads for rejected payloads, got %d", gd.UploadCount())
		}
	})
}

func TestE2ETier2_F5(t *testing.T) {
	t.Run("T2-F5-04_LargeDocxExamOver4MiBResumable", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		bigDocx := append([]byte("PK\x03\x04"), bytes.Repeat([]byte("Z"), (4<<20)+4096)...)
		r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "高三高清配图卷（学生版）.docx",
			"mimeType":     docxMimeType,
			"overwrite":    true,
		}, bigDocx)
		if r.Code != http.StatusOK {
			t.Fatalf("large docx auto-upload failed: %d %s", r.Code, r.Body.String())
		}
		evs := gd.UploadsFor("高三高清配图卷（学生版）.docx")
		if len(evs) != 1 || evs[0].Mode != "resumable" {
			t.Fatalf("expected resumable upload for >4 MiB docx, got %+v", evs)
		}
		if got, ok := gd.ReadPath("Biofolio/已组试卷/高三高清配图卷（学生版）.docx"); !ok || sha256Hex(got) != sha256Hex(bigDocx) {
			t.Fatal("large docx bytes corrupted on Drive")
		}
	})

	t.Run("T2-F5-05_DoubleExamAutoUploadOverwritesWithoutDuplicates", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		ed := map[string]any{"id": "ed-505", "isExam": true, "name": "2026期中", "ids": []string{"q1"}}
		mustPostJSON(t, nas.h, "/biology/library/personal", nas.adminCookie, map[string]any{
			"bag":      []string{},
			"editions": []any{ed},
		})
		cfgV1 := []byte(`{"id":"ed-505","v":1}`)
		cfgV2 := []byte(`{"id":"ed-505","v":2}`)
		docxV1 := []byte("PK\x03\x04EXAM-505-V1")
		docxV2 := []byte("PK\x03\x04EXAM-505-V2")

		uploadExam := func(name, mime string, data []byte) string {
			r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
				"providerKind": "gdrive",
				"relDir":       "已组试卷",
				"filename":     name,
				"mimeType":     mime,
				"overwrite":    true,
			}, data)
			if r.Code != http.StatusOK {
				t.Fatalf("auto-upload %s failed: %d %s", name, r.Code, r.Body.String())
			}
			var out struct {
				Entry struct {
					ID string `json:"id"`
				} `json:"entry"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || out.Entry.ID == "" {
				t.Fatalf("missing entry.id in auto-upload response: %s", r.Body.String())
			}
			return out.Entry.ID
		}

		docxID1 := uploadExam("2026期中（学生版）.docx", docxMimeType, docxV1)
		cfgID1 := uploadExam("2026期中.json", "application/json", cfgV1)
		docxID2 := uploadExam("2026期中（学生版）.docx", docxMimeType, docxV2)
		cfgID2 := uploadExam("2026期中.json", "application/json", cfgV2)

		if docxID1 != docxID2 || cfgID1 != cfgID2 {
			t.Fatalf("expected overwrite to reuse file IDs: docx=(%s,%s) cfg=(%s,%s)", docxID1, docxID2, cfgID1, cfgID2)
		}
		docxEvs := gd.UploadsFor("2026期中（学生版）.docx")
		cfgEvs := gd.UploadsFor("2026期中.json")
		if len(docxEvs) != 2 || docxEvs[1].Method != http.MethodPatch {
			t.Fatalf("expected 2nd docx upload to PATCH, got %+v", docxEvs)
		}
		if len(cfgEvs) != 2 || cfgEvs[1].Method != http.MethodPatch {
			t.Fatalf("expected 2nd json upload to PATCH, got %+v", cfgEvs)
		}
		if len(gd.ListDir("Biofolio/已组试卷")) != 2 {
			t.Fatalf("expected 2 files in 已组试卷/, got %d", len(gd.ListDir("Biofolio/已组试卷")))
		}
		if dups := gd.DuplicateFolders(); len(dups) > 0 {
			t.Fatalf("duplicate folders detected: %v", dups)
		}
		if got, ok := gd.ReadPath("Biofolio/已组试卷/2026期中（学生版）.docx"); !ok || !bytes.Equal(got, docxV2) {
			t.Fatalf("expected docxV2 bytes, ok=%v", ok)
		}
		if got, ok := gd.ReadPath("Biofolio/已组试卷/2026期中.json"); !ok || !bytes.Equal(got, cfgV2) {
			t.Fatalf("expected cfgV2 bytes, ok=%v", ok)
		}
	})

	t.Run("T2-F5-06_ExamAutoUploadMissingFilenameRejected", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		dav := newMockWebDAVServer(t, "u", "p")
		davRes := mustPostJSON(t, nas.h, "/biology/library/cloud/connections", nas.adminCookie, map[string]any{
			"action":   "save_webdav",
			"name":     "AdminDAV",
			"baseUrl":  dav.srv.URL + "/dav",
			"username": "u",
			"password": "p",
		})
		var davOut struct {
			Connections []cloudConnectionView `json:"connections"`
		}
		_ = json.Unmarshal(davRes.Body.Bytes(), &davOut)
		var webdavConnID string
		for _, c := range davOut.Connections {
			if c.Kind == "webdav" {
				webdavConnID = c.ID
				break
			}
		}
		if webdavConnID == "" {
			t.Fatalf("expected webdav connection ID in %s", davRes.Body.String())
		}

		badMetas := []map[string]any{
			{"providerKind": "gdrive", "relDir": "已组试卷", "filename": ""},
			{"providerKind": "gdrive", "relDir": "已组试卷", "filename": "."},
			{"providerKind": "gdrive", "relDir": "已组试卷", "filename": ".."},
			{"providerKind": "gdrive", "relDir": "已组试卷", "filename": "sub/exam.docx"},
			{"providerKind": "gdrive", "relDir": "已组试卷/../secret", "filename": "exam.docx"},
			{"connectionId": webdavConnID, "providerKind": "gdrive", "relDir": "已组试卷", "filename": "exam.docx"},
		}
		for _, meta := range badMetas {
			r := postAutoUpload(nas.h, nas.adminCookie, meta, []byte("PK-BAD"))
			if r.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request for meta %+v, got %d %s", meta, r.Code, r.Body.String())
			}
		}
		if gd.UploadCount() != 0 {
			t.Fatalf("expected 0 Drive uploads for invalid exam metas, got %d", gd.UploadCount())
		}
		if len(gd.ListDir("Biofolio/已组试卷")) != 0 {
			t.Fatalf("expected empty 已组试卷/ dir, got %d", len(gd.ListDir("Biofolio/已组试卷")))
		}
	})

	t.Run("T2-F5-07_ExamAutoUploadUnconfiguredDriveSkipsCleanly", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		dav := newMockWebDAVServer(t, "u", "p")
		mustPostJSON(t, nas.h, "/biology/library/cloud/connections", nas.liCookie, map[string]any{
			"action":    "save_webdav",
			"name":      "LiDefaultDAV",
			"baseUrl":   dav.srv.URL + "/dav",
			"username":  "u",
			"password":  "p",
			"isDefault": true,
		})

		ed := map[string]any{"id": "ed-li-507", "isExam": true, "name": "周练", "ids": []string{"q1"}}
		mustPostJSON(t, nas.h, "/biology/library/personal", nas.liCookie, map[string]any{
			"bag":      []string{},
			"editions": []any{ed},
		})
		cfgBytes, _ := json.Marshal(ed)

		for _, f := range []struct {
			name string
			mime string
			data []byte
		}{
			{"周练（学生版）.docx", docxMimeType, []byte("PK-LI-DOCX")},
			{"周练.json", "application/json", cfgBytes},
		} {
			r := postAutoUpload(nas.h, nas.liCookie, map[string]any{
				"connectionId": "",
				"providerKind": "gdrive",
				"relDir":       "已组试卷",
				"filename":     f.name,
				"mimeType":     f.mime,
				"overwrite":    true,
			}, f.data)
			if r.Code != http.StatusOK {
				t.Fatalf("expected 200 skip for unconfigured Drive (%s), got %d %s", f.name, r.Code, r.Body.String())
			}
			out := decodeSyncJSON(t, r)
			if out["ok"] != true || out["skipped"] != true || out["reason"] != "no_gdrive_connection" {
				t.Fatalf("unexpected skip payload for %s: %s", f.name, r.Body.String())
			}
		}
		if gd.UploadCount() != 0 {
			t.Fatalf("expected 0 Drive uploads for unconfigured li, got %d", gd.UploadCount())
		}
		home := callLibraryAPI(nas.h, http.MethodGet, "/biology/", nas.liCookie, nil, nil)
		srv := parseLibraryServerJSON(t, home.Body.Bytes())
		if !strings.Contains(fmt.Sprint(srv["state"]), "ed-li-507") {
			t.Fatalf("saved edition ed-li-507 missing from li state: %v", srv["state"])
		}
	})

	t.Run("T2-F5-08_ExamAutoUploadDriveFaultPreservesSavedEdition", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		ed := map[string]any{"id": "ed-508", "isExam": true, "name": "故障保护卷", "ids": []string{"q1"}}
		mustPostJSON(t, nas.h, "/biology/library/personal", nas.adminCookie, map[string]any{
			"bag":      []string{},
			"editions": []any{ed},
		})

		gd.mu.Lock()
		gd.faults.UploadStatus = http.StatusServiceUnavailable
		gd.mu.Unlock()
		t.Cleanup(func() {
			gd.mu.Lock()
			gd.faults.UploadStatus = 0
			gd.mu.Unlock()
		})

		r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive",
			"relDir":       "已组试卷",
			"filename":     "故障保护卷（学生版）.docx",
			"mimeType":     docxMimeType,
			"overwrite":    true,
		}, []byte("PK-508"))
		if r.Code != http.StatusBadGateway {
			t.Fatalf("expected 502 Bad Gateway on Drive 503 outage, got %d %s", r.Code, r.Body.String())
		}
		var pBody string
		if err := nas.l.db.QueryRow("SELECT body FROM library_personal WHERE account=''").Scan(&pBody); err != nil || !strings.Contains(pBody, "ed-508") {
			t.Fatalf("library_personal lost saved edition ed-508: err=%v body=%s", err, pBody)
		}
		home := callLibraryAPI(nas.h, http.MethodGet, "/biology/", nas.adminCookie, nil, nil)
		srv := parseLibraryServerJSON(t, home.Body.Bytes())
		if !strings.Contains(fmt.Sprint(srv["state"]), "ed-508") {
			t.Fatalf("GET /biology/ state missing ed-508 after Drive fault: %v", srv["state"])
		}
	})
}

func TestE2ETier2_F6(t *testing.T) {
	t.Run("T2-F6-01_HeavyAttachmentShardSkippedOnIncrementalEdit", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		heavyOrig := bytes.Repeat([]byte("P"), 5<<20)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-heavy", "重型试卷", "重型.docx", heavyOrig, "", nil))
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-h1", "小编辑题", "A", "解析"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		if len(gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")) != 1 {
			t.Fatalf("expected 1 initial paper shard upload, got %d", len(gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")))
		}
		gd.ResetUploadStats()

		// Edit teachingNote + favorite -> flush sync -> heavy paper shard MUST NOT re-upload!
		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{
			"version": 0,
			"state":   map[string]any{"fixes": map[string]any{"q-h1": map[string]any{"teachingNote": "新讲评"}}},
		})
		mustPostJSON(t, nas.h, "/biology/library/favorite", nas.adminCookie, map[string]any{"question": "q-h1", "selected": true})
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		if len(gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")) != 0 {
			t.Fatal("heavy paper shard was needlessly re-uploaded on teachingNote/favorite edit")
		}
		if gd.TotalUploadedBytes() >= 100<<10 {
			t.Fatalf("expected tiny delta (<100 KB), got %d bytes", gd.TotalUploadedBytes())
		}
	})

	t.Run("T2-F6-02_DebounceCoalescingBurstMutations", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-burst", "突发编辑", "A", "析"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		gd.ResetUploadStats()

		ver := 0
		for i := 0; i < 5; i++ {
			mustPostJSON(t, nas.h, "/biology/library/favorite", nas.adminCookie, map[string]any{"question": "q-burst", "selected": i%2 == 0})
			r := mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{
				"version": ver,
				"state":   map[string]any{"fixes": map[string]any{"q-burst": map[string]any{"teachingNote": fmt.Sprintf("v-%d", i)}}},
			})
			_ = json.Unmarshal(r.Body.Bytes(), &ver)
		}
		waitUntil(t, 15*time.Second, "consolidated burst sync", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		if n := len(gd.UploadsFor("manifest.json")); n != 1 {
			t.Fatalf("expected 1 consolidated sync cycle, got %d manifest uploads", n)
		}
	})

	t.Run("T2-F6-03_TransientRemoteFailurePreservesDirtyAndRecovers", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-retry", "重试题", "B", "析"))
		gd.mu.Lock()
		gd.faults.UploadStatus = http.StatusServiceUnavailable
		gd.mu.Unlock()

		failPush := callJSON(nas.h, http.MethodPost, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		if failPush.Code == http.StatusOK {
			t.Fatalf("expected sync/push failure during 503 outage, got 200")
		}
		st := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/cloud/sync/status", nas.adminCookie, nil, nil)
		stMap := decodeSyncJSON(t, st)
		lastErr, ok := stMap["lastSyncError"].(string)
		if stMap["dirty"] != true || !ok || strings.TrimSpace(lastErr) == "" {
			t.Fatalf("expected dirty=true and non-empty string lastSyncError, got %s", st.Body.String())
		}
		if sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records WHERE id='q-retry'") != 1 {
			t.Fatal("local SQLite record lost after sync failure")
		}

		gd.mu.Lock()
		gd.faults.UploadStatus = 0
		gd.mu.Unlock()
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		_ = mustReadSyncManifest(t, gd, "Biofolio")
	})

	t.Run("T2-F6-04_PersonalMalformedReturns400", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		for _, bad := range []string{`{"bag":["q1"]}`, `{"bag":"bad","editions":[]}`} {
			r := callJSON(nas.h, http.MethodPost, "/biology/library/personal", nas.adminCookie, bad)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for malformed personal %s, got %d", bad, r.Code)
			}
		}
	})

	t.Run("T2-F6-05_ReviewDraftInvalidOrTooLargeReturns400", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		if r := callJSON(nas.h, http.MethodPost, "/biology/library/review-draft", nas.adminCookie, `{"paper":"bad-id","version":1}`); r.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing paper- prefix, got %d", r.Code)
		}
		big := fmt.Sprintf(`{"paper":"paper-1","version":1,"pad":%q}`, strings.Repeat("a", (4<<20)+100))
		if r := callJSON(nas.h, http.MethodPost, "/biology/library/review-draft", nas.adminCookie, big); r.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for >4 MiB review-draft, got %d", r.Code)
		}
	})

	t.Run("T2-F6-06_TeacherForbiddenWritesNeverLeakToSync", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.wangCookie, makeValidQuestionRecord("q-wang-1", "王老师题", "C", "析"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		gd.ResetUploadStats()

		// li tries to add teachingNote / delete q-wang-1 / purge -> 403
		for _, badState := range []map[string]any{
			{"version": 0, "state": map[string]any{"fixes": map[string]any{"q-wang-1": map[string]any{"teachingNote": "篡改"}}}},
			{"version": 0, "state": map[string]any{"deletedQuestions": []string{"q-wang-1"}}},
			{"version": 0, "state": map[string]any{"purgedQuestions": []string{"q-wang-1"}}},
		} {
			r := callJSON(nas.h, http.MethodPost, "/biology/library/state", nas.liCookie, badState)
			if r.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for cross-teacher mutation %v, got %d %s", badState, r.Code, r.Body.String())
			}
		}
		if gd.UploadCount() != 0 {
			t.Fatal("rejected 403 writes triggered Drive uploads")
		}
	})
}

func TestE2ETier2_F7(t *testing.T) {
	t.Run("T2-F7-01_EmptyRemotePopulatedLocalProtected", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)

		for i := 0; i < 10; i++ {
			mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord(fmt.Sprintf("q-701-%d", i), "题", "A", "析"))
		}
		for v := 0; v < 3; v++ {
			mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{"version": v, "state": map[string]any{"aliases": map[string]string{"k": fmt.Sprintf("v%d", v)}}})
		}
		// Empty 同步库/ folder on Drive.
		gd.PutFileAtPath("Biofolio/同步库/.placeholder", "text/plain", []byte(""))
		gd.DeletePath("Biofolio/同步库/.placeholder")

		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chk)) != "remote_empty_local_populated" {
			t.Fatalf("expected remote_empty_local_populated, got %s", chk.Body.String())
		}
		if sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records") != 10 {
			t.Fatal("local records altered")
		}
		if sqliteCount(t, nas.l, "SELECT version FROM library_state WHERE id=1") != 3 {
			t.Fatal("local state version altered")
		}
	})

	t.Run("T2-F7-02_CorruptedRemoteManifestHandledSafely", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-702", "题", "A", "析"))

		gd.PutFileAtPath("Biofolio/同步库/manifest.json", "application/json", []byte("{corrupt-json"))

		chk := callJSON(nas.h, http.MethodPost, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		out := decodeSyncJSON(t, chk)
		if syncStatusString(out) != "error" {
			t.Fatalf("expected status=error on corrupt manifest, got %s", chk.Body.String())
		}
		rest := callJSON(nas.h, http.MethodPost, "/biology/library/cloud/sync/restore", nas.adminCookie, map[string]any{"mode": "overwrite"})
		if rest.Code == http.StatusOK {
			t.Fatalf("restore must refuse corrupted manifest, got 200")
		}
		if sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records") != 1 {
			t.Fatal("local record lost after corrupt manifest")
		}
	})

	t.Run("T2-F7-03_DivergedLocalAndRemoteDetected", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "SyncGD", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-703a", "基准", "A", "析"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		// Bump remote syncVersion as if another NAS pushed, AND mutate local.
		m := mustReadSyncManifest(t, gd, "Biofolio")
		m["syncVersion"] = m["syncVersion"].(float64) + 5
		b, _ := json.Marshal(m)
		gd.PutFileAtPath("Biofolio/同步库/manifest.json", "application/json", b)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-703b", "本地新增", "B", "析"))

		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chk)) != "diverged" {
			t.Fatalf("expected diverged status, got %s", chk.Body.String())
		}
	})

	t.Run("T2-F7-04_UnconfiguredGDriveStatusAndCheck", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		st := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/cloud/sync/status", nas.adminCookie, nil, nil)
		if st.Code != http.StatusOK {
			t.Fatalf("expected 200 on unconfigured sync/status, got %d", st.Code)
		}
		m := decodeSyncJSON(t, st)
		if m["configured"] != false || syncStatusString(m) != "unconfigured" {
			t.Fatalf("unexpected unconfigured status: %s", st.Body.String())
		}
		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chk)) != "unconfigured" {
			t.Fatalf("unexpected unconfigured check: %s", chk.Body.String())
		}
	})

	t.Run("T2-F7-05_NonAdminForbiddenOnGlobalRestore", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		rest := callJSON(nas.h, http.MethodPost, "/biology/library/cloud/sync/restore", nas.liCookie, map[string]any{"mode": "merge"})
		if rest.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-admin sync/restore, got %d %s", rest.Code, rest.Body.String())
		}
	})
}

func TestE2ETier2_F8(t *testing.T) {
	t.Run("T2-F8-01_EmptyRemoteGuardBlocksBothModes409", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "EmptyGD", "Biofolio", true)

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-keep", "必须保留", "A", "解析"))
		mustPostJSON(t, nas.h, "/biology/library/favorite", nas.adminCookie, map[string]any{"question": "q-keep", "selected": true})
		mustPostJSON(t, nas.h, "/biology/library/personal", nas.adminCookie, map[string]any{"bag": []string{"q-keep"}, "editions": []any{}})

		for _, mode := range []string{"overwrite", "merge"} {
			r := callJSON(nas.h, http.MethodPost, "/biology/library/cloud/sync/restore", nas.adminCookie, map[string]any{"mode": mode})
			if r.Code != http.StatusConflict {
				t.Fatalf("mode %q must return 409 on empty remote, got %d %s", mode, r.Code, r.Body.String())
			}
		}
		if sqliteCount(t, nas.l, "SELECT COUNT(*) FROM library_records WHERE id='q-keep'") != 1 {
			t.Fatal("local record wiped despite empty-remote guard")
		}
	})

	t.Run("T2-F8-02_MissingShardRollsBackTransaction", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-802", "完整题", "A", "析"))
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		// Delete one record shard on Drive.
		buckets := gd.ListDir("Biofolio/同步库/records")
		if len(buckets) == 0 {
			t.Fatal("expected at least 1 bucket shard in 同步库/records/")
		}
		gd.DeletePath("Biofolio/同步库/records/" + buckets[0].Name)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nasB.h, "/biology/library/record", nasB.adminCookie, makeValidQuestionRecord("q-nasb-pre", "原有题", "B", "析"))

		rest := callJSON(nasB.h, http.MethodPost, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "overwrite"})
		if rest.Code == http.StatusOK {
			t.Fatalf("expected restore error on missing shard, got 200")
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records WHERE id='q-nasb-pre'") != 1 {
			t.Fatal("transaction did not roll back: q-nasb-pre lost")
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records WHERE id='q-802'") != 0 {
			t.Fatal("partial shard committed despite missing shard error")
		}
	})

	t.Run("T2-F8-03_ShardHashMismatchAbortsBeforeCommit", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-803", "校验题", "A", "析"))
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		buckets := gd.ListDir("Biofolio/同步库/records")
		if len(buckets) == 0 {
			t.Fatal("expected bucket shard")
		}
		// Tamper shard bytes while leaving manifest.json hash unchanged.
		gd.PutFileAtPath("Biofolio/同步库/records/"+buckets[0].Name, "application/json", []byte(`[{"id":"tampered"}]`))

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "GD", "Biofolio", true)
		rest := callJSON(nasB.h, http.MethodPost, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "overwrite"})
		if rest.Code == http.StatusOK {
			t.Fatal("expected restore failure on shard hash mismatch")
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records") != 0 {
			t.Fatal("tampered shard committed to SQLite")
		}
	})

	t.Run("T2-F8-04_OlderRemoteMergePreservesLocalNewer", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-804-old", "旧远端题", "A", "析"))
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		// Lower remote syncVersion artificially while NAS-B has higher local version & newer question.
		m := mustReadSyncManifest(t, gd, "Biofolio")
		m["syncVersion"] = float64(1)
		b, _ := json.Marshal(m)
		gd.PutFileAtPath("Biofolio/同步库/manifest.json", "application/json", b)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nasB.h, "/biology/library/record", nasB.adminCookie, makeValidQuestionRecord("q-804-new", "本地新题", "B", "析"))

		r := callJSON(nasB.h, http.MethodPost, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})
		if r.Code != http.StatusOK && r.Code != http.StatusConflict {
			t.Fatalf("unexpected status %d: %s", r.Code, r.Body.String())
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records WHERE id='q-804-new'") != 1 {
			t.Fatal("merge restore must never silently delete newer local question")
		}
	})

	t.Run("T2-F8-05_SeedStudentHashMismatchGuard", func(t *testing.T) {
		_, nasA := newE2EEnv(t)
		nasA.l.page = []byte(`<html><head><script id="paper-data" type="application/json">{"studentHash":"seed-A","questions":[]}</script></head><body>ok</body></html>`)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-805", "样卷题", "A", "析"))
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, `<html><head><script id="paper-data" type="application/json">{"studentHash":"seed-B","questions":[]}</script></head><body>ok</body></html>`)
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "GD", "Biofolio", true)

		r := callJSON(nasB.h, http.MethodPost, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "overwrite"})
		if r.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on seed studentHash mismatch, got %d %s", r.Code, r.Body.String())
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records") != 0 {
			t.Fatal("mismatched seed restore must roll back cleanly")
		}
	})

	t.Run("T2-F8-06_MissingAuthOrCSRFOnRestore", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		if r := callJSON(nas.h, http.MethodPost, "/biology/library/cloud/sync/restore", nil, map[string]any{"mode": "merge"}); r.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 without cookie, got %d", r.Code)
		}
		req := httptest.NewRequest(http.MethodPost, "/biology/library/cloud/sync/restore", strings.NewReader(`{"mode":"merge"}`))
		req.AddCookie(nas.adminCookie)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		nas.h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 without X-Biology-Library, got %d", w.Code)
		}
	})
}

// -----------------------------------------------------------------------------
// Tier 3: Pairwise Cross-Feature Combinatorial Matrix
// -----------------------------------------------------------------------------

func TestE2ETier3_Pairwise(t *testing.T) {
	t.Run("T3-01_F4PlusF6_ImportAndEditSplitSync", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		orig := []byte("PK\x03\x04PAPER-301")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-301", "一模卷", "一模卷.docx", orig, "", nil))
		q1 := makeValidQuestionRecord("paper-301-1", "题1", "A", "析1")
		q2 := makeValidQuestionRecord("paper-301-2", "题2", "B", "析2")
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, q1)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, q2)

		postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive", "relDir": "已导入试卷/一模卷", "filename": "一模卷.json", "mimeType": "application/json", "overwrite": true,
		}, []byte(`{"id":"paper-301"}`))
		postAutoUpload(nas.h, nas.adminCookie, map[string]any{
			"providerKind": "gdrive", "relDir": "已导入试卷/一模卷", "filename": "一模卷.docx", "mimeType": docxMimeType, "overwrite": true,
		}, orig)

		waitUntil(t, 15*time.Second, "initial split sync after import", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		gd.ResetUploadStats()

		q1Obj := q1["questions"].([]map[string]any)[0]
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, map[string]any{
			"id": "revision:paper-301-1:1", "before": q1Obj, "question": map[string]any{"id": "paper-301-1", "stem": "修订题1", "answer": "C", "a": "新析1"},
		})
		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{
			"version": 0, "state": map[string]any{"fixes": map[string]any{"paper-301-1": map[string]any{"teachingNote": "讲评"}}},
		})
		waitUntil(t, 15*time.Second, "split sync after edit", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		if len(gd.UploadsMatchingPrefix("/Biofolio/已导入试卷/")) > 0 || len(gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")) > 0 {
			t.Fatal("editing question must not re-upload 已导入试卷/ or 同步库/papers/*")
		}
	})

	t.Run("T3-02_F5PlusF8_ExamAssemblyPushAndRestore", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "GD", "Biofolio", true)

		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-302a", "题A", "A", "析A"))
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-302b", "题B", "B", "析B"))
		mustPostJSON(t, nasA.h, "/biology/library/favorite", nasA.adminCookie, map[string]any{"question": "q-302a", "selected": true})
		mustPostJSON(t, nasA.h, "/biology/library/favorite", nasA.adminCookie, map[string]any{"question": "q-302b", "selected": true})
		ed := map[string]any{
			"id": "ed-1", "name": "高三周测", "isExam": true, "ids": []string{"q-302a", "q-302b"},
			"grading": map[string]any{"total": 10}, "fixes": map[string]any{"q-302a": map[string]any{"teachingNote": "注意"}},
		}
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.adminCookie, map[string]any{"bag": []any{}, "editions": []any{ed}})
		edBytes, _ := json.Marshal(ed)
		postAutoUpload(nasA.h, nasA.adminCookie, map[string]any{
			"providerKind": "gdrive", "relDir": "已组试卷", "filename": "高三周测（学生版）.docx", "mimeType": docxMimeType, "overwrite": true,
		}, []byte("PK-EXAM-302"))
		postAutoUpload(nasA.h, nasA.adminCookie, map[string]any{
			"providerKind": "gdrive", "relDir": "已组试卷", "filename": "高三周测.json", "mimeType": "application/json", "overwrite": true,
		}, edBytes)
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})

		home := callLibraryAPI(nasB.h, http.MethodGet, "/biology/", nasB.adminCookie, nil, nil)
		srv := parseLibraryServerJSON(t, home.Body.Bytes())
		st := srv["state"].(map[string]any)
		if !strings.Contains(fmt.Sprint(st["good"]), "q-302a") || !strings.Contains(fmt.Sprint(st["editions"]), "ed-1") {
			t.Fatalf("NAS-B state missing favorites/edition: %v", st)
		}
		if _, ok := gd.ReadPath("Biofolio/已组试卷/高三周测（学生版）.docx"); !ok {
			t.Fatal("已组试卷 .docx missing")
		}
		if _, ok := gd.ReadPath("Biofolio/已组试卷/高三周测.json"); !ok {
			t.Fatal("已组试卷 .json missing")
		}
	})

	t.Run("T3-03_F3PlusF6PlusF8_MultiAccountIsolation", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "AdminGD", "Biofolio", true)
		connectGoogleDrive(t, nasA.h, nasA.liCookie, "LiGD", "LiRoot", true)
		connectGoogleDrive(t, nasA.h, nasA.wangCookie, "WangGD", "WangRoot", true)

		postAutoUpload(nasA.h, nasA.liCookie, map[string]any{"providerKind": "gdrive", "relDir": "已组试卷", "filename": "li.json", "mimeType": "application/json"}, []byte(`{"owner":"li"}`))
		postAutoUpload(nasA.h, nasA.wangCookie, map[string]any{"providerKind": "gdrive", "relDir": "已组试卷", "filename": "wang.json", "mimeType": "application/json"}, []byte(`{"owner":"wang"}`))
		if _, ok := gd.ReadPath("LiRoot/已组试卷/li.json"); !ok {
			t.Fatal("li upload missing from LiRoot")
		}
		if _, wrong := gd.ResolvePath("WangRoot/已组试卷/li.json"); wrong {
			t.Fatal("li upload leaked into WangRoot")
		}

		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-303", "共享题", "A", "析"))
		mustPostJSON(t, nasA.h, "/biology/library/favorite", nasA.adminCookie, map[string]any{"question": "q-303", "selected": true})
		mustPostJSON(t, nasA.h, "/biology/library/favorite", nasA.liCookie, map[string]any{"question": "q-303", "selected": true})
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.liCookie, map[string]any{"bag": []string{"q-303"}, "editions": []map[string]any{{"id": "ed-li", "name": "李卷"}}})
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.wangCookie, map[string]any{"bag": []string{}, "editions": []map[string]any{{"id": "ed-wang", "name": "王卷"}}})
		mustPostJSON(t, nasA.h, "/biology/library/review-draft", nasA.liCookie, map[string]any{"paper": "paper-1", "version": 1})
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "AdminGD", "Biofolio", true)
		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})

		liHome := parseLibraryServerJSON(t, callLibraryAPI(nasB.h, http.MethodGet, "/biology/", nasB.liCookie, nil, nil).Body.Bytes())
		wangHome := parseLibraryServerJSON(t, callLibraryAPI(nasB.h, http.MethodGet, "/biology/", nasB.wangCookie, nil, nil).Body.Bytes())
		if !strings.Contains(fmt.Sprint(liHome["state"]), "ed-li") || strings.Contains(fmt.Sprint(liHome["state"]), "ed-wang") {
			t.Fatalf("li state leaked or missing: %v", liHome["state"])
		}
		if !strings.Contains(fmt.Sprint(wangHome["state"]), "ed-wang") || strings.Contains(fmt.Sprint(wangHome["state"]), "ed-li") {
			t.Fatalf("wang state leaked or missing: %v", wangHome["state"])
		}
	})

	t.Run("T3-04_F7PlusF8_FullEmptyRemoteToTwoMachineSync", func(t *testing.T) {
		_, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-304", "跨机题", "A", "析"))

		if syncStatusString(decodeSyncJSON(t, mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/check", nasA.adminCookie, nil))) != "remote_empty_local_populated" {
			t.Fatal("expected remote_empty_local_populated on NAS-A")
		}
		if r := callJSON(nasA.h, http.MethodPost, "/biology/library/cloud/sync/restore", nasA.adminCookie, map[string]any{"mode": "overwrite"}); r.Code != http.StatusConflict {
			t.Fatalf("expected 409 guard on NAS-A, got %d", r.Code)
		}
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "GD", "Biofolio", true)
		if syncStatusString(decodeSyncJSON(t, mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/check", nasB.adminCookie, nil))) != "remote_ahead" {
			t.Fatal("expected remote_ahead on empty NAS-B")
		}
		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})
		if syncStatusString(decodeSyncJSON(t, mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/check", nasB.adminCookie, nil))) != "in_sync" {
			t.Fatal("expected in_sync on NAS-B after restore")
		}
	})

	t.Run("T3-05_F1PlusF6_SlowFailingDriveNeverBlocksLocalReads", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("p1", "快读题", "A", "析"))

		gd.mu.Lock()
		gd.faults.UploadDelay = 2500 * time.Millisecond
		gd.faults.UploadStatus = http.StatusInternalServerError
		gd.mu.Unlock()

		var pushWg sync.WaitGroup
		t.Cleanup(func() {
			gd.mu.Lock()
			gd.faults.UploadDelay = 0
			gd.faults.UploadStatus = 0
			gd.mu.Unlock()
			pushWg.Wait()
		})

		// Trigger dirty state AND an in-flight sync/push so Drive is actively sleeping 2500ms & failing during reads.
		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{"version": 0, "state": map[string]any{"aliases": map[string]string{"p1": "卷1"}}})
		pushWg.Add(1)
		go func() {
			defer pushWg.Done()
			_ = callJSON(nas.h, http.MethodPost, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		}()
		time.Sleep(50 * time.Millisecond)

		var wg sync.WaitGroup
		errs := make(chan string, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				start := time.Now()
				path := []string{"/biology/", "/biology/library/records?brief=1&compact=1", "/biology/library/record?id=p1"}[idx%3]
				r := callLibraryAPI(nas.h, http.MethodGet, path, nas.adminCookie, nil, nil)
				if r.Code != http.StatusOK || time.Since(start) > 300*time.Millisecond {
					errs <- fmt.Sprintf("read %s failed or slow: code=%d dur=%v", path, r.Code, time.Since(start))
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
	})

	t.Run("T3-06_F2PlusF3_ConcurrentMultiFolderAutoUploadRace", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		jobs := []struct{ dir, name, mime string }{
			{"已导入试卷/一模", "一模.json", "application/json"},
			{"已导入试卷/一模", "一模.docx", docxMimeType},
			{"已导入试卷/二模", "二模.json", "application/json"},
			{"已导入试卷/二模", "二模.pdf", pdfMimeType},
			{"已组试卷", "周练.docx", docxMimeType},
			{"已组试卷", "周练.json", "application/json"},
		}
		var wg sync.WaitGroup
		errs := make(chan string, len(jobs))
		for _, j := range jobs {
			wg.Add(1)
			go func(dir, name, m string) {
				defer wg.Done()
				r := postAutoUpload(nas.h, nas.adminCookie, map[string]any{
					"providerKind": "gdrive", "relDir": dir, "filename": name, "mimeType": m, "overwrite": true,
				}, []byte("DATA-"+name))
				if r.Code != http.StatusOK {
					errs <- fmt.Sprintf("%s/%s: %d %s", dir, name, r.Code, r.Body.String())
				}
			}(j.dir, j.name, j.mime)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
		if dups := gd.DuplicateFolders(); len(dups) > 0 {
			t.Fatalf("duplicate folders detected: %v", dups)
		}
	})

	t.Run("T3-07_F1PlusF4_EmbeddedImageCompactAndFullRecord", func(t *testing.T) {
		_, nas := newE2EEnv(t)
		online, err := splitOnlinePage([]byte(`<html><head><script data-lib="vue">window.Vue={};</script><script id="paper-data" type="application/json">{"studentHash":"e2e-seed","questions":[]}</script></head><body>function libraryPicture(x){return x}</body></html>`))
		if err != nil {
			t.Fatal(err)
		}
		nas.l.online = online
		png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3, 4}
		b64 := base64.StdEncoding.EncodeToString(png)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidPaperRecord("paper-img", "配图卷", "配图卷.docx", []byte("PK-IMG"), "", nil))
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, map[string]any{
			"id": "paper-img-1",
			"questions": []map[string]any{
				{"id": "paper-img-1", "stem": "看图答题", "contentHtml": `<p><img src="data:image/png;base64,` + b64 + `"></p>`, "answer": "A", "a": "析"},
			},
		})
		recList := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?brief=1&compact=1", nas.adminCookie, nil, nil)
		if !strings.Contains(recList.Body.String(), `"sourceKind":"docx"`) {
			t.Fatalf("missing sourceKind projection: %s", recList.Body.String())
		}
		fullList := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/records?compact=1", nas.adminCookie, nil, nil)
		m := regexp.MustCompile(`library/picture/([0-9a-f]{64})`).FindStringSubmatch(fullList.Body.String())
		if len(m) != 2 {
			t.Fatalf("expected compact picture URL, got %s", fullList.Body.String())
		}
		pic := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/picture/"+m[1], nas.adminCookie, nil, nil)
		if pic.Code != 200 || !bytes.Equal(pic.Body.Bytes(), png) {
			t.Fatal("picture endpoint bytes mismatch")
		}
		rawPaper := callLibraryAPI(nas.h, http.MethodGet, "/biology/library/record?id=paper-img", nas.adminCookie, nil, nil)
		if !strings.Contains(rawPaper.Body.String(), "original") {
			t.Fatal("full paper record missing original")
		}
	})

	t.Run("T3-09_F6PlusF7_OfflineMutationsThenReconnectCheckPush", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-309a", "在线题", "A", "析"))
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		gd.mu.Lock()
		gd.faults.UploadStatus = http.StatusBadGateway
		gd.mu.Unlock()

		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("q-309b", "离线新增题", "B", "析"))
		mustPostJSON(t, nas.h, "/biology/library/favorite", nas.adminCookie, map[string]any{"question": "q-309b", "selected": true})
		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{"version": 0, "state": map[string]any{"aliases": map[string]string{"x": "y"}}})

		gd.mu.Lock()
		gd.faults.UploadStatus = 0
		gd.mu.Unlock()

		chk := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chk)) != "local_ahead" {
			t.Fatalf("expected local_ahead on recovery, got %s", chk.Body.String())
		}
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)
		chk2 := mustPostJSON(t, nas.h, "/biology/library/cloud/sync/check", nas.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chk2)) != "in_sync" {
			t.Fatalf("expected in_sync after recovery push, got %s", chk2.Body.String())
		}
	})

	t.Run("T3-10_F3PlusF8_ExpiredTokenRefreshedForUploadAndRestore", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connA := connectGoogleDrive(t, nasA.h, nasA.adminCookie, "GD-A", "Biofolio", true)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord("q-310", "刷新题", "C", "析"))
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		row, plain, _ := nasA.l.getCloudConnection("admin", connA)
		var sec gdriveSecret
		_ = json.Unmarshal(plain, &sec)
		sec.ExpiryUnix = 1
		secBytes, _ := json.Marshal(sec)
		_ = nasA.l.updateCloudConnectionSecret(row.Account, row.ID, secBytes, sec.Email)

		gd.mu.Lock()
		gd.faults.EnforceBearerToken = true
		gd.mu.Unlock()

		up := postAutoUpload(nasA.h, nasA.adminCookie, map[string]any{
			"connectionId": connA, "providerKind": "gdrive", "relDir": "已组试卷", "filename": "tok.json", "mimeType": "application/json",
		}, []byte(`{"ok":1}`))
		if up.Code != http.StatusOK || gd.RefreshCount() == 0 {
			t.Fatalf("expected refresh + 200 on auto-upload, got %d refreshes=%d", up.Code, gd.RefreshCount())
		}
		rest := mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/restore", nasA.adminCookie, map[string]any{"mode": "merge"})
		if rest.Code != http.StatusOK {
			t.Fatalf("restore after token refresh failed: %d", rest.Code)
		}
	})
}

// -----------------------------------------------------------------------------
// Tier 4: Real-World E2E Application Scenarios
// -----------------------------------------------------------------------------

func TestE2ETier4_Scenarios(t *testing.T) {
	t.Run("T4-01_FullTeacherLifecycleAndCrossMachineRestore", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "主教学盘", "Biofolio", true)

		origDocx := []byte("PK\x03\x04REAL-PAPER-2026-YIMO")
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidPaperRecord("paper-yimo", "2026高三生物一模", "2026高三生物一模.docx", origDocx, "", nil))
		q1 := makeValidQuestionRecord("paper-yimo-1", "光合作用光反应", "A", "类囊体薄膜")
		q2 := makeValidQuestionRecord("paper-yimo-2", "有氧呼吸第三阶段", "B", "线粒体内膜")
		q3 := makeValidQuestionRecord("paper-yimo-3", "神经冲动传导", "D", "双向传导")
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, q1)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, q2)
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, q3)

		pkgBytes, _ := json.Marshal(map[string]any{"id": "paper-yimo", "name": "2026高三生物一模", "sourceKind": "docx", "questions": []string{"paper-yimo-1", "paper-yimo-2", "paper-yimo-3"}})
		postAutoUpload(nasA.h, nasA.adminCookie, map[string]any{"providerKind": "gdrive", "relDir": "已导入试卷/2026高三生物一模", "filename": "2026高三生物一模.json", "mimeType": "application/json", "overwrite": true}, pkgBytes)
		postAutoUpload(nasA.h, nasA.adminCookie, map[string]any{"providerKind": "gdrive", "relDir": "已导入试卷/2026高三生物一模", "filename": "2026高三生物一模.docx", "mimeType": docxMimeType, "overwrite": true}, origDocx)

		q1Obj := q1["questions"].([]map[string]any)[0]
		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, map[string]any{
			"id": "revision:paper-yimo-1:1", "before": q1Obj, "question": map[string]any{"id": "paper-yimo-1", "stem": "光合作用光反应（修订）", "answer": "A", "a": "类囊体薄膜"},
		})
		mustPostJSON(t, nasA.h, "/biology/library/favorite", nasA.adminCookie, map[string]any{"question": "paper-yimo-2", "selected": true})
		mustPostJSON(t, nasA.h, "/biology/library/state", nasA.adminCookie, map[string]any{
			"version": 0, "state": map[string]any{"fixes": map[string]any{"paper-yimo-2": map[string]any{"teachingNote": "讲评要点：强调还原氢去向"}}},
		})
		mustPostJSON(t, nasA.h, "/biology/library/review-draft", nasA.adminCookie, map[string]any{"paper": "paper-yimo", "version": 1, "items": []any{}})

		ed := map[string]any{"id": "ed-yimo", "isExam": true, "name": "一模复习卷", "ids": []string{"paper-yimo-1", "paper-yimo-2"}, "grading": map[string]any{"total": 20}}
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.adminCookie, map[string]any{"bag": []string{}, "editions": []any{ed}})
		edJSON, _ := json.Marshal(ed)
		examDocx := []byte("PK\x03\x04EXAM-DOCX")
		postAutoUpload(nasA.h, nasA.adminCookie, map[string]any{"providerKind": "gdrive", "relDir": "已组试卷", "filename": "一模复习卷（学生版）.docx", "mimeType": docxMimeType, "overwrite": true}, examDocx)
		postAutoUpload(nasA.h, nasA.adminCookie, map[string]any{"providerKind": "gdrive", "relDir": "已组试卷", "filename": "一模复习卷.json", "mimeType": "application/json", "overwrite": true}, edJSON)

		waitUntil(t, 15*time.Second, "debounced split sync in T4-01", func() bool {
			_, ok := gd.ReadPath("Biofolio/同步库/manifest.json")
			return ok
		})
		// Ensure final flush if multiple debounce windows occurred.
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		for _, p := range []string{
			"Biofolio/已导入试卷/2026高三生物一模/2026高三生物一模.json",
			"Biofolio/已导入试卷/2026高三生物一模/2026高三生物一模.docx",
			"Biofolio/已组试卷/一模复习卷（学生版）.docx",
			"Biofolio/已组试卷/一模复习卷.json",
			"Biofolio/同步库/manifest.json",
			"Biofolio/同步库/state.json",
			"Biofolio/同步库/favorites/__admin__.json",
			"Biofolio/同步库/personal/__admin__.json",
		} {
			if _, ok := gd.ReadPath(p); !ok {
				t.Fatalf("missing Drive artifact %q", p)
			}
		}

		// Fresh NAS-B startup check + one-click restore.
		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "主教学盘", "Biofolio", true)

		chk := mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/check", nasB.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chk)) != "remote_ahead" {
			t.Fatalf("expected remote_ahead on NAS-B, got %s", chk.Body.String())
		}
		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})

		// Verify all restored state on NAS-B.
		homeB := parseLibraryServerJSON(t, callLibraryAPI(nasB.h, http.MethodGet, "/biology/", nasB.adminCookie, nil, nil).Body.Bytes())
		stB := homeB["state"].(map[string]any)
		if !strings.Contains(fmt.Sprint(stB["good"]), "paper-yimo-2") ||
			!strings.Contains(fmt.Sprint(stB["fixes"]), "讲评要点") ||
			!strings.Contains(fmt.Sprint(stB["editions"]), "一模复习卷") {
			t.Fatalf("NAS-B state incomplete: %v", stB)
		}
		origRec := callLibraryAPI(nasB.h, http.MethodGet, "/biology/library/record?id=paper-yimo", nasB.adminCookie, nil, nil)
		if !strings.Contains(origRec.Body.String(), base64.StdEncoding.EncodeToString(origDocx)) {
			t.Fatal("NAS-B missing original .docx attachment after restore")
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_review_drafts WHERE paper='paper-yimo'") != 1 {
			t.Fatal("NAS-B missing review draft after restore")
		}
	})

	t.Run("T4-02_MultiTeacherCollaborationAndIsolation", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "AdminSyncGD", "Biofolio", true)
		connectGoogleDrive(t, nasA.h, nasA.liCookie, "LiPersonalGD", "LiBio", true)

		// Verify /biology/members works for creating an additional teacher too.
		mustPostJSON(t, nasA.h, "/biology/members", nasA.adminCookie, map[string]any{
			"username": "zhao", "password": "teacher-pass-3", "confirm": "teacher-pass-3",
		})

		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.liCookie, makeValidQuestionRecord("q-li-1", "李老师题", "A", "析"))
		postAutoUpload(nasA.h, nasA.liCookie, map[string]any{"providerKind": "gdrive", "relDir": "已导入试卷/李卷", "filename": "李卷.json", "mimeType": "application/json"}, []byte(`{"by":"li"}`))
		if _, ok := gd.ReadPath("LiBio/已导入试卷/李卷/李卷.json"); !ok {
			t.Fatal("li paper missing from LiBio")
		}

		mustPostJSON(t, nasA.h, "/biology/library/record", nasA.wangCookie, makeValidQuestionRecord("q-wang-1", "王老师题", "B", "析"))
		wangUp := postAutoUpload(nasA.h, nasA.wangCookie, map[string]any{"providerKind": "gdrive", "relDir": "已导入试卷/王卷", "filename": "王卷.json", "mimeType": "application/json"}, []byte(`{"by":"wang"}`))
		if wangUp.Code != http.StatusOK || decodeSyncJSON(t, wangUp)["skipped"] != true {
			t.Fatalf("expected graceful skip for unconfigured wang, got %d %s", wangUp.Code, wangUp.Body.String())
		}

		if r := callJSON(nasA.h, http.MethodPost, "/biology/library/state", nasA.liCookie, map[string]any{
			"version": 0, "state": map[string]any{"deletedQuestions": []string{"q-wang-1"}},
		}); r.Code != http.StatusForbidden {
			t.Fatalf("expected 403 when li tries to delete q-wang-1, got %d", r.Code)
		}
		mustPostJSON(t, nasA.h, "/biology/library/state", nasA.liCookie, map[string]any{
			"version": 0, "state": map[string]any{"suggestions": []map[string]any{{"id": "sug-1", "qId": "q-wang-1", "byId": "m1", "text": "建议补充解析"}}},
		})
		mustPostJSON(t, nasA.h, "/biology/library/favorite", nasA.liCookie, map[string]any{"question": "q-li-1", "selected": true})
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.liCookie, map[string]any{"bag": []string{"q-li-1"}, "editions": []map[string]any{{"id": "ed-li", "name": "李的卷"}}})
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.wangCookie, map[string]any{"bag": []string{"q-wang-1"}, "editions": []map[string]any{{"id": "ed-wang", "name": "王的卷"}}})
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "AdminSyncGD", "Biofolio", true)
		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "merge"})

		var liID, wangID string
		_ = nasB.l.db.QueryRow("SELECT id FROM library_member_ids WHERE username='li'").Scan(&liID)
		_ = nasB.l.db.QueryRow("SELECT id FROM library_member_ids WHERE username='wang'").Scan(&wangID)
		if liID != "m1" || wangID != "m2" {
			t.Fatalf("member IDs not preserved: li=%q wang=%q", liID, wangID)
		}
		liHome := parseLibraryServerJSON(t, callLibraryAPI(nasB.h, http.MethodGet, "/biology/", nasB.liCookie, nil, nil).Body.Bytes())
		wangHome := parseLibraryServerJSON(t, callLibraryAPI(nasB.h, http.MethodGet, "/biology/", nasB.wangCookie, nil, nil).Body.Bytes())
		if !strings.Contains(fmt.Sprint(liHome["state"]), "ed-li") || strings.Contains(fmt.Sprint(liHome["state"]), "ed-wang") {
			t.Fatalf("li state mismatch after restore: %v", liHome["state"])
		}
		if !strings.Contains(fmt.Sprint(wangHome["state"]), "ed-wang") || strings.Contains(fmt.Sprint(wangHome["state"]), "ed-li") {
			t.Fatalf("wang state mismatch after restore: %v", wangHome["state"])
		}
		if !strings.Contains(fmt.Sprint(wangHome["state"]), "sug-1") {
			t.Fatalf("shared suggestions missing after restore: %v", wangHome["state"])
		}
	})

	t.Run("T4-03_EmptyRemoteNewNASSafetyAnd353PlusPush", func(t *testing.T) {
		gd, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "ProdLikeGD", "Biofolio", true)

		tx, err := nasA.l.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 360; i++ {
			id := fmt.Sprintf("q-seed-%03d", i)
			body, _ := json.Marshal(makeValidQuestionRecord(id, fmt.Sprintf("第%d题题干", i), "A", "解析"))
			if _, err := tx.Exec("INSERT INTO library_records(id,body) VALUES(?,?)", id, string(body)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(`UPDATE library_state SET version=12, body='{"aliases":{"p1":"一模"}}' WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		nasA.l.bumpRecords()

		chk := mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/check", nasA.adminCookie, nil)
		chkMap := decodeSyncJSON(t, chk)
		loc, _ := chkMap["local"].(map[string]any)
		rem, _ := chkMap["remote"].(map[string]any)
		if syncStatusString(chkMap) != "remote_empty_local_populated" || rem["empty"] != true || loc["recordCount"].(float64) < 353 {
			t.Fatalf("unexpected status on >=353 seeded DB + empty remote: %s", chk.Body.String())
		}

		for _, mode := range []string{"overwrite", "merge"} {
			r := callJSON(nasA.h, http.MethodPost, "/biology/library/cloud/sync/restore", nasA.adminCookie, map[string]any{"mode": mode})
			if r.Code != http.StatusConflict {
				t.Fatalf("expected 409 guard on %s, got %d", mode, r.Code)
			}
		}
		var integrity string
		_ = nasA.l.db.QueryRow("PRAGMA integrity_check").Scan(&integrity)
		if integrity != "ok" || sqliteCount(t, nasA.l, "SELECT COUNT(*) FROM library_records") != 360 || sqliteCount(t, nasA.l, "SELECT version FROM library_state WHERE id=1") != 12 {
			t.Fatalf("database integrity or counts compromised: integrity=%q", integrity)
		}

		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)
		_ = mustReadSyncManifest(t, gd, "Biofolio")
		chkAfter := mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/check", nasA.adminCookie, nil)
		if syncStatusString(decodeSyncJSON(t, chkAfter)) != "in_sync" {
			t.Fatalf("expected in_sync after initial push, got %s", chkAfter.Body.String())
		}
	})

	t.Run("T4-04_LargePaperResumableAndHeavyShardSkip", func(t *testing.T) {
		gd, nas := newE2EEnv(t)
		setupAdminGoogleCreds(t, nas.h, nas.adminCookie)
		connectGoogleDrive(t, nas.h, nas.adminCookie, "GD", "Biofolio", true)

		bigPDF := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("BIOLOGY-PDF-PAYLOAD-"), 300000)...)
		if len(bigPDF) <= 4<<20 {
			t.Fatalf("test fixture must exceed 4 MiB, got %d", len(bigPDF))
		}
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, map[string]any{
			"id": "paper-big", "paper": true, "sourceKind": "pdf", "name": "大型试卷", "questions": []any{}, "links": []any{},
			"original": map[string]any{"name": "大型试卷.pdf", "type": pdfMimeType, "data": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(bigPDF)},
		})
		for i := 1; i <= 5; i++ {
			mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord(fmt.Sprintf("paper-big-%d", i), "大卷题", "B", "析"))
		}
		upPDF := postAutoUpload(nas.h, nas.adminCookie, map[string]any{"providerKind": "gdrive", "relDir": "已导入试卷/大型试卷", "filename": "大型试卷.pdf", "mimeType": pdfMimeType, "overwrite": true}, bigPDF)
		if upPDF.Code != http.StatusOK {
			t.Fatalf("large pdf upload failed: %d", upPDF.Code)
		}
		mustPostJSON(t, nas.h, "/biology/library/cloud/sync/push", nas.adminCookie, nil)

		pdfEvs := gd.UploadsFor("大型试卷.pdf")
		if len(pdfEvs) != 1 || pdfEvs[0].Mode != "resumable" {
			t.Fatalf("expected resumable upload for >4 MiB PDF, got %+v", pdfEvs)
		}
		paperShards := gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")
		if len(paperShards) != 1 || paperShards[0].Mode != "resumable" {
			t.Fatalf("expected resumable upload for >4 MiB paper shard, got %+v", paperShards)
		}
		if got, ok := gd.ReadPath("Biofolio/已导入试卷/大型试卷/大型试卷.pdf"); !ok || sha256Hex(got) != sha256Hex(bigPDF) {
			t.Fatal("uploaded large PDF bytes mismatch")
		}

		gd.ResetUploadStats()
		mustPostJSON(t, nas.h, "/biology/library/state", nas.adminCookie, map[string]any{
			"version": 0, "state": map[string]any{"fixes": map[string]any{"paper-big-1": map[string]any{"teachingNote": "重点讲评"}}},
		})
		mustPostJSON(t, nas.h, "/biology/library/favorite", nas.adminCookie, map[string]any{"question": "paper-big-2", "selected": true})
		mustPostJSON(t, nas.h, "/biology/library/record", nas.adminCookie, makeValidQuestionRecord("paper-big-6", "新增小题", "C", "析"))
		waitUntil(t, 15*time.Second, "incremental split sync in T4-04", func() bool {
			return len(gd.UploadsFor("manifest.json")) > 0
		})
		if len(gd.UploadsMatchingPrefix("/Biofolio/同步库/papers/")) != 0 {
			t.Fatal("heavy paper-big.json shard must not be re-uploaded on incremental edits")
		}
		if gd.TotalUploadedBytes() >= 100<<10 {
			t.Fatalf("expected incremental delta << 100 KB, got %d bytes", gd.TotalUploadedBytes())
		}
	})

	t.Run("T4-05_DisasterRecoveryPreservesConnectionsAndSecrets", func(t *testing.T) {
		_, nasA := newE2EEnv(t)
		setupAdminGoogleCreds(t, nasA.h, nasA.adminCookie)
		connectGoogleDrive(t, nasA.h, nasA.adminCookie, "GD-A", "Biofolio", true)
		for i := 0; i < 20; i++ {
			mustPostJSON(t, nasA.h, "/biology/library/record", nasA.adminCookie, makeValidQuestionRecord(fmt.Sprintf("q-405-%02d", i), "灾备题", "A", "析"))
		}
		mustPostJSON(t, nasA.h, "/biology/library/personal", nasA.adminCookie, map[string]any{
			"bag": []string{},
			"editions": []map[string]any{
				{"id": "ed-1", "name": "卷1"},
				{"id": "ed-2", "name": "卷2"},
			},
		})
		mustPostJSON(t, nasA.h, "/biology/library/cloud/sync/push", nasA.adminCookie, nil)

		// Replacement NAS-B already has its own connections + secret key + settings + account file.
		nasB := newE2ENAS(t, true, "")
		setupAdminGoogleCreds(t, nasB.h, nasB.adminCookie)
		connectGoogleDrive(t, nasB.h, nasB.adminCookie, "NASB-GD", "Biofolio", true)
		dav := newMockWebDAVServer(t, "t", "p")
		mustPostJSON(t, nasB.h, "/biology/library/cloud/connections", nasB.liCookie, map[string]any{
			"action": "save_webdav", "name": "TeacherDAV", "baseUrl": dav.srv.URL + "/dav", "username": "t", "password": "p",
		})
		mustPostJSON(t, nasB.h, "/biology/library/record", nasB.adminCookie, makeValidQuestionRecord("q-corrupt-stale", "损坏旧题", "D", "析"))

		keyBefore, _ := os.ReadFile(filepath.Join(nasB.l.dir, "cloud-secret.key"))
		setBefore, _ := os.ReadFile(filepath.Join(nasB.l.dir, "cloud-settings.json"))
		accBefore, _ := os.ReadFile(filepath.Join(nasB.l.dir, "library-account.json"))

		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/restore", nasB.adminCookie, map[string]any{"mode": "overwrite"})

		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records") != 20 {
			t.Fatalf("expected 20 restored records, got %d", sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_records"))
		}
		if sqliteCount(t, nasB.l, "SELECT COUNT(*) FROM library_cloud_connections") != 2 {
			t.Fatal("connections lost on disaster recovery restore")
		}
		keyAfter, _ := os.ReadFile(filepath.Join(nasB.l.dir, "cloud-secret.key"))
		setAfter, _ := os.ReadFile(filepath.Join(nasB.l.dir, "cloud-settings.json"))
		accAfter, _ := os.ReadFile(filepath.Join(nasB.l.dir, "library-account.json"))
		if !bytes.Equal(keyBefore, keyAfter) || !bytes.Equal(setBefore, setAfter) || !bytes.Equal(accBefore, accAfter) {
			t.Fatal("credential or account files altered during restore")
		}

		// Preserved connection must immediately allow a follow-up edit + push without re-auth.
		mustPostJSON(t, nasB.h, "/biology/library/favorite", nasB.adminCookie, map[string]any{"question": "q-405-00", "selected": true})
		mustPostJSON(t, nasB.h, "/biology/library/cloud/sync/push", nasB.adminCookie, nil)
	})
}
