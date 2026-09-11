package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// verdict 一次验证/复检的结论。
type verdict struct {
	OK     bool
	Retry  bool   // 瞬时故障，稍后再试
	Reason string // 不通过原因（给接单方看）
	Text   string // 通过时的规范化正文
	Tweet  *Tweet
}

// matchContent 规范化正文与任务任一变体比对，优先分配到的变体。返回命中的变体下标。
func matchContent(t *Task, norm string, prefer int64) (bool, int) {
	norms := taskNorms(t)
	order := make([]int, 0, len(norms))
	if prefer >= 0 && int(prefer) < len(norms) {
		order = append(order, int(prefer))
	}
	for i := range norms {
		if int64(i) != prefer {
			order = append(order, i)
		}
	}
	for _, i := range order {
		want := norms[i]
		if want == "" {
			continue
		}
		if t.MatchMode == "contains" {
			if strings.Contains(norm, want) {
				return true, i
			}
		} else if norm == want {
			return true, i
		}
	}
	return false, -1
}

// taskNorms 任务文案的规范化结果，比对时现算（规范化规则升级后旧任务不会因存的旧结果而误判）。
func taskNorms(t *Task) []string {
	if len(t.Contents) == 0 {
		return t.ContentsNorm
	}
	out := make([]string, len(t.Contents))
	for i, c := range t.Contents {
		out[i] = normTweet(c)
	}
	return out
}

// diffHint 指出第一处差异，帮接单方改正。
func diffHint(want, got string) string {
	w, g := []rune(want), []rune(got)
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	if i == len(w) && i == len(g) {
		return ""
	}
	lo := i - 12
	if lo < 0 {
		lo = 0
	}
	seg := func(r []rune) string {
		hi := i + 12
		if hi > len(r) {
			hi = len(r)
		}
		s := string(r[lo:hi])
		if lo > 0 {
			s = "…" + s
		}
		if hi < len(r) {
			s += "…"
		}
		return s
	}
	return fmt.Sprintf("第 %d 个字符起不一致。任务文案：「%s」 你的推文：「%s」", i+1, seg(w), seg(g))
}

// closeVerifyDispute 推文已通过验证：这条记录上未结的 C 类（验证误判）申诉自动结案。
func (a *App) closeVerifyDispute(x *Submission, w *User) {
	d, _ := a.st.OpenDisputeForSub(x.ID)
	if d == nil || d.Type != "C" {
		return
	}
	a.st.ResolveDispute(d.ID, "resolved_by_verify", "推文已通过验证，申诉自动结案", 0)
	a.st.Audit(0, "dispute.auto_close", "dispute", d.ID, map[string]any{"by": "verify"}, "")
	a.notify(w.ID, "dispute", "申诉 "+d.Code+" 已自动结案", "你的推文已通过验证，无需再人工复核。", x.Path())
}

// checkTweet 七项检查里除"链接可解析"以外的六项（抓取失败由调用方处理）。
func checkTweet(t *Task, worker *User, tw *Tweet, prefer int64) verdict {
	if tw.User.ID == "" || tw.User.ID != worker.XID {
		return verdict{Reason: "这条推文不是你的账号发的（@" + tw.User.Handle + "）", Tweet: tw}
	}
	if tw.CreatedMs > 0 && tw.CreatedMs < t.PublishedAt-5*60*1000 {
		return verdict{Reason: "推文早于任务发布时间，不能用旧推文", Tweet: tw}
	}
	if t.Kind == "reply" {
		if !tw.IsReply || (t.TargetTweetID != "" && tw.InReplyTo != t.TargetTweetID) {
			return verdict{Reason: "这条不是对目标推文的直接回复，请在目标推文下方回复后再提交", Tweet: tw}
		}
	} else if tw.IsReply {
		return verdict{Reason: "这是一条回复，请发原帖（可以引用，但不能回复）", Tweet: tw}
	}
	norm := normTweet(tw.Text)
	if norm == "" {
		return verdict{Reason: "推文正文为空", Tweet: tw}
	}
	if t.Kind == "reply" && t.MatchMode == "any" {
		if n := int64(len([]rune(norm))); n < t.MinLen {
			return verdict{Reason: fmt.Sprintf("评论太短：至少 %d 个字，当前 %d 个", t.MinLen, n), Tweet: tw}
		}
		return verdict{OK: true, Text: norm, Tweet: tw}
	}
	ok, idx := matchContent(t, norm, prefer)
	if !ok {
		norms := taskNorms(t)
		want := ""
		if prefer >= 0 && int(prefer) < len(norms) {
			want = norms[prefer]
		} else if len(norms) > 0 {
			want = norms[0]
		}
		hint := diffHint(want, norm)
		if t.MatchMode == "contains" {
			hint = "推文里没有完整包含任务文案。" + hint
		} else {
			hint = "推文正文与任务文案不一致。" + hint
		}
		if tw.Truncated {
			hint = "这是一条长推文，公开接口只读到了前 280 字，暂时无法补全全文。" + hint + " 可以把任务文案放在推文开头，或稍后再提交一次。"
		}
		return verdict{Reason: hint, Tweet: tw}
	}
	_ = idx
	return verdict{OK: true, Text: norm, Tweet: tw}
}

