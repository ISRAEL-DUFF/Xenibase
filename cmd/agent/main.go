// Command xenibase-agent runs on each node and executes xenibase-server's
// commands over mutual TLS: health, host metrics, and pg_dump/pg_restore
// based dumps, restores, and copies (spec §3.2). Dedicated instances arrive
// in M4.
//
//	xenibase-agent register --server https://xenibase.example.com --token <one-time>
//	xenibase-agent run
//	xenibase-agent decrypt --key-file xenibase-backup-key.txt < x.dump.enc > x.dump
//
// Environment variables mirror the flags (XENIBASE_AGENT_*).
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/israel-duff/xenibase/internal/agentsvc"
	"github.com/israel-duff/xenibase/internal/backupfmt"
	"github.com/israel-duff/xenibase/internal/floatip"
	"github.com/israel-duff/xenibase/internal/logging"
	"github.com/israel-duff/xenibase/internal/version"
)

// DefaultPGImage is the instance image this release was built and tested
// with (deploy/images/postgres).
// DefaultPGImage is the instance image, {major} replaced by each
// instance's Postgres major (V3 §2.4).
const DefaultPGImage = "xenibase-postgres:{major}-walg3.0.9"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "xenibase-agent:", err)
		os.Exit(1)
	}
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type opts struct {
	state, listen, server, token, bootstrap, node, advertise, caFile, pgBin, diskPath string
	dockerHost, image, network, publish, dbAllow, moveAllow, etcdImage                string
	insecure                                                                          bool

	// Pooler hosts (V3 §2.1).
	poolerDir, poolerSession, poolerPooled, poolerLocal string
	serverID, hetznerToken, hetznerAPI, floatingIP      string
	poolerPidFiles                                      string
}

