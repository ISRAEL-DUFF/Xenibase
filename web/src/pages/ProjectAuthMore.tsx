import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent, type ReactNode } from "react";
import { api, errorMessage, type Project } from "../api/client";
import type { components } from "../api/schema";
import {
  Alert,
  Badge,
  Button,
  CopyButton,
  FormRow,
  Input,
  KeyValues,
  Panel,
  Select,
  Stat,
  Switch,
  Table,
} from "../components/ui";
import { relativeTime } from "../lib/format";

type S = components["schemas"];
type AuthConfig = S["AuthConfig"];
type Update = S["AuthConfigUpdate"];
type PhoneProvider = S["AuthPhoneProvider"];

// Project → Authentication's phone, providers, MFA and hooks sections
// (V4 §4.1, §4.6, §4.7, §4.9).

const textarea =
  "min-h-16 w-full rounded-md border border-line-strong bg-surface-2 p-2.5 font-mono text-xs";

function useSave(p: Project) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const save = async (u: Update) => {
    setBusy(true);
    setErr(null);
    setSaved(false);
    try {
      await api.updateAuthConfig(p.id, u);
      await qc.invalidateQueries({ queryKey: ["auth-config", p.id] });
      setSaved(true);
      return true;
    } catch (e) {
      setErr(errorMessage(e));
      return false;
    } finally {
      setBusy(false);
    }
  };
  return { busy, err, saved, save };
}

function SaveBar({
  admin,
  busy,
  err,
  saved,
}: {
  admin: boolean;
  busy: boolean;
  err: string | null;
  saved: boolean;
}) {
  if (!admin) return null;
  return (
    <div className="flex items-center justify-end gap-3">
      {saved && (
        <span className="text-xs text-muted">
          Saved; the edge applies it within seconds.
        </span>
      )}
      {err && <Alert>{err}</Alert>}
      <Button type="submit" variant="primary" busy={busy}>
        Save
      </Button>
    </div>
  );
}

function Toggle({
  label,
  value,
  set,
  admin,
  description,
}: {
  label: string;
  value: boolean;
  set: (v: boolean) => void;
  admin: boolean;
  description?: ReactNode;
}) {
  return (
    <FormRow label={label} description={description}>
      <Switch
        aria-label={label}
        checked={value}
        onCheckedChange={set}
        disabled={!admin}
      />
    </FormRow>
  );
}

function money(minor: number, currency?: string) {
  const v = (minor / 100).toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
  return currency ? `${currency} ${v}` : v;
}

// ---- Phone ----------------------------------------------------------------------

const smsProviders: { id: PhoneProvider["provider"]; label: string }[] = [
  { id: "termii", label: "Termii" },
  { id: "africastalking", label: "Africa's Talking" },
  { id: "twilio", label: "Twilio" },
];

