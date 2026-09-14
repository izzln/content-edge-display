// display-server 是内容分发服务端：清单下发、媒体分发、心跳与管理查询。
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/izzln/content-edge-display/internal/server"
)

func main() {
	configPath := flag.String("config", "server.json", "配置文件路径")
	flag.Parse()

	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	s, err := server.New(cfg)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}
	log.Printf("display-server listening on %s, media_root=%s, devices=%d, admin UI at /admin",
		cfg.Listen, cfg.MediaRoot, len(cfg.Devices))
	if err := http.ListenAndServe(cfg.Listen, s.Handler()); err != nil {
		log.Fatal(err)
	}
}
