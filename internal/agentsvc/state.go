package agentsvc

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
)

// State is what registration leaves in the agent's state directory.
type State struct {
	Dir    string
	NodeID string
	Cert   tls.Certificate
	CA     *x509.CertPool
	// CACert is the pinned agent CA, which signs the client certificate
	// allow list (LoadClientAllow).
	CACert *x509.Certificate
}

const (
	keyFile  = "agent.key"
	certFile = "agent.crt"
	caFile   = "ca.crt"
	nodeFile = "node.json"
)

// LoadState reads a registered agent's state, or os.ErrNotExist.
func LoadState(dir string) (*State, error) {
	nodeJSON, err := os.ReadFile(filepath.Join(dir, nodeFile))
	if err != nil {
		return nil, err
	}
	var meta struct {
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(nodeJSON, &meta); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode(caPEM)
	if b == nil {
		return nil, errors.New("agent state: bad CA certificate")
	}
	caCert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("agent state: bad CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &State{Dir: dir, NodeID: meta.NodeID, Cert: cert, CA: pool, CACert: caCert}, nil
}

// RegisterOptions configures Register.
type RegisterOptions struct {
	Server        string // xenibase-server base URL
	Token         string
	Node          string // with a bootstrap token
	AdvertiseHost string
	AdvertisePort int
	Version       string
	// RootCAs verifies xenibase-server's HTTPS certificate; nil means the
	// system roots. InsecureSkipVerify is for development only.
	RootCAs            *x509.CertPool
	InsecureSkipVerify bool
}

// Register creates a key, has xenibase-server sign it, and saves the result
// (key, certificate, pinned CA) in dir.
func Register(ctx context.Context, dir string, o RegisterOptions) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	csr, keyPEM, err := agentca.NewCSR()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(agentapi.RegisterRequest{
		Token: o.Token, Node: o.Node, CSR: string(csr),
		AdvertiseHost: o.AdvertiseHost, AdvertisePort: o.AdvertisePort, Version: o.Version,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.Server, "/")+agentapi.PathRegister, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{RootCAs: o.RootCAs, InsecureSkipVerify: o.InsecureSkipVerify, MinVersion: tls.VersionTLS12}, //nolint:gosec // opt-in for development
	}}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register with %s: %w", o.Server, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = res.Status
		}
		return nil, fmt.Errorf("registration refused: %s", e.Message)
	}
	var rr agentapi.RegisterResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, fmt.Errorf("registration response: %w", err)
	}
	files := map[string][]byte{
		keyFile:  keyPEM,
		certFile: []byte(rr.CertPEM),
		caFile:   []byte(rr.CAPEM),
	}
	meta, _ := json.Marshal(map[string]string{"node_id": rr.NodeID, "server": o.Server})
	files[nodeFile] = meta
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	return LoadState(dir)
}
