package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

func ms() int64 { return time.Now().UnixMilli() }

const hourMs = 3600 * 1000
const dayMs = 24 * hourMs

// 去掉 0/O/1/I 的 32 字符表；256 % 32 == 0，取模无偏。
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func randHex(n int) string { return hex.EncodeToString(randBytes(n)) }

func randCode(n int) string {
	b := randBytes(n)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

func randToken() string { return base64.RawURLEncoding.EncodeToString(randBytes(24)) }

// randInt 均匀随机 [0,n)。
func randInt(n int) int {
	if n <= 1 {
		return 0
	}
	b := randBytes(4)
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return int(v % uint32(n))
}

// ---- 口令：PBKDF2-SHA256 ----

const pbkdfIters = 120000

func hashPassword(pw string) string {
	salt := randBytes(16)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdfIters, 32)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdfIters, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters < 1000 || iters > 10000000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iters, len(want))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// ---- 会话：userID.exp.pwtag.hmac（pwtag 随口令变化，改口令后旧会话作废）----

func pwTag(passHash string) string { return hashToken(passHash)[:8] }

func signSession(secret []byte, uid, exp int64, tag string) string {
	payload := strconv.FormatInt(uid, 10) + "." + strconv.FormatInt(exp, 10) + "." + tag
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(m.Sum(nil))[:40]
}

func parseSession(secret []byte, v string) (int64, string) {
	parts := strings.Split(v, ".")
	if len(parts) != 4 {
		return 0, ""
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || exp < time.Now().Unix() {
		return 0, ""
	}
	if !hmac.Equal([]byte(signSession(secret, uid, exp, parts[2])), []byte(v)) {
		return 0, ""
	}
	return uid, parts[2]
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// ---- 金额：内部一律用 1e-8 整数（e8），字符串进出 ----

var reAmount = regexp.MustCompile(`^(\d{1,10})(?:\.(\d{1,8}))?$`)

func parseAmountE8(s string, maxDecimals int) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+")
	if strings.HasPrefix(s, ".") {
		s = "0" + s
	}
	m := reAmount.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("金额格式不对")
	}
	if len(m[2]) > maxDecimals {
		return 0, fmt.Errorf("最多 %d 位小数", maxDecimals)
	}
	ip, _ := strconv.ParseInt(m[1], 10, 64)
	frac := m[2] + strings.Repeat("0", 8-len(m[2]))
	fp, _ := strconv.ParseInt(frac, 10, 64)
	return ip*100000000 + fp, nil
}

func fmtE8(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatInt(v/100000000, 10)
	if fp := v % 100000000; fp > 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%08d", fp), "0")
	}
	if neg {
		s = "-" + s
	}
	return s
}

// ---- 文本 ----

func cleanText(s string, max int, multiline bool) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' && multiline {
			b.WriteRune(r)
			continue
		}
		if unicode.IsControl(r) || r == '\uFEFF' {
			continue
		}
		b.WriteRune(r)
	}
	s = b.String()
	if multiline {
		lines := strings.Split(s, "\n")
		for i := range lines {
			lines[i] = strings.Join(strings.Fields(lines[i]), " ")
		}
		s = strings.Join(lines, "\n")
		for strings.Contains(s, "\n\n\n") {
			s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
		}
	} else {
		s = strings.Join(strings.Fields(s), " ")
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return strings.TrimSpace(s)
}

var reURL = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+|\bwww\.[^\s<>"']+`)

// normTweet 推文正文规范化，验证与复检都比对这个结果：HTML 实体反转义、NFKC、链接小写、空白折叠。
func normTweet(s string) string {
	s = html.UnescapeString(s)
	s = norm.NFKC.String(s)
	s = reURL.ReplaceAllStringFunc(s, func(u string) string { return strings.ToLower(strings.TrimRight(u, ".,;:!?)）]】」』")) })
	return strings.Join(strings.Fields(s), " ")
}

// weightedLen X 的加权长度：链接固定 23；U+0000–U+10FF、U+2000–U+200D、U+2010–U+201F、U+2032–U+2037 算 1，其余（CJK、emoji）算 2。
func weightedLen(s string) int {
	s = norm.NFC.String(s)
	n := 0
	rest := reURL.ReplaceAllStringFunc(s, func(string) string { n += 23; return "" })
	for _, r := range rest {
		if r == ' ' || r == '\n' {
			n++
			continue
		}
		switch {
		case r <= 0x10FF, r >= 0x2000 && r <= 0x200D, r >= 0x2010 && r <= 0x201F, r >= 0x2032 && r <= 0x2037:
			n++
		default:
			n += 2
		}
	}
	return n
}

var reBinanceOrder = regexp.MustCompile(`^\d{18}$`)
var reUID = regexp.MustCompile(`^\d{5,20}$`)
var reXHandle = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)
var reCode = regexp.MustCompile(`^[A-Z0-9]{5,8}$`)

var reTweetPath = regexp.MustCompile(`^/(?:[A-Za-z0-9_]{1,15}|i/web)/status(?:es)?/(\d{1,25})`)

var xHosts = map[string]bool{"x.com": true, "www.x.com": true, "mobile.x.com": true, "twitter.com": true, "www.twitter.com": true, "mobile.twitter.com": true}

// tweetIDFrom 从推文链接里取出 ID：只认 X / Twitter 域名，防止 evil.com/x.com/... 混过。
func tweetIDFrom(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || !xHosts[strings.ToLower(u.Hostname())] {
		return ""
	}
	m := reTweetPath.FindStringSubmatch(u.Path)
	if m == nil {
		return ""
	}
	return m[1]
}

