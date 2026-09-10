package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---- 信用等级 ----

func (a *App) pubTier(st PubStats) Tier {
	c := a.cfg
	switch {
	case st.PaidGateway >= 20 && st.Overdue <= 1 && st.AvgPayMs > 0 && st.AvgPayMs < dayMs:
		return Tier{Name: "资深", ExposureE8: c.ExposureSeniorE8, OpenTasks: c.OpenTasksSenior}
	case st.PaidGateway >= 3 && st.Overdue <= 1:
		return Tier{Name: "常规", ExposureE8: c.ExposureRegularE8, OpenTasks: c.OpenTasksRegular}
	}
	return Tier{Name: "新手", ExposureE8: c.ExposureNewbieE8, OpenTasks: c.OpenTasksNewbie}
}

func (a *App) workerTier(st WorkerStats) Tier {
	c := a.cfg
	switch {
	case st.DoneGateway >= 30 && st.Void30d == 0:
		return Tier{Name: "资深", Concurrent: c.ConcurSenior, Daily: c.DailySenior}
	case st.DoneGateway >= 3:
		return Tier{Name: "常规", Concurrent: c.ConcurRegular, Daily: c.DailyRegular}
	}
	return Tier{Name: "新手", Concurrent: c.ConcurNewbie, Daily: c.DailyNewbie}
}

// canPublish 发布前置条件，返回不能发布的原因（空 = 可以）。
func (a *App) canPublish(u *User, st PubStats) string {
	if u.Blacklisted() {
		return "你在黑名单上，不能发布任务"
	}
	if st.Frozen {
		return "你有逾期未付的记录，付清后才能发布新任务"
	}
	if a.workerLockedNow(u.ID) {
		return "你有超过 24 小时未处理的待确认到账记录，先去确认或申诉"
	}
	p, _ := a.st.GetPayProfile(u.ID)
	if p == nil {
		return "请先在「收款设置」里绑定币安 UID"
	}
	if a.cfg.CertFeeEnabled && u.CertPaidAt == 0 {
		return "首次发布前需要完成一次认证付款（" + a.cfg.CertFeeAmount + " U）"
	}
	tier := a.pubTier(st)
	if st.OpenTasks >= tier.OpenTasks {
		return fmt.Sprintf("%s等级最多同时在线 %d 个任务", tier.Name, tier.OpenTasks)
	}
	return ""
}

// canClaim 接单前置条件。
func (a *App) canClaim(u *User, t *Task, st WorkerStats, refresh bool) string {
	if u.ID == t.OwnerID {
		return "不能接自己发布的任务"
	}
	if u.Blacklisted() {
		return "你在黑名单上，不能接单"
	}
	if u.SuspendedUntil > ms() {
		return "你因留存不达标被暂停接单至 " + fmtTime(u.SuspendedUntil)
	}
	if a.workerLockedNow(u.ID) {
		return "你有超过 24 小时未处理的待确认到账记录，先去确认或申诉"
	}
	if p, _ := a.st.GetPayProfile(u.ID); p == nil {
		return "请先在「收款设置」里绑定币安 UID，否则没法收钱"
	}
	if t.Status != "open" {
		return "任务当前不接受接单"
	}
	if a.st.PublisherFrozen(t.OwnerID) > 0 {
		return "发布方当前处于冻结状态（有逾期未付），暂不能接"
	}
	if t.DeadlineAt <= ms() {
		return "任务已截止"
	}
	if t.Left() <= 0 {
		return "名额已满"
	}
	if t.MinAccountDays > 0 && u.XCreatedMs > 0 && ms()-u.XCreatedMs < t.MinAccountDays*dayMs {
		return fmt.Sprintf("该任务要求 X 账号注册满 %d 天", t.MinAccountDays)
	}
	if t.MinFollowers > 0 {
		if refresh {
			a.refreshFollowers(u, false)
		}
		if u.Followers < 0 {
			if !refresh {
				return "" // 页面展示不发外部请求，接单时再读
			}
			return "该任务要求粉丝数，暂时读不到你的粉丝数，请稍后再试（可在「我的 → 设置」手动刷新）"
		}
		if u.Followers < t.MinFollowers {
			return fmt.Sprintf("该任务要求粉丝 ≥ %d，你当前 %d", t.MinFollowers, u.Followers)
		}
	}
	tier := a.workerTier(st)
	if st.Active >= tier.Concurrent {
		return fmt.Sprintf("%s等级最多同时进行 %d 个任务，先完成手头的", tier.Name, tier.Concurrent)
	}
	if st.Today >= tier.Daily {
		return fmt.Sprintf("%s等级每天最多接 %d 单", tier.Name, tier.Daily)
	}
	return ""
}

