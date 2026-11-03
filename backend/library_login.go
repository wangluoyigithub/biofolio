package main

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
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

func libraryLogin(password string, next http.Handler) http.Handler {
	if password == "" {
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
			if r.PostForm.Get("username") != "teacher" || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("password")), []byte(password)) != 1 {
				render(true, fragment)
				return
			}
			now := time.Now()
			c.Value = rand.Text()
			mu.Lock()
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
		app.ServeHTTP(w, r)
	})
}
