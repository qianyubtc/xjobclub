package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

type subPage struct {
	Base
	X            *Submission
	T            *Task
	Worker       *User
	Owner        *User
	IsWorker     bool
	IsOwner      bool
	Content      string
	IntentURL    string
	Timeline     []AuditRow
	Actors       map[int64]*User
	Payments     []*Payment
	ActivePay    *Payment
	QR           string
	Payee        *PayProfile
	Dispute      *Dispute
	Disputes     []*Dispute
	CanSubmit    bool
	CanPay       bool
	CanMark      bool
	CanPayNow    bool  // 发布方可提前付款（留存期内、无申诉）
	AutoAt       int64 // 待确认到账：不处理则于此刻视为已收到（等补差中为 0）
	CanConfirm   bool
	CanDone      bool     // 点赞/转发：接单方可提交核对
	CanCheck     bool     // 发布方可核对
	TargetIntent string   // 一键点赞/转发
	CheckDue     int64    // 核对期限
	DisputeOK    []string // 当前身份可发起的申诉类型
	Err          string
	Amount       string // 应付金额（含少付时的差额）
	TopupE8      int64
	Due          int64 // 应付金额（按浏览量结算过则为结算金额）
	EstE8        int64 // 按浏览量：当前预计报酬
	Anomaly      bool  // 浏览量远超粉丝数
	Cfg          *Config
}

func (a *App) loadSub(w http.ResponseWriter, r *http.Request) (*Submission, *Task, *User, bool) {
	u := a.currentUser(r)
	x, err := a.st.GetSubByCode(r.PathValue("code"))
	if err != nil {
		a.fail(w, r, err)
		return nil, nil, nil, false
	}
	if x == nil {
		a.errorPage(w, r, http.StatusNotFound, "记录不存在", "")
		return nil, nil, nil, false
	}
	t, _ := a.st.GetTaskByID(x.TaskID)
	if t == nil {
		a.errorPage(w, r, http.StatusNotFound, "记录不存在", "")
		return nil, nil, nil, false
	}
	// 只有当事双方、管理员、受邀陪审员（通过申诉页）能看
	if u == nil || (u.ID != x.WorkerID && u.ID != t.OwnerID && !a.isAdmin(u)) {
		if u == nil && r.Method == http.MethodGet {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return nil, nil, nil, false
		}
		a.errorPage(w, r, http.StatusNotFound, "记录不存在", "")
		return nil, nil, nil, false
	}
	return x, t, u, true
}

// disputeOptions 当前身份在当前状态下可发起的申诉类型。
func (a *App) disputeOptions(x *Submission, t *Task, u *User) []string {
	var out []string
	if open, _ := a.st.OpenDisputeForSub(x.ID); open != nil {
		return out
	}
	if u.ID == x.WorkerID {
		switch {
		case x.Status == SOverdue:
			out = append(out, "A")
		case x.Status == SAwait:
			out = append(out, "B")
		case x.Status == SPaid && x.ConfirmMethod == "auto" && (x.TopupMarkedAt > 0 || x.UnderpaidE8 == 0) && ms()-x.ConfirmedAt < a.cfg.AutoDisputeDays*dayMs:
			out = append(out, "B") // 自动完成后发现没到账
		case x.Status == SClaimed && x.LastError != "" && x.VerifyAttempts > 0:
			out = append(out, "C")
		case x.Status == SVoid && strings.HasPrefix(x.VoidReason, "发布方两次核对") && ms()-x.UpdatedAt < dayMs:
			out = append(out, "G") // 被发布方两次「没看到」作废，24 小时内可申诉
		}
	}
	if u.ID == t.OwnerID && x.Status == SPayable && (strings.HasPrefix(x.RecheckFlag, "复检未确认") || x.CheckAuto == 1 || t.Manual()) {
		out = append(out, "D")
	}
	return out
}

func qrBase64(link string) string {
	if link == "" {
		return ""
	}
	png, err := qrcode.Encode(link, qrcode.Medium, 220)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(png)
}

