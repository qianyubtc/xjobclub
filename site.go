package main

import (
	"net/http"
	"strconv"
	"strings"
)

type mePage struct {
	Base
	U          *User
	Stats      PubStats
	WStats     WorkerStats
	PubTier    Tier
	WorkerTier Tier
	ToPay      []*Submission // 我要付的
	ToConfirm  []*Submission // 我要确认的
	Active     []*Submission // 我进行中的接单
	MySubs     []*Submission
	MyTasks    []*Task
	Tasks      map[int64]*Task
	Users      map[int64]*User
	Pay        *PayProfile
	BL         *BlacklistEntry
	Disputes   []*Dispute
	Invites    []*JuryCase
	Locked     bool
	CertNeeded bool
	Tab        string
	Cfg        *Config
	TaskStats  map[int64]TaskCounts
	Latest     []Notification
}

// TaskCounts 发布方任务卡上的计数。
type TaskCounts struct{ Done, PayDue, Await, Active int64 }

func (a *App) fillTasksUsers(tasks map[int64]*Task, users map[int64]*User, subs []*Submission) {
	for _, x := range subs {
		if _, ok := tasks[x.TaskID]; !ok {
			if t, _ := a.st.GetTaskByID(x.TaskID); t != nil {
				tasks[x.TaskID] = t
			}
		}
		if t := tasks[x.TaskID]; t != nil {
			if _, ok := users[t.OwnerID]; !ok {
				if u, _ := a.st.GetUserByID(t.OwnerID); u != nil {
					users[t.OwnerID] = u
				}
			}
		}
		if _, ok := users[x.WorkerID]; !ok {
			if u, _ := a.st.GetUserByID(x.WorkerID); u != nil {
				users[x.WorkerID] = u
			}
		}
	}
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	p := mePage{Base: a.base(w, r), U: u, Tasks: map[int64]*Task{}, Users: map[int64]*User{}, Tab: r.URL.Query().Get("tab"), Cfg: a.cfg}
	p.Stats = a.st.PubStats(u.ID)
	p.WStats = a.st.WorkerStats(u.ID)
	p.PubTier = a.pubTier(p.Stats)
	p.WorkerTier = a.workerTier(p.WStats)
	p.ToPay, _ = a.st.SubsByOwnerStatus(u.ID, SPayable, SOverdue)
	p.ToConfirm, _ = a.st.SubsByWorkerStatus(u.ID, SAwait)
	p.Active, _ = a.st.SubsByWorkerStatus(u.ID, SClaimed, SSubmit, SVerified)
	p.MySubs, _ = a.st.SubsByWorker(u.ID, 50)
	p.MyTasks, _ = a.st.TasksByOwner(u.ID)
	p.TaskStats = map[int64]TaskCounts{}
	for _, t := range p.MyTasks {
		var c TaskCounts
		c.Done = t.DoneCount
		c.PayDue = a.st.count(`SELECT COUNT(*) FROM submissions WHERE task_id=? AND status IN ('payable','overdue')`, t.ID)
		c.Await = a.st.count(`SELECT COUNT(*) FROM submissions WHERE task_id=? AND status='awaiting_confirm'`, t.ID)
		c.Active = a.st.count(`SELECT COUNT(*) FROM submissions WHERE task_id=? AND status IN ('claimed','submitted','verified','disputed')`, t.ID)
		p.TaskStats[t.ID] = c
	}
	p.Latest, _ = a.st.Notifications(u.ID, 5)
	a.fillTasksUsers(p.Tasks, p.Users, p.ToPay)
	a.fillTasksUsers(p.Tasks, p.Users, p.ToConfirm)
	a.fillTasksUsers(p.Tasks, p.Users, p.Active)
	a.fillTasksUsers(p.Tasks, p.Users, p.MySubs)
	p.Pay, _ = a.st.GetPayProfile(u.ID)
	p.BL, _ = a.st.ActiveBlacklist(u.ID)
	p.Disputes, _ = a.st.DisputesForUser(u.ID, 20)
	p.Invites, _ = a.st.InvitedCasesFor(u.ID)
	p.Locked = a.workerLockedNow(u.ID)
	p.CertNeeded = a.cfg.CertFeeEnabled && u.CertPaidAt == 0
	a.render(w, http.StatusOK, "me", p)
}

