package api

import "testing"

// TestParseInboundEmailAuthentication: the sender is authenticated only on
// the provider's verdicts, in Postmark's headers or this API's field.
func TestParseInboundEmailAuthentication(t *testing.T) {
	const header = `{"From":"owner@shop.example","Subject":"hi","TextBody":"x","Headers":[
			{"Name":"Authentication-Results","Value":"mx.provider.example; dmarc=pass header.from=shop.example"}]}`
	for _, c := range []struct {
		name, body, authserv string
		want                 bool
	}{
		// A message's own Authentication-Results counts only when it names
		// the configured provider.
		{"header, no authserv-id", header, "", false},
		{"header, the provider's", header, "mx.provider.example", true},
		{"header, another server's", header, "mx.other.example", false},
		{"plain, no verdict", `{"from":"owner@shop.example","subject":"hi","text":"x"}`, "", false},
		{"plain, dmarc pass", `{"from":"owner@shop.example","subject":"hi","text":"x","authentication_results":"mx.relay.example; dmarc=pass header.from=shop.example"}`, "", true},
		{"plain, several", `{"from":"owner@shop.example","authentication_results":["mx.relay.example; dkim=pass header.d=shop.example; spf=pass smtp.mailfrom=shop.example"]}`, "", true},
		{"postmark, forged From", `{"FromFull":{"Email":"owner@shop.example","Name":"Owner"},"Subject":"hi","TextBody":"x","Headers":[
			{"Name":"Received-SPF","Value":"Fail (sender not authorized) identity=mailfrom; envelope-from=owner@shop.example"},
			{"Name":"X-Spam-Tests","Value":"SPF_FAIL"},
			{"Name":"Authentication-Results","Value":"mx.evil.example; dmarc=pass header.from=shop.example"}]}`, "", false},
		{"postmark, genuine", `{"FromFull":{"Email":"owner@shop.example","Name":"Owner"},"Subject":"hi","TextBody":"x","Headers":[
			{"Name":"Received-SPF","Value":"Pass (sender SPF authorized) identity=mailfrom; envelope-from=owner@shop.example"},
			{"Name":"X-Spam-Tests","Value":"DKIM_SIGNED,DKIM_VALID,DKIM_VALID_AU,SPF_PASS"}]}`, "", true},
	} {
		e, err := parseInboundEmail([]byte(c.body), c.authserv)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if e.Authenticated != c.want {
			t.Errorf("%s: authenticated %v, want %v", c.name, e.Authenticated, c.want)
		}
	}
}
