package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	bpaygate "github.com/qianyubtc/BinancePayTool/sdk/go"
)

var errNoGateway = errors.New("未配置支付网关")

func gatewayErr(err error) string {
	var ae *bpaygate.APIError
	if errors.As(err, &ae) {
		if ae.Message != "" {
			return ae.Message
		}
		return "网关返回 " + ae.Code
	}
	if errors.Is(err, errNoGateway) {
		return "本站未配置支付网关"
	}
	return "网关连不上，请稍后再试"
}

// createOrder 为一条记录开一张网关结账会话（30 分钟）。
func (a *App) createOrder(x *Submission, t *Task, payee *PayProfile, amountE8 int64, kind string, payerID int64) (*Payment, error) {
	if a.gwc == nil {
		return nil, errNoGateway
	}
	n := a.st.count(`SELECT COUNT(*) FROM payments WHERE submission_id=?`, x.ID) + 1
	mid := fmt.Sprintf("TLM-%s-%d", x.Code, n)
	o, err := a.gwc.CreateOrder(bpaygate.CreateOrderReq{AccountID: payee.BPGAccountID, MerchantOrderID: mid, Currency: t.Currency, Amount: fmtE8(amountE8),
		CallbackURL: a.callbackURL(), ReturnURL: a.cfg.BaseURL + x.Path(), Timeout: a.cfg.OrderTTL})
	if err != nil {
		return nil, err
	}
	p := &Payment{SubmissionID: x.ID, Kind: kind, UserID: payerID, BPGOrderID: o.OrderID, MerchantOrderID: mid, PayAmount: o.PayAmount, BaseE8: amountE8, NoteCode: o.NoteCode,
		PayURL: o.PayURL, ReceiveUID: o.ReceiveUID, ReceiveLink: o.ReceiveLink, Status: "pending", ExpiresAt: o.ExpiresAt}
	id, err := a.st.CreatePayment(p)
	if err != nil {
		return nil, err
	}
	p.ID = id
	return p, nil
}

