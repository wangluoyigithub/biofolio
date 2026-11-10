package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The administrator selects one destination. A browser cannot redirect a key
// or turn this route into a general-purpose proxy for the NAS network.
func serveLibraryAI(w http.ResponseWriter, r *http.Request, shared aiShared) {
	endpoint := os.Getenv("BIOLOGY_AI_ENDPOINT")
	if endpoint == "" || r.Header.Get("X-Biology-AI-Endpoint") != endpoint {
		http.Error(w, "AI 地址与服务端配置不符", http.StatusBadRequest)
		return
	}
	token := r.Header.Get("Authorization")
	if !strings.HasPrefix(token, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(token, "Bearer ")) == "" {
		// A teacher without a key of their own uses the administrator's.
		if shared.Key == "" {
			http.Error(w, "缺少 API 密钥：请管理员在「设置 → AI」保存共用密钥，或自己填写密钥", http.StatusBadRequest)
			return
		}
		token = "Bearer " + shared.Key
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
	client := http.Client{Timeout: aiTimeout(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		if e, ok := err.(interface{ Timeout() bool }); ok && e.Timeout() {
			http.Error(w, "AI 服务在 "+aiTimeout().String()+" 内没有回复，可在服务端用 BIOLOGY_AI_TIMEOUT 调长", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "无法连接 AI 服务", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Pass on the start of the provider's explanation (wrong key, model
		// name, quota…), as plain text.
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 600))
		text := strings.Map(func(r rune) rune {
			if r < ' ' && r != '\n' {
				return ' '
			}
			return r
		}, strings.ToValidUTF8(string(detail), ""))
		message := "AI 服务返回 " + response.Status
		if text = strings.TrimSpace(text); text != "" {
			message += "：" + text
		}
		http.Error(w, message, response.StatusCode)
		return
	}
	const limit = 16 << 20
	reply, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		http.Error(w, "读取 AI 回复失败", http.StatusBadGateway)
		return
	}
	if len(reply) > limit {
		http.Error(w, "AI 回复超过 16MB，已放弃以免保存不完整的结果", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(reply)
}

// aiTimeout is how long one AI request may take: BIOLOGY_AI_TIMEOUT seconds,
// 120 by default, between 10 and 600.
func aiTimeout() time.Duration {
	n, err := strconv.Atoi(os.Getenv("BIOLOGY_AI_TIMEOUT"))
	if err != nil || n < 10 || n > 600 {
		n = 120
	}
	return time.Duration(n) * time.Second
}
