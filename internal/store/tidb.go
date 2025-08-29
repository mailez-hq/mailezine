// TiDBKV implements the TxnKV contract over a TiDB server (MySQL wire
// protocol, pure-Go driver). One clustered table maps byte keys to byte
// values; WithTxn maps onto native SQL transactions so every Store
// read-modify-write cycle gets server-side conflict detection, which is what
// enables horizontal scale-out across process instances (ARCHITECTURE.md §3.3).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// maxTxnAttempts bounds the replay loop when multiple instances contend on
// the same logical records (quota counters, counters, mailbox modseq).
const maxTxnAttempts = 16

// retryableSQLCode reports whether err is a transient transaction failure
// whose whole closure must be replayed: deadlock victim (1213), lock wait
// timeout (1205), TiDB write conflict in optimistic mode (8028) and the
// pessimistic latched variant (9007).
func retryableSQLCode(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	switch me.Number {
	case 1213, 1205, 9007, 8028:
		return true
	}
	return false
}

func retryDelay(attempt int) time.Duration {
	d := time.Duration(attempt) * 10 * time.Millisecond
	if d > 200*time.Millisecond {
		d = 200 * time.Millisecond
	}
	return d
}

// validTableName enforces an ASCII identifier ([A-Za-z0-9_]{1,64}) so table
// names can be interpolated into DDL/statements inside backticks safely.
func validTableName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// TiDBKV is a TxnKV backed by a single MySQL-compatible table.
type TiDBKV struct {
	db    *sql.DB
	table string
	once  sync.Once
}

// OpenTiDB connects to a TiDB (or MySQL-compatible) server. The kv table is
// created automatically if missing; the caller's database must exist.
func OpenTiDB(dsn, table string) (*TiDBKV, error) {
	if !validTableName(table) {
		return nil, fmt.Errorf("store: invalid kv table name %q", table)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// Bound the pool: an unbounded default lets a traffic burst open
	// hundreds of concurrent TiDB sessions (each a txn memory footprint
	// server-side). Sensible fixed limits; callers requiring more can
	// raise them via the returned *sql.DB accessors if ever exposed.
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	ddl := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS `%s` (k VARBINARY(1024) NOT NULL PRIMARY KEY, v MEDIUMBLOB NOT NULL)",
		table)
	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return nil, err
	}
	return &TiDBKV{db: db, table: table}, nil
}

func (k *TiDBKV) Get(key []byte) ([]byte, error) {
	var v []byte
	err := k.db.QueryRow(fmt.Sprintf("SELECT v FROM `%s` WHERE k = ?", k.table), key).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return v, nil
}

func (k *TiDBKV) Put(key, value []byte) error {
	q := fmt.Sprintf("INSERT INTO `%s` (k, v) VALUES (?, ?) ON DUPLICATE KEY UPDATE v = VALUES(v)", k.table)
	_, err := k.db.Exec(q, key, value)
	return err
}

func (k *TiDBKV) Delete(key []byte) error {
	_, err := k.db.Exec(fmt.Sprintf("DELETE FROM `%s` WHERE k = ?", k.table), key)
	return err
}

// Scan streams prefix-matching rows in ascending key order using keyset
// pagination so arbitrarily large prefixes do not need unbounded memory.
func (k *TiDBKV) Scan(prefix []byte, fn func(k, v []byte) error) error {
	const pageSize = 512
	start := append([]byte(nil), prefix...)
	var end []byte
	hasEnd := len(prefix) > 0
	if hasEnd {
		end = prefixEnd(prefix)
	}
	sel := fmt.Sprintf("SELECT k, v FROM `%s`", k.table)
	first := true
	for {
		q := sel
		var args []any
		op := ">="
		if !first {
			op = ">"
		}
		q += " WHERE k " + op + " ?"
		args = append(args, start)
		if hasEnd {
			q += " AND k < ?"
			args = append(args, end)
		}
		q += " ORDER BY k ASC LIMIT ?"
		args = append(args, pageSize)

		rows, err := k.db.Query(q, args...)
		if err != nil {
			return err
		}
		type pair struct{ kb, vb []byte }
		buf := make([]pair, 0, pageSize)
		for rows.Next() {
			var kb, vb []byte
			if err := rows.Scan(&kb, &vb); err != nil {
				rows.Close()
				return err
			}
			buf = append(buf, pair{kb, vb})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, p := range buf {
			if err := fn(p.kb, p.vb); err != nil {
				return err
			}
		}
		if len(buf) < pageSize {
			return nil
		}
		start = buf[len(buf)-1].kb
		first = false
	}
}

func (k *TiDBKV) Batch(ops []Op) error {
	putQ := fmt.Sprintf("INSERT INTO `%s` (k, v) VALUES (?, ?) ON DUPLICATE KEY UPDATE v = VALUES(v)", k.table)
	delQ := fmt.Sprintf("DELETE FROM `%s` WHERE k = ?", k.table)
	var lastErr error
	for attempt := 0; attempt < maxTxnAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay(attempt))
		}
		tx, err := k.db.Begin()
		if err != nil {
			return err
		}
		for _, o := range ops {
			if o.Delete {
				_, err = tx.Exec(delQ, o.Key)
			} else {
				_, err = tx.Exec(putQ, o.Key, o.Value)
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil && retryableSQLCode(err) {
			lastErr = err
			continue
		}
		return err
	}
	return fmt.Errorf("store: batch failed after %d retries: %w", maxTxnAttempts, lastErr)
}

