// Package cli is the xenibase command-line tool (V2 §7.1): contexts, device
// login, and commands for organisations, projects, branches, SQL, backups,
// promotion, members, tokens and operations, over the generated API
// client. Every command takes --json; destructive ones need --confirm.
//
// Exit codes: 0 success, 1 error, 2 usage error, 3 operation failed,
// 4 permission denied.
package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/israel-duff/xenibase/internal/api/client"
	"github.com/israel-duff/xenibase/internal/version"
)

// Exit codes.
const (
	ExitOK         = 0
	ExitError      = 1
	ExitUsage      = 2
	ExitOpFailed   = 3
	ExitPermission = 4
)

// App runs one invocation.
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	// Getenv reads the environment (os.Getenv when nil).
	Getenv func(string) string
	// OpenBrowser opens a URL for the device login (none when nil).
	OpenBrowser func(url string) error
	// HTTPClient overrides the transport (tests).
	HTTPClient *http.Client

	// Global flags.
	json        bool
	contextName string
	server      string
	noWait      bool

	t   target
	api *client.ClientWithResponses
	hc  *http.Client
}

func (a *App) getenv(k string) string {
	if a.Getenv != nil {
		return a.Getenv(k)
	}
	return os.Getenv(k)
}

// ---- Errors -------------------------------------------------------------------

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// apiError is a non-2xx answer from the server.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the server answered %d", e.Status)
}

// opFailed is an operation that finished unsuccessfully.
type opFailed struct{ msg string }

func (e opFailed) Error() string { return e.msg }

func exitCode(err error) int {
	var ue usageError
	var ae *apiError
	var of opFailed
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ue):
		return ExitUsage
	case errors.As(err, &of):
		return ExitOpFailed
	case errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden):
		return ExitPermission
	}
	return ExitError
}

// check turns a generated response into an error unless it is 2xx.
func check(r interface{ StatusCode() int }, err error) error {
	if err != nil {
		return fmt.Errorf("could not reach the server: %w", err)
	}
	code := r.StatusCode()
	if code >= 200 && code < 300 {
		return nil
	}
	ae := &apiError{Status: code}
	if f := reflect.ValueOf(r).Elem().FieldByName("Body"); f.IsValid() {
		var e client.Error
		if json.Unmarshal(f.Bytes(), &e) == nil {
			ae.Code, ae.Message = e.Code, e.Message
		}
	}
	if ae.Message == "" {
		ae.Message = http.StatusText(code)
	}
	return ae
}

// ---- Running ---------------------------------------------------------------------

type command struct {
	name    string
	summary string
	run     func(a *App, args []string) error
	sub     []command
}

