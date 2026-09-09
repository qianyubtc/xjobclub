package main

import (
	"database/sql"
)

func (s *Store) CreateJuryCase(disputeID, deadline int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO jury_cases(dispute_id,status,round,deadline_at,created_at) VALUES(?,'voting',1,?,?)`, disputeID, deadline, ms())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanCase(r scanner) (*JuryCase, error) {
	var c JuryCase
	err := r.Scan(&c.ID, &c.DisputeID, &c.Status, &c.Round, &c.DeadlineAt, &c.Verdict, &c.VotesFor, &c.VotesAgainst, &c.Abstain, &c.CreatedAt, &c.ClosedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

const caseCols = `id,dispute_id,status,round,deadline_at,verdict,votes_for,votes_against,abstain,created_at,closed_at`

func (s *Store) GetJuryCase(id int64) (*JuryCase, error) {
	return scanCase(s.db.QueryRow(`SELECT `+caseCols+` FROM jury_cases WHERE id=?`, id))
}

func (s *Store) VotingCases() ([]*JuryCase, error) {
	rows, err := s.db.Query(`SELECT ` + caseCols + ` FROM jury_cases WHERE status='voting' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JuryCase
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Invite(caseID int64, userIDs []int64, round int64) {
	for _, id := range userIDs {
		s.db.Exec(`INSERT OR IGNORE INTO jury_invites(case_id,user_id,round,invited_at) VALUES(?,?,?,?)`, caseID, id, round, ms())
	}
}

func (s *Store) Invited(caseID, userID int64) bool {
	return s.count(`SELECT COUNT(*) FROM jury_invites WHERE case_id=? AND user_id=?`, caseID, userID) > 0
}

func (s *Store) InvitedIDs(caseID int64) []int64 {
	rows, err := s.db.Query(`SELECT user_id FROM jury_invites WHERE case_id=?`, caseID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func (s *Store) NextRound(caseID int64) error {
	_, err := s.db.Exec(`UPDATE jury_cases SET round=round+1 WHERE id=?`, caseID)
	return err
}

// CastVote 投票（一人一票，不可改）。返回 false 表示已投过。
func (s *Store) CastVote(caseID, jurorID int64, vote, reason string) (bool, error) {
	var ok bool
	err := s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT OR IGNORE INTO jury_votes(case_id,juror_id,vote,reason,created_at) VALUES(?,?,?,?,?)`, caseID, jurorID, vote, reason, ms())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		ok = n == 1
		if !ok {
			return nil
		}
		col := map[string]string{"for": "votes_for", "against": "votes_against", "abstain": "abstain"}[vote]
		if _, err := tx.Exec(`UPDATE jury_cases SET `+col+`=`+col+`+1 WHERE id=?`, caseID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE jury_invites SET voted_at=? WHERE case_id=? AND user_id=?`, ms(), caseID, jurorID); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE users SET jury_noshow=0 WHERE id=?`, jurorID)
		return err
	})
	return ok, err
}

func (s *Store) Votes(caseID int64) ([]JuryVote, error) {
	rows, err := s.db.Query(`SELECT case_id,juror_id,vote,reason,created_at FROM jury_votes WHERE case_id=? ORDER BY created_at`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JuryVote
	for rows.Next() {
		var v JuryVote
		if err := rows.Scan(&v.CaseID, &v.JurorID, &v.Vote, &v.Reason, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) MyVote(caseID, jurorID int64) *JuryVote {
	var v JuryVote
	if err := s.db.QueryRow(`SELECT case_id,juror_id,vote,reason,created_at FROM jury_votes WHERE case_id=? AND juror_id=?`, caseID, jurorID).Scan(&v.CaseID, &v.JurorID, &v.Vote, &v.Reason, &v.CreatedAt); err != nil {
		return nil
	}
	return &v
}

// CloseCase 只对 voting 状态生效，返回是否命中（防两次结案重复计分）。
func (s *Store) CloseCase(id int64, status, verdict string) (bool, error) {
	res, err := s.db.Exec(`UPDATE jury_cases SET status=?, verdict=?, closed_at=? WHERE id=? AND status='voting'`, status, verdict, ms(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SettleJurors 结案后更新陪审员统计：有终裁时投中 +1；没有终裁（转管理员 / 平局）不计入一致率；没投票的记一次缺席。
func (s *Store) SettleJurors(caseID int64, verdict string) {
	if verdict != "" {
		votes, _ := s.Votes(caseID)
		for _, v := range votes {
			if v.Vote == "abstain" {
				continue
			}
			agree := v.Vote == verdict
			s.db.Exec(`UPDATE users SET jury_total=jury_total+1, jury_agree=jury_agree+?, jury_score=jury_score+? WHERE id=?`, b2i(agree), b2i(agree), v.JurorID)
		}
	}
	s.db.Exec(`UPDATE users SET jury_noshow=jury_noshow+1 WHERE id IN (SELECT user_id FROM jury_invites WHERE case_id=? AND voted_at=0)`, caseID)
}

func (s *Store) SetJuryBan(userID, until int64) {
	s.db.Exec(`UPDATE users SET jury_banned_until=? WHERE id=?`, until, userID)
}

// JuryPoolSize 合格陪审员数量（冷启动门槛）。
func (s *Store) JuryPoolSize(minAgeMs int64) int64 {
	return s.count(`SELECT COUNT(*) FROM users u WHERE u.status='active' AND u.created_at<=? AND u.jury_banned_until<? AND u.suspended_until<?
		AND (SELECT COUNT(*) FROM submissions x WHERE x.status='paid' AND x.confirm_method='gateway' AND x.self_deal=0 AND (x.worker_id=u.id OR x.task_id IN (SELECT id FROM tasks WHERE owner_id=u.id)))>=3`,
		ms()-minAgeMs, ms(), ms())
}

// JuryCandidates 随机抽合格陪审员：排除双方、与双方 90 天内有交易关系的、同案已邀请的、正在审 ≥3 件的、一年内审过同一对当事人的。
func (s *Store) JuryCandidates(caseID, opener, against int64, minAgeMs int64, limit int64) ([]int64, error) {
	now := ms()
	rows, err := s.db.Query(`SELECT u.id FROM users u WHERE u.status='active' AND u.created_at<=? AND u.jury_banned_until<? AND u.suspended_until<?
		AND u.id NOT IN (?,?) AND u.jury_noshow<3
		AND (SELECT COUNT(*) FROM submissions x WHERE x.status='paid' AND x.confirm_method='gateway' AND x.self_deal=0 AND (x.worker_id=u.id OR x.task_id IN (SELECT id FROM tasks WHERE owner_id=u.id)))>=3
		AND NOT EXISTS (SELECT 1 FROM submissions x JOIN tasks t ON t.id=x.task_id WHERE x.created_at>? AND ((x.worker_id=u.id AND t.owner_id IN (?,?)) OR (t.owner_id=u.id AND x.worker_id IN (?,?))))
		AND NOT EXISTS (SELECT 1 FROM jury_invites i WHERE i.case_id=? AND i.user_id=u.id)
		AND (SELECT COUNT(*) FROM jury_invites i JOIN jury_cases c ON c.id=i.case_id WHERE i.user_id=u.id AND i.voted_at=0 AND c.status='voting')<3
		AND NOT EXISTS (SELECT 1 FROM jury_votes v JOIN jury_cases c ON c.id=v.case_id JOIN disputes d ON d.id=c.dispute_id WHERE v.juror_id=u.id AND v.created_at>? AND ((d.opener_id=? AND d.against_id=?) OR (d.opener_id=? AND d.against_id=?)))
		AND NOT EXISTS (SELECT 1 FROM users a WHERE a.id IN (?,?) AND a.payer_id<>'' AND a.payer_id=u.payer_id)
		ORDER BY RANDOM() LIMIT ?`,
		now-minAgeMs, now, now, opener, against, now-90*dayMs, opener, against, opener, against, caseID, now-365*dayMs, opener, against, against, opener, opener, against, limit*2)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InvitedCasesFor 我被邀请且未投票的进行中案件。
func (s *Store) InvitedCasesFor(userID int64) ([]*JuryCase, error) {
	rows, err := s.db.Query(`SELECT `+caseCols+` FROM jury_cases WHERE status='voting' AND id IN (SELECT case_id FROM jury_invites WHERE user_id=? AND voted_at=0) ORDER BY deadline_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JuryCase
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) RecentCases(limit int) ([]*JuryCase, error) {
	rows, err := s.db.Query(`SELECT `+caseCols+` FROM jury_cases ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JuryCase
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
