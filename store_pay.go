package main

import (
	"database/sql"
)

const payCols = `id,submission_id,kind,user_id,bpg_order_id,merchant_order_id,pay_amount,base_e8,note_code,pay_url,receive_uid,receive_link,status,actual_e8,matched_by,binance_order_id,payer_id,paid_at,expires_at,raw_callback,last_sync_at,created_at,updated_at`

func scanPay(r scanner) (*Payment, error) {
	var p Payment
	err := r.Scan(&p.ID, &p.SubmissionID, &p.Kind, &p.UserID, &p.BPGOrderID, &p.MerchantOrderID, &p.PayAmount, &p.BaseE8, &p.NoteCode, &p.PayURL, &p.ReceiveUID, &p.ReceiveLink, &p.Status, &p.ActualE8, &p.MatchedBy, &p.BinanceOrderID, &p.PayerID, &p.PaidAt, &p.ExpiresAt, &p.RawCallback, &p.LastSyncAt, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) CreatePayment(p *Payment) (int64, error) {
	now := ms()
	res, err := s.db.Exec(`INSERT INTO payments(submission_id,kind,user_id,bpg_order_id,merchant_order_id,pay_amount,base_e8,note_code,pay_url,receive_uid,receive_link,status,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.SubmissionID, p.Kind, p.UserID, p.BPGOrderID, p.MerchantOrderID, p.PayAmount, p.BaseE8, p.NoteCode, p.PayURL, p.ReceiveUID, p.ReceiveLink, p.Status, p.ExpiresAt, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetPaymentByID(id int64) (*Payment, error) {
	return scanPay(s.db.QueryRow(`SELECT `+payCols+` FROM payments WHERE id=?`, id))
}

func (s *Store) GetPaymentByMerchant(mid string) (*Payment, error) {
	return scanPay(s.db.QueryRow(`SELECT `+payCols+` FROM payments WHERE merchant_order_id=?`, mid))
}

func (s *Store) queryPays(where string, args ...any) ([]*Payment, error) {
	rows, err := s.db.Query(`SELECT `+payCols+` FROM payments `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Payment
	for rows.Next() {
		p, err := scanPay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ActivePayment 某记录当前未过期的网关结账会话。
func (s *Store) ActivePayment(subID int64, kind string) (*Payment, error) {
	ps, err := s.queryPays(`WHERE submission_id=? AND kind=? AND status='pending' AND expires_at>? ORDER BY id DESC LIMIT 1`, subID, kind, ms())
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	return ps[0], nil
}

func (s *Store) PaymentsForSub(subID int64) ([]*Payment, error) {
	return s.queryPays(`WHERE submission_id=? ORDER BY id DESC`, subID)
}

func (s *Store) PendingPayments() ([]*Payment, error) {
	return s.queryPays(`WHERE status='pending' AND bpg_order_id<>'' ORDER BY id LIMIT 200`)
}

// ExpiredRecentPayments 7 天内过期的网关订单（回填窗口内仍可能变 paid）。
func (s *Store) ExpiredRecentPayments(gap int64) ([]*Payment, error) {
	return s.queryPays(`WHERE status='expired' AND bpg_order_id<>'' AND expires_at>? AND last_sync_at<? ORDER BY id LIMIT 100`, ms()-7*dayMs, ms()-gap)
}

// PendingForSubs 某些记录名下仍 pending 的网关订单（终态时关闭用）。
func (s *Store) PendingForSub(subID int64) ([]*Payment, error) {
	return s.queryPays(`WHERE submission_id=? AND status='pending' AND bpg_order_id<>''`, subID)
}

// PendingForAccount 某网关账号名下的在途订单（降级前关闭用）。
func (s *Store) PendingForUserPayee(userID int64) ([]*Payment, error) {
	return s.queryPays(`WHERE status='pending' AND bpg_order_id<>'' AND submission_id IN (SELECT id FROM submissions WHERE worker_id=?)`, userID)
}

func toAny(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// ActiveCertPayment 用户未过期的认证付款会话。
func (s *Store) ActiveCertPayment(userID int64) (*Payment, error) {
	ps, err := s.queryPays(`WHERE user_id=? AND kind='cert' AND status='pending' AND expires_at>? ORDER BY id DESC LIMIT 1`, userID, ms())
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	return ps[0], nil
}

// TouchSync 主动查单节流：距上次查单不足 gap 毫秒返回 false。
func (s *Store) TouchSync(id, now, gap int64) bool {
	res, err := s.db.Exec(`UPDATE payments SET last_sync_at=? WHERE id=? AND last_sync_at<=?`, now, id, now-gap)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// SettlePayment 把网关状态落库（幂等：只改 pending 的单）。返回是否本次改变了状态。
func (s *Store) SettlePayment(id int64, status string, actualE8 int64, matchedBy, boid, payerID string, paidAt int64, raw string) (bool, error) {
	from := []string{"pending"}
	if status == "paid" || status == "underpaid" {
		from = []string{"pending", "expired", "closed"} // 宽限期 / 回填窗口内到账可覆盖已过期
	}
	res, err := s.db.Exec(`UPDATE payments SET status=?, actual_e8=?, matched_by=?, binance_order_id=?, payer_id=?, paid_at=?, raw_callback=?, updated_at=? WHERE id=? AND status IN (`+placeholders(len(from))+`)`,
		append([]any{status, actualE8, matchedBy, boid, payerID, paidAt, raw, ms(), id}, toAny(from)...)...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
