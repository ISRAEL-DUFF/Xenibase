package support

import "testing"

func TestSenderAuthenticated(t *testing.T) {
	const from = "owner@shop.example"
	ar := func(v ...string) AuthHeaders { return AuthHeaders{AuthenticationResults: v} }
	for _, c := range []struct {
		name     string
		h        AuthHeaders
		authserv string
		want     bool
	}{
		{"nothing", AuthHeaders{}, "", false},
		{"dmarc pass", ar("mx.provider.example; dmarc=pass (p=reject) header.from=shop.example"), "", true},
		{"dmarc pass, no header.from", ar("mx.provider.example; dmarc=pass"), "", true},
		{"dmarc pass for another domain", ar("mx.provider.example; dmarc=pass header.from=evil.example"), "", false},
		{"dmarc fail", ar("mx.provider.example; spf=pass smtp.mailfrom=shop.example; dmarc=fail header.from=shop.example"), "", false},
		{"dkim and spf aligned", ar("mx.provider.example; dkim=pass header.d=shop.example; spf=pass smtp.mailfrom=bounce@mail.shop.example"), "", true},
		{"dkim only", ar("mx.provider.example; dkim=pass header.d=shop.example; spf=fail smtp.mailfrom=shop.example"), "", false},
		{"dkim for another domain", ar("mx.provider.example; dkim=pass header.d=evil.example; spf=pass smtp.mailfrom=shop.example"), "", false},
		{"dkim by header.i", ar("mx.provider.example; dkim/1=pass header.i=@shop.example; spf=pass smtp.mailfrom=a@shop.example"), "", true},
		{"authserv-id picks the provider's", ar("mx.evil.example; dmarc=pass", "mx.provider.example; dmarc=pass header.from=shop.example"), "mx.provider.example", true},
		{"authserv-id ignores others", ar("mx.evil.example; dmarc=pass header.from=shop.example"), "mx.provider.example", false},
		{"a comment hiding a result", ar("mx.provider.example; dmarc=fail (dmarc=pass) header.from=shop.example"), "", false},
		// The sender's own Received-SPF below the provider's.
		{"forged lower Received-SPF", AuthHeaders{
			ReceivedSPF: []string{"Fail identity=mailfrom; envelope-from=owner@shop.example", "Pass identity=mailfrom; envelope-from=owner@shop.example"},
			SpamTests:   []string{"DKIM_VALID_AU"},
		}, "", false},
		{"postmark", AuthHeaders{
			ReceivedSPF: []string{"Pass (sender SPF authorized) identity=mailfrom; client-ip=192.0.2.1; helo=mail.shop.example; envelope-from=owner@shop.example; receiver=inbound@postmarkapp.com"},
			SpamTests:   []string{"DKIM_SIGNED,DKIM_VALID,DKIM_VALID_AU,SPF_PASS"},
		}, "", true},
		{"postmark, dkim not the author's", AuthHeaders{
			ReceivedSPF: []string{"Pass (sender SPF authorized) identity=mailfrom; envelope-from=owner@shop.example"},
			SpamTests:   []string{"DKIM_SIGNED,DKIM_VALID,SPF_PASS"},
		}, "", false},
		{"postmark, spf for another domain", AuthHeaders{
			ReceivedSPF: []string{"Pass (sender SPF authorized) identity=mailfrom; envelope-from=x@evil.example"},
			SpamTests:   []string{"DKIM_VALID_AU"},
		}, "", false},
		{"postmark, spf softfail", AuthHeaders{
			ReceivedSPF: []string{"SoftFail (domain transitioning) identity=mailfrom; envelope-from=owner@shop.example"},
			SpamTests:   []string{"DKIM_VALID_AU"},
		}, "", false},
		{"spamassassin dmarc", AuthHeaders{SpamTests: []string{"DMARC_PASS, SPF_PASS"}}, "", true},
	} {
		if got := SenderAuthenticated(from, c.h, c.authserv); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if SenderAuthenticated("no-at-sign", ar("mx; dmarc=pass"), "") {
		t.Error("an address without a domain")
	}
}
