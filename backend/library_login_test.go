package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestLibraryLogin(t *testing.T) {
	account, err := openLibraryAccount(t.TempDir(), "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	handler := libraryLogin(account, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/backup" {
			t.Fatal("bad route", r.URL.Path)
		}
		w.Write([]byte("protected"))
	}))
	send := func(method, path string, form url.Values, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, p := range []string{"/library/backup", "/biology/library/backup"} {
		w := send("GET", p, nil, nil)
		if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "" {
			t.Fatal("challenge or exposed data")
		}
	}
	if w := send("GET", "/biology/", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "登录题库") {
		t.Fatal("form missing")
	}
	f := url.Values{"username": {"teacher"}, "password": {"bad"}, "fragment": {"review/123"}}
	if w := send("POST", "/biology/login", f, nil); w.Code != 401 || !strings.Contains(w.Body.String(), "密码不正确") {
		t.Fatal("wrong password")
	}
	f.Set("password", "test-pass")
	w := send("POST", "/biology/login", f, nil)
	if w.Code != 303 || w.Header().Get("Location") != "/biology/#review/123" {
		t.Fatal(w.Header())
	}
	c := w.Result().Cookies()[0]
	if !c.HttpOnly || c.Path != "/biology/" || send("GET", "/biology/library/backup", nil, c).Body.String() != "protected" {
		t.Fatal("session")
	}
	send("POST", "/biology/logout", nil, c)
	if send("GET", "/biology/library/backup", nil, c).Code != 401 {
		t.Fatal("logout")
	}
}

