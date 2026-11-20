package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	cloudSyncStateFilename = "cloud-sync-state.json"
	syncDirName            = "同步库"
	syncManifestName       = "manifest.json"
	syncRecordBucketCount  = 16
	defaultSyncDebounce    = 5 * time.Second
)

type syncShardMeta struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Kind string `json:"kind"`
}

type cloudSyncManifest struct {
	SchemaVersion   int                      `json:"schemaVersion"`
	SyncVersion     int64                    `json:"syncVersion"`
	UpdatedAt       string                   `json:"updatedAt"`
	StateVersion    int                      `json:"stateVersion"`
	RecordCount     int                      `json:"recordCount"`
	ShardCount      int                      `json:"shardCount"`
	SeedStudentHash string                   `json:"seedStudentHash,omitempty"`
	Shards          map[string]syncShardMeta `json:"shards"`
}

type cloudSyncLocalInfo struct {
	StateVersion int   `json:"stateVersion"`
	RecordCount  int   `json:"recordCount"`
	SyncVersion  int64 `json:"syncVersion"`
}

type cloudSyncRemoteInfo struct {
	Checked     bool   `json:"checked"`
	CheckedAt   string `json:"checkedAt"`
	Exists      bool   `json:"exists"`
	Empty       bool   `json:"empty"`
	SyncVersion int64  `json:"syncVersion"`
	UpdatedAt   string `json:"updatedAt"`
	RecordCount int    `json:"recordCount"`
	ShardCount  int    `json:"shardCount"`
	Error       string `json:"error"`
}

type cloudSyncStatusResponse struct {
	Ok            bool                `json:"ok"`
	Configured    bool                `json:"configured"`
	ConnectionID  string              `json:"connectionId"`
	RootDir       string              `json:"rootDir"`
	Syncing       bool                `json:"syncing"`
	Dirty         bool                `json:"dirty"`
	PendingAt     string              `json:"pendingAt"`
	LastSyncAt    string              `json:"lastSyncAt"`
	LastSyncError string              `json:"lastSyncError"`
	Local         cloudSyncLocalInfo  `json:"local"`
	Remote        cloudSyncRemoteInfo `json:"remote"`
	Status        string              `json:"status"`
}

type cloudSyncConnState struct {
	ConnectionID           string                   `json:"connectionId"`
	RootDir                string                   `json:"rootDir"`
	LastSyncAt             string                   `json:"lastSyncAt,omitempty"`
	LastSyncError          string                   `json:"lastSyncError,omitempty"`
	LastSyncedSyncVersion  int64                    `json:"lastSyncedSyncVersion,omitempty"`
	LastSyncedStateVersion int                      `json:"lastSyncedStateVersion,omitempty"`
	LastSyncedRecordCount  int                      `json:"lastSyncedRecordCount,omitempty"`
	LastSyncedFingerprint  string                   `json:"lastSyncedFingerprint,omitempty"`
	LastSyncedShards       map[string]syncShardMeta `json:"lastSyncedShards,omitempty"`
	LastRemoteCheck        *cloudSyncRemoteInfo     `json:"lastRemoteCheck,omitempty"`
}

type cloudSyncPersistedState struct {
	Connections map[string]cloudSyncConnState `json:"connections,omitempty"`
}

type syncHTTPError struct {
	status int
	msg    string
}

func (e *syncHTTPError) Error() string {
	return e.msg
}

func syncErrorStatus(err error, fallback int) int {
	var he *syncHTTPError
	if errors.As(err, &he) && he.status > 0 {
		return he.status
	}
	return fallback
}

type cloudSyncEngine struct {
	l         *libraryPage
	execMu    sync.Mutex
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	triggerCh chan struct{}

	started       bool
	workerStarted bool
	closed        bool
	dirty         bool
	dirtySeq      uint64
	syncing       bool
	pendingAt     string
}

func newCloudSyncEngine(l *libraryPage) *cloudSyncEngine {
	ctx, cancel := context.WithCancel(context.Background())
	return &cloudSyncEngine{
		l:         l,
		ctx:       ctx,
		cancel:    cancel,
		triggerCh: make(chan struct{}, 1),
	}
}

func (l *libraryPage) sync() *cloudSyncEngine {
	l.syncOnce.Do(func() {
		l.syncEngine = newCloudSyncEngine(l)
	})
	return l.syncEngine
}

func (l *libraryPage) effectiveSyncDebounce() time.Duration {
	if l.syncDebounce > 0 {
		return l.syncDebounce
	}
	return defaultSyncDebounce
}

func (l *libraryPage) hasAdminGDriveConnection() bool {
	if l.db == nil {
		return false
	}
	var cnt int
	if err := l.db.QueryRow(`SELECT count(*) FROM library_cloud_connections WHERE account=? AND kind='gdrive'`, adminOwner).Scan(&cnt); err != nil {
		return false
	}
	return cnt > 0
}

func (l *libraryPage) StartCloudSync() {
	e := l.sync()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.ensureWorkerLocked()
	e.mu.Unlock()

	if _, _, _, err := l.resolveSyncTarget(""); err == nil {
		e.mu.Lock()
		if !e.closed {
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
				defer cancel()
				resp, checkErr := e.check(ctx, "")
				if checkErr != nil || !resp.Ok || !resp.Configured {
					return
				}
				// Only auto-schedule a startup push when this machine already has an
				// established sync baseline (LastSyncedSyncVersion > 0) and remote has
				// not advanced (status == "local_ahead"). Leave Case A
				// ("remote_empty_local_populated") and Case B ("diverged") untouched.
				if resp.Status == "local_ahead" {
					e.mu.Lock()
					if !e.closed && !e.dirty {
						e.dirty = true
						e.dirtySeq++
						e.pendingAt = time.Now().UTC().Format(time.RFC3339)
					}
					e.mu.Unlock()
					e.signalWorker()
				}
			}()
		}
		e.mu.Unlock()
	}
}

func (l *libraryPage) Close() {
	if l == nil {
		return
	}
	e := l.sync()
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		e.cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()
}

func (l *libraryPage) markSyncDirty() {
	if l == nil {
		return
	}
	e := l.sync()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.dirty = true
	e.dirtySeq++
	if e.pendingAt == "" {
		e.pendingAt = time.Now().UTC().Format(time.RFC3339)
	}
	needCheckConn := !e.workerStarted && !e.started
	if e.started && !e.workerStarted {
		e.ensureWorkerLocked()
	}
	e.mu.Unlock()

	if needCheckConn && l.hasAdminGDriveConnection() {
		e.mu.Lock()
		if !e.closed && !e.workerStarted {
			e.ensureWorkerLocked()
		}
		e.mu.Unlock()
	}
	e.signalWorker()
}

func (e *cloudSyncEngine) ensureWorkerLocked() {
	if e.closed || e.workerStarted {
		return
	}
	e.workerStarted = true
	e.wg.Add(1)
	go e.workerLoop()
}

func (e *cloudSyncEngine) signalWorker() {
	select {
	case e.triggerCh <- struct{}{}:
	default:
	}
}

func (e *cloudSyncEngine) workerLoop() {
	defer e.wg.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-e.triggerCh:
		}

		debounce := e.l.effectiveSyncDebounce()
		timer := time.NewTimer(debounce)
	debounceWait:
		for {
			select {
			case <-e.ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-e.triggerCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)
			case <-timer.C:
				break debounceWait
			}
		}

		if e.ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		isDirty := e.dirty
		e.mu.Unlock()
		if !isDirty {
			continue
		}
		if e.l.db == nil || e.l.db.PingContext(e.ctx) != nil {
			return
		}
		if err := e.push(e.ctx, "", true); err != nil {
			// Failure keeps dirty=true and stores lastSyncError; do not tight-loop.
			continue
		}
		e.mu.Lock()
		stillDirty := e.dirty && !e.closed
		e.mu.Unlock()
		if stillDirty {
			e.signalWorker()
		}
	}
}

func (l *libraryPage) syncStatePath() string {
	return filepath.Join(l.dir, cloudSyncStateFilename)
}

