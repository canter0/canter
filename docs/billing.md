# Canter billing

## Current integration (2026-09-18)

Every workspace defaults to **Pay as you go: $0/month plus resource usage**.
Owners may instead select **Pro: $20/month including $20 of infrastructure
usage**. At $21 of usage, Pro costs $20 upfront plus $1 overage. Unused credit
expires at renewal. This does not include AI model credit.

The Plans screen always labels the current plan and offers both options. New
subscribers use hosted Stripe Checkout. Existing active subscribers can schedule
an upgrade or downgrade for their next renewal, or cancel the pending switch.
Stripe Subscription Schedules preserve the current period without proration or
resetting paid credit. Requests are owner-only, require same-origin authentication,
serialize on the workspace, and reject stale renewal dates. The existing
subscription changes only when Stripe reports its actual new prices.

The server checks current Stripe subscription status, automatic collection,
card ownership/expiry and settled Pro payment; it never trusts a return URL.
With billing enabled, initial deployment enqueue and execution require payment
readiness. Changes with positive monthly cost deltas are checked at execution.
Disabled billing retains the existing beta provisioning behavior.

The existing Autodisc Stripe account has separate Canter product, prices, meter,
portal and webhook configuration. Unrelated customers' events are ignored.
`scripts/setup-billing.py` creates both plans by default (`--payg-only` is optional).
Credentials remain in ignored mode-0600 local environment files and the protected
production control-plane environment; browser bundles never receive them.

Resource collection now runs every minute when billing is enabled. It queries
actual server flavors and complete paginated S3 inventories in each workspace's
canonical system namespaces. Rates (`resources-2026-09-18-v1`) are $3 per 720
hours per allocated 1-vCPU/1-GiB bundle (the larger CPU or memory requirement),
and $0.014 per decimal GB per 720 hours. Reads, writes and direct downloads are
free. Shared control-plane artifacts outside system namespaces are not billed.

Only adjacent successful observations at most five minutes apart are charged,
using the smaller of the two observed allocations. The first observation,
unobserved outages, the final unobserved interval at deletion/renewal, and less
than one remaining cent at period end are absorbed by Canter. No pre-signup or
inactive-period usage is backfilled. Exact integer numerator remainders retain
fractional cents within each resource and billing period. Snapshots and usage
outbox events commit atomically under a workspace lock; provider reads also use
an advisory lock. Failed or partial inventories never produce charges. The UI
reports collection failures and retained-resource billing holds.

Cancellation is scheduled at period end. Payment failure, cancellation, or an
expired unreconciled period blocks new paid provisioning. Inactive billing
clears sampling baselines and does not charge for retained resources. Existing
resources and data are preserved for explicit owner/operator review; this is a
manual retention policy, not automatic destruction or suspension. Canter bears
the retention cost. Operators must review `billing_collection_status.issue`
and arrange resource removal with the owner. Billing recovery starts a fresh
observation interval, never charging the inactive interval retroactively.

Live activation requires green database/provider/sandbox tests, catalog
validation, the dedicated webhook enabled, and `CANTER_BILLING_ENABLED=true`.
No tax registration or automatic tax collection is enabled.

