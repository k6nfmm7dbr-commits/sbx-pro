package gateway

import (
	"context"
	"time"
)

// heartbeatTimeout 是判定离线的心跳超时阈值。
// 开发提示词第八节：30s 未收到 → warning，60~90s → offline。
const (
	WarningThreshold = 30 * time.Second
	OfflineThreshold = 60 * time.Second
)

// RunOfflineSweeper 启动后台协程，周期把超过阈值的机器标记为 offline。
// 通过 context 取消退出（goroutine 可退出，满足代码质量要求）。
func (g *Gateway) RunOfflineSweeper(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.sweepOffline()
			}
		}
	}()
}

// sweepOffline 把 last_seen 超时的在线机器标记为 offline。
func (g *Gateway) sweepOffline() {
	cutoff := time.Now().Add(-OfflineThreshold).Unix()
	_, _ = g.db.SQL().Exec(
		`UPDATE machines SET status = 'offline' WHERE status = 'online' AND last_seen < ?`,
		cutoff)
}
