package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	baiduOAuthBase = "https://openapi.baidu.com/oauth/2.0"
	baiduPanBase   = "https://pan.baidu.com/rest/2.0/xpan"
	baiduPCSBase   = "https://d.pcs.baidu.com/rest/2.0/pcs"
)

const (
	baiduUserAgent = "pan.baidu.com"
	baiduChunkSize = 4 << 20
)

var baiduTokenRefreshLocks sync.Map // connID -> *sync.Mutex

func lockForBaiduConn(connID string) *sync.Mutex {
	mu, _ := baiduTokenRefreshLocks.LoadOrStore(connID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

type baiduSecret struct {
	RefreshToken    string `json:"refreshToken"`
	AccessToken     string `json:"accessToken"`
	ExpiryUnix      int64  `json:"expiryUnix"`
	NetdiskName     string `json:"netdiskName"`
	AppDir          string `json:"appDir"`
	CustomAppKey    string `json:"customAppKey,omitempty"`
	CustomSecretKey string `json:"customSecretKey,omitempty"`
}

type baiduTokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type baiduDeviceCodeReply struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	QRCodeURL       string `json:"qrcode_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

type baiduProvider struct {
	l      *libraryPage
	cfg    cloudAdminSettings
	row    cloudConnectionRow
	sec    baiduSecret
	client *http.Client
}

func resolveBaiduAppCreds(l *libraryPage, cfg cloudAdminSettings, customKey, customSecret, customDir string) (appKey, secretKey, appDir string, err error) {
	key, err := l.getCloudKey()
	if err != nil {
		return "", "", "", err
	}
	sharedKey, sharedSecret, sharedDir, err := cfg.effectiveBaiduCreds(key)
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(customKey) != "" {
		appKey = strings.TrimSpace(customKey)
		secretKey = strings.TrimSpace(customSecret)
		if secretKey == "" {
			return "", "", "", errors.New("填写自定义百度 AppKey 时需同时填写 SecretKey")
		}
	} else {
		appKey, secretKey = sharedKey, sharedSecret
	}
	if appKey == "" || secretKey == "" {
		return "", "", "", errors.New("尚未配置百度网盘 AppKey / SecretKey：请联系管理员在「设置 → 云盘接入服务」配置，或填写自己的应用凭证")
	}
	if strings.TrimSpace(customDir) != "" {
		appDir, err = normalizeBaiduAppDir(customDir)
		if err != nil {
			return "", "", "", err
		}
	} else {
		appDir = sharedDir
	}
	return appKey, secretKey, appDir, nil
}

func startBaiduDeviceAuth(ctx context.Context, l *libraryPage, cfg cloudAdminSettings, useProxy bool, customKey, customSecret, customDir string) (baiduDeviceCodeReply, string, string, string, string, error) {
	appKey, secretKey, appDir, err := resolveBaiduAppCreds(l, cfg, customKey, customSecret, customDir)
	if err != nil {
		return baiduDeviceCodeReply{}, "", "", "", "", err
	}
	client, err := newCloudHTTPClient(cfg, "baidu", useProxy, false, false, 30*time.Second)
	if err != nil {
		return baiduDeviceCodeReply{}, "", "", "", "", err
	}
	q := url.Values{}
	q.Set("response_type", "device_code")
	q.Set("client_id", appKey)
	q.Set("scope", "basic,netdisk")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baiduOAuthBase+"/device/code?"+q.Encode(), nil)
	if err != nil {
		return baiduDeviceCodeReply{}, "", "", "", "", err
	}
	req.Header.Set("User-Agent", baiduUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return baiduDeviceCodeReply{}, "", "", "", "", fmt.Errorf("连接百度开放平台失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return baiduDeviceCodeReply{}, "", "", "", "", err
	}
	var out baiduDeviceCodeReply
	if json.Unmarshal(raw, &out) != nil {
		return baiduDeviceCodeReply{}, "", "", "", "", fmt.Errorf("百度开放平台返回异常（%s）", resp.Status)
	}
	if out.Error != "" || out.DeviceCode == "" {
		msg := out.ErrorDesc
		if msg == "" {
			msg = out.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return baiduDeviceCodeReply{}, "", "", "", "", fmt.Errorf("获取百度网盘二维码失败：%s", msg)
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 300
	}
	qrDataURL := ""
	if out.QRCodeURL != "" {
		if qrReq, err := http.NewRequestWithContext(ctx, http.MethodGet, out.QRCodeURL, nil); err == nil {
			if qrResp, err := client.Do(qrReq); err == nil {
				func() {
					defer qrResp.Body.Close()
					if qrResp.StatusCode == http.StatusOK {
						if imgBytes, err := readBodyLimited(qrResp.Body, 256<<10); err == nil && len(imgBytes) > 0 {
							mime := qrResp.Header.Get("Content-Type")
							if !strings.HasPrefix(mime, "image/") {
								mime = "image/png"
							}
							qrDataURL = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imgBytes)
						}
					}
				}()
			}
		}
	}
	return out, qrDataURL, appKey, secretKey, appDir, nil
}

func pollBaiduDeviceToken(ctx context.Context, client *http.Client, deviceCode, appKey, secretKey string) (tok baiduTokenReply, pending bool, err error) {
	q := url.Values{}
	q.Set("grant_type", "device_token")
	q.Set("code", deviceCode)
	q.Set("client_id", appKey)
	q.Set("client_secret", secretKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baiduOAuthBase+"/token?"+q.Encode(), nil)
	if err != nil {
		return baiduTokenReply{}, false, err
	}
	req.Header.Set("User-Agent", baiduUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return baiduTokenReply{}, false, err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return baiduTokenReply{}, false, err
	}
	if json.Unmarshal(raw, &tok) != nil {
		return baiduTokenReply{}, false, fmt.Errorf("百度授权返回格式无效（%s）", resp.Status)
	}
	if tok.Error == "authorization_pending" || tok.Error == "slow_down" {
		return tok, true, nil
	}
	if tok.Error != "" || tok.AccessToken == "" {
		msg := tok.ErrorDesc
		if msg == "" {
			msg = tok.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return tok, false, fmt.Errorf("百度授权失败：%s", msg)
	}
	return tok, false, nil
}

func fetchBaiduNetdiskName(ctx context.Context, client *http.Client, accessToken string) string {
	q := url.Values{}
	q.Set("method", "uinfo")
	q.Set("access_token", accessToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baiduPanBase+"/nas?"+q.Encode(), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", baiduUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var info struct {
		NetdiskName string `json:"netdisk_name"`
		BaiduName   string `json:"baidu_name"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info) == nil {
		if info.NetdiskName != "" {
			return info.NetdiskName
		}
		return info.BaiduName
	}
	return ""
}

