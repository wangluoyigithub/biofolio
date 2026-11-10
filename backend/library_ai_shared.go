package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// The administrator can save one API key and model for the NAS AI gateway.
// The key stays in the data directory; the AI route adds it to requests from
// signed-in teachers who did not enter a key of their own. It is never sent
// to a browser.
type aiShared struct {
	Model string `json:"model"`
	Key   string `json:"key"`
}

var aiSharedMu sync.Mutex

func (l *libraryPage) aiSharedPath() string { return filepath.Join(l.dir, "ai-settings.json") }

func (l *libraryPage) loadAIShared() aiShared {
	aiSharedMu.Lock()
	defer aiSharedMu.Unlock()
	var s aiShared
	if data, err := os.ReadFile(l.aiSharedPath()); err == nil {
		json.Unmarshal(data, &s)
	}
	return s
}

func (l *libraryPage) saveAIShared(s aiShared) error {
	aiSharedMu.Lock()
	defer aiSharedMu.Unlock()
	data, _ := json.Marshal(s)
	f, err := os.CreateTemp(l.dir, ".ai-settings-*")
	if err != nil {
		return errors.New("AI 设置保存失败")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err != nil || closeErr != nil || os.Rename(f.Name(), l.aiSharedPath()) != nil {
		return errors.New("AI 设置保存失败")
	}
	return nil
}

// serveAISettings lets the administrator set the shared model and key.
// Body: {"model": "...", "key": "..."}; an empty key keeps the saved one,
// "clear": true removes both.
func (l *libraryPage) serveAISettings(w http.ResponseWriter, body []byte, admin bool) {
	if !admin {
		http.Error(w, "只有管理员可以设置共用 AI", 403)
		return
	}
	if os.Getenv("BIOLOGY_AI_ENDPOINT") == "" {
		http.Error(w, "服务端没有配置 BIOLOGY_AI_ENDPOINT，无法设置共用 AI", 400)
		return
	}
	var in struct {
		Model string `json:"model"`
		Key   string `json:"key"`
		Clear bool   `json:"clear"`
	}
	if json.Unmarshal(body, &in) != nil {
		http.Error(w, "格式无效", 400)
		return
	}
	next := aiShared{}
	if !in.Clear {
		next = l.loadAIShared()
		next.Model = strings.TrimSpace(in.Model)
		if key := strings.TrimSpace(in.Key); key != "" {
			next.Key = key
		}
		if next.Model == "" || next.Key == "" || utf8.RuneCountInString(next.Model) > 200 || len(next.Key) > 4096 || strings.ContainsAny(next.Key, "\r\n") {
			http.Error(w, "请填写模型名称和 API 密钥", 400)
			return
		}
	}
	if err := l.saveAIShared(next); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonResponse(w, map[string]any{"model": next.Model, "shared": next.Key != ""})
}
