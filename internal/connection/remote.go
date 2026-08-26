package connection

import (
	"encoding/binary"
	"net"
	"strconv"
	"strings"
)

// ActiveRemote 是一个活跃的远端 IP。
type ActiveRemote struct {
	IP       string
	Protocol string // tcp / udp
}

// RemoteIPsByPort 读取 /proc/net/{tcp,udp}[6]，返回 local_port -> 去重后的远端 IP 集合。
// TCP 只统计 ESTABLISHED；UDP 统计已建立会话的远端（RemConnected）。
// IPv4 rem_address 为 8 hex（小端），IPv6 为 32 hex。
func RemoteIPsByPort(tcpFiles, udpFiles []string, readFile func(string) (string, error)) (map[int]map[string]ActiveRemote, bool) {
	out := map[int]map[string]ActiveRemote{}
	partial := false
	// tcp
	for _, f := range tcpFiles {
		text, err := readFile(f)
		if err != nil {
			if !isNotExist(err) {
				partial = true
			}
			continue
		}
		collectRemote(out, text, tcpEstablished, "tcp")
	}
	// udp
	for _, f := range udpFiles {
		text, err := readFile(f)
		if err != nil {
			if !isNotExist(err) {
				partial = true
			}
			continue
		}
		collectRemote(out, text, "", "udp")
	}
	return out, partial
}

// collectRemote 解析 /proc 文本，把远端 IP 归入对应本地端口。
// tcpKeep 为空表示 UDP（用 RemConnected 判定）；否则用 st==tcpKeep。
func collectRemote(out map[int]map[string]ActiveRemote, text, tcpKeep, proto string) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if i == 0 {
			continue // 表头
		}
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		local, rem, st := parts[1], parts[2], parts[3]
		// TCP 需 ESTABLISHED；UDP 需远端已连接。
		if tcpKeep != "" {
			if st != tcpKeep {
				continue
			}
		} else {
			if !RemConnected(rem) {
				continue
			}
		}
		// 本地端口。
		li := strings.IndexByte(local, ':')
		if li < 0 {
			continue
		}
		port, err := strconv.ParseInt(local[li+1:], 16, 64)
		if err != nil {
			continue
		}
		// 远端 IP。
		ip := parseRemoteIP(rem)
		if ip == "" {
			continue
		}
		if out[int(port)] == nil {
			out[int(port)] = map[string]ActiveRemote{}
		}
		out[int(port)][ip] = ActiveRemote{IP: ip, Protocol: proto}
	}
}

// parseRemoteIP 从 rem_address（HEX_IP:HEX_PORT）解析 IP 字符串。
func parseRemoteIP(rem string) string {
	i := strings.LastIndexByte(rem, ':')
	if i < 0 {
		return ""
	}
	hexIP := rem[:i]
	switch len(hexIP) {
	case 8: // IPv4（小端 4 字节）
		b := make([]byte, 4)
		for j := 0; j < 4; j++ {
			v, err := strconv.ParseUint(hexIP[j*2:j*2+2], 16, 8)
			if err != nil {
				return ""
			}
			b[3-j] = byte(v) // 小端 → 网络序
		}
		return net.IP(b).String()
	case 32: // IPv6（16 字节，每 4 hex 一组小端）
		b := make([]byte, 16)
		for j := 0; j < 8; j++ {
			v, err := strconv.ParseUint(hexIP[j*4:j*4+4], 16, 16)
			if err != nil {
				return ""
			}
			binary.LittleEndian.PutUint16(b[j*2:j*2+2], uint16(v))
		}
		return net.IP(b).String()
	default:
		return ""
	}
}

func isNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such file")
}
