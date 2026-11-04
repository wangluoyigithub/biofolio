package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSplitHomeFromPrivateAndOptionalContent(t *testing.T) {
	raw := []byte(`<script data-lib="vue">VUE</script><script id="pdf-libraries" type="text/plain">cGRm</script><script id="paper-data" type="application/json">{"originalDocx":"ZG9j","questions":[{"id":"q","answer":"private answer","contentHtml":"private figure"}]}</script>`)
	original := bytes.Clone(raw)
	page, err := splitOnlinePage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("offline template modified")
	}
	for _, heavy := range []string{"private answer", "private figure", "cGRm", "ZG9j", "VUE"} {
		if strings.Contains(string(page.html), heavy) {
			t.Fatalf("eager content: %s", heavy)
		}
	}
	for key, asset := range page.files {
		switch key {
		case "/library/assets/seed-questions":
			if asset.immutable || !bytes.Contains(asset.content, []byte("private figure")) {
				t.Fatal("invalid seed storage")
			}
		case "/library/assets/original.docx":
			if asset.immutable || string(asset.content) != "doc" {
				t.Fatal("invalid original storage")
			}
		default:
			if !asset.immutable {
				t.Fatal("tool not cacheable")
			}
		}
	}
	if len(page.files) != 4 {
		t.Fatal("missing assets")
	}
	summary, err := recordSummary([]byte(`{"id":"r","paper":true,"questions":[{"id":"q","contentHtml":"face","answerHtml":"answer"}]}`))
	if err != nil || !json.Valid(summary) || bytes.Contains(summary, []byte("face")) || !bytes.Contains(summary, []byte(`"answerHtml":"1"`)) {
		t.Fatal("invalid record summary", err)
	}
}