func TestAccountRotation(t *testing.T) {
	dir := t.TempDir()
	a, err := openLibraryAccount(dir, "old-pass")
	if err != nil {
		t.Fatal(err)
	}
	h := libraryLogin(a, http.NotFoundHandler())
	call := func(path, body, ct string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/biology/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", ct)
		r.Header.Set("X-Biology-Library", "1")
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	login := func(user, pw string) *httptest.ResponseRecorder {
		return call("login", url.Values{"username": {user}, "password": {pw}}.Encode(), "application/x-www-form-urlencoded", nil)
	}
	c := login("teacher", "old-pass").Result().Cookies()[0]
	if call("account", `{"Username":"new","Current":"bad"}`, "application/json", c).Code != 403 {
		t.Fatal("current password not checked")
	}
	if call("account", `{"Username":"new","Current":"old-pass","Password":"12345678","Confirm":"123"}`, "application/json", c).Code != 400 {
		t.Fatal("confirmation not checked")
	}
	if call("account", `{"Username":"新用户","Current":"old-pass"}`, "application/json", c).Code != 204 {
		t.Fatal("rename failed")
	}
	if call("account", "{}", "application/json", c).Code != 401 || login("teacher", "old-pass").Code != 401 {
		t.Fatal("old login still valid")
	}
	c = login("新用户", "old-pass").Result().Cookies()[0]
	if call("account", `{"Username":"新用户","Current":"old-pass","Password":"new-password","Confirm":"new-password"}`, "application/json", c).Code != 204 {
		t.Fatal("password update failed")
	}
	if login("新用户", "old-pass").Code != 401 || login("新用户", "new-password").Code != 303 {
		t.Fatal("password not rotated")
	}
	a, err = openLibraryAccount(dir, "old-pass")
	if err != nil || a.Username != "新用户" || !a.matches("new-password") {
		t.Fatal("changes lost after restart")
	}
}

func TestLibraryLoginLockout(t *testing.T) {
	account, err := openLibraryAccount(t.TempDir(), "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	handler := libraryLogin(account, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	post := func(password string) *httptest.ResponseRecorder {
		f := url.Values{"username": {"teacher"}, "password": {password}}
		r := httptest.NewRequest("POST", "/biology/login", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < loginMaxFailures; i++ {
		if w := post("wrong"); w.Code != 401 {
			t.Fatalf("attempt %d: want 401, got %d", i, w.Code)
		}
	}
	if w := post("test-pass"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password during lockout: want 429, got %d", w.Code)
	}
}

func TestAdminResetAndDeleteMember(t *testing.T) {
	account, err := openLibraryAccount(t.TempDir(), "teacher-pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := account.update("", "colleague", "colleague-pass"); err != nil {
		t.Fatal(err)
	}
	handler := libraryLogin(account, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	send := func(method, path string, body string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Biology-Library", "1")
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	postLogin := func(user, pass string) *httptest.ResponseRecorder {
		f := url.Values{"username": {user}, "password": {pass}}
		r := httptest.NewRequest("POST", "/biology/login", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	login := func(user, pass string) *http.Cookie {
		w := postLogin(user, pass)
		if w.Code != 303 {
			t.Fatal("login failed for", user, w.Code, w.Body.String())
		}
		return w.Result().Cookies()[0]
	}
	admin := login("teacher", "teacher-pass")
	member := login("colleague", "colleague-pass")
	if send("GET", "/biology/account", "", member).Code != 200 {
		t.Fatal("member cannot read own account")
	}
	if w := send("POST", "/biology/member", `{"action":"reset","username":"colleague","password":"new-pass-123","confirm":"new-pass-123"}`, member); w.Code != 403 {
		t.Fatal("member must not manage accounts", w.Code)
	}
	if w := send("POST", "/biology/member", `{"action":"reset","username":"colleague","password":"new-pass-123","confirm":"different"}`, admin); w.Code != 400 {
		t.Fatal("mismatched confirm accepted", w.Code)
	}
	if w := send("POST", "/biology/member", `{"action":"reset","username":"colleague","password":"new-pass-123","confirm":"new-pass-123"}`, admin); w.Code != 204 {
		t.Fatal("reset failed", w.Code, w.Body.String())
	}
	if send("GET", "/biology/account", "", member).Code != 401 {
		t.Fatal("reset must end the member's session")
	}
	if w := postLogin("colleague", "colleague-pass"); w.Code != 401 {
		t.Fatal("old password still valid", w.Code)
	}
	login("colleague", "new-pass-123")
	if w := send("POST", "/biology/member", `{"action":"delete","username":"teacher"}`, admin); w.Code != 400 {
		t.Fatal("administrator deleted", w.Code)
	}
	if w := send("POST", "/biology/member", `{"action":"delete","username":"colleague"}`, admin); w.Code != 204 {
		t.Fatal("delete failed", w.Code)
	}
	if w := send("GET", "/biology/account", "", admin); !strings.Contains(w.Body.String(), `"members":[]`) {
		t.Fatal("deleted member still listed", w.Body.String())
	}
}

// Parallel wrong guesses must not get more password checks than the limit,
// and guesses from one address must not lock out the account elsewhere.
func TestLibraryLoginLockoutConcurrent(t *testing.T) {
	account, err := openLibraryAccount(t.TempDir(), "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	handler := libraryLogin(account, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	post := func(password, addr string) int {
		f := url.Values{"username": {"teacher"}, "password": {password}}
		r := httptest.NewRequest("POST", "/biology/login", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	const tries = 40
	codes := make(chan int, tries)
	var wg sync.WaitGroup
	for range tries {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- post("wrong", "10.0.0.66:5000") }()
	}
	wg.Wait()
	close(codes)
	checked := 0
	for c := range codes {
		switch c {
		case http.StatusUnauthorized:
			checked++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if checked > loginMaxFailures {
		t.Fatalf("%d of %d parallel guesses reached the password check; limit is %d", checked, tries, loginMaxFailures)
	}
	if c := post("test-pass", "10.0.0.66:5001"); c != http.StatusTooManyRequests {
		t.Fatalf("guessing address should stay locked, got %d", c)
	}
	if c := post("test-pass", "10.0.0.7:6000"); c != http.StatusSeeOther {
		t.Fatalf("administrator on another machine was locked out: %d", c)
	}
}
