package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/docker"
	"github.com/israel-duff/xenibase/internal/walg"
)

// InstanceConfig configures instance management (dedicated tier, spec §4.2).
type InstanceConfig struct {
	// Docker is the daemon's address (empty: the local socket).
	Docker string
	// Image is the Postgres + WAL-G image instances run.
	Image string
	// Network, if set, is the Docker network instances join; they are then
	// reached by container name on port 5432.
	Network string
	// PublishAddr, if set, is the node address instance ports are published
	// on (an ephemeral port each), e.g. the node's private IP.
	PublishAddr string
	// HBAAllow are the CIDRs new instances accept network logins from
	// (written to pg_hba.conf at initdb; spec §7.1).
	HBAAllow []string
	// MoveAllow are the CIDRs other instances connect from to replicate a
	// project during a move (V3 §2.3). Only the moves' own logins
	// (xenibase_move_<id>) may use them; added to pg_hba.conf on every start,
	// so instances created before V3 get them too.
	MoveAllow []string
	// EtcdImage is the image of the node's etcd member (V3 §2.2).
	EtcdImage string
}

// instances manages Postgres containers.
type instances struct {
	cfg   InstanceConfig
	dc    *docker.Client
	err   error // why instances are unavailable
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newInstances(cfg InstanceConfig) *instances {
	in := &instances{cfg: cfg, locks: map[string]*sync.Mutex{}}
	if cfg.Image == "" {
		in.err = errors.New("no instance image configured (XENIBASE_AGENT_PG_IMAGE)")
		return in
	}
	in.dc, in.err = docker.New(cfg.Docker)
	return in
}

// status is "ok" or why instances cannot be managed.
func (in *instances) status(ctx context.Context) string {
	if in.err != nil {
		return in.err.Error()
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := in.dc.Ping(cctx); err != nil {
		return err.Error()
	}
	return "ok"
}

// lock serialises operations on one instance.
func (in *instances) lock(id string) func() {
	in.mu.Lock()
	l, ok := in.locks[id]
	if !ok {
		l = &sync.Mutex{}
		in.locks[id] = l
	}
	in.mu.Unlock()
	l.Lock()
	return l.Unlock
}

var instanceID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func names(id string) (container, volume, restore string) {
	return "xenibase-" + id, "xenibase-" + id, "xenibase-" + id + "-restore"
}

const (
	pgPort   = "5432/tcp"
	dataRoot = "/var/lib/postgresql"
	// recoveryEnv holds a point-in-time recovery's source archive settings
	// until recovery ends.
	recoveryEnv = dataRoot + "/xenibase-recovery.env"
)

// validSetting keeps settings to plain parameter names and values that
// cannot smuggle extra flags.
var settingName = regexp.MustCompile(`^[a-z_][a-z0-9_.]*$`)

// Create creates (or finds) and starts an instance, restoring it first if
// asked, and waits until Postgres accepts TCP connections.
func (in *instances) Create(ctx context.Context, spec agentapi.InstanceSpec) (agentapi.Instance, error) {
	if !instanceID.MatchString(spec.ID) {
		return agentapi.Instance{}, fmt.Errorf("bad instance id %q", spec.ID)
	}
	if spec.Kind != agentapi.InstanceDedicated && spec.Kind != agentapi.InstanceShared {
		return agentapi.Instance{}, fmt.Errorf("bad instance kind %q", spec.Kind)
	}
	defer in.lock(spec.ID)()
	name, vol, restoreName := names(spec.ID)

	var prev memberPorts
	if old, err := in.dc.InspectContainer(ctx, name); err == nil && spec.Recreate && spec.Restore == nil {
		prev = portsOf(old)
		if err := in.dc.StopContainer(ctx, name, 60*time.Second); err != nil {
			return agentapi.Instance{}, fmt.Errorf("stop for recreate: %w", err)
		}
		if err := in.dc.RemoveContainer(ctx, name); err != nil {
			return agentapi.Instance{}, fmt.Errorf("remove for recreate: %w", err)
		}
		// The volume stays: the new container below starts on the same data.
	} else if err == nil {
		if err := in.dc.StartContainer(ctx, name); err != nil {
			return agentapi.Instance{}, err
		}
		if err := in.ready(ctx, name); err != nil {
			return agentapi.Instance{}, err
		}
		return in.info(ctx, spec.ID)
	} else if !errors.Is(err, docker.ErrNotFound) {
		return agentapi.Instance{}, err
	}

	image := agentapi.ImageFor(in.cfg.Image, spec.PGVersion)
	if ok, err := in.dc.ImageExists(ctx, image); err != nil {
		return agentapi.Instance{}, err
	} else if !ok {
		if err := in.dc.Pull(ctx, image); err != nil {
			return agentapi.Instance{}, err
		}
	}
	labels := map[string]string{"xenibase.instance": spec.ID, "xenibase.kind": spec.Kind}
	if spec.Restore != nil {
		// A half-finished earlier attempt may have left data behind.
		_ = in.dc.RemoveContainer(ctx, restoreName)
		if err := in.dc.RemoveVolume(ctx, vol); err != nil {
			return agentapi.Instance{}, err
		}
	}
	if err := in.dc.CreateVolume(ctx, vol, labels); err != nil {
		return agentapi.Instance{}, err
	}
	if spec.Restore != nil {
		if err := in.restore(ctx, spec, vol, restoreName); err != nil {
			return agentapi.Instance{}, err
		}
	}

	cmd := []string{"postgres"}
	// wal_level=logical: moves replicate out of any instance (V3 §2.3);
	// WAL-G archives it as it would replica.
	settings := map[string]string{"listen_addresses": "*", "password_encryption": "scram-sha-256", "wal_level": "logical"}
	if spec.WALG != nil {
		settings["archive_mode"] = "on"
		settings["archive_command"] = "wal-g wal-push %p"
		settings["archive_timeout"] = "60"
	}
	for k, v := range spec.Settings {
		if !settingName.MatchString(k) || strings.ContainsAny(v, "\x00\n") {
			return agentapi.Instance{}, fmt.Errorf("bad setting %q", k)
		}
		settings[k] = v
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd = append(cmd, "-c", k+"="+settings[k])
	}
	env := []string{
		"POSTGRES_USER=" + spec.AdminUser,
		"POSTGRES_PASSWORD=" + spec.AdminPassword,
		"POSTGRES_DB=postgres",
		"POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 --auth-local=trust",
		"PGDATA=" + pgdataFor(spec.PGVersion),
	}
	if len(in.cfg.HBAAllow) > 0 {
		env = append(env, "XENIBASE_HBA_ALLOW="+strings.Join(in.cfg.HBAAllow, ","))
	}
	if spec.WALG != nil {
		env = append(env, walg.Env(*spec.WALG)...)
	}
	mem := int64(spec.MemoryMB) << 20
	shm := min(max(mem/4, 64<<20), 1<<30)
	stop := 60
	cc := docker.ContainerConfig{
		Image: image, Cmd: cmd, Env: env, Labels: labels,
		ExposedPorts: map[string]struct{}{pgPort: {}},
		StopSignal:   "SIGINT", // Postgres fast shutdown
		StopTimeout:  &stop,
		HostConfig: docker.HostConfig{
			Mounts:        []docker.Mount{{Type: "volume", Source: vol, Target: dataRoot}},
			NanoCPUs:      int64(spec.CPUs * 1e9),
			Memory:        mem,
			ShmSize:       shm,
			RestartPolicy: &docker.RestartPolicy{Name: "unless-stopped"},
		},
	}
	if in.cfg.PublishAddr != "" {
		cc.HostConfig.PortBindings = map[string][]docker.PortBinding{pgPort: {{HostIP: in.cfg.PublishAddr, HostPort: ""}}}
	}
	if in.cfg.Network != "" {
		cc.HostConfig.NetworkMode = in.cfg.Network
		cc.Networking = &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{in.cfg.Network: {Aliases: []string{name}}}}
	}
	if spec.Patroni != nil {
		if err := in.applyPatroni(&cc, spec, name, settings, prev); err != nil {
			return agentapi.Instance{}, err
		}
	}
	if _, err := in.dc.CreateContainer(ctx, name, cc); err != nil {
		return agentapi.Instance{}, err
	}
	if err := in.dc.StartContainer(ctx, name); err != nil {
		return agentapi.Instance{}, err
	}
	if err := in.ready(ctx, name); err != nil {
		return agentapi.Instance{}, err
	}
	if err := in.checkMajor(ctx, name, image, spec.PGVersion); err != nil {
		return agentapi.Instance{}, err
	}
	return in.info(ctx, spec.ID)
}

var serverVersion = regexp.MustCompile(`PostgreSQL\) (\d+)`)

// checkMajor refuses an instance whose image runs another Postgres major
// than asked: an agent image without {major} serves one version only.
func (in *instances) checkMajor(ctx context.Context, name, image string, want int) error {
	if want == 0 {
		return nil
	}
	r, err := in.dc.Exec(ctx, name, "postgres", nil, []string{"postgres", "-V"})
	if err != nil {
		return err
	}
	m := serverVersion.FindStringSubmatch(r.Stdout)
	if m == nil {
		return fmt.Errorf("instance %s: unexpected postgres -V output %q", name, strings.TrimSpace(r.Stdout))
	}
	if got, _ := strconv.Atoi(m[1]); got != want {
		return fmt.Errorf("image %s runs Postgres %d, not %d: set the agent's image to a template with {major} (XENIBASE_AGENT_PG_IMAGE)", image, got, want)
	}
	return nil
}

var releaseRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

// pgRelease trims a PG_VERSION value to major.minor.
func pgRelease(v string) string { return releaseRe.FindString(v) }

// pgdataFor is the data directory for a major, inside the instance's
// volume (mounted at dataRoot). Postgres 18's image uses this layout; older
// images default to dataRoot/data, which they also declare as an anonymous
// volume: data there would be lost when the container is recreated, so
// PGDATA is always set.
func pgdataFor(major int) string {
	if major == 0 {
		major = agentapi.DefaultPGVersion
	}
	return dataRoot + "/" + strconv.Itoa(major) + "/docker"
}

// ready waits for the instance to accept connections and makes sure its
// pg_hba.conf lets move logins in.
func (in *instances) ready(ctx context.Context, name string) error {
	if c, err := in.dc.InspectContainer(ctx, name); err == nil && c.Config.Labels[patroniLabel] != "" {
		// Patroni owns pg_hba.conf (the move rules are in its
		// configuration), and a new standby may restore for a long time:
		// ready is Patroni answering. xenibase-server follows the member's
		// role and state through the REST API.
		return in.waitPatroni(ctx, name)
	}
	if err := in.waitReady(ctx, name); err != nil {
		return err
	}
	return in.ensureMoveHBA(ctx, name)
}

// ensureMoveRules adds the move rules to every running instance, for those
// started before this agent knew them (instances created before V3).
func (in *instances) ensureMoveRules(ctx context.Context, log *slog.Logger) {
	if in.err != nil || len(in.cfg.MoveAllow) == 0 {
		return
	}
	cs, err := in.dc.ListContainers(ctx, "xenibase.instance")
	if err != nil {
		log.Warn("listing instances for the move rules", "err", err)
		return
	}
	for _, c := range cs {
		id := c.Labels["xenibase.instance"]
		name, _, _ := names(id)
		if c.State != "running" || !instanceID.MatchString(id) || !slices.Contains(c.Names, "/"+name) {
			continue // stopped, or a restore's one-off container
		}
		unlock := in.lock(id)
		if err := in.ensureMoveHBA(ctx, name); err != nil {
			log.Warn("adding the move rules to pg_hba.conf", "instance", id, "err", err)
		}
		unlock()
	}
}

// moveHBAMark heads the rules ensureMoveHBA adds.
const moveHBAMark = "# xenibase: logical-replication moves (V3 §2.3)"

// ensureMoveHBA appends, once, rules letting only the moves' logins in
// from MoveAllow, and reloads the configuration.
func (in *instances) ensureMoveHBA(ctx context.Context, name string) error {
	if len(in.cfg.MoveAllow) == 0 {
		return nil
	}
	var rules strings.Builder
	rules.WriteString(moveHBAMark + "\n")
	for _, c := range in.cfg.MoveAllow {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return fmt.Errorf("move CIDR %q: %w", c, err)
		}
		fmt.Fprintf(&rules, "host all /^xenibase_move_[0-9a-f]+$ %s scram-sha-256\n", c)
	}
	r, err := in.dc.Exec(ctx, name, "postgres", []string{"XENIBASE_MOVE_RULES=" + rules.String(), "XENIBASE_MOVE_MARK=" + moveHBAMark}, []string{"sh", "-c",
		`f="$(psql -XAtq -U "${POSTGRES_USER:-${XENIBASE_ADMIN_USER:-xenibase_admin}}" -h /var/run/postgresql -d postgres -c 'SHOW hba_file')" || exit 1
grep -qxF "$XENIBASE_MOVE_MARK" "$f" && exit 0
printf '%s' "$XENIBASE_MOVE_RULES" >> "$f" || exit 1
psql -XAtq -U "${POSTGRES_USER:-${XENIBASE_ADMIN_USER:-xenibase_admin}}" -h /var/run/postgresql -d postgres -c 'SELECT pg_reload_conf()' >/dev/null`})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("instance %s: add the move rules to pg_hba.conf: %s", name, strings.TrimSpace(r.Stderr+r.Stdout))
	}
	return nil
}

