package api

import "crypto/subtle"

// constantTimeEq 常量时间比较两等长字符串（避免 timing 差异）。
func constantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
