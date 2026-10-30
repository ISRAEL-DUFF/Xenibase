package agentapi

import "time"

// PathClientSerials is where xenibase-server pushes the client certificate
// serials an agent accepts (PUT, a ClientSerials body).
const PathClientSerials = "/v1/client-serials"

// ErrClientSerialNotAllowed is the Error an agent answers 403 with when the
// caller's client certificate is not on the list it was last given. The
// server pushes the list again and retries once.
const ErrClientSerialNotAllowed = "client certificate serial is not on this agent's allow list"

// ClientSerials is the allow list as pushed: List, JSON-encoded, and the
// agent CA's ECDSA signature over its SHA-256 (agentca.SignList). The
// agent checks the signature against the CA it pinned at registration, so
// only a holder of the CA key can change what it accepts, whichever
// connection the list arrives on.
type ClientSerials struct {
	List      []byte `json:"list"`
	Signature []byte `json:"signature"`
}

// ClientSerialList is the signed content. Generation rises with every
// change in the metadata database; an agent refuses a list older than the
// one it has.
type ClientSerialList struct {
	Generation int64          `json:"generation"`
	SignedAt   time.Time      `json:"signed_at"`
	Serials    []ClientSerial `json:"serials"`
}

// ClientSerial is one live client certificate.
type ClientSerial struct {
	Serial    string    `json:"serial"` // lower-case hex
	Server    string    `json:"server"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ClientSerialsAck is the agent's answer: the generation it now holds.
// With 409 it is a newer one than was pushed, which it kept.
type ClientSerialsAck struct {
	Generation int64 `json:"generation"`
}
