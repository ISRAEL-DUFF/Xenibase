package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/faildomain"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
)

func (s *Server) requireBackups(w http.ResponseWriter) bool {
	if s.backups == nil || s.nodes == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "backups are not configured on this server")
		return false
	}
	return true
}

func (s *Server) backupError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, backup.ErrInvalid), errors.Is(err, backup.ErrWrongKey):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, backup.ErrNotFound), errors.Is(err, nodes.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, backup.ErrConflict), errors.Is(err, backup.ErrKeyExists), errors.Is(err, nodes.ErrBusy):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, backup.ErrNoStorage), errors.Is(err, backup.ErrNoBackupKey), errors.Is(err, nodes.ErrNoAgent):
		writeError(w, http.StatusConflict, "not_configured", err.Error())
	case errors.Is(err, nodes.ErrBadToken):
		writeError(w, http.StatusUnauthorized, "bad_token", err.Error())
	case errors.Is(err, nodes.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		s.provisionError(w, what, err)
	}
}

func toAPIBackup(b store.Backup) gen.Backup {
	enc := gen.BackupEncryptionInstance
	if b.EncryptionKeyID != nil {
		enc = gen.BackupEncryptionProject
	}
	return gen.Backup{
		Id: b.ID, ProjectId: b.ProjectID, Kind: gen.BackupKind(b.Kind), Status: gen.BackupStatus(b.Status),
		SizeBytes: b.SizeBytes, Checksum: b.Checksum, StartedAt: b.StartedAt, FinishedAt: b.FinishedAt,
		ExpiresAt: b.ExpiresAt, OperationId: b.OperationID, Error: b.Error, Encryption: &enc,
	}
}

func (s *Server) writeOperation(w http.ResponseWriter, what string, op store.Operation) {
	o, err := toAPIOperation(op)
	if err != nil {
		s.internalError(w, what, err)
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+op.ID.String())
	writeJSON(w, http.StatusAccepted, o)
}

// lastBackups maps project IDs to their latest finished backup.
func (s *Server) lastBackups(ctx context.Context) map[uuid.UUID]time.Time {
	out := map[uuid.UUID]time.Time{}
	if s.db == nil {
		return out
	}
	rows, err := store.New(s.db).LastBackupTimes(ctx)
	if err != nil {
		s.log.Warn("last backup times", "err", err)
		return out
	}
	for _, r := range rows {
		if r.ProjectID != nil {
			out[*r.ProjectID] = r.LastBackupAt
		}
	}
	return out
}

// ---- Backups -------------------------------------------------------------------

// ListBackups implements GET /api/v1/backups.
func (s *Server) ListBackups(w http.ResponseWriter, r *http.Request, params gen.ListBackupsParams) {
	if !s.requireDB(w) {
		return
	}
	limit := 100
	if params.Limit != nil {
		if *params.Limit < 1 || *params.Limit > 500 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 500")
			return
		}
		limit = *params.Limit
	}
	out := gen.BackupList{Items: []gen.Backup{}}
	var kind *string
	if params.Kind != nil {
		k := string(*params.Kind)
		kind = &k
	}
	acc := accessFrom(r.Context())
	seeAll, ids := s.visibleProjects(r.Context(), acc)
	rows, err := store.New(s.db).ListOrgBackups(r.Context(), store.ListOrgBackupsParams{
		OrgID: acc.OrgID, Kind: kind, ProjectID: params.ProjectId, SeeAll: seeAll, ProjectIds: ids, MaxRows: int32(limit),
	})
	if err != nil {
		s.internalError(w, "list backups", err)
		return
	}
	for _, row := range rows {
		b := toAPIBackup(store.Backup{
			ID: row.ID, ProjectID: row.ProjectID, Kind: row.Kind, ObjectKey: row.ObjectKey, SizeBytes: row.SizeBytes,
			Checksum: row.Checksum, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, Status: row.Status,
			ExpiresAt: row.ExpiresAt, OperationID: row.OperationID, Error: row.Error, EncryptionKeyID: row.EncryptionKeyID,
		})
		b.KeyFingerprint = row.KeyFingerprint
		if row.TargetName != nil {
			kind := gen.StorageTargetKindPlatform
			target := *row.TargetName
			if row.TargetOrgID != nil {
				kind = gen.StorageTargetKindOrg
				if *row.TargetOrgID != acc.OrgID || acc.BreakGlass {
					// Org targets are invisible outside their organisation
					// and to the platform admin (V2 §6).
					target = "an org target"
				}
			}
			b.StorageTarget, b.StorageKind = &target, &kind
		}
		name := row.ProjectName
		b.ProjectName = &name
		deleted := row.ProjectDeleted
		b.ProjectDeleted = &deleted
		out.Items = append(out.Items, b)
	}
	writeJSON(w, http.StatusOK, out)
}

