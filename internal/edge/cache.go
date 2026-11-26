package edge

import (
	"container/list"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/israel-duff/xenibase/internal/datacat"
)

// Edge caching of anonymous reads (V4.1 §10): a project lists tables and
// functions with a TTL (settings.cache_ttl_seconds); publishable-key GETs
// without a user's token are then served from an in-process LRU for up to
// that long. A TTL is a staleness budget: writes through this edge's data
// API drop the entries they could change at once, realtime's change feed
// drops them on every edge listening to the project, and everything else
// (SQL, jobs, another edge's writes) shows within the TTL.

// DefaultCacheBytes bounds the response cache of one edge process.
const DefaultCacheBytes = 64 << 20

// rpcTag tags every function result: a write may change what any function
// reads, so writes drop them all.
const rpcTag = "rpc:"

type cacheEntry struct {
	key     string
	ref     string
	body    []byte
	stored  time.Time
	expires time.Time
	ttl     int
	tags    []string // the relations it read ("schema.table"), or rpcTag
}

// respCache is the response cache: an LRU bounded by bytes, with each
// project's entries indexed by the relations they read.
type respCache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List               // front is the most recently used
	byKey map[string]*list.Element // of *cacheEntry
	// byTag is ref -> tag -> keys, for dropping on a write.
	byTag map[string]map[string]map[string]bool
	// gen counts each project's drops, so a read that began before a
	// write doesn't store what it read after the write dropped the entry.
	gen map[string]uint64
}

func newRespCache(maxBytes int) *respCache {
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	return &respCache{max: maxBytes, order: list.New(), byKey: map[string]*list.Element{},
		byTag: map[string]map[string]map[string]bool{}, gen: map[string]uint64{}}
}

func (c *respCache) generation(ref string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen[ref]
}

// get is a live entry, or nil.
func (c *respCache) get(key string, now time.Time) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[key]
	if !ok {
		return nil
	}
	ent := el.Value.(*cacheEntry)
	if !now.Before(ent.expires) {
		c.remove(el)
		return nil
	}
	c.order.MoveToFront(el)
	return ent
}

