package main

import (
	"database/sql"
	"fmt"
	"strings"
)

const taskCols = `id,code,owner_id,title,contents,contents_norm,match_mode,reward_e8,currency,slots_total,claim_ttl_min,retention_h,pay_window_h,deadline_at,min_account_days,min_followers,ad_tag,status,close_reason,paused_by_freeze,published_at,created_at,updated_at`

func scanTask(r scanner) (*Task, error) {
	var t Task
	var contents, norm string
	var ad, pbf int64
	err := r.Scan(&t.ID, &t.Code, &t.OwnerID, &t.Title, &contents, &norm, &t.MatchMode, &t.RewardE8, &t.Currency, &t.SlotsTotal, &t.ClaimTTLMin, &t.RetentionH, &t.PayWindowH, &t.DeadlineAt, &t.MinAccountDays, &t.MinFollowers, &ad, &t.Status, &t.CloseReason, &pbf, &t.PublishedAt, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Contents, t.ContentsNorm, t.AdTag, t.PausedByFreeze = parseJSONList(contents), parseJSONList(norm), ad == 1, pbf == 1
	return &t, nil
}

func (s *Store) CreateTask(t *Task) (int64, error) {
	now := ms()
	res, err := s.db.Exec(`INSERT INTO tasks(code,owner_id,title,contents,contents_norm,match_mode,reward_e8,currency,slots_total,claim_ttl_min,retention_h,pay_window_h,deadline_at,min_account_days,min_followers,ad_tag,status,published_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'open',?,?,?)`,
		t.Code, t.OwnerID, t.Title, jsonList(t.Contents), jsonList(t.ContentsNorm), t.MatchMode, t.RewardE8, t.Currency, t.SlotsTotal, t.ClaimTTLMin, t.RetentionH, t.PayWindowH, t.DeadlineAt, t.MinAccountDays, t.MinFollowers, b2i(t.AdTag), now, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (s *Store) GetTaskByCode(code string) (*Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE code=?`, strings.ToUpper(code)))
	if err != nil || t == nil {
		return t, err
	}
	s.fillTaskUsage(t)
	return t, nil
}

func (s *Store) GetTaskByID(id int64) (*Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
	if err != nil || t == nil {
		return t, err
	}
	s.fillTaskUsage(t)
	return t, nil
}

func (s *Store) fillTaskUsage(t *Task) {
	t.Used = s.count(`SELECT COUNT(*) FROM submissions WHERE task_id=? AND status NOT IN ('expired','void')`, t.ID)
	t.DoneCount = s.count(`SELECT COUNT(*) FROM submissions WHERE task_id=? AND status='paid'`, t.ID)
}

func (s *Store) queryTasks(where string, args ...any) ([]*Task, error) {
	rows, err := s.db.Query(`SELECT `+taskCols+` FROM tasks `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range out {
		s.fillTaskUsage(t)
	}
	return out, nil
}

// TaskFilter 任务大厅筛选。
type TaskFilter struct {
	MinRewardE8 int64
	MaxRetentH  int64  // -1 = 不限
	Sort        string // new | reward
}

func (s *Store) ListOpenTasks(f TaskFilter, limit, offset int) ([]*Task, int64, error) {
	where := `WHERE status='open' AND deadline_at>? AND slots_total>(SELECT COUNT(*) FROM submissions x WHERE x.task_id=tasks.id AND x.status NOT IN ('expired','void'))`
	args := []any{ms()}
	if f.MinRewardE8 > 0 {
		where += ` AND reward_e8>=?`
		args = append(args, f.MinRewardE8)
	}
	if f.MaxRetentH >= 0 {
		where += ` AND retention_h<=?`
		args = append(args, f.MaxRetentH)
	}
	total := s.count(`SELECT COUNT(*) FROM tasks `+where, args...)
	order := ` ORDER BY published_at DESC`
	if f.Sort == "reward" {
		order = ` ORDER BY reward_e8 DESC, published_at DESC`
	}
	ts, err := s.queryTasks(where+order+fmt.Sprintf(` LIMIT %d OFFSET %d`, limit, offset), args...)
	if err != nil {
		return nil, 0, err
	}
	return ts, total, nil
}

func (s *Store) TasksByOwner(ownerID int64) ([]*Task, error) {
	return s.queryTasks(`WHERE owner_id=? ORDER BY id DESC`, ownerID)
}

func (s *Store) OpenTaskCount(ownerID int64) int64 {
	return s.count(`SELECT COUNT(*) FROM tasks WHERE owner_id=? AND status IN ('open','paused')`, ownerID)
}

// Exposure 发布方敞口：未关闭任务的（总名额 − 已完成）× 单价 + 已关闭任务里还没付清的记录 × 单价。
func (s *Store) Exposure(ownerID int64) int64 {
	stale := ms() - 7*dayMs
	open := s.sum(`SELECT SUM(t.reward_e8 * (t.slots_total - (SELECT COUNT(*) FROM submissions x WHERE x.task_id=t.id AND (x.status='paid' OR (x.status='awaiting_confirm' AND x.marked_paid_at>0 AND x.marked_paid_at<?))))) FROM tasks t WHERE t.owner_id=? AND t.status IN ('open','paused')`, stale, ownerID)
	closed := s.sum(`SELECT SUM(t.reward_e8) FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE t.owner_id=? AND t.status='closed' AND (x.status IN ('claimed','submitted','verified','payable','overdue','disputed') OR (x.status='awaiting_confirm' AND NOT (x.marked_paid_at>0 AND x.marked_paid_at<?)))`, ownerID, stale)
	return open + closed
}

func (s *Store) SetTaskStatus(id int64, status, reason string, byFreeze bool) error {
	_, err := s.db.Exec(`UPDATE tasks SET status=?, close_reason=?, paused_by_freeze=?, updated_at=? WHERE id=?`, status, reason, b2i(byFreeze), ms(), id)
	return err
}

// CancelRemaining 取消剩余名额：把总名额缩到已占用数，任务关闭。
func (s *Store) CancelRemaining(id int64) error {
	_, err := s.db.Exec(`UPDATE tasks SET slots_total=(SELECT COUNT(*) FROM submissions WHERE task_id=tasks.id AND status NOT IN ('expired','void')), status='closed', close_reason='cancelled', updated_at=? WHERE id=?`, ms(), id)
	return err
}

func (s *Store) ExtendDeadline(id, deadline int64) error {
	_, err := s.db.Exec(`UPDATE tasks SET deadline_at=?, updated_at=? WHERE id=? AND status<>'closed'`, deadline, ms(), id)
	return err
}

// UpdateTaskContent 只在还没人接单时允许改文案与标题。
func (s *Store) UpdateTaskContent(id int64, title string, contents, norm []string, mode string) (bool, error) {
	if s.count(`SELECT COUNT(*) FROM submissions WHERE task_id=?`, id) > 0 {
		return false, nil
	}
	_, err := s.db.Exec(`UPDATE tasks SET title=?, contents=?, contents_norm=?, match_mode=?, updated_at=? WHERE id=?`, title, jsonList(contents), jsonList(norm), mode, ms(), id)
	return err == nil, err
}

// FreezeOwnerTasks 发布方逾期：在线任务自动暂停。返回受影响数。
func (s *Store) FreezeOwnerTasks(ownerID int64) int64 {
	res, err := s.db.Exec(`UPDATE tasks SET status='paused', paused_by_freeze=1, updated_at=? WHERE owner_id=? AND status='open'`, ms(), ownerID)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

// UnfreezeOwnerTasks 付清后恢复因冻结暂停的任务。
func (s *Store) UnfreezeOwnerTasks(ownerID int64) {
	s.db.Exec(`UPDATE tasks SET status='open', paused_by_freeze=0, updated_at=? WHERE owner_id=? AND status='paused' AND paused_by_freeze=1 AND deadline_at>?`, ms(), ownerID, ms())
}

// CloseOwnerTasks 上黑名单：全部关闭。
func (s *Store) CloseOwnerTasks(ownerID int64, reason string) {
	s.db.Exec(`UPDATE tasks SET status='closed', close_reason=?, updated_at=? WHERE owner_id=? AND status<>'closed'`, reason, ms(), ownerID)
}

// TasksToClose 到期或名额全部完成的任务。
func (s *Store) TasksToClose() ([]*Task, error) {
	return s.queryTasks(`WHERE status IN ('open','paused') AND deadline_at<=?`, ms())
}
