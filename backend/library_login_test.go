package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLibraryLogin(t *testing.T) {
	handler := libraryLogin("test-pass", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
