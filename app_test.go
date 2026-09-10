package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bpaygate "github.com/qianyubtc/BinancePayTool/sdk/go"
)

// ---- mock X syndication ----

type mockTweet struct {
	ID, Text, UserID, Handle, Name string
	CreatedMs                      int64
	Reply                          bool
	InReplyTo                      string // 直接回复的推文 ID
}

type syndMock struct {
	mu     sync.Mutex
	tweets map[string]mockTweet
	srv    *httptest.Server
}

func newSynd() *syndMock {
	m := &syndMock{tweets: map[string]mockTweet{}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		m.mu.Lock()
		tw, ok := m.tweets[id]
		m.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		out := map[string]any{"__typename": "Tweet", "id_str": tw.ID, "text": tw.Text, "created_at": time.UnixMilli(tw.CreatedMs).UTC().Format(time.RFC3339Nano),
			"user":     map[string]any{"id_str": tw.UserID, "name": tw.Name, "screen_name": tw.Handle, "profile_image_url_https": "https://pbs.twimg.com/x_normal.jpg"},
			"entities": map[string]any{"urls": []any{}}}
		if tw.InReplyTo != "" {
			out["in_reply_to_status_id_str"] = tw.InReplyTo
			out["parent"] = map[string]any{"id_str": tw.InReplyTo}
		} else if tw.Reply {
			out["in_reply_to_status_id_str"] = "1"
		}
		json.NewEncoder(w).Encode(out)
	}))
	return m
}

func (m *syndMock) add(tw mockTweet) {
	if tw.CreatedMs == 0 {
		tw.CreatedMs = ms()
	}
	m.mu.Lock()
	m.tweets[tw.ID] = tw
	m.mu.Unlock()
}

func (m *syndMock) del(id string) { m.mu.Lock(); delete(m.tweets, id); m.mu.Unlock() }

// ---- mock gateway ----

type gwOrder struct {
	OrderID, AccountID, MerchantOrderID, Status, Currency, BaseAmount, PayAmount, NoteCode, ActualAmount, MatchedBy, BinanceOrderID, PayerID string
	PaidAt, ExpiresAt                                                                                                                        int64
	CallbackURL                                                                                                                              string
}

type gwMock struct {
	mu       sync.Mutex
	sub      bool // 减尾数模式：pay_amount = base - 尾数
	key      string
	orders   map[string]*gwOrder
	byMid    map[string]*gwOrder
	accounts map[string]map[string]any
	n        int
	srv      *httptest.Server
	appURL   string
}

func newGW(key string) *gwMock {
	g := &gwMock{key: key, orders: map[string]*gwOrder{}, byMid: map[string]*gwOrder{}, accounts: map[string]map[string]any{}}
	mux := http.NewServeMux()
	env := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": data})
	}
	fail := func(w http.ResponseWriter, code, msg string) {
		json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
	}
	mux.HandleFunc("POST /api/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		var req bpaygate.CreateOrderReq
		json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		defer g.mu.Unlock()
		if _, dup := g.byMid[req.MerchantOrderID]; dup {
			fail(w, "ERR_DUPLICATE", "dup")
			return
		}
		g.n++
		o := &gwOrder{OrderID: fmt.Sprintf("GW%d", g.n), AccountID: req.AccountID, MerchantOrderID: req.MerchantOrderID, Status: "pending", Currency: req.Currency, BaseAmount: req.Amount,
			PayAmount: req.Amount + "01", NoteCode: "ABC123", ExpiresAt: ms() + req.Timeout*1000, CallbackURL: req.CallbackURL}
		if !strings.Contains(req.Amount, ".") {
			o.PayAmount = req.Amount + ".0001"
		}
		if g.sub {
			base, _ := parseAmountE8(req.Amount, 8)
			o.PayAmount = fmtE8(base - 720000) // 例：0.1 → 0.0928
		}
		if o.AccountID == "" {
			o.AccountID = "default"
		}
		g.orders[o.OrderID], g.byMid[o.MerchantOrderID] = o, o
		env(w, g.view(o))
	})
	mux.HandleFunc("GET /api/v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		o := g.orders[r.PathValue("id")]
		if o == nil {
			fail(w, "ERR_NOT_FOUND", "no")
			return
		}
		env(w, g.view(o))
	})
	mux.HandleFunc("POST /api/v1/orders/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		o := g.orders[r.PathValue("id")]
		if o == nil {
			fail(w, "ERR_NOT_FOUND", "no")
			return
		}
		if o.Status == "pending" {
			o.Status = "closed"
		}
		env(w, g.view(o))
	})
	mux.HandleFunc("POST /api/v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		var req bpaygate.CreateAccountReq
		json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		defer g.mu.Unlock()
		if req.APIKey == "badkey-badkey-badkey" {
			fail(w, "ERR_BINANCE", "Invalid API-key, IP, or permissions for action")
			return
		}
		id := "acct-" + req.APIKey[:6]
		g.accounts[id] = map[string]any{"account_id": id, "label": req.Label, "api_key_masked": req.APIKey[:4] + "****", "uid": req.UID, "status": "active", "last_ok": ms()}
		env(w, g.accounts[id])
	})
	mux.HandleFunc("GET /api/v1/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if a := g.accounts[r.PathValue("id")]; a != nil {
			env(w, a)
			return
		}
		fail(w, "ERR_NOT_FOUND", "no")
	})
	mux.HandleFunc("POST /api/v1/accounts/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		a := g.accounts[r.PathValue("id")]
		if a == nil {
			fail(w, "ERR_NOT_FOUND", "no")
			return
		}
		if r.PathValue("action") == "disable" {
			a["status"] = "disabled"
		}
		env(w, a)
	})
	g.srv = httptest.NewServer(mux)
	return g
}

func (g *gwMock) view(o *gwOrder) map[string]any {
	return map[string]any{"order_id": o.OrderID, "account_id": o.AccountID, "merchant_order_id": o.MerchantOrderID, "status": o.Status, "currency": o.Currency, "base_amount": o.BaseAmount,
		"pay_amount": o.PayAmount, "actual_amount": o.ActualAmount, "note_code": o.NoteCode, "pay_url": g.srv.URL + "/pay/tok" + o.OrderID, "receive_uid": "10001", "receive_link": "https://app.binance.com/uni-qr/x",
		"matched_by": o.MatchedBy, "binance_order_id": o.BinanceOrderID, "payer_id": "legacy-" + o.PayerID, "counterparty_id": o.PayerID, "paid_at": o.PaidAt, "expires_at": o.ExpiresAt, "created_at": ms()}
}

