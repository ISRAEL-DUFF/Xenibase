package pooler

import "testing"

func TestDecide(t *testing.T) {
	up := func(name, sid, vrrp string) HostView {
		return HostView{Name: name, ServerID: sid, Reachable: true, Ready: true, VRRP: vrrp}
	}
	down := func(name, sid string) HostView { return HostView{Name: name, ServerID: sid} }
	stale := func(name, sid, vrrp string) HostView {
		return HostView{Name: name, ServerID: sid, Reachable: true, Stale: true, VRRP: vrrp}
	}
	cases := []struct {
		name     string
		hosts    []HostView
		holder   string
		manage   bool
		assignTo string // "" = none
		split    bool
		none     bool
	}{
		{"healthy holder stays", []HostView{up("a", "1", "MASTER"), up("b", "2", "BACKUP")}, "1", true, "", false, false},
		// Agents no longer assign the IP: keepalived's MASTER gets it.
		{"keepalived's MASTER takes it from a healthy holder", []HostView{up("a", "1", "BACKUP"), up("b", "2", "MASTER")}, "1", true, "b", false, false},
		{"a MASTER that can't serve doesn't take it", []HostView{up("a", "1", "BACKUP"), stale("b", "2", "MASTER")}, "1", true, "", false, false},
		{"dead holder moves to the healthy host", []HostView{down("a", "1"), up("b", "2", "BACKUP")}, "1", true, "b", false, false},
		{"stale holder moves to the current host", []HostView{stale("a", "1", "MASTER"), up("b", "2", "BACKUP")}, "1", true, "b", false, false},
		{"unassigned goes to keepalived's MASTER", []HostView{up("a", "1", "BACKUP"), up("b", "2", "MASTER")}, "", true, "b", false, false},
		{"unknown holder goes to a healthy host", []HostView{up("a", "1", "BACKUP"), up("b", "2", "BACKUP")}, "99", true, "a", false, false},
		{"split brain on a healthy holder: keep it, flag it", []HostView{up("a", "1", "MASTER"), up("b", "2", "MASTER")}, "1", true, "", true, false},
		{"nothing healthy: nowhere to go", []HostView{down("a", "1"), stale("b", "2", "BACKUP")}, "1", true, "", false, true},
		{"a host without a server ID can't take it", []HostView{down("a", "1"), up("b", "", "MASTER")}, "1", true, "", false, false},
		{"no floating IP managed: report only", []HostView{down("a", "1"), up("b", "2", "MASTER")}, "", false, "", false, false},
		{"no hosts", nil, "", true, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.hosts, c.holder, c.manage)
			got := ""
			if d.AssignTo != nil {
				got = d.AssignTo.Name
			}
			if got != c.assignTo || d.SplitBrain != c.split || d.NoHealthy != c.none {
				t.Fatalf("assign %q split %v none %v; want %q %v %v", got, d.SplitBrain, d.NoHealthy, c.assignTo, c.split, c.none)
			}
		})
	}
}
