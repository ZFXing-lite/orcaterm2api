// orcaterm2api: OrcaTerm 内置 AI 的多账号 OpenAI 兼容网关。
//
// 单二进制 + 一个 YAML 配置：账号池轮询、限速冷却、跨号重试、SOCKS5 代理池、
// Web 管理面板，对外暴露 OpenAI / Anthropic / Responses 三套接口。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
	"github.com/ZFXing-lite/orcaterm2api/internal/keypool"
	"github.com/ZFXing-lite/orcaterm2api/internal/metrics"
	"github.com/ZFXing-lite/orcaterm2api/internal/proxypool"
	"github.com/ZFXing-lite/orcaterm2api/internal/server"
)

const version = "1.1.4"

func main() {
	cfgPath := flag.String("c", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	snap := cfg.Snapshot()
	if snap.RemoteManagement.SecretKey == "" {
		log.Printf("[WARN] 未设置面板密码（remote-management.secret-key / ORCATERM2API_MGMT_KEY），管理面板仅限本地访问")
	}

	// 代理池
	proxyPool := proxypool.New(
		snap.Proxy.Policy,
		config.ParseDuration(snap.Proxy.HealthEvery, 30*time.Second),
		config.ParseDuration(snap.Proxy.FailCooldown, 30*time.Second),
	)
	proxyPool.Sync(snap.Proxy.SOCKS5)

	// 账号池
	poolOpts := keypool.OptionsFromConfig(cfg)
	pool := keypool.New(poolOpts)
	pool.SyncAccounts(snap.Upstream.Keys)

	// 用量统计（与账号状态同目录，落在 state/usage.json）
	usageFile := filepath.Join(filepath.Dir(snap.Metrics.StateFile), "usage.json")
	tracker := metrics.New(snap.Metrics.Enabled)
	if err := tracker.Load(usageFile); err != nil {
		log.Printf("[WARN] 用量文件回读失败（将从头计数）: %v", err)
	}
	go tracker.Persist(usageFile, config.ParseDuration(snap.Metrics.FlushEvery, 5*time.Second))

	srv := server.New(cfg, pool, proxyPool, tracker)

	// 配置热重载：文件变化时同步账号池/代理池/池参数
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go cfg.Watch(stopWatch, func() {
		ns := cfg.Snapshot()
		pool.SyncAccounts(ns.Upstream.Keys)
		pool.UpdateOptions(keypool.OptionsFromConfig(cfg))
		proxyPool.Sync(ns.Proxy.SOCKS5)
		proxyPool.SetPolicy(ns.Proxy.Policy)
		log.Printf("[INFO] 配置已热重载：账号 %d，代理 %d", len(ns.Upstream.Keys), len(ns.Proxy.SOCKS5))
	})

	addr := fmt.Sprintf(":%d", snap.Port)
	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE 流式不限制写超时
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("[INFO] orcaterm2api v%s 启动于 http://0.0.0.0%s", version, addr)
		log.Printf("[INFO] 管理面板: http://127.0.0.1%s/v0/management/panel", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("[INFO] 正在关闭...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	pool.Close()
	proxyPool.Close()
	tracker.Close()
	log.Printf("[INFO] 已退出")
}