// pay 模拟付款方转账：网关匹配后回调平台（HMAC 签名）。amount 为空 = 精确金额。
func (g *gwMock) pay(t *testing.T, mid, payer, amount string) {
	g.mu.Lock()
	o := g.byMid[mid]
	if o == nil {
		g.mu.Unlock()
		t.Fatalf("no gateway order %s", mid)
	}
	if amount == "" {
		amount = o.PayAmount
	}
	o.Status, o.ActualAmount, o.MatchedBy, o.BinanceOrderID, o.PayerID, o.PaidAt = "paid", amount, "amount", "452021922068888888", payer, ms()
	base, _ := parseAmountE8(o.BaseAmount, 8)
	act, _ := parseAmountE8(amount, 8)
	exact, _ := parseAmountE8(o.PayAmount, 8)
	if act != exact && act < base { // 与网关一致：唯一金额精确命中即 paid；备注/回填命中但不足基础金额才 underpaid
		o.Status, o.MatchedBy = "underpaid", "note"
	}
	body, _ := json.Marshal(map[string]any{"event": o.Status, "order_id": o.OrderID, "account_id": o.AccountID, "merchant_order_id": o.MerchantOrderID, "status": o.Status, "currency": o.Currency, "base_amount": o.BaseAmount,
		"pay_amount": o.PayAmount, "actual_amount": amount, "matched_by": o.MatchedBy, "binance_order_id": o.BinanceOrderID, "payer_id": "legacy-" + payer, "counterparty_id": payer, "paid_at": o.PaidAt, "timestamp": ms()})
	g.mu.Unlock()
	ts := strconv.FormatInt(ms(), 10)
	nonce := randHex(8)
	req, _ := http.NewRequest("POST", g.appURL+"/bpg/notify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BPG-Timestamp", ts)
	req.Header.Set("X-BPG-Nonce", nonce)
	req.Header.Set("X-BPG-Signature", bpaygate.SignCallback(g.key, ts, nonce, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("callback status %d", resp.StatusCode)
	}
}

// ---- 测试环境 ----

type env struct {
	t    *testing.T
	a    *App
	srv  *httptest.Server
	synd *syndMock
	gw   *gwMock
	dir  string
	prof *profMock
}

// profMock 粉丝数来源 mock：/{handle} 返回 FxTwitter 风格 JSON；/srv/... 返回 404 模拟官方接口不可用。
type profMock struct {
	mu        sync.Mutex
	followers map[string]int64
	retweets  map[string][]string // handle → 转发过的推文 ID
	views     map[string]int64    // 推文 ID → 浏览量
	srv       *httptest.Server
}

func newProf() *profMock {
	m := &profMock{followers: map[string]int64{}, retweets: map[string][]string{}, views: map[string]int64{}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/status/") {
			id := strings.TrimPrefix(r.URL.Path, "/status/")
			m.mu.Lock()
			v, ok := m.views[id]
			m.mu.Unlock()
			if !ok {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "tweet": map[string]any{"id": id, "views": v}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/srv/") {
			h := strings.TrimPrefix(r.URL.Path, "/srv/timeline-profile/screen-name/")
			m.mu.Lock()
			rts := m.retweets[h]
			m.mu.Unlock()
			if len(rts) == 0 {
				w.WriteHeader(404)
				return
			}
			var b strings.Builder
			b.WriteString(`{"props":{"pageProps":{"timeline":{"entries":[`)
			for i, id := range rts {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"content":{"tweet":{"id_str":"9` + id + `","entities":{"urls":[{"expanded_url":"https://x.com/a/status/1"}]},"retweeted_status":{"created_at":"x","entities":{"media":[{"id_str":"111"}]},"id_str":"` + id + `","user":{"id_str":"5","screen_name":"own"}}}}}`)
			}
			b.WriteString(`]}}}}`)
			w.Write([]byte(b.String()))
			return
		}
		h := strings.TrimPrefix(r.URL.Path, "/")
		m.mu.Lock()
		n, ok := m.followers[h]
		m.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "user": map[string]any{"followers": n}})
	}))
	return m
}

func (m *profMock) set(handle string, n int64) { m.mu.Lock(); m.followers[handle] = n; m.mu.Unlock() }
func (m *profMock) setViews(tweetID string, n int64) {
	m.mu.Lock()
	m.views[tweetID] = n
	m.mu.Unlock()
}

func (m *profMock) setRetweets(handle string, ids ...string) {
	m.mu.Lock()
	m.retweets[handle] = ids
	m.mu.Unlock()
}

type browser struct {
	e   *env
	c   *http.Client
	who string
	ip  string // 每个浏览器独立来源 IP（X-Real-IP），同 IP 段会被判自导自演
}

var browserSeq int

func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	dir := t.TempDir()
	synd := newSynd()
	gw := newGW("testkey-testkey")
	prof := newProf()
	cfgPath := filepath.Join(dir, "config.env")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf("LISTEN=127.0.0.1:0\nBASE_URL=http://test.local\nTRUST_PROXY=true\nTRUST_PROXY_HEADER=X-Real-IP\nDB_PATH=%s\nUPLOAD_DIR=%s\nBPG_URL=%s\nBPG_KEY=testkey-testkey\nX_TWEET_API=%s\nX_SYND_API=%s\nX_PROFILE_API=%s\nADMIN_HANDLES=admin\nCERT_FEE_ENABLED=true\nJURY_MIN_POOL=1000\nREVIEW_ENABLED=false\n%s",
		filepath.Join(dir, "t.db"), filepath.Join(dir, "up"), gw.srv.URL, synd.srv.URL, prof.srv.URL, prof.srv.URL, extra)), 0o644)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	gw.appURL = srv.URL
	e := &env{t: t, a: app, srv: srv, synd: synd, gw: gw, dir: dir, prof: prof}
	t.Cleanup(func() { srv.Close(); synd.srv.Close(); gw.srv.Close(); prof.srv.Close(); app.Close() })
	return e
}

func (e *env) browser(who string) *browser {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	browserSeq++
	return &browser{e: e, c: c, who: who, ip: fmt.Sprintf("10.%d.%d.1", (browserSeq/250)%250, browserSeq%250)}
}

func (b *browser) get(path string) (*http.Response, string) {
	req, _ := http.NewRequest("GET", b.e.srv.URL+path, nil)
	req.Header.Set("X-Real-IP", b.ip)
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	req, _ := http.NewRequest("POST", b.e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.e.srv.URL)
	req.Header.Set("X-Real-IP", b.ip)
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

var reCodeTLM = regexp.MustCompile(`TLM-[A-Z0-9]{6}`)

// register 走完整发帖注册流程。
func (b *browser) register(handle, xid string) *User {
	t := b.e.t
	t.Helper()
	_, body := b.get("/register")
	code := reCodeTLM.FindString(body)
	if code == "" {
		t.Fatalf("[%s] no verify code in page", handle)
	}
	tid := fmt.Sprintf("9%s%d", xid, ms()%100000)
	b.e.synd.add(mockTweet{ID: tid, Text: "我在推了么注册 " + code + " test.local", UserID: xid, Handle: handle, Name: "User " + handle})
	resp, body := b.post("/register/verify", url.Values{"code": {code}, "tweet_url": {"https://x.com/" + handle + "/status/" + tid}})
	if resp.StatusCode != 200 || !strings.Contains(body, "设置密码") {
		t.Fatalf("[%s] verify failed: %d %s", handle, resp.StatusCode, snippet(body))
	}
	resp, _ = b.post("/register", url.Values{"code": {code}, "password": {"password123"}, "password2": {"password123"}})
	if resp.StatusCode != 302 {
		t.Fatalf("[%s] register status %d", handle, resp.StatusCode)
	}
	u, _ := b.e.a.st.GetUserByHandle(handle)
	if u == nil {
		t.Fatalf("[%s] user not created", handle)
	}
	return u
}

func snippet(s string) string {
	if i := strings.Index(s, "notice bad"); i >= 0 {
		end := i + 300
		if end > len(s) {
			end = len(s)
		}
		return s[i:end]
	}
	if len(s) > 400 {
		return s[:400]
	}
	return s
}

func (b *browser) setUID(uid string) {
	resp, _ := b.post("/me/pay/uid", url.Values{"uid": {uid}, "uid2": {uid}})
	if resp.StatusCode != 302 {
		b.e.t.Fatalf("set uid %d", resp.StatusCode)
	}
}

func (b *browser) bindKey(uid string) {
	resp, _ := b.post("/me/pay/bind", url.Values{"api_key": {"goodkey-" + uid + "-xxxxxxxx"}, "api_secret": {"secret-secret-secret"}, "uid": {uid}})
	if resp.StatusCode != 302 {
		b.e.t.Fatalf("bind %d", resp.StatusCode)
	}
}

func (b *browser) certify(payer string) {
	t := b.e.t
	resp, _ := b.post("/me/cert", nil)
	if resp.StatusCode != 302 {
		t.Fatalf("cert %d", resp.StatusCode)
	}
	u, _ := b.e.a.st.GetUserByLogin(b.who)
	p, _ := b.e.a.st.ActiveCertPayment(u.ID)
	if p == nil {
		t.Fatal("no cert payment")
	}
	b.e.gw.pay(t, p.MerchantOrderID, payer, "")
	u, _ = b.e.a.st.GetUserByID(u.ID)
	if u.CertPaidAt == 0 || u.PayerID != payer {
		t.Fatalf("cert not applied: %+v", u)
	}
}

func (b *browser) publish(form url.Values) *Task {
	t := b.e.t
	resp, body := b.post("/new", form)
	if resp.StatusCode != 302 {
		t.Fatalf("publish: %d %s", resp.StatusCode, snippet(body))
	}
	loc := resp.Header.Get("Location")
	task, _ := b.e.a.st.GetTaskByCode(strings.TrimPrefix(loc, "/t/"))
	if task == nil {
		t.Fatalf("task not found from %s", loc)
	}
	return task
}

func (b *browser) claim(task *Task) *Submission {
	t := b.e.t
	resp, _ := b.post("/t/"+task.Code+"/claim", nil)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || !strings.HasPrefix(loc, "/s/") {
		_, body := b.get(loc)
		t.Fatalf("claim failed: %d -> %s (%s)", resp.StatusCode, loc, snippet(body))
	}
	x, _ := b.e.a.st.GetSubByCode(strings.TrimPrefix(loc, "/s/"))
	if x == nil {
		t.Fatal("submission missing")
	}
	return x
}

func (b *browser) submitTweet(x *Submission, tid string) *Submission {
	resp, _ := b.post("/s/"+x.Code+"/submit", url.Values{"tweet_url": {"https://x.com/u/status/" + tid}})
	if resp.StatusCode != 302 {
		_, body := b.get(x.Path())
		b.e.t.Fatalf("submit %d %s", resp.StatusCode, snippet(body))
	}
	nx, _ := b.e.a.st.GetSubByID(x.ID)
	return nx
}

func taskForm(over url.Values) url.Values {
	f := url.Values{"title": {"测试任务"}, "content1": {"推了么测试文案 https://example.com/p"}, "match_mode": {"exact"}, "reward": {"1"}, "slots": {"3"}, "claim_ttl": {"120"}, "retention": {"0"}, "pay_window": {"48"}, "deadline": {"7"}, "min_days": {"0"}, "ad_tag": {"1"}}
	for k, v := range over {
		f[k] = v
	}
	return f
}

// ---- 主线 1：注册 → 认证 → 发布 → 接单 → 验证 → 网关付款 → 完成 ----

func TestFullFlowGateway(t *testing.T) {
	e := newEnv(t, "")
	alice := e.browser("alice")
	bob := e.browser("bob")
	au := alice.register("alice", "1001")
	bu := bob.register("bob", "1002")
	// 重复注册同一 X 应被拒
	dup := e.browser("dup")
	_, body := dup.get("/register")
	code := reCodeTLM.FindString(body)
	e.synd.add(mockTweet{ID: "77", Text: code, UserID: "1001", Handle: "alice"})
	resp, body := dup.post("/register/verify", url.Values{"code": {code}, "tweet_url": {"https://x.com/alice/status/77"}})
	if resp.StatusCode != 400 || !strings.Contains(body, "已经注册过") {
		t.Fatalf("dup register should fail: %d", resp.StatusCode)
	}

	alice.setUID("20001")
	// 未认证不能发布
	if resp, body := alice.post("/new", taskForm(nil)); resp.StatusCode != 400 || !strings.Contains(body, "认证付款") {
		t.Fatalf("publish before cert should fail: %d", resp.StatusCode)
	}
	alice.certify("payer-A")
	bob.bindKey("20002")
	if p, _ := e.a.st.GetPayProfile(bu.ID); !p.Gateway() {
		t.Fatal("bob should be gateway mode")
	}

	task := alice.publish(taskForm(nil))
	if len(task.Contents) != 1 || !strings.HasSuffix(task.Contents[0], "#广告") {
		t.Fatalf("ad tag not appended: %v", task.Contents)
	}
	// 敞口：新手 20U，单价 10 × 3 名额 = 30 应被拒
	if resp, body := alice.post("/new", taskForm(url.Values{"reward": {"10"}})); resp.StatusCode != 400 || !strings.Contains(body, "敞口") {
		t.Fatalf("exposure cap should block: %d", resp.StatusCode)
	}
	// 发布方不能接自己的任务
	if resp, _ := alice.post("/t/"+task.Code+"/claim", nil); resp.Header.Get("Location") != task.Path() {
		t.Fatal("owner claim should bounce back")
	}
	x := bob.claim(task)
	if x.Status != SClaimed || x.WorkerID != bu.ID {
		t.Fatalf("claim state %+v", x)
	}
	// 错误推文：别人的账号
	e.synd.add(mockTweet{ID: "3001", Text: task.Contents[0], UserID: "1001", Handle: "alice"})
	x = bob.submitTweet(x, "3001")
	if x.Status != SClaimed || !strings.Contains(x.LastError, "不是你的账号") {
		t.Fatalf("author check failed: %s / %s", x.Status, x.LastError)
	}
	// 错误推文：正文不符
	e.synd.add(mockTweet{ID: "3002", Text: "随便写的", UserID: "1002", Handle: "bob"})
	x = bob.submitTweet(x, "3002")
	if x.Status != SClaimed || !strings.Contains(x.LastError, "不一致") {
		t.Fatalf("content check failed: %s / %s", x.Status, x.LastError)
	}
	// 正确推文（第 3 次机会）
	e.synd.add(mockTweet{ID: "3003", Text: task.Contents[0], UserID: "1002", Handle: "bob"})
	x = bob.submitTweet(x, "3003")
	if x.Status != SPayable {
		t.Fatalf("expected payable (retention 0), got %s / %s", x.Status, x.LastError)
	}
	if e.a.st.count(`SELECT COUNT(*) FROM tweet_cache WHERE tweet_id='3003'`) == 0 {
		t.Fatal("snapshot not stored")
	}
	// 发布方付款：开网关订单 → 模拟到账 → 回调
	if resp, _ := alice.post("/s/"+x.Code+"/pay/order", nil); resp.StatusCode != 302 {
		t.Fatalf("pay order %d", resp.StatusCode)
	}
	p, _ := e.a.st.ActivePayment(x.ID, "gateway")
	if p == nil || !strings.HasPrefix(p.MerchantOrderID, "TLM-"+x.Code) {
		t.Fatalf("payment %+v", p)
	}
	// 记录页对第三方不可见
	if resp, _ := dup.get(x.Path()); resp.StatusCode != 302 && resp.StatusCode != 404 {
		t.Fatalf("stranger should not see record: %d", resp.StatusCode)
	}
	e.gw.pay(t, p.MerchantOrderID, "payer-A", "")
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || x.ConfirmMethod != "gateway" || x.PaidAmountE8 != 100010000 {
		t.Fatalf("after pay: %s %s %d", x.Status, x.ConfirmMethod, x.PaidAmountE8)
	}
	st := e.a.st.PubStats(au.ID)
	if st.PaidCredit != 1 || st.Paid != 1 {
		t.Fatalf("stats %+v", st)
	}
	// 重复回调幂等
	e.gw.pay(t, p.MerchantOrderID, "payer-A", "")
	if e.a.st.count(`SELECT COUNT(*) FROM audit_log WHERE action='sub.paid'`) != 1 {
		t.Fatal("duplicate callback should be idempotent")
	}
	// 状态接口
	if resp, body := bob.get("/s/" + x.Code + "/status"); resp.StatusCode != 200 || !strings.Contains(body, `"status":"paid"`) {
		t.Fatalf("status json: %d %s", resp.StatusCode, body)
	}
	// 页面渲染无错
	for _, path := range []string{"/", task.Path(), x.Path(), "/me", "/me?tab=tasks", "/me?tab=subs", "/me/pay", "/me/cert", "/u/alice", "/u/bob", "/rules", "/blacklist", "/court", "/me/notifications", "/records"} {
		resp, body := alice.get(path)
		if resp.StatusCode != 200 || strings.Contains(body, "页面渲染失败") {
			t.Fatalf("page %s: %d", path, resp.StatusCode)
		}
		if strings.Contains(body, "[0x") || strings.Contains(body, "%!") {
			t.Fatalf("page %s leaks raw Go values (field shadowing?)", path)
		}
	}
}

// ---- 主线 2：手动模式 → 标记 → 确认；逾期 → 冻结 → 举报 → 管理员上榜 ----

func TestManualOverdueBlacklist(t *testing.T) {
	e := newEnv(t, "")
	alice, carol, admin := e.browser("alice"), e.browser("carol"), e.browser("admin")
	au := alice.register("alice", "2001")
	cu := carol.register("carol", "2002")
	admin.register("admin", "2003")
	alice.setUID("30001")
	alice.certify("payer-A2")
	carol.setUID("30002")
	task := alice.publish(taskForm(url.Values{"slots": {"2"}}))

	x := carol.claim(task)
	e.synd.add(mockTweet{ID: "4001", Text: task.Contents[0], UserID: "2002", Handle: "carol"})
	x = carol.submitTweet(x, "4001")
	if x.Status != SPayable {
		t.Fatalf("payable expected: %s %s", x.Status, x.LastError)
	}
	// 手动标记：订单号格式校验
	if resp, body := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {"123"}}); resp.StatusCode != 400 || !strings.Contains(body, "18 位") {
		t.Fatal("bad order id should be rejected")
	}
	if resp, _ := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068888801"}}); resp.StatusCode != 302 {
		t.Fatal("mark failed")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SAwait {
		t.Fatalf("await expected: %s", x.Status)
	}
	// 24 小时内不会自动完成
	e.a.runJobs()
	if nx, _ := e.a.st.GetSubByID(x.ID); nx.Status != SAwait {
		t.Fatalf("must stay awaiting within 24h: %s", nx.Status)
	}
	_ = cu
	// 确认收到 → 完成，解锁
	if resp, _ := carol.post("/s/"+x.Code+"/confirm", nil); resp.StatusCode != 302 {
		t.Fatal("confirm failed")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || x.ConfirmMethod != "manual" {
		t.Fatalf("manual paid expected: %s", x.Status)
	}
	if st := e.a.st.PubStats(au.ID); st.PaidCredit != 1 || st.PaidGateway != 0 {
		t.Fatalf("manual confirmation counts as credit but not as gateway: %+v", st)
	}

	// 第二条：另一个接单方 frank，走到逾期
	frank := e.browser("frank")
	fu := frank.register("frank", "2004")
	frank.setUID("30004")
	x2 := frank.claim(task)
	e.synd.add(mockTweet{ID: "4002", Text: task.Contents[0], UserID: "2004", Handle: "frank"})
	x2 = frank.submitTweet(x2, "4002")
	if x2.Status != SPayable {
		t.Fatalf("payable expected: %s", x2.Status)
	}
	e.a.st.db.Exec(`UPDATE submissions SET pay_deadline_at=? WHERE id=?`, ms()-1000, x2.ID)
	e.a.runJobs()
	x2, _ = e.a.st.GetSubByID(x2.ID)
	if x2.Status != SOverdue {
		t.Fatalf("overdue expected: %s", x2.Status)
	}
	if e.a.st.PublisherFrozen(au.ID) == 0 {
		t.Fatal("publisher should be frozen")
	}
	tk, _ := e.a.st.GetTaskByID(task.ID)
	if tk.Status != "paused" || !tk.PausedByFreeze {
		t.Fatalf("task should be auto-paused: %s", tk.Status)
	}
	if resp, body := alice.post("/new", taskForm(nil)); resp.StatusCode != 400 || !strings.Contains(body, "逾期") {
		t.Fatal("frozen publisher should not publish")
	}
	// 假标记不能解冻
	if resp, _ := alice.post("/s/"+x2.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068888802"}}); resp.StatusCode != 302 {
		t.Fatal("late mark failed")
	}
	if e.a.st.PublisherFrozen(au.ID) == 0 {
		t.Fatal("marking must not lift the freeze")
	}
	// 接单方申诉 B（未收到）
	if resp, _ := frank.post("/s/"+x2.Code+"/dispute", url.Values{"type": {"B"}, "text": {"没有收到这笔钱，流水里没有"}}); resp.StatusCode != 302 {
		t.Fatal("dispute B failed")
	}
	d, _ := e.a.st.OpenDisputeForSub(x2.ID)
	if d == nil || d.Type != "B" || d.Status != "evidence" {
		t.Fatalf("dispute %+v", d)
	}
	_ = fu
	// 管理员交小法庭（池子不够也允许手动开庭）→ 页面渲染 → 接管
	if resp, _ := admin.post("/admin/dispute/"+d.Code+"/tojury", nil); resp.StatusCode != 302 {
		t.Fatal("tojury failed")
	}
	d, _ = e.a.st.GetDisputeByID(d.ID)
	if d.Status != "jury" || d.JuryCaseID == 0 {
		t.Fatalf("dispute should be in jury: %s %d", d.Status, d.JuryCaseID)
	}
	for _, path := range []string{"/court", fmt.Sprintf("/court/%d", d.JuryCaseID), d.Path(), "/admin"} {
		if resp, body := admin.get(path); resp.StatusCode != 200 || strings.Contains(body, "页面渲染失败") {
			t.Fatalf("page %s: %d", path, resp.StatusCode)
		}
	}
	// 非陪审员投票应被拒
	if resp, _ := carol.post(fmt.Sprintf("/court/%d/vote", d.JuryCaseID), url.Values{"vote": {"for"}}); resp.StatusCode != 403 {
		t.Fatalf("non-juror vote should be 403, got %d", resp.StatusCode)
	}
	if resp, _ := admin.post("/admin/dispute/"+d.Code+"/takeover", nil); resp.StatusCode != 302 {
		t.Fatal("takeover failed")
	}
	if c, _ := e.a.st.GetJuryCase(d.JuryCaseID); c.Status != "escalated" {
		t.Fatalf("case should be escalated: %s", c.Status)
	}
	// 管理员裁决：虚假标记 → 发布方上黑名单
	if resp, body := admin.post("/admin/dispute/"+d.Code+"/resolve", url.Values{"resolution": {"b_fake"}, "note": {"核实未付款"}}); resp.StatusCode != 302 {
		t.Fatalf("admin resolve: %d %s", resp.StatusCode, snippet(body))
	}
	au, _ = e.a.st.GetUserByID(au.ID)
	if au.Status != "blacklisted" {
		t.Fatalf("alice should be blacklisted: %s", au.Status)
	}
	x2, _ = e.a.st.GetSubByID(x2.ID)
	if x2.Status != SDefault {
		t.Fatalf("record should be defaulted: %s", x2.Status)
	}
	bl, _ := e.a.st.ActiveBlacklist(au.ID)
	if bl == nil || bl.AmountOwedE8 != 100000000 || bl.XID != "2001" {
		t.Fatalf("blacklist entry %+v", bl)
	}
	if resp, body := carol.get("/blacklist"); resp.StatusCode != 200 || !strings.Contains(body, "@alice") || !strings.Contains(body, "x.com/i/user/2001") {
		t.Fatal("blacklist board should list alice by X id link")
	}
	// 黑名单用户不能再注册同一 X（换号时 X ID 阻断）
	if !e.a.st.BlacklistedIdentity("2001", "", "") {
		t.Fatal("identity should be blacklisted")
	}
	// 补付：网关到账 → repaid 标记
	// 申诉页可见性
	if resp, _ := frank.get(d.Path()); resp.StatusCode != 200 {
		t.Fatal("party should see dispute")
	}
	_ = cu
	stranger := e.browser("s")
	stranger.register("stranger", "2009")
	if resp, _ := stranger.get(d.Path()); resp.StatusCode != 404 {
		t.Fatalf("stranger should not see dispute: %d", resp.StatusCode)
	}
	if resp, body := admin.get("/admin"); resp.StatusCode != 200 || strings.Contains(body, "页面渲染失败") {
		t.Fatal("admin page")
	}
	if resp, body := admin.get("/admin/users?q=alice"); resp.StatusCode != 200 || !strings.Contains(body, "blacklisted") {
		t.Fatal("admin user page")
	}
}

// ---- 留存复检：通过 / 删帖作废（3 次确认） ----

func TestRecheck(t *testing.T) {
	e := newEnv(t, "")
	alice, dave := e.browser("alice"), e.browser("dave")
	alice.register("alice", "5001")
	du := dave.register("dave", "5002")
	alice.setUID("40001")
	alice.certify("payer-A5")
	dave.setUID("40002")
	task := alice.publish(taskForm(url.Values{"retention": {"24"}, "slots": {"3"}}))
	x := dave.claim(task)
	e.synd.add(mockTweet{ID: "6001", Text: task.Contents[0], UserID: "5002", Handle: "dave"})
	x = dave.submitTweet(x, "6001")
	if x.Status != SVerified || x.RecheckDueAt == 0 {
		t.Fatalf("verified expected: %s", x.Status)
	}
	// 到期复检通过
	e.a.st.db.Exec(`UPDATE submissions SET recheck_due_at=? WHERE id=?`, ms()-1, x.ID)
	e.a.runJobs()
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPayable || x.PayDeadlineAt == 0 {
		t.Fatalf("payable after recheck expected: %s", x.Status)
	}
	// 第二条（ed）：改文 → 立即作废
	ed := e.browser("ed")
	eu := ed.register("ed", "5003")
	ed.setUID("40003")
	x2 := ed.claim(task)
	e.synd.add(mockTweet{ID: "6002", Text: task.Contents[0], UserID: "5003", Handle: "ed"})
	x2 = ed.submitTweet(x2, "6002")
	e.synd.add(mockTweet{ID: "6002", Text: "改掉了", UserID: "5003", Handle: "ed"})
	e.a.st.db.Exec(`UPDATE submissions SET recheck_due_at=? WHERE id=?`, ms()-1, x2.ID)
	e.a.runJobs()
	x2, _ = e.a.st.GetSubByID(x2.ID)
	if x2.Status != SVoid || !strings.Contains(x2.VoidReason, "留存不达标") {
		t.Fatalf("void expected: %s %s", x2.Status, x2.VoidReason)
	}
	// 第三条（ed 再接：作废释放了名额，但 24 小时内不能再接同一任务）
	tk, _ := e.a.st.GetTaskByID(task.ID)
	if tk.Left() != 2 {
		t.Fatalf("void should release slot, left=%d", tk.Left())
	}
	if resp, _ := ed.post("/t/"+task.Code+"/claim", nil); resp.Header.Get("Location") != task.Path() {
		t.Fatal("re-claim within 24h should be refused")
	}
	e.a.st.db.Exec(`UPDATE submissions SET updated_at=? WHERE id=?`, ms()-25*hourMs, x2.ID)
	x3 := ed.claim(task)
	e.synd.add(mockTweet{ID: "6003", Text: task.Contents[0], UserID: "5003", Handle: "ed"})
	x3 = ed.submitTweet(x3, "6003")
	e.synd.del("6003")
	for i := 1; i <= 3; i++ {
		e.a.st.db.Exec(`UPDATE submissions SET recheck_due_at=? WHERE id=?`, ms()-1, x3.ID)
		e.a.runJobs()
		x3, _ = e.a.st.GetSubByID(x3.ID)
		if i < 3 && x3.Status != SVerified {
			t.Fatalf("round %d: should still be verified, got %s", i, x3.Status)
		}
	}
	if x3.Status != SVoid {
		t.Fatalf("void after 3 unreadable expected: %s (%s)", x3.Status, x3.RecheckFlag)
	}
	if st := e.a.st.WorkerStats(eu.ID); st.Void30d != 2 {
		t.Fatalf("void count %d", st.Void30d)
	}
	_ = du
}

// ---- 少付 → 补差；网关 Key 绑定失败提示 ----

func TestUnderpaidAndBindError(t *testing.T) {
	e := newEnv(t, "")
	alice, erin := e.browser("alice"), e.browser("erin")
	alice.register("alice", "7001")
	erin.register("erin", "7002")
	alice.setUID("50001")
	alice.certify("payer-A7")
	// 绑定失败：网关返回币安原因
	if resp, _ := erin.post("/me/pay/bind", url.Values{"api_key": {"badkey-badkey-badkey"}, "api_secret": {"secret-secret-secret"}, "uid": {"50002"}}); resp.StatusCode != 302 {
		t.Fatal("bind should redirect with flash")
	}
	if p, _ := e.a.st.GetPayProfile(2); p != nil && p.Gateway() {
		t.Fatal("bad key must not bind")
	}
	erin.bindKey("50002")
	task := alice.publish(taskForm(url.Values{"reward": {"2"}}))
	x := erin.claim(task)
	e.synd.add(mockTweet{ID: "8001", Text: task.Contents[0], UserID: "7002", Handle: "erin"})
	x = erin.submitTweet(x, "8001")
	alice.post("/s/"+x.Code+"/pay/order", nil)
	p, _ := e.a.st.ActivePayment(x.ID, "gateway")
	e.gw.pay(t, p.MerchantOrderID, "payer-A7", "1.5")
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SAwait || x.UnderpaidE8 != 150000000 {
		t.Fatalf("underpaid expected: %s %d", x.Status, x.UnderpaidE8)
	}
	// 要求补差 → 发布方开补差订单（0.5）→ 到账 → 完成
	erin.post("/s/"+x.Code+"/underpaid", url.Values{"action": {"topup"}})
	alice.post("/s/"+x.Code+"/pay/order", nil)
	tp, _ := e.a.st.ActivePayment(x.ID, "topup")
	if tp == nil || tp.BaseE8 != 50000000 {
		t.Fatalf("topup order %+v", tp)
	}
	e.gw.pay(t, tp.MerchantOrderID, "payer-A7", "")
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || x.PaidAmountE8 < 200000000 {
		t.Fatalf("paid after topup expected: %s %d", x.Status, x.PaidAmountE8)
	}
}

// ---- 审查修复回归：管理员绑定 X ID、A 类举报后登记付款、D 类暂停付款倒计时、补差不锁定 ----

func TestAdminBoundToXID(t *testing.T) {
	e := newEnv(t, "")
	admin := e.browser("admin")
	au := admin.register("admin", "9001")
	if !e.a.isAdmin(au) {
		t.Fatal("first admin handle should bind")
	}
	// 模拟：管理员在 X 改名后旧名被别人注册（CreateUser 会把老账号 handle_lower 改成 xid:…）
	imp := e.browser("imp")
	iu := imp.register("admin", "9002")
	old, _ := e.a.st.GetUserByID(au.ID)
	if old.HandleLower != "xid:9001" || !old.HandleStale {
		t.Fatalf("old account should be marked stale: %+v", old)
	}
	if e.a.isAdmin(iu) {
		t.Fatal("impostor with the admin handle must not be admin")
	}
	if !e.a.isAdmin(old) {
		t.Fatal("original admin must keep admin by X id")
	}
	if u, _ := e.a.st.GetUserByLogin("9001"); u == nil || u.ID != au.ID {
		t.Fatal("stale account should log in by X id")
	}
}

func TestReportThenMarkAndDisputePause(t *testing.T) {
	e := newEnv(t, "")
	alice, gina := e.browser("alice"), e.browser("gina")
	au := alice.register("alice", "9101")
	gu := gina.register("gina", "9102")
	alice.setUID("60001")
	alice.certify("payer-A9")
	gina.setUID("60002")
	task := alice.publish(taskForm(url.Values{"slots": {"3"}}))
	// 记录 1：逾期 → 举报 A（宽限 24h）→ 发布方登记付款 → A 自动结案 → 接单方确认
	x := gina.claim(task)
	e.synd.add(mockTweet{ID: "9201", Text: task.Contents[0], UserID: "9102", Handle: "gina"})
	x = gina.submitTweet(x, "9201")
	e.a.st.db.Exec(`UPDATE submissions SET pay_deadline_at=? WHERE id=?`, ms()-1, x.ID)
	e.a.runJobs()
	if resp, _ := gina.post("/s/"+x.Code+"/dispute", url.Values{"type": {"A"}, "text": {"逾期未付，请处理"}}); resp.StatusCode != 302 {
		t.Fatal("report A failed")
	}
	d, _ := e.a.st.OpenDisputeForSub(x.ID)
	if d == nil || d.Type != "A" || d.EvidenceUntil > ms()+25*hourMs {
		t.Fatalf("A dispute should use 24h grace: %+v", d)
	}
	if e.a.st.PublisherFrozen(au.ID) == 0 {
		t.Fatal("reported publisher must stay frozen")
	}
	if resp, _ := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068889001"}}); resp.StatusCode != 302 {
		t.Fatal("mark during A dispute should be allowed")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	d, _ = e.a.st.GetDisputeByID(d.ID)
	if x.Status != SAwait || d.Status != "resolved" || !x.Late {
		t.Fatalf("after mark: sub=%s late=%v dispute=%s", x.Status, x.Late, d.Status)
	}
	if e.a.st.PublisherFrozen(au.ID) == 0 {
		t.Fatal("marking must not unfreeze (overdue_at>0 and not yet confirmed)")
	}
	if resp, _ := gina.post("/s/"+x.Code+"/confirm", nil); resp.StatusCode != 302 {
		t.Fatal("confirm failed")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || e.a.st.PublisherFrozen(au.ID) != 0 {
		t.Fatalf("confirm should complete and unfreeze: %s", x.Status)
	}
	// 记录 2（另一个接单方）：复检未确认 → 发布方发起 D → 倒计时暂停 → 不成立 → 顺延
	hank := e.browser("hank")
	hank.register("hank", "9103")
	hank.setUID("60003")
	x2 := hank.claim(task)
	e.synd.add(mockTweet{ID: "9202", Text: task.Contents[0], UserID: "9103", Handle: "hank"})
	x2 = hank.submitTweet(x2, "9202")
	e.a.st.db.Exec(`UPDATE submissions SET recheck_flag='复检未确认：测试', pay_deadline_at=? WHERE id=?`, ms()+hourMs, x2.ID)
	if resp, _ := alice.post("/s/"+x2.Code+"/dispute", url.Values{"type": {"D"}, "text": {"推文看起来被改过"}}); resp.StatusCode != 302 {
		t.Fatal("open D failed")
	}
	e.a.st.db.Exec(`UPDATE submissions SET pay_deadline_at=? WHERE id=?`, ms()-1, x2.ID)
	e.a.runJobs()
	x2, _ = e.a.st.GetSubByID(x2.ID)
	if x2.Status != SPayable {
		t.Fatalf("open D dispute must pause the pay deadline, got %s", x2.Status)
	}
	d2, _ := e.a.st.OpenDisputeForSub(x2.ID)
	admin := e.browser("admin")
	admin.register("admin", "9104")
	if resp, _ := admin.post("/admin/dispute/"+d2.Code+"/resolve", url.Values{"resolution": {"rejected"}, "note": {"推文合格"}}); resp.StatusCode != 302 {
		t.Fatal("resolve D failed")
	}
	x2, _ = e.a.st.GetSubByID(x2.ID)
	if x2.Status != SPayable || x2.PayDeadlineAt < ms()+23*hourMs {
		t.Fatalf("rejected D should extend the deadline: %s %d", x2.Status, x2.PayDeadlineAt-ms())
	}
	_ = gu
}

func TestTopupRequestDoesNotLock(t *testing.T) {
	e := newEnv(t, "")
	alice, ivy := e.browser("alice"), e.browser("ivy")
	alice.register("alice", "9301")
	iu := ivy.register("ivy", "9302")
	alice.setUID("70001")
	alice.certify("payer-A93")
	ivy.bindKey("70002")
	task := alice.publish(taskForm(url.Values{"reward": {"2"}}))
	x := ivy.claim(task)
	e.synd.add(mockTweet{ID: "9401", Text: task.Contents[0], UserID: "9302", Handle: "ivy"})
	x = ivy.submitTweet(x, "9401")
	alice.post("/s/"+x.Code+"/pay/order", nil)
	p, _ := e.a.st.ActivePayment(x.ID, "gateway")
	e.gw.pay(t, p.MerchantOrderID, "payer-A93", "1.2")
	ivy.post("/s/"+x.Code+"/underpaid", url.Values{"action": {"topup"}})
	e.a.st.db.Exec(`UPDATE submissions SET marked_paid_at=? WHERE id=?`, ms()-30*hourMs, x.ID)
	e.a.runJobs()
	if nx, _ := e.a.st.GetSubByID(x.ID); nx.Status != SAwait {
		t.Fatalf("waiting for top-up must not auto-complete: %s", nx.Status)
	}
	_ = iu
	// 发布方手动登记补差订单号 → 接单方确认按全额完成
	if resp, _ := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068889301"}}); resp.StatusCode != 302 {
		t.Fatal("manual top-up mark failed")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.TopupMarkedAt == 0 || x.Status != SAwait {
		t.Fatalf("top-up mark state: %s %d", x.Status, x.TopupMarkedAt)
	}
	ivy.post("/s/"+x.Code+"/confirm", nil)
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || x.PaidAmountE8 != 200000000 {
		t.Fatalf("confirm after top-up mark should pay full: %s %d", x.Status, x.PaidAmountE8)
	}
}

// ---- 第二轮审查回归：裁决路径不重复记警告、补差订单号独立、认证少付不算认证、改名后的主页地址 ----

func TestResolutionPathsWarnOnce(t *testing.T) {
	e := newEnv(t, "")
	alice, kim, admin := e.browser("alice"), e.browser("kim"), e.browser("admin")
	au := alice.register("alice", "9501")
	kim.register("kim", "9502")
	admin.register("admin", "9503")
	alice.setUID("80001")
	alice.certify("payer-A95")
	kim.setUID("80002")
	task := alice.publish(taskForm(url.Values{"slots": {"3"}}))
	warnings := func() int64 {
		return e.a.st.count(`SELECT COUNT(*) FROM audit_log WHERE action='user.warning' AND target_type='user' AND target_id=?`, au.ID)
	}
	openB := func(tid string) (*Submission, *Dispute) {
		x := kim.claim(task)
		e.synd.add(mockTweet{ID: tid, Text: task.Contents[0], UserID: "9502", Handle: "kim"})
		x = kim.submitTweet(x, tid)
		if resp, _ := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {"4520219220688" + tid}}); resp.StatusCode != 302 {
			t.Fatal("mark failed")
		}
		if resp, _ := kim.post("/s/"+x.Code+"/dispute", url.Values{"type": {"B"}, "text": {"没有收到这笔款项"}}); resp.StatusCode != 302 {
			t.Fatal("dispute failed")
		}
		d, _ := e.a.st.OpenDisputeForSub(x.ID)
		return x, d
	}
	// b_wrong_uid：发布方无过错，不能记警告
	x1, d1 := openB("95001")
	if resp, _ := admin.post("/admin/dispute/"+d1.Code+"/resolve", url.Values{"resolution": {"b_wrong_uid"}, "note": {"UID 填错"}}); resp.StatusCode != 302 {
		t.Fatal("resolve wrong_uid failed")
	}
	x1, _ = e.a.st.GetSubByID(x1.ID)
	if x1.Status != SPaid || warnings() != 0 {
		t.Fatalf("wrong_uid: status=%s warnings=%d", x1.Status, warnings())
	}
	// b_late：恰好一次警告，不上黑名单
	kim2 := e.browser("kim2")
	kim2.register("kim2", "9504")
	kim2.setUID("80004")
	x2 := kim2.claim(task)
	e.synd.add(mockTweet{ID: "95002", Text: task.Contents[0], UserID: "9504", Handle: "kim2"})
	x2 = kim2.submitTweet(x2, "95002")
	alice.post("/s/"+x2.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068895002"}})
	kim2.post("/s/"+x2.Code+"/dispute", url.Values{"type": {"B"}, "text": {"没有收到这笔款项"}})
	d2, _ := e.a.st.OpenDisputeForSub(x2.ID)
	if resp, _ := admin.post("/admin/dispute/"+d2.Code+"/resolve", url.Values{"resolution": {"b_late"}, "note": {"申诉后才到账"}}); resp.StatusCode != 302 {
		t.Fatal("resolve b_late failed")
	}
	au, _ = e.a.st.GetUserByID(au.ID)
	x2, _ = e.a.st.GetSubByID(x2.ID)
	if x2.Status != SPaid || !x2.Late || warnings() != 1 || au.Status != "active" {
		t.Fatalf("b_late: status=%s late=%v warnings=%d user=%s", x2.Status, x2.Late, warnings(), au.Status)
	}
	// 自然路径：接单方申诉后自己确认收到 → 也是一次警告（累计 2 → 上榜），验证自然路径仍记警告
	kim3 := e.browser("kim3")
	kim3.register("kim3", "9505")
	kim3.setUID("80005")
	x3 := kim3.claim(task)
	e.synd.add(mockTweet{ID: "95003", Text: task.Contents[0], UserID: "9505", Handle: "kim3"})
	x3 = kim3.submitTweet(x3, "95003")
	alice.post("/s/"+x3.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068895003"}})
	kim3.post("/s/"+x3.Code+"/dispute", url.Values{"type": {"B"}, "text": {"没有收到这笔款项"}})
	e.a.st.RestoreFromDispute(x3.ID, SAwait) // 模拟：申诉中款到了，接单方回来确认
	kim3.post("/s/"+x3.Code+"/confirm", nil)
	au, _ = e.a.st.GetUserByID(au.ID)
	if warnings() != 2 || au.Status != "blacklisted" {
		t.Fatalf("second natural warning should blacklist: warnings=%d user=%s", warnings(), au.Status)
	}
}

func TestTopupOrderKeptSeparately(t *testing.T) {
	e := newEnv(t, "")
	alice, lee := e.browser("alice"), e.browser("lee")
	alice.register("alice", "9601")
	lee.register("lee", "9602")
	alice.setUID("81001")
	alice.certify("payer-A96")
	lee.bindKey("81002")
	task := alice.publish(taskForm(url.Values{"reward": {"2"}}))
	x := lee.claim(task)
	e.synd.add(mockTweet{ID: "96001", Text: task.Contents[0], UserID: "9602", Handle: "lee"})
	x = lee.submitTweet(x, "96001")
	alice.post("/s/"+x.Code+"/pay/order", nil)
	p, _ := e.a.st.ActivePayment(x.ID, "gateway")
	e.gw.pay(t, p.MerchantOrderID, "payer-A96", "1.2")
	x, _ = e.a.st.GetSubByID(x.ID)
	first := x.MarkedPaidAt
	if resp, _ := lee.post("/s/"+x.Code+"/underpaid", url.Values{"action": {"topup"}}); resp.StatusCode != 302 {
		t.Fatal("topup request failed")
	}
	// 重复要求补差应被拒绝（flash），状态不变
	lee.post("/s/"+x.Code+"/underpaid", url.Values{"action": {"topup"}})
	if resp, _ := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068896001"}}); resp.StatusCode != 302 {
		t.Fatal("topup mark failed")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.TopupOrderID != "452021922068896001" || x.MarkedPaidAt != first || x.MarkedOrderID != "" || x.TopupRequested != 0 {
		t.Fatalf("topup fields: %+v", x)
	}
	// 接单方页面应出现按全额完成的按钮
	if _, body := lee.get(x.Path()); !strings.Contains(body, "已收到补差，按全额完成") {
		t.Fatal("worker page should offer full-amount confirm after top-up mark")
	}
	lee.post("/s/"+x.Code+"/confirm", nil)
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || x.PaidAmountE8 != 200000000 {
		t.Fatalf("after confirm: %s %d", x.Status, x.PaidAmountE8)
	}
}

func TestCertUnderpaidNotCertified(t *testing.T) {
	e := newEnv(t, "")
	alice := e.browser("alice")
	au := alice.register("alice", "9701")
	alice.setUID("82001")
	alice.post("/me/cert", nil)
	p, _ := e.a.st.ActiveCertPayment(au.ID)
	e.gw.pay(t, p.MerchantOrderID, "payer-A97", "0.05")
	au, _ = e.a.st.GetUserByID(au.ID)
	if au.CertPaidAt != 0 || au.PayerID != "payer-A97" {
		t.Fatalf("underpaid cert must anchor payer but not certify: %+v", au)
	}
}

func TestStaleHandleProfilePath(t *testing.T) {
	e := newEnv(t, "")
	old := e.browser("old")
	ou := old.register("dupname", "9801")
	imp := e.browser("imp")
	imp.register("dupname", "9802")
	ou, _ = e.a.st.GetUserByID(ou.ID)
	if !ou.HandleStale || ou.Path() != "/u/xid:9801" {
		t.Fatalf("stale user path: %s", ou.Path())
	}
	if resp, body := old.get(ou.Path()); resp.StatusCode != 200 || !strings.Contains(body, "9801") {
		t.Fatalf("xid profile: %d", resp.StatusCode)
	}
	if resp, body := old.get("/u/dupname"); resp.StatusCode != 200 || !strings.Contains(body, "9802") {
		t.Fatalf("handle profile should be the new owner: %d", resp.StatusCode)
	}
}

// ---- 减尾数唯一金额：按面板金额付款即足额（线上真实踩到的 bug） ----

func TestSubModeUniqueAmountIsFullPayment(t *testing.T) {
	e := newEnv(t, "")
	e.gw.sub = true
	alice, mia := e.browser("alice"), e.browser("mia")
	au := alice.register("alice", "9901")
	mia.register("mia", "9902")
	alice.setUID("83001")
	// 认证付款：应付 0.0928，按面板金额付 → 必须认证成功
	alice.post("/me/cert", nil)
	p, _ := e.a.st.ActiveCertPayment(au.ID)
	if p.PayAmount != "0.0928" {
		t.Fatalf("mock sub amount %s", p.PayAmount)
	}
	e.gw.pay(t, p.MerchantOrderID, "payer-A99", "")
	au, _ = e.a.st.GetUserByID(au.ID)
	if au.CertPaidAt == 0 {
		t.Fatal("exact unique amount in sub mode must certify")
	}
	if e.a.st.count(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND title LIKE '%不足%'`, au.ID) != 0 {
		t.Fatal("must not send 金额不足 notice")
	}
	// 任务付款：应付 1.9928 → 足额完成，不能判少付
	mia.bindKey("83002")
	task := alice.publish(taskForm(url.Values{"reward": {"2"}}))
	x := mia.claim(task)
	e.synd.add(mockTweet{ID: "99001", Text: task.Contents[0], UserID: "9902", Handle: "mia"})
	x = mia.submitTweet(x, "99001")
	alice.post("/s/"+x.Code+"/pay/order", nil)
	tp, _ := e.a.st.ActivePayment(x.ID, "gateway")
	e.gw.pay(t, tp.MerchantOrderID, "payer-A99", "")
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || x.UnderpaidE8 != 0 {
		t.Fatalf("sub-mode full payment: status=%s underpaid=%d", x.Status, x.UnderpaidE8)
	}
}

