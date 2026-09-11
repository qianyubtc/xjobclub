package main

import (
	"strings"
	"testing"
	"time"
)

func TestNormTweet(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Hello   World ", "Hello World"},
		{"看 HTTPS://Example.com/Abc 这个", "看 example.com/abc 这个"},
		{"a &amp; b", "a & b"},
		{"全角，逗号", "全角,逗号"},
		{"链接 https://x.com/abc。", "链接 x.com/abc。"},
		{"官网 bitget.com/ref 走起", "官网 bitget.com/ref 走起"},
		{"官网 http://www.Bitget.com/ref 走起", "官网 bitget.com/ref 走起"},
	}
	for _, c := range cases {
		if got := normTweet(c.in); got != c.want {
			t.Errorf("normTweet(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWeightedLen(t *testing.T) {
	if n := weightedLen("hello"); n != 5 {
		t.Errorf("ascii: %d", n)
	}
	if n := weightedLen("你好"); n != 4 {
		t.Errorf("cjk: %d", n)
	}
	if n := weightedLen("看 https://example.com/very/long/path/that/is/longer/than/23"); n != 2+1+23 {
		t.Errorf("url: %d", n)
	}
	if n := weightedLen("😀"); n != 2 {
		t.Errorf("emoji: %d", n)
	}
}

func TestMatchContent(t *testing.T) {
	task := &Task{MatchMode: "exact", ContentsNorm: []string{normTweet("推了么 上线了 https://xjob.club #广告"), normTweet("来推了么接任务 https://xjob.club #广告")}}
	ok, idx := matchContent(task, normTweet("来推了么接任务 https://xjob.club #广告"), 0)
	if !ok || idx != 1 {
		t.Fatalf("exact any-variant: ok=%v idx=%d", ok, idx)
	}
	if ok, _ := matchContent(task, normTweet("推了么 上线了 https://xjob.club #广告 加一句"), 0); ok {
		t.Fatal("exact should reject extra words")
	}
	task.MatchMode = "contains"
	if ok, _ := matchContent(task, normTweet("我觉得不错：推了么 上线了 https://xjob.club #广告 试试"), 0); !ok {
		t.Fatal("contains should accept extra words")
	}
}

func TestDiffHint(t *testing.T) {
	h := diffHint("abcdef", "abcxef")
	if !strings.HasPrefix(h, "第") {
		t.Errorf("hint: %q", h)
	}
	if diffHint("same", "same") != "" {
		t.Error("identical should be empty")
	}
}

func TestXCreatedMs(t *testing.T) {
	// 2023 年左右的 snowflake 用户 ID
	ms := xCreatedMs("1640000000000000000")
	y := time.UnixMilli(ms).Year()
	if y < 2022 || y > 2024 {
		t.Errorf("snowflake year %d", y)
	}
	if time.UnixMilli(xCreatedMs("12")).Year() != 2015 {
		t.Error("small id should map to 2015 sentinel")
	}
	if xCreatedMs("abc") != 0 {
		t.Error("bad id should be 0")
	}
}

func TestTweetHelpers(t *testing.T) {
	if id := tweetIDFrom("https://x.com/jack/status/20?s=20"); id != "20" {
		t.Errorf("id %q", id)
	}
	if id := tweetIDFrom("https://twitter.com/i/web/status/1234567890123456789"); id != "1234567890123456789" {
		t.Errorf("id %q", id)
	}
	if tweetIDFrom("https://evil.com/x.com/a/status/1") != "" {
		t.Error("should reject foreign host")
	}
	if tok := tweetToken("20"); tok != "6dq1a2xwd93" {
		t.Errorf("token %q", tok)
	}
}

func TestAmounts(t *testing.T) {
	e8, err := parseAmountE8("10.0037", 4)
	if err != nil || e8 != 1000370000 {
		t.Fatalf("parse: %d %v", e8, err)
	}
	if fmtE8(e8) != "10.0037" {
		t.Errorf("fmt %s", fmtE8(e8))
	}
	if _, err := parseAmountE8("1.00001", 4); err == nil {
		t.Error("too many decimals should fail")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	secret := []byte("s3cret")
	v := signSession(secret, 42, time.Now().Add(time.Hour).Unix(), "tag12345")
	id, tag := parseSession(secret, v)
	if id != 42 || tag != "tag12345" {
		t.Fatalf("got %d %s", id, tag)
	}
	if id, _ := parseSession([]byte("other"), v); id != 0 {
		t.Error("wrong secret should fail")
	}
}

func TestAdTagged(t *testing.T) {
	if adTagged("hello") != "hello #广告" {
		t.Error("should append")
	}
	if adTagged("hello #推广") != "hello #推广" {
		t.Error("should not duplicate")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/me?tab=subs": "/me?tab=subs", "//evil.com": "/me", "/\\evil.com": "/me", "https://evil.com/x": "/me", "": "/me", "/t/T-ABCDE": "/t/T-ABCDE"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJuryMajority(t *testing.T) {
	a := &App{cfg: &Config{JuryPanel: 7}}
	if a.readyToClose(&JuryCase{VotesFor: 1, Abstain: 6}) {
		t.Error("1 for + 6 abstain must not close")
	}
	if !a.readyToClose(&JuryCase{VotesFor: 4, VotesAgainst: 3}) {
		t.Error("4:3 should close")
	}
	if a.readyToClose(&JuryCase{VotesFor: 3, VotesAgainst: 3, Abstain: 1}) {
		t.Error("tie must not close")
	}
	if validMajority(&JuryCase{VotesFor: 3, VotesAgainst: 1}) {
		t.Error("fewer than 5 valid votes is not a majority")
	}
}
