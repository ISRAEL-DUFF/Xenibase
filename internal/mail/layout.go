package mail

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/israel-duff/xenibase/internal/brand"
)

// Xenibase's email is sent as multipart/alternative: a plain-text part (what
// the code has always written, kept as it is, for clients and filters that
// want text) and an HTML part in one branded layout. A message is either
// written as structured Content, or it is plain text that FromText lays out.

var (
	publicMu  sync.RWMutex
	publicURL string
)

// SetPublicURL sets the address of the web app (https://app.example.com),
// which the logo in a message is loaded from and its footer links to. Without
// it a message has no logo and no link. Call it once at startup.
func SetPublicURL(u string) {
	publicMu.Lock()
	defer publicMu.Unlock()
	publicURL = strings.TrimRight(strings.TrimSpace(u), "/")
}

func baseURL() string {
	publicMu.RLock()
	defer publicMu.RUnlock()
	return publicURL
}

// Block kinds.
const (
	BlockPara   = "p"
	BlockList   = "list"
	BlockFacts  = "facts"
	BlockButton = "button"
	BlockCode   = "code"
)

// Fact is one line of a facts table: an invoice's number, an amount, a date.
type Fact struct{ Label, Value string }

// Block is one piece of a message's body.
type Block struct {
	Kind  string
	Text  string   // a paragraph, a button's label, a code
	Items []string // a list's items
	Facts []Fact
	URL   string // a button's address
}

// P is a paragraph. Line breaks in it are kept.
func P(text string) Block { return Block{Kind: BlockPara, Text: text} }

// List is a bulleted list.
func List(items ...string) Block { return Block{Kind: BlockList, Items: items} }

// Facts is a table of labelled values.
func Facts(f ...Fact) Block { return Block{Kind: BlockFacts, Facts: f} }

// Button is the one thing to do: a link with a label.
func Button(label, url string) Block { return Block{Kind: BlockButton, Text: label, URL: url} }

// Code is a short code to copy, such as a verification code.
func Code(code string) Block { return Block{Kind: BlockCode, Text: code} }

// Content is a message as structured text.
type Content struct {
	// Preheader is the preview some clients show beside the subject.
	Preheader string
	// Title is the heading at the top of the message.
	Title  string
	Blocks []Block
	// SignOff closes the message ("The Xenibase team"); empty for none.
	SignOff string
	// Note is the small print: why the recipient got this.
	Note string
	// Neutral lays the message out under someone else's name instead of
	// Xenibase's, with no Xenibase logo, tagline or link: for a customer's own
	// app sending to its own users. BrandName is that name; Accent (#rrggbb)
	// is the colour of its button and links, a neutral slate when empty.
	Neutral   bool
	BrandName string
	Accent    string
}

