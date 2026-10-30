package api

import (
	"net/http"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/store"
)

// The client certificates xenibase-servers issue themselves for agents, and
// their withdrawal (nodes.Service.RevokeClientCerts).

func toAPIAgentClientCert(c store.AgentClientCert, current bool) gen.AgentClientCert {
	return gen.AgentClientCert{Serial: c.Serial, Server: c.ServerName, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt, RevokedAt: c.RevokedAt, Current: current}
}

func (s *Server) requireAgents(w http.ResponseWriter) bool {
	if s.nodes == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "agents are not configured on this server")
		return false
	}
	return true
}

// ListAgentClientCerts implements GET /api/v1/admin/agent-client-certs.
func (s *Server) ListAgentClientCerts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgents(w) {
		return
	}
	certs, err := s.nodes.ListClientCerts(r.Context())
	if err != nil {
		s.internalError(w, "agent client certificates", err)
		return
	}
	items := make([]gen.AgentClientCert, 0, len(certs))
	for _, c := range certs {
		items = append(items, toAPIAgentClientCert(c.AgentClientCert, c.Current))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// RevokeAgentClientCerts implements POST /api/v1/admin/agent-client-certs/revoke.
func (s *Server) RevokeAgentClientCerts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgents(w) {
		return
	}
	var req gen.AgentClientCertRevoke
	if !decodeJSON(w, r, &req) {
		return
	}
	var serial, server string
	if req.Serial != nil {
		serial = *req.Serial
	}
	if req.Server != nil {
		server = *req.Server
	}
	a := auditFrom(r.Context())
	if serial != "" {
		a.target("agent_client_cert", serial)
	} else {
		a.target("xenibase_server", server)
	}
	res, err := s.nodes.RevokeClientCerts(r.Context(), serial, server, userID(r.Context()))
	if err != nil {
		s.backupError(w, "revoke agent client certificate", err)
		return
	}
	out := gen.AgentClientCertRevoked{Revoked: make([]gen.AgentClientCert, 0, len(res.Revoked)), NotPushed: res.NotPushed}
	if out.NotPushed == nil {
		out.NotPushed = []string{}
	}
	for _, c := range res.Revoked {
		out.Revoked = append(out.Revoked, toAPIAgentClientCert(c, false))
	}
	a.set("revoked", len(res.Revoked))
	writeJSON(w, http.StatusOK, out)
}