func (l *libraryPage) loadSyncPersistedStateLocked() cloudSyncPersistedState {
	if l.dir == "" {
		if l.syncMemState.Connections == nil {
			l.syncMemState.Connections = map[string]cloudSyncConnState{}
		}
		return l.syncMemState
	}
	var st cloudSyncPersistedState
	if data, err := os.ReadFile(l.syncStatePath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Connections == nil {
		st.Connections = map[string]cloudSyncConnState{}
	}
	return st
}

func (l *libraryPage) saveSyncPersistedStateLocked(st cloudSyncPersistedState) error {
	if st.Connections == nil {
		st.Connections = map[string]cloudSyncConnState{}
	}
	if l.dir == "" {
		l.syncMemState = st
		return nil
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(l.dir, ".cloud-sync-state-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)
	_ = f.Chmod(0600)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err != nil || closeErr != nil {
		if err != nil {
			return err
		}
		return closeErr
	}
	return os.Rename(tmpName, l.syncStatePath())
}

func cloneConnSyncState(in cloudSyncConnState) cloudSyncConnState {
	out := in
	if in.LastSyncedShards != nil {
		out.LastSyncedShards = make(map[string]syncShardMeta, len(in.LastSyncedShards))
		for k, v := range in.LastSyncedShards {
			out.LastSyncedShards[k] = v
		}
	}
	if in.LastRemoteCheck != nil {
		rc := *in.LastRemoteCheck
		out.LastRemoteCheck = &rc
	}
	return out
}

func (l *libraryPage) getConnSyncState(connID, rootDir string) cloudSyncConnState {
	l.syncStateMu.Lock()
	defer l.syncStateMu.Unlock()
	st := l.loadSyncPersistedStateLocked()
	cs, ok := st.Connections[connID]
	if !ok {
		return cloudSyncConnState{ConnectionID: connID, RootDir: rootDir}
	}
	if cs.RootDir != rootDir {
		return cloudSyncConnState{ConnectionID: connID, RootDir: rootDir}
	}
	return cloneConnSyncState(cs)
}

func (l *libraryPage) updateConnSyncState(connID, rootDir string, fn func(*cloudSyncConnState)) error {
	l.syncStateMu.Lock()
	defer l.syncStateMu.Unlock()
	st := l.loadSyncPersistedStateLocked()
	cs, ok := st.Connections[connID]
	if !ok || cs.RootDir != rootDir {
		cs = cloudSyncConnState{ConnectionID: connID, RootDir: rootDir}
	} else {
		cs = cloneConnSyncState(cs)
	}
	fn(&cs)
	cs.ConnectionID = connID
	cs.RootDir = rootDir
	st.Connections[connID] = cs
	return l.saveSyncPersistedStateLocked(st)
}

func effectiveSyncRootDir(rawRoot string) string {
	norm, err := normalizeRelativeCloudDir(rawRoot)
	if err != nil || norm == "" {
		return defaultGDriveRelRootDir
	}
	return strings.TrimPrefix(norm, "/")
}

func (l *libraryPage) resolveSyncTarget(explicitConnID string) (cloudConnectionRow, []byte, cloudProvider, error) {
	connID := strings.TrimSpace(explicitConnID)
	var row cloudConnectionRow
	var plain []byte
	var err error
	if connID != "" {
		row, plain, err = l.getCloudConnection(adminOwner, connID)
		if errors.Is(err, errCloudConnectionNotFound) {
			return row, nil, nil, &syncHTTPError{status: http.StatusNotFound, msg: err.Error()}
		}
		if err != nil {
			return row, nil, nil, &syncHTTPError{status: http.StatusInternalServerError, msg: "读取云盘连接失败"}
		}
		if row.Kind != "gdrive" {
			return row, nil, nil, &syncHTTPError{status: http.StatusBadRequest, msg: "同步库仅支持 Google Drive 连接"}
		}
	} else {
		row, plain, err = l.getDefaultCloudConnection(adminOwner, "gdrive")
		if errors.Is(err, errCloudConnectionNotFound) {
			return row, nil, nil, err
		}
		if err != nil {
			return row, nil, nil, &syncHTTPError{status: http.StatusInternalServerError, msg: "读取云盘连接失败"}
		}
	}
	provider, err := l.openCloudProviderFromRow(row, plain, true)
	if err != nil {
		return row, plain, nil, &syncHTTPError{status: http.StatusBadRequest, msg: err.Error()}
	}
	return row, plain, provider, nil
}

func syncSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func syncSeedStudentHash(seed json.RawMessage) string {
	if len(bytes.TrimSpace(seed)) == 0 {
		return ""
	}
	var s struct {
		StudentHash string `json:"studentHash"`
	}
	if json.Unmarshal(seed, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s.StudentHash)
}

func canonicalSyncJSON(raw []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

func canonicalStateObjectWithoutPersonal(raw []byte) (json.RawMessage, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, false, err
	}
	if m == nil {
		m = map[string]any{}
	}
	delete(m, "bag")
	delete(m, "editions")
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(out), len(m) > 0, nil
}

func syncAccountKey(account string) string {
	if account == "" {
		return "__admin__"
	}
	if account != "__admin__" && isSimpleCloudKey(account) && len(account) <= 64 {
		return account
	}
	slug := sanitizeCloudSlug(account, 32)
	return slug + "_" + syncSHA256Hex([]byte(account))[:16]
}

func isSimpleCloudKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func sanitizeCloudSlug(s string, maxLen int) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < maxLen; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" || res == "." || res == ".." {
		return "item"
	}
	return res
}

func paperShardFilename(id string) string {
	if id != "." && id != ".." && !strings.Contains(id, "__") && len(id) > 0 && len(id) <= 100 {
		simple := true
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
				simple = false
				break
			}
		}
		if simple {
			return id + ".json"
		}
	}
	return sanitizeCloudSlug(id, 40) + "__" + syncSHA256Hex([]byte(id))[:16] + ".json"
}

func draftShardFilename(account, paper string) string {
	return syncAccountKey(account) + "__" + syncSHA256Hex([]byte(paper))[:16] + ".json"
}

func syncRecordBucket(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % syncRecordBucketCount)
}

type syncLocalStats struct {
	stateVersion int
	recordCount  int
	populated    bool
}

func (l *libraryPage) computeSyncLocalStats() (string, syncLocalStats, error) {
	if l.db == nil {
		return "", syncLocalStats{}, errors.New("数据库未打开")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return "", syncLocalStats{}, err
	}
	defer tx.Rollback()

	var stats syncLocalStats
	var stateBody string
	if err := tx.QueryRow(`SELECT version, body FROM library_state WHERE id=1`).Scan(&stats.stateVersion, &stateBody); err != nil {
		return "", stats, err
	}
	canonState, stateNonEmpty, err := canonicalStateObjectWithoutPersonal([]byte(stateBody))
	if err != nil {
		canonState = json.RawMessage("{}")
	}

	h := sha256.New()
	_, _ = fmt.Fprintf(h, "state:%d:%s\n", stats.stateVersion, canonState)
	if stats.stateVersion > 0 || stateNonEmpty {
		stats.populated = true
	}

	// Records (id + length(body) avoids reading 50 MB blobs during status checks).
	recRows, err := tx.Query(`SELECT id, length(body) FROM library_records ORDER BY id ASC`)
	if err != nil {
		return "", stats, err
	}
	for recRows.Next() {
		var rid string
		var rlen int64
		if err := recRows.Scan(&rid, &rlen); err != nil {
			recRows.Close()
			return "", stats, err
		}
		stats.recordCount++
		stats.populated = true
		_, _ = fmt.Fprintf(h, "rec:%s:%d\n", rid, rlen)
	}
	if err := recRows.Err(); err != nil {
		recRows.Close()
		return "", stats, err
	}
	recRows.Close()

	// Favorites
	favRows, err := tx.Query(`SELECT account, marks FROM library_favorites ORDER BY account ASC`)
	if err != nil {
		return "", stats, err
	}
	for favRows.Next() {
		var acc, marks string
		if err := favRows.Scan(&acc, &marks); err != nil {
			favRows.Close()
			return "", stats, err
		}
		canon, cErr := canonicalSyncJSON([]byte(marks))
		if cErr != nil {
			canon = json.RawMessage(marks)
		}
		if len(canon) > 2 {
			stats.populated = true
		}
		_, _ = fmt.Fprintf(h, "fav:%s:%s\n", acc, canon)
	}
	if err := favRows.Err(); err != nil {
		favRows.Close()
		return "", stats, err
	}
	favRows.Close()

	// Personal
	perRows, err := tx.Query(`SELECT account, body FROM library_personal ORDER BY account ASC`)
	if err != nil {
		return "", stats, err
	}
	for perRows.Next() {
		var acc, body string
		if err := perRows.Scan(&acc, &body); err != nil {
			perRows.Close()
			return "", stats, err
		}
		canon, cErr := canonicalSyncJSON([]byte(body))
		if cErr != nil {
			canon = json.RawMessage(body)
		}
		var pCheck struct {
			Bag      []json.RawMessage `json:"bag"`
			Editions []json.RawMessage `json:"editions"`
		}
		if json.Unmarshal(canon, &pCheck) == nil && (len(pCheck.Bag) > 0 || len(pCheck.Editions) > 0) {
			stats.populated = true
		}
		_, _ = fmt.Fprintf(h, "per:%s:%s\n", acc, canon)
	}
	if err := perRows.Err(); err != nil {
		perRows.Close()
		return "", stats, err
	}
	perRows.Close()

	// Review drafts
	draftRows, err := tx.Query(`SELECT account, paper, version, body FROM library_review_drafts ORDER BY account ASC, paper ASC`)
	if err != nil {
		return "", stats, err
	}
	for draftRows.Next() {
		var acc, paper, body string
		var ver int
		if err := draftRows.Scan(&acc, &paper, &ver, &body); err != nil {
			draftRows.Close()
			return "", stats, err
		}
		stats.populated = true
		_, _ = fmt.Fprintf(h, "draft:%s:%s:%d:%s\n", acc, paper, ver, syncSHA256Hex([]byte(body)))
	}
	if err := draftRows.Err(); err != nil {
		draftRows.Close()
		return "", stats, err
	}
	draftRows.Close()

	// Member IDs (table + account map)
	members := map[string]string{}
	memRows, err := tx.Query(`SELECT username, id FROM library_member_ids ORDER BY username ASC`)
	if err == nil {
		for memRows.Next() {
			var u, mid string
			if memRows.Scan(&u, &mid) == nil && u != "" && mid != "" && mid != adminOwner {
				members[u] = mid
			}
		}
		memRows.Close()
	}
	if l.account != nil {
		for u, mid := range l.account.memberIDMap() {
			if u != "" && mid != "" && mid != adminOwner {
				members[u] = mid
			}
		}
	}
	memBytes, _ := json.Marshal(members)
	_, _ = fmt.Fprintf(h, "members:%s\n", memBytes)

	if err := tx.Commit(); err != nil {
		return "", stats, err
	}
	return hex.EncodeToString(h.Sum(nil)), stats, nil
}