func (a *App) callbackURL() string {
	if a.cfg.CallbackURL != "" {
		return a.cfg.CallbackURL
	}
	// 网关与平台同机时走内网地址，不绕公网；通配监听地址换成回环
	host, port, err := net.SplitHostPort(a.cfg.Listen)
	if err != nil {
		return "http://127.0.0.1:8125/bpg/notify"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/bpg/notify"
}

// handlePayOrder 发布方点「去付款」：开（或复用）网关订单。
func (a *App) handlePayOrder(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != t.OwnerID {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	if a.limited(w, r, "payorder", 20, 10*time.Minute) {
		return
	}
	payee, _ := a.st.GetPayProfile(x.WorkerID)
	if !payee.Gateway() || a.gwc == nil {
		a.flash(w, "接单方未绑定自动到账，请按面板手动转账并登记订单号")
		http.Redirect(w, r, x.Path(), http.StatusFound)
		return
	}
	amount, kind := payAmount(x, t), "gateway"
	switch {
	case x.Status == SPayable || x.Status == SOverdue || (x.Status == SDisputed && x.PrevStatus == SOverdue):
	case x.Status == SAwait && x.UnderpaidE8 > 0 && x.UnderpaidE8 < payAmount(x, t):
		amount, kind = payAmount(x, t)-x.UnderpaidE8, "topup"
	default:
		a.flash(w, "当前状态不需要付款")
		http.Redirect(w, r, x.Path(), http.StatusFound)
		return
	}
	if ap, _ := a.st.ActivePayment(x.ID, kind); ap == nil {
		if _, err := a.createOrder(x, t, payee, amount, kind, u.ID); err != nil {
			a.flash(w, "下单失败："+gatewayErr(err)+"。可以直接转账后登记订单号。")
		}
	}
	http.Redirect(w, r, x.Path()+"#pay", http.StatusFound)
}

// handlePayClaim 网关订单「付了没变绿」：把币安订单编号转给网关回填接口。
func (a *App) handlePayClaim(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if u.ID != t.OwnerID || a.gwc == nil {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	if a.limited(w, r, "payclaim", 5, time.Minute) {
		return
	}
	boid := strings.TrimSpace(r.FormValue("binance_order_id"))
	isFetch := r.Header.Get("X-Requested-With") == "fetch"
	reply := func(status int, v map[string]any) {
		if isFetch {
			replyJSON(w, status, v)
			return
		}
		if m, _ := v["msg"].(string); m != "" {
			a.flash(w, m)
		}
		http.Redirect(w, r, x.Path()+"#pay", http.StatusFound)
	}
	if !reBinanceOrder.MatchString(boid) {
		reply(http.StatusBadRequest, map[string]any{"ok": false, "msg": "订单编号应为 18 位数字"})
		return
	}
	var p *Payment
	for _, kind := range []string{"gateway", "topup"} {
		if p, _ = a.st.ActivePayment(x.ID, kind); p != nil {
			break
		}
	}
	if p == nil {
		// 没有在途会话：退回手动登记流程
		reply(http.StatusOK, map[string]any{"ok": false, "manual": true, "msg": "结账会话已过期，请用「手动登记」提交这个订单号"})
		return
	}
	code, err := a.gatewayClaim(p, boid)
	if err != nil {
		reply(http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.syncPaymentNow(p)
	msgs := map[string]string{"OK": "核对成功，已到账", "UNDERPAID": "查到了这笔转账但金额不足", "NOT_FOUND": "没查到这笔转账：确认订单编号与收款账号，币安到账后再试，或改用手动登记", "CONSUMED": "这笔转账已被别的订单用过了", "CURRENCY": "币种不对", "STATE": "订单当前状态不能回填"}
	reply(http.StatusOK, map[string]any{"ok": code == "OK", "code": code, "msg": msgs[code]})
}

func (a *App) gatewayClaim(p *Payment, boid string) (string, error) {
	token := p.PayURL[strings.LastIndex(p.PayURL, "/")+1:]
	body, _ := json.Marshal(map[string]string{"binance_order_id": boid})
	req, _ := http.NewRequest("POST", a.cfg.BPGURL+"/pay/"+token+"/claim", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.gwc.HC.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("网关响应异常 (HTTP %d)", resp.StatusCode)
	}
	if env.Code != "OK" {
		if env.Code == "ERR_RATE_LIMIT" {
			return "", errors.New("试得太频繁了，一分钟后再试")
		}
		return "", errors.New(env.Message)
	}
	return env.Data.Code, nil
}

// ---- 网关状态落地 ----

// applyGateway 把网关侧订单状态落到本地并推进接单记录（幂等）。付款方锚定优先用 counterparty_id（稳定标识），缺失退回 payer_id。
func (a *App) applyGateway(p *Payment, status, actual, payAmount, matchedBy, boid, payerID, counterpartyID string, paidAt int64, raw string) {
	if counterpartyID != "" {
		payerID = counterpartyID
	}
	switch status {
	case "paid", "underpaid":
		amt := actual
		if amt == "" {
			amt = payAmount
		}
		e8, err := parseAmountE8(amt, 8)
		if err != nil || e8 <= 0 {
			if status != "paid" {
				log.Printf("[error] %s 实付金额无法解析 %q", p.MerchantOrderID, amt)
				return
			}
			e8 = p.BaseE8
		}
		if paidAt <= 0 {
			paidAt = ms()
		}
		changed, err := a.st.SettlePayment(p.ID, status, e8, matchedBy, boid, payerID, paidAt, raw)
		if err != nil || !changed {
			return
		}
		log.Printf("[info] 到账 %s %s status=%s by %s payer=%s", p.MerchantOrderID, fmtE8(e8), status, matchedBy, payerID)
		a.onPaid(p, e8, payerID, status == "paid")
	case "expired", "closed":
		a.st.SettlePayment(p.ID, status, 0, "", "", "", 0, raw)
	}
}

// onPaid 网关确认到账后推进业务。full 以网关判定为准（status=paid）：唯一金额可能带减尾数（如 0.1 → 0.0928），
// 实付按面板金额一分不差就是足额，不能拿实付与基础金额比较；只有网关报 underpaid 才是少付。
func (a *App) onPaid(p *Payment, actualE8 int64, payerID string, full bool) {
	if p.Kind == "cert" {
		u, _ := a.st.GetUserByID(p.UserID)
		if u == nil {
			return
		}
		enough := full
		if err := a.st.SetPayerID(u.ID, payerID, enough); errors.Is(err, ErrPayerTaken) {
			a.st.Audit(0, "user.payer_conflict", "user", u.ID, map[string]any{"payer_id": payerID}, "")
			a.notify(u.ID, "account", "认证付款的付款账户已被其他账号使用", "同一个币安账户只能认证一个平台账号，请联系管理员。", "/me/pay")
			return
		}
		if !enough {
			a.notify(u.ID, "account", "认证付款金额不足", fmt.Sprintf("实付 %s U，需要 %s U。", fmtE8(actualE8), a.cfg.CertFeeAmount), "/me/cert")
			return
		}
		a.st.db.Exec(`UPDATE users SET cert_paid_at=CASE WHEN cert_paid_at=0 THEN ? ELSE cert_paid_at END WHERE id=?`, ms(), u.ID)
		a.st.Audit(u.ID, "user.cert_paid", "user", u.ID, map[string]any{"payer_id": payerID}, "")
		a.notify(u.ID, "account", "认证付款已确认", "现在可以发布任务了。", "/new")
		return
	}
	x, _ := a.st.GetSubByID(p.SubmissionID)
	if x == nil {
		return
	}
	t, _ := a.st.GetTaskByID(x.TaskID)
	if t == nil {
		return
	}
	// 付款方身份锚：软匹配，不阻断
	if payerID != "" {
		if owner, _ := a.st.GetUserByID(t.OwnerID); owner != nil {
			if owner.PayerID == "" {
				if err := a.st.SetPayerID(owner.ID, payerID, false); errors.Is(err, ErrPayerTaken) {
					a.st.Audit(0, "pay.payer_conflict", "user", owner.ID, map[string]any{"payer_id": payerID, "submission": x.Code}, "")
					log.Printf("[warn] 付款账户冲突：用户 #%d 用了已认证给他人的 payer %s（记录 %s）", owner.ID, payerID, x.Code)
					if a.st.BlacklistedIdentity("", "", payerID) {
						a.notifyAdmins("付款账户命中黑名单", fmt.Sprintf("发布方 @%s（#%d）的付款账户与黑名单中的账号相同，疑似换号规避，请到后台处理。记录 %s。", owner.Handle, owner.ID, x.Code), x.Path())
					}
				}
			} else if owner.PayerID != payerID {
				a.st.Audit(0, "pay.payer_mismatch", "submission", x.ID, map[string]any{"expected": owner.PayerID, "got": payerID}, "")
			}
		}
	}
	total := actualE8
	if p.Kind == "topup" {
		total = x.UnderpaidE8 + actualE8
	}
	switch x.Status {
	case SPayable, SOverdue, SAwait, SDisputed:
		if full {
			if ok, _ := a.st.SetPaid(x.ID, "gateway", total); ok {
				a.afterPaid(x, t, 0, "gateway")
			}
		} else if p.Kind != "topup" {
			if ok, _ := a.st.SetUnderpaid(x.ID, actualE8); !ok {
				a.st.Audit(0, "pay.unexpected", "submission", x.ID, map[string]any{"actual": fmtE8(actualE8), "status": x.Status}, "")
				a.notify(x.WorkerID, "pay", "收到一笔少付的款项（记录状态特殊）", fmt.Sprintf("任务 %s 收到 %s U，少于应付 %s U，但记录当前处于「%s」，请联系管理员处理。", t.Code, fmtE8(actualE8), fmtE8(payAmount(x, t)), subStatus(x.Status)), x.Path())
				a.notify(t.OwnerID, "pay", "付款金额不足", fmt.Sprintf("任务 %s 实付 %s U，少于应付 %s U。", t.Code, fmtE8(actualE8), fmtE8(payAmount(x, t))), x.Path())
			} else {
				a.st.Audit(0, "sub.underpaid", "submission", x.ID, map[string]any{"actual": fmtE8(actualE8)}, "")
				a.notify(x.WorkerID, "pay", "收到一笔少付的款项", fmt.Sprintf("任务 %s 应付 %s U，实收 %s U。你可以接受实付完成，或要求补差。", t.Code, fmtE8(payAmount(x, t)), fmtE8(actualE8)), x.Path())
				a.notify(t.OwnerID, "pay", "付款金额不足", fmt.Sprintf("任务 %s 应付 %s U，实付 %s U，请等待接单方选择或补差。", t.Code, fmtE8(payAmount(x, t)), fmtE8(actualE8)), x.Path())
			}
		} else {
			a.st.db.Exec(`UPDATE submissions SET underpaid_e8=?, updated_at=? WHERE id=?`, total, ms(), x.ID)
			a.notify(x.WorkerID, "pay", "收到补差，但仍不足", fmt.Sprintf("累计实收 %s U，应付 %s U。", fmtE8(total), fmtE8(payAmount(x, t))), x.Path())
		}
	case SDefault:
		a.st.SetRepaid(x.ID, "gateway", actualE8)
		a.st.RepayBlacklist(t.OwnerID, actualE8)
		a.st.Audit(0, "sub.repaid", "submission", x.ID, map[string]any{"actual": fmtE8(actualE8)}, "")
		a.notify(x.WorkerID, "pay", "违约记录已补付", fmt.Sprintf("任务 %s 的 %s U 已到账。", t.Code, fmtE8(actualE8)), x.Path())
		a.notify(t.OwnerID, "pay", "补付已确认", "黑名单记录会标注「已补付」；解除需通过申诉。", x.Path())
	default:
		a.st.Audit(0, "pay.unexpected", "submission", x.ID, map[string]any{"status": x.Status, "actual": fmtE8(actualE8)}, "")
		a.notify(t.OwnerID, "pay", "收到一笔多付/重复付款", fmt.Sprintf("记录 %s 已是「%s」状态，又收到 %s U，请与接单方自行协商。", x.Code, subStatus(x.Status), fmtE8(actualE8)), x.Path())
		a.notify(x.WorkerID, "pay", "收到一笔多付/重复付款", fmt.Sprintf("记录 %s 又收到 %s U，请与发布方协商处理。", x.Code, fmtE8(actualE8)), x.Path())
	}
}

// syncPayment 主动查单（4 秒节流）。
func (a *App) syncPayment(p *Payment) {
	if a.gwc == nil || p.BPGOrderID == "" || (p.Status != "pending" && p.Status != "expired") {
		return
	}
	if !a.st.TouchSync(p.ID, ms(), 4000) {
		return
	}
	a.syncPaymentNow(p)
}

func (a *App) syncPaymentNow(p *Payment) {
	o, err := a.gwc.GetOrder(p.BPGOrderID)
	if err != nil {
		log.Printf("[warn] 查单 %s: %v", p.MerchantOrderID, err)
		return
	}
	a.applyGateway(p, o.Status, o.ActualAmount, o.PayAmount, o.MatchedBy, o.BinanceOrderID, o.PayerID, o.CounterpartyID, o.PaidAt, "")
}

// closePendingForSub 记录到达终态 / 已登记手动付款：关掉名下仍 pending 的网关订单，释放唯一金额。
func (a *App) closePendingForSub(subID int64) {
	ps, _ := a.st.PendingForSub(subID)
	for _, p := range ps {
		if a.gwc != nil {
			a.gwc.CloseOrder(p.BPGOrderID)
		}
		a.st.SettlePayment(p.ID, "closed", 0, "", "", "", 0, "")
	}
}

// handleNotify 网关回调：验签后按 merchant_order_id 落状态。
func (a *App) handleNotify(w http.ResponseWriter, r *http.Request) {
	if a.gwc == nil {
		http.Error(w, "no gateway", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	cb, err := bpaygate.VerifyCallback(r.Header, body, a.cfg.BPGKey, 5*time.Minute)
	if err != nil {
		log.Printf("[warn] 回调验签失败: %v", err)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	p, err := a.st.GetPaymentByMerchant(cb.MerchantOrderID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if p == nil || (cb.OrderID != "" && cb.OrderID != p.BPGOrderID) {
		http.Error(w, "unknown order", http.StatusNotFound)
		return
	}
	a.applyGateway(p, cb.Status, cb.ActualAmount, cb.PayAmount, cb.MatchedBy, cb.BinanceOrderID, cb.PayerID, cb.CounterpartyID, cb.PaidAt, string(body))
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// ---- 认证付款 ----

type certPage struct {
	Base
	P      *Payment
	QR     string
	Amount string
	Err    string
}

func (a *App) handleCertGet(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !a.cfg.CertFeeEnabled {
		http.Redirect(w, r, "/new", http.StatusFound)
		return
	}
	p, _ := a.st.ActiveCertPayment(u.ID)
	if p != nil {
		a.syncPayment(p)
		p, _ = a.st.GetPaymentByID(p.ID)
	}
	pg := certPage{Base: a.base(w, r), P: p, Amount: a.cfg.CertFeeAmount}
	if p != nil {
		pg.QR = qrBase64(p.ReceiveLink)
	}
	a.render(w, http.StatusOK, "certfee", pg)
}

func (a *App) handleCertPost(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !a.cfg.CertFeeEnabled || a.gwc == nil || u.CertPaidAt > 0 {
		http.Redirect(w, r, "/me/cert", http.StatusFound)
		return
	}
	if a.limited(w, r, "cert", 5, 10*time.Minute) {
		return
	}
	if p, _ := a.st.ActiveCertPayment(u.ID); p == nil {
		n := a.st.count(`SELECT COUNT(*) FROM payments WHERE user_id=? AND kind='cert'`, u.ID) + 1
		mid := fmt.Sprintf("TLM-CERT-%d-%d", u.ID, n)
		o, err := a.gwc.CreateOrder(bpaygate.CreateOrderReq{MerchantOrderID: mid, Currency: a.cfg.Currency, Amount: a.cfg.CertFeeAmount, CallbackURL: a.callbackURL(), ReturnURL: a.cfg.BaseURL + "/me/cert", Timeout: a.cfg.OrderTTL})
		if err != nil {
			a.flash(w, "下单失败："+gatewayErr(err))
		} else {
			a.st.CreatePayment(&Payment{Kind: "cert", UserID: u.ID, BPGOrderID: o.OrderID, MerchantOrderID: mid, PayAmount: o.PayAmount, BaseE8: a.cfg.CertFeeE8, NoteCode: o.NoteCode,
				PayURL: o.PayURL, ReceiveUID: o.ReceiveUID, ReceiveLink: o.ReceiveLink, Status: "pending", ExpiresAt: o.ExpiresAt})
		}
	}
	http.Redirect(w, r, "/me/cert", http.StatusFound)
}

func (a *App) handleCertStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if p, _ := a.st.ActiveCertPayment(u.ID); p != nil {
		a.syncPayment(p)
	}
	fresh, _ := a.st.GetUserByID(u.ID)
	replyJSON(w, http.StatusOK, map[string]any{"paid": fresh != nil && fresh.CertPaidAt > 0})
}

// ---- 收款设置 ----

type paySettingsPage struct {
	Base
	P       *PayProfile
	Err     string
	Welcome bool
	Cert    bool
}

func (a *App) handlePaySettings(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	p, _ := a.st.GetPayProfile(u.ID)
	a.render(w, http.StatusOK, "paysettings", paySettingsPage{Base: a.base(w, r), P: p, Cert: a.cfg.CertFeeEnabled && u.CertPaidAt == 0})
}

func (a *App) handlePayUID(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	uid := strings.TrimSpace(r.FormValue("uid"))
	uid2 := strings.TrimSpace(r.FormValue("uid2"))
	email := cleanText(r.FormValue("email"), 80, false)
	switch {
	case !reUID.MatchString(uid):
		a.flash(w, "币安 UID 应为纯数字（币安 App 头像页可见）")
	case uid != uid2:
		a.flash(w, "两次输入的 UID 不一致，请核对——填错了钱会转给别人")
	case a.st.BlacklistedIdentity("", uid, ""):
		a.flash(w, "这个币安 UID 关联着黑名单中的账号，不能使用")
	case email != "" && !strings.Contains(email, "@"):
		a.flash(w, "邮箱格式不对")
	default:
		old, _ := a.st.GetPayProfile(u.ID)
		p := &PayProfile{UserID: u.ID, BinanceUID: uid, ReceiveEmail: email, Mode: "manual"}
		if old != nil && old.Gateway() {
			p.Mode, p.BPGAccountID, p.APIKeyMasked, p.BPGLastOK, p.BPGLastErr = old.Mode, old.BPGAccountID, old.APIKeyMasked, old.BPGLastOK, old.BPGLastErr
		}
		if err := a.st.UpsertPayProfile(p); err != nil {
			a.flash(w, err.Error())
		} else {
			a.st.Audit(u.ID, "pay.set_uid", "user", u.ID, nil, a.ip(r))
			a.flash(w, "收款 UID 已保存")
		}
	}
	http.Redirect(w, r, "/me/pay", http.StatusFound)
}

func (a *App) handlePayBind(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "bind", 5, 10*time.Minute) {
		return
	}
	if a.gwc == nil {
		a.flash(w, "本站未配置支付网关，暂不支持自动到账")
		http.Redirect(w, r, "/me/pay", http.StatusFound)
		return
	}
	key, secret, uid := strings.TrimSpace(r.FormValue("api_key")), strings.TrimSpace(r.FormValue("api_secret")), strings.TrimSpace(r.FormValue("uid"))
	if len(key) < 16 || len(secret) < 16 || !reUID.MatchString(uid) {
		a.flash(w, "请完整填写 API Key、Secret 与 UID")
		http.Redirect(w, r, "/me/pay", http.StatusFound)
		return
	}
	acct, err := a.gwc.CreateAccount(bpaygate.CreateAccountReq{Label: "tlm:" + strconv.FormatInt(u.ID, 10), APIKey: key, APISecret: secret, UID: uid})
	if err != nil {
		a.flash(w, "绑定失败："+gatewayErr(err))
		http.Redirect(w, r, "/me/pay", http.StatusFound)
		return
	}
	old, _ := a.st.GetPayProfile(u.ID)
	p := &PayProfile{UserID: u.ID, BinanceUID: uid, Mode: "gateway", BPGAccountID: acct.AccountID, APIKeyMasked: acct.APIKeyMasked, BPGLastOK: ms()}
	if old != nil {
		p.ReceiveEmail = old.ReceiveEmail
	}
	if err := a.st.UpsertPayProfile(p); err != nil {
		a.gwc.DisableAccount(acct.AccountID) // 本站没记下这个账号，别让网关白轮询
		if errors.Is(err, ErrAccountTaken) {
			a.flash(w, "这把 API Key 已经被另一个账号绑定了")
		} else {
			a.flash(w, err.Error())
		}
		http.Redirect(w, r, "/me/pay", http.StatusFound)
		return
	}
	a.st.Audit(u.ID, "pay.bind_key", "user", u.ID, map[string]any{"account": acct.AccountID}, a.ip(r))
	a.flash(w, "绑定成功：网关已用这把 Key 成功读取到你账户的 Pay 流水。请再核对一遍 UID 与这把 Key 属于同一个币安账户，否则别人付的款会进到别处且无法自动确认。")
	http.Redirect(w, r, "/me/pay", http.StatusFound)
}

func (a *App) handlePayUnbind(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	p, _ := a.st.GetPayProfile(u.ID)
	if p == nil || !p.Gateway() {
		http.Redirect(w, r, "/me/pay", http.StatusFound)
		return
	}
	a.downgradePayee(u.ID, p, "用户解绑")
	a.st.Audit(u.ID, "pay.unbind_key", "user", u.ID, nil, a.ip(r))
	a.flash(w, "已解绑，收款方式改为手动确认")
	http.Redirect(w, r, "/me/pay", http.StatusFound)
}

// handlePayVerify 用户手动触发一次 Key 校验：网关立即用该 Key 试拉一条流水。
func (a *App) handlePayVerify(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "payverify", 6, 10*time.Minute) {
		return
	}
	p, _ := a.st.GetPayProfile(u.ID)
	if p == nil || !p.Gateway() || a.gwc == nil {
		a.flash(w, "还没有绑定只读 Key")
		http.Redirect(w, r, "/me/pay", http.StatusFound)
		return
	}
	acct, err := a.gwc.VerifyAccount(p.BPGAccountID)
	if err != nil {
		a.st.SetPayHealth(u.ID, p.BPGLastOK, gatewayErr(err))
		a.flash(w, "验证失败："+gatewayErr(err)+"。请检查 Key 是否被删除、权限是否仍含「允许读取」、IP 白名单是否包含网关服务器。")
	} else {
		a.st.SetPayHealth(u.ID, ms(), "")
		a.flash(w, "验证通过：网关刚刚成功读取了这把 Key（"+acct.APIKeyMasked+"）的 Pay 流水。")
	}
	a.st.Audit(u.ID, "pay.verify_key", "user", u.ID, map[string]any{"ok": err == nil}, a.ip(r))
	http.Redirect(w, r, "/me/pay", http.StatusFound)
}

// downgradePayee 解绑 / Key 失效：先关在途订单并通知付款方改手动，再降级。
func (a *App) downgradePayee(userID int64, p *PayProfile, reason string) {
	ps, _ := a.st.PendingForUserPayee(userID)
	for _, pay := range ps {
		if a.gwc != nil {
			a.gwc.CloseOrder(pay.BPGOrderID)
		}
		a.st.SettlePayment(pay.ID, "closed", 0, "", "", "", 0, "")
		if x, _ := a.st.GetSubByID(pay.SubmissionID); x != nil {
			if t, _ := a.st.GetTaskByID(x.TaskID); t != nil {
				a.notify(t.OwnerID, "pay", "接单方的自动到账已失效，请改为手动转账并登记订单号", "记录 "+x.Code, x.Path())
			}
		}
	}
	if a.gwc != nil && p.BPGAccountID != "" {
		a.gwc.DisableAccount(p.BPGAccountID)
	}
	a.st.DowngradePay(userID, reason)
}

// checkGatewayHealth 每小时：以 status + last_ok 时效判断，连续 3 次不健康才降级。
func (a *App) checkGatewayHealth() {
	if a.gwc == nil {
		return
	}
	ps, _ := a.st.GatewayProfiles()
	for _, p := range ps {
		acct, err := a.gwc.GetAccount(p.BPGAccountID)
		healthy := err == nil && acct.Status == "active" && (acct.LastErr == "" || ms()-acct.LastOK < 2*hourMs)
		if healthy {
			a.st.SetPayHealth(p.UserID, ms(), "")
			continue
		}
		msg := "网关账号不健康"
		if err != nil {
			msg = gatewayErr(err)
		} else if acct.LastErr != "" {
			msg = acct.LastErr
		}
		fails := a.st.count(`SELECT COUNT(*) FROM audit_log WHERE action='pay.unhealthy' AND target_type='user' AND target_id=? AND created_at>?`, p.UserID, ms()-4*hourMs) + 1
		a.st.Audit(0, "pay.unhealthy", "user", p.UserID, map[string]any{"msg": msg, "n": fails}, "")
		a.st.SetPayHealth(p.UserID, p.BPGLastOK, msg)
		if fails >= 3 {
			a.downgradePayee(p.UserID, p, "Key 失效："+msg)
			a.notify(p.UserID, "account", "自动到账已停用", "币安只读 Key 连续检测失败："+msg+"。已切回手动确认，可重新绑定。", "/me/pay")
		}
	}
}

// repairCertPayments 启动时的幂等修复：网关已判定 paid 的认证付款却没拿到认证的用户（早期版本误把减尾数唯一金额当作金额不足），补上认证并通知。
func (a *App) repairCertPayments() {
	rows, err := a.st.db.Query(`SELECT p.id, p.user_id, p.paid_at, p.pay_amount, p.payer_id FROM payments p JOIN users u ON u.id=p.user_id WHERE p.kind='cert' AND p.status='paid' AND u.cert_paid_at=0`)
	if err != nil {
		return
	}
	type row struct {
		id, uid, paidAt int64
		amt, payer      string
	}
	var rs []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.uid, &r.paidAt, &r.amt, &r.payer) == nil {
			rs = append(rs, r)
		}
	}
	rows.Close()
	for _, r := range rs {
		if r.paidAt == 0 {
			r.paidAt = ms()
		}
		if r.payer != "" {
			a.st.SetPayerID(r.uid, r.payer, false)
		}
		if _, err := a.st.db.Exec(`UPDATE users SET cert_paid_at=? WHERE id=? AND cert_paid_at=0`, r.paidAt, r.uid); err != nil {
			continue
		}
		a.st.Audit(0, "user.cert_paid", "user", r.uid, map[string]any{"repair": true, "payment": r.id}, "")
		a.notify(r.uid, "account", "认证付款已确认（此前的「金额不足」提示有误，抱歉）", "你按面板金额付的 "+r.amt+" U 已被网关确认，认证已生效，现在可以发布任务了。", "/new")
		log.Printf("[info] 修复认证：用户 #%d 付款单 #%d", r.uid, r.id)
	}
}

// ---- 自定义收款方式（平台不核验到账）----

func (a *App) handlePayMethodAdd(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	p, _ := a.st.GetPayProfile(u.ID)
	if p == nil {
		a.flash(w, "请先保存币安 UID，再添加其它收款方式")
		http.Redirect(w, r, "/me/pay", http.StatusFound)
		return
	}
	label := cleanText(r.FormValue("label"), 20, false)
	value := cleanText(r.FormValue("value"), 120, false)
	switch {
	case label == "" || value == "":
		a.flash(w, "名称和内容都要填，例如「BSC 钱包地址」和 0x…")
	case strings.Contains(value, "://") || strings.Contains(strings.ToLower(value), "http"):
		a.flash(w, "收款信息里不能放链接")
	case len(p.Extra) >= 5:
		a.flash(w, "最多添加 5 种")
	default:
		for _, m := range p.Extra {
			if strings.EqualFold(m.Label, label) {
				a.flash(w, "这个名称已存在，先删掉再加")
				http.Redirect(w, r, "/me/pay", http.StatusFound)
				return
			}
		}
		if err := a.st.SetExtraMethods(u.ID, append(p.Extra, PayMethod{Label: label, Value: value})); err != nil {
			a.fail(w, r, err)
			return
		}
		a.st.Audit(u.ID, "pay.method_add", "user", u.ID, map[string]any{"label": label}, a.ip(r))
		a.flash(w, "已添加「"+label+"」。注意：这类方式平台不核验到账，只能由你手动确认。")
	}
	http.Redirect(w, r, "/me/pay", http.StatusFound)
}

func (a *App) handlePayMethodDel(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	p, _ := a.st.GetPayProfile(u.ID)
	idx, err := strconv.Atoi(r.FormValue("idx"))
	if p == nil || err != nil || idx < 0 || idx >= len(p.Extra) {
		http.Redirect(w, r, "/me/pay", http.StatusFound)
		return
	}
	label := p.Extra[idx].Label
	rest := append(append([]PayMethod{}, p.Extra[:idx]...), p.Extra[idx+1:]...)
	if err := a.st.SetExtraMethods(u.ID, rest); err != nil {
		a.fail(w, r, err)
		return
	}
	a.st.Audit(u.ID, "pay.method_del", "user", u.ID, map[string]any{"label": label}, a.ip(r))
	a.flash(w, "已删除「"+label+"」")
	http.Redirect(w, r, "/me/pay", http.StatusFound)
}
