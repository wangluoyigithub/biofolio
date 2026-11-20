package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	googleAuthURL         = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL        = "https://oauth2.googleapis.com/token"
	googleDriveAPIBase    = "https://www.googleapis.com/drive/v3"
	googleDriveUploadBase = "https://www.googleapis.com/upload/drive/v3"
)

const (
	googleDriveScope          = "https://www.googleapis.com/auth/drive"
	googleManualRedirect      = "http://localhost:1"
	googleDocMime             = "application/vnd.google-apps.document"
	googleSheetMime           = "application/vnd.google-apps.spreadsheet"
	googleSlidesMime          = "application/vnd.google-apps.presentation"
	googleFolderMime          = "application/vnd.google-apps.folder"
	docxMimeType              = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	xlsxMimeType              = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	pdfMimeType               = "application/pdf"
	defaultGDriveRelRootDir   = "Biofolio"
	gdriveFolderCacheTTL      = 30 * time.Second
	gdriveMaxTransientRetries = 3
	gdriveMaxRetryAfter       = 5 * time.Second
)

var gdriveRetryBaseDelay = 50 * time.Millisecond

var googleDriveIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,128}$`)

var googleTokenRefreshLocks sync.Map // connID -> *sync.Mutex

type gdriveFolderCacheEntry struct {
	id        string
	expiresAt time.Time
}

type gdriveConnFolderState struct {
	mu    sync.Mutex
	cache map[string]gdriveFolderCacheEntry // key: parentID + "\x00" + segName
}

var googleFolderStates sync.Map // googleDriveAPIBase + "\x00" + connKey -> *gdriveConnFolderState

var errGDriveFolderTrashed = errors.New("Google Drive 目录已被移入回收站或不存在")

type gdriveHTTPError struct {
	status int
	msg    string
}

func (e *gdriveHTTPError) Error() string {
	return e.msg
}

func gdriveErrorStatus(err error) int {
	var he *gdriveHTTPError
	if errors.As(err, &he) {
		return he.status
	}
	return 0
}

func isGDriveStaleFolderErr(err error) bool {
	return gdriveErrorStatus(err) == http.StatusNotFound || errors.Is(err, errGDriveFolderTrashed)
}

func invalidateGDriveFolderCache(connID string) {
	connID = strings.TrimSpace(connID)
	if connID == "" {
		return
	}
	suffix := "\x00" + connID
	googleFolderStates.Range(func(k, v any) bool {
		ks, ok := k.(string)
		if ok && strings.HasSuffix(ks, suffix) {
			st := v.(*gdriveConnFolderState)
			st.mu.Lock()
			st.cache = nil
			st.mu.Unlock()
		}
		return true
	})
}

func lockForGoogleConn(connID string) *sync.Mutex {
	mu, _ := googleTokenRefreshLocks.LoadOrStore(connID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

type gdriveSecret struct {
	RefreshToken       string `json:"refreshToken"`
	AccessToken        string `json:"accessToken"`
	ExpiryUnix         int64  `json:"expiryUnix"`
	Email              string `json:"email"`
	CustomClientID     string `json:"customClientId,omitempty"`
	CustomClientSecret string `json:"customClientSecret,omitempty"`
}

type gdriveTokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type gdriveFileJSON struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	Size         string   `json:"size"`
	ModifiedTime string   `json:"modifiedTime"`
	Parents      []string `json:"parents"`
	Trashed      bool     `json:"trashed"`
}

type gdriveProvider struct {
	l      *libraryPage
	cfg    cloudAdminSettings
	row    cloudConnectionRow
	secMu  sync.Mutex
	sec    gdriveSecret
	client *http.Client
}

func (p *gdriveProvider) folderStateKey() string {
	connKey := strings.TrimSpace(p.row.ID)
	if connKey == "" {
		connKey = p.row.Account + "\x00" + p.row.RootDir
	}
	return googleDriveAPIBase + "\x00" + connKey
}

func (p *gdriveProvider) folderState() *gdriveConnFolderState {
	key := p.folderStateKey()
	if v, ok := googleFolderStates.Load(key); ok {
		return v.(*gdriveConnFolderState)
	}
	st := &gdriveConnFolderState{}
	actual, _ := googleFolderStates.LoadOrStore(key, st)
	return actual.(*gdriveConnFolderState)
}

func (p *gdriveProvider) clearFolderCache() {
	st := p.folderState()
	st.mu.Lock()
	st.cache = nil
	st.mu.Unlock()
}

func validGoogleDriveID(id string) bool {
	return id == "root" || googleDriveIDPattern.MatchString(id)
}

func escapeDriveQuery(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, `'`, `\'`)
}

func generatePKCEVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func resolveGoogleOAuthClient(l *libraryPage, cfg cloudAdminSettings, customID, customSecret string) (clientID, clientSecret, redirectURI string, err error) {
	key, err := l.getCloudKey()
	if err != nil {
		return "", "", "", err
	}
	sharedID, sharedSecret, sharedRedirect, err := cfg.effectiveGoogleCreds(key)
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(customID) != "" {
		return strings.TrimSpace(customID), strings.TrimSpace(customSecret), sharedRedirect, nil
	}
	if sharedID == "" {
		return "", "", "", errors.New("尚未配置 Google OAuth Client ID：请联系管理员在「设置 → 云盘接入服务」配置，或填写自己的应用凭证")
	}
	return sharedID, sharedSecret, sharedRedirect, nil
}

func buildGoogleAuthURL(clientID, redirectURI, state, codeVerifier string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", googleDriveScope)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("include_granted_scopes", "true")
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(codeVerifier))
	q.Set("code_challenge_method", "S256")
	return googleAuthURL + "?" + q.Encode()
}

