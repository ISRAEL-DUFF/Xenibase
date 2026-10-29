// Command xenibase-server is the Xenibase control plane: REST API, embedded web
// UI, operation workers, and (in later milestones) pooler management.
package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/alerts"
	"github.com/israel-duff/xenibase/internal/api"
	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auditlog"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/billing"
	"github.com/israel-duff/xenibase/internal/billing/iswallet"
	"github.com/israel-duff/xenibase/internal/branching"
	"github.com/israel-duff/xenibase/internal/capacity"
	"github.com/israel-duff/xenibase/internal/cdn"
	"github.com/israel-duff/xenibase/internal/cloud"
	"github.com/israel-duff/xenibase/internal/config"
	"github.com/israel-duff/xenibase/internal/console"
	"github.com/israel-duff/xenibase/internal/costs"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/floatip"
	"github.com/israel-duff/xenibase/internal/freetier"
	"github.com/israel-duff/xenibase/internal/ha"
	"github.com/israel-duff/xenibase/internal/incidents"
	"github.com/israel-duff/xenibase/internal/insights"
	"github.com/israel-duff/xenibase/internal/isocheck"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/legal"
	"github.com/israel-duff/xenibase/internal/logging"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/messaging"
	"github.com/israel-duff/xenibase/internal/metrics"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/pgversions"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/regions"
	"github.com/israel-duff/xenibase/internal/rotate"
	"github.com/israel-duff/xenibase/internal/schedjobs"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/settings"
	"github.com/israel-duff/xenibase/internal/statusapi"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/support"
	"github.com/israel-duff/xenibase/internal/tenancy"
	"github.com/israel-duff/xenibase/internal/tlscert"
	"github.com/israel-duff/xenibase/internal/tokens"
	"github.com/israel-duff/xenibase/internal/version"
	"github.com/israel-duff/xenibase/internal/waker"
	"github.com/israel-duff/xenibase/internal/webhooks"
	"github.com/israel-duff/xenibase/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "xenibase-server:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		return runAdmin(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "mail-preview" {
		// xenibase-server mail-preview <dir> [https://app.example.com]: write sample
		// emails as HTML and text files to look at in a browser.
		if len(os.Args) < 3 {
			return errors.New("usage: xenibase-server mail-preview <dir> [public-url]")
		}
		if len(os.Args) > 3 {
			mail.SetPublicURL(os.Args[3])
		}
		n, err := mail.WritePreviews(os.Args[2])
		fmt.Printf("wrote %d samples to %s\n", n, os.Args[2])
		return err
	}
	showVersion := flag.Bool("version", false, "print version and exit")
	requireUI := flag.Bool("require-ui", false, "exit with an error if only the placeholder UI is embedded")
	genKey := flag.Bool("gen-master-key", false, "print a new random master key for XENIBASE_MASTER_KEY and exit")
	healthcheck := flag.String("healthcheck", "", "GET this URL's /healthz and exit 0 if healthy (for container health checks)")
	rotateKey := flag.Bool("rotate-master-key", false,
		"re-encrypt every stored secret under XENIBASE_MASTER_KEY (old key in XENIBASE_MASTER_KEY_PREVIOUS), verify, and exit")
	flag.Parse()

	v := version.Get()
	switch {
	case *showVersion:
		fmt.Printf("xenibase-server %s (commit %s, built %s, %s)\n", v.Version, v.Commit, v.BuildDate, v.GoVersion)
		return nil
	case *requireUI:
		if !web.HasUI() {
			return errors.New("binary embeds only the placeholder UI; build with `make build`")
		}
		fmt.Println("web UI embedded")
		return nil
	case *healthcheck != "":
		c := &http.Client{Timeout: 3 * time.Second}
		res, err := c.Get(strings.TrimRight(*healthcheck, "/") + "/healthz")
		if err != nil {
			return err
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("healthz: %s", res.Status)
		}
		return nil
	case *genKey:
		k, err := crypto.GenerateKey()
		if err != nil {
			return err
		}
		fmt.Println(crypto.EncodeKey(k))
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(os.Stderr, cfg.LogFormat, cfg.LogLevel)
	slog.SetDefault(log)

	// Validate the master key at startup; secrets that use it arrive in M1+.
	keyring, err := crypto.NewKeyring(cfg.MasterKey, cfg.PreviousMasterKeys...)
	if err != nil {
		return err
	}
	log.Info("master key loaded", "key_id", keyring.PrimaryID().String(), "previous_keys", len(cfg.PreviousMasterKeys))
	if *rotateKey {
		return rotateMasterKey(cfg, keyring, log)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, log); err != nil {
		return err
	}

	// Background services stop when bgCtx is cancelled, after HTTP drains.
	bgCtx, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBG()
	var bg sync.WaitGroup

	// Migrate refused a newer schema just now; a newer release migrating
	// later (a rolling upgrade) is noticed here.
	schema := store.NewSchemaWatcher(pool, log)
	bg.Add(1)
	go func() { defer bg.Done(); schema.Run(bgCtx, 30*time.Second) }()

	settingsStore := settings.New(pool, cfg.Public.Host)
	if err := settingsStore.Load(ctx); err != nil {
		return err
	}

	authSvc, err := setupAuth(ctx, cfg, pool, keyring, log)
	if err != nil {
		return err
	}
	mailSvc := mail.New(pool, keyring)
	authSvc.SetMailer(mailSvc)
	if err := authSvc.EnsureTerms(ctx); err != nil {
		return err
	}
	if !mailSvc.Configured(ctx) {
		log.Warn("email is not set up: account verification, password resets, and invitation emails need SMTP (platform settings)")
	}
	bg.Add(1)
	go func() { defer bg.Done(); sweepAuth(bgCtx, authSvc, log) }()
	// Open signup's protection (V3 §7.4).
	guard := auth.SignupGuard{PerIP: cfg.Signup.PerIP}
	if cfg.Signup.TurnstileSecret != "" {
		guard.Check = auth.Turnstile{SiteKey: cfg.Signup.TurnstileSiteKey, Secret: cfg.Signup.TurnstileSecret, URL: cfg.Signup.TurnstileURL}
		guard.SiteKey = cfg.Signup.TurnstileSiteKey
	}
	authSvc.SetSignupGuard(guard)

	kinds := map[string]jobs.Kind{jobs.KindNoop: jobs.Noop()}
	// Regions (V3 §6.1): the home region is where projects go by default.
	regionSvc := regions.New(pool, cfg.Cloud.Region, log)
	regionSvc.SetPlatformDomain(cfg.Edge.Domain)
	if err := regionSvc.Ensure(ctx); err != nil {
		return fmt.Errorf("regions: %w", err)
	}
	bg.Add(1)
	go func() { defer bg.Done(); regionSvc.Run(bgCtx, 30*time.Second) }()
	projects, pm, err := setupProvisioning(ctx, cfg, pool, keyring, settingsStore, regionSvc, log)
	if err != nil {
		return err
	}
	if projects != nil {
		for name, k := range projects.Kinds() {
			kinds[name] = k
		}
	}

	var backups *backup.Service
	var nodeSvc *nodes.Service
	var poolerArbiter *pooler.Arbiter
	if projects != nil {
		if backups, nodeSvc, err = setupBackups(ctx, cfg, pool, keyring, projects, log); err != nil {
			return err
		}
		for name, k := range backups.Kinds() {
			kinds[name] = k
		}
		for name, k := range backups.Dedicated.Kinds() {
			kinds[name] = k
		}
		// The etcd cluster for HA instances (V3 §2.2).
		etcdSvc := ha.New(pool, keyring, nodeSvc, log)
		backups.Dedicated.Etcd = etcdSvc
		for name, k := range etcdSvc.Kinds() {
			kinds[name] = k
		}
		bg.Add(1)
		go func() { defer bg.Done(); etcdSvc.Run(bgCtx, 30*time.Second) }()
		// Follow each HA instance's leader (V3 §2.2 "Routing").
		bg.Add(1)
		go func() { defer bg.Done(); backups.Dedicated.RunHAWatcher(bgCtx, time.Second) }()
		// The standby edge pooler (V3 §2.1): push the configuration to the
		// pooler hosts and keep the floating IP on a healthy one.
		poolerArbiter = setupPoolerHosts(cfg, pm, nodeSvc, regionSvc, log)
		if poolerArbiter != nil {
			bg.Add(1)
			go func() { defer bg.Done(); poolerArbiter.Run(bgCtx, 3*time.Second) }()
		}
		bg.Add(4)
		go func() { defer bg.Done(); nodeSvc.Run(bgCtx, 30*time.Second) }()
		go func() { defer bg.Done(); backups.Run(bgCtx) }()
		go func() { defer bg.Done(); backups.Dedicated.RunReaper(bgCtx, time.Minute) }()
		// Postgres minor releases, one instance at a time in the weekly
		// maintenance window (V3 §2.4).
		go func() { defer bg.Done(); backups.Dedicated.RunMaintenance(bgCtx, 5*time.Minute) }()
		// Logins dedicated owners create with role management on (V4.2).
		bg.Add(1)
		go func() { defer bg.Done(); backups.Dedicated.RunOwnedRoles(bgCtx, 30*time.Second) }()
	}

	// Email carries the logo from, and links to, the web app.
	mail.SetPublicURL(cfg.Insight.PublicURL)
	orgSvc := orgs.New(pool, authSvc, projects, mailSvc, cfg.Insight.PublicURL, log)
	authSvc.SetHooks(orgSvc.Hooks())

	// Billing (V3 §3): price books, accounts, plan changes.
	billingSvc := billing.New(pool, mailSvc, cfg.Insight.PublicURL, log)
	if !cfg.Payments.ISwalletOn() {
		// Prices in dollars are converted to naira at the wallet provider's
		// rate: without a provider a new platform prices in naira.
		billingSvc.SetDefaultPrices(billing.DefaultPricesNGN)
	}
	if cfg.Payments.ISwalletOn() {
		billingSvc.SetWallets(iswallet.New(iswallet.Config{BaseURL: cfg.Payments.ISwalletURL, APIKey: cfg.Payments.ISwalletAPIKey,
			WebhookSecret: cfg.Payments.ISwalletWebhookSecret, ClientID: cfg.Payments.ISwalletClientID, Log: log}), cfg.Payments.ISwalletOwnerPrefix)
	}
	if backups != nil {
		billingSvc.SetDocStore(billingDocs{backups})
	}
	if err := billingSvc.Init(ctx); err != nil {
		return fmt.Errorf("billing: %w", err)
	}
	bg.Add(1)
	go func() { defer bg.Done(); billingSvc.Run(bgCtx, 10*time.Minute) }()

	// Quotas, storage locks, the reaper, usage, suspension (V2 §10).
	tokenSvc := tokens.New(pool, keyring, mailSvc, tokens.Config{PublicURL: cfg.Insight.PublicURL}, log)
	bg.Add(1)
	go func() { defer bg.Done(); tokenSvc.Run(bgCtx, time.Hour) }()

	var tenancySvc *tenancy.Service
	if projects != nil {
		tenancySvc = tenancy.New(pool, projects, mailSvc, tenancy.Config{PublicURL: cfg.Insight.PublicURL, SweepInterval: cfg.Insight.TenancySweep}, log)
		if backups != nil {
			tenancySvc.FinalBackup = func(ctx context.Context, p store.Project) error {
				_, err := backups.BackupNow(ctx, p.ID, nil)
				return err
			}
			backups.Dedicated.Quotas = tenancySvc
		}
		bg.Add(1)
		go func() { defer bg.Done(); tenancySvc.Run(bgCtx) }()
		// Dunning suspends through tenancy and, when allowed, deletes an
		// org's dedicated projects (keeping final backups).
		billingSvc.SetDunning(tenancySvc, func(ctx context.Context, orgID uuid.UUID) (int, error) {
			ps, err := store.New(pool).OrgLiveProjects(ctx, orgID)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, p := range ps {
				if p.Tier != provision.TierDedicated {
					continue
				}
				if _, err := projects.Delete(ctx, p.ID, p.Name, false, nil); err != nil {
					return n, err
				}
				n++
			}
			return n, nil
		})
	}

	// Database branches and their hourly expiry (V2 §8).
	var branchSvc *branching.Service
	if backups != nil {
		branchSvc = branching.New(pool, projects, backups, nodeSvc, mailSvc, branching.Config{PublicURL: cfg.Insight.PublicURL}, log)
		for name, k := range branchSvc.Kinds() {
			kinds[name] = k
		}
		bg.Add(1)
		go func() { defer bg.Done(); branchSvc.Run(bgCtx, 10*time.Minute) }()
	}

	// The Free tier: pause idle Free projects, archive long-paused ones,
	// and wake them on the next connection (V3 §4).
	var freeSvc *freetier.Service
	if backups != nil {
		freeSvc = freetier.New(pool, projects, backups, mailSvc, freetier.Config{
			PauseAfter: cfg.FreeTier.PauseAfter, ArchiveAfter: cfg.FreeTier.ArchiveAfter, DeleteAfter: cfg.FreeTier.DeleteAfter,
			PublicURL: cfg.Insight.PublicURL,
		}, log)
		for name, k := range freeSvc.Kinds() {
			kinds[name] = k
		}
		if host, port := cfg.FreeTier.WakerHostPort(); host != "" {
			ln, err := net.Listen("tcp", cfg.FreeTier.WakerListen)
			if err != nil {
				return fmt.Errorf("waker: listen on %s: %w", cfg.FreeTier.WakerListen, err)
			}
			pm.SetWaker(host, port)
			ws := waker.New(freeSvc, log)
			bg.Add(1)
			go func() {
				defer bg.Done()
				// If accepting fails the waker listens again, so paused
				// projects don't stay unreachable (M27 chaos test).
				for {
					err := ws.Serve(bgCtx, ln)
					if bgCtx.Err() != nil {
						return
					}
					log.Error("waker stopped; restarting", "err", err)
					_ = ln.Close()
					for {
						select {
						case <-bgCtx.Done():
							return
						case <-time.After(5 * time.Second):
						}
						if ln, err = net.Listen("tcp", cfg.FreeTier.WakerListen); err == nil {
							break
						}
						log.Error("waker: listen again", "err", err)
					}
				}
			}()
			log.Info("waker listening", "listen", cfg.FreeTier.WakerListen, "pooler_address", cfg.FreeTier.WakerAddr)
		} else {
			log.Info("XENIBASE_WAKER_ADDR is not set: idle Free projects are not paused")
		}
		bg.Add(1)
		go func() { defer bg.Done(); freeSvc.Run(bgCtx, time.Hour) }()
	}

	// Backend services (V4 §2): per-project API keys, roles and the feed
	// xenibase-edge follows.
	var servicesSvc *services.Service
	if projects != nil {
		servicesSvc = services.New(pool, projects, services.Config{Domain: cfg.Edge.Domain, RegionDomain: regionSvc.APIDomain, EdgeSecret: cfg.Edge.Secret}, log)
		if branchSvc != nil {
			branchSvc.API = servicesSvc // branches get an API of their own (V4.1 §9.5)
		}
		if freeSvc != nil {
			servicesSvc.Waker = func(ctx context.Context, projectID uuid.UUID) error {
				_, err := freeSvc.Resume(ctx, projectID, nil)
				if errors.Is(err, freetier.ErrConflict) {
					return nil // already waking
				}
				return err
			}
		}
		for name, k := range servicesSvc.Kinds() {
			kinds[name] = k
		}
		projects.DatabaseRecreated = servicesSvc.MarkForReconcile
		if backups != nil && backups.Dedicated != nil {
			backups.Dedicated.ServiceRoles = servicesSvc.EnsureRolesOn
		}
		bg.Add(1)
		servicesSvc.Mail = mailSvc
		servicesSvc.Phone = platformPhone(cfg, log)
		if backups != nil {
			servicesSvc.Files = backups.FilesTarget
		}
		if cfg.CDN.On() {
			servicesSvc.CDN = cdn.Cloudflare{ZoneID: cfg.CDN.CloudflareZoneID, Token: cfg.CDN.CloudflareToken}
		}
		go func() { defer bg.Done(); servicesSvc.Run(bgCtx, 15*time.Second) }()
	}

	// Support (V3 §7.1): tickets from the dashboard, email and WhatsApp.
	supportSvc := support.New(pool, mailSvc, support.Config{
		Address: cfg.Support.Email, PublicURL: cfg.Insight.PublicURL, InboundSecret: cfg.Support.InboundSecret,
	}, log)
	if cfg.Support.WhatsAppOn() {
		supportSvc.SetWhatsApp(support.CloudAPI{
			BaseURL: cfg.Support.WhatsAppGraphURL, PhoneNumberID: cfg.Support.WhatsAppPhoneNumberID, AccessToken: cfg.Support.WhatsAppAccessToken,
			AppSecret: cfg.Support.WhatsAppAppSecret, VerifyToken: cfg.Support.WhatsAppVerifyToken,
		})
	}

	// Legal documents (V3 §7.3): the default SLA and DPA until the
	// company publishes its own.
	legalSvc := legal.New(pool)
	if err := legalSvc.EnsureDefaults(ctx); err != nil {
		return fmt.Errorf("legal documents: %w", err)
	}

	// Cost attribution and exchange rates (V3 §5.4), and capacity
	// automation: proposals, provisioning, drains, rebalancing (§5.2, §5.3).
	costSvc := costs.New(pool, billingSvc, cfg.Cloud.Region, log)
	bg.Add(1)
	go func() { defer bg.Done(); costSvc.Run(bgCtx, time.Hour) }()
	// The Postgres version lifecycle's notices (V4.1 §6.1).
	var pgVersionSvc *pgversions.Service
	if projects != nil {
		pgVersionSvc = pgversions.New(pool, projects, mailSvc, pgversions.Config{PublicURL: cfg.Insight.PublicURL}, log)
		bg.Add(1)
		go func() { defer bg.Done(); pgVersionSvc.Run(bgCtx, time.Hour) }()
	}
	var capacitySvc *capacity.Service
	if backups != nil {
		var edgeBoot *cloud.EdgeBootstrap
		if cfg.Cloud.EdgeImage != "" && cfg.Edge.Secret != "" && cfg.Edge.Domain != "" {
			// Edge nodes (V4.1 §11) run xenibase-edge with the edge secret.
			edgeBoot = &cloud.EdgeBootstrap{Image: cfg.Cloud.EdgeImage, ControlURL: cfg.Cloud.ServerURL,
				Secret: cfg.Edge.Secret, Domain: cfg.Edge.Domain}
		}
		capacitySvc = capacity.New(pool, nodeSvc, backups.Dedicated, cloudProvider(cfg.Cloud), costSvc, capacity.Config{
			Region: cfg.Cloud.Region, Location: cfg.Cloud.HetznerLocation, Image: cfg.Cloud.HetznerImage,
			Network: cfg.Cloud.HetznerNetworkID, PlacementGroup: cfg.Cloud.HetznerPlacementGroup, SSHKeys: cfg.Cloud.HetznerSSHKeys,
			Bootstrap: cloud.Bootstrap{ServerURL: cfg.Cloud.ServerURL, ServerCA: cfg.Cloud.ServerCA, AgentImage: cfg.Cloud.AgentImage,
				PGImage: cfg.Cloud.PGImage, PrivateCIDR: cfg.Cloud.PrivateCIDR},
			Edge: edgeBoot, RegionAPIDomain: regionSvc.APIDomain,
		}, log)
		for name, k := range capacitySvc.Kinds() {
			kinds[name] = k
		}
		if backups.Dedicated != nil {
			// Dedicated hosts on demand (V4.1 §5.3).
			backups.Dedicated.Hosts = capacitySvc.HostFor
		}
		bg.Add(1)
		go func() { defer bg.Done(); capacitySvc.Run(bgCtx, 30*time.Second) }()
	}

	// Database webhooks, scheduled jobs and their outbound requests (V2 §9).
	var webhookSvc *webhooks.Service
	var jobSvc *schedjobs.Service
	var outboundSvc *outbound.Service
	if projects != nil && tenancySvc != nil {
		outboundSvc = outbound.New(pool, outbound.Config{Blocked: cfg.Insight.OutboundBlock}, log)
		if servicesSvc != nil {
			servicesSvc.Outbound = outboundSvc
		}
		webhookSvc = webhooks.New(pool, keyring, projects, outboundSvc, tenancySvc, mailSvc, webhooks.Config{PublicURL: cfg.Insight.PublicURL}, log)
		// SQL jobs run through the console's login (it holds no privileges
		// of its own), whether or not the console itself is turned off.
		jobConsole := console.New(pool, projects, keyring, cfg.Insight.ConsoleDisabled, log)
		jobSvc = schedjobs.New(pool, keyring, projects, jobConsole, outboundSvc, tenancySvc, mailSvc, schedjobs.Config{PublicURL: cfg.Insight.PublicURL}, log)
		projects.RefreshWebhooks = webhookSvc.Reinstall
		bg.Add(2)
		go func() { defer bg.Done(); webhookSvc.Run(bgCtx) }()
		go func() { defer bg.Done(); jobSvc.Run(bgCtx) }()
	}

	// Auth messages and hooks (V4 §4.5–§4.7), once their outbound client
	// (if any) is set.
	if servicesSvc != nil {
		bg.Add(1)
		go func() { defer bg.Done(); servicesSvc.RunAuthEmail(bgCtx) }()
	}

	var consoleSvc *console.Service
	var insightSvc *insights.Service
	var isoChecks *isocheck.Service
	var alertSvc *alerts.Service
	if projects != nil {
		isoChecks = isocheck.New(pool, projects, log)
		for name, k := range isoChecks.Kinds() {
			kinds[name] = k
		}
		bg.Add(1)
		go func() { defer bg.Done(); isoChecks.Run(bgCtx) }()
		consoleSvc = console.New(pool, projects, keyring, cfg.Insight.ConsoleDisabled, log)
		alertSvc = alerts.New(pool, keyring, alerts.Config{
			Interval: cfg.Insight.AlertsInterval, PublicURL: cfg.Insight.PublicURL, Poolers: pm.Admins(), WakerAddr: pm.WakerAddr,
		}, log)
		bg.Add(1)
		go func() { defer bg.Done(); alertSvc.Run(bgCtx) }()
		collector := metrics.NewCollector(pool, projects, pm, nodeSvc, cfg.Insight.MetricsInterval, log)
		bg.Add(1)
		go func() { defer bg.Done(); collector.Run(bgCtx) }()
		insightSvc = insights.New(pool, projects, consoleSvc, insights.Config{
			Interval: cfg.Insight.QueryInsightsInterval, SlowQuery: cfg.Insight.SlowQuery, Plans: cfg.Insight.InsightsPlans,
		}, log)
		bg.Add(1)
		go func() { defer bg.Done(); insightSvc.Run(bgCtx) }()
	}
	if cfg.Insight.ConsoleDisabled {
		log.Warn("SQL console disabled (XENIBASE_CONSOLE_DISABLED)")
	}

	var certs *tlscert.Manager
	if pm != nil {
		if certs, err = setupPoolerTLS(cfg, pm, settingsStore, log); err != nil {
			return err
		}
	}

	// The status page (V3 §2.6): incidents, and heartbeats for what
	// xenibase-status can't probe from outside.
	incidentSvc := incidents.New(pool, incidents.Config{
		URL: cfg.Status.URL, Secret: cfg.Status.PushSecret, Components: cfg.Status.Components, Region: cfg.Status.Region,
	}, log)
	incidentSvc.SetMailer(mailSvc, cfg.Insight.PublicURL)
	incidentSvc.Billing = billingSvc.Health
	if backups != nil && backups.Dedicated != nil {
		backups.Dedicated.SetProposer(incidentSvc)
	}
	if cfg.Status.URL != "" {
		log.Info("pushing heartbeats and incidents to the status page", "url", cfg.Status.URL)
		bg.Add(1)
		go func() { defer bg.Done(); incidentSvc.Run(bgCtx, time.Minute) }()
	}
	// SLA probes of HA projects, from here and, through the status page,
	// from outside (V3 §2.7).
	if backups != nil && backups.Dedicated != nil {
		var status *statusapi.Client
		if cfg.Status.URL != "" {
			status = &statusapi.Client{URL: cfg.Status.URL, Secret: cfg.Status.PushSecret}
		}
		bg.Add(1)
		go func() { defer bg.Done(); backups.Dedicated.RunSLA(bgCtx, time.Minute, status) }()
	}

	notifier := jobs.NewNotifier(pool, log)
	runner := jobs.NewRunner(pool, notifier, log, jobs.RunnerConfig{
		Concurrency: cfg.Workers, Paused: func() bool { return schema.Err() != nil },
	}, kinds)
	bg.Add(2)
	go func() { defer bg.Done(); notifier.Run(bgCtx) }()
	go func() { defer bg.Done(); runner.Run(bgCtx) }()

	index := web.IndexFile
	if !web.HasUI() {
		index = web.PlaceholderFile
		log.Warn("web UI not embedded; serving placeholder page")
	}
	if cfg.DevEndpoints {
		log.Warn("development endpoints enabled (XENIBASE_DEV_ENDPOINTS); do not use in production")
	}

	tlsStatus := func() gen.TlsStatus { return gen.TlsStatus{Mode: gen.TlsStatusModeOff, State: gen.TlsStatusStateOff} }
	if certs != nil {
		tlsStatus = func() gen.TlsStatus { return toAPITLS(certs.Status()) }
	}
	auditWriter := auditlog.New(pool, log, auditlog.Config{})
	bg.Add(1)
	go func() { defer bg.Done(); auditWriter.Run(bgCtx) }()
	handler := api.NewHandler(api.Options{
		Logger:       log,
		DB:           pool,
		Audit:        auditWriter,
		Schema:       schema.Err,
		Notifier:     notifier,
		DevEndpoints: cfg.DevEndpoints,
		StreamCtx:    bgCtx,
		Projects:     projects,
		UI:           web.Dist(),
		UIIndex:      index,
		Auth:         authSvc,
		Security: api.SecurityOptions{
			SecureCookies:  cfg.Web.SecureCookies,
			TrustedProxies: cfg.Web.TrustedProxies,
		},
		Settings:  settingsStore,
		PublicIPs: cfg.Web.PublicIPs,
		TLS:       tlsStatus,
		Backups:   backups,
		Nodes:     nodeSvc,

		Console:         consoleSvc,
		Insights:        insightSvc,
		IsoChecks:       isoChecks,
		Alerts:          alertSvc,
		Orgs:            orgSvc,
		Mail:            mailSvc,
		Tenancy:         tenancySvc,
		Branches:        branchSvc,
		FreeTier:        freeSvc,
		Support:         supportSvc,
		Legal:           legalSvc,
		Capacity:        capacitySvc,
		Costs:           costSvc,
		Regions:         regionSvc,
		PGVersions:      pgVersionSvc,
		Webhooks:        webhookSvc,
		Jobs:            jobSvc,
		Outbound:        outboundSvc,
		Incidents:       incidentSvc,
		PoolerArbiter:   poolerArbiter,
		Billing:         billingSvc,
		Tokens:          tokenSvc,
		Services:        servicesSvc,
		PublicURL:       cfg.Insight.PublicURL,
		MetricsInterval: cfg.Insight.MetricsInterval,
		MetricsToken:    cfg.Insight.MetricsToken,
	})
	if certs != nil {
		handler = certs.HTTPChallengeHandler(handler)
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	log.Info("xenibase-server listening",
		"addr", ln.Addr().String(), "version", v.Version, "commit", v.Commit)
	if certs != nil {
		// Only once listening: ACME challenges are answered by this server.
		bg.Add(1)
		go func() { defer bg.Done(); certs.Run(bgCtx) }()
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	// Stopping background work ends open SSE streams, which would otherwise
	// hold Shutdown until its timeout. Workers requeue interrupted attempts.
	srv.RegisterOnShutdown(stopBG)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
		_ = srv.Close()
	}
	stopBG()
	bg.Wait()
	// Requests have drained: write the audit rows still waiting for a retry.
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 10*time.Second)
	auditWriter.Close(closeCtx)
	cancelClose()

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// setupProvisioning registers the configured shared cluster and builds the
// provisioning service. It returns nil when no pooler is configured.
// setupAuth builds the auth service. Before the owner exists it logs the
// one-time setup code the first-run wizard asks for.
func setupAuth(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, keyring *crypto.Keyring, log *slog.Logger) (*auth.Service, error) {
	if !cfg.Web.SecureCookies {
		log.Warn("XENIBASE_COOKIE_SECURE=false: session cookies are sent over plain HTTP; development only")
	}
	code := cfg.Web.SetupCode
	if code == "" {
		b := make([]byte, 9)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		// No "=" padding: it is easy to lose when copying the code.
		code = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	}
	if cfg.Insight.PublicURL == "" {
		log.Warn("XENIBASE_PUBLIC_URL is not set: links in account and invitation emails will be relative")
	}
	svc := auth.NewService(pool, keyring, auth.Config{PublicURL: cfg.Insight.PublicURL}, code, log)
	needed, err := svc.SetupNeeded(ctx)
	if err != nil {
		return nil, err
	}
	if needed {
		log.Warn("first-run setup required: open the web UI and enter this setup code", "setup_code", code)
	}
	return svc, nil
}

func sweepAuth(ctx context.Context, svc *auth.Service, log *slog.Logger) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		if err := svc.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.Warn("sweep expired sessions", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// setupPoolerTLS builds the certificate manager for the poolers and makes
// sure a certificate exists before they need one.
func setupPoolerTLS(cfg config.Config, pm *pooler.Manager, st *settings.Store, log *slog.Logger) (*tlscert.Manager, error) {
	mode, err := tlscert.ParseMode(cfg.PoolerTLS.Mode)
	if err != nil {
		return nil, err
	}
	tc := tlscert.Config{
		Mode: mode, Dir: pm.Dir(), FileMode: pm.FileMode(), Reload: pm.Reload,
		DataDir: cfg.PoolerTLS.DataDir, ACMEEmail: cfg.PoolerTLS.ACMEEmail, ACMECA: cfg.PoolerTLS.ACMECA,
		SourceCert: cfg.PoolerTLS.CertFile, SourceKey: cfg.PoolerTLS.KeyFile, Log: log,
	}
	if _, port, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
		tc.ChallengePort, _ = strconv.Atoi(port)
	}
	if path := cfg.PoolerTLS.ACMECARoots; path != "" {
		pemBytes, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("XENIBASE_ACME_CA_ROOTS: %w", err)
		}
		tc.ACMERoots = x509.NewCertPool()
		if !tc.ACMERoots.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("XENIBASE_ACME_CA_ROOTS: no certificates in %s", path)
		}
	}
	m, err := tlscert.New(tc, st.DBHost())
	if err != nil {
		return nil, err
	}
	if err := m.Bootstrap(); err != nil {
		return nil, fmt.Errorf("pooler TLS: %w", err)
	}
	st.OnDBHostChange(m.SetHost)
	log.Info("pooler TLS", "mode", mode, "host", st.DBHost())
	return m, nil
}

