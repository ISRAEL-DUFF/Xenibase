package regions

import (
	"context"
	"fmt"

	"github.com/israel-duff/xenibase/internal/faildomain"
	"github.com/israel-duff/xenibase/internal/store"
)

// Check is one launch check of a region (V4.1 §13, docs/lagos-launch.md).
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Blocking checks keep a hidden region hidden; the others are warnings.
	Blocking bool   `json:"blocking"`
	Detail   string `json:"detail"`
}

// Readiness runs the launch checks of region id: what docs/lagos-launch.md
// asks before a region is opened, as far as Xenibase can see it.
func (s *Service) Readiness(ctx context.Context, id string) ([]Check, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	q := store.New(s.db)
	nodes, err := q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := q.PoolerHosts(ctx)
	if err != nil {
		return nil, err
	}
	members, err := q.ListEtcdMembers(ctx)
	if err != nil {
		return nil, err
	}
	problems, err := faildomain.Check(ctx, q)
	if err != nil {
		return nil, err
	}
	var out []Check
	add := func(name string, ok, blocking bool, detail string, a ...any) {
		out = append(out, Check{Name: name, OK: ok, Blocking: blocking && !ok, Detail: fmt.Sprintf(detail, a...)})
	}

	// Nodes that can take projects.
	up, dedicated := 0, 0
	for _, n := range nodes {
		if n.Region != id || n.Role == "pooler" || n.Status != "healthy" || n.Lifecycle != "active" {
			continue
		}
		up++
		if n.Role == "dedicated" || n.Role == "both" {
			dedicated++
		}
	}
	add("nodes", up > 0, true, "%d healthy node(s) in the region, %d of them for dedicated instances", up, dedicated)

	// The pooler pair.
	var pool []store.Node
	for _, h := range hosts {
		if h.Region == id {
			pool = append(pool, h)
		}
	}
	switch {
	case len(pool) >= 2:
		ok, _ := faildomain.AllSeparated(pool)
		add("pooler pair", ok, false, "%d pooler hosts (%s)", len(pool), faildomain.Describe(pool))
	case id == s.home:
		add("pooler pair", true, false, "the home region's poolers serve it")
	default:
		add("pooler pair", false, false, "%d pooler host(s): connections go through the home region's poolers until the region has two of its own", len(pool))
	}
	if len(pool) > 0 {
		add("floating IP", r.FloatingIpID != nil && *r.FloatingIpID != "", false, "the address clients connect to moves between the pooler hosts")
	}
	add("hostname", r.PoolerHost != "", false, "projects in the region connect to %q", s.Host(id))

	// Backups.
	add("backup target", r.StorageTargetID != nil, r.Residency, "a platform storage target for the region's backups (needed for residency)")
	if !r.Residency {
		add("copy target", r.CopyTargetID != nil, false, "backups copied to a second target (V3 §2.5)")
	}
	add("files target", r.FilesTargetID != nil, false, "backend services' files kept apart from the backups, whose credentials the edges would otherwise hold (V4 §5.1)")

	// HA: the region's etcd cluster.
	var etcd []store.Node
	byID := map[string]store.Node{}
	for _, n := range nodes {
		byID[n.ID.String()] = n
	}
	for _, m := range members {
		if m.Region == id {
			if n, ok := byID[m.NodeID.String()]; ok {
				etcd = append(etcd, n)
			}
		}
	}
	switch len(etcd) {
	case 0:
		add("etcd", true, false, "no etcd cluster: HA can't be enabled in the region (set one up on three nodes in three failure domains to offer HA)")
	case 3:
		ok, _ := faildomain.AllSeparated(etcd)
		add("etcd", ok, true, "three members (%s)", faildomain.Describe(etcd))
	default:
		add("etcd", false, true, "%d etcd member(s): a cluster has three, in three failure domains", len(etcd))
	}

	// Anything in the region breaking the failure-domain rule.
	n := 0
	for _, p := range problems {
		if p.Region == id && p.Group != faildomain.GroupPoolerPair {
			n++
		}
	}
	add("failure domains", n == 0, true, "%d HA pair(s) or etcd cluster(s) in the region share a failure domain (Platform → Nodes)", n)
	return out, nil
}

// Blocking lists the blocking checks that failed.
func Blocking(cs []Check) []Check {
	var out []Check
	for _, c := range cs {
		if c.Blocking {
			out = append(out, c)
		}
	}
	return out
}
