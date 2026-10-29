package mail

import (
	"strings"
	"testing"
)

// TestInertTextIsNeverALink: text marked Inert, and a greeting's name,
// keep their words but never become a link, while the message's own
// addresses still do.
func TestInertTextIsNeverALink(t *testing.T) {
	c := Content{Blocks: []Block{
		P(Hi("https://evil.test/a Smith")),
		P("Acme (" + Inert("Security team https://evil.test/reset") + ") invited you. See https://app.example.com/docs."),
	}}
	html, text := c.HTML(), c.Text()
	for _, s := range []string{html, text} {
		if strings.Contains(s, "https://evil.test") {
			t.Fatalf("an inert address can be followed: %s", s)
		}
	}
	if strings.Contains(html, `href="https://evil`) {
		t.Fatal("an inert address was made a link")
	}
	if !strings.Contains(html, `href="https://app.example.com/docs"`) {
		t.Fatal("the message's own address is no longer a link")
	}
	if got := strings.ReplaceAll(Inert("a.b:c/d@e"), "\u200b", ""); got != "a.b:c/d@e" {
		t.Fatalf("Inert changed the words: %q", got)
	}
}
