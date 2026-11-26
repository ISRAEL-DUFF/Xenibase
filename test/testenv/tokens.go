package testenv

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// BearerDo sends a JSON request with an API token and no cookies, as the
// CLI or a CI job would, decoding the response into out (if non-nil). It
// returns the status code and the raw body.
func (e *Env) BearerDo(token, method, path string, body, out any) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.URL+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil && len(b) > 0 && res.StatusCode < 300 {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
	return res.StatusCode, b
}

// CreateToken creates an API token for the signed-in owner through the API
// and returns its secret.
func (e *Env) CreateToken(req map[string]any) string {
	e.t.Helper()
	var out struct {
		Secret string `json:"secret"`
	}
	e.Reauth() // a session creates a token only after a recent step-up
	if code := e.Do("POST", "/api/v1/tokens", req, &out); code != http.StatusCreated {
		e.t.Fatalf("create token %v: status %d", req, code)
	}
	return out.Secret
}