func flags(name string, args []string) (*opts, error) {
	o := &opts{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&o.state, "state", env("XENIBASE_AGENT_STATE_DIR", "/var/lib/xenibase-agent"), "state directory (key, certificate, pinned CA)")
	fs.StringVar(&o.listen, "listen", env("XENIBASE_AGENT_LISTEN", ":7070"), "mTLS listen address")
	fs.StringVar(&o.server, "server", env("XENIBASE_AGENT_SERVER", ""), "xenibase-server URL, for registration")
	fs.StringVar(&o.token, "token", env("XENIBASE_AGENT_TOKEN", ""), "one-time registration token")
	fs.StringVar(&o.bootstrap, "bootstrap-token", env("XENIBASE_AGENT_BOOTSTRAP_TOKEN", ""), "install-time bootstrap token (with --node)")
	fs.StringVar(&o.node, "node", env("XENIBASE_AGENT_NODE", ""), "node name, with --bootstrap-token")
	fs.StringVar(&o.advertise, "advertise", env("XENIBASE_AGENT_ADVERTISE", ""), "host[:port] xenibase-server dials to reach this agent")
	fs.StringVar(&o.caFile, "server-ca", env("XENIBASE_AGENT_SERVER_CA", ""), "PEM file to verify xenibase-server's HTTPS certificate")
	fs.BoolVar(&o.insecure, "insecure-skip-verify", env("XENIBASE_AGENT_INSECURE_SKIP_VERIFY", "") == "true", "do not verify xenibase-server's HTTPS certificate (development only)")
	fs.StringVar(&o.pgBin, "pg-bin", env("XENIBASE_AGENT_PG_BIN", ""), "directory with pg_dump and pg_restore (default: $PATH)")
	fs.StringVar(&o.diskPath, "disk-path", env("XENIBASE_AGENT_DISK_PATH", "/"), "path whose disk usage is reported")
	fs.StringVar(&o.dockerHost, "docker", env("XENIBASE_AGENT_DOCKER", env("DOCKER_HOST", "")), "Docker daemon for instances (default: the local socket)")
	fs.StringVar(&o.image, "pg-image", env("XENIBASE_AGENT_PG_IMAGE", DefaultPGImage), "Postgres + WAL-G image for instances")
	fs.StringVar(&o.network, "network", env("XENIBASE_AGENT_NETWORK", ""), "Docker network instances join (reached by container name)")
	fs.StringVar(&o.publish, "publish", env("XENIBASE_AGENT_PUBLISH", ""), "node address to publish instance ports on (e.g. its private IP)")
	fs.StringVar(&o.dbAllow, "db-allow", env("XENIBASE_AGENT_DB_ALLOW", defaultDBAllow),
		"comma-separated CIDRs new instances accept logins from: the control plane and poolers (spec §7.1)")
	fs.StringVar(&o.etcdImage, "etcd-image", env("XENIBASE_AGENT_ETCD_IMAGE", agentsvc.DefaultEtcdImage), "image of the node's etcd member (HA instances)")
	fs.StringVar(&o.moveAllow, "move-allow", env("XENIBASE_AGENT_MOVE_ALLOW", defaultMoveAllow),
		"comma-separated CIDRs other instances replicate from during a move; only the moves' own logins may use them (V3 §2.3)")
	fs.StringVar(&o.poolerDir, "pooler-dir", env("XENIBASE_AGENT_POOLER_DIR", ""), "pooler host: directory the PgBouncers read their xenibase files from (enables pooler mode)")
	fs.StringVar(&o.poolerSession, "pooler-session-addr", env("XENIBASE_AGENT_POOLER_SESSION_ADDR", "127.0.0.1:5432"), "pooler host: the session PgBouncer, checked for readiness")
	fs.StringVar(&o.poolerPooled, "pooler-pooled-addr", env("XENIBASE_AGENT_POOLER_POOLED_ADDR", "127.0.0.1:6543"), "pooler host: the transaction PgBouncer, checked for readiness")
	fs.StringVar(&o.poolerLocal, "pooler-local", env("XENIBASE_AGENT_POOLER_LOCAL", "127.0.0.1:7071"), "pooler host: loopback address for keepalived's check and notify calls")
	fs.StringVar(&o.serverID, "server-id", env("XENIBASE_AGENT_SERVER_ID", ""), "pooler host: the provider's ID for this machine, or \"hetzner-metadata\" to ask Hetzner's metadata service")
	fs.StringVar(&o.hetznerToken, "hetzner-token", env("XENIBASE_AGENT_HETZNER_TOKEN", ""), "pooler host: Hetzner Cloud API token for assigning the floating IP")
	fs.StringVar(&o.hetznerAPI, "hetzner-api", env("XENIBASE_AGENT_HETZNER_API", floatip.DefaultHetznerAPI), "pooler host: Hetzner Cloud API base URL")
	fs.StringVar(&o.poolerPidFiles, "pooler-pidfiles", env("XENIBASE_AGENT_POOLER_PIDFILES", ""), "pooler host: comma-separated PgBouncer PID files, signalled to reload after each new configuration")
	fs.StringVar(&o.floatingIP, "floating-ip-id", env("XENIBASE_AGENT_FLOATING_IP_ID", ""), "pooler host: the floating IP's ID; empty when keepalived alone moves the address")
	return o, fs.Parse(args)
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: xenibase-agent register|run|decrypt|pooler-seed|keepalived-config|version [flags]")
	}
	switch args[0] {
	case "version", "-version", "--version":
		v := version.Get()
		fmt.Printf("xenibase-agent %s (commit %s, built %s, %s)\n", v.Version, v.Commit, v.BuildDate, v.GoVersion)
		return nil
	case "decrypt":
		return decrypt(args[1:])
	case "pooler-seed":
		return poolerSeed(args[1:])
	case "keepalived-config":
		return keepalivedConfig(args[1:])
	case "register":
		o, err := flags("register", args[1:])
		if err != nil {
			return err
		}
		st, err := register(context.Background(), o)
		if err != nil {
			return err
		}
		fmt.Printf("registered as node %s; state in %s\n", st.NodeID, st.Dir)
		return nil
	case "run":
		o, err := flags("run", args[1:])
		if err != nil {
			return err
		}
		return serve(o)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func register(ctx context.Context, o *opts) (*agentsvc.State, error) {
	if o.server == "" {
		return nil, errors.New("--server is required to register")
	}
	token := o.token
	if token == "" {
		token = o.bootstrap
	}
	if token == "" {
		return nil, errors.New("--token (or --bootstrap-token with --node) is required to register")
	}
	host, port, err := advertise(o)
	if err != nil {
		return nil, err
	}
	ro := agentsvc.RegisterOptions{
		Server: o.server, Token: token, AdvertiseHost: host, AdvertisePort: port,
		Version: version.Get().Version, InsecureSkipVerify: o.insecure,
	}
	if o.token == "" {
		ro.Node = o.node
	}
	if o.caFile != "" {
		pemBytes, err := os.ReadFile(o.caFile)
		if err != nil {
			return nil, err
		}
		ro.RootCAs = x509.NewCertPool()
		ro.RootCAs.AppendCertsFromPEM(pemBytes)
	}
	return agentsvc.Register(ctx, o.state, ro)
}

func advertise(o *opts) (string, int, error) {
	_, lport, _ := net.SplitHostPort(o.listen)
	port, _ := strconv.Atoi(lport)
	if o.advertise == "" {
		h, err := os.Hostname()
		return h, port, err
	}
	h, p, err := net.SplitHostPort(o.advertise)
	if err != nil {
		return o.advertise, port, nil //nolint:nilerr // a bare host
	}
	port, err = strconv.Atoi(p)
	return h, port, err
}

func serve(o *opts) error {
	log := logging.New(os.Stderr, env("XENIBASE_AGENT_LOG_FORMAT", "json"), slog.LevelInfo)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := agentsvc.LoadState(o.state)
	if errors.Is(err, fs.ErrNotExist) {
		// First start: register if given a token (install bundles do this).
		log.Info("not registered yet; registering", "server", o.server, "node", o.node)
		for attempt := 1; ; attempt++ {
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			st, err = register(rctx, o)
			cancel()
			if err == nil || attempt == 30 || ctx.Err() != nil {
				break
			}
			log.Warn("registration failed; retrying", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	moveCIDRs, err := parseCIDRs(o.moveAllow)
	if err != nil {
		return err
	}
	cidrs, err := parseCIDRs(o.dbAllow)
	if err != nil {
		return err
	}
	if o.dbAllow == defaultDBAllow && o.poolerDir == "" {
		// The agent can't tell which private addresses are the platform's.
		log.Warn("XENIBASE_AGENT_DB_ALLOW is not set: new instances accept SCRAM logins from every private range, "+
			"past projects' IP allowlists; set it to the networks of xenibase-server, the poolers and the other nodes "+
			"(docs/network-access.md)", "db_allow", o.dbAllow)
	}
	svc := agentsvc.New(agentsvc.Config{
		Version: version.Get().Version, NodeID: st.NodeID, PGBinDir: o.pgBin, DiskPath: o.diskPath,
		Instances: agentsvc.InstanceConfig{Docker: o.dockerHost, Image: o.image, Network: o.network, PublishAddr: o.publish, HBAAllow: cidrs, MoveAllow: moveCIDRs, EtcdImage: o.etcdImage},
	}, log)
	if o.poolerDir != "" {
		if err := enablePooler(ctx, svc, o, log); err != nil {
			return err
		}
	}
	go svc.EnsureMoveRules(ctx)
	log.Info("xenibase-agent listening", "addr", ln.Addr().String(), "node_id", st.NodeID, "version", version.Get().Version)
	return svc.Serve(ctx, ln, agentsvc.TLSConfig(st.Cert, st.CA))
}

// enablePooler turns on pooler-host mode and the loopback API keepalived
// calls (V3 §2.1).
func enablePooler(ctx context.Context, svc *agentsvc.Service, o *opts, log *slog.Logger) error {
	serverID := o.serverID
	if serverID == "hetzner-metadata" {
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		id, err := floatip.HetznerServerID(mctx)
		cancel()
		if err != nil {
			return fmt.Errorf("--server-id hetzner-metadata: %w", err)
		}
		serverID = id
	}
	var fip floatip.Provider = floatip.None{}
	if o.floatingIP != "" {
		if o.hetznerToken == "" || serverID == "" {
			return errors.New("--floating-ip-id needs --hetzner-token and --server-id")
		}
		fip = &floatip.Hetzner{API: o.hetznerAPI, Token: o.hetznerToken, IPID: o.floatingIP}
	}
	var pidfiles []string
	for _, f := range strings.Split(o.poolerPidFiles, ",") {
		if f = strings.TrimSpace(f); f != "" {
			pidfiles = append(pidfiles, f)
		}
	}
	if err := svc.EnablePooler(agentsvc.PoolerConfig{
		Dir: o.poolerDir, SessionAddr: o.poolerSession, PooledAddr: o.poolerPooled, ServerID: serverID, FloatIP: fip, PidFiles: pidfiles,
	}); err != nil {
		return err
	}
	go func() {
		if err := svc.ServeLocal(ctx, o.poolerLocal); err != nil {
			log.Error("pooler local API stopped", "err", err)
		}
	}()
	log.Info("pooler host mode", "dir", o.poolerDir, "local", o.poolerLocal, "server_id", serverID, "floating_ip", o.floatingIP)
	return nil
}

// poolerSeed writes placeholder PgBouncer files on a pooler host so the
// PgBouncers start before xenibase-server's first push.
func poolerSeed(args []string) error {
	fs := flag.NewFlagSet("pooler-seed", flag.ContinueOnError)
	dir := fs.String("dir", env("XENIBASE_AGENT_POOLER_DIR", ""), "the PgBouncers' xenibase directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("pooler-seed: --dir is required")
	}
	return agentsvc.SeedPoolerDir(*dir, 0o640)
}

// keepalivedConfig prints keepalived.conf for this pooler host.
func keepalivedConfig(args []string) error {
	fs := flag.NewFlagSet("keepalived-config", flag.ContinueOnError)
	c := agentsvc.KeepalivedConfig{}
	fs.StringVar(&c.Interface, "interface", env("XENIBASE_EDGE_INTERFACE", "eth0"), "interface for VRRP adverts (and the VIP)")
	fs.StringVar(&c.SelfIP, "self", env("XENIBASE_EDGE_SELF_IP", ""), "this host's private IP")
	fs.StringVar(&c.PeerIP, "peer", env("XENIBASE_EDGE_PEER_IP", ""), "the other pooler host's private IP")
	fs.StringVar(&c.VIP, "vip", env("XENIBASE_EDGE_VIP", ""), "address keepalived adds on becoming MASTER (the floating IP)")
	routerID, priority := env("XENIBASE_EDGE_ROUTER_ID", "51"), env("XENIBASE_EDGE_PRIORITY", "100")
	fs.StringVar(&routerID, "router-id", routerID, "VRRP router ID (1-255, the same on both hosts)")
	fs.StringVar(&priority, "priority", priority, "VRRP priority (1-254)")
	fs.StringVar(&c.Password, "password", env("XENIBASE_EDGE_VRRP_PASSWORD", ""), "VRRP password, at most 8 characters (the same on both hosts)")
	local := env("XENIBASE_AGENT_POOLER_LOCAL", "127.0.0.1:7071")
	fs.StringVar(&local, "local", local, "the agent's loopback pooler API")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var err error
	if c.RouterID, err = strconv.Atoi(routerID); err != nil {
		return fmt.Errorf("--router-id: %w", err)
	}
	if c.Priority, err = strconv.Atoi(priority); err != nil {
		return fmt.Errorf("--priority: %w", err)
	}
	c.CheckURL, c.NotifyURL = "http://"+local+"/ready", "http://"+local
	out, err := agentsvc.RenderKeepalived(c)
	if err != nil {
		return fmt.Errorf("keepalived-config: %w", err)
	}
	_, err = os.Stdout.Write(out)
	return err
}

// decrypt turns a backup object back into a pg_dump archive with the
// downloaded backup key, without xenibase-server: the disaster-recovery path
// for metadata self-backups (docs/disaster-recovery.md).
func decrypt(args []string) error {
	fs := flag.NewFlagSet("decrypt", flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "the backup key file downloaded from Xenibase")
	in := fs.String("in", "-", "encrypted object (- for stdin)")
	out := fs.String("out", "-", "pg_dump archive to write (- for stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyFile == "" {
		return errors.New("decrypt: --key-file is required")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	key, err := backupfmt.DecodeKey(string(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", *keyFile, err)
	}
	r, w := io.Reader(os.Stdin), io.Writer(os.Stdout)
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	var outFile *os.File
	if *out != "-" {
		if outFile, err = os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			return err
		}
		w = outFile
	}
	dec, err := backupfmt.OpenWithBackupKey(r, key)
	if err == nil {
		_, err = io.Copy(w, dec)
	}
	if outFile != nil {
		if cerr := outFile.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(*out)
		}
	}
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}
	return nil
}

// defaultDBAllow is every private range: the control plane and poolers
// reach instances over a private network.
const defaultDBAllow = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.1/32"

// defaultMoveAllow: other nodes and the Docker networks, on private ranges.
const defaultMoveAllow = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"

func parseCIDRs(s string) ([]string, error) {
	var out []string
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("-db-allow: %q is not a CIDR: %w", c, err)
		}
		out = append(out, p.Masked().String())
	}
	return out, nil
}