type syncStatePayload struct {
	Version int             `json:"version"`
	Body    json.RawMessage `json:"body"`
}

type syncAccountMarksPayload struct {
	Account string          `json:"account"`
	Marks   json.RawMessage `json:"marks"`
}

type syncAccountBodyPayload struct {
	Account string          `json:"account"`
	Body    json.RawMessage `json:"body"`
}

type syncBucketPayload struct {
	Bucket  int               `json:"bucket"`
	Records []json.RawMessage `json:"records"`
}

type preparedShard struct {
	relPath string
	meta    syncShardMeta
	data    []byte // nil if skipped due to unchanged heavy paper optimization
	paperID string // non-empty for paper shards so we can reload from DB if needed
}

type syncSnapshot struct {
	stateVersion    int
	recordCount     int
	fingerprint     string
	seedStudentHash string
	shards          map[string]preparedShard
	manifestShards  map[string]syncShardMeta
}

func (l *libraryPage) buildSyncSnapshot(prevShards map[string]syncShardMeta) (*syncSnapshot, error) {
	fp, stats, err := l.computeSyncLocalStats()
	if err != nil {
		return nil, err
	}

	tx, err := l.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	snap := &syncSnapshot{
		recordCount:     stats.recordCount,
		fingerprint:     fp,
		seedStudentHash: syncSeedStudentHash(l.seed()),
		shards:          map[string]preparedShard{},
		manifestShards:  map[string]syncShardMeta{},
	}

	addShard := func(relPath, kind string, data []byte, paperID string) {
		meta := syncShardMeta{
			Hash: syncSHA256Hex(data),
			Size: int64(len(data)),
			Kind: kind,
		}
		if paperID != "" && prevShards != nil {
			if prev, ok := prevShards[relPath]; ok && prev.Hash == meta.Hash && prev.Size == meta.Size {
				// Drop large paper bytes immediately when hash & size match local sync cache.
				data = nil
			}
		}
		snap.shards[relPath] = preparedShard{
			relPath: relPath,
			meta:    meta,
			data:    data,
			paperID: paperID,
		}
		snap.manifestShards[relPath] = meta
	}

	// 1. state.json
	var stateVer int
	var stateRaw string
	if err := tx.QueryRow(`SELECT version, body FROM library_state WHERE id=1`).Scan(&stateVer, &stateRaw); err != nil {
		return nil, err
	}
	snap.stateVersion = stateVer
	canonState, _, err := canonicalStateObjectWithoutPersonal([]byte(stateRaw))
	if err != nil {
		return nil, fmt.Errorf("状态 JSON 无效: %w", err)
	}
	stateBytes, err := json.Marshal(syncStatePayload{Version: stateVer, Body: canonState})
	if err != nil {
		return nil, err
	}
	addShard("state.json", "state", stateBytes, "")

	// 2. member_ids.json
	members := map[string]string{}
	memRows, err := tx.Query(`SELECT username, id FROM library_member_ids ORDER BY username ASC`)
	if err != nil {
		return nil, err
	}
	for memRows.Next() {
		var u, mid string
		if err := memRows.Scan(&u, &mid); err != nil {
			memRows.Close()
			return nil, err
		}
		if u != "" && mid != "" && mid != adminOwner {
			members[u] = mid
		}
	}
	if err := memRows.Err(); err != nil {
		memRows.Close()
		return nil, err
	}
	memRows.Close()
	if l.account != nil {
		for u, mid := range l.account.memberIDMap() {
			if u != "" && mid != "" && mid != adminOwner {
				members[u] = mid
			}
		}
	}
	memBytes, err := json.Marshal(members)
	if err != nil {
		return nil, err
	}
	addShard("member_ids.json", "member_ids", memBytes, "")

	// 3. favorites/<accountKey>.json
	favRows, err := tx.Query(`SELECT account, marks FROM library_favorites ORDER BY account ASC`)
	if err != nil {
		return nil, err
	}
	sawAdminFav := false
	for favRows.Next() {
		var acc, marks string
		if err := favRows.Scan(&acc, &marks); err != nil {
			favRows.Close()
			return nil, err
		}
		if acc == "" {
			sawAdminFav = true
		}
		canon, err := canonicalSyncJSON([]byte(marks))
		if err != nil {
			canon = json.RawMessage("[]")
		}
		b, err := json.Marshal(syncAccountMarksPayload{Account: acc, Marks: canon})
		if err != nil {
			favRows.Close()
			return nil, err
		}
		addShard("favorites/"+syncAccountKey(acc)+".json", "favorites", b, "")
	}
	if err := favRows.Err(); err != nil {
		favRows.Close()
		return nil, err
	}
	favRows.Close()

	// Bridge v0.9.4 legacy admin $.good when library_favorites lacks an account="" row.
	if !sawAdminFav {
		var stateObj map[string]any
		if json.Unmarshal(canonState, &stateObj) == nil && stateObj != nil {
			if legacyGood := unionStringSlices(nil, anySliceToStrings(stateObj["good"])); len(legacyGood) > 0 {
				marksJSON, err := json.Marshal(legacyGood)
				if err != nil {
					return nil, err
				}
				b, err := json.Marshal(syncAccountMarksPayload{Account: "", Marks: json.RawMessage(marksJSON)})
				if err != nil {
					return nil, err
				}
				addShard("favorites/__admin__.json", "favorites", b, "")
			}
		}
	}

	// 4. personal/<accountKey>.json
	perRows, err := tx.Query(`SELECT account, body FROM library_personal ORDER BY account ASC`)
	if err != nil {
		return nil, err
	}
	for perRows.Next() {
		var acc, body string
		if err := perRows.Scan(&acc, &body); err != nil {
			perRows.Close()
			return nil, err
		}
		canon, err := canonicalSyncJSON([]byte(body))
		if err != nil {
			canon = json.RawMessage(`{"bag":[],"editions":[]}`)
		}
		b, err := json.Marshal(syncAccountBodyPayload{Account: acc, Body: canon})
		if err != nil {
			perRows.Close()
			return nil, err
		}
		addShard("personal/"+syncAccountKey(acc)+".json", "personal", b, "")
	}
	if err := perRows.Err(); err != nil {
		perRows.Close()
		return nil, err
	}
	perRows.Close()

	// 5. drafts/<accountKey>__<paperHash>.json (flat inside 同步库/drafts/)
	draftRows, err := tx.Query(`SELECT account, paper, version, body FROM library_review_drafts ORDER BY account ASC, paper ASC`)
	if err != nil {
		return nil, err
	}
	for draftRows.Next() {
		var acc, paper, body string
		var ver int
		if err := draftRows.Scan(&acc, &paper, &ver, &body); err != nil {
			draftRows.Close()
			return nil, err
		}
		canon, err := canonicalSyncJSON([]byte(body))
		if err != nil {
			canon = json.RawMessage(body)
		}
		b, err := json.Marshal(reviewDraftBackup{
			Account: acc,
			Paper:   paper,
			Version: ver,
			Body:    canon,
		})
		if err != nil {
			draftRows.Close()
			return nil, err
		}
		addShard("drafts/"+draftShardFilename(acc, paper), "draft", b, "")
	}
	if err := draftRows.Err(); err != nil {
		draftRows.Close()
		return nil, err
	}
	draftRows.Close()

	// 6. papers/<safeRecordId>.json and 7. records/bucket_<00..15>.json
	var buckets [syncRecordBucketCount][]json.RawMessage
	recRows, err := tx.Query(`SELECT id, body, CASE WHEN json_extract(body,'$.paper')=1 THEN 1 ELSE 0 END FROM library_records ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	for recRows.Next() {
		var id string
		var bodyBytes []byte
		var isPaper int
		if err := recRows.Scan(&id, &bodyBytes, &isPaper); err != nil {
			recRows.Close()
			return nil, err
		}
		if id == "" || !json.Valid(bodyBytes) {
			recRows.Close()
			return nil, fmt.Errorf("记录 %q JSON 无效", id)
		}
		if isPaper == 1 {
			addShard("papers/"+paperShardFilename(id), "paper", bodyBytes, id)
		} else {
			bIdx := syncRecordBucket(id)
			buckets[bIdx] = append(buckets[bIdx], json.RawMessage(bodyBytes))
		}
	}
	if err := recRows.Err(); err != nil {
		recRows.Close()
		return nil, err
	}
	recRows.Close()

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	for i := 0; i < syncRecordBucketCount; i++ {
		if len(buckets[i]) == 0 {
			continue // Never upload empty buckets
		}
		b, err := json.Marshal(syncBucketPayload{Bucket: i, Records: buckets[i]})
		if err != nil {
			return nil, err
		}
		addShard(fmt.Sprintf("records/bucket_%02d.json", i), "records", b, "")
	}

	return snap, nil
}

func parseRemoteManifest(data []byte) (cloudSyncManifest, error) {
	var m cloudSyncManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("远端同步库清单 manifest.json 损坏: %w", err)
	}
	if m.SchemaVersion != 1 || m.SyncVersion < 1 || m.Shards == nil {
		return m, errors.New("远端同步库清单 manifest.json 格式无效")
	}
	return m, nil
}

func splitSyncRelShardPath(relPath string) (uploadDir, filename string, err error) {
	cleanRel, err := cleanCloudRelDir(relPath)
	if err != nil || cleanRel == "" {
		return "", "", fmt.Errorf("分片路径无效: %q", relPath)
	}
	dir := path.Dir(cleanRel)
	base := path.Base(cleanRel)
	cleanBase, err := cleanCloudFilename(base)
	if err != nil {
		return "", "", fmt.Errorf("分片文件名无效: %w", err)
	}
	if dir == "." || dir == "" {
		return cloudRelJoin(syncDirName), cleanBase, nil
	}
	return cloudRelJoin(syncDirName, dir), cleanBase, nil
}

func (e *cloudSyncEngine) check(ctx context.Context, explicitConnID string) (cloudSyncStatusResponse, error) {
	e.execMu.Lock()
	defer e.execMu.Unlock()

	row, _, provider, err := e.l.resolveSyncTarget(explicitConnID)
	if errors.Is(err, errCloudConnectionNotFound) {
		return e.buildStatusResponse("", "", false, nil), nil
	}
	if err != nil {
		return e.buildStatusResponse("", "", false, nil), err
	}
	rootDir := effectiveSyncRootDir(row.RootDir)
	now := time.Now().UTC().Format(time.RFC3339)

	var remote cloudSyncRemoteInfo
	remote.Checked = true
	remote.CheckedAt = now

	dl, dlErr := provider.Download(ctx, cloudRelJoin(syncDirName, syncManifestName))
	if dlErr != nil {
		if errors.Is(dlErr, errCloudNotFound) {
			remote.Exists = false
			remote.Empty = true
			remote.Error = ""
		} else {
			remote.Exists = false
			remote.Empty = false
			remote.Error = dlErr.Error()
		}
	} else {
		manifest, parseErr := parseRemoteManifest(dl.Data)
		if parseErr != nil {
			remote.Exists = true
			remote.Empty = false
			remote.Error = parseErr.Error()
		} else if len(manifest.Shards) == 0 {
			remote.Exists = false
			remote.Empty = true
			remote.Error = ""
		} else {
			remote.Exists = true
			remote.Empty = false
			remote.SyncVersion = manifest.SyncVersion
			remote.UpdatedAt = manifest.UpdatedAt
			remote.RecordCount = manifest.RecordCount
			remote.ShardCount = len(manifest.Shards)
			remote.Error = ""
		}
	}

	_ = e.l.updateConnSyncState(row.ID, rootDir, func(cs *cloudSyncConnState) {
		rc := remote
		cs.LastRemoteCheck = &rc
	})

	resp := e.buildStatusResponse(row.ID, rootDir, true, &remote)
	if remote.Error != "" {
		resp.Ok = false
	}
	return resp, nil
}

func (e *cloudSyncEngine) status(explicitConnID string) (cloudSyncStatusResponse, error) {
	row, _, _, err := e.l.resolveSyncTarget(explicitConnID)
	if errors.Is(err, errCloudConnectionNotFound) {
		return e.buildStatusResponse("", "", false, nil), nil
	}
	if err != nil {
		return e.buildStatusResponse("", "", false, nil), err
	}
	rootDir := effectiveSyncRootDir(row.RootDir)
	return e.buildStatusResponse(row.ID, rootDir, true, nil), nil
}

func (e *cloudSyncEngine) buildStatusResponse(connID, rootDir string, configured bool, overrideRemote *cloudSyncRemoteInfo) cloudSyncStatusResponse {
	resp := cloudSyncStatusResponse{
		Ok:           true,
		Configured:   configured,
		ConnectionID: connID,
		RootDir:      rootDir,
		Local:        cloudSyncLocalInfo{},
		Remote:       cloudSyncRemoteInfo{},
		Status:       "unconfigured",
	}

	e.mu.Lock()
	inMemDirty := e.dirty
	resp.Syncing = e.syncing
	resp.PendingAt = e.pendingAt
	e.mu.Unlock()

	fp, stats, fpErr := e.l.computeSyncLocalStats()
	if fpErr == nil {
		resp.Local.StateVersion = stats.stateVersion
		resp.Local.RecordCount = stats.recordCount
	}

	if !configured {
		resp.Dirty = inMemDirty || (fpErr == nil && stats.populated)
		resp.Status = "unconfigured"
		return resp
	}

	cs := e.l.getConnSyncState(connID, rootDir)
	resp.LastSyncAt = cs.LastSyncAt
	resp.LastSyncError = cs.LastSyncError
	if overrideRemote != nil {
		resp.Remote = *overrideRemote
	} else if cs.LastRemoteCheck != nil {
		resp.Remote = *cs.LastRemoteCheck
	}

	localPopulated := fpErr == nil && stats.populated
	localChanged := inMemDirty
	if fpErr == nil {
		if cs.LastSyncedSyncVersion == 0 {
			if localPopulated {
				localChanged = true
			}
		} else if fp != cs.LastSyncedFingerprint {
			localChanged = true
		}
	}
	resp.Dirty = localChanged
	if localChanged {
		resp.Local.SyncVersion = cs.LastSyncedSyncVersion + 1
	} else {
		resp.Local.SyncVersion = cs.LastSyncedSyncVersion
	}

	if resp.Remote.Checked && resp.Remote.Error != "" {
		resp.Ok = false
		resp.Status = "error"
		return resp
	}
	if !resp.Remote.Checked {
		if cs.LastSyncError != "" {
			resp.Status = "error"
		} else if localChanged {
			resp.Status = "local_ahead"
		} else {
			resp.Status = "idle"
		}
		return resp
	}
	if resp.Remote.Empty || !resp.Remote.Exists {
		if localPopulated || localChanged {
			resp.Status = "remote_empty_local_populated"
		} else {
			resp.Status = "idle"
		}
		return resp
	}

	remoteAhead := resp.Remote.SyncVersion != cs.LastSyncedSyncVersion
	switch {
	case localChanged && remoteAhead:
		resp.Status = "diverged"
	case localChanged && !remoteAhead:
		resp.Status = "local_ahead"
	case !localChanged && remoteAhead:
		resp.Status = "remote_ahead"
	default:
		resp.Status = "in_sync"
	}
	return resp
}

func (e *cloudSyncEngine) push(ctx context.Context, explicitConnID string, background bool) error {
	e.execMu.Lock()
	defer e.execMu.Unlock()

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return context.Canceled
	}
	if background && !e.dirty {
		e.mu.Unlock()
		return nil
	}
	snapSeq := e.dirtySeq
	e.mu.Unlock()

	row, _, provider, err := e.l.resolveSyncTarget(explicitConnID)
	if err != nil {
		if background && errors.Is(err, errCloudConnectionNotFound) {
			e.mu.Lock()
			if e.dirtySeq == snapSeq {
				e.dirty = false
				e.pendingAt = ""
			}
			e.mu.Unlock()
			return nil
		}
		if errors.Is(err, errCloudConnectionNotFound) {
			return &syncHTTPError{status: http.StatusBadRequest, msg: "尚未配置 Google Drive 连接"}
		}
		return err
	}
	rootDir := effectiveSyncRootDir(row.RootDir)
	connState := e.l.getConnSyncState(row.ID, rootDir)

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return context.Canceled
	}
	e.syncing = true
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.syncing = false
		e.mu.Unlock()
	}()

	recordFail := func(status int, cause error) error {
		msg := cause.Error()
		_ = e.l.updateConnSyncState(row.ID, rootDir, func(cs *cloudSyncConnState) {
			cs.LastSyncError = msg
		})
		e.mu.Lock()
		e.dirty = true
		e.mu.Unlock()
		return &syncHTTPError{status: status, msg: msg}
	}

	// Inspect remote manifest.json BEFORE building snapshot or uploading any shards.
	var remoteExists bool
	var remoteSyncVer int64
	var remoteShards map[string]syncShardMeta
	remoteCheck := cloudSyncRemoteInfo{
		Checked:   true,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}
	dl, dlErr := provider.Download(ctx, cloudRelJoin(syncDirName, syncManifestName))
	if dlErr != nil {
		if !errors.Is(dlErr, errCloudNotFound) {
			return recordFail(http.StatusBadGateway, dlErr)
		}
		remoteCheck.Exists = false
		remoteCheck.Empty = true
	} else {
		rm, pErr := parseRemoteManifest(dl.Data)
		if pErr != nil {
			if background {
				msg := pErr.Error()
				_ = e.l.updateConnSyncState(row.ID, rootDir, func(cs *cloudSyncConnState) {
					rc := remoteCheck
					rc.Exists = true
					rc.Empty = false
					rc.Error = msg
					cs.LastRemoteCheck = &rc
					cs.LastSyncError = msg
				})
				e.mu.Lock()
				e.dirty = true
				e.mu.Unlock()
				return &syncHTTPError{status: http.StatusUnprocessableEntity, msg: msg}
			}
		} else if len(rm.Shards) == 0 {
			remoteCheck.Exists = false
			remoteCheck.Empty = true
		} else {
			remoteExists = true
			remoteSyncVer = rm.SyncVersion
			remoteShards = rm.Shards
			remoteCheck.Exists = true
			remoteCheck.Empty = false
			remoteCheck.SyncVersion = rm.SyncVersion
			remoteCheck.UpdatedAt = rm.UpdatedAt
			remoteCheck.RecordCount = rm.RecordCount
			remoteCheck.ShardCount = len(rm.Shards)
			remoteCheck.Error = ""

			if background && remoteSyncVer != connState.LastSyncedSyncVersion {
				var msg string
				if remoteSyncVer > connState.LastSyncedSyncVersion {
					msg = fmt.Sprintf(
						"远端同步库已有更新版本（远端 v%d / %d 条，本地基准 v%d），已暂停自动上传以避免覆盖远端数据；请先执行同步恢复或手动推送。",
						remoteSyncVer, rm.RecordCount, connState.LastSyncedSyncVersion,
					)
				} else {
					msg = fmt.Sprintf(
						"远端同步库版本（v%d）与本地基准（v%d）不一致，已暂停自动上传以避免覆盖远端数据；请先执行同步恢复或手动推送。",
						remoteSyncVer, connState.LastSyncedSyncVersion,
					)
				}
				_ = e.l.updateConnSyncState(row.ID, rootDir, func(cs *cloudSyncConnState) {
					rc := remoteCheck
					cs.LastRemoteCheck = &rc
					cs.LastSyncError = msg
				})
				e.mu.Lock()
				e.dirty = true
				e.mu.Unlock()
				return &syncHTTPError{status: http.StatusConflict, msg: msg}
			}
		}
	}

	snap, err := e.l.buildSyncSnapshot(connState.LastSyncedShards)
	if err != nil {
		return recordFail(http.StatusInternalServerError, err)
	}

	// Determine shards to upload in deterministic order (manifest.json uploaded LAST).
	paths := make([]string, 0, len(snap.shards))
	for p := range snap.shards {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var toUpload []string
	for _, p := range paths {
		sh := snap.shards[p]
		if remoteExists {
			prevLocal, okL := connState.LastSyncedShards[p]
			prevRemote, okR := remoteShards[p]
			if okL && okR && prevLocal.Hash == sh.meta.Hash && prevLocal.Size == sh.meta.Size && prevRemote.Hash == sh.meta.Hash && prevRemote.Size == sh.meta.Size {
				continue
			}
		}
		toUpload = append(toUpload, p)
	}

	for _, p := range toUpload {
		if ctx.Err() != nil {
			return recordFail(http.StatusBadGateway, ctx.Err())
		}
		sh := snap.shards[p]
		data := sh.data
		if data == nil && sh.paperID != "" {
			var bodyBytes []byte
			if qErr := e.l.db.QueryRow(`SELECT body FROM library_records WHERE id=?`, sh.paperID).Scan(&bodyBytes); qErr != nil {
				return recordFail(http.StatusInternalServerError, qErr)
			}
			data = bodyBytes
		}
		upDir, fileName, splitErr := splitSyncRelShardPath(p)
		if splitErr != nil {
			return recordFail(http.StatusInternalServerError, splitErr)
		}
		if _, upErr := provider.Upload(ctx, upDir, fileName, data, "application/json", true); upErr != nil {
			return recordFail(http.StatusBadGateway, upErr)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	nextSyncVer := connState.LastSyncedSyncVersion
	if remoteSyncVer > nextSyncVer {
		nextSyncVer = remoteSyncVer
	}

	needManifest := !remoteExists || len(toUpload) > 0 || len(snap.manifestShards) != len(remoteShards)
	if !needManifest {
		for p := range snap.manifestShards {
			if _, ok := remoteShards[p]; !ok {
				needManifest = true
				break
			}
		}
	}

	if needManifest {
		nextSyncVer++
		manifest := cloudSyncManifest{
			SchemaVersion:   1,
			SyncVersion:     nextSyncVer,
			UpdatedAt:       now,
			StateVersion:    snap.stateVersion,
			RecordCount:     snap.recordCount,
			ShardCount:      len(snap.manifestShards),
			SeedStudentHash: snap.seedStudentHash,
			Shards:          snap.manifestShards,
		}
		mBytes, mErr := json.Marshal(manifest)
		if mErr != nil {
			return recordFail(http.StatusInternalServerError, mErr)
		}
		if _, upErr := provider.Upload(ctx, cloudRelJoin(syncDirName), syncManifestName, mBytes, "application/json", true); upErr != nil {
			return recordFail(http.StatusBadGateway, upErr)
		}
	} else if nextSyncVer == 0 {
		nextSyncVer = 1
	}

	remoteSnap := cloudSyncRemoteInfo{
		Checked:     true,
		CheckedAt:   now,
		Exists:      true,
		Empty:       false,
		SyncVersion: nextSyncVer,
		UpdatedAt:   now,
		RecordCount: snap.recordCount,
		ShardCount:  len(snap.manifestShards),
		Error:       "",
	}

	_ = e.l.updateConnSyncState(row.ID, rootDir, func(cs *cloudSyncConnState) {
		cs.LastSyncAt = now
		cs.LastSyncError = ""
		cs.LastSyncedSyncVersion = nextSyncVer
		cs.LastSyncedStateVersion = snap.stateVersion
		cs.LastSyncedRecordCount = snap.recordCount
		cs.LastSyncedFingerprint = snap.fingerprint
		cs.LastSyncedShards = snap.manifestShards
		rc := remoteSnap
		cs.LastRemoteCheck = &rc
	})

	e.mu.Lock()
	if e.dirtySeq == snapSeq {
		e.dirty = false
		e.pendingAt = ""
	}
	e.mu.Unlock()
	return nil
}

type decodedSyncBundle struct {
	state       syncStatePayload
	stateMap    map[string]any
	memberIDs   map[string]string
	favorites   []syncAccountMarksPayload
	personal    []syncAccountBodyPayload
	drafts      []reviewDraftBackup
	recordIDs   []string
	recordBytes [][]byte
}

func validateSyncShardRelPath(relPath string) (string, error) {
	clean, err := cleanCloudRelDir(relPath)
	if err != nil || clean == "" {
		return "", fmt.Errorf("同步分片路径无效: %q", relPath)
	}
	if clean == "state.json" || clean == "member_ids.json" {
		return clean, nil
	}
	parts := strings.Split(clean, "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".json") {
		return "", fmt.Errorf("同步分片路径不支持: %q", relPath)
	}
	switch parts[0] {
	case "favorites", "personal", "drafts", "papers", "records":
		return clean, nil
	default:
		return "", fmt.Errorf("同步分片目录不支持: %q", relPath)
	}
}

func decodeSyncBundle(manifest cloudSyncManifest, rawShards map[string][]byte) (*decodedSyncBundle, error) {
	bundle := &decodedSyncBundle{
		memberIDs: map[string]string{},
	}
	paths := make([]string, 0, len(manifest.Shards))
	for p := range manifest.Shards {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	hasState := false
	for _, p := range paths {
		data := rawShards[p]
		if !json.Valid(data) {
			return nil, fmt.Errorf("远端分片 %s 不是有效 JSON", p)
		}
		switch {
		case p == "state.json":
			var st syncStatePayload
			if err := json.Unmarshal(data, &st); err != nil {
				return nil, fmt.Errorf("state.json 解析失败: %w", err)
			}
			dec := json.NewDecoder(bytes.NewReader(st.Body))
			dec.UseNumber()
			var stMap map[string]any
			if err := dec.Decode(&stMap); err != nil || stMap == nil {
				return nil, errors.New("state.json 状态主体格式无效")
			}
			delete(stMap, "bag")
			delete(stMap, "editions")
			bundle.state = st
			bundle.stateMap = stMap
			hasState = true

		case p == "member_ids.json":
			var mids map[string]string
			if err := json.Unmarshal(data, &mids); err != nil {
				return nil, fmt.Errorf("member_ids.json 解析失败: %w", err)
			}
			for u, mid := range mids {
				if u != "" && mid != "" && mid != adminOwner {
					bundle.memberIDs[u] = mid
				}
			}

		case strings.HasPrefix(p, "favorites/"):
			var fav syncAccountMarksPayload
			if err := json.Unmarshal(data, &fav); err != nil {
				return nil, fmt.Errorf("%s 解析失败: %w", p, err)
			}
			var rawArr []json.RawMessage
			var arr []string
			if json.Unmarshal(fav.Marks, &rawArr) != nil || rawArr == nil || json.Unmarshal(fav.Marks, &arr) != nil || arr == nil {
				return nil, fmt.Errorf("%s marks 格式无效", p)
			}
			bundle.favorites = append(bundle.favorites, fav)

		case strings.HasPrefix(p, "personal/"):
			var per syncAccountBodyPayload
			if err := json.Unmarshal(data, &per); err != nil {
				return nil, fmt.Errorf("%s 解析失败: %w", p, err)
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(per.Body, &obj) != nil || obj == nil {
				return nil, fmt.Errorf("%s body 格式无效", p)
			}
			if rawBag, ok := obj["bag"]; ok {
				var bag []string
				if json.Unmarshal(rawBag, &bag) != nil || bag == nil {
					return nil, fmt.Errorf("%s bag 格式无效", p)
				}
			}
			if rawEd, ok := obj["editions"]; ok {
				var eds []json.RawMessage
				if json.Unmarshal(rawEd, &eds) != nil || eds == nil {
					return nil, fmt.Errorf("%s editions 格式无效", p)
				}
			}
			bundle.personal = append(bundle.personal, per)

		case strings.HasPrefix(p, "drafts/"):
			var dr reviewDraftBackup
			if err := json.Unmarshal(data, &dr); err != nil || dr.Paper == "" || dr.Version < 1 || !json.Valid(dr.Body) {
				return nil, fmt.Errorf("%s 草稿格式无效", p)
			}
			bundle.drafts = append(bundle.drafts, dr)

		case strings.HasPrefix(p, "papers/"):
			var head struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(data, &head) != nil || head.ID == "" {
				return nil, fmt.Errorf("%s 试卷记录缺少 id", p)
			}
			bundle.recordIDs = append(bundle.recordIDs, head.ID)
			bundle.recordBytes = append(bundle.recordBytes, data)

		case strings.HasPrefix(p, "records/"):
			var bkt syncBucketPayload
			if err := json.Unmarshal(data, &bkt); err != nil {
				return nil, fmt.Errorf("%s 分桶格式无效: %w", p, err)
			}
			for _, rawRec := range bkt.Records {
				var head struct {
					ID string `json:"id"`
				}
				if !json.Valid(rawRec) || json.Unmarshal(rawRec, &head) != nil || head.ID == "" {
					return nil, fmt.Errorf("%s 中包含无效记录", p)
				}
				bundle.recordIDs = append(bundle.recordIDs, head.ID)
				bundle.recordBytes = append(bundle.recordBytes, rawRec)
			}
		}
	}
	if !hasState {
		return nil, errors.New("远端同步库缺少 state.json，已保护本地数据")
	}
	return bundle, nil
}

func (e *cloudSyncEngine) restore(ctx context.Context, explicitConnID, mode string) (cloudSyncStatusResponse, int, error) {
	if mode == "" {
		mode = "merge"
	}
	if mode != "merge" && mode != "overwrite" {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusBadRequest, msg: "无效的恢复模式，仅支持 merge 或 overwrite"}
	}

	e.execMu.Lock()
	defer e.execMu.Unlock()

	row, _, provider, err := e.l.resolveSyncTarget(explicitConnID)
	if err != nil {
		if errors.Is(err, errCloudConnectionNotFound) {
			return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusBadRequest, msg: "尚未绑定 Google Drive 云盘"}
		}
		return cloudSyncStatusResponse{}, 0, err
	}
	rootDir := effectiveSyncRootDir(row.RootDir)
	prevConnState := e.l.getConnSyncState(row.ID, rootDir)

	// 1. Download and validate manifest.json (strict empty-remote guard).
	mRes, err := provider.Download(ctx, cloudRelJoin(syncDirName, syncManifestName))
	if err != nil {
		if errors.Is(err, errCloudNotFound) {
			return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusConflict, msg: "远端同步库为空，已保护本地数据不被清空或覆盖"}
		}
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusBadGateway, msg: err.Error()}
	}
	manifest, err := parseRemoteManifest(mRes.Data)
	if err != nil {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusUnprocessableEntity, msg: err.Error()}
	}
	if len(manifest.Shards) == 0 {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusConflict, msg: "远端同步库为空或未完成初始化，已保护本地数据"}
	}
	if _, ok := manifest.Shards["state.json"]; !ok {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusConflict, msg: "远端同步库缺少核心状态分片，已保护本地数据"}
	}

	// 2. Seed compatibility guard (when both non-empty).
	localSeedHash := syncSeedStudentHash(e.l.seed())
	if manifest.SeedStudentHash != "" && localSeedHash != "" && manifest.SeedStudentHash != localSeedHash {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusBadRequest, msg: "内置样卷不一致，请使用导出备份时的题库页面恢复。"}
	}

	// 3. Validate all shard paths & download + verify SHA-256 + JSON BEFORE opening SQLite tx.
	shardPaths := make([]string, 0, len(manifest.Shards))
	for p, meta := range manifest.Shards {
		cleanP, vErr := validateSyncShardRelPath(p)
		if vErr != nil || cleanP != p {
			return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusBadRequest, msg: fmt.Sprintf("远端分片路径无效: %s", p)}
		}
		if len(meta.Hash) != 64 || meta.Size < 0 {
			return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusUnprocessableEntity, msg: fmt.Sprintf("远端分片 %s 校验元数据无效", p)}
		}
		shardPaths = append(shardPaths, p)
	}
	sort.Strings(shardPaths)

	rawShards := make(map[string][]byte, len(shardPaths))
	for _, p := range shardPaths {
		meta := manifest.Shards[p]
		dl, dlErr := provider.Download(ctx, cloudRelJoin(syncDirName, p))
		if dlErr != nil {
			if errors.Is(dlErr, errCloudNotFound) {
				return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusConflict, msg: fmt.Sprintf("远端分片缺失（%s），已中止恢复以保护本地数据", p)}
			}
			return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusBadGateway, msg: dlErr.Error()}
		}
		if int64(len(dl.Data)) != meta.Size || syncSHA256Hex(dl.Data) != strings.ToLower(meta.Hash) {
			return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusUnprocessableEntity, msg: fmt.Sprintf("远端分片 %s 校验失败（SHA-256 或大小不匹配），已中止恢复", p)}
		}
		rawShards[p] = dl.Data
	}

	bundle, err := decodeSyncBundle(manifest, rawShards)
	if err != nil {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusUnprocessableEntity, msg: err.Error()}
	}

	// 4. Transactional SQLite apply (never touching library_cloud_connections).
	tx, err := e.l.db.Begin()
	if err != nil {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusInternalServerError, msg: "打开数据库事务失败"}
	}
	defer tx.Rollback()

	localHasPreexisting, err := txHasPreexistingLibraryData(tx)
	if err != nil {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusInternalServerError, msg: "检查本地数据库状态失败"}
	}

	if mode == "overwrite" {
		if err := applySyncOverwriteTx(tx, bundle); err != nil {
			return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusInternalServerError, msg: err.Error()}
		}
	} else {
		if err := applySyncMergeTx(tx, bundle, localHasPreexisting); err != nil {
			return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusInternalServerError, msg: err.Error()}
		}
	}

	if err := tx.Commit(); err != nil {
		return cloudSyncStatusResponse{}, 0, &syncHTTPError{status: http.StatusInternalServerError, msg: "提交恢复事务失败"}
	}

	// 5. Post-commit cache invalidation & offline archive update (NEVER call markSyncDirty here).
	e.l.bumpRecords()
	e.l.markOfflineDirty()

	fp, stats, _ := e.l.computeSyncLocalStats()
	now := time.Now().UTC().Format(time.RFC3339)
	remoteSnap := cloudSyncRemoteInfo{
		Checked:     true,
		CheckedAt:   now,
		Exists:      true,
		Empty:       false,
		SyncVersion: manifest.SyncVersion,
		UpdatedAt:   manifest.UpdatedAt,
		RecordCount: manifest.RecordCount,
		ShardCount:  len(manifest.Shards),
		Error:       "",
	}

	cleanMatch := mode == "overwrite" || !localHasPreexisting || (prevConnState.LastSyncedFingerprint != "" && fp == prevConnState.LastSyncedFingerprint)

	_ = e.l.updateConnSyncState(row.ID, rootDir, func(cs *cloudSyncConnState) {
		cs.LastSyncAt = now
		cs.LastSyncError = ""
		cs.LastSyncedSyncVersion = manifest.SyncVersion
		cs.LastSyncedShards = manifest.Shards
		rc := remoteSnap
		cs.LastRemoteCheck = &rc
		if cleanMatch {
			cs.LastSyncedStateVersion = stats.stateVersion
			cs.LastSyncedRecordCount = stats.recordCount
			cs.LastSyncedFingerprint = fp
		}
	})

	e.mu.Lock()
	e.dirtySeq++
	if cleanMatch {
		e.dirty = false
		e.pendingAt = ""
	} else {
		e.dirty = true
	}
	e.mu.Unlock()

	resp := e.buildStatusResponse(row.ID, rootDir, true, &remoteSnap)
	return resp, len(bundle.recordIDs), nil
}

func txHasPreexistingLibraryData(tx *sql.Tx) (bool, error) {
	var recCnt int
	if err := tx.QueryRow(`SELECT count(*) FROM library_records`).Scan(&recCnt); err != nil {
		return false, err
	}
	if recCnt > 0 {
		return true, nil
	}
	var stVer int
	var stBody string
	if err := tx.QueryRow(`SELECT version, body FROM library_state WHERE id=1`).Scan(&stVer, &stBody); err != nil {
		return false, err
	}
	if stVer > 0 {
		return true, nil
	}
	if _, nonEmpty, err := canonicalStateObjectWithoutPersonal([]byte(stBody)); err == nil && nonEmpty {
		return true, nil
	}
	var favNonEmpty int
	if err := tx.QueryRow(`SELECT count(*) FROM library_favorites WHERE json_array_length(marks) > 0`).Scan(&favNonEmpty); err == nil && favNonEmpty > 0 {
		return true, nil
	}
	var perNonEmpty int
	if err := tx.QueryRow(`SELECT count(*) FROM library_personal WHERE json_array_length(body,'$.bag') > 0 OR json_array_length(body,'$.editions') > 0`).Scan(&perNonEmpty); err == nil && perNonEmpty > 0 {
		return true, nil
	}
	var draftCnt int
	if err := tx.QueryRow(`SELECT count(*) FROM library_review_drafts`).Scan(&draftCnt); err == nil && draftCnt > 0 {
		return true, nil
	}
	return false, nil
}

func applySyncOverwriteTx(tx *sql.Tx, bundle *decodedSyncBundle) error {
	if _, err := tx.Exec(`DELETE FROM library_records`); err != nil {
		return err
	}
	for i, rid := range bundle.recordIDs {
		if _, err := tx.Exec(`INSERT INTO library_records(id, body) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`, rid, string(bundle.recordBytes[i])); err != nil {
			return err
		}
	}
	stateBodyBytes, err := json.Marshal(bundle.stateMap)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE library_state SET body=json_remove(?, '$.bag', '$.editions'), version=? WHERE id=1`, string(stateBodyBytes), bundle.state.Version); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM library_favorites`); err != nil {
		return err
	}
	for _, fav := range bundle.favorites {
		if _, err := tx.Exec(`INSERT INTO library_favorites(account, marks) VALUES(?, ?) ON CONFLICT(account) DO UPDATE SET marks=excluded.marks`, fav.Account, string(fav.Marks)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM library_personal`); err != nil {
		return err
	}
	for _, per := range bundle.personal {
		if _, err := tx.Exec(`INSERT INTO library_personal(account, body) VALUES(?, ?) ON CONFLICT(account) DO UPDATE SET body=excluded.body`, per.Account, string(per.Body)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM library_review_drafts`); err != nil {
		return err
	}
	for _, dr := range bundle.drafts {
		if _, err := tx.Exec(`INSERT INTO library_review_drafts(account, paper, version, body) VALUES(?, ?, ?, ?) ON CONFLICT(account, paper) DO UPDATE SET version=excluded.version, body=excluded.body`, dr.Account, dr.Paper, dr.Version, string(dr.Body)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM library_member_ids`); err != nil {
		return err
	}
	for u, mid := range bundle.memberIDs {
		if u != "" && mid != "" && mid != adminOwner {
			if _, err := tx.Exec(`INSERT INTO library_member_ids(username, id) VALUES(?, ?) ON CONFLICT(username) DO UPDATE SET id=excluded.id`, u, mid); err != nil {
				return err
			}
		}
	}
	return nil
}

