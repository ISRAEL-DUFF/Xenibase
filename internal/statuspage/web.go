package statuspage

import (
	"context"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/israel-duff/xenibase/internal/signature"
	"github.com/israel-duff/xenibase/internal/statusapi"
	"github.com/israel-duff/xenibase/internal/version"
)

//go:embed templates/*.html templates/style.css
var assets embed.FS

var funcs = template.FuncMap{
	"label":  stateLabel,
	"when":   func(t time.Time) string { return t.UTC().Format("2 Jan 2006, 15:04 MST") },
	"title":  statusLabel,
	"uptime": uptimeText,
	"sub":    func(a, b int) int { return a - b },
}

var pages = map[string]*template.Template{}

func init() {
	for _, name := range []string{"index", "incident", "message"} {
		pages[name] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+name+".html"))
	}
}

func stateLabel(s string) string {
	switch s {
	case statusapi.Operational:
		return "Operational"
	case statusapi.Degraded:
		return "Degraded"
	case statusapi.Down:
		return "Outage"
	default:
		return "No data"
	}
}

// View types, shared by the HTML page and the JSON API.

type componentView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Region      string    `json:"region,omitempty"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"`
	Detail      string    `json:"detail,omitempty"`
	Since       time.Time `json:"since"`
	// Uptime over the shown days, percent; nil without data.
	Uptime *float64  `json:"uptime_percent"`
	Days   []dayView `json:"days"`
}

type dayView struct {
	Day      string `json:"day"`
	Up       int    `json:"up_minutes"`
	Degraded int    `json:"degraded_minutes"`
	Down     int    `json:"down_minutes"`
	Unknown  int    `json:"unknown_minutes"`
	Class    string `json:"-"`
	Title    string `json:"-"`
}

type regionView struct {
	Name       string
	Components []componentView
}

type snapshot struct {
	Title        string               `json:"title"`
	URL          string               `json:"url"`
	UpdatedAt    time.Time            `json:"updated_at"`
	Status       string               `json:"status"`
	StatusLabel  string               `json:"status_label"`
	Components   []componentView      `json:"components"`
	Active       []statusapi.Incident `json:"active_incidents"`
	Upcoming     []statusapi.Incident `json:"upcoming_maintenance"`
	Recent       []statusapi.Incident `json:"recent_incidents"`
	Regions      []regionView         `json:"-"`
	Names        map[string]string    `json:"-"`
	Subscribable bool                 `json:"-"`
	Notice       string               `json:"-"`
	Version      string               `json:"-"`
}

const historyDays = 90

func uptimeText(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", math.Floor(*p*100)/100)
}

func (s *Service) snapshot(ctx context.Context, recentDays int) (*snapshot, error) {
	now := s.Now().UTC()
	tracks, err := s.st.tracks(ctx)
	if err != nil {
		return nil, err
	}
	from := now.Truncate(24*time.Hour).AddDate(0, 0, -(historyDays - 1))
	days, err := s.st.days(ctx, from)
	if err != nil {
		return nil, err
	}
	incidents, err := s.st.incidentsSince(ctx, now.AddDate(0, 0, -recentDays))
	if err != nil {
		return nil, err
	}
	snap := &snapshot{Title: s.cfg.Title, URL: s.cfg.PublicURL, UpdatedAt: now, Names: map[string]string{},
		Subscribable: s.subscriptions(), Version: version.Version}
	for _, in := range incidents {
		// Announced maintenance starts at its window (V3.1 §4.1): until
		// then it is upcoming, and leaves the components alone.
		if in.ResolvedAt == nil && in.StartedAt.After(now) {
			snap.Upcoming = append([]statusapi.Incident{in}, snap.Upcoming...)
			continue
		}
		if in.ResolvedAt == nil {
			snap.Active = append(snap.Active, in)
		} else {
			snap.Recent = append(snap.Recent, in)
		}
	}
	var states []string
	byRegion := map[string]int{}
	for _, c := range s.cfg.Components {
		snap.Names[c.ID] = c.Name
		t, ok := tracks[c.ID]
		measured := statusapi.Unknown
		if ok && now.Sub(t.CheckedAt) <= 3*s.cfg.Interval.Duration+time.Minute {
			measured = t.State
		}
		cv := componentView{ID: c.ID, Name: c.Name, Region: c.Region, Description: c.Description,
			Status: displayState(measured, c.ID, snap.Active), Detail: t.Detail, Since: t.Since}
		if cv.Status == statusapi.Operational {
			cv.Detail = ""
		}
		var good, total int
		for i := range historyDays {
			day := from.AddDate(0, 0, i).Format(time.DateOnly)
			d := days[c.ID][day]
			dv := dayView{Day: day, Up: d.Up, Degraded: d.Degraded, Down: d.Down, Unknown: d.Unknown}
			measuredMin := d.Up + d.Degraded + d.Down
			good += d.Up + d.Degraded
			total += measuredMin
			switch measuredMin {
			case 0:
				dv.Class, dv.Title = "none", day+": no data"
			default:
				pct := 100 * float64(d.Up+d.Degraded) / float64(measuredMin)
				dv.Class = "up"
				if d.Down > 0 || d.Degraded > 0 {
					dv.Class = "partial"
				}
				if pct < 99 {
					dv.Class = "down"
				}
				dv.Title = fmt.Sprintf("%s: %s up", day, uptimeText(&pct))
				if d.Down > 0 {
					dv.Title += fmt.Sprintf(", %d min down", d.Down)
				}
				if d.Degraded > 0 {
					dv.Title += fmt.Sprintf(", %d min degraded", d.Degraded)
				}
			}
			cv.Days = append(cv.Days, dv)
		}
		if total > 0 {
			p := 100 * float64(good) / float64(total)
			cv.Uptime = &p
		}
		states = append(states, cv.Status)
		snap.Components = append(snap.Components, cv)
		i, ok := byRegion[c.Region]
		if !ok {
			i = len(snap.Regions)
			byRegion[c.Region] = i
			snap.Regions = append(snap.Regions, regionView{Name: c.Region})
		}
		snap.Regions[i].Components = append(snap.Regions[i].Components, cv)
	}
	snap.Status, snap.StatusLabel = overall(states)
	return snap, nil
}

