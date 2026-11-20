package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	cloudKeyFilename      = "cloud-secret.key"
	cloudSettingsFilename = "cloud-settings.json"
	cloudAuthTTL          = 15 * time.Minute
	defaultBaiduAppDir    = "/apps/Biofolio"
)

type cloudAdminSettings struct {
	ProxyURL              string   `json:"proxyUrl,omitempty"`
	ProxyScope            string   `json:"proxyScope,omitempty"`
	AllowPrivateWebDAV    *bool    `json:"allowPrivateWebdav,omitempty"`
	AllowedWebDAVPrefixes []string `json:"allowedWebdavPrefixes,omitempty"`
	GoogleClientID        string   `json:"googleClientId,omitempty"`
	GoogleClientSecretEnc string   `json:"googleClientSecretEnc,omitempty"`
	GoogleRedirectURI     string   `json:"googleRedirectUri,omitempty"`
	BaiduAppKey            string   `json:"baiduAppKey,omitempty"`
	BaiduSecretKeyEnc      string   `json:"baiduSecretKeyEnc,omitempty"`
	BaiduAppDir            string   `json:"baiduAppDir,omitempty"`
	AutoBackupEnabled      bool     `json:"autoBackupEnabled,omitempty"`
	AutoBackupConnectionID string   `json:"autoBackupConnectionId,omitempty"`
	AutoBackupDir          string   `json:"autoBackupDir,omitempty"`
	AutoBackupHour         *int     `json:"autoBackupHour,omitempty"`
}

type cloudAdminSettingsView struct {
	ProxyURL               string   `json:"proxyUrl"`
	ProxyScope             string   `json:"proxyScope"`
	AllowPrivateWebDAV     bool     `json:"allowPrivateWebdav"`
	AllowedWebDAVPrefixes  []string `json:"allowedWebdavPrefixes"`
	GoogleClientID         string   `json:"googleClientId"`
	GoogleHasSecret        bool     `json:"googleHasSecret"`
	GoogleRedirectURI      string   `json:"googleRedirectUri"`
	BaiduAppKey            string   `json:"baiduAppKey"`
	BaiduHasSecret         bool     `json:"baiduHasSecret"`
	BaiduAppDir            string   `json:"baiduAppDir"`
	AutoBackupEnabled      bool     `json:"autoBackupEnabled"`
	AutoBackupConnectionID string   `json:"autoBackupConnectionId"`
	AutoBackupDir          string   `json:"autoBackupDir"`
	AutoBackupHour         int      `json:"autoBackupHour"`
	EnvProxy               string   `json:"envProxy,omitempty"`
	EnvAllowPrivateWebDAV  bool     `json:"envAllowPrivateWebdav,omitempty"`
	EnvGoogleReady         bool     `json:"envGoogleReady,omitempty"`
	EnvBaiduReady          bool     `json:"envBaiduReady,omitempty"`
}

type cloudConnectionRow struct {
	ID           string
	Account      string
	Kind         string
	Name         string
	AccountLabel string
	RootDir      string
	IsDefault    bool
	UseProxy     bool
	Secret       []byte
	UpdatedAt    string
}

type cloudConnectionView struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	AccountLabel string `json:"accountLabel"`
	RootDir      string `json:"rootDir"`
	IsDefault    bool   `json:"isDefault"`
	UseProxy     bool   `json:"useProxy"`
	UpdatedAt    string `json:"updatedAt"`
}

type googleOAuthSession struct {
	State              string
	Account            string
	CodeVerifier       string
	RedirectURI        string
	Name               string
	RootDir            string
	CustomClientID     string
	CustomClientSecret string
	UseProxy           bool
	CreatedAt          time.Time
}

type baiduDeviceSession struct {
	ID              string
	Account         string
	DeviceCode      string
	AppKey          string
	SecretKey       string
	AppDir          string
	Name            string
	RootDir         string
	UseProxy        bool
	CustomAppKey    string
	CustomSecretKey string
	Interval        int
	ExpiresAt       time.Time
}