// refreshFollowers 粉丝数 24 小时内有效；未知或过期就抓一次。
// 成功抓到后 10 分钟内不再抓（手动刷新也一样）；抓失败的话 1 分钟后允许重试，避免注册时一次失败把用户卡住 10 分钟。
func (a *App) refreshFollowers(u *User, force bool) {
	if u == nil {
		return
	}
	if !force && u.Followers >= 0 && ms()-u.FollowersAt < dayMs {
		return
	}
	if u.Followers >= 0 && ms()-u.FollowersAt < 10*time.Minute.Milliseconds() {
		return
	}
	if !a.lim.allow("followers:"+strconv.FormatInt(u.ID, 10), 1, time.Minute) {
		return
	}
	n, err := a.fetchFollowers(u.Handle)
	if err != nil {
		a.logf("[warn] 粉丝数抓取 @%s: %v", u.Handle, err)
		return
	}
	if a.st.SetFollowers(u.ID, n) == nil {
		u.Followers, u.FollowersAt = n, ms()
	}
}

// workerLockedNow 待确认到账超过 24 小时未处理即锁定。
func (a *App) workerLockedNow(userID int64) bool {
	return a.st.count(`SELECT COUNT(*) FROM submissions WHERE worker_id=? AND status='awaiting_confirm' AND marked_paid_at>0 AND marked_paid_at<? AND topup_requested_at=0`, userID, ms()-dayMs) > 0
}

// ---- 大厅 ----

type indexPage struct {
	Base
	Tasks   []*Task
	Owners  map[int64]*User
	Stats   map[int64]PubStats
	Filter  TaskFilter
	Total   int64
	Page    int
	Pages   int
	MinU    string
	Retent  string
	Kind    string
	Sort    string
	Counts  struct{ Users, Open, Paid int64 }
	PaidSum int64
	Recent  []recentDone
}

type recentDone struct {
	Handle   string
	AmountE8 int64
	Ago      string
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := TaskFilter{MaxRetentH: -1, Sort: q.Get("sort")}
	switch q.Get("kind") {
	case "post", "reply", "like", "repost":
		f.Kind = q.Get("kind")
	}
	if v := q.Get("min"); v != "" {
		if e8, err := parseAmountE8(v, 4); err == nil {
			f.MinRewardE8 = e8
		}
	}
	if v := q.Get("ret"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			f.MaxRetentH = n
		}
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	const size = 20
	tasks, total, err := a.st.ListOpenTasks(f, size, (page-1)*size)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	p := indexPage{Base: a.base(w, r), Tasks: tasks, Filter: f, Total: total, Page: page, Pages: int((total + size - 1) / size), MinU: q.Get("min"), Retent: q.Get("ret"), Kind: f.Kind, Sort: f.Sort, Owners: map[int64]*User{}, Stats: map[int64]PubStats{}}
	for _, t := range tasks {
		if _, ok := p.Owners[t.OwnerID]; !ok {
			if u, _ := a.st.GetUserByID(t.OwnerID); u != nil {
				p.Owners[t.OwnerID] = u
				p.Stats[t.OwnerID] = a.st.PubStats(t.OwnerID)
			}
		}
	}
	p.Counts.Users = a.st.count(`SELECT COUNT(*) FROM users`)
	p.Counts.Open = a.st.count(`SELECT COUNT(*) FROM tasks WHERE status='open' AND deadline_at>? AND slots_total>(SELECT COUNT(*) FROM submissions x WHERE x.task_id=tasks.id AND x.status NOT IN ('expired','void'))`, ms()) // 与大厅列表同口径：名额已满的不算「可接」
	p.Counts.Paid = a.st.count(`SELECT COUNT(*) FROM submissions WHERE status='paid'`)
	p.PaidSum = a.st.sum(`SELECT SUM(paid_amount_e8) FROM submissions WHERE status='paid'`)
	if rows, err := a.st.db.Query(`SELECT u.handle, x.paid_amount_e8, x.confirmed_at FROM submissions x JOIN users u ON u.id=x.worker_id WHERE x.status='paid' ORDER BY x.confirmed_at DESC LIMIT 12`); err == nil {
		for rows.Next() {
			var r recentDone
			var at int64
			if rows.Scan(&r.Handle, &r.AmountE8, &at) == nil {
				r.Ago = ago(at)
				p.Recent = append(p.Recent, r)
			}
		}
		rows.Close()
	}
	a.render(w, http.StatusOK, "index", p)
}