// Handler serves the page, its API and feed, and xenibase-server's pushes.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /incidents/{id}", s.incidentPage)
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, _ *http.Request) {
		b, _ := assets.ReadFile("templates/style.css")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /api/v1/status", s.apiStatus)
	mux.HandleFunc("GET /api/v1/incidents", s.apiIncidents)
	mux.HandleFunc("GET /api/v1/incidents/{id}", s.apiIncident)
	mux.HandleFunc("GET /feed.rss", s.feed)
	mux.HandleFunc("PUT "+statusapi.PathIncidents+"{id}", s.signed(s.pushIncident))
	mux.HandleFunc("POST "+statusapi.PathHeartbeat, s.signed(s.pushHeartbeat))
	mux.HandleFunc("PUT "+statusapi.PathSLATargets, s.signed(s.pushSLATargets))
	mux.HandleFunc("POST "+statusapi.PathSLAResults, s.signed(s.slaResultsHandler))
	mux.HandleFunc("PUT "+statusapi.PathManagedSubscribers, s.signed(s.pushManaged))
	mux.HandleFunc("POST /subscribe", s.subscribe)
	mux.HandleFunc("GET /subscribe/confirm", s.tokenForm("Confirm your subscription", "Confirm", "/subscribe/confirm"))
	mux.HandleFunc("POST /subscribe/confirm", s.tokenAction(s.Confirm, "You're subscribed. We'll email you when incidents are opened, updated and resolved."))
	mux.HandleFunc("GET /unsubscribe", s.tokenForm("Unsubscribe", "Unsubscribe", "/unsubscribe"))
	mux.HandleFunc("POST /unsubscribe", s.tokenAction(s.Unsubscribe, "You're unsubscribed. You won't get more emails from this status page."))
	return secureHeaders(mux)
}

func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

func (s *Service) render(w http.ResponseWriter, status int, page string, data any) {
	var b strings.Builder
	if err := pages[page].Execute(&b, data); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, b.String())
}

var notices = map[string]string{
	"check-email": "Check your inbox: we sent a link to confirm the subscription.",
	"invalid":     "That doesn't look like an email address.",
	"limit":       "Too many subscription requests from your address. Try again later.",
}

func (s *Service) index(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context(), 14)
	if err != nil {
		s.fail(w, err)
		return
	}
	snap.Notice = notices[r.URL.Query().Get("m")]
	w.Header().Set("Cache-Control", "no-cache")
	s.render(w, http.StatusOK, "index", snap)
}

type incidentData struct {
	*snapshot
	Incident *statusapi.Incident
}

