package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The deployed page is also the portable offline archive template. Split the
// online copy once at startup; never strip attachments from the offline copy.
type pageAsset struct {
	content, compressed []byte
	mime                string
	immutable           bool
}
type onlinePage struct {
	pictures pictureStore
	html     []byte
	files    map[string]pageAsset
}

func zipBytes(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}
func (p *onlinePage) add(name, mime string, b []byte, cache bool) string {
	if cache {
		name = fmt.Sprintf("%x-%s", sha256.Sum256(b), name)
	}
	route := "library/assets/" + name
	p.files["/"+route] = pageAsset{b, zipBytes(b), mime, cache}
	return route
}

// The workbench needs IDs and completeness flags for its counts, but not the
// original formatted faces, answers or attachments.
func questionSummary(q map[string]json.RawMessage) map[string]json.RawMessage {
	small := map[string]json.RawMessage{"id": q["id"]}
	for _, f := range []string{"revision", "date", "collectedAt"} {
		if v := q[f]; v != nil {
			small[f] = v
		}
	}
	var stem, face string
	_ = json.Unmarshal(q["stem"], &stem)
	if photoStem.MatchString(strings.TrimSpace(stem)) && json.Unmarshal(q["contentHtml"], &face) == nil && strings.Contains(face, "<img") {
		small["photoPlaceholder"] = json.RawMessage("true")
		small["stem"] = q["stem"]
	}
	for _, f := range []string{"answer", "answerHtml", "analysisHtml", "a"} {
		var value string
		if json.Unmarshal(q[f], &value) == nil && solutionPresent(value) {
			small[f] = json.RawMessage(`"1"`)
		}
	}
	return small
}
func recordSummary(b []byte) (json.RawMessage, error) {
	var r struct {
		ID         string                       `json:"id"`
		Name       string                       `json:"name,omitempty"`
		Paper      bool                         `json:"paper,omitempty"`
		Template   bool                         `json:"wordTemplate,omitempty"`
		UploadedBy string                       `json:"uploadedBy,omitempty"`
		Questions  []map[string]json.RawMessage `json:"questions"`
		Before     map[string]json.RawMessage   `json:"before,omitempty"`
		Question   map[string]json.RawMessage   `json:"question,omitempty"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Questions == nil {
		r.Questions = []map[string]json.RawMessage{}
	}
	for i, q := range r.Questions {
		r.Questions[i] = questionSummary(q)
	}
	if r.Before != nil {
		r.Before = questionSummary(r.Before)
		r.Question = questionSummary(r.Question)
	}
	return json.Marshal(r)
}
func splitOnlinePage(raw []byte) (*onlinePage, error) {
	p := &onlinePage{html: raw, files: make(map[string]pageAsset)}
	if !bytes.Contains(raw, []byte(`<script data-lib="vue">`)) {
		return p, nil
	} // Older templates expect a self-contained page.
	split := func(start string, replace func([]byte) (string, error)) error {
		pre, rest, ok := bytes.Cut(p.html, []byte(start))
		if !ok {
			return nil
		}
		content, post, ok := bytes.Cut(rest, []byte("</script>"))
		if !ok {
			return fmt.Errorf("missing end tag: %s", start)
		}
		tag, err := replace(content)
		if err != nil {
			return err
		}
		p.html = bytes.Join([][]byte{pre, []byte(tag), post}, nil)
		return nil
	}
	for _, name := range []string{"xlsx", "docx", "zip", "vue"} {
		err := split(`<script data-lib="`+name+`">`, func(b []byte) (string, error) {
			url := p.add(name+".js", "text/javascript; charset=utf-8", b, true)
			if name == "vue" {
				return `<script src="` + url + `"></script>`, nil
			}
			return `<script id="lib-` + name + `" type="text/plain" data-url="` + url + `"></script>`, nil
		})
		if err != nil {
			return nil, err
		}
	}
	for _, id := range []string{"pdf-libraries", "word-libraries"} {
		err := split(`<script id="`+id+`" type="text/plain">`, func(b []byte) (string, error) {
			data, err := base64.StdEncoding.DecodeString(string(b))
			if err != nil {
				return "", err
			}
			url := p.add(id, "application/octet-stream", data, true)
			return `<script id="` + id + `" type="text/plain" data-url="` + url + `"></script>`, nil
		})
		if err != nil {
			return nil, err
		}
	}
	err := split(`<script id="paper-data" type="application/json">`, func(b []byte) (string, error) {
		var seed map[string]json.RawMessage
		if err := json.Unmarshal(b, &seed); err != nil {
			return "", err
		}
		if original := seed["originalDocx"]; original != nil {
			var encoded string
			if err := json.Unmarshal(original, &encoded); err != nil {
				return "", err
			}
			if encoded != "" {
				doc, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return "", err
				}
				url := p.add("original.docx", "application/octet-stream", doc, false)
				seed["originalUrl"], _ = json.Marshal(url)
			}
			delete(seed, "originalDocx")
		}
		var questions []map[string]json.RawMessage
		if q := seed["questions"]; len(q) > 0 {
			if err := json.Unmarshal(q, &questions); err != nil {
				return "", err
			}
		}
		if len(questions) > 0 {
			faces := []byte(seed["questions"])
			if bytes.Contains(raw, []byte("function libraryPicture(")) {
				faces = p.pictures.compact(faces)
			}
			url := p.add("seed-questions", "application/json", faces, false)
			seed["questionsUrl"], _ = json.Marshal(url)
			for i, q := range questions {
				questions[i] = questionSummary(q)
			}
			seed["questions"], _ = json.Marshal(questions)
		}
		slim, err := json.Marshal(seed)
		return `<script id="paper-data" type="application/json">` + string(slim) + "</script>", err
	})
	return p, err
}
func sendAsset(w http.ResponseWriter, r *http.Request, a pageAsset) {
	w.Header().Set("Content-Type", a.mime)
	if a.immutable {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	}
	w.Header().Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip;q=0") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(a.compressed)
		return
	}
	w.Write(a.content)
}