func (a *App) buildSubPage(w http.ResponseWriter, r *http.Request, x *Submission, t *Task, u *User, errMsg string) subPage {
	p := subPage{Base: a.base(w, r), X: x, T: t, Err: errMsg, Actors: map[int64]*User{}, Cfg: a.cfg}
	p.Worker, _ = a.st.GetUserByID(x.WorkerID)
	p.Owner, _ = a.st.GetUserByID(t.OwnerID)
	p.IsWorker = u.ID == x.WorkerID
	p.IsOwner = u.ID == t.OwnerID || a.isAdmin(u) // 管理员看得到发布方视角，但下面的付款类动作只给本人
	strict := u.ID == t.OwnerID
	if int(x.VariantIdx) < len(t.Contents) {
		p.Content = t.Contents[x.VariantIdx]
	} else if len(t.Contents) > 0 {
		p.Content = t.Contents[0]
	}
	p.IntentURL = intentFor(p.Content)
	switch t.Kind {
	case "reply":
		p.IntentURL = "https://x.com/intent/post?in_reply_to=" + t.TargetTweetID + "&text=" + urlEscape(p.Content)
	case "like":
		p.TargetIntent = "https://x.com/intent/like?tweet_id=" + t.TargetTweetID
	case "repost":
		p.TargetIntent = "https://x.com/intent/retweet?tweet_id=" + t.TargetTweetID
	}
	p.Timeline, _ = a.st.AuditFor("submission", x.ID)
	for _, row := range p.Timeline {
		if row.ActorID > 0 {
			if _, ok := p.Actors[row.ActorID]; !ok {
				if au, _ := a.st.GetUserByID(row.ActorID); au != nil {
					p.Actors[row.ActorID] = au
				}
			}
		}
	}
	p.Payments, _ = a.st.PaymentsForSub(x.ID)
	p.Payee, _ = a.st.GetPayProfile(x.WorkerID)
	p.Disputes, _ = a.st.DisputesForSub(x.ID)
	p.Dispute, _ = a.st.OpenDisputeForSub(x.ID)
	p.DisputeOK = a.disputeOptions(x, t, u)
	p.CanSubmit = p.IsWorker && (x.Status == SClaimed || (x.Status == SSubmit && x.NextVerifyAt == 0)) && x.VerifyAttempts < a.cfg.VerifyAttempts && x.ClaimExpiresAt > ms()
	if t.Manual() {
		p.CanSubmit = false
		p.CanDone = p.IsWorker && x.Status == SClaimed && x.ClaimExpiresAt > ms() && x.VerifyAttempts < a.cfg.VerifyAttempts
	}
	p.CanCheck = strict && x.Status == SChecking
	p.CheckDue = x.CheckingAt + checkWindowMs
	p.TopupE8 = 0
	p.Due = payAmount(x, t)
	if t.CPM() && x.AmountE8 == 0 {
		p.EstE8 = cpmAmount(t, x.Views)
		if x.Status == SVerified && ms()-x.ViewsAt > 10*60*1000 && a.lim.allow("views:"+strconv.FormatInt(x.ID, 10), 1, 10*time.Minute) {
			sid, tid := x.ID, x.TweetID
			safeGo("views", func() { a.sampleViews(sid, tid) }) // 记录页顺手刷新一次浏览量（异步，10 分钟一次）
		}
	}
	if t.CPM() && p.IsOwner && x.Views > 5000 && p.Worker != nil && p.Worker.Followers > 0 && x.Views > 50*p.Worker.Followers {
		p.Anomaly = true // 浏览量远超粉丝数：提示发布方留意刷量
	}
	if x.Status == SAwait && x.UnderpaidE8 > 0 && x.UnderpaidE8 < p.Due {
		p.TopupE8 = p.Due - x.UnderpaidE8
	}
	disputedOverdue := x.Status == SDisputed && x.PrevStatus == SOverdue
	payState := x.Status == SPayable || x.Status == SOverdue || disputedOverdue || p.TopupE8 > 0
	p.CanPay = strict && payState && p.Payee.Gateway() && a.gwc != nil
	noOpenD := p.Dispute == nil || p.Dispute.Type != "D"
	p.CanMark = strict && noOpenD && (x.Status == SPayable || x.Status == SOverdue || disputedOverdue || (p.TopupE8 > 0 && x.TopupMarkedAt == 0))
	p.CanConfirm = p.IsWorker && x.Status == SAwait
	p.CanPayNow = strict && x.Status == SVerified && p.Dispute == nil
	if x.Status == SAwait && a.cfg.AutoConfirmH > 0 && !(x.TopupMarkedAt == 0 && x.TopupRequested > 0) {
		p.AutoAt = max(x.MarkedPaidAt, x.TopupMarkedAt) + a.cfg.AutoConfirmH*hourMs
	}
	if p.TopupE8 > 0 {
		p.Amount = fmtE8(p.TopupE8)
	} else {
		p.Amount = fmtE8(p.Due)
	}
	if p.CanPay {
		kind := "gateway"
		if p.TopupE8 > 0 {
			kind = "topup"
		}
		p.ActivePay, _ = a.st.ActivePayment(x.ID, kind)
		if p.ActivePay != nil {
			p.QR = qrBase64(p.ActivePay.ReceiveLink)
		}
	}
	return p
}

