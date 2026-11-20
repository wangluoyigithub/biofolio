package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode"
)

const maxCloudFileBytes int64 = 128 << 20

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

func (l *libraryPage) openCloudProvider(account, connID string, isAdmin bool) (cloudConnectionRow, cloudProvider, error) {
	row, plain, err := l.getCloudConnection(account, connID)
	if err != nil {
		return row, nil, err
	}
	cfg := l.loadCloudAdminSettings()
	switch row.Kind {
	case "webdav":
		var sec webdavSecret
		if json.Unmarshal(plain, &sec) != nil {
			return row, nil, errors.New("WebDAV 凭据损坏，请重新保存")
		}
		p, err := newWebDAVProvider(cfg, row, sec, isAdmin)
		return row, p, err
	case "gdrive":
		var sec gdriveSecret
		if json.Unmarshal(plain, &sec) != nil {
			return row, nil, errors.New("Google Drive 凭据损坏，请重新授权")
		}
		p, err := newGDriveProvider(l, cfg, row, sec, isAdmin)
		return row, p, err
	case "baidu":
		var sec baiduSecret
		if json.Unmarshal(plain, &sec) != nil {
			return row, nil, errors.New("百度网盘凭据损坏，请重新扫码绑定")
		}
		p, err := newBaiduProvider(l, cfg, row, sec, isAdmin)
		return row, p, err
	default:
		return row, nil, errors.New("不支持的云盘类型")
	}
}
