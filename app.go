package main

import (
	"bytes"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	bpaygate "github.com/qianyubtc/BinancePayTool/sdk/go"
)

//go:embed templates/*.html
var tplFS embed.FS

//go:embed static/*
var staticFS embed.FS

type App struct {
	cfg      *Config
	st       *Store
	mux      *http.ServeMux
	tpl      map[string]*template.Template
	lim      *limiter
	secure   bool
	session  []byte
	gwc      *bpaygate.Client // nil = 未配置网关
	origin   string
	fetchSem chan struct{} // 推文抓取并发闸
	cssVer   string        // 静态样式内容哈希，做缓存穿透

	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	jobMu     sync.Mutex // 定时任务不重入
}

// Base 每个页面都带的公共字段。
type Base struct {
	SiteTitle string
	BaseURL   string
	RepoURL   string
	AuthorX   string
	IsMobile  bool
	InApp     bool
	Me        *User
	MeID      int64
	IsAdmin   bool
	Unread    int64
	Todo      int64 // 待办数（待付款 + 待确认 + 陪审邀请）
	Path      string
	Flash     string
	NoIndex   bool
	Desc      string
	Gateway   bool // 网关是否配置
	Now       int64
	CSSVer    string
}

var pages = []string{"index", "task", "new", "sub", "me", "paysettings", "profile", "blacklist", "dispute", "court", "courtcase", "verify", "login", "rules", "admin", "adminuser", "error", "notifications", "certfee", "records"}

func newApp(cfg *Config) (*App, error) {
	st, err := openStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	sec, err := st.GetMeta("session_secret")
	if err != nil {
		st.Close()
		return nil, err
	}
	if sec == "" {
		sec = randHex(32)
		if err := st.SetMeta("session_secret", sec); err != nil {
			st.Close()
			return nil, err
		}
	}
	a := &App{cfg: cfg, st: st, lim: newLimiter(), secure: strings.HasPrefix(cfg.BaseURL, "https://"), session: []byte(sec), stop: make(chan struct{}), fetchSem: make(chan struct{}, 4)}
	if u, err := url.Parse(cfg.BaseURL); err == nil {
		a.origin = u.Scheme + "://" + u.Host
	}
	if cfg.BPGURL != "" {
		a.gwc = bpaygate.New(cfg.BPGURL, cfg.BPGKey)
	}
	if b, err := staticFS.ReadFile("static/app.css"); err == nil {
		sum := sha1.Sum(b)
		a.cssVer = hex.EncodeToString(sum[:4])
	}
	if err := a.loadTemplates(); err != nil {
		st.Close()
		return nil, err
	}
	a.routes()
	return a, nil
}

func (a *App) Close() {
	a.closeOnce.Do(func() {
		close(a.stop)
		a.wg.Wait()
		a.st.Close()
	})
}

