package api

import (
	"errors"
	"net/http"
	"sort"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

func toAPIWindow(w dedicated.MaintenanceWindow) gen.MaintenanceWindow {
	return gen.MaintenanceWindow{Enabled: w.Enabled, Weekday: int(w.Weekday), StartHour: w.StartHour, Hours: w.Hours}
}

func toAPIMinorUpgrade(m store.MinorUpgrade, kind, node string) gen.MinorUpgrade {
	return gen.MinorUpgrade{
		Id: m.ID, InstanceId: m.InstanceID, Kind: gen.MinorUpgradeKind(kind), NodeName: node,
		FromRelease: m.FromRelease, ToRelease: m.ToRelease, StartedAt: m.StartedAt, FinishedAt: m.FinishedAt,
		PauseMs: i32(m.PauseMs), Error: m.Error,
	}
}

// GetMaintenance implements GET /api/v1/admin/maintenance.
func (s *Server) GetMaintenance(w http.ResponseWriter, r *http.Request) {
	ds := s.dedicatedSvc(w)
	if ds == nil {
		return
	}
	ctx := r.Context()
	win, err := ds.MaintenanceWindow(ctx)
	if err != nil {
		s.internalError(w, "maintenance window", err)
		return
	}
	now := time.Now()
	out := gen.MaintenanceStatus{Window: toAPIWindow(win), InWindow: win.Contains(now), NextWindow: win.Next(now),
		Behind: []gen.InstanceSummary{}, History: []gen.MinorUpgrade{}}
	for _, sum := range s.instanceSummaries(ctx) {
		if sum.PgRelease != nil && sum.PgReleaseAvailable != nil && sum.Status == "running" &&
			dedicated.NewerRelease(*sum.PgRelease, *sum.PgReleaseAvailable) {
			out.Behind = append(out.Behind, sum)
		}
	}
	sort.Slice(out.Behind, func(i, j int) bool { return out.Behind[i].NodeName < out.Behind[j].NodeName })
	rows, err := store.New(s.db).LatestMinorUpgrades(ctx, 20)
	if err != nil {
		s.internalError(w, "minor upgrades", err)
		return
	}
	for _, m := range rows {
		out.History = append(out.History, toAPIMinorUpgrade(store.MinorUpgrade{
			ID: m.ID, InstanceID: m.InstanceID, FromRelease: m.FromRelease, ToRelease: m.ToRelease,
			StartedAt: m.StartedAt, FinishedAt: m.FinishedAt, PauseMs: m.PauseMs, Error: m.Error,
		}, m.Kind, m.NodeName))
	}
	writeJSON(w, http.StatusOK, out)
}

// PutMaintenanceWindow implements PUT /api/v1/admin/maintenance/window.
func (s *Server) PutMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	ds := s.dedicatedSvc(w)
	if ds == nil {
		return
	}
	var req gen.MaintenanceWindow
	if !decodeJSON(w, r, &req) {
		return
	}
	win := dedicated.MaintenanceWindow{Enabled: req.Enabled, Weekday: time.Weekday(req.Weekday), StartHour: req.StartHour, Hours: req.Hours}
	if err := ds.SetMaintenanceWindow(r.Context(), win); err != nil {
		s.provisionError(w, "maintenance window", err)
		return
	}
	a := auditFrom(r.Context())
	a.set("enabled", req.Enabled)
	a.set("weekday", req.Weekday)
	a.set("start_hour", req.StartHour)
	a.set("hours", req.Hours)
	writeJSON(w, http.StatusOK, toAPIWindow(win))
}

// MinorUpgradeInstance implements POST /api/v1/admin/instances/{instance_id}/minor-upgrade.
func (s *Server) MinorUpgradeInstance(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	ds := s.dedicatedSvc(w)
	if ds == nil {
		return
	}
	auditFrom(r.Context()).target("instance", id.String())
	m, err := ds.MinorUpgradeNow(r.Context(), id, userID(r.Context()))
	switch {
	case errors.Is(err, provision.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return
	case m.ID == [16]byte{}:
		s.provisionError(w, "minor upgrade", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIMinorUpgrade(store.MinorUpgrade{
		ID: m.ID, InstanceID: m.InstanceID, FromRelease: m.FromRelease, ToRelease: m.ToRelease,
		StartedAt: m.StartedAt, FinishedAt: m.FinishedAt, PauseMs: m.PauseMs, Error: m.Error,
	}, m.Kind, m.NodeName))
}
