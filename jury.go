package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const juryMinAgeMs = 14 * dayMs

// shouldJury 该申诉是否交小法庭。
func (a *App) shouldJury(d *Dispute) bool {
	if !a.cfg.JuryEnabled || !a.cfg.JuryTypes[d.Type] || d.SubmissionID == 0 {
		return false
	}
	return a.st.JuryPoolSize(juryMinAgeMs) >= a.cfg.JuryMinPool
}

// openJury 开庭：建案、抽人、通知。
func (a *App) openJury(d *Dispute) error {
	id, err := a.st.CreateJuryCase(d.ID, ms()+a.cfg.JuryWindowH*hourMs)
	if err != nil {
		return err
	}
	if err := a.st.SetDisputeJury(d.ID, id); err != nil {
		return err
	}
	a.inviteRound(id, d, 1)
	a.st.Audit(0, "jury.open", "dispute", d.ID, map[string]any{"case": id}, "")
	for _, uid := range []int64{d.OpenerID, d.AgainstID} {
		a.notify(uid, "dispute", "申诉 "+d.Code+" 已交小法庭", fmt.Sprintf("随机抽选的用户将在 %s 内投票裁决。", dur(a.cfg.JuryWindowH)), d.Path())
	}
	return nil
}

func (a *App) inviteRound(caseID int64, d *Dispute, round int64) {
	cands, err := a.st.JuryCandidates(caseID, d.OpenerID, d.AgainstID, juryMinAgeMs, a.cfg.JuryInvite)
	if err != nil {
		a.logf("[error] 抽选陪审员: %v", err)
		return
	}
	var picked []int64
	for _, id := range cands {
		if a.st.SharedIP(id, d.OpenerID) || a.st.SharedIP(id, d.AgainstID) {
			continue
		}
		picked = append(picked, id)
		if int64(len(picked)) >= a.cfg.JuryInvite {
			break
		}
	}
	a.st.Invite(caseID, picked, round)
	for _, id := range picked {
		a.notify(id, "jury", "你被抽中担任陪审员", fmt.Sprintf("案件 #%d（%s），请在截止前投票。", caseID, disputeType(d.Type)), fmt.Sprintf("/court/%d", caseID))
	}
}

// juryTick 定时：凑齐即结案；24h 未凑齐补抽；截止按多数或转管理员。
func (a *App) juryTick() {
	cases, _ := a.st.VotingCases()
	for _, c := range cases {
		d, _ := a.st.GetDisputeByID(c.DisputeID)
		if d == nil || d.Status != "jury" {
			a.st.CloseCase(c.ID, "escalated", "")
			continue
		}
		switch {
		case a.readyToClose(c):
			a.closeJury(c, d)
		case ms() >= c.DeadlineAt:
			if validMajority(c) {
				a.closeJury(c, d)
			} else if ok, _ := a.st.CloseCase(c.ID, "escalated", ""); ok {
				a.st.SettleJurors(c.ID, "")
				a.st.SetDisputeStatus(d.ID, "review")
				a.st.Audit(0, "jury.escalate", "dispute", d.ID, map[string]any{"for": c.VotesFor, "against": c.VotesAgainst}, "")
			}
		case c.Round == 1 && ms()-c.CreatedAt > dayMs:
			a.st.NextRound(c.ID)
			a.inviteRound(c.ID, d, 2)
		}
	}
}

// validMajority 有效票（非弃权）≥5 且不平局。
func validMajority(c *JuryCase) bool {
	return c.VotesFor+c.VotesAgainst >= 5 && c.VotesFor != c.VotesAgainst
}

// readyToClose 凑齐成庭人数且已有有效多数。
func (a *App) readyToClose(c *JuryCase) bool {
	return c.VotesFor+c.VotesAgainst+c.Abstain >= a.cfg.JuryPanel && validMajority(c)
}

