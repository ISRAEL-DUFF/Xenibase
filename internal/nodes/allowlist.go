package nodes

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/store"
)

// Every server's client certificates are recorded in agent_client_certs,
// and the live ones (unexpired, unrevoked, from every server sharing the
// metadata database) are the allow list each agent is given, signed by the
// agent CA. An agent that has a list accepts only its serials, so revoking
// a serial withdraws a copied certificate within one push.

// allowRepush is how often a list an agent already acknowledged is pushed
// again anyway: an agent upgraded from a version without lists, or whose
// state was reset, gets one within this long.
const allowRepush = 10 * time.Minute

// allowList is this server's copy of the signed list, and which generation
// each agent last acknowledged.
type allowList struct {
	mu     sync.Mutex
	loaded bool
	gen    int64
	signed agentapi.ClientSerials
	live   map[string]bool
	acked  map[uuid.UUID]allowAck
}

type allowAck struct {
	gen int64
	at  time.Time
}

// defaultServerName names this server in its client certificates: the
// host name.
func defaultServerName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "xenibase-server"
}

// registerClientCert records a new client certificate's serial and raises
// the list's generation, in one transaction.
func (s *Service) registerClientCert(ctx context.Context, leaf *x509.Certificate) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.InsertAgentClientCert(ctx, store.InsertAgentClientCertParams{
			Serial: agentca.Serial(leaf), ServerName: s.name, IssuedAt: time.Now(), ExpiresAt: leaf.NotAfter,
		}); err != nil {
			return err
		}
		if _, err := q.BumpAgentClientCertGeneration(ctx); err != nil {
			return err
		}
		_, err := q.PruneAgentClientCerts(ctx)
		return err
	})
}

// refreshAllow reads the live serials and their generation in one
// snapshot, signs them, and keeps the result for pushes.
func (s *Service) refreshAllow(ctx context.Context) error {
	var gen int64
	var rows []store.AgentClientCert
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if gen, err = q.AgentClientCertGeneration(ctx); err != nil {
			return err
		}
		rows, err = q.LiveAgentClientCerts(ctx)
		return err
	})
	if err != nil {
		return err
	}
	list := agentapi.ClientSerialList{Generation: gen, SignedAt: time.Now().UTC(), Serials: make([]agentapi.ClientSerial, 0, len(rows))}
	live := make(map[string]bool, len(rows))
	for _, r := range rows {
		list.Serials = append(list.Serials, agentapi.ClientSerial{Serial: r.Serial, Server: r.ServerName, ExpiresAt: r.ExpiresAt})
		live[r.Serial] = true
	}
	signed, err := s.ca.SignList(list)
	if err != nil {
		return err
	}
	s.allow.mu.Lock()
	defer s.allow.mu.Unlock()
	if s.allow.loaded && gen < s.allow.gen {
		return nil // a concurrent refresh read a later snapshot
	}
	s.allow.loaded, s.allow.gen, s.allow.signed, s.allow.live = true, gen, signed, live
	return nil
}

// checkOwnSerial has this server reissue its certificate when its serial
// is no longer on the list (an admin revoked it).
func (s *Service) checkOwnSerial() {
	own := s.certs.serial()
	s.allow.mu.Lock()
	revoked := s.allow.loaded && own != "" && !s.allow.live[own]
	s.allow.mu.Unlock()
	if revoked {
		s.log.Warn("this server's agent client certificate is no longer live (revoked); issuing a new one", "serial", own)
		s.certs.markStale(own)
	}
}

// forget drops an agent's acknowledgement, so the next call pushes again.
func (s *Service) forget(node uuid.UUID) {
	s.allow.mu.Lock()
	delete(s.allow.acked, node)
	s.allow.mu.Unlock()
}

