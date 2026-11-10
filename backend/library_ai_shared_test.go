package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestSharedAIKey(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()
	t.Setenv("BIOLOGY_AI_ENDPOINT", upstream.URL)
	l, _ := rightsLibrary(t)
	l.dir = t.TempDir()
	send := func(id, path, body string, header map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/library/"+path, bytes.NewBufferString(body))
		r.Header.Set("X-Biology-Library", "1")
		r.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			r.Header.Set(k, v)
		}
		r = r.WithContext(context.WithValue(r.Context(), personalKey{}, id))
		w := httptest.NewRecorder()
		l.serve(w, r)
		return w
	}
	ai := map[string]string{"X-Biology-AI-Endpoint": upstream.URL}
	if w := send("m1", "ai", `{}`, ai); w.Code != 400 {
		t.Fatal("request without any key accepted", w.Code)
	}
	if w := send("m1", "ai-settings", `{"model":"m","key":"secret-1"}`, nil); w.Code != 403 {
		t.Fatal("teacher saved the shared key", w.Code)
	}
	if w := send("", "ai-settings", `{"model":"m","key":""}`, nil); w.Code != 400 {
		t.Fatal("saved without a key", w.Code)
	}
	w := send("", "ai-settings", `{"model":"qwen","key":"secret-1"}`, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("admin save failed or echoed the key", w.Code, w.Body.String())
	}
	if info, _ := os.Stat(l.aiSharedPath()); info == nil {
		t.Fatal("settings file missing")
	}
	if w := send("m1", "ai", `{}`, ai); w.Code != 200 || seen != "Bearer secret-1" {
		t.Fatal("shared key not used", w.Code, seen)
	}
	ai["Authorization"] = "Bearer own-key"
	if w := send("m1", "ai", `{}`, ai); w.Code != 200 || seen != "Bearer own-key" {
		t.Fatal("teacher's own key not preferred", seen)
	}
	// Changing only the model keeps the saved key.
	if w := send("", "ai-settings", `{"model":"other"}`, nil); w.Code != 200 || l.loadAIShared().Key != "secret-1" || l.loadAIShared().Model != "other" {
		t.Fatal("model change lost the key")
	}
	if w := send("", "ai-settings", `{"clear":true}`, nil); w.Code != 200 || l.loadAIShared().Key != "" {
		t.Fatal("clear failed")
	}
	// The page tells browsers the model and that a key exists, never the key.
	send("", "ai-settings", `{"model":"qwen","key":"secret-2"}`, nil)
	d := libraryData{AIEndpoint: upstream.URL}
	shared := l.loadAIShared()
	d.AIModel, d.AIShared = shared.Model, shared.Key != ""
	if d.AIModel != "qwen" || !d.AIShared {
		t.Fatal("page flags wrong")
	}
}
