package edge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/datacat"
)

// The data API's view of a project's schema (V4 §3.1): the tables, views
// and their columns, keys and foreign keys in the exposed schemas, read
// from the catalog. It is cached per project and re-read when a cheap
// fingerprint of the catalog changes (checked at most every
// fingerprintEvery, or Config.CatalogEvery, inside a request's
// transaction).
const fingerprintEvery = 2 * time.Second

// catalogState is a project's cached catalog.
type catalogState struct {
	mu      sync.Mutex
	cat     *Catalog
	checked time.Time
	// public are the public tables cat.Reach was worked out with.
	public []string
	// fp is the last fingerprint read, kept when cat is dropped.
	fp string
}

// catalog returns p's catalog, re-reading it in tx when it may be stale.
// When it changed, the project's database first takes back from anon and
// user what nothing protects (syncGrants), and the catalog records what
// each of them can still reach that way (Catalog.Reach).
func (e *Edge) catalog(ctx context.Context, p *project, tx pgx.Tx) (*Catalog, *catalogState, error) {
	st := p.catalog
	st.mu.Lock()
	defer st.mu.Unlock()
	schemas := p.exposed()
	every := e.cfg.CatalogEvery
	if every <= 0 {
		every = fingerprintEvery
	}
	same := st.cat != nil && sameStrings(st.cat.Schemas, schemas) && sameStrings(st.public, p.cfg.PublicTables)
	if same && time.Since(st.checked) < every {
		return st.cat, st, nil
	}
	var fp string
	if err := tx.QueryRow(ctx, datacat.FingerprintSQL, schemas).Scan(&fp); err != nil {
		return nil, nil, err
	}
	st.checked = time.Now()
	if same && st.cat.Fingerprint == fp {
		return st.cat, st, nil
	}
	if st.fp != "" && st.fp != fp {
		// A policy, a grant or the schema changed: no cached answer may
		// outlive it.
		e.cache.dropTables(p.cfg.Ref, nil)
	}
	if e.syncGrants(ctx, p) {
		// The grants changed, and with them the fingerprint.
		if err := tx.QueryRow(ctx, datacat.FingerprintSQL, schemas).Scan(&fp); err != nil {
			return nil, nil, err
		}
	}
	cat, err := datacat.Introspect(ctx, tx, schemas)
	if err != nil {
		return nil, nil, err
	}
	cat.Fingerprint, st.fp = fp, fp
	cat.Reach = map[string][]string{}
	for role, name := range map[string]string{"anon": p.cfg.AnonRole, "user": p.cfg.UserRole} {
		if name == "" {
			continue
		}
		if cat.Reach[role], err = datacat.Reach(ctx, tx, name, p.cfg.PublicTables); err != nil {
			return nil, nil, err
		}
	}
	st.cat, st.public = cat, append([]string(nil), p.cfg.PublicTables...)
	return cat, st, nil
}

// syncGrants runs xb_auth.sync_grants as the edge login, in a transaction
// of its own on the primary: anon and user keep privileges only on what
// protects itself and SELECT on the public tables, and get back what it
// took once a table has row-level security again. It reports whether
// anything changed. A project behind schema version 7 has no such
// function; any failure is logged, and the gates still refuse what isn't
// protected.
func (e *Edge) syncGrants(ctx context.Context, p *project) bool {
	if p.cfg.EdgeUser == "" {
		return false
	}
	pool, err := e.loginPool(ctx, p, "", false)
	if err == nil {
		public := p.cfg.PublicTables
		if public == nil {
			public = []string{}
		}
		var n int
		if err = pool.QueryRow(ctx, `SELECT xb_auth.sync_grants($1)`, public).Scan(&n); err == nil {
			return n > 0
		}
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "42883" || pe.Code == "3F000") {
		return false
	}
	e.cfg.Log.Warn("edge: sync the request roles' grants", "project", p.cfg.Ref, "err", err)
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
