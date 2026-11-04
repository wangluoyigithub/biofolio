package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"sync"
)

// Store immutable image data once per server run. The durable records, seed,
// backups and offline HTML keep their original data URLs.
const picturePrefix = "library/picture/"

var embeddedPicture = regexp.MustCompile(`data:image/(?:png|jpeg|jpg|webp|gif);base64,[A-Za-z0-9+/]+=*`)
var pictureReference = regexp.MustCompile(picturePrefix + `[a-f0-9]{64}`)

type pictureStore struct{ images sync.Map }

func (p *pictureStore) compact(body []byte) []byte {
	return embeddedPicture.ReplaceAllFunc(body, func(uri []byte) []byte {
		key := fmt.Sprintf("%s%x", picturePrefix, sha256.Sum256(uri))
		if _, ok := p.images.Load(key); !ok {
			p.images.LoadOrStore(key, string(uri))
		}
		return []byte(key)
	})
}
func (p *pictureStore) expand(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(picturePrefix)) {
		return body, nil
	}
	var err error
	result := pictureReference.ReplaceAllFunc(body, func(key []byte) []byte {
		if uri, ok := p.images.Load(string(key)); ok {
			return []byte(uri.(string))
		}
		err = fmt.Errorf("题图已失效，请刷新后重试")
		return key
	})
	return result, err
}
func (p *pictureStore) serve(w http.ResponseWriter, key string) {
	stored, ok := p.images.Load(key)
	if !ok {
		http.Error(w, "题图不存在，请刷新", 404)
		return
	}
	uri := []byte(stored.(string))
	header, payload, _ := bytes.Cut(uri, []byte(";base64,"))
	image, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		http.Error(w, "图片格式无效", 422)
		return
	}
	w.Header().Set("Content-Type", string(header[len("data:"):]))
	w.Write(image)
}