func applySyncMergeTx(tx *sql.Tx, bundle *decodedSyncBundle, localHasPreexisting bool) error {
	for i, rid := range bundle.recordIDs {
		if _, err := tx.Exec(`INSERT INTO library_records(id, body) VALUES(?, ?) ON CONFLICT(id) DO NOTHING`, rid, string(bundle.recordBytes[i])); err != nil {
			return err
		}
	}

	var localVer int
	var localStateRaw string
	if err := tx.QueryRow(`SELECT version, body FROM library_state WHERE id=1`).Scan(&localVer, &localStateRaw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(localStateRaw)))
	dec.UseNumber()
	var localStateMap map[string]any
	if err := dec.Decode(&localStateMap); err != nil || localStateMap == nil {
		localStateMap = map[string]any{}
	}

	if !localHasPreexisting {
		stateBodyBytes, err := json.Marshal(bundle.stateMap)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE library_state SET body=json_remove(?, '$.bag', '$.editions'), version=? WHERE id=1`, string(stateBodyBytes), bundle.state.Version); err != nil {
			return err
		}
	} else {
		mergedState := mergeStateMaps(localStateMap, bundle.stateMap)
		mergedBytes, err := json.Marshal(mergedState)
		if err != nil {
			return err
		}
		nextVer := localVer
		if bundle.state.Version > nextVer {
			nextVer = bundle.state.Version
		}
		localCanon, _ := json.Marshal(localStateMap)
		if !bytes.Equal(localCanon, mergedBytes) && nextVer <= localVer {
			nextVer = localVer + 1
		}
		if _, err := tx.Exec(`UPDATE library_state SET body=json_remove(?, '$.bag', '$.editions'), version=? WHERE id=1`, string(mergedBytes), nextVer); err != nil {
			return err
		}
	}

	// Merge favorites per account (including v0.9.4 legacy $.good fallback for admin "").
	hasRemoteAdminFav := false
	for _, fav := range bundle.favorites {
		if fav.Account == "" {
			hasRemoteAdminFav = true
		}
		var localMarksRaw string
		var localMarks []string
		err := tx.QueryRow(`SELECT marks FROM library_favorites WHERE account=?`, fav.Account).Scan(&localMarksRaw)
		if err == sql.ErrNoRows {
			if fav.Account == "" {
				localMarks = anySliceToStrings(localStateMap["good"])
			}
		} else if err != nil {
			return err
		} else {
			_ = json.Unmarshal([]byte(localMarksRaw), &localMarks)
		}
		var remoteMarks []string
		_ = json.Unmarshal(fav.Marks, &remoteMarks)
		merged := unionStringSlices(localMarks, remoteMarks)
		mergedJSON, _ := json.Marshal(merged)
		if _, err := tx.Exec(`INSERT INTO library_favorites(account, marks) VALUES(?, ?) ON CONFLICT(account) DO UPDATE SET marks=excluded.marks`, fav.Account, string(mergedJSON)); err != nil {
			return err
		}
	}
	if !hasRemoteAdminFav {
		if remoteLegacyGood := anySliceToStrings(bundle.stateMap["good"]); len(remoteLegacyGood) > 0 {
			var localMarksRaw string
			err := tx.QueryRow(`SELECT marks FROM library_favorites WHERE account=''`).Scan(&localMarksRaw)
			if err == nil {
				var localMarks []string
				_ = json.Unmarshal([]byte(localMarksRaw), &localMarks)
				merged := unionStringSlices(localMarks, remoteLegacyGood)
				mergedJSON, _ := json.Marshal(merged)
				if _, err := tx.Exec(`UPDATE library_favorites SET marks=? WHERE account=''`, string(mergedJSON)); err != nil {
					return err
				}
			} else if err != sql.ErrNoRows {
				return err
			}
		}
	}

	// Merge personal bag & editions per account.
	for _, per := range bundle.personal {
		var localPerRaw string
		err := tx.QueryRow(`SELECT body FROM library_personal WHERE account=?`, per.Account).Scan(&localPerRaw)
		if err == sql.ErrNoRows {
			if _, err := tx.Exec(`INSERT INTO library_personal(account, body) VALUES(?, ?)`, per.Account, string(per.Body)); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		mergedPer := mergePersonalJSON([]byte(localPerRaw), per.Body)
		if _, err := tx.Exec(`UPDATE library_personal SET body=? WHERE account=?`, string(mergedPer), per.Account); err != nil {
			return err
		}
	}

	// Merge review drafts (higher version wins).
	for _, dr := range bundle.drafts {
		if _, err := tx.Exec(
			`INSERT INTO library_review_drafts(account, paper, version, body) VALUES(?, ?, ?, ?)
			 ON CONFLICT(account, paper) DO UPDATE SET version=excluded.version, body=excluded.body
			 WHERE excluded.version > library_review_drafts.version`,
			dr.Account, dr.Paper, dr.Version, string(dr.Body),
		); err != nil {
			return err
		}
	}

	// Merge member IDs (preserve existing mappings).
	for u, mid := range bundle.memberIDs {
		if u != "" && mid != "" && mid != adminOwner {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO library_member_ids(username, id) VALUES(?, ?)`, u, mid); err != nil {
				return err
			}
		}
	}

	return nil
}