// verifySubmission 验证一条已提交的记录：抓取 → 检查 → 状态转移 → 通知。
func (a *App) verifySubmission(x *Submission) {
	t, err := a.st.GetTaskByID(x.TaskID)
	if err != nil || t == nil {
		return
	}
	w, err := a.st.GetUserByID(x.WorkerID)
	if err != nil || w == nil {
		return
	}
	tw, ferr := a.fetchTweet(x.TweetID, "verify")
	if ferr != nil {
		if isRetryable(ferr) && x.VerifyRetries < 4 {
			delay := []int64{1, 5, 15, 60}[x.VerifyRetries] * 60 * 1000
			a.st.SetVerifyRetry(x.ID, ms()+delay, "验证中："+ferr.Error()+"，稍后自动重试")
			return
		}
		a.st.SetVerifyFailed(x.ID, ferr.Error()+"。请确认推文已公开发布、账号未设保护后重新提交")
		a.notify(w.ID, "verify", "验证未通过", ferr.Error(), x.Path())
		return
	}
	if tw.ID != "" && (tw.ID != x.TweetID || (tw.RootID != "" && tw.RootID != x.TweetRoot)) {
		// 编辑过的推文归一到最新版本；同一条推文的任何版本只能核销一条记录
		root := tw.RootID
		if root == "" {
			root = tw.ID
		}
		if a.st.TweetUsedElsewhere(tw.ID, root, x.ID) {
			a.st.SetVerifyFailed(x.ID, "这条推文（或它的编辑版本）已用于其它记录")
			a.notify(w.ID, "verify", "验证未通过", "这条推文（或它的编辑版本）已用于其它记录", x.Path())
			return
		}
		if err := a.st.SetTweetCanonical(x.ID, tw.ID, root); err != nil {
			a.st.SetVerifyFailed(x.ID, "这条推文已用于其它记录")
			a.notify(w.ID, "verify", "验证未通过", "这条推文已用于其它记录", x.Path())
			return
		}
		x.TweetID, x.TweetRoot = tw.ID, root
	}
	v := checkTweet(t, w, tw, x.VariantIdx)
	if !v.OK {
		a.st.SetVerifyFailed(x.ID, v.Reason)
		a.notify(w.ID, "verify", "验证未通过", v.Reason, x.Path())
		return
	}
	// 同一推文只能核销一条记录：唯一索引在提交时已保证，这里再防一手作者冒用别人的推文 ID
	var recheck, deadline int64
	if t.RetentionH > 0 {
		recheck = ms() + t.RetentionH*hourMs
	} else {
		deadline = ms() + t.PayWindowH*hourMs
	}
	ok, err := a.st.SetVerified(x.ID, v.Text, tw.CreatedMs, recheck, deadline)
	if err != nil || !ok {
		return
	}
	a.st.Audit(0, "sub.verified", "submission", x.ID, map[string]any{"tweet": x.TweetID}, "")
	a.closeVerifyDispute(x, w) // 之前因误判发起的 C 类申诉自动结案
	if t.CPM() {
		sid, tid := x.ID, x.TweetID
		safeGo("views", func() { a.sampleViews(sid, tid) })
	}
	if recheck > 0 {
		a.notify(w.ID, "verify", "验证通过", fmt.Sprintf("推文需保留 %s，复检通过后进入待付款。", dur(t.RetentionH)), x.Path())
		a.notify(t.OwnerID, "task", "@"+w.Handle+" 已"+t.DoneVerb()+"并通过验证", fmt.Sprintf("《%s》：推文进入 %s 留存期，%s 复检通过后需要你付款%s。等不及可以在记录页点「现在就付」提前结算（放弃留存保护）。", t.Title, dur(t.RetentionH), fmtTime(recheck), payDesc(t)), x.Path())
	} else {
		a.notify(w.ID, "verify", "验证通过，等待付款", fmt.Sprintf("发布方须在 %s 内付款。", dur(t.PayWindowH)), x.Path())
		a.notify(t.OwnerID, "pay", "有一条记录待付款", fmt.Sprintf("@%s 已完成任务 %s，请在 %s 内付款 %s U。", w.Handle, t.Code, dur(t.PayWindowH), fmtE8(payAmount(x, t))), x.Path())
	}
}