func (a *App) funcs() template.FuncMap {
	return template.FuncMap{
		"amt":       fmtE8,
		"ago":       ago,
		"left":      left,
		"ftime":     fmtTime,
		"fdate":     fmtDate,
		"dur":       dur,
		"durMin":    durMin,
		"subStatus": subStatus,
		"dtype":     disputeType,
		"dstatus":   disputeStatus,
		"inc":       func(i int) int { return i + 1 },
		"i64":       func(i int) int64 { return int64(i) },
		"xlink":     xUserLink,
		"kindText":  kindText,
		"auditText": auditText,
		"trunc":     truncate,
		"add":       func(a, b int64) int64 { return a + b },
		"sub":       func(a, b int64) int64 { return a - b },
		"mul":       func(a, b int64) int64 { return a * b },
		"initial":   initial,
		"hue":       hue,
		"safeHTML":  func(s string) template.HTML { return template.HTML(s) },
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"stClass":   stClass,
		"ownerNext": ownerNext,
		"pubStatus": func(s string) string {
			switch s {
			case SClaimed, SSubmit:
				return "进行中"
			case SChecking:
				return "待核对"
			case SVerified:
				return "已发帖·留存中"
			case SPayable, SAwait, SDisputed:
				return "待付款"
			case SPaid:
				return "已完成"
			case SOverdue:
				return "发布方逾期"
			case SDefault:
				return "发布方违约"
			}
			return subStatus(s)
		},
		"taskStatus": func(s string) string {
			return map[string]string{"open": "接单中", "paused": "已暂停", "closed": "已关闭"}[s]
		},
		"payStatus": func(s string) string {
			if v, ok := map[string]string{"pending": "等待到账", "paid": "已到账", "underpaid": "少付", "expired": "已过期", "closed": "已关闭"}[s]; ok {
				return v
			}
			return s
		},
		"methodText": func(s string) string {
			if v, ok := map[string]string{"gateway": "网关自动核销", "manual": "接单方确认", "admin": "管理员核定"}[s]; ok {
				return v
			}
			return s
		},
		"caseStatus": func(s string) string {
			if v, ok := map[string]string{"voting": "投票中", "closed": "已结案", "escalated": "已转管理员"}[s]; ok {
				return v
			}
			return s
		},
		"tier": func(name string) string { return name },
		"nl2br": func(s string) template.HTML {
			return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(s), "\n", "<br>"))
		},
		"seq": func(n int64) []int64 {
			out := make([]int64, 0, n)
			for i := int64(0); i < n; i++ {
				out = append(out, i)
			}
			return out
		},
		"pct": func(a, b int64) int64 {
			if b <= 0 {
				return 0
			}
			return a * 100 / b
		},
		"avgDur": func(msv int64) string {
			if msv <= 0 {
				return "—"
			}
			d := time.Duration(msv) * time.Millisecond
			if d < time.Hour {
				return "<1 小时"
			}
			return dur(int64(d.Hours()))
		},
	}
}

func (a *App) loadTemplates() error {
	a.tpl = map[string]*template.Template{}
	for _, p := range pages {
		t, err := template.New(p).Funcs(a.funcs()).ParseFS(tplFS, "templates/layout.html", "templates/_parts.html", "templates/"+p+".html")
		if err != nil {
			return err
		}
		a.tpl[p] = t
	}
	return nil
}

// stClass 状态 → 样式类（good / warn / bad / wait / muted）。
func stClass(s string) string {
	switch s {
	case SPaid, "resolved", "closed":
		return "good"
	case SPayable, SAwait, SSubmit, SVerified, SChecking, "evidence", "review", "jury", "voting", "appeal":
		return "wait"
	case SOverdue, SDisputed:
		return "warn"
	case SDefault, SVoid, "blacklisted", "escalated":
		return "bad"
	}
	return "muted"
}