func (a *App) commands() []command {
	return []command{
		{name: "login", summary: "Log in with the browser (device login) or a pasted token", run: (*App).login},
		{name: "logout", summary: "Revoke this context's token and forget it", run: (*App).logout},
		{name: "context", summary: "Contexts: list, use, remove", sub: []command{
			{name: "list", summary: "List contexts", run: (*App).contextList},
			{name: "use", summary: "Switch to a context", run: (*App).contextUse},
			{name: "remove", summary: "Forget a context", run: (*App).contextRemove},
		}},
		{name: "whoami", summary: "Who the token acts as, and where", run: (*App).whoami},
		{name: "orgs", summary: "Organisations you belong to", sub: []command{
			{name: "list", summary: "List organisations", run: (*App).orgsList},
			{name: "create", summary: "Create an organisation", run: (*App).orgsCreate},
		}},
		{name: "org", summary: "The context's organisation", sub: []command{
			{name: "members", summary: "List members", run: (*App).orgMembers},
			{name: "invite", summary: "Invite someone: invite <email> --role member|admin|owner", run: (*App).orgInvite},
			{name: "remove", summary: "Remove a member: remove <email>", run: (*App).orgRemove},
			{name: "usage", summary: "Usage totals [--from --to]", run: (*App).orgUsage},
			{name: "quotas", summary: "Plan limits and use", run: (*App).orgQuotas},
		}},
		{name: "projects", summary: "Projects", sub: []command{
			{name: "list", summary: "List projects", run: (*App).projectsList},
			{name: "info", summary: "Show a project: info <p>", run: (*App).projectsInfo},
			{name: "create", summary: "Create a project: create <name> [--tier shared|dedicated]", run: (*App).projectsCreate},
			{name: "delete", summary: "Delete a project: delete <p> --confirm <name>", run: (*App).projectsDelete},
		}},
		{name: "regions", summary: "The regions projects can be created in", run: (*App).regionsList},
		{name: "connect", summary: "Print a connection URL with your personal login, or --psql to open psql", run: (*App).connect},
		{name: "creds", summary: "Your personal database login: creds <p> [--rotate]", run: (*App).creds},
		{name: "sql", summary: "Run SQL: sql <p> -c \"select …\" | -f file.sql [--csv]", run: (*App).sql},
		{name: "backup", summary: "Backups", sub: []command{
			{name: "list", summary: "List a project's backups: list <p>", run: (*App).backupList},
			{name: "create", summary: "Back up now: create <p>", run: (*App).backupCreate},
			{name: "download", summary: "Save as a pg_dump file (owners): download <p> [--backup <id>] [-o file]", run: (*App).backupDownload},
			{name: "restore", summary: "Restore: restore <p> --backup <id> [--into <name>] | --in-place --confirm <name>", run: (*App).backupRestore},
			{name: "retention", summary: "How long nightly backups are kept: retention <p> standard|extended|long", run: (*App).backupRetention},
		}},
		{name: "pitr", summary: "Point-in-time recovery of a dedicated project", sub: []command{
			{name: "window", summary: "The recovery window: window <p> 7|14|30", run: (*App).pitrWindow},
		}},
		{name: "branch", summary: "Branches: throwaway copies of a project", sub: []command{
			{name: "list", summary: "List a project's branches: list <p>", run: (*App).branchList},
			{name: "create", summary: "create <p> <name> [--from backup|live] [--schema-only] [--ttl 72h] [--env] [--replace]", run: (*App).branchCreate},
			{name: "reset", summary: "Reset from the parent, keeping URL and password: reset <branch> [--from backup|live]", run: (*App).branchReset},
			{name: "delete", summary: "Delete: delete <branch> --confirm <name>", run: (*App).branchDelete},
			{name: "extend", summary: "Push back the expiry: extend <branch> [--ttl 7d]", run: (*App).branchExtend},
			{name: "detach", summary: "Make a branch a standalone project: detach <branch>", run: (*App).branchDetach},
		}},
		{name: "webhooks", summary: "Database webhooks: table changes POSTed to a URL", sub: []command{
			{name: "list", summary: "List: list <p>", run: (*App).webhooksList},
			{name: "create", summary: "create <p> <name> --tables orders --url https://… [--events INSERT,UPDATE] [--columns c] [--header K=V]", run: (*App).webhooksCreate},
			{name: "delete", summary: "Delete: delete <p> <webhook>", run: (*App).webhooksDelete},
			{name: "deliveries", summary: "The delivery log: deliveries <p> <webhook> [--dead]", run: (*App).webhooksDeliveries},
			{name: "replay", summary: "Send dead letters again: replay <p> <webhook> --all | --ids 1,2", run: (*App).webhooksReplay},
		}},
		{name: "jobs", summary: "Scheduled jobs: SQL or HTTP on a cron schedule", sub: []command{
			{name: "list", summary: "List: list <p>", run: (*App).jobsList},
			{name: "create", summary: "create <p> <name> --cron '0 3 * * *' (--sql '…' | --url https://…) [--tz Europe/Berlin]", run: (*App).jobsCreate},
			{name: "pause", summary: "Pause: pause <p> <job>", run: (*App).jobsPause},
			{name: "resume", summary: "Resume: resume <p> <job>", run: (*App).jobsResume},
			{name: "run", summary: "Run now: run <p> <job>", run: (*App).jobsRun},
			{name: "history", summary: "Recent runs: history <p> <job>", run: (*App).jobsHistory},
		}},
		{name: "promote", summary: "Move a project to a dedicated instance: promote <p> [--node <id>] [--profile]", run: (*App).promote},
		{name: "demote", summary: "Move a dedicated project back to the shared tier: demote <p> [--node <id>] [--check] [--accept-warnings]", run: (*App).demote},
		{name: "insights", summary: "Query insights (Pro, Team and dedicated)", sub: []command{
			{name: "queries", summary: "Top queries: queries <p> [--range 24h] [--sort total|mean|calls|rows]", run: (*App).insightsQueries},
			{name: "slow", summary: "Slow queries: slow <p> [--range 24h]", run: (*App).insightsSlow},
			{name: "indexes", summary: "Index suggestions, unused and duplicate indexes: indexes <p>", run: (*App).insightsIndexes},
		}},
		{name: "instance", summary: "A dedicated project's instance size and disk", sub: []command{
			{name: "resize", summary: "resize <p> (--profile medium | --cpus 4 --memory 8192) [--dry-run]", run: (*App).instanceResize},
			{name: "disk", summary: "Grow the disk: disk <p> --gb 160 [--dry-run]", run: (*App).instanceDisk},
		}},
		{name: "ha", summary: "High availability for a dedicated project", sub: []command{
			{name: "status", summary: "Members, lag, failovers and availability: status <p>", run: (*App).haStatus},
			{name: "enable", summary: "Add a standby on another node: enable <p> [--node <id>] [--sync]", run: (*App).haEnable},
			{name: "disable", summary: "Remove the standby: disable <p>", run: (*App).haDisable},
			{name: "switchover", summary: "Planned switchover: switchover <p> [--to <member>]", run: (*App).haSwitchover},
			{name: "sync", summary: "Synchronous replication: sync <p> on|off", run: (*App).haSync},
		}},
		{name: "replicas", summary: "Read replicas of a dedicated project", sub: []command{
			{name: "list", summary: "Replicas, lag, rotation and the read-only URL: list <p>", run: (*App).replicasList},
			{name: "create", summary: "Add one (up to 2): create <p> [--node <id>] [--region <r>] [--feedback on|off] [--standby-delay <s>]", run: (*App).replicasCreate},
			{name: "set", summary: "Standby settings: set <p> <replica-id> [--feedback on|off] [--standby-delay <s>]", run: (*App).replicasSet},
			{name: "delete", summary: "Remove one: delete <p> <replica-id>", run: (*App).replicasDelete},
			{name: "detach", summary: "Make one a standalone project: detach <p> <replica-id> --name <new>", run: (*App).replicasDetach},
		}},
		{name: "migrate", summary: "Move a project's users, files and policies across from another platform", sub: []command{
			{name: "supabase", summary: "From Supabase, after importing its database: supabase <p> [--step …] [--source <url>] [--s3-…]", run: (*App).migrateSupabase},
		}},
		{name: "resume", summary: "Resume a paused Free project, or restore an archived one: resume <p>", run: (*App).resume},
		{name: "upgrade", summary: "Upgrade to a newer Postgres major: upgrade <p> --to 18 [--check]", run: (*App).upgrade},
		{name: "move", summary: "Move a project to another node (platform admin): move <p> --node <id>", run: (*App).move},
		{name: "moves", summary: "A project's recent moves between instances: moves <p>", run: (*App).moves},
		{name: "members", summary: "Project members", sub: []command{
			{name: "list", summary: "List: list <p>", run: (*App).membersList},
			{name: "invite", summary: "Add: invite <p> <email> --role admin|developer|read_only", run: (*App).membersInvite},
			{name: "remove", summary: "Remove: remove <p> <email>", run: (*App).membersRemove},
		}},
		{name: "billing", summary: "The organisation's plan, forecast and invoices (owners and billing members)", sub: []command{
			{name: "show", summary: "Plan, forecast, budget and spend cap: show", run: (*App).billingShow},
			{name: "plan", summary: "Change plan: plan free|pro|team [--annual] [--now] [--dry-run]", run: (*App).billingPlan},
			{name: "invoices", summary: "List invoices", run: (*App).billingInvoices},
			{name: "invoice", summary: "Show or download one: invoice <number|id> [--pdf file.pdf]", run: (*App).billingInvoice},
			{name: "pay", summary: "Pay an invoice from the wallet now: pay <number|id>", run: (*App).billingPay},
			{name: "wallet", summary: "The wallet's balance, what is owed and the debit consent", run: (*App).billingWallet},
			{name: "transfer", summary: "Show the bank account to transfer to", run: (*App).billingTransfer},
			{name: "payments", summary: "List payments received", run: (*App).billingPayments},
		}},
		{name: "services", summary: "Backend services: the project's data API, auth, storage and realtime", sub: []command{
			{name: "status", summary: "URL and keys: status <p>", run: (*App).servicesStatus},
			{name: "enable", summary: "Turn on (prints the first keys once): enable <p>", run: (*App).servicesEnable},
			{name: "disable", summary: "Turn off (keys revoked): disable <p>", run: (*App).servicesDisable},
		}},
		{name: "auth", summary: "Your app's users and signing keys (backend services)", sub: []command{
			{name: "users", summary: "App users", sub: []command{
				{name: "list", summary: "list <p> [--search …] [--page n]", run: (*App).authUsersList},
				{name: "show", summary: "A user's sessions and activity: show <p> <user id>", run: (*App).authUsersShow},
				{name: "invite", summary: "Email an invitation: invite <p> <email>", run: (*App).authUsersInvite},
				{name: "ban", summary: "ban <p> <user id> [--for 24h]", run: (*App).authUsersBan},
				{name: "unban", summary: "unban <p> <user id>", run: (*App).authUsersUnban},
				{name: "signout", summary: "End all sessions: signout <p> <user id>", run: (*App).authUsersSignOut},
				{name: "delete", summary: "delete <p> <user id>", run: (*App).authUsersDelete},
			}},
			{name: "rotate-key", summary: "Sign tokens with a new key: rotate-key <p>", run: (*App).authRotateKey},
			{name: "config", summary: "Sign-in settings, providers, phone spend and caps: config <p>", run: (*App).authConfig},
			{name: "set", summary: `Change settings: set <p> '{"settings":{"phone_channels":["whatsapp"]}}' (an AuthConfigUpdate)`, run: (*App).authSet},
			{name: "hooks", summary: "Recent auth webhook deliveries: hooks <p>", run: (*App).authHooks},
		}},
		{name: "storage", summary: "Your app's files (backend services); remote paths are ss:///bucket/path", sub: []command{
			{name: "buckets", summary: "Buckets", sub: []command{
				{name: "list", summary: "list <p>", run: (*App).storageBuckets},
				{name: "create", summary: "create <p> <bucket> [--public] [--size-limit 10MB] [--types image/*]", run: (*App).storageBucketsCreate},
				{name: "delete", summary: "delete <p> <bucket> [--force]", run: (*App).storageBucketsDelete},
			}},
			{name: "ls", summary: "List files: ls <p> ss:///bucket[/folder/]", run: (*App).storageLs},
			{name: "cp", summary: "Upload (up to 50 MB) or download: cp <p> <file> ss:///bucket/path | cp <p> ss:///bucket/path <file|->", run: (*App).storageCp},
			{name: "rm", summary: "Delete files: rm <p> ss:///bucket/path…", run: (*App).storageRm},
			{name: "sign", summary: "A signed download URL: sign <p> ss:///bucket/path [--expires 1h]", run: (*App).storageSign},
		}},
		{name: "realtime", summary: "Your app's realtime (backend services): which tables' changes clients get", sub: []command{
			{name: "status", summary: "Tables, limits and this month's use: status <p>", run: (*App).realtimeStatus},
			{name: "enable", summary: "Deliver a table's changes: enable <p> <[schema.]table>", run: (*App).realtimeEnable},
			{name: "disable", summary: "Stop: disable <p> <[schema.]table>", run: (*App).realtimeDisable},
			{name: "history", summary: "Keep broadcasts on these topics 7 days: history <p> [topic…]", run: (*App).realtimeHistory},
		}},
		{name: "policies", summary: "Row-level security of the data API's tables (backend services)", sub: []command{
			{name: "list", summary: "Policies per table, with their roles: list <p> [--table t]", run: (*App).policiesList},
			{name: "lint", summary: "The security advisor's findings; exits 1 on any marked danger (for CI): lint <p>", run: (*App).policiesLint},
		}},
		{name: "logs", summary: "Logs", sub: []command{
			{name: "api", summary: "The API's request log: api <p> [--follow] [--status 5xx] [--path /data/v1/…]", run: (*App).logsAPI},
		}},
		{name: "gen", summary: "Generate code", sub: []command{
			{name: "types", summary: "Types for the SDKs: types --lang ts|dart|go --project <p> [-o file] [--package name]", run: (*App).genTypes},
		}},
		{name: "keys", summary: "Backend services' API keys", sub: []command{
			{name: "list", summary: "List: list <p>", run: (*App).keysList},
			{name: "create", summary: "create <p> --name … [--kind publishable|secret]", run: (*App).keysCreate},
			{name: "revoke", summary: "Revoke: revoke <p> <key id>", run: (*App).keysRevoke},
		}},
		{name: "tokens", summary: "API tokens", sub: []command{
			{name: "list", summary: "List your tokens", run: (*App).tokensList},
			{name: "create", summary: "create --name … --scopes read,write [--project <p>] [--expires 90d]", run: (*App).tokensCreate},
			{name: "revoke", summary: "Revoke a token: revoke <id>", run: (*App).tokensRevoke},
		}},
		{name: "operations", summary: "Operations", sub: []command{
			{name: "get", summary: "Show an operation: get <id> [--follow]", run: (*App).operationsGet},
		}},
		{name: "version", summary: "Print the CLI version", run: func(a *App, _ []string) error {
			fmt.Fprintf(a.Stdout, "xenibase %s (%s)\n", version.Version, version.Commit)
			return nil
		}},
	}
}