// ---- 发布 ----

type newForm struct {
	Title     string
	Contents  []string
	MatchMode string
	Reward    string
	Slots     string
	ClaimTTL  string
	Retention string
	PayWindow string
	Deadline  string
	MinDays   string
	MinFans   string
	TType     string // post | reply | like | repost
	Target    string // 目标推文链接
	MinLen    string
	AdTag     bool
	Lens      []int
	EditCode  string // 编辑已有任务
}

type newPage struct {
	Base
	F          newForm
	Err        string
	Block      string // 不能发布的原因
	Tier       Tier
	Stats      PubStats
	Cfg        *Config
	Exposure   int64
	Remaining  int64
	TTLs       []int64
	CertNeeded bool
	Kinds      []kindOpt
}

type kindOpt struct{ V, N, D string }

var kindOpts = []kindOpt{{"post", "发帖", "发一条推文"}, {"reply", "评论", "回复目标推文"}, {"like", "点赞", "给目标推文点赞"}, {"repost", "转发", "转发目标推文"}}

func (a *App) defaultForm() newForm {
	c := a.cfg
	return newForm{TType: "post", MinLen: "10", Contents: make([]string, c.MaxVariants), MatchMode: "exact", Slots: "5", ClaimTTL: strconv.FormatInt(c.ClaimTTLDefault, 10),
		Retention: strconv.FormatInt(c.RetentionDefault, 10), PayWindow: strconv.FormatInt(c.PayWindowDefault, 10), Deadline: strconv.FormatInt(c.DeadlineDefault, 10), MinDays: "0", MinFans: "0", AdTag: true}
}

func (a *App) newPageData(w http.ResponseWriter, r *http.Request, u *User, f newForm, errMsg string) newPage {
	st := a.st.PubStats(u.ID)
	tier := a.pubTier(st)
	rem := tier.ExposureE8 - st.ExposureE8
	if rem < 0 {
		rem = 0
	}
	var ttls []int64
	for _, v := range []int64{30, 60, 120, 240, 480, 720, 1440} {
		if v >= a.cfg.ClaimTTLMin && v <= a.cfg.ClaimTTLMax {
			ttls = append(ttls, v)
		}
	}
	if !containsInt(ttls, a.cfg.ClaimTTLDefault) {
		ttls = append(ttls, a.cfg.ClaimTTLDefault)
	}
	return newPage{Base: a.base(w, r), Kinds: kindOpts, F: f, Err: errMsg, Block: a.canPublish(u, st), Tier: tier, Stats: st, Cfg: a.cfg, Exposure: st.ExposureE8, Remaining: rem, TTLs: ttls, CertNeeded: a.cfg.CertFeeEnabled && u.CertPaidAt == 0}
}

func (a *App) handleNewGet(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	f := a.defaultForm()
	if code := r.URL.Query().Get("edit"); code != "" {
		if t, _ := a.st.GetTaskByCode(code); t != nil && t.OwnerID == u.ID && t.Status != "closed" {
			f.EditCode, f.Title, f.MatchMode = t.Code, t.Title, t.MatchMode
			f.TType, f.Target, f.MinLen = t.Kind, t.TargetURL, strconv.FormatInt(t.MinLen, 10)
			copy(f.Contents, t.Contents)
		}
	}
	a.render(w, http.StatusOK, "new", a.newPageData(w, r, u, f, ""))
}