// GetBackupOverview implements GET /api/v1/backups/overview.
func (s *Server) GetBackupOverview(w http.ResponseWriter, r *http.Request, _ gen.GetBackupOverviewParams) {
	if !s.requireBackups(w) {
		return
	}
	o, err := s.backups.Overview(r.Context())
	if err != nil {
		s.internalError(w, "backup overview", err)
		return
	}
	cfg := s.backups.Config()
	out := gen.BackupOverview{
		StorageConfigured: o.StorageConfigured, Key: toAPIKeyInfo(o.Key), AgentAvailable: o.AgentAvailable,
		WindowHourUtc: cfg.Hour, RetentionDaily: cfg.Retention.Daily, RetentionWeekly: cfg.Retention.Weekly,
	}
	if o.LastRestoreTest != nil {
		op, err := toAPIOperation(*o.LastRestoreTest)
		if err != nil {
			s.internalError(w, "backup overview", err)
			return
		}
		out.LastRestoreTest = &op
	}
	if o.LastMetadataBackup != nil {
		b := toAPIBackup(*o.LastMetadataBackup)
		out.LastMetadataBackup = &b
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateProjectBackup implements POST /api/v1/projects/{id}/backups.
func (s *Server) CreateProjectBackup(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireBackups(w) {
		return
	}
	auditFrom(r.Context()).target("project", id.String())
	if s.tenancy != nil && !s.checkQuota(w, s.tenancy.CheckOperation(r.Context(), accessFrom(r.Context()).OrgID)) {
		return
	}
	if s.tenancy != nil {
		// Backups on platform targets count against the backup quota; on
		// the organisation's own target they don't (V2 §6).
		p, err := s.projects.Get(r.Context(), id)
		if err != nil {
			s.provisionError(w, "backup now", err)
			return
		}
		if pl, err := s.backups.PlacementFor(r.Context(), p); err == nil && !pl.OrgTarget &&
			!s.checkQuota(w, s.tenancy.CheckBackupStorage(r.Context(), p.OrgID)) {
			return
		}
	}
	op, err := s.backups.BackupNow(r.Context(), id, userID(r.Context()))
	if err != nil {
		s.backupError(w, "backup now", err)
		return
	}
	s.writeOperation(w, "backup now", op)
}

// RestoreBackup implements POST /api/v1/backups/{id}/restore.
func (s *Server) RestoreBackup(w http.ResponseWriter, r *http.Request, id gen.BackupID) {
	if !s.requireBackups(w) {
		return
	}
	var req gen.RestoreRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	mode := backup.ModeNew
	if req.Mode != nil {
		mode = string(*req.Mode)
	}
	a := auditFrom(r.Context())
	a.target("backup", id.String())
	a.set("mode", mode)
	if mode == backup.ModeInPlace {
		if ok, err := s.can(r.Context(), authz.RestoreInPlace); err != nil {
			s.internalError(w, "restore", err)
			return
		} else if !ok {
			writeError(w, http.StatusForbidden, "forbidden", "restoring in place needs the project admin role")
			return
		}
	}
	if mode == backup.ModeInPlace && s.auth != nil {
		// In-place restore is destructive (spec §7.2): step-up auth.
		if sess, ok := sessionFrom(r.Context()); !ok || !s.auth.RecentlyReauthenticated(sess) {
			writeError(w, http.StatusForbidden, "reauth_required", "confirm your password and code to continue")
			return
		}
	}
	if s.tenancy != nil {
		org := accessFrom(r.Context()).OrgID
		if mode != backup.ModeInPlace && !s.checkQuota(w, s.tenancy.CheckCreateProject(r.Context(), org)) {
			return
		}
		if !s.checkQuota(w, s.tenancy.CheckOperation(r.Context(), org)) {
			return
		}
	}
	p := backup.RestoreParams{BackupID: id, Mode: mode, CreatedBy: userID(r.Context()), CreatorRole: creatorRole(accessFrom(r.Context()))}
	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.Confirm != nil {
		p.Confirm = *req.Confirm
	}
	op, created, err := s.backups.Restore(r.Context(), p)
	if err != nil {
		s.backupError(w, "restore", err)
		return
	}
	o, err := toAPIOperation(op)
	if err != nil {
		s.internalError(w, "restore", err)
		return
	}
	out := gen.RestoreResponse{Operation: o}
	if created != nil {
		a.set("new_project", created.Project.ID.String())
		gp, err := s.toAPIProject(created.Project)
		if err != nil {
			s.internalError(w, "restore", err)
			return
		}
		out.Credentials = &gen.ProjectCredentials{
			Project: gp, Operation: o, Password: created.Password,
			Connection: toAPIConnection(s.projects.ConnectionFor(created.Project), created.Password),
		}
	}
	w.Header().Set("Location", "/api/v1/operations/"+op.ID.String())
	writeJSON(w, http.StatusAccepted, out)
}

// RunRestoreTest implements POST /api/v1/restore-tests.
func (s *Server) RunRestoreTest(w http.ResponseWriter, r *http.Request, params gen.RunRestoreTestParams) {
	if !s.requireBackups(w) {
		return
	}
	op, err := s.backups.TestRestoreNow(r.Context(), params.ProjectId, userID(r.Context()))
	if err != nil {
		s.backupError(w, "restore test", err)
		return
	}
	s.writeOperation(w, "restore test", op)
}

// ---- Storage and backup key -----------------------------------------------------

func storageTarget(req gen.StorageRequest) storage.Target {
	t := storage.Target{Endpoint: req.Endpoint, Bucket: req.Bucket, AccessKey: req.AccessKey}
	if req.Region != nil {
		t.Region = *req.Region
	}
	if req.Prefix != nil {
		t.Prefix = *req.Prefix
	}
	if req.SecretKey != nil {
		t.SecretKey = *req.SecretKey
	}
	if req.PathStyle != nil {
		t.PathStyle = *req.PathStyle
	}
	return t
}

func toAPIStorageTest(steps []storage.TestStep, ok, saved bool) gen.StorageTestResult {
	out := gen.StorageTestResult{Ok: ok, Saved: saved, Steps: []gen.StorageTestStep{}}
	for _, st := range steps {
		gs := gen.StorageTestStep{Step: st.Step, Ok: st.OK, TookMs: int(st.Took.Milliseconds())}
		if st.Err != "" {
			e := st.Err
			gs.Error = &e
		}
		out.Steps = append(out.Steps, gs)
	}
	return out
}

// GetStorageSettings implements GET /api/v1/settings/storage.
func (s *Server) GetStorageSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	_, t, err := s.backups.StorageTarget(r.Context())
	if errors.Is(err, backup.ErrNoStorage) {
		writeJSON(w, http.StatusOK, gen.StorageSettings{Configured: false})
		return
	}
	if err != nil {
		s.internalError(w, "storage settings", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.StorageSettings{
		Configured: true, Endpoint: &t.Endpoint, Region: &t.Region, Bucket: &t.Bucket,
		Prefix: &t.Prefix, AccessKey: &t.AccessKey, PathStyle: &t.PathStyle,
	})
}

func (s *Server) storageRequest(w http.ResponseWriter, r *http.Request) (storage.Target, bool) {
	var req gen.StorageRequest
	if !decodeJSON(w, r, &req) {
		return storage.Target{}, false
	}
	t := storageTarget(req)
	a := auditFrom(r.Context())
	a.set("endpoint", t.Endpoint)
	a.set("bucket", t.Bucket)
	return t, true
}

// PutStorageSettings implements PUT /api/v1/settings/storage.
func (s *Server) PutStorageSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	t, ok := s.storageRequest(w, r)
	if !ok {
		return
	}
	steps, passed, err := s.backups.SaveStorage(r.Context(), t)
	if err != nil && steps == nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err != nil {
		s.internalError(w, "save storage", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIStorageTest(steps, passed, passed))
}

// TestStorageSettings implements POST /api/v1/settings/storage/test.
func (s *Server) TestStorageSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	t, ok := s.storageRequest(w, r)
	if !ok {
		return
	}
	if t.SecretKey == "" {
		if _, cur, err := s.backups.StorageTarget(r.Context()); err == nil && cur.AccessKey == t.AccessKey {
			t.SecretKey = cur.SecretKey
		}
	}
	steps, passed, err := s.backups.TestStorage(r.Context(), t)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toAPIStorageTest(steps, passed, false))
}