type cloudAuthStore struct {
	mu     sync.Mutex
	google map[string]googleOAuthSession
	baidu  map[string]baiduDeviceSession
}

func newCloudAuthStore() *cloudAuthStore {
	return &cloudAuthStore{
		google: make(map[string]googleOAuthSession),
		baidu:  make(map[string]baiduDeviceSession),
	}
}

func (s *cloudAuthStore) pruneLocked(now time.Time) {
	for k, v := range s.google {
		if now.Sub(v.CreatedAt) > cloudAuthTTL {
			delete(s.google, k)
		}
	}
	for k, v := range s.baidu {
		if now.After(v.ExpiresAt) {
			delete(s.baidu, k)
		}
	}
}

func (s *cloudAuthStore) putGoogle(sess googleOAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneLocked(now)
	sess.CreatedAt = now
	s.google[sess.State] = sess
}

func (s *cloudAuthStore) takeGoogle(state string) (googleOAuthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneLocked(now)
	sess, ok := s.google[state]
	if ok {
		delete(s.google, state)
	}
	return sess, ok
}

func (s *cloudAuthStore) putBaidu(sess baiduDeviceSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	s.baidu[sess.ID] = sess
}

func (s *cloudAuthStore) getBaidu(id, account string) (baiduDeviceSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
	sess, ok := s.baidu[id]
	if !ok || sess.Account != account {
		return baiduDeviceSession{}, false
	}
	return sess, true
}

func (s *cloudAuthStore) deleteBaidu(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.baidu, id)
}

func (l *libraryPage) authStore() *cloudAuthStore {
	l.cloudAuthOnce.Do(func() {
		l.cloudAuth = newCloudAuthStore()
	})
	return l.cloudAuth
}

func (l *libraryPage) getCloudKey() ([]byte, error) {
	l.cloudKeyOnce.Do(func() {
		if l.dir == "" {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				l.cloudKeyErr = err
				return
			}
			l.cloudKey = key
			return
		}
		keyPath := filepath.Join(l.dir, cloudKeyFilename)
		if data, err := os.ReadFile(keyPath); err == nil {
			if len(data) == 32 {
				l.cloudKey = data
				return
			}
			if decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data))); decErr == nil && len(decoded) == 32 {
				l.cloudKey = decoded
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			l.cloudKeyErr = err
			return
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			l.cloudKeyErr = err
			return
		}
		f, err := os.CreateTemp(l.dir, ".cloud-key-*")
		if err != nil {
			l.cloudKeyErr = err
			return
		}
		tmpName := f.Name()
		cleanup := true
		defer func() {
			if cleanup {
				_ = os.Remove(tmpName)
			}
		}()
		if err = f.Chmod(0600); err != nil {
			_ = f.Close()
			l.cloudKeyErr = err
			return
		}
		if _, err = f.Write(key); err != nil {
			_ = f.Close()
			l.cloudKeyErr = err
			return
		}
		if err = f.Sync(); err != nil {
			_ = f.Close()
			l.cloudKeyErr = err
			return
		}
		if err = f.Close(); err != nil {
			l.cloudKeyErr = err
			return
		}
		if err = os.Rename(tmpName, keyPath); err != nil {
			l.cloudKeyErr = err
			return
		}
		cleanup = false
		l.cloudKey = key
	})
	return l.cloudKey, l.cloudKeyErr
}

func encryptCloudSecret(key, plain []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("云盘加密密钥长度无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func decryptCloudSecret(key, ciphertext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("云盘加密密钥长度无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("云盘凭据已损坏，请重新绑定")
	}
	plain, err := gcm.Open(nil, ciphertext[:ns], ciphertext[ns:], nil)
	if err != nil {
		return nil, errors.New("云盘凭据解密失败，请重新绑定")
	}
	return plain, nil
}