func (s *Service) incidentPage(w http.ResponseWriter, r *http.Request) {
	in, err := s.st.incident(r.Context(), s.st.db, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	snap, err := s.snapshot(r.Context(), 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	if in == nil {
		s.render(w, http.StatusNotFound, "message", messageData{snapshot: snap, Heading: "Not found", Text: "There is no such incident."})
		return
	}
	slices.Reverse(in.Updates)
	s.render(w, http.StatusOK, "incident", incidentData{snapshot: snap, Incident: in})
}

func (s *Service) fail(w http.ResponseWriter, err error) {
	s.log.Error("status page", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Service) apiStatus(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context(), 14)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Service) apiIncidents(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.incidentsSince(r.Context(), s.Now().AddDate(0, 0, -historyDays))
	if err != nil {
		s.fail(w, err)
		return
	}
	if list == nil {
		list = []statusapi.Incident{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": list})
}

func (s *Service) apiIncident(w http.ResponseWriter, r *http.Request) {
	in, err := s.st.incident(r.Context(), s.st.db, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if in == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such incident"})
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// signed checks the Xenibase-Signature of a push from xenibase-server.
func (s *Service) signed(h func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.PushSecret == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "pushes are not enabled: set push_secret"})
			return
		}
		limit := int64(1 << 20)
		if r.URL.Path == statusapi.PathManagedSubscribers {
			limit = 32 << 20 // every paying organisation's contacts
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
			return
		}
		// The signature binds the method and path too: a body signed for
		// one endpoint is refused at another.
		if err := statusapi.VerifyPush(s.cfg.PushSecret, r.Header.Get(signature.Header), r.Method, r.URL.Path, body, s.Now(), 5*time.Minute); err != nil {
			s.log.Warn("refused an unsigned or badly signed push", "path", r.URL.Path, "remote", r.RemoteAddr, "err", err)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad signature"})
			return
		}
		h(w, r, body)
	}
}

func (s *Service) pushHeartbeat(w http.ResponseWriter, r *http.Request, body []byte) {
	var hb statusapi.Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	switch err := s.Heartbeat(r.Context(), hb); {
	case errors.Is(err, errStaleHeartbeat):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) pushIncident(w http.ResponseWriter, r *http.Request, body []byte) {
	var in statusapi.Incident
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if in.ID != r.PathValue("id") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the incident ID does not match the path"})
		return
	}
	for _, c := range in.Components {
		if !s.known[c] {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("component %q is not on this status page", c)})
			return
		}
	}
	switch err := s.PutIncident(r.Context(), in); {
	case errors.Is(err, errAutoIncident):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// RSS.

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Items       []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

func (s *Service) feed(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.incidentsSince(r.Context(), s.Now().AddDate(0, 0, -historyDays))
	if err != nil {
		s.fail(w, err)
		return
	}
	f := rssFeed{Version: "2.0", Channel: rssChannel{Title: s.cfg.Title, Link: s.cfg.PublicURL, Description: "Incidents on " + s.cfg.Title}}
	for _, in := range list {
		link := s.cfg.PublicURL + "/incidents/" + in.ID
		item := rssItem{Title: in.Title, Link: link, GUID: link, PubDate: in.StartedAt.Format(time.RFC1123Z)}
		if n := len(in.Updates); n > 0 {
			u := in.Updates[n-1]
			item.Title = in.Title + " — " + statusLabel(u.Status)
			item.GUID = link + "#" + u.ID
			item.PubDate = u.PostedAt.Format(time.RFC1123Z)
			item.Description = u.Body
		}
		f.Channel.Items = append(f.Channel.Items, item)
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(f)
}

// Subscriptions.

func (s *Service) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Service) subscribe(w http.ResponseWriter, r *http.Request) {
	if !s.subscriptions() {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !s.limiter.allow(s.clientIP(r), s.Now()) {
		http.Redirect(w, r, "/?m=limit#subscribe", http.StatusSeeOther)
		return
	}
	switch err := s.Subscribe(r.Context(), r.PostFormValue("email")); {
	case errors.Is(err, errBadEmail):
		http.Redirect(w, r, "/?m=invalid#subscribe", http.StatusSeeOther)
	case err != nil:
		s.fail(w, err)
	default:
		http.Redirect(w, r, "/?m=check-email#subscribe", http.StatusSeeOther)
	}
}

type messageData struct {
	*snapshot
	Heading, Text string
	// A form posting Token to Action, when set.
	Action, Button, Token string
}

// tokenForm shows a button that POSTs the link's token: following a link
// (or a mail scanner prefetching it) changes nothing by itself.
func (s *Service) tokenForm(heading, button, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap, err := s.snapshot(r.Context(), 0)
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, http.StatusOK, "message", messageData{snapshot: snap, Heading: heading, Action: action, Button: button, Token: r.URL.Query().Get("token")})
	}
}

func (s *Service) tokenAction(do func(context.Context, string) error, done string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		snap, err := s.snapshot(r.Context(), 0)
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		switch err := do(r.Context(), r.PostFormValue("token")); {
		case errors.Is(err, errBadToken):
			s.render(w, http.StatusBadRequest, "message", messageData{snapshot: snap, Heading: "Link not valid", Text: err.Error() + "."})
		case err != nil:
			s.fail(w, err)
		default:
			s.render(w, http.StatusOK, "message", messageData{snapshot: snap, Heading: "Done", Text: done})
		}
	}
}

func (s *Service) pushManaged(w http.ResponseWriter, r *http.Request, body []byte) {
	var m statusapi.ManagedSubscribers
	if err := json.Unmarshal(body, &m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.PutManaged(r.Context(), m); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