// Run runs args (without the program name) and returns the exit code.
func (a *App) Run(args []string) int {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	if a.Stdin == nil {
		a.Stdin = os.Stdin
	}
	args = a.globalFlags(args)
	err := a.dispatch(a.commands(), args, "xenibase")
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		fmt.Fprintln(a.Stderr, "xenibase: "+err.Error())
	}
	return exitCode(err)
}

// globalFlags takes --json, --context, --server and --no-wait from
// anywhere in args.
func (a *App) globalFlags(args []string) []string {
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, val, hasVal := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "--") {
			rest = append(rest, arg)
			continue
		}
		switch name {
		case "json":
			a.json = true
		case "no-wait":
			a.noWait = true
		case "context", "server":
			if !hasVal && i+1 < len(args) {
				i++
				val = args[i]
			}
			if name == "context" {
				a.contextName = val
			} else {
				a.server = val
			}
		default:
			rest = append(rest, arg)
		}
	}
	return rest
}

func (a *App) dispatch(cmds []command, args []string, path string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		a.usage(cmds, path)
		if len(args) == 0 {
			return usageErrorf("missing command")
		}
		return flag.ErrHelp
	}
	for _, c := range cmds {
		if c.name != args[0] {
			continue
		}
		if c.sub != nil {
			return a.dispatch(c.sub, args[1:], path+" "+c.name)
		}
		return c.run(a, args[1:])
	}
	a.usage(cmds, path)
	return usageErrorf("unknown command %q", args[0])
}

