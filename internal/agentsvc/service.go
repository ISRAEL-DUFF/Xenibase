// Package agentsvc is xenibase-agent's HTTPS service: a fixed set of commands
// (health, host metrics, dump, restore, copy) behind mutual TLS that only
// xenibase-server's client certificate can pass (spec §3.3).
package agentsvc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/backupfmt"
	"github.com/israel-duff/xenibase/internal/pgpstream"
	"github.com/israel-duff/xenibase/internal/storage"
)

// Config configures the service.
type Config struct {
	Version string
	NodeID  string
	// PGBinDir holds pg_dump and pg_restore; empty means $PATH.
	PGBinDir string
	// DiskPath is reported in host metrics (the Postgres data volume).
	DiskPath string
	// MaxJobs bounds concurrent dumps, restores, copies, and base backups.
	MaxJobs int
	// Instances configures Postgres containers (dedicated tier).
	Instances InstanceConfig
	// ClientAllow is the client certificate serials the agent accepts
	// (LoadClientAllow). Nil accepts every xenibase-server certificate the
	// CA signed, and the agent takes no allow list.
	ClientAllow *ClientAllow
}

// Service implements the agent API.
type Service struct {
	cfg  Config
	log  *slog.Logger
	jobs chan struct{}
	inst *instances
	pool *poolerHost // nil unless this is a pooler host (V3 §2.1)
}

// EnablePooler makes this agent a pooler host's agent.
func (s *Service) EnablePooler(cfg PoolerConfig) error {
	p, err := newPoolerHost(cfg, s.log)
	if err != nil {
		return err
	}
	s.pool = p
	return nil
}

// New returns a Service.
func New(cfg Config, log *slog.Logger) *Service {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 2
	}
	if cfg.DiskPath == "" {
		cfg.DiskPath = "/"
	}
	return &Service{cfg: cfg, log: log, jobs: make(chan struct{}, cfg.MaxJobs), inst: newInstances(cfg.Instances)}
}

// EnsureMoveRules gives running instances the pg_hba.conf rules moves need
// (V3 §2.3); the agent calls it once at start.
func (s *Service) EnsureMoveRules(ctx context.Context) { s.inst.ensureMoveRules(ctx, s.log) }

// Handler returns the API's HTTP handler.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+agentapi.PathHealth, s.health)
	mux.HandleFunc("GET "+agentapi.PathMetrics, s.metrics)
	mux.HandleFunc("POST "+agentapi.PathDump, s.dump)
	mux.HandleFunc("POST "+agentapi.PathRestore, s.restore)
	mux.HandleFunc("POST "+agentapi.PathCopy, s.copy)
	mux.HandleFunc("POST "+agentapi.PathInstances, s.createInstance)
	mux.HandleFunc("GET "+agentapi.PathInstance, s.getInstance)
	mux.HandleFunc("DELETE "+agentapi.PathInstance, s.instanceAction("destroy"))
	mux.HandleFunc("POST "+agentapi.PathInstanceStart, s.instanceAction("start"))
	mux.HandleFunc("POST "+agentapi.PathInstanceStop, s.instanceAction("stop"))
	mux.HandleFunc("POST "+agentapi.PathWALGBackup, s.walgBackup)
	mux.HandleFunc("GET "+agentapi.PathWALGBackupList, s.walgBackups)
	mux.HandleFunc("GET "+agentapi.PathEtcdAddress, s.etcdAddressHandler)
	mux.HandleFunc("PUT "+agentapi.PathEtcd, s.runEtcd)
	mux.HandleFunc("GET "+agentapi.PathEtcd, s.getEtcd)
	mux.HandleFunc("DELETE "+agentapi.PathEtcd, s.removeEtcd)
	mux.HandleFunc("POST "+agentapi.PathEtcdMembers, s.etcdMembers)
	mux.HandleFunc("PUT "+agentapi.PathPoolerConfig, s.poolerConfig)
	mux.HandleFunc("PUT "+agentapi.PathPoolerExpected, s.poolerExpected)
	mux.HandleFunc("GET "+agentapi.PathPoolerStatus, s.poolerStatus)
	mux.HandleFunc("PUT "+agentapi.PathClientSerials, s.clientSerials)
	return s.checkSerial(mux)
}

