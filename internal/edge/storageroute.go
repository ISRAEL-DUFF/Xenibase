package edge

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/israel-duff/xenibase/internal/files"
)

// Storage routes (V4 §5.2). Paths follow the spec, with the Supabase
// clients' spellings accepted where they differ.

// storageRateOK applies the project's per-IP limit to a keyless request,
// tightened like keyed requests' under a spend cap (V4 §12).
func (e *Edge) storageRateOK(c *call) bool {
	rate, bucket := c.p.cfg.Settings.RatePerIP, ""
	if c.p.cfg.SpendCapped {
		rate, bucket = spendCapRate(rate), "capped:"
	}
	if !e.limits.allow(bucket+"ip:"+c.p.cfg.Ref+":"+c.ip, rate) {
		c.w.Header().Set("Retry-After", "1")
		if c.p.cfg.SpendCapped {
			c.fail(http.StatusTooManyRequests, "spend_cap_rate_limited", "the organisation has reached its spend cap, so this project's request rate is reduced; slow down or raise the cap")
		} else {
			c.fail(http.StatusTooManyRequests, "rate_limited", "too many requests; slow down")
		}
		return false
	}
	return true
}

func hasKey(c *call) bool {
	return c.r.Header.Get("apikey") != "" || c.r.URL.Query().Get("apikey") != ""
}

// keylessStorage serves what needs no key: public buckets, signed URLs, and
// renders of either. True means c was answered.
func (e *Edge) keylessStorage(c *call) bool {
	rest, ok := strings.CutPrefix(c.r.URL.Path, "/storage/v1/")
	if !ok {
		return false
	}
	get := c.r.Method == http.MethodGet || c.r.Method == http.MethodHead
	put := c.r.Method == http.MethodPut || c.r.Method == http.MethodPost
	token := c.r.URL.Query().Get("token") != ""
	var tail string
	var kind string
	switch {
	case get && cut(rest, &tail, "public/", "object/public/"):
		kind = "public"
	case get && token && cut(rest, &tail, "object/sign/"):
		kind = "signed"
	case put && token && cut(rest, &tail, "upload/sign/", "object/upload/sign/"):
		kind = "upload"
	case get && token && cut(rest, &tail, "render/sign/", "render/image/sign/"):
		kind = "render-signed"
	case get && cut(rest, &tail, "render/public/", "render/image/public/"):
		kind = "render-public"
	case get && !hasKey(c) && cut(rest, &tail, "render/"):
		kind = "render-public"
	default:
		return false
	}
	if !e.storageRateOK(c) {
		return true
	}
	c.billed = true
	bucket, path, ok := splitBucketPath(tail)
	if !ok {
		c.fail(http.StatusBadRequest, "invalid_path", "the URL must name a bucket and an object path")
		return true
	}
	switch kind {
	case "public":
		e.publicDownload(c, bucket, path)
	case "signed":
		e.signedDownload(c, bucket, path)
	case "upload":
		e.signedUpload(c, bucket, path)
	case "render-signed":
		e.render(c, nil, bucket, path, renderSigned)
	case "render-public":
		e.render(c, nil, bucket, path, renderPublic)
	}
	return true
}

// cut sets *tail to s after the first of prefixes it starts with.
func cut(s string, tail *string, prefixes ...string) bool {
	for _, p := range prefixes {
		if t, ok := strings.CutPrefix(s, p); ok {
			*tail = t
			return true
		}
	}
	return false
}

// storageRoute serves /storage/v1/... after the key check.
func (e *Edge) storageRoute(c *call, req Request) {
	if mfaRequired(c.p.cfg.Auth, req) {
		c.fail(http.StatusForbidden, "mfa_required", "this project requires a second factor: verify one to reach aal2")
		return
	}
	rest := strings.TrimPrefix(c.r.URL.Path, "/storage/v1/")
	m := c.r.Method
	get := m == http.MethodGet || m == http.MethodHead
	var tail string
	bp := func() (string, string, bool) {
		b, p, ok := splitBucketPath(tail)
		if !ok {
			c.fail(http.StatusBadRequest, "invalid_path", "the URL must name a bucket and an object path")
		}
		return b, p, ok
	}
	switch {
	case rest == "bucket" || strings.HasPrefix(rest, "bucket/"):
		e.buckets(c, req, strings.TrimPrefix(rest, "bucket"))
	case rest == "object/move" && m == http.MethodPost:
		e.moveOrCopy(c, req, false)
	case rest == "object/copy" && m == http.MethodPost:
		e.moveOrCopy(c, req, true)
	case get && cut(rest, &tail, "object/info/authenticated/", "object/info/public/", "object/info/"):
		if b, p, ok := bp(); ok {
			e.info(c, req, b, p)
		}
	case m == http.MethodPost && cut(rest, &tail, "sign-upload/", "object/upload/sign/"):
		if b, p, ok := bp(); ok {
			e.signUpload(c, req, b, p)
		}
	case m == http.MethodPost && cut(rest, &tail, "object/sign/", "sign/"):
		if !strings.Contains(tail, "/") && files.BucketRe.MatchString(tail) {
			e.signDownload(c, req, tail, "")
			return
		}
		if b, p, ok := bp(); ok {
			e.signDownload(c, req, b, p)
		}
	case get && cut(rest, &tail, "list/"):
		if !files.BucketRe.MatchString(tail) {
			c.fail(http.StatusBadRequest, "invalid_bucket", "the URL must name a bucket")
			return
		}
		q := c.r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		e.list(c, req, tail, listInput{Prefix: q.Get("prefix"), Cursor: q.Get("cursor"), Limit: limit, Recursive: q.Get("recursive") == "true"})
	case m == http.MethodPost && cut(rest, &tail, "object/list/"):
		if !files.BucketRe.MatchString(tail) {
			c.fail(http.StatusBadRequest, "invalid_bucket", "the URL must name a bucket")
			return
		}
		var in listInput
		if !authBody(c, &in) {
			return
		}
		e.list(c, req, tail, in)
	case cut(rest, &tail, "upload/"):
		e.multipart(c, req, tail)
	case get && cut(rest, &tail, "render/authenticated/", "render/image/authenticated/", "render/"):
		if b, p, ok := bp(); ok {
			e.render(c, &req, b, p, renderAuthenticated)
		}
	case cut(rest, &tail, "object/"):
		if get {
			tail = strings.TrimPrefix(tail, "authenticated/")
		}
		if !strings.Contains(tail, "/") && m == http.MethodDelete {
			if !files.BucketRe.MatchString(tail) {
				c.fail(http.StatusBadRequest, "invalid_bucket", "the URL must name a bucket")
				return
			}
			var in struct {
				Paths    []string `json:"paths"`
				Prefixes []string `json:"prefixes"`
			}
			if !authBody(c, &in) {
				return
			}
			e.remove(c, req, tail, append(in.Paths, in.Prefixes...), false)
			return
		}
		b, p, ok := bp()
		if !ok {
			return
		}
		switch m {
		case http.MethodGet, http.MethodHead:
			e.download(c, req, b, p)
		case http.MethodPost:
			e.putObject(c, req, b, p, c.r.Header.Get("X-Upsert") == "true", c.userID, nil)
		case http.MethodPut:
			e.putObject(c, req, b, p, true, c.userID, nil)
		case http.MethodDelete:
			e.remove(c, req, b, []string{p}, true)
		default:
			c.fail(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	default:
		c.fail(http.StatusNotFound, "no_such_endpoint", "no such endpoint")
	}
}