func extractCodeAndStateFromInput(raw string) (code, state string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("请粘贴授权后的跳转链接或授权码")
	}
	if strings.Contains(raw, "://") || strings.Contains(raw, "code=") {
		if !strings.Contains(raw, "://") && strings.HasPrefix(raw, "?") {
			raw = "http://localhost" + raw
		} else if !strings.Contains(raw, "://") {
			raw = "http://localhost/?" + strings.TrimPrefix(raw, "?")
		}
		u, parseErr := url.Parse(raw)
		if parseErr != nil {
			return "", "", errors.New("无法解析粘贴的授权链接")
		}
		q := u.Query()
		if oauthErr := q.Get("error"); oauthErr != "" {
			return "", "", fmt.Errorf("Google 授权未完成：%s", oauthErr)
		}
		code = strings.TrimSpace(q.Get("code"))
		state = strings.TrimSpace(q.Get("state"))
		if code == "" {
			return "", "", errors.New("粘贴的链接中未包含 code 授权参数，请复制完整跳转链接")
		}
		return code, state, nil
	}
	return raw, "", nil
}

func exchangeGoogleCode(ctx context.Context, client *http.Client, clientID, clientSecret, code, redirectURI, codeVerifier string) (gdriveTokenReply, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	return postGoogleToken(ctx, client, form)
}

func postGoogleToken(ctx context.Context, client *http.Client, form url.Values) (gdriveTokenReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return gdriveTokenReply{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return gdriveTokenReply{}, fmt.Errorf("连接 Google 认证服务失败（如在国内 NAS 请检查代理设置）：%w", err)
	}
	defer resp.Body.Close()
	body, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return gdriveTokenReply{}, err
	}
	var tok gdriveTokenReply
	if json.Unmarshal(body, &tok) != nil {
		return gdriveTokenReply{}, fmt.Errorf("Google 返回无效令牌响应（%s）", resp.Status)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tok.Error != "" {
		msg := tok.ErrorDesc
		if msg == "" {
			msg = tok.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return gdriveTokenReply{}, fmt.Errorf("Google 授权失败：%s", msg)
	}
	if tok.AccessToken == "" {
		return gdriveTokenReply{}, errors.New("Google 未返回访问令牌")
	}
	return tok, nil
}

func fetchGoogleAboutEmail(ctx context.Context, client *http.Client, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleDriveAPIBase+"/about?fields=user(emailAddress,displayName)", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var info struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
			DisplayName  string `json:"displayName"`
		} `json:"user"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info) != nil {
		return ""
	}
	if info.User.EmailAddress != "" {
		return info.User.EmailAddress
	}
	return info.User.DisplayName
}

func newGDriveProvider(l *libraryPage, cfg cloudAdminSettings, row cloudConnectionRow, sec gdriveSecret, isAdmin bool) (*gdriveProvider, error) {
	client, err := newCloudHTTPClient(cfg, "gdrive", true, false, false, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	return &gdriveProvider{
		l:      l,
		cfg:    cfg,
		row:    row,
		sec:    sec,
		client: client,
	}, nil
}

func isGDriveRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func parseGDriveRetryAfter(hdr string) (time.Duration, bool) {
	hdr = strings.TrimSpace(hdr)
	if hdr == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(hdr); err == nil {
		if secs <= 0 {
			return 0, true
		}
		d := time.Duration(secs) * time.Second
		if d > gdriveMaxRetryAfter {
			d = gdriveMaxRetryAfter
		}
		return d, true
	}
	if t, err := http.ParseTime(hdr); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0, true
		}
		if d > gdriveMaxRetryAfter {
			d = gdriveMaxRetryAfter
		}
		return d, true
	}
	return 0, false
}

func gdriveRetryDelay(retryAfter string, attempt int) time.Duration {
	if d, ok := parseGDriveRetryAfter(retryAfter); ok {
		return d
	}
	d := gdriveRetryBaseDelay << attempt
	if d > gdriveMaxRetryAfter {
		d = gdriveMaxRetryAfter
	}
	return d
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *gdriveProvider) ensureToken(ctx context.Context) (string, error) {
	return p.ensureTokenWithFailed(ctx, "")
}

func (p *gdriveProvider) forceRefreshToken(ctx context.Context, failedToken string) (string, error) {
	return p.ensureTokenWithFailed(ctx, failedToken)
}

func (p *gdriveProvider) ensureTokenWithFailed(ctx context.Context, failedToken string) (string, error) {
	tokenFresh := func(sec gdriveSecret) bool {
		if sec.AccessToken == "" || sec.ExpiryUnix <= time.Now().Unix()+60 {
			return false
		}
		if failedToken != "" && sec.AccessToken == failedToken {
			return false
		}
		return true
	}

	p.secMu.Lock()
	if tokenFresh(p.sec) {
		tok := p.sec.AccessToken
		p.secMu.Unlock()
		return tok, nil
	}
	p.secMu.Unlock()

	mu := lockForGoogleConn(p.row.ID)
	mu.Lock()
	defer mu.Unlock()

	// Re-check in-memory secret in case a concurrent goroutine sharing this provider refreshed it.
	p.secMu.Lock()
	if tokenFresh(p.sec) {
		tok := p.sec.AccessToken
		p.secMu.Unlock()
		return tok, nil
	}
	p.secMu.Unlock()

	// Reload latest secret from DB in case another request refreshed it.
	if p.l != nil && p.row.ID != "" {
		if _, plain, err := p.l.getCloudConnection(p.row.Account, p.row.ID); err == nil {
			var latest gdriveSecret
			if json.Unmarshal(plain, &latest) == nil && tokenFresh(latest) {
				p.secMu.Lock()
				p.sec = latest
				tok := p.sec.AccessToken
				p.secMu.Unlock()
				return tok, nil
			}
		}
	}

	p.secMu.Lock()
	refreshToken := p.sec.RefreshToken
	customClientID := p.sec.CustomClientID
	customClientSecret := p.sec.CustomClientSecret
	p.secMu.Unlock()
	if refreshToken == "" {
		return "", errors.New("Google Drive 授权已过期，请在设置中重新授权")
	}
	clientID, clientSecret, _, err := resolveGoogleOAuthClient(p.l, p.cfg, customClientID, customClientSecret)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")
	tok, err := postGoogleToken(ctx, p.client, form)
	if err != nil {
		return "", err
	}
	p.secMu.Lock()
	p.sec.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		p.sec.RefreshToken = tok.RefreshToken
	}
	exp := tok.ExpiresIn
	if exp <= 0 {
		exp = 3500
	}
	p.sec.ExpiryUnix = time.Now().Unix() + exp
	needEmail := p.sec.Email == ""
	accessToken := p.sec.AccessToken
	p.secMu.Unlock()

	if needEmail {
		if email := fetchGoogleAboutEmail(ctx, p.client, accessToken); email != "" {
			p.secMu.Lock()
			p.sec.Email = email
			p.secMu.Unlock()
		}
	}
	p.secMu.Lock()
	secCopy := p.sec
	p.secMu.Unlock()
	if p.l != nil && p.row.ID != "" {
		if enc, err := json.Marshal(secCopy); err == nil {
			_ = p.l.updateCloudConnectionSecret(p.row.Account, p.row.ID, enc, secCopy.Email)
		}
	}
	return accessToken, nil
}

func (p *gdriveProvider) doAuth(ctx context.Context, method, rawURL string, body io.Reader, contentType string) (*http.Response, error) {
	return p.doAuthWithHeaders(ctx, method, rawURL, body, contentType, nil)
}

func (p *gdriveProvider) doAuthWithHeaders(ctx context.Context, method, rawURL string, body io.Reader, contentType string, extraHeaders map[string]string) (*http.Response, error) {
	var bodyBytes []byte
	hasBody := body != nil
	if hasBody {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return nil, err
		}
	}

	token, err := p.ensureToken(ctx)
	if err != nil {
		return nil, err
	}

	refreshed401 := false
	for transientAttempt := 0; ; {
		var reqBody io.Reader
		if hasBody {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, reqBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !refreshed401 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			newTok, rErr := p.forceRefreshToken(ctx, token)
			if rErr != nil {
				return nil, rErr
			}
			token = newTok
			refreshed401 = true
			continue
		}
		if isGDriveRetryableStatus(resp.StatusCode) && transientAttempt < gdriveMaxTransientRetries {
			delay := gdriveRetryDelay(resp.Header.Get("Retry-After"), transientAttempt)
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if sErr := sleepWithContext(ctx, delay); sErr != nil {
				return nil, sErr
			}
			transientAttempt++
			continue
		}
		return resp, nil
	}
}

func (p *gdriveProvider) Test(ctx context.Context) (string, error) {
	resp, err := p.doAuth(ctx, http.MethodGet, googleDriveAPIBase+"/about?fields=user(emailAddress,displayName)", nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return "", fmt.Errorf("Google Drive 连接失败（%s）：%s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var info struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
			DisplayName  string `json:"displayName"`
		} `json:"user"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info) == nil {
		label := info.User.EmailAddress
		if label == "" {
			label = info.User.DisplayName
		}
		if label != "" {
			return label, nil
		}
	}
	p.secMu.Lock()
	email := p.sec.Email
	p.secMu.Unlock()
	return email, nil
}

