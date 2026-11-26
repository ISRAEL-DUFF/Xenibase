package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/israel-duff/xenibase/internal/api/client"
)

// login: xenibase login --server https://… [--name ctx] [--scopes read,write]
// [--token xb_…] [--no-browser] [--ca-file f]
func (a *App) login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	name := fs.String("name", "", "context name (default: the server's host and the organisation)")
	scopes := fs.String("scopes", "read,write", "scopes to ask for: read, write, admin")
	token := fs.String("token", "", "use this token instead of the browser (or - to read it from stdin)")
	noBrowser := fs.Bool("no-browser", false, "don't open the browser; print the link")
	caFile := fs.String("ca-file", "", "trust this CA certificate for the server")
	clientName := fs.String("client-name", "", "how the login is labelled on the approval page")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	server := strings.TrimRight(a.server, "/")
	if server == "" {
		server = strings.TrimRight(a.getenv("XENIBASE_SERVER"), "/")
	}
	if server == "" {
		return usageErrorf("usage: xenibase login --server https://xenibase.example.com")
	}
	if err := a.checkServerURL(server); err != nil {
		return err
	}
	ca := *caFile
	if ca == "" {
		ca = a.getenv("XENIBASE_CA_FILE")
	}
	if err := a.useTarget(target{Server: server, CAFile: ca}); err != nil {
		return err
	}
	secret := *token
	if secret == "-" {
		b, err := io.ReadAll(io.LimitReader(a.Stdin, 4096))
		if err != nil {
			return err
		}
		secret = strings.TrimSpace(string(b))
	}
	if secret == "" {
		var err error
		if secret, err = a.deviceLogin(*clientName, *scopes, *noBrowser); err != nil {
			return err
		}
	}
	// Who is this, and which organisation?
	if err := a.useTarget(target{Server: server, Token: secret, CAFile: ca}); err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	me, err := a.api.GetMeWithResponse(c)
	if err := check(me, err); err != nil {
		return err
	}
	if me.JSON200.Token == nil {
		return fmt.Errorf("that is not an API token")
	}
	orgs, err := a.api.ListOrgsWithResponse(c)
	if err := check(orgs, err); err != nil {
		return err
	}
	ctxt := &Context{Server: server, Token: secret, TokenID: me.JSON200.Token.Id.String(), Org: me.JSON200.Token.OrgId.String(),
		User: string(me.JSON200.Email), CAFile: ca}
	slug := ""
	for _, o := range orgs.JSON200.Items {
		if o.Id == me.JSON200.Token.OrgId {
			ctxt.OrgName, slug = o.Name, o.Slug
		}
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	n := *name
	if n == "" {
		u, _ := url.Parse(server)
		n = u.Hostname()
		if slug != "" {
			n += "/" + slug
		}
	}
	cfg.Contexts[n] = ctxt
	cfg.Current = n
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	return a.emit(map[string]any{"context": n, "server": server, "org_id": ctxt.Org, "org": ctxt.OrgName, "user": ctxt.User}, func(w io.Writer) {
		fmt.Fprintf(w, "Logged in to %s as %s, in organisation %s (context %q).\n", server, ctxt.User, ctxt.OrgName, n)
	})
}

// deviceLogin runs the device flow and returns the token.
func (a *App) deviceLogin(clientName, scopes string, noBrowser bool) (string, error) {
	if clientName == "" {
		host, _ := os.Hostname()
		clientName = "xenibase CLI"
		if host != "" {
			clientName += " on " + host
		}
	}
	var sc []client.TokenScope
	for _, s := range strings.Split(scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			sc = append(sc, client.TokenScope(s))
		}
	}
	c, cancel := ctx()
	defer cancel()
	start, err := a.api.StartDeviceLoginWithResponse(c, client.DeviceStartRequest{ClientName: &clientName, Scopes: &sc})
	if err := check(start, err); err != nil {
		return "", err
	}
	d := start.JSON200
	fmt.Fprintf(a.Stderr, "To log in, open %s\nand confirm the code %s (it expires in %d minutes).\n", d.VerificationUriComplete, d.UserCode, d.ExpiresIn/60)
	if !noBrowser && a.OpenBrowser != nil {
		if err := a.OpenBrowser(d.VerificationUriComplete); err == nil {
			fmt.Fprintln(a.Stderr, "Opened your browser. Waiting for you to approve…")
		}
	}
	interval := time.Duration(max(d.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		pc, cancel := ctx()
		r, err := a.api.PollDeviceLoginWithResponse(pc, client.DevicePollRequest{DeviceCode: d.DeviceCode})
		cancel()
		err = check(r, err)
		if err == nil {
			return r.JSON200.Secret, nil
		}
		var ae *apiError
		ok := errors.As(err, &ae)
		switch {
		case ok && ae.Code == "authorization_pending":
		case ok && ae.Code == "slow_down":
			interval += 5 * time.Second
		case ok && ae.Code == "access_denied":
			return "", &apiError{Status: 403, Code: ae.Code, Message: "the login was denied"}
		default:
			return "", err
		}
	}
	return "", fmt.Errorf("the login code expired; run xenibase login again")
}