type notifPage struct {
	Base
	Notes []Notification
}

func (a *App) handleNotifications(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	notes, _ := a.st.Notifications(u.ID, 100)
	a.st.MarkAllRead(u.ID)
	p := notifPage{Base: a.base(w, r), Notes: notes}
	p.Unread = 0
	a.render(w, http.StatusOK, "notifications", p)
}

type profilePage struct {
	Base
	U          *User
	Stats      PubStats
	WStats     WorkerStats
	PubTier    Tier
	WorkerTier Tier
	BL         []*BlacklistEntry
	ActiveBL   *BlacklistEntry
	Certified  bool
	Tasks      []*Task
	XCreated   int64
	Done       []PublicRecord
}

func (a *App) handleProfile(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("handle")
	var u *User
	if strings.HasPrefix(raw, "xid:") {
		u, _ = a.st.GetUserByXID(strings.TrimPrefix(raw, "xid:"))
	} else if h := normHandle(raw); h != "" {
		u, _ = a.st.GetUserByHandle(h)
	}
	if u == nil {
		a.errorPage(w, r, http.StatusNotFound, "用户不存在", "")
		return
	}
	p := profilePage{Base: a.base(w, r), U: u, XCreated: u.XCreatedMs}
	p.Stats = a.st.PubStats(u.ID)
	p.WStats = a.st.WorkerStats(u.ID)
	p.PubTier = a.pubTier(p.Stats)
	p.WorkerTier = a.workerTier(p.WStats)
	p.BL, _ = a.st.BlacklistForUser(u.ID)
	p.ActiveBL, _ = a.st.ActiveBlacklist(u.ID)
	p.Certified = u.CertPaidAt > 0
	p.Done, _ = a.st.UserDoneRecords(u.ID, 20)
	all, _ := a.st.TasksByOwner(u.ID)
	for _, t := range all {
		if t.Status == "open" && t.DeadlineAt > ms() {
			p.Tasks = append(p.Tasks, t)
		}
	}
	a.render(w, http.StatusOK, "profile", p)
}

type rulesPage struct {
	Base
	Cfg *Config
}

func (a *App) handleRules(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "rules", rulesPage{Base: a.base(w, r), Cfg: a.cfg})
}

type blacklistPage struct {
	Base
	Rows  []*BlacklistEntry
	Users map[int64]*User
	Total int64
	Page  int
	Pages int
}

func (a *App) handleBlacklist(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const size = 30
	rows, total, err := a.st.ListBlacklist(size, (page-1)*size)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	p := blacklistPage{Base: a.base(w, r), Rows: rows, Total: total, Page: page, Pages: int((total + size - 1) / size), Users: map[int64]*User{}}
	for _, b := range rows {
		if _, ok := p.Users[b.UserID]; !ok {
			if u, _ := a.st.GetUserByID(b.UserID); u != nil {
				p.Users[b.UserID] = u
			}
		}
	}
	a.render(w, http.StatusOK, "blacklist", p)
}

// xUserLink X 主页链接：有数字 ID 用 /i/user/{id}（跟随改名），否则用用户名。
func xUserLink(xid, handle string) string {
	if strings.TrimSpace(xid) != "" {
		return "https://x.com/i/user/" + xid
	}
	return "https://x.com/" + handle
}

type recordsPage struct {
	Base
	Tab    string
	Rows   []PublicRecord
	Total  int64
	Page   int
	Pages  int
	Counts map[string]int64
}

// handleRecords 公开成交记录：谁接了、谁完成了、谁逾期了。不含任何付款细节。
func (a *App) handleRecords(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if _, ok := publicStatuses[tab]; !ok {
		tab = ""
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const size = 30
	rows, total, err := a.st.PublicRecords(tab, size, (page-1)*size)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	p := recordsPage{Base: a.base(w, r), Tab: tab, Rows: rows, Total: total, Page: page, Pages: int((total + size - 1) / size), Counts: map[string]int64{}}
	for _, k := range []string{"", "done", "active", "overdue"} {
		sts := publicStatuses[k]
		args := []any{}
		for _, st := range sts {
			args = append(args, st)
		}
		p.Counts[k] = a.st.count(`SELECT COUNT(*) FROM submissions WHERE status IN (`+placeholders(len(sts))+`)`, args...)
	}
	a.render(w, http.StatusOK, "records", p)
}
