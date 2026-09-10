package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// 发布审核：新任务先公示投票，通过才进大厅。挡诈骗 / 钓鱼 / 违法任务，也让社区对发布方有个第一印象。

const (
	TReview   = "review"   // 审核中
	TRejected = "rejected" // 未通过
)

// ReviewInfo 任务页 / 审核页展示用。
type ReviewInfo struct {
	Pass, Fail int64
	MyVote     int64
	MyReason   string
	CanVote    bool
	Why        string // 不能投票的原因
	Deadline   int64
	Hold       bool
	Reasons    []TaskVote // 反对票理由（发布方 / 管理员可见）
	MinVotes   int64
	EarlyPass  int64
	EarlyFail  int64
}

var voteReasons = []string{"诈骗 / 钓鱼 / 引流付费", "违法违规内容", "报酬明显不合理", "发布方可疑", "文案有问题", "其它"}

// reviewSkip 免审：管理员，或网关核销付款够多且无逾期、无违约的发布方。
func (a *App) reviewSkip(u *User, st PubStats) bool {
	if a.isAdmin(u) {
		return true
	}
	return a.cfg.ReviewTrustedPaid > 0 && st.PaidGateway >= a.cfg.ReviewTrustedPaid && st.Overdue == 0 && st.Defaulted == 0
}

// voterEligible 谁能投：注册满一定时长、X 账号够老、不在黑名单、不是任务发布方、今天没投满。
func (a *App) voterEligible(u *User, t *Task) (bool, string) {
	if u == nil {
		return false, "登录后参与审核"
	}
	if t != nil && u.ID == t.OwnerID {
		return false, "不能审核自己的任务"
	}
	if bl, _ := a.st.ActiveBlacklist(u.ID); bl != nil {
		return false, "黑名单中的账号不能参与审核"
	}
	if ms()-u.CreatedAt < a.cfg.ReviewVoterAgeH*hourMs {
		return false, fmt.Sprintf("注册满 %s 后可参与审核", dur(a.cfg.ReviewVoterAgeH))
	}
	if a.cfg.ReviewVoterXDays > 0 && u.XCreatedMs > 0 && ms()-u.XCreatedMs < a.cfg.ReviewVoterXDays*dayMs {
		return false, fmt.Sprintf("X 账号注册满 %d 天后可参与审核", a.cfg.ReviewVoterXDays)
	}
	if a.st.VotesSince(u.ID, dayStartMs()) >= a.cfg.ReviewVotesPerDay {
		return false, "今天的审核次数已用完"
	}
	return true, ""
}

func (a *App) reviewInfo(t *Task, me *User) *ReviewInfo {
	if t.Status != TReview && t.Status != TRejected {
		return nil
	}
	ri := &ReviewInfo{Deadline: t.ReviewDeadlineAt, Hold: t.ReviewHold == 1, MinVotes: a.cfg.ReviewMinVotes, EarlyPass: a.cfg.ReviewEarlyPass, EarlyFail: a.cfg.ReviewEarlyFail}
	ri.Pass, ri.Fail = a.st.VoteCounts(t.ID)
	if me != nil {
		ri.MyVote, ri.MyReason = a.st.UserVote(t.ID, me.ID)
		if t.Status == TReview {
			ri.CanVote, ri.Why = a.voterEligible(me, t)
		}
		if me.ID == t.OwnerID || a.isAdmin(me) {
			ri.Reasons = a.st.VoteReasons(t.ID)
		}
	}
	return ri
}

// decideReview 按票数与期限判定：票够且通过率高 → 提前上线；反对率高 → 提前驳回；
// 到期：反对票达到阈值转管理员，否则自动上线（小站早期投票少，不能让任务卡死）。
func (a *App) decideReview(t *Task, now int64) {
	if t.Status != TReview || t.ReviewHold == 1 {
		return
	}
	c := a.cfg
	pass, fail := a.st.VoteCounts(t.ID)
	total := pass + fail
	switch {
	case total >= c.ReviewMinVotes && pass*100 >= total*c.ReviewEarlyPass:
		if a.finishReview(t, true, "vote_pass", fmt.Sprintf("社区投票通过（%d 通过 / %d 反对）", pass, fail), 0) {
			a.notifyAdmins("任务经社区投票上线（抽查）", fmt.Sprintf("《%s》%d 通过 / %d 反对，已上线；如有问题可到任务页下架。", t.Title, pass, fail), t.Path())
		}
	case total >= c.ReviewMinVotes && fail*100 >= total*c.ReviewEarlyFail:
		// 反对票多不直接驳回（防止几个小号联手封杀），转管理员裁定
		a.st.HoldReview(t.ID)
		a.notifyAdmins("有任务被社区投票反对，请裁定", fmt.Sprintf("《%s》%d 通过 / %d 反对，请到后台通过或驳回。", t.Title, pass, fail), t.Path())
		a.notify(t.OwnerID, "task", "任务审核转管理员裁定", fmt.Sprintf("《%s》收到较多反对票（%d 通过 / %d 反对），已转管理员裁定。", t.Title, pass, fail), t.Path())
	case now >= t.ReviewDeadlineAt:
		if fail >= c.ReviewHoldFails && fail*3 >= total {
			a.st.HoldReview(t.ID)
			a.notifyAdmins("有任务审核需要你裁定", fmt.Sprintf("《%s》审核期满，%d 通过 / %d 反对，请到后台通过或驳回。", t.Title, pass, fail), t.Path())
			a.notify(t.OwnerID, "task", "任务审核转管理员裁定", fmt.Sprintf("《%s》审核期内收到 %d 票反对，已转管理员裁定，请耐心等待。", t.Title, fail), t.Path())
		} else {
			a.finishReview(t, true, "timeout_pass", fmt.Sprintf("审核期满无有效反对（%d 通过 / %d 反对），自动上线", pass, fail), 0)
		}
	}
}

