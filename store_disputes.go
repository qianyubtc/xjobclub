package main

import (
	"database/sql"
	"encoding/json"
	"strings"
)

const dispCols = `id,code,type,submission_id,task_id,opener_id,against_id,status,evidence_until,resolution,resolution_note,resolved_by,resolved_at,jury_case_id,appeal_until,appeal_by,created_at,updated_at`

func scanDispute(r scanner) (*Dispute, error) {
	var d Dispute
	err := r.Scan(&d.ID, &d.Code, &d.Type, &d.SubmissionID, &d.TaskID, &d.OpenerID, &d.AgainstID, &d.Status, &d.EvidenceUntil, &d.Resolution, &d.ResolutionNote, &d.ResolvedBy, &d.ResolvedAt, &d.JuryCaseID, &d.AppealUntil, &d.AppealBy, &d.CreatedAt, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) CreateDispute(d *Dispute) (int64, error) {
	now := ms()
	res, err := s.db.Exec(`INSERT INTO disputes(code,type,submission_id,task_id,opener_id,against_id,status,evidence_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		d.Code, d.Type, d.SubmissionID, d.TaskID, d.OpenerID, d.AgainstID, d.Status, d.EvidenceUntil, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetDisputeByCode(code string) (*Dispute, error) {
	return scanDispute(s.db.QueryRow(`SELECT `+dispCols+` FROM disputes WHERE code=?`, strings.ToUpper(strings.TrimSpace(code))))
}

func (s *Store) GetDisputeByID(id int64) (*Dispute, error) {
	return scanDispute(s.db.QueryRow(`SELECT `+dispCols+` FROM disputes WHERE id=?`, id))
}

func (s *Store) queryDisputes(where string, args ...any) ([]*Dispute, error) {
	rows, err := s.db.Query(`SELECT `+dispCols+` FROM disputes `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Dispute
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// OpenDisputeForSub 某记录上未结案的申诉（同一记录同时只允许一件）。
func (s *Store) OpenDisputeForSub(subID int64) (*Dispute, error) {
	ds, err := s.queryDisputes(`WHERE submission_id=? AND status<>'resolved' ORDER BY id DESC LIMIT 1`, subID)
	if err != nil || len(ds) == 0 {
		return nil, err
	}
	return ds[0], nil
}

func (s *Store) DisputesForSub(subID int64) ([]*Dispute, error) {
	return s.queryDisputes(`WHERE submission_id=? ORDER BY id DESC`, subID)
}

func (s *Store) DisputesByStatus(statuses []string, limit int) ([]*Dispute, error) {
	args := []any{}
	for _, st := range statuses {
		args = append(args, st)
	}
	args = append(args, limit)
	return s.queryDisputes(`WHERE status IN (`+placeholders(len(statuses))+`) ORDER BY id LIMIT ?`, args...)
}

func (s *Store) DisputesForUser(userID int64, limit int) ([]*Dispute, error) {
	return s.queryDisputes(`WHERE opener_id=? OR against_id=? ORDER BY id DESC LIMIT ?`, userID, userID, limit)
}

func (s *Store) OpenBlacklistAppeal(userID int64) (*Dispute, error) {
	ds, err := s.queryDisputes(`WHERE type='F' AND opener_id=? AND status<>'resolved' ORDER BY id DESC LIMIT 1`, userID)
	if err != nil || len(ds) == 0 {
		return nil, err
	}
	return ds[0], nil
}

func (s *Store) CountBlacklistAppeals(userID, since int64) int64 {
	return s.count(`SELECT COUNT(*) FROM disputes WHERE type='F' AND opener_id=? AND created_at>=?`, userID, since)
}

func (s *Store) SetDisputeStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE disputes SET status=?, updated_at=? WHERE id=?`, status, ms(), id)
	return err
}

func (s *Store) SetDisputeJury(id, caseID int64) error {
	_, err := s.db.Exec(`UPDATE disputes SET status='jury', jury_case_id=?, updated_at=? WHERE id=?`, caseID, ms(), id)
	return err
}

func (s *Store) SetDisputeAppeal(id, until int64, resolution, note string) error {
	_, err := s.db.Exec(`UPDATE disputes SET status='appeal', appeal_until=?, resolution=?, resolution_note=?, resolved_by=0, updated_at=? WHERE id=?`, until, resolution, note, ms(), id)
	return err
}

func (s *Store) SetAppealRequested(id, by int64) error {
	_, err := s.db.Exec(`UPDATE disputes SET status='review', appeal_by=?, updated_at=? WHERE id=? AND status='appeal'`, by, ms(), id)
	return err
}

func (s *Store) ResolveDispute(id int64, resolution, note string, by int64) error {
	_, err := s.db.Exec(`UPDATE disputes SET status='resolved', resolution=?, resolution_note=?, resolved_by=?, resolved_at=?, updated_at=? WHERE id=?`, resolution, note, by, ms(), ms(), id)
	return err
}

// EvidenceExpired 举证期已过、尚未进入审理的申诉。
func (s *Store) EvidenceExpired(now int64) ([]*Dispute, error) {
	return s.queryDisputes(`WHERE status='evidence' AND evidence_until<=? ORDER BY id LIMIT 100`, now)
}

func (s *Store) AppealExpired(now int64) ([]*Dispute, error) {
	return s.queryDisputes(`WHERE status='appeal' AND appeal_until<=? ORDER BY id LIMIT 100`, now)
}

// ---- 申诉留言 / 证据 ----

func (s *Store) AddDisputeMessage(disputeID, authorID int64, text string, images []string) error {
	img, _ := json.Marshal(images)
	if images == nil {
		img = []byte("[]")
	}
	_, err := s.db.Exec(`INSERT INTO dispute_messages(dispute_id,author_id,text,images,created_at) VALUES(?,?,?,?,?)`, disputeID, authorID, text, string(img), ms())
	return err
}

func (s *Store) DisputeMessages(disputeID int64) ([]DisputeMessage, error) {
	rows, err := s.db.Query(`SELECT id,dispute_id,author_id,text,images,created_at FROM dispute_messages WHERE dispute_id=? ORDER BY id`, disputeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DisputeMessage
	for rows.Next() {
		var m DisputeMessage
		var img string
		if err := rows.Scan(&m.ID, &m.DisputeID, &m.AuthorID, &m.Text, &img, &m.CreatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(img), &m.Images)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) CountMessagesBy(disputeID, authorID int64) int64 {
	return s.count(`SELECT COUNT(*) FROM dispute_messages WHERE dispute_id=? AND author_id=?`, disputeID, authorID)
}

// ---- 黑名单 ----

const blCols = `b.id,b.user_id,b.x_id,b.handle,b.binance_uid,b.payer_id,b.role,b.reason,b.dispute_id,COALESCE(d.code,''),b.amount_owed_e8,b.repaid_at,b.lifted_at,b.lifted_by,b.note,b.created_at`

func scanBL(r scanner) (*BlacklistEntry, error) {
	var b BlacklistEntry
	err := r.Scan(&b.ID, &b.UserID, &b.XID, &b.Handle, &b.BinanceUID, &b.PayerID, &b.Role, &b.Reason, &b.DisputeID, &b.DisputeCode, &b.AmountOwedE8, &b.RepaidAt, &b.LiftedAt, &b.LiftedBy, &b.Note, &b.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) AddBlacklist(b *BlacklistEntry) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO blacklist(user_id,x_id,handle,binance_uid,payer_id,role,reason,dispute_id,amount_owed_e8,note,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		b.UserID, b.XID, b.Handle, b.BinanceUID, b.PayerID, b.Role, b.Reason, b.DisputeID, b.AmountOwedE8, b.Note, ms())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) queryBL(where string, args ...any) ([]*BlacklistEntry, error) {
	rows, err := s.db.Query(`SELECT `+blCols+` FROM blacklist b LEFT JOIN disputes d ON d.id=b.dispute_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BlacklistEntry
	for rows.Next() {
		b, err := scanBL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) ListBlacklist(limit, offset int) ([]*BlacklistEntry, int64, error) {
	total := s.count(`SELECT COUNT(*) FROM blacklist`)
	bs, err := s.queryBL(`ORDER BY b.id DESC LIMIT ? OFFSET ?`, limit, offset)
	return bs, total, err
}

func (s *Store) BlacklistForUser(userID int64) ([]*BlacklistEntry, error) {
	return s.queryBL(`WHERE b.user_id=? ORDER BY b.id DESC`, userID)
}

func (s *Store) ActiveBlacklist(userID int64) (*BlacklistEntry, error) {
	bs, err := s.queryBL(`WHERE b.user_id=? AND b.lifted_at=0 ORDER BY b.id DESC LIMIT 1`, userID)
	if err != nil || len(bs) == 0 {
		return nil, err
	}
	return bs[0], nil
}

// BlacklistedIdentity 任一身份键命中在榜记录（换 X 号回归也认得出）。
func (s *Store) BlacklistedIdentity(xid, uid, payerID string) bool {
	n := s.count(`SELECT COUNT(*) FROM blacklist WHERE lifted_at=0 AND ((x_id<>'' AND x_id=?) OR (binance_uid<>'' AND binance_uid=?) OR (payer_id<>'' AND payer_id=?))`, xid, uid, payerID)
	return n > 0
}

func (s *Store) LiftBlacklist(id, by int64, note string) error {
	return s.tx(func(tx *sql.Tx) error {
		var uid int64
		if err := tx.QueryRow(`SELECT user_id FROM blacklist WHERE id=?`, id).Scan(&uid); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE blacklist SET lifted_at=?, lifted_by=?, note=? WHERE id=?`, ms(), by, note, id); err != nil {
			return err
		}
		var remaining int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM blacklist WHERE user_id=? AND lifted_at=0`, uid).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			_, err := tx.Exec(`UPDATE users SET status='active' WHERE id=? AND status='blacklisted'`, uid)
			return err
		}
		return nil
	})
}

func (s *Store) MarkRepaid(disputeID int64) {
	s.db.Exec(`UPDATE blacklist SET repaid_at=? WHERE dispute_id=? AND repaid_at=0`, ms(), disputeID)
}

// RepayBlacklist 违约补付：从该用户在榜条目的欠款里扣减，扣到 0 标记已补付（不论条目由 A/B 还是管理员产生）。
func (s *Store) RepayBlacklist(userID, amountE8 int64) {
	s.db.Exec(`UPDATE blacklist SET amount_owed_e8=MAX(0, amount_owed_e8-?) WHERE id=(SELECT id FROM blacklist WHERE user_id=? AND lifted_at=0 AND amount_owed_e8>0 ORDER BY id DESC LIMIT 1)`, amountE8, userID)
	s.db.Exec(`UPDATE blacklist SET repaid_at=? WHERE user_id=? AND lifted_at=0 AND amount_owed_e8=0 AND repaid_at=0`, ms(), userID)
}

// AddOwed 已在榜用户新增欠款并入现有条目。
func (s *Store) AddOwed(userID, amountE8 int64) {
	s.db.Exec(`UPDATE blacklist SET amount_owed_e8=amount_owed_e8+?, repaid_at=0 WHERE id=(SELECT id FROM blacklist WHERE user_id=? AND lifted_at=0 ORDER BY id DESC LIMIT 1)`, amountE8, userID)
}

func (s *Store) DisputeIDForSub(subID int64, typ string) int64 {
	var id int64
	s.db.QueryRow(`SELECT id FROM disputes WHERE submission_id=? AND type=? ORDER BY id DESC LIMIT 1`, subID, typ).Scan(&id)
	return id
}