// closeJury 多数裁决 → 进入上诉期（涉黑名单）或直接执行。CloseCase 只对 voting 生效，防止重复结案。
func (a *App) closeJury(c *JuryCase, d *Dispute) {
	verdict := "for"
	if c.VotesAgainst > c.VotesFor {
		verdict = "against"
	} else if c.VotesAgainst == c.VotesFor {
		if ok, _ := a.st.CloseCase(c.ID, "escalated", ""); ok {
			a.st.SettleJurors(c.ID, "")
			a.st.SetDisputeStatus(d.ID, "review")
		}
		return
	}
	if ok, _ := a.st.CloseCase(c.ID, "closed", verdict); !ok {
		return
	}
	a.st.SettleJurors(c.ID, verdict)
	a.banBadJurors(c.ID)
	resolution, note := juryResolution(d.Type, verdict)
	heavy := resolution == "b_fake" || resolution == "upheld" && d.Type == "A"
	if heavy && a.cfg.JuryAppealH > 0 {
		a.st.SetDisputeAppeal(d.ID, ms()+a.cfg.JuryAppealH*hourMs, resolution, note)
		for _, uid := range []int64{d.OpenerID, d.AgainstID} {
			a.notify(uid, "dispute", "小法庭已裁决："+note, fmt.Sprintf("%s 内可申请管理员复核，逾期自动生效。", dur(a.cfg.JuryAppealH)), d.Path())
		}
		return
	}
	if err := a.applyResolution(d, resolution, "小法庭裁决："+note, 0, "", ""); err != nil && !errors.Is(err, ErrState) {
		a.logf("[error] 执行小法庭裁决 %s: %v", d.Code, err)
		a.st.SetDisputeStatus(d.ID, "review")
	}
}

// juryResolution 二元裁决映射到结论：重罚由管理员复核链兜底。
func juryResolution(typ, verdict string) (string, string) {
	switch typ {
	case "B":
		if verdict == "for" {
			return "b_fake", "发布方未付款，标记不实"
		}
		return "b_wrong_uid", "款项已到账或责任不在发布方，记录完成（如认为接单方故意否认，可上诉请管理员复核）"
	case "D":
		if verdict == "for" {
			return "upheld", "推文不合格，记录作废"
		}
		return "rejected", "推文合格，维持待付款"
	case "A":
		if verdict == "for" {
			return "upheld", "确未付款"
		}
		return "rejected", "不成立"
	case "C":
		if verdict == "for" {
			return "rejected", "小法庭不能强制通过验证，转管理员"
		}
		return "rejected", "不成立"
	}
	return "rejected", "不成立"
}

// banBadJurors 20 票内一致率 < 50% 的陪审员停权 30 天。
func (a *App) banBadJurors(caseID int64) {
	votes, _ := a.st.Votes(caseID)
	for _, v := range votes {
		u, _ := a.st.GetUserByID(v.JurorID)
		if u != nil && u.JuryTotal >= 20 && u.JuryAgree*2 < u.JuryTotal {
			a.st.SetJuryBan(u.ID, ms()+30*dayMs)
			a.st.db.Exec(`UPDATE users SET jury_total=0, jury_agree=0 WHERE id=?`, u.ID)
			a.notify(u.ID, "jury", "陪审资格暂停 30 天", "近 20 票与最终裁决一致率不足 50%。", "/court")
		}
	}
}

// ---- 页面 ----

type courtPage struct {
	Base
	Invited  []*JuryCase
	Recent   []*JuryCase
	Disputes map[int64]*Dispute
	Pool     int64
	Enabled  bool
	Eligible bool
	Cfg      *Config
}