func toAPIKeyInfo(k backup.KeyInfo) gen.BackupKeyInfo {
	out := gen.BackupKeyInfo{Exists: k.Exists, ConfirmedAt: k.ConfirmedAt}
	if k.Exists {
		out.Fingerprint = &k.Fingerprint
		out.CreatedAt = &k.CreatedAt
	}
	return out
}

// GetBackupKey implements GET /api/v1/settings/backup-key.
func (s *Server) GetBackupKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	k, err := s.backups.KeyInfo(r.Context())
	if err != nil {
		s.internalError(w, "backup key", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIKeyInfo(k))
}

// GenerateBackupKey implements POST /api/v1/settings/backup-key.
func (s *Server) GenerateBackupKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	k, info, err := s.backups.GenerateKey(r.Context())
	if err != nil {
		s.backupError(w, "generate backup key", err)
		return
	}
	auditFrom(r.Context()).set("fingerprint", info.Fingerprint)
	writeJSON(w, http.StatusCreated, gen.BackupKeyExport{Key: backup.EncodeKey(k), Info: toAPIKeyInfo(info)})
}

// ExportBackupKey implements POST /api/v1/settings/backup-key/export.
func (s *Server) ExportBackupKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	k, err := s.backups.BackupKey(r.Context())
	if err != nil {
		s.backupError(w, "export backup key", err)
		return
	}
	info, err := s.backups.KeyInfo(r.Context())
	if err != nil {
		s.internalError(w, "export backup key", err)
		return
	}
	auditFrom(r.Context()).set("fingerprint", info.Fingerprint)
	writeJSON(w, http.StatusOK, gen.BackupKeyExport{Key: backup.EncodeKey(k), Info: toAPIKeyInfo(info)})
}

