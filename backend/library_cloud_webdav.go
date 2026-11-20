package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type webdavSecret struct {
	BaseURL  string `json:"baseUrl"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type webdavProvider struct {
	cfg           cloudAdminSettings
	row           cloudConnectionRow
	sec           webdavSecret
	base          *url.URL
	isAdmin       bool
	prefixTrusted bool
	client        *http.Client
}

func newWebDAVProvider(cfg cloudAdminSettings, row cloudConnectionRow, sec webdavSecret, isAdmin bool) (*webdavProvider, error) {
	u, trusted, err := validateWebDAVTargetURL(sec.BaseURL, cfg, isAdmin)
	if err != nil {
		return nil, err
	}
	allowPrivate := cfg.effectiveAllowPrivateWebDAV() || isAdmin || trusted
	client, err := newCloudHTTPClient(cfg, "webdav", row.UseProxy, allowPrivate, trusted, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	return &webdavProvider{
		cfg:           cfg,
		row:           row,
		sec:           sec,
		base:          u,
		isAdmin:       isAdmin,
		prefixTrusted: trusted,
		client:        client,
	}, nil
}

func (p *webdavProvider) buildURL(relPath string, includeRoot bool, isDir bool) (string, string, error) {
	baseSegs := splitCleanSegments(p.base.Path)
	var rootSegs []string
	if includeRoot {
		rootNorm, err := normalizeRelativeCloudDir(p.row.RootDir)
		if err != nil {
			return "", "", fmt.Errorf("默认目录配置无效：%w", err)
		}
		rootSegs = splitCleanSegments(rootNorm)
	}
	relNorm, err := normalizeRelativeCloudDir(relPath)
	if err != nil {
		return "", "", err
	}
	relSegs := splitCleanSegments(relNorm)

	all := make([]string, 0, len(baseSegs)+len(rootSegs)+len(relSegs))
	all = append(all, baseSegs...)
	all = append(all, rootSegs...)
	all = append(all, relSegs...)

	encoded := make([]string, len(all))
	for i, seg := range all {
		encoded[i] = url.PathEscape(seg)
	}
	rawPath := "/" + strings.Join(encoded, "/")
	if isDir && rawPath != "/" {
		rawPath += "/"
	}
	u := *p.base
	u.RawPath = ""
	u.Path = "/" + strings.Join(all, "/")
	if isDir && u.Path != "/" {
		u.Path += "/"
	}
	u.RawQuery = ""
	u.Fragment = ""
	// Construct exact URL string with RawPath preserved cleanly.
	u.RawPath = rawPath
	return u.String(), relNorm, nil
}

func splitCleanSegments(p string) []string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
}

func (p *webdavProvider) newRequest(ctx context.Context, method, targetURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, body)
	if err != nil {
		return nil, err
	}
	if p.sec.Username != "" || p.sec.Password != "" {
		req.SetBasicAuth(p.sec.Username, p.sec.Password)
	}
	return req, nil
}

type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href      string        `xml:"href"`
	Propstats []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}

type davProp struct {
	DisplayName      string          `xml:"displayname"`
	GetContentLength string          `xml:"getcontentlength"`
	GetLastModified  string          `xml:"getlastmodified"`
	GetContentType   string          `xml:"getcontenttype"`
	ResourceType     davResourceType `xml:"resourcetype"`
}

type davResourceType struct {
	Collection *struct{} `xml:"collection"`
}

func (p *webdavProvider) propfind(ctx context.Context, targetURL, depth string) ([]davResponse, int, error) {
	const propBody = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:displayname/>
    <D:getcontentlength/>
    <D:getlastmodified/>
    <D:getcontenttype/>
    <D:resourcetype/>
  </D:prop>
</D:propfind>`
	req, err := p.newRequest(ctx, "PROPFIND", targetURL, strings.NewReader(propBody))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, resp.StatusCode, errors.New("WebDAV 认证失败，请检查用户名、应用密码及目录权限")
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, resp.StatusCode, errors.New("WebDAV 路径不存在")
	}
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return nil, resp.StatusCode, fmt.Errorf("WebDAV 返回异常状态 %s：%s", resp.Status, strings.TrimSpace(string(msg)))
	}
	raw, err := readBodyLimited(resp.Body, 8<<20)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusOK && len(bytes.TrimSpace(raw)) == 0 {
		return nil, resp.StatusCode, nil
	}
	var ms davMultistatus
	if err := xml.Unmarshal(raw, &ms); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("无法解析 WebDAV 目录响应：%w", err)
	}
	return ms.Responses, resp.StatusCode, nil
}

