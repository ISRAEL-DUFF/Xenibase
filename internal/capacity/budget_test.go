package capacity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/store"
)

// The budget is a ceiling on what is bought, not a check of one proposal
// at a time: with nodes costing 1,500 a month and a budget of 3,000, four
// servers of 1,000 proposed at once (an Evaluate pass over shared regions
// and dedicated creations finding no host, on several servers) start one
// between them; the others wait for approval. Before, each saw the same
// headroom and all four started: 5,500 a month.
func TestBudgetCountsStartedProposals(t *testing.T) {
	h := newHarness(t, 1000)
	ctx := context.Background()
	h.settings(t, 3000)
	h.node(t, "existing", "r0", "both", 1500)
	for i := 1; i <= 4; i++ {
		h.region(t, fmt.Sprintf("r%d", i))
	}
	st, err := h.s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			region := fmt.Sprintf("r%d", i+1)
			if i%2 == 0 {
				_, errs[i] = h.s.propose(ctx, st, region, TierShared, "shared disk is filling up")
			} else {
				_, errs[i] = h.s.HostFor(ctx, region, dedicated.Size{CPUs: 8, MemoryMB: 16384, DiskGB: 100})
			}
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, dedicated.ErrHostPending) {
			t.Fatalf("proposal %d: %v", i+1, err)
		}
	}
	got := h.proposals(t)
	if got[ProposalProvisioning] != 1 || got[ProposalPending] != 3 {
		t.Fatalf("proposals: %v, want 1 provisioning and 3 waiting for approval", got)
	}
	rows, err := store.New(h.db).ListCapacityProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.Status == ProposalPending && !strings.Contains(p.Reason, "over the budget of EUR 30.00") {
			t.Errorf("a waiting proposal's reason doesn't name the budget: %q", p.Reason)
		}
	}

	// The started proposal counts once, whether its node exists yet (a
	// HostFor call makes it at once) or not: the next proposal still waits.
	h.region(t, "r5")
	p, err := h.s.propose(ctx, st, "r5", TierShared, "shared disk is filling up")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != ProposalPending {
		t.Errorf("a fifth proposal is %s, want pending", p.Status)
	}
}

// Within the budget, proposals one after another start until the started
// ones fill it.
func TestBudgetStartsWhatFits(t *testing.T) {
	h := newHarness(t, 1000)
	ctx := context.Background()
	h.settings(t, 3500)
	h.node(t, "existing", "r0", "both", 1500)
	st, err := h.s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for i := 1; i <= 3; i++ {
		region := fmt.Sprintf("r%d", i)
		h.region(t, region)
		p, err := h.s.propose(ctx, st, region, TierShared, "shared disk is filling up")
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, p.Status)
	}
	if want := []string{ProposalProvisioning, ProposalProvisioning, ProposalPending}; fmt.Sprint(statuses) != fmt.Sprint(want) {
		t.Errorf("proposals are %v, want %v (1,500 + 2 × 1,000 fills 3,500)", statuses, want)
	}
}
