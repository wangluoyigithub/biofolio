package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"html/template"
	"net"
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

// Failed sign-ins are counted per username and client address, so someone
// guessing from one machine cannot lock the account out for everyone else.
// Five failures within ten minutes lock that pair for fifteen minutes. A
// successful sign-in clears the count. Each attempt reserves its place before
// the slow password check, so parallel guesses cannot exceed the limit.
const (
	loginMaxFailures = 5
	loginFailWindow  = 10 * time.Minute
	loginLockout     = 15 * time.Minute
	loginMaxTracked  = 1024
	// At most this many password checks run at once; the rest wait their turn.
	loginMaxHashing = 2
)

type loginRecord struct {
	failures int
	inflight int
	last     time.Time
	until    time.Time
}

// loginLimiter is guarded by the caller's mutex.
type loginLimiter map[string]*loginRecord

// reserve claims one attempt for key, or reports that the key is locked or
// already has as many attempts under way as it has left.
func (l loginLimiter) reserve(key string, now time.Time) bool {
	if len(l) >= loginMaxTracked {
		for k, rec := range l {
			if rec.inflight == 0 && now.After(rec.until) && now.Sub(rec.last) > loginFailWindow {
				delete(l, k)
			}
		}
	}
	rec := l[key]
	if rec == nil {
		rec = &loginRecord{}
		l[key] = rec
	}
	if now.Before(rec.until) {
		return false
	}
	if now.Sub(rec.last) > loginFailWindow {
		rec.failures = 0
	}
	if rec.failures+rec.inflight >= loginMaxFailures {
		return false
	}
	rec.inflight++
	return true
}

// finish settles an attempt claimed by reserve.
func (l loginLimiter) finish(key string, now time.Time, ok bool) {
	rec := l[key]
	if rec == nil {
		return
	}
	rec.inflight--
	if ok {
		rec.failures = 0
		rec.until = time.Time{}
		if rec.inflight == 0 {
			delete(l, key)
		}
		return
	}
	rec.failures++
	rec.last = now
	if rec.failures >= loginMaxFailures {
		rec.until = now.Add(loginLockout)
		rec.failures = 0
	}
}

// loginKey identifies who is trying: the username and the connecting address.
// Forwarded headers are ignored because any client can set them.
func loginKey(username string, r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return strings.ToLower(username) + "\x00" + host
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
	hashing := make(chan struct{}, loginMaxHashing)
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
			key := loginKey(username, r)
			now := time.Now()
			mu.Lock()
			allowed := limits.reserve(key, now)
			mu.Unlock()
			if !allowed {
				render(http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试。", fragment)
				return
			}
			select {
			case hashing <- struct{}{}:
			case <-r.Context().Done():
				mu.Lock()
				limits.finish(key, time.Now(), false)
				mu.Unlock()
				return
			}
			defer func() { <-hashing }()
			accountMu.Lock()
			found := account.find(username)
			var candidate libraryAccount
			if found != nil {
				candidate = *found
			}
			accountMu.Unlock()
			password := r.PostForm.Get("password")
			if found == nil {
				// Hash anyway so an unknown username takes as long as a wrong password.
				candidate = libraryAccount{Salt: make([]byte, 16), Hash: make([]byte, 32)}
			}
			if !candidate.matches(password) || found == nil {
				mu.Lock()
				limits.finish(key, time.Now(), false)
				mu.Unlock()
				render(http.StatusUnauthorized, "用户名或密码不正确，请重新输入。", fragment)
				return
			}
			c.Value = rand.Text()
			mu.Lock()
			limits.finish(key, now, true)
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
		if r.URL.Path == path+"member" {
			accountMu.Lock()
			defer accountMu.Unlock()
			if account.find(session.user) == nil {
				http.Error(w, "登录已失效", 401)
				return
			}
			if session.user != account.Username || r.Method != http.MethodPost {
				http.Error(w, "仅管理员可管理账号", 403)
				return
			}
			if r.Header.Get("X-Biology-Library") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "请求无效", 403)
				return
			}
			var input struct{ Username, Action, Password, Confirm string }
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input) != nil {
				http.Error(w, "账号信息格式无效", 400)
				return
			}
			var err error
			switch input.Action {
			case "reset":
				if input.Password != input.Confirm {
					http.Error(w, "两次输入的新密码不一致", 400)
					return
				}
				err = account.resetMember(input.Username, input.Password)
			case "delete":
				err = account.deleteMember(input.Username)
			default:
				http.Error(w, "不支持此操作", 400)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			// A changed or removed teacher must sign in again everywhere.
			mu.Lock()
			for key, s := range sessions {
				if s.user == input.Username {
					delete(sessions, key)
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
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
				info := map[string]any{"username": member.Username, "admin": member == account, "id": ownerKey(member.ID)}
				if member == account {
					names := []string{}
					for _, m := range account.Members {
						names = append(names, m.Username)
					}
					info["members"] = names
				}
				jsonResponse(w, info)
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
