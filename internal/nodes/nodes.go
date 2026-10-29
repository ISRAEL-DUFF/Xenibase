// Package nodes registers nodes' agents and talks to them (spec §3.3, M3
// "Agent v0").
package nodes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/version"
)

// Errors for the API layer.
var (
	ErrBadToken = errors.New("the registration token is invalid or has expired")
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("node not found")
	ErrBusy     = errors.New("node is in use")
)

// Service manages nodes and their agents.
type Service struct {
	db         *pgxpool.Pool
	ca         *agentca.CA
	clientCert tls.Certificate
	bootstrap  string // install-time token, see Register
	home       string // the home region (V3 §6.1)
	log        *slog.Logger

	mu     sync.Mutex
	health map[uuid.UUID]Status
	agents agentClients
}

// Status is the last known state of a node's agent.
type Status struct {
	Reachable bool
	Health    agentapi.Health
	Metrics   agentapi.HostMetrics
	Err       string
	CheckedAt time.Time
}

// SetHomeRegion sets the region nodes are added to when none is named.
func (s *Service) SetHomeRegion(r string) { s.home = r }

// HomeRegion is the home region (V3 §6.1), or "eu-central" when unset.
func (s *Service) HomeRegion() string { return s.regionOr("") }

func (s *Service) regionOr(r string) string {
	if r = strings.TrimSpace(r); r != "" {
		return r
	}
	if s.home != "" {
		return s.home
	}
	return "eu-central"
}

// NewService returns a Service. bootstrapToken (XENIBASE_AGENT_BOOTSTRAP_TOKEN)
// lets the install bundle's own agent register the local node without an
// operator; empty disables it.
func NewService(db *pgxpool.Pool, ca *agentca.CA, bootstrapToken string, log *slog.Logger) (*Service, error) {
	cert, err := ca.ServerClientCert()
	if err != nil {
		return nil, err
	}
	return &Service{db: db, ca: ca, clientCert: cert, bootstrap: bootstrapToken, log: log, health: map[uuid.UUID]Status{}}, nil
}

// newToken returns a registration token, its hash, and its expiry (a day).
func newToken() (token, hash string, exp time.Time, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", time.Time{}, err
	}
	token = "xbreg_" + base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), time.Now().Add(24 * time.Hour), nil
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

// NewToken creates a one-time registration token for a node, valid for a
// day, replacing any earlier one.
func (s *Service) NewToken(ctx context.Context, nodeID uuid.UUID) (string, time.Time, error) {
	if n, err := store.New(s.db).GetNode(ctx, nodeID); errors.Is(err, pgx.ErrNoRows) || (err == nil && n.Status == "removed") {
		return "", time.Time{}, ErrNotFound
	}
	tok, h, exp, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	if err := store.New(s.db).SetRegistrationToken(ctx, store.SetRegistrationTokenParams{ID: nodeID, RegistrationToken: &h, RegistrationExpiresAt: &exp}); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// Register signs an agent's CSR and pins its certificate on the node. The
// token is either a node's one-time token or, with req.Node naming a node
// that has no agent yet, the bootstrap token.
func (s *Service) Register(ctx context.Context, req agentapi.RegisterRequest) (agentapi.RegisterResponse, error) {
	if req.AdvertiseHost == "" || req.AdvertisePort < 1 || req.AdvertisePort > 65535 || strings.ContainsAny(req.AdvertiseHost, " /") {
		return agentapi.RegisterResponse{}, fmt.Errorf("%w: advertise host and port are required", ErrInvalid)
	}
	var out agentapi.RegisterResponse
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		var node store.Node
		var err error
		if s.bootstrap != "" && req.Node != "" && subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.bootstrap)) == 1 {
			node, err = q.GetNodeByName(ctx, req.Node)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: no node named %q", ErrBadToken, req.Node)
			}
			if err == nil && node.AgentCertFp != nil {
				// The bootstrap token only claims nodes without an agent;
				// re-registering needs a token from the UI.
				return fmt.Errorf("%w: node %q already has an agent; issue a new token from the UI", ErrBadToken, req.Node)
			}
		} else {
			h := hashToken(req.Token)
			node, err = q.GetNodeByRegistrationToken(ctx, &h)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBadToken
			}
		}
		if err != nil {
			return err
		}
		certPEM, fp, err := s.ca.SignAgent([]byte(req.CSR), node.ID.String(), []string{req.AdvertiseHost})
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		v := req.Version
		if _, err := q.CompleteNodeRegistration(ctx, store.CompleteNodeRegistrationParams{
			ID: node.ID, AgentCertFp: &fp, AgentHost: &req.AdvertiseHost, AgentPort: int32(req.AdvertisePort), AgentVersion: &v,
		}); err != nil {
			return err
		}
		out = agentapi.RegisterResponse{NodeID: node.ID.String(), CertPEM: string(certPEM), CAPEM: string(s.ca.CertPEM())}
		s.log.Info("agent registered", "node", node.Name, "advertise", fmt.Sprintf("%s:%d", req.AdvertiseHost, req.AdvertisePort), "fingerprint", fp)
		return nil
	})
	return out, err
}

