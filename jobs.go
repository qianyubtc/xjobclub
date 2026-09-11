package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// startJobs 进程内定时任务：每分钟一轮，小时/天级任务按上次执行时间判断。
func (a *App) startJobs() {
	a.startTG()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		a.repairCertPayments()
		a.runJobs()
		for {
			select {
			case <-a.stop:
				return
			case <-t.C:
				a.runJobs()
			}
		}
	}()
}

func (a *App) runJobs() {
	if !a.jobMu.TryLock() {
		return
	}
	defer a.jobMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[error] 定时任务 panic: %v", r)
		}
	}()
	now := ms()
	a.expireClaims(now)
	a.retryVerifies(now)
	a.runRechecks(now)
	a.overduePayables(now)
	a.autoApproveCheckings(now)
	a.autoConfirmAwaiting(now)
	a.closeTasks()
	a.reviewTick(now)
	a.syncPending()
	a.routeDisputes(now)
	a.juryTick()
	last, _ := a.st.GetMeta("last_hourly")
	if lastMs := atoi64(last); now-lastMs >= hourMs {
		a.st.SetMeta("last_hourly", fmt.Sprint(now))
		a.ipTouched.Range(func(k, v any) bool { // 节流表只需要最近 10 分钟，超过 1 小时的键清掉，别跟着进程一直长
			if t, ok := v.(int64); ok && now-t > hourMs {
				a.ipTouched.Delete(k)
			}
			return true
		})
		a.checkGatewayHealth()
		since := lastMs
		if since == 0 || now-since > 7*dayMs {
			since = now - hourMs
		}
		a.remind(now, since)
		a.syncExpired()
	}
	lastD, _ := a.st.GetMeta("last_daily")
	if lastMs := atoi64(lastD); now-lastMs >= dayMs {
		a.st.SetMeta("last_daily", fmt.Sprint(now))
		a.st.PruneVerify()
		a.st.db.Exec(`DELETE FROM tweet_cache WHERE fetched_at<? AND tweet_id NOT IN (SELECT tweet_id FROM submissions WHERE tweet_id<>'') AND tweet_id NOT IN (SELECT reg_tweet_id FROM users WHERE reg_tweet_id<>'')`, now-180*dayMs)
		a.st.db.Exec(`DELETE FROM notifications WHERE created_at<?`, now-90*dayMs)
		a.st.db.Exec(`DELETE FROM ip_log WHERE last_seen<?`, now-90*dayMs)
		a.st.db.Exec(`DELETE FROM ip_seen WHERE last_seen<?`, now-90*dayMs)
		a.st.db.Exec(`DELETE FROM device_seen WHERE last_seen<?`, now-90*dayMs)
		a.backup()
	}
}

func atoi64(s string) int64 {
	var n int64
	fmt.Sscan(s, &n)
	return n
}

func (a *App) expireClaims(now int64) {
	subs, _ := a.st.ExpiredClaims(now)
	for _, x := range subs {
		if ok, _ := a.st.SetExpired(x.ID); ok {
			a.st.Audit(0, "sub.expired", "submission", x.ID, nil, "")
			a.notify(x.WorkerID, "verify", "接单已过期", "记录 "+x.Code+" 未在时限内提交，名额已释放。", x.Path())
			if t, _ := a.st.GetTaskByID(x.TaskID); t != nil {
				if w, _ := a.st.GetUserByID(x.WorkerID); w != nil {
					a.notify(t.OwnerID, "task", "@"+w.Handle+" 接单超时，名额已释放", "《"+t.Title+"》可接名额 +1。", t.Path()+"#manage")
				}
			}
		}
	}
}

func (a *App) retryVerifies(now int64) {
	subs, _ := a.st.VerifyRetries(now)
	for _, x := range subs {
		a.verifySubmission(x)
	}
}

func (a *App) runRechecks(now int64) {
	subs, _ := a.st.DueRechecks(now)
	for _, x := range subs {
		a.recheckSubmission(x, false)
	}
}

func (a *App) overduePayables(now int64) {
	subs, _ := a.st.DuePayables(now)
	for _, x := range subs {
		if ok, _ := a.st.SetOverdue(x.ID); !ok {
			continue
		}
		a.st.Audit(0, "sub.overdue", "submission", x.ID, nil, "")
		t, _ := a.st.GetTaskByID(x.TaskID)
		if t == nil {
			continue
		}
		n := a.st.FreezeOwnerTasks(t.OwnerID)
		a.notify(t.OwnerID, "pay", "记录 "+x.Code+" 已逾期未付", fmt.Sprintf("你的账号已冻结（%d 个任务暂停接单），付清后自动恢复。接单方可以举报，核实后将进入黑名单。", n), x.Path())
		a.notify(x.WorkerID, "pay", "发布方逾期未付款", "记录 "+x.Code+" 已逾期，你可以在记录页举报；对方付清会自动通知你。", x.Path())
	}
}

func (a *App) closeTasks() {
	ts, _ := a.st.TasksToClose()
	for _, t := range ts {
		a.st.SetTaskStatus(t.ID, "closed", "deadline", false)
	}
}