func encryptCloudString(key []byte, plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	enc, err := encryptCloudSecret(key, []byte(plain))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

func decryptCloudString(key []byte, encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("云盘密钥格式损坏")
	}
	plain, err := decryptCloudSecret(key, raw)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func envBoolTrue(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func (s cloudAdminSettings) effectiveProxyURL() string {
	if p := strings.TrimSpace(s.ProxyURL); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv("BIOLOGY_CLOUD_PROXY"))
}

func (s cloudAdminSettings) effectiveProxyScope() string {
	if s.ProxyScope == "all" {
		return "all"
	}
	return "google"
}

func (s cloudAdminSettings) effectiveAllowPrivateWebDAV() bool {
	if s.AllowPrivateWebDAV != nil {
		return *s.AllowPrivateWebDAV
	}
	return envBoolTrue("BIOLOGY_WEBDAV_ALLOW_PRIVATE")
}

func (s cloudAdminSettings) effectiveAutoBackupHour() int {
	if s.AutoBackupHour == nil {
		return 3
	}
	h := *s.AutoBackupHour
	if h < 0 || h > 23 {
		return 3
	}
	return h
}

func (s cloudAdminSettings) effectiveGoogleCreds(key []byte) (clientID, clientSecret, redirectURI string, err error) {
	clientID = strings.TrimSpace(s.GoogleClientID)
	if s.GoogleClientSecretEnc != "" {
		clientSecret, err = decryptCloudString(key, s.GoogleClientSecretEnc)
		if err != nil {
			return "", "", "", err
		}
	}
	redirectURI = strings.TrimSpace(s.GoogleRedirectURI)
	if clientID == "" {
		clientID = strings.TrimSpace(os.Getenv("BIOLOGY_GOOGLE_CLIENT_ID"))
		if clientSecret == "" {
			clientSecret = strings.TrimSpace(os.Getenv("BIOLOGY_GOOGLE_CLIENT_SECRET"))
		}
	}
	if redirectURI == "" {
		redirectURI = strings.TrimSpace(os.Getenv("BIOLOGY_GOOGLE_REDIRECT_URI"))
	}
	return clientID, strings.TrimSpace(clientSecret), redirectURI, nil
}

func normalizeBaiduAppDir(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultBaiduAppDir, nil
	}
	if strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
		return "", errors.New("百度网盘应用目录不能包含控制字符")
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/apps/" + trimmed
	}
	cleaned := path.Clean(trimmed)
	if !strings.HasPrefix(cleaned, "/apps/") {
		return "", errors.New("百度网盘应用目录必须位于 /apps/<应用目录名> 下")
	}
	name := strings.TrimPrefix(cleaned, "/apps/")
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", errors.New("请填写有效的百度网盘应用目录名（如 /apps/Biofolio）")
	}
	return "/apps/" + name, nil
}

func (s cloudAdminSettings) effectiveBaiduCreds(key []byte) (appKey, secretKey, appDir string, err error) {
	appKey = strings.TrimSpace(s.BaiduAppKey)
	if s.BaiduSecretKeyEnc != "" {
		secretKey, err = decryptCloudString(key, s.BaiduSecretKeyEnc)
		if err != nil {
			return "", "", "", err
		}
	}
	if appKey == "" {
		appKey = strings.TrimSpace(os.Getenv("BIOLOGY_BAIDU_APP_KEY"))
		if secretKey == "" {
			secretKey = strings.TrimSpace(os.Getenv("BIOLOGY_BAIDU_SECRET_KEY"))
		}
	}
	rawDir := s.BaiduAppDir
	if strings.TrimSpace(rawDir) == "" {
		rawDir = os.Getenv("BIOLOGY_BAIDU_APP_DIR")
	}
	appDir, err = normalizeBaiduAppDir(rawDir)
	if err != nil {
		appDir = defaultBaiduAppDir
	}
	return appKey, strings.TrimSpace(secretKey), appDir, nil
}

func (l *libraryPage) cloudSettingsPath() string {
	return filepath.Join(l.dir, cloudSettingsFilename)
}

