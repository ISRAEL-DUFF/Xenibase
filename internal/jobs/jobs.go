// Package jobs runs Xenibase's long actions as operations: rows in the
// operations table that a pool of workers claims with FOR UPDATE SKIP
// LOCKED, runs with retries and a step log, and streams to clients over
// Server-Sent Events (spec §6).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// Operation statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// IsTerminal reports whether status is final.
func IsTerminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed
}

// Log levels for step log entries.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// LogEntry is one line of an operation's step log.
type LogEntry struct {
	TS    time.Time `json:"ts"`
	Step  string    `json:"step"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

// DecodeLog parses an operation's log column.
func DecodeLog(raw json.RawMessage) ([]LogEntry, error) {
	var entries []LogEntry
	if len(raw) == 0 {
		return entries, nil
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("decode operation log: %w", err)
	}
	return entries, nil
}

// Handler runs one attempt of an operation. It must be safe to run again
// after a failure or crash (operations are resumable): use op.Attempts and
// the existing step log to skip work that already completed.
type Handler func(ctx context.Context, op store.Operation, log *StepLogger) error

// Kind registers a handler for an operation kind.
type Kind struct {
	Handler Handler
	// MaxAttempts is the number of attempts before the operation fails.
	// Zero means DefaultMaxAttempts. An operation whose worker crashed on
	// the last attempt gets one more run, since the handler may have
	// finished before the crash; a second crash fails it unrun.
	MaxAttempts int
	// Timeout bounds a single attempt. Zero means DefaultTimeout.
	Timeout time.Duration
	// OnFail, if set, runs once when the operation fails for good (a
	// permanent error or the last attempt), before it is marked failed. It
	// undoes partial work (compensating actions); it must be idempotent,
	// since a crash during OnFail runs it again. Its error is logged, and
	// the operation is marked failed regardless.
	OnFail func(ctx context.Context, op store.Operation, log *StepLogger, cause error) error
}

// Defaults for Kind.
const (
	DefaultMaxAttempts = 3
	DefaultTimeout     = 30 * time.Minute
)

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as not worth retrying; the operation fails at once.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// ErrNotFound is returned when an operation does not exist.
var ErrNotFound = errors.New("operation not found")

// EnqueueParams describes a new operation.
type EnqueueParams struct {
	Kind      string
	ProjectID *uuid.UUID
	Params    any // marshalled to JSON; nil means {}
	CreatedBy *uuid.UUID
}

// Enqueue inserts a queued operation. The table trigger wakes idle workers.
func Enqueue(ctx context.Context, db store.DBTX, p EnqueueParams) (store.Operation, error) {
	params := json.RawMessage(`{}`)
	if p.Params != nil {
		b, err := json.Marshal(p.Params)
		if err != nil {
			return store.Operation{}, fmt.Errorf("marshal params: %w", err)
		}
		params = b
	}
	return store.New(db).EnqueueOperation(ctx, store.EnqueueOperationParams{
		Kind:      p.Kind,
		ProjectID: p.ProjectID,
		Params:    params,
		CreatedBy: p.CreatedBy,
	})
}

// Get returns an operation by ID, or ErrNotFound.
func Get(ctx context.Context, db store.DBTX, id uuid.UUID) (store.Operation, error) {
	op, err := store.New(db).GetOperation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return op, ErrNotFound
	}
	return op, err
}