func toAPITLS(s tlscert.Status) gen.TlsStatus {
	out := gen.TlsStatus{Mode: gen.TlsStatusMode(s.Mode), State: gen.TlsStatusState(s.State)}
	if s.Host != "" {
		out.Host = &s.Host
	}
	if s.Issuer != "" {
		out.Issuer = &s.Issuer
	}
	if !s.NotAfter.IsZero() {
		out.NotAfter = &s.NotAfter
	}
	if s.Err != "" {
		out.Error = &s.Err
	}
	return out
}

func setupProvisioning(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, keyring *crypto.Keyring, st *settings.Store, rs *regions.Service, log *slog.Logger) (*provision.Service, *pooler.Manager, error) {
	if cfg.Shared.AdminURL != "" {
		// A cluster that is down at boot should not keep the control plane
		// down; creates fail until it is back.
		if err := provision.RegisterSharedCluster(ctx, pool, keyring, provision.SharedCluster{
			NodeName:   cfg.Shared.NodeName,
			AdminURL:   cfg.Shared.AdminURL,
			PoolerHost: cfg.Shared.PoolerHost,
			PoolerPort: cfg.Shared.PoolerPort,
			NodeRole:   cfg.Shared.NodeRole,
		}, log); err != nil {
			log.Error("could not register shared cluster", "err", err)
		}
	}
	pc := cfg.Pooler
	if pc.ConfigDir == "" {
		log.Warn("project provisioning disabled: XENIBASE_POOLER_CONFIG_DIR is not set")
		return nil, nil, nil
	}

	// A stable, secret salt keeps the admin entry identical across syncs.
	adminVerifier, err := crypto.SCRAMVerifierWithSalt(pc.AdminPassword,
		keyring.Derive("pooler admin scram salt:"+pc.AdminUser, 16), crypto.SCRAMIterations)
	if err != nil {
		return nil, nil, fmt.Errorf("pooler admin password: %w", err)
	}
	// The poolers next to xenibase-server (V1/V2), unless the edge runs on
	// pooler hosts (V3 §2.1), which are administered through their nodes.
	var admins []*pooler.Admin
	if pc.Local {
		for _, a := range []struct{ name, addr string }{{"session", pc.SessionAddr}, {"transaction", pc.PooledAddr}} {
			adm, err := pooler.NewAdmin(a.name, a.addr, pc.AdminUser, pc.AdminPassword, pc.SSLMode)
			if err != nil {
				return nil, nil, err
			}
			admins = append(admins, adm)
		}
	}
	pm, err := pooler.NewManager(pc.ConfigDir, pc.FileMode, pool, admins,
		[]pooler.User{{Name: pc.AdminUser, Secret: adminVerifier}}, log)
	if err != nil {
		return nil, nil, err
	}
	pm.SetHome(rs.Home())
	// Bring the poolers in line with the metadata DB (e.g. after a restore
	// or a lost reload). Failure is not fatal: every flow syncs again.
	syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pm.Sync(syncCtx); err != nil {
		// Normal on first boot of the bundle: the poolers start after us.
		log.Warn("initial pooler sync incomplete; the files are written and the next sync reloads the poolers", "err", err)
	} else {
		log.Info("pooler config synced", "dir", pc.ConfigDir)
	}

	return provision.NewService(pool, keyring, pm, provision.Config{
		DBHost:           cfg.Public.Host,
		DBHostFunc:       st.DBHost,
		SessionPort:      cfg.Public.SessionPort,
		PooledPort:       cfg.Public.PooledPort,
		SSLMode:          cfg.Public.SSLMode,
		PGVersions:       cfg.PGVersions,
		HomeRegion:       rs.Home(),
		RegionHost:       rs.Host,
		SmokeSessionAddr: pc.SessionAddr,
		SmokePooledAddr:  pc.PooledAddr,
		SmokeSSLMode:     pc.SSLMode,
	}, log), pm, nil
}