// Text renders c as plain text.
func (c Content) Text() string {
	var b strings.Builder
	if c.Title != "" {
		b.WriteString(c.Title + "\n\n")
	}
	for _, bl := range c.Blocks {
		switch bl.Kind {
		case BlockList:
			for _, it := range bl.Items {
				b.WriteString("- " + it + "\n")
			}
		case BlockFacts:
			for _, f := range bl.Facts {
				b.WriteString(f.Label + ": " + f.Value + "\n")
			}
		case BlockButton:
			b.WriteString(bl.Text + ":\n" + bl.URL + "\n")
		case BlockCode:
			b.WriteString("Your code: " + bl.Text + "\n")
		default:
			b.WriteString(bl.Text + "\n")
		}
		b.WriteString("\n")
	}
	if c.SignOff != "" {
		b.WriteString(c.SignOff + "\n\n")
	}
	if c.Note != "" {
		b.WriteString(c.Note + "\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// neutralAccent is the button colour of a message that isn't Xenibase's:
// a quiet slate, so nothing in it looks like our brand.
const neutralAccent = "#1f2937"

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

var urlLine = regexp.MustCompile(`^https?://\S+$`)

// FromText lays out a plain-text message: its paragraphs, bulleted lists,
// and any line that is only a link (which becomes a button). The subject is
// the title.
func FromText(subject, body string) Content {
	c := Content{Title: strings.TrimSpace(strings.TrimPrefix(subject, "[Xenibase]"))}
	for _, para := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		var lines []string
		for _, l := range strings.Split(para, "\n") {
			if l = strings.TrimRight(l, " \t"); strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) == 0 {
			continue
		}
		bullets := true
		for _, l := range lines {
			if t := strings.TrimSpace(l); !strings.HasPrefix(t, "- ") && !strings.HasPrefix(t, "* ") {
				bullets = false
			}
		}
		switch {
		case len(lines) == 1 && urlLine.MatchString(strings.TrimSpace(lines[0])):
			c.Blocks = append(c.Blocks, Button("Open the link", strings.TrimSpace(lines[0])))
		case bullets:
			items := make([]string, len(lines))
			for i, l := range lines {
				items[i] = strings.TrimSpace(strings.TrimSpace(l)[2:])
			}
			c.Blocks = append(c.Blocks, List(items...))
		default:
			c.Blocks = append(c.Blocks, P(strings.Join(lines, "\n")))
		}
	}
	for _, bl := range c.Blocks {
		if bl.Kind == BlockPara {
			c.Preheader = firstWords(bl.Text, 110)
			break
		}
	}
	return c
}

func firstWords(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}

// safeURL is u if it is an http or https address, and empty otherwise: a
// message never carries a link with another scheme.
func safeURL(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return ""
	}
	return p.String()
}

var linkRe = regexp.MustCompile(`https?://[^\s<>"]+`)

// linkify escapes s and turns the web addresses in it into links.
func linkify(s string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range linkRe.FindAllStringIndex(s, -1) {
		raw := strings.TrimRight(s[m[0]:m[1]], ".,;:!?)")
		end := m[0] + len(raw)
		b.WriteString(template.HTMLEscapeString(s[last:m[0]]))
		if u := safeURL(raw); u != "" {
			fmt.Fprintf(&b, `<a href="%s" style="color:%s;text-decoration:underline">%s</a>`,
				template.HTMLEscapeString(u), brand.Teal, template.HTMLEscapeString(raw))
		} else {
			b.WriteString(template.HTMLEscapeString(raw))
		}
		last = end
	}
	b.WriteString(template.HTMLEscapeString(s[last:]))
	return template.HTML(b.String()) //nolint:gosec // every piece above is escaped
}

type view struct {
	Content
	Base      string
	LogoURL   string
	Brand     string
	Tagline   string
	Teal      string
	TealDeep  string
	Mint      string
	Ink       string
	Muted     string
	Line      string
	NoteLines []string
	Year      int
}

var tmpl = template.Must(template.New("email").Funcs(template.FuncMap{
	"linkify": linkify,
	"safe":    safeURL,
}).Parse(htmlTemplate))

// HTML renders c as the HTML part of a message.
func (c Content) HTML() string {
	base := baseURL()
	v := view{Content: c, Base: base, Brand: brand.Name, Tagline: brand.Tagline, Teal: brand.Teal, TealDeep: brand.TealDeep,
		Mint: brand.Mint, Ink: brand.Ink, Muted: brand.Muted, Line: brand.Line}
	if base != "" {
		v.LogoURL = base + brand.LogoPath
	}
	if c.Neutral {
		v.LogoURL, v.Base, v.Tagline = "", "", ""
		v.Brand = strings.TrimSpace(c.BrandName)
		accent := neutralAccent
		if a := c.Accent; len(a) == 7 && a[0] == '#' && hexRe.MatchString(a[1:]) {
			accent = a
		}
		v.Teal, v.TealDeep, v.Mint = accent, accent, "#f4f5f7"
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, v); err != nil {
		// The template is fixed and the data is strings; a failure is a bug,
		// and the plain-text part is still sent.
		return ""
	}
	return b.String()
}

const htmlTemplate = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark"><meta name="supported-color-schemes" content="light dark">
<title>{{.Title}}</title>
<style>
@media (prefers-color-scheme: dark){
 .bg{background:#0d1514!important}.card{background:#152120!important;border-color:#233431!important}
 .ink{color:#ecf3f2!important}.muted{color:#9db3af!important}.line{border-color:#233431!important}
 .mint{background:#0f2a27!important}.link{color:#5eead4!important}
}
@media (max-width:620px){.wrap{width:100%!important}.pad{padding:24px 20px!important}}
</style></head>
<body class="bg" style="margin:0;padding:0;background:#f3f7f6;-webkit-text-size-adjust:100%">
{{if .Preheader}}<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent;font-size:1px;line-height:1px">{{.Preheader}}</div>{{end}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="bg" style="background:#f3f7f6"><tr><td align="center" style="padding:28px 12px">
<table role="presentation" class="wrap" width="560" cellpadding="0" cellspacing="0" border="0" style="width:560px;max-width:100%">
<tr><td style="padding:0 4px 16px 4px">
 <table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
 {{if .LogoURL}}<td style="padding-right:10px;vertical-align:middle"><img src="{{.LogoURL}}" width="32" height="32" alt="" style="display:block;border:0"></td>{{end}}
 <td class="ink" style="vertical-align:middle;font:600 18px/1 -apple-system,'Segoe UI',Inter,Helvetica,Arial,sans-serif;color:{{.Ink}};letter-spacing:-0.2px">{{.Brand}}</td>
 </tr></table>
</td></tr>
<tr><td class="card pad" style="background:#ffffff;border:1px solid {{.Line}};border-radius:14px;padding:32px 32px 28px 32px;font:400 16px/1.6 -apple-system,'Segoe UI',Inter,Helvetica,Arial,sans-serif;color:{{.Ink}}">
{{if .Title}}<h1 class="ink" style="margin:0 0 18px 0;font:650 22px/1.3 -apple-system,'Segoe UI',Inter,Helvetica,Arial,sans-serif;color:{{.Ink}};letter-spacing:-0.3px">{{.Title}}</h1>{{end}}
{{range .Blocks}}
{{if eq .Kind "p"}}<p class="ink" style="margin:0 0 16px 0;white-space:pre-line;color:{{$.Ink}}">{{linkify .Text}}</p>
{{else if eq .Kind "list"}}<ul class="ink" style="margin:0 0 16px 0;padding-left:20px;color:{{$.Ink}}">{{range .Items}}<li style="margin:0 0 6px 0">{{linkify .}}</li>{{end}}</ul>
{{else if eq .Kind "facts"}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="mint" style="margin:0 0 20px 0;background:{{$.Mint}};border-radius:10px"><tr><td style="padding:6px 16px">
 <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .Facts}}<tr><td class="muted" style="padding:8px 0;font-size:14px;color:{{$.Muted}}">{{.Label}}</td><td class="ink" align="right" style="padding:8px 0;font-size:15px;font-weight:600;color:{{$.Ink}}">{{.Value}}</td></tr>{{end}}</table>
</td></tr></table>
{{else if eq .Kind "code"}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="mint" style="margin:0 0 20px 0;background:{{$.Mint}};border-radius:10px"><tr><td align="center" style="padding:16px 12px;font:600 28px/1.2 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;letter-spacing:6px;color:{{$.TealDeep}}">{{.Text}}</td></tr></table>
{{else if eq .Kind "button"}}{{$blk := .}}{{with safe .URL}}<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:4px 0 20px 0"><tr><td align="center" bgcolor="{{$.Teal}}" style="border-radius:9px;background:{{$.Teal}}"><a href="{{.}}" style="display:inline-block;padding:13px 24px;font:600 16px/1 -apple-system,'Segoe UI',Inter,Helvetica,Arial,sans-serif;color:#ffffff;text-decoration:none;border-radius:9px">{{$blk.Text}}</a></td></tr></table>
<p class="muted" style="margin:-6px 0 18px 0;font-size:13px;line-height:1.5;color:{{$.Muted}}">If the button doesn’t work, copy this address into your browser:<br><a class="link" href="{{.}}" style="color:{{$.Teal}};word-break:break-all">{{.}}</a></p>{{end}}
{{end}}
{{end}}
{{if .SignOff}}<p class="ink" style="margin:8px 0 0 0;white-space:pre-line;color:{{.Ink}}">{{.SignOff}}</p>{{end}}
</td></tr>
<tr><td style="padding:18px 8px 0 8px;text-align:center;font:400 12.5px/1.6 -apple-system,'Segoe UI',Inter,Helvetica,Arial,sans-serif" class="muted">
 {{if .Note}}<div class="muted" style="color:{{.Muted}};margin-bottom:6px">{{.Note}}</div>{{end}}
 <div class="muted" style="color:{{.Muted}}">{{if .Neutral}}{{if .Brand}}Sent by {{.Brand}}{{end}}{{else}}{{.Brand}} · {{.Tagline}}{{if .Base}} · <a class="link" href="{{.Base}}" style="color:{{.Muted}}">{{.Base}}</a>{{end}}{{end}}</div>
</td></tr>
</table></td></tr></table></body></html>
`

// Compose is a message written as Content: Body is its plain-text form, so
// the text part and anything that reads Body see the same words.
func Compose(to []string, subject string, c Content) Message {
	return Message{To: to, Subject: subject, Body: c.Text(), Content: &c}
}

// Hi is a greeting by first name, or "Hello," when there is no name. The
// name is the account's own choice, so it is made Inert.
func Hi(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return "Hi " + Inert(f[0]) + ","
	}
	return "Hello,"
}

// inertBreak follows each character that joins the parts of a web address
// or domain in Inert's output: a zero-width space, which shows as nothing.
const inertBreak = "\u200b"

// Inert makes text someone else chose (an organisation's or a person's
// name) safe to put in a message: a web address or domain in it is not
// made a link by the HTML part, and isn't one a mail client recognises,
// since an invisible zero-width space follows every ".", ":", "/" and "@"
// in it. The words look the same.
func Inert(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(r)
		switch r {
		case '.', ':', '/', '@':
			b.WriteString(inertBreak)
		}
	}
	return b.String()
}

// SignOff closes a message that Xenibase writes in its own voice.
const SignOff = "Warmly,\nThe Xenibase team"

// Notice is a plain-text notice with one link: the text is laid out as
// FromText does, and the link is the button at the end.
func Notice(to []string, subject, body, buttonLabel, link string) Message {
	c := FromText(subject, body)
	if link != "" {
		c.Blocks = append(c.Blocks, Button(buttonLabel, link))
	}
	c.SignOff = SignOff
	return Compose(to, subject, c)
}
