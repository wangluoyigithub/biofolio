package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode"
)

const (
	maxCloudFileBytes int64 = 128 << 20
	cloudRelDirPrefix       = "rel:"
)

var errCloudNotFound = fmt.Errorf("%w: 云盘文件或目录不存在", os.ErrNotExist)

type cloudCrumb struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cloudEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
	Mime     string `json:"mime,omitempty"`
	ExportAs string `json:"exportAs,omitempty"`
}

type cloudListResult struct {
	Dir       string       `json:"dir"`
	DirName   string       `json:"dirName,omitempty"`
	ParentDir string       `json:"parentDir"`
	Crumbs    []cloudCrumb `json:"crumbs,omitempty"`
	Entries   []cloudEntry `json:"entries"`
}

type cloudDownloadResult struct {
	Filename string
	MimeType string
	Data     []byte
}

type cloudProvider interface {
	Test(ctx context.Context) (label string, err error)
	List(ctx context.Context, dir string) (cloudListResult, error)
	Mkdir(ctx context.Context, parentDir, name string) (cloudEntry, error)
	Download(ctx context.Context, fileID string) (cloudDownloadResult, error)
	Upload(ctx context.Context, dir, filename string, data []byte, mimeType string, overwrite bool) (cloudEntry, error)
}

func sortCloudEntries(entries []cloudEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		if !a.IsDir && a.Modified != b.Modified {
			return a.Modified > b.Modified
		}
		la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if la != lb {
			return la < lb
		}
		return a.Name < b.Name
	})
}

func cleanCloudFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", errors.New("文件名或文件夹名不能为空")
	}
	if strings.ContainsAny(name, "/\\\x00") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", errors.New("名称不能包含斜杠或控制字符")
	}
	return name, nil
}

func isRelCloudPath(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), cloudRelDirPrefix)
}

func cleanCloudRelSegments(relDir string) ([]string, error) {
	if strings.ContainsRune(relDir, 0) || strings.IndexFunc(relDir, unicode.IsControl) >= 0 {
		return nil, errors.New("目录路径不能包含控制字符")
	}
	s := strings.TrimSpace(relDir)
	s = strings.TrimPrefix(s, cloudRelDirPrefix)
	s = strings.TrimSpace(s)
	if strings.ContainsRune(s, '\\') {
		return nil, errors.New("目录路径不能包含反斜杠")
	}
	s = strings.Trim(s, "/")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, "/")
	clean := make([]string, 0, len(parts))
	for _, seg := range parts {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		validSeg, err := cleanCloudFilename(seg)
		if err != nil {
			return nil, fmt.Errorf("目录路径无效：%w", err)
		}
		clean = append(clean, validSeg)
	}
	return clean, nil
}

func cleanCloudRelDir(relDir string) (string, error) {
	segs, err := cleanCloudRelSegments(relDir)
	if err != nil {
		return "", err
	}
	return strings.Join(segs, "/"), nil
}

func parseCloudRelPath(raw string) (string, bool, error) {
	if !isRelCloudPath(raw) {
		return "", false, nil
	}
	clean, err := cleanCloudRelDir(raw)
	if err != nil {
		return "", true, err
	}
	return clean, true, nil
}

func parseCloudRelFilePath(raw string) (string, string, bool, error) {
	if !isRelCloudPath(raw) {
		return "", "", false, nil
	}
	segs, err := cleanCloudRelSegments(raw)
	if err != nil {
		return "", "", true, err
	}
	if len(segs) == 0 {
		return "", "", true, errors.New("请选择有效的云盘文件")
	}
	return strings.Join(segs[:len(segs)-1], "/"), segs[len(segs)-1], true, nil
}

func cloudRelJoin(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(p, cloudRelDirPrefix)
		p = strings.Trim(p, "/")
		if p != "" {
			out = append(out, p)
		}
	}
	return cloudRelDirPrefix + strings.Join(out, "/")
}

func normalizeRelativeCloudDir(raw string) (string, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" || raw == "/" {
		return "", nil
	}
	parts := strings.Split(raw, "/")
	clean := make([]string, 0, len(parts))
	for _, seg := range parts {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." || strings.IndexFunc(seg, unicode.IsControl) >= 0 {
			return "", errors.New("目录路径无效")
		}
		clean = append(clean, seg)
	}
	if len(clean) == 0 {
		return "", nil
	}
	return "/" + strings.Join(clean, "/"), nil
}

func parentRelativeCloudDir(rel string) string {
	if rel == "" || rel == "/" {
		return ""
	}
	p := path.Dir(strings.TrimSuffix(rel, "/"))
	if p == "." || p == "/" {
		return ""
	}
	return p
}

func buildRelativeCrumbs(rootLabel, relDir string) []cloudCrumb {
	if rootLabel == "" {
		rootLabel = "根目录"
	}
	crumbs := []cloudCrumb{{ID: "", Name: rootLabel}}
	if relDir == "" {
		return crumbs
	}
	acc := ""
	for _, seg := range strings.Split(strings.Trim(relDir, "/"), "/") {
		if seg == "" {
			continue
		}
		acc += "/" + seg
		crumbs = append(crumbs, cloudCrumb{ID: acc, Name: seg})
	}
	return crumbs
}

func dedupeCloudFilename(existing map[string]bool, filename string) string {
	if !existing[strings.ToLower(filename)] {
		return filename
	}
	ext := path.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for i := 1; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !existing[strings.ToLower(cand)] {
			return cand
		}
	}
	return fmt.Sprintf("%s_%s%s", stem, newCloudID(), ext)
}

func readBodyLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("文件大小超过 %d MB 上限", limit>>20)
	}
	return data, nil
}

func (l *libraryPage) openCloudProviderFromRow(row cloudConnectionRow, plain []byte, isAdmin bool) (cloudProvider, error) {
	cfg := l.loadCloudAdminSettings()
	switch row.Kind {
	case "webdav":
		var sec webdavSecret
		if json.Unmarshal(plain, &sec) != nil {
			return nil, errors.New("WebDAV 凭据损坏，请重新保存")
		}
		return newWebDAVProvider(cfg, row, sec, isAdmin)
	case "gdrive":
		var sec gdriveSecret
		if json.Unmarshal(plain, &sec) != nil {
			return nil, errors.New("Google Drive 凭据损坏，请重新授权")
		}
		return newGDriveProvider(l, cfg, row, sec, isAdmin)
	case "baidu":
		var sec baiduSecret
		if json.Unmarshal(plain, &sec) != nil {
			return nil, errors.New("百度网盘凭据损坏，请重新扫码绑定")
		}
		return newBaiduProvider(l, cfg, row, sec, isAdmin)
	default:
		return nil, errors.New("不支持的云盘类型")
	}
}

func (l *libraryPage) openCloudProvider(account, connID string, isAdmin bool) (cloudConnectionRow, cloudProvider, error) {
	row, plain, err := l.getCloudConnection(account, connID)
	if err != nil {
		return row, nil, err
	}
	p, err := l.openCloudProviderFromRow(row, plain, isAdmin)
	return row, p, err
}