// ConfirmBackupKey implements POST /api/v1/settings/backup-key/confirm.
func (s *Server) ConfirmBackupKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	var req gen.BackupKeyConfirmRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	info, err := s.backups.ConfirmKey(r.Context(), req.Key)
	if err != nil {
		s.backupError(w, "confirm backup key", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIKeyInfo(info))
}

// ---- Import ----------------------------------------------------------------------

// ImportPreflight implements POST /api/v1/imports/preflight.
func (s *Server) ImportPreflight(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	var req gen.ImportSource
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	// The source must pass an organisation's outbound rules: the token's,
	// or any the user belongs to (a preflight names none).
	var orgs []uuid.UUID
	if sess, ok := sessionFrom(r.Context()); ok {
		if sess.Token != nil {
			orgs = []uuid.UUID{sess.Token.OrgID}
		} else if rows, err := store.New(s.db).ListUserOrgs(ctx, sess.UserID); err == nil {
			for _, o := range rows {
				orgs = append(orgs, o.ID)
			}
		} else {
			s.internalError(w, "import preflight", err)
			return
		}
	}
	pf, err := s.backups.Preflight(ctx, orgs, req.SourceUrl)
	if err != nil {
		s.backupError(w, "import preflight", err)
		return
	}
	writeJSON(w, http.StatusOK, pf)
}

