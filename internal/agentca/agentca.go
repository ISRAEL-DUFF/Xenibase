// Package agentca is the control plane's private CA for agent mTLS (spec
// §3.3): it signs each agent's certificate at registration and a client
// certificate for xenibase-server itself. Agents accept only that client
// certificate; the server accepts only agents whose certificate it signed
// and whose fingerprint it pinned.
package agentca

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store"
)

const (
	settingsKey = "agent_ca"
	keyAAD      = "settings.agent_ca.key"
	// ServerCN is the subject of xenibase-server's client certificate.
	ServerCN = "xenibase-server"
	// NodeCNPrefix prefixes agent certificate subjects: "node:<uuid>".
	NodeCNPrefix = "node:"
)

// CA is the agent certificate authority.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
}

type stored struct {
	CertPEM   string `json:"cert_pem"`
	KeySealed []byte `json:"key_sealed"`
}

// LoadOrCreate loads the CA from the settings table, creating it on first
// use. Its private key is sealed with the master key (spec §7.3).
func LoadOrCreate(ctx context.Context, db *pgxpool.Pool, keyring *crypto.Keyring) (*CA, error) {
	return loadOrCreate(ctx, db, keyring)
}

func loadOrCreate(ctx context.Context, db *pgxpool.Pool, keyring *crypto.Keyring) (*CA, error) {
	var ca *CA
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('xenibase_agent_ca'))`); err != nil {
			return err
		}
		q := store.New(tx)
		raw, err := q.GetSetting(ctx, settingsKey)
		if err == nil {
			var s stored
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			keyDER, err := keyring.Decrypt(s.KeySealed, []byte(keyAAD))
			if err != nil {
				return fmt.Errorf("agent CA key: %w", err)
			}
			ca, err = parse([]byte(s.CertPEM), keyDER)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		ca, err = generate()
		if err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(ca.key)
		if err != nil {
			return err
		}
		sealed, err := keyring.Encrypt(keyDER, []byte(keyAAD))
		if err != nil {
			return err
		}
		b, _ := json.Marshal(stored{CertPEM: string(ca.certPEM), KeySealed: sealed})
		return q.PutSetting(ctx, store.PutSettingParams{Key: settingsKey, Value: b})
	})
	return ca, err
}

func generate() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Xenibase agent CA", Organization: []string{"Xenibase"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{cert: cert, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key: key}, nil
}

func parse(certPEM, keyDER []byte) (*CA, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return nil, errors.New("agent CA: bad certificate")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("agent CA: unexpected key type")
	}
	return &CA{cert: cert, certPEM: certPEM, key: key}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// CertPEM is the CA certificate agents pin.
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// Pool is a cert pool holding only this CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// SignAgent signs an agent's CSR for nodeID. hosts become the certificate's
// SANs (the address the server dials). The certificate serves TLS and is
// valid for two years; re-registration replaces it.
func (ca *CA) SignAgent(csrPEM []byte, nodeID string, hosts []string) (certPEM []byte, fingerprint string, err error) {
	b, _ := pem.Decode(csrPEM)
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return nil, "", errors.New("agent CA: not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("agent CA: bad CSR signature: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: NodeCNPrefix + nodeID, Organization: []string{"Xenibase agent"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Fingerprint(der), nil
}

// ServerClientCertValidity is how long xenibase-server's client certificate
// is valid. The server renews it well before then (nodes.Service).
const ServerClientCertValidity = 30 * 24 * time.Hour

// ServerClientCert issues xenibase-server's client certificate, valid for
// ServerClientCertValidity from now.
func (ca *CA) ServerClientCert() (tls.Certificate, error) {
	return ca.ServerClientCertAt(time.Now())
}

// ServerClientCertAt is ServerClientCert issued as at now.
func (ca *CA) ServerClientCertAt(now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: ServerCN, Organization: []string{"Xenibase"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(ServerClientCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// Fingerprint is the SHA-256 of a DER certificate, as pinned in
// nodes.agent_cert_fp.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NodeIDFromCert extracts the node ID from an agent certificate.
func NodeIDFromCert(c *x509.Certificate) (string, bool) {
	return strings.CutPrefix(c.Subject.CommonName, NodeCNPrefix)
}

// NewCSR creates a key and certificate request for an agent.
func NewCSR() (csrPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "xenibase-agent"},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
