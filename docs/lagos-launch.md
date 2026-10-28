# Lagos launch (V3 M27)

M27 ends V3 with the **Lagos region launch**. This page is the launch gate:
what the code and tests already show, and what operations must do before
the region opens to customers. Work through it top to bottom. Every item
in the second half needs a person; none of it is code.

## What the build shows

| Area | Evidence | Where |
| --- | --- | --- |
| Region model, placement, per-region poolers, forwarding | `TestPoolerRegions`, `TestRegionBackupsAndCopies` | [Regions](regions.md) |
| Residency HA project in Lagos survives losing its primary's node | `TestLagosHAResidency` (M25 done-when) | [Regions](regions.md), [HA](ha.md) |
| Billing ledger tied to invoices, credit notes and payments | `billing.Check` with the M27 invariants; audit tests | [Billing audit](billing-audit.md) |
| Payment webhooks can't be forged or redirected; money-moving admin actions need step-up | M27 security review | [Security review](security-review.md) |
| Wallet provider outage: no false declines, retried charges, nothing posted twice | `TestProviderOutage`, `TestChargeRetryDuringOutage` | [Payments](payments.md) |
| Pooler split brain in one region; the other region unaffected | `TestChaosPoolerSplitBrain` | [Edge poolers](edge-poolers.md) |
| Waker failure: alert, dashboard resume, no pausing into a dead waker | `TestChaosWakerFailure` | [Free tier](free-tier.md) |
| etcd member loss and quorum loss: writes continue | `TestChaosEtcdMemberLoss` | [HA](ha.md) |
| Rating and the month end at 1,000 organisations | `TestRatingLoad` | [Load test](load-test.md#v3-rating-load-test) |

Fixes M27 made on the way, all covered by the tests above:

- A credit note on a partly paid invoice made the receivable negative,
  and a fully credited invoice stayed open (so dunning saw the org as
  overdue).
- A signed but forged provider event could redirect another org's
  transfer to the attacker's payment intent.
- An automatic charge that hit a provider outage was never retried, and a
  retry during an outage was used up and emailed the customer that
  their payment had failed. (At the time the charge was to a card; it is a
  wallet debit now.)
- The split-brain alert counted keepalived MASTERs across regions, so a
  second region's healthy pair would have raised a permanent critical
  alert.
- A dead waker was never restarted or alerted on, and the sweep went on
  pausing projects behind it.
- Losing etcd's quorum demoted every HA primary within seconds (Patroni
  failsafe mode is now on).
- A switchover asked for just after enabling synchronous HA failed with
  Patroni's "candidate name does not match with sync_standby".

## Before opening the region

### Money

1. **The accountant answers the six questions** in
   [Billing audit](billing-audit.md#for-the-accountant), and the
   answers are recorded there. Change the billing settings (VAT rate, WHT
   rate, invoice layout) to match.
2. **The paid launch gate** in the [admin runbook](admin-runbook.md#before-charging-anyone-the-paid-launch)
   is complete: the wallet provider's live key and webhook registered,
   legal documents published, a week of internal billing reconciled with
   no differences and `Ledger check` balanced.
3. A **price book** for Lagos customers, if prices differ, is published at
   least 30 days before it applies.

### Infrastructure in Lagos

4. **Three machines** for the first nodes, in at least two racks or
   facilities: one with a shared cluster, all three able to take dedicated
   instances. Their agents registered with `"region": "ng-lagos"`.
5. **Two pooler hosts** in Lagos with keepalived, on a private network
   with the nodes. The region's **floating IP** (or the colocation
   provider's equivalent) assigned, and its ID set on the region.
6. **An in-country S3-compatible bucket** as a platform storage target,
   chosen as the region's backup target; **residency** turned on for the
   region.
7. **A copy target outside Nigeria** for projects without residency.
8. **DNS**: `db.ng.<your domain>` points at the floating IP, and the
   region's TLS certificate names it:
   `openssl s_client -connect db.ng.<domain>:6543 -starttls postgres`.
9. The **etcd cluster** for HA: set up the Lagos region's own cluster
   (Platform → Nodes → etcd cluster, region `ng-lagos`) on three Lagos
   nodes in **three different failure domains** (racks or power feeds,
   recorded on each node; V3.1 §2). HA projects created in Lagos then use
   it, and HA projects already in Lagos on the home region's cluster move
   with **Move to the region's etcd** on their HA card (V3.1 §3.3; a pause
   of a few seconds each). With only two failure domains in the facility,
   don't offer HA in Lagos yet ([HA](ha.md), [failure domains](failure-domains.md)).
10. Open the **waker port** (`XENIBASE_WAKER_ADDR`) from the Lagos pooler
    hosts to xenibase-server, and check `waker_down` isn't firing.
11. **Backend services** (if Lagos projects use the data API, auth, files
    or realtime): give the region its own **API domain**
    (`ng.api.<your domain>`), with a wildcard DNS record and certificate
    for it, and run xenibase-edge in Lagos with
    `XENIBASE_EDGE_DOMAIN=ng.api.<your domain>` and
    `XENIBASE_EDGE_REGION=ng-lagos` ([Regions](regions.md#a-regions-api-domain)).
    Without it, Lagos projects' API calls go through the home region.
12. **Early access** for the rehearsals: add your own organisation to the
    region's **Early-access organisations**, so you can create projects
    there while it is hidden. Add a first customer here too if they start
    before everyone else.

### Rehearsals in Lagos (with a client writing throughout)

13. **Kill the pooler host** holding the floating IP: queries are back
    within 10 seconds (docs/edge-poolers.md).
14. **Disconnect the pooler hosts from each other**: one `pooler_split_brain`
    alert for `ng-lagos`, none for the home region, and the floating IP
    doesn't flap.
15. **Kill a dedicated HA project's primary node**: writes are back within
    60 seconds on the standby.
16. **Restore a backup from the in-country bucket** and, for a project
    without residency, from the copy target.
17. **Stop one etcd member**, then a second for a minute: writes continue.
18. **Pause a Free project in Lagos** and connect to it: the waker's
    message, then the project resumes.

### Opening

19. Check the region's **launch checks** (Platform → Regions → the region):
    every blocking one must pass, and read every warning. Then turn off
    **Hidden**. New projects can now choose Lagos. The early-access
    list stays; empty it once it no longer matters.
20. Post an announcement through the status page, and watch for a week:
    alerts, pooler events, failover history, reconciliation, and the
    first month end's invoices for Lagos organisations.

Record who did each step and when below.

| Step | Done by | Date | Notes |
| --- | --- | --- | --- |
| | | | |