export function PhoneTab({
  p,
  cfg,
  admin,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
}) {
  const st = cfg.settings;
  const chans = st.phone_channels ?? [];
  const [sms, setSms] = useState(chans.includes("sms"));
  const [wa, setWa] = useState(chans.includes("whatsapp"));
  const [confirm, setConfirm] = useState(st.phone_confirm ?? true);
  const [countries, setCountries] = useState(
    (st.phone_countries ?? ["NG"]).join(", "),
  );
  const [cap, setCap] = useState(st.phone_daily_cap ?? 200);
  const [tpl, setTpl] = useState(st.sms_template ?? "");
  const { busy, err, saved, save } = useSave(p);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const phone_channels: ("sms" | "whatsapp")[] = [];
    if (sms) phone_channels.push("sms");
    if (wa) phone_channels.push("whatsapp");
    void save({
      settings: {
        phone_channels,
        phone_confirm: confirm,
        phone_countries: countries
          .split(/[\s,]+/)
          .map((c) => c.trim().toUpperCase())
          .filter(Boolean),
        phone_daily_cap: cap,
        sms_template: tpl,
      },
    });
  };
  const ph = cfg.phone;
  const month = ph?.month ?? [];
  const spend = month.reduce((n, m) => n + m.cost_minor, 0);
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <Stat
          label="Codes in the last 24 hours"
          value={`${ph?.sent_24h ?? 0} / ${ph?.daily_cap ?? cap}`}
          hint="Sending stops at the daily cap."
          testId="auth-phone-today"
        />
        <Stat
          label="Platform messages this month"
          value={month.reduce((n, m) => n + m.messages, 0)}
          hint={month.map((m) => `${m.channel}: ${m.messages}`).join(" · ")}
        />
        <Stat
          label="Spend this month"
          value={money(spend, ph?.currency)}
          hint="Billed per message sent through Xenibase's providers."
          testId="auth-phone-spend"
        />
      </div>
      <form
        className="flex flex-col gap-4"
        onSubmit={submit}
        data-testid="auth-phone"
      >
        <Panel title="Phone sign-in">
          <Toggle
            label="Codes by SMS"
            value={sms}
            set={setSms}
            admin={admin}
            description={
              cfg.sms
                ? `Through your ${cfg.sms.provider} account.`
                : ph?.platform_sms
                  ? "Through Xenibase's SMS (the DND route reaches do-not-disturb numbers)."
                  : "This install has no SMS provider: add your own below."
            }
          />
          <Toggle
            label="Codes by WhatsApp"
            value={wa}
            set={setWa}
            admin={admin}
            description={
              cfg.whatsapp
                ? "Through your WhatsApp Business number."
                : ph?.platform_whatsapp
                  ? "Through Xenibase's WhatsApp Business number."
                  : "This install has no WhatsApp sender: add your own below."
            }
          />
          <Toggle
            label="Confirm numbers at sign-up"
            value={confirm}
            set={setConfirm}
            admin={admin}
            description="A phone and password sign-up needs its code before the password works."
          />
          <FormRow
            label="Countries"
            description='ISO codes numbers may be in, such as "NG, GH". Opening more countries opens more room for SMS pumping; "*" allows any.'
          >
            <Input
              aria-label="Countries"
              value={countries}
              onChange={(e) => setCountries(e.target.value)}
              disabled={!admin}
            />
          </FormRow>
          <FormRow
            label="Daily cap"
            description="SMS and WhatsApp codes a day. Each number also gets at most 5 an hour. You're emailed at a quarter of the cap in an hour and when it's reached."
          >
            <Input
              type="number"
              min={1}
              aria-label="Daily cap"
              value={cap}
              onChange={(e) => setCap(Number(e.target.value))}
              disabled={!admin}
            />
          </FormRow>
          <FormRow
            label="SMS text"
            description="With {{.Code}}; empty for the default. WhatsApp uses the approved authentication template."
          >
            <Input
              aria-label="SMS text"
              placeholder="{{.Code}} is your verification code. It expires in 10 minutes."
              value={tpl}
              onChange={(e) => setTpl(e.target.value)}
              disabled={!admin}
            />
          </FormRow>
        </Panel>
        <SaveBar admin={admin} busy={busy} err={err} saved={saved} />
      </form>
      <ProviderPanel p={p} cfg={cfg} admin={admin} channel="sms" />
      <ProviderPanel p={p} cfg={cfg} admin={admin} channel="whatsapp" />
    </div>
  );
}

