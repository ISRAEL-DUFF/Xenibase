package nodes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/store"
)

// Agent is a connection to one node's agent, verified against the CA and
// the node's pinned certificate fingerprint.
type Agent struct {
	Node store.Node
	base string
	http *http.Client
}

// ErrNoAgent means the node has no registered agent.
var ErrNoAgent = errors.New("node has no registered agent")

func (s *Service) agentFor(n store.Node) (*Agent, error) {
	if err := checkVersion(n); err != nil {
		return nil, err
	}
	return s.dial(n)
}

// dial returns a client for a node's agent without the version check
// (health checks must still reach an agent that needs upgrading).
func (s *Service) dial(n store.Node) (*Agent, error) {
	if n.AgentCertFp == nil || *n.AgentCertFp == "" {
		return nil, fmt.Errorf("%w (node %s)", ErrNoAgent, n.Name)
	}
	host := n.PrivateAddr
	if n.AgentHost != nil && *n.AgentHost != "" {
		host = *n.AgentHost
	}
	pin := *n.AgentCertFp
	addr := net.JoinHostPort(host, strconv.Itoa(int(n.AgentPort)))
	return &Agent{
		Node: n,
		base: "https://" + addr,
		http: s.agents.client(n.ID, addr+" "+pin, func() *http.Client {
			return &http.Client{Transport: &http.Transport{
				TLSClientConfig: s.agentTLS(n, host, pin), ResponseHeaderTimeout: 0, IdleConnTimeout: time.Minute,
			}}
		}),
	}, nil
}

// agentTLS is the TLS configuration for one node's agent: the agent CA, and
// the node's pinned certificate. The server's client certificate is read at
// each handshake, not when the configuration is built, so the cached
// transports present a renewed one.
func (s *Service) agentTLS(n store.Node, host, pin string) *tls.Config {
	return &tls.Config{
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &s.clientCert, nil
		},
		RootCAs:    s.ca.Pool(),
		ServerName: host,
		MinVersion: tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, chains [][]*x509.Certificate) error {
			if len(raw) == 0 || agentca.Fingerprint(raw[0]) != pin {
				return errors.New("agent certificate does not match the pinned fingerprint")
			}
			if id, ok := agentca.NodeIDFromCert(chains[0][0]); !ok || id != n.ID.String() {
				return errors.New("agent certificate is for another node")
			}
			return nil
		},
	}
}

// agentClients keeps one HTTP client per node, so calls reuse connections
// instead of a TLS handshake each. A client is replaced, and its idle
// connections closed, when the node's address or pinned fingerprint
// changes (re-registration).
type agentClients struct {
	mu sync.Mutex
	m  map[uuid.UUID]agentClient
}

type agentClient struct {
	key  string // address and pinned fingerprint
	http *http.Client
}

func (c *agentClients) client(node uuid.UUID, key string, build func() *http.Client) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.m[node]; ok {
		if old.key == key {
			return old.http
		}
		old.http.CloseIdleConnections()
	}
	if c.m == nil {
		c.m = map[uuid.UUID]agentClient{}
	}
	h := build()
	c.m[node] = agentClient{key: key, http: h}
	return h
}

