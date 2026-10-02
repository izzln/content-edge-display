// display-server 是内容分发服务端：清单下发、媒体分发、心跳与管理查询。
package main

import (
	"flag"
	"log"
	"net/http"
	// 内嵌时区数据：server.json 写了 timezone 而服务器没装 tzdata 包（精简系统、离线装机常见）时，
	// time.LoadLocation 会失败、服务端拒绝启动。内嵌后约增加 450KB。
	_ "time/tzdata"

	"github.com/izzln/content-edge-display/internal/server"
)

func main() {
	configPath := flag.String("config", "server.json", "path to the config file")
	flag.Parse()

	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	s, err := server.New(cfg)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}
	log.Printf("display-server listening on %s, media_root=%s, data_dir=%s, admin UI at /admin",
		cfg.Listen, cfg.MediaRoot, cfg.DataDir)
	if err := http.ListenAndServe(cfg.Listen, s.Handler()); err != nil {
		log.Fatal(err)
	}
}