// tweetToken 模拟 X 嵌入组件的 token 算法：((id/1e15)*π).toString(36) 去掉 "0" 与 "."。
// 小数部分按 V8 的 DoubleToRadixCString 逐位生成（生成到半 ULP 精度为止），与浏览器 JS 输出逐字一致。
func tweetToken(id string) string {
	n, err := strconv.ParseFloat(id, 64)
	if err != nil {
		return "x"
	}
	value := n / 1e15 * math.Pi
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	integer := math.Floor(value)
	fraction := value - integer
	delta := 0.5 * (math.Nextafter(value, math.Inf(1)) - value)
	if d0 := math.Nextafter(0, 1); delta < d0 {
		delta = d0
	}
	var frac []byte
	if fraction >= delta {
		for {
			fraction *= 36
			delta *= 36
			digit := int(fraction)
			frac = append(frac, chars[digit])
			fraction -= float64(digit)
			if fraction > 0.5 || (fraction == 0.5 && digit&1 == 1) {
				if fraction+delta > 1 {
					// 进位
					i := len(frac) - 1
					for ; i >= 0; i-- {
						if frac[i] == 'z' {
							frac = frac[:i]
							continue
						}
						frac[i] = chars[strings.IndexByte(chars, frac[i])+1]
						break
					}
					if i < 0 {
						integer++
					}
					break
				}
			}
			if fraction < delta {
				break
			}
		}
	}
	out := strconv.FormatInt(int64(integer), 36) + string(frac)
	return strings.NewReplacer("0", "", ".", "").Replace(out)
}

func normHandle(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "@")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if !reXHandle.MatchString(s) {
		return ""
	}
	return s
}

// xCreatedMs 按 X 数字 ID 估算账号创建时间：snowflake ID（>10^12）可解出毫秒；老式小 ID 视为 2016 年前的老号。
func xCreatedMs(idStr string) int64 {
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || id == 0 {
		return 0
	}
	if id < 1_000_000_000_000 {
		return time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	}
	return int64(id>>22) + 1288834974657
}

// shortURL 去掉 scheme 和末尾斜杠，给推文用。
func shortURL(u string) string {
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.TrimPrefix(u, "www.")
}

// ---- 展示 ----

func ago(t int64) string {
	if t <= 0 {
		return ""
	}
	d := time.Since(time.UnixMilli(t))
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
	return time.UnixMilli(t).Format("2006-01-02")
}

// left 剩余时间：到 t 还有多久（负数为已过）。
func left(t int64) string {
	if t <= 0 {
		return ""
	}
	d := time.Until(time.UnixMilli(t))
	if d < 0 {
		return "已到期"
	}
	switch {
	case d < time.Minute:
		return "不到 1 分钟"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d 天 %d 小时", int(d.Hours()/24), int(d.Hours())%24)
}

func fmtTime(t int64) string {
	if t <= 0 {
		return "—"
	}
	return time.UnixMilli(t).Format("2006-01-02 15:04")
}

func fmtDate(t int64) string {
	if t <= 0 {
		return "—"
	}
	return time.UnixMilli(t).Format("2006-01-02")
}

func dur(h int64) string {
	switch {
	case h == 0:
		return "无"
	case h%24 == 0:
		return fmt.Sprintf("%d 天", h/24)
	}
	return fmt.Sprintf("%d 小时", h)
}

func durMin(m int64) string {
	switch {
	case m%60 == 0 && m >= 60:
		return fmt.Sprintf("%d 小时", m/60)
	}
	return fmt.Sprintf("%d 分钟", m)
}

// ---- HTTP ----

var reMobileUA = regexp.MustCompile(`(?i)android|iphone|ipad|ipod|mobile|harmonyos`)

func isMobile(r *http.Request) bool { return reMobileUA.MatchString(r.UserAgent()) }

var reInApp = regexp.MustCompile(`(?i)Twitter|; wv\)|FBAN|FBAV|MicroMessenger|Line/|Instagram|Weibo`)

// inAppBrowser 是否在 App 内置浏览器里（深链拉不起、cookie 不稳）。
func inAppBrowser(ua string) bool {
	if reInApp.MatchString(ua) {
		return true
	}
	return strings.Contains(ua, "Mobile/") && strings.Contains(ua, "AppleWebKit") && !strings.Contains(ua, "Safari/")
}

// clientIP 访客 IP。开启 TRUST_PROXY 时，只有请求来自回环/内网（即反代）才信 header；
// X-Forwarded-For 取最后一跳（反代自己追加的那个，客户端伪造不了），其它头取整值。
func clientIP(r *http.Request, trustProxy bool, header string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !trustProxy {
		return host
	}
	if ip := net.ParseIP(host); ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified()) {
		return host
	}
	v := strings.TrimSpace(r.Header.Get(header))
	if v == "" {
		return host
	}
	if strings.EqualFold(header, "X-Forwarded-For") {
		parts := strings.Split(v, ",")
		v = strings.TrimSpace(parts[len(parts)-1])
	}
	if net.ParseIP(v) == nil {
		return host
	}
	return v
}

// ipPrefix 同 IP 段判断用：IPv4 取 /24，IPv6 取 /48。
func ipPrefix(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d", v4[0], v4[1], v4[2])
	}
	return p.Mask(net.CIDRMask(48, 128)).String()
}

func urlEscape(s string) string { return url.QueryEscape(s) }

func intentFor(text string) string { return "https://x.com/intent/post?text=" + urlEscape(text) }

func dayStartMs() int64 {
	t := time.Now()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).UnixMilli()
}