func TestRepairCertPayments(t *testing.T) {
	e := newEnv(t, "")
	alice := e.browser("alice")
	au := alice.register("alice", "9951")
	alice.setUID("84001")
	alice.post("/me/cert", nil)
	p, _ := e.a.st.ActiveCertPayment(au.ID)
	e.gw.pay(t, p.MerchantOrderID, "payer-A995", "")
	// 模拟旧版本留下的坏状态：付款单 paid 但用户未认证
	e.a.st.db.Exec(`UPDATE users SET cert_paid_at=0 WHERE id=?`, au.ID)
	e.a.repairCertPayments()
	au, _ = e.a.st.GetUserByID(au.ID)
	if au.CertPaidAt == 0 {
		t.Fatal("repair should certify")
	}
	if e.a.st.count(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND title LIKE '%认证付款已确认%'`, au.ID) == 0 {
		t.Fatal("repair should notify")
	}
	e.a.repairCertPayments() // 幂等
	if e.a.st.count(`SELECT COUNT(*) FROM audit_log WHERE action='user.cert_paid' AND target_id=? AND meta LIKE '%repair%'`, au.ID) != 1 {
		t.Fatal("repair must be idempotent")
	}
}

func TestPayVerifyKey(t *testing.T) {
	e := newEnv(t, "")
	nia := e.browser("nia")
	nu := nia.register("nia", "9961")
	if resp, _ := nia.post("/me/pay/verify", nil); resp.StatusCode != 302 {
		t.Fatal("verify without key should redirect with flash")
	}
	nia.bindKey("85001")
	if resp, _ := nia.post("/me/pay/verify", nil); resp.StatusCode != 302 {
		t.Fatal("verify failed")
	}
	p, _ := e.a.st.GetPayProfile(nu.ID)
	if p.BPGLastErr != "" || p.BPGLastOK == 0 {
		t.Fatalf("verify should record health: %+v", p)
	}
	if _, body := nia.get("/me/pay"); !strings.Contains(body, "立即重新验证 Key") || !strings.Contains(body, "同一个") {
		t.Fatal("pay settings should show verify button and UID warning")
	}
}

// ---- 公开成交记录 ----

func TestPublicRecords(t *testing.T) {
	e := newEnv(t, "")
	alice, ola := e.browser("alice"), e.browser("ola")
	alice.register("alice", "9971")
	ola.register("ola", "9972")
	alice.setUID("86001")
	alice.certify("payer-A971")
	ola.bindKey("86002")
	task := alice.publish(taskForm(url.Values{"slots": {"2"}}))
	x := ola.claim(task)
	e.synd.add(mockTweet{ID: "97101", Text: task.Contents[0], UserID: "9972", Handle: "ola"})
	x = ola.submitTweet(x, "97101")
	alice.post("/s/"+x.Code+"/pay/order", nil)
	p, _ := e.a.st.ActivePayment(x.ID, "gateway")
	e.gw.pay(t, p.MerchantOrderID, "payer-A97", "")
	anon := e.browser("anon")
	for _, path := range []string{"/records", "/records?tab=done", "/records?tab=active", "/records?tab=overdue"} {
		resp, body := anon.get(path)
		if resp.StatusCode != 200 || strings.Contains(body, "页面渲染失败") {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	if _, body := anon.get("/records?tab=done"); !strings.Contains(body, "@ola") || !strings.Contains(body, "status/97101") || strings.Contains(body, "86002") {
		t.Fatal("done tab should list worker and tweet, never UID")
	}
	if _, body := anon.get(task.Path()); !strings.Contains(body, "接单动态") || !strings.Contains(body, "@ola") {
		t.Fatal("task page should show public activity")
	}
	if _, body := anon.get("/u/ola"); !strings.Contains(body, "最近完成的单") {
		t.Fatal("profile should list completions")
	}
}

// ---- 粉丝数门槛 / 自定义收款方式 / 非币安方式登记 ----

func TestMinFollowersAndCustomMethods(t *testing.T) {
	e := newEnv(t, "")
	alice, pam := e.browser("alice"), e.browser("pam")
	alice.register("alice", "9981")
	e.prof.set("pam", 50)
	pu := pam.register("pam", "9982")
	e.a.st.SetFollowers(pu.ID, 50) // 注册时的异步抓取结果一样是 50，这里直接落库让断言确定
	alice.setUID("87001")
	alice.certify("payer-A98")
	pam.setUID("87002")
	// 自定义收款方式
	if resp, _ := pam.post("/me/pay/method", url.Values{"label": {"BSC 钱包地址"}, "value": {"0xabcDEF0123456789"}}); resp.StatusCode != 302 {
		t.Fatal("add method failed")
	}
	pam.post("/me/pay/method", url.Values{"label": {"坏的"}, "value": {"https://evil.example"}})
	p, _ := e.a.st.GetPayProfile(pu.ID)
	if len(p.Extra) != 1 || p.Extra[0].Label != "BSC 钱包地址" {
		t.Fatalf("extra methods: %+v", p.Extra)
	}
	if _, body := pam.get("/me/pay"); !strings.Contains(body, "0xabcDEF0123456789") {
		t.Fatal("pay settings should list the method")
	}
	// 粉丝门槛：pam 只有 50 粉，任务要求 100
	task := alice.publish(taskForm(url.Values{"min_followers": {"100"}}))
	if resp, _ := pam.post("/t/"+task.Code+"/claim", nil); resp.Header.Get("Location") != task.Path() {
		t.Fatal("claim should bounce for too few followers")
	}
	if _, body := pam.get(task.Path()); !strings.Contains(body, "粉丝 ≥ 100") || !strings.Contains(body, "你当前 50") {
		t.Fatal("task page should explain the follower requirement")
	}
	// 粉丝涨到 500（缓存 24h：直接改库模拟刷新后的状态）
	e.prof.set("pam", 500)
	e.a.st.SetFollowers(pu.ID, 500)
	x := pam.claim(task)
	e.synd.add(mockTweet{ID: "98101", Text: task.Contents[0], UserID: "9982", Handle: "pam"})
	x = pam.submitTweet(x, "98101")
	if x.Status != SPayable {
		t.Fatalf("payable expected: %s", x.Status)
	}
	// 发布方看到其它收款方式，并用「其它方式」登记
	if _, body := alice.get(x.Path()); !strings.Contains(body, "BSC 钱包地址") || !strings.Contains(body, "其它方式") {
		t.Fatal("owner pay panel should show custom methods")
	}
	if resp, _ := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"via": {"other"}, "method": {"BSC 钱包地址"}, "ref": {"0x9f8e7d6c5b4a"}}); resp.StatusCode != 302 {
		t.Fatal("mark via other failed")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SAwait || x.MarkedOrderID != "" || !strings.Contains(x.MarkedNote, "0x9f8e7d6c5b4a") {
		t.Fatalf("other-method mark: %s %q %q", x.Status, x.MarkedOrderID, x.MarkedNote)
	}
	pam.post("/s/"+x.Code+"/confirm", nil)
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid {
		t.Fatalf("confirm after other-method mark: %s", x.Status)
	}
	// 删除自定义方式
	pam.post("/me/pay/method/del", url.Values{"idx": {"0"}})
	p, _ = e.a.st.GetPayProfile(pu.ID)
	if len(p.Extra) != 0 {
		t.Fatal("method should be deleted")
	}
}

func TestFollowersFetchedAtRegister(t *testing.T) {
	e := newEnv(t, "")
	e.prof.set("quinn", 7754)
	qu := e.browser("quinn").register("quinn", "9983")
	deadline := time.Now().Add(3 * time.Second)
	for {
		u, _ := e.a.st.GetUserByID(qu.ID)
		if u.Followers == 7754 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("followers not fetched at register: %d", u.Followers)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, body := e.browser("quinn").get("/u/quinn"); !strings.Contains(body, "7754") {
		t.Fatal("profile should show followers")
	}
}

// ---- 评论 / 点赞 / 转发 任务 ----

func TestReplyLikeRepostTasks(t *testing.T) {
	e := newEnv(t, "OPEN_TASKS_NEWBIE=10\n") // 一个发布方连发四个任务
	owner := e.browser("own")
	ou := owner.register("own", "7001")
	owner.setUID("88001")
	owner.certify("payer-O7")
	ws := map[string]*browser{}
	admin := e.browser("admin")
	admin.register("admin", "7009")
	for i, h := range []string{"wa", "wb", "wc"} { // 连 owner、admin 共 5 个注册，正好在同 IP 限额内
		b := e.browser(h)
		b.register(h, fmt.Sprint(7002+i))
		b.setUID(fmt.Sprint(88002 + i))
		ws[h] = b
	}
	e.synd.add(mockTweet{ID: "77001", Text: "目标推文", UserID: "7001", Handle: "own"})
	e.synd.add(mockTweet{ID: "77002", Text: "别人的推文", UserID: "7002", Handle: "wa"})

	// 1) 评论任务（自由发挥 ≥ 5 字）
	task := owner.publish(taskForm(url.Values{"ttype": {"reply"}, "target": {"https://x.com/own/status/77001"}, "match_mode": {"any"}, "min_len": {"5"}, "content1": {""}}))
	if task.Kind != "reply" || task.TargetTweetID != "77001" || task.TargetAuthor != "own" || task.MatchMode != "any" || task.MinLen != 5 {
		t.Fatalf("reply task: %+v", task)
	}
	if _, body := ws["wa"].get(task.Path()); !strings.Contains(body, "目标推文") || !strings.Contains(body, "评论") {
		t.Fatal("task page should show target and kind")
	}
	x := ws["wa"].claim(task)
	if _, body := ws["wa"].get(x.Path()); !strings.Contains(body, "in_reply_to=77001") || !strings.Contains(body, "一键去 X 评论") {
		t.Fatal("sub page should offer the reply intent")
	}
	e.synd.add(mockTweet{ID: "77101", Text: "这是一条很长的评论内容", UserID: "7002", Handle: "wa", InReplyTo: "5"})
	x = ws["wa"].submitTweet(x, "77101")
	if x.Status != SClaimed || !strings.Contains(x.LastError, "目标推文") {
		t.Fatalf("reply to wrong tweet should fail: %s %q", x.Status, x.LastError)
	}
	e.synd.add(mockTweet{ID: "77102", Text: "短", UserID: "7002", Handle: "wa", InReplyTo: "77001"})
	x = ws["wa"].submitTweet(x, "77102")
	if x.Status != SClaimed || !strings.Contains(x.LastError, "太短") {
		t.Fatalf("short reply should fail: %s %q", x.Status, x.LastError)
	}
	e.synd.add(mockTweet{ID: "77103", Text: "这条评论够长了吧朋友", UserID: "7002", Handle: "wa", InReplyTo: "77001"})
	x = ws["wa"].submitTweet(x, "77103")
	if x.Status != SPayable {
		t.Fatalf("valid reply should be payable: %s %q", x.Status, x.LastError)
	}
	// 发帖任务里回复仍然被拒
	post := owner.publish(taskForm(url.Values{"content1": {"发帖任务文案 https://example.com/q"}}))
	px := ws["wb"].claim(post)
	e.synd.add(mockTweet{ID: "77201", Text: "发帖任务文案 https://example.com/q", UserID: "7003", Handle: "wb", InReplyTo: "77001"})
	if px = ws["wb"].submitTweet(px, "77201"); px.Status != SClaimed || !strings.Contains(px.LastError, "回复") {
		t.Fatalf("post task must reject replies: %s %q", px.Status, px.LastError)
	}

	// 2) 点赞任务：目标必须是自己的推文；留存被强制为 0；退回一次后确认；另一人两次退回作废后可发起核对争议
	if resp, _ := owner.post("/new", taskForm(url.Values{"ttype": {"like"}, "target": {"https://x.com/wa/status/77002"}, "content1": {""}})); resp.StatusCode != 400 {
		t.Fatal("like task must target the owner's own tweet")
	}
	like := owner.publish(taskForm(url.Values{"ttype": {"like"}, "target": {"https://x.com/own/status/77001"}, "content1": {""}, "retention": {"24"}, "slots": {"2"}}))
	if like.Kind != "like" || like.RetentionH != 0 || len(like.Contents) != 0 {
		t.Fatalf("like task: %+v", like)
	}
	y := ws["wc"].claim(like)
	if resp, _ := ws["wc"].post("/s/"+y.Code+"/submit", url.Values{"tweet_url": {"https://x.com/wc/status/1"}}); resp.StatusCode != 400 {
		t.Fatal("like task must not accept tweet urls")
	}
	if _, body := ws["wc"].get(y.Path()); !strings.Contains(body, "intent/like?tweet_id=77001") || !strings.Contains(body, "提交核对") {
		t.Fatal("like sub page should offer like intent")
	}
	ws["wc"].post("/s/"+y.Code+"/done", nil)
	y, _ = e.a.st.GetSubByID(y.ID)
	if y.Status != SChecking || y.CheckingAt == 0 {
		t.Fatalf("after done: %s", y.Status)
	}
	if _, body := owner.get(y.Path()); !strings.Contains(body, "请核对") || !strings.Contains(body, "已点赞，确认") {
		t.Fatal("owner should see the check panel")
	}
	if _, body := owner.get("/me"); !strings.Contains(body, "待我核对") {
		t.Fatal("owner todo should list checking")
	}
	if resp, _ := ws["wc"].post("/s/"+y.Code+"/check", url.Values{"action": {"ok"}}); resp.StatusCode != 403 {
		t.Fatal("worker must not self-check")
	}
	owner.post("/s/"+y.Code+"/check", url.Values{"action": {"no"}, "note": {"名单里没有你"}})
	y, _ = e.a.st.GetSubByID(y.ID)
	if y.Status != SClaimed || y.CheckRejects != 1 || y.CheckNote != "名单里没有你" {
		t.Fatalf("after reject: %s %d %q", y.Status, y.CheckRejects, y.CheckNote)
	}
	ws["wc"].post("/s/"+y.Code+"/done", nil)
	owner.post("/s/"+y.Code+"/check", url.Values{"action": {"ok"}})
	y, _ = e.a.st.GetSubByID(y.ID)
	if y.Status != SPayable || y.CheckAuto != 0 || y.PayDeadlineAt == 0 {
		t.Fatalf("after confirm: %s", y.Status)
	}
	z := ws["wa"].claim(like)
	ws["wa"].post("/s/"+z.Code+"/done", nil)
	owner.post("/s/"+z.Code+"/check", url.Values{"action": {"no"}})
	ws["wa"].post("/s/"+z.Code+"/done", nil)
	if resp, _ := owner.post("/s/"+z.Code+"/check", url.Values{"action": {"maybe"}}); resp.StatusCode != 400 {
		t.Fatal("unknown check action must be rejected")
	}
	owner.post("/s/"+z.Code+"/check", url.Values{"action": {"no"}})
	z, _ = e.a.st.GetSubByID(z.ID)
	if z.Status != SVoid || !strings.Contains(z.VoidReason, "两次") {
		t.Fatalf("second reject should void: %s %q", z.Status, z.VoidReason)
	}
	if lk, _ := e.a.st.GetTaskByID(like.ID); lk.Left() != 1 {
		t.Fatalf("void should release the slot: left=%d", lk.Left())
	}
	if st := e.a.st.PubStats(ou.ID); st.CheckVoids != 1 {
		t.Fatalf("owner check-void count: %d", st.CheckVoids)
	}
	// 接单方 24 小时内可发起核对争议（G），管理员裁决成立 → 记录恢复为待付款
	if _, body := ws["wa"].get(z.Path()); !strings.Contains(body, `value="G"`) {
		t.Fatal("worker should be offered the G dispute after a check-void")
	}
	if resp, _ := ws["wa"].post("/s/"+z.Code+"/dispute", url.Values{"type": {"G"}, "text": {"我确实用本账号点了赞，有截图"}}); resp.StatusCode != 302 {
		t.Fatal("open G dispute failed")
	}
	gd, _ := e.a.st.OpenDisputeForSub(z.ID)
	if gd == nil || gd.Type != "G" {
		t.Fatal("G dispute not opened")
	}
	if resp, body := admin.post("/admin/dispute/"+gd.Code+"/resolve", url.Values{"resolution": {"upheld"}, "note": {"截图属实"}}); resp.StatusCode != 302 {
		t.Fatalf("resolve G: %d %s", resp.StatusCode, body[:min(200, len(body))])
	}
	z, _ = e.a.st.GetSubByID(z.ID)
	if z.Status != SPayable || z.CheckAuto != 2 {
		t.Fatalf("upheld G should restore payable: %s auto=%d", z.Status, z.CheckAuto)
	}
	if st := e.a.st.PubStats(ou.ID); st.CheckVoids != 0 {
		t.Fatalf("check-void count after upheld dispute: %d", st.CheckVoids)
	}

	// 3) 转发任务：时间线里能看到 → 直接待付款；看不到 → 待核对 → 48 小时无人处理视为通过
	rp := owner.publish(taskForm(url.Values{"ttype": {"repost"}, "target": {"https://x.com/own/status/77001"}, "content1": {""}, "slots": {"2"}}))
	r1 := ws["wa"].claim(rp)
	e.prof.setRetweets("wa", "123", "77001")
	ws["wa"].post("/s/"+r1.Code+"/done", nil)
	r1, _ = e.a.st.GetSubByID(r1.ID)
	if r1.Status != SPayable || r1.CheckAuto != 0 {
		t.Fatalf("detected repost should be payable: %s", r1.Status)
	}
	r2 := ws["wb"].claim(rp)
	ws["wb"].post("/s/"+r2.Code+"/done", nil)
	r2, _ = e.a.st.GetSubByID(r2.ID)
	if r2.Status != SChecking {
		t.Fatalf("undetected repost should wait for check: %s", r2.Status)
	}
	e.a.runJobs()
	if r2, _ = e.a.st.GetSubByID(r2.ID); r2.Status != SChecking {
		t.Fatal("must not auto-approve before 48h")
	}
	e.a.st.db.Exec(`UPDATE submissions SET checking_at=checking_at-49*3600*1000 WHERE id=?`, r2.ID)
	e.a.runJobs()
	r2, _ = e.a.st.GetSubByID(r2.ID)
	if r2.Status != SPayable || r2.CheckAuto != 1 {
		t.Fatalf("48h silence should auto-approve: %s auto=%d", r2.Status, r2.CheckAuto)
	}
	// 超时视为通过的记录，发布方可以发起 D 类申诉
	if _, body := owner.get(r2.Path()); !strings.Contains(body, `value="D"`) {
		t.Fatal("owner should be able to dispute an auto-approved record")
	}
	// 公开记录与首页不泄漏原始值
	for _, pth := range []string{"/records", "/records?tab=active", "/", rp.Path(), like.Path(), task.Path()} {
		if _, body := owner.get(pth); strings.Contains(body, "[0x") || strings.Contains(body, "%!") {
			t.Fatalf("raw value leaked on %s", pth)
		}
	}
	// 目标推文读不到 → 不能发布
	if resp, _ := owner.post("/new", taskForm(url.Values{"ttype": {"like"}, "target": {"https://x.com/own/status/404404"}, "content1": {""}})); resp.StatusCode != 400 {
		t.Fatal("unreadable target must be rejected")
	}
}

func TestRetweetedIn(t *testing.T) {
	body := `{"a":{"retweeted_status":{"entities":{"media":[{"id_str":"111"}],"urls":[]},"user":{"id_str":"222","screen_name":"x"},"id_str":"333"}},"b":{"retweeted_status_id_str":"444"}}`
	for id, want := range map[string]bool{"333": true, "444": true, "111": false, "222": false, "": false} {
		if got := retweetedIn(body, id); got != want {
			t.Fatalf("retweetedIn(%q)=%v want %v", id, got, want)
		}
	}
}

// 少付后的补差用「其它方式」登记：没有币安订单号也要能记上（此前 SQL 把空订单号当作重复而失败）。
func TestTopupViaOtherMethod(t *testing.T) {
	e := newEnv(t, "")
	alice, kim := e.browser("alice"), e.browser("kim")
	alice.register("alice", "9501")
	kim.register("kim", "9502")
	alice.setUID("71001")
	alice.certify("payer-A95")
	kim.bindKey("71002")
	kim.post("/me/pay/method", url.Values{"label": {"TRC20"}, "value": {"TQn9Y2khEsLJW1ChVWFMSMeRDow5KcbLSE"}})
	task := alice.publish(taskForm(url.Values{"reward": {"2"}}))
	x := kim.claim(task)
	e.synd.add(mockTweet{ID: "9601", Text: task.Contents[0], UserID: "9502", Handle: "kim"})
	x = kim.submitTweet(x, "9601")
	alice.post("/s/"+x.Code+"/pay/order", nil)
	p, _ := e.a.st.ActivePayment(x.ID, "gateway")
	e.gw.pay(t, p.MerchantOrderID, "payer-A95", "1.5")
	kim.post("/s/"+x.Code+"/underpaid", url.Values{"action": {"topup"}})
	if resp, _ := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"via": {"other"}, "method": {"TRC20"}, "ref": {"a1b2c3d4e5f6"}}); resp.StatusCode != 302 {
		t.Fatal("top-up via other method should be accepted")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SAwait || x.TopupMarkedAt == 0 || x.TopupOrderID != "" {
		t.Fatalf("top-up other-method state: %s marked=%d order=%q", x.Status, x.TopupMarkedAt, x.TopupOrderID)
	}
	kim.post("/s/"+x.Code+"/confirm", nil)
	if x, _ = e.a.st.GetSubByID(x.ID); x.Status != SPaid || x.PaidAmountE8 != 200000000 {
		t.Fatalf("confirm after other-method top-up: %s %d", x.Status, x.PaidAmountE8)
	}
}

// ---- 发布审核：新任务先投票，通过才上线 ----

func TestPublishReviewFlow(t *testing.T) {
	e := newEnv(t, "REVIEW_ENABLED=true\nREVIEW_MIN_VOTES=3\nREVIEW_WINDOW_H=1\nOPEN_TASKS_NEWBIE=10\n")
	admin := e.browser("admin")
	admin.register("admin", "8009")
	pub := e.browser("pub")
	pub.register("pub", "8001")
	pub.setUID("90001")
	pub.certify("payer-P8")
	var vs []*browser
	for i, h := range []string{"v1", "v2", "v3"} {
		b := e.browser(h)
		b.register(h, fmt.Sprint(8002+i))
		vs = append(vs, b)
	}
	// 投票人注册时长要够；发布方与管理员保持"新注册"
	e.a.st.db.Exec(`UPDATE users SET created_at=created_at-2*24*3600*1000 WHERE handle IN ('v1','v2','v3')`)

	// 1) 发布 → 审核中，不进大厅，接单被挡
	task := pub.publish(taskForm(url.Values{"title": {"第一个任务"}}))
	if task.Status != TReview || task.ReviewDeadlineAt == 0 || task.DeadlineDays != 7 {
		t.Fatalf("new task should be in review: %+v", task.Status)
	}
	if _, body := vs[0].get("/"); strings.Contains(body, "第一个任务") {
		t.Fatal("review task must not be listed in the lobby")
	}
	if resp, _ := vs[0].post("/t/"+task.Code+"/claim", nil); resp.Header.Get("Location") != task.Path() {
		t.Fatal("claim on a review task should bounce")
	}
	// 游客 / 刚注册的人看不到审核中的任务页；有资格的审核人和发布方能看到
	if resp, _ := e.browser("anon").get(task.Path()); resp.StatusCode != 404 {
		t.Fatalf("anon should get 404 for review task, got %d", resp.StatusCode)
	}
	if resp, _ := admin.get(task.Path()); resp.StatusCode != 200 { // 管理员
		t.Fatal("admin should see the review task")
	}
	if _, body := pub.get(task.Path()); !strings.Contains(body, "社区审核中") {
		t.Fatal("owner should see the review banner")
	}
	if _, body := vs[0].get("/review"); !strings.Contains(body, "第一个任务") || !strings.Contains(body, "没问题，可以上线") {
		t.Fatal("eligible voter should see the task on /review with vote buttons")
	}
	if _, body := vs[0].get("/"); !strings.Contains(body, "等你审核") {
		t.Fatal("home banner should invite eligible voters")
	}
	// 发布方不能投自己的；刚注册的 admin 不满足注册时长
	pub.post("/review/"+task.Code+"/vote", url.Values{"vote": {"pass"}})
	admin.post("/review/"+task.Code+"/vote", url.Values{"vote": {"pass"}})
	if p, f := e.a.st.VoteCounts(task.ID); p != 0 || f != 0 {
		t.Fatalf("owner/fresh votes must not count: %d/%d", p, f)
	}
	// 三票通过 → 提前上线，截止从上线时起算
	for _, b := range vs {
		b.post("/review/"+task.Code+"/vote", url.Values{"vote": {"pass"}})
	}
	task, _ = e.a.st.GetTaskByID(task.ID)
	if task.Status != "open" || task.ReviewResult != "vote_pass" || task.DeadlineAt < task.PublishedAt+6*dayMs {
		t.Fatalf("3 pass votes should publish: %s %s", task.Status, task.ReviewResult)
	}
	if _, body := vs[0].get("/"); !strings.Contains(body, "第一个任务") {
		t.Fatal("published task should be in the lobby")
	}
	if n := e.a.st.count(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND title LIKE '%通过审核%'`, task.OwnerID); n != 1 {
		t.Fatalf("owner should be notified once: %d", n)
	}

	// 2) 两票反对、到期 → 转管理员裁定 → 管理员通过
	t2 := pub.publish(taskForm(url.Values{"title": {"第二个任务"}}))
	vs[0].post("/review/"+t2.Code+"/vote", url.Values{"vote": {"fail"}, "reason": {"发布方可疑"}, "text": {"链接可疑"}})
	vs[1].post("/review/"+t2.Code+"/vote", url.Values{"vote": {"fail"}})
	e.a.runJobs()
	if t2, _ = e.a.st.GetTaskByID(t2.ID); t2.Status != TReview {
		t.Fatal("must not decide before the deadline with only 2 votes")
	}
	if _, body := pub.get(t2.Path()); !strings.Contains(body, "发布方可疑：链接可疑") {
		t.Fatal("owner should see anonymous rejection reasons")
	}
	e.a.st.db.Exec(`UPDATE tasks SET review_deadline_at=? WHERE id=?`, ms()-1, t2.ID)
	e.a.runJobs()
	t2, _ = e.a.st.GetTaskByID(t2.ID)
	if t2.Status != TReview || t2.ReviewHold != 1 {
		t.Fatalf("2 objections at deadline should hold for admin: %s hold=%d", t2.Status, t2.ReviewHold)
	}
	if _, body := admin.get("/admin"); !strings.Contains(body, "第二个任务") || !strings.Contains(body, "待裁定") {
		t.Fatal("admin queue should list the held task")
	}
	if resp, _ := vs[2].post("/admin/task/"+t2.Code+"/approve", nil); resp.StatusCode != 403 && resp.StatusCode != 404 {
		t.Fatalf("non-admin approve must be refused, got %d", resp.StatusCode)
	}
	admin.post("/admin/task/"+t2.Code+"/approve", nil)
	if t2, _ = e.a.st.GetTaskByID(t2.ID); t2.Status != "open" || t2.ReviewResult != "admin_pass" {
		t.Fatalf("admin approve: %s %s", t2.Status, t2.ReviewResult)
	}

	// 3) 无票到期 → 自动上线
	t3 := pub.publish(taskForm(url.Values{"title": {"第三个任务"}}))
	e.a.st.db.Exec(`UPDATE tasks SET review_deadline_at=? WHERE id=?`, ms()-1, t3.ID)
	e.a.runJobs()
	if t3, _ = e.a.st.GetTaskByID(t3.ID); t3.Status != "open" || t3.ReviewResult != "timeout_pass" {
		t.Fatalf("silent deadline should auto-publish: %s %s", t3.Status, t3.ReviewResult)
	}

	// 4) 三票反对 → 转管理员裁定（不直接驳回，防小号联手封杀）→ 管理员驳回；发布方改文案 → 重新审核，旧票清空
	t4 := pub.publish(taskForm(url.Values{"title": {"第四个任务"}}))
	for _, b := range vs {
		b.post("/review/"+t4.Code+"/vote", url.Values{"vote": {"fail"}, "reason": {"诈骗 / 钓鱼 / 引流付费"}})
	}
	t4, _ = e.a.st.GetTaskByID(t4.ID)
	if t4.Status != TReview || t4.ReviewHold != 1 {
		t.Fatalf("3 fail votes should hold for admin: %s hold=%d", t4.Status, t4.ReviewHold)
	}
	admin.post("/admin/task/"+t4.Code+"/reject", url.Values{"reason": {"钓鱼链接"}})
	t4, _ = e.a.st.GetTaskByID(t4.ID)
	if t4.Status != TRejected || t4.ReviewResult != "admin_reject" {
		t.Fatalf("admin reject: %s %s", t4.Status, t4.ReviewResult)
	}
	if resp, _ := vs[0].get(t4.Path()); resp.StatusCode != 404 {
		t.Fatal("rejected task should be hidden from others")
	}
	if _, body := pub.get(t4.Path()); !strings.Contains(body, "未通过审核") {
		t.Fatal("owner should see rejection")
	}
	pub.post("/new", url.Values{"edit": {t4.Code}, "title": {"第四个任务（改）"}, "content1": {"改过的文案 https://example.com/z"}, "match_mode": {"exact"}, "ttype": {"post"}})
	t4, _ = e.a.st.GetTaskByID(t4.ID)
	if t4.Status != TReview || t4.Title != "第四个任务（改）" {
		t.Fatalf("edit should resubmit for review: %s %q", t4.Status, t4.Title)
	}
	if p, f := e.a.st.VoteCounts(t4.ID); p != 0 || f != 0 {
		t.Fatal("old votes must be cleared on resubmit")
	}

	// 上线后的任务改了内容也要重新过审（否则先过审再改成钓鱼文案）
	pub.post("/new", url.Values{"edit": {task.Code}, "title": {"第一个任务（改）"}, "content1": {"改过的文案 https://example.com/y"}, "match_mode": {"exact"}, "ttype": {"post"}})
	if task, _ = e.a.st.GetTaskByID(task.ID); task.Status != TReview {
		t.Fatalf("editing a live task must send it back to review: %s", task.Status)
	}

	// 5) 管理员发布也要过审（默认谁都不免），发布方在审核页能看到自己的任务和进度
	admin.setUID("90009")
	admin.certify("payer-A8")
	ta := admin.publish(taskForm(url.Values{"title": {"管理员任务"}}))
	if ta.Status != TReview {
		t.Fatalf("admin task must go through review too: %s %s", ta.Status, ta.ReviewResult)
	}
	if _, body := admin.get("/review"); !strings.Contains(body, "你发布的") || !strings.Contains(body, "管理员任务") {
		t.Fatal("owner should see own task in the review hall")
	}
	// 页面不泄漏原始值
	for _, pth := range []string{"/review", "/admin", task.Path(), t2.Path(), "/me?tab=tasks", "/"} {
		if _, body := pub.get(pth); strings.Contains(body, "[0x") || strings.Contains(body, "%!") {
			t.Fatalf("raw value leaked on %s", pth)
		}
	}
}

