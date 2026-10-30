// Package waker answers connections to paused and archived Free projects
// (V3 §4.2). A paused project's pooler route points here instead of at its
// shared cluster: the waker speaks just enough of the Postgres wire
// protocol to read the startup message, asks for the project to be woken,
// and refuses the connection with a message that says what is happening.
// The client's retry, once the project is back, goes to the database.
//
// It only ever sees connections from the poolers, which have already
// authenticated the client against the project's credentials, so it does
// not authenticate again. It should listen on an address only the poolers
// reach.
package waker

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// State is what waking a database found, which picks the message.
type State int

const (
	// Unknown: no paused or archived project has this database.
	Unknown State = iota
	// Resuming: a paused project is resuming (seconds).
	Resuming
	// Restoring: an archived project is being restored (minutes).
	Restoring
	// Awake: the project is active; the route is about to switch back.
	Awake
)

// Messages the client sees (V3 §4.2, §4.3).
const (
	MsgResuming  = "This project was paused for inactivity and is resuming. Retry in about 30 seconds."
	MsgRestoring = "This project was archived after long inactivity and is being restored from its archive. Retry in a few minutes."
	MsgAwake     = "This project is resuming. Retry in a few seconds."
	MsgUnknown   = "This database is not available."
)

// Message returns the client-facing message for st.
func Message(st State) string {
	switch st {
	case Resuming:
		return MsgResuming
	case Restoring:
		return MsgRestoring
	case Awake:
		return MsgAwake
	default:
		return MsgUnknown
	}
}

// Waker wakes the project behind a client-facing database name.
type Waker interface {
	Wake(ctx context.Context, database string) (State, error)
}

// Protocol request codes (Postgres frontend/backend protocol).
const (
	protocolV3    = 196608   // 3.0
	sslRequest    = 80877103 // 1234.5679
	gssencRequest = 80877104 // 1234.5680
	cancelRequest = 80877102 // 1234.5678
	maxStartup    = 10000    // as Postgres
)

// Server accepts connections and answers each with an error.
type Server struct {
	waker Waker
	log   *slog.Logger
	// MaxConns caps concurrent connections; Timeout bounds each one.
	MaxConns int
	Timeout  time.Duration
	// Allow, when set, decides which source addresses may connect: the
	// waker does not authenticate (PgBouncer checked the client's password
	// before routing here), so only the poolers should reach it. Others
	// are refused before anything is read or woken.
	Allow func(netip.Addr) bool

	mu    sync.Mutex
	ln    net.Listener
	slots chan struct{}
}

// New returns a server that wakes projects through w.
func New(w Waker, log *slog.Logger) *Server {
	return &Server{waker: w, log: log, MaxConns: 256, Timeout: 10 * time.Second}
}

// Serve accepts connections on ln until it is closed or ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.slots = make(chan struct{}, max(s.MaxConns, 1))
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		select {
		case s.slots <- struct{}{}:
			go func() {
				defer func() { <-s.slots }()
				s.handle(ctx, c)
			}()
		default:
			// Too many at once: refuse without waking anything.
			_ = c.SetWriteDeadline(time.Now().Add(time.Second))
			_ = writeError(c, "53300", "too many connections waiting for projects to resume; retry shortly")
			_ = c.Close()
		}
	}
}

// Addr is the address the server listens on, once serving.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(s.Timeout))
	if s.Allow != nil && !s.Allow(remoteAddr(c)) {
		s.log.Warn("waker: refused a connection from outside the poolers", "remote", c.RemoteAddr())
		_ = writeError(c, "28000", "only Xenibase's poolers may connect here")
		return
	}
	r := bufio.NewReader(c)
	params, err := readStartup(r, c)
	if err != nil {
		if !errors.Is(err, errCancel) && !errors.Is(err, io.EOF) {
			s.log.Debug("waker: startup", "remote", c.RemoteAddr(), "err", err)
		}
		return
	}
	db := params["database"]
	if db == "" {
		db = params["user"]
	}
	wctx, cancel := context.WithTimeout(ctx, s.Timeout/2)
	st, err := s.waker.Wake(wctx, db)
	cancel()
	if err != nil {
		s.log.Warn("waker: wake", "database", db, "err", err)
		// The project is still asleep; say so, so the client retries.
		if st == Unknown {
			st = Resuming
		}
	}
	if err := writeError(c, Code(st), Message(st)); err != nil {
		s.log.Debug("waker: reply", "database", db, "err", err)
	}
}