Validation includes database-backed checkout/webhook/idempotency tests, card
readiness and provisioning checks, renewal schedule tests, and real Stripe
sandbox verification: $0 initial pay-as-you-go invoice, $1.23 usage -> $1.23
preview; Pro $20 paid upfront, $21 usage -> $1 current-period overage (the renewal
preview also includes the next month's $20 base). A real sandbox renewal schedule
was created and released successfully. No real customer charge was performed.

## Plan contract and pricing background

### Current local pricing comparison

The user requested restoration of the original seven-row resource comparison.
The local page uses $3 per 720 hours for 1 vCPU / 1 GB RAM with IPv4 and local
disk. The user's latest explicit storage direction supersedes the calculated
storage markup: **$0.014/GB-month, $0 reads, $0 writes and $0 direct object
egress**. These storage prices undercut the quoted supplier storage cost and
absorb supplier request charges; they do not establish storage break-even.
The compute price is a calculation from this page correction, not a recovered
prior customer quote or verified live tariff. Its model rounds
`(720*0.00138 + 0.59 + 0.75 + 0.30) / 0.886` to $3. The $0.75 shared cash allocation,
6.4% payment fee and 5% margin target remain modeling assumptions; founder
time is excluded. Live billing stays disabled pending actual metering and cost
verification. The two whole-app comparison examples below are superseded and
are retained only as calculation history.

The public plan cards and Go billing service share `pricing/catalog.json`.
Prices are USD, before tax, with two options and no per-member charges:

| Plan | Monthly payment | Usage credit | Additional usage |
| --- | ---: | ---: | --- |
| Pay as you go | $0 | $0 | All rated usage |
| Pro | $20 upfront | $20 each billing month | `max(0, usage - $20)` |

At $21 of usage, Pro costs $20 upfront plus $1 additional: $21 total.
Credit expires at the end of the subscription billing period and resets at
renewal. It never produces a negative invoice and is not a resource-rate discount.
Founder time is excluded from pricing. Spread actual shared bills across the
customer base and retain a small overall cash surplus.

## What is implemented

- `/pricing`: two simple plan cards, four FAQs and a sourced cost
  comparison with Railway Compute/Buckets and AWS EC2/S3. No fabricated resource rates,
  savings percentages, customer testimonials or feature wall.
- `/app/billing`: plan choice, hosted checkout, current-period usage/credit/total,
  pending-payment state and hosted invoice/payment management.
- PostgreSQL migration `014_billing`: workspace/customer association, verified
  subscription state, immutable rated-usage events and webhook deduplication.
- Owner-only checkout and portal. Agents can inspect the workspace billing view
  under existing read authority; they cannot subscribe, set usage or grant credit.
- Stripe signature verification, current-state reconciliation to withstand stale
  webhooks, retry-safe Checkout reuse and duplicate-subscription prevention.
  Customer associations are committed before Checkout creation, so a provider
  timeout cannot leave a payable subscription disconnected from its workspace.
- Durable usage outbox with provider idempotency. Uncertain sends older than 23
  hours are marked for reconciliation rather than replayed beyond Stripe's
  documented minimum 24-hour event-deduplication window.

## Stripe setup

Keep `CANTER_BILLING_ENABLED=false` until the steps below pass. Disabled checkout
is explicit in the account UI and rejects payment attempts on the server.

1. Put a **test** secret key in `CANTER_STRIPE_SECRET_KEY` using your environment
   or secret manager. Do not place it in frontend variables or commit it.
2. Run `python3 scripts/setup-billing.py`. It creates/reuses one hosting product,
   a sum meter and three monthly prices, then prints their non-secret IDs. It
   also creates a dedicated billing portal configuration. Reused prices are
   validated against the shared catalog at service startup and before Checkout.
3. Set the printed variables. Supply `CANTER_BILLING_INGEST_TOKEN` as a random
   secret of at least 32 characters, private to the rated-usage producer.
4. Point a Stripe webhook at `/api/canter/v1/billing/webhook` on the public web
   origin (or `/v1/billing/webhook` directly on the API). Configure its signing
   secret in `CANTER_STRIPE_WEBHOOK_SECRET`. Subscribe to:
   `checkout.session.completed`, `customer.subscription.created`,
   `customer.subscription.updated`, `customer.subscription.deleted`,
   `invoice.paid`, and `invoice.payment_failed`.
5. Configure taxes for the merchant's actual situation in Stripe before launch.
   Public amounts are exclusive of tax; this implementation does not assume a
   tax registration or automatically enable Stripe Tax.
6. Connect and verify the rated usage producer below; enable billing in the test
   environment. Complete hosted Checkout using Stripe's test payment methods,
   verify receipt of a signed webhook, and confirm Pro stays pending until paid.
7. Test usage below, at and above $20, renewal, cancellation, provider downtime,
   and webhook retries. Check real Stripe invoice previews against the local bill.
8. To create a live catalog, explicitly pass `--live` with the live key and use
   separate live webhook/price/meter/portal IDs. No live customers, subscriptions,
   payments or billing configuration were created by this implementation task.

### How the $20 credit reaches the invoice

Pay-as-you-go has one metered price: one cent per rated-usage cent.
Pro has a $20 fixed recurring price plus a graduated metered price: the first
2,000 rated cents are zero-priced, and each rated cent above that costs one cent.
The sum meter receives **full usage**, not already-discounted overage. Applying
the credit before sending to Stripe would discount the customer twice.

The fixed subscription fee is paid in advance; usage is billed in arrears.
A renewal invoice can therefore combine the next month's $20 subscription with
the previous month's additional usage. The dashboard labels its estimate as a
period total, rather than claiming it is that combined invoice's amount due.

This integration uses the supported Stripe Billing Meters API with version
`2025-03-31.basil`. Stripe now recommends Metronome for new advanced usage billing;
the adapter is deliberately limited to one aggregated monetary usage dimension.
Reassess that choice before adding dimensional tariffs or enterprise contracts.

## Rated usage producer: required before live billing

There is no verified Canter resource tariff or billing-grade resource collector
in the reviewed application. This work implements the billing contract and
payment/ledger integration; it does not invent CPU, RAM or storage rates.

An internal producer must turn measured/reserved resource use and a reviewed,
versioned customer quote into **incremental integer USD cents**. Preserve
fractional amounts between samples rather than rounding each sample up. Post:

```http
POST /v1/billing/usage
Authorization: Bearer <private ingestion token>
Content-Type: application/json

{
  "id": "stable-unique-usage-event-id",
  "workspaceId": "wrk_...",
  "amountCents": 2100,
  "resource": "app/example",
  "rateVersion": "reviewed-quote-version",
  "occurredAt": "2026-09-15T12:00:00Z"
}
```

Use a unique event ID per increment; retries must reuse identical data. Do not
send repeated cumulative totals. The request is accepted only for an active
billing period; a period's end is exclusive. Late events from a closed period
require explicit reconciliation. The dispatcher retries queued events every
15 seconds. Successful ingestion is not confirmation of invoice payment.

Before enabling live billing, the producer and runtime must also reconcile
period transitions, failed payment, cancellation and retained resources. Do not
promise a hard spending cap or automatically delete user data from this module.
The existing beta provider-spend reservation is **not** customer usage and must
not be passed through as a customer charge. Rate actual commercial usage only.

## Verification

Run Go unit tests with `go test ./pricing ./internal/controlplane`.
Run database coverage against a **dedicated disposable database** by setting
`CANTER_TEST_DATABASE_URL` and running `go test ./internal/controlplane -run Billing`.
The existing integration helper truncates tables, so never use a development or
production database containing data you need.

Check the responsive plan cards and full-width section dividers at `/pricing`.
Follow each plan CTA through authentication;
selection must survive sign-in. Account settings links to the billing screen.

### Implementation checks, September 15, 2026

- Production Next.js build, TypeScript and scoped ESLint passed.
- Browser checks passed at 320, 390, 768 and 1440 pixels, including calculator
  values below/at/above the credit, FAQ interaction and both plan signup paths.
- An isolated local account statement showed $20 subscription, $21 usage,
  $20 applied credit, $1 additional usage and $21 period total. This was test
  data in a disposable database, with payments disabled.
- Pricing, control-plane integration and command tests passed in a disposable
  source copy. Concurrent operator work in the shared checkout changed
  `NewHTTPServer`'s return type and currently prevents two existing auth test
  files from compiling. The verification copy excluded that unfinished operator
  change; no shared edits from the other task were reverted.
- Stripe requests were tested against a local provider fixture. Hosted Stripe
  Checkout, actual Stripe invoices and live charging remain unverified because
  no Stripe credentials were configured.

The production build needs `turbopack.root` set to the repository root because
the shared JSON catalog lives outside `web/`.

## Comparison sources

Verified September 15, 2026:

- [Railway billing](https://docs.railway.com/pricing/understanding-your-bill)
- [AWS EC2 T3 instance pricing](https://aws.amazon.com/ec2/instance-types/t3/) —
  Linux in US East (N. Virginia), with burstable CPU. Instance time is billed
  while running; this differs from Railway consumption billing.
- [AWS S3 object storage](https://aws.amazon.com/s3/pricing/)
- [Railway Buckets](https://docs.railway.com/storage-buckets/billing)
- [AWS EC2 transfer](https://aws.amazon.com/ec2/pricing/on-demand/)
- [AWS IPv4](https://aws.amazon.com/vpc/pricing/)
- [Stripe meter events](https://docs.stripe.com/api/billing/meter-event/create)
- [Stripe graduated prices](https://docs.stripe.com/api/prices/create)
- [Basil invoice status migration](https://docs.stripe.com/changelog/basil/2025-03-31/add-support-for-multiple-partial-payments-on-invoices)

The comparison shows numerical monthly app estimates: $3.25 / $4.50 for
Canter, $5 / $11.40 for Railway, and $12.05 / $19.61 for AWS EC2 with S3.
Savings badges apply only to those stated examples. Inputs, assumed cash
allocations and calculations are in
[`canter-comparison-quotes-2026-09-15.md`](../artifacts/planning/canter-comparison-quotes-2026-09-15.md).

### Quote and storage correction

The earlier public comparison used EBS gp3 and Railway volumes for storage.
That compared block storage, while the intended product comparison is S3-style
object storage. The page now uses S3 Standard and Railway Buckets, including
their distinct request and direct-download charges. EC2 boot disks are a
separate compute cost and do not replace S3 in the object-storage comparison.

The agreed policy is usage priced just above actual cash costs, with shared
expenses spread across customers and founder time excluded. The two payment
options are $0 plus usage and $20 including $20 of usage credit. The public
comparison leads with dollars and example savings. Internal margin explanations
and "rates being finalized" placeholders do not belong in that section.

The illustrative quotes are held in `pricing/comparison-quotes.json`; they
do not define a billing tariff. They were calculated for this page correction,
not recovered as previously approved numerical rates. The rated usage producer
still needs a verified commercial mapping and actual cost inputs before live
charging. The old package proposal remains withdrawn, and the 500-cent
initial-deployment reservation remains an internal beta spend guard.