// ForNode returns the agent of a node.
func (s *Service) ForNode(ctx context.Context, nodeID uuid.UUID) (*Agent, error) {
	n, err := store.New(s.db).GetNode(ctx, nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.agentFor(n)
}

// ForInstance returns the agent of the node hosting an instance.
func (s *Service) ForInstance(ctx context.Context, instanceID uuid.UUID) (*Agent, error) {
	n, err := store.New(s.db).NodeForInstance(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("node for instance %s: %w", instanceID, err)
	}
	return s.agentFor(n)
}

// Any returns the first node with a registered agent (for work that is not
// tied to a project, like metadata self-backups).
func (s *Service) Any(ctx context.Context) (*Agent, error) {
	n, err := store.New(s.db).FirstAgentNode(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoAgent
	}
	if err != nil {
		return nil, err
	}
	return s.agentFor(n)
}

// Status returns the last health check of a node.
func (s *Service) Status(id uuid.UUID) (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.health[id]
	return st, ok
}

// Check polls one node's agent now.
func (s *Service) Check(ctx context.Context, n store.Node) Status {
	st := Status{CheckedAt: time.Now()}
	a, err := s.dial(n)
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st.Health, err = a.Health(cctx)
		if err == nil {
			st.Metrics, err = a.Metrics(cctx)
		}
		cancel()
	}
	if err != nil {
		st.Err = err.Error()
	} else {
		st.Reachable = true
		if server := version.Get().Version; !Compatible(server, st.Health.Version) {
			st.Err = fmt.Sprintf("agent %s is incompatible with xenibase-server %s: no work is sent to it until it is upgraded", st.Health.Version, server)
			s.log.Warn("incompatible agent", "node", n.Name, "agent_version", st.Health.Version, "server_version", server)
		}
	}
	s.mu.Lock()
	s.health[n.ID] = st
	s.mu.Unlock()

	if n.AgentCertFp != nil {
		status := "unreachable"
		capacity := json.RawMessage(`{}`)
		var ver *string
		if st.Reachable {
			status = "healthy"
			capacity, _ = json.Marshal(st.Metrics)
			ver = &st.Health.Version
		}
		_ = store.New(s.db).RecordNodeHeartbeat(ctx, store.RecordNodeHeartbeatParams{ID: n.ID, Status: status, Capacity: capacity, AgentVersion: ver})
	}
	return st
}

