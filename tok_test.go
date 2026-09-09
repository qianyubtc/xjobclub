package main

import "testing"

// 参考值由浏览器 JS 生成：((Number(id)/1e15)*Math.PI).toString(36).replace(/(0+|\.)/g, "")
func TestTweetTokenMatchesJS(t *testing.T) {
	for _, c := range []struct{ id, want string }{
		{"20", "6dq1a2xwd93"},
		{"1234567890123456789", "2zqic77uqyk"},
		{"1965432109876543210", "4ril4uosi92"},
		{"1000000000000000000", "2f9lc2ug9mm"},
		{"999999999999", "42ko3e882oe"},
		{"1500000000000000000", "3mwe49oefy"},
		{"1965432109876543211", "4ril4uosi92"},
	} {
		if got := tweetToken(c.id); got != c.want {
			t.Errorf("id %s: go %q js %q", c.id, got, c.want)
		}
	}
}
