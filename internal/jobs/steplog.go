package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
)

// ErrLeaseLost means another worker reclaimed the operation; the current
// attempt must stop without writing further.
var ErrLeaseLost = errors.New("operation lease lost")

// StepLogger appends entries to an operation's step log. Writes only
// succeed while this attempt still holds the operation.
type StepLogger struct {
	q       *store.Queries
	id      uuid.UUID
	worker  string
	attempt int32
	slog    *slog.Logger
}

// Info appends an info entry.
func (l *StepLogger) Info(ctx context.Context, step, format string, args ...any) error {
	return l.write(ctx, LevelInfo, step, fmt.Sprintf(format, args...))
}

// Warn appends a warning entry.
func (l *StepLogger) Warn(ctx context.Context, step, format string, args ...any) error {
	return l.write(ctx, LevelWarn, step, fmt.Sprintf(format, args...))
}

// Error appends an error entry.
func (l *StepLogger) Error(ctx context.Context, step, format string, args ...any) error {
	return l.write(ctx, LevelError, step, fmt.Sprintf(format, args...))
}

func (l *StepLogger) write(ctx context.Context, level, step, msg string) error {
	if l == nil {
		return nil // a step run outside an operation (e.g. the isolation check's probes)
	}
	entry, err := json.Marshal(LogEntry{TS: time.Now().UTC(), Step: step, Level: level, Msg: msg})
	if err != nil {
		return err
	}
	n, err := l.q.AppendOperationLog(ctx, store.AppendOperationLogParams{ID: l.id, Worker: l.worker, Attempt: l.attempt, Entry: entry})
	if err != nil {
		return fmt.Errorf("append operation log: %w", err)
	}
	if n == 0 {
		return ErrLeaseLost
	}
	l.slog.LogAttrs(ctx, slogLevel(level), msg,
		slog.String("operation_id", l.id.String()), slog.String("step", step))
	return nil
}

func slogLevel(level string) slog.Level {
	switch level {
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
