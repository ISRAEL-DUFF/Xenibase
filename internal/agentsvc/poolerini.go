package agentsvc

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// ServerTLSModes are the server_tls_sslmode values a pooler host may run
// with. disable and allow are left out: they would send a backend login in
// plain text whenever the backend offers TLS and something in the path
// strips it.
var ServerTLSModes = []string{"prefer", "require", "verify-ca", "verify-full"}

var serverTLSLine = regexp.MustCompile(`(?m)^[ \t]*server_tls_(sslmode|ca_file)[ \t]*=.*\n?`)

// PoolerINI returns a pgbouncer.ini with how PgBouncer connects to the
// backends (instances, and the waker) set: server_tls_sslmode = mode and,
// when caFile is set, server_tls_ca_file. verify-ca and verify-full need
// caFile, the CA that signed the backends' certificates. A pooler host
// renders its two files with it at start
// (XENIBASE_POOLER_SERVER_TLS_SSLMODE, _CA_FILE).
func PoolerINI(in []byte, mode, caFile string) ([]byte, error) {
	if mode == "" {
		mode = "prefer"
	}
	valid := false
	for _, m := range ServerTLSModes {
		valid = valid || m == mode
	}
	if !valid {
		return nil, fmt.Errorf("server TLS mode %q: use one of %s", mode, strings.Join(ServerTLSModes, ", "))
	}
	if strings.HasPrefix(mode, "verify-") && caFile == "" {
		return nil, fmt.Errorf("server TLS mode %s needs the backends' CA file", mode)
	}
	if strings.ContainsAny(caFile, "\n\r;#") {
		return nil, fmt.Errorf("server TLS CA file %q: not a plain path", caFile)
	}
	start := bytes.Index(in, []byte("[pgbouncer]"))
	if start < 0 {
		return nil, fmt.Errorf("no [pgbouncer] section")
	}
	out := serverTLSLine.ReplaceAll(in, nil)
	start = bytes.Index(out, []byte("[pgbouncer]"))
	eol := bytes.IndexByte(out[start:], '\n')
	if eol < 0 {
		return nil, fmt.Errorf("an empty [pgbouncer] section")
	}
	at := start + eol + 1
	set := "server_tls_sslmode = " + mode + "\n"
	if caFile != "" {
		set += "server_tls_ca_file = " + caFile + "\n"
	}
	return append(append(append([]byte(nil), out[:at]...), set...), out[at:]...), nil
}
