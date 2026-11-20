package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (l *libraryPage) sharedCloudSummary() map[string]any {
	cfg := l.loadCloudAdminSettings()
	key, err := l.getCloudKey()
	var googleReady, baiduReady bool
	var baiduDir string
	if err == nil {
		if cid, _, _, gErr := cfg.effectiveGoogleCreds(key); gErr == nil && cid != "" {
			googleReady = true
		}
		if ak, sk, dir, bErr := cfg.effectiveBaiduCreds(key); bErr == nil && ak != "" && sk != "" {
			baiduReady = true
			baiduDir = dir
		} else {
			baiduDir = defaultBaiduAppDir
		}
	}
	return map[string]any{
		"googleReady":        googleReady,
		"baiduReady":         baiduReady,
		"baiduAppDir":        baiduDir,
		"allowPrivateWebdav": cfg.effectiveAllowPrivateWebDAV(),
		"proxyConfigured":    cfg.effectiveProxyURL() != "",
		"proxyScope":         cfg.effectiveProxyScope(),
	}
}

func (l *libraryPage) cloudConfigPayload(id string) (map[string]any, error) {
	account := ownerKey(id)
	conns, err := l.listCloudConnections(account)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"connections": conns,
		"shared":      l.sharedCloudSummary(),
	}
	if id == "" {
		out["adminConfig"] = l.cloudAdminView()
		out["autoBackupState"] = l.loadAutoBackupState()
	}
	return out, nil
}

