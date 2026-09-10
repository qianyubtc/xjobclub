package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

type adminPage struct {
	Base
	Review      []*Dispute
	Evidence    []*Dispute
	Jury        []*Dispute
	Appeal      []*Dispute
	Users       map[int64]*User
	Subs        map[int64]*Submission
	RecentBL    []*BlacklistEntry
	Counts      map[string]int64
	Pool        int64
	ReviewTasks []*Task
	TaskOwners  map[int64]*User
	Votes       map[int64][2]int64
}

func (a *App) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	p := adminPage{Base: a.base(w, r), Users: map[int64]*User{}, Subs: map[int64]*Submission{}, Counts: map[string]int64{}}
	p.Review, _ = a.st.DisputesByStatus([]string{"review", "open"}, 100)
	p.Evidence, _ = a.st.DisputesByStatus([]string{"evidence"}, 100)
	p.Jury, _ = a.st.DisputesByStatus([]string{"jury"}, 100)
	p.Appeal, _ = a.st.DisputesByStatus([]string{"appeal"}, 100)
	p.ReviewTasks, _ = a.st.TasksInReview()
	p.TaskOwners, p.Votes = map[int64]*User{}, map[int64][2]int64{}
	for _, t := range p.ReviewTasks {
		if _, ok := p.TaskOwners[t.OwnerID]; !ok {
			if o, _ := a.st.GetUserByID(t.OwnerID); o != nil {
				p.TaskOwners[t.OwnerID] = o
			}
		}
		pass, fail := a.st.VoteCounts(t.ID)
		p.Votes[t.ID] = [2]int64{pass, fail}
	}
	for _, list := range [][]*Dispute{p.Review, p.Evidence, p.Jury, p.Appeal} {
		for _, d := range list {
			for _, id := range []int64{d.OpenerID, d.AgainstID} {
				if _, ok := p.Users[id]; !ok && id > 0 {
					if u, _ := a.st.GetUserByID(id); u != nil {
						p.Users[id] = u
					}
				}
			}
			if d.SubmissionID > 0 {
				if _, ok := p.Subs[d.SubmissionID]; !ok {
					if x, _ := a.st.GetSubByID(d.SubmissionID); x != nil {
						p.Subs[d.SubmissionID] = x
					}
				}
			}
		}
	}
	p.RecentBL, _, _ = a.st.ListBlacklist(20, 0)
	for _, b := range p.RecentBL {
		if _, ok := p.Users[b.UserID]; !ok {
			if u, _ := a.st.GetUserByID(b.UserID); u != nil {
				p.Users[b.UserID] = u
			}
		}
	}
	p.Counts["users"] = a.st.count(`SELECT COUNT(*) FROM users`)
	p.Counts["tasks"] = a.st.count(`SELECT COUNT(*) FROM tasks`)
	p.Counts["open"] = a.st.count(`SELECT COUNT(*) FROM tasks WHERE status='open'`)
	p.Counts["subs"] = a.st.count(`SELECT COUNT(*) FROM submissions`)
	p.Counts["paid"] = a.st.count(`SELECT COUNT(*) FROM submissions WHERE status='paid'`)
	p.Counts["overdue"] = a.st.count(`SELECT COUNT(*) FROM submissions WHERE status='overdue'`)
	p.Counts["await"] = a.st.count(`SELECT COUNT(*) FROM submissions WHERE status='awaiting_confirm'`)
	p.Counts["bl"] = a.st.count(`SELECT COUNT(*) FROM blacklist WHERE lifted_at=0`)
	p.Pool = a.st.JuryPoolSize(juryMinAgeMs)
	a.render(w, http.StatusOK, "admin", p)
}

type adminUserPage struct {
	Base
	Q       string
	Found   []*User
	U       *User
	Pay     *PayProfile
	Stats   PubStats
	WStats  WorkerStats
	BL      []*BlacklistEntry
	Subs    []*Submission
	Tasks   []*Task
	Audit   []AuditRow
	TaskMap map[int64]*Task
}

func (a *App) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	p := adminUserPage{Base: a.base(w, r), Q: strings.TrimSpace(r.URL.Query().Get("q")), TaskMap: map[int64]*Task{}}
	if id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64); err == nil && id > 0 {
		p.U, _ = a.st.GetUserByID(id)
	} else if p.Q != "" {
		p.Found, _ = a.st.SearchUsers(p.Q, 30)
		if len(p.Found) == 1 {
			p.U = p.Found[0]
		}
	}
	if p.U != nil {
		p.Pay, _ = a.st.GetPayProfile(p.U.ID)
		p.Stats = a.st.PubStats(p.U.ID)
		p.WStats = a.st.WorkerStats(p.U.ID)
		p.BL, _ = a.st.BlacklistForUser(p.U.ID)
		p.Subs, _ = a.st.SubsByWorker(p.U.ID, 30)
		p.Tasks, _ = a.st.TasksByOwner(p.U.ID)
		p.Audit, _ = a.st.AuditFor("user", p.U.ID)
		for _, x := range p.Subs {
			if _, ok := p.TaskMap[x.TaskID]; !ok {
				if t, _ := a.st.GetTaskByID(x.TaskID); t != nil {
					p.TaskMap[x.TaskID] = t
				}
			}
		}
	}
	a.render(w, http.StatusOK, "adminuser", p)
}

