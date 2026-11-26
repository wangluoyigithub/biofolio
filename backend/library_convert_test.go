package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeDocxToPdfValidationAndMockConvert(t *testing.T) {
	workDir := t.TempDir()

	// 1. Reject non-ZIP payload
	badBody, _ := json.Marshal(map[string]string{"docx": base64.StdEncoding.EncodeToString([]byte("not-a-docx"))})
	reqBad := httptest.NewRequest(http.MethodPost, "/library/convert-docx", bytes.NewReader(badBody))
	recBad := httptest.NewRecorder()
	serveDocxToPdf(recBad, reqBad, workDir)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid docx magic, got %d", recBad.Code)
	}

	// 2. Mock soffice binary via BIOLOGY_SOFFICE_BIN to verify full handler pipeline deterministically
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "mock-soffice.sh")
	script := "#!/bin/sh\noutdir=\"\"\nfor arg in \"$@\"; do\n  if [ \"$prev\" = \"--outdir\" ]; then outdir=\"$arg\"; fi\n  prev=\"$arg\"\ndone\nprintf '%%PDF-1.7\\n1 0 obj<<>>endobj\\n%%%%EOF' > \"$outdir/paper.pdf\"\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BIOLOGY_SOFFICE_BIN", fakeBin)

	fakeDocx := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0}, 32)...)
	okBody, _ := json.Marshal(map[string]string{"docx": base64.StdEncoding.EncodeToString(fakeDocx)})
	reqOk := httptest.NewRequest(http.MethodPost, "/library/convert-docx", bytes.NewReader(okBody))
	recOk := httptest.NewRecorder()
	serveDocxToPdf(recOk, reqOk, workDir)
	if recOk.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recOk.Code, recOk.Body.String())
	}
	var resp struct {
		Pdf string `json:"pdf"`
	}
	if err := json.Unmarshal(recOk.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	pdfRaw, err := base64.StdEncoding.DecodeString(resp.Pdf)
	if err != nil || !strings.HasPrefix(string(pdfRaw), "%PDF-1.7") {
		t.Fatalf("unexpected converted PDF bytes: %q, err=%v", string(pdfRaw), err)
	}
	entries, err := os.ReadDir(filepath.Join(workDir, ".lo-tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected .lo-tmp job dir to be cleaned up, got %d entries (err=%v)", len(entries), err)
	}
}

func TestMergeSeedIntoAppPage(t *testing.T) {
	oldPriv := []byte(`<!doctype html><html><head><meta name="application-name" content="生物题册 v0.9.8"><script id="paper-data" type="application/json">{"questions":[{"id":"q1"}],"studentHash":"abc"}</script></head><body>old</body></html>`)
	newApp := []byte(`<!doctype html><html><head><meta name="application-name" content="生物题册 v0.10.2"><script id="paper-data" type="application/json">{"questions":[]}</script></head><body>new-v0102</body></html>`)
	merged := mergeSeedIntoAppPage(oldPriv, newApp)
	if !bytes.Contains(merged, []byte(`生物题册 v0.10.2`)) || !bytes.Contains(merged, []byte(`new-v0102`)) {
		t.Fatalf("merged page did not preserve new app shell: %s", merged)
	}
	if !bytes.Contains(merged, []byte(`"studentHash":"abc"`)) {
		t.Fatalf("merged page did not preserve private seed: %s", merged)
	}
}
