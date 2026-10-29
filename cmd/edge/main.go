// Command xenibase-edge is the backend services gateway (V4 §2.1): one per
// region, serving every project with backend services at
// https://<ref>.<domain>. It is stateless: it follows xenibase-server's
// configuration feed and reports usage back.
//
//	xenibase-edge run
//	xenibase-edge version
//
// Configuration is by environment; see docs/backend-services.md.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/logging"
	"github.com/israel-duff/xenibase/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "xenibase-edge:", err)
		os.Exit(1)
	}
}

// renderASFactor sizes a render worker's address-space cap from its
// memory: the WebAssembly decoders reserve far more address space than
// they use.
const renderASFactor = 16

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(args []string) error {
	cmd := "run"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "version", "--version", "-version":
		v := version.Get()
		fmt.Printf("xenibase-edge %s (commit %s, built %s, %s)\n", v.Version, v.Commit, v.BuildDate, v.GoVersion)
		return nil
	case "render-worker":
		// A child of the edge that renders images (V4.1 §12.4).
		return edge.RunRenderWorker(os.Stdin, os.Stdout)
	case "run":
	default:
		return fmt.Errorf("unknown command %q: use run or version", cmd)
	}
	host, _ := os.Hostname()
	cfg := edge.Config{
		Name:          env("XENIBASE_EDGE_NAME", host),
		ControlURL:    os.Getenv("XENIBASE_EDGE_CONTROL_URL"),
		Secret:        os.Getenv("XENIBASE_EDGE_SECRET"),
		Region:        os.Getenv("XENIBASE_EDGE_REGION"),
		Domain:        os.Getenv("XENIBASE_EDGE_DOMAIN"),
		PoolerAddr:    os.Getenv("XENIBASE_EDGE_POOLER_ADDR"),
		SessionAddr:   os.Getenv("XENIBASE_EDGE_SESSION_ADDR"),
		PoolerSSLMode: env("XENIBASE_EDGE_POOLER_SSLMODE", "require"),
		// verify-full with the poolers' CA when their certificate is
		// self-signed or private (docs/backend-services.md).
		PoolerRootCert: os.Getenv("XENIBASE_EDGE_POOLER_ROOT_CERT"),
	}
	switch cfg.PoolerSSLMode {
	case "require", "verify-ca", "verify-full":
	default:
		// prefer and below would fall back to plain text, with the
		// service logins' sessions on it.
		return fmt.Errorf("XENIBASE_EDGE_POOLER_SSLMODE %q: use require, verify-ca or verify-full", cfg.PoolerSSLMode)
	}
	if os.Getenv("XENIBASE_EDGE_RENDER_IN_PROCESS") == "" {
		// Image transforms in child processes (V4.1 §12.4), each capped.
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("find xenibase-edge's own binary for render workers (or set XENIBASE_EDGE_RENDER_IN_PROCESS=1): %w", err)
		}
		mb, err := strconv.Atoi(env("XENIBASE_EDGE_RENDER_MEMORY_MB", "512"))
		if err != nil || mb < 64 {
			return errors.New("XENIBASE_EDGE_RENDER_MEMORY_MB is each render worker's memory in MB, at least 64 (default 512)")
		}
		cfg.RenderWorker = []string{self, "render-worker"}
		cfg.RenderWorkerEnv = []string{fmt.Sprintf("GOMEMLIMIT=%dMiB", mb),
			fmt.Sprintf("%s=%d", edge.RenderWorkerASEnv, int64(mb)*renderASFactor<<20)}
	}
	if mb := os.Getenv("XENIBASE_EDGE_CACHE_MB"); mb != "" {
		n, err := strconv.Atoi(mb)
		if err != nil || n < 1 {
			return errors.New("XENIBASE_EDGE_CACHE_MB is the anonymous-read cache's size in MB (default 64)")
		}
		cfg.CacheBytes = n << 20
	}
	switch {
	case cfg.ControlURL == "":
		return errors.New("XENIBASE_EDGE_CONTROL_URL is required (xenibase-server's URL)")
	case len(cfg.Secret) < 32:
		return errors.New("XENIBASE_EDGE_SECRET is required: at least 32 characters, the same as xenibase-server's")
	case cfg.Domain == "":
		return errors.New("XENIBASE_EDGE_DOMAIN is required (projects are served at <ref>.<domain>)")
	}
	for _, s := range strings.Split(os.Getenv("XENIBASE_EDGE_TRUSTED_PROXIES"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("XENIBASE_EDGE_TRUSTED_PROXIES: %w", err)
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, p)
	}
	log := logging.New(os.Stderr, env("XENIBASE_EDGE_LOG_FORMAT", "json"), slog.LevelInfo)
	cfg.Log = log

	e := edge.New(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.Run(ctx) }()

	certFile, keyFile := os.Getenv("XENIBASE_EDGE_TLS_CERT"), os.Getenv("XENIBASE_EDGE_TLS_KEY")
	listen := env("XENIBASE_EDGE_LISTEN", ":8443")
	srv := &http.Server{Addr: listen, Handler: e, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 64 << 10}
	errc := make(chan error, 1)
	go func() {
		if certFile == "" {
			log.Warn("serving plain HTTP: put a TLS terminator in front, or set XENIBASE_EDGE_TLS_CERT and XENIBASE_EDGE_TLS_KEY", "listen", listen)
			errc <- srv.ListenAndServe()
			return
		}
		certs := &reloadingCert{cert: certFile, key: keyFile}
		if _, err := certs.get(nil); err != nil {
			errc <- err
			return
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.get}
		log.Info("xenibase-edge listening", "listen", listen, "domain", cfg.Domain, "region", cfg.Region)
		errc <- srv.ListenAndServeTLS("", "")
	}()
	select {
	case err := <-errc:
		stop()
		wg.Wait()
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	wg.Wait()
	return nil
}

// reloadingCert serves the wildcard certificate from files, re-reading them
// when they change (a DNS-01 renewal writes new ones).
type reloadingCert struct {
	cert, key string
	mu        sync.Mutex
	loaded    *tls.Certificate
	modTime   time.Time
	checked   time.Time
}

func (r *reloadingCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded != nil && time.Since(r.checked) < time.Minute {
		return r.loaded, nil
	}
	r.checked = time.Now()
	st, err := os.Stat(r.cert)
	if err != nil {
		if r.loaded != nil {
			return r.loaded, nil
		}
		return nil, err
	}
	if r.loaded != nil && st.ModTime().Equal(r.modTime) {
		return r.loaded, nil
	}
	c, err := tls.LoadX509KeyPair(r.cert, r.key)
	if err != nil {
		if r.loaded != nil {
			return r.loaded, nil // keep serving the old one
		}
		return nil, err
	}
	r.loaded, r.modTime = &c, st.ModTime()
	return r.loaded, nil
}