// put stores an entry unless the project's entries were dropped since gen.
func (c *respCache) put(ent *cacheEntry, gen uint64) {
	cost := len(ent.body) + len(ent.key)
	if cost > c.max/8 {
		return // one response may not take an eighth of the cache
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen[ent.ref] != gen {
		return
	}
	if el, ok := c.byKey[ent.key]; ok {
		c.remove(el)
	}
	el := c.order.PushFront(ent)
	c.byKey[ent.key] = el
	c.size += cost
	tags := c.byTag[ent.ref]
	if tags == nil {
		tags = map[string]map[string]bool{}
		c.byTag[ent.ref] = tags
	}
	for _, t := range ent.tags {
		if tags[t] == nil {
			tags[t] = map[string]bool{}
		}
		tags[t][ent.key] = true
	}
	for c.size > c.max {
		c.remove(c.order.Back())
	}
}

func (c *respCache) remove(el *list.Element) {
	ent := el.Value.(*cacheEntry)
	c.order.Remove(el)
	delete(c.byKey, ent.key)
	c.size -= len(ent.body) + len(ent.key)
	tags := c.byTag[ent.ref]
	for _, t := range ent.tags {
		delete(tags[t], ent.key)
		if len(tags[t]) == 0 {
			delete(tags, t)
		}
	}
	if len(tags) == 0 {
		delete(c.byTag, ent.ref)
	}
}

// dropTables drops ref's entries that read any of tables, and its
// function results. No tables drops all of ref's entries (a function
// called with POST may have written anything).
func (c *respCache) dropTables(ref string, tables []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen[ref]++
	tags := c.byTag[ref]
	if tags == nil {
		return
	}
	var keys []string
	if len(tables) == 0 {
		for _, ks := range tags {
			for k := range ks {
				keys = append(keys, k)
			}
		}
	} else {
		for _, t := range append(tables, rpcTag) {
			for k := range tags[t] {
				keys = append(keys, k)
			}
		}
	}
	for _, k := range keys {
		if el, ok := c.byKey[k]; ok {
			c.remove(el)
		}
	}
}

// cacheKey is a request's key: the project and its configuration's
// version, the catalog's fingerprint, the path, and the query with its
// parameters sorted (the apikey left out; repeated parameters keep their
// order, which order= depends on).
func cacheKey(ref string, version int64, fingerprint, path string, q url.Values) string {
	q2 := url.Values{}
	for k, v := range q {
		if k != "apikey" {
			q2[k] = v
		}
	}
	return ref + "\x00" + strconv.FormatInt(version, 10) + "\x00" + fingerprint + "\x00" + path + "?" + q2.Encode()
}

// cacheable reports whether a request may be served from the cache, and
// for how long, given the relation's cache name ("schema.table", or
// "rpc.schema.function"): a GET with the publishable key, no user's token
// and no Read-Replica: primary, on a listed relation.
func (c *call) cacheable(req Request, rel string) int {
	ttl := c.p.cfg.Settings.CacheTTLSeconds[rel]
	if ttl <= 0 || c.r.Method != http.MethodGet || !c.publishable || req.Role != "anon" ||
		strings.EqualFold(c.r.Header.Get("Read-Replica"), "primary") {
		return 0
	}
	return min(ttl, 3600)
}

// cacheName is a function's name in cache_ttl_seconds: rpc.<name> in the
// public schema, rpc.<schema>.<name> elsewhere; tables are always
// schema.table.
func cacheName(schema, name string, function bool) string {
	n := schema + "." + name
	if function {
		if schema == "public" {
			n = name
		}
		return "rpc." + n
	}
	return n
}

// fingerprint is the cached catalog's fingerprint while it was checked
// less than every ago, else "" (no lookup then): a hit never opens a
// transaction, so without this a popular URL would never see a dropped
// policy. The request then goes to the database, whose catalog check drops
// the project's entries if anything changed.
func (st *catalogState) fingerprint(every time.Duration) string {
	if every <= 0 {
		every = fingerprintEvery
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cat == nil || time.Since(st.checked) >= every {
		return ""
	}
	return st.cat.Fingerprint
}

// serveCached answers a request from the cache; false is a miss.
func (e *Edge) serveCached(c *call, key string) bool {
	now := time.Now()
	ent := e.cache.get(key, now)
	if ent == nil {
		return false
	}
	age := int(now.Sub(ent.stored).Seconds())
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(max(ent.ttl-age, 0)))
	h.Add("Vary", "Authorization, Read-Replica")
	h.Set("X-Cache", "HIT")
	h.Set("Age", strconv.Itoa(age))
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(ent.body)
	return true
}

// cachedHeaders marks a response that was stored: a CDN in front may
// cache it too, but never for a request with a user's token.
func cachedHeaders(h http.Header, ttl int) {
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(ttl))
	h.Add("Vary", "Authorization, Read-Replica")
	h.Set("X-Cache", "MISS")
	h.Set("Age", "0")
}

// cacheStore caches a fresh response and marks it a miss.
func (e *Edge) cacheStore(c *call, key string, body []byte, ttl int, tags []string, gen uint64) {
	now := time.Now()
	e.cache.put(&cacheEntry{key: key, ref: c.p.cfg.Ref, body: append([]byte(nil), body...), stored: now,
		expires: now.Add(time.Duration(ttl) * time.Second), ttl: ttl, tags: tags}, gen)
	cachedHeaders(c.w.Header(), ttl)
}

// dropWritten drops the entries a committed write could have changed.
func (e *Edge) dropWritten(c *call, tables []string, all bool) {
	if len(c.p.cfg.Settings.CacheTTLSeconds) == 0 || (!all && len(tables) == 0) {
		return
	}
	if all {
		tables = nil
	}
	e.cache.dropTables(c.p.cfg.Ref, tables)
}

// function is a function of the cached catalog by its URL name (the first
// overload: a cache name covers them all).
func (st *catalogState) function(name string) *datacat.Function {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cat == nil {
		return nil
	}
	if fs := st.cat.FindFunction(name); len(fs) > 0 {
		return fs[0]
	}
	return nil
}