function ProviderPanel({
  p,
  cfg,
  admin,
  channel,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
  channel: "sms" | "whatsapp";
}) {
  const cur = channel === "sms" ? cfg.sms : cfg.whatsapp;
  const [prov, setProv] = useState<PhoneProvider>(
    cur ?? { provider: channel === "sms" ? "termii" : "whatsapp_cloud" },
  );
  const { busy, err, saved, save } = useSave(p);
  const set = (k: keyof PhoneProvider) => (v: string) =>
    setProv((x) => ({ ...x, [k]: v }));
  const field = (k: keyof PhoneProvider, label: string, secret = false) => (
    <FormRow
      key={k}
      label={label}
      description={
        secret && cur ? "Stored; leave empty to keep it." : undefined
      }
    >
      <Input
        aria-label={label}
        type={secret ? "password" : "text"}
        value={(prov[k] as string | undefined) ?? ""}
        onChange={(e) => set(k)(e.target.value)}
        disabled={!admin}
      />
    </FormRow>
  );
  const submit = (e: FormEvent) => {
    e.preventDefault();
    void save(channel === "sms" ? { sms: prov } : { whatsapp: prov });
  };
  const clear = () =>
    void save(
      channel === "sms" ? { clear_sms: true } : { clear_whatsapp: true },
    );
  const fields: ReactNode[] = [];
  switch (prov.provider) {
    case "termii":
      fields.push(
        field("api_key", "API key", true),
        field("sender_id", "Sender ID"),
        field("base_url", "API base URL (optional)"),
      );
      break;
    case "africastalking":
      fields.push(
        field("username", "Username"),
        field("api_key", "API key", true),
        field("sender_id", "Sender ID or short code (optional)"),
      );
      break;
    case "twilio":
      fields.push(
        field("account_sid", "Account SID"),
        field("auth_token", "Auth token", true),
        field("from", channel === "sms" ? "From number" : "WhatsApp sender"),
      );
      if (channel === "sms")
        fields.push(
          field(
            "messaging_service_sid",
            "Messaging service SID (instead of a number)",
          ),
        );
      break;
    case "whatsapp_cloud":
      fields.push(
        field("phone_number_id", "Phone number ID"),
        field("access_token", "Access token", true),
        field("template", "Authentication template name"),
        field("language", "Template language (default en)"),
      );
      break;
  }
  return (
    <form onSubmit={submit} data-testid={`auth-provider-${channel}`}>
      <Panel
        title={channel === "sms" ? "Your SMS provider" : "Your WhatsApp sender"}
        description={
          cur
            ? "Codes go through your own account; Xenibase doesn't bill for them."
            : "Optional: bring your own account instead of Xenibase's (required for Free projects where the platform's isn't offered)."
        }
      >
        <FormRow label="Provider">
          <Select
            aria-label={`${channel} provider`}
            value={prov.provider}
            onChange={(e) =>
              setProv({ provider: e.target.value as PhoneProvider["provider"] })
            }
            disabled={!admin}
          >
            {(channel === "sms"
              ? smsProviders
              : [
                  {
                    id: "whatsapp_cloud" as const,
                    label: "WhatsApp Cloud API (Meta)",
                  },
                  { id: "twilio" as const, label: "Twilio" },
                ]
            ).map((o) => (
              <option key={o.id} value={o.id}>
                {o.label}
              </option>
            ))}
          </Select>
        </FormRow>
        {fields}
        {admin && (
          <div className="mt-2 flex items-center justify-end gap-3">
            {saved && <span className="text-xs text-muted">Saved.</span>}
            {err && <Alert>{err}</Alert>}
            {cur && (
              <Button type="button" onClick={clear} disabled={busy}>
                Use Xenibase's
              </Button>
            )}
            <Button type="submit" variant="primary" busy={busy}>
              Save provider
            </Button>
          </div>
        )}
      </Panel>
    </form>
  );
}

// ---- Providers ---------------------------------------------------------------------

const oauthProviders: { id: string; label: string; console: string }[] = [
  {
    id: "google",
    label: "Google",
    console: "Google Cloud console → APIs & Services → Credentials",
  },
  {
    id: "apple",
    label: "Apple",
    console:
      "Apple Developer → Certificates, IDs & Profiles (a Services ID and a key)",
  },
  {
    id: "github",
    label: "GitHub",
    console: "GitHub → Settings → Developer settings → OAuth Apps",
  },
  {
    id: "facebook",
    label: "Facebook",
    console: "Meta for Developers → your app → Facebook Login",
  },
  {
    id: "microsoft",
    label: "Microsoft",
    console: "Microsoft Entra admin center → App registrations",
  },
];

