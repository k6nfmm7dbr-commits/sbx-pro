package connection

import "testing"

func TestParseRemoteIPv4(t *testing.T) {
	// 1.2.3.4 = 0x01020304，little-endian 显示为 04 03 02 01
	if got := parseRemoteIP("04030201:01BB"); got != "1.2.3.4" {
		t.Errorf("IPv4 = %q, want 1.2.3.4", got)
	}
	// 8.8.8.8（对称，无歧义）
	if got := parseRemoteIP("08080808:0035"); got != "8.8.8.8" {
		t.Errorf("IPv4 = %q, want 8.8.8.8", got)
	}
}

func TestParseRemoteIPv6(t *testing.T) {
	// ::1 的小端表示
	// ::1 = 0000...0001，最后 4 字节 00 00 00 01 → little endian 0100 0000
	got := parseRemoteIP("00000000000000000000000001000000:01BB")
	if got == "" {
		t.Error("IPv6 解析失败")
	}
}

func TestParseRemoteIPEmpty(t *testing.T) {
	if parseRemoteIP("") != "" {
		t.Error("empty should return empty")
	}
	if parseRemoteIP("0102:0") != "" {
		t.Error("invalid length should return empty")
	}
}

func TestRemoteIPsByPortBasic(t *testing.T) {
	// 模拟 /proc/net/tcp 文本：表头 + 两行 ESTABLISHED 连接。
	tcpText := "  sl  local_address rem_address   st tx_queue rx_queue\n" +
		"   0: 0100007F:01BB 02010101:1234 01 00000000:00000000\n" + // local 127.0.0.1:443, rem 1.1.1.2
		"   1: 0100007F:01BB 08080808:5678 01 00000000:00000000\n" + // local 127.0.0.1:443, rem 8.8.8.8
		"   2: 0100007F:0050 09090909:5678 01 00000000:00000000\n" // local 127.0.0.1:80, rem 9.9.9.9
	readFile := func(path string) (string, error) { return tcpText, nil }

	out, partial := RemoteIPsByPort([]string{"/proc/net/tcp"}, nil, readFile)
	if partial {
		t.Error("partial should be false")
	}
	// local port 443 (0x01BB = 443)
	ips443 := out[443]
	if len(ips443) != 2 {
		t.Errorf("port 443 remote IPs = %d, want 2", len(ips443))
	}
	if _, ok := ips443["1.1.1.2"]; !ok {
		t.Error("missing 1.1.1.2")
	}
	if _, ok := ips443["8.8.8.8"]; !ok {
		t.Error("missing 8.8.8.8")
	}
	// local port 80 (0x0050 = 80)
	if len(out[80]) != 1 {
		t.Errorf("port 80 remote IPs = %d, want 1", len(out[80]))
	}
}

func TestRemoteIPsDedup(t *testing.T) {
	// 同一 IP 多条连接 → 去重。
	tcpText := "  sl  local_address rem_address   st tx_queue rx_queue\n" +
		"   0: 0100007F:01BB 02010101:1234 01 00000000:00000000\n" +
		"   1: 0100007F:01BB 02010101:9999 01 00000000:00000000\n"
	readFile := func(path string) (string, error) { return tcpText, nil }
	out, _ := RemoteIPsByPort([]string{"/proc/net/tcp"}, nil, readFile)
	if len(out[443]) != 1 {
		t.Errorf("去重后 = %d, want 1", len(out[443]))
	}
}
