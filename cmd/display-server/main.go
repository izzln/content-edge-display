// display-server 是内容分发服务端：清单下发、媒体分发、心跳与管理查询。
package main

import (
	"flag"
	"log"
	"net/http"
	"time"
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
	log.Printf("display-server: HTTPS on %s (admin UI at /admin), install entry on http %s, media_root=%s, data_dir=%s",
		cfg.Listen, cfg.BootstrapListen, cfg.MediaRoot, cfg.DataDir)
	log.Printf("TLS certificate fingerprint (devices pin it): %s", s.CertFingerprint())

	// 装机入口（HTTP）：只有 install.sh 与装机用的程序包，其余跳到 HTTPS
	boot := &http.Server{Addr: cfg.BootstrapListen, Handler: s.BootstrapHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { log.Fatal(boot.ListenAndServe()) }()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Handler(),
		TLSConfig:         s.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		// 空闲长连接比设备端（90 秒）晚关：总是设备先放手。反过来的话，服务端刚关掉一条连接、
		// 设备恰好拿它发请求，就是一次 connection reset by peer。
		// 不设读写总超时：上传几百 MB 的视频、设备下载大文件都要很久。
		IdleTimeout: 120 * time.Second,
	}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