// CreateImport implements POST /api/v1/imports.
func (s *Server) CreateImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) || !s.requireProjects(w) {
		return
	}
	var req gen.ImportRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	acc, ok := s.authorizeOrg(w, r, req.OrgId, authz.OrgCreateProject)
	if !ok {
		return
	}
	if s.tenancy != nil && (!s.checkQuota(w, s.tenancy.CheckCreateProject(r.Context(), acc.OrgID)) ||
		!s.checkQuota(w, s.tenancy.CheckOperation(r.Context(), acc.OrgID))) {
		return
	}
	a := auditFrom(r.Context())
	a.set("name", req.Name)
	a.set("schemas", req.Schemas)
	if pg, err := agentapi.ParseURL(req.SourceUrl); err == nil {
		a.set("source", pg.Redacted())
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	c, err := s.backups.Import(ctx, backup.ImportParams{
		OrgID: acc.OrgID, CreatorRole: creatorRole(acc),
		SourceURL: req.SourceUrl, Name: req.Name, Description: req.Description,
		Schemas: req.Schemas, CreatedBy: userID(r.Context()),
	})
	if err != nil {
		s.backupError(w, "import", err)
		return
	}
	a.target("project", c.Project.ID.String())
	s.writeCredentials(w, c.Project, c.Operation, c.Password)
}

// ---- Nodes and agents ------------------------------------------------------------