// handleAdminDispute resolve / takeover / tojury。
func (a *App) handleAdminDispute(w http.ResponseWriter, r *http.Request) {
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	d, _ := a.st.GetDisputeByCode(r.PathValue("code"))
	if d == nil {
		a.errorPage(w, r, http.StatusNotFound, "申诉不存在", "")
		return
	}
	var err error
	switch r.PathValue("action") {
	case "resolve":
		note := cleanText(r.FormValue("note"), 1000, true)
		if note == "" {
			note = "管理员裁决"
		}
		err = a.applyResolution(d, strings.TrimSpace(r.FormValue("resolution")), note, admin.ID, strings.TrimSpace(r.FormValue("extra")), a.ip(r))
	case "takeover":
		if d.Status == "resolved" {
			err = errors.New("该申诉已结案")
			break
		}
		if d.JuryCaseID > 0 {
			if c, _ := a.st.GetJuryCase(d.JuryCaseID); c != nil && c.Status == "voting" {
				a.st.CloseCase(c.ID, "escalated", "")
			}
		}
		err = a.st.SetDisputeStatus(d.ID, "review")
		a.st.Audit(admin.ID, "dispute.takeover", "dispute", d.ID, nil, a.ip(r))
	case "tojury":
		if d.Status == "resolved" || d.JuryCaseID > 0 {
			err = errors.New("该申诉不能交小法庭")
		} else if !a.cfg.JuryEnabled || d.SubmissionID == 0 {
			err = errors.New("小法庭未启用或类型不支持")
		} else {
			err = a.openJury(d)
		}
	default:
		a.errorPage(w, r, http.StatusNotFound, "操作不存在", "")
		return
	}
	if err != nil {
		a.flash(w, err.Error())
	} else {
		a.flash(w, "已处理")
	}
	http.Redirect(w, r, d.Path(), http.StatusFound)
}

func (a *App) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, _ := a.st.GetUserByID(id)
	if u == nil {
		a.errorPage(w, r, http.StatusNotFound, "用户不存在", "")
		return
	}
	action := r.PathValue("action")
	reason := cleanText(r.FormValue("reason"), 200, false)
	var err error
	switch action {
	case "blacklist":
		role := r.FormValue("role")
		if role != "worker" {
			role = "publisher"
		}
		if reason == "" {
			reason = "管理员处罚"
		}
		a.blacklistUser(u, role, reason, 0, a.ip(r))
	case "ban":
		err = a.st.SetUserStatus(u.ID, "banned")
	case "activate":
		if bl, _ := a.st.ActiveBlacklist(u.ID); bl != nil {
			err = a.st.LiftBlacklist(bl.ID, admin.ID, reason)
		} else {
			err = a.st.SetUserStatus(u.ID, "active")
		}
	case "unsuspend":
		err = a.st.SetSuspended(u.ID, 0)
	case "cert":
		_, err = a.st.db.Exec(`UPDATE users SET cert_paid_at=? WHERE id=? AND cert_paid_at=0`, ms(), u.ID)
	case "downgrade":
		if p, _ := a.st.GetPayProfile(u.ID); p != nil && p.Gateway() {
			a.downgradePayee(u.ID, p, "管理员操作")
		}
	default:
		a.errorPage(w, r, http.StatusNotFound, "操作不存在", "")
		return
	}
	if err != nil {
		a.flash(w, err.Error())
	} else {
		a.st.Audit(admin.ID, "admin.user."+action, "user", u.ID, map[string]any{"reason": reason}, a.ip(r))
		a.flash(w, "已处理")
	}
	http.Redirect(w, r, "/admin/users?id="+strconv.FormatInt(u.ID, 10), http.StatusFound)
}

func (a *App) handleAdminTask(w http.ResponseWriter, r *http.Request) {
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	t, _ := a.st.GetTaskByCode(r.PathValue("code"))
	if t == nil {
		a.errorPage(w, r, http.StatusNotFound, "任务不存在", "")
		return
	}
	switch r.PathValue("action") {
	case "takedown":
		a.takedownTask(t, admin.ID, a.ip(r))
		a.notify(t.OwnerID, "account", "任务 "+t.Code+" 已被管理员下架", cleanText(r.FormValue("reason"), 200, false), t.Path())
		a.flash(w, "已下架")
	case "approve":
		if !a.finishReview(t, true, "admin_pass", "管理员审核通过", admin.ID) {
			a.flash(w, "任务不在审核中")
		} else {
			a.flash(w, "已通过，任务上线")
		}
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	case "reject":
		reason := cleanText(r.FormValue("reason"), 200, false)
		if !a.finishReview(t, false, "admin_reject", strings.TrimSpace("管理员驳回 "+reason), admin.ID) {
			a.flash(w, "任务不在审核中")
		} else {
			a.flash(w, "已驳回")
		}
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	default:
		a.errorPage(w, r, http.StatusNotFound, "操作不存在", "")
		return
	}
	http.Redirect(w, r, t.Path(), http.StatusFound)
}