func (a *App) logout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	keep := fs.Bool("keep-token", false, "forget the context without revoking its token")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	name := a.contextName
	if name == "" {
		name = cfg.Current
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		return usageErrorf("not logged in")
	}
	if !*keep && c.TokenID != "" {
		// Only with the context's own server and token: with --server
		// naming another server, resolve picks a different context.
		if err := a.connectAPI(); err == nil && a.t.Name == name {
			cc, cancel := ctx()
			defer cancel()
			if r, err := a.api.RevokeMyTokenWithResponse(cc, mustUUID(c.TokenID)); check(r, err) != nil {
				fmt.Fprintln(a.Stderr, "warning: could not revoke the token on the server")
			}
		} else {
			fmt.Fprintln(a.Stderr, "warning: could not revoke the token on the server")
		}
	}
	delete(cfg.Contexts, name)
	if cfg.Current == name {
		cfg.Current = ""
	}
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "Logged out of %s.\n", name)
	return nil
}

func (a *App) contextList(args []string) error {
	if _, err := parse(flag.NewFlagSet("context list", flag.ContinueOnError), args); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	type row struct {
		Name    string `json:"name"`
		Current bool   `json:"current"`
		Server  string `json:"server"`
		Org     string `json:"org"`
		User    string `json:"user"`
	}
	var rows []row
	for _, n := range cfg.names() {
		c := cfg.Contexts[n]
		rows = append(rows, row{n, n == cfg.Current, c.Server, c.OrgName, c.User})
	}
	return a.emit(rows, func(w io.Writer) {
		fmt.Fprintln(w, "\tNAME\tSERVER\tORG\tUSER")
		for _, r := range rows {
			mark := ""
			if r.Current {
				mark = "*"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, r.Name, r.Server, r.Org, r.User)
		}
	})
}

func (a *App) contextUse(args []string) error {
	pos, err := parse(flag.NewFlagSet("context use", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "context use <name>"); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if _, ok := cfg.Contexts[pos[0]]; !ok {
		return usageErrorf("no context named %q", pos[0])
	}
	cfg.Current = pos[0]
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "Using %s.\n", pos[0])
	return nil
}

func (a *App) contextRemove(args []string) error {
	pos, err := parse(flag.NewFlagSet("context remove", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "context remove <name>"); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	delete(cfg.Contexts, pos[0])
	if cfg.Current == pos[0] {
		cfg.Current = ""
	}
	return a.saveConfig(cfg)
}

func (a *App) whoami(args []string) error {
	if _, err := parse(flag.NewFlagSet("whoami", flag.ContinueOnError), args); err != nil {
		return err
	}
	if err := a.connectAPI(); err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	me, err := a.api.GetMeWithResponse(c)
	if err := check(me, err); err != nil {
		return err
	}
	return a.emit(me.JSON200, func(w io.Writer) {
		fmt.Fprintf(w, "User:\t%s\n", me.JSON200.Email)
		fmt.Fprintf(w, "Server:\t%s\n", a.t.Server)
		if t := me.JSON200.Token; t != nil {
			fmt.Fprintf(w, "Token:\t%s (%s)\n", t.Name, t.Id)
			fmt.Fprintf(w, "Organisation:\t%s\n", t.OrgId)
			sc := make([]string, len(t.Scopes))
			for i, s := range t.Scopes {
				sc[i] = string(s)
			}
			fmt.Fprintf(w, "Scopes:\t%s\n", strings.Join(sc, ", "))
			if t.ProjectIds != nil {
				fmt.Fprintf(w, "Projects:\t%d (restricted)\n", len(*t.ProjectIds))
			}
			fmt.Fprintf(w, "Expires:\t%s\n", when(&t.ExpiresAt))
		}
	})
}