func (l *libraryPage) loadCloudAdminSettings() cloudAdminSettings {
	l.cloudSettingsMu.Lock()
	defer l.cloudSettingsMu.Unlock()
	if l.dir == "" {
		return l.cloudMemSettings
	}
	var s cloudAdminSettings
	if data, err := os.ReadFile(l.cloudSettingsPath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func (l *libraryPage) saveCloudAdminSettings(s cloudAdminSettings) error {
	l.cloudSettingsMu.Lock()
	defer l.cloudSettingsMu.Unlock()
	if l.dir == "" {
		l.cloudMemSettings = s
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(l.dir, ".cloud-settings-*")
	if err != nil {
		return errors.New("云盘设置保存失败")
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)
	_ = f.Chmod(0600)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err != nil || closeErr != nil || os.Rename(tmpName, l.cloudSettingsPath()) != nil {
		return errors.New("云盘设置保存失败")
	}
	return nil
}

func (l *libraryPage) cloudAdminView() cloudAdminSettingsView {
	s := l.loadCloudAdminSettings()
	prefixes := s.AllowedWebDAVPrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	dir := s.BaiduAppDir
	if dir == "" {
		if d, err := normalizeBaiduAppDir(os.Getenv("BIOLOGY_BAIDU_APP_DIR")); err == nil {
			dir = d
		} else {
			dir = defaultBaiduAppDir
		}
	}
	return cloudAdminSettingsView{
		ProxyURL:               s.ProxyURL,
		ProxyScope:             s.effectiveProxyScope(),
		AllowPrivateWebDAV:     s.effectiveAllowPrivateWebDAV(),
		AllowedWebDAVPrefixes:  prefixes,
		GoogleClientID:         s.GoogleClientID,
		GoogleHasSecret:        s.GoogleClientSecretEnc != "",
		GoogleRedirectURI:      s.GoogleRedirectURI,
		BaiduAppKey:            s.BaiduAppKey,
		BaiduHasSecret:         s.BaiduSecretKeyEnc != "",
		BaiduAppDir:            dir,
		AutoBackupEnabled:      s.AutoBackupEnabled,
		AutoBackupConnectionID: s.AutoBackupConnectionID,
		AutoBackupDir:          s.AutoBackupDir,
		AutoBackupHour:         s.effectiveAutoBackupHour(),
		EnvProxy:               strings.TrimSpace(os.Getenv("BIOLOGY_CLOUD_PROXY")),
		EnvAllowPrivateWebDAV:  envBoolTrue("BIOLOGY_WEBDAV_ALLOW_PRIVATE"),
		EnvGoogleReady:         strings.TrimSpace(os.Getenv("BIOLOGY_GOOGLE_CLIENT_ID")) != "",
		EnvBaiduReady:          strings.TrimSpace(os.Getenv("BIOLOGY_BAIDU_APP_KEY")) != "" && strings.TrimSpace(os.Getenv("BIOLOGY_BAIDU_SECRET_KEY")) != "",
	}
}

func (l *libraryPage) listCloudConnections(account string) ([]cloudConnectionView, error) {
	rows, err := l.db.Query(
		`SELECT id, kind, name, account_label, root_dir, is_default, use_proxy, updated_at
		 FROM library_cloud_connections WHERE account=? ORDER BY is_default DESC, updated_at DESC, id ASC`,
		account,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cloudConnectionView{}
	for rows.Next() {
		var v cloudConnectionView
		var isDef, usePrx int
		if err := rows.Scan(&v.ID, &v.Kind, &v.Name, &v.AccountLabel, &v.RootDir, &isDef, &usePrx, &v.UpdatedAt); err != nil {
			return nil, err
		}
		v.IsDefault = isDef != 0
		v.UseProxy = usePrx != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

func (l *libraryPage) getCloudConnection(account, id string) (cloudConnectionRow, []byte, error) {
	var row cloudConnectionRow
	var isDef, usePrx int
	err := l.db.QueryRow(
		`SELECT id, account, kind, name, account_label, root_dir, is_default, use_proxy, secret, updated_at
		 FROM library_cloud_connections WHERE id=? AND account=?`,
		id, account,
	).Scan(&row.ID, &row.Account, &row.Kind, &row.Name, &row.AccountLabel, &row.RootDir, &isDef, &usePrx, &row.Secret, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return row, nil, errors.New("找不到该云盘连接或无权访问")
	}
	if err != nil {
		return row, nil, err
	}
	row.IsDefault = isDef != 0
	row.UseProxy = usePrx != 0
	key, err := l.getCloudKey()
	if err != nil {
		return row, nil, err
	}
	plain, err := decryptCloudSecret(key, row.Secret)
	if err != nil {
		return row, nil, err
	}
	return row, plain, nil
}

func (l *libraryPage) saveCloudConnection(row cloudConnectionRow, plainSecret []byte) error {
	key, err := l.getCloudKey()
	if err != nil {
		return err
	}
	enc, err := encryptCloudSecret(key, plainSecret)
	if err != nil {
		return err
	}
	row.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM library_cloud_connections WHERE account=? AND id!=?`, row.Account, row.ID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		row.IsDefault = true
	}
	if row.IsDefault {
		if _, err := tx.Exec(`UPDATE library_cloud_connections SET is_default=0 WHERE account=?`, row.Account); err != nil {
			return err
		}
	}
	isDef, usePrx := 0, 0
	if row.IsDefault {
		isDef = 1
	}
	if row.UseProxy {
		usePrx = 1
	}
	_, err = tx.Exec(
		`INSERT INTO library_cloud_connections(id, account, kind, name, account_label, root_dir, is_default, use_proxy, secret, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   name=excluded.name,
		   account_label=excluded.account_label,
		   root_dir=excluded.root_dir,
		   is_default=excluded.is_default,
		   use_proxy=excluded.use_proxy,
		   secret=excluded.secret,
		   updated_at=excluded.updated_at
		 WHERE library_cloud_connections.account=excluded.account`,
		row.ID, row.Account, row.Kind, row.Name, row.AccountLabel, row.RootDir, isDef, usePrx, enc, row.UpdatedAt,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (l *libraryPage) updateCloudConnectionSecret(account, id string, plainSecret []byte, accountLabel string) error {
	key, err := l.getCloudKey()
	if err != nil {
		return err
	}
	enc, err := encryptCloudSecret(key, plainSecret)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if accountLabel != "" {
		_, err = l.db.Exec(
			`UPDATE library_cloud_connections SET secret=?, account_label=?, updated_at=? WHERE id=? AND account=?`,
			enc, accountLabel, now, id, account,
		)
		return err
	}
	_, err = l.db.Exec(
		`UPDATE library_cloud_connections SET secret=?, updated_at=? WHERE id=? AND account=?`,
		enc, now, id, account,
	)
	return err
}

func (l *libraryPage) updateCloudConnectionMeta(account, id, name, rootDir string, isDefault, useProxy bool) error {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 80 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("云盘名称需为 1 至 80 个字符")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if isDefault {
		if _, err := tx.Exec(`UPDATE library_cloud_connections SET is_default=0 WHERE account=?`, account); err != nil {
			return err
		}
	}
	isDef, usePrx := 0, 0
	if isDefault {
		isDef = 1
	}
	if useProxy {
		usePrx = 1
	}
	res, err := tx.Exec(
		`UPDATE library_cloud_connections SET name=?, root_dir=?, is_default=?, use_proxy=?, updated_at=? WHERE id=? AND account=?`,
		name, strings.TrimSpace(rootDir), isDef, usePrx, time.Now().UTC().Format(time.RFC3339), id, account,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("找不到该云盘连接或无权修改")
	}
	return tx.Commit()
}

func (l *libraryPage) deleteCloudConnection(account, id string) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM library_cloud_connections WHERE id=? AND account=?`, id, account)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("找不到该云盘连接或无权删除")
	}
	var hasDefault int
	if err := tx.QueryRow(`SELECT count(*) FROM library_cloud_connections WHERE account=? AND is_default=1`, account).Scan(&hasDefault); err == nil && hasDefault == 0 {
		_, _ = tx.Exec(`UPDATE library_cloud_connections SET is_default=1 WHERE id=(SELECT id FROM library_cloud_connections WHERE account=? ORDER BY updated_at DESC LIMIT 1)`, account)
	}
	return tx.Commit()
}

func newCloudID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("cloud-%x", b)
}
