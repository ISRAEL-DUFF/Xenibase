# Support

Xenibase keeps every support conversation in one place. Tickets come from the
dashboard, from email and from WhatsApp, and the support console shows each
one with the organisation it came from (V3 §7.1).

## For customers

**Organisation → Support** lists the organisation's tickets. Owners and
admins see all of them; other members see the ones they opened.
**New ticket** asks for a subject and a description. Tick **Urgent** when
production is down or data is at risk. Xenibase emails an acknowledgement
with the ticket's reference, such as `[T-1001]`. Replying to any email
about the ticket adds the reply to it, and so does replying on the
dashboard.

First responses are counted in Nigerian business hours, 09:00 to 17:00
WAT, Monday to Friday. Public holidays aren't counted yet.

| Plan | First response |
| --- | --- |
| Free | Best effort |
| Pro | Within one business day (8 business hours) |
| Team | Within 4 business hours |
| Urgent, on Pro or Team | Within 1 hour, at any time |

**WhatsApp** is for Pro and Team organisations. An owner or admin
registers the numbers that may write to Xenibase (Organisation → Support →
WhatsApp). Local Nigerian numbers such as `0803…` are stored as `+234…`. A
new number shows a code such as `XB-12345678`: send it, as the whole
message, from that number to Xenibase's support number on WhatsApp within
7 days (registering the number again gives a new code). Until then the
number is pending and its messages don't reach the organisation. Once
confirmed, a message from the number opens a ticket for that organisation,
or adds to its open ticket from the same number, and replies arrive on
WhatsApp. Any other number gets a reply explaining how to reach support.
Several organisations can have a number pending, but only one can confirm
it; the others' registrations are then dropped.

**Email** to the support address opens a ticket for your organisation when
it comes from a member's verified address, or from a billing contact who
confirmed the link Xenibase emailed them when they were added (Billing →
Billing contacts shows **unconfirmed** until then; contacts who are members
need no link). Unconfirmed contacts still get invoices and billing email.
A reply joins a ticket only from the person who opened it or from a member
of the ticket's organisation, and only when the email provider could
authenticate the sender (below).

## For support staff

**Platform → Support** is the console. It is open to platform admins and
to users with the **support** platform role. Platform admins give that role
under Users → **Platform role** (their own password and a code are
needed). Support staff see the console and nothing else of the platform:
not billing settings, nodes, other organisations' pages or their data.

- The queue shows open tickets first, ordered by when they are due an
  answer, with counts of open, pending and overdue tickets. Filters: Open,
  Pending, Mine, All.
- A ticket shows its thread beside the organisation's context. The
  context covers the plan and term, billing mode, standing and balance,
  members, projects (metadata only), recent operations, open incidents and
  quotas. It never includes the databases' contents. For access to data,
  use break-glass ([operations](operations.md)).
- **Reply to customer** sends the reply by the channel the ticket came in
  on: email (threaded, with the reference in the subject) or WhatsApp.
  Email replies always go to the requester's address. **Add note** is
  internal: customers never see notes, by email or on the dashboard.
- Status: *open*, *pending* (waiting on the customer), *solved*,
  *closed*. A reply from the customer reopens the ticket. You can
  also set the priority and the assignee. The assignee must be support
  staff or a platform admin.
- An email from someone Xenibase can't place opens a ticket with no
  organisation. The sender is matched first by an organisation's verified
  billing contact, then by the verified address of a member of exactly
  one organisation.
- **Unverified sender** marks an email whose sender the provider couldn't
  authenticate. Its From address may be forged: it has no organisation or
  user, never joins an existing ticket, and its acknowledgement only gives
  the reference. Check with the organisation another way before acting on
  what it asks, and before assigning it an organisation.

## Setting it up

| Variable | What |
| --- | --- |
| `XENIBASE_SUPPORT_EMAIL` | The support address, e.g. `support@example.com`. It is the Reply-To of support emails, and mail from it is never turned into tickets. |
| `XENIBASE_SUPPORT_INBOUND_SECRET` (or `_FILE`) | At least 16 characters. The email provider's inbound webhook presents it. |
| `XENIBASE_SUPPORT_AUTHSERV_ID` | Optional. The authserv-id of the `Authentication-Results` header your email provider adds (e.g. `mx.example.net`); only headers naming it count. Not needed with Postmark. |
| `XENIBASE_WHATSAPP_PHONE_NUMBER_ID` | The WhatsApp Business number's ID (Meta Cloud API). |
| `XENIBASE_WHATSAPP_ACCESS_TOKEN` | A system-user token with `whatsapp_business_messaging`. |
| `XENIBASE_WHATSAPP_APP_SECRET` | The Meta app's secret, at least 16 characters. It checks `X-Hub-Signature-256` on every webhook. |
| `XENIBASE_WHATSAPP_VERIFY_TOKEN` | Any string. Meta sends it back when you register the webhook. |
| `XENIBASE_WHATSAPP_GRAPH_URL` | Optional; defaults to `https://graph.facebook.com/v21.0`. |

The bundle's `compose.yaml` passes all of these to xenibase-server. Without
an inbound secret, email can't open tickets, though dashboard tickets and
replies by email still work. Without the WhatsApp variables, the WhatsApp
channel is off.

**Inbound email.** Point your email provider's inbound webhook at:

    https://<your Xenibase>/api/v1/support/inbound/email

Authenticate it in one of two ways:

- basic auth, with any username and the inbound secret as the password
  (e.g. `https://inbound:<secret>@<your Xenibase>/…`, as Postmark sends it);
- an `X-Xenibase-Inbound-Secret` header.

The endpoint accepts either of these JSON bodies:

- Postmark's inbound JSON;
- a plain body: `from`, `from_name`, `subject`, `text`, `message_id`,
  `in_reply_to`, `references`, and `authentication_results`: your
  provider's `Authentication-Results` value for the message (a string, or
  an array of them).

The webhook's secret proves your provider called, not who wrote the
email, so Xenibase trusts the sender only on the provider's verdicts: a
DMARC pass for the From domain, or DKIM and SPF passes for domains aligned
with it (equal, or one a subdomain of the other). It reads them from:

- Postmark's `Received-SPF` (the topmost one) and `X-Spam-Tests`
  (`DKIM_VALID_AU` with an SPF pass for the envelope sender's domain, or
  `DMARC_PASS`);
- `Authentication-Results` headers, only those whose authserv-id is
  `XENIBASE_SUPPORT_AUTHSERV_ID` (a sender can write the header too);
- the plain body's `authentication_results`, which is taken as the
  provider's (and filtered by `XENIBASE_SUPPORT_AUTHSERV_ID` when set).

Mail without such a verdict is still accepted, as a new ticket with no
organisation, flagged **unverified sender**, and never joins a thread. If
every inbound email shows as unverified, your provider's verdicts aren't
reaching Xenibase: check which of these it sends.

An authenticated reply joins its ticket by the `[T-n]` reference in its
subject, or by `In-Reply-To` or `References` naming one of Xenibase's
emails, when it is from the ticket's requester or a verified member of its
organisation. Quoted text
(`On … wrote:` and the lines starting with `>`) is removed. A redelivered
email is recorded once.

**WhatsApp.** In the Meta app, set the webhook's callback URL and verify
token, and subscribe to `messages`:

- callback URL: `https://<your Xenibase>/api/v1/support/whatsapp`
- verify token: the value of `XENIBASE_WHATSAPP_VERIFY_TOKEN`

Replies outside WhatsApp's 24-hour customer-service window need an
approved template, which Xenibase doesn't send yet. Answer within the day,
or ask the customer to write again.
