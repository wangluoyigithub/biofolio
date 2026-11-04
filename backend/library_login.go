package main

import (
	"context"
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

type actorKey struct{}
type personalKey struct{}
type librarySession struct {
	user    string
	id      string
	expires time.Time
}

// Failed sign-ins are counted per username. Five failures within ten minutes
// lock that username for fifteen minutes. A successful sign-in clears the count.
const (
	loginMaxFailures = 5
	loginFailWindow  = 10 * time.Minute
	loginLockout     = 15 * time.Minute
	loginMaxTracked  = 1024
)

type loginRecord struct {
	failures int
	last     time.Time
	until    time.Time
}

// loginLimiter is guarded by the caller's mutex.
type loginLimiter map[string]*loginRecord

func (l loginLimiter) locked(key string, now time.Time) bool {
	rec := l[key]
	return rec != nil && now.Before(rec.until)
}
func (l loginLimiter) fail(key string, now time.Time) {
	if len(l) >= loginMaxTracked {
		for k, rec := range l {
			if now.After(rec.until) && now.Sub(rec.last) > loginFailWindow {
				delete(l, k)
			}
		}
	}
	rec := l[key]
	if rec == nil {
		rec = &loginRecord{}
		l[key] = rec
	}
	if now.Sub(rec.last) > loginFailWindow {
		rec.failures = 0
	}
	rec.failures++
	rec.last = now
	if rec.failures >= loginMaxFailures {
		rec.until = now.Add(loginLockout)
		rec.failures = 0
	}
}

func libraryLogin(account *libraryAccount, next http.Handler) http.Handler {
	if account == nil {
		return next
	}
	const path = "/biology/"
	const name = "biology_session"
	// mu guards sessions and limits. accountMu serialises reads and writes of
	// the account file. Slow password hashing runs under accountMu only, never
	// under mu, so one sign-in cannot stall every other request.
	var mu, accountMu sync.Mutex
	sessions := make(map[string]librarySession)
	limits := make(loginLimiter)
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
		session := sessions[token]
		valid := time.Now().Before(session.expires)
		mu.Unlock()
		render := func(status int, message, fragment string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if message != "" {
				w.WriteHeader(status)
			}
			loginPage.Execute(w, struct {
				Message  string
				Fragment string
			}{message, fragment})
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
			username := strings.TrimSpace(r.PostForm.Get("username"))
			key := strings.ToLower(username)
			now := time.Now()
			mu.Lock()
			locked := limits.locked(key, now)
			mu.Unlock()
			if locked {
				render(http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试。", fragment)
				return
			}
			accountMu.Lock()
			found := account.find(username)
			var candidate libraryAccount
			if found != nil {
				candidate = *found
			}
			accountMu.Unlock()
			password := r.PostForm.Get("password")
			if found == nil || !candidate.matches(password) {
				mu.Lock()
				limits.fail(key, now)
				mu.Unlock()
				render(http.StatusUnauthorized, "用户名或密码不正确，请重新输入。", fragment)
				return
			}
			c.Value = rand.Text()
			mu.Lock()
			delete(limits, key)
			delete(sessions, token)
			for k, expiry := range sessions {
				if !now.Before(expiry.expires) {
					delete(sessions, k)
				}
			}
			sessions[c.Value] = librarySession{candidate.Username, candidate.ID, now.Add(12 * time.Hour)}
			mu.Unlock()
			http.SetCookie(w, &c)
			dest := url.URL{Path: path, Fragment: fragment}
			http.Redirect(w, r, dest.String(), 303)
			return
		}
		if !valid {
			if r.Method == "GET" && (r.URL.Path == path || r.URL.Path == path+"login") {
				render(http.StatusOK, "", "")
				return
			}
			http.Error(w, "请重新登录题库", 401)
			return
		}
		if r.URL.Path == path+"account" || r.URL.Path == path+"members" {
			accountMu.Lock()
			defer accountMu.Unlock()
			member := account.find(session.user)
			if member == nil {
				http.Error(w, "登录已失效", 401)
				return
			}
			create := r.URL.Path == path+"members"
			if create && (member != account || r.Method != "POST") {
				http.Error(w, "仅管理员可添加账号", 403)
				return
			}
			switch r.Method {
			case http.MethodGet:
				jsonResponse(w, map[string]any{"username": member.Username, "admin": member == account})
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
				if !create && !member.matches(input.Current) {
					http.Error(w, "当前密码不正确", 403)
					return
				}
				if input.Password != input.Confirm {
					http.Error(w, "两次输入的新密码不一致", 400)
					return
				}
				current := session.user
				if create {
					current = ""
				}
				if err := account.update(current, input.Username, input.Password); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				if create {
					w.WriteHeader(204)
					return
				}
				mu.Lock()
				for key, s := range sessions {
					if s.user == session.user {
						delete(sessions, key)
					}
				}
				mu.Unlock()
				http.SetCookie(w, &http.Cookie{Name: name, Path: path, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "不支持此操作", 405)
			}
			return
		}
		if session.id != "" && r.URL.Path == path+"library/restore" {
			http.Error(w, "仅管理员可恢复题库", 403)
			return
		}
		ctx := context.WithValue(r.Context(), actorKey{}, session.user)
		app.ServeHTTP(w, r.WithContext(context.WithValue(ctx, personalKey{}, session.id)))
	})
}
