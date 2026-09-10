package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type disputePage struct {
	Base
	D          *Dispute
	X          *Submission
	T          *Task
	Opener     *User
	Against    *User
	Msgs       []DisputeMessage
	Authors    map[int64]*User
	IsParty    bool
	CanPost    bool
	CanAppeal  bool
	Case       *JuryCase
	Votes      []JuryVote
	Facts      []AuditRow
	Payments   []*Payment
	Err        string
	Resolution []resOption
	BL         *BlacklistEntry
	HideOpener bool
}

type resOption struct{ Code, Label string }

// resolutionOptions 各类型可选的裁决结论（管理员用；小法庭只在 for/against 之间选）。
func resolutionOptions(t string) []resOption {
	switch t {
	case "A":
		return []resOption{{"upheld", "成立：确未付款，发布方上黑名单，记录转违约"}, {"rejected", "不成立：记录回到逾期未付"}}
	case "B":
		return []resOption{{"b_fake", "成立：根本没付，发布方上黑名单，记录回逾期"}, {"b_late", "申诉后才到账：记录完成，发布方记一次虚假标记警告"}, {"b_underpaid", "到了但少付：按少付处理（填写实付金额）"},
			{"b_wrong_uid", "接单方 UID 填错：记录完成，不处罚"}, {"b_worker_lied", "申诉前已全额到账仍否认：记录完成，接单方上黑名单"}}
	case "C":
		return []resOption{{"upheld", "成立：强制通过验证（填写推文链接）"}, {"rejected", "不成立：恢复接单倒计时"}}
	case "D":
		return []resOption{{"upheld", "成立：推文不合格，记录作废"}, {"rejected", "不成立：维持待付款，顺延付款时限"}}
	case "E":
		return []resOption{{"upheld", "成立：下架任务"}, {"banned", "成立且情节严重：下架并封禁发布方"}, {"rejected", "不成立"}}
	case "F":
		return []resOption{{"upheld", "成立：解除黑名单"}, {"rejected", "不成立：维持"}}
	case "G":
		return []resOption{{"upheld", "成立：接单方确已完成，记录进入待付款"}, {"rejected", "不成立：维持作废"}}
	}
	return nil
}

func (a *App) openDisputeOn(x *Submission, t *Task, typ string, opener *User, text string, ip string) (*Dispute, error) {
	against := t.OwnerID
	if opener.ID == t.OwnerID {
		against = x.WorkerID
	}
	window := a.cfg.EvidenceWindowH
	if typ == "A" {
		window = a.cfg.GraceReportH // 举报未付款：宽限期满即进核实
	}
	d := &Dispute{Code: newCode("D") + randCode(3), Type: typ, SubmissionID: x.ID, TaskID: t.ID, OpenerID: opener.ID, AgainstID: against, Status: "evidence", EvidenceUntil: ms() + window*hourMs}
	id, err := a.st.CreateDispute(d)
	if err != nil {
		return nil, err
	}
	d.ID = id
	a.st.AddDisputeMessage(id, opener.ID, text, nil)
	switch typ {
	case "A":
		a.st.SetDisputed(x.ID, []string{SOverdue})
		a.st.SetReported(x.ID)
		a.st.db.Exec(`UPDATE submissions SET grace_until=? WHERE id=?`, d.EvidenceUntil, x.ID)
	case "B":
		a.st.SetDisputed(x.ID, []string{SAwait, SPaid}) // paid 只可能是自动完成后的申诉（disputeOptions 把关）
		a.st.FreezeOwnerTasks(t.OwnerID)                // 钱在争议中：发布方在线任务暂停
	case "C":
		a.st.db.Exec(`UPDATE submissions SET claim_expires_at=MAX(claim_expires_at, ?), updated_at=? WHERE id=?`, ms()+a.cfg.EvidenceWindowH*hourMs+dayMs, ms(), x.ID)
	case "D":
		// 付款倒计时由 DuePayables 排除未结 D 类申诉实现暂停，裁决不成立时按占用时长顺延
	}
	a.st.Audit(opener.ID, "dispute.open", "submission", x.ID, map[string]any{"type": typ, "dispute": d.Code}, ip)
	a.st.Audit(opener.ID, "dispute.open", "dispute", id, map[string]any{"type": typ}, ip)
	a.notify(against, "dispute", "有人对记录 "+x.Code+" 发起了申诉（"+disputeType(typ)+"）", fmt.Sprintf("请在 %s 内提交你的说明与证据。", dur(a.cfg.EvidenceWindowH)), d.Path())
	return d, nil
}

