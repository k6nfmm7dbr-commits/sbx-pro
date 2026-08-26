// Package sysinfo 采集 Agent 本机信息（hostname/os/kernel/arch），
// 用于 Hello 握手与后续 heartbeat。
package sysinfo

import (
	"os"
	"runtime"
	"strings"
	"syscall"
)

// Info 是本机基础信息。
type Info struct {
	Hostname string
	OS       string
	Kernel   string
	Arch     string
}

// Gather 采集本机信息（不依赖 exec，纯 syscall/标准库）。
func Gather() Info {
	host, _ := os.Hostname()
	var uts syscall.Utsname
	_ = syscall.Uname(&uts)
	return Info{
		Hostname: host,
		OS:       runtime.GOOS,
		Kernel:   charsToString(uts.Release[:]),
		Arch:     runtime.GOARCH,
	}
}

// charsToString 把 [N]int8 转 string（含尾部 null 截断）。
func charsToString(a []int8) string {
	var b []byte
	for _, v := range a {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return strings.TrimSpace(string(b))
}
