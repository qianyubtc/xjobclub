package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfgPath := flag.String("config", "config.env", "配置文件路径")
	cancelOpen := flag.String("cancel-open", "", "维护：把所有在售任务停止接新单（保留已接记录），参数为通知发布方的原因，执行后退出")
	resolveSpec := flag.String("resolve", "", "维护：裁决一条申诉，格式 申诉编号:结论[:补充]（如 D-XXXXXX:upheld:https://x.com/u/status/123），效果等同后台裁决，执行后退出")
	flag.Parse()
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}
	app, err := newApp(cfg)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	if *cancelOpen != "" {
		n := app.cancelOpenTasks(*cancelOpen, 0)
		log.Printf("[info] 已撤回 %d 个在售任务（已接记录保留）", n)
		app.flushTG()
		app.Close()
		return
	}
	if *resolveSpec != "" {
		msg, err := app.resolveDisputeSpec(*resolveSpec)
		app.flushTG()
		app.Close()
		if err != nil {
			log.Fatalf("裁决失败: %v", err)
		}
		log.Printf("[info] %s", msg)
		return
	}
	app.startJobs()
	srv := &http.Server{Addr: cfg.Listen, Handler: app, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		log.Printf("[info] %s 监听 %s（%s）", cfg.SiteTitle, cfg.Listen, cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("监听失败: %v", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	app.Close()
	log.Println("[info] 已退出")
}