// setupPoolerHosts connects the pooler manager to the pooler hosts and
// returns the arbiter that watches them and the floating IP.
func setupPoolerHosts(cfg config.Config, pm *pooler.Manager, ns *nodes.Service, rs *regions.Service, log *slog.Logger) *pooler.Arbiter {
	if pm == nil || ns == nil {
		return nil
	}
	pc := cfg.Pooler
	pm.SetHostDriver(nodes.PoolerDriver{S: ns}, pooler.HostAdminConfig{
		User: pc.AdminUser, Password: pc.AdminPassword, SSLMode: pc.SSLMode,
		SessionPort: pc.HostSessionPort, PooledPort: pc.HostPooledPort,
	})
	var fip floatip.Provider
	if pc.FloatingIP.ID != "" {
		fip = &floatip.Hetzner{API: pc.FloatingIP.API, Token: pc.FloatingIP.Token, IPID: pc.FloatingIP.ID}
		log.Info("managing the edge pooler's floating IP", "floating_ip", pc.FloatingIP.ID)
	}
	a := pooler.NewArbiter(pm, fip, log)
	// Other regions' pairs have their own floating IPs (V3 §6.1), in the
	// same Hetzner project.
	a.SetRegionIPs(func(region string) floatip.Provider {
		r, ok := rs.Cached(region)
		if !ok || r.FloatingIpID == nil || *r.FloatingIpID == "" || pc.FloatingIP.Token == "" {
			return nil
		}
		return &floatip.Hetzner{API: pc.FloatingIP.API, Token: pc.FloatingIP.Token, IPID: *r.FloatingIpID}
	})
	return a
}

