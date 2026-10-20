// Package pgsourcetest is a fake Postgres that behaves like a PgBouncer with
// the default ignore list: it refuses a startup message that carries
// default_transaction_read_only (or any other parameter the tests name) and
// otherwise answers a simple query with a bare success.
package pgsourcetest

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Pooler is the fake server.
type Pooler struct {
	Addr string

	mu      sync.Mutex
	queries []string
}

// Queries are the simple queries the server was sent, in order.
func (p *Pooler) Queries() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.queries...)
}

// Start serves until the test ends. Startup parameters named in refuse are
// answered with "unsupported startup parameter", as PgBouncer does.
func Start(t *testing.T, refuse ...string) *Pooler {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &Pooler{Addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c, refuse)
		}
	}()
	return p
}

func (p *Pooler) serve(c net.Conn, refuse []string) {
	defer c.Close()
	be := pgproto3.NewBackend(c, c)
	for {
		m, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		switch m := m.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := c.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		case *pgproto3.StartupMessage:
			for _, name := range refuse {
				if _, ok := m.Parameters[name]; ok {
					be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "08P01", Message: "unsupported startup parameter: " + name})
					_ = be.Flush()
					return
				}
			}
			be.Send(&pgproto3.AuthenticationOk{})
			be.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "17.0"})
			be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := be.Flush(); err != nil {
				return
			}
		default:
			return
		}
		break
	}
	for {
		m, err := be.Receive()
		if err != nil {
			return
		}
		switch m := m.(type) {
		case *pgproto3.Query:
			p.mu.Lock()
			p.queries = append(p.queries, m.String)
			p.mu.Unlock()
			tag := strings.ToUpper(strings.Fields(m.String + " ")[0])
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := be.Flush(); err != nil {
				return
			}
		case *pgproto3.Terminate:
			return
		default:
			return
		}
	}
}
