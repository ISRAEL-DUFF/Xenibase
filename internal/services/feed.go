package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/files"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// feedPage bounds a page of the configuration feed.
const feedPage = 500

// Config is the configuration feed for xenibase-edge (V4 §2.1): projects
// changed after since, waiting up to wait for one. A project in a region
// the edge may not see (allow, from EdgeScope.Feed) comes as disabled, so
// an edge that served it before a move drops it.
func (s *Service) Config(ctx context.Context, allow func(region string) bool, since int64, wait time.Duration) (edgeapi.Config, error) {
	q := store.New(s.db)
	deadline := time.Now().Add(wait)
	for {
		rows, err := q.EdgeConfigChanges(ctx, store.EdgeConfigChangesParams{Since: since, Lim: feedPage})
		if err != nil {
			return edgeapi.Config{}, err
		}
		if len(rows) > 0 || !time.Now().Before(deadline) {
			return s.page(ctx, allow, since, rows)
		}
		select {
		case <-ctx.Done():
			return edgeapi.Config{Next: since}, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Service) page(ctx context.Context, allow func(region string) bool, since int64, rows []store.EdgeConfigChangesRow) (edgeapi.Config, error) {
	out := edgeapi.Config{Projects: []edgeapi.Project{}, Next: since, More: len(rows) == feedPage}
	var live []uuid.UUID
	for _, r := range rows {
		if state(r) == edgeapi.StateActive || state(r) == edgeapi.StatePaused || state(r) == edgeapi.StateSuspended {
			live = append(live, r.ProjectID)
		}
	}
	q := store.New(s.db)
	keys := map[uuid.UUID][]edgeapi.Key{}
	jwks := map[uuid.UUID][]json.RawMessage{}
	signing := map[uuid.UUID]*edgeapi.SigningKey{}
	auth := map[uuid.UUID]edgeapi.AuthConfig{}
	if len(live) > 0 {
		ks, err := q.EdgeAPIKeys(ctx, live)
		if err != nil {
			return out, err
		}
		for _, k := range ks {
			keys[k.ProjectID] = append(keys[k.ProjectID], edgeapi.Key{ID: k.ID, Kind: k.Kind, Hash: k.KeyHash})
		}
		js, err := q.EdgeJWTKeys(ctx, live)
		if err != nil {
			return out, err
		}
		for _, j := range js {
			jwks[j.ProjectID] = append(jwks[j.ProjectID], j.PublicJwk)
		}
		ss, err := q.EdgeSigningKeys(ctx, live)
		if err != nil {
			return out, err
		}
		for _, k := range ss {
			der, err := s.keyring.Decrypt(k.PrivateEnc, jwtAAD(k.ID))
			if err != nil {
				return out, fmt.Errorf("signing key %s: %w", k.Kid, err)
			}
			signing[k.ProjectID] = &edgeapi.SigningKey{Kid: k.Kid, Private: der}
		}
		as, err := q.EdgeAuthConfigs(ctx, live)
		if err != nil {
			return out, err
		}
		for _, a := range as {
			var st AuthSettings
			if err := json.Unmarshal(a.Config, &st); err != nil {
				return out, fmt.Errorf("auth settings: %w", err)
			}
			var prov sealedProviders
			if len(a.ProvidersEnc) > 0 {
				raw, err := s.keyring.Decrypt(a.ProvidersEnc, smtpAAD(a.ProjectID))
				if err != nil {
					return out, fmt.Errorf("auth providers: %w", err)
				}
				if err := json.Unmarshal(raw, &prov); err != nil {
					return out, fmt.Errorf("auth providers: %w", err)
				}
			}
			auth[a.ProjectID] = edgeAuth(st, prov, s.cfg.CaptchaVerifyURL)
		}
	}
	targets := map[[2]any]*storage.Target{}
	for _, r := range rows {
		out.Next = r.ChangedSeq
		p := edgeapi.Project{Ref: r.Ref, ProjectID: r.ProjectID, OrgID: r.OrgID, Region: r.Region, State: state(r),
			Version: r.ConfigVersion, Seq: r.ChangedSeq}
		if !allow(r.Region) {
			p.State = edgeapi.StateDisabled
		}
		if p.State == edgeapi.StateDisabled {
			out.Projects = append(out.Projects, p)
			continue
		}
		st, err := DecodeSettings(r.Settings)
		if err != nil {
			return out, fmt.Errorf("%s settings: %w", r.Ref, err)
		}
		host, port := "", 0
		if addr := s.projects.PooledAddr(r.Region); addr != "" {
			h, ps, err := net.SplitHostPort(addr)
			if err == nil {
				host = h
				port, _ = strconv.Atoi(ps)
			}
		}
		edge := store.EdgeRole(r.DbName)
		p.Database, p.EdgeUser, p.Password = r.DbName, edge, s.edgePassword(edge)
		p.PoolerHost, p.PoolerPort = host, port
		if addr := s.projects.SessionAddr(r.Region); addr != "" {
			if h, ps, err := net.SplitHostPort(addr); err == nil {
				p.SessionHost = h
				p.SessionPort, _ = strconv.Atoi(ps)
			}
		}
		p.AnonRole, p.UserRole, p.ServiceRole = store.AnonRole(r.DbName), store.UserRole(r.DbName), store.ServiceRole(r.DbName)
		p.HookRole = store.AuthHookRole(r.DbName)
		p.Logins = map[string]string{}
		for _, role := range store.RequestRoles(r.DbName) {
			p.Logins[role] = s.edgePassword(role)
		}
		p.Keys, p.JWKs, p.CORSOrigins = keys[r.ProjectID], jwks[r.ProjectID], r.CorsOrigins
		p.ExposedSchemas, p.PublicTables = r.ExposedSchemas, r.PublicTables
		p.SigningKey = signing[r.ProjectID]
		if a, ok := auth[r.ProjectID]; ok {
			p.Auth = a
		} else {
			p.Auth = AuthSettings{}.Resolve()
		}
		timeout, perIP, perKey := EffectiveSettings(st, PlanCeilings{TimeoutMs: r.PlanTimeoutMs, RatePerIP: r.PlanRatePerIp, RatePerKey: r.PlanRatePerKey})
		p.Settings = edgeapi.Settings{StatementTimeoutMs: timeout, RatePerIP: perIP, RatePerKey: perKey,
			AllowSecretInBrowser: st.AllowSecretInBrowser, MaxQueryCost: float64(or(st.MaxQueryCost, DefaultMaxQueryCost)),
			ReplicaReads: st.ReplicaReads, CacheTTLSeconds: st.CacheTTLSeconds}
		p.RequestsBlocked, p.MAUBlocked = r.ApiRequestsBlocked, r.MauBlocked
		if r.MauBlocked {
			p.MAUCounted = r.MauCounted
		}
		if r.HasReplicas {
			p.ReadDatabase = r.DbName + pooler.ReadOnlySuffix
		}
		p.SpendCapped = r.SpendCapped
		p.Storage = s.storageConfig(ctx, r, targets)
		p.Realtime = edgeapi.RealtimeConfig{MessagesBlocked: r.RealtimeMessagesBlocked,
			MaxChangesPerSecond: DefaultRealtimeChangesPerSecond, MaxGroups: DefaultRealtimeGroups}
		if r.RealtimeMaxConnections != nil {
			p.Realtime.MaxConnections = int(*r.RealtimeMaxConnections)
		}
		out.Projects = append(out.Projects, p)
	}
	return out, nil
}

// DefaultUploadMax is the largest object without a plan limit (V4 §5.3).
const DefaultUploadMax = 5 << 30

// storageConfig is a project's storage for the edge, nil when its region has
// no object store; targets caches the resolution per region within a page.
func (s *Service) storageConfig(ctx context.Context, r store.EdgeConfigChangesRow, targets map[[2]any]*storage.Target) *edgeapi.StorageConfig {
	if s.Files == nil {
		return nil
	}
	k := [2]any{r.Region, r.DataResidency}
	t, ok := targets[k]
	if !ok {
		tg, err := s.Files(ctx, r.Region, r.DataResidency)
		if err != nil {
			s.log.Warn("backend services file storage", "region", r.Region, "err", err)
		} else {
			t = &tg
		}
		targets[k] = t
	}
	if t == nil {
		return nil
	}
	c := &edgeapi.StorageConfig{Target: *t, Prefix: FilesPrefix(r.Ref), SigningSecret: s.storageSecret(r.Ref),
		UploadMaxBytes: DefaultUploadMax, EgressBlocked: r.StorageEgressBlocked, TransformsBlocked: r.TransformsBlocked}
	if r.StorageQuotaBytes != nil {
		c.QuotaBytes = max(*r.StorageQuotaBytes, 1)
	}
	if r.UploadMaxBytes != nil {
		c.UploadMaxBytes = *r.UploadMaxBytes
	}
	return c
}

// FilesPrefix is where a project's files are in its region's object store.
func FilesPrefix(ref string) string { return files.Prefix(ref) }

// storageSecret signs a project's file URLs.
func (s *Service) storageSecret(ref string) []byte {
	return s.keyring.Derive("xenibase storage urls "+ref, 32)
}

func or(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

// state is how the edge treats a project.
func state(r store.EdgeConfigChangesRow) string {
	switch {
	case !r.Enabled || !r.EdgeReady || r.DeletedAt != nil:
		return edgeapi.StateDisabled
	case r.Status == "deleting" || r.Status == "deleted" || r.Status == "failed" || r.Status == "provisioning":
		return edgeapi.StateDisabled
	case r.OrgStatus != "active":
		return edgeapi.StateSuspended
	case r.Lifecycle != "active":
		return edgeapi.StatePaused
	}
	return edgeapi.StateActive
}

// Report records an edge's usage, request logs and keys in use, once per
// batch.
func (s *Service) Report(ctx context.Context, r edgeapi.Report) error {
	if r.BatchID == "" || len(r.BatchID) > 100 {
		return fmt.Errorf("%w: a report needs a batch id", ErrInvalid)
	}
	if r.Edge != "" && len(r.Edge) <= 200 && len(r.Region) <= 64 {
		if err := store.New(s.db).TouchEdge(ctx, store.TouchEdgeParams{Name: r.Edge, Region: r.Region}); err != nil {
			return err
		}
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.InsertEdgeReport(ctx, store.InsertEdgeReportParams{BatchID: r.BatchID, Edge: r.Edge})
		if err != nil || n == 0 {
			return err // n == 0: already recorded
		}
		if r.CPUPercent != nil && r.Edge != "" && len(r.Edge) <= 200 && len(r.Region) <= 64 {
			if err := q.InsertEdgeCPUSample(ctx, store.InsertEdgeCPUSampleParams{Edge: r.Edge, Region: r.Region,
				CpuPercent: float32(min(max(*r.CPUPercent, 0), 100))}); err != nil {
				return err
			}
		}
		ids := map[uuid.UUID]bool{}
		for _, u := range r.Usage {
			ids[u.ProjectID] = true
		}
		wanted := map[uuid.UUID]bool{}
		for id := range ids {
			wanted[id] = true
		}
		for _, a := range r.ActiveUsers {
			wanted[a.ProjectID] = true
		}
		owners := map[uuid.UUID]store.ProjectUsageOwnerRow{}
		if len(wanted) > 0 {
			list := make([]uuid.UUID, 0, len(wanted))
			for id := range wanted {
				list = append(list, id)
			}
			rows, err := q.ProjectUsageOwner(ctx, list)
			if err != nil {
				return err
			}
			for _, o := range rows {
				owners[o.ID] = o
			}
		}
		for _, u := range r.Usage {
			o, ok := owners[u.ProjectID]
			if !ok {
				continue // deleted since
			}
			hour := u.Hour.UTC().Truncate(time.Hour)
			if u.Requests > 0 {
				if err := q.AddUsage(ctx, store.AddUsageParams{OrgID: o.OrgID, ProjectID: o.ID, Metric: tenancy.MetricAPIRequests,
					PeriodStart: hour, Quantity: intNumeric(u.Requests), PlanID: o.PlanID}); err != nil {
					return err
				}
			}
			if u.EgressBytes > 0 {
				if err := q.AddUsage(ctx, store.AddUsageParams{OrgID: o.OrgID, ProjectID: o.ID, Metric: tenancy.MetricAPIEgress,
					PeriodStart: hour, Quantity: gbNumeric(u.EgressBytes), PlanID: o.PlanID}); err != nil {
					return err
				}
			}
			if u.StorageEgressBytes > 0 {
				if err := q.AddUsage(ctx, store.AddUsageParams{OrgID: o.OrgID, ProjectID: o.ID, Metric: tenancy.MetricStorageEgress,
					PeriodStart: hour, Quantity: gbNumeric(u.StorageEgressBytes), PlanID: o.PlanID}); err != nil {
					return err
				}
			}
			if u.RealtimeConnectionSeconds > 0 {
				if err := q.AddUsage(ctx, store.AddUsageParams{OrgID: o.OrgID, ProjectID: o.ID, Metric: tenancy.MetricRealtimeConnMinutes,
					PeriodStart: hour, Quantity: minutesNumeric(u.RealtimeConnectionSeconds), PlanID: o.PlanID}); err != nil {
					return err
				}
			}
			if u.RealtimeMessages > 0 {
				if err := q.AddUsage(ctx, store.AddUsageParams{OrgID: o.OrgID, ProjectID: o.ID, Metric: tenancy.MetricRealtimeMessages,
					PeriodStart: hour, Quantity: intNumeric(u.RealtimeMessages), PlanID: o.PlanID}); err != nil {
					return err
				}
			}
			if u.Transforms > 0 {
				if err := q.AddUsage(ctx, store.AddUsageParams{OrgID: o.OrgID, ProjectID: o.ID, Metric: tenancy.MetricImageTransforms,
					PeriodStart: hour, Quantity: intNumeric(u.Transforms), PlanID: o.PlanID}); err != nil {
					return err
				}
			}
		}
		if len(r.Logs) > 0 {
			logs := make([]store.InsertRequestLogsParams, 0, len(r.Logs))
			for _, l := range r.Logs {
				if !ids[l.ProjectID] {
					// Logs come with usage for the same project; anything else
					// is not ours to record.
					continue
				}
				logs = append(logs, store.InsertRequestLogsParams{ProjectID: l.ProjectID, At: l.At, RequestID: trunc(l.RequestID, 64),
					Method: trunc(l.Method, 10), Path: trunc(l.Path, 500), Status: int32(l.Status), LatencyMs: int32(l.LatencyMs),
					Role: strPtr(l.Role), UserID: l.UserID, KeyID: l.KeyID, Ip: strPtr(trunc(l.IP, 64)), BytesOut: l.BytesOut})
			}
			if _, err := q.InsertRequestLogs(ctx, logs); err != nil {
				return err
			}
		}
		if err := recordActive(ctx, q, r.ActiveUsers, owners); err != nil {
			return err
		}
		if len(r.KeysUsed) > 0 {
			at := r.At
			if at.IsZero() || at.After(time.Now()) {
				at = time.Now()
			}
			if err := q.TouchAPIKeys(ctx, store.TouchAPIKeysParams{At: at, Ids: r.KeysUsed}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Wake resumes the paused project ref names.
func (s *Service) Wake(ctx context.Context, ref string) error {
	svc, err := store.New(s.db).ProjectServicesByRef(ctx, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if s.Waker == nil {
		return nil
	}
	return s.Waker(ctx, svc.ProjectID)
}

func intNumeric(n int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(n), Valid: true}
}

// gbNumeric is bytes as GB (10^9) with nine decimal places.
func gbNumeric(bytes int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(bytes), Exp: -9, Valid: true}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// minutesNumeric is seconds as minutes, to four decimal places.
func minutesNumeric(seconds int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(seconds * 10000 / 60), Exp: -4, Valid: true}
}
