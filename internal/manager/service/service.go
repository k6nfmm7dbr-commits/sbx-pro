// Package service 编排 sbx-manager 的运行态。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/api"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/db"
)

// Serve 启动 Manager HTTP 服务，阻塞直至 SIGINT/SIGTERM。
func Serve() int {
	cfg, err := config.LoadStrict()
	if err != nil {
		slog.Error(err.Error())
		return 1
	}

	// 非 loopback 监听 + 空 admin_token：拒绝启动（fail-closed）。
	if cfg.AdminToken == "" && listenIsPublic(cfg.Listen) {
		slog.Error("拒绝启动: 面板监听非本机地址且未设置 admin_token，" +
			"请先执行 sbx-manager ensure-admin-token")
		return 1
	}

	db, err := db.Open(cfg.DB)
	if err != nil {
		slog.Error("数据库打开失败", "err", err)
		return 1
	}
	defer db.Close()

	srv := api.New(cfg, db)
	hs := srv.NewHTTPServer()

	ln, lerr := net.Listen("tcp", hs.Addr)
	if lerr != nil {
		slog.Error("无法监听 " + hs.Addr + " — " + lerr.Error())
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 启动在线状态维护（周期把超时机器标记 offline）。
	srv.StartOfflineSweeper(ctx, 10*time.Second)

	slog.Info("Manager 已启动 http://" + hs.Addr)

	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP 服务异常退出", "err", err)
			return 1
		}
	}

	hsCtx, hsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer hsCancel()
	if err := hs.Shutdown(hsCtx); err != nil {
		slog.Warn("HTTP 关闭超时", "err", err)
	}
	return 0
}

func listenIsPublic(listen string) bool {
	switch listen {
	case "", "0.0.0.0", "::":
		return true
	case "localhost", "127.0.0.1", "::1":
		return false
	}
	if ip := net.ParseIP(listen); ip != nil {
		return !ip.IsLoopback()
	}
	return true
}

var _ = fmt.Sprint
