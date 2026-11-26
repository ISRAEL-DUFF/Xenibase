import { useQuery } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import { useEffect, useState, type FormEvent } from "react";
import { ApiRequestError, api, errorMessage, type TokenScope } from "../api/client";
import { ProjectPicker, ScopePicker } from "../components/Tokens";
import { Alert, Button, Field, PageHeading, Panel, PanelSkeleton, Select } from "../components/ui";
import { isFollowableRedirect, missingAuthorizeParams, redirectHost, type OAuthAuthorizeSearch } from "../lib/oauth";

/**
 * A third-party app's consent page (RFC 6749 s4.1.1): like approving a CLI
 * device login, the user picks the organisation, may restrict projects,
 * narrows the scopes and chooses the expiry, then approves or denies. The
 * browser then goes back to the app with a code, or with access_denied.
 */
export function OAuthAuthorizePage() {
  const search = useSearch({ from: "/app/oauth/authorize" });
  const missing = missingAuthorizeParams(search);
  if (missing.length > 0) {
    return (
      <>
        <PageHeading title="Connect an app" />
        <Alert>
          This link from the app is incomplete: it lacks {missing.join(", ")}. Go back to the app and try again; if it keeps happening, tell the app's
          developers.
        </Alert>
      </>
    );
  }
  return <Consent search={search} />;
}

function Consent({ search }: { search: OAuthAuthorizeSearch }) {
  const req = useQuery({ queryKey: ["oauth", "authorize", search], queryFn: () => api.oauthAuthorization(search), retry: false });
  const [org, setOrg] = useState("");
  const [scopes, setScopes] = useState<TokenScope[] | null>(null);
  const [projects, setProjects] = useState<string[] | null>(null);
  const [days, setDays] = useState(90);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [leaving, setLeaving] = useState<string | null>(null);
  useEffect(() => {
    if (!org && req.data?.organisations.length) setOrg(req.data.organisations[0].id);
  }, [org, req.data]);
  if (req.isPending) return <PanelSkeleton rows={3} />;
  if (req.isError) {
    const bad = req.error instanceof ApiRequestError && req.error.status === 400;
    return (
      <>
        <PageHeading title="Connect an app" />
        <Alert>{bad ? `This request can't be approved: ${errorMessage(req.error)}. Go back to the app and try again.` : errorMessage(req.error)}</Alert>
      </>
    );
  }
  const info = req.data;
  const host = redirectHost(info.redirect_uri);
  const chosenOrg = info.organisations.find((o) => o.id === org);
  const chosen = scopes ?? info.scopes;
  if (leaving) {
    return (
      <>
        <PageHeading title={`Connect ${info.client_name}`} />
        <Panel className="max-w-lg">
          <p className="text-sm" data-testid="oauth-leaving">
            {leaving === "approved" ? `Connected. Sending you back to ${host}…` : `Not connected. Sending you back to ${host}…`}
          </p>
        </Panel>
      </>
    );
  }
  const decide = async (approve: boolean, e?: FormEvent) => {
    e?.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      const base = {
        client_id: info.client_id,
        redirect_uri: info.redirect_uri,
        state: info.state,
        code_challenge: info.code_challenge,
        code_challenge_method: info.code_challenge_method,
        consent: info.consent,
      };
      const { redirect_to } = await api.decideOAuth(
        approve ? { ...base, approve: true, org_id: org, scopes: chosen, project_ids: projects, expires_in_days: days } : { ...base, approve: false },
      );
      if (!isFollowableRedirect(redirect_to)) throw new Error(`the app's redirect address can't be opened: ${redirect_to}`);
      setLeaving(approve ? "approved" : "denied");
      window.location.assign(redirect_to);
    } catch (e) {
      setErr(errorMessage(e));
      setBusy(false);
    }
  };
  return (
    <>
      <PageHeading
        title={`Connect ${info.client_name}`}
        description={`${info.client_name} asks to act in one of your Xenibase organisations on your behalf. Afterwards you'll be sent back to ${host}.`}
      />
      <Panel className="max-w-lg">
        <form className="flex flex-col gap-4" onSubmit={(e) => void decide(true, e)}>
          <Alert tone="warn">Only approve if you started this from {info.client_name} yourself, just now. You can disconnect it at any time from Account.</Alert>
          {info.organisations.length === 0 && <Alert>You have no active organisation to connect. Create one first, then try again from the app.</Alert>}
          <Field label="Organisation" hint={`${info.client_name} will act in this organisation only, never with more than you can do.`}>
            {(id) => (
              <Select
                id={id}
                value={org}
                onChange={(e) => {
                  setOrg(e.target.value);
                  setProjects(null);
                }}
                data-testid="oauth-org"
              >
                {info.organisations.map((o) => (
                  <option key={o.id} value={o.id}>
                    {o.name}
                    {o.personal ? " (personal)" : ""}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <ScopePicker value={chosen} onChange={setScopes} allowed={info.scopes} />
          {chosenOrg && <ProjectPicker org={chosenOrg.id} value={projects} onChange={setProjects} projects={chosenOrg.projects} />}
          <Field label="Access expires after">
            {(id) => (
              <Select id={id} value={days} onChange={(e) => setDays(Number(e.target.value))} data-testid="oauth-expiry">
                {[7, 30, 90, 180, 365].map((d) => (
                  <option key={d} value={d}>
                    {d} days
                  </option>
                ))}
              </Select>
            )}
          </Field>
          {err && <Alert>{err}</Alert>}
          <div className="flex justify-end gap-2">
            <Button onClick={() => void decide(false)} disabled={busy} data-testid="oauth-deny">
              Deny
            </Button>
            <Button type="submit" variant="primary" busy={busy} disabled={!org || (projects !== null && projects.length === 0)} data-testid="oauth-approve">
              Approve
            </Button>
          </div>
        </form>
      </Panel>
    </>
  );
}