// handleOpenDispute 记录页发起申诉（A/B/C/D）。
func (a *App) handleOpenDispute(w http.ResponseWriter, r *http.Request) {
	x, t, u, ok := a.loadSub(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "dispute", 5, 10*time.Minute) {
		return
	}
	typ := strings.ToUpper(strings.TrimSpace(r.FormValue("type")))
	text := cleanText(r.FormValue("text"), 2000, true)
	allowed := false
	for _, o := range a.disputeOptions(x, t, u) {
		if o == typ {
			allowed = true
		}
	}
	bad := func(m string) { a.render(w, http.StatusBadRequest, "sub", a.buildSubPage(w, r, x, t, u, m)) }
	if !allowed {
		bad("当前状态不能发起这类申诉")
		return
	}
	if len([]rune(text)) < 5 {
		bad("请描述一下情况（至少 5 个字）")
		return
	}
	d, err := a.openDisputeOn(x, t, typ, u, text, a.ip(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if typ == "A" {
		a.notify(t.OwnerID, "dispute", "你被举报逾期未付款", fmt.Sprintf("记录 %s 已逾期。%s 内付清可自动结案，否则进入核实并可能上黑名单。", x.Code, dur(a.cfg.GraceReportH)), x.Path())
	}
	http.Redirect(w, r, d.Path(), http.StatusFound)
}

// handleReportTask 举报任务违规（E）。
func (a *App) handleReportTask(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "report", 5, time.Hour) {
		return
	}
	t, _ := a.st.GetTaskByCode(r.PathValue("code"))
	if t == nil {
		a.errorPage(w, r, http.StatusNotFound, "任务不存在", "")
		return
	}
	text := cleanText(r.FormValue("text"), 1000, true)
	if len([]rune(text)) < 5 {
		a.flash(w, "请写明举报理由")
		http.Redirect(w, r, t.Path(), http.StatusFound)
		return
	}
	d := &Dispute{Code: newCode("D") + randCode(3), Type: "E", TaskID: t.ID, OpenerID: u.ID, AgainstID: t.OwnerID, Status: "review"}
	id, err := a.st.CreateDispute(d)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.st.AddDisputeMessage(id, u.ID, text, nil)
	a.st.Audit(u.ID, "dispute.open", "task", t.ID, map[string]any{"type": "E", "dispute": d.Code}, a.ip(r))
	a.flash(w, "已举报，管理员会尽快处理")
	http.Redirect(w, r, t.Path(), http.StatusFound)
}

// handleBlacklistAppeal 黑名单申诉（F）：每条处罚一次。
func (a *App) handleBlacklistAppeal(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	bl, _ := a.st.ActiveBlacklist(u.ID)
	if bl == nil {
		a.flash(w, "你不在黑名单上")
		http.Redirect(w, r, "/me", http.StatusFound)
		return
	}
	if open, _ := a.st.OpenBlacklistAppeal(u.ID); open != nil {
		http.Redirect(w, r, open.Path(), http.StatusFound)
		return
	}
	if a.st.count(`SELECT COUNT(*) FROM disputes WHERE type='F' AND opener_id=? AND task_id=?`, u.ID, bl.ID) > 0 {
		a.flash(w, "这条处罚已经申诉过一次")
		http.Redirect(w, r, "/me", http.StatusFound)
		return
	}
	text := cleanText(r.FormValue("text"), 2000, true)
	if len([]rune(text)) < 10 {
		a.flash(w, "请写清楚申诉理由（至少 10 个字）")
		http.Redirect(w, r, "/me", http.StatusFound)
		return
	}
	// F 类借 task_id 列记黑名单条目 ID
	d := &Dispute{Code: newCode("D") + randCode(3), Type: "F", TaskID: bl.ID, OpenerID: u.ID, Status: "review"}
	id, err := a.st.CreateDispute(d)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.st.AddDisputeMessage(id, u.ID, text, nil)
	a.st.Audit(u.ID, "dispute.open", "dispute", id, map[string]any{"type": "F", "blacklist": bl.ID}, a.ip(r))
	http.Redirect(w, r, d.Path(), http.StatusFound)
}

// canSeeDispute 当事双方、管理员、受邀陪审员。
func (a *App) canSeeDispute(d *Dispute, u *User) bool {
	if u == nil {
		return false
	}
	if a.isAdmin(u) || u.ID == d.OpenerID || u.ID == d.AgainstID {
		return true
	}
	return d.JuryCaseID > 0 && a.st.Invited(d.JuryCaseID, u.ID)
}

func (a *App) loadDispute(w http.ResponseWriter, r *http.Request) (*Dispute, *User, bool) {
	u := a.currentUser(r)
	d, err := a.st.GetDisputeByCode(r.PathValue("code"))
	if err != nil {
		a.fail(w, r, err)
		return nil, nil, false
	}
	if d == nil || !a.canSeeDispute(d, u) {
		if u == nil && r.Method == http.MethodGet {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return nil, nil, false
		}
		a.errorPage(w, r, http.StatusNotFound, "申诉不存在", "")
		return nil, nil, false
	}
	return d, u, true
}

func (a *App) buildDisputePage(w http.ResponseWriter, r *http.Request, d *Dispute, u *User, errMsg string) disputePage {
	p := disputePage{Base: a.base(w, r), D: d, Err: errMsg, Authors: map[int64]*User{}}
	p.Opener, _ = a.st.GetUserByID(d.OpenerID)
	p.Against, _ = a.st.GetUserByID(d.AgainstID)
	if d.SubmissionID > 0 {
		p.X, _ = a.st.GetSubByID(d.SubmissionID)
		p.Facts, _ = a.st.AuditFor("submission", d.SubmissionID)
		p.Payments, _ = a.st.PaymentsForSub(d.SubmissionID)
	}
	if d.Type == "F" {
		if bls, _ := a.st.queryBL(`WHERE b.id=?`, d.TaskID); len(bls) > 0 {
			p.BL = bls[0]
		}
	} else if d.TaskID > 0 {
		p.T, _ = a.st.GetTaskByID(d.TaskID)
	}
	p.Msgs, _ = a.st.DisputeMessages(d.ID)
	for _, m := range p.Msgs {
		if m.AuthorID > 0 {
			if _, ok := p.Authors[m.AuthorID]; !ok {
				if au, _ := a.st.GetUserByID(m.AuthorID); au != nil {
					p.Authors[m.AuthorID] = au
				}
			}
		}
	}
	p.IsParty = u.ID == d.OpenerID || u.ID == d.AgainstID
	p.CanPost = (p.IsParty || p.IsAdmin) && d.Status != "resolved"
	if d.Type == "E" {
		p.HideOpener = !p.IsAdmin && u.ID != d.OpenerID // 举报人对被举报方匿名
		p.CanPost = p.IsAdmin || u.ID == d.OpenerID
	}
	p.CanAppeal = p.IsParty && d.Status == "appeal" && d.AppealBy == 0
	if d.JuryCaseID > 0 {
		p.Case, _ = a.st.GetJuryCase(d.JuryCaseID)
		if p.Case != nil && p.Case.Status != "voting" {
			p.Votes, _ = a.st.Votes(p.Case.ID)
		}
	}
	if p.IsAdmin {
		p.Resolution = resolutionOptions(d.Type)
	}
	return p
}

func (a *App) handleDispute(w http.ResponseWriter, r *http.Request) {
	d, u, ok := a.loadDispute(w, r)
	if !ok {
		return
	}
	a.render(w, http.StatusOK, "dispute", a.buildDisputePage(w, r, d, u, ""))
}

// ---- 留言与证据 ----

var reEvidence = regexp.MustCompile(`^[a-f0-9]{40}\.(jpg|png)$`)

// saveEvidence 校验并重新编码图片（去 EXIF），返回文件名。
func (a *App) saveEvidence(data []byte) (string, error) {
	if len(data) > 2<<20 {
		return "", errors.New("图片超过 2MB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return "", errors.New("只支持 jpg / png 图片")
	}
	if cfg.Width > 2500 || cfg.Height > 2500 || cfg.Width*cfg.Height > 5_000_000 {
		return "", errors.New("图片尺寸过大，请缩到 2500×2500 以内")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", errors.New("图片解码失败")
	}
	var buf bytes.Buffer
	ext := ".jpg"
	if format == "png" {
		ext = ".png"
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	}
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(a.cfg.UploadDir, 0o755); err != nil {
		return "", err
	}
	sum := sha1.Sum(buf.Bytes())
	name := hex.EncodeToString(sum[:]) + ext
	return name, os.WriteFile(filepath.Join(a.cfg.UploadDir, name), buf.Bytes(), 0o644)
}

func (a *App) handleDisputeMessage(w http.ResponseWriter, r *http.Request) {
	d, u, ok := a.loadDispute(w, r)
	if !ok {
		return
	}
	isParty := u.ID == d.OpenerID || u.ID == d.AgainstID
	if d.Type == "E" && u.ID == d.AgainstID && !a.isAdmin(u) {
		isParty = false // 被举报方不能给匿名举报人留言
	}
	if (!isParty && !a.isAdmin(u)) || d.Status == "resolved" {
		a.errorPage(w, r, http.StatusForbidden, "不能留言", "")
		return
	}
	if a.limited(w, r, "dmsg", 20, 10*time.Minute) {
		return
	}
	if a.st.CountMessagesBy(d.ID, u.ID) >= 30 {
		a.flash(w, "留言太多了")
		http.Redirect(w, r, d.Path(), http.StatusFound)
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		a.flash(w, "上传内容过大（图片单张 ≤2MB，最多 3 张）")
		http.Redirect(w, r, d.Path(), http.StatusFound)
		return
	}
	text := cleanText(r.FormValue("text"), 2000, true)
	var images []string
	if r.MultipartForm != nil {
		files := r.MultipartForm.File["images"]
		if len(files) > 3 {
			files = files[:3]
		}
		for _, fh := range files {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(io.LimitReader(f, 2<<20+1))
			f.Close()
			name, err := a.saveEvidence(data)
			if err != nil {
				a.flash(w, err.Error())
				http.Redirect(w, r, d.Path(), http.StatusFound)
				return
			}
			images = append(images, name)
		}
	}
	if text == "" && len(images) == 0 {
		a.flash(w, "写点内容或上传截图")
		http.Redirect(w, r, d.Path(), http.StatusFound)
		return
	}
	author := u.ID
	if a.isAdmin(u) && !isParty {
		author = 0
	}
	if err := a.st.AddDisputeMessage(d.ID, author, text, images); err != nil {
		a.fail(w, r, err)
		return
	}
	if author == 0 {
		for _, id := range []int64{d.OpenerID, d.AgainstID} {
			a.notify(id, "dispute", "申诉 "+d.Code+" 管理员留言", truncate(text, 60), d.Path())
		}
	} else {
		other := d.AgainstID
		if u.ID == d.AgainstID {
			other = d.OpenerID
		}
		a.notify(other, "dispute", "申诉 "+d.Code+" 有新留言", truncate(text, 60), d.Path())
	}
	http.Redirect(w, r, d.Path()+"#msgs", http.StatusFound)
}

// handleEvidence 证据图经鉴权输出。
func (a *App) handleEvidence(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !reEvidence.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	u := a.currentUser(r)
	var did int64
	a.st.db.QueryRow(`SELECT dispute_id FROM dispute_messages WHERE images LIKE ? LIMIT 1`, "%\""+name+"\"%").Scan(&did)
	d, _ := a.st.GetDisputeByID(did)
	if d == nil || !a.canSeeDispute(d, u) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeFile(w, r, filepath.Join(a.cfg.UploadDir, name))
}

// handleDisputeAppeal 小法庭裁决后的上诉期内请求管理员复核。
func (a *App) handleDisputeAppeal(w http.ResponseWriter, r *http.Request) {
	d, u, ok := a.loadDispute(w, r)
	if !ok {
		return
	}
	if d.Status != "appeal" || (u.ID != d.OpenerID && u.ID != d.AgainstID) {
		a.flash(w, "当前不能上诉")
		http.Redirect(w, r, d.Path(), http.StatusFound)
		return
	}
	text := cleanText(r.FormValue("text"), 2000, true)
	if len([]rune(text)) < 10 {
		a.flash(w, "请写明上诉理由（至少 10 个字）")
		http.Redirect(w, r, d.Path(), http.StatusFound)
		return
	}
	a.st.AddDisputeMessage(d.ID, u.ID, "【上诉】"+text, nil)
	a.st.SetAppealRequested(d.ID, u.ID)
	a.st.Audit(u.ID, "dispute.appeal", "dispute", d.ID, nil, a.ip(r))
	a.flash(w, "已提交上诉，等待管理员复核")
	http.Redirect(w, r, d.Path(), http.StatusFound)
}

// ---- 裁决执行 ----

// applyResolution 执行裁决（管理员或小法庭上诉期满后调用）。extra：b_underpaid 的实付金额 / C 的推文链接。
func (a *App) applyResolution(d *Dispute, resolution, note string, by int64, extra string, ip string) error {
	if d.Status == "resolved" {
		return ErrState
	}
	var x *Submission
	var t *Task
	if d.SubmissionID > 0 {
		x, _ = a.st.GetSubByID(d.SubmissionID)
		if x != nil {
			t, _ = a.st.GetTaskByID(x.TaskID)
		}
	} else if d.TaskID > 0 && d.Type == "E" {
		t, _ = a.st.GetTaskByID(d.TaskID)
	}
	opener, _ := a.st.GetUserByID(d.OpenerID)
	against, _ := a.st.GetUserByID(d.AgainstID)
	switch d.Type {
	case "A":
		if x == nil || t == nil {
			return ErrState
		}
		if resolution == "upheld" {
			owner, _ := a.st.GetUserByID(t.OwnerID)
			if owner != nil {
				a.blacklistUser(owner, "publisher", "逾期未付款（A 类申诉成立）", d.ID, ip)
			}
		} else if x.Status == SDisputed {
			a.st.RestoreFromDispute(x.ID, SOverdue)
		}
	case "B":
		if x == nil || t == nil {
			return ErrState
		}
		owner, _ := a.st.GetUserByID(t.OwnerID)
		worker, _ := a.st.GetUserByID(x.WorkerID)
		switch resolution {
		case "b_fake":
			a.st.RestoreFromDispute(x.ID, SOverdue)
			if owner != nil {
				a.blacklistUser(owner, "publisher", "虚假标记已付款（B 类申诉成立）", d.ID, ip)
			}
		case "b_late":
			if ok, _ := a.st.SetPaid(x.ID, "admin", payAmount(x, t)); ok {
				a.st.db.Exec(`UPDATE submissions SET late=1 WHERE id=?`, x.ID)
				a.settlePaid(x, t, by, "admin", false)
			}
			if owner != nil {
				a.warnPublisher(owner, "虚假标记后补付（裁决）", d.ID, x.Path(), ip)
			}
		case "b_underpaid":
			amt, err := parseAmountE8(extra, 8)
			if err != nil || amt <= 0 || amt >= payAmount(x, t) {
				return errors.New("请填写正确的实付金额（小于应付）")
			}
			a.st.RestoreFromDispute(x.ID, SAwait)
			a.st.db.Exec(`UPDATE submissions SET underpaid_e8=?, marked_paid_at=?, topup_requested_at=?, topup_marked_at=0, confirmed_at=0, confirm_method='', paid_amount_e8=0, updated_at=? WHERE id=?`, amt, ms(), ms(), ms(), x.ID)
			a.notify(x.WorkerID, "pay", "裁决：款项少付", fmt.Sprintf("实付 %s U，你可以接受或要求补差。", fmtE8(amt)), x.Path())
			if a.st.PublisherFrozen(t.OwnerID) == 0 {
				a.st.UnfreezeOwnerTasks(t.OwnerID) // 申诉开庭时冻结的任务恢复
			}
		case "b_wrong_uid":
			if ok, _ := a.st.SetPaid(x.ID, "admin", payAmount(x, t)); ok {
				a.settlePaid(x, t, by, "admin", false)
			}
		case "b_worker_lied":
			if ok, _ := a.st.SetPaid(x.ID, "admin", payAmount(x, t)); ok {
				a.settlePaid(x, t, by, "admin", false)
			}
			if worker != nil {
				a.blacklistUser(worker, "worker", "收到款项却称未收到（B 类申诉查实）", d.ID, ip)
			}
		default:
			return errors.New("未知结论")
		}
	case "C":
		if x == nil || t == nil {
			return ErrState
		}
		if resolution == "upheld" {
			id := tweetIDFrom(extra)
			if id == "" {
				return errors.New("请填写要强制通过的推文链接")
			}
			if a.st.count(`SELECT COUNT(*) FROM submissions WHERE tweet_id=? AND id<>?`, id, x.ID) > 0 {
				return errors.New("这条推文已用于其它记录")
			}
			if ok, _ := a.st.SetSubmitted(x.ID, id, "https://x.com/i/web/status/"+id); !ok {
				return errors.New("记录当前状态不能强制通过")
			}
			var recheck, deadline int64
			if t.RetentionH > 0 {
				recheck = ms() + t.RetentionH*hourMs
			} else {
				deadline = ms() + t.PayWindowH*hourMs
			}
			a.st.SetVerified(x.ID, "（管理员强制通过）", 0, recheck, deadline)
			a.st.db.Exec(`UPDATE submissions SET recheck_flag='forced' WHERE id=? AND status='verified'`, x.ID)
			a.st.Audit(by, "sub.force_verified", "submission", x.ID, map[string]any{"tweet": id}, ip)
			if deadline > 0 {
				a.notify(t.OwnerID, "pay", "有一条记录待付款（管理员核定）", fmt.Sprintf("请在 %s 内付款 %s U。", dur(t.PayWindowH), fmtE8(payAmount(x, t))), x.Path())
			}
		} else if x.Status == SClaimed && x.ClaimExpiresAt < ms()+30*60*1000 {
			a.st.db.Exec(`UPDATE submissions SET claim_expires_at=?, updated_at=? WHERE id=?`, ms()+30*60*1000, ms(), x.ID)
		}
	case "D":
		if x == nil || t == nil {
			return ErrState
		}
		if resolution == "upheld" {
			ok, _ := a.st.SetVoid(x.ID, []string{SPayable, SOverdue, SAwait}, "裁决：未完成或不合格")
			if !ok {
				return errors.New("记录当前状态不能作废（可能已完成）")
			}
			a.st.db.Exec(`UPDATE submissions SET marked_paid_at=0, marked_order_id='', marked_note='', updated_at=? WHERE id=? AND status='void'`, ms(), x.ID)
			a.closePendingForSub(x.ID)
			a.st.Audit(by, "sub.void", "submission", x.ID, map[string]any{"reason": "D 类申诉成立"}, ip)
			if a.st.PublisherFrozen(t.OwnerID) == 0 {
				a.st.UnfreezeOwnerTasks(t.OwnerID)
			}
		} else {
			paused := ms() - d.CreatedAt
			a.st.db.Exec(`UPDATE submissions SET pay_deadline_at=MAX(pay_deadline_at+?, ?), updated_at=? WHERE id=? AND status='payable'`, paused, ms()+dayMs, ms(), x.ID)
		}
	case "G":
		// 点赞/转发被发布方两次「没看到」作废后，接单方申诉：成立则视为已完成，重新进入待付款
		if x == nil || t == nil {
			return ErrState
		}
		if resolution == "upheld" {
			ok, _ := a.st.SetCheckedOK(x.ID, []string{SVoid}, ms()+t.PayWindowH*hourMs, 2)
			if !ok {
				return errors.New("记录当前状态不能恢复")
			}
			a.st.db.Exec(`UPDATE submissions SET void_reason='', updated_at=? WHERE id=?`, ms(), x.ID)
			a.st.Audit(by, "sub.check_ok", "submission", x.ID, map[string]any{"reason": "G 类申诉成立"}, ip)
			a.notify(t.OwnerID, "pay", "核对争议成立，请付款", fmt.Sprintf("记录 %s 经裁决视为已完成，请在 %s 内付款 %s U。注意：作废期间名额可能已被他人接走，本条按额外一单付款。", x.Code, dur(t.PayWindowH), fmtE8(payAmount(x, t))), x.Path())
		}
	case "E":
		if t == nil {
			return ErrState
		}
		if resolution == "upheld" || resolution == "banned" {
			a.takedownTask(t, by, ip)
			if resolution == "banned" {
				if owner, _ := a.st.GetUserByID(t.OwnerID); owner != nil {
					a.blacklistUser(owner, "publisher", "发布违规任务", d.ID, ip)
					a.st.SetUserStatus(owner.ID, "banned")
				}
			}
		}
	case "F":
		if resolution == "upheld" {
			if err := a.st.LiftBlacklist(d.TaskID, by, note); err != nil {
				return err
			}
			a.st.Audit(by, "blacklist.lift", "user", d.OpenerID, map[string]any{"entry": d.TaskID}, ip)
		}
	default:
		return errors.New("未知类型")
	}
	if err := a.st.ResolveDispute(d.ID, resolution, note, by); err != nil {
		return err
	}
	a.st.Audit(by, "dispute.resolve", "dispute", d.ID, map[string]any{"resolution": resolution}, ip)
	if d.SubmissionID > 0 {
		a.st.Audit(by, "dispute.resolve", "submission", d.SubmissionID, map[string]any{"type": d.Type, "resolution": resolution, "dispute": d.Code}, ip)
	}
	label := "不成立"
	if strings.HasPrefix(resolution, "upheld") || strings.HasPrefix(resolution, "b_") || resolution == "banned" {
		label = "已裁决"
	}
	for _, uu := range []*User{opener, against} {
		if uu != nil {
			a.notify(uu.ID, "dispute", "申诉 "+d.Code+" "+label, note, d.Path())
		}
	}
	if d.JuryCaseID > 0 {
		if c, _ := a.st.GetJuryCase(d.JuryCaseID); c != nil && c.Status == "voting" {
			a.st.CloseCase(c.ID, "escalated", "")
		}
	}
	return nil
}

// blacklistUser 上黑名单：状态、关闭任务、逾期记录转违约、公示条目。
func (a *App) blacklistUser(u *User, role, reason string, disputeID int64, ip string) {
	existing, _ := a.st.ActiveBlacklist(u.ID)
	p, _ := a.st.GetPayProfile(u.ID)
	uid := ""
	if p != nil {
		uid = p.BinanceUID
	}
	owed := int64(0)
	if role == "publisher" {
		subs, _ := a.st.SubsByOwnerStatus(u.ID, SOverdue, SDisputed)
		for _, x := range subs {
			if x.Status == SDisputed && x.PrevStatus != SOverdue {
				continue
			}
			if ok, _ := a.st.SetDefaulted(x.ID); ok {
				if t, _ := a.st.GetTaskByID(x.TaskID); t != nil {
					owed += max(payAmount(x, t)-x.UnderpaidE8, 0) // 网关已确认的部分实付不算欠
				}
				a.st.Audit(0, "sub.defaulted", "submission", x.ID, nil, "")
				a.notify(x.WorkerID, "dispute", "发布方已上黑名单，记录转为违约", "欠款会公示在黑名单榜；对方补付后会通知你。", x.Path())
			}
		}
	}
	a.st.CloseOwnerTasks(u.ID, "blacklisted") // 无论以哪种身份上榜，在线任务全部关闭
	if existing != nil {
		if owed > 0 {
			a.st.AddOwed(u.ID, owed)
		}
		a.st.Audit(0, "blacklist.again", "user", u.ID, map[string]any{"role": role, "reason": reason, "owed": fmtE8(owed)}, ip)
		return
	}
	a.st.AddBlacklist(&BlacklistEntry{UserID: u.ID, XID: u.XID, Handle: u.Handle, BinanceUID: uid, PayerID: u.PayerID, Role: role, Reason: reason, DisputeID: disputeID, AmountOwedE8: owed})
	a.st.SetUserStatus(u.ID, "blacklisted")
	a.st.Audit(0, "blacklist.add", "user", u.ID, map[string]any{"role": role, "reason": reason, "owed": fmtE8(owed)}, ip)
	a.notify(u.ID, "account", "你已被列入黑名单", reason+"。不能再发布或接单；如有异议可在「我的」页面申诉一次。", "/me")
}

// warnPublisher 虚假标记警告：第一次警告，第二次上黑名单。
func (a *App) warnPublisher(owner *User, reason string, disputeID int64, link, ip string) {
	n := a.st.count(`SELECT COUNT(*) FROM audit_log WHERE action='user.warning' AND target_type='user' AND target_id=?`, owner.ID) + 1
	a.st.Audit(0, "user.warning", "user", owner.ID, map[string]any{"reason": reason, "n": n}, ip)
	if n >= 2 {
		a.blacklistUser(owner, "publisher", "第二次虚假标记已付款", disputeID, ip)
		return
	}
	a.notify(owner.ID, "account", "警告：先标记后付款", "这次记录完成，但再有一次虚假标记将进入黑名单。", link)
}

// takedownTask 下架：已验证的免留存进待付款，未提交的作废并通知删帖。
func (a *App) takedownTask(t *Task, by int64, ip string) {
	a.st.SetTaskStatus(t.ID, "closed", "takedown", false)
	subs, _ := a.st.SubsByTask(t.ID)
	for _, x := range subs {
		switch x.Status {
		case SClaimed, SSubmit, SChecking:
			a.st.SetVoid(x.ID, []string{SClaimed, SSubmit, SChecking}, "任务已下架")
			a.notify(x.WorkerID, "verify", "任务 "+t.Code+" 已被下架", "请删除相关推文，本条记录作废。", x.Path())
		case SVerified:
			var ok bool
			if t.CPM() {
				v := x.Views
				if v < 0 {
					v = 0
				}
				ok, _ = a.st.SetPayableAmount(x.ID, ms()+t.PayWindowH*hourMs, "任务下架，免留存，按已记录浏览量结算", cpmAmount(t, v), v)
			} else {
				ok, _ = a.st.SetPayable(x.ID, ms()+t.PayWindowH*hourMs, "任务下架，免留存")
			}
			if ok {
				a.notify(x.WorkerID, "verify", "任务 "+t.Code+" 已被下架", "你的推文已验证通过，免留存直接进入待付款；可以删除推文。", x.Path())
				a.notify(t.OwnerID, "pay", "任务被下架，已验证记录仍需付款", "记录 "+x.Code, x.Path())
			}
		}
	}
	a.st.Audit(by, "task.takedown", "task", t.ID, nil, ip)
}
