package main

import (
	"net/http"
	"time"
)

// 运营看板（管理员）：注册 → 绑 UID → 接单 → 完成的漏斗、近 14 天走势、任务供给与门槛、记录与付款用时、风险信号。
// 全是只读聚合查询，页面不缓存（管理员偶尔看，量小）。

type statsPage struct {
	Base
	Funnel   map[string]int64
	Days     []dayRow
	Tasks    map[string]int64
	Reviews  []kv
	Retents  []kv
	OpenGate []gateRow
	Subs     []kv
	Pays     []kv
	Errs     []kv
	Timing   map[string]float64
	Risk     map[string]int64
}

type kv struct {
	K string
	N int64
}

type dayRow struct {
	Day                                    string
	Regs, Claims, Verified, Paid, NewTasks int64
}

type gateRow struct {
	Code, Title  string
	MinDays      int64
	MinFollowers int64
	Left         int64
	Eligible     int64
}

func (a *App) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	s := a.st
	now := ms()
	p := statsPage{Base: a.base(w, r), Funnel: map[string]int64{}, Tasks: map[string]int64{}, Timing: map[string]float64{}, Risk: map[string]int64{}}
	p.NoIndex = true
	p.Funnel["users"] = s.count(`SELECT COUNT(*) FROM users`)
	p.Funnel["uid"] = s.count(`SELECT COUNT(*) FROM pay_profiles`)
	p.Funnel["claimed"] = s.count(`SELECT COUNT(DISTINCT worker_id) FROM submissions`)
	p.Funnel["done"] = s.count(`SELECT COUNT(DISTINCT worker_id) FROM submissions WHERE status='paid'`)
	p.Funnel["publishers"] = s.count(`SELECT COUNT(DISTINCT owner_id) FROM tasks`)
	p.Funnel["active24"] = s.count(`SELECT COUNT(*) FROM users WHERE last_login_at > ?`, now-dayMs)
	p.Funnel["active7"] = s.count(`SELECT COUNT(*) FROM users WHERE last_login_at > ?`, now-7*dayMs)
	p.Funnel["tg"] = s.TGLinkCount()
	p.Funnel["new24"] = s.count(`SELECT COUNT(*) FROM users WHERE created_at > ?`, now-dayMs)

	// 近 14 天（按东八区日期）
	const day = `strftime('%m-%d', created_at/1000, 'unixepoch', '+8 hours')`
	perDay := func(q string, args ...any) map[string]int64 {
		out := map[string]int64{}
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			var n int64
			if rows.Scan(&d, &n) == nil {
				out[d] = n
			}
		}
		return out
	}
	since := now - 14*dayMs
	regs := perDay(`SELECT `+day+`, COUNT(*) FROM users WHERE created_at > ? GROUP BY 1`, since)
	claims := perDay(`SELECT `+day+`, COUNT(*) FROM audit_log WHERE action='sub.claim' AND created_at > ? GROUP BY 1`, since)
	verified := perDay(`SELECT `+day+`, COUNT(*) FROM audit_log WHERE action='sub.verified' AND created_at > ? GROUP BY 1`, since)
	paid := perDay(`SELECT `+day+`, COUNT(*) FROM audit_log WHERE action='sub.paid' AND created_at > ? GROUP BY 1`, since)
	newTasks := perDay(`SELECT `+day+`, COUNT(*) FROM tasks WHERE created_at > ? GROUP BY 1`, since)
	loc := time.FixedZone("CST", 8*3600)
	for i := 13; i >= 0; i-- {
		d := time.UnixMilli(now).In(loc).AddDate(0, 0, -i).Format("01-02")
		p.Days = append(p.Days, dayRow{Day: d, Regs: regs[d], Claims: claims[d], Verified: verified[d], Paid: paid[d], NewTasks: newTasks[d]})
	}

	// 任务供给
	p.Tasks["open"] = s.count(`SELECT COUNT(*) FROM tasks WHERE status='open'`)
	p.Tasks["review"] = s.count(`SELECT COUNT(*) FROM tasks WHERE status='review'`)
	p.Tasks["left"] = s.count(`SELECT COALESCE(SUM(slots_total - (SELECT COUNT(*) FROM submissions x WHERE x.task_id=t.id AND x.status NOT IN ('expired','void'))),0) FROM tasks t WHERE status='open'`)
	p.Tasks["total"] = s.count(`SELECT COUNT(*) FROM tasks`)
	p.Tasks["review_h"] = s.count(`SELECT COALESCE(CAST(AVG(published_at-review_started_at)/3600000 AS INTEGER),0) FROM tasks WHERE review_started_at>0 AND published_at>review_started_at`)
	p.Reviews = a.kvQuery(`SELECT COALESCE(NULLIF(review_result,''),''), COUNT(*) FROM tasks GROUP BY 1 ORDER BY 2 DESC`)
	for i := range p.Reviews {
		p.Reviews[i].K = reviewResultText(p.Reviews[i].K)
	}
	p.Retents = a.kvQuery(`SELECT CASE WHEN retention_h=0 THEN '无' ELSE retention_h || ' 小时' END, COUNT(*) FROM tasks GROUP BY retention_h ORDER BY retention_h`)
	if rows, err := s.db.Query(`SELECT code,title,min_account_days,min_followers,slots_total-(SELECT COUNT(*) FROM submissions x WHERE x.task_id=t.id AND x.status NOT IN ('expired','void')),
		(SELECT COUNT(*) FROM users u WHERE (t.min_account_days=0 OR u.x_created_ms=0 OR u.x_created_ms < ?-t.min_account_days*86400000) AND (t.min_followers=0 OR u.followers>=t.min_followers))
		FROM tasks t WHERE status='open' ORDER BY id DESC`, now); err == nil {
		for rows.Next() {
			var g gateRow
			if rows.Scan(&g.Code, &g.Title, &g.MinDays, &g.MinFollowers, &g.Left, &g.Eligible) == nil {
				p.OpenGate = append(p.OpenGate, g)
			}
		}
		rows.Close()
	}

	// 记录与付款
	p.Subs = a.kvQuery(`SELECT status, COUNT(*) FROM submissions GROUP BY 1 ORDER BY 2 DESC`)
	p.Pays = a.kvQuery(`SELECT COALESCE(NULLIF(confirm_method,''),'—'), COUNT(*) FROM submissions WHERE status='paid' GROUP BY 1 ORDER BY 2 DESC`)
	p.Errs = a.kvQuery(`SELECT substr(last_error,1,40), COUNT(*) FROM submissions WHERE last_error<>'' GROUP BY 1 ORDER BY 2 DESC LIMIT 6`)
	p.Timing["claim_to_verified_min"] = a.floatQuery(`SELECT COALESCE(AVG((verified_at-claimed_at)/60000.0),0) FROM submissions WHERE verified_at>claimed_at AND claimed_at>0`)
	p.Timing["payable_to_paid_h"] = a.floatQuery(`SELECT COALESCE(AVG((confirmed_at-payable_at)/3600000.0),0) FROM submissions WHERE status='paid' AND confirmed_at>payable_at AND payable_at>0`)
	p.Timing["verify_fail_rate"] = 0
	if sub := s.count(`SELECT COUNT(*) FROM audit_log WHERE action='sub.submit'`); sub > 0 {
		p.Timing["verify_fail_rate"] = 100 - 100*float64(s.count(`SELECT COUNT(*) FROM audit_log WHERE action='sub.verified'`))/float64(sub)
	}

	// 风险信号
	p.Risk["blacklist"] = s.count(`SELECT COUNT(*) FROM blacklist WHERE lifted_at=0`)
	p.Risk["disputes_open"] = s.count(`SELECT COUNT(*) FROM disputes WHERE status<>'resolved'`)
	p.Risk["overdue"] = s.count(`SELECT COUNT(*) FROM submissions WHERE status='overdue'`)
	p.Risk["followers_unknown"] = s.count(`SELECT COUNT(*) FROM users WHERE followers<0`)
	p.Risk["self_deal"] = s.count(`SELECT COUNT(*) FROM submissions WHERE self_deal=1`)
	p.Risk["shared_prefix"] = s.count(`SELECT COUNT(*) FROM (SELECT prefix FROM ip_log GROUP BY prefix HAVING COUNT(DISTINCT user_id)>=3)`)
	p.Risk["x_young"] = s.count(`SELECT COUNT(*) FROM users WHERE x_created_ms>0 AND x_created_ms > ?`, now-30*dayMs)
	a.render(w, http.StatusOK, "adminstats", p)
}

func (a *App) kvQuery(q string, args ...any) []kv {
	rows, err := a.st.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []kv
	for rows.Next() {
		var k kv
		if rows.Scan(&k.K, &k.N) == nil {
			out = append(out, k)
		}
	}
	return out
}

func (a *App) floatQuery(q string, args ...any) float64 {
	var f float64
	a.st.db.QueryRow(q, args...).Scan(&f)
	return f
}

// reviewResultText 审核结果代码 → 文案。
func reviewResultText(r string) string {
	switch r {
	case "":
		return "未经审核（审核上线前发布）"
	case "vote_pass":
		return "投票通过"
	case "vote_fail":
		return "投票驳回"
	case "timeout_pass":
		return "到期自动上线"
	case "skipped":
		return "免审"
	case "admin_pass", "admin_approve":
		return "管理员通过"
	case "admin_reject":
		return "管理员驳回"
	case "hold":
		return "等管理员裁定"
	}
	return r
}