// adTagged 给文案加上 #广告 标签（已有广告/推广标签则不重复加）。
func adTagged(s string) string {
	low := strings.ToLower(s)
	for _, tag := range []string{"#广告", "#推广", "#ad ", "#ad\n", "#sponsored"} {
		if strings.Contains(low, tag) || strings.HasSuffix(low, strings.TrimSpace(tag)) {
			return s
		}
	}
	return strings.TrimSpace(s) + " #广告"
}

func (a *App) handleNewPost(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "newtask", 10, 10*time.Minute) {
		return
	}
	c := a.cfg
	f := newForm{Title: cleanText(r.FormValue("title"), 40, false), MatchMode: r.FormValue("match_mode"), Reward: strings.TrimSpace(r.FormValue("reward")), Slots: r.FormValue("slots"),
		ClaimTTL: r.FormValue("claim_ttl"), Retention: r.FormValue("retention"), PayWindow: r.FormValue("pay_window"), Deadline: r.FormValue("deadline"), MinDays: r.FormValue("min_days"), MinFans: strings.TrimSpace(r.FormValue("min_followers")),
		AdTag: r.FormValue("ad_tag") == "1", EditCode: strings.TrimSpace(r.FormValue("edit")), TType: r.FormValue("ttype"), Target: strings.TrimSpace(r.FormValue("target")), MinLen: strings.TrimSpace(r.FormValue("min_len"))}
	for i := int64(0); i < c.MaxVariants; i++ {
		f.Contents = append(f.Contents, cleanText(r.FormValue("content"+strconv.FormatInt(i+1, 10)), 1000, true))
	}
	bad := func(m string) { a.render(w, http.StatusBadRequest, "new", a.newPageData(w, r, u, f, m)) }

	// 编辑：类型以已存的任务为准，不信表单
	if f.EditCode != "" {
		if et, _ := a.st.GetTaskByCode(f.EditCode); et != nil && et.OwnerID == u.ID {
			f.TType = et.Kind
		}
	}
	switch f.TType {
	case "reply", "like", "repost":
	default:
		f.TType = "post"
	}
	manual := f.TType == "like" || f.TType == "repost" // 点赞/转发：无文案、无留存、由发布方核对
	if manual {
		f.AdTag, f.MatchMode = false, "exact"
	}
	if f.MatchMode != "contains" && !(f.TType == "reply" && f.MatchMode == "any") {
		f.MatchMode = "exact"
	}
	freeReply := f.TType == "reply" && f.MatchMode == "any"
	var contents, norm []string
	for _, s := range f.Contents {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if f.AdTag {
			s = adTagged(s)
		}
		if n := weightedLen(s); n > 280 {
			bad(fmt.Sprintf("有一条文案按 X 的算法长 %d（上限 280，中文每字算 2、链接算 23%s）", n, map[bool]string{true: "、含 #广告 标签", false: ""}[f.AdTag]))
			return
		}
		contents = append(contents, s)
		norm = append(norm, normTweet(s))
	}
	if f.Title == "" {
		bad("请填任务标题")
		return
	}
	if manual {
		contents, norm = nil, nil
	}
	if len(contents) == 0 && !manual && !freeReply {
		if f.TType == "reply" {
			bad("至少写一条评论文案，或者把匹配方式选成「自由发挥」")
		} else {
			bad("至少写一条推文文案")
		}
		return
	}
	var minLen int64
	if freeReply {
		minLen, _ = strconv.ParseInt(f.MinLen, 10, 64)
		if minLen < 5 || minLen > 200 {
			minLen = 10
		}
	}
	// 编辑：只允许改标题/文案/匹配方式，且尚无人接单
	if f.EditCode != "" {
		t, _ := a.st.GetTaskByCode(f.EditCode)
		if t == nil || t.OwnerID != u.ID || t.Status == "closed" {
			bad("任务不存在或不能编辑")
			return
		}
		ok, err := a.st.UpdateTaskContent(t.ID, f.Title, contents, norm, f.MatchMode, minLen)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if !ok {
			bad("已经有人接单，文案锁定不能改")
			return
		}
		a.st.Audit(u.ID, "task.edit", "task", t.ID, nil, a.ip(r))
		a.flash(w, "已保存")
		http.Redirect(w, r, t.Path(), http.StatusFound)
		return
	}
	st := a.st.PubStats(u.ID)
	if m := a.canPublish(u, st); m != "" {
		bad(m)
		return
	}
	// 评论/点赞/转发：目标推文必须存在且可读（读一次并缓存作者与摘要）
	var target *Tweet
	if f.TType != "post" {
		tid := tweetIDFrom(f.Target)
		if tid == "" {
			bad("请填目标推文链接，形如 https://x.com/某人/status/1234567890")
			return
		}
		tw, ferr := a.fetchTweet(tid, "target")
		if ferr != nil {
			bad("目标推文读不到：" + ferr.Error())
			return
		}
		if f.TType == "like" && tw.User.ID != u.XID {
			bad("点赞任务的目标必须是你自己发的推文：X 只让作者看到点赞名单，别人的推文你无法核对")
			return
		}
		target = tw
	}
	reward, err := parseAmountE8(f.Reward, 4)
	if err != nil || reward < c.MinRewardE8 || reward > c.MaxRewardE8 {
		bad(fmt.Sprintf("单价须在 %s–%s U 之间，最多 4 位小数", fmtE8(c.MinRewardE8), fmtE8(c.MaxRewardE8)))
		return
	}
	slots, _ := strconv.ParseInt(f.Slots, 10, 64)
	if slots < 1 || slots > c.MaxSlots {
		bad(fmt.Sprintf("名额须在 1–%d 之间", c.MaxSlots))
		return
	}
	ttl, _ := strconv.ParseInt(f.ClaimTTL, 10, 64)
	if ttl < c.ClaimTTLMin || ttl > c.ClaimTTLMax {
		bad(fmt.Sprintf("接单时限须在 %d–%d 分钟之间", c.ClaimTTLMin, c.ClaimTTLMax))
		return
	}
	ret, _ := strconv.ParseInt(f.Retention, 10, 64)
	if manual {
		ret = 0 // 点赞/转发无法复检
	} else if !containsInt(c.RetentionOptions, ret) {
		bad("留存时长不在可选范围")
		return
	}
	pw, _ := strconv.ParseInt(f.PayWindow, 10, 64)
	if !containsInt(c.PayWindowOptions, pw) {
		bad("付款时限不在可选范围")
		return
	}
	days, _ := strconv.ParseInt(f.Deadline, 10, 64)
	if days < 1 || days > c.DeadlineMax {
		bad(fmt.Sprintf("任务截止须在 1–%d 天之间", c.DeadlineMax))
		return
	}
	minDays, _ := strconv.ParseInt(f.MinDays, 10, 64)
	if minDays < 0 || minDays > 3650 {
		minDays = 0
	}
	minFans, _ := strconv.ParseInt(f.MinFans, 10, 64)
	if minFans < 0 || minFans > 100000000 {
		minFans = 0
	}
	tier := a.pubTier(st)
	if st.ExposureE8+reward*slots > tier.ExposureE8 {
		bad(fmt.Sprintf("超出%s等级的敞口上限 %s U（当前已占用 %s U，本任务需要 %s U）。多完成几单网关核销的付款可以升级。", tier.Name, fmtE8(tier.ExposureE8), fmtE8(st.ExposureE8), fmtE8(reward*slots)))
		return
	}
	t := &Task{Code: newCode("T"), OwnerID: u.ID, Title: f.Title, Contents: contents, ContentsNorm: norm, MatchMode: f.MatchMode, RewardE8: reward, Currency: c.Currency,
		SlotsTotal: slots, ClaimTTLMin: ttl, RetentionH: ret, PayWindowH: pw, DeadlineAt: ms() + days*dayMs, MinAccountDays: minDays, MinFollowers: minFans, AdTag: f.AdTag, Kind: f.TType, MinLen: minLen}
	if target != nil {
		t.TargetTweetID, t.TargetURL, t.TargetAuthor, t.TargetText = target.ID, "https://x.com/"+target.User.Handle+"/status/"+target.ID, target.User.Handle, truncate(target.Text, 140)
	}
	var id int64
	for i := 0; i < 3; i++ {
		id, err = a.st.CreateTask(t)
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			t.Code = newCode("T")
			continue
		}
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.st.Audit(u.ID, "task.create", "task", id, map[string]any{"reward": fmtE8(reward), "slots": slots, "kind": f.TType}, a.ip(r))
	a.flash(w, "任务已发布")
	http.Redirect(w, r, t.Path(), http.StatusFound)
}

