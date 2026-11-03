package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed library_login.html
var loginHTML string
var loginPage = template.Must(template.New("login").Parse(loginHTML))

func libraryLogin(account *libraryAccount, next http.Handler) http.Handler {
	if account == nil {
		return next
	}
	const path = "/biology/"
	const name = "biology_session"
	var mu sync.Mutex
	sessions := make(map[string]time.Time)
	app := http.StripPrefix("/biology", next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == "GET" && r.URL.Path == "/" {
			http.Redirect(w, r, path, http.StatusFound)
			return
		}
		cookie, _ := r.Cookie(name)
		var token string
		if cookie != nil {
			token = cookie.Value
		}
		mu.Lock()
		valid := time.Now().Before(sessions[token])
		mu.Unlock()
		render := func(failed bool, fragment string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if failed {
				w.WriteHeader(http.StatusUnauthorized)
			}
			loginPage.Execute(w, struct {
				Failed   bool
				Fragment string
			}{failed, fragment})
		}
		if r.Method == "POST" && (r.URL.Path == path+"login" || r.URL.Path == path+"logout") {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "请求来源不匹配", 403)
					return
				}
			}
			c := http.Cookie{Name: name, Path: path, HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode}
			if r.URL.Path == path+"logout" {
				mu.Lock()
				delete(sessions, token)
				mu.Unlock()
				c.MaxAge = -1
				http.SetCookie(w, &c)
				http.Redirect(w, r, path, 303)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
			if r.ParseForm() != nil {
				http.Error(w, "登录请求无效", 400)
				return
			}
			fragment := r.PostForm.Get("fragment")
			mu.Lock()
			if r.PostForm.Get("username") != account.Username || !account.matches(r.PostForm.Get("password")) {
				mu.Unlock()
				render(true, fragment)
				return
			}
			now := time.Now()
			c.Value = rand.Text()
			delete(sessions, token)
			for k, expiry := range sessions {
				if !now.Before(expiry) {
					delete(sessions, k)
				}
			}
			sessions[c.Value] = now.Add(12 * time.Hour)
			mu.Unlock()
			http.SetCookie(w, &c)
			dest := url.URL{Path: path, Fragment: fragment}
			http.Redirect(w, r, dest.String(), 303)
			return
		}
		if !valid {
			if r.Method == "GET" && (r.URL.Path == path || r.URL.Path == path+"login") {
				render(false, "")
				return
			}
			http.Error(w, "请重新登录题库", 401)
			return
		}
		if r.URL.Path == path+"account" {
			mu.Lock()
			defer mu.Unlock()
			if !time.Now().Before(sessions[token]) {
				http.Error(w, "请重新登录题库", 401)
				return
			}
			switch r.Method {
			case http.MethodGet:
				jsonResponse(w, map[string]string{"username": account.Username})
			case http.MethodPost:
				if r.Header.Get("X-Biology-Library") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
					http.Error(w, "请求无效", 403)
					return
				}
				var input struct{ Username, Current, Password, Confirm string }
				if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input) != nil {
					http.Error(w, "账号信息格式无效", 400)
					return
				}
				if !account.matches(input.Current) {
					http.Error(w, "当前密码不正确", 403)
					return
				}
				if input.Password != input.Confirm {
					http.Error(w, "两次输入的新密码不一致", 400)
					return
				}
				if err := account.change(input.Username, input.Password); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				clear(sessions)
				http.SetCookie(w, &http.Cookie{Name: name, Path: path, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "不支持此操作", 405)
			}
			return
		}
		app.ServeHTTP(w, r)
	})
}