func (a *App) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	m.HandleFunc("GET /static/{file}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFileFS(w, r, staticFS, "static/"+r.PathValue("file"))
	})
	m.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("User-agent: *\nDisallow: /me\nDisallow: /admin\nDisallow: /d/\nDisallow: /s/\n"))
	})
	m.HandleFunc("GET /{$}", a.handleIndex)
	m.HandleFunc("GET /rules", a.handleRules)
	m.HandleFunc("GET /records", a.handleRecords)
	// 账号
	m.HandleFunc("GET /register", a.handleRegisterGet)
	m.HandleFunc("POST /register/verify", a.handleRegisterVerify)
	m.HandleFunc("POST /register", a.handleRegisterPost)
	m.HandleFunc("GET /reset", a.handleResetGet)
	m.HandleFunc("POST /reset/verify", a.handleResetVerify)
	m.HandleFunc("POST /reset", a.handleResetPost)
	m.HandleFunc("GET /login", a.handleLoginGet)
	m.HandleFunc("POST /login", a.handleLoginPost)
	m.HandleFunc("POST /logout", a.handleLogout)
	// 我
	m.HandleFunc("GET /me", a.handleMe)
	m.HandleFunc("GET /me/notifications", a.handleNotifications)
	m.HandleFunc("GET /me/pay", a.handlePaySettings)
	m.HandleFunc("POST /me/pay/uid", a.handlePayUID)
	m.HandleFunc("POST /me/pay/bind", a.handlePayBind)
	m.HandleFunc("POST /me/pay/unbind", a.handlePayUnbind)
	m.HandleFunc("POST /me/pay/verify", a.handlePayVerify)
	m.HandleFunc("POST /me/password", a.handlePassword)
	m.HandleFunc("POST /me/followers", a.handleRefreshFollowers)
	m.HandleFunc("POST /me/pay/method", a.handlePayMethodAdd)
	m.HandleFunc("POST /me/pay/method/del", a.handlePayMethodDel)
	m.HandleFunc("GET /me/cert", a.handleCertGet)
	m.HandleFunc("POST /me/cert", a.handleCertPost)
	m.HandleFunc("GET /me/cert/status", a.handleCertStatus)
	m.HandleFunc("GET /u/{handle}", a.handleProfile)
	// 任务
	m.HandleFunc("GET /new", a.handleNewGet)
	m.HandleFunc("POST /new", a.handleNewPost)
	m.HandleFunc("GET /t/{code}", a.handleTask)
	m.HandleFunc("POST /t/{code}/claim", a.handleClaim)
	m.HandleFunc("POST /t/{code}/{action}", a.handleTaskAction)
	// 接单记录
	m.HandleFunc("GET /s/{code}", a.handleSub)
	m.HandleFunc("GET /s/{code}/status", a.handleSubStatus)
	m.HandleFunc("POST /s/{code}/submit", a.handleSubmit)
	m.HandleFunc("POST /s/{code}/done", a.handleDone)
	m.HandleFunc("POST /s/{code}/check", a.handleCheck)
	m.HandleFunc("POST /s/{code}/pay/order", a.handlePayOrder)
	m.HandleFunc("POST /s/{code}/pay/mark", a.handlePayMark)
	m.HandleFunc("POST /s/{code}/pay/claim", a.handlePayClaim)
	m.HandleFunc("POST /s/{code}/confirm", a.handleConfirm)
	m.HandleFunc("POST /s/{code}/underpaid", a.handleUnderpaid)
	m.HandleFunc("POST /s/{code}/dispute", a.handleOpenDispute)
	m.HandleFunc("POST /bpg/notify", a.handleNotify)
	// 申诉 / 黑名单 / 小法庭
	m.HandleFunc("GET /d/{code}", a.handleDispute)
	m.HandleFunc("POST /d/{code}/message", a.handleDisputeMessage)
	m.HandleFunc("POST /d/{code}/appeal", a.handleDisputeAppeal)
	m.HandleFunc("GET /evidence/{file}", a.handleEvidence)
	m.HandleFunc("GET /blacklist", a.handleBlacklist)
	m.HandleFunc("POST /blacklist/appeal", a.handleBlacklistAppeal)
	m.HandleFunc("POST /report/task/{code}", a.handleReportTask)
	m.HandleFunc("GET /court", a.handleCourt)
	m.HandleFunc("GET /court/{id}", a.handleCourtCase)
	m.HandleFunc("POST /court/{id}/vote", a.handleCourtVote)
	// 管理
	m.HandleFunc("GET /admin", a.handleAdmin)
	m.HandleFunc("GET /admin/users", a.handleAdminUsers)
	m.HandleFunc("POST /admin/dispute/{code}/{action}", a.handleAdminDispute)
	m.HandleFunc("POST /admin/user/{id}/{action}", a.handleAdminUser)
	m.HandleFunc("POST /admin/task/{code}/{action}", a.handleAdminTask)
	m.HandleFunc("POST /admin/sub/{code}/{action}", a.handleAdminSub)
	m.HandleFunc("POST /admin/blacklist/{id}/lift", a.handleAdminLift)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		a.errorPage(w, r, http.StatusNotFound, "页面不存在", "链接可能失效了，或者这条任务 / 记录已被删除。")
	})
	a.mux = m
}

// ServeHTTP 统一加安全响应头、限制请求体、POST 同源校验（网关回调除外）。
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https://pbs.twimg.com https://abs.twimg.com; style-src 'self' 'unsafe-inline'; font-src 'self'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; form-action 'self' https://x.com")
	if r.URL.Path != "/bpg/notify" {
		limit := int64(64 << 10)
		if strings.HasPrefix(r.URL.Path, "/d/") && strings.HasSuffix(r.URL.Path, "/message") {
			limit = 8 << 20 // 证据图片
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if r.Method == http.MethodPost && !a.sameOrigin(r) {
			a.errorPage(w, r, http.StatusForbidden, "请求来源不对", "请从本站页面操作。")
			return
		}
	}
	a.mux.ServeHTTP(w, r)
}

