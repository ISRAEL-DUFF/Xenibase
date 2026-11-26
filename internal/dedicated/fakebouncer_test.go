package dedicated_test

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/israel-duff/xenibase/internal/pooler"
)

// fakeBouncer is a PgBouncer admin console that acknowledges every command
// and records them.
type fakeBouncer struct {
	addr string
	mu   sync.Mutex
	cmds []string
}

func startFakeBouncer(t *testing.T) *fakeBouncer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeBouncer{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeBouncer) admin(t *testing.T) *pooler.Admin {
	t.Helper()
	a, err := pooler.NewAdmin("transaction", f.addr, "xenibase", "pw", "disable")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// count is how many times cmd was received.
func (f *fakeBouncer) count(cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if c == cmd {
			n++
		}
	}
	return n
}

func (f *fakeBouncer) serve(c net.Conn) {
	defer c.Close()
	be := pgproto3.NewBackend(c, c)
	for {
		m, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		if _, ok := m.(*pgproto3.StartupMessage); ok {
			break
		}
		if _, err := c.Write([]byte{'N'}); err != nil { // no TLS
			return
		}
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "1.24.1/bouncer"})
	be.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	be.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		return
	}
	for {
		m, err := be.Receive()
		if err != nil {
			return
		}
		q, ok := m.(*pgproto3.Query)
		if !ok {
			return
		}
		cmd := strings.TrimSpace(q.String)
		f.mu.Lock()
		f.cmds = append(f.cmds, cmd)
		f.mu.Unlock()
		be.Send(&pgproto3.CommandComplete{CommandTag: []byte(strings.Fields(cmd + " ")[0])})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := be.Flush(); err != nil {
			return
		}
	}
}