func (a *Agent) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("agent %s: %w", a.Node.Name, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode == http.StatusNoContent {
		return nil
	}
	if res.StatusCode != http.StatusOK {
		var e agentapi.Error
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("agent %s: %s", a.Node.Name, e.Error)
		}
		return fmt.Errorf("agent %s: %s", a.Node.Name, res.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Health calls GET /v1/health.
func (a *Agent) Health(ctx context.Context) (agentapi.Health, error) {
	var h agentapi.Health
	return h, a.do(ctx, http.MethodGet, agentapi.PathHealth, nil, &h)
}

// Metrics calls GET /v1/host/metrics.
func (a *Agent) Metrics(ctx context.Context) (agentapi.HostMetrics, error) {
	var m agentapi.HostMetrics
	return m, a.do(ctx, http.MethodGet, agentapi.PathMetrics, nil, &m)
}

// Dump calls POST /v1/dump.
func (a *Agent) Dump(ctx context.Context, req agentapi.DumpRequest) (agentapi.DumpResult, error) {
	var r agentapi.DumpResult
	return r, a.do(ctx, http.MethodPost, agentapi.PathDump, req, &r)
}

// Restore calls POST /v1/restore.
func (a *Agent) Restore(ctx context.Context, req agentapi.RestoreRequest) (agentapi.RestoreResult, error) {
	var r agentapi.RestoreResult
	return r, a.do(ctx, http.MethodPost, agentapi.PathRestore, req, &r)
}

// Copy calls POST /v1/copy.
func (a *Agent) Copy(ctx context.Context, req agentapi.CopyRequest) (agentapi.RestoreResult, error) {
	var r agentapi.RestoreResult
	return r, a.do(ctx, http.MethodPost, agentapi.PathCopy, req, &r)
}

func instancePath(p, id string) string { return strings.Replace(p, "{id}", id, 1) }

// CreateInstance calls POST /v1/instances (idempotent).
func (a *Agent) CreateInstance(ctx context.Context, spec agentapi.InstanceSpec) (agentapi.Instance, error) {
	var r agentapi.Instance
	return r, a.do(ctx, http.MethodPost, agentapi.PathInstances, spec, &r)
}

// EtcdAddress calls GET /v1/etcd/address.
func (a *Agent) EtcdAddress(ctx context.Context) (agentapi.EtcdAddress, error) {
	var r agentapi.EtcdAddress
	return r, a.do(ctx, http.MethodGet, agentapi.PathEtcdAddress, nil, &r)
}

// RunEtcd calls PUT /v1/etcd.
func (a *Agent) RunEtcd(ctx context.Context, spec agentapi.EtcdSpec) (agentapi.Etcd, error) {
	var r agentapi.Etcd
	return r, a.do(ctx, http.MethodPut, agentapi.PathEtcd, spec, &r)
}

// Etcd calls GET /v1/etcd.
func (a *Agent) Etcd(ctx context.Context) (agentapi.Etcd, error) {
	var r agentapi.Etcd
	return r, a.do(ctx, http.MethodGet, agentapi.PathEtcd, nil, &r)
}

// EtcdMembers calls POST /v1/etcd/members.
func (a *Agent) EtcdMembers(ctx context.Context, req agentapi.EtcdMembersRequest) (agentapi.EtcdMembers, error) {
	var r agentapi.EtcdMembers
	return r, a.do(ctx, http.MethodPost, agentapi.PathEtcdMembers, req, &r)
}

// RemoveEtcd calls DELETE /v1/etcd: the container and its data.
func (a *Agent) RemoveEtcd(ctx context.Context) error {
	return a.do(ctx, http.MethodDelete, agentapi.PathEtcd, nil, nil)
}

// Instance calls GET /v1/instances/{id}.
func (a *Agent) Instance(ctx context.Context, id string) (agentapi.Instance, error) {
	var r agentapi.Instance
	return r, a.do(ctx, http.MethodGet, instancePath(agentapi.PathInstance, id), nil, &r)
}

// StartInstance calls POST /v1/instances/{id}/start.
func (a *Agent) StartInstance(ctx context.Context, id string) (agentapi.Instance, error) {
	var r agentapi.Instance
	return r, a.do(ctx, http.MethodPost, instancePath(agentapi.PathInstanceStart, id), nil, &r)
}

// StopInstance calls POST /v1/instances/{id}/stop.
func (a *Agent) StopInstance(ctx context.Context, id string) (agentapi.Instance, error) {
	var r agentapi.Instance
	return r, a.do(ctx, http.MethodPost, instancePath(agentapi.PathInstanceStop, id), nil, &r)
}

// DestroyInstance calls DELETE /v1/instances/{id}: container and volume.
func (a *Agent) DestroyInstance(ctx context.Context, id string) error {
	var r agentapi.Instance
	return a.do(ctx, http.MethodDelete, instancePath(agentapi.PathInstance, id), nil, &r)
}

// BaseBackup calls POST /v1/instances/{id}/walg/backup.
func (a *Agent) BaseBackup(ctx context.Context, id string, req agentapi.WALGBackupRequest) (agentapi.WALGBackupResult, error) {
	var r agentapi.WALGBackupResult
	return r, a.do(ctx, http.MethodPost, instancePath(agentapi.PathWALGBackup, id), req, &r)
}

// BaseBackups calls GET /v1/instances/{id}/walg/backups.
func (a *Agent) BaseBackups(ctx context.Context, id string) ([]agentapi.WALGBackup, error) {
	var r []agentapi.WALGBackup
	return r, a.do(ctx, http.MethodGet, instancePath(agentapi.PathWALGBackupList, id), nil, &r)
}
