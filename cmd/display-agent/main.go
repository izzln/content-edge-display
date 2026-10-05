// display-agent 是显示屏端播放代理：自注册、轮询清单、下载校验、驱动 GStreamer 循环播放、心跳上报、程序更新。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/izzln/content-edge-display/internal/agent"
	"github.com/izzln/content-edge-display/internal/player"
)

func main() {
	configPath := flag.String("config", "agent.json", "path to the config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(agent.Version)
		return
	}

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	var p player.Player
	switch cfg.Player {
	case "gst":
		w, h := agent.OutputMode(cfg.DisplayMode)
		p = player.NewGST(cfg.CacheDir, w, h)
	case "null":
		p = player.NewNull()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("display-agent %s starting: server=%s player=%s", agent.Version, cfg.ServerURL, cfg.Player)
	err = agent.New(cfg, p).Run(ctx)
	if errors.Is(err, agent.ErrRestartForUpdate) {
		log.Printf("display-agent: exiting to apply update (systemd will restart)")
		return // 退出码 0；systemd Restart=always 拉起 current 指向的新版本
	}
	if err != nil {
		log.Fatal(err)
	}
}