func (a *App) handleSub(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	// 页面打开时顺手对一次网关（4 秒节流在 syncPayment 里）
	if ap, _ := a.st.ActivePayment(x.ID, "gateway"); ap != nil {
		a.syncPayment(ap)
		x, _ = a.st.GetSubByID(x.ID)
	}
	a.render(w, http.StatusOK, "sub", a.buildSubPage(w, r, x, t, u, ""))
}

func (a *App) handleSubStatus(w http.ResponseWriter, r *http.Request) {
	x, t, _, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	out := map[string]any{"status": x.Status, "text": subStatus(x.Status), "error": x.LastError}
	for _, kind := range []string{"gateway", "topup"} {
		if ap, _ := a.st.ActivePayment(x.ID, kind); ap != nil {
			a.syncPayment(ap)
			if np, _ := a.st.GetPaymentByID(ap.ID); np != nil {
				out["pay"] = map[string]any{"status": np.Status, "amount": np.PayAmount, "expires_at": np.ExpiresAt}
			}
		}
	}
	if nx, _ := a.st.GetSubByID(x.ID); nx != nil {
		out["status"], out["text"] = nx.Status, subStatus(nx.Status)
		if nx.Status == SPaid {
			out["paid_amount"] = fmtE8(nx.PaidAmountE8)
		}
	}
	_ = t
	replyJSON(w, http.StatusOK, out)
}

// handleSubmit 接单方回填推文链接。
func (a *App) handleSubmit(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != x.WorkerID {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	if a.limited(w, r, "submit", 10, time.Minute) {
		return
	}
	bad := func(m string) { a.render(w, http.StatusBadRequest, "sub", a.buildSubPage(w, r, x, t, u, m)) }
	if t.Manual() {
		bad("这个任务不需要提交链接，完成后点「提交核对」即可")
		return
	}
	if !(x.Status == SClaimed || (x.Status == SSubmit && x.NextVerifyAt == 0)) {
		bad("当前状态不能提交")
		return
	}
	if x.ClaimExpiresAt <= ms() {
		bad("接单时限已过")
		return
	}
	if x.VerifyAttempts >= a.cfg.VerifyAttempts {
		bad(fmt.Sprintf("已用完 %d 次提交机会", a.cfg.VerifyAttempts))
		return
	}
	url := strings.TrimSpace(r.FormValue("tweet_url"))
	id := tweetIDFrom(url)
	if id == "" {
		bad("推文链接不对，应形如 https://x.com/你的名字/status/1234567890")
		return
	}
	if n := a.st.count(`SELECT COUNT(*) FROM submissions WHERE tweet_id=? AND id<>?`, id, x.ID); n > 0 {
		bad("这条推文已经用过了，一条推文只能核销一条记录")
		return
	}
	if ok, err := a.st.SetSubmitted(x.ID, id, "https://x.com/i/web/status/"+id); err != nil || !ok {
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			bad("这条推文已经用过了")
			return
		}
		bad("当前状态不能提交")
		return
	}
	a.st.Audit(u.ID, "sub.submit", "submission", x.ID, map[string]any{"tweet": id}, a.ip(r))
	if nx, _ := a.st.GetSubByID(x.ID); nx != nil {
		a.verifySubmission(nx) // 同步验证，几秒内给结果
	}
	http.Redirect(w, r, x.Path(), http.StatusFound)
}

const checkWindowMs = 48 * hourMs // 点赞/转发：发布方核对期限，超时视为通过