// Run checks every agent every interval until ctx ends (spec §8.7: node
// metrics every 30s).
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		ns, err := store.New(s.db).ListNodes(ctx)
		if err == nil {
			for _, n := range ns {
				if n.AgentCertFp != nil {
					s.Check(ctx, n)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// List returns all nodes.
func (s *Service) List(ctx context.Context) ([]store.Node, error) {
	return store.New(s.db).ListNodes(ctx)
}

// Node roles (spec §9). Database nodes can switch between the first three;
// a pooler host (V3 §2.1) stays one.
var roles = map[string]bool{"shared": true, "dedicated": true, "both": true}

// RolePooler is an edge pooler host.
const RolePooler = "pooler"

// RoleEdge is a node that runs xenibase-edge only (V4.1 §11): never given a
// database, its cost split by backend services' use.
const RoleEdge = "edge"

var nodeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// CreateNode records a node an operator is adding and issues its one-time
// registration token (spec §10: POST /nodes returns a token).
func (s *Service) CreateNode(ctx context.Context, name, privateAddr, role, region, failureDomain string) (store.Node, string, time.Time, error) {
	if err := checkDomain(failureDomain); err != nil {
		return store.Node{}, "", time.Time{}, err
	}
	if !nodeName.MatchString(name) {
		return store.Node{}, "", time.Time{}, fmt.Errorf("%w: node names are lowercase letters, digits, and dashes", ErrInvalid)
	}
	if !roles[role] && role != RolePooler && role != RoleEdge {
		return store.Node{}, "", time.Time{}, fmt.Errorf("%w: role must be shared, dedicated, both, pooler or edge", ErrInvalid)
	}
	privateAddr = strings.TrimSpace(privateAddr)
	if privateAddr == "" || strings.ContainsAny(privateAddr, " /:") {
		return store.Node{}, "", time.Time{}, fmt.Errorf("%w: private address must be a host name or IP", ErrInvalid)
	}
	token, hash, exp, err := newToken()
	if err != nil {
		return store.Node{}, "", time.Time{}, err
	}
	n, err := store.New(s.db).InsertNode(ctx, store.InsertNodeParams{
		Name: name, PrivateAddr: privateAddr, Role: role, RegistrationToken: &hash, RegistrationExpiresAt: &exp, Region: s.regionOr(region),
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return store.Node{}, "", time.Time{}, fmt.Errorf("%w: a node named %s exists", ErrInvalid, name)
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return store.Node{}, "", time.Time{}, fmt.Errorf("%w: no region %q", ErrInvalid, region)
	}
	if err == nil && failureDomain != "" {
		n, err = store.New(s.db).SetNodeFailureDomain(ctx, store.SetNodeFailureDomainParams{ID: n.ID, FailureDomain: &failureDomain})
	}
	return n, token, exp, err
}

var domainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,62}$`)

func checkDomain(d string) error {
	if d != "" && !domainName.MatchString(d) {
		return fmt.Errorf("%w: a failure domain is up to 63 letters, digits, dots, colons, dashes and underscores, e.g. lagos-dc1-r3", ErrInvalid)
	}
	return nil
}

// SetFailureDomain records what fails with the node (V3.1 §2); "" clears
// it, so the node counts as alone again. Existing placements aren't moved:
// the failure-domain check reports any that now share a domain.
func (s *Service) SetFailureDomain(ctx context.Context, id uuid.UUID, d string) (store.Node, error) {
	d = strings.TrimSpace(d)
	if err := checkDomain(d); err != nil {
		return store.Node{}, err
	}
	var arg *string
	if d != "" {
		arg = &d
	}
	n, err := store.New(s.db).SetNodeFailureDomain(ctx, store.SetNodeFailureDomainParams{ID: id, FailureDomain: arg})
	if errors.Is(err, pgx.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

// RemoveNode takes a node out of service once nothing runs on it: its
// agent's certificate is no longer trusted and nothing is placed there.
func (s *Service) RemoveNode(ctx context.Context, id uuid.UUID) error {
	q := store.New(s.db)
	if _, err := q.GetNode(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	n, err := q.NodeLiveInstances(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %d instance(s) still run on this node", ErrBusy, n)
	}
	s.mu.Lock()
	delete(s.health, id)
	s.mu.Unlock()
	return q.RemoveNode(ctx, id)
}

// SetRole changes which tiers new projects may be placed on the node with
// (existing instances stay).
func (s *Service) SetRole(ctx context.Context, id uuid.UUID, role string) (store.Node, error) {
	if !roles[role] {
		return store.Node{}, fmt.Errorf("%w: role must be shared, dedicated, or both", ErrInvalid)
	}
	if cur, err := store.New(s.db).GetNode(ctx, id); err == nil && cur.Role == RolePooler {
		return store.Node{}, fmt.Errorf("%w: a pooler host can't become a database node; remove it and add the machine again", ErrInvalid)
	}
	n, err := store.New(s.db).SetNodeRole(ctx, store.SetNodeRoleParams{ID: id, Role: role})
	if errors.Is(err, pgx.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}