// recheckSubmission 留存复检：重新跑全部检查。early = 发布方提前付款（不等留存到期）：
// 只接受"通过 → 待付款"和"不达标 → 作废"两种确定性结论，读不到就原样返回提示、不改复检计划。
// 返回值是给发布方看的提示，定时任务忽略。
func (a *App) recheckSubmission(x *Submission, early bool) string {
	t, err := a.st.GetTaskByID(x.TaskID)
	if err != nil || t == nil {
		return "任务不存在"
	}
	w, err := a.st.GetUserByID(x.WorkerID)
	if err != nil || w == nil {
		return "接单方不存在"
	}
	deadline := ms() + t.PayWindowH*hourMs
	actor := int64(0)
	if early {
		actor = t.OwnerID
	}
	notifyPayable := func(amount, views int64) {
		vs := ""
		if views >= 0 {
			vs = fmt.Sprintf("结算浏览量 %d，", views)
		}
		if early {
			a.notify(w.ID, "verify", "发布方提前付款，不用等留存到期了", fmt.Sprintf("%s报酬 %s U，发布方须在 %s 内付款。", vs, fmtE8(amount), dur(t.PayWindowH)), x.Path())
			return
		}
		a.notify(t.OwnerID, "pay", "有一条记录待付款", fmt.Sprintf("@%s 的推文留存复检通过，%s请在 %s 内付款 %s U。", w.Handle, vs, dur(t.PayWindowH), fmtE8(amount)), x.Path())
		a.notify(w.ID, "verify", "留存复检通过", fmt.Sprintf("%s报酬 %s U，发布方须在 %s 内付款。", vs, fmtE8(amount), dur(t.PayWindowH)), x.Path())
	}
	pass := func(flag string) string {
		if early {
			flag = strings.TrimSpace("发布方提前付款（未等留存到期） " + flag)
		}
		if t.CPM() {
			// 按浏览量计价：此刻读一次浏览量结算；读不到先重试，超过 24 小时按已记录的最大值算
			v, verr := a.fetchViews(x.TweetID)
			if verr != nil {
				if early {
					return "浏览量暂时读不到，现在无法结算，稍后再试"
				}
				if ms()-(x.VerifiedAt+t.RetentionH*hourMs) < dayMs { // 以原始留存到期时间算，重试会改写 recheck_due_at
					a.st.SetRecheckRetry(x.ID, ms()+hourMs, "浏览量暂时读不到，1 小时后重试")
					return ""
				}
				v = x.Views
				flag = strings.TrimSpace(flag + " 浏览量读取失败，按已记录值结算")
			} else if v < x.Views {
				v = x.Views
			}
			if v < 0 {
				v = 0
			}
			amount := cpmAmount(t, v)
			if ok, _ := a.st.SetPayableAmount(x.ID, deadline, flag, amount, v); ok {
				a.st.Audit(actor, "sub.payable", "submission", x.ID, map[string]any{"flag": flag, "views": v, "amount": fmtE8(amount)}, "")
				notifyPayable(amount, v)
			}
			return ""
		}
		if ok, _ := a.st.SetPayable(x.ID, deadline, flag); ok {
			a.st.Audit(actor, "sub.payable", "submission", x.ID, map[string]any{"flag": flag}, "")
			notifyPayable(t.RewardE8, -1)
		}
		return ""
	}
	tw, ferr := a.fetchTweet(x.TweetID, "recheck")
	if ferr != nil && isRetryable(ferr) {
		if early {
			return "X 接口暂时不可用，稍后再试"
		}
		if (x.RecheckTries+1)*30 >= a.cfg.RecheckUnknownH*60 {
			return pass("复检未确认：X 接口持续不可用，按通过处理")
		}
		a.st.SetRecheckRetry(x.ID, ms()+30*60*1000, "复检暂时无法读取推文，稍后重试")
		return ""
	}
	forced := strings.HasPrefix(x.RecheckFlag, "forced")
	reason := ""
	if ferr != nil {
		if early {
			return "现在读不到这条推文（可能已删除或账号受保护），暂不能提前付款；系统会继续按留存期复检"
		}
		// 读不到（已删 / 受保护 / 临时锁号）不是确定性结论：间隔 ≥1 小时连续 3 次才作废
		n := x.Unreadable + 1
		if n < 3 {
			a.st.db.Exec(`UPDATE submissions SET unreadable=?, recheck_due_at=?, recheck_flag=?, updated_at=? WHERE id=? AND status='verified'`, n, ms()+hourMs, fmt.Sprintf("复检读不到推文（第 %d/3 次），1 小时后再确认", n), ms(), x.ID)
			return ""
		}
		reason = ferr.Error() + "（连续 3 次读不到）"
	} else if forced {
		if tw.User.ID != w.XID {
			reason = "推文作者与账号不符"
		}
	} else if v := checkTweet(t, w, tw, x.VariantIdx); !v.OK {
		reason = v.Reason
	} else if x.Unreadable > 0 {
		a.st.db.Exec(`UPDATE submissions SET unreadable=0 WHERE id=?`, x.ID)
	}
	if reason == "" {
		if forced {
			return pass("forced：管理员强制通过，复检仅核对可读性与作者")
		}
		return pass("")
	}
	if ok, _ := a.st.SetVoid(x.ID, []string{SVerified}, "留存不达标："+reason); ok {
		a.st.Audit(0, "sub.void", "submission", x.ID, map[string]any{"reason": reason}, "")
		a.notify(w.ID, "verify", "留存复检未通过，记录作废", reason, x.Path())
		a.notify(t.OwnerID, "verify", "一条记录留存复检未通过", "@"+w.Handle+"："+reason+"，无需付款。", x.Path())
		if a.st.VoidCount(w.ID, ms()-30*dayMs) >= a.cfg.VoidStrikes {
			until := ms() + a.cfg.VoidSuspendDays*dayMs
			a.st.SetSuspended(w.ID, until)
			a.notify(w.ID, "account", fmt.Sprintf("30 天内 %d 次留存不达标，暂停接单 %d 天", a.cfg.VoidStrikes, a.cfg.VoidSuspendDays), "", "/me")
		}
	}
	return ""
}

// truncate 截断展示用。
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// sampleViews 顺手记一次浏览量（只增不减），供记录页展示与读取失败时兜底。
func (a *App) sampleViews(subID int64, tweetID string) {
	if v, err := a.fetchViews(tweetID); err == nil && v >= 0 {
		a.st.SetViews(subID, v)
	}
}

// payDesc 通知里的应付描述：固定单价给金额，按浏览量给封顶。
func payDesc(t *Task) string {
	if t.CPM() {
		return "（按浏览量结算，封顶 " + fmtE8(t.RewardE8) + " U）"
	}
	return " " + fmtE8(t.RewardE8) + " U"
}