export function ProvidersTab({
  p,
  cfg,
  admin,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
}) {
  const st = cfg.settings;
  const [oauth, setOauth] = useState<Record<string, S["AuthOAuthSetting"]>>(
    st.oauth ?? {},
  );
  const [secrets, setSecrets] = useState<Record<string, S["AuthOAuthSecret"]>>(
    {},
  );
  const [anon, setAnon] = useState(!!st.anonymous_enabled);
  const [linking, setLinking] = useState(st.manual_linking ?? true);
  const { busy, err, saved, save } = useSave(p);
  const upd = (id: string, v: Partial<S["AuthOAuthSetting"]>) =>
    setOauth((o) => {
      const cur = o[id] ?? { enabled: false, client_id: "" };
      return { ...o, [id]: { ...cur, ...v } };
    });
  const sec = (id: string, v: Partial<S["AuthOAuthSecret"]>) =>
    setSecrets((s) => ({ ...s, [id]: { ...s[id], ...v } }));
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const oauth_secrets: Record<string, S["AuthOAuthSecret"]> = {};
    for (const [k, v] of Object.entries(secrets))
      if (v.client_secret || v.private_key) oauth_secrets[k] = v;
    void save({
      settings: { oauth, anonymous_enabled: anon, manual_linking: linking },
      oauth_secrets,
    }).then((ok) => ok && setSecrets({}));
  };
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={submit}
      data-testid="auth-providers"
    >
      <Panel title="Callback URL">
        <p className="text-[13px] text-muted">
          Register this as the redirect URI with each provider. Your app passes
          its own redirect_to, which must be the site URL or one of the redirect
          URLs.
        </p>
        <span className="mt-2 inline-flex items-center gap-2 font-mono text-xs">
          {cfg.oauth_callback_url}
          {cfg.oauth_callback_url && (
            <CopyButton value={cfg.oauth_callback_url} />
          )}
        </span>
      </Panel>
      {oauthProviders.map((o) => {
        const v = oauth[o.id] ?? { enabled: false, client_id: "" };
        const stored = !!cfg.oauth_secret_set?.[o.id];
        return (
          <Panel
            key={o.id}
            title={
              <span className="inline-flex items-center gap-2">
                {o.label}
                {v.enabled && <Badge tone="ok">On</Badge>}
              </span>
            }
            description={o.console}
            testId={`auth-oauth-${o.id}`}
          >
            <Toggle
              label={`Sign in with ${o.label}`}
              value={v.enabled}
              set={(b) => upd(o.id, { enabled: b })}
              admin={admin}
            />
            <FormRow label={o.id === "apple" ? "Services ID" : "Client ID"}>
              <Input
                aria-label={`${o.label} client ID`}
                value={v.client_id}
                onChange={(e) => upd(o.id, { client_id: e.target.value })}
                disabled={!admin}
              />
            </FormRow>
            {o.id === "apple" ? (
              <>
                <FormRow label="Team ID">
                  <Input
                    aria-label="Apple team ID"
                    value={v.team_id ?? ""}
                    onChange={(e) => upd(o.id, { team_id: e.target.value })}
                    disabled={!admin}
                  />
                </FormRow>
                <FormRow label="Key ID">
                  <Input
                    aria-label="Apple key ID"
                    value={v.key_id ?? ""}
                    onChange={(e) => upd(o.id, { key_id: e.target.value })}
                    disabled={!admin}
                  />
                </FormRow>
                <FormRow
                  label="Private key (.p8)"
                  description={
                    stored
                      ? "Stored; paste a new one to replace it."
                      : undefined
                  }
                >
                  <textarea
                    aria-label="Apple private key"
                    className={textarea}
                    placeholder="-----BEGIN PRIVATE KEY-----"
                    value={secrets[o.id]?.private_key ?? ""}
                    onChange={(e) => sec(o.id, { private_key: e.target.value })}
                    disabled={!admin}
                  />
                </FormRow>
              </>
            ) : (
              <FormRow
                label="Client secret"
                description={
                  stored ? "Stored; leave empty to keep it." : undefined
                }
              >
                <Input
                  type="password"
                  aria-label={`${o.label} client secret`}
                  value={secrets[o.id]?.client_secret ?? ""}
                  onChange={(e) => sec(o.id, { client_secret: e.target.value })}
                  disabled={!admin}
                />
              </FormRow>
            )}
          </Panel>
        );
      })}
      <Panel title="Anonymous users and linking">
        <Toggle
          label="Anonymous sign-in"
          value={anon}
          set={setAnon}
          admin={admin}
          description="Users start without credentials and become permanent when they add an email, phone or provider."
        />
        <Toggle
          label="Let users link and unlink identities"
          value={linking}
          set={setLinking}
          admin={admin}
          description="A provider sign-in with a verified email already joins that user's account either way."
        />
      </Panel>
      <SaveBar admin={admin} busy={busy} err={err} saved={saved} />
    </form>
  );
}

// ---- MFA and security ---------------------------------------------------------------

