package tenancy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/israel-duff/xenibase/internal/store"
)

// How long tenants' data is kept in the metadata database
// (docs/operations.md, "What is kept").
const (
	// ReapedSessionRetention is how long the reaper's records (with the
	// statement's text) are kept, as long as query insights keep theirs;
	// a deleted project's go at the next sweep.
	ReapedSessionRetention = 30 * 24 * time.Hour
	// OperationRetention is how long a finished operation keeps its
	// parameters and step log; the row stays as a summary.
	OperationRetention = 90 * 24 * time.Hour
	// operationPruneBatch bounds one sweep's work: each pruned row is a
	// rewrite and a notification.
	operationPruneBatch = 500
)

// PruneRetention removes what is past its retention.
func (s *Service) PruneRetention(ctx context.Context) error {
	q := store.New(s.db)
	now := s.Now()
	var errs []error
	if n, err := q.PruneReapedSessions(ctx, now.Add(-ReapedSessionRetention)); err != nil {
		errs = append(errs, fmt.Errorf("reaped sessions: %w", err))
	} else if n > 0 {
		s.log.Info("pruned reaped sessions", "rows", n)
	}
	if n, err := q.PruneFinishedOperations(ctx, store.PruneFinishedOperationsParams{
		Before: now.Add(-OperationRetention), MaxRows: operationPruneBatch,
	}); err != nil {
		errs = append(errs, fmt.Errorf("operations: %w", err))
	} else if n > 0 {
		s.log.Info("pruned finished operations' parameters and logs", "rows", n)
	}
	return errors.Join(errs...)
}
