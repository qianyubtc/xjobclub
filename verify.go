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
	order := make([]int, 0, len(t.ContentsNorm))
	if prefer >= 0 && int(prefer) < len(t.ContentsNorm) {
		order = append(order, int(prefer))
	}
	for i := range t.ContentsNorm {
		if int64(i) != prefer {
			order = append(order, i)
		}
	}
	for _, i := range order {
		want := t.ContentsNorm[i]
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

// checkTweet 七项检查里除"链接可解析"以外的六项（抓取失败由调用方处理）。
func checkTweet(t *Task, worker *User, tw *Tweet, prefer int64) verdict {
	if tw.User.ID == "" || tw.User.ID != worker.XID {
		return verdict{Reason: "这条推文不是你的账号发的（@" + tw.User.Handle + "）", Tweet: tw}
	}
	if tw.CreatedMs > 0 && tw.CreatedMs < t.PublishedAt-5*60*1000 {
		return verdict{Reason: "推文早于任务发布时间，不能用旧推文", Tweet: tw}
	}
	if tw.IsReply {
		return verdict{Reason: "这是一条回复，请发原帖（可以引用，但不能回复）", Tweet: tw}
	}
	norm := normTweet(tw.Text)
	if norm == "" {
		return verdict{Reason: "推文正文为空", Tweet: tw}
	}
	ok, idx := matchContent(t, norm, prefer)
	if !ok {
		want := ""
		if prefer >= 0 && int(prefer) < len(t.ContentsNorm) {
			want = t.ContentsNorm[prefer]
		} else if len(t.ContentsNorm) > 0 {
			want = t.ContentsNorm[0]
		}
		hint := diffHint(want, norm)
		if t.MatchMode == "contains" {
			hint = "推文里没有完整包含任务文案。" + hint
		} else {
			hint = "推文正文与任务文案不一致。" + hint
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
	if recheck > 0 {
		a.notify(w.ID, "verify", "验证通过", fmt.Sprintf("推文需保留 %s，复检通过后进入待付款。", dur(t.RetentionH)), x.Path())
	} else {
		a.notify(w.ID, "verify", "验证通过，等待付款", fmt.Sprintf("发布方须在 %s 内付款。", dur(t.PayWindowH)), x.Path())
		a.notify(t.OwnerID, "pay", "有一条记录待付款", fmt.Sprintf("@%s 已完成任务 %s，请在 %s 内付款 %s U。", w.Handle, t.Code, dur(t.PayWindowH), fmtE8(t.RewardE8)), x.Path())
	}
}

// recheckSubmission 留存复检：重新跑全部检查。
func (a *App) recheckSubmission(x *Submission) {
	t, err := a.st.GetTaskByID(x.TaskID)
	if err != nil || t == nil {
		return
	}
	w, err := a.st.GetUserByID(x.WorkerID)
	if err != nil || w == nil {
		return
	}
	deadline := ms() + t.PayWindowH*hourMs
	pass := func(flag string) {
		if ok, _ := a.st.SetPayable(x.ID, deadline, flag); ok {
			a.st.Audit(0, "sub.payable", "submission", x.ID, map[string]any{"flag": flag}, "")
			a.notify(t.OwnerID, "pay", "有一条记录待付款", fmt.Sprintf("@%s 的推文留存复检通过，请在 %s 内付款 %s U。", w.Handle, dur(t.PayWindowH), fmtE8(t.RewardE8)), x.Path())
			a.notify(w.ID, "verify", "留存复检通过", fmt.Sprintf("发布方须在 %s 内付款。", dur(t.PayWindowH)), x.Path())
		}
	}
	tw, ferr := a.fetchTweet(x.TweetID, "recheck")
	if ferr != nil && isRetryable(ferr) {
		if (x.RecheckTries+1)*30 >= a.cfg.RecheckUnknownH*60 {
			pass("复检未确认：X 接口持续不可用，按通过处理")
			return
		}
		a.st.SetRecheckRetry(x.ID, ms()+30*60*1000, "复检暂时无法读取推文，稍后重试")
		return
	}
	forced := strings.HasPrefix(x.RecheckFlag, "forced")
	reason := ""
	if ferr != nil {
		// 读不到（已删 / 受保护 / 临时锁号）不是确定性结论：间隔 ≥1 小时连续 3 次才作废
		n := x.Unreadable + 1
		if n < 3 {
			a.st.db.Exec(`UPDATE submissions SET unreadable=?, recheck_due_at=?, recheck_flag=?, updated_at=? WHERE id=? AND status='verified'`, n, ms()+hourMs, fmt.Sprintf("复检读不到推文（第 %d/3 次），1 小时后再确认", n), ms(), x.ID)
			return
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
			pass("forced：管理员强制通过，复检仅核对可读性与作者")
		} else {
			pass("")
		}
		return
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
}

// truncate 截断展示用。
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
