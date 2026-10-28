package agentapi

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// Pooler host paths (V3 §2.1). Only agents run with a pooler directory
// serve them.
const (
	PathPoolerConfig   = "/v1/pooler/config"   // PUT a PoolerBundle
	PathPoolerExpected = "/v1/pooler/expected" // PUT a PoolerExpected
	PathPoolerStatus   = "/v1/pooler/status"   // GET a PoolerStatus
)

// PoolerFiles are the only files a pooler bundle may carry: the routes,
// the auth file, and the TLS pair the PgBouncers present.
var PoolerFiles = []string{"databases.ini", "userlist.txt", "hba.conf", "server.crt", "server.key"}

// PoolerBundle is a complete pooler configuration. Generation goes up each
// time the content changes; Hash is PoolerHash(Files).
type PoolerBundle struct {
	Generation int64             `json:"generation"`
	Hash       string            `json:"hash"`
	Files      map[string][]byte `json:"files"`
}

// PoolerExpected tells a pooler host the newest generation xenibase-server
// has rendered. A host serving an older one reports itself stale and
// fails keepalived's check, so it can't take the floating IP.
type PoolerExpected struct {
	Generation int64  `json:"generation"`
	Hash       string `json:"hash"`
}

// PoolerStatus is what a pooler host reports.
type PoolerStatus struct {
	Generation int64  `json:"generation"`
	Hash       string `json:"hash"`
	Expected   int64  `json:"expected"`
	Stale      bool   `json:"stale"`
	// Ready means both PgBouncers accept connections and the config is
	// current: exactly what keepalived's check asks.
	Ready       bool   `json:"ready"`
	Session     bool   `json:"session"`
	Transaction bool   `json:"transaction"`
	Reason      string `json:"reason,omitempty"`
	// VRRPState is keepalived's last reported state: MASTER, BACKUP,
	// FAULT, STOP, or "" before the first report.
	VRRPState string `json:"vrrp_state"`
	// ServerID is the provider's ID for this machine (a Hetzner server).
	ServerID string `json:"server_id,omitempty"`
	// ClaimError is the last failure to assign the floating IP to this
	// host after becoming MASTER.
	ClaimError string `json:"claim_error,omitempty"`
}

// PoolerHash is the SHA-256 over the bundle's file names and contents in
// name order, hex encoded.
func PoolerHash(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
		var size [8]byte
		l := uint64(len(files[n]))
		for i := range size {
			size[i] = byte(l >> (56 - 8*i))
		}
		h.Write(size[:])
		h.Write(files[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}