func newBaiduProvider(l *libraryPage, cfg cloudAdminSettings, row cloudConnectionRow, sec baiduSecret, isAdmin bool) (*baiduProvider, error) {
	if normDir, err := normalizeBaiduAppDir(sec.AppDir); err == nil {
		sec.AppDir = normDir
	} else {
		sec.AppDir = defaultBaiduAppDir
	}
	client, err := newCloudHTTPClient(cfg, "baidu", row.UseProxy, false, false, 3*time.Minute)
	if err != nil {
		return nil, err
	}
	return &baiduProvider{
		l:      l,
		cfg:    cfg,
		row:    row,
		sec:    sec,
		client: client,
	}, nil
}

func (p *baiduProvider) ensureToken(ctx context.Context) (string, error) {
	now := time.Now().Unix()
	if p.sec.AccessToken != "" && p.sec.ExpiryUnix > now+86400 {
		return p.sec.AccessToken, nil
	}
	mu := lockForBaiduConn(p.row.ID)
	mu.Lock()
	defer mu.Unlock()
	if _, plain, err := p.l.getCloudConnection(p.row.Account, p.row.ID); err == nil {
		var latest baiduSecret
		if json.Unmarshal(plain, &latest) == nil && latest.AccessToken != "" && latest.ExpiryUnix > time.Now().Unix()+86400 {
			p.sec = latest
			return p.sec.AccessToken, nil
		}
	}
	if p.sec.RefreshToken == "" {
		if p.sec.AccessToken != "" && p.sec.ExpiryUnix > now+60 {
			return p.sec.AccessToken, nil
		}
		return "", errors.New("百度网盘授权已过期，请重新扫码绑定")
	}
	appKey, secretKey, _, err := resolveBaiduAppCreds(p.l, p.cfg, p.sec.CustomAppKey, p.sec.CustomSecretKey, p.sec.AppDir)
	if err != nil {
		if p.sec.AccessToken != "" && p.sec.ExpiryUnix > now+60 {
			return p.sec.AccessToken, nil
		}
		return "", err
	}
	q := url.Values{}
	q.Set("grant_type", "refresh_token")
	q.Set("refresh_token", p.sec.RefreshToken)
	q.Set("client_id", appKey)
	q.Set("client_secret", secretKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baiduOAuthBase+"/token?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", baiduUserAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		if p.sec.AccessToken != "" && p.sec.ExpiryUnix > now+60 {
			return p.sec.AccessToken, nil
		}
		return "", err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return "", err
	}
	var tok baiduTokenReply
	if json.Unmarshal(raw, &tok) != nil || tok.Error != "" || tok.AccessToken == "" {
		if p.sec.AccessToken != "" && p.sec.ExpiryUnix > now+60 {
			return p.sec.AccessToken, nil
		}
		return "", fmt.Errorf("刷新百度网盘令牌失败：%s", tok.ErrorDesc)
	}
	p.sec.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		p.sec.RefreshToken = tok.RefreshToken
	}
	exp := tok.ExpiresIn
	if exp <= 0 {
		exp = 2592000
	}
	p.sec.ExpiryUnix = time.Now().Unix() + exp
	if enc, err := json.Marshal(p.sec); err == nil {
		_ = p.l.updateCloudConnectionSecret(p.row.Account, p.row.ID, enc, p.sec.NetdiskName)
	}
	return p.sec.AccessToken, nil
}