// WithTxn replays fn from scratch whenever the backend reports a conflicting
// commit, matching the TxnKV contract. Application-level errors returned by
// fn abort immediately without retry.
func (k *TiDBKV) WithTxn(ctx context.Context, fn func(t TxnOps) error) error {
	putQ := fmt.Sprintf("INSERT INTO `%s` (k, v) VALUES (?, ?) ON DUPLICATE KEY UPDATE v = VALUES(v)", k.table)
	delQ := fmt.Sprintf("DELETE FROM `%s` WHERE k = ?", k.table)
	getQ := fmt.Sprintf("SELECT v FROM `%s` WHERE k = ?", k.table)

	var lastErr error
	for attempt := 0; attempt < maxTxnAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			time.Sleep(retryDelay(attempt))
		}
		tx, err := k.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		t := &tidbTxn{ctx: ctx, tx: tx, table: k.table, getQ: getQ, putQ: putQ, delQ: delQ}
		if err := fn(t); err != nil {
			tx.Rollback()
			return err
		}
		// Statement-level failures poison the SQL transaction; route them
		// through the same retry decision as commit-time conflicts.
		err = t.err
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil && retryableSQLCode(err) {
			lastErr = err
			continue
		}
		return err
	}
	return fmt.Errorf("store: transaction failed after %d retries: %w", maxTxnAttempts, lastErr)
}

// tidbTxn applies staged writes directly on the SQL transaction; the engine
// provides read-your-writes natively, so Get after Put/Delete observes them.
type tidbTxn struct {
	ctx              context.Context
	tx               *sql.Tx
	table            string
	getQ, putQ, delQ string
	// err holds the first statement failure; the commit phase consumes it.
	err error
}

func (t *tidbTxn) Get(key []byte) ([]byte, error) {
	var v []byte
	err := t.tx.QueryRowContext(t.ctx, t.getQ, key).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return v, nil
}

func (t *tidbTxn) Put(key, val []byte) {
	t.exec(t.putQ, key, val)
}

func (t *tidbTxn) Delete(key []byte) {
	t.exec(t.delQ, key)
}

func (t *tidbTxn) Append(ops ...Op) {
	for _, o := range ops {
		if o.Delete {
			t.exec(t.delQ, o.Key)
		} else {
			t.exec(t.putQ, o.Key, o.Value)
		}
	}
}

// Scan reads the prefix range inside the SQL transaction: the engine serves
// read-your-writes natively, and the reads join the transaction read set
// that commit-time conflict validation checks. Transaction-scope prefixes
// are bounded (document key ranges), so a single ordered query suffices.
func (t *tidbTxn) Scan(prefix []byte, fn func(k, v []byte) error) error {
	if t.err != nil {
		return t.err
	}
	q := fmt.Sprintf("SELECT k, v FROM `%s` WHERE k >= ?", t.table)
	args := []any{prefix}
	if len(prefix) > 0 {
		q += " AND k < ?"
		args = append(args, prefixEnd(prefix))
	}
	q += " ORDER BY k ASC"
	rows, err := t.tx.QueryContext(t.ctx, q, args...)
	if err != nil {
		t.err = err
		return err
	}
	defer rows.Close()
	type pair struct{ kb, vb []byte }
	var buf []pair
	for rows.Next() {
		var kb, vb []byte
		if err := rows.Scan(&kb, &vb); err != nil {
			t.err = err
			return err
		}
		buf = append(buf, pair{kb, vb})
	}
	if err := rows.Err(); err != nil {
		t.err = err
		return err
	}
	for _, p := range buf {
		if err := fn(p.kb, p.vb); err != nil {
			return err
		}
	}
	return nil
}

func (t *tidbTxn) exec(q string, args ...any) {
	if t.err != nil {
		return
	}
	if _, err := t.tx.ExecContext(t.ctx, q, args...); err != nil {
		t.err = err
	}
}

// Close closes the connection pool; it is safe to call more than once.
func (k *TiDBKV) Close() error {
	var err error
	k.once.Do(func() { err = k.db.Close() })
	return err
}

var (
	_ KV    = (*TiDBKV)(nil)
	_ TxnKV = (*TiDBKV)(nil)
)