// ---- 任务页 ----

type taskPage struct {
	Base
	T          *Task
	Owner      *User
	OwnerStats PubStats
	OwnerTier  Tier
	Mine       *Submission // 我在这个任务上的进行中记录
	Block      string      // 不能接单的原因
	IsOwner    bool
	Subs       []*Submission
	Workers    map[int64]*User
	Full       bool
	Done       bool
	CanEdit    bool
	Public     []PublicRecord
	Pending    []*Submission // 需要发布方处理：待付款 / 逾期 / 待确认（含补差）——不能叫 Todo，会遮住 Base.Todo
	Active     []*Submission // 进行中：已接单 / 验证中 / 留存中 / 申诉中
	DoneList   []*Submission
	Dead       []*Submission // 过期 / 作废 / 违约
	PayDueN    int64         // 待付款 + 逾期（要掏钱的）
	ActiveN    int64
	DeadN      int64
}

func (a *App) handleTask(w http.ResponseWriter, r *http.Request) {
	t, err := a.st.GetTaskByCode(r.PathValue("code"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if t == nil {
		a.errorPage(w, r, http.StatusNotFound, "任务不存在", "")
		return
	}
	p := taskPage{Base: a.base(w, r), T: t, Workers: map[int64]*User{}}
	p.Owner, _ = a.st.GetUserByID(t.OwnerID)
	p.OwnerStats = a.st.PubStats(t.OwnerID)
	p.OwnerTier = a.pubTier(p.OwnerStats)
	p.Full = t.Left() <= 0
	p.Done = t.SlotsTotal <= t.DoneCount
	p.Public, _ = a.st.TaskPublicRecords(t.ID)
	if p.Me != nil {
		p.IsOwner = p.Me.ID == t.OwnerID || p.IsAdmin
		if subs, _ := a.st.querySubs(`WHERE task_id=? AND worker_id=? ORDER BY id DESC LIMIT 1`, t.ID, p.Me.ID); len(subs) > 0 {
			p.Mine = subs[0]
		}
		if !p.IsOwner {
			p.Block = a.canClaim(p.Me, t, a.st.WorkerStats(p.Me.ID), false)
		}
		if p.IsOwner {
			p.Subs, _ = a.st.SubsByTask(t.ID)
			for _, x := range p.Subs {
				if _, ok := p.Workers[x.WorkerID]; !ok {
					if u, _ := a.st.GetUserByID(x.WorkerID); u != nil {
						p.Workers[x.WorkerID] = u
					}
				}
				switch x.Status {
				case SPayable, SOverdue:
					p.Pending = append(p.Pending, x)
					p.PayDueN++
				case SAwait, SChecking:
					p.Pending = append(p.Pending, x)
				case SClaimed, SSubmit, SVerified, SDisputed:
					p.Active = append(p.Active, x)
					p.ActiveN++
				case SPaid:
					p.DoneList = append(p.DoneList, x)
				default:
					p.Dead = append(p.Dead, x)
					p.DeadN++
				}
			}
			p.CanEdit = a.st.count(`SELECT COUNT(*) FROM submissions WHERE task_id=?`, t.ID) == 0 && t.Status != "closed"
		}
	} else {
		p.Block = "登录后接单"
	}
	a.render(w, http.StatusOK, "task", p)
}

func (a *App) handleClaim(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.limited(w, r, "claim", 20, 10*time.Minute) {
		return
	}
	t, err := a.st.GetTaskByCode(r.PathValue("code"))
	if err != nil || t == nil {
		a.errorPage(w, r, http.StatusNotFound, "任务不存在", "")
		return
	}
	if m := a.canClaim(u, t, a.st.WorkerStats(u.ID), true); m != "" {
		a.flash(w, m)
		http.Redirect(w, r, t.Path(), http.StatusFound)
		return
	}
	variant := int64(0)
	if n := int64(len(t.Contents)); n > 0 {
		variant = t.Used % n
	}
	var x *Submission
	for i := 0; i < 3; i++ {
		x, err = a.st.CreateClaim(t, u.ID, newCode("S")+randCode(3), variant, t.ClaimTTLMin)
		if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			break
		}
	}
	if err != nil {
		a.flash(w, err.Error())
		http.Redirect(w, r, t.Path(), http.StatusFound)
		return
	}
	if a.st.SelfDealing(t.OwnerID, u.ID) {
		a.st.db.Exec(`UPDATE submissions SET self_deal=1 WHERE id=?`, x.ID)
	}
	a.st.Audit(u.ID, "sub.claim", "submission", x.ID, map[string]any{"task": t.Code}, a.ip(r))
	a.notify(t.OwnerID, "task", "有人接单：@"+u.Handle+" 接了《"+t.Title+"》", fmt.Sprintf("对方需在 %s 内%s；本任务还剩 %d 个名额。", durMin(t.ClaimTTLMin), claimTodo(t), t.Left()-1), t.Path()+"#manage")
	http.Redirect(w, r, x.Path(), http.StatusFound)
}

// handleTaskAction 发布方操作：pause / resume / cancel / extend。
func (a *App) handleTaskAction(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	t, err := a.st.GetTaskByCode(r.PathValue("code"))
	if err != nil || t == nil {
		a.errorPage(w, r, http.StatusNotFound, "任务不存在", "")
		return
	}
	if t.OwnerID != u.ID && !a.isAdmin(u) {
		a.errorPage(w, r, http.StatusForbidden, "没有权限", "")
		return
	}
	action := r.PathValue("action")
	switch action {
	case "pause":
		if t.Status == "open" {
			err = a.st.SetTaskStatus(t.ID, "paused", "", false)
		}
	case "resume":
		if t.Status == "paused" {
			if a.st.PublisherFrozen(t.OwnerID) > 0 {
				err = errors.New("有逾期未付记录，付清后才能恢复接单")
			} else if t.DeadlineAt <= ms() {
				err = errors.New("任务已过截止时间，请先延长截止")
			} else {
				err = a.st.SetTaskStatus(t.ID, "open", "", false)
			}
		}
	case "cancel":
		if t.Status != "closed" {
			err = a.st.CancelRemaining(t.ID)
		}
	case "extend":
		days, _ := strconv.ParseInt(r.FormValue("days"), 10, 64)
		if days < 1 || days > a.cfg.DeadlineMax {
			err = errors.New("延长天数不合理")
		} else {
			base := t.DeadlineAt
			if base < ms() {
				base = ms()
			}
			if nd := base + days*dayMs; nd-t.PublishedAt > a.cfg.DeadlineMax*dayMs {
				err = fmt.Errorf("任务总时长不能超过 %d 天", a.cfg.DeadlineMax)
			} else {
				err = a.st.ExtendDeadline(t.ID, nd)
			}
		}
	default:
		a.errorPage(w, r, http.StatusNotFound, "操作不存在", "")
		return
	}
	if err != nil {
		a.flash(w, err.Error())
	} else {
		a.st.Audit(u.ID, "task."+action, "task", t.ID, nil, a.ip(r))
		a.flash(w, "已"+map[string]string{"pause": "暂停接单", "resume": "恢复接单", "cancel": "取消剩余名额并关闭任务", "extend": "延长截止"}[action])
	}
	http.Redirect(w, r, t.Path(), http.StatusFound)
}

// claimTodo 接单后对方要做的事（通知文案）。
func claimTodo(t *Task) string {
	switch t.Kind {
	case "reply":
		return "评论并回填链接"
	case "like", "repost":
		return "完成" + t.DoneVerb() + "并提交核对"
	}
	return "发帖并回填链接"
}