func baiduErrnoMessage(errno int) string {
	switch errno {
	case -6:
		return "百度网盘登录已失效，请重新扫码绑定（errno -6）"
	case -7:
		return "文件或文件夹名称包含百度网盘不支持的字符（errno -7）"
	case -8:
		return "百度网盘已存在同名文件或目录（errno -8）"
	case -9:
		return "百度网盘目录或文件不存在（errno -9）"
	case 31024, 31034:
		return fmt.Sprintf("百度网盘无权限访问或请求过于频繁（errno %d）", errno)
	case 31362:
		return "百度网盘下载签名校验失败（errno 31362）"
	default:
		return fmt.Sprintf("百度网盘接口返回错误（errno %d）", errno)
	}
}

func (p *baiduProvider) remotePath(relPath string) (remote string, relNorm string, err error) {
	rootNorm, err := normalizeRelativeCloudDir(p.row.RootDir)
	if err != nil {
		return "", "", fmt.Errorf("默认目录配置无效：%w", err)
	}
	relNorm, err = normalizeRelativeCloudDir(relPath)
	if err != nil {
		return "", "", err
	}
	remote = p.sec.AppDir + rootNorm + relNorm
	return remote, relNorm, nil
}

func (p *baiduProvider) rootLabel() string {
	if rootNorm, err := normalizeRelativeCloudDir(p.row.RootDir); err == nil && rootNorm != "" {
		return strings.TrimPrefix(rootNorm, "/")
	}
	return strings.TrimPrefix(p.sec.AppDir, "/apps/")
}