// ensureAllowed pushes the allow list to a's agent unless it already
// acknowledged this generation recently. Agent.do calls it before every
// command, so an agent learns a new serial before the first call made
// with it.
func (s *Service) ensureAllowed(ctx context.Context, a *Agent) error {
	// Renew first if due, so the list read below already names the
	// certificate the push (and the call after it) will present.
	if _, err := s.certs.get(); err != nil {
		return err
	}
	s.allow.mu.Lock()
	loaded := s.allow.loaded
	s.allow.mu.Unlock()
	if !loaded {
		if err := s.refreshAllow(ctx); err != nil {
			return fmt.Errorf("allow list: %w", err)
		}
	}
	s.allow.mu.Lock()
	gen, signed := s.allow.gen, s.allow.signed
	ack, ok := s.allow.acked[a.Node.ID]
	s.allow.mu.Unlock()
	if ok && ack.gen >= gen && time.Since(ack.at) < allowRepush {
		return nil
	}
	status, raw, err := a.roundTrip(ctx, http.MethodPut, agentapi.PathClientSerials, signed)
	if err != nil {
		return err
	}
	var res agentapi.ClientSerialsAck
	switch status {
	case http.StatusOK, http.StatusConflict:
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("agent %s: allow list: %w", a.Node.Name, err)
		}
	case http.StatusNotFound:
		// An agent from before allow lists: it accepts by subject.
		res.Generation = gen
	case http.StatusForbidden:
		// Not on the agent's list, and not on this one either: this
		// server's own serial was revoked.
		if own := s.certs.serial(); own != "" {
			s.certs.markStale(own)
		}
		return fmt.Errorf("agent %s refused this server's client certificate", a.Node.Name)
	default:
		return fmt.Errorf("agent %s: allow list: %s", a.Node.Name, agentError(raw, status))
	}
	s.allow.mu.Lock()
	if s.allow.acked == nil {
		s.allow.acked = map[uuid.UUID]allowAck{}
	}
	s.allow.acked[a.Node.ID] = allowAck{gen: max(res.Generation, gen), at: time.Now()}
	s.allow.mu.Unlock()
	return nil
}

// PushAllowList sends the current list to every registered agent now (after
// a revocation), and returns the agents that did not take it.
func (s *Service) PushAllowList(ctx context.Context) []string {
	ns, err := store.New(s.db).ListNodes(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	var failed []string
	for _, n := range ns {
		if n.AgentCertFp == nil || n.Status == "removed" {
			continue
		}
		s.forget(n.ID)
		a, err := s.dial(n)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = s.ensureAllowed(cctx, a)
			cancel()
		}
		if err != nil {
			failed = append(failed, n.Name+": "+err.Error())
		}
	}
	return failed
}

// ClientCert is one recorded client certificate.
type ClientCert struct {
	store.AgentClientCert
	Current bool // this server's certificate in use
}

// ListClientCerts lists the client certificates every server has issued
// itself, newest first, live and recently expired or revoked.
func (s *Service) ListClientCerts(ctx context.Context) ([]ClientCert, error) {
	rows, err := store.New(s.db).ListAgentClientCerts(ctx)
	if err != nil {
		return nil, err
	}
	own := s.certs.serial()
	out := make([]ClientCert, 0, len(rows))
	for _, r := range rows {
		out = append(out, ClientCert{AgentClientCert: r, Current: r.Serial == own})
	}
	return out, nil
}

// RevokeResult is what RevokeClientCerts did.
type RevokeResult struct {
	Revoked []store.AgentClientCert
	// NotPushed names the agents the new list did not reach; each gets it
	// at its next health check or call.
	NotPushed []string
}

// RevokeClientCerts revokes one live client certificate by serial, or
// every live one of a server by name, and pushes the new list to every
// agent. Revoking this server's own certificate makes it issue and
// register a new one first, so it keeps its agents.
func (s *Service) RevokeClientCerts(ctx context.Context, serial, server string, by *uuid.UUID) (RevokeResult, error) {
	serial, server = strings.ToLower(strings.TrimSpace(serial)), strings.TrimSpace(server)
	if (serial == "") == (server == "") {
		return RevokeResult{}, fmt.Errorf("%w: give a serial or a server, not both", ErrInvalid)
	}
	p := store.RevokeAgentClientCertsParams{RevokedBy: by}
	if serial != "" {
		p.Serial = &serial
	} else {
		p.ServerName = &server
	}
	var res RevokeResult
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if res.Revoked, err = q.RevokeAgentClientCerts(ctx, p); err != nil || len(res.Revoked) == 0 {
			return err
		}
		_, err = q.BumpAgentClientCertGeneration(ctx)
		return err
	})
	if err != nil {
		return RevokeResult{}, err
	}
	if len(res.Revoked) == 0 {
		return res, ErrNotFound
	}
	for _, r := range res.Revoked {
		s.certs.markStale(r.Serial)
	}
	if _, err := s.certs.get(); err != nil { // reissue now if it was ours
		return res, err
	}
	if err := s.refreshAllow(ctx); err != nil {
		return res, err
	}
	res.NotPushed = s.PushAllowList(ctx)
	return res, nil
}
