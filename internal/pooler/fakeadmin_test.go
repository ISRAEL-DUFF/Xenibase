package pooler

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakeConsole is a PgBouncer admin console that answers SHOW commands from
// tables and records every other command.
type fakeConsole struct {
	addr   string
	mu     sync.Mutex
	tables map[string]fakeTable // "SHOW CLIENTS" -> rows
	cmds   []string
}

type fakeTable struct {
	cols []string
	rows [][]string
}

func startFakeConsole(t *testing.T, tables map[string]fakeTable) *fakeConsole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeConsole{addr: ln.Addr().String(), tables: tables}
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

func (f *fakeConsole) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeConsole) admin(t *testing.T) *Admin {
	t.Helper()
	a, err := NewAdmin("session", f.addr, "xenibase", "pw", "disable")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fakeConsole) serve(c net.Conn) {
	defer c.Close()
	be := pgproto3.NewBackend(c, c)
	for {
		m, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		if _, ok := m.(*pgproto3.StartupMessage); !ok {
			if _, err := c.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		}
		break
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
		tbl, isTable := f.tables[strings.ToUpper(cmd)]
		if !isTable {
			f.cmds = append(f.cmds, cmd)
		}
		f.mu.Unlock()
		if isTable {
			var rd pgproto3.RowDescription
			for _, col := range tbl.cols {
				rd.Fields = append(rd.Fields, pgproto3.FieldDescription{Name: []byte(col), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1})
			}
			be.Send(&rd)
			for _, r := range tbl.rows {
				var dr pgproto3.DataRow
				for _, v := range r {
					dr.Values = append(dr.Values, []byte(v))
				}
				be.Send(&dr)
			}
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SHOW")})
		} else {
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte(strings.Fields(cmd + " ")[0])})
		}
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := be.Flush(); err != nil {
			return
		}
	}
}