func (a *App) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		if o == a.origin {
			return true
		}
		if u, err := url.Parse(o); err == nil && u.Host == r.Host {
			return true
		}
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

func (a *App) base(w http.ResponseWriter, r *http.Request) Base {
	b := Base{SiteTitle: a.cfg.SiteTitle, BaseURL: a.cfg.BaseURL, RepoURL: a.cfg.RepoURL, AuthorX: a.cfg.AuthorX, IsMobile: isMobile(r), InApp: inAppBrowser(r.UserAgent()),
		Path: r.URL.Path, Gateway: a.gwc != nil, Now: ms(), CSSVer: a.cssVer}
	b.Me = a.currentUser(r)
	if b.Me != nil {
		b.MeID = b.Me.ID
		b.IsAdmin = a.isAdmin(b.Me)
		b.Unread = a.st.UnreadCount(b.Me.ID)
		b.Todo = a.todoCount(b.Me)
		a.st.TouchIP(b.Me.ID, a.ip(r))
	}
	if w != nil {
		b.Flash = a.takeFlash(w, r)
	}
	return b
}

func (a *App) todoCount(u *User) int64 {
	n := a.st.count(`SELECT COUNT(*) FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE t.owner_id=? AND x.status IN ('payable','overdue','checking')`, u.ID)
	n += a.st.WorkerLocked(u.ID)
	n += a.st.count(`SELECT COUNT(*) FROM jury_invites i JOIN jury_cases c ON c.id=i.case_id WHERE i.user_id=? AND i.voted_at=0 AND c.status='voting'`, u.ID)
	return n
}

// isAdmin 管理员 = 配置里点名的 X 用户名，但权限绑定到第一次以该用户名出现的 X 数字 ID（用户名可能被顶替，ID 不会）。
func (a *App) isAdmin(u *User) bool {
	if u == nil || u.XID == "" || len(a.cfg.AdminHandles) == 0 {
		return false
	}
	for h := range a.cfg.AdminHandles {
		key := "admin_xid:" + h
		bound, _ := a.st.GetMeta(key)
		if bound == "" {
			if u.HandleLower == h {
				a.st.SetMeta(key, u.XID)
				a.st.Audit(u.ID, "admin.bind", "user", u.ID, map[string]any{"handle": h}, "")
				return true
			}
			continue
		}
		if bound == u.XID {
			return true
		}
	}
	return false
}