export function SecurityTab({
  p,
  cfg,
  admin,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
}) {
  const st = cfg.settings;
  const [mfa, setMfa] = useState(st.mfa_policy ?? "optional");
  const [mfaPhone, setMfaPhone] = useState(!!st.mfa_phone);
  const [captcha, setCaptcha] = useState(!!st.captcha_enabled);
  const [siteKey, setSiteKey] = useState(st.captcha_site_key ?? "");
  const [secret, setSecret] = useState("");
  const { busy, err, saved, save } = useSave(p);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const u: Update = {
      settings: {
        mfa_policy: mfa,
        mfa_phone: mfaPhone,
        captcha_enabled: captcha,
        captcha_site_key: siteKey,
      },
    };
    if (secret) u.captcha_secret = secret;
    void save(u).then((ok) => ok && setSecret(""));
  };
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={submit}
      data-testid="auth-security"
    >
      <Panel title="Multi-factor authentication">
        <FormRow
          label="Policy"
          description="Required: the data API answers only sessions at aal2. By claim: only users whose app_metadata.mfa_required is true need it."
        >
          <Select
            aria-label="MFA policy"
            value={mfa}
            onChange={(e) => setMfa(e.target.value as typeof mfa)}
            disabled={!admin}
          >
            <option value="off">Off</option>
            <option value="optional">Optional (users choose)</option>
            <option value="claim">Required for some users (by claim)</option>
            <option value="required">Required for everyone</option>
          </Select>
        </FormRow>
        <Toggle
          label="Phone codes as a second factor"
          value={mfaPhone}
          set={setMfaPhone}
          admin={admin}
          description="Authenticator apps (TOTP) are always allowed; phone codes are sent and billed like sign-in codes."
        />
      </Panel>
      <Panel
        title="Captcha (Cloudflare Turnstile)"
        description="Checked on sign-up, sign-in and code requests. Clients send the token as gotrue_meta_security.captcha_token."
      >
        <Toggle
          label="Require captcha"
          value={captcha}
          set={setCaptcha}
          admin={admin}
        />
        <FormRow label="Site key">
          <Input
            aria-label="Turnstile site key"
            value={siteKey}
            onChange={(e) => setSiteKey(e.target.value)}
            disabled={!admin}
          />
        </FormRow>
        <FormRow
          label="Secret key"
          description={
            cfg.captcha_secret_set
              ? "Stored; leave empty to keep it."
              : undefined
          }
        >
          <Input
            type="password"
            aria-label="Turnstile secret key"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            disabled={!admin}
          />
        </FormRow>
      </Panel>
      <SaveBar admin={admin} busy={busy} err={err} saved={saved} />
    </form>
  );
}

// ---- Hooks ------------------------------------------------------------------------------

