package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	_ "modernc.org/sqlite"
)

var (
	ErrXTaken       = errors.New("这个 X 账号已经注册过了")
	ErrUIDTaken     = errors.New("这个币安 UID 已被其他账号绑定")
	ErrAccountTaken = errors.New("这个 API Key 已被其他账号绑定")
	ErrPayerTaken   = errors.New("这个币安付款账户已被其他账号认证")
	ErrState        = errors.New("当前状态不允许此操作")
	ErrNotFound     = errors.New("不存在")
)

type Store struct{ db *sql.DB }

var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		x_id TEXT NOT NULL UNIQUE,
		handle TEXT NOT NULL,
		handle_lower TEXT NOT NULL UNIQUE,
		display_name TEXT NOT NULL DEFAULT '',
		avatar_url TEXT NOT NULL DEFAULT '',
		pass_hash TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		reg_tweet_id TEXT NOT NULL DEFAULT '',
		x_created_ms INTEGER NOT NULL DEFAULT 0,
		payer_id TEXT NOT NULL DEFAULT '',
		cert_paid_at INTEGER NOT NULL DEFAULT 0,
		suspended_until INTEGER NOT NULL DEFAULT 0,
		jury_score INTEGER NOT NULL DEFAULT 0,
		jury_total INTEGER NOT NULL DEFAULT 0,
		jury_agree INTEGER NOT NULL DEFAULT 0,
		jury_noshow INTEGER NOT NULL DEFAULT 0,
		jury_banned_until INTEGER NOT NULL DEFAULT 0,
		handle_stale INTEGER NOT NULL DEFAULT 0,
		followers INTEGER NOT NULL DEFAULT -1,
		followers_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		last_login_at INTEGER NOT NULL DEFAULT 0)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_payer ON users(payer_id) WHERE payer_id<>''`,
	`CREATE TABLE IF NOT EXISTS xverify (code TEXT PRIMARY KEY, purpose TEXT NOT NULL, secret TEXT NOT NULL, profile TEXT NOT NULL DEFAULT '', used INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS pay_profiles (
		user_id INTEGER PRIMARY KEY,
		binance_uid TEXT NOT NULL,
		receive_email TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT 'manual',
		bpg_account_id TEXT NOT NULL DEFAULT '',
		api_key_masked TEXT NOT NULL DEFAULT '',
		bpg_last_ok INTEGER NOT NULL DEFAULT 0,
		bpg_last_err TEXT NOT NULL DEFAULT '',
		extra_methods TEXT NOT NULL DEFAULT '[]',
		updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_pay_uid ON pay_profiles(binance_uid)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_pay_acct ON pay_profiles(bpg_account_id) WHERE bpg_account_id<>''`,
	`CREATE TABLE IF NOT EXISTS tasks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		owner_id INTEGER NOT NULL,
		title TEXT NOT NULL,
		contents TEXT NOT NULL,
		contents_norm TEXT NOT NULL,
		match_mode TEXT NOT NULL DEFAULT 'exact',
		reward_e8 INTEGER NOT NULL,
		currency TEXT NOT NULL DEFAULT 'USDT',
		slots_total INTEGER NOT NULL,
		claim_ttl_min INTEGER NOT NULL,
		retention_h INTEGER NOT NULL,
		pay_window_h INTEGER NOT NULL,
		deadline_at INTEGER NOT NULL,
		min_account_days INTEGER NOT NULL DEFAULT 0,
		min_followers INTEGER NOT NULL DEFAULT 0,
		kind TEXT NOT NULL DEFAULT 'post',
		target_tweet_id TEXT NOT NULL DEFAULT '',
		target_url TEXT NOT NULL DEFAULT '',
		target_author TEXT NOT NULL DEFAULT '',
		target_text TEXT NOT NULL DEFAULT '',
		min_len INTEGER NOT NULL DEFAULT 0,
		deadline_days INTEGER NOT NULL DEFAULT 0,
		review_started_at INTEGER NOT NULL DEFAULT 0,
		review_deadline_at INTEGER NOT NULL DEFAULT 0,
		review_result TEXT NOT NULL DEFAULT '',
		review_note TEXT NOT NULL DEFAULT '',
		review_hold INTEGER NOT NULL DEFAULT 0,
		price_mode TEXT NOT NULL DEFAULT 'fixed',
		cpm_e8 INTEGER NOT NULL DEFAULT 0,
		floor_e8 INTEGER NOT NULL DEFAULT 0,
		ad_tag INTEGER NOT NULL DEFAULT 1,
		status TEXT NOT NULL DEFAULT 'open',
		close_reason TEXT NOT NULL DEFAULT '',
		paused_by_freeze INTEGER NOT NULL DEFAULT 0,
		published_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_tasks_owner ON tasks(owner_id, status)`,
	`CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, published_at)`,
	`CREATE TABLE IF NOT EXISTS submissions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		task_id INTEGER NOT NULL,
		worker_id INTEGER NOT NULL,
		variant_idx INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		prev_status TEXT NOT NULL DEFAULT '',
		claimed_at INTEGER NOT NULL,
		claim_expires_at INTEGER NOT NULL,
		tweet_id TEXT NOT NULL DEFAULT '',
		tweet_url TEXT NOT NULL DEFAULT '',
		tweet_root TEXT NOT NULL DEFAULT '',
		tweet_text TEXT NOT NULL DEFAULT '',
		tweet_created_at INTEGER NOT NULL DEFAULT 0,
		verify_attempts INTEGER NOT NULL DEFAULT 0,
		verify_retries INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		next_verify_at INTEGER NOT NULL DEFAULT 0,
		verified_at INTEGER NOT NULL DEFAULT 0,
		recheck_due_at INTEGER NOT NULL DEFAULT 0,
		recheck_flag TEXT NOT NULL DEFAULT '',
		recheck_tries INTEGER NOT NULL DEFAULT 0,
		payable_at INTEGER NOT NULL DEFAULT 0,
		pay_deadline_at INTEGER NOT NULL DEFAULT 0,
		overdue_at INTEGER NOT NULL DEFAULT 0,
		reported_at INTEGER NOT NULL DEFAULT 0,
		grace_until INTEGER NOT NULL DEFAULT 0,
		marked_paid_at INTEGER NOT NULL DEFAULT 0,
		marked_order_id TEXT NOT NULL DEFAULT '',
		marked_note TEXT NOT NULL DEFAULT '',
		underpaid_e8 INTEGER NOT NULL DEFAULT 0,
		topup_requested_at INTEGER NOT NULL DEFAULT 0,
		topup_marked_at INTEGER NOT NULL DEFAULT 0,
		topup_order_id TEXT NOT NULL DEFAULT '',
		confirmed_at INTEGER NOT NULL DEFAULT 0,
		confirm_method TEXT NOT NULL DEFAULT '',
		paid_amount_e8 INTEGER NOT NULL DEFAULT 0,
		late INTEGER NOT NULL DEFAULT 0,
		void_reason TEXT NOT NULL DEFAULT '',
		defaulted_at INTEGER NOT NULL DEFAULT 0,
		self_deal INTEGER NOT NULL DEFAULT 0,
		unreadable INTEGER NOT NULL DEFAULT 0,
		checking_at INTEGER NOT NULL DEFAULT 0,
		check_note TEXT NOT NULL DEFAULT '',
		check_rejects INTEGER NOT NULL DEFAULT 0,
		check_auto INTEGER NOT NULL DEFAULT 0,
		amount_e8 INTEGER NOT NULL DEFAULT 0,
		views INTEGER NOT NULL DEFAULT -1,
		views_at INTEGER NOT NULL DEFAULT 0,
		settle_views INTEGER NOT NULL DEFAULT -1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sub_tweet ON submissions(tweet_id) WHERE tweet_id<>''`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sub_marked ON submissions(marked_order_id) WHERE marked_order_id<>''`,
	`CREATE INDEX IF NOT EXISTS idx_sub_worker ON submissions(worker_id, status)`,
	`CREATE INDEX IF NOT EXISTS idx_sub_task ON submissions(task_id, status)`,
	`CREATE INDEX IF NOT EXISTS idx_sub_task_worker ON submissions(task_id, worker_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_sub_status ON submissions(status)`,
	`CREATE TABLE IF NOT EXISTS payments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		submission_id INTEGER NOT NULL DEFAULT 0,
		kind TEXT NOT NULL,
		user_id INTEGER NOT NULL DEFAULT 0,
		bpg_order_id TEXT NOT NULL DEFAULT '',
		merchant_order_id TEXT NOT NULL UNIQUE,
		pay_amount TEXT NOT NULL DEFAULT '',
		base_e8 INTEGER NOT NULL DEFAULT 0,
		note_code TEXT NOT NULL DEFAULT '',
		pay_url TEXT NOT NULL DEFAULT '',
		receive_uid TEXT NOT NULL DEFAULT '',
		receive_link TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending',
		actual_e8 INTEGER NOT NULL DEFAULT 0,
		matched_by TEXT NOT NULL DEFAULT '',
		binance_order_id TEXT NOT NULL DEFAULT '',
		payer_id TEXT NOT NULL DEFAULT '',
		paid_at INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER NOT NULL DEFAULT 0,
		raw_callback TEXT NOT NULL DEFAULT '',
		last_sync_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_pay_sub ON payments(submission_id, status)`,
	`CREATE INDEX IF NOT EXISTS idx_pay_status ON payments(status)`,
	`CREATE TABLE IF NOT EXISTS tweet_cache (id INTEGER PRIMARY KEY AUTOINCREMENT, tweet_id TEXT NOT NULL, purpose TEXT NOT NULL, ok INTEGER NOT NULL, err TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '', fetched_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_tc_tweet ON tweet_cache(tweet_id, fetched_at)`,
	`CREATE TABLE IF NOT EXISTS disputes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		type TEXT NOT NULL,
		submission_id INTEGER NOT NULL DEFAULT 0,
		task_id INTEGER NOT NULL DEFAULT 0,
		opener_id INTEGER NOT NULL,
		against_id INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'open',
		evidence_until INTEGER NOT NULL DEFAULT 0,
		resolution TEXT NOT NULL DEFAULT '',
		resolution_note TEXT NOT NULL DEFAULT '',
		resolved_by INTEGER NOT NULL DEFAULT 0,
		resolved_at INTEGER NOT NULL DEFAULT 0,
		jury_case_id INTEGER NOT NULL DEFAULT 0,
		appeal_until INTEGER NOT NULL DEFAULT 0,
		appeal_by INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_disp_status ON disputes(status, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_disp_sub ON disputes(submission_id)`,
	`CREATE TABLE IF NOT EXISTS dispute_messages (id INTEGER PRIMARY KEY AUTOINCREMENT, dispute_id INTEGER NOT NULL, author_id INTEGER NOT NULL DEFAULT 0, text TEXT NOT NULL, images TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_dm_disp ON dispute_messages(dispute_id, id)`,
	`CREATE TABLE IF NOT EXISTS blacklist (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		x_id TEXT NOT NULL DEFAULT '',
		handle TEXT NOT NULL DEFAULT '',
		binance_uid TEXT NOT NULL DEFAULT '',
		payer_id TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL,
		dispute_id INTEGER NOT NULL DEFAULT 0,
		amount_owed_e8 INTEGER NOT NULL DEFAULT 0,
		repaid_at INTEGER NOT NULL DEFAULT 0,
		lifted_at INTEGER NOT NULL DEFAULT 0,
		lifted_by INTEGER NOT NULL DEFAULT 0,
		note TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_bl_user ON blacklist(user_id, lifted_at)`,
	`CREATE TABLE IF NOT EXISTS notifications (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, kind TEXT NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', link TEXT NOT NULL DEFAULT '', read_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_notif_user ON notifications(user_id, read_at, id)`,
	`CREATE TABLE IF NOT EXISTS audit_log (id INTEGER PRIMARY KEY AUTOINCREMENT, actor_id INTEGER NOT NULL DEFAULT 0, action TEXT NOT NULL, target_type TEXT NOT NULL DEFAULT '', target_id INTEGER NOT NULL DEFAULT 0, meta TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_log(target_type, target_id, id)`,
	`CREATE TABLE IF NOT EXISTS jury_cases (id INTEGER PRIMARY KEY AUTOINCREMENT, dispute_id INTEGER NOT NULL UNIQUE, status TEXT NOT NULL DEFAULT 'voting', round INTEGER NOT NULL DEFAULT 1, deadline_at INTEGER NOT NULL, verdict TEXT NOT NULL DEFAULT '', votes_for INTEGER NOT NULL DEFAULT 0, votes_against INTEGER NOT NULL DEFAULT 0, abstain INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, closed_at INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS jury_invites (case_id INTEGER NOT NULL, user_id INTEGER NOT NULL, round INTEGER NOT NULL, invited_at INTEGER NOT NULL, voted_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(case_id, user_id))`,
	`CREATE INDEX IF NOT EXISTS idx_ji_user ON jury_invites(user_id, voted_at)`,
	`CREATE TABLE IF NOT EXISTS jury_votes (case_id INTEGER NOT NULL, juror_id INTEGER NOT NULL, vote TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, PRIMARY KEY(case_id, juror_id))`,
	`CREATE TABLE IF NOT EXISTS task_votes (task_id INTEGER NOT NULL, user_id INTEGER NOT NULL, vote INTEGER NOT NULL, reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, PRIMARY KEY(task_id,user_id))`,
	`CREATE INDEX IF NOT EXISTS idx_task_votes_user ON task_votes(user_id, created_at)`,
	`CREATE TABLE IF NOT EXISTS ip_log (user_id INTEGER NOT NULL, prefix TEXT NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY(user_id, prefix))`,
}