// ListNodes implements GET /api/v1/nodes.
func (s *Server) ListNodes(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	ns, err := s.nodes.List(r.Context())
	if err != nil {
		s.internalError(w, "list nodes", err)
		return
	}
	out := gen.NodeList{Items: make([]gen.Node, 0, len(ns))}
	for _, n := range ns {
		out.Items = append(out.Items, s.toAPINode(n))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateNodeRegistrationToken implements POST /api/v1/nodes/{id}/registration-token.
func (s *Server) CreateNodeRegistrationToken(w http.ResponseWriter, r *http.Request, id gen.NodeID) {
	if !s.requireBackups(w) {
		return
	}
	auditFrom(r.Context()).target("node", id.String())
	tok, exp, err := s.nodes.NewToken(r.Context(), id)
	if err != nil {
		s.backupError(w, "registration token", err)
		return
	}
	cmd := fmt.Sprintf("xenibase-agent register --server https://%s --token %s --advertise <this node's address>:7070", r.Host, tok)
	writeJSON(w, http.StatusCreated, gen.RegistrationToken{Token: tok, ExpiresAt: exp, Command: cmd})
}

// RegisterAgent implements POST /api/v1/agent/register. The token in the
// body authenticates; there is no session.
func (s *Server) RegisterAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	if s.auth != nil {
		if err := s.auth.Allow(ipFrom(r.Context()).String()); err != nil {
			writeError(w, http.StatusTooManyRequests, "rate_limited", auth.ErrRateLimited.Error())
			return
		}
	}
	var req gen.AgentRegisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in := agentapi.RegisterRequest{Token: req.Token, CSR: req.Csr}
	if req.Node != nil {
		in.Node = *req.Node
	}
	if req.AdvertiseHost != nil {
		in.AdvertiseHost = *req.AdvertiseHost
	}
	if req.AdvertisePort != nil {
		in.AdvertisePort = *req.AdvertisePort
	}
	if req.Version != nil {
		in.Version = *req.Version
	}
	a := auditFrom(r.Context())
	a.set("node", in.Node)
	a.set("advertise", fmt.Sprintf("%s:%d", in.AdvertiseHost, in.AdvertisePort))
	res, err := s.nodes.Register(r.Context(), in)
	if err != nil {
		s.backupError(w, "register agent", err)
		return
	}
	a.target("node", res.NodeID)
	writeJSON(w, http.StatusOK, gen.AgentRegisterResponse{NodeId: res.NodeID, CertPem: res.CertPEM, CaPem: res.CAPEM})
}

func (s *Server) toAPINode(n store.Node) gen.Node {
	gn := gen.Node{
		Id: n.ID, Name: n.Name, PrivateAddr: n.PrivateAddr, Role: n.Role, Status: n.Status,
		LastHeartbeat: n.LastHeartbeat, CreatedAt: n.CreatedAt,
		Agent:  gen.AgentStatus{Registered: n.AgentCertFp != nil, CertFingerprint: n.AgentCertFp, Version: n.AgentVersion},
		Region: &n.Region, Provider: &n.Provider, ServerType: n.ServerType, MonthlyCostMinor: n.MonthlyCostMinor,
		CostCurrency: &n.CostCurrency, EmptySince: n.EmptySince, Keep: &n.Keep,
		FailureDomain: n.FailureDomain, PlacementGroup: n.PlacementGroup, FailureDomainLabel: ptrTo(faildomain.Label(n)),
	}
	lc := gen.NodeLifecycle(n.Lifecycle)
	gn.Lifecycle = &lc
	if n.AgentCertFp != nil {
		host := n.PrivateAddr
		if n.AgentHost != nil && *n.AgentHost != "" {
			host = *n.AgentHost
		}
		addr := fmt.Sprintf("%s:%d", host, n.AgentPort)
		gn.Agent.Address = &addr
	}
	if st, ok := s.nodes.Status(n.ID); ok {
		gn.Agent.Reachable = st.Reachable
		checked := st.CheckedAt
		gn.Agent.CheckedAt = &checked
		if st.Err != "" {
			e := st.Err
			gn.Agent.Error = &e
		}
		if st.Reachable {
			dump, dk, img := st.Health.PGDump, st.Health.Docker, st.Health.Image
			gn.Agent.PgDump, gn.Agent.Docker, gn.Agent.Image = &dump, &dk, &img
			var m map[string]any
			if b, err := json.Marshal(st.Metrics); err == nil && json.Unmarshal(b, &m) == nil {
				gn.Agent.Metrics = &m
			}
		}
	}
	return gn
}

// exportsPerOrg caps an organisation's concurrent backup downloads.
const exportsPerOrg = 2

// DownloadBackup implements GET /api/v1/backups/{id}/download: the backup
// as a plain pg_dump archive, for the organisation's owners (V2 §10.10).
func (s *Server) DownloadBackup(w http.ResponseWriter, r *http.Request, id gen.BackupID) {
	if !s.requireBackups(w) {
		return
	}
	a := auditFrom(r.Context())
	a.target("backup", id.String())
	b, err := store.New(s.db).GetBackup(r.Context(), id)
	if err != nil {
		s.backupError(w, "download backup", err)
		return
	}
	if b.ProjectID != nil {
		a.set("project", b.ProjectID.String())
	}
	org := accessFrom(r.Context()).OrgID
	s.exportMu.Lock()
	if s.exporting == nil {
		s.exporting = map[uuid.UUID]int{}
	}
	busy := s.exporting[org] >= exportsPerOrg
	if !busy {
		s.exporting[org]++
	}
	s.exportMu.Unlock()
	if busy {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("at most %d backup downloads at a time per organisation", exportsPerOrg))
		return
	}
	defer func() {
		s.exportMu.Lock()
		s.exporting[org]--
		s.exportMu.Unlock()
	}()
	cw := &startWriter{w: w, start: func() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="xenibase-%s-%s.dump"`, b.ID, b.StartedAt.UTC().Format("20060102-1504")))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	}}
	if err := s.backups.Export(r.Context(), b, cw); err != nil {
		if cw.started {
			// Mid-stream: the archive is cut short, which pg_restore reports.
			s.log.Warn("backup download interrupted", "backup_id", b.ID, "err", err)
			a.set("error", err.Error())
			return
		}
		if errors.Is(err, backup.ErrNotExportable) {
			writeError(w, http.StatusConflict, "not_exportable", err.Error())
			return
		}
		s.backupError(w, "download backup", err)
	}
}

// startWriter sends the response's headers on the first write, so an error
// before any byte is still a proper error response.
type startWriter struct {
	w       io.Writer
	start   func()
	started bool
}

func (s *startWriter) Write(p []byte) (int, error) {
	if !s.started {
		s.started = true
		s.start()
	}
	return s.w.Write(p)
}