// syncPending 每分钟主动对账 pending 的网关订单（回调丢了也能补）。
func (a *App) syncPending() {
	if a.gwc == nil {
		return
	}
	ps, _ := a.st.PendingPayments()
	for _, p := range ps {
		if p.ExpiresAt > 0 && p.ExpiresAt+35*60*1000 < ms() {
			// 早该过期却没收到回调：查一次，网关说过期就落过期
			a.syncPaymentNow(p)
			continue
		}
		if a.st.TouchSync(p.ID, ms(), 50*1000) {
			a.syncPaymentNow(p)
		}
	}
}

// syncExpired 每小时对 7 天内过期的订单再查一次（回填窗口内可能变 paid）。
func (a *App) syncExpired() {
	if a.gwc == nil {
		return
	}
	ps, _ := a.st.ExpiredRecentPayments(50 * 60 * 1000)
	for _, p := range ps {
		a.st.TouchSync(p.ID, ms(), 0)
		a.syncPaymentNow(p)
	}
}

// routeDisputes 举证期满 → 小法庭或管理员；上诉期满 → 执行裁决。
func (a *App) routeDisputes(now int64) {
	ds, _ := a.st.EvidenceExpired(now)
	for _, d := range ds {
		if d.Type == "A" {
			// A 类：宽限期内已付则 afterPaid 已结案；这里只把未付的送去核实
			if x, _ := a.st.GetSubByID(d.SubmissionID); x != nil && x.Status != SDisputed {
				a.st.ResolveDispute(d.ID, "resolved_by_payment", "款项已到账，自动结案", 0)
				continue
			}
		}
		if a.shouldJury(d) {
			if err := a.openJury(d); err == nil {
				continue
			}
		}
		a.st.SetDisputeStatus(d.ID, "review")
	}
	as, _ := a.st.AppealExpired(now)
	for _, d := range as {
		if err := a.applyResolution(d, d.Resolution, d.ResolutionNote, 0, "", ""); err != nil {
			log.Printf("[error] 上诉期满执行裁决 %s: %v", d.Code, err)
			a.st.SetDisputeStatus(d.ID, "review")
		}
	}
}

// remind 每小时：待确认 24h/72h、付款时限剩 12h。窗口下界用上次执行时间，停机期间的阈值不会漏。
func (a *App) remind(now, since int64) {
	for _, h := range a.cfg.ConfirmRemindH {
		subs, _ := a.st.AwaitingSince(now - h*hourMs)
		for _, x := range subs {
			if x.MarkedPaidAt > since-h*hourMs { // 阈值时刻落在 (since, now] 内的只提醒一次
				until := ""
				if a.cfg.AutoConfirmH > h && !(x.TopupRequested > 0 && x.TopupMarkedAt == 0) {
					until = fmt.Sprintf("，%s 后不处理会视为已收到、自动完成", dur(a.cfg.AutoConfirmH-h))
				}
				a.notify(x.WorkerID, "pay", fmt.Sprintf("待确认到账已 %d 小时", h), "记录 "+x.Code+"：请核对后点「已收到」或发起申诉"+until+"。", x.Path())
			}
		}
	}
	subs, _ := a.st.querySubs(`WHERE status='payable' AND pay_deadline_at > ? AND pay_deadline_at <= ?`, since+12*hourMs, now+12*hourMs)
	for _, x := range subs {
		if t, _ := a.st.GetTaskByID(x.TaskID); t != nil {
			a.notify(t.OwnerID, "pay", "付款时限还剩 12 小时", "记录 "+x.Code+"，逾期将被冻结。", x.Path())
		}
	}
}

// backup SQLite 在线备份，保留 14 天。
func (a *App) backup() {
	dir := filepath.Join(filepath.Dir(a.cfg.DBPath), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	name := filepath.Join(dir, "xjobclub-"+time.Now().Format("20060102")+".db")
	if _, err := a.st.db.Exec(`VACUUM INTO ?`, name); err != nil {
		log.Printf("[warn] 备份失败: %v", err)
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 14*24*time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// autoApproveCheckings 点赞/转发待核对超过 48 小时发布方未处理：视为通过，进入待付款。
func (a *App) autoApproveCheckings(now int64) {
	list, _ := a.st.DueCheckings(now - checkWindowMs)
	for _, x := range list {
		t, _ := a.st.GetTaskByID(x.TaskID)
		if t == nil {
			continue
		}
		if ok, _ := a.st.SetCheckedOK(x.ID, []string{SChecking}, now+t.PayWindowH*hourMs, 1); !ok {
			continue
		}
		a.st.Audit(0, "sub.check_auto", "submission", x.ID, nil, "")
		a.notify(x.WorkerID, "verify", "发布方超时未核对，视为通过", fmt.Sprintf("发布方须在 %s 内付款 %s U。", dur(t.PayWindowH), fmtE8(payAmount(x, t))), x.Path())
		a.notify(t.OwnerID, "pay", "待核对超时视为通过，请付款", fmt.Sprintf("《%s》有一条%s记录 48 小时未核对，已进入待付款，请在 %s 内付款 %s U。确实没完成的话可在记录页发起申诉。", t.Title, t.DoneVerb(), dur(t.PayWindowH), fmtE8(payAmount(x, t))), x.Path())
	}
}
