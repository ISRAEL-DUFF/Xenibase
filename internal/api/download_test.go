package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// A download that fails after its first byte ends without the response's
// final chunk, through the server's Recoverer, so the client sees a
// failed download instead of a complete-looking truncated archive; one
// that fails before it is an ordinary error response.
func TestDownloadFailingMidStreamIsAborted(t *testing.T) {
	var interrupted error
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Get("/{when}", func(w http.ResponseWriter, req *http.Request) {
		start := func() { w.WriteHeader(http.StatusOK) }
		err := streamDownload(w, start, func(cw io.Writer) error {
			if chi.URLParam(req, "when") == "mid" {
				_, _ = cw.Write([]byte("PGDMP partial archive"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			return errors.New("chunk 3 failed authentication")
		}, func(err error) { interrupted = err })
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/mid")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err == nil {
		t.Fatalf("a download cut short read as complete: %d, %q", res.StatusCode, body)
	}
	if interrupted == nil {
		t.Fatal("the interruption was not reported")
	}

	res, err = http.Get(srv.URL + "/before")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("an error before the first byte: %d", res.StatusCode)
	}
}
