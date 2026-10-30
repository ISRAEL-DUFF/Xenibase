package support

import (
	"strings"
)

// AuthHeaders are the verdicts the receiving mail server (the email
// provider) put on an inbound email, top header first.
type AuthHeaders struct {
	// AuthenticationResults are Authentication-Results values (RFC 8601).
	AuthenticationResults []string
	// ReceivedSPF are Received-SPF values (RFC 7208 §9.1).
	ReceivedSPF []string
	// SpamTests are X-Spam-Tests values: SpamAssassin's rule names, which
	// Postmark adds (DKIM_VALID_AU, SPF_PASS, DMARC_PASS).
	SpamTests []string
}

// SenderAuthenticated reports whether the provider's verdicts show that
// from (an address) is genuine: DMARC passed for its domain, or both DKIM
// and SPF passed for domains aligned with it.
//
// A sender can put any header in its message, so only verdicts the provider
// added count. With authservID set (XENIBASE_SUPPORT_AUTHSERV_ID), only
// Authentication-Results naming it count; without, every one in h does, so
// the caller passes only values the provider vouches for (the inbound API's
// own field), never headers copied from the message. Only the topmost
// Received-SPF counts, which the receiving server prepends; SpamAssassin
// removes X-Spam-* headers already in a message, so the topmost
// X-Spam-Tests is the provider's.
func SenderAuthenticated(from string, h AuthHeaders, authservID string) bool {
	at := strings.LastIndex(from, "@")
	if at < 0 || at == len(from)-1 {
		return false
	}
	domain := strings.ToLower(strings.TrimSuffix(from[at+1:], "."))
	var dkim, spf bool
	for _, v := range h.AuthenticationResults {
		id, results := parseAuthResults(v)
		if authservID != "" && !strings.EqualFold(id, authservID) {
			continue
		}
		for _, r := range results {
			if r.result != "pass" {
				continue
			}
			switch r.method {
			case "dmarc":
				if d := r.props["header.from"]; d == "" || strings.EqualFold(d, domain) {
					return true
				}
			case "dkim":
				d := r.props["header.d"]
				if d == "" {
					d = domainOf(r.props["header.i"])
				}
				dkim = dkim || (d != "" && aligned(d, domain))
			case "spf":
				d := domainOf(r.props["smtp.mailfrom"])
				spf = spf || (d != "" && aligned(d, domain))
			}
		}
	}
	if len(h.ReceivedSPF) > 0 {
		result, envelope := parseReceivedSPF(h.ReceivedSPF[0])
		if d := domainOf(envelope); result == "pass" && d != "" && aligned(d, domain) {
			spf = true
		}
	}
	if len(h.SpamTests) > 0 {
		for _, t := range strings.FieldsFunc(h.SpamTests[0], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
			switch strings.ToUpper(strings.TrimSpace(t)) {
			case "DMARC_PASS":
				return true
			case "DKIM_VALID_AU": // valid, and from the author's domain
				dkim = true
			}
		}
	}
	return dkim && spf
}

type authResult struct {
	method, result string
	props          map[string]string
}

// parseAuthResults splits an Authentication-Results value into its
// authserv-id and its method results, lower-cased, comments dropped.
func parseAuthResults(v string) (string, []authResult) {
	v = stripComments(v)
	parts := strings.Split(v, ";")
	id := ""
	if f := strings.Fields(parts[0]); len(f) > 0 {
		id = f[0]
	}
	var out []authResult
	for _, p := range parts[1:] {
		f := strings.Fields(p)
		if len(f) == 0 {
			continue
		}
		method, result, ok := strings.Cut(strings.ToLower(f[0]), "=")
		if !ok {
			continue
		}
		if i := strings.IndexByte(method, '/'); i >= 0 {
			method = method[:i] // a method version: dkim/1
		}
		r := authResult{method: method, result: result, props: map[string]string{}}
		for _, kv := range f[1:] {
			k, val, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			k = strings.ToLower(k)
			if _, seen := r.props[k]; !seen {
				r.props[k] = strings.ToLower(strings.Trim(val, `"`))
			}
		}
		out = append(out, r)
	}
	return id, out
}

// parseReceivedSPF returns a Received-SPF value's result and its
// envelope-from (or smtp.mailfrom) address.
func parseReceivedSPF(v string) (string, string) {
	v = stripComments(v)
	f := strings.Fields(v)
	if len(f) == 0 {
		return "", ""
	}
	result := strings.ToLower(f[0])
	envelope := ""
	for _, kv := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' }) {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if k = strings.ToLower(k); envelope == "" && (k == "envelope-from" || k == "smtp.mailfrom") {
			envelope = strings.Trim(val, `"<>`)
		}
	}
	return result, envelope
}

// stripComments drops (comments), which may nest.
func stripComments(v string) string {
	var b strings.Builder
	depth := 0
	for _, r := range v {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// domainOf is an address's domain ("user@d" or "@d"), or a bare domain.
func domainOf(s string) string {
	s = strings.ToLower(strings.Trim(strings.TrimSpace(s), `"<>`))
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(s, ".")
}

// aligned is DMARC's relaxed alignment, approximated without a public
// suffix list: the domains are equal, or one is a subdomain of the other.
// Two sibling subdomains don't align here, which only errs on the side of
// not trusting a sender.
func aligned(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if !strings.Contains(a, ".") || !strings.Contains(b, ".") {
		return false
	}
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}
