package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func resolveSofficeBinary() string {
	if env := os.Getenv("BIOLOGY_SOFFICE_BIN"); env != "" {
		if st, err := os.Stat(env); err == nil && !st.IsDir() {
			return env
		}
	}
	for _, name := range []string{"soffice", "libreoffice"} {
		if p, err := exec.LookPath(name); err == nil && p != "" {
			if name == "soffice" && p == "/opt/homebrew/bin/soffice" {
				if _, err2 := os.Stat("/Applications/LibreOffice.app/Contents/MacOS/soffice"); err2 != nil {
					continue
				}
			}
			return p
		}
	}
	macPaths := []string{
		"/Volumes/AI/Applications/LibreOffice.app/Contents/MacOS/soffice",
		"/Applications/LibreOffice.app/Contents/MacOS/soffice",
	}
	for _, p := range macPaths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// convertDocxBytesToPdf converts a .docx byte slice into PDF via headless LibreOffice.
// workBaseDir points to the writable --data directory so conversion works cleanly inside
// read-only rootfs containers (read_only: true, no /tmp mount, cap_drop: ALL).
func convertDocxBytesToPdf(ctx context.Context, docxBytes []byte, workBaseDir string) ([]byte, error) {
	bin := resolveSofficeBinary()
	if bin == "" {
		return nil, os.ErrNotExist
	}
	tmpRoot := ""
	if workBaseDir != "" {
		tmpRoot = filepath.Join(workBaseDir, ".lo-tmp")
		_ = os.MkdirAll(tmpRoot, 0700)
		if ents, err := os.ReadDir(tmpRoot); err == nil {
			cutoff := time.Now().Add(-5 * time.Minute)
			for _, ent := range ents {
				if strings.HasPrefix(ent.Name(), "OSL_PIPE_") {
					if info, err := ent.Info(); err == nil && info.ModTime().Before(cutoff) {
						_ = os.Remove(filepath.Join(tmpRoot, ent.Name()))
					}
				}
			}
		}
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, "job-*")
	if err != nil && tmpRoot != "" {
		tmpDir, err = os.MkdirTemp("", "biofolio-docx-*")
	}
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	profileDir := filepath.Join(tmpDir, "profile")
	sysTmpDir := filepath.Join(tmpDir, "tmp")
	cacheDir := filepath.Join(tmpDir, "cache")
	configDir := filepath.Join(tmpDir, "config")
	for _, d := range []string{profileDir, sysTmpDir, cacheDir, configDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, err
		}
	}
	inPath := filepath.Join(tmpDir, "paper.docx")
	outPath := filepath.Join(tmpDir, "paper.pdf")
	if err := os.WriteFile(inPath, docxBytes, 0600); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(
		ctx,
		bin,
		"--headless",
		"--nologo",
		"--nodefault",
		"--norestore",
		"--nofirststartwizard",
		"-env:UserInstallation=file://"+filepath.ToSlash(profileDir),
		"--convert-to", "pdf",
		"--outdir", tmpDir,
		inPath,
	)
	cmd.Env = append(
		os.Environ(),
		"HOME="+tmpDir,
		"TMPDIR="+sysTmpDir,
		"TMP="+sysTmpDir,
		"TEMP="+sysTmpDir,
		"XDG_CACHE_HOME="+cacheDir,
		"XDG_CONFIG_HOME="+configDir,
		"XDG_RUNTIME_DIR="+sysTmpDir,
		"SAL_USE_VCLPLUGIN=svp",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("soffice convert error: %v (%s)", err, strings.TrimSpace(stderr.String()))
		return nil, err
	}
	return os.ReadFile(outPath)
}

func serveDocxToPdf(w http.ResponseWriter, r *http.Request, workBaseDir string) {
	var req struct {
		Docx string `json:"docx"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 48<<20)).Decode(&req); err != nil || req.Docx == "" {
		http.Error(w, "请上传有效的 .docx 文件", http.StatusBadRequest)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Docx)
	if err != nil || len(raw) < 4 || !bytes.Equal(raw[:4], []byte("PK\x03\x04")) {
		http.Error(w, "文件不是有效的标准 .docx 文档", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	pdfBytes, err := convertDocxBytesToPdf(ctx, raw, workBaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "当前服务端未安装 LibreOffice，请先在 Word 中另存为 PDF 后上传封版", http.StatusNotImplemented)
			return
		}
		http.Error(w, "Word 转 PDF 失败，请检查文档格式或另存为 PDF 后上传", http.StatusInternalServerError)
		return
	}
	if len(pdfBytes) < 8 || !bytes.HasPrefix(pdfBytes, []byte("%PDF-")) {
		http.Error(w, "转换结果不是有效 PDF", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{
		"pdf": base64.StdEncoding.EncodeToString(pdfBytes),
	})
}