// ---- 按浏览量计价（CPM） ----

func TestCPMPricing(t *testing.T) {
	e := newEnv(t, "OPEN_TASKS_NEWBIE=10\n")
	alice := e.browser("alice")
	alice.register("alice", "6001")
	alice.setUID("61001")
	alice.certify("payer-A60")
	var ws []*browser
	for i, h := range []string{"w1", "w2", "w3", "w4"} {
		b := e.browser(h)
		b.register(h, fmt.Sprint(6002+i))
		ws = append(ws, b)
	}
	ws[0].bindKey("61002")
	for _, b := range ws[1:] {
		b.setUID("6100" + b.who[1:])
	}
	// 校验：留存 0 不行；点赞不能按浏览量；保底高于封顶不行
	base := url.Values{"price_mode": {"cpm"}, "cpm": {"1"}, "floor_amt": {"0.2"}, "cap_amt": {"3"}, "retention": {"24"}, "slots": {"4"}}
	if resp, _ := alice.post("/new", taskForm(merge(base, url.Values{"retention": {"0"}}))); resp.StatusCode != 400 {
		t.Fatal("cpm with zero retention must be rejected")
	}
	if resp, _ := alice.post("/new", taskForm(merge(base, url.Values{"floor_amt": {"5"}}))); resp.StatusCode != 400 {
		t.Fatal("floor above cap must be rejected")
	}
	e.synd.add(mockTweet{ID: "66001", Text: "目标", UserID: "6001", Handle: "alice"})
	if resp, _ := alice.post("/new", taskForm(merge(base, url.Values{"ttype": {"like"}, "target": {"https://x.com/alice/status/66001"}, "content1": {""}}))); resp.StatusCode != 400 {
		t.Fatal("like task cannot be cpm priced")
	}
	task := alice.publish(taskForm(base))
	if !task.CPM() || task.CpmE8 != 100000000 || task.FloorE8 != 20000000 || task.RewardE8 != 300000000 || task.RetentionH != 24 {
		t.Fatalf("cpm task: %+v", task)
	}
	if _, body := ws[0].get("/"); !strings.Contains(body, "千浏览") || !strings.Contains(body, "封顶 3") {
		t.Fatal("lobby should show cpm pricing")
	}
	// 四个人接单发帖 → 已验证（留存 24h）
	var subs []*Submission
	for i, b := range ws {
		x := b.claim(task)
		tid := fmt.Sprint(66100 + i)
		e.synd.add(mockTweet{ID: tid, Text: task.Contents[0], UserID: fmt.Sprint(6002 + i), Handle: b.who})
		x = b.submitTweet(x, tid)
		if x.Status != SVerified {
			t.Fatalf("worker %d should be verified: %s %q", i, x.Status, x.LastError)
		}
		subs = append(subs, x)
	}
	// 记录页在结算前显示预计报酬
	e.prof.setViews("66100", 2500) // 2.5 U
	e.prof.setViews("66101", 100)  // 0.1 → 保底 0.2
	e.prof.setViews("66102", 9000) // 9 → 封顶 3
	// 66103 读不到
	e.a.st.db.Exec(`UPDATE submissions SET recheck_due_at=? WHERE task_id=?`, ms()-1, task.ID)
	e.a.runJobs()
	want := []int64{250000000, 20000000, 300000000}
	for i, w := range want {
		x, _ := e.a.st.GetSubByID(subs[i].ID)
		if x.Status != SPayable || x.AmountE8 != w {
			t.Fatalf("sub %d: %s amount=%d want %d", i, x.Status, x.AmountE8, w)
		}
	}
	x3, _ := e.a.st.GetSubByID(subs[3].ID)
	if x3.Status != SVerified || !strings.Contains(x3.RecheckFlag, "浏览量") {
		t.Fatalf("unreadable views should retry: %s %q", x3.Status, x3.RecheckFlag)
	}
	// 超过 24 小时仍读不到 → 按已记录值（无 → 0）结算为保底
	e.a.runJobs() // 重试改写了 recheck_due_at，但兜底期限按原始到期时间算，这里仍不应结算
	if x3, _ = e.a.st.GetSubByID(x3.ID); x3.Status != SVerified {
		t.Fatalf("retry must not settle early: %s", x3.Status)
	}
	e.a.st.db.Exec(`UPDATE submissions SET recheck_due_at=?, verified_at=? WHERE id=?`, ms()-1, ms()-49*hourMs, x3.ID)
	e.a.runJobs()
	if x3, _ = e.a.st.GetSubByID(x3.ID); x3.Status != SPayable || x3.AmountE8 != 20000000 || !strings.Contains(x3.RecheckFlag, "读取失败") {
		t.Fatalf("fallback settlement: %s amount=%d flag=%q", x3.Status, x3.AmountE8, x3.RecheckFlag)
	}
	// 页面与公开记录用结算金额；网关订单金额也是结算金额
	if _, body := alice.get(subs[0].Path()); !strings.Contains(body, "2.5 U") || !strings.Contains(body, "2500") {
		t.Fatal("record page should show the settled amount and views")
	}
	if _, body := alice.get(task.Path()); !strings.Contains(body, "去付款 2.5 U") {
		t.Fatal("owner dashboard should use the settled amount")
	}
	if _, body := ws[0].get("/records"); !strings.Contains(body, "2.5 U") {
		t.Fatal("public records should show the settled amount")
	}
	alice.post("/s/"+subs[0].Code+"/pay/order", nil)
	p0, _ := e.a.st.ActivePayment(subs[0].ID, "gateway")
	if p0 == nil || p0.BaseE8 != 250000000 {
		t.Fatalf("gateway order should be for the settled amount: %+v", p0)
	}
	e.gw.pay(t, p0.MerchantOrderID, "payer-A60", "2.5")
	if x0, _ := e.a.st.GetSubByID(subs[0].ID); x0.Status != SPaid || x0.PaidAmountE8 != 250000000 {
		t.Fatalf("paid amount: %s %d", x0.Status, x0.PaidAmountE8)
	}
	// 手动确认路径：按结算金额（保底 0.2）
	alice.post("/s/"+subs[1].Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068889601"}})
	ws[1].post("/s/"+subs[1].Code+"/confirm", nil)
	if x1, _ := e.a.st.GetSubByID(subs[1].ID); x1.Status != SPaid || x1.PaidAmountE8 != 20000000 {
		t.Fatalf("manual confirm amount: %s %d", x1.Status, x1.PaidAmountE8)
	}
}

func merge(a, b url.Values) url.Values {
	out := url.Values{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func TestEligibleCount(t *testing.T) {
	e := newEnv(t, "")
	b := e.browser("cnt")
	if resp, _ := b.get("/new/eligible?days=0&fans=0"); resp.StatusCode != 302 && resp.StatusCode != 401 && resp.StatusCode != 403 {
		t.Fatalf("anon should not get counts: %d", resp.StatusCode)
	}
	u := b.register("cnt", "5001")
	e.a.st.SetFollowers(u.ID, 800)
	if _, body := b.get("/new/eligible?days=0&fans=500"); !strings.Contains(body, `"n":1`) || !strings.Contains(body, `"total":1`) {
		t.Fatalf("eligible count: %s", body)
	}
	if _, body := b.get("/new/eligible?days=0&fans=5000"); !strings.Contains(body, `"n":0`) {
		t.Fatalf("eligible count high bar: %s", body)
	}
}

func TestCancelOpenTasksKeepsClaims(t *testing.T) {
	e := newEnv(t, "OPEN_TASKS_NEWBIE=10\n")
	admin := e.browser("admin")
	admin.register("admin", "4009")
	alice := e.browser("alice")
	alice.register("alice", "4001")
	alice.setUID("41001")
	alice.certify("payer-A40")
	bob := e.browser("bob")
	bob.register("bob", "4002")
	bob.setUID("41002")
	t1 := alice.publish(taskForm(url.Values{"title": {"甲"}, "slots": {"3"}}))
	t2 := alice.publish(taskForm(url.Values{"title": {"乙"}}))
	x := bob.claim(t1)
	if resp, _ := bob.post("/admin/tasks/cancel-open", nil); resp.StatusCode != 403 && resp.StatusCode != 404 {
		t.Fatalf("non-admin must not cancel: %d", resp.StatusCode)
	}
	admin.post("/admin/tasks/cancel-open", url.Values{"reason": {"改为先审核再上线"}})
	t1, _ = e.a.st.GetTaskByID(t1.ID)
	t2, _ = e.a.st.GetTaskByID(t2.ID)
	if t1.Status != "closed" || t1.SlotsTotal != 1 || t2.Status != "closed" || t2.SlotsTotal != 0 {
		t.Fatalf("cancel-open: %s/%d %s/%d", t1.Status, t1.SlotsTotal, t2.Status, t2.SlotsTotal)
	}
	if x, _ = e.a.st.GetSubByID(x.ID); x.Status != SClaimed {
		t.Fatalf("existing claim must survive: %s", x.Status)
	}
	if _, body := bob.get("/"); strings.Contains(body, "甲") || strings.Contains(body, "乙") {
		t.Fatal("cancelled tasks must leave the lobby")
	}
	if _, body := alice.get(t2.Path()); strings.Contains(body, "名额已全部完成") || !strings.Contains(body, "已撤回") {
		t.Fatal("cancelled task must read 已撤回, not 全部完成")
	}
	if n := e.a.st.count(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND title LIKE '%停止接新单%'`, t1.OwnerID); n != 2 {
		t.Fatalf("owner notifications: %d", n)
	}
	// H5 底栏有审核入口
	if _, body := bob.get("/"); !strings.Contains(body, `href="/review"`) {
		t.Fatal("tabbar should link to /review")
	}
}

// ---- 提前付款：发布方不等留存到期主动结算 ----

func TestEarlyPay(t *testing.T) {
	e := newEnv(t, "OPEN_TASKS_NEWBIE=10\n")
	alice, bob, carl, dan := e.browser("alice"), e.browser("bob"), e.browser("carl"), e.browser("dan")
	au := alice.register("alice", "7101")
	bu := bob.register("bob", "7102")
	carl.register("carl", "7103")
	dan.register("dan", "7104")
	alice.setUID("41001")
	alice.certify("payer-EP")
	bob.setUID("41002")
	carl.setUID("41003")
	dan.setUID("41004")
	task := alice.publish(taskForm(url.Values{"retention": {"24"}, "slots": {"3"}}))
	x := bob.claim(task)
	e.synd.add(mockTweet{ID: "7201", Text: task.Contents[0], UserID: "7102", Handle: "bob"})
	x = bob.submitTweet(x, "7201")
	if x.Status != SVerified {
		t.Fatalf("verified expected: %s", x.Status)
	}
	// 入口：记录页「现在就付」、任务管理页的行内按钮；接单方无权触发
	if _, body := alice.get(x.Path()); !strings.Contains(body, "现在就付") {
		t.Fatal("owner should see the early-pay button")
	}
	if _, body := alice.get(task.Path()); !strings.Contains(body, "/paynow") {
		t.Fatal("task manage should offer early pay for verified rows")
	}
	if resp, _ := bob.post("/s/"+x.Code+"/paynow", nil); resp.StatusCode != 403 {
		t.Fatalf("worker must not trigger early pay: %d", resp.StatusCode)
	}
	// 提前付款：立刻复检通过 → 待付款，跳到付款面板；接单方收到通知，发布方自己不收「待付款」通知；全站横幅出现
	resp, _ := alice.post("/s/"+x.Code+"/paynow", nil)
	if resp.StatusCode != 302 || !strings.HasSuffix(resp.Header.Get("Location"), "#pay") {
		t.Fatalf("early pay should redirect to the pay panel: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPayable || x.PayDeadlineAt <= ms() || !strings.Contains(x.RecheckFlag, "提前") {
		t.Fatalf("payable expected: %s %d %q", x.Status, x.PayDeadlineAt, x.RecheckFlag)
	}
	if n := e.a.st.count(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND title LIKE '%提前付款%'`, bu.ID); n != 1 {
		t.Fatalf("worker should be told about early pay, got %d", n)
	}
	if n := e.a.st.count(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND title LIKE '%有一条记录待付款%'`, au.ID); n != 0 {
		t.Fatal("owner triggered it, no payable notification needed")
	}
	if _, body := alice.get(x.Path()); !strings.Contains(body, "去付款") || !strings.Contains(body, `id="pay"`) {
		t.Fatal("record page should lead the owner to the pay panel")
	}
	if _, body := alice.get("/"); !strings.Contains(body, `class="paybar"`) {
		t.Fatal("pay banner expected on other pages")
	}
	if _, body := alice.get("/me"); strings.Contains(body, `class="paybar"`) {
		t.Fatal("no banner on /me itself")
	}
	if _, body := bob.get("/"); strings.Contains(body, `class="paybar"`) {
		t.Fatal("banner is for publishers only")
	}
	// 改过文的推文：提前付款时复检不达标 → 作废，无需付款
	x2 := carl.claim(task)
	e.synd.add(mockTweet{ID: "7202", Text: task.Contents[0], UserID: "7103", Handle: "carl"})
	x2 = carl.submitTweet(x2, "7202")
	e.synd.add(mockTweet{ID: "7202", Text: "改掉了", UserID: "7103", Handle: "carl"})
	if resp, _ := alice.post("/s/"+x2.Code+"/paynow", nil); resp.StatusCode != 302 {
		t.Fatalf("altered tweet: %d", resp.StatusCode)
	}
	if x2, _ = e.a.st.GetSubByID(x2.ID); x2.Status != SVoid {
		t.Fatalf("altered tweet must void: %s", x2.Status)
	}
	// 读不到推文：什么都不改（状态、复检计划、未读计数都不动），只提示稍后再试
	x3 := dan.claim(task)
	e.synd.add(mockTweet{ID: "7203", Text: task.Contents[0], UserID: "7104", Handle: "dan"})
	x3 = dan.submitTweet(x3, "7203")
	e.synd.del("7203")
	if resp, body := alice.post("/s/"+x3.Code+"/paynow", nil); resp.StatusCode != 400 || !strings.Contains(body, "读不到") {
		t.Fatalf("unreadable tweet should just explain: %d %s", resp.StatusCode, snippet(body))
	}
	if nx, _ := e.a.st.GetSubByID(x3.ID); nx.Status != SVerified || nx.Unreadable != 0 || nx.RecheckDueAt != x3.RecheckDueAt {
		t.Fatalf("unreadable early pay must not change anything: %s %d", nx.Status, nx.Unreadable)
	}
	// 按浏览量计价：读不到浏览量时不结算；读到后按此刻浏览量结算
	ct := alice.publish(taskForm(url.Values{"price_mode": {"cpm"}, "cpm": {"1"}, "floor_amt": {"0.2"}, "cap_amt": {"3"}, "retention": {"24"}, "slots": {"2"}}))
	x4 := bob.claim(ct)
	e.synd.add(mockTweet{ID: "7301", Text: ct.Contents[0], UserID: "7102", Handle: "bob"})
	x4 = bob.submitTweet(x4, "7301")
	if x4.Status != SVerified {
		t.Fatalf("cpm verified expected: %s", x4.Status)
	}
	if resp, body := alice.post("/s/"+x4.Code+"/paynow", nil); resp.StatusCode != 400 || !strings.Contains(body, "浏览量") {
		t.Fatalf("cpm without views should wait: %d %s", resp.StatusCode, snippet(body))
	}
	e.prof.setViews("7301", 1500)
	if resp, _ := alice.post("/s/"+x4.Code+"/paynow", nil); resp.StatusCode != 302 {
		t.Fatalf("cpm early pay: %d", resp.StatusCode)
	}
	if x4, _ = e.a.st.GetSubByID(x4.ID); x4.Status != SPayable || x4.SettleViews != 1500 || x4.AmountE8 != 150000000 {
		t.Fatalf("cpm early settle: %s views=%d amount=%d", x4.Status, x4.SettleViews, x4.AmountE8)
	}
	for _, pth := range []string{x.Path(), x3.Path(), x4.Path(), task.Path(), "/review", "/me", "/"} {
		if _, body := alice.get(pth); strings.Contains(body, "[0x") || strings.Contains(body, "%!") {
			t.Fatalf("raw value leaked on %s", pth)
		}
	}
}

// ---- 待确认到账 24 小时不处理自动完成；之后 7 天内仍可申诉 ----

func TestAutoConfirm(t *testing.T) {
	e := newEnv(t, "OPEN_TASKS_NEWBIE=10\n")
	alice, bob, carl, dan := e.browser("alice"), e.browser("bob"), e.browser("carl"), e.browser("dan")
	au := alice.register("alice", "7501")
	bu := bob.register("bob", "7502")
	carl.register("carl", "7503")
	dan.register("dan", "7504")
	alice.setUID("42001")
	alice.certify("payer-AC")
	bob.setUID("42002")
	carl.setUID("42003")
	dan.setUID("42004")
	task := alice.publish(taskForm(url.Values{"slots": {"6"}}))
	mark := func(b *browser, xid string, tid string, oid string) *Submission {
		x := b.claim(task)
		e.synd.add(mockTweet{ID: tid, Text: task.Contents[0], UserID: xid, Handle: b.who})
		x = b.submitTweet(x, tid)
		if x.Status != SPayable {
			t.Fatalf("payable expected: %s", x.Status)
		}
		if resp, _ := alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {oid}}); resp.StatusCode != 302 {
			t.Fatal("mark failed")
		}
		x, _ = e.a.st.GetSubByID(x.ID)
		if x.Status != SAwait {
			t.Fatalf("await expected: %s", x.Status)
		}
		return x
	}
	// 1) 24 小时不处理 → 自动完成（不计信用），双方收到通知；接单方页面出现事后申诉入口
	x := mark(bob, "7502", "7601", "452021922068888811")
	e.a.runJobs()
	if nx, _ := e.a.st.GetSubByID(x.ID); nx.Status != SAwait {
		t.Fatal("must not auto-complete before 24h")
	}
	e.a.st.db.Exec(`UPDATE submissions SET marked_paid_at=? WHERE id=?`, ms()-25*hourMs, x.ID)
	e.a.runJobs()
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || x.ConfirmMethod != "auto" || x.ConfirmedAt == 0 {
		t.Fatalf("auto paid expected: %s %s", x.Status, x.ConfirmMethod)
	}
	if st := e.a.st.PubStats(au.ID); st.PaidCredit != 0 {
		t.Fatal("auto confirmation must not build credit")
	}
	if n := e.a.st.count(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND title LIKE '%自动完成%'`, bu.ID); n != 1 {
		t.Fatalf("worker should be told, got %d", n)
	}
	if _, body := bob.get(x.Path()); !strings.Contains(body, "自动完成") || !strings.Contains(body, "实际没有收到") {
		t.Fatal("worker should see the post-completion dispute entry")
	}
	if _, body := alice.get(x.Path()); !strings.Contains(body, "自动确认") {
		t.Fatal("owner should see how it completed")
	}
	// 3) 申诉不成立（款已到账 / UID 填错）：记录完成
	x2 := mark(carl, "7503", "7602", "452021922068888812")
	e.a.st.db.Exec(`UPDATE submissions SET marked_paid_at=? WHERE id=?`, ms()-25*hourMs, x2.ID)
	e.a.runJobs()
	carl.post("/s/"+x2.Code+"/dispute", url.Values{"type": {"B"}, "text": {"我没有收到这笔转账"}})
	d2, _ := e.a.st.OpenDisputeForSub(x2.ID)
	if d2 == nil {
		t.Fatal("dispute expected")
	}
	if err := e.a.applyResolution(d2, "b_wrong_uid", "UID 填错", 0, "", ""); err != nil {
		t.Fatal(err)
	}
	if x2, _ = e.a.st.GetSubByID(x2.ID); x2.Status != SPaid {
		t.Fatalf("paid expected: %s", x2.Status)
	}
	// 4) 申诉窗口过期后不能再申诉
	x3 := mark(dan, "7504", "7603", "452021922068888813")
	e.a.st.db.Exec(`UPDATE submissions SET marked_paid_at=? WHERE id=?`, ms()-25*hourMs, x3.ID)
	e.a.runJobs()
	e.a.st.db.Exec(`UPDATE submissions SET confirmed_at=? WHERE id=?`, ms()-8*dayMs, x3.ID)
	if _, body := dan.get(x3.Path()); strings.Contains(body, "实际没有收到") {
		t.Fatal("dispute window should be closed after 7 days")
	}
	if resp, _ := dan.post("/s/"+x3.Code+"/dispute", url.Values{"type": {"B"}, "text": {"太晚了但我还是想申诉"}}); resp.StatusCode == 302 {
		if dd, _ := e.a.st.OpenDisputeForSub(x3.ID); dd != nil {
			t.Fatal("late dispute must be refused")
		}
	}
	// 4b) 已举报过的记录、申诉中的记录不自动完成（申诉会冻结发布方任务，放在接单用例之后）
	eve := e.browser("eve")
	eve.register("eve", "7505")
	eve.setUID("42005")
	x5 := mark(eve, "7505", "7605", "452021922068888815")
	e.a.st.db.Exec(`UPDATE submissions SET marked_paid_at=?, reported_at=1 WHERE id=?`, ms()-25*hourMs, x5.ID)
	e.a.runJobs()
	if nx, _ := e.a.st.GetSubByID(x5.ID); nx.Status != SAwait {
		t.Fatalf("reported record must wait for explicit confirmation: %s", nx.Status)
	}
	e.a.st.db.Exec(`UPDATE submissions SET reported_at=0 WHERE id=?`, x5.ID)
	if resp, _ := eve.post("/s/"+x5.Code+"/dispute", url.Values{"type": {"B"}, "text": {"没有收到这笔钱，流水里没有"}}); resp.StatusCode != 302 {
		t.Fatal("dispute B failed")
	}
	if ok, _ := e.a.st.SetAutoPaid(x5.ID, 100000000); ok {
		t.Fatal("auto completion must never override an open dispute")
	}
	e.a.runJobs()
	if nx, _ := e.a.st.GetSubByID(x5.ID); nx.Status != SDisputed {
		t.Fatalf("disputed record must stay disputed: %s", nx.Status)
	}
	if d5, _ := e.a.st.OpenDisputeForSub(x5.ID); d5 == nil {
		t.Fatal("dispute must stay open")
	}
	// 5) 事后 B 类申诉成立：撤销完成、记录转逾期、发布方上黑名单（放最后：会冻结并关闭发布方的任务）
	if resp, _ := bob.post("/s/"+x.Code+"/dispute", url.Values{"type": {"B"}, "text": {"币安支付记录里没有这笔转账"}}); resp.StatusCode != 302 {
		t.Fatal("post-auto dispute B failed")
	}
	d, _ := e.a.st.OpenDisputeForSub(x.ID)
	if d == nil || d.Type != "B" {
		t.Fatalf("dispute %+v", d)
	}
	if nx, _ := e.a.st.GetSubByID(x.ID); nx.Status != SDisputed || nx.PrevStatus != SPaid {
		t.Fatalf("disputed from paid expected: %s/%s", nx.Status, nx.PrevStatus)
	}
	if err := e.a.applyResolution(d, "b_fake", "查无此转账", 0, "", ""); err != nil {
		t.Fatal(err)
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	// 撤销完成 → 逾期；黑名单流程随即把发布方的逾期记录记为违约
	if (x.Status != SOverdue && x.Status != SDefault) || x.ConfirmMethod != "" || x.ConfirmedAt != 0 || x.PaidAmountE8 != 0 {
		t.Fatalf("b_fake must undo the completion: %s %q %d", x.Status, x.ConfirmMethod, x.PaidAmountE8)
	}
	if bl, _ := e.a.st.ActiveBlacklist(au.ID); bl == nil {
		t.Fatal("publisher should be blacklisted")
	}
	for _, pth := range []string{x.Path(), x2.Path(), x3.Path(), task.Path(), "/me", "/rules"} {
		if _, body := alice.get(pth); strings.Contains(body, "[0x") || strings.Contains(body, "%!") {
			t.Fatalf("raw value leaked on %s", pth)
		}
	}
}

// ---- 信用口径：接单方确认也计信用，超时自动完成不计 ----

func TestCreditCountsManualConfirm(t *testing.T) {
	e := newEnv(t, "OPEN_TASKS_NEWBIE=10\n")
	alice, bob, carl := e.browser("alice"), e.browser("bob"), e.browser("carl")
	au := alice.register("alice", "7701")
	bu := bob.register("bob", "7702")
	carl.register("carl", "7703")
	alice.setUID("43001")
	alice.certify("payer-CR")
	bob.setUID("43002")
	carl.setUID("43003")
	task := alice.publish(taskForm(url.Values{"slots": {"3"}}))
	x := bob.claim(task)
	e.synd.add(mockTweet{ID: "7801", Text: task.Contents[0], UserID: "7702", Handle: "bob"})
	x = bob.submitTweet(x, "7801")
	alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068888821"}})
	if resp, _ := bob.post("/s/"+x.Code+"/confirm", nil); resp.StatusCode != 302 {
		t.Fatal("confirm failed")
	}
	if x, _ = e.a.st.GetSubByID(x.ID); x.Status != SPaid || x.ConfirmMethod != "manual" {
		t.Fatalf("manual paid expected: %s %s", x.Status, x.ConfirmMethod)
	}
	ps := e.a.st.PubStats(au.ID)
	if ps.PaidCredit != 1 || ps.PaidGateway != 0 {
		t.Fatalf("manual confirmation must count toward credit: credit=%d gateway=%d", ps.PaidCredit, ps.PaidGateway)
	}
	if ws := e.a.st.WorkerStats(bu.ID); ws.DoneCredit != 1 {
		t.Fatalf("worker credit %d", ws.DoneCredit)
	}
	// 超时自动完成不计
	x2 := carl.claim(task)
	e.synd.add(mockTweet{ID: "7802", Text: task.Contents[0], UserID: "7703", Handle: "carl"})
	x2 = carl.submitTweet(x2, "7802")
	alice.post("/s/"+x2.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068888822"}})
	e.a.st.db.Exec(`UPDATE submissions SET marked_paid_at=? WHERE id=?`, ms()-25*hourMs, x2.ID)
	e.a.runJobs()
	if x2, _ = e.a.st.GetSubByID(x2.ID); x2.Status != SPaid || x2.ConfirmMethod != "auto" {
		t.Fatalf("auto paid expected: %s %s", x2.Status, x2.ConfirmMethod)
	}
	if ps := e.a.st.PubStats(au.ID); ps.PaidCredit != 1 {
		t.Fatalf("auto completion must not count: %d", ps.PaidCredit)
	}
	if _, body := alice.get("/me"); !strings.Contains(body, "计信用付款") {
		t.Fatal("label")
	}
}

// ---- 自导自演不计信用：接单时同网段，或付款结算时才同网段 ----

func TestSelfDealNotCredited(t *testing.T) {
	e := newEnv(t, "OPEN_TASKS_NEWBIE=10\n")
	alice, bob, carl := e.browser("alice"), e.browser("bob"), e.browser("carl")
	au := alice.register("alice", "7901")
	bu := bob.register("bob", "7902")
	cu := carl.register("carl", "7903")
	alice.setUID("44001")
	alice.certify("payer-SD")
	bob.setUID("44002")
	carl.setUID("44003")
	task := alice.publish(taskForm(url.Values{"slots": {"3"}}))
	// bob 从头到尾和 alice 同一网段 → 接单即标自导自演（ip_log 在渲染页面时记录，先各开一页）
	alice.get("/me")
	bob.ip = alice.ip
	bob.get("/me")
	x := bob.claim(task)
	if x.SelfDeal != 1 {
		t.Fatal("same /24 as the publisher must be flagged at claim")
	}
	e.synd.add(mockTweet{ID: "7911", Text: task.Contents[0], UserID: "7902", Handle: "bob"})
	x = bob.submitTweet(x, "7911")
	alice.post("/s/"+x.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068888831"}})
	bob.post("/s/"+x.Code+"/confirm", nil)
	if st := e.a.st.PubStats(au.ID); st.Paid != 1 || st.PaidCredit != 0 {
		t.Fatalf("self-deal must show as paid but not as credit: %+v", st)
	}
	if ws := e.a.st.WorkerStats(bu.ID); ws.DoneCredit != 0 {
		t.Fatalf("worker credit %d", ws.DoneCredit)
	}
	// carl 接单时是另一网段，确认收款时换到 alice 的网段 → 结算时补判
	x2 := carl.claim(task)
	if x2.SelfDeal != 0 {
		t.Fatal("different network must not be flagged")
	}
	e.synd.add(mockTweet{ID: "7912", Text: task.Contents[0], UserID: "7903", Handle: "carl"})
	x2 = carl.submitTweet(x2, "7912")
	alice.post("/s/"+x2.Code+"/pay/mark", url.Values{"binance_order_id": {"452021922068888832"}})
	carl.ip = alice.ip
	carl.get("/me")
	carl.post("/s/"+x2.Code+"/confirm", nil)
	if x2, _ = e.a.st.GetSubByID(x2.ID); x2.Status != SPaid || x2.SelfDeal != 1 {
		t.Fatalf("settle-time recheck must flag: %s self_deal=%d", x2.Status, x2.SelfDeal)
	}
	if st := e.a.st.PubStats(au.ID); st.PaidCredit != 0 || st.Paid != 2 {
		t.Fatalf("stats %+v", st)
	}
	_ = cu
}
