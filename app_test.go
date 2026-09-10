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
		if tw.Reply {
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
}

type browser struct {
	e   *env
	c   *http.Client
	who string
}

func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	dir := t.TempDir()
	synd := newSynd()
	gw := newGW("testkey-testkey")
	cfgPath := filepath.Join(dir, "config.env")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf("LISTEN=127.0.0.1:0\nBASE_URL=http://test.local\nDB_PATH=%s\nUPLOAD_DIR=%s\nBPG_URL=%s\nBPG_KEY=testkey-testkey\nX_TWEET_API=%s\nADMIN_HANDLES=admin\nCERT_FEE_ENABLED=true\nJURY_MIN_POOL=1000\n%s",
		filepath.Join(dir, "t.db"), filepath.Join(dir, "up"), gw.srv.URL, synd.srv.URL, extra)), 0o644)
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
	e := &env{t: t, a: app, srv: srv, synd: synd, gw: gw, dir: dir}
	t.Cleanup(func() { srv.Close(); synd.srv.Close(); gw.srv.Close(); app.Close() })
	return e
}

func (e *env) browser(who string) *browser {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &browser{e: e, c: c, who: who}
}

func (b *browser) get(path string) (*http.Response, string) {
	resp, err := b.c.Get(b.e.srv.URL + path)
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
	if st.PaidGateway != 1 || st.Paid != 1 {
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
	for _, path := range []string{"/", task.Path(), x.Path(), "/me", "/me/pay", "/me/cert", "/u/alice", "/u/bob", "/rules", "/blacklist", "/court", "/me/notifications"} {
		if resp, body := alice.get(path); resp.StatusCode != 200 || strings.Contains(body, "页面渲染失败") {
			t.Fatalf("page %s: %d", path, resp.StatusCode)
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
	// 24 小时内不锁定
	if e.a.workerLockedNow(cu.ID) {
		t.Fatal("should not lock within 24h")
	}
	e.a.st.db.Exec(`UPDATE submissions SET marked_paid_at=? WHERE id=?`, ms()-25*hourMs, x.ID)
	if !e.a.workerLockedNow(cu.ID) {
		t.Fatal("should lock after 24h")
	}
	// 确认收到 → 完成，解锁
	if resp, _ := carol.post("/s/"+x.Code+"/confirm", nil); resp.StatusCode != 302 {
		t.Fatal("confirm failed")
	}
	x, _ = e.a.st.GetSubByID(x.ID)
	if x.Status != SPaid || x.ConfirmMethod != "manual" {
		t.Fatalf("manual paid expected: %s", x.Status)
	}
	if st := e.a.st.PubStats(au.ID); st.PaidGateway != 0 || st.Paid != 1 {
		t.Fatalf("manual should not count as gateway: %+v", st)
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
	if e.a.workerLockedNow(fu.ID) {
		t.Fatal("dispute should lift the lock")
	}
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
	if e.a.workerLockedNow(iu.ID) {
		t.Fatal("worker waiting for top-up must not be locked")
	}
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