func (a *App) handleAdminSub(w http.ResponseWriter, r *http.Request) {
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	x, _ := a.st.GetSubByCode(r.PathValue("code"))
	if x == nil {
		a.errorPage(w, r, http.StatusNotFound, "记录不存在", "")
		return
	}
	t, _ := a.st.GetTaskByID(x.TaskID)
	if t == nil {
		a.errorPage(w, r, http.StatusNotFound, "记录不存在", "")
		return
	}
	reason := cleanText(r.FormValue("reason"), 200, false)
	action := r.PathValue("action")
	var err error
	switch action {
	case "void":
		if ok, _ := a.st.SetVoid(x.ID, []string{SClaimed, SSubmit, SChecking, SVerified, SPayable, SOverdue, SDisputed, SAwait}, "管理员作废："+reason); !ok {
			err = ErrState
		} else {
			a.closePendingForSub(x.ID)
		}
	case "paid":
		if ok, _ := a.st.SetPaid(x.ID, "admin", payAmount(x, t)); !ok {
			err = ErrState
		} else {
			a.afterPaid(x, t, admin.ID, "admin")
		}
	case "verify":
		id := tweetIDFrom(r.FormValue("tweet_url"))
		if id == "" {
			err = errors.New("请填写推文链接")
			break
		}
		if ok, _ := a.st.SetSubmitted(x.ID, id, "https://x.com/i/web/status/"+id); !ok {
			err = ErrState
			break
		}
		var recheck, deadline int64
		if t.RetentionH > 0 {
			recheck = ms() + t.RetentionH*hourMs
		} else {
			deadline = ms() + t.PayWindowH*hourMs
		}
		a.st.SetVerified(x.ID, "（管理员强制通过）", 0, recheck, deadline)
		a.st.db.Exec(`UPDATE submissions SET recheck_flag='forced' WHERE id=? AND status='verified'`, x.ID)
	case "repaid":
		if x.Status != SDefault {
			err = ErrState
		} else {
			a.st.SetRepaid(x.ID, "admin", payAmount(x, t))
			a.st.RepayBlacklist(t.OwnerID, payAmount(x, t))
		}
	default:
		a.errorPage(w, r, http.StatusNotFound, "操作不存在", "")
		return
	}
	if err != nil {
		a.flash(w, err.Error())
	} else {
		a.st.Audit(admin.ID, "admin.sub."+action, "submission", x.ID, map[string]any{"reason": reason}, a.ip(r))
		a.flash(w, "已处理")
	}
	http.Redirect(w, r, x.Path(), http.StatusFound)
}

func (a *App) handleAdminLift(w http.ResponseWriter, r *http.Request) {
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.st.LiftBlacklist(id, admin.ID, cleanText(r.FormValue("note"), 200, false)); err != nil {
		a.flash(w, err.Error())
	} else {
		a.st.Audit(admin.ID, "blacklist.lift", "blacklist", id, nil, a.ip(r))
		a.flash(w, "已解除")
	}
	a.redirectBack(w, r, "/admin")
}

// cancelOpenTasks 把所有在售 / 暂停的任务转为只保留已接名额（CancelRemaining），并通知发布方。
// 用于「平台改为先审核再上线」这类切换：旧任务不再开放新接单，已接的照常走完。
func (a *App) cancelOpenTasks(reason string, by int64) int {
	list, _ := a.st.queryTasks(`WHERE status IN ('open','paused') ORDER BY id`)
	n := 0
	for _, t := range list {
		if err := a.st.CancelRemaining(t.ID); err != nil {
			log.Printf("[warn] 撤回任务 %s 失败: %v", t.Code, err)
			continue
		}
		n++
		a.st.Audit(by, "task.cancel_open", "task", t.ID, map[string]any{"reason": reason}, "")
		a.notify(t.OwnerID, "task", "任务 "+t.Code+" 已停止接新单", reason+" 已被接的记录不受影响，可以重新发布新任务（新任务会先经社区审核）。", t.Path())
	}
	return n
}

func (a *App) handleAdminCancelOpen(w http.ResponseWriter, r *http.Request) {
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	reason := cleanText(r.FormValue("reason"), 200, false)
	if reason == "" {
		reason = "平台调整，旧任务停止接新单。"
	}
	n := a.cancelOpenTasks(reason, admin.ID)
	a.flash(w, fmt.Sprintf("已撤回 %d 个在售任务（已接的记录保留）", n))
	http.Redirect(w, r, "/admin", http.StatusFound)
}