// restore fills vol from a WAL-G base backup and sets up recovery, in a
// one-off container that exits when done.
func (in *instances) restore(ctx context.Context, spec agentapi.InstanceSpec, vol, name string) error {
	r := spec.Restore
	if r.BackupName == "" {
		r.BackupName = "LATEST"
	}
	if !regexp.MustCompile(`^(LATEST|base_[0-9A-F]{24}(_D_[0-9A-F]{24})?)$`).MatchString(r.BackupName) {
		return fmt.Errorf("bad backup name %q", r.BackupName)
	}
	conf := []string{
		// Fetch WAL from the source's archive, which may be on another
		// target with another key than the instance's own (V2 §6): its
		// settings come from a file only the postgres user reads, removed
		// when recovery ends. The instance archives its new timeline to
		// spec.WALG once promoted.
		"restore_command = '. " + recoveryEnv + " && wal-g wal-fetch %f %p'",
		"recovery_end_command = 'rm -f " + recoveryEnv + "'",
		"recovery_target_action = 'promote'",
	}
	if r.TargetTime != "" {
		t, err := time.Parse(time.RFC3339Nano, r.TargetTime)
		if err != nil {
			return fmt.Errorf("bad target time: %w", err)
		}
		// Postgres rejects a bare "Z"; spell the zone as an offset.
		conf = append(conf, "recovery_target_time = '"+t.UTC().Format("2006-01-02 15:04:05.999999")+"+00'")
	}
	script := `set -eu
mkdir -p "$PGDATA" && chmod 700 "$PGDATA"
wal-g backup-fetch "$PGDATA" "$BACKUP_NAME"
touch "$PGDATA/recovery.signal"
printf '\n# Xenibase point-in-time recovery\n%s\n' "$RECOVERY_CONF" >> "$PGDATA/postgresql.auto.conf"
(umask 077; printf '%s\n' "$RECOVERY_ENV" > "$RECOVERY_ENV_FILE")
echo restored`
	env := append(walg.Env(r.Source), "BACKUP_NAME="+r.BackupName, "RECOVERY_CONF="+strings.Join(conf, "\n"), "PGDATA="+pgdataFor(spec.PGVersion),
		"RECOVERY_ENV="+shellExports(walg.Env(r.Source)), "RECOVERY_ENV_FILE="+recoveryEnv)
	id, err := in.dc.CreateContainer(ctx, name, docker.ContainerConfig{
		Image: agentapi.ImageFor(in.cfg.Image, spec.PGVersion), Entrypoint: []string{"sh", "-c"}, Cmd: []string{script}, Env: env, User: "postgres",
		Labels: map[string]string{"xenibase.instance": spec.ID, "xenibase.role": "restore"},
		HostConfig: docker.HostConfig{
			Mounts:      []docker.Mount{{Type: "volume", Source: vol, Target: dataRoot}},
			NetworkMode: in.cfg.Network,
		},
	})
	if err != nil {
		return err
	}
	defer func() { _ = in.dc.RemoveContainer(context.WithoutCancel(ctx), id) }()
	if err := in.dc.StartContainer(ctx, id); err != nil {
		return err
	}
	code, err := in.dc.WaitContainer(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		logs, _ := in.dc.Logs(ctx, id, 20)
		return fmt.Errorf("wal-g backup-fetch failed (exit %d): %s", code, strings.TrimSpace(logs))
	}
	return nil
}

