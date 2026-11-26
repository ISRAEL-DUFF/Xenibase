package edge

import (
	"testing"

	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// TestRedirectPatternsMatchHostAndPathApart: a wildcard redirect URL is
// matched against the parsed URL's parts, so a "?" or "#" can't move the
// pattern's host into the query or fragment, "*" in a host is one label,
// and a URL with a user part is refused.
func TestRedirectPatternsMatchHostAndPathApart(t *testing.T) {
	a := edgeapi.AuthConfig{SiteURL: "https://site.example.io/app", AllowWildcardRedirects: true,
		RedirectURLs: []string{"https://*.example.com/**", "https://preview-*.example.net/callback", "http://localhost:*/auth/*",
			"myapp://login", "https://exact.example.org/x?y=1"}}
	for u, want := range map[string]bool{
		"https://site.example.io/app":                      true,
		"https://site.example.io/app/next":                 true,
		"https://site.example.io/application":              false,
		"https://a.example.com/":                           true,
		"https://a.example.com":                            true,
		"https://a.example.com/x/y?z=1#frag":               true,
		"https://A.Example.COM/x":                          true,
		"https://evil.test?.example.com/":                  false,
		"https://evil.test#.example.com/x":                 false,
		"https://evil.test/.example.com/x":                 false,
		"https://a.b.example.com/":                         false,
		"https://.example.com/":                            false,
		"https://example.com/":                             false,
		"http://a.example.com/":                            false,
		"https://a.example.com:8443/":                      false,
		"https://user@a.example.com/":                      false,
		"https://a.example.com@evil.test/":                 false,
		"https://preview-42.example.net/callback":          true,
		"https://preview-42.example.net/callback?x=1":      false,
		"https://preview-42.example.net/callback/x":        false,
		"https://preview-4.2.example.net/callback":         false,
		"https://evil.test#preview-1.example.net/callback": false,
		"http://localhost:3000/auth/callback":              true,
		"http://localhost/auth/callback":                   true,
		"http://localhost:3000/auth/a/b":                   false,
		"http://evil.test?localhost:3000/auth/x":           false,
		"myapp://login":                                    true,
		"https://exact.example.org/x?y=1":                  true,
		"https://exact.example.org/x?y=2":                  false,
	} {
		if got := redirectAllowed(a, u); got != want {
			t.Errorf("%s: %v, want %v", u, got, want)
		}
	}
	a.AllowWildcardRedirects = false
	if redirectAllowed(a, "https://a.example.com/") {
		t.Error("a pattern matched with wildcards off")
	}
}