export function HooksTab({
  p,
  cfg,
  admin,
}: {
  p: Project;
  cfg: AuthConfig;
  admin: boolean;
}) {
  const st = cfg.settings;
  const [claims, setClaims] = useState(st.custom_claims_hook ?? "");
  const [before, setBefore] = useState(st.before_signup_hook ?? "");
  const [beforeURL, setBeforeURL] = useState(st.before_signup_url ?? "");
  const [afterUp, setAfterUp] = useState(st.after_signup_url ?? "");
  const [afterIn, setAfterIn] = useState(st.after_signin_url ?? "");
  const [send, setSend] = useState(st.send_message_url ?? "");
  const { busy, err, saved, save } = useSave(p);
  const deliveries = useQuery({
    queryKey: ["auth-hooks", p.id],
    queryFn: () => api.authHookDeliveries(p.id),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    void save({
      settings: {
        custom_claims_hook: claims,
        before_signup_hook: before,
        before_signup_url: beforeURL,
        after_signup_url: afterUp,
        after_signin_url: afterIn,
        send_message_url: send,
      },
    });
  };
  const url = (
    label: string,
    v: string,
    set: (v: string) => void,
    description: string,
  ) => (
    <FormRow label={label} description={description}>
      <Input
        aria-label={label}
        placeholder="https://api.example.com/hooks/auth"
        value={v}
        onChange={(e) => set(e.target.value)}
        disabled={!admin}
      />
    </FormRow>
  );
  return (
    <div className="flex flex-col gap-4">
      <form
        className="flex flex-col gap-4"
        onSubmit={submit}
        data-testid="auth-hooks"
      >
        <Panel
          title="Postgres hooks"
          description={
            <>
              Functions (event jsonb) returns jsonb, run with a 2-second timeout
              as <code className="font-mono">{cfg.hook_role}</code>, which can
              read only what you grant it, e.g.{" "}
              <code className="font-mono">
                GRANT SELECT ON members TO "{cfg.hook_role}";
              </code>
            </>
          }
        >
          <FormRow
            label="Custom claims"
            description='Called when a token is issued with {"user_id", "claims", "authentication_method"}; return {"claims": {...}} to add claims such as org_id (sub, role, aud, aal and the times stay as they are).'
          >
            <Input
              aria-label="Custom claims function"
              placeholder="public.custom_claims"
              value={claims}
              onChange={(e) => setClaims(e.target.value)}
              disabled={!admin}
            />
          </FormRow>
          <FormRow
            label="Before sign-up"
            description='Called with {"user", "method", "ip"}; return {"decision": "reject", "message": "…"} to refuse.'
          >
            <Input
              aria-label="Before sign-up function"
              placeholder="public.check_signup"
              value={before}
              onChange={(e) => setBefore(e.target.value)}
              disabled={!admin}
            />
          </FormRow>
        </Panel>
        <Panel
          title="Webhooks"
          description="Signed with the hook secret (Xenibase-Signature, as database webhooks), retried with backoff, and checked against the organisation's outbound rules."
        >
          {url(
            "Before sign-up URL",
            beforeURL,
            setBeforeURL,
            'Instead of the Postgres hook: a 4xx or {"decision":"reject"} refuses; no answer in 3 seconds lets the sign-up through.',
          )}
          {url(
            "After sign-up URL",
            afterUp,
            setAfterUp,
            "Told about each new user.",
          )}
          {url(
            "After sign-in URL",
            afterIn,
            setAfterIn,
            "Told about each sign-in.",
          )}
          {url(
            "Send message URL",
            send,
            setSend,
            "Receives every email and code (with the text) instead of Xenibase sending them.",
          )}
          {cfg.hook_secret ? (
            <KeyValues
              items={[
                [
                  "Hook secret",
                  <span
                    key="s"
                    className="inline-flex items-center gap-2 font-mono text-xs"
                  >
                    {cfg.hook_secret.slice(0, 10)}…
                    <CopyButton value={cfg.hook_secret} />
                  </span>,
                ],
              ]}
            />
          ) : (
            cfg.hook_secret_set && (
              <KeyValues
                items={[["Hook secret", "Set (shown to project admins)"]]}
              />
            )
          )}
        </Panel>
        <SaveBar admin={admin} busy={busy} err={err} saved={saved} />
      </form>
      {admin && (cfg.hook_secret || cfg.hook_secret_set) && (
        <div className="flex justify-end">
          <Button
            type="button"
            onClick={() => void save({ rotate_hook_secret: true })}
          >
            Rotate the hook secret
          </Button>
        </div>
      )}
      <Panel title="Recent deliveries" testId="auth-hook-deliveries">
        {deliveries.data?.items.length ? (
          <Table head={["Event", "When", "Attempts", "Result"]}>
            {deliveries.data.items.map((d) => (
              <tr key={d.id} className="border-t border-line">
                <td className="px-3 py-2 font-mono text-xs">{d.event}</td>
                <td className="px-3 py-2 text-xs">
                  {relativeTime(d.created_at)}
                </td>
                <td className="px-3 py-2 text-xs">{d.attempts}</td>
                <td className="px-3 py-2 text-xs">
                  {d.delivered_at ? (
                    <Badge tone="ok">Delivered {d.last_status ?? ""}</Badge>
                  ) : d.failed_at ? (
                    <Badge tone="danger">Failed</Badge>
                  ) : (
                    <Badge>Retrying</Badge>
                  )}
                  {d.last_error && !d.delivered_at && (
                    <span className="ml-2 text-muted">{d.last_error}</span>
                  )}
                </td>
              </tr>
            ))}
          </Table>
        ) : (
          <p className="text-[13px] text-muted">No webhook deliveries yet.</p>
        )}
      </Panel>
    </div>
  );
}