func (p *webdavProvider) Test(ctx context.Context) (string, error) {
	baseURL, _, err := p.buildURL("", false, true)
	if err != nil {
		return "", err
	}
	if _, _, err := p.propfind(ctx, baseURL, "0"); err != nil {
		return "", err
	}
	if normRoot, _ := normalizeRelativeCloudDir(p.row.RootDir); normRoot != "" {
		rootURL, _, err := p.buildURL("", true, true)
		if err != nil {
			return "", err
		}
		if _, status, err := p.propfind(ctx, rootURL, "0"); err != nil && status != http.StatusNotFound {
			return "", err
		}
	}
	if p.sec.Username != "" {
		return p.sec.Username + " @ " + p.base.Host, nil
	}
	return p.base.Host, nil
}

func decodeHrefPath(rawHref string) string {
	rawHref = strings.TrimSpace(rawHref)
	if u, err := url.Parse(rawHref); err == nil && (u.Scheme != "" || u.Host != "") {
		rawHref = u.EscapedPath()
	}
	if idx := strings.IndexAny(rawHref, "?#"); idx >= 0 {
		rawHref = rawHref[:idx]
	}
	decoded, err := url.PathUnescape(rawHref)
	if err != nil {
		decoded = rawHref
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(decoded, "/"))
	return cleaned
}

func formatWebDAVTime(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC822, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return raw
}