// TLSConfig requires a client certificate from ca with xenibase-server's
// subject. Which of those certificates may call the agent is then decided
// per request by the allow list (Config.ClientAllow).
func TLSConfig(cert tls.Certificate, ca *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca,
		MinVersion:   tls.VersionTLS13,
		VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error {
			if len(chains) == 0 || chains[0][0].Subject.CommonName != agentca.ServerCN {
				return errors.New("client certificate is not xenibase-server's")
			}
			return nil
		},
	}
}

// Serve runs the API on ln with tlsCfg until ctx ends.
func (s *Service) Serve(ctx context.Context, ln net.Listener, tlsCfg *tls.Config) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, TLSConfig: tlsCfg,
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(tls.NewListener(ln, tlsCfg))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, agentapi.Error{Error: err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("bad request: %w", err))
		return false
	}
	return true
}

// acquire takes a job slot, or reports the agent busy.
func (s *Service) acquire(ctx context.Context) (func(), error) {
	select {
	case s.jobs <- struct{}{}:
		return func() { <-s.jobs }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, agentapi.Health{
		Version: s.cfg.Version, Hostname: host, NodeID: s.cfg.NodeID,
		PGDump: s.toolVersion("pg_dump"), PGRestore: s.toolVersion("pg_restore"),
		Docker: s.inst.status(r.Context()), Image: s.cfg.Instances.Image,
	})
}

func (s *Service) metrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, hostMetrics(s.cfg.DiskPath))
}