func (l *libraryPage) serveCloudGet(w http.ResponseWriter, r *http.Request, id string) {
	switch r.URL.Path {
	case "/library/cloud/google/callback":
		l.serveGoogleCallback(w, r)
	case "/library/cloud/config":
		payload, err := l.cloudConfigPayload(id)
		if err != nil {
			http.Error(w, "读取云盘配置失败", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, payload)
	case "/library/cloud/sync/status":
		l.serveCloudSyncStatus(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeOAuthCallbackHTML(w http.ResponseWriter, ok bool, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}
	payload, _ := json.Marshal(map[string]any{
		"ok": ok,
		"error": func() string {
			if ok {
				return ""
			}
			return detail
		}(),
		"name": func() string {
			if ok {
				return detail
			}
			return ""
		}(),
	})
	safePayload := safeJSONScript(payload)
	fmt.Fprintf(w, `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,"PingFang SC",sans-serif;max-width:460px;margin:12vh auto;padding:24px;color:#1d2621;background:#f5f6f2}main{background:#fff;border:1px solid #d9ded7;border-radius:12px;padding:24px}h1{font-size:20px;margin:0 0 10px}p{line-height:1.6;color:#515e56}button{margin-top:14px;padding:8px 16px;border-radius:8px;border:1px solid #295743;background:#295743;color:#fff;cursor:pointer}</style>
</head><body><main><h1>%s</h1><p>%s</p><button type="button" onclick="window.close()">关闭窗口</button></main>
<script>
try{
  const msg=%s;
  if(typeof BroadcastChannel!=='undefined'){
    const ch=new BroadcastChannel('biofolio-cloud-oauth');
    ch.postMessage(msg);
    ch.close();
  }
  if(window.opener && !window.opener.closed){
    window.opener.postMessage({type:'biofolio-cloud-oauth',...msg},window.location.origin);
  }
}catch(e){}
if(%t){setTimeout(()=>{try{window.close()}catch(e){}},700);}
</script></body></html>`,
		html.EscapeString(title),
		html.EscapeString(title),
		html.EscapeString(detail),
		string(safePayload),
		ok,
	)
}

func (l *libraryPage) serveGoogleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if oauthErr := q.Get("error"); oauthErr != "" {
		writeOAuthCallbackHTML(w, false, "Google Drive 授权已取消", "返回错误："+oauthErr+"。请关闭窗口后重试。")
		return
	}
	state := strings.TrimSpace(q.Get("state"))
	code := strings.TrimSpace(q.Get("code"))
	if state == "" || code == "" {
		writeOAuthCallbackHTML(w, false, "Google Drive 授权参数缺失", "缺少 state 或 code 参数，请回到题库设置重新发起授权。")
		return
	}
	sess, ok := l.authStore().takeGoogle(state)
	if !ok {
		writeOAuthCallbackHTML(w, false, "Google Drive 授权已过期", "该授权请求已失效或已被使用，请回到题库设置重新点击授权。")
		return
	}
	conn, err := l.finishGoogleSession(r, sess, code)
	if err != nil {
		writeOAuthCallbackHTML(w, false, "Google Drive 绑定失败", err.Error())
		return
	}
	writeOAuthCallbackHTML(w, true, "Google Drive 已连接", "已绑定云盘「"+conn.Name+"」，正在返回题库…")
}

func (l *libraryPage) finishGoogleSession(r *http.Request, sess googleOAuthSession, code string) (cloudConnectionView, error) {
	cfg := l.loadCloudAdminSettings()
	clientID, clientSecret, _, err := resolveGoogleOAuthClient(l, cfg, sess.CustomClientID, sess.CustomClientSecret)
	if err != nil {
		return cloudConnectionView{}, err
	}
	client, err := newCloudHTTPClient(cfg, "gdrive", true, false, false, 45*time.Second)
	if err != nil {
		return cloudConnectionView{}, err
	}
	tok, err := exchangeGoogleCode(r.Context(), client, clientID, clientSecret, code, sess.RedirectURI, sess.CodeVerifier)
	if err != nil {
		return cloudConnectionView{}, err
	}
	if tok.RefreshToken == "" {
		return cloudConnectionView{}, errors.New("Google 未返回 refresh_token，请重试授权并在授权页确认允许访问")
	}
	email := fetchGoogleAboutEmail(r.Context(), client, tok.AccessToken)
	exp := tok.ExpiresIn
	if exp <= 0 {
		exp = 3500
	}
	sec := gdriveSecret{
		RefreshToken:       tok.RefreshToken,
		AccessToken:        tok.AccessToken,
		ExpiryUnix:         time.Now().Unix() + exp,
		Email:              email,
		CustomClientID:     sess.CustomClientID,
		CustomClientSecret: sess.CustomClientSecret,
	}
	secBytes, _ := json.Marshal(sec)
	name := strings.TrimSpace(sess.Name)
	if name == "" {
		name = "我的 Google Drive"
	}
	row := cloudConnectionRow{
		ID:           newCloudID(),
		Account:      sess.Account,
		Kind:         "gdrive",
		Name:         name,
		AccountLabel: email,
		RootDir:      strings.TrimSpace(sess.RootDir),
		IsDefault:    false,
		UseProxy:     true,
	}
	if err := l.saveCloudConnection(row, secBytes); err != nil {
		return cloudConnectionView{}, err
	}
	return cloudConnectionView{
		ID:           row.ID,
		Kind:         row.Kind,
		Name:         row.Name,
		AccountLabel: row.AccountLabel,
		RootDir:      row.RootDir,
		UseProxy:     row.UseProxy,
	}, nil
}

func (l *libraryPage) serveCloudPost(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	account := ownerKey(id)
	isAdmin := id == ""
	switch r.URL.Path {
	case "/library/cloud/admin-settings":
		l.serveCloudAdminSettings(w, body, isAdmin)
	case "/library/cloud/connections":
		l.serveCloudConnections(w, r, account, isAdmin, body)
	case "/library/cloud/google/start":
		l.serveGoogleStart(w, r, account, body)
	case "/library/cloud/google/complete":
		l.serveGoogleComplete(w, r, account, body)
	case "/library/cloud/baidu/start":
		l.serveBaiduStart(w, r, account, body)
	case "/library/cloud/baidu/poll":
		l.serveBaiduPoll(w, r, account, body)
	case "/library/cloud/list":
		l.serveCloudList(w, r, account, isAdmin, body)
	case "/library/cloud/mkdir":
		l.serveCloudMkdir(w, r, account, isAdmin, body)
	case "/library/cloud/download":
		l.serveCloudDownload(w, r, account, isAdmin, body)
	case "/library/cloud/backup-export":
		l.serveCloudBackupExport(w, r, account, isAdmin, body)
	case "/library/cloud/backup-restore":
		l.serveCloudBackupRestore(w, r, account, isAdmin, body)
	case "/library/cloud/auto-backup":
		l.serveCloudAutoBackupSettings(w, body, isAdmin)
	case "/library/cloud/auto-backup-run":
		l.serveCloudAutoBackupRun(w, r, isAdmin, body)
	case "/library/cloud/sync/check":
		l.serveCloudSyncCheck(w, r, isAdmin, body)
	case "/library/cloud/sync/push":
		l.serveCloudSyncPush(w, r, isAdmin, body)
	case "/library/cloud/sync/restore":
		l.serveCloudSyncRestore(w, r, isAdmin, body)
	default:
		http.NotFound(w, r)
	}
}

func (l *libraryPage) serveCloudAdminSettings(w http.ResponseWriter, body []byte, isAdmin bool) {
	if !isAdmin {
		http.Error(w, "只有管理员可以修改云盘接入服务设置", http.StatusForbidden)
		return
	}
	var in struct {
		ProxyURL              string   `json:"proxyUrl"`
		ProxyScope            string   `json:"proxyScope"`
		AllowPrivateWebDAV    bool     `json:"allowPrivateWebdav"`
		AllowedWebDAVPrefixes []string `json:"allowedWebdavPrefixes"`
		GoogleClientID        string   `json:"googleClientId"`
		GoogleClientSecret    string   `json:"googleClientSecret"`
		ClearGoogleSecret     bool     `json:"clearGoogleSecret"`
		GoogleRedirectURI     string   `json:"googleRedirectUri"`
		BaiduAppKey           string   `json:"baiduAppKey"`
		BaiduSecretKey        string   `json:"baiduSecretKey"`
		ClearBaiduSecret      bool     `json:"clearBaiduSecret"`
		BaiduAppDir           string   `json:"baiduAppDir"`
	}
	if json.Unmarshal(body, &in) != nil {
		http.Error(w, "云盘服务配置格式无效", http.StatusBadRequest)
		return
	}
	if _, err := validateProxyURL(in.ProxyURL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prefixes := make([]string, 0, len(in.AllowedWebDAVPrefixes))
	seen := map[string]bool{}
	for _, raw := range in.AllowedWebDAVPrefixes {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		u, err := normalizeWebDAVPrefix(raw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		norm := u.String()
		if !seen[norm] {
			seen[norm] = true
			prefixes = append(prefixes, norm)
		}
	}
	if redir := strings.TrimSpace(in.GoogleRedirectURI); redir != "" {
		u, err := url.Parse(redir)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			http.Error(w, "Google 回调地址格式无效", http.StatusBadRequest)
			return
		}
	}
	appDir, err := normalizeBaiduAppDir(in.BaiduAppDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key, err := l.getCloudKey()
	if err != nil {
		http.Error(w, "云盘密钥初始化失败", http.StatusInternalServerError)
		return
	}
	prev := l.loadCloudAdminSettings()
	allowPrivate := in.AllowPrivateWebDAV
	next := cloudAdminSettings{
		ProxyURL:               strings.TrimSpace(in.ProxyURL),
		ProxyScope:             "google",
		AllowPrivateWebDAV:     &allowPrivate,
		AllowedWebDAVPrefixes:  prefixes,
		GoogleClientID:         strings.TrimSpace(in.GoogleClientID),
		GoogleClientSecretEnc:  prev.GoogleClientSecretEnc,
		GoogleRedirectURI:      strings.TrimSpace(in.GoogleRedirectURI),
		BaiduAppKey:            strings.TrimSpace(in.BaiduAppKey),
		BaiduSecretKeyEnc:      prev.BaiduSecretKeyEnc,
		BaiduAppDir:            appDir,
		AutoBackupEnabled:      prev.AutoBackupEnabled,
		AutoBackupConnectionID: prev.AutoBackupConnectionID,
		AutoBackupDir:          prev.AutoBackupDir,
		AutoBackupHour:         prev.AutoBackupHour,
	}
	if in.ProxyScope == "all" {
		next.ProxyScope = "all"
	}
	if in.ClearGoogleSecret || next.GoogleClientID == "" {
		next.GoogleClientSecretEnc = ""
	}
	if s := strings.TrimSpace(in.GoogleClientSecret); s != "" {
		enc, err := encryptCloudString(key, s)
		if err != nil {
			http.Error(w, "加密 Google 密钥失败", http.StatusInternalServerError)
			return
		}
		next.GoogleClientSecretEnc = enc
	}
	if in.ClearBaiduSecret || next.BaiduAppKey == "" {
		next.BaiduSecretKeyEnc = ""
	}
	if s := strings.TrimSpace(in.BaiduSecretKey); s != "" {
		enc, err := encryptCloudString(key, s)
		if err != nil {
			http.Error(w, "加密百度密钥失败", http.StatusInternalServerError)
			return
		}
		next.BaiduSecretKeyEnc = enc
	}
	if err := l.saveCloudAdminSettings(next); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{
		"adminConfig": l.cloudAdminView(),
		"shared":      l.sharedCloudSummary(),
	})
}

func (l *libraryPage) serveCloudConnections(w http.ResponseWriter, r *http.Request, account string, isAdmin bool, body []byte) {
	var in struct {
		Action    string `json:"action"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		BaseURL   string `json:"baseUrl"`
		Username  string `json:"username"`
		Password  string `json:"password"`
		RootDir   string `json:"rootDir"`
		IsDefault bool   `json:"isDefault"`
		UseProxy  bool   `json:"useProxy"`
	}
	if json.Unmarshal(body, &in) != nil {
		http.Error(w, "云盘参数格式无效", http.StatusBadRequest)
		return
	}
	switch in.Action {
	case "save_webdav":
		name := strings.TrimSpace(in.Name)
		if name == "" || utf8.RuneCountInString(name) > 80 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			http.Error(w, "云盘名称需为 1 至 80 个字符", http.StatusBadRequest)
			return
		}
		if _, err := normalizeRelativeCloudDir(in.RootDir); err != nil {
			http.Error(w, "默认目录无效："+err.Error(), http.StatusBadRequest)
			return
		}
		cfg := l.loadCloudAdminSettings()
		u, _, err := validateWebDAVTargetURL(in.BaseURL, cfg, isAdmin)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		password := in.Password
		connID := strings.TrimSpace(in.ID)
		if connID != "" {
			oldRow, oldPlain, err := l.getCloudConnection(account, connID)
			if err != nil || oldRow.Kind != "webdav" {
				http.Error(w, "找不到要编辑的 WebDAV 连接", http.StatusNotFound)
				return
			}
			if password == "" {
				var oldSec webdavSecret
				if json.Unmarshal(oldPlain, &oldSec) == nil {
					password = oldSec.Password
				}
			}
		} else {
			connID = newCloudID()
		}
		sec := webdavSecret{
			BaseURL:  u.String(),
			Username: strings.TrimSpace(in.Username),
			Password: password,
		}
		row := cloudConnectionRow{
			ID:        connID,
			Account:   account,
			Kind:      "webdav",
			Name:      name,
			RootDir:   strings.TrimSpace(in.RootDir),
			IsDefault: in.IsDefault,
			UseProxy:  in.UseProxy,
		}
		provider, err := newWebDAVProvider(cfg, row, sec, isAdmin)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		label, err := provider.Test(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		row.AccountLabel = label
		secBytes, _ := json.Marshal(sec)
		if err := l.saveCloudConnection(row, secBytes); err != nil {
			http.Error(w, "保存云盘连接失败", http.StatusInternalServerError)
			return
		}
		conns, _ := l.listCloudConnections(account)
		jsonResponse(w, map[string]any{"connections": conns, "label": label})

	case "update_meta":
		if _, err := normalizeRelativeCloudDir(in.RootDir); err != nil {
			http.Error(w, "默认目录无效："+err.Error(), http.StatusBadRequest)
			return
		}
		if err := l.updateCloudConnectionMeta(account, in.ID, in.Name, in.RootDir, in.IsDefault, in.UseProxy); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		conns, _ := l.listCloudConnections(account)
		jsonResponse(w, map[string]any{"connections": conns})

	case "test":
		row, provider, err := l.openCloudProvider(account, in.ID, isAdmin)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		label, err := provider.Test(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if label != "" && label != row.AccountLabel {
			_, _ = l.db.Exec(`UPDATE library_cloud_connections SET account_label=? WHERE id=? AND account=?`, label, row.ID, account)
		}
		conns, _ := l.listCloudConnections(account)
		jsonResponse(w, map[string]any{"ok": true, "label": label, "connections": conns})

	case "delete":
		if err := l.deleteCloudConnection(account, in.ID); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		conns, _ := l.listCloudConnections(account)
		jsonResponse(w, map[string]any{"connections": conns})

	default:
		http.Error(w, "不支持的云盘操作", http.StatusBadRequest)
	}
}

func randomURLState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (l *libraryPage) defaultGoogleRedirectURI(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	prefix := ""
	if l.account != nil {
		prefix = "/biology"
	}
	return scheme + "://" + r.Host + prefix + "/library/cloud/google/callback"
}

func (l *libraryPage) serveGoogleStart(w http.ResponseWriter, r *http.Request, account string, body []byte) {
	var in struct {
		Mode               string `json:"mode"`
		Name               string `json:"name"`
		RootDir            string `json:"rootDir"`
		CustomClientID     string `json:"customClientId"`
		CustomClientSecret string `json:"customClientSecret"`
	}
	if json.Unmarshal(body, &in) != nil {
		http.Error(w, "参数格式无效", http.StatusBadRequest)
		return
	}
	if _, err := normalizeRelativeCloudDir(in.RootDir); err != nil {
		http.Error(w, "默认目录无效："+err.Error(), http.StatusBadRequest)
		return
	}
	cfg := l.loadCloudAdminSettings()
	clientID, _, sharedRedirect, err := resolveGoogleOAuthClient(l, cfg, in.CustomClientID, in.CustomClientSecret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	state, err := randomURLState()
	if err != nil {
		http.Error(w, "生成授权状态失败", http.StatusInternalServerError)
		return
	}
	verifier, err := generatePKCEVerifier()
	if err != nil {
		http.Error(w, "生成授权校验码失败", http.StatusInternalServerError)
		return
	}
	mode := in.Mode
	if mode != "manual" {
		mode = "web"
	}
	redirectURI := googleManualRedirect
	if mode == "web" {
		if sharedRedirect != "" {
			redirectURI = sharedRedirect
		} else {
			redirectURI = l.defaultGoogleRedirectURI(r)
		}
	}
	l.authStore().putGoogle(googleOAuthSession{
		State:              state,
		Account:            account,
		CodeVerifier:       verifier,
		RedirectURI:        redirectURI,
		Name:               strings.TrimSpace(in.Name),
		RootDir:            strings.TrimSpace(in.RootDir),
		CustomClientID:     strings.TrimSpace(in.CustomClientID),
		CustomClientSecret: strings.TrimSpace(in.CustomClientSecret),
		UseProxy:           true,
	})
	authURL := buildGoogleAuthURL(clientID, redirectURI, state, verifier)
	jsonResponse(w, map[string]any{
		"state":       state,
		"mode":        mode,
		"redirectUri": redirectURI,
		"authUrl":     authURL,
	})
}

func (l *libraryPage) serveGoogleComplete(w http.ResponseWriter, r *http.Request, account string, body []byte) {
	var in struct {
		State string `json:"state"`
		Input string `json:"input"`
	}
	if json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.State) == "" {
		http.Error(w, "缺少授权状态参数", http.StatusBadRequest)
		return
	}
	code, pastedState, err := extractCodeAndStateFromInput(in.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if pastedState != "" && pastedState != strings.TrimSpace(in.State) {
		http.Error(w, "粘贴的授权链接与当前授权会话不匹配，请重新发起授权", http.StatusBadRequest)
		return
	}
	sess, ok := l.authStore().takeGoogle(strings.TrimSpace(in.State))
	if !ok || sess.Account != account {
		http.Error(w, "授权会话已过期，请重新打开 Google 授权链接", http.StatusBadRequest)
		return
	}
	conn, err := l.finishGoogleSession(r, sess, code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conns, _ := l.listCloudConnections(account)
	jsonResponse(w, map[string]any{"connection": conn, "connections": conns})
}

func (l *libraryPage) serveBaiduStart(w http.ResponseWriter, r *http.Request, account string, body []byte) {
	var in struct {
		Name            string `json:"name"`
		RootDir         string `json:"rootDir"`
		UseProxy        bool   `json:"useProxy"`
		CustomAppKey    string `json:"customAppKey"`
		CustomSecretKey string `json:"customSecretKey"`
		CustomAppDir    string `json:"customAppDir"`
	}
	if json.Unmarshal(body, &in) != nil {
		http.Error(w, "参数格式无效", http.StatusBadRequest)
		return
	}
	if _, err := normalizeRelativeCloudDir(in.RootDir); err != nil {
		http.Error(w, "默认目录无效："+err.Error(), http.StatusBadRequest)
		return
	}
	cfg := l.loadCloudAdminSettings()
	reply, qrDataURL, appKey, secretKey, appDir, err := startBaiduDeviceAuth(
		r.Context(), l, cfg, in.UseProxy, in.CustomAppKey, in.CustomSecretKey, in.CustomAppDir,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sessID, err := randomURLState()
	if err != nil {
		http.Error(w, "创建扫码会话失败", http.StatusInternalServerError)
		return
	}
	l.authStore().putBaidu(baiduDeviceSession{
		ID:              sessID,
		Account:         account,
		DeviceCode:      reply.DeviceCode,
		AppKey:          appKey,
		SecretKey:       secretKey,
		AppDir:          appDir,
		Name:            strings.TrimSpace(in.Name),
		RootDir:         strings.TrimSpace(in.RootDir),
		UseProxy:        in.UseProxy,
		CustomAppKey:    strings.TrimSpace(in.CustomAppKey),
		CustomSecretKey: strings.TrimSpace(in.CustomSecretKey),
		Interval:        reply.Interval,
		ExpiresAt:       time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second),
	})
	jsonResponse(w, map[string]any{
		"sessionId":       sessID,
		"userCode":        reply.UserCode,
		"verificationUrl": reply.VerificationURL,
		"qrDataUrl":       qrDataURL,
		"expiresIn":       reply.ExpiresIn,
		"interval":        reply.Interval,
		"appDir":          appDir,
	})
}

func (l *libraryPage) serveBaiduPoll(w http.ResponseWriter, r *http.Request, account string, body []byte) {
	var in struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.SessionID) == "" {
		http.Error(w, "缺少扫码会话编号", http.StatusBadRequest)
		return
	}
	sess, ok := l.authStore().getBaidu(strings.TrimSpace(in.SessionID), account)
	if !ok {
		http.Error(w, "二维码已过期，请重新生成", http.StatusBadRequest)
		return
	}
	cfg := l.loadCloudAdminSettings()
	client, err := newCloudHTTPClient(cfg, "baidu", sess.UseProxy, false, false, 30*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tok, pending, err := pollBaiduDeviceToken(r.Context(), client, sess.DeviceCode, sess.AppKey, sess.SecretKey)
	if pending {
		jsonResponse(w, map[string]any{"status": "pending"})
		return
	}
	l.authStore().deleteBaidu(sess.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	nick := fetchBaiduNetdiskName(r.Context(), client, tok.AccessToken)
	exp := tok.ExpiresIn
	if exp <= 0 {
		exp = 2592000
	}
	sec := baiduSecret{
		RefreshToken:    tok.RefreshToken,
		AccessToken:     tok.AccessToken,
		ExpiryUnix:      time.Now().Unix() + exp,
		NetdiskName:     nick,
		AppDir:          sess.AppDir,
		CustomAppKey:    sess.CustomAppKey,
		CustomSecretKey: sess.CustomSecretKey,
	}
	secBytes, _ := json.Marshal(sec)
	name := sess.Name
	if name == "" {
		name = "我的百度网盘"
	}
	row := cloudConnectionRow{
		ID:           newCloudID(),
		Account:      account,
		Kind:         "baidu",
		Name:         name,
		AccountLabel: nick,
		RootDir:      sess.RootDir,
		UseProxy:     sess.UseProxy,
	}
	if err := l.saveCloudConnection(row, secBytes); err != nil {
		http.Error(w, "保存百度网盘连接失败", http.StatusInternalServerError)
		return
	}
	conns, _ := l.listCloudConnections(account)
	jsonResponse(w, map[string]any{
		"status":      "connected",
		"connections": conns,
		"label":       nick,
	})
}

func (l *libraryPage) serveCloudList(w http.ResponseWriter, r *http.Request, account string, isAdmin bool, body []byte) {
	var in struct {
		ConnectionID string `json:"connectionId"`
		Dir          string `json:"dir"`
	}
	if json.Unmarshal(body, &in) != nil || in.ConnectionID == "" {
		http.Error(w, "请选择云盘连接", http.StatusBadRequest)
		return
	}
	_, provider, err := l.openCloudProvider(account, in.ConnectionID, isAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := provider.List(r.Context(), in.Dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, res)
}

func (l *libraryPage) serveCloudMkdir(w http.ResponseWriter, r *http.Request, account string, isAdmin bool, body []byte) {
	var in struct {
		ConnectionID string `json:"connectionId"`
		ParentDir    string `json:"parentDir"`
		Name         string `json:"name"`
	}
	if json.Unmarshal(body, &in) != nil || in.ConnectionID == "" {
		http.Error(w, "参数无效", http.StatusBadRequest)
		return
	}
	_, provider, err := l.openCloudProvider(account, in.ConnectionID, isAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	entry, err := provider.Mkdir(r.Context(), in.ParentDir, in.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]any{"entry": entry})
}

func (l *libraryPage) serveCloudDownload(w http.ResponseWriter, r *http.Request, account string, isAdmin bool, body []byte) {
	var in struct {
		ConnectionID string `json:"connectionId"`
		FileID       string `json:"fileId"`
	}
	if json.Unmarshal(body, &in) != nil || in.ConnectionID == "" || in.FileID == "" {
		http.Error(w, "请选择要下载的云盘文件", http.StatusBadRequest)
		return
	}
	_, provider, err := l.openCloudProvider(account, in.ConnectionID, isAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := provider.Download(r.Context(), in.FileID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", res.MimeType)
	w.Header().Set("X-Cloud-Filename", url.PathEscape(res.Filename))
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(res.Filename))
	_, _ = w.Write(res.Data)
}

func (l *libraryPage) serveCloudUpload(w http.ResponseWriter, r *http.Request, id string) {
	account := ownerKey(id)
	isAdmin := id == ""
	rawMeta := strings.TrimSpace(r.Header.Get("X-Cloud-Meta"))
	if rawMeta == "" {
		http.Error(w, "缺少上传文件信息", http.StatusBadRequest)
		return
	}
	metaBytes, err := base64.StdEncoding.DecodeString(rawMeta)
	if err != nil {
		http.Error(w, "上传文件信息编码无效", http.StatusBadRequest)
		return
	}
	var meta struct {
		ConnectionID string `json:"connectionId"`
		Dir          string `json:"dir"`
		Filename     string `json:"filename"`
		MimeType     string `json:"mimeType"`
		Overwrite    bool   `json:"overwrite"`
	}
	if json.Unmarshal(metaBytes, &meta) != nil || meta.ConnectionID == "" || meta.Filename == "" {
		http.Error(w, "上传文件参数不完整", http.StatusBadRequest)
		return
	}
	_, provider, err := l.openCloudProvider(account, meta.ConnectionID, isAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCloudFileBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf("文件大小超过 %d MB 上限或读取失败", maxCloudFileBytes>>20), http.StatusRequestEntityTooLarge)
		return
	}
	entry, err := provider.Upload(r.Context(), meta.Dir, meta.Filename, data, meta.MimeType, meta.Overwrite)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]any{"entry": entry})
}

const maxCloudMetaHeaderBytes = 16 << 10

func (l *libraryPage) serveCloudAutoUpload(w http.ResponseWriter, r *http.Request, id string) {
	account := ownerKey(id)
	isAdmin := id == ""
	rawMeta := strings.TrimSpace(r.Header.Get("X-Cloud-Meta"))
	if rawMeta == "" {
		http.Error(w, "缺少上传文件信息", http.StatusBadRequest)
		return
	}
	if len(rawMeta) > maxCloudMetaHeaderBytes {
		http.Error(w, "上传文件信息过大", http.StatusBadRequest)
		return
	}
	metaBytes, err := base64.StdEncoding.DecodeString(rawMeta)
	if err != nil {
		http.Error(w, "上传文件信息编码无效", http.StatusBadRequest)
		return
	}
	var meta struct {
		ConnectionID string `json:"connectionId"`
		ProviderKind string `json:"providerKind"`
		RelDir       string `json:"relDir"`
		Filename     string `json:"filename"`
		MimeType     string `json:"mimeType"`
		Overwrite    *bool  `json:"overwrite"`
	}
	if json.Unmarshal(metaBytes, &meta) != nil {
		http.Error(w, "上传文件参数格式无效", http.StatusBadRequest)
		return
	}
	cleanName, err := cleanCloudFilename(meta.Filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cleanRelDir, err := cleanCloudRelDir(meta.RelDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	explicitKind := strings.TrimSpace(strings.ToLower(meta.ProviderKind))
	providerKind := explicitKind
	if providerKind == "" {
		providerKind = "gdrive"
	}
	if providerKind != "gdrive" && providerKind != "webdav" && providerKind != "baidu" {
		http.Error(w, "不支持的云盘类型", http.StatusBadRequest)
		return
	}
	overwrite := true
	if meta.Overwrite != nil {
		overwrite = *meta.Overwrite
	}
	connID := strings.TrimSpace(meta.ConnectionID)
	mimeType := strings.TrimSpace(meta.MimeType)

	var row cloudConnectionRow
	var plain []byte
	if connID != "" {
		row, plain, err = l.getCloudConnection(account, connID)
		if errors.Is(err, errCloudConnectionNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "读取云盘连接失败", http.StatusInternalServerError)
			return
		}
		if explicitKind != "" && row.Kind != explicitKind {
			http.Error(w, "指定的云盘连接类型与请求不一致", http.StatusBadRequest)
			return
		}
	} else {
		row, plain, err = l.getDefaultCloudConnection(account, providerKind)
		if errors.Is(err, errCloudConnectionNotFound) {
			jsonResponse(w, map[string]any{
				"ok":      true,
				"skipped": true,
				"reason":  "no_" + providerKind + "_connection",
			})
			return
		}
		if err != nil {
			http.Error(w, "读取云盘连接失败", http.StatusInternalServerError)
			return
		}
	}

	provider, err := l.openCloudProviderFromRow(row, plain, isAdmin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCloudFileBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("文件大小超过 %d MB 上限", maxCloudFileBytes>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "读取上传内容失败", http.StatusBadRequest)
		return
	}

	uploadDir := cleanRelDir
	if row.Kind == "gdrive" {
		uploadDir = cloudRelDirPrefix + cleanRelDir
	}
	entry, err := provider.Upload(r.Context(), uploadDir, cleanName, data, mimeType, overwrite)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]any{
		"ok":           true,
		"skipped":      false,
		"connectionId": row.ID,
		"relDir":       cleanRelDir,
		"entry":        entry,
	})
}

func (l *libraryPage) serveCloudBackupExport(w http.ResponseWriter, r *http.Request, account string, isAdmin bool, body []byte) {
	if !isAdmin {
		http.Error(w, "只有管理员可将整库备份或离线题库导出到云盘", http.StatusForbidden)
		return
	}
	var in struct {
		ConnectionID string `json:"connectionId"`
		Dir          string `json:"dir"`
		Kind         string `json:"kind"` // "json" or "offline"
		Overwrite    bool   `json:"overwrite"`
	}
	if json.Unmarshal(body, &in) != nil || in.ConnectionID == "" {
		http.Error(w, "参数无效", http.StatusBadRequest)
		return
	}
	_, provider, err := l.openCloudProvider(account, in.ConnectionID, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stamp := time.Now().Format("2006-01-02")
	var data []byte
	var filename, mimeType string
	if in.Kind == "offline" {
		data, err = l.renderOfflineBytes()
		if err != nil {
			http.Error(w, "生成离线题库失败", http.StatusInternalServerError)
			return
		}
		filename = fmt.Sprintf("生物离线题库-%s.html", stamp)
		mimeType = "text/html; charset=utf-8"
	} else {
		d, err := l.exportBackupData()
		if err != nil {
			http.Error(w, "生成整库备份失败", http.StatusInternalServerError)
			return
		}
		data, err = json.Marshal(d)
		if err != nil {
			http.Error(w, "编码备份数据失败", http.StatusInternalServerError)
			return
		}
		filename = fmt.Sprintf("biofolio-backup-%s.json", stamp)
		mimeType = "application/json; charset=utf-8"
	}
	entry, err := provider.Upload(r.Context(), in.Dir, filename, data, mimeType, in.Overwrite)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]any{"entry": entry})
}

func (l *libraryPage) serveCloudBackupRestore(w http.ResponseWriter, r *http.Request, account string, isAdmin bool, body []byte) {
	if !isAdmin {
		http.Error(w, "只有管理员可从云盘恢复整库备份", http.StatusForbidden)
		return
	}
	var in struct {
		ConnectionID string `json:"connectionId"`
		FileID       string `json:"fileId"`
	}
	if json.Unmarshal(body, &in) != nil || in.ConnectionID == "" || in.FileID == "" {
		http.Error(w, "请选择云盘备份文件", http.StatusBadRequest)
		return
	}
	var ver, recCount int
	_ = l.db.QueryRow("select version from library_state where id=1").Scan(&ver)
	_ = l.db.QueryRow("select count(*) from library_records").Scan(&recCount)
	if ver != 0 || recCount != 0 {
		http.Error(w, "只可恢复到空库", http.StatusConflict)
		return
	}
	_, provider, err := l.openCloudProvider(account, in.ConnectionID, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := provider.Download(r.Context(), in.FileID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !json.Valid(res.Data) {
		http.Error(w, "备份文件不是有效的 JSON 格式", http.StatusBadRequest)
		return
	}
	if status, msg := l.restoreBackupBody(res.Data); status != http.StatusNoContent {
		http.Error(w, msg, status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