func mergeStateMaps(localMap, remoteMap map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range localMap {
		if k == "bag" || k == "editions" {
			continue
		}
		out[k] = v
	}

	stringSetFields := map[string]bool{
		"wrong":            true,
		"purposeNames":     true,
		"examPurposes":     true,
		"deletedQuestions": true,
		"deletedFormats":   true,
		"purgedQuestions":  true,
		"purgedPapers":     true,
		"good":             true,
	}
	objectArrayByID := map[string]bool{
		"deletedPapers":  true,
		"suggestions":    true,
		"extraQuestions": true,
	}
	mapFields := map[string]bool{
		"aliases":       true,
		"paperNotes":    true,
		"paperTags":     true,
		"formatAliases": true,
	}

	for k, rv := range remoteMap {
		if k == "bag" || k == "editions" || !allowedStateFields[k] {
			continue
		}
		lv, exists := out[k]
		switch {
		case stringSetFields[k]:
			out[k] = unionStringSlices(anySliceToStrings(lv), anySliceToStrings(rv))
		case objectArrayByID[k]:
			out[k] = unionObjectSlicesByID(lv, rv)
		case mapFields[k]:
			out[k] = mergeSimpleMaps(lv, rv)
		case k == "fixes":
			out[k] = mergeFixesMaps(lv, rv)
		case k == "clearsSavedBasket":
			lb, _ := lv.(bool)
			rb, _ := rv.(bool)
			out[k] = lb || rb
		default:
			if !exists {
				out[k] = rv
			}
		}
	}
	return out
}

func anySliceToStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func unionStringSlices(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func unionObjectSlicesByID(localVal, remoteVal any) []any {
	la, _ := localVal.([]any)
	ra, _ := remoteVal.([]any)
	seen := make(map[string]bool, len(la)+len(ra))
	out := make([]any, 0, len(la)+len(ra))
	appendList := func(items []any) {
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				if id, _ := m["id"].(string); id != "" {
					if seen[id] {
						continue
					}
					seen[id] = true
				}
			}
			out = append(out, item)
		}
	}
	appendList(la)
	appendList(ra)
	return out
}

func mergeSimpleMaps(localVal, remoteVal any) map[string]any {
	lm, _ := localVal.(map[string]any)
	rm, _ := remoteVal.(map[string]any)
	out := map[string]any{}
	for k, v := range lm {
		out[k] = v
	}
	for k, v := range rm {
		if cur, ok := out[k]; !ok || cur == nil || cur == "" {
			out[k] = v
		}
	}
	return out
}

func mergeFixesMaps(localVal, remoteVal any) map[string]any {
	lm, _ := localVal.(map[string]any)
	rm, _ := remoteVal.(map[string]any)
	out := map[string]any{}
	for qid, v := range lm {
		if qm, ok := v.(map[string]any); ok {
			cp := make(map[string]any, len(qm))
			for fk, fv := range qm {
				cp[fk] = fv
			}
			out[qid] = cp
		} else {
			out[qid] = v
		}
	}
	for qid, rv := range rm {
		rqm, rOk := rv.(map[string]any)
		lqm, lOk := out[qid].(map[string]any)
		if !lOk || !rOk {
			if _, exists := out[qid]; !exists {
				out[qid] = rv
			}
			continue
		}
		for fk, fv := range rqm {
			if cur, exists := lqm[fk]; !exists || cur == nil || cur == "" {
				lqm[fk] = fv
			}
		}
	}
	return out
}