// countingWriter hashes and counts what passes through.
type countingWriter struct {
	w io.Writer
	h interface {
		io.Writer
		Sum([]byte) []byte
	}
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	_, _ = c.h.Write(p[:n])
	return n, err
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func (s *Service) dump(w http.ResponseWriter, r *http.Request) {
	var req agentapi.DumpRequest
	if !decode(w, r, &req) {
		return
	}
	release, err := s.acquire(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	defer release()
	start := time.Now()
	res, err := s.doDump(r.Context(), req)
	if err != nil {
		s.log.Warn("dump failed", "pg", req.PG.Redacted(), "err", err)
		fail(w, http.StatusBadGateway, err)
		return
	}
	res.DurationMS = time.Since(start).Milliseconds()
	s.log.Info("dump uploaded", "pg", req.PG.Redacted(), "object", req.Upload.ObjectKey, "bytes", res.SizeBytes, "ms", res.DurationMS)
	writeJSON(w, http.StatusOK, res)
}

// doDump streams pg_dump -> encrypt -> S3 without touching the disk.
func (s *Service) doDump(ctx context.Context, req agentapi.DumpRequest) (agentapi.DumpResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client, err := storage.New(req.Upload.Storage)
	if err != nil {
		return agentapi.DumpResult{}, err
	}
	out, wait, err := s.startDump(ctx, req.PG, req.Options)
	if err != nil {
		return agentapi.DumpResult{}, err
	}
	pr, pw := io.Pipe()
	hashed := &countingWriter{w: pw, h: sha256.New()}
	plain := &counter{}
	encErr := make(chan error, 1)
	go func() {
		var ew io.WriteCloser
		var err error
		if req.Upload.PGPPublicKey != "" {
			ew, err = pgpstream.Encrypt(hashed, req.Upload.PGPPublicKey)
		} else {
			ew, err = backupfmt.NewWriter(hashed, req.Upload.FileKey, req.Upload.WrappedKey)
		}
		if err == nil {
			_, err = io.Copy(io.MultiWriter(ew, plain), out)
			if cerr := ew.Close(); err == nil {
				err = cerr
			}
		}
		if werr := wait(); err == nil {
			err = werr
		}
		_ = pw.CloseWithError(err)
		encErr <- err
	}()
	upErr := client.Upload(ctx, req.Upload.ObjectKey, pr)
	if upErr != nil {
		cancel()
		_ = pr.CloseWithError(upErr)
	}
	if err := <-encErr; err != nil {
		if upErr == nil {
			// pg_dump failed after a complete-looking upload: remove it.
			_ = client.Delete(context.WithoutCancel(ctx), req.Upload.ObjectKey)
		}
		return agentapi.DumpResult{}, err
	}
	if upErr != nil {
		return agentapi.DumpResult{}, upErr
	}
	return agentapi.DumpResult{SizeBytes: hashed.n, DumpBytes: plain.n, SHA256: hex.EncodeToString(hashed.h.Sum(nil))}, nil
}

func (s *Service) restore(w http.ResponseWriter, r *http.Request) {
	var req agentapi.RestoreRequest
	if !decode(w, r, &req) {
		return
	}
	release, err := s.acquire(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	defer release()
	start := time.Now()
	warnings, err := s.doRestore(r.Context(), req)
	if err != nil {
		s.log.Warn("restore failed", "pg", req.PG.Redacted(), "err", err)
		fail(w, http.StatusBadGateway, err)
		return
	}
	res := agentapi.RestoreResult{DurationMS: time.Since(start).Milliseconds(), Warnings: warnings}
	s.log.Info("restore finished", "pg", req.PG.Redacted(), "object", req.Download.ObjectKey, "ms", res.DurationMS, "warnings", len(warnings))
	writeJSON(w, http.StatusOK, res)
}

// doRestore streams S3 -> decrypt -> pg_restore.
func (s *Service) doRestore(ctx context.Context, req agentapi.RestoreRequest) ([]string, error) {
	client, err := storage.New(req.Download.Storage)
	if err != nil {
		return nil, err
	}
	body, err := client.Download(ctx, req.Download.ObjectKey)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var dec io.Reader
	if req.Download.PGPPrivateKey != "" {
		dec, err = pgpstream.Decrypt(body, req.Download.PGPPrivateKey)
	} else {
		dec, err = backupfmt.NewReader(body, req.Download.FileKey)
	}
	if err != nil {
		return nil, err
	}
	// pg_restore stops reading on error; report decryption failures, which
	// would otherwise surface as a truncated archive.
	tr := &trackingReader{r: dec}
	warnings, err := s.runRestore(ctx, tr, req.PG, req.Options)
	if tr.err != nil && !errors.Is(tr.err, io.EOF) {
		return warnings, fmt.Errorf("backup object: %w", tr.err)
	}
	return warnings, err
}

type trackingReader struct {
	r   io.Reader
	err error
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil {
		t.err = err
	}
	return n, err
}

func (s *Service) copy(w http.ResponseWriter, r *http.Request) {
	var req agentapi.CopyRequest
	if !decode(w, r, &req) {
		return
	}
	release, err := s.acquire(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	defer release()
	start := time.Now()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	out, wait, err := s.startDump(ctx, req.Source, req.Dump)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	warnings, rerr := s.runRestore(ctx, out, req.Target, req.Restore)
	if rerr != nil {
		cancel()
	}
	derr := wait()
	if derr != nil && rerr == nil {
		rerr = derr
	}
	if rerr != nil {
		s.log.Warn("copy failed", "source", req.Source.Redacted(), "target", req.Target.Redacted(), "err", rerr)
		fail(w, http.StatusBadGateway, rerr)
		return
	}
	res := agentapi.RestoreResult{DurationMS: time.Since(start).Milliseconds(), Warnings: warnings}
	s.log.Info("copy finished", "source", req.Source.Redacted(), "target", req.Target.Redacted(), "ms", res.DurationMS, "warnings", len(warnings))
	writeJSON(w, http.StatusOK, res)
}
