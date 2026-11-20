package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const cloudAutoBackupFilename = "cloud-autobackup.json"

type cloudAutoBackupState struct {
	LastAttemptDate        string `json:"lastAttemptDate,omitempty"`
	LastRunAt              string `json:"lastRunAt,omitempty"`
	LastSuccessAt          string `json:"lastSuccessAt,omitempty"`
	LastSuccessFingerprint string `json:"lastSuccessFingerprint,omitempty"`
	LastStatus             string `json:"lastStatus,omitempty"` // "ok", "skipped", "error"
	LastMessage            string `json:"lastMessage,omitempty"`
	LastJSONFile           string `json:"lastJsonFile,omitempty"`
	LastOfflineFile        string `json:"lastOfflineFile,omitempty"`
}

func autoBackupLocation() *time.Location {
	if os.Getenv("TZ") != "" {
		return time.Local
	}
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

func (l *libraryPage) autoBackupStatePath() string {
	return filepath.Join(l.dir, cloudAutoBackupFilename)
}

func (l *libraryPage) loadAutoBackupState() cloudAutoBackupState {
	l.autoBackupStateMu.Lock()
	defer l.autoBackupStateMu.Unlock()
	if l.dir == "" {
		return l.autoBackupMemState
	}
	var st cloudAutoBackupState
	if data, err := os.ReadFile(l.autoBackupStatePath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func (l *libraryPage) saveAutoBackupState(st cloudAutoBackupState) {
	l.autoBackupStateMu.Lock()
	defer l.autoBackupStateMu.Unlock()
	if l.dir == "" {
		l.autoBackupMemState = st
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(l.dir, ".cloud-autobackup-*")
	if err != nil {
		return
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)
	_ = f.Chmod(0600)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil && closeErr == nil {
		_ = os.Rename(tmpName, l.autoBackupStatePath())
	}
}

func (l *libraryPage) libraryFingerprint() (string, error) {
	var stateVer int
	if err := l.db.QueryRow("select version from library_state where id=1").Scan(&stateVer); err != nil {
		return "", err
	}
	var recCount, recLen, favCount, favLen, perCount, perLen, draftCount, draftVer int64
	_ = l.db.QueryRow("select count(*), coalesce(sum(length(body)),0) from library_records").Scan(&recCount, &recLen)
	_ = l.db.QueryRow("select count(*), coalesce(sum(length(marks)),0) from library_favorites").Scan(&favCount, &favLen)
	_ = l.db.QueryRow("select count(*), coalesce(sum(length(body)),0) from library_personal").Scan(&perCount, &perLen)
	_ = l.db.QueryRow("select count(*), coalesce(sum(version),0) from library_review_drafts").Scan(&draftCount, &draftVer)
	return fmt.Sprintf("v%d:r%d-%d:f%d-%d:p%d-%d:d%d-%d", stateVer, recCount, recLen, favCount, favLen, perCount, perLen, draftCount, draftVer), nil
}

func (l *libraryPage) runCloudAutoBackup(ctx context.Context, force bool) (cloudAutoBackupState, error) {
	l.autoBackupRunMu.Lock()
	defer l.autoBackupRunMu.Unlock()

	cfg := l.loadCloudAdminSettings()
	st := l.loadAutoBackupState()
	now := time.Now().In(autoBackupLocation())
	today := now.Format("2006-01-02")

	if cfg.AutoBackupConnectionID == "" {
		err := errors.New("未选择自动备份目标云盘")
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "error"
		st.LastMessage = err.Error()
		l.saveAutoBackupState(st)
		return st, err
	}

	fp, err := l.libraryFingerprint()
	if err != nil {
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "error"
		st.LastMessage = "读取题库指纹失败：" + err.Error()
		l.saveAutoBackupState(st)
		return st, err
	}

	if !force && fp == st.LastSuccessFingerprint {
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "skipped"
		st.LastMessage = "题库自上次备份后无新改动，本次已跳过上传"
		l.saveAutoBackupState(st)
		return st, nil
	}

	_, provider, err := l.openCloudProvider(adminOwner, cfg.AutoBackupConnectionID, true)
	if err != nil {
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "error"
		st.LastMessage = err.Error()
		l.saveAutoBackupState(st)
		return st, err
	}

	backupData, err := l.exportBackupData()
	if err != nil {
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "error"
		st.LastMessage = err.Error()
		l.saveAutoBackupState(st)
		return st, err
	}
	jsonBytes, err := json.Marshal(backupData)
	if err != nil {
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "error"
		st.LastMessage = "编码备份 JSON 失败"
		l.saveAutoBackupState(st)
		return st, err
	}
	offlineBytes, err := l.renderOfflineBytes()
	if err != nil {
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "error"
		st.LastMessage = "生成离线题库失败"
		l.saveAutoBackupState(st)
		return st, err
	}

	jsonName := fmt.Sprintf("biofolio-backup-%s.json", today)
	offlineName := fmt.Sprintf("生物离线题库-%s.html", today)

	jsonEntry, err := provider.Upload(ctx, cfg.AutoBackupDir, jsonName, jsonBytes, "application/json; charset=utf-8", true)
	if err != nil {
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "error"
		st.LastMessage = "上传整库 JSON 失败：" + err.Error()
		l.saveAutoBackupState(st)
		return st, err
	}
	offlineEntry, err := provider.Upload(ctx, cfg.AutoBackupDir, offlineName, offlineBytes, "text/html; charset=utf-8", true)
	if err != nil {
		st.LastAttemptDate = today
		st.LastRunAt = now.Format(time.RFC3339)
		st.LastStatus = "error"
		st.LastMessage = "上传离线题库 HTML 失败：" + err.Error()
		l.saveAutoBackupState(st)
		return st, err
	}

	st.LastAttemptDate = today
	st.LastRunAt = now.Format(time.RFC3339)
	st.LastSuccessAt = st.LastRunAt
	st.LastSuccessFingerprint = fp
	st.LastStatus = "ok"
	st.LastJSONFile = jsonEntry.Name
	st.LastOfflineFile = offlineEntry.Name
	st.LastMessage = fmt.Sprintf("已自动备份 %s 与 %s", jsonEntry.Name, offlineEntry.Name)
	l.saveAutoBackupState(st)
	return st, nil
}

func (l *libraryPage) runCloudAutoBackupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cfg := l.loadCloudAdminSettings()
		if !cfg.AutoBackupEnabled || cfg.AutoBackupConnectionID == "" {
			continue
		}
		now := time.Now().In(autoBackupLocation())
		if now.Hour() != cfg.effectiveAutoBackupHour() {
			continue
		}
		st := l.loadAutoBackupState()
		if st.LastAttemptDate == now.Format("2006-01-02") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		nextState, err := l.runCloudAutoBackup(ctx, false)
		cancel()
		if err != nil {
			log.Printf("云盘自动备份失败: %v", err)
		} else if nextState.LastStatus == "ok" {
			log.Printf("云盘自动备份完成: %s", nextState.LastMessage)
		}
	}
}

func (l *libraryPage) serveCloudAutoBackupSettings(w http.ResponseWriter, body []byte, isAdmin bool) {
	if !isAdmin {
		http.Error(w, "只有管理员可以配置自动云盘备份", http.StatusForbidden)
		return
	}
	var in struct {
		Enabled      bool   `json:"enabled"`
		ConnectionID string `json:"connectionId"`
		Dir          string `json:"dir"`
		Hour         int    `json:"hour"`
	}
	if json.Unmarshal(body, &in) != nil {
		http.Error(w, "自动备份参数格式无效", http.StatusBadRequest)
		return
	}
	if in.Hour < 0 || in.Hour > 23 {
		http.Error(w, "备份时间需在 0 至 23 点之间", http.StatusBadRequest)
		return
	}
	normDir, err := normalizeRelativeCloudDir(in.Dir)
	if err != nil {
		http.Error(w, "备份目录无效："+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Enabled {
		if in.ConnectionID == "" {
			http.Error(w, "请先选择用于自动备份的目标云盘", http.StatusBadRequest)
			return
		}
		if _, _, err := l.getCloudConnection(adminOwner, in.ConnectionID); err != nil {
			http.Error(w, "目标云盘不存在或无权访问", http.StatusBadRequest)
			return
		}
	}
	next := l.loadCloudAdminSettings()
	h := in.Hour
	next.AutoBackupEnabled = in.Enabled
	next.AutoBackupConnectionID = in.ConnectionID
	next.AutoBackupDir = normDir
	next.AutoBackupHour = &h
	if err := l.saveCloudAdminSettings(next); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{
		"adminConfig":     l.cloudAdminView(),
		"autoBackupState": l.loadAutoBackupState(),
	})
}

func (l *libraryPage) serveCloudAutoBackupRun(w http.ResponseWriter, r *http.Request, isAdmin bool, body []byte) {
	if !isAdmin {
		http.Error(w, "只有管理员可执行云盘自动备份", http.StatusForbidden)
		return
	}
	var in struct {
		Force bool `json:"force"`
	}
	_ = json.Unmarshal(body, &in)
	st, err := l.runCloudAutoBackup(r.Context(), in.Force)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]any{
		"autoBackupState": st,
	})
}