func (p *gdriveProvider) rootLabel() string {
	if norm, err := normalizeRelativeCloudDir(p.row.RootDir); err == nil && norm != "" {
		return strings.TrimPrefix(norm, "/")
	}
	return "我的云端硬盘"
}

func (p *gdriveProvider) listQuery(ctx context.Context, query string) ([]gdriveFileJSON, error) {
	var all []gdriveFileJSON
	pageToken := ""
	for page := 0; page < 5; page++ {
		q := url.Values{}
		q.Set("q", query)
		q.Set("fields", "files(id,name,mimeType,size,modifiedTime,parents),nextPageToken")
		q.Set("orderBy", "folder,modifiedTime desc,name")
		q.Set("pageSize", "200")
		q.Set("supportsAllDrives", "true")
		q.Set("includeItemsFromAllDrives", "true")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		resp, err := p.doAuth(ctx, http.MethodGet, googleDriveAPIBase+"/files?"+q.Encode(), nil, "")
		if err != nil {
			return nil, err
		}
		raw, readErr := readBodyLimited(resp.Body, 8<<20)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &gdriveHTTPError{
				status: resp.StatusCode,
				msg:    fmt.Sprintf("读取 Google Drive 目录失败（%s）：%s", resp.Status, strings.TrimSpace(string(raw))),
			}
		}
		var out struct {
			Files         []gdriveFileJSON `json:"files"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Files...)
		if out.NextPageToken == "" {
			break
		}
		pageToken = out.NextPageToken
	}
	return all, nil
}

func (p *gdriveProvider) effectiveRelRootSegments() ([]string, string, error) {
	normRoot, err := normalizeRelativeCloudDir(p.row.RootDir)
	if err != nil {
		return nil, "", fmt.Errorf("默认目录配置无效：%w", err)
	}
	if normRoot == "" {
		return []string{defaultGDriveRelRootDir}, defaultGDriveRelRootDir, nil
	}
	return splitCleanSegments(normRoot), strings.TrimPrefix(normRoot, "/"), nil
}

func (p *gdriveProvider) findFolderByName(ctx context.Context, parentID, name string) (gdriveFileJSON, bool, error) {
	query := fmt.Sprintf("'%s' in parents and name='%s' and mimeType='%s' and trashed=false",
		escapeDriveQuery(parentID), escapeDriveQuery(name), googleFolderMime)
	files, err := p.listQuery(ctx, query)
	if err != nil {
		return gdriveFileJSON{}, false, err
	}
	for _, f := range files {
		if f.MimeType == googleFolderMime && f.Name == name {
			return f, true, nil
		}
	}
	return gdriveFileJSON{}, false, nil
}

func (p *gdriveProvider) isFolderAlive(ctx context.Context, folderID string) (bool, error) {
	if folderID == "" || folderID == "root" {
		return true, nil
	}
	meta, err := p.getFileMeta(ctx, folderID, "id,trashed")
	if errors.Is(err, errCloudNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !meta.Trashed, nil
}

func (p *gdriveProvider) resolveSegmentsFromRoot(ctx context.Context, segs []string, createMissing, verifyFinal bool) (folderID string, found bool, usedCache bool, err error) {
	st := p.folderState()
	st.mu.Lock()
	defer st.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		cur := "root"
		curFromCache := false
		now := time.Now()
		retry := false
		for i, seg := range segs {
			isFinal := i == len(segs)-1
			cacheKey := cur + "\x00" + seg
			if createMissing && !(verifyFinal && isFinal) && st.cache != nil {
				if ent, ok := st.cache[cacheKey]; ok && now.Before(ent.expiresAt) {
					cur = ent.id
					curFromCache = true
					continue
				}
			}
			f, segFound, err := p.findFolderByName(ctx, cur, seg)
			if err != nil {
				if attempt == 0 && gdriveErrorStatus(err) == http.StatusNotFound {
					// st.mu is already held; reset st.cache directly (never call p.clearFolderCache()).
					st.cache = nil
					retry = true
					break
				}
				return "", false, false, err
			}
			if segFound {
				if st.cache == nil {
					st.cache = make(map[string]gdriveFolderCacheEntry)
				}
				st.cache[cacheKey] = gdriveFolderCacheEntry{id: f.ID, expiresAt: time.Now().Add(gdriveFolderCacheTTL)}
				cur = f.ID
				curFromCache = false
				continue
			}
			if !createMissing {
				if st.cache != nil {
					delete(st.cache, cacheKey)
				}
				return "", false, false, nil
			}
			if curFromCache {
				alive, aliveErr := p.isFolderAlive(ctx, cur)
				if aliveErr != nil {
					return "", false, false, aliveErr
				}
				if !alive {
					if attempt == 0 {
						// st.mu is already held; reset st.cache directly (never call p.clearFolderCache()).
						st.cache = nil
						retry = true
						break
					}
					return "", false, false, errGDriveFolderTrashed
				}
			}
			created, err := p.mkdirByParentID(ctx, cur, seg)
			if err != nil {
				if attempt == 0 && gdriveErrorStatus(err) == http.StatusNotFound {
					// st.mu is already held; reset st.cache directly (never call p.clearFolderCache()).
					st.cache = nil
					retry = true
					break
				}
				return "", false, false, err
			}
			if st.cache == nil {
				st.cache = make(map[string]gdriveFolderCacheEntry)
			}
			st.cache[cacheKey] = gdriveFolderCacheEntry{id: created.ID, expiresAt: time.Now().Add(gdriveFolderCacheTTL)}
			cur = created.ID
			curFromCache = false
		}
		if !retry {
			return cur, true, curFromCache, nil
		}
	}
	return "", false, false, errors.New("无法解析 Google Drive 目录")
}

func (p *gdriveProvider) resolveRelDir(ctx context.Context, cleanRel string, createMissing bool) (string, bool, error) {
	id, found, _, err := p.resolveRelDirWithVerify(ctx, cleanRel, createMissing, false)
	return id, found, err
}

func (p *gdriveProvider) resolveRelDirWithVerify(ctx context.Context, cleanRel string, createMissing, verifyFinal bool) (string, bool, bool, error) {
	rootSegs, _, err := p.effectiveRelRootSegments()
	if err != nil {
		return "", false, false, err
	}
	segs := make([]string, 0, len(rootSegs)+4)
	segs = append(segs, rootSegs...)
	if cleanRel != "" {
		segs = append(segs, strings.Split(cleanRel, "/")...)
	}
	return p.resolveSegmentsFromRoot(ctx, segs, createMissing, verifyFinal)
}

func (p *gdriveProvider) resolveOrCreateRootFolderWithCache(ctx context.Context, createMissing bool) (string, bool, error) {
	normRoot, err := normalizeRelativeCloudDir(p.row.RootDir)
	if err != nil {
		return "", false, fmt.Errorf("默认目录配置无效：%w", err)
	}
	if normRoot == "" {
		return "root", false, nil
	}
	id, found, usedCache, err := p.resolveSegmentsFromRoot(ctx, splitCleanSegments(normRoot), createMissing, false)
	if err != nil || !found {
		return "", false, err
	}
	return id, usedCache, nil
}

func (p *gdriveProvider) resolveOrCreateRootFolder(ctx context.Context, createMissing bool) (string, error) {
	id, _, err := p.resolveOrCreateRootFolderWithCache(ctx, createMissing)
	return id, err
}

func (p *gdriveProvider) getFileMeta(ctx context.Context, fileID, fields string) (gdriveFileJSON, error) {
	if !validGoogleDriveID(fileID) {
		return gdriveFileJSON{}, errors.New("无效的 Google Drive 文件编号")
	}
	q := url.Values{}
	q.Set("fields", fields)
	q.Set("supportsAllDrives", "true")
	resp, err := p.doAuth(ctx, http.MethodGet, googleDriveAPIBase+"/files/"+url.PathEscape(fileID)+"?"+q.Encode(), nil, "")
	if err != nil {
		return gdriveFileJSON{}, err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return gdriveFileJSON{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return gdriveFileJSON{}, fmt.Errorf("%w: %s", errCloudNotFound, fileID)
	}
	if resp.StatusCode != http.StatusOK {
		return gdriveFileJSON{}, fmt.Errorf("获取文件信息失败（%s）：%s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var f gdriveFileJSON
	if err := json.Unmarshal(raw, &f); err != nil {
		return gdriveFileJSON{}, err
	}
	return f, nil
}

func gdriveFilesToEntries(files []gdriveFileJSON, relPrefix string) []cloudEntry {
	entries := make([]cloudEntry, 0, len(files))
	for _, f := range files {
		isDir := f.MimeType == googleFolderMime
		var size int64
		if f.Size != "" {
			size, _ = strconv.ParseInt(f.Size, 10, 64)
		}
		exportAs := ""
		switch f.MimeType {
		case googleDocMime:
			exportAs = ".docx"
		case googleSheetMime:
			exportAs = ".xlsx"
		case googleSlidesMime:
			exportAs = ".pdf"
		}
		entryPath := ""
		if relPrefix != "" {
			entryPath = cloudRelJoin(relPrefix, f.Name)
		}
		entries = append(entries, cloudEntry{
			ID:       f.ID,
			Name:     f.Name,
			Path:     entryPath,
			IsDir:    isDir,
			Size:     size,
			Modified: f.ModifiedTime,
			Mime:     f.MimeType,
			ExportAs: exportAs,
		})
	}
	sortCloudEntries(entries)
	return entries
}

func (p *gdriveProvider) listRelDir(ctx context.Context, cleanRel string) (cloudListResult, error) {
	_, rootName, err := p.effectiveRelRootSegments()
	if err != nil {
		return cloudListResult{}, err
	}
	outDir := cloudRelDirPrefix + cleanRel
	dirName := rootName
	parentDir := ""
	if cleanRel != "" {
		dirName = path.Base(cleanRel)
		pRel := strings.TrimPrefix(parentRelativeCloudDir("/"+cleanRel), "/")
		parentDir = cloudRelDirPrefix + pRel
	}
	crumbs := []cloudCrumb{{ID: cloudRelDirPrefix, Name: rootName}}
	if cleanRel != "" {
		acc := ""
		for _, seg := range strings.Split(cleanRel, "/") {
			if acc == "" {
				acc = seg
			} else {
				acc += "/" + seg
			}
			crumbs = append(crumbs, cloudCrumb{ID: cloudRelDirPrefix + acc, Name: seg})
		}
	}
	folderID, found, err := p.resolveRelDir(ctx, cleanRel, false)
	if err != nil {
		return cloudListResult{}, err
	}
	if !found {
		return cloudListResult{
			Dir:       outDir,
			DirName:   dirName,
			ParentDir: parentDir,
			Crumbs:    crumbs,
			Entries:   []cloudEntry{},
		}, nil
	}
	files, err := p.listQuery(ctx, fmt.Sprintf("'%s' in parents and trashed=false", escapeDriveQuery(folderID)))
	if err != nil {
		return cloudListResult{}, err
	}
	relPrefix := cleanRel
	if relPrefix == "" {
		relPrefix = cloudRelDirPrefix
	}
	return cloudListResult{
		Dir:       outDir,
		DirName:   dirName,
		ParentDir: parentDir,
		Crumbs:    crumbs,
		Entries:   gdriveFilesToEntries(files, relPrefix),
	}, nil
}

func (p *gdriveProvider) List(ctx context.Context, dir string) (cloudListResult, error) {
	if cleanRel, isRel, err := parseCloudRelPath(dir); isRel {
		if err != nil {
			return cloudListResult{}, err
		}
		return p.listRelDir(ctx, cleanRel)
	}
	rootID, err := p.resolveOrCreateRootFolder(ctx, false)
	if err != nil {
		return cloudListResult{}, err
	}
	folderID := strings.TrimSpace(dir)
	dirName := p.rootLabel()
	parentDir := ""
	if folderID == "" {
		if rootID == "" {
			return cloudListResult{
				Dir:       "",
				DirName:   dirName,
				ParentDir: "",
				Entries:   []cloudEntry{},
			}, nil
		}
		folderID = rootID
	} else {
		if !validGoogleDriveID(folderID) {
			return cloudListResult{}, errors.New("无效的 Google Drive 文件夹编号")
		}
		if folderID != rootID && folderID != "root" {
			if meta, err := p.getFileMeta(ctx, folderID, "id,name,parents"); err == nil {
				if meta.Name != "" {
					dirName = meta.Name
				}
				if len(meta.Parents) > 0 {
					if meta.Parents[0] == rootID {
						parentDir = ""
					} else {
						parentDir = meta.Parents[0]
					}
				}
			}
		}
	}

	files, err := p.listQuery(ctx, fmt.Sprintf("'%s' in parents and trashed=false", escapeDriveQuery(folderID)))
	if err != nil {
		return cloudListResult{}, err
	}
	entries := gdriveFilesToEntries(files, "")
	outDir := dir
	if dir == rootID {
		outDir = ""
	}
	return cloudListResult{
		Dir:       outDir,
		DirName:   dirName,
		ParentDir: parentDir,
		Entries:   entries,
	}, nil
}

func (p *gdriveProvider) mkdirByParentID(ctx context.Context, parentID, cleanName string) (cloudEntry, error) {
	payload, _ := json.Marshal(map[string]any{
		"name":     cleanName,
		"mimeType": googleFolderMime,
		"parents":  []string{parentID},
	})
	resp, err := p.doAuth(ctx, http.MethodPost, googleDriveAPIBase+"/files?supportsAllDrives=true&fields=id,name,mimeType,modifiedTime", bytes.NewReader(payload), "application/json")
	if err != nil {
		return cloudEntry{}, err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return cloudEntry{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cloudEntry{}, &gdriveHTTPError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("新建 Google Drive 文件夹失败（%s）：%s", resp.Status, strings.TrimSpace(string(raw))),
		}
	}
	var f gdriveFileJSON
	if err := json.Unmarshal(raw, &f); err != nil {
		return cloudEntry{}, err
	}
	return cloudEntry{
		ID:       f.ID,
		Name:     f.Name,
		IsDir:    true,
		Modified: f.ModifiedTime,
		Mime:     googleFolderMime,
	}, nil
}

func (p *gdriveProvider) Mkdir(ctx context.Context, parentDir, name string) (cloudEntry, error) {
	cleanName, err := cleanCloudFilename(name)
	if err != nil {
		return cloudEntry{}, err
	}
	if cleanRel, isRel, err := parseCloudRelPath(parentDir); isRel {
		if err != nil {
			return cloudEntry{}, err
		}
		joined := cleanName
		if cleanRel != "" {
			joined = cleanRel + "/" + cleanName
		}
		for attempt := 0; attempt < 2; attempt++ {
			folderID, _, _, err := p.resolveRelDirWithVerify(ctx, joined, true, true)
			if err == nil {
				return cloudEntry{
					ID:    folderID,
					Name:  cleanName,
					Path:  cloudRelDirPrefix + joined,
					IsDir: true,
					Mime:  googleFolderMime,
				}, nil
			}
			if attempt == 0 && isGDriveStaleFolderErr(err) {
				p.clearFolderCache()
				continue
			}
			return cloudEntry{}, err
		}
	}
	parentID := strings.TrimSpace(parentDir)
	if parentID == "" {
		for attempt := 0; attempt < 2; attempt++ {
			resolvedID, usedCache, err := p.resolveOrCreateRootFolderWithCache(ctx, true)
			if err == nil && usedCache {
				alive, aliveErr := p.isFolderAlive(ctx, resolvedID)
				if aliveErr != nil {
					return cloudEntry{}, aliveErr
				}
				if !alive {
					err = errGDriveFolderTrashed
				}
			}
			if err == nil {
				var entry cloudEntry
				entry, err = p.mkdirByParentID(ctx, resolvedID, cleanName)
				if err == nil {
					return entry, nil
				}
			}
			if attempt == 0 && isGDriveStaleFolderErr(err) {
				p.clearFolderCache()
				continue
			}
			return cloudEntry{}, err
		}
	}
	if !validGoogleDriveID(parentID) {
		return cloudEntry{}, errors.New("无效的 Google Drive 目录编号")
	}
	return p.mkdirByParentID(ctx, parentID, cleanName)
}

func ensureFilenameExt(name, ext string) string {
	if strings.EqualFold(path.Ext(name), ext) {
		return name
	}
	return name + ext
}

func (p *gdriveProvider) downloadFileWithMeta(ctx context.Context, meta gdriveFileJSON) (cloudDownloadResult, error) {
	if meta.MimeType == googleFolderMime {
		return cloudDownloadResult{}, errors.New("不能直接下载文件夹")
	}
	fileID := meta.ID
	downloadURL := googleDriveAPIBase + "/files/" + url.PathEscape(fileID) + "?alt=media&supportsAllDrives=true"
	outName := meta.Name
	outMime := meta.MimeType
	switch meta.MimeType {
	case googleDocMime:
		downloadURL = googleDriveAPIBase + "/files/" + url.PathEscape(fileID) + "/export?mimeType=" + url.QueryEscape(docxMimeType)
		outName = ensureFilenameExt(meta.Name, ".docx")
		outMime = docxMimeType
	case googleSheetMime:
		downloadURL = googleDriveAPIBase + "/files/" + url.PathEscape(fileID) + "/export?mimeType=" + url.QueryEscape(xlsxMimeType)
		outName = ensureFilenameExt(meta.Name, ".xlsx")
		outMime = xlsxMimeType
	case googleSlidesMime:
		downloadURL = googleDriveAPIBase + "/files/" + url.PathEscape(fileID) + "/export?mimeType=" + url.QueryEscape(pdfMimeType)
		outName = ensureFilenameExt(meta.Name, ".pdf")
		outMime = pdfMimeType
	}
	resp, err := p.doAuth(ctx, http.MethodGet, downloadURL, nil, "")
	if err != nil {
		return cloudDownloadResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return cloudDownloadResult{}, fmt.Errorf("%w: %s", errCloudNotFound, fileID)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return cloudDownloadResult{}, fmt.Errorf("下载 Google Drive 文件失败（%s）：%s", resp.Status, strings.TrimSpace(string(msg)))
	}
	data, err := readBodyLimited(resp.Body, maxCloudFileBytes)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	if outMime == "" {
		outMime = resp.Header.Get("Content-Type")
	}
	if outMime == "" {
		outMime = "application/octet-stream"
	}
	return cloudDownloadResult{
		Filename: outName,
		MimeType: outMime,
		Data:     data,
	}, nil
}

func (p *gdriveProvider) Download(ctx context.Context, fileID string) (cloudDownloadResult, error) {
	if dirRel, targetName, isRel, err := parseCloudRelFilePath(fileID); isRel {
		if err != nil {
			return cloudDownloadResult{}, err
		}
		parentID, found, err := p.resolveRelDir(ctx, dirRel, false)
		if err != nil {
			return cloudDownloadResult{}, err
		}
		if !found {
			return cloudDownloadResult{}, fmt.Errorf("%w: %s", errCloudNotFound, strings.TrimPrefix(strings.TrimSpace(fileID), cloudRelDirPrefix))
		}
		query := fmt.Sprintf("'%s' in parents and name='%s' and trashed=false",
			escapeDriveQuery(parentID), escapeDriveQuery(targetName))
		files, err := p.listQuery(ctx, query)
		if err != nil {
			return cloudDownloadResult{}, err
		}
		var matchedFile *gdriveFileJSON
		var matchedDir bool
		for i := range files {
			f := &files[i]
			if f.Name != targetName {
				continue
			}
			if f.MimeType == googleFolderMime {
				matchedDir = true
				continue
			}
			matchedFile = f
			break
		}
		if matchedFile == nil {
			if matchedDir {
				return cloudDownloadResult{}, errors.New("不能直接下载文件夹")
			}
			return cloudDownloadResult{}, fmt.Errorf("%w: %s", errCloudNotFound, targetName)
		}
		return p.downloadFileWithMeta(ctx, *matchedFile)
	}
	if fileID == "" || fileID == "root" || !validGoogleDriveID(fileID) {
		return cloudDownloadResult{}, errors.New("请选择有效的 Google Drive 文件")
	}
	meta, err := p.getFileMeta(ctx, fileID, "id,name,mimeType,size,trashed")
	if err != nil {
		return cloudDownloadResult{}, err
	}
	if meta.Trashed {
		return cloudDownloadResult{}, fmt.Errorf("%w: %s", errCloudNotFound, fileID)
	}
	return p.downloadFileWithMeta(ctx, meta)
}

func (p *gdriveProvider) Upload(ctx context.Context, dir, filename string, data []byte, mimeType string, overwrite bool) (cloudEntry, error) {
	if int64(len(data)) > maxCloudFileBytes {
		return cloudEntry{}, fmt.Errorf("文件大小超过 %d MB 上限", maxCloudFileBytes>>20)
	}
	cleanName, err := cleanCloudFilename(filename)
	if err != nil {
		return cloudEntry{}, err
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if cleanRel, isRel, err := parseCloudRelPath(dir); isRel {
		if err != nil {
			return cloudEntry{}, err
		}
		for attempt := 0; attempt < 2; attempt++ {
			parentID, _, usedCache, err := p.resolveRelDirWithVerify(ctx, cleanRel, true, false)
			if err == nil {
				var entry cloudEntry
				entry, err = p.uploadIntoParentID(ctx, parentID, cleanName, data, mimeType, overwrite, usedCache)
				if err == nil {
					relPrefix := cleanRel
					if relPrefix == "" {
						relPrefix = cloudRelDirPrefix
					}
					entry.Path = cloudRelJoin(relPrefix, entry.Name)
					return entry, nil
				}
			}
			if attempt == 0 && isGDriveStaleFolderErr(err) {
				p.clearFolderCache()
				continue
			}
			return cloudEntry{}, err
		}
	}
	parentID := strings.TrimSpace(dir)
	if parentID == "" {
		for attempt := 0; attempt < 2; attempt++ {
			resolvedID, usedCache, err := p.resolveOrCreateRootFolderWithCache(ctx, true)
			if err == nil {
				var entry cloudEntry
				entry, err = p.uploadIntoParentID(ctx, resolvedID, cleanName, data, mimeType, overwrite, usedCache)
				if err == nil {
					return entry, nil
				}
			}
			if attempt == 0 && isGDriveStaleFolderErr(err) {
				p.clearFolderCache()
				continue
			}
			return cloudEntry{}, err
		}
	}
	if !validGoogleDriveID(parentID) {
		return cloudEntry{}, errors.New("无效的 Google Drive 目录编号")
	}
	return p.uploadIntoParentID(ctx, parentID, cleanName, data, mimeType, overwrite, false)
}

func (p *gdriveProvider) uploadIntoParentID(ctx context.Context, parentID, cleanName string, data []byte, mimeType string, overwrite, verifyParentOnEmpty bool) (cloudEntry, error) {
	query := fmt.Sprintf("'%s' in parents and trashed=false", escapeDriveQuery(parentID))
	if overwrite {
		query = fmt.Sprintf("'%s' in parents and name='%s' and trashed=false", escapeDriveQuery(parentID), escapeDriveQuery(cleanName))
	}
	existingFiles, err := p.listQuery(ctx, query)
	if err != nil {
		return cloudEntry{}, err
	}
	if verifyParentOnEmpty && parentID != "root" && len(existingFiles) == 0 {
		alive, aliveErr := p.isFolderAlive(ctx, parentID)
		if aliveErr != nil {
			return cloudEntry{}, aliveErr
		}
		if !alive {
			return cloudEntry{}, errGDriveFolderTrashed
		}
	}
	var overwriteID string
	existingNames := make(map[string]bool, len(existingFiles))
	for _, f := range existingFiles {
		existingNames[strings.ToLower(f.Name)] = true
		if overwrite && overwriteID == "" && f.Name == cleanName && f.MimeType != googleFolderMime && !strings.HasPrefix(f.MimeType, "application/vnd.google-apps.") {
			overwriteID = f.ID
		}
	}

	if !overwrite && overwriteID == "" {
		cleanName = dedupeCloudFilename(existingNames, cleanName)
	}
	if len(data) > 4<<20 {
		return p.uploadResumable(ctx, parentID, overwriteID, cleanName, data, mimeType)
	}

	if overwriteID != "" {
		u := googleDriveUploadBase + "/files/" + url.PathEscape(overwriteID) + "?uploadType=media&supportsAllDrives=true&fields=id,name,mimeType,size,modifiedTime"
		resp, err := p.doAuth(ctx, http.MethodPatch, u, bytes.NewReader(data), mimeType)
		if err != nil {
			return cloudEntry{}, err
		}
		defer resp.Body.Close()
		raw, err := readBodyLimited(resp.Body, 1<<20)
		if err != nil {
			return cloudEntry{}, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return cloudEntry{}, &gdriveHTTPError{
				status: resp.StatusCode,
				msg:    fmt.Sprintf("覆盖上传到 Google Drive 失败（%s）：%s", resp.Status, strings.TrimSpace(string(raw))),
			}
		}
		var out gdriveFileJSON
		if err := json.Unmarshal(raw, &out); err != nil {
			return cloudEntry{}, err
		}
		return cloudEntry{ID: out.ID, Name: out.Name, Size: int64(len(data)), Modified: out.ModifiedTime, Mime: out.MimeType}, nil
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	metaHdr := make(textproto.MIMEHeader)
	metaHdr.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := mw.CreatePart(metaHdr)
	if err != nil {
		return cloudEntry{}, err
	}
	metaJSON, _ := json.Marshal(map[string]any{
		"name":    cleanName,
		"parents": []string{parentID},
	})
	if _, err := metaPart.Write(metaJSON); err != nil {
		return cloudEntry{}, err
	}
	dataHdr := make(textproto.MIMEHeader)
	dataHdr.Set("Content-Type", mimeType)
	dataPart, err := mw.CreatePart(dataHdr)
	if err != nil {
		return cloudEntry{}, err
	}
	if _, err := dataPart.Write(data); err != nil {
		return cloudEntry{}, err
	}
	if err := mw.Close(); err != nil {
		return cloudEntry{}, err
	}

	u := googleDriveUploadBase + "/files?uploadType=multipart&supportsAllDrives=true&fields=id,name,mimeType,size,modifiedTime"
	resp, err := p.doAuth(ctx, http.MethodPost, u, &body, "multipart/related; boundary="+mw.Boundary())
	if err != nil {
		return cloudEntry{}, err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return cloudEntry{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cloudEntry{}, &gdriveHTTPError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("上传到 Google Drive 失败（%s）：%s", resp.Status, strings.TrimSpace(string(raw))),
		}
	}
	var out gdriveFileJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		return cloudEntry{}, err
	}
	return cloudEntry{
		ID:       out.ID,
		Name:     out.Name,
		Size:     int64(len(data)),
		Modified: out.ModifiedTime,
		Mime:     out.MimeType,
	}, nil
}

func (p *gdriveProvider) uploadResumable(ctx context.Context, parentID, overwriteID, cleanName string, data []byte, mimeType string) (cloudEntry, error) {
	method := http.MethodPost
	initURL := googleDriveUploadBase + "/files?uploadType=resumable&supportsAllDrives=true&fields=id,name,mimeType,size,modifiedTime"
	meta := map[string]any{
		"name":    cleanName,
		"parents": []string{parentID},
	}
	if overwriteID != "" {
		method = http.MethodPatch
		initURL = googleDriveUploadBase + "/files/" + url.PathEscape(overwriteID) + "?uploadType=resumable&supportsAllDrives=true&fields=id,name,mimeType,size,modifiedTime"
		meta = map[string]any{}
	}
	metaJSON, _ := json.Marshal(meta)
	initResp, err := p.doAuthWithHeaders(ctx, method, initURL, bytes.NewReader(metaJSON), "application/json; charset=UTF-8", map[string]string{
		"X-Upload-Content-Type":   mimeType,
		"X-Upload-Content-Length": strconv.Itoa(len(data)),
	})
	if err != nil {
		return cloudEntry{}, err
	}
	initRaw, _ := readBodyLimited(initResp.Body, 1<<20)
	initResp.Body.Close()
	if initResp.StatusCode < 200 || initResp.StatusCode >= 300 {
		return cloudEntry{}, &gdriveHTTPError{
			status: initResp.StatusCode,
			msg:    fmt.Sprintf("初始化 Google Drive 分片上传失败（%s）：%s", initResp.Status, strings.TrimSpace(string(initRaw))),
		}
	}
	uploadURL := strings.TrimSpace(initResp.Header.Get("Location"))
	if uploadURL == "" {
		return cloudEntry{}, errors.New("Google Drive 未返回分片上传地址")
	}

	const chunkSize = 8 << 20 // 8 MiB (multiple of 256 KiB as required by Drive API)
	total := len(data)
	for offset := 0; offset < total; offset += chunkSize {
		end := offset + chunkSize
		if end > total {
			end = total
		}
		chunk := data[offset:end]
		var statusCode int
		var statusText string
		var raw []byte
		for attempt := 0; ; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(chunk))
			if err != nil {
				return cloudEntry{}, err
			}
			req.ContentLength = int64(len(chunk))
			req.Header.Set("Content-Type", mimeType)
			req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end-1, total))

			resp, err := p.client.Do(req)
			if err != nil {
				return cloudEntry{}, err
			}
			b, readErr := readBodyLimited(resp.Body, 1<<20)
			resp.Body.Close()
			if readErr != nil {
				return cloudEntry{}, readErr
			}
			if isGDriveRetryableStatus(resp.StatusCode) && attempt < gdriveMaxTransientRetries {
				delay := gdriveRetryDelay(resp.Header.Get("Retry-After"), attempt)
				if sErr := sleepWithContext(ctx, delay); sErr != nil {
					return cloudEntry{}, sErr
				}
				continue
			}
			statusCode = resp.StatusCode
			statusText = resp.Status
			raw = b
			break
		}
		if statusCode == http.StatusPermanentRedirect {
			continue
		}
		if statusCode >= 200 && statusCode < 300 {
			var out gdriveFileJSON
			if err := json.Unmarshal(raw, &out); err != nil {
				return cloudEntry{}, err
			}
			return cloudEntry{
				ID:       out.ID,
				Name:     out.Name,
				Size:     int64(total),
				Modified: out.ModifiedTime,
				Mime:     out.MimeType,
			}, nil
		}
		return cloudEntry{}, &gdriveHTTPError{
			status: statusCode,
			msg:    fmt.Sprintf("分片上传到 Google Drive 失败（%s）：%s", statusText, strings.TrimSpace(string(raw))),
		}
	}
	return cloudEntry{}, errors.New("Google Drive 分片上传未返回完成结果")
}
