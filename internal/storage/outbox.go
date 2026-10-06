package storage

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

type OutboxItem struct {
	ID             int64
	AgentID, Drive string
	ScanID         int64
	IdempotencyKey string
	Status         string
	Attempts       int
	LeaseUntil     time.Time
}

const outboxLease = 2 * time.Minute

func OutboxKey(agentID, drive string, scanID int64) string {
	return "diskhist:" + agentID + ":" + drive + ":" + strconv.FormatInt(scanID, 10)
}

func EnqueueScanOutbox(db *sql.DB, agentID, drive string, scanID int64) error {
	return enqueueScanOutbox(db, agentID, drive, scanID, time.Now().UTC())
}

func EnqueueScanOutboxTx(tx *sql.Tx, agentID, drive string, scanID int64, now time.Time) error {
	return enqueueScanOutbox(tx, agentID, drive, scanID, now)
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func enqueueScanOutbox(e execer, agentID, drive string, scanID int64, now time.Time) error {
	if agentID == "" {
		agentID = "unknown"
	}
	_, err := e.Exec(`INSERT INTO scan_outbox(agent_id,drive_letter,scan_id,idempotency_key,status,attempts,created_at)
		VALUES(?,?,?,?, 'pending', 0, ?) ON CONFLICT(idempotency_key) DO NOTHING`, agentID, drive, scanID, OutboxKey(agentID, drive, scanID), now)
	if err != nil {
		return fmt.Errorf("enqueue scan %d: %w", scanID, err)
	}
	return nil
}

func ClaimOutbox(db *sql.DB, now time.Time) (OutboxItem, error) {
	tx, err := db.Begin()
	if err != nil {
		return OutboxItem{}, err
	}
	defer tx.Rollback()
	var item OutboxItem
	var lease sql.NullTime
	err = tx.QueryRow(`SELECT id,agent_id,drive_letter,scan_id,idempotency_key,status,attempts,lease_until FROM scan_outbox
		WHERE status='pending' OR (status='sending' AND (lease_until IS NULL OR lease_until <= ?)) ORDER BY created_at,id LIMIT 1`, now).Scan(&item.ID, &item.AgentID, &item.Drive, &item.ScanID, &item.IdempotencyKey, &item.Status, &item.Attempts, &lease)
	if err != nil {
		return OutboxItem{}, err
	}
	if lease.Valid {
		item.LeaseUntil = lease.Time
	}
	leaseUntil := now.Add(outboxLease)
	res, err := tx.Exec(`UPDATE scan_outbox SET status='sending', attempts=attempts+1, lease_until=?, updated_at=? WHERE id=? AND (status='pending' OR (status='sending' AND (lease_until IS NULL OR lease_until <= ?)))`, leaseUntil, now, item.ID, now)
	if err != nil {
		return OutboxItem{}, err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return OutboxItem{}, sql.ErrNoRows
	}
	item.Status = "sending"
	item.Attempts++
	item.LeaseUntil = leaseUntil
	return item, tx.Commit()
}

func MarkOutboxSent(db *sql.DB, id int64, now time.Time) error {
	_, err := db.Exec(`UPDATE scan_outbox SET status='sent', lease_until=NULL, sent_at=?, updated_at=? WHERE id=?`, now, now, id)
	return err
}

func MarkOutboxPending(db *sql.DB, id int64, message string, now time.Time) error {
	_, err := db.Exec(`UPDATE scan_outbox SET status='pending', lease_until=NULL, last_error=?, updated_at=? WHERE id=?`, message, now, id)
	return err
}

func MarkOutboxDeadLetter(db *sql.DB, id int64, message string, now time.Time) error {
	_, err := db.Exec(`UPDATE scan_outbox SET status='dead_letter', lease_until=NULL, last_error=?, updated_at=? WHERE id=?`, message, now, id)
	return err
}