func (p *baiduProvider) Test(ctx context.Context) (string, error) {
	token, err := p.ensureToken(ctx)
	if err != nil {
		return "", err
	}
	name := fetchBaiduNetdiskName(ctx, p.client, token)
	if name == "" {
		return p.sec.NetdiskName, nil
	}
	return name, nil
}

func (p *baiduProvider) List(ctx context.Context, dir string) (cloudListResult, error) {
	token, err := p.ensureToken(ctx)
	if err != nil {
		return cloudListResult{}, err
	}
	remoteDir, relNorm, err := p.remotePath(dir)
	if err != nil {
		return cloudListResult{}, err
	}
	q := url.Values{}
	q.Set("method", "list")
	q.Set("access_token", token)
	q.Set("dir", remoteDir)
	q.Set("order", "time")
	q.Set("desc", "1")
	q.Set("start", "0")
	q.Set("limit", "500")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baiduPanBase+"/file?"+q.Encode(), nil)
	if err != nil {
		return cloudListResult{}, err
	}
	req.Header.Set("User-Agent", baiduUserAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return cloudListResult{}, err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 8<<20)
	if err != nil {
		return cloudListResult{}, err
	}
	var out struct {
		Errno int `json:"errno"`
		List  []struct {
			FsID           uint64 `json:"fs_id"`
			Path           string `json:"path"`
			ServerFilename string `json:"server_filename"`
			IsDir          int    `json:"isdir"`
			Size           int64  `json:"size"`
			ServerMtime    int64  `json:"server_mtime"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return cloudListResult{}, fmt.Errorf("解析百度网盘目录响应失败（%s）", resp.Status)
	}
	if out.Errno == -9 {
		dirName := p.rootLabel()
		if relNorm != "" {
			dirName = path.Base(relNorm)
		}
		return cloudListResult{
			Dir:       relNorm,
			DirName:   dirName,
			ParentDir: parentRelativeCloudDir(relNorm),
			Crumbs:    buildRelativeCrumbs(p.rootLabel(), relNorm),
			Entries:   []cloudEntry{},
		}, nil
	}
	if out.Errno != 0 {
		return cloudListResult{}, errors.New(baiduErrnoMessage(out.Errno))
	}
	entries := make([]cloudEntry, 0, len(out.List))
	for _, it := range out.List {
		name := it.ServerFilename
		if name == "" {
			name = path.Base(it.Path)
		}
		relEntry := "/" + name
		if relNorm != "" {
			relEntry = relNorm + "/" + name
		}
		mod := ""
		if it.ServerMtime > 0 {
			mod = time.Unix(it.ServerMtime, 0).UTC().Format(time.RFC3339)
		}
		id := relEntry
		if it.IsDir == 0 {
			id = fmt.Sprintf("%d|%s", it.FsID, relEntry)
		}
		entries = append(entries, cloudEntry{
			ID:       id,
			Name:     name,
			Path:     relEntry,
			IsDir:    it.IsDir != 0,
			Size:     it.Size,
			Modified: mod,
		})
	}
	sortCloudEntries(entries)
	dirName := p.rootLabel()
	if relNorm != "" {
		dirName = path.Base(relNorm)
	}
	return cloudListResult{
		Dir:       relNorm,
		DirName:   dirName,
		ParentDir: parentRelativeCloudDir(relNorm),
		Crumbs:    buildRelativeCrumbs(p.rootLabel(), relNorm),
		Entries:   entries,
	}, nil
}

func (p *baiduProvider) Mkdir(ctx context.Context, parentDir, name string) (cloudEntry, error) {
	cleanName, err := cleanCloudFilename(name)
	if err != nil {
		return cloudEntry{}, err
	}
	token, err := p.ensureToken(ctx)
	if err != nil {
		return cloudEntry{}, err
	}
	parentRel, err := normalizeRelativeCloudDir(parentDir)
	if err != nil {
		return cloudEntry{}, err
	}
	relEntry := "/" + cleanName
	if parentRel != "" {
		relEntry = parentRel + "/" + cleanName
	}
	remote, _, err := p.remotePath(relEntry)
	if err != nil {
		return cloudEntry{}, err
	}
	form := url.Values{}
	form.Set("path", remote)
	form.Set("isdir", "1")
	form.Set("rtype", "0")
	q := url.Values{}
	q.Set("method", "create")
	q.Set("access_token", token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baiduPanBase+"/file?"+q.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return cloudEntry{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", baiduUserAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return cloudEntry{}, err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return cloudEntry{}, err
	}
	var out struct {
		Errno int    `json:"errno"`
		Path  string `json:"path"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return cloudEntry{}, fmt.Errorf("百度网盘创建文件夹返回异常（%s）", resp.Status)
	}
	if out.Errno != 0 && out.Errno != -8 {
		return cloudEntry{}, errors.New(baiduErrnoMessage(out.Errno))
	}
	return cloudEntry{
		ID:    relEntry,
		Name:  cleanName,
		Path:  relEntry,
		IsDir: true,
	}, nil
}