func (p *webdavProvider) List(ctx context.Context, dir string) (cloudListResult, error) {
	targetURL, relNorm, err := p.buildURL(dir, true, true)
	if err != nil {
		return cloudListResult{}, err
	}
	responses, status, err := p.propfind(ctx, targetURL, "1")
	if err != nil {
		if status == http.StatusNotFound && relNorm == "" {
			return cloudListResult{
				Dir:       "",
				DirName:   "根目录",
				ParentDir: "",
				Crumbs:    buildRelativeCrumbs(p.rootLabel(), ""),
				Entries:   []cloudEntry{},
			}, nil
		}
		return cloudListResult{}, err
	}
	parsedTarget, _ := url.Parse(targetURL)
	targetClean := path.Clean("/" + strings.TrimPrefix(parsedTarget.Path, "/"))

	entries := make([]cloudEntry, 0, len(responses))
	for _, r := range responses {
		hrefClean := decodeHrefPath(r.Href)
		if hrefClean == targetClean || hrefClean == "/" {
			continue
		}
		// Skip entries outside target directory.
		if targetClean != "/" && !strings.HasPrefix(hrefClean, targetClean+"/") {
			continue
		}
		name := path.Base(hrefClean)
		if name == "" || name == "." || name == "/" {
			continue
		}
		var chosen *davProp
		for i := range r.Propstats {
			st := r.Propstats[i].Status
			if st == "" || strings.Contains(st, " 200 ") || strings.HasSuffix(st, " 200") {
				chosen = &r.Propstats[i].Prop
				break
			}
		}
		if chosen == nil && len(r.Propstats) > 0 {
			chosen = &r.Propstats[0].Prop
		}
		isDir := strings.HasSuffix(strings.TrimSpace(r.Href), "/")
		var size int64
		var mod, mime string
		if chosen != nil {
			if chosen.ResourceType.Collection != nil {
				isDir = true
			}
			if !isDir && chosen.GetContentLength != "" {
				size, _ = strconv.ParseInt(strings.TrimSpace(chosen.GetContentLength), 10, 64)
			}
			mod = formatWebDAVTime(chosen.GetLastModified)
			mime = strings.TrimSpace(chosen.GetContentType)
		}
		relEntry := "/" + name
		if relNorm != "" {
			relEntry = relNorm + "/" + name
		}
		entries = append(entries, cloudEntry{
			ID:       relEntry,
			Name:     name,
			Path:     relEntry,
			IsDir:    isDir,
			Size:     size,
			Modified: mod,
			Mime:     mime,
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

func (p *webdavProvider) rootLabel() string {
	if r, err := normalizeRelativeCloudDir(p.row.RootDir); err == nil && r != "" {
		return strings.TrimPrefix(r, "/")
	}
	return "根目录"
}

func (p *webdavProvider) mkcolOne(ctx context.Context, folderURL string) error {
	req, err := p.newRequest(ctx, "MKCOL", folderURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent, http.StatusMethodNotAllowed:
		return nil
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("创建目录失败（%s）：%s", resp.Status, strings.TrimSpace(string(msg)))
	}
}

func (p *webdavProvider) ensureDirs(ctx context.Context, relDir string) (string, error) {
	normDir, err := normalizeRelativeCloudDir(relDir)
	if err != nil {
		return "", err
	}
	dirURL, _, err := p.buildURL(normDir, true, true)
	if err != nil {
		return "", err
	}
	if _, status, err := p.propfind(ctx, dirURL, "0"); err == nil {
		return normDir, nil
	} else if status != http.StatusNotFound {
		return "", err
	}

	rootNorm, err := normalizeRelativeCloudDir(p.row.RootDir)
	if err != nil {
		return "", err
	}
	// Create missing rootDir segments first, then relative directory segments.
	var acc string
	for _, seg := range splitCleanSegments(rootNorm) {
		acc += "/" + seg
		u, _, err := p.buildURL(acc, false, true)
		if err != nil {
			return "", err
		}
		if _, status, _ := p.propfind(ctx, u, "0"); status == http.StatusNotFound {
			if err := p.mkcolOne(ctx, u); err != nil {
				return "", err
			}
		}
	}
	acc = ""
	for _, seg := range splitCleanSegments(normDir) {
		acc += "/" + seg
		u, _, err := p.buildURL(acc, true, true)
		if err != nil {
			return "", err
		}
		if _, status, _ := p.propfind(ctx, u, "0"); status == http.StatusNotFound {
			if err := p.mkcolOne(ctx, u); err != nil {
				return "", err
			}
		}
	}
	return normDir, nil
}

func (p *webdavProvider) Mkdir(ctx context.Context, parentDir, name string) (cloudEntry, error) {
	cleanName, err := cleanCloudFilename(name)
	if err != nil {
		return cloudEntry{}, err
	}
	parentNorm, err := p.ensureDirs(ctx, parentDir)
	if err != nil {
		return cloudEntry{}, err
	}
	relPath := "/" + cleanName
	if parentNorm != "" {
		relPath = parentNorm + "/" + cleanName
	}
	targetURL, _, err := p.buildURL(relPath, true, true)
	if err != nil {
		return cloudEntry{}, err
	}
	if err := p.mkcolOne(ctx, targetURL); err != nil {
		return cloudEntry{}, err
	}
	return cloudEntry{
		ID:    relPath,
		Name:  cleanName,
		Path:  relPath,
		IsDir: true,
	}, nil
}

func (p *webdavProvider) Download(ctx context.Context, fileID string) (cloudDownloadResult, error) {
	targetURL, relNorm, err := p.buildURL(fileID, true, false)
	if err != nil || relNorm == "" {
		return cloudDownloadResult{}, errors.New("请选择有效的云盘文件")
	}
	req, err := p.newRequest(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return cloudDownloadResult{}, fmt.Errorf("下载文件失败（%s）", resp.Status)
	}
	data, err := readBodyLimited(resp.Body, maxCloudFileBytes)
	if err != nil {
		return cloudDownloadResult{}, err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	return cloudDownloadResult{
		Filename: path.Base(relNorm),
		MimeType: mime,
		Data:     data,
	}, nil
}

func (p *webdavProvider) Upload(ctx context.Context, dir, filename string, data []byte, mimeType string, overwrite bool) (cloudEntry, error) {
	if int64(len(data)) > maxCloudFileBytes {
		return cloudEntry{}, fmt.Errorf("文件大小超过 %d MB 上限", maxCloudFileBytes>>20)
	}
	cleanName, err := cleanCloudFilename(filename)
	if err != nil {
		return cloudEntry{}, err
	}
	dirNorm, err := p.ensureDirs(ctx, dir)
	if err != nil {
		return cloudEntry{}, err
	}
	if !overwrite {
		if listing, err := p.List(ctx, dirNorm); err == nil {
			existing := make(map[string]bool, len(listing.Entries))
			for _, e := range listing.Entries {
				existing[strings.ToLower(e.Name)] = true
			}
			cleanName = dedupeCloudFilename(existing, cleanName)
		}
	}
	relPath := "/" + cleanName
	if dirNorm != "" {
		relPath = dirNorm + "/" + cleanName
	}
	targetURL, _, err := p.buildURL(relPath, true, false)
	if err != nil {
		return cloudEntry{}, err
	}
	req, err := p.newRequest(ctx, http.MethodPut, targetURL, bytes.NewReader(data))
	if err != nil {
		return cloudEntry{}, err
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", mimeType)
	resp, err := p.client.Do(req)
	if err != nil {
		return cloudEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return cloudEntry{}, fmt.Errorf("上传到 WebDAV 失败（%s）：%s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return cloudEntry{
		ID:    relPath,
		Name:  cleanName,
		Path:  relPath,
		IsDir: false,
		Size:  int64(len(data)),
		Mime:  mimeType,
	}, nil
}