func (a *App) usage(cmds []command, path string) {
	fmt.Fprintf(a.Stderr, "Usage: %s <command> [flags]\n\n", path)
	w := tabwriter.NewWriter(a.Stderr, 0, 2, 2, ' ', 0)
	for _, c := range cmds {
		fmt.Fprintf(w, "  %s\t%s\n", c.name, c.summary)
	}
	_ = w.Flush()
	if path == "xenibase" {
		fmt.Fprint(a.Stderr, "\nGlobal flags: --json, --context <name>, --server <url>, --no-wait\n"+
			"Environment: XENIBASE_TOKEN and XENIBASE_SERVER (CI), XENIBASE_CONTEXT, XENIBASE_CA_FILE\n")
	}
}

// parse parses a command's flags wherever they appear among its
// positional arguments, and returns the positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageErrorf("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func need(pos []string, n int, usage string) error {
	if len(pos) != n {
		return usageErrorf("usage: xenibase %s", usage)
	}
	return nil
}

// ---- The API ----------------------------------------------------------------------

func httpClient(caFile string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr, Timeout: 0}, nil
}

// connectAPI resolves the target and builds the authenticated client.
func (a *App) connectAPI() error {
	if a.api != nil {
		return nil
	}
	t, err := a.resolve()
	if err != nil {
		return err
	}
	return a.useTarget(t)
}

func (a *App) useTarget(t target) error {
	hc := a.HTTPClient
	if hc == nil {
		var err error
		if hc, err = httpClient(t.CAFile); err != nil {
			return err
		}
	}
	c, err := client.NewClientWithResponses(t.Server, client.WithHTTPClient(hc), client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		if t.Token != "" {
			r.Header.Set("Authorization", "Bearer "+t.Token)
		}
		r.Header.Set("User-Agent", "xenibase-cli/"+version.Version)
		return nil
	}))
	if err != nil {
		return err
	}
	a.t, a.api, a.hc = t, c, hc
	return nil
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

// ---- Output -------------------------------------------------------------------------

// emit prints v as JSON with --json; otherwise human runs.
func (a *App) emit(v any, human func(w io.Writer)) error {
	if a.json {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	w := tabwriter.NewWriter(a.Stdout, 0, 2, 2, ' ', 0)
	human(w)
	return w.Flush()
}

func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}
