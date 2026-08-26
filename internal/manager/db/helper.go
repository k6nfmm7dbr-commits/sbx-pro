package db

import "strings"

// splitStatements 把 SQL 脚本按分号切成独立语句，跳过空语句与纯注释。
// 与 internal/database 的 splitStatements 行为一致（该函数未导出，故在此重写）。
func splitStatements(script string) []string {
	var out []string
	for _, part := range strings.Split(script, ";") {
		stmt := strings.TrimSpace(part)
		if stmt == "" || isCommentOnly(stmt) {
			continue
		}
		out = append(out, stmt)
	}
	return out
}

func isCommentOnly(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "--") {
			return false
		}
	}
	return true
}