// setupBackups builds the agent CA, the nodes service, and the backup
// service (which hooks final backups into deletes).
func setupBackups(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, keyring *crypto.Keyring, projects *provision.Service, log *slog.Logger) (*backup.Service, *nodes.Service, error) {
	ca, err := agentca.LoadOrCreate(ctx, pool, keyring)
	if err != nil {
		return nil, nil, fmt.Errorf("agent CA: %w", err)
	}
	ns, err := nodes.NewService(pool, ca, cfg.Backups.AgentBootstrapToken, log)
	if err == nil {
		ns.SetHomeRegion(cfg.Cloud.Region)
	}
	if err != nil {
		return nil, nil, err
	}
	bc := backup.Config{Hour: cfg.Backups.Hour, Jitter: cfg.Backups.Jitter}
	if u := cfg.Backups.MetadataURL; u != "" {
		pg, err := agentapi.ParseURL(u)
		if err != nil {
			return nil, nil, fmt.Errorf("metadata backup URL: %w", err)
		}
		bc.MetadataPG = pg
	}
	bs := backup.NewService(pool, keyring, ns, projects, bc, log)
	ds := dedicated.New(pool, keyring, ns, projects, bs, dedicated.Config{AdminVia: cfg.Backups.DedicatedAdminVia,
		RequireAnnouncement: cfg.Backups.RequireMaintenanceAnnouncement}, log)
	ds.Snapshot = bs.Snapshot
	projects.Instances = ds
	bs.Dedicated = ds
	return bs, ns, nil
}