func (p *baiduProvider) Download(ctx context.Context, fileID string) (cloudDownloadResult, error) {
	fsPart := fileID
	if idx := strings.IndexByte(fileID, '|'); idx >= 0 {
		fsPart = fileID[:idx]
	}
	fsID, err := strconv.ParseUint(strings.TrimSpace(fsPart), 10, 64)
	if err != nil || fsID == 0 {
		return cloudDownloadResult{}, errors.New("无效的百度网盘文件编号")
	}
	token, err := p.ensureToken(ctx)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	q := url.Values{}
	q.Set("method", "filemetas")
	q.Set("access_token", token)
	q.Set("fsids", fmt.Sprintf("[%d]", fsID))
	q.Set("dlink", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baiduPanBase+"/multimedia?"+q.Encode(), nil)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	req.Header.Set("User-Agent", baiduUserAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	defer resp.Body.Close()
	raw, err := readBodyLimited(resp.Body, 1<<20)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	var metas struct {
		Errno int `json:"errno"`
		List  []struct {
			FsID     uint64 `json:"fs_id"`
			Filename string `json:"filename"`
			IsDir    int    `json:"isdir"`
			Dlink    string `json:"dlink"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &metas); err != nil {
		return cloudDownloadResult{}, err
	}
	if metas.Errno != 0 {
		return cloudDownloadResult{}, errors.New(baiduErrnoMessage(metas.Errno))
	}
	if len(metas.List) == 0 || metas.List[0].Dlink == "" {
		return cloudDownloadResult{}, errors.New("未找到该百度网盘文件或暂无下载链接")
	}
	item := metas.List[0]
	if item.IsDir != 0 {
		return cloudDownloadResult{}, errors.New("不能直接下载文件夹")
	}
	sep := "&"
	if !strings.Contains(item.Dlink, "?") {
		sep = "?"
	}
	dlURL := item.Dlink + sep + "access_token=" + url.QueryEscape(token)
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	dlReq.Header.Set("User-Agent", baiduUserAgent)
	dlResp, err := p.client.Do(dlReq)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK && dlResp.StatusCode != http.StatusPartialContent {
		return cloudDownloadResult{}, fmt.Errorf("下载百度网盘文件失败（%s）", dlResp.Status)
	}
	data, err := readBodyLimited(dlResp.Body, maxCloudFileBytes)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	mime := dlResp.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	return cloudDownloadResult{
		Filename: item.Filename,
		MimeType: mime,
		Data:     data,
	}, nil
}

func (p *baiduProvider) Upload(ctx context.Context, dir, filename string, data []byte, mimeType string, overwrite bool) (cloudEntry, error) {
	if int64(len(data)) > maxCloudFileBytes {
		return cloudEntry{}, fmt.Errorf("文件大小超过 %d MB 上限", maxCloudFileBytes>>20)
	}
	cleanName, err := cleanCloudFilename(filename)
	if err != nil {
		return cloudEntry{}, err
	}
	token, err := p.ensureToken(ctx)
	if err != nil {
		return cloudEntry{}, err
	}
	dirNorm, err := normalizeRelativeCloudDir(dir)
	if err != nil {
		return cloudEntry{}, err
	}
	relEntry := "/" + cleanName
	if dirNorm != "" {
		relEntry = dirNorm + "/" + cleanName
	}
	remotePath, _, err := p.remotePath(relEntry)
	if err != nil {
		return cloudEntry{}, err
	}

	var chunks [][]byte
	var hashes []string
	if len(data) == 0 {
		chunks = [][]byte{{}}
		sum := md5.Sum(nil)
		hashes = []string{hex.EncodeToString(sum[:])}
	} else {
		for off := 0; off < len(data); off += baiduChunkSize {
			end := off + baiduChunkSize
			if end > len(data) {
				end = len(data)
			}
			chunk := data[off:end]
			chunks = append(chunks, chunk)
			sum := md5.Sum(chunk)
			hashes = append(hashes, hex.EncodeToString(sum[:]))
		}
	}
	blockListBytes, _ := json.Marshal(hashes)
	blockListJSON := string(blockListBytes)
	rtype := "1"
	if overwrite {
		rtype = "3"
	}

	// Step 1: precreate
	preForm := url.Values{}
	preForm.Set("path", remotePath)
	preForm.Set("size", strconv.Itoa(len(data)))
	preForm.Set("isdir", "0")
	preForm.Set("autoinit", "1")
	preForm.Set("rtype", rtype)
	preForm.Set("block_list", blockListJSON)
	preQ := url.Values{}
	preQ.Set("method", "precreate")
	preQ.Set("access_token", token)
	preReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baiduPanBase+"/file?"+preQ.Encode(), strings.NewReader(preForm.Encode()))
	if err != nil {
		return cloudEntry{}, err
	}
	preReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	preReq.Header.Set("User-Agent", baiduUserAgent)
	preResp, err := p.client.Do(preReq)
	if err != nil {
		return cloudEntry{}, err
	}
	defer preResp.Body.Close()
	preRaw, err := readBodyLimited(preResp.Body, 1<<20)
	if err != nil {
		return cloudEntry{}, err
	}
	var preOut struct {
		Errno      int    `json:"errno"`
		Path       string `json:"path"`
		UploadID   string `json:"uploadid"`
		ReturnType int    `json:"return_type"`
		BlockList  []int  `json:"block_list"`
		Info       struct {
			FsID uint64 `json:"fs_id"`
			Path string `json:"path"`
		} `json:"info"`
	}
	if err := json.Unmarshal(preRaw, &preOut); err != nil {
		return cloudEntry{}, fmt.Errorf("百度网盘预上传返回异常（%s）", preResp.Status)
	}
	if preOut.Errno != 0 {
		return cloudEntry{}, errors.New(baiduErrnoMessage(preOut.Errno))
	}
	if preOut.ReturnType == 2 {
		finalName := cleanName
		if preOut.Info.Path != "" {
			finalName = path.Base(preOut.Info.Path)
		} else if preOut.Path != "" {
			finalName = path.Base(preOut.Path)
		}
		finalRel := "/" + finalName
		if dirNorm != "" {
			finalRel = dirNorm + "/" + finalName
		}
		return cloudEntry{
			ID:    fmt.Sprintf("%d|%s", preOut.Info.FsID, finalRel),
			Name:  finalName,
			Path:  finalRel,
			IsDir: false,
			Size:  int64(len(data)),
			Mime:  mimeType,
		}, nil
	}

	// Step 2: superfile2 chunk upload
	toUpload := preOut.BlockList
	if len(toUpload) == 0 {
		for i := range chunks {
			toUpload = append(toUpload, i)
		}
	}
	for _, seq := range toUpload {
		if seq < 0 || seq >= len(chunks) {
			return cloudEntry{}, fmt.Errorf("百度网盘分片索引越界：%d", seq)
		}
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, err := mw.CreateFormFile("file", cleanName)
		if err != nil {
			return cloudEntry{}, err
		}
		if _, err := part.Write(chunks[seq]); err != nil {
			return cloudEntry{}, err
		}
		if err := mw.Close(); err != nil {
			return cloudEntry{}, err
		}
		upQ := url.Values{}
		upQ.Set("method", "upload")
		upQ.Set("type", "tmpfile")
		upQ.Set("access_token", token)
		upQ.Set("path", remotePath)
		upQ.Set("uploadid", preOut.UploadID)
		upQ.Set("partseq", strconv.Itoa(seq))
		upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baiduPCSBase+"/superfile2?"+upQ.Encode(), &body)
		if err != nil {
			return cloudEntry{}, err
		}
		upReq.Header.Set("Content-Type", mw.FormDataContentType())
		upReq.Header.Set("User-Agent", baiduUserAgent)
		upResp, err := p.client.Do(upReq)
		if err != nil {
			return cloudEntry{}, err
		}
		upRaw, readErr := readBodyLimited(upResp.Body, 1<<20)
		upResp.Body.Close()
		if readErr != nil {
			return cloudEntry{}, readErr
		}
		if upResp.StatusCode < 200 || upResp.StatusCode >= 300 {
			return cloudEntry{}, fmt.Errorf("上传分片到百度网盘失败（%s）：%s", upResp.Status, strings.TrimSpace(string(upRaw)))
		}
		var upErr struct {
			ErrorCode int    `json:"error_code"`
			ErrorMsg  string `json:"error_msg"`
		}
		if json.Unmarshal(upRaw, &upErr) == nil && upErr.ErrorCode != 0 {
			return cloudEntry{}, fmt.Errorf("上传分片失败（%d）：%s", upErr.ErrorCode, upErr.ErrorMsg)
		}
	}

	// Step 3: create
	createForm := url.Values{}
	createForm.Set("path", remotePath)
	createForm.Set("size", strconv.Itoa(len(data)))
	createForm.Set("isdir", "0")
	createForm.Set("rtype", rtype)
	createForm.Set("uploadid", preOut.UploadID)
	createForm.Set("block_list", blockListJSON)
	createQ := url.Values{}
	createQ.Set("method", "create")
	createQ.Set("access_token", token)
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baiduPanBase+"/file?"+createQ.Encode(), strings.NewReader(createForm.Encode()))
	if err != nil {
		return cloudEntry{}, err
	}
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.Header.Set("User-Agent", baiduUserAgent)
	createResp, err := p.client.Do(createReq)
	if err != nil {
		return cloudEntry{}, err
	}
	defer createResp.Body.Close()
	createRaw, err := readBodyLimited(createResp.Body, 1<<20)
	if err != nil {
		return cloudEntry{}, err
	}
	var createOut struct {
		Errno int    `json:"errno"`
		FsID  uint64 `json:"fs_id"`
		Path  string `json:"path"`
		Size  int64  `json:"size"`
	}
	if err := json.Unmarshal(createRaw, &createOut); err != nil {
		return cloudEntry{}, fmt.Errorf("百度网盘合并文件返回异常（%s）", createResp.Status)
	}
	if createOut.Errno != 0 {
		return cloudEntry{}, errors.New(baiduErrnoMessage(createOut.Errno))
	}
	finalName := cleanName
	if createOut.Path != "" {
		finalName = path.Base(createOut.Path)
	}
	finalRel := "/" + finalName
	if dirNorm != "" {
		finalRel = dirNorm + "/" + finalName
	}
	return cloudEntry{
		ID:    fmt.Sprintf("%d|%s", createOut.FsID, finalRel),
		Name:  finalName,
		Path:  finalRel,
		IsDir: false,
		Size:  int64(len(data)),
		Mime:  mimeType,
	}, nil
}