func (a *App) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	t := a.tpl[name]
	if t == nil {
		log.Printf("[error] 模板不存在 %s", name)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("[error] 渲染 %s: %v", name, err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (a *App) renderFragment(w http.ResponseWriter, status int, page, name string, data any) {
	var buf bytes.Buffer
	if err := a.tpl[page].ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("[error] 渲染片段 %s: %v", name, err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

type errorPageData struct {
	Base
	Title string
	Msg   string
}

func (a *App) errorPage(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	a.render(w, status, "error", errorPageData{Base: a.base(nil, r), Title: title, Msg: msg})
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[error] %s %s: %v", r.Method, r.URL.Path, err)
	a.errorPage(w, r, http.StatusInternalServerError, "服务器开小差了", "稍后再试。")
}

func replyJSON(w http.ResponseWriter, status int, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *App) setCookie(w http.ResponseWriter, name, val string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: val, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

func cookieVal(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// ---- 会话 ----

func (a *App) currentUser(r *http.Request) *User {
	id, tag := parseSession(a.session, cookieVal(r, "tlm"))
	if id == 0 {
		return nil
	}
	u, err := a.st.GetUserByID(id)
	if err != nil || u == nil || u.Status == "banned" || tag != pwTag(u.PassHash) {
		return nil
	}
	return u
}

func (a *App) login(w http.ResponseWriter, u *User) {
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	a.setCookie(w, "tlm", signSession(a.session, u.ID, exp, pwTag(u.PassHash)), 30*24*3600)
	a.st.TouchLogin(u.ID)
}

func (a *App) logout(w http.ResponseWriter) { a.setCookie(w, "tlm", "", -1) }

// requireUser 未登录跳登录页（带回跳）。
func (a *App) requireUser(w http.ResponseWriter, r *http.Request) (*User, bool) {
	u := a.currentUser(r)
	if u == nil {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		} else {
			a.errorPage(w, r, http.StatusUnauthorized, "请先登录", "")
		}
		return nil, false
	}
	return u, true
}

func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request) (*User, bool) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return nil, false
	}
	if !a.isAdmin(u) || !a.cfg.adminIPOK(a.ip(r)) {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return nil, false
	}
	return u, true
}

func (a *App) flash(w http.ResponseWriter, msg string) {
	a.setCookie(w, "tflash", url.QueryEscape(msg), 120)
}

func (a *App) takeFlash(w http.ResponseWriter, r *http.Request) string {
	v := cookieVal(r, "tflash")
	if v == "" {
		return ""
	}
	a.setCookie(w, "tflash", "", -1)
	s, _ := url.QueryUnescape(v)
	return s
}

func (a *App) ip(r *http.Request) string { return clientIP(r, a.cfg.TrustProxy, a.cfg.TrustHeader) }

func (a *App) limited(w http.ResponseWriter, r *http.Request, key string, max int, window time.Duration) bool {
	if a.lim.allow(key+":"+a.ip(r), max, window) {
		return false
	}
	a.errorPage(w, r, http.StatusTooManyRequests, "手速太快了", "歇一会儿再来。")
	return true
}

func (a *App) logf(f string, args ...any) { log.Printf(f, args...) }

// newCode 带前缀的对外编号，撞库概率极低，撞了由唯一索引兜底后重试。
func newCode(prefix string) string { return prefix + "-" + randCode(5) }

func initial(s string) string {
	for _, r := range s {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func hue(key string) int {
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % 360)
}

// notify 站内通知（薄封装，便于以后接 Telegram）。
func (a *App) notify(userID int64, kind, title, body, link string) {
	a.st.Notify(userID, kind, title, body, link)
}

func (a *App) redirectBack(w http.ResponseWriter, r *http.Request, def string) {
	ref := r.Header.Get("Referer")
	if ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host == r.Host {
			http.Redirect(w, r, u.RequestURI(), http.StatusFound)
			return
		}
	}
	http.Redirect(w, r, def, http.StatusFound)
}

// ownerNext 发布方视角：这条记录下一步是什么、还剩多久。
func ownerNext(x *Submission) string {
	switch x.Status {
	case SClaimed:
		if x.LastError != "" {
			return "对方验证未通过，正在改（提交剩 " + left(x.ClaimExpiresAt) + "）"
		}
		if x.CheckNote != "" || x.CheckRejects > 0 {
			return "已退回，等对方重做后再提交（剩 " + left(x.ClaimExpiresAt) + "）"
		}
		return "等对方完成并提交（剩 " + left(x.ClaimExpiresAt) + "）"
	case SSubmit:
		return "推文验证中"
	case SChecking:
		return "请到 X 核对后确认（" + left(x.CheckingAt+checkWindowMs) + " 内不处理视为通过）"
	case SVerified:
		return "已发帖，留存至 " + fmtTime(x.RecheckDueAt) + " 复检"
	case SPayable:
		return "请付款，剩 " + left(x.PayDeadlineAt)
	case SOverdue:
		return "已逾期 " + ago(x.OverdueAt) + "，请立即付款"
	case SAwait:
		if x.UnderpaidE8 > 0 && x.TopupMarkedAt == 0 && x.TopupRequested > 0 {
			return "对方要求补差，请补付"
		}
		if x.UnderpaidE8 > 0 && x.TopupMarkedAt == 0 {
			return "少付，等对方选择"
		}
		return "等对方确认到账（登记于 " + ago(x.MarkedPaidAt) + "）"
	case SDisputed:
		return "申诉处理中"
	case SPaid:
		return "完成于 " + fmtTime(x.ConfirmedAt)
	case SVoid:
		return "作废：" + x.VoidReason
	case SExpired:
		return "对方超时未提交，名额已释放"
	case SDefault:
		return "违约记录（欠款公示中）"
	}
	return subStatus(x.Status)
}