func openStore(path string) (*Store, error) {
	dsn := path
	if !strings.HasPrefix(path, "file:") && !strings.Contains(path, "?") {
		dsn = "file:" + path + "?_txlock=immediate" // 事务一开始就拿写锁，并发写者排队等 busy_timeout，而不是读后写时报 SQLITE_BUSY_SNAPSHOT
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者；modernc 驱动下多连接易 SQLITE_BUSY
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA synchronous=NORMAL", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	// 老库补列：ALTER 重复执行会报 duplicate column，忽略即可。以后每加一列都要在这里补一条。
	for _, q := range migrations {
		if _, err := db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("迁移失败 %q: %w", q, err)
		}
	}
	return &Store{db: db}, nil
}

// migrations 给旧库补列（幂等）。列的默认值必须与 schema 里一致。
var migrations = []string{
	`ALTER TABLE submissions ADD COLUMN verify_retries INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN topup_requested_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN topup_marked_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN topup_order_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE submissions ADD COLUMN self_deal INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN unreadable INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE users ADD COLUMN jury_noshow INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE users ADD COLUMN jury_banned_until INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE users ADD COLUMN followers INTEGER NOT NULL DEFAULT -1`,
	`ALTER TABLE users ADD COLUMN followers_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE pay_profiles ADD COLUMN extra_methods TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE tasks ADD COLUMN min_followers INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'post'`,
	`ALTER TABLE tasks ADD COLUMN target_tweet_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tasks ADD COLUMN target_url TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tasks ADD COLUMN target_author TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tasks ADD COLUMN target_text TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tasks ADD COLUMN min_len INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN checking_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN check_note TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE submissions ADD COLUMN check_rejects INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN check_auto INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tasks ADD COLUMN deadline_days INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tasks ADD COLUMN review_started_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tasks ADD COLUMN review_deadline_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tasks ADD COLUMN review_result TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tasks ADD COLUMN review_note TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE tasks ADD COLUMN review_hold INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tasks ADD COLUMN price_mode TEXT NOT NULL DEFAULT 'fixed'`,
	`ALTER TABLE tasks ADD COLUMN cpm_e8 INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tasks ADD COLUMN floor_e8 INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN amount_e8 INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN views INTEGER NOT NULL DEFAULT -1`,
	`ALTER TABLE submissions ADD COLUMN views_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE submissions ADD COLUMN settle_views INTEGER NOT NULL DEFAULT -1`,
	`ALTER TABLE submissions ADD COLUMN tweet_root TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_sub_root ON submissions(tweet_root) WHERE tweet_root<>''`,
}