func mergePersonalJSON(localRaw, remoteRaw []byte) []byte {
	type personalDoc struct {
		Bag      []string `json:"bag"`
		Editions []any    `json:"editions"`
	}
	var lp, rp personalDoc
	decL := json.NewDecoder(bytes.NewReader(localRaw))
	decL.UseNumber()
	_ = decL.Decode(&lp)
	decR := json.NewDecoder(bytes.NewReader(remoteRaw))
	decR.UseNumber()
	_ = decR.Decode(&rp)

	merged := personalDoc{
		Bag:      unionStringSlices(lp.Bag, rp.Bag),
		Editions: unionObjectSlicesByID(toAnySlice(lp.Editions), toAnySlice(rp.Editions)),
	}
	if merged.Bag == nil {
		merged.Bag = []string{}
	}
	if merged.Editions == nil {
		merged.Editions = []any{}
	}
	out, _ := json.Marshal(merged)
	return out
}

func toAnySlice(in []any) []any {
	if in == nil {
		return nil
	}
	return in
}

func (l *libraryPage) serveCloudSyncStatus(w http.ResponseWriter, r *http.Request) {
	connID := strings.TrimSpace(r.URL.Query().Get("connectionId"))
	resp, err := l.sync().status(connID)
	if err != nil {
		http.Error(w, err.Error(), syncErrorStatus(err, http.StatusInternalServerError))
		return
	}
	jsonResponse(w, resp)
}

