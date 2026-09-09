package main

import (
	"database/sql"
	"strings"
)

const userCols = `id,x_id,handle,handle_lower,display_name,avatar_url,pass_hash,status,reg_tweet_id,x_created_ms,payer_id,cert_paid_at,suspended_until,jury_score,jury_total,jury_agree,jury_noshow,jury_banned_until,handle_stale,created_at,last_login_at`

type scanner interface{ Scan(dest ...any) error }

func scanUser(r scanner) (*User, error) {
	var u User
	var stale int64
	err := r.Scan(&u.ID, &u.XID, &u.Handle, &u.HandleLower, &u.DisplayName, &u.AvatarURL, &u.PassHash, &u.Status, &u.RegTweetID, &u.XCreatedMs, &u.PayerID, &u.CertPaidAt, &u.SuspendedUntil, &u.JuryScore, &u.JuryTotal, &u.JuryAgree, &u.JuryNoShow, &u.JuryBannedUntil, &stale, &u.CreatedAt, &u.LastLoginAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.HandleStale = stale == 1
	return &u, nil
}

func (s *Store) userBy(where string, args ...any) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE `+where, args...))
}

func (s *Store) GetUserByID(id int64) (*User, error) { return s.userBy(`id=?`, id) }
func (s *Store) GetUserByXID(xid string) (*User, error) {
	if xid == "" {
		return nil, nil
	}
	return s.userBy(`x_id=?`, xid)
}
func (s *Store) GetUserByHandle(h string) (*User, error) {
	return s.userBy(`handle_lower=?`, strings.ToLower(h))
}

// GetUserByLogin 登录名：X 用户名（可带 @）或 X 数字 ID（用户名被顶替后的兜底）。
func (s *Store) GetUserByLogin(in string) (*User, error) {
	in = strings.TrimPrefix(strings.TrimSpace(in), "@")
	if in == "" {
		return nil, nil
	}
	if u, err := s.GetUserByHandle(in); err != nil || u != nil {
		return u, err
	}
	if strings.Trim(in, "0123456789") == "" {
		return s.GetUserByXID(in)
	}
	return nil, nil
}

// CreateUser 建号。同 X 数字 ID 已存在 → ErrXTaken；用户名被别的 X 号占着（原主改名了）→ 把原主的登录名改成 X 数字 ID 并打过期标。
func (s *Store) CreateUser(u *User) (int64, error) {
	var id int64
	err := s.tx(func(tx *sql.Tx) error {
		var n int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE x_id=?`, u.XID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrXTaken
		}
		lower := strings.ToLower(u.Handle)
		var oldID int64
		err := tx.QueryRow(`SELECT id FROM users WHERE handle_lower=? AND x_id<>?`, lower, u.XID).Scan(&oldID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if oldID > 0 {
			if _, err := tx.Exec(`UPDATE users SET handle_lower='xid:'||x_id, handle_stale=1 WHERE id=?`, oldID); err != nil {
				return err
			}
			tx.Exec(`INSERT INTO notifications(user_id,kind,title,body,link,created_at) VALUES(?,?,?,?,?,?)`, oldID, "account", "你的 X 用户名已被别人使用",
				"你在 X 上改过名，原用户名被其他账号注册到本站。请用 X 数字 ID "+u.XID+" 登录后重新做一次发帖验证以同步新用户名。", "/reset", ms())
		}
		res, err := tx.Exec(`INSERT INTO users(x_id,handle,handle_lower,display_name,avatar_url,pass_hash,status,reg_tweet_id,x_created_ms,created_at,last_login_at) VALUES(?,?,?,?,?,?,'active',?,?,?,?)`,
			u.XID, u.Handle, lower, u.DisplayName, u.AvatarURL, u.PassHash, u.RegTweetID, u.XCreatedMs, ms(), ms())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

func (s *Store) SetPassword(id int64, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET pass_hash=? WHERE id=?`, hash, id)
	return err
}

func (s *Store) TouchLogin(id int64) {
	s.db.Exec(`UPDATE users SET last_login_at=? WHERE id=?`, ms(), id)
}

// SyncX 发帖验证后同步 X 资料（用户名可能变了）。用户名被别人占着就不改用户名，只改昵称头像。
func (s *Store) SyncX(id int64, handle, name, avatar string) error {
	lower := strings.ToLower(handle)
	return s.tx(func(tx *sql.Tx) error {
		var other int64
		err := tx.QueryRow(`SELECT id FROM users WHERE handle_lower=? AND id<>?`, lower, id).Scan(&other)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if other > 0 {
			_, err = tx.Exec(`UPDATE users SET display_name=?, avatar_url=? WHERE id=?`, name, avatar, id)
			return err
		}
		_, err = tx.Exec(`UPDATE users SET handle=?, handle_lower=?, display_name=?, avatar_url=?, handle_stale=0 WHERE id=?`, handle, lower, name, avatar, id)
		return err
	})
}

func (s *Store) SetUserStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE users SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) SetSuspended(id, until int64) error {
	_, err := s.db.Exec(`UPDATE users SET suspended_until=? WHERE id=?`, until, id)
	return err
}

// SetPayerID 记录付款方 Pay 账户 ID；被别人占用返回 ErrPayerTaken。
func (s *Store) SetPayerID(id int64, payerID string, certPaid bool) error {
	if payerID == "" {
		return nil
	}
	return s.tx(func(tx *sql.Tx) error {
		var other int64
		err := tx.QueryRow(`SELECT id FROM users WHERE payer_id=? AND id<>?`, payerID, id).Scan(&other)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if other > 0 {
			return ErrPayerTaken
		}
		if certPaid {
			_, err = tx.Exec(`UPDATE users SET payer_id=?, cert_paid_at=CASE WHEN cert_paid_at=0 THEN ? ELSE cert_paid_at END WHERE id=?`, payerID, ms(), id)
		} else {
			_, err = tx.Exec(`UPDATE users SET payer_id=CASE WHEN payer_id='' THEN ? ELSE payer_id END WHERE id=?`, payerID, id)
		}
		return err
	})
}

func (s *Store) UserByPayerID(payerID string) (*User, error) {
	if payerID == "" {
		return nil, nil
	}
	return s.userBy(`payer_id=?`, payerID)
}

func (s *Store) SearchUsers(q string, limit int) ([]*User, error) {
	q = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(q), "@"))
	rows, err := s.db.Query(`SELECT `+userCols+` FROM users WHERE handle_lower LIKE ? OR x_id=? ORDER BY id DESC LIMIT ?`, q+"%", q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UsersByIDs(ids []int64) (map[int64]*User, error) {
	out := map[int64]*User{}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		u, err := s.GetUserByID(id)
		if err != nil {
			return nil, err
		}
		if u != nil {
			out[id] = u
		}
	}
	return out, nil
}

// ---- 验证码 ----

type Verify struct {
	Code      string
	Purpose   string
	Secret    string
	Profile   string
	Used      bool
	CreatedAt int64
}

func (s *Store) CreateVerify(code, purpose, secretHash string) error {
	_, err := s.db.Exec(`INSERT INTO xverify(code,purpose,secret,created_at) VALUES(?,?,?,?)`, code, purpose, secretHash, ms())
	return err
}

func (s *Store) GetVerify(code string) (*Verify, error) {
	var v Verify
	var used int64
	err := s.db.QueryRow(`SELECT code,purpose,secret,profile,used,created_at FROM xverify WHERE code=?`, code).Scan(&v.Code, &v.Purpose, &v.Secret, &v.Profile, &used, &v.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.Used = used == 1
	return &v, nil
}

func (s *Store) SetVerifyProfile(code, profile string) error {
	_, err := s.db.Exec(`UPDATE xverify SET profile=? WHERE code=?`, profile, code)
	return err
}

// UseVerify 一次性消费；已用过返回 false。
func (s *Store) UseVerify(code string) (bool, error) {
	res, err := s.db.Exec(`UPDATE xverify SET used=1 WHERE code=? AND used=0`, code)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) PruneVerify() {
	s.db.Exec(`DELETE FROM xverify WHERE created_at<?`, ms()-2*dayMs)
}

// ---- 收款设置 ----

func (s *Store) GetPayProfile(userID int64) (*PayProfile, error) {
	var p PayProfile
	err := s.db.QueryRow(`SELECT user_id,binance_uid,receive_email,mode,bpg_account_id,api_key_masked,bpg_last_ok,bpg_last_err,updated_at FROM pay_profiles WHERE user_id=?`, userID).
		Scan(&p.UserID, &p.BinanceUID, &p.ReceiveEmail, &p.Mode, &p.BPGAccountID, &p.APIKeyMasked, &p.BPGLastOK, &p.BPGLastErr, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertPayProfile 保存收款设置；网关账号被别人占用返回 ErrAccountTaken（UID 自填不核验，不做唯一）。
func (s *Store) UpsertPayProfile(p *PayProfile) error {
	return s.tx(func(tx *sql.Tx) error {
		var other int64
		var err error
		if p.BPGAccountID != "" {
			err = tx.QueryRow(`SELECT user_id FROM pay_profiles WHERE bpg_account_id=? AND user_id<>?`, p.BPGAccountID, p.UserID).Scan(&other)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if other > 0 {
				return ErrAccountTaken
			}
		}
		_, err = tx.Exec(`INSERT INTO pay_profiles(user_id,binance_uid,receive_email,mode,bpg_account_id,api_key_masked,bpg_last_ok,bpg_last_err,updated_at) VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id) DO UPDATE SET binance_uid=excluded.binance_uid, receive_email=excluded.receive_email, mode=excluded.mode, bpg_account_id=excluded.bpg_account_id,
			api_key_masked=excluded.api_key_masked, bpg_last_ok=excluded.bpg_last_ok, bpg_last_err=excluded.bpg_last_err, updated_at=excluded.updated_at`,
			p.UserID, p.BinanceUID, p.ReceiveEmail, p.Mode, p.BPGAccountID, p.APIKeyMasked, p.BPGLastOK, p.BPGLastErr, ms())
		return err
	})
}

func (s *Store) SetPayHealth(userID, lastOK int64, lastErr string) {
	s.db.Exec(`UPDATE pay_profiles SET bpg_last_ok=?, bpg_last_err=? WHERE user_id=?`, lastOK, lastErr, userID)
}

// DowngradePay 网关 Key 失效：降级为手动模式。
func (s *Store) DowngradePay(userID int64, reason string) error {
	_, err := s.db.Exec(`UPDATE pay_profiles SET mode='manual', bpg_account_id='', api_key_masked='', bpg_last_err=?, updated_at=? WHERE user_id=?`, reason, ms(), userID)
	return err
}

func (s *Store) GatewayProfiles() ([]*PayProfile, error) {
	rows, err := s.db.Query(`SELECT user_id,binance_uid,receive_email,mode,bpg_account_id,api_key_masked,bpg_last_ok,bpg_last_err,updated_at FROM pay_profiles WHERE mode='gateway' AND bpg_account_id<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PayProfile
	for rows.Next() {
		var p PayProfile
		if err := rows.Scan(&p.UserID, &p.BinanceUID, &p.ReceiveEmail, &p.Mode, &p.BPGAccountID, &p.APIKeyMasked, &p.BPGLastOK, &p.BPGLastErr, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}
