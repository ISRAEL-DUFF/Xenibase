package freetier

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Swept counts what one sweep did.
type Swept struct {
	Warned, Paused, Archived, Noticed, Deleted, Resumed int
	// WakerDown: the waker didn't answer, so nothing was paused or
	// archived (its clients could never wake it).
	WakerDown bool
}

// WakerReachable reports whether the waker accepts connections at addr.
func WakerReachable(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Run sweeps every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("free tier sweep", "err", err)
		}
		t.Reset(interval)
	}
}

// Sweep does what is due (V3 §4): projects of organisations that now pay
// wake; idle Free projects are warned, then paused; long-paused ones are
// archived; long-archived ones get their notices, then are deleted. Safe
// on several servers at once: operations are one per project, and each
// notice is claimed by a conditional update before it is sent.
func (s *Service) Sweep(ctx context.Context) (Swept, error) {
	var out Swept
	q := store.New(s.db)
	now := s.now()
	var errs []error
	try := func(err error) {
		if err != nil && !errors.Is(err, ErrConflict) {
			errs = append(errs, err)
		}
	}

	// Paid plans are never paused or archived (V3 §4.3).
	paid, err := q.SleepingPaidProjects(ctx)
	try(err)
	for _, p := range paid {
		if _, err := s.Resume(ctx, p.ID, nil); err == nil {
			out.Resumed++
		} else {
			try(err)
		}
	}

	// Without a waker, a paused project could only fail its clients; with
	// one that is down, its clients couldn't wake it (M27 chaos test), so
	// pausing and archiving wait until it is back.
	pm := s.projects.Pooler()
	wakerUp := pm != nil && pm.WakerSet() && WakerReachable(ctx, pm.WakerAddr())
	if pm != nil && pm.WakerSet() && !wakerUp {
		out.WakerDown = true
		s.log.Warn("free tier: the waker is unreachable; not pausing or archiving", "waker", pm.WakerAddr())
	}
	if wakerUp {
		idle, err := q.FreeProjectsIdleSince(ctx, ptr(now.Add(-(s.cfg.PauseAfter - s.cfg.Warn))))
		try(err)
		for _, p := range idle {
			switch {
			case p.PauseWarnedAt == nil:
				if n, err := q.MarkPauseWarned(ctx, p.ID); err != nil || n == 0 {
					try(err)
					continue
				}
				s.notify(ctx, p, fmt.Sprintf("%s will be paused in %s", p.Name, hours(s.cfg.Warn)), mail.Content{
					Preheader: "Connect to it before then and it stays active.",
					Title:     fmt.Sprintf("%s will be paused soon", p.Name),
					Blocks: []mail.Block{
						mail.P("Hello,"),
						mail.P(fmt.Sprintf("%s hasn't had a client connection since %s. Free projects are paused after %d days without one, so it will be paused in about %s.",
							mail.Inert(p.Name), lastActive(p).UTC().Format("2 January 2006"), days(s.cfg.PauseAfter), hours(s.cfg.Warn))),
						mail.P("Connecting to it before then keeps it active. And if it does pause, nothing is lost: it keeps its data and wakes up on the next connection."),
						mail.Button("Open the project", s.projectLink(p)),
					},
					SignOff: mail.SignOff,
				})
				out.Warned++
			case now.Sub(lastActive(p)) >= s.cfg.PauseAfter && now.Sub(*p.PauseWarnedAt) >= s.cfg.Warn:
				if _, err := s.enqueue(ctx, p.ID, nil, func(store.Project) (string, error) { return KindPause, nil }); err == nil {
					out.Paused++
				} else {
					try(err)
				}
			}
		}
	}

	if _, _, err := s.backups.StorageTarget(ctx); err == nil && !out.WakerDown {
		long, err := q.FreeProjectsPausedSince(ctx, ptr(now.Add(-s.cfg.ArchiveAfter)))
		try(err)
		for _, p := range long {
			if _, err := s.enqueue(ctx, p.ID, nil, func(store.Project) (string, error) { return KindArchive, nil }); err == nil {
				out.Archived++
			} else {
				try(err)
			}
		}
	}

	archived, err := q.ArchivedProjects(ctx)
	try(err)
	for _, p := range archived {
		n, d, err := s.sweepArchived(ctx, p, now)
		try(err)
		out.Noticed += n
		out.Deleted += d
	}
	return out, errors.Join(errs...)
}

// sweepArchived sends an archived project's deletion notices and deletes
// it when due.
func (s *Service) sweepArchived(ctx context.Context, p store.Project, now time.Time) (noticed, deleted int, err error) {
	if p.ArchivedAt == nil {
		return 0, 0, nil
	}
	due := p.ArchivedAt.Add(s.cfg.DeleteAfter)
	last := slices.Min(s.cfg.NoticeDays)
	// The most urgent notice due, once: a late sweep skips the earlier one.
	for _, d := range slices.Sorted(slices.Values(s.cfg.NoticeDays)) {
		sent := p.ArchiveNoticeDays != nil && int(*p.ArchiveNoticeDays) <= d
		if sent || now.Before(due.AddDate(0, 0, -d)) {
			continue
		}
		if n, err := store.New(s.db).SetArchiveNotice(ctx, store.SetArchiveNoticeParams{ID: p.ID, Days: ptr(int32(d))}); err != nil || n == 0 {
			return noticed, 0, err // another server sent it
		}
		s.notify(ctx, p, fmt.Sprintf("%s will be deleted in %d days", p.Name, d), mail.Content{
			Preheader: "Restore it before then and it stays.",
			Title:     fmt.Sprintf("%s will be deleted in %d days", p.Name, d),
			Blocks: []mail.Block{
				mail.P("Hello,"),
				mail.P(fmt.Sprintf("%s has been archived since %s. Archived Free projects are deleted after %d days, so it will be deleted on %s.",
					mail.Inert(p.Name), p.ArchivedAt.UTC().Format("2 January 2006"), days(s.cfg.DeleteAfter), due.UTC().Format("2 January 2006"))),
				mail.P("If you still want it, connect to it or restore it from the dashboard before then, and it stays."),
				mail.Button("Restore the project", s.projectLink(p)),
			},
			SignOff: mail.SignOff,
		})
		p.ArchiveNoticeDays = ptr(int32(d))
		noticed++
		break
	}
	if now.Before(due) || p.ArchiveNoticeDays == nil || int(*p.ArchiveNoticeDays) > last {
		return noticed, 0, nil
	}
	// Its database is gone already: no final backup to take.
	if _, err := s.projects.Delete(ctx, p.ID, p.Name, true, nil); err != nil {
		if errors.Is(err, provision.ErrConflict) {
			s.log.Info("archived project not deleted yet", "project_id", p.ID, "reason", err)
			return noticed, 0, nil
		}
		return noticed, 0, err
	}
	if p.ArchiveBackupID != nil {
		if err := store.New(s.db).ExpireBackupAt(ctx, store.ExpireBackupAtParams{ID: *p.ArchiveBackupID, ExpiresAt: ptr(now.Add(30 * 24 * time.Hour))}); err != nil {
			return noticed, 1, err
		}
	}
	return noticed, 1, nil
}

func hours(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", days(d))
	}
	return fmt.Sprintf("%d hours", int(d.Round(time.Hour).Hours()))
}