// finishReview 通过 → open 并从现在起算任务截止；驳回 → rejected，发布方可改后重提。
func (a *App) finishReview(t *Task, pass bool, result, note string, by int64) bool {
	now := ms()
	status, pub, dl := TRejected, int64(0), int64(0)
	if pass {
		status, pub = "open", now
		if t.DeadlineDays > 0 {
			dl = now + t.DeadlineDays*dayMs
		}
	}
	ok, _ := a.st.FinishReview(t.ID, status, result, note, pub, dl)
	if !ok {
		return false
	}
	a.st.Audit(by, "task.review", "task", t.ID, map[string]any{"result": result}, "")
	if pass {
		a.notify(t.OwnerID, "task", "任务已通过审核，正式上线", fmt.Sprintf("《%s》：%s。现在会出现在大厅，可以被接单了。", t.Title, note), t.Path())
	} else {
		a.notify(t.OwnerID, "task", "任务未通过审核", fmt.Sprintf("《%s》：%s。可以修改文案后重新提交审核。", t.Title, note), t.Path())
	}
	return true
}

func (a *App) notifyAdmins(title, body, link string) {
	for h := range a.cfg.AdminHandles {
		if u, _ := a.st.GetUserByHandle(h); u != nil && a.isAdmin(u) {
			a.notify(u.ID, "admin", title, body, link)
		}
	}
}

// reviewTick 每分钟：判定审核中的任务。
func (a *App) reviewTick(now int64) {
	list, _ := a.st.TasksInReview()
	for _, t := range list {
		if !a.cfg.ReviewEnabled {
			a.finishReview(t, true, "timeout_pass", "审核功能已关闭，自动上线", 0)
			continue
		}
		a.decideReview(t, now)
	}
}

// ---- 页面 ----

type reviewPage struct {
	Base
	Tasks    []*Task
	Owners   map[int64]*User
	Stats    map[int64]PubStats
	Infos    map[int64]*ReviewInfo
	Eligible bool
	Why      string
	Reasons  []string
	MyVotes  int64
	Cfg      *Config
}

func (a *App) handleReview(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	p := reviewPage{Base: a.base(w, r), Owners: map[int64]*User{}, Stats: map[int64]PubStats{}, Infos: map[int64]*ReviewInfo{}, Reasons: voteReasons, Cfg: a.cfg}
	p.NoIndex = true
	p.Eligible, p.Why = a.voterEligible(u, nil)
	p.Tasks, _ = a.st.ReviewTasksFor(u.ID)
	for _, t := range p.Tasks {
		if _, ok := p.Owners[t.OwnerID]; !ok {
			if o, _ := a.st.GetUserByID(t.OwnerID); o != nil {
				p.Owners[t.OwnerID] = o
				p.Stats[t.OwnerID] = a.st.PubStats(t.OwnerID)
			}
		}
		p.Infos[t.ID] = a.reviewInfo(t, u)
	}
	p.MyVotes = a.st.VotesSince(u.ID, 0)
	a.render(w, http.StatusOK, "review", p)
}

func (a *App) handleReviewVote(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "vote", 60, 10*time.Minute) {
		return
	}
	next := safeNext(r.FormValue("next"))
	if next == "/me" {
		next = "/review"
	}
	t, _ := a.st.GetTaskByCode(r.PathValue("code"))
	if t == nil || t.Status != TReview {
		a.flash(w, "任务不在审核中")
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	if ok, why := a.voterEligible(u, t); !ok {
		a.flash(w, why)
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	var vote int64
	switch r.FormValue("vote") {
	case "pass":
		vote = 1
	case "fail":
		vote = -1
	default:
		a.flash(w, "请选择「没问题」或「有问题」")
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	reason := ""
	if vote == -1 {
		reason = cleanText(r.FormValue("reason"), 40, false)
		if txt := cleanText(r.FormValue("text"), 200, false); txt != "" {
			reason = strings.Trim(reason+"："+txt, "：")
		}
		if reason == "" {
			reason = "未说明"
		}
	}
	if err := a.st.Vote(t.ID, u.ID, vote, reason); err != nil {
		a.fail(w, r, err)
		return
	}
	a.st.Audit(u.ID, "task.vote", "task", t.ID, map[string]any{"vote": vote}, a.ip(r))
	if nt, _ := a.st.GetTaskByID(t.ID); nt != nil {
		a.decideReview(nt, ms())
	}
	if vote == 1 {
		a.flash(w, "已投「没问题」")
	} else {
		a.flash(w, "已投「有问题」")
	}
	http.Redirect(w, r, next, http.StatusFound)
}
