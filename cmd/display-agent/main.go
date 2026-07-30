// display-agent 是显示屏端播放代理：轮询清单、下载校验、驱动 mpv 循环播放、心跳上报。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/izzln/content-edge-display/internal/agent"
	"github.com/izzln/content-edge-display/internal/player"
)

func main() {
	configPath := flag.String("config", "agent.json", "配置文件路径")
	flag.Parse()

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	var p player.Player
	switch cfg.Player {
	case "mpv":
		if err := os.MkdirAll(filepath.Dir(cfg.MpvSocket), 0o755); err != nil {
			log.Fatalf("create mpv socket dir: %v", err)
		}
		p = player.NewMPV(cfg.MpvSocket,
			filepath.Join(cfg.CacheDir, "playlist.m3u"),
			cfg.ImageDurationS, cfg.MpvExtraArgs)
	case "null":
		p = player.NewNull()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("display-agent starting: device=%s server=%s player=%s",
		cfg.DeviceID, cfg.ServerURL, cfg.Player)
	if err := agent.New(cfg, p).Run(ctx); err != nil {
		log.Fatal(err)
	}
}
