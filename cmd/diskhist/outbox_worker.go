package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"diskhist-monitor/internal/config"
	"diskhist-monitor/internal/report"
	"diskhist-monitor/internal/storage"
)

func resolveSecret(ref config.SecretRef) (string, error) {
	switch ref.Scheme {
	case "env":
		value := os.Getenv(ref.Value)
		if value == "" {
			return "", fmt.Errorf("environment variable %q is empty", ref.Value)
		}
		return value, nil
	case "file":
		value, err := os.ReadFile(ref.Value)
		if err != nil {
			return "", fmt.Errorf("read secret file %q: %w", ref.Value, err)
		}
		secret := strings.TrimSpace(string(value))
		if secret == "" {
			return "", fmt.Errorf("secret file %q is empty", ref.Value)
		}
		return secret, nil
	default:
		return "", fmt.Errorf("unsupported secret reference scheme %q", ref.Scheme)
	}
}

type outboxWorker struct {
	db          *sql.DB
	client      *report.Client
	poll        time.Duration
	maxAttempts int
	stop        chan struct{}
	once        sync.Once
}

func newReportWorker(db *sql.DB, client *report.Client, poll time.Duration, maxAttempts int) *outboxWorker {
	if poll <= 0 {
		poll = time.Second
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &outboxWorker{db: db, client: client, poll: poll, maxAttempts: maxAttempts, stop: make(chan struct{})}
}
func (w *outboxWorker) Stop() { w.once.Do(func() { close(w.stop) }) }
func (w *outboxWorker) Run(ctx context.Context) {
	for {
		err := w.processOne(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, context.Canceled) {
			log.Printf("[WARN] report outbox worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-time.After(w.poll):
		}
	}
}
func (w *outboxWorker) processOne(ctx context.Context) error {
	item, err := storage.ClaimOutbox(w.db, time.Now().UTC())
	if err != nil {
		return err
	}
	if item.Attempts >= w.maxAttempts {
		return storage.MarkOutboxPending(w.db, item.ID, "dead-letter: retry limit reached", time.Now().UTC())
	}
	payload, err := report.LoadScanByID(ctx, w.db, item.ScanID)
	if err == nil {
		payload.IdempotencyKey = item.IdempotencyKey
		_, err = w.client.Push(ctx, payload)
	}
	if err == nil {
		return storage.MarkOutboxSent(w.db, item.ID, time.Now().UTC())
	}
	if markErr := storage.MarkOutboxPending(w.db, item.ID, boundedReportError(err, item.Attempts >= w.maxAttempts), time.Now().UTC()); markErr != nil {
		return fmt.Errorf("report item %d failed: %v; reset pending: %w", item.ID, err, markErr)
	}
	if item.Attempts >= w.maxAttempts {
		log.Printf("[ERROR] report outbox item %d dead-lettered after %d attempts: %v", item.ID, item.Attempts, err)
	}
	return err
}
func boundedReportError(err error, dead bool) string {
	if dead {
		return "dead-letter: " + err.Error()
	}
	return err.Error()
}
