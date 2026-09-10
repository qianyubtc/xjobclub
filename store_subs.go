package main

import (
	"database/sql"
	"fmt"
	"strings"
)

const subCols = `id,code,task_id,worker_id,variant_idx,status,prev_status,claimed_at,claim_expires_at,tweet_id,tweet_url,tweet_text,tweet_created_at,verify_attempts,verify_retries,last_error,next_verify_at,verified_at,recheck_due_at,recheck_flag,recheck_tries,payable_at,pay_deadline_at,overdue_at,reported_at,grace_until,marked_paid_at,marked_order_id,marked_note,underpaid_e8,topup_requested_at,topup_marked_at,topup_order_id,confirmed_at,confirm_method,paid_amount_e8,late,void_reason,defaulted_at,self_deal,unreadable,created_at,updated_at`

func scanSub(r scanner) (*Submission, error) {
	var x Submission
	var late int64
	err := r.Scan(&x.ID, &x.Code, &x.TaskID, &x.WorkerID, &x.VariantIdx, &x.Status, &x.PrevStatus, &x.ClaimedAt, &x.ClaimExpiresAt, &x.TweetID, &x.TweetURL, &x.TweetText, &x.TweetCreatedAt, &x.VerifyAttempts, &x.VerifyRetries, &x.LastError, &x.NextVerifyAt, &x.VerifiedAt, &x.RecheckDueAt, &x.RecheckFlag, &x.RecheckTries, &x.PayableAt, &x.PayDeadlineAt, &x.OverdueAt, &x.ReportedAt, &x.GraceUntil, &x.MarkedPaidAt, &x.MarkedOrderID, &x.MarkedNote, &x.UnderpaidE8, &x.TopupRequested, &x.TopupMarkedAt, &x.TopupOrderID, &x.ConfirmedAt, &x.ConfirmMethod, &x.PaidAmountE8, &late, &x.VoidReason, &x.DefaultedAt, &x.SelfDeal, &x.Unreadable, &x.CreatedAt, &x.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	x.Late = late == 1
	return &x, nil
}

func (s *Store) subBy(where string, args ...any) (*Submission, error) {
	return scanSub(s.db.QueryRow(`SELECT `+subCols+` FROM submissions WHERE `+where, args...))
}

func (s *Store) GetSubByCode(code string) (*Submission, error) {
	return s.subBy(`code=?`, strings.ToUpper(strings.TrimSpace(code)))
}
func (s *Store) GetSubByID(id int64) (*Submission, error) { return s.subBy(`id=?`, id) }

func (s *Store) querySubs(where string, args ...any) ([]*Submission, error) {
	rows, err := s.db.Query(`SELECT `+subCols+` FROM submissions `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Submission
	for rows.Next() {
		x, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// CreateClaim 接单：在事务里复核名额与重复接单后插入。
func (s *Store) CreateClaim(t *Task, workerID int64, code string, variant int64, ttlMin int64) (*Submission, error) {
	now := ms()
	var id int64
	err := s.tx(func(tx *sql.Tx) error {
		var used, slots int64
		var status string
		if err := tx.QueryRow(`SELECT slots_total,status FROM tasks WHERE id=?`, t.ID).Scan(&slots, &status); err != nil {
			return err
		}
		if status != "open" {
			return ErrState
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM submissions WHERE task_id=? AND status NOT IN ('expired','void')`, t.ID).Scan(&used); err != nil {
			return err
		}
		if used >= slots {
			return fmt.Errorf("名额已满")
		}
		var dup int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM submissions WHERE task_id=? AND worker_id=? AND status NOT IN ('expired','void')`, t.ID, workerID).Scan(&dup); err != nil {
			return err
		}
		if dup > 0 {
			return fmt.Errorf("你已经接过这个任务")
		}
		var recent int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM submissions WHERE task_id=? AND worker_id=? AND status IN ('expired','void') AND updated_at>?`, t.ID, workerID, now-dayMs).Scan(&recent); err != nil {
			return err
		}
		if recent > 0 {
			return fmt.Errorf("你在这个任务上有 24 小时内过期/作废的记录，明天再来")
		}
		res, err := tx.Exec(`INSERT INTO submissions(code,task_id,worker_id,variant_idx,status,claimed_at,claim_expires_at,created_at,updated_at) VALUES(?,?,?,?,'claimed',?,?,?,?)`,
			code, t.ID, workerID, variant, now, now+ttlMin*60*1000, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetSubByID(id)
}

// transition 条件状态转移：只有当前状态在 from 里才更新，返回是否命中。
func (s *Store) transition(id int64, from []string, to string, set string, args ...any) (bool, error) {
	q := `UPDATE submissions SET status=?, updated_at=?`
	all := []any{to, ms()}
	if set != "" {
		q += `, ` + set
		all = append(all, args...)
	}
	q += ` WHERE id=? AND status IN (` + placeholders(len(from)) + `)`
	all = append(all, id)
	for _, f := range from {
		all = append(all, f)
	}
	res, err := s.db.Exec(q, all...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return "''"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func (s *Store) SetSubmitted(id int64, tweetID, url string) (bool, error) {
	return s.transition(id, []string{SClaimed, SSubmit}, SSubmit, `tweet_id=?, tweet_url=?, verify_attempts=verify_attempts+1, verify_retries=0, last_error='', next_verify_at=0`, tweetID, url)
}

// SetVerifyFailed 验证不通过：回到 claimed，清掉推文 ID（让这条推文能换别的记录用……不能，推文只能给本人用；但清掉便于重提）。
func (s *Store) SetVerifyFailed(id int64, reason string) (bool, error) {
	return s.transition(id, []string{SSubmit}, SClaimed, `last_error=?, tweet_id='', next_verify_at=0`, reason)
}

func (s *Store) SetVerifyRetry(id, nextAt int64, reason string) (bool, error) {
	return s.transition(id, []string{SSubmit}, SSubmit, `last_error=?, next_verify_at=?, verify_retries=verify_retries+1, claim_expires_at=MAX(claim_expires_at, ?+15*60*1000)`, reason, nextAt, nextAt)
}

// SetVerified 验证通过：留存 0 直接待付款，否则排复检。
func (s *Store) SetVerified(id int64, text string, tweetCreated, recheckDue, payDeadline int64) (bool, error) {
	now := ms()
	if recheckDue == 0 {
		return s.transition(id, []string{SSubmit}, SPayable, `tweet_text=?, tweet_created_at=?, verified_at=?, last_error='', payable_at=?, pay_deadline_at=?`, text, tweetCreated, now, now, payDeadline)
	}
	return s.transition(id, []string{SSubmit}, SVerified, `tweet_text=?, tweet_created_at=?, verified_at=?, last_error='', recheck_due_at=?`, text, tweetCreated, now, recheckDue)
}

func (s *Store) SetPayable(id, payDeadline int64, flag string) (bool, error) {
	now := ms()
	return s.transition(id, []string{SVerified}, SPayable, `payable_at=?, pay_deadline_at=?, recheck_flag=?`, now, payDeadline, flag)
}

func (s *Store) SetRecheckRetry(id int64, nextDue int64, flag string) (bool, error) {
	return s.transition(id, []string{SVerified}, SVerified, `recheck_due_at=?, recheck_tries=recheck_tries+1, recheck_flag=?`, nextDue, flag)
}

// SetRecheckUnreadable 复检读不到推文：记一次，1 小时后再看（连续 3 次才作废，调用方判断）。
func (s *Store) SetRecheckUnreadable(id int64, nextDue int64, flag string) (bool, error) {
	return s.transition(id, []string{SVerified}, SVerified, `recheck_due_at=?, recheck_flag=?`, nextDue, flag)
}

func (s *Store) SetVoid(id int64, from []string, reason string) (bool, error) {
	return s.transition(id, from, SVoid, `void_reason=?`, reason)
}

func (s *Store) SetExpired(id int64) (bool, error) {
	return s.transition(id, []string{SClaimed, SSubmit}, SExpired, "")
}

func (s *Store) SetOverdue(id int64) (bool, error) {
	return s.transition(id, []string{SPayable}, SOverdue, `overdue_at=?`, ms())
}

// SetMarkedPaid 手动登记已付。允许：待付款 / 逾期 / 因 A 类举报进入 disputed 的逾期记录 / 少付待补差（登记补差订单号）。订单号全局唯一。
func (s *Store) SetMarkedPaid(id int64, orderID, note string) (bool, error) {
	if orderID != "" && s.count(`SELECT COUNT(*) FROM submissions WHERE marked_order_id=? AND id<>?`, orderID, id) > 0 {
		return false, fmt.Errorf("这个币安订单编号已经用在别的记录上了")
	}
	if orderID != "" && s.count(`SELECT COUNT(*) FROM submissions WHERE topup_order_id=? AND id<>?`, orderID, id) > 0 {
		return false, fmt.Errorf("这个币安订单编号已经用在别的记录上了")
	}
	now := ms()
	// 少付后的补差登记：只写 topup_* 列，不动原标记（原订单号是证据）
	res, err := s.db.Exec(`UPDATE submissions SET topup_order_id=?, topup_marked_at=?, topup_requested_at=0, updated_at=?
		WHERE id=? AND status='awaiting_confirm' AND underpaid_e8>0 AND topup_marked_at=0 AND marked_order_id<>?`, orderID, now, now, id, orderID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	res, err = s.db.Exec(`UPDATE submissions SET status='awaiting_confirm', marked_paid_at=?, marked_order_id=?, marked_note=?, updated_at=?,
		late=CASE WHEN status='overdue' OR prev_status='overdue' OR overdue_at>0 THEN 1 ELSE late END
		WHERE id=? AND (status IN ('payable','overdue') OR (status='disputed' AND prev_status='overdue'))`,
		now, orderID, note, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetTopupRequested 接单方要求补差：期间不锁定接单方。
func (s *Store) SetTopupRequested(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE submissions SET topup_requested_at=?, updated_at=? WHERE id=? AND status='awaiting_confirm' AND underpaid_e8>0 AND topup_requested_at=0`, ms(), ms(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) SetUnderpaid(id, actualE8 int64) (bool, error) {
	return s.transition(id, []string{SPayable, SOverdue}, SAwait, `underpaid_e8=?, marked_paid_at=?, late=CASE WHEN status='overdue' THEN 1 ELSE late END`, actualE8, ms())
}

// SetPaid 完成。late 由是否曾逾期决定（overdue_at>0）。
func (s *Store) SetPaid(id int64, method string, amountE8 int64) (bool, error) {
	return s.transition(id, []string{SPayable, SOverdue, SAwait, SDisputed}, SPaid, `confirmed_at=?, confirm_method=?, paid_amount_e8=?, late=CASE WHEN overdue_at>0 THEN 1 ELSE late END`, ms(), method, amountE8)
}

func (s *Store) SetDisputed(id int64, from []string) (bool, error) {
	// prev_status 用子查询保存当前状态
	q := `UPDATE submissions SET prev_status=status, status='disputed', updated_at=? WHERE id=? AND status IN (` + placeholders(len(from)) + `)`
	args := []any{ms(), id}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) RestoreFromDispute(id int64, to string) (bool, error) {
	set := ``
	if to == SOverdue {
		set = `overdue_at=CASE WHEN overdue_at=0 THEN ? ELSE overdue_at END, marked_paid_at=0, marked_order_id='', marked_note=''`
		return s.transition(id, []string{SDisputed}, to, set, ms())
	}
	return s.transition(id, []string{SDisputed}, to, "")
}

func (s *Store) SetDefaulted(id int64) (bool, error) {
	return s.transition(id, []string{SOverdue, SDisputed}, SDefault, `defaulted_at=?`, ms())
}

// SetRepaid 违约后补付：状态不变，记录金额与时间。
func (s *Store) SetRepaid(id int64, method string, amountE8 int64) error {
	_, err := s.db.Exec(`UPDATE submissions SET confirmed_at=?, confirm_method=?, paid_amount_e8=?, late=1, updated_at=? WHERE id=? AND status='defaulted'`, ms(), method, amountE8, ms(), id)
	return err
}

func (s *Store) SetReported(id int64) error {
	_, err := s.db.Exec(`UPDATE submissions SET reported_at=CASE WHEN reported_at=0 THEN ? ELSE reported_at END, updated_at=? WHERE id=?`, ms(), ms(), id)
	return err
}

// ---- 列表 ----

func (s *Store) SubsByTask(taskID int64) ([]*Submission, error) {
	return s.querySubs(`WHERE task_id=? ORDER BY id DESC`, taskID)
}

func (s *Store) SubsByWorker(workerID int64, limit int) ([]*Submission, error) {
	return s.querySubs(`WHERE worker_id=? ORDER BY id DESC LIMIT ?`, workerID, limit)
}

// SubsByOwnerStatus 发布方名下指定状态的记录。
func (s *Store) SubsByOwnerStatus(ownerID int64, statuses ...string) ([]*Submission, error) {
	args := []any{ownerID}
	for _, st := range statuses {
		args = append(args, st)
	}
	return s.querySubs(`WHERE task_id IN (SELECT id FROM tasks WHERE owner_id=?) AND status IN (`+placeholders(len(statuses))+`) ORDER BY pay_deadline_at, id`, args...)
}

func (s *Store) SubsByWorkerStatus(workerID int64, statuses ...string) ([]*Submission, error) {
	args := []any{workerID}
	for _, st := range statuses {
		args = append(args, st)
	}
	return s.querySubs(`WHERE worker_id=? AND status IN (`+placeholders(len(statuses))+`) ORDER BY id DESC`, args...)
}

// ---- 定时任务用 ----

func (s *Store) ExpiredClaims(now int64) ([]*Submission, error) {
	return s.querySubs(`WHERE status IN ('claimed','submitted') AND claim_expires_at<=? AND NOT (status='submitted' AND next_verify_at>0) ORDER BY id LIMIT 200`, now)
}

func (s *Store) VerifyRetries(now int64) ([]*Submission, error) {
	return s.querySubs(`WHERE status='submitted' AND next_verify_at>0 AND next_verify_at<=? ORDER BY next_verify_at LIMIT 50`, now)
}

func (s *Store) DueRechecks(now int64) ([]*Submission, error) {
	return s.querySubs(`WHERE status='verified' AND recheck_due_at>0 AND recheck_due_at<=? ORDER BY recheck_due_at LIMIT 50`, now)
}

func (s *Store) DuePayables(now int64) ([]*Submission, error) {
	return s.querySubs(`WHERE status='payable' AND pay_deadline_at>0 AND pay_deadline_at<=? AND NOT EXISTS (SELECT 1 FROM disputes d WHERE d.submission_id=submissions.id AND d.type='D' AND d.status<>'resolved') ORDER BY pay_deadline_at LIMIT 200`, now)
}

func (s *Store) AwaitingSince(before int64) ([]*Submission, error) {
	return s.querySubs(`WHERE status='awaiting_confirm' AND marked_paid_at<=? ORDER BY marked_paid_at LIMIT 200`, before)
}

// ---- 锁定 / 冻结 / 统计 ----

func (s *Store) WorkerLocked(userID int64) int64 {
	return s.count(`SELECT COUNT(*) FROM submissions WHERE worker_id=? AND status='awaiting_confirm'`, userID)
}

// PublisherFrozen 冻结条件：存在 overdue；或曾逾期（overdue_at>0）且未完成（awaiting_confirm / disputed）；或有 B 类申诉在审。
func (s *Store) PublisherFrozen(userID int64) int64 {
	return s.count(`SELECT COUNT(*) FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE t.owner_id=? AND (x.status='overdue'
		OR (x.overdue_at>0 AND x.status IN ('awaiting_confirm','disputed'))
		OR (x.status='disputed' AND EXISTS (SELECT 1 FROM disputes d WHERE d.submission_id=x.id AND d.type='B' AND d.status<>'resolved')))`, userID)
}

func (s *Store) ActiveClaims(workerID int64) int64 {
	return s.count(`SELECT COUNT(*) FROM submissions WHERE worker_id=? AND status IN ('claimed','submitted')`, workerID)
}

func (s *Store) ClaimsSince(workerID, since int64) int64 {
	return s.count(`SELECT COUNT(*) FROM submissions WHERE worker_id=? AND claimed_at>=?`, workerID, since)
}

func (s *Store) VoidCount(workerID, since int64) int64 {
	return s.count(`SELECT COUNT(*) FROM submissions WHERE worker_id=? AND status='void' AND void_reason LIKE '留存%' AND updated_at>=?`, workerID, since)
}

func (s *Store) PubStats(userID int64) PubStats {
	var st PubStats
	st.Paid = s.count(`SELECT COUNT(*) FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE t.owner_id=? AND x.status='paid'`, userID)
	st.PaidGateway = s.count(`SELECT COUNT(*) FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE t.owner_id=? AND x.status='paid' AND x.confirm_method='gateway' AND x.self_deal=0`, userID)
	st.Overdue = s.count(`SELECT COUNT(*) FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE t.owner_id=? AND x.overdue_at>0`, userID)
	st.Defaulted = s.count(`SELECT COUNT(*) FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE t.owner_id=? AND x.status='defaulted'`, userID)
	st.AvgPayMs = s.sum(`SELECT CAST(AVG(CASE WHEN x.marked_paid_at>0 THEN x.marked_paid_at ELSE x.confirmed_at END - x.payable_at) AS INTEGER) FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE t.owner_id=? AND x.status='paid' AND x.payable_at>0 AND x.confirmed_at>x.payable_at`, userID)
	st.OpenTasks = s.OpenTaskCount(userID)
	st.ExposureE8 = s.Exposure(userID)
	st.Frozen = s.PublisherFrozen(userID) > 0
	return st
}

func (s *Store) WorkerStats(userID int64) WorkerStats {
	var st WorkerStats
	st.Done = s.count(`SELECT COUNT(*) FROM submissions WHERE worker_id=? AND status='paid'`, userID)
	st.DoneGateway = s.count(`SELECT COUNT(*) FROM submissions WHERE worker_id=? AND status='paid' AND confirm_method='gateway' AND self_deal=0`, userID)
	st.Void30d = s.VoidCount(userID, ms()-30*dayMs)
	st.Active = s.ActiveClaims(userID)
	st.Today = s.ClaimsSince(userID, dayStartMs())
	st.EarnedE8 = s.sum(`SELECT SUM(paid_amount_e8) FROM submissions WHERE worker_id=? AND status='paid'`, userID)
	st.AwaitCount = s.WorkerLocked(userID)
	st.Locked = st.AwaitCount > 0
	return st
}

// SelfDealing 发布方与接单方是否疑似同一人（同 IP 段 / 同付款账户）。
func (s *Store) SelfDealing(ownerID, workerID int64) bool {
	if ownerID == workerID {
		return true
	}
	var a, b string
	s.db.QueryRow(`SELECT payer_id FROM users WHERE id=?`, ownerID).Scan(&a)
	s.db.QueryRow(`SELECT payer_id FROM users WHERE id=?`, workerID).Scan(&b)
	if a != "" && a == b {
		return true
	}
	return s.SharedIP(ownerID, workerID)
}

// PublicRecord 公开成交记录（不含任何付款细节）。
type PublicRecord struct {
	Code        string
	Status      string
	TaskCode    string
	TaskTitle   string
	RewardE8    int64
	Worker      string
	WorkerXID   string
	Owner       string
	OwnerXID    string
	TweetID     string
	At          int64 // 最近一次状态变化
	ConfirmedAt int64
}

var publicStatuses = map[string][]string{
	"":        {SClaimed, SSubmit, SVerified, SPayable, SAwait, SOverdue, SDisputed, SPaid, SDefault},
	"done":    {SPaid},
	"active":  {SClaimed, SSubmit, SVerified, SPayable, SAwait, SDisputed},
	"overdue": {SOverdue, SDefault},
}

// PublicRecords 公开成交记录列表；tab 为 "" / done / active / overdue。
func (s *Store) PublicRecords(tab string, limit, offset int) ([]PublicRecord, int64, error) {
	sts, ok := publicStatuses[tab]
	if !ok {
		sts = publicStatuses[""]
	}
	args := []any{}
	for _, st := range sts {
		args = append(args, st)
	}
	where := `WHERE x.status IN (` + placeholders(len(sts)) + `)`
	total := s.count(`SELECT COUNT(*) FROM submissions x `+where, args...)
	q := `SELECT x.code,x.status,t.code,t.title,t.reward_e8,w.handle,w.x_id,o.handle,o.x_id,x.tweet_id,x.updated_at,x.confirmed_at
		FROM submissions x JOIN tasks t ON t.id=x.task_id JOIN users w ON w.id=x.worker_id JOIN users o ON o.id=t.owner_id ` + where +
		fmt.Sprintf(` ORDER BY x.updated_at DESC LIMIT %d OFFSET %d`, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []PublicRecord
	for rows.Next() {
		var r PublicRecord
		if err := rows.Scan(&r.Code, &r.Status, &r.TaskCode, &r.TaskTitle, &r.RewardE8, &r.Worker, &r.WorkerXID, &r.Owner, &r.OwnerXID, &r.TweetID, &r.At, &r.ConfirmedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// TaskPublicRecords 某任务的公开接单动态（不含过期/作废）。
func (s *Store) TaskPublicRecords(taskID int64) ([]PublicRecord, error) {
	rows, err := s.db.Query(`SELECT x.code,x.status,t.code,t.title,t.reward_e8,w.handle,w.x_id,o.handle,o.x_id,x.tweet_id,x.updated_at,x.confirmed_at
		FROM submissions x JOIN tasks t ON t.id=x.task_id JOIN users w ON w.id=x.worker_id JOIN users o ON o.id=t.owner_id
		WHERE x.task_id=? AND x.status NOT IN ('expired','void') ORDER BY x.id DESC LIMIT 100`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PublicRecord
	for rows.Next() {
		var r PublicRecord
		if err := rows.Scan(&r.Code, &r.Status, &r.TaskCode, &r.TaskTitle, &r.RewardE8, &r.Worker, &r.WorkerXID, &r.Owner, &r.OwnerXID, &r.TweetID, &r.At, &r.ConfirmedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserDoneRecords 某用户作为接单方最近完成的单。
func (s *Store) UserDoneRecords(userID int64, limit int) ([]PublicRecord, error) {
	rows, err := s.db.Query(`SELECT x.code,x.status,t.code,t.title,t.reward_e8,w.handle,w.x_id,o.handle,o.x_id,x.tweet_id,x.updated_at,x.confirmed_at
		FROM submissions x JOIN tasks t ON t.id=x.task_id JOIN users w ON w.id=x.worker_id JOIN users o ON o.id=t.owner_id
		WHERE x.worker_id=? AND x.status='paid' ORDER BY x.confirmed_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PublicRecord
	for rows.Next() {
		var r PublicRecord
		if err := rows.Scan(&r.Code, &r.Status, &r.TaskCode, &r.TaskTitle, &r.RewardE8, &r.Worker, &r.WorkerXID, &r.Owner, &r.OwnerXID, &r.TweetID, &r.At, &r.ConfirmedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