// waitReady waits until Postgres in the container accepts TCP connections
// (the entrypoint's temporary init server listens on the socket only).
func (in *instances) waitReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		c, err := in.dc.InspectContainer(ctx, name)
		if err != nil {
			return err
		}
		// Crash-looping (e.g. a bad setting): give up with its log rather
		// than wait for the restart policy forever.
		if c.RestartCount >= 3 || (!c.State.Running && c.State.Status != "created" && c.State.Status != "restarting") {
			logs, _ := in.dc.Logs(ctx, name, 20)
			return fmt.Errorf("instance %s is %s (exit %d): %s", name, c.State.Status, c.State.ExitCode, strings.TrimSpace(logs))
		}
		if c.State.Running {
			r, err := in.dc.Exec(ctx, name, "postgres", nil, []string{"pg_isready", "-q", "-h", "127.0.0.1", "-p", "5432"})
			if err == nil && r.ExitCode == 0 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			logs, _ := in.dc.Logs(ctx, name, 20)
			return fmt.Errorf("instance %s did not become ready: %s", name, strings.TrimSpace(logs))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (in *instances) info(ctx context.Context, id string) (agentapi.Instance, error) {
	name, vol, _ := names(id)
	c, err := in.dc.InspectContainer(ctx, name)
	if errors.Is(err, docker.ErrNotFound) {
		return agentapi.Instance{ID: id, Container: name, Volume: vol, State: "missing"}, nil
	}
	if err != nil {
		return agentapi.Instance{}, err
	}
	out := agentapi.Instance{
		ID: id, ContainerID: c.ID, Container: name, Volume: vol, State: c.State.Status, Running: c.State.Running,
		Image: c.Config.Image,
	}
	// The official image records its release in PG_VERSION
	// ("18.1-1.pgdg13+1"); a lookup that fails leaves the field empty.
	if img, err := in.dc.InspectImage(ctx, c.ImageID); err == nil {
		out.Version = pgRelease(img.Env("PG_VERSION"))
	}
	if img, err := in.dc.InspectImage(ctx, c.Config.Image); err == nil {
		out.ImageVersion = pgRelease(img.Env("PG_VERSION"))
	}
	if b := c.NetworkSettings.Ports[pgPort]; len(b) > 0 {
		out.PublishedHost = b[0].HostIP
		out.PublishedPort, _ = strconv.Atoi(b[0].HostPort)
	}
	switch {
	case in.cfg.Network != "":
		out.Host, out.Port = name, 5432
	case out.PublishedPort != 0:
		out.Host, out.Port = out.PublishedHost, out.PublishedPort
	}
	if c.Config.Labels[patroniLabel] != "" {
		out.Patroni = true
		out.PublishedRestPort = portsOf(c).rest
		out.RestPort = 8008
		if in.cfg.Network == "" {
			out.RestPort = out.PublishedRestPort
		}
	}
	return out, nil
}

// Destroy removes an instance's container and volume (spec §6.2 step 4).
func (in *instances) Destroy(ctx context.Context, id string) error {
	defer in.lock(id)()
	name, vol, restoreName := names(id)
	_ = in.dc.RemoveContainer(ctx, restoreName)
	if err := in.dc.RemoveContainer(ctx, name); err != nil {
		return err
	}
	return in.dc.RemoveVolume(ctx, vol)
}

// walgShell runs a WAL-G command line in the instance as postgres,
// connected to its server over the local socket as the admin user.
func (in *instances) walgShell(ctx context.Context, id, script string) (docker.ExecResult, error) {
	name, _, _ := names(id)
	r, err := in.dc.Exec(ctx, name, "postgres", nil, []string{"sh", "-c",
		`export PGUSER="${POSTGRES_USER:-${XENIBASE_ADMIN_USER:-xenibase_admin}}" PGHOST=/var/run/postgresql PGDATABASE=postgres; ` + script})
	if err != nil {
		return r, err
	}
	if r.ExitCode != 0 {
		return r, fmt.Errorf("wal-g failed (exit %d): %s", r.ExitCode, lastLines(r.Stderr, 8))
	}
	return r, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Backups lists the instance's base backups, oldest first. A listing that
// fails is an error, not an empty archive: the control plane forgets the
// base backups a listing lacks.
func (in *instances) Backups(ctx context.Context, id string) ([]agentapi.WALGBackup, error) {
	r, err := in.walgShell(ctx, id, `wal-g backup-list --json --detail`)
	return parseBackupList(r, err)
}

// parseBackupList reads wal-g backup-list --json --detail. An archive with
// no backups yet lists nothing; some WAL-G releases say so on stderr with
// a failing exit, which is still an empty list.
func parseBackupList(r docker.ExecResult, err error) ([]agentapi.WALGBackup, error) {
	if err != nil {
		if r.ExitCode != 0 && strings.Contains(r.Stderr, "No backups found") {
			return nil, nil
		}
		return nil, fmt.Errorf("wal-g backup-list: %w", err)
	}
	out := strings.TrimSpace(r.Stdout)
	if out == "" || out == "null" {
		return nil, nil
	}
	var list []agentapi.WALGBackup
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse wal-g backup-list: %w", err)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].FinishTime.Before(list[j].FinishTime) })
	return list, nil
}

// newBackup is the backup a backup-push begun at start made: the newest
// listed, which must have started since (a minute's slack for clocks). A
// listing without it would report an older backup as the new one.
func newBackup(list []agentapi.WALGBackup, start time.Time) (agentapi.WALGBackup, error) {
	if len(list) == 0 {
		return agentapi.WALGBackup{}, errors.New("wal-g reported success but lists no backups")
	}
	b := list[len(list)-1]
	if b.StartTime.Before(start.Add(-time.Minute)) {
		return agentapi.WALGBackup{}, fmt.Errorf("wal-g reported success but its newest listed backup, %s, started before this one", b.Name)
	}
	return b, nil
}

// Backup takes a base backup (spec §6.4) and applies retention.
func (in *instances) Backup(ctx context.Context, id string, req agentapi.WALGBackupRequest) (agentapi.WALGBackupResult, error) {
	defer in.lock(id)()
	start := time.Now()
	if _, err := in.walgShell(ctx, id, `wal-g backup-push "$PGDATA"`); err != nil {
		return agentapi.WALGBackupResult{}, err
	}
	list, err := in.Backups(ctx, id)
	if err != nil {
		return agentapi.WALGBackupResult{}, err
	}
	newest, err := newBackup(list, start)
	if err != nil {
		return agentapi.WALGBackupResult{}, err
	}
	res := agentapi.WALGBackupResult{Backup: newest, DurationMS: time.Since(start).Milliseconds()}
	if req.RetainFull > 0 {
		r, err := in.walgShell(ctx, id, fmt.Sprintf(`wal-g delete retain FULL %d --confirm 2>&1`, req.RetainFull))
		if err != nil {
			return res, err
		}
		res.Deleted = lastLines(r.Stdout, 5)
	}
	return res, nil
}

// ---- HTTP handlers -------------------------------------------------------

func (s *Service) instanceGuard(w http.ResponseWriter, r *http.Request) bool {
	if st := s.inst.status(r.Context()); st != "ok" {
		fail(w, http.StatusServiceUnavailable, fmt.Errorf("instances unavailable on this node: %s", st))
		return false
	}
	return true
}

func (s *Service) createInstance(w http.ResponseWriter, r *http.Request) {
	if !s.instanceGuard(w, r) {
		return
	}
	var spec agentapi.InstanceSpec
	if !decode(w, r, &spec) {
		return
	}
	start := time.Now()
	inst, err := s.inst.Create(r.Context(), spec)
	if err != nil {
		s.log.Warn("create instance failed", "instance", spec.ID, "err", err)
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("instance ready", "instance", spec.ID, "kind", spec.Kind, "restored", spec.Restore != nil,
		"host", inst.Host, "port", inst.Port, "ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, inst)
}

func (s *Service) getInstance(w http.ResponseWriter, r *http.Request) {
	if !s.instanceGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	if !instanceID.MatchString(id) {
		fail(w, http.StatusBadRequest, errors.New("bad instance id"))
		return
	}
	inst, err := s.inst.info(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Service) instanceAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.instanceGuard(w, r) {
			return
		}
		id := r.PathValue("id")
		if !instanceID.MatchString(id) {
			fail(w, http.StatusBadRequest, errors.New("bad instance id"))
			return
		}
		name, _, _ := names(id)
		var err error
		switch action {
		case "start":
			if err = s.inst.dc.StartContainer(r.Context(), name); err == nil {
				err = s.inst.ready(r.Context(), name)
			}
		case "stop":
			err = s.inst.dc.StopContainer(r.Context(), name, time.Minute)
		case "destroy":
			err = s.inst.Destroy(r.Context(), id)
		}
		if errors.Is(err, docker.ErrNotFound) && action != "start" {
			err = nil
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		s.log.Info("instance "+action, "instance", id)
		inst, err := s.inst.info(r.Context(), id)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, inst)
	}
}

func (s *Service) walgBackup(w http.ResponseWriter, r *http.Request) {
	if !s.instanceGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	if !instanceID.MatchString(id) {
		fail(w, http.StatusBadRequest, errors.New("bad instance id"))
		return
	}
	var req agentapi.WALGBackupRequest
	if !decode(w, r, &req) {
		return
	}
	done, err := s.acquire(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	defer done()
	res, err := s.inst.Backup(r.Context(), id, req)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("base backup pushed", "instance", id, "backup", res.Backup.Name, "ms", res.DurationMS)
	writeJSON(w, http.StatusOK, res)
}

func (s *Service) walgBackups(w http.ResponseWriter, r *http.Request) {
	if !s.instanceGuard(w, r) {
		return
	}
	id := r.PathValue("id")
	if !instanceID.MatchString(id) {
		fail(w, http.StatusBadRequest, errors.New("bad instance id"))
		return
	}
	list, err := s.inst.Backups(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if list == nil {
		list = []agentapi.WALGBackup{}
	}
	writeJSON(w, http.StatusOK, list)
}

// shellExports renders KEY=value pairs as a sh script that sets exactly
// them (AWS_ENDPOINT is cleared first: a source without one must not use
// the instance's).
func shellExports(env []string) string {
	var b strings.Builder
	b.WriteString("unset AWS_ENDPOINT\n")
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString("export " + k + "='" + strings.ReplaceAll(v, "'", `'\''`) + "'\n")
	}
	return b.String()
}