func (a *App) handleCourt(w http.ResponseWriter, r *http.Request) {
	p := courtPage{Base: a.base(w, r), Disputes: map[int64]*Dispute{}, Enabled: a.cfg.JuryEnabled, Cfg: a.cfg}
	p.Pool = a.st.JuryPoolSize(juryMinAgeMs)
	if p.Me != nil {
		p.Invited, _ = a.st.InvitedCasesFor(p.Me.ID)
		p.Eligible = a.st.count(`SELECT COUNT(*) FROM submissions x WHERE x.status='paid' AND x.confirm_method IN ('gateway','manual','admin') AND x.self_deal=0 AND x.task_id IN (SELECT id FROM tasks WHERE kind NOT IN ('like','repost')) AND (x.worker_id=? OR x.task_id IN (SELECT id FROM tasks WHERE owner_id=?))`, p.Me.ID, p.Me.ID) >= 3 && ms()-p.Me.CreatedAt >= juryMinAgeMs
	}
	p.Recent, _ = a.st.RecentCases(30)
	for _, c := range append(append([]*JuryCase{}, p.Invited...), p.Recent...) {
		if _, ok := p.Disputes[c.DisputeID]; !ok {
			if d, _ := a.st.GetDisputeByID(c.DisputeID); d != nil {
				p.Disputes[c.DisputeID] = d
			}
		}
	}
	a.render(w, http.StatusOK, "court", p)
}

type courtCasePage struct {
	Base
	C        *JuryCase
	D        *Dispute
	DP       disputePage
	Invited  bool
	MyVote   *JuryVote
	CanVote  bool
	Votes    []JuryVote
	Reasons  []string
	Deadline int64
}

func (a *App) handleCourtCase(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, _ := a.st.GetJuryCase(id)
	if c == nil {
		a.errorPage(w, r, http.StatusNotFound, "案件不存在", "")
		return
	}
	d, _ := a.st.GetDisputeByID(c.DisputeID)
	if d == nil {
		a.errorPage(w, r, http.StatusNotFound, "案件不存在", "")
		return
	}
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	p := courtCasePage{Base: a.base(w, r), C: c, D: d, Deadline: c.DeadlineAt}
	p.Invited = a.st.Invited(c.ID, u.ID)
	p.MyVote = a.st.MyVote(c.ID, u.ID)
	p.CanVote = p.Invited && c.Status == "voting" && p.MyVote == nil && u.ID != d.OpenerID && u.ID != d.AgainstID
	// 案件页对所有登录用户公开事实与陈述；证据图只对受邀陪审员/当事人/管理员开放（handleEvidence 里判定）
	p.DP = a.buildDisputePage(w, r, d, u, "")
	if !a.canSeeDispute(d, u) {
		for i := range p.DP.Msgs {
			p.DP.Msgs[i].Images = nil // 证据图只给当事方、管理员、受邀陪审员
		}
	}
	if c.Status != "voting" {
		p.Votes, _ = a.st.Votes(c.ID)
		for _, v := range p.Votes {
			if strings.TrimSpace(v.Reason) != "" {
				p.Reasons = append(p.Reasons, v.Reason)
			}
		}
	}
	a.render(w, http.StatusOK, "courtcase", p)
}

func (a *App) handleCourtVote(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "vote", 10, time.Minute) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, _ := a.st.GetJuryCase(id)
	if c == nil || c.Status != "voting" || !a.st.Invited(c.ID, u.ID) {
		a.errorPage(w, r, http.StatusForbidden, "你不是本案陪审员或案件已结束", "")
		return
	}
	d, _ := a.st.GetDisputeByID(c.DisputeID)
	if d == nil || u.ID == d.OpenerID || u.ID == d.AgainstID {
		a.errorPage(w, r, http.StatusForbidden, "当事人不能投票", "")
		return
	}
	vote := r.FormValue("vote")
	if vote != "for" && vote != "against" && vote != "abstain" {
		a.flash(w, "请选择投票选项")
		http.Redirect(w, r, fmt.Sprintf("/court/%d", id), http.StatusFound)
		return
	}
	reason := cleanText(r.FormValue("reason"), 100, false)
	if ok, err := a.st.CastVote(c.ID, u.ID, vote, reason); err != nil {
		a.fail(w, r, err)
		return
	} else if !ok {
		a.flash(w, "你已经投过票了")
	} else {
		a.st.Audit(u.ID, "jury.vote", "dispute", d.ID, map[string]any{"case": c.ID}, a.ip(r))
		a.flash(w, "已投票，感谢参与")
		if nc, _ := a.st.GetJuryCase(c.ID); nc != nil && a.readyToClose(nc) {
			a.closeJury(nc, d)
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/court/%d", id), http.StatusFound)
}