func (l *libraryPage) serveCloudSyncCheck(w http.ResponseWriter, r *http.Request, isAdmin bool, body []byte) {
	if !isAdmin {
		http.Error(w, "只有管理员可执行云端同步检查", http.StatusForbidden)
		return
	}
	var in struct {
		ConnectionID string `json:"connectionId"`
	}
	if len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &in) != nil {
		http.Error(w, "请求参数格式无效", http.StatusBadRequest)
		return
	}
	resp, err := l.sync().check(r.Context(), strings.TrimSpace(in.ConnectionID))
	if err != nil {
		http.Error(w, err.Error(), syncErrorStatus(err, http.StatusInternalServerError))
		return
	}
	jsonResponse(w, resp)
}

func (l *libraryPage) serveCloudSyncPush(w http.ResponseWriter, r *http.Request, isAdmin bool, body []byte) {
	if !isAdmin {
		http.Error(w, "只有管理员可执行云端同步上传", http.StatusForbidden)
		return
	}
	var in struct {
		ConnectionID string `json:"connectionId"`
	}
	if len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &in) != nil {
		http.Error(w, "请求参数格式无效", http.StatusBadRequest)
		return
	}
	connID := strings.TrimSpace(in.ConnectionID)
	if err := l.sync().push(r.Context(), connID, false); err != nil {
		http.Error(w, err.Error(), syncErrorStatus(err, http.StatusBadGateway))
		return
	}
	resp, err := l.sync().status(connID)
	if err != nil {
		http.Error(w, err.Error(), syncErrorStatus(err, http.StatusInternalServerError))
		return
	}
	jsonResponse(w, resp)
}

func (l *libraryPage) serveCloudSyncRestore(w http.ResponseWriter, r *http.Request, isAdmin bool, body []byte) {
	if !isAdmin {
		http.Error(w, "只有管理员可从 Google Drive 同步恢复题库", http.StatusForbidden)
		return
	}
	var in struct {
		ConnectionID string `json:"connectionId"`
		Mode         string `json:"mode"`
	}
	if len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &in) != nil {
		http.Error(w, "恢复参数格式无效", http.StatusBadRequest)
		return
	}
	mode := strings.TrimSpace(strings.ToLower(in.Mode))
	if mode == "" {
		mode = "merge"
	}
	resp, restoredCount, err := l.sync().restore(r.Context(), strings.TrimSpace(in.ConnectionID), mode)
	if err != nil {
		http.Error(w, err.Error(), syncErrorStatus(err, http.StatusInternalServerError))
		return
	}
	jsonResponse(w, map[string]any{
		"ok":              resp.Ok,
		"mode":            mode,
		"restoredRecords": restoredCount,
		"configured":      resp.Configured,
		"connectionId":    resp.ConnectionID,
		"rootDir":         resp.RootDir,
		"syncing":         resp.Syncing,
		"dirty":           resp.Dirty,
		"pendingAt":       resp.PendingAt,
		"lastSyncAt":      resp.LastSyncAt,
		"lastSyncError":   resp.LastSyncError,
		"local":           resp.Local,
		"remote":          resp.Remote,
		"status":          resp.Status,
	})
}