// remoteAddr is c's peer address (the zero Addr when it isn't TCP).
func remoteAddr(c net.Conn) netip.Addr {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return ta.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}

// AllowList admits loopback, this machine's own addresses (xenibase-server
// probes the waker, and in the compose bundle reaches it at its own
// network address), the addresses addrs returns (the poolers'), cached for
// a few seconds, and nets (XENIBASE_WAKER_ALLOW).
func AllowList(addrs func() []netip.Addr, nets []netip.Prefix) func(netip.Addr) bool {
	var mu sync.Mutex
	var cached map[netip.Addr]bool
	var at time.Time
	return func(a netip.Addr) bool {
		a = a.Unmap()
		if a.IsLoopback() {
			return true
		}
		for _, n := range nets {
			if n.Contains(a) {
				return true
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if cached == nil || time.Since(at) > 10*time.Second || !cached[a] && time.Since(at) > time.Second {
			cached = map[netip.Addr]bool{}
			for _, p := range localAddrs() {
				cached[p] = true
			}
			if addrs != nil {
				for _, p := range addrs() {
					cached[p.Unmap()] = true
				}
			}
			at = time.Now()
		}
		return cached[a]
	}
}

// localAddrs are the addresses of this machine's network interfaces.
func localAddrs() []netip.Addr {
	ifs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range ifs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}

// Code is the SQLSTATE for st. PgBouncer treats 57P03 (cannot_connect_now)
// as a server starting up and keeps the client waiting; that suits a
// project already awake (its route is about to switch back), but a
// project waking must say so, so it gets 08004 (the server rejected the
// connection), which PgBouncer passes on to the client.
func Code(st State) string {
	switch st {
	case Awake:
		return "57P03"
	case Unknown:
		return "3D000" // invalid_catalog_name
	default:
		return "08004"
	}
}

var errCancel = errors.New("cancel request")

// readStartup reads the startup message, declining TLS and GSS encryption
// on the way (the poolers fall back to plain text on a private network).
func readStartup(r *bufio.Reader, w io.Writer) (map[string]string, error) {
	for range 3 {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint32(hdr[:4]))
		code := binary.BigEndian.Uint32(hdr[4:])
		if n < 8 || n > maxStartup {
			return nil, fmt.Errorf("startup packet of %d bytes", n)
		}
		body := make([]byte, n-8)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		switch code {
		case sslRequest, gssencRequest:
			if _, err := w.Write([]byte{'N'}); err != nil {
				return nil, err
			}
			continue
		case cancelRequest:
			return nil, errCancel
		case protocolV3:
			return parseParams(body)
		default:
			return nil, fmt.Errorf("unsupported protocol %d.%d", code>>16, code&0xffff)
		}
	}
	return nil, errors.New("too many encryption requests")
}

func parseParams(body []byte) (map[string]string, error) {
	out := map[string]string{}
	parts := strings.Split(string(body), "\x00")
	// Pairs, then an empty terminator (and the trailing split element).
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "" {
			break
		}
		out[parts[i]] = parts[i+1]
	}
	return out, nil
}

// writeError sends a FATAL ErrorResponse.
func writeError(w io.Writer, code, msg string) error {
	var b []byte
	field := func(t byte, v string) {
		b = append(b, t)
		b = append(b, v...)
		b = append(b, 0)
	}
	field('S', "FATAL")
	field('V', "FATAL")
	field('C', code)
	field('M', msg)
	b = append(b, 0)
	out := make([]byte, 5, 5+len(b))
	out[0] = 'E'
	binary.BigEndian.PutUint32(out[1:], uint32(4+len(b)))
	_, err := w.Write(append(out, b...))
	return err
}