func (s *Store) Close() { s.db.Close() }

func (s *Store) tx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Audit 审计日志：所有状态变更与管理员操作都记一笔。
func (s *Store) Audit(actorID int64, action, targetType string, targetID int64, meta any, ip string) {
	m := ""
	if meta != nil {
		if b, err := json.Marshal(meta); err == nil {
			m = string(b)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO audit_log(actor_id,action,target_type,target_id,meta,ip,created_at) VALUES(?,?,?,?,?,?,?)`, actorID, action, targetType, targetID, m, ip, ms()); err != nil {
		log.Printf("[error] 审计写入失败 %s: %v", action, err)
	}
}

type AuditRow struct {
	ID         int64
	ActorID    int64
	Action     string
	TargetType string
	TargetID   int64
	Meta       string
	IP         string
	CreatedAt  int64
	Actor      *User
}

func (s *Store) AuditFor(targetType string, targetID int64) ([]AuditRow, error) {
	rows, err := s.db.Query(`SELECT id,actor_id,action,target_type,target_id,meta,ip,created_at FROM audit_log WHERE target_type=? AND target_id=? ORDER BY id`, targetType, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var a AuditRow
		if err := rows.Scan(&a.ID, &a.ActorID, &a.Action, &a.TargetType, &a.TargetID, &a.Meta, &a.IP, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- 通知 ----

func (s *Store) Notify(userID int64, kind, title, body, link string) {
	if userID <= 0 {
		return
	}
	if _, err := s.db.Exec(`INSERT INTO notifications(user_id,kind,title,body,link,created_at) VALUES(?,?,?,?,?,?)`, userID, kind, title, body, link, ms()); err != nil {
		log.Printf("[error] 通知写入失败: %v", err)
	}
}

func (s *Store) Notifications(userID int64, limit int) ([]Notification, error) {
	rows, err := s.db.Query(`SELECT id,user_id,kind,title,body,link,read_at,created_at FROM notifications WHERE user_id=? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.Kind, &n.Title, &n.Body, &n.Link, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) UnreadCount(userID int64) int64 {
	var n int64
	s.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND read_at=0`, userID).Scan(&n)
	return n
}

func (s *Store) MarkAllRead(userID int64) error {
	_, err := s.db.Exec(`UPDATE notifications SET read_at=? WHERE user_id=? AND read_at=0`, ms(), userID)
	return err
}

// ---- 小工具 ----

func jsonList(xs []string) string {
	if xs == nil {
		xs = []string{}
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

func parseJSONList(s string) []string {
	var out []string
	if s == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return strings.Split(s, "\n")
	}
	return out
}

func (s *Store) count(q string, args ...any) int64 {
	var n int64
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		log.Printf("[error] count: %v (%s)", err, q)
	}
	return n
}

func (s *Store) sum(q string, args ...any) int64 {
	var n sql.NullInt64
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		log.Printf("[error] sum: %v (%s)", err, q)
	}
	return n.Int64
}

// TouchIP 记录用户最近使用的 IP 段（陪审员关联排除、自导自演打标用）。
func (s *Store) TouchIP(userID int64, ip string) {
	if userID <= 0 || ip == "" {
		return
	}
	s.db.Exec(`INSERT INTO ip_log(user_id,prefix,last_seen) VALUES(?,?,?) ON CONFLICT(user_id,prefix) DO UPDATE SET last_seen=excluded.last_seen`, userID, ipPrefix(ip), ms())
}

// SharedIP 两个用户 30 天内是否用过同一 IP 段。
func (s *Store) SharedIP(a, b int64) bool {
	return s.count(`SELECT COUNT(*) FROM ip_log x JOIN ip_log y ON x.prefix=y.prefix WHERE x.user_id=? AND y.user_id=? AND x.last_seen>? AND y.last_seen>?`, a, b, ms()-30*dayMs, ms()-30*dayMs) > 0
}
