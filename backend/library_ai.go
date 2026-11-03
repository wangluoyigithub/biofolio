package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The administrator selects one destination. A browser cannot redirect a key
// or turn this route into a general-purpose proxy for the NAS network.
func serveLibraryAI(w http.ResponseWriter, r *http.Request) {
	endpoint := os.Getenv("BIOLOGY_AI_ENDPOINT")
	if endpoint == "" || r.Header.Get("X-Biology-AI-Endpoint") != endpoint {
		http.Error(w, "AI 地址与服务端配置不符", http.StatusBadRequest)
		return
	}
	token := r.Header.Get("Authorization")
	if !strings.HasPrefix(token, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(token, "Bearer ")) == "" {
		http.Error(w, "缺少 API 密钥", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		http.Error(w, "请求过大或读取失败", http.StatusRequestEntityTooLarge)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "AI 服务地址无效", http.StatusBadGateway)
		return
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		http.Error(w, "无法连接 AI 服务", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		http.Error(w, http.StatusText(response.StatusCode), response.StatusCode)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, io.LimitReader(response.Body, 16<<20))
}
