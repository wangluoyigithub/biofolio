package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