// handleDone 点赞/转发任务：接单方声明已完成。转发先从公开时间线自动检测，检测到直接待付款；否则交发布方核对。
func (a *App) handleDone(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != x.WorkerID {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	if a.limited(w, r, "done", 10, time.Minute) {
		return
	}
	bad := func(m string) { a.render(w, http.StatusBadRequest, "sub", a.buildSubPage(w, r, x, t, u, m)) }
	if !t.Manual() {
		bad("这个任务需要提交推文链接")
		return
	}
	if x.Status != SClaimed {
		bad("当前状态不能提交")
		return
	}
	if x.ClaimExpiresAt <= ms() {
		bad("接单时限已过")
		return
	}
	if x.VerifyAttempts >= a.cfg.VerifyAttempts {
		bad(fmt.Sprintf("已用完 %d 次提交机会", a.cfg.VerifyAttempts))
		return
	}
	if t.Kind == "repost" {
		if found, ferr := a.fetchRetweeted(u.Handle, t.TargetTweetID); ferr == nil && found {
			if ok2, _ := a.st.SetCheckedOK(x.ID, []string{SClaimed}, ms()+t.PayWindowH*hourMs, 0); ok2 {
				a.st.Audit(u.ID, "sub.repost_detected", "submission", x.ID, map[string]any{"target": t.TargetTweetID}, a.ip(r))
				a.notify(u.ID, "verify", "已检测到你的转发，等待付款", fmt.Sprintf("发布方须在 %s 内付款 %s U。", dur(t.PayWindowH), fmtE8(payAmount(x, t))), x.Path())
				a.notify(t.OwnerID, "pay", "有一条记录待付款", fmt.Sprintf("@%s 已转发《%s》（已自动检测到），请在 %s 内付款 %s U。", u.Handle, t.Title, dur(t.PayWindowH), fmtE8(payAmount(x, t))), x.Path())
				a.flash(w, "已检测到你的转发，进入待付款")
				http.Redirect(w, r, x.Path(), http.StatusFound)
				return
			}
		}
	}
	if ok2, err := a.st.SetChecking(x.ID); err != nil || !ok2 {
		bad("当前状态不能提交")
		return
	}
	a.st.Audit(u.ID, "sub.checking", "submission", x.ID, nil, a.ip(r))
	a.notify(t.OwnerID, "task", "请核对 @"+u.Handle+" 是否已"+t.DoneVerb(), fmt.Sprintf("《%s》：到 X 打开目标推文核对，确认后进入待付款；48 小时不处理视为通过。", t.Title), x.Path())
	a.flash(w, "已提交，等发布方核对")
	http.Redirect(w, r, x.Path(), http.StatusFound)
}

// handleCheck 发布方核对点赞/转发：ok 进入待付款；no 退回重做（第二次未见即作废并释放名额）。
// handlePayNow 发布方提前付款：不等留存到期，立刻复检一次，通过即进入待付款。
// 放弃留存保护是发布方自己的选择；读不到推文时什么都不改，只提示稍后再试。
func (a *App) handlePayNow(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != t.OwnerID {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	if a.limited(w, r, "paynow", 20, 10*time.Minute) {
		return
	}
	bad := func(m string) { a.render(w, http.StatusBadRequest, "sub", a.buildSubPage(w, r, x, t, u, m)) }
	if x.Status != SVerified {
		bad("只有留存期内（已验证）的记录可以提前付款")
		return
	}
	if d, _ := a.st.OpenDisputeForSub(x.ID); d != nil {
		bad("有申诉进行中，先等裁决")
		return
	}
	msg := a.recheckSubmission(x, true)
	if nx, _ := a.st.GetSubByCode(x.Code); nx != nil {
		x = nx
	}
	switch x.Status {
	case SPayable:
		a.st.Audit(u.ID, "sub.paynow", "submission", x.ID, nil, a.ip(r))
		a.flash(w, "已进入待付款，按下面的金额付款即可")
		http.Redirect(w, r, x.Path()+"#pay", http.StatusFound)
	case SVoid:
		a.flash(w, "复检发现推文已不符合要求，记录作废，无需付款")
		http.Redirect(w, r, x.Path(), http.StatusFound)
	default:
		if msg == "" {
			msg = "暂时无法结算，请稍后再试"
		}
		bad(msg)
	}
}

func (a *App) handleCheck(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != t.OwnerID && !a.isAdmin(u) {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	if a.limited(w, r, "check", 60, 10*time.Minute) {
		return
	}
	bad := func(m string) { a.render(w, http.StatusBadRequest, "sub", a.buildSubPage(w, r, x, t, u, m)) }
	if x.Status != SChecking {
		bad("当前状态不能核对")
		return
	}
	handle := ""
	if wk, _ := a.st.GetUserByID(x.WorkerID); wk != nil {
		handle = wk.Handle
	}
	switch r.FormValue("action") {
	case "ok":
		if ok2, _ := a.st.SetCheckedOK(x.ID, []string{SChecking}, ms()+t.PayWindowH*hourMs, 0); !ok2 {
			bad("当前状态不能核对")
			return
		}
		a.st.Audit(u.ID, "sub.check_ok", "submission", x.ID, nil, a.ip(r))
		a.notify(x.WorkerID, "verify", "发布方已确认，等待付款", fmt.Sprintf("发布方须在 %s 内付款 %s U。", dur(t.PayWindowH), fmtE8(payAmount(x, t))), x.Path())
		a.flash(w, "已确认，进入待付款")
	case "no":
		note := cleanText(r.FormValue("note"), 200, false)
		if x.CheckRejects+1 >= 2 {
			if ok2, _ := a.st.SetVoid(x.ID, []string{SChecking}, "发布方两次核对都未见到"+t.DoneVerb()); !ok2 {
				bad("当前状态不能核对（可能已超时视为通过）")
				return
			}
			a.st.Audit(u.ID, "sub.check_void", "submission", x.ID, map[string]any{"note": note}, a.ip(r))
			a.notify(x.WorkerID, "verify", "记录作废", "发布方两次核对都没有看到你的"+t.DoneVerb()+"，名额已释放。如果你确实完成了，可在 24 小时内到记录页发起「核对争议」。"+note, x.Path())
			a.flash(w, "已作废并释放名额")
		} else {
			if ok2, _ := a.st.SetCheckRejected(x.ID, note); !ok2 {
				bad("当前状态不能核对（可能已超时视为通过）")
				return
			}
			a.st.Audit(u.ID, "sub.check_no", "submission", x.ID, map[string]any{"note": note}, a.ip(r))
			a.notify(x.WorkerID, "verify", "发布方没有看到你的"+t.DoneVerb(), "请确认是用 @"+handle+" 完成的，然后重新提交核对。"+note, x.Path())
			a.flash(w, "已退回给对方重做")
		}
	default:
		bad("参数不对")
		return
	}
	http.Redirect(w, r, x.Path(), http.StatusFound)
}

// handlePayMark 发布方手动登记已付（手动模式，或网关看不见时的兜底）。
func (a *App) handlePayMark(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != t.OwnerID {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	if a.limited(w, r, "mark", 20, 10*time.Minute) {
		return
	}
	boid := strings.TrimSpace(r.FormValue("binance_order_id"))
	note := cleanText(r.FormValue("note"), 200, false)
	bad := func(m string) { a.render(w, http.StatusBadRequest, "sub", a.buildSubPage(w, r, x, t, u, m)) }
	if via := r.FormValue("via"); via == "other" {
		// 通过接单方自定义的收款方式付款：没有币安订单号，改填交易哈希/凭证；平台不核验，接单方手动确认
		ref := cleanText(r.FormValue("ref"), 120, false)
		label := cleanText(r.FormValue("method"), 20, false)
		if len([]rune(ref)) < 6 {
			bad("请填写交易哈希或付款凭证（至少 6 个字符）")
			return
		}
		boid = ""
		note = strings.TrimSpace("经「" + label + "」付款，凭证 " + ref + " " + note)
	} else if !reBinanceOrder.MatchString(boid) {
		bad("请填写币安 App 转账详情里的 18 位订单编号")
		return
	}
	ok2, err := a.st.SetMarkedPaid(x.ID, boid, note)
	if err != nil {
		bad(err.Error())
		return
	}
	if !ok2 {
		bad("当前状态不能标记")
		return
	}
	a.st.Audit(u.ID, "sub.mark_paid", "submission", x.ID, map[string]any{"binance_order_id": boid}, a.ip(r))
	a.closePendingForSub(x.ID)
	if d, _ := a.st.OpenDisputeForSub(x.ID); d != nil && d.Type == "A" {
		a.st.ResolveDispute(d.ID, "marked_paid", "发布方已登记付款，转待接单方确认；未收到可再发起申诉", 0)
		a.st.Audit(u.ID, "dispute.auto_close", "dispute", d.ID, map[string]any{"reason": "marked_paid"}, a.ip(r))
	}
	ref := boid
	if ref == "" {
		ref = note
	}
	a.notify(x.WorkerID, "pay", "发布方已标记付款，请核对到账", fmt.Sprintf("任务 %s 的 %s U，%s。请核对后点「已收到」；没收到请发起申诉。%s", t.Code, fmtE8(payAmount(x, t)), ref, a.autoConfirmHint()), x.Path())
	a.flash(w, "已登记，等待接单方确认")
	http.Redirect(w, r, x.Path(), http.StatusFound)
}

// handleConfirm 接单方确认收到。
func (a *App) handleConfirm(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != x.WorkerID {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	if x.Status != SAwait {
		a.flash(w, "当前状态不能确认")
		http.Redirect(w, r, x.Path(), http.StatusFound)
		return
	}
	amount := payAmount(x, t)
	if x.UnderpaidE8 > 0 && x.TopupMarkedAt == 0 {
		amount = x.UnderpaidE8
	}
	ok2, err := a.st.SetPaid(x.ID, "manual", amount)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !ok2 {
		a.flash(w, "当前状态不能确认")
		http.Redirect(w, r, x.Path(), http.StatusFound)
		return
	}
	a.afterPaid(x, t, u.ID, "manual")
	a.flash(w, "已确认收到，记录完成")
	http.Redirect(w, r, x.Path(), http.StatusFound)
}

// handleUnderpaid 少付：接单方选择接受实付或要求补差。
func (a *App) handleUnderpaid(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != x.WorkerID || x.Status != SAwait || x.UnderpaidE8 <= 0 {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	switch r.FormValue("action") {
	case "accept":
		if ok2, _ := a.st.SetPaid(x.ID, "gateway", x.UnderpaidE8); ok2 {
			a.afterPaid(x, t, u.ID, "gateway")
			a.flash(w, "已按实付金额完成")
		}
	case "topup":
		if ok, _ := a.st.SetTopupRequested(x.ID); !ok {
			a.flash(w, "已经要求过补差，等发布方处理")
			break
		}
		a.notify(t.OwnerID, "pay", "接单方要求补足差额", fmt.Sprintf("任务 %s 实付 %s U，少 %s U，请补付。", t.Code, fmtE8(x.UnderpaidE8), fmtE8(payAmount(x, t)-x.UnderpaidE8)), x.Path())
		a.st.Audit(u.ID, "sub.topup_requested", "submission", x.ID, nil, a.ip(r))
		a.flash(w, "已通知发布方补差")
	}
	http.Redirect(w, r, x.Path(), http.StatusFound)
}

// autoConfirmHint 待确认到账的自动完成预告（功能关闭时为空）。
func (a *App) autoConfirmHint() string {
	if a.cfg.AutoConfirmH <= 0 {
		return ""
	}
	return fmt.Sprintf("%s 内不处理会视为已收到、自动完成（之后 %d 天内仍可申诉）。", dur(a.cfg.AutoConfirmH), a.cfg.AutoDisputeDays)
}

// autoConfirmAwaiting 待确认到账超过 AutoConfirmH 未处理：视为已收到，自动完成（类似电商的自动收货）。
// 等发布方补差中的记录不算——卡在发布方那边。自动完成不计信用（confirm_method=auto），
// 之后 AutoDisputeDays 内接单方仍可发起 B 类申诉，成立则发布方照样上黑名单。
func (a *App) autoConfirmAwaiting(now int64) {
	if a.cfg.AutoConfirmH <= 0 {
		return
	}
	cut := now - a.cfg.AutoConfirmH*hourMs
	// 已举报过（A 类）的记录不自动完成：接单方已经在追，必须由本人确认，否则"先赖账再假标记"就能靠对方沉默洗白并解冻
	rows, err := a.st.querySubs(`WHERE status='awaiting_confirm' AND reported_at=0 AND ((topup_marked_at>0 AND topup_marked_at<?) OR (topup_marked_at=0 AND topup_requested_at=0 AND marked_paid_at>0 AND marked_paid_at<?))
		AND NOT EXISTS (SELECT 1 FROM disputes d WHERE d.submission_id=submissions.id AND d.status<>'resolved') ORDER BY id LIMIT 200`, cut, cut)
	if err != nil {
		return
	}
	for _, x := range rows {
		t, _ := a.st.GetTaskByID(x.TaskID)
		if t == nil {
			continue
		}
		amount := payAmount(x, t)
		if x.UnderpaidE8 > 0 && x.TopupMarkedAt == 0 {
			amount = x.UnderpaidE8 // 网关确认少付、接单方没要求补差：按实付完成
		}
		if ok, _ := a.st.SetAutoPaid(x.ID, amount); !ok {
			continue // 状态已变或刚被申诉：交给正常流程 / 裁决
		}
		a.st.Audit(0, "sub.auto_confirm", "submission", x.ID, map[string]any{"amount": fmtE8(amount)}, "")
		a.settlePaid(x, t, 0, "auto", false) // 自动完成不是到账证据，绝不替申诉结案
	}
}

// afterPaid 自然付款路径（网关核销 / 接单方确认 / 管理员置为完成）的收尾：审计、通知、解冻、关单、自动结掉 A/B 申诉。
func (a *App) afterPaid(x *Submission, t *Task, actor int64, method string) {
	a.settlePaid(x, t, actor, method, true)
}

// settlePaid touchDispute=false 用于裁决路径：申诉由 applyResolution 自己结案，不重复结案也不记警告。
func (a *App) settlePaid(x *Submission, t *Task, actor int64, method string, touchDispute bool) {
	a.st.Audit(actor, "sub.paid", "submission", x.ID, map[string]any{"method": method}, "")
	if x.SelfDeal == 0 && a.st.SelfDealing(t.OwnerID, x.WorkerID) {
		// 接单时没露馅、付款确认时同一网段 / 同一付款账户：照样不计信用（接单时的判定只是快照）
		a.st.db.Exec(`UPDATE submissions SET self_deal=1 WHERE id=?`, x.ID)
		x.SelfDeal = 1
	}
	a.closePendingForSub(x.ID)
	if a.st.PublisherFrozen(t.OwnerID) == 0 {
		a.st.UnfreezeOwnerTasks(t.OwnerID)
	}
	if method == "auto" && x.UnderpaidE8 > 0 && x.TopupMarkedAt == 0 {
		// 网关确认少付、接单方没要求补差：按实付完成，没有"没收到"可申诉
		a.notify(t.OwnerID, "pay", "一条记录已按实付完成", fmt.Sprintf("任务 %s：网关确认实付 %s U，接单方 %s 内未要求补差，已按实付完成。", t.Code, fmtE8(x.UnderpaidE8), dur(a.cfg.AutoConfirmH)), x.Path())
		a.notify(x.WorkerID, "pay", "待确认到账已按实付完成", fmt.Sprintf("任务 %s：网关确认到账 %s U（少于应付），你 %s 内未要求补差，已按实付完成。", t.Code, fmtE8(x.UnderpaidE8), dur(a.cfg.AutoConfirmH)), x.Path())
	} else if method == "auto" {
		a.notify(t.OwnerID, "pay", "一条记录已自动完成", fmt.Sprintf("任务 %s：接单方 %s 内未确认到账，已视为已收到。对方 %d 天内仍可申诉未收到，届时会通知你。", t.Code, dur(a.cfg.AutoConfirmH), a.cfg.AutoDisputeDays), x.Path())
		a.notify(x.WorkerID, "pay", "待确认到账已自动完成", fmt.Sprintf("任务 %s：你 %s 内没有处理，已按已收到完成。如果实际没有收到，%d 天内可在记录页发起申诉。", t.Code, dur(a.cfg.AutoConfirmH), a.cfg.AutoDisputeDays), x.Path())
	} else {
		a.notify(t.OwnerID, "pay", "一条记录已完成", fmt.Sprintf("任务 %s：接单方已确认收款。", t.Code), x.Path())
		a.notify(x.WorkerID, "pay", "记录已完成", fmt.Sprintf("任务 %s 的报酬已确认到账。", t.Code), x.Path())
	}
	if !touchDispute {
		return
	}
	if d, _ := a.st.OpenDisputeForSub(x.ID); d != nil && (d.Type == "A" || d.Type == "B") {
		a.st.ResolveDispute(d.ID, "resolved_by_payment", "款项已确认到账，申诉自动结案", 0)
		a.st.Audit(0, "dispute.auto_close", "dispute", d.ID, nil, "")
		if d.Type == "B" && method != "admin" && x.MarkedPaidAt > 0 && x.MarkedPaidAt < d.CreatedAt {
			// 先标记、被申诉后款才到：记一次虚假标记警告
			if owner, _ := a.st.GetUserByID(t.OwnerID); owner != nil {
				a.warnPublisher(owner, "标记已付后款项才到账（B 类申诉期间）", d.ID, x.Path(), "")
			}
		}
	}
}