// defaultMaxConns is the metadata pool size unless XENIBASE_DATABASE_URL sets
// pool_max_conns.
const defaultMaxConns = 16

func connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("XENIBASE_DATABASE_URL: %w", err)
	}
	if pcfg.ConnConfig.RuntimeParams["application_name"] == "" {
		pcfg.ConnConfig.RuntimeParams["application_name"] = "xenibase-server"
	}
	// pgx's default (4 on a small box) is too few: the webhook and job
	// schedulers each keep a connection for their lock, and a burst of
	// requests and workers then queue behind each other.
	if !strings.Contains(url, "pool_max_conns") {
		pcfg.MaxConns = max(pcfg.MaxConns, defaultMaxConns)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to metadata database: %w", err)
	}
	return pool, nil
}

// rotateMasterKey implements -rotate-master-key (spec §7.3): re-encrypt
// everything under the primary key, then prove the primary key alone opens
// it all.
func rotateMasterKey(cfg config.Config, keyring *crypto.Keyring, log *slog.Logger) error {
	if len(cfg.PreviousMasterKeys) == 0 {
		return errors.New("set the old key in XENIBASE_MASTER_KEY_PREVIOUS and the new one in XENIBASE_MASTER_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, log); err != nil {
		return err
	}
	res, err := rotate.Run(ctx, pool, keyring)
	if err != nil {
		return fmt.Errorf("rotation rolled back, nothing changed: %w", err)
	}
	only, err := crypto.NewKeyring(cfg.MasterKey)
	if err != nil {
		return err
	}
	if _, err := rotate.Verify(ctx, pool, only); err != nil {
		return fmt.Errorf("after rotation a secret does not open with the new key alone: %w", err)
	}
	for what, n := range res.Checked {
		log.Info("secrets re-encrypted", "kind", what, "checked", n, "rewrapped", res.Rewrapped[what])
	}
	log.Info("master key rotated; remove XENIBASE_MASTER_KEY_PREVIOUS and restart", "key_id", keyring.PrimaryID().String(),
		"sign_in_challenges_cleared", res.Cleared)
	return nil
}

// billingDocs keeps billing documents (proofs of payment, WHT credit
// notes) in the platform's backup storage, under billing/.
type billingDocs struct{ b *backup.Service }

func (d billingDocs) client(ctx context.Context) (*storage.Client, error) {
	_, t, err := d.b.StorageTarget(ctx)
	if err != nil {
		return nil, err
	}
	return storage.New(t)
}

func (d billingDocs) Put(ctx context.Context, key string, r io.Reader) error {
	c, err := d.client(ctx)
	if err != nil {
		return err
	}
	return c.Upload(ctx, c.Target().Key("billing/"+key), r)
}

func (d billingDocs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	return c.Download(ctx, c.Target().Key("billing/"+key))
}

// cloudProvider is the provider capacity automation creates servers with
// (V3 §5.1): Hetzner Cloud, or none (machines are registered by hand).
func cloudProvider(c config.Cloud) cloud.Provider {
	if c.Provider == cloud.Hetzner {
		return &cloud.HetznerProvider{API: c.HetznerAPI, Token: c.HetznerToken}
	}
	return cloud.ManualProvider{}
}

// platformPhone is the platform's SMS (Termii) and WhatsApp (support's
// number, an authentication template) for project auth codes (V4 §6.2).
func platformPhone(cfg config.Config, log *slog.Logger) services.PlatformPhone {
	a := cfg.AuthPhone
	p := services.PlatformPhone{SMSCostMinor: a.SMSPriceMinor, WhatsAppCostMinor: a.WhatsAppPriceMinor, Currency: a.Currency,
		DisallowFreePlans: !a.FreeAllowed}
	var sms []messaging.Provider
	if a.TermiiAPIKey != "" {
		sms = append(sms, messaging.Termii{BaseURL: a.TermiiURL, APIKey: a.TermiiAPIKey, SenderID: a.TermiiSenderID})
	}
	if a.ATOn() {
		sms = append(sms, messaging.AfricasTalking{BaseURL: a.ATURL, Username: a.ATUsername, APIKey: a.ATAPIKey, From: a.ATFrom})
	}
	switch len(sms) {
	case 1:
		p.SMS = sms[0]
	case 2:
		// Termii first; Africa's Talking when it fails (V4 §15).
		p.SMS = &messaging.Failover{Providers: sms, OnFail: func(provider string, err error) {
			log.Warn("platform SMS provider failed; trying the next", "provider", provider, "err", err)
		}}
	}
	if a.WhatsAppTemplate != "" && cfg.Support.WhatsAppOn() {
		p.WhatsApp = messaging.WhatsAppCloud{BaseURL: cfg.Support.WhatsAppGraphURL, PhoneNumberID: cfg.Support.WhatsAppPhoneNumberID,
			AccessToken: cfg.Support.WhatsAppAccessToken, Template: a.WhatsAppTemplate, Language: a.WhatsAppLanguage}
	}
	return p
}
