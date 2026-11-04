package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"

	_ "modernc.org/sqlite"
)

const releaseVersion = "v0.3.2"

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
func main() {
	listen := flag.String("listen", "127.0.0.1:18765", "监听地址")
	page := flag.String("library-page", "library.html", "独立题库页面")
	data := flag.String("data", "data", "题库数据目录")
	version := flag.Bool("version", false, "显示发行版本")
	flag.Parse()
	if *version {
		fmt.Println(releaseVersion)
		return
	}
	log.Fatal(runSelection(*page, *data, *listen))
}
