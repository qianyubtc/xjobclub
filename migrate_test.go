package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// 旧库缺少后加的列时，openStore 应通过 migrations 自动补齐。
func TestMigrationsAddMissingColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// 用当前 schema 建表，再把 submissions 重建成缺列的旧版本
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`DROP TABLE submissions`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE submissions (id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT NOT NULL UNIQUE, task_id INTEGER NOT NULL, worker_id INTEGER NOT NULL, variant_idx INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL, prev_status TEXT NOT NULL DEFAULT '', claimed_at INTEGER NOT NULL, claim_expires_at INTEGER NOT NULL, tweet_id TEXT NOT NULL DEFAULT '', tweet_url TEXT NOT NULL DEFAULT '', tweet_text TEXT NOT NULL DEFAULT '', tweet_created_at INTEGER NOT NULL DEFAULT 0, verify_attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', next_verify_at INTEGER NOT NULL DEFAULT 0, verified_at INTEGER NOT NULL DEFAULT 0, recheck_due_at INTEGER NOT NULL DEFAULT 0, recheck_flag TEXT NOT NULL DEFAULT '', recheck_tries INTEGER NOT NULL DEFAULT 0, payable_at INTEGER NOT NULL DEFAULT 0, pay_deadline_at INTEGER NOT NULL DEFAULT 0, overdue_at INTEGER NOT NULL DEFAULT 0, reported_at INTEGER NOT NULL DEFAULT 0, grace_until INTEGER NOT NULL DEFAULT 0, marked_paid_at INTEGER NOT NULL DEFAULT 0, marked_order_id TEXT NOT NULL DEFAULT '', marked_note TEXT NOT NULL DEFAULT '', underpaid_e8 INTEGER NOT NULL DEFAULT 0, confirmed_at INTEGER NOT NULL DEFAULT 0, confirm_method TEXT NOT NULL DEFAULT '', paid_amount_e8 INTEGER NOT NULL DEFAULT 0, late INTEGER NOT NULL DEFAULT 0, void_reason TEXT NOT NULL DEFAULT '', defaulted_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore on old db: %v", err)
	}
	defer st.Close()
	// 能用新列查询即迁移成功
	if _, err := st.db.Exec(`INSERT INTO submissions(code,task_id,worker_id,status,claimed_at,claim_expires_at,created_at,updated_at) VALUES('S-1',1,1,'claimed',1,2,1,1)`); err != nil {
		t.Fatal(err)
	}
	x, err := st.GetSubByCode("S-1")
	if err != nil || x == nil || x.TopupRequested != 0 || x.SelfDeal != 0 {
		t.Fatalf("query after migration: %v %+v", err, x)
	}
	// 再开一次不应报错（重复 ALTER 被忽略）
	st2, err := openStore(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	st2.Close()
}
