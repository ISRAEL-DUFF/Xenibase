package config

import (
	"fmt"
	"strconv"
)

// Invitations caps organisation invitations, which send email to any
// address an inviter enters.
type Invitations struct {
	// PerOrgDay is how many invitations one organisation may send in any 24
	// hours (XENIBASE_INVITES_PER_ORG_DAY, default 100; 0 turns the cap off).
	PerOrgDay int
	// PerUserDay is how many one inviter may send across their
	// organisations (XENIBASE_INVITES_PER_USER_DAY, default 50; 0: no cap).
	PerUserDay int
}

func loadInvitations(getenv func(string) string, cfg *Config) []error {
	var errs []error
	inv := Invitations{PerOrgDay: 100, PerUserDay: 50}
	for _, v := range []struct {
		name string
		dst  *int
	}{{"XENIBASE_INVITES_PER_ORG_DAY", &inv.PerOrgDay}, {"XENIBASE_INVITES_PER_USER_DAY", &inv.PerUserDay}} {
		raw := getenv(v.name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s %q: want a whole number", v.name, raw))
			continue
		}
		*v.dst = n
	}
	cfg.Invitations = inv
	return errs
}
