import type { components } from "./schema";
import type { OAuthAuthorizeSearch } from "../lib/oauth";

type S = components["schemas"];
export type Version = S["Version"];
export type TableInfo = S["TableInfo"];
export type EditColumn = S["EditColumn"];
export type RowChange = S["RowChange"];
export type SaveRowsResult = S["SaveRowsResult"];
// Fields with server-side defaults are optional in requests.
export type SchemaChange = Omit<
  S["SchemaChange"],
  "schema" | "concurrently" | "columns" | "column" | "changes"
> & {
  schema?: string;
  concurrently?: boolean;
  columns?: SchemaColumnDef[];
  column?: SchemaColumnDef;
  changes?: SchemaChange[];
};
export type ColumnRef = S["ColumnRef"];
export type SavedQuery = S["SavedQuery"];
export type RowCount = S["RowCount"];
export type SchemaColumnDef = Omit<S["SchemaColumnDef"], "nullable"> & {
  nullable?: boolean;
};
export type SchemaPlan = S["SchemaPlan"];
export type MigrationFormat = S["EditorPreferences"]["migration_format"];
export type GridFilter = {
  column: string;
  op:
    | "eq"
    | "neq"
    | "lt"
    | "lte"
    | "gt"
    | "gte"
    | "contains"
    | "is_null"
    | "not_null"
    | "in";
  value?: string;
  values?: string[];
};
export type GridSort = { column: string; desc: boolean };
export type GridOptions = {
  filters?: GridFilter[];
  /** A raw condition typed in the filter bar. */ where?: string;
  sort?: string;
  desc?: boolean;
  order?: GridSort[];
};

function tableBase(id: string, schema: string, table: string) {
  return `/api/v1/projects/${id}/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}`;
}

/** The grid's query string: repeated filter=JSON, sort, desc, and extras. */
function gridQs(
  g: GridOptions,
  extra: Record<string, string | undefined>,
): string {
  const p = new URLSearchParams();
  for (const f of g.filters ?? []) p.append("filter", JSON.stringify(f));
  if (g.where?.trim()) p.set("where", g.where.trim());
  for (const o of g.order ?? []) p.append("order", JSON.stringify(o));
  if (g.sort) {
    p.set("sort", g.sort);
    if (g.desc) p.set("desc", "true");
  }
  for (const [k, v] of Object.entries(extra)) if (v !== undefined) p.set(k, v);
  const s = p.toString();
  return s ? `?${s}` : "";
}
export type BillingAccount = S["BillingAccount"];
export type BillingContact = S["BillingContact"];
export type BillingForecast = S["BillingForecast"];
export type BillingSettings = S["BillingSettings"];
export type PlanChange = S["PlanChange"];
export type PlanOption = S["PlanOption"];
export type PlanChangeRequest = S["PlanChangeRequest"];
/** The generated schema still says NGN-only; a price book may be in USD
 * (cents) and the public pricing may carry the indicative rate. */
export type Prices = Omit<S["Prices"], "currency"> & { currency: string };
export type PriceBook = Omit<S["PriceBook"], "prices"> & { prices: Prices };
export type Pricing = Omit<S["Pricing"], "prices" | "next"> & {
  prices: Prices;
  next?: { version: number; effective_at: string; prices: Prices };
  fx?: FXQuote | null;
};
export type Invoice = S["Invoice"];
export type InvoiceDetail = S["InvoiceDetail"];
export type InvoiceLine = S["InvoiceLine"];
export type CreditNote = S["CreditNote"];
export type LedgerCheck = S["LedgerCheck"];
export type EstimateRequest = {
  cpus?: number;
  memory_mb?: number;
  disk_gb?: number;
  ha?: boolean;
  synchronous?: boolean;
  standby_only?: boolean;
  pitr_days?: 7 | 14 | 30;
  backup_retention?: S["BackupRetention"];
  region?: string;
};
export type FXQuote = S["FXQuote"];
export type Wallet = S["Wallet"];
export type WalletSetup = S["WalletSetup"];
export type WalletCharge = S["WalletCharge"];
export type DebitConsent = S["DebitConsent"];
export type Payment = S["Payment"];
export type VirtualAccount = S["VirtualAccount"];
export type PaymentEvent = S["PaymentEvent"];
export type OutstandingWht = S["OutstandingWht"];
export type Reconciliation = S["Reconciliation"];
export type CostEstimate = {
  hourly_minor: number;
  monthly_minor: number;
  /** The same in cents, for a dollar price book. */
  monthly_usd_minor?: number | null;
  fx?: FXQuote | null;
  lines: InvoiceLine[];
};
export type PricePreview = {
  period: string;
  current_total_minor: number;
  projected_total_minor: number;
  items: {
    org_id: string;
    org_name: string;
    plan: string;
    current_minor: number;
    projected_minor: number;
  }[];
};
export type Ticket = S["Ticket"];
export type TicketDetail = S["TicketDetail"];
export type TicketMessage = S["TicketMessage"];
export type TicketUpdate = S["TicketUpdate"];
export type SupportContext = S["SupportContext"];
export type SupportPhone = S["SupportPhone"];
export type SupportStaff = {
  id: string;
  email: string;
  role: string;
  name?: string;
};
export type TicketQueue = {
  items: Ticket[];
  open: number;
  pending: number;
  overdue: number;
};
export type Revenue = S["Revenue"];
export type RevenueMonth = S["RevenueMonth"];
export type LegalDocument = S["LegalDocument"];
export type OrgLegal = S["OrgLegal"];
export type LegalVersion = S["LegalVersion"];
export type LegalDocumentDetail = S["LegalDocumentDetail"];
export type Capacity = S["Capacity"];
export type CapacitySettings = S["CapacitySettings"];
export type CapacityProposal = S["CapacityProposal"];
export type Region = S["Region"];
export type InsightQuery = S["InsightQuery"];
export type InsightQueryDetail = S["InsightQueryDetail"];
export type InsightPlan = S["InsightPlan"];
export type IndexReport = S["IndexReport"];
export type IndexSuggestion = S["IndexSuggestion"];
export type InsightRange = "1h" | "24h" | "7d" | "30d";
export type InsightSort = "total" | "mean" | "calls" | "rows";
export type AdminRegion = S["AdminRegion"];
export type AdminRegionList = S["AdminRegionList"];
export type AdminRegionRequest = S["AdminRegionRequest"];
export type RegionReadiness = S["RegionReadiness"];
export type ProjectResidencyResult = S["ProjectResidencyResult"];
export type ManageRoles = S["ManageRoles"];
export type ProjectNetwork = S["ProjectNetwork"];
export type RebalanceMove = S["RebalanceMove"];
export type Margins = S["Margins"];
export type CostSettings = S["CostSettings"];
export type FXRate = S["FXRate"];
export type ServerPrice = S["ServerPrice"];
export type APIToken = S["APIToken"];
export type Incident = S["Incident"];
export type IncidentSeverity = S["IncidentSeverity"];
export type IncidentStatus = S["IncidentStatus"];
export type PoolerHosts = S["PoolerHosts"];
export type TokenScope = S["TokenScope"];
export type CreatedToken = S["CreatedToken"];
export type DeviceRequest = S["DeviceRequest"];
export type OAuthAuthorizeInfo = S["OAuthAuthorizeInfo"];
export type OAuthClient = S["OAuthClient"];
export type ApiErrorBody = S["Error"];
export type SessionState = S["SessionState"];
export type Project = S["Project"];
export type ProjectSettings = S["ProjectSettings"];
export type ProjectCredentials = S["ProjectCredentials"];
export type ProjectUpdated = S["ProjectUpdated"];
export type Operation = S["Operation"];
export type OperationLogEntry = S["OperationLogEntry"];
export type AuditEntry = S["AuditEntry"];
export type AuditList = S["AuditList"];
export type GeneralSettings = S["GeneralSettings"];
export type DnsCheck = S["DnsCheck"];
export type SetupEnrollment = S["SetupEnrollment"];
export type LoginChallenge = S["LoginChallenge"];
export type Backup = S["Backup"];
export type BackupOverview = S["BackupOverview"];
export type BackupKeyInfo = S["BackupKeyInfo"];
export type BackupKeyExport = S["BackupKeyExport"];
export type StorageRequest = S["StorageRequest"];
export type StorageSettings = S["StorageSettings"];
export type StorageTarget = S["StorageTarget"];
export type ProjectBackupStorage = S["ProjectBackupStorage"];
export type StorageTestResult = S["StorageTestResult"];
export type RestoreResponse = S["RestoreResponse"];
export type ImportPreflight = S["ImportPreflight"];
export type Node = S["Node"];
export type RegistrationToken = S["RegistrationToken"];
export type NodeDetail = S["NodeDetail"];
export type NodeCreated = S["NodeCreated"];
export type ProfileList = S["ProfileList"];
export type InstanceSummary = S["InstanceSummary"];
export type UpgradePreflight = S["UpgradePreflight"];
export type Move = S["Move"];
export type MaintenanceStatus = S["MaintenanceStatus"];
export type MaintenanceWindow = S["MaintenanceWindow"];
export type MinorUpgrade = S["MinorUpgrade"];
export type HAStatus = S["HAStatus"];
export type ReplicaList = S["ReplicaList"];
export type ReadReplica = S["ReadReplica"];
export type EtcdCluster = S["EtcdCluster"];
export type CreateProjectRequest = S["CreateProjectRequest"];
export type SqlResult = S["SqlResult"];
export type SqlStatementResult = S["SqlStatementResult"];
export type DbSchema = S["DbSchema"];
export type DbTable = S["DbTable"];
export type TablePage = S["TablePage"];
export type Extension = S["Extension"];
export type MetricsResponse = S["MetricsResponse"];
export type MetricRange = "1h" | "24h" | "7d";
export type AlertItem = S["Alert"];
export type AlertSettings = S["AlertSettings"];
export type AlertSettingsRequest = S["AlertSettingsRequest"];
export type IsolationCheck = S["IsolationCheck"];
export type User = S["User"];
export type Org = S["Org"];
export type OrgIncident = S["OrgIncident"];
export type OrgRole = S["OrgRole"];
export type ProjectRole = S["ProjectRole"];
export type OrgMember = S["OrgMember"];
export type Invitation = S["Invitation"];
export type InvitationCreated = S["InvitationCreated"];
export type InvitationPreview = S["InvitationPreview"];
export type ProjectMember = S["ProjectMember"];
export type PersonalCredentials = S["PersonalCredentials"];
export type PersonalCredentialsInfo = S["PersonalCredentialsInfo"];
export type AdminUser = S["AdminUser"];
export type MailSettings = S["MailSettings"];
export type MailSettingsRequest = S["MailSettingsRequest"];
export type SignupSettings = S["SignupSettings"];
export type Terms = S["Terms"];
export type SessionInfo = S["SessionInfo"];
export type OrgQuotas = S["OrgQuotas"];
export type QuotaItem = S["QuotaItem"];
export type UsageReport = S["UsageReport"];
export type UsageRecord = S["UsageRecord"];
export type DedicatedRequest = S["DedicatedRequest"];
export type DemotePreflight = S["DemotePreflight"];
export type Webhook = S["Webhook"];
export type WebhookRequest = S["WebhookRequest"];
export type WebhookDelivery = S["WebhookDelivery"];
export type Job = S["Job"];
export type JobRequest = S["JobRequest"];
export type JobRun = S["JobRun"];
export type DemoteRequest = S["DemoteRequest"];
export type BreakGlassSession = S["BreakGlassSession"];
export type ProjectStorage = S["ProjectStorage"];
export type StorageState = S["StorageState"];
export type ReapedSession = S["ReapedSession"];
export type SwitchedCredentials = S["SwitchedCredentials"];
export type AdminOrg = S["AdminOrg"];
export type AdminOrgSummary = S["AdminOrgSummary"];
export type Plan = S["Plan"];
export type SharedCluster = S["SharedCluster"];
export type PlatformUsage = S["PlatformUsage"];
export type AuditQuery = {
  action?: string;
  outcome?: string;
  target_id?: string;
  before?: number;
  limit?: number;
};

/** An error response from the API, with the server's error code. */
export class ApiRequestError extends Error {
  constructor(
    readonly status: number,
    readonly body: ApiErrorBody | undefined,
  ) {
    super(body?.message ?? `request failed with status ${status}`);
  }

  get code(): string | undefined {
    return this.body?.code;
  }
}

let csrfToken = "";

/** Remembers the CSRF token from /api/v1/session for mutating requests. */
export function setCsrfToken(token: string) {
  csrfToken = token;
}

/**
 * The CSRF cookie as the browser holds it now. Concurrent first requests can
 * each be issued a token, and the cookie keeps the last one, so it wins over
 * the remembered token.
 */
function cookieToken(): string {
  if (typeof document === "undefined") return "";
  const m = /(?:^|;\s*)(?:__Host-)?xenibase_csrf=([^;]+)/.exec(document.cookie);
  return m ? decodeURIComponent(m[1]) : "";
}

type Method = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

let reauthHandler: (() => Promise<boolean>) | null = null;

/**
 * Registers what to do when the server wants a fresh step-up (password and
 * code) before a sensitive action: resolve true once the user has confirmed,
 * and the request is sent again.
 */
export function setReauthHandler(h: (() => Promise<boolean>) | null) {
  reauthHandler = h;
}

export async function request<T>(
  method: Method,
  path: string,
  body?: unknown,
  fetchFn: typeof fetch = fetch,
): Promise<T> {
  try {
    return await send<T>(method, path, body, fetchFn);
  } catch (e) {
    if (
      e instanceof ApiRequestError &&
      e.code === "reauth_required" &&
      reauthHandler &&
      path !== "/api/v1/auth/reauth"
    ) {
      if (await reauthHandler()) return send<T>(method, path, body, fetchFn);
    }
    throw e;
  }
}

async function send<T>(
  method: Method,
  path: string,
  body: unknown,
  fetchFn: typeof fetch,
): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") {
    let token = cookieToken() || csrfToken;
    // A page that writes on load (an invitation or email link) may get here
    // before anything has handed out a CSRF token.
    if (!token) {
      const s = await request<{ csrf_token: string }>(
        "GET",
        "/api/v1/session",
        undefined,
        fetchFn,
      );
      csrfToken ||= s.csrf_token;
      token = cookieToken() || csrfToken;
    }
    headers["X-CSRF-Token"] = token;
  }
  const res = await fetchFn(path, {
    method,
    headers,
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    let err: ApiErrorBody | undefined;
    try {
      err = (await res.json()) as ApiErrorBody;
    } catch {
      err = undefined;
    }
    throw new ApiRequestError(res.status, err);
  }
  // Some successes carry no body: 204, and 202 for requests that answer the
  // same whatever happened (sign-up, password reset).
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  return (text === "" ? undefined : JSON.parse(text)) as T;
}

/**
 * Sends a file as the raw body: POSTed as octet-stream by default (billing
 * documents), or PUT with the file's own type (storage objects).
 */
export async function uploadFile(
  path: string,
  file: Blob,
  method: "POST" | "PUT" = "POST",
  contentType = "application/octet-stream",
): Promise<unknown> {
  let token = cookieToken() || csrfToken;
  if (!token) {
    const s = await request<{ csrf_token: string }>("GET", "/api/v1/session");
    csrfToken ||= s.csrf_token;
    token = cookieToken() || csrfToken;
  }
  const res = await fetch(path, {
    method,
    headers: {
      Accept: "application/json",
      "Content-Type": contentType,
      "X-CSRF-Token": token,
    },
    credentials: "same-origin",
    body: file,
  });
  if (!res.ok) {
    let err: ApiErrorBody | undefined;
    try {
      err = (await res.json()) as ApiErrorBody;
    } catch {
      err = undefined;
    }
    throw new ApiRequestError(res.status, err);
  }
  const text = await res.text();
  return text === "" ? undefined : JSON.parse(text);
}

export function getJSON<T>(path: string, fetchFn?: typeof fetch): Promise<T> {
  return request<T>("GET", path, undefined, fetchFn);
}

export function getVersion(fetchFn?: typeof fetch): Promise<Version> {
  return getJSON<Version>("/api/v1/version", fetchFn);
}

function qs(
  params: Record<string, string | number | undefined | null>,
): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== "") p.set(k, String(v));
  }
  const s = p.toString();
  return s ? `?${s}` : "";
}

export const api = {
  session: () => getJSON<SessionState>("/api/v1/session"),
  setupBegin: (b: { setup_code: string; email: string; password: string }) =>
    request<SetupEnrollment>("POST", "/api/v1/setup/begin", b),
  setupComplete: (b: { enrollment_token: string; code: string }) =>
    request<SessionState>("POST", "/api/v1/setup/complete", b),
  login: (b: { email: string; password: string }) =>
    request<LoginChallenge>("POST", "/api/v1/auth/login", b),
  totp: (b: { challenge_id: string; code: string }) =>
    request<SessionState>("POST", "/api/v1/auth/totp", b),
  reauth: (b: { password: string; code?: string }) =>
    request<void>("POST", "/api/v1/auth/reauth", b),
  totpStatus: () => getJSON<S["TotpStatus"]>("/api/v1/me/totp"),
  setupTotp: (b: { password: string }) =>
    request<S["TotpSetup"]>("POST", "/api/v1/me/totp/setup", b),
  enableTotp: (b: { setup_token: string; code: string }) =>
    request<S["RecoveryCodes"]>("POST", "/api/v1/me/totp/enable", b),
  disableTotp: (b: { password: string; code: string }) =>
    request<void>("POST", "/api/v1/me/totp/disable", b),
  logout: () => request<void>("POST", "/api/v1/auth/logout"),

  signup: (b: S["SignupRequest"]) =>
    request<void>("POST", "/api/v1/auth/signup", b),
  verifyEmail: (token: string) =>
    request<S["VerifyEmailResult"]>("POST", "/api/v1/auth/verify-email", {
      token,
    }),
  resendVerification: (email: string) =>
    request<void>("POST", "/api/v1/auth/verify-email/resend", { email }),
  requestPasswordReset: (email: string) =>
    request<void>("POST", "/api/v1/auth/password-reset", { email }),
  confirmPasswordReset: (token: string, password: string) =>
    request<void>("POST", "/api/v1/auth/password-reset/confirm", {
      token,
      password,
    }),
  terms: () => getJSON<Terms>("/api/v1/terms"),
  acceptTerms: (version: number) =>
    request<void>("POST", "/api/v1/me/terms/accept", { version }),
  previewInvitation: (token: string) =>
    request<InvitationPreview>("POST", "/api/v1/invitations/preview", {
      token,
    }),
  acceptInvitation: (b: S["AcceptInvitationRequest"]) =>
    request<S["AcceptInvitationResult"]>(
      "POST",
      "/api/v1/invitations/accept",
      b,
    ),

  me: () => getJSON<User>("/api/v1/me"),
  updateMe: (b: S["UpdateMeRequest"]) =>
    request<User>("PATCH", "/api/v1/me", b),
  changePassword: (b: S["ChangePasswordRequest"]) =>
    request<void>("POST", "/api/v1/me/password", b),
  mySessions: () => getJSON<S["SessionList"]>("/api/v1/me/sessions"),
  revokeSession: (id: string) =>
    request<void>("DELETE", `/api/v1/me/sessions/${encodeURIComponent(id)}`),
  recoveryCodes: () =>
    getJSON<S["RecoveryCodesStatus"]>("/api/v1/me/recovery-codes"),
  regenerateRecoveryCodes: () =>
    request<S["RecoveryCodes"]>("POST", "/api/v1/me/recovery-codes"),
  myInvitations: () => getJSON<S["MyInvitationList"]>("/api/v1/me/invitations"),
  acceptMyInvitation: (id: string) =>
    request<S["AcceptInvitationResult"]>(
      "POST",
      `/api/v1/me/invitations/${id}/accept`,
    ),

  myTokens: () => getJSON<S["APITokenList"]>("/api/v1/tokens"),
  createToken: (b: S["CreateTokenRequest"]) =>
    request<CreatedToken>("POST", "/api/v1/tokens", b),
  revokeMyToken: (id: string) =>
    request<void>("DELETE", `/api/v1/tokens/${id}`),
  orgTokens: (org: string) =>
    getJSON<S["APITokenList"]>(`/api/v1/orgs/${org}/tokens`),
  revokeOrgToken: (org: string, id: string) =>
    request<void>("DELETE", `/api/v1/orgs/${org}/tokens/${id}`),
  deviceRequest: (code: string) =>
    getJSON<DeviceRequest>(
      `/api/v1/auth/device/requests/${encodeURIComponent(code)}`,
    ),
  approveDevice: (b: S["DeviceApproveRequest"]) =>
    request<APIToken | undefined>("POST", "/api/v1/auth/device/approve", b),
  oauthAuthorization: (p: OAuthAuthorizeSearch) =>
    getJSON<OAuthAuthorizeInfo>(`/api/v1/oauth/authorize${qs(p)}`),
  decideOAuth: (b: S["OAuthDecision"]) =>
    request<S["OAuthRedirect"]>("POST", "/api/v1/oauth/authorize", b),
  oauthClients: () =>
    getJSON<S["OAuthClientList"]>("/api/v1/admin/oauth/clients"),
  createOAuthClient: (b: S["CreateOAuthClientRequest"]) =>
    request<S["OAuthClientCreated"]>("POST", "/api/v1/admin/oauth/clients", b),
  disableOAuthClient: (clientId: string) =>
    request<void>(
      "DELETE",
      `/api/v1/admin/oauth/clients/${encodeURIComponent(clientId)}`,
    ),
  tokenSettings: () =>
    getJSON<S["TokenSettings"]>("/api/v1/admin/settings/tokens"),
  putTokenSettings: (b: S["TokenSettings"]) =>
    request<S["TokenSettings"]>("PUT", "/api/v1/admin/settings/tokens", b),

  orgs: () => getJSON<S["OrgList"]>("/api/v1/orgs"),
  createOrg: (name: string) => request<Org>("POST", "/api/v1/orgs", { name }),
  org: (id: string) => getJSON<Org>(`/api/v1/orgs/${id}`),
  updateOrg: (id: string, b: S["UpdateOrgRequest"]) =>
    request<Org>("PATCH", `/api/v1/orgs/${id}`, b),
  orgMembers: (id: string) =>
    getJSON<S["OrgMemberList"]>(`/api/v1/orgs/${id}/members`),
  inviteOrgMember: (id: string, b: S["InviteRequest"]) =>
    request<InvitationCreated>("POST", `/api/v1/orgs/${id}/members`, b),
  setOrgRole: (id: string, user: string, role: OrgRole) =>
    request<void>("PATCH", `/api/v1/orgs/${id}/members/${user}`, { role }),
  removeOrgMember: (id: string, user: string) =>
    request<void>("DELETE", `/api/v1/orgs/${id}/members/${user}`),
  leaveOrg: (id: string) => request<void>("POST", `/api/v1/orgs/${id}/leave`),
  transferOwnership: (id: string, user_id: string) =>
    request<void>("POST", `/api/v1/orgs/${id}/transfer-ownership`, { user_id }),
  orgInvitations: (id: string) =>
    getJSON<S["InvitationList"]>(`/api/v1/orgs/${id}/invitations`),
  revokeOrgInvitation: (id: string, inv: string) =>
    request<void>("DELETE", `/api/v1/orgs/${id}/invitations/${inv}`),
  orgAudit: (id: string, p: AuditQuery = {}) =>
    getJSON<AuditList>(`/api/v1/orgs/${id}/audit${qs(p)}`),
  orgQuotas: (id: string) => getJSON<OrgQuotas>(`/api/v1/orgs/${id}/quotas`),
  orgUsage: (
    id: string,
    p: { from?: string; to?: string; metric?: string } = {},
  ) => getJSON<UsageReport>(`/api/v1/orgs/${id}/usage${qs(p)}`),
  orgUsageCsvUrl: (id: string, p: { from?: string; to?: string } = {}) =>
    `/api/v1/orgs/${id}/usage${qs({ ...p, format: "csv" })}`,
  orgDedicatedRequests: (id: string) =>
    getJSON<S["DedicatedRequestList"]>(`/api/v1/orgs/${id}/dedicated-requests`),
  deleteOrg: (id: string, b: S["DeleteOrgRequest"]) =>
    request<S["OrgDeletion"]>("DELETE", `/api/v1/orgs/${id}`, b),
  cancelOrgDeletion: (id: string) =>
    request<void>("POST", `/api/v1/orgs/${id}/cancel-deletion`),
  endBreakGlass: (id: string, session: string) =>
    request<void>("POST", `/api/v1/orgs/${id}/break-glass/${session}/end`),

  projectMembers: (id: string) =>
    getJSON<S["ProjectMemberList"]>(`/api/v1/projects/${id}/members`),
  addProjectMember: (id: string, b: S["ProjectMemberRequest"]) =>
    request<S["ProjectMemberAdded"]>(
      "POST",
      `/api/v1/projects/${id}/members`,
      b,
    ),
  setProjectRole: (id: string, user: string, role: ProjectRole) =>
    request<void>("PATCH", `/api/v1/projects/${id}/members/${user}`, { role }),
  removeProjectMember: (id: string, user: string) =>
    request<void>("DELETE", `/api/v1/projects/${id}/members/${user}`),
  myCredentials: (id: string) =>
    getJSON<PersonalCredentialsInfo>(`/api/v1/projects/${id}/credentials`),
  issueMyCredentials: (id: string) =>
    request<PersonalCredentials>("POST", `/api/v1/projects/${id}/credentials`),
  transferProject: (id: string, org_id: string) =>
    request<Project>("POST", `/api/v1/projects/${id}/transfer`, { org_id }),
  projectAudit: (id: string, p: AuditQuery = {}) =>
    getJSON<AuditList>(`/api/v1/projects/${id}/audit${qs(p)}`),
  projectStorage: (id: string) =>
    getJSON<ProjectStorage>(`/api/v1/projects/${id}/storage`),
  reclaimSpace: (id: string, schema: string, table: string) =>
    request<Operation>("POST", `/api/v1/projects/${id}/reclaim-space`, {
      schema,
      table,
    }),
  reapedSessions: (id: string) =>
    getJSON<S["ReapedSessionList"]>(`/api/v1/projects/${id}/reaped`),
  switchCredentials: (id: string, grace_days: number) =>
    request<SwitchedCredentials>(
      "POST",
      `/api/v1/projects/${id}/switch-credentials`,
      { grace_days },
    ),

  users: (p: { q?: string; pending?: boolean } = {}) =>
    getJSON<S["UserList"]>(
      `/api/v1/admin/users${qs({ q: p.q, pending: p.pending ? "true" : undefined })}`,
    ),
  updateUser: (id: string, b: S["UpdateUserRequest"]) =>
    request<AdminUser>("PATCH", `/api/v1/admin/users/${id}`, b),
  resetUserTotp: (id: string) =>
    request<void>("POST", `/api/v1/admin/users/${id}/reset-2fa`),
  platformInvitations: () =>
    getJSON<S["InvitationList"]>("/api/v1/admin/invitations"),
  invitePlatform: (email: string) =>
    request<InvitationCreated>("POST", "/api/v1/admin/invitations", { email }),
  revokePlatformInvitation: (id: string) =>
    request<void>("DELETE", `/api/v1/admin/invitations/${id}`),
  signupSettings: () =>
    getJSON<SignupSettings>("/api/v1/admin/settings/signup"),
  saveSignupSettings: (b: SignupSettings) =>
    request<SignupSettings>("PUT", "/api/v1/admin/settings/signup", b),
  mailSettings: () => getJSON<MailSettings>("/api/v1/admin/settings/mail"),
  saveMailSettings: (b: MailSettingsRequest) =>
    request<MailSettings>("PUT", "/api/v1/admin/settings/mail", b),
  publishTerms: (b: S["PublishTermsRequest"]) =>
    request<Terms>("POST", "/api/v1/admin/settings/terms", b),
  platformAudit: (p: AuditQuery = {}) =>
    getJSON<AuditList>(`/api/v1/admin/audit${qs(p)}`),
  adminOrgs: (q?: string) =>
    getJSON<S["AdminOrgList"]>(`/api/v1/admin/orgs${qs({ q })}`),
  adminOrg: (id: string) => getJSON<AdminOrg>(`/api/v1/admin/orgs/${id}`),
  adminUpdateOrg: (id: string, b: S["AdminUpdateOrgRequest"]) =>
    request<AdminOrg>("PATCH", `/api/v1/admin/orgs/${id}`, b),
  suspendOrg: (id: string, reason: string) =>
    request<void>("POST", `/api/v1/admin/orgs/${id}/suspend`, { reason }),
  reinstateOrg: (id: string) =>
    request<void>("POST", `/api/v1/admin/orgs/${id}/reinstate`),
  setOrgCluster: (id: string, instance_id: string, reserved: boolean) =>
    request<AdminOrg>("POST", `/api/v1/admin/orgs/${id}/cluster`, {
      instance_id,
      reserved,
    }),
  startBreakGlass: (id: string, reason: string, duration_minutes: number) =>
    request<BreakGlassSession>("POST", `/api/v1/admin/orgs/${id}/break-glass`, {
      reason,
      duration_minutes,
    }),
  plans: () => getJSON<S["PlanList"]>("/api/v1/admin/plans"),
  createPlan: (b: S["PlanRequest"]) =>
    request<Plan>("POST", "/api/v1/admin/plans", b),
  updatePlan: (id: string, b: S["PlanRequest"]) =>
    request<Plan>("PATCH", `/api/v1/admin/plans/${id}`, b),
  dedicatedRequests: (status?: string) =>
    getJSON<S["DedicatedRequestList"]>(
      `/api/v1/admin/dedicated-requests${qs({ status })}`,
    ),
  approveDedicatedRequest: (id: string, b: S["DecideRequest"]) =>
    request<Operation>(
      "POST",
      `/api/v1/admin/dedicated-requests/${id}/approve`,
      b,
    ),
  rejectDedicatedRequest: (id: string, b: S["DecideRequest"]) =>
    request<void>("POST", `/api/v1/admin/dedicated-requests/${id}/reject`, b),
  platformUsage: (p: { from?: string; to?: string } = {}) =>
    getJSON<PlatformUsage>(`/api/v1/admin/usage${qs(p)}`),
  sharedClusters: () =>
    getJSON<S["SharedClusterList"]>("/api/v1/admin/shared-clusters"),

  projects: (org?: string, status?: string) =>
    getJSON<S["ProjectList"]>(
      `/api/v1/projects${qs({ org, status, limit: 500 })}`,
    ),
  project: (id: string) => getJSON<Project>(`/api/v1/projects/${id}`),
  createProject: (b: CreateProjectRequest) =>
    request<ProjectCredentials>("POST", "/api/v1/projects", b),
  profiles: () => getJSON<ProfileList>("/api/v1/profiles"),
  promotionEstimate: (id: string) =>
    getJSON<S["PromotionEstimate"]>(`/api/v1/projects/${id}/promote`),
  /** An operation, or a dedicated request when beyond the org's allowance (V2 §10.6). */
  promote: (id: string, b: S["PromoteRequest"]) =>
    request<Operation | DedicatedRequest>(
      "POST",
      `/api/v1/projects/${id}/promote`,
      b,
    ),
  demotePreflight: (id: string, b: DemoteRequest) =>
    request<DemotePreflight>(
      "POST",
      `/api/v1/projects/${id}/demote/preflight`,
      b,
    ),
  demote: (id: string, b: DemoteRequest) =>
    request<Operation>("POST", `/api/v1/projects/${id}/demote`, b),
  upgradePreflight: (id: string, pg_version: number) =>
    request<UpgradePreflight>(
      "POST",
      `/api/v1/projects/${id}/upgrade/preflight`,
      { pg_version },
    ),
  upgrade: (id: string, pg_version: number) =>
    request<Operation>("POST", `/api/v1/projects/${id}/upgrade`, {
      pg_version,
    }),
  projectMoves: (id: string) =>
    getJSON<S["MoveList"]>(`/api/v1/projects/${id}/moves`),
  /** Platform admin: move a project to another node (V3 §2.3). */
  moveProject: (id: string, b: S["MoveProjectRequest"]) =>
    request<Operation>("POST", `/api/v1/admin/projects/${id}/move`, b),
  projectHA: (id: string) => getJSON<HAStatus>(`/api/v1/projects/${id}/ha`),
  /** A step of the Supabase migration helper (V4 §9). */
  migrateSupabase: (id: string, b: S["SupabaseMigrationRequest"]) =>
    request<Operation>("POST", `/api/v1/projects/${id}/migrate/supabase`, b),
  /** A dedicated project's read replicas (V4 §7). */
  replicas: (id: string) =>
    getJSON<ReplicaList>(`/api/v1/projects/${id}/replicas`),
  createReplica: (id: string, b: S["ReplicaCreateRequest"]) =>
    request<Operation>("POST", `/api/v1/projects/${id}/replicas`, b),
  updateReplica: (id: string, replica: string, b: S["ReplicaSettings"]) =>
    request<ReadReplica>(
      "PATCH",
      `/api/v1/projects/${id}/replicas/${replica}`,
      b,
    ),
  deleteReplica: (id: string, replica: string) =>
    request<Operation>("DELETE", `/api/v1/projects/${id}/replicas/${replica}`),
  detachReplica: (id: string, replica: string, name: string) =>
    request<ProjectCredentials>(
      "POST",
      `/api/v1/projects/${id}/replicas/${replica}/detach`,
      { name },
    ),
  enableHA: (id: string, b: S["HAEnableRequest"]) =>
    request<Operation>("POST", `/api/v1/projects/${id}/ha`, b),
  updateHA: (id: string, synchronous: boolean) =>
    request<HAStatus>("PATCH", `/api/v1/projects/${id}/ha`, { synchronous }),
  disableHA: (id: string) =>
    request<Operation>("DELETE", `/api/v1/projects/${id}/ha`),
  switchover: (id: string, candidate?: string) =>
    request<Operation>("POST", `/api/v1/projects/${id}/switchover`, {
      candidate,
    }),
  etcd: (region?: string) =>
    getJSON<EtcdCluster>(`/api/v1/admin/etcd${qs({ region })}`),
  replaceEtcdMember: (node: string, to?: string) =>
    request<Operation>(
      "POST",
      `/api/v1/admin/etcd/members/${node}/replace`,
      to ? { node_id: to } : {},
    ),
  moveProjectEtcd: (id: string) =>
    request<Operation>("POST", `/api/v1/projects/${id}/ha/etcd-move`),
  setupEtcd: (node_ids: string[]) =>
    request<Operation>("POST", "/api/v1/admin/etcd", { node_ids }),
  maintenance: () => getJSON<MaintenanceStatus>("/api/v1/admin/maintenance"),
  putMaintenanceWindow: (b: MaintenanceWindow) =>
    request<MaintenanceWindow>("PUT", "/api/v1/admin/maintenance/window", b),
  minorUpgrade: (instanceId: string) =>
    request<MinorUpgrade>(
      "POST",
      `/api/v1/admin/instances/${instanceId}/minor-upgrade`,
    ),
  sql: (id: string, b: S["SqlRequest"]) =>
    request<SqlResult>("POST", `/api/v1/projects/${id}/sql`, b),
  cancelSql: (id: string, query_id: string) =>
    request<S["SqlCancelResult"]>("POST", `/api/v1/projects/${id}/sql/cancel`, {
      query_id,
    }),
  schema: (id: string) => getJSON<DbSchema>(`/api/v1/projects/${id}/schema`),
  tableRows: (
    id: string,
    schema: string,
    table: string,
    after?: string,
    grid: GridOptions = {},
  ) =>
    getJSON<TablePage>(
      `${tableBase(id, schema, table)}/rows${gridQs(grid, { after })}`,
    ),
  tableInfo: (id: string, schema: string, table: string) =>
    getJSON<TableInfo>(tableBase(id, schema, table)),
  /** A numbered page (offset paging) of up to 1,000 rows. */
  tablePage: (
    id: string,
    schema: string,
    table: string,
    grid: GridOptions,
    offset: number,
    limit: number,
  ) =>
    getJSON<TablePage>(
      `${tableBase(id, schema, table)}/rows${gridQs(grid, { offset: String(offset), limit: String(limit) })}`,
    ),
  tableCount: (
    id: string,
    schema: string,
    table: string,
    grid: GridOptions = {},
  ) =>
    getJSON<RowCount>(
      `${tableBase(id, schema, table)}/count${gridQs({ filters: grid.filters, where: grid.where }, {})}`,
    ),
  tableDefinition: (id: string, schema: string, table: string) =>
    getJSON<S["TableDefinition"]>(`${tableBase(id, schema, table)}/definition`),
  exportUrl: (
    id: string,
    schema: string,
    table: string,
    format: "csv" | "json",
    grid: GridOptions = {},
  ) => `${tableBase(id, schema, table)}/export${gridQs(grid, { format })}`,
  saveRows: (id: string, schema: string, table: string, changes: RowChange[]) =>
    request<SaveRowsResult>("POST", `${tableBase(id, schema, table)}/changes`, {
      changes,
    }),
  previewSchema: (id: string, change: SchemaChange) =>
    request<SchemaPlan>("POST", `/api/v1/projects/${id}/schema/preview`, {
      change,
    }),
  applySchema: (
    id: string,
    change: SchemaChange,
    hash: string,
    confirm?: string,
  ) =>
    request<S["SchemaApplied"]>("POST", `/api/v1/projects/${id}/schema/apply`, {
      change,
      hash,
      confirm,
    }),
  schemaMigration: (
    id: string,
    change: SchemaChange,
    format: MigrationFormat,
  ) =>
    request<S["SchemaMigration"]>(
      "POST",
      `/api/v1/projects/${id}/schema/migration`,
      { change, format },
    ),
  savedQueries: (id: string) =>
    getJSON<S["SavedQueryList"]>(`/api/v1/projects/${id}/queries`),
  createSavedQuery: (id: string, b: S["SavedQueryRequest"]) =>
    request<SavedQuery>("POST", `/api/v1/projects/${id}/queries`, b),
  updateSavedQuery: (id: string, queryId: string, b: S["SavedQueryPatch"]) =>
    request<SavedQuery>(
      "PATCH",
      `/api/v1/projects/${id}/queries/${queryId}`,
      b,
    ),
  deleteSavedQuery: (id: string, queryId: string) =>
    request<void>("DELETE", `/api/v1/projects/${id}/queries/${queryId}`),
  favoriteSavedQuery: (id: string, queryId: string, favorite: boolean) =>
    request<SavedQuery>(
      "PUT",
      `/api/v1/projects/${id}/queries/${queryId}/favorite`,
      { favorite },
    ),
  editorPreferences: (id: string) =>
    getJSON<S["EditorPreferences"]>(
      `/api/v1/projects/${id}/editor-preferences`,
    ),
  extensions: (id: string) =>
    getJSON<S["ExtensionList"]>(`/api/v1/projects/${id}/extensions`),
  enableExtension: (id: string, name: string) =>
    request<S["ExtensionList"]>("POST", `/api/v1/projects/${id}/extensions`, {
      name,
    }),
  projectMetrics: (id: string, range: MetricRange) =>
    getJSON<MetricsResponse>(`/api/v1/projects/${id}/metrics${qs({ range })}`),
  nodeMetrics: (id: string, range: MetricRange) =>
    getJSON<MetricsResponse>(`/api/v1/nodes/${id}/metrics${qs({ range })}`),
  pitr: (id: string, b: S["PitrRequest"]) =>
    request<ProjectCredentials>("POST", `/api/v1/projects/${id}/pitr`, b),
  instanceAction: (id: string, action: "start" | "stop" | "restart") =>
    request<S["InstanceState"]>("POST", `/api/v1/projects/${id}/instance`, {
      action,
    }),
  adminPgVersions: () =>
    getJSON<{ items: S["PgVersionInfo"][] }>("/api/v1/admin/pg-versions"),
  updatePgVersion: (major: number, b: S["PgVersionUpdate"]) =>
    request<S["PgVersionInfo"]>(
      "PATCH",
      `/api/v1/admin/pg-versions/${major}`,
      b,
    ),
  updateProjectInstance: (id: string, b: S["InstanceUpdate"]) =>
    request<S["InstanceUpdated"]>(
      "PATCH",
      `/api/v1/projects/${id}/instance`,
      b,
    ),
  updateProject: (id: string, b: S["UpdateProjectRequest"]) =>
    request<ProjectUpdated>("PATCH", `/api/v1/projects/${id}/settings`, b),
  resumeProject: (id: string) =>
    request<Operation>("POST", `/api/v1/projects/${id}/resume`),
  pricing: () => getJSON<Pricing>("/api/v1/pricing"),
  rotatePassword: (id: string) =>
    request<ProjectCredentials>(
      "POST",
      `/api/v1/projects/${id}/rotate-password`,
    ),
  branches: (id: string) =>
    getJSON<S["ProjectList"]>(`/api/v1/projects/${id}/branches`),
  webhooks: (id: string) =>
    getJSON<S["WebhookList"]>(`/api/v1/projects/${id}/webhooks`),
  createWebhook: (id: string, b: WebhookRequest) =>
    request<S["WebhookCreated"]>("POST", `/api/v1/projects/${id}/webhooks`, b),
  updateWebhook: (id: string, wid: string, b: S["WebhookUpdate"]) =>
    request<Webhook>("PATCH", `/api/v1/projects/${id}/webhooks/${wid}`, b),
  deleteWebhook: (id: string, wid: string) =>
    request<void>("DELETE", `/api/v1/projects/${id}/webhooks/${wid}`),
  testWebhook: (id: string, wid: string) =>
    request<S["WebhookTestResult"]>(
      "POST",
      `/api/v1/projects/${id}/webhooks/${wid}/test`,
    ),
  rotateWebhookSecret: (id: string, wid: string) =>
    request<S["WebhookSecret"]>(
      "POST",
      `/api/v1/projects/${id}/webhooks/${wid}/rotate-secret`,
    ),
  webhookDeliveries: (id: string, wid: string, dead = false) =>
    getJSON<S["WebhookDeliveryList"]>(
      `/api/v1/projects/${id}/webhooks/${wid}/deliveries?limit=100${dead ? "&dead=true" : ""}`,
    ),
  replayWebhook: (id: string, wid: string, b: S["ReplayRequest"]) =>
    request<S["ReplayResult"]>(
      "POST",
      `/api/v1/projects/${id}/webhooks/${wid}/replay`,
      b,
    ),
  jobs: (id: string) => getJSON<S["JobList"]>(`/api/v1/projects/${id}/jobs`),
  createJob: (id: string, b: JobRequest) =>
    request<S["JobCreated"]>("POST", `/api/v1/projects/${id}/jobs`, b),
  updateJob: (id: string, jid: string, b: S["JobUpdate"]) =>
    request<Job>("PATCH", `/api/v1/projects/${id}/jobs/${jid}`, b),
  deleteJob: (id: string, jid: string) =>
    request<void>("DELETE", `/api/v1/projects/${id}/jobs/${jid}`),
  runJob: (id: string, jid: string) =>
    request<JobRun>("POST", `/api/v1/projects/${id}/jobs/${jid}/run`),
  jobRuns: (id: string, jid: string) =>
    getJSON<S["JobRunList"]>(
      `/api/v1/projects/${id}/jobs/${jid}/runs?limit=50`,
    ),
  orgOutbound: (org: string) =>
    getJSON<S["OrgOutbound"]>(`/api/v1/admin/orgs/${org}/outbound`),
  setOrgOutbound: (org: string, hosts: string[]) =>
    request<S["OrgOutbound"]>("PUT", `/api/v1/admin/orgs/${org}/outbound`, {
      hosts,
    }),
  createBranch: (id: string, b: S["BranchRequest"]) =>
    request<ProjectCredentials>("POST", `/api/v1/projects/${id}/branches`, b),
  resetBranch: (id: string, b: S["BranchResetRequest"] = {}) =>
    request<Operation>("POST", `/api/v1/projects/${id}/reset`, b),
  detachBranch: (id: string) =>
    request<Project>("POST", `/api/v1/projects/${id}/detach`),
  deleteProject: (id: string, confirm: string, skipFinalBackup = false) =>
    request<Operation>(
      "DELETE",
      `/api/v1/projects/${id}${qs({ confirm, skip_final_backup: skipFinalBackup ? "true" : undefined })}`,
    ),

  backups: (
    p: {
      org?: string;
      project_id?: string;
      kind?: string;
      limit?: number;
    } = {},
  ) => getJSON<S["BackupList"]>(`/api/v1/backups${qs(p)}`),
  backupOverview: (org?: string) =>
    getJSON<BackupOverview>(`/api/v1/backups/overview${qs({ org })}`),
  backupNow: (projectId: string) =>
    request<Operation>("POST", `/api/v1/projects/${projectId}/backups`),
  restore: (backupId: string, b: S["RestoreRequest"]) =>
    request<RestoreResponse>("POST", `/api/v1/backups/${backupId}/restore`, b),
  restoreTest: (projectId?: string) =>
    request<Operation>(
      "POST",
      `/api/v1/restore-tests${qs({ project_id: projectId })}`,
    ),

  storage: () => getJSON<StorageSettings>("/api/v1/settings/storage"),
  saveStorage: (b: StorageRequest) =>
    request<StorageTestResult>("PUT", "/api/v1/settings/storage", b),
  testStorage: (b: StorageRequest) =>
    request<StorageTestResult>("POST", "/api/v1/settings/storage/test", b),
  backupKey: () => getJSON<BackupKeyInfo>("/api/v1/settings/backup-key"),
  generateBackupKey: () =>
    request<BackupKeyExport>("POST", "/api/v1/settings/backup-key"),
  exportBackupKey: () =>
    request<BackupKeyExport>("POST", "/api/v1/settings/backup-key/export"),
  confirmBackupKey: (key: string) =>
    request<BackupKeyInfo>("POST", "/api/v1/settings/backup-key/confirm", {
      key,
    }),

  // Storage targets (V2 §6). org undefined: platform targets (platform admin).
  storageTargets: (org?: string) =>
    getJSON<S["StorageTargetList"]>(
      org
        ? `/api/v1/orgs/${org}/storage-targets`
        : "/api/v1/admin/storage-targets",
    ),
  createStorageTarget: (
    org: string | undefined,
    b: S["StorageTargetRequest"],
  ) =>
    request<S["StorageTargetSaveResult"]>(
      "POST",
      org
        ? `/api/v1/orgs/${org}/storage-targets`
        : "/api/v1/admin/storage-targets",
      b,
    ),
  updateStorageTarget: (
    org: string | undefined,
    id: string,
    b: S["StorageTargetRequest"],
  ) =>
    request<S["StorageTargetSaveResult"]>(
      "PATCH",
      org
        ? `/api/v1/orgs/${org}/storage-targets/${id}`
        : `/api/v1/admin/storage-targets/${id}`,
      b,
    ),
  deleteStorageTarget: (
    org: string | undefined,
    id: string,
    acceptUnrestorable = false,
  ) =>
    request<void>(
      "DELETE",
      `${org ? `/api/v1/orgs/${org}/storage-targets/${id}` : `/api/v1/admin/storage-targets/${id}`}${qs({ accept_unrestorable: acceptUnrestorable ? "true" : undefined })}`,
    ),
  testStorageTarget: (
    org: string | undefined,
    b: S["StorageTargetTestRequest"],
  ) =>
    request<StorageTestResult>(
      "POST",
      `/api/v1/storage-targets/test${qs({ org })}`,
      b,
    ),
  projectBackupStorage: (id: string) =>
    getJSON<S["ProjectBackupStorage"]>(`/api/v1/projects/${id}/storage-target`),
  setProjectStorageTarget: (id: string, b: S["ProjectStorageTargetRequest"]) =>
    request<S["ProjectStorageTargetResult"]>(
      "PUT",
      `/api/v1/projects/${id}/storage-target`,
      b,
    ),
  enableProjectBackupKey: (id: string, rotate = false) =>
    request<S["ProjectBackupKey"]>(
      "POST",
      `/api/v1/projects/${id}/backup-key`,
      { rotate },
    ),
  /** The key file (README + armored OpenPGP key); needs a recent re-authentication. */
  downloadProjectBackupKey: async (id: string): Promise<string> => {
    const res = await fetch(`/api/v1/projects/${id}/backup-key/download`, {
      credentials: "same-origin",
    });
    if (!res.ok) {
      let err: ApiErrorBody | undefined;
      try {
        err = (await res.json()) as ApiErrorBody;
      } catch {
        err = undefined;
      }
      throw new ApiRequestError(res.status, err);
    }
    return res.text();
  },

  importPreflight: (source_url: string) =>
    request<ImportPreflight>("POST", "/api/v1/imports/preflight", {
      source_url,
    }),
  createImport: (b: S["ImportRequest"]) =>
    request<ProjectCredentials>("POST", "/api/v1/imports", b),

  nodes: () => getJSON<S["NodeList"]>("/api/v1/nodes"),
  nodeToken: (id: string) =>
    request<RegistrationToken>(
      "POST",
      `/api/v1/nodes/${id}/registration-token`,
    ),
  node: (id: string) => getJSON<NodeDetail>(`/api/v1/nodes/${id}`),
  createNode: (b: S["CreateNodeRequest"]) =>
    request<NodeCreated>("POST", "/api/v1/nodes", b),
  removeNode: (id: string) => request<void>("DELETE", `/api/v1/nodes/${id}`),
  updateNode: (id: string, role: "shared" | "dedicated" | "both") =>
    request<Node>("PATCH", `/api/v1/nodes/${id}`, { role }),
  setFailureDomain: (id: string, failure_domain: string) =>
    request<Node>("PATCH", `/api/v1/nodes/${id}`, { failure_domain }),
  failureDomainProblems: () =>
    getJSON<S["FailureDomainProblems"]>("/api/v1/admin/failure-domains"),
  createSharedCluster: (id: string, memory_mb: number, pg_version?: number) =>
    request<Operation>("POST", `/api/v1/nodes/${id}/shared-cluster`, {
      memory_mb,
      pg_version,
    }),

  operations: (
    p: {
      org?: string;
      platform?: string;
      project_id?: string;
      status?: string;
      kind?: string;
      limit?: number;
    } = {},
  ) => getJSON<S["OperationList"]>(`/api/v1/operations${qs(p)}`),
  operation: (id: string) => getJSON<Operation>(`/api/v1/operations/${id}`),

  alerts: (status?: "firing" | "resolved") =>
    getJSON<S["AlertList"]>(`/api/v1/alerts${qs({ status, limit: 200 })}`),
  alertSettings: () => getJSON<AlertSettings>("/api/v1/settings/alerts"),
  saveAlertSettings: (b: AlertSettingsRequest) =>
    request<AlertSettings>("PUT", "/api/v1/settings/alerts", b),
  testAlerts: () =>
    request<S["AlertTestResult"]>("POST", "/api/v1/settings/alerts/test"),
  incidents: () => getJSON<S["IncidentList"]>("/api/v1/incidents"),
  createIncident: (b: S["CreateIncidentRequest"]) =>
    request<Incident>("POST", "/api/v1/incidents", b),
  updateIncident: (id: string, b: S["UpdateIncidentRequest"]) =>
    request<Incident>("PATCH", `/api/v1/incidents/${id}`, b),
  postIncidentUpdate: (id: string, b: S["IncidentUpdateRequest"]) =>
    request<Incident>("POST", `/api/v1/incidents/${id}/updates`, b),
  // Backend services (V4 §2).
  backendServices: (id: string) =>
    getJSON<S["BackendServices"]>(`/api/v1/projects/${id}/services`),
  enableBackendServices: (id: string) =>
    request<S["BackendServicesEnabled"]>(
      "POST",
      `/api/v1/projects/${id}/services`,
    ),
  disableBackendServices: (id: string) =>
    request<Operation>("DELETE", `/api/v1/projects/${id}/services`),
  updateBackendServices: (id: string, b: S["BackendServicesUpdate"]) =>
    request<S["BackendServices"]>(
      "PATCH",
      `/api/v1/projects/${id}/services`,
      b,
    ),
  createAPIKey: (id: string, b: S["CreateApiKeyRequest"]) =>
    request<S["CreatedApiKey"]>(
      "POST",
      `/api/v1/projects/${id}/services/keys`,
      b,
    ),
  revokeAPIKey: (id: string, keyId: string) =>
    request<S["ApiKey"]>(
      "DELETE",
      `/api/v1/projects/${id}/services/keys/${keyId}`,
    ),
  apiRequestLogs: (id: string, before?: number) =>
    getJSON<S["ApiRequestLogList"]>(
      `/api/v1/projects/${id}/services/logs${qs({ before, limit: 100 })}`,
    ),
  /** What the data API exposes, for the API docs (V4.1 §9.2). */
  servicesCatalog: (id: string) =>
    getJSON<S["ServicesCatalog"]>(`/api/v1/projects/${id}/services/catalog`),
  /** This month's usage and charges (V4.1 §9.3). */
  servicesUsage: (id: string) =>
    getJSON<S["ServicesUsage"]>(`/api/v1/projects/${id}/services/usage`),
  /** Typed definitions of the exposed tables, views and functions. */
  serviceTypes: async (
    id: string,
    lang: "ts" | "dart" | "go",
    pkg?: string,
  ): Promise<string> => {
    const res = await fetch(
      `/api/v1/projects/${id}/services/types${qs({ lang, package: pkg })}`,
      { credentials: "same-origin" },
    );
    if (!res.ok) {
      let err: ApiErrorBody | undefined;
      try {
        err = (await res.json()) as ApiErrorBody;
      } catch {
        err = undefined;
      }
      throw new ApiRequestError(res.status, err);
    }
    return res.text();
  },
  securityAdvisor: (id: string) =>
    getJSON<S["AdvisorFindings"]>(`/api/v1/projects/${id}/services/advisor`),
  exploreDataAPI: (id: string, b: S["ExploreRequest"]) =>
    request<S["ExploreResponse"]>(
      "POST",
      `/api/v1/projects/${id}/services/explore`,
      b,
    ),
  // Auth (V4 §4.9).
  authConfig: (id: string) =>
    getJSON<S["AuthConfig"]>(`/api/v1/projects/${id}/auth/config`),
  updateAuthConfig: (id: string, b: S["AuthConfigUpdate"]) =>
    request<S["AuthConfig"]>("PATCH", `/api/v1/projects/${id}/auth/config`, b),
  authHookDeliveries: (id: string) =>
    getJSON<S["AuthHookDeliveryList"]>(`/api/v1/projects/${id}/auth/hooks`),
  testAuthSMTP: (id: string, b: S["AuthSMTPTest"]) =>
    request<void>("POST", `/api/v1/projects/${id}/auth/smtp/test`, b),
  previewAuthTemplate: (id: string, b: S["AuthTemplatePreview"]) =>
    request<S["AuthEmailTemplate"]>(
      "POST",
      `/api/v1/projects/${id}/auth/templates/preview`,
      b,
    ),
  signingKeys: (id: string) =>
    getJSON<S["SigningKeyList"]>(`/api/v1/projects/${id}/auth/signing-keys`),
  rotateSigningKey: (id: string) =>
    request<S["SigningKeyList"]>(
      "POST",
      `/api/v1/projects/${id}/auth/signing-keys/rotate`,
    ),
  revokeSigningKey: (id: string, keyId: string) =>
    request<S["SigningKeyList"]>(
      "POST",
      `/api/v1/projects/${id}/auth/signing-keys/${keyId}/revoke`,
    ),
  authUsers: (id: string, q?: string, page?: number) =>
    getJSON<S["AuthUserList"]>(
      `/api/v1/projects/${id}/auth/users${qs({ q, page, per_page: 50 })}`,
    ),
  createAuthUser: (id: string, b: S["AuthUserCreate"]) =>
    request<S["AuthUser"]>("POST", `/api/v1/projects/${id}/auth/users`, b),
  authUser: (id: string, userId: string) =>
    getJSON<S["AuthUserDetail"]>(`/api/v1/projects/${id}/auth/users/${userId}`),
  updateAuthUser: (id: string, userId: string, b: S["AuthUserUpdate"]) =>
    request<S["AuthUser"]>(
      "PATCH",
      `/api/v1/projects/${id}/auth/users/${userId}`,
      b,
    ),
  deleteAuthUser: (id: string, userId: string) =>
    request<void>("DELETE", `/api/v1/projects/${id}/auth/users/${userId}`),
  signOutAuthUser: (id: string, userId: string) =>
    request<S["AuthSignOutResult"]>(
      "POST",
      `/api/v1/projects/${id}/auth/users/${userId}/signout`,
    ),
  authAudit: (id: string) =>
    getJSON<S["AuthAuditList"]>(`/api/v1/projects/${id}/auth/audit`),
  realtime: (id: string) =>
    getJSON<S["RealtimeOverview"]>(`/api/v1/projects/${id}/realtime`),
  setRealtimeTable: (
    id: string,
    schema: string,
    table: string,
    enabled: boolean,
  ) =>
    request<void>(
      "PUT",
      `/api/v1/projects/${id}/realtime/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}`,
      { enabled },
    ),
  setRealtimeTopics: (id: string, topics: string[]) =>
    request<void>("PUT", `/api/v1/projects/${id}/realtime/persisted-topics`, {
      topics,
    }),
  files: (id: string) =>
    getJSON<S["StorageOverview"]>(`/api/v1/projects/${id}/files`),
  createBucket: (id: string, b: S["StorageBucketInput"]) =>
    request<S["StorageBucket"]>(
      "POST",
      `/api/v1/projects/${id}/files/buckets`,
      b,
    ),
  updateBucket: (id: string, bucket: string, b: S["StorageBucketUpdate"]) =>
    request<S["StorageBucket"]>(
      "PATCH",
      `/api/v1/projects/${id}/files/buckets/${encodeURIComponent(bucket)}`,
      b,
    ),
  deleteBucket: (id: string, bucket: string, empty?: boolean) =>
    request<void>(
      "DELETE",
      `/api/v1/projects/${id}/files/buckets/${encodeURIComponent(bucket)}${qs({ empty: empty ? "true" : undefined })}`,
    ),
  bucketObjects: (
    id: string,
    bucket: string,
    p: { prefix?: string; cursor?: string; limit?: number },
  ) =>
    getJSON<S["StorageObjectList"]>(
      `/api/v1/projects/${id}/files/buckets/${encodeURIComponent(bucket)}/objects${qs(p)}`,
    ),
  deleteObjects: (id: string, bucket: string, paths: string[]) =>
    request<{ deleted: number }>(
      "DELETE",
      `/api/v1/projects/${id}/files/buckets/${encodeURIComponent(bucket)}/objects`,
      { paths },
    ),
  objectURL: (id: string, bucket: string, path: string) =>
    `/api/v1/projects/${id}/files/buckets/${encodeURIComponent(bucket)}/object${qs({ path })}`,
  uploadObject: (id: string, bucket: string, path: string, file: File) =>
    uploadFile(
      `/api/v1/projects/${id}/files/buckets/${encodeURIComponent(bucket)}/object${qs({ path })}`,
      file,
      "PUT",
      file.type || "application/octet-stream",
    ) as Promise<S["StorageObject"]>,
  signObject: (id: string, bucket: string, path: string, expiresIn: number) =>
    request<{ signed_url: string; expires_at: string }>(
      "POST",
      `/api/v1/projects/${id}/files/buckets/${encodeURIComponent(bucket)}/sign`,
      { path, expires_in: expiresIn },
    ),
  maintenanceAnnouncements: () =>
    getJSON<S["MaintenanceAnnouncementList"]>(
      "/api/v1/admin/maintenance/announcements",
    ),
  announceMaintenance: (b: S["MaintenanceAnnouncementRequest"]) =>
    request<S["MaintenanceAnnouncement"]>(
      "POST",
      "/api/v1/admin/maintenance/announcements",
      b,
    ),
  confirmMaintenanceDraft: (id: string) =>
    request<S["MaintenanceAnnouncement"]>(
      "POST",
      `/api/v1/admin/maintenance/announcements/${id}/confirm`,
    ),
  discardMaintenanceDraft: (id: string) =>
    request<S["MaintenanceAnnouncement"]>(
      "POST",
      `/api/v1/admin/maintenance/announcements/${id}/discard`,
    ),
  previewMaintenance: (b: S["MaintenancePreviewRequest"]) =>
    request<S["MaintenancePreview"]>(
      "POST",
      "/api/v1/admin/maintenance/announcements/preview",
      b,
    ),
  cancelMaintenance: (id: string) =>
    request<S["MaintenanceAnnouncement"]>(
      "DELETE",
      `/api/v1/admin/maintenance/announcements/${id}`,
    ),
  poolerHosts: () => getJSON<S["PoolerHosts"]>("/api/v1/pooler-hosts"),
  isolationChecks: () =>
    getJSON<S["IsolationCheckList"]>("/api/v1/security/isolation-checks"),
  runIsolationChecks: () =>
    request<S["OperationList"]>("POST", "/api/v1/security/isolation-checks"),

  // Billing (V3 §3): owners and billing members.
  billing: (org: string) =>
    getJSON<BillingAccount>(`/api/v1/orgs/${org}/billing`),
  updateBilling: (org: string, b: S["BillingDetailsUpdate"]) =>
    request<BillingAccount>("PATCH", `/api/v1/orgs/${org}/billing`, b),
  changePlan: (org: string, b: PlanChangeRequest) =>
    request<PlanChange>("POST", `/api/v1/orgs/${org}/billing/plan`, b),
  billingContacts: (org: string) =>
    getJSON<{ items: BillingContact[] }>(
      `/api/v1/orgs/${org}/billing/contacts`,
    ),
  addBillingContact: (org: string, b: BillingContact) =>
    request<BillingContact>("POST", `/api/v1/orgs/${org}/billing/contacts`, b),
  updateBillingContact: (org: string, email: string, statusEmails: boolean) =>
    request<BillingContact>(
      "PATCH",
      `/api/v1/orgs/${org}/billing/contacts/${encodeURIComponent(email)}`,
      { status_emails: statusEmails },
    ),
  orgIncidents: (org: string) =>
    getJSON<S["OrgIncidentList"]>(`/api/v1/orgs/${org}/incidents`),
  removeBillingContact: (org: string, email: string) =>
    request<void>(
      "DELETE",
      `/api/v1/orgs/${org}/billing/contacts/${encodeURIComponent(email)}`,
    ),
  forecast: (org: string) =>
    getJSON<BillingForecast>(`/api/v1/orgs/${org}/billing/forecast`),
  invoices: (org: string) =>
    getJSON<S["InvoiceList"]>(`/api/v1/orgs/${org}/billing/invoices`),
  invoice: (org: string, id: string) =>
    getJSON<InvoiceDetail>(`/api/v1/orgs/${org}/billing/invoices/${id}`),
  invoicePdfUrl: (org: string, id: string) =>
    `/api/v1/orgs/${org}/billing/invoices/${id}/pdf`,
  /** What a dedicated size, HA or sync replication costs; anyone in the org may ask. */
  estimate: (org: string, b: EstimateRequest) =>
    request<CostEstimate>("POST", `/api/v1/orgs/${org}/billing/estimate`, b),

  virtualAccount: (org: string) =>
    request<VirtualAccount>(
      "POST",
      `/api/v1/orgs/${org}/billing/virtual-account`,
    ),
  wallet: (org: string) =>
    getJSON<Wallet>(`/api/v1/orgs/${org}/billing/wallet`),
  openWallet: (org: string, b: WalletSetup) =>
    request<Wallet>("POST", `/api/v1/orgs/${org}/billing/wallet`, b),
  allowDebits: (org: string, termsVersion: string) =>
    request<DebitConsent>("POST", `/api/v1/orgs/${org}/billing/debit-consent`, {
      terms_version: termsVersion,
    }),
  withdrawDebitConsent: (org: string) =>
    request<void>("DELETE", `/api/v1/orgs/${org}/billing/debit-consent`),
  payFromWallet: (org: string, invoice: string) =>
    request<WalletCharge>(
      "POST",
      `/api/v1/orgs/${org}/billing/invoices/${invoice}/pay-from-wallet`,
    ),
  payments: (org: string) =>
    getJSON<S["PaymentList"]>(`/api/v1/orgs/${org}/billing/payments`),
  receiptUrl: (org: string, id: string) =>
    `/api/v1/orgs/${org}/billing/payments/${id}/receipt`,
  uploadWht: (org: string, invoice: string, file: File) =>
    uploadFile(
      `/api/v1/orgs/${org}/billing/invoices/${invoice}/wht-certificate${qs({ filename: file.name })}`,
      file,
    ),

  adminPayments: (p: { provider?: string } = {}) =>
    getJSON<S["PaymentList"]>(`/api/v1/admin/payments${qs(p)}`),
  recordPayment: (b: {
    org_id: string;
    amount_minor: number;
    reference: string;
    note?: string;
    invoice_id?: string;
    topup?: boolean;
    received_at?: string;
    proof_key?: string;
  }) => request<Payment>("POST", "/api/v1/admin/payments", b),
  uploadProof: (org: string, file: File) =>
    uploadFile(
      `/api/v1/admin/billing/documents${qs({ org_id: org, filename: file.name })}`,
      file,
    ) as Promise<{ key: string }>,
  refundPayment: (
    id: string,
    amount_minor: number,
    reason: string,
    reopen_invoices = false,
  ) =>
    request<{ id: string; status: string; amount_minor: number }>(
      "POST",
      `/api/v1/admin/payments/${id}/refund`,
      { amount_minor, reason, reopen_invoices },
    ),
  adminOrgBilling: (org: string) =>
    getJSON<BillingAccount>(`/api/v1/admin/orgs/${org}/billing`),
  paymentEvents: (outcome?: string) =>
    getJSON<{ items: PaymentEvent[] }>(
      `/api/v1/admin/payment-events${qs({ outcome })}`,
    ),
  attributeEvent: (id: number, org_id: string) =>
    request<Payment>("POST", `/api/v1/admin/payment-events/${id}/attribute`, {
      org_id,
    }),
  outstandingWht: () =>
    getJSON<{ items: OutstandingWht[] }>("/api/v1/admin/wht"),
  whtCsvUrl: () => "/api/v1/admin/wht?format=csv",
  adminUploadWht: (invoice: string, file: File) =>
    uploadFile(
      `/api/v1/admin/invoices/${invoice}/wht-certificate${qs({ filename: file.name })}`,
      file,
    ),
  reconciliation: () =>
    getJSON<{ items: Reconciliation[] }>("/api/v1/admin/reconciliation"),
  runReconciliation: (from?: string, to?: string) =>
    request<{ items: Reconciliation[] }>(
      "POST",
      "/api/v1/admin/reconciliation",
      { from, to },
    ),
  setGrace: (org: string, until: string | null) =>
    request<void>("PUT", `/api/v1/admin/orgs/${org}/billing/grace`, { until }),

  billingSettings: () =>
    getJSON<BillingSettings>("/api/v1/admin/billing/settings"),
  saveBillingSettings: (b: BillingSettings) =>
    request<BillingSettings>("PUT", "/api/v1/admin/billing/settings", b),
  priceBooks: () =>
    getJSON<{ current_version: number; items: PriceBook[] }>(
      "/api/v1/admin/price-books",
    ),
  createPriceBook: (b: S["PriceBookInput"]) =>
    request<PriceBook>("POST", "/api/v1/admin/price-books", b),
  updatePriceBook: (v: number, b: S["PriceBookInput"]) =>
    request<PriceBook>("PUT", `/api/v1/admin/price-books/${v}`, b),
  deletePriceBook: (v: number) =>
    request<void>("DELETE", `/api/v1/admin/price-books/${v}`),
  publishPriceBook: (v: number) =>
    request<{ price_book: PriceBook; notified: number }>(
      "POST",
      `/api/v1/admin/price-books/${v}/publish`,
    ),
  previewPriceBook: (v: number, period?: string) =>
    request<PricePreview>("POST", `/api/v1/admin/price-books/${v}/preview`, {
      period,
    }),
  adminInvoices: (p: { status?: Invoice["status"]; period?: string } = {}) =>
    getJSON<S["InvoiceList"]>(`/api/v1/admin/invoices${qs(p)}`),
  adminInvoice: (id: string) =>
    getJSON<InvoiceDetail>(`/api/v1/admin/invoices/${id}`),
  adminInvoicePdfUrl: (id: string) => `/api/v1/admin/invoices/${id}/pdf`,
  draftInvoices: (period: string, org_id?: string) =>
    request<{ drafts: number }>("POST", "/api/v1/admin/invoices/draft", {
      period,
      org_id,
    }),
  holdInvoice: (id: string, held: boolean, reason?: string) =>
    request<Invoice>("POST", `/api/v1/admin/invoices/${id}/hold`, {
      held,
      reason,
    }),
  issueInvoice: (id: string) =>
    request<Invoice>("POST", `/api/v1/admin/invoices/${id}/issue`),
  creditNote: (id: string, amount_minor: number, reason: string) =>
    request<CreditNote>("POST", `/api/v1/admin/invoices/${id}/credit-notes`, {
      amount_minor,
      reason,
    }),
  ledgerCheck: () => getJSON<LedgerCheck>("/api/v1/admin/ledger/check"),
  adminUpdateOrgBilling: (org: string, b: S["AdminBillingUpdate"]) =>
    request<BillingAccount>("PATCH", `/api/v1/admin/orgs/${org}/billing`, b),

  // Support (V3 §7.1): the organisation's tickets and WhatsApp numbers.
  orgTickets: (org: string) =>
    getJSON<S["TicketList"]>(`/api/v1/orgs/${org}/support/tickets`),
  openTicket: (org: string, b: S["TicketOpen"]) =>
    request<Ticket>("POST", `/api/v1/orgs/${org}/support/tickets`, b),
  orgTicket: (org: string, id: string) =>
    getJSON<TicketDetail>(`/api/v1/orgs/${org}/support/tickets/${id}`),
  replyTicket: (org: string, id: string, body: string) =>
    request<void>(
      "POST",
      `/api/v1/orgs/${org}/support/tickets/${id}/messages`,
      { body },
    ),
  supportPhones: (org: string) =>
    getJSON<{ items: SupportPhone[]; available: boolean }>(
      `/api/v1/orgs/${org}/support/phones`,
    ),
  addSupportPhone: (org: string, phone: string) =>
    request<SupportPhone>("POST", `/api/v1/orgs/${org}/support/phones`, {
      phone,
    }),
  removeSupportPhone: (org: string, phone: string) =>
    request<void>(
      "DELETE",
      `/api/v1/orgs/${org}/support/phones/${encodeURIComponent(phone)}`,
    ),
  // The support console (support staff and platform admins).
  supportQueue: (
    p: { status?: Ticket["status"]; assignee?: string; org_id?: string } = {},
  ) => getJSON<TicketQueue>(`/api/v1/admin/support/tickets${qs(p)}`),
  supportTicket: (id: string) =>
    getJSON<TicketDetail>(`/api/v1/admin/support/tickets/${id}`),
  updateTicket: (id: string, b: TicketUpdate) =>
    request<Ticket>("PATCH", `/api/v1/admin/support/tickets/${id}`, b),
  staffReply: (id: string, body: string, note: boolean) =>
    request<void>("POST", `/api/v1/admin/support/tickets/${id}/messages`, {
      body,
      note,
    }),
  supportStaff: () =>
    getJSON<{ items: SupportStaff[] }>("/api/v1/admin/support/staff"),
  // The revenue dashboard (V3 §7.2).
  revenue: (months = 12) =>
    getJSON<Revenue>(`/api/v1/admin/revenue${qs({ months: String(months) })}`),
  revenueCsvUrl: (months = 12) =>
    `/api/v1/admin/revenue${qs({ months: String(months), format: "csv" })}`,
  // Legal documents (V3 §7.3).
  legal: () => getJSON<{ items: LegalDocument[] }>("/api/v1/legal"),
  orgLegal: (org: string) => getJSON<OrgLegal>(`/api/v1/orgs/${org}/legal`),
  acceptLegal: (org: string, id: string) =>
    request<OrgLegal>("POST", `/api/v1/orgs/${org}/legal/${id}/accept`),
  legalVersions: () =>
    getJSON<{ items: LegalVersion[] }>("/api/v1/admin/legal"),
  legalDocument: (id: string) =>
    getJSON<LegalDocumentDetail>(`/api/v1/admin/legal/${id}`),
  publishLegal: (b: S["LegalPublish"]) =>
    request<LegalDocument>("POST", "/api/v1/admin/legal", b),
  publishOrderForm: (org: string, b: S["OrderFormPublish"]) =>
    request<LegalDocument>("POST", `/api/v1/admin/orgs/${org}/order-form`, b),

  // Query insights (V3 §8).
  insightQueries: (id: string, range: InsightRange, sort: InsightSort) =>
    getJSON<S["InsightQueryList"]>(
      `/api/v1/projects/${id}/insights/queries?range=${range}&sort=${sort}&limit=50`,
    ),
  insightQuery: (id: string, queryId: string, range: InsightRange) =>
    getJSON<InsightQueryDetail>(
      `/api/v1/projects/${id}/insights/queries/${encodeURIComponent(queryId)}?range=${range}`,
    ),
  explainQuery: (id: string, queryId: string, generic: boolean) =>
    request<InsightPlan>("POST", `/api/v1/projects/${id}/insights/explain`, {
      query_id: queryId,
      generic,
    }),
  slowQueries: (id: string, range: InsightRange) =>
    getJSON<S["SlowQueryList"]>(
      `/api/v1/projects/${id}/insights/slow?range=${range}`,
    ),
  insightIndexes: (id: string) =>
    getJSON<IndexReport>(`/api/v1/projects/${id}/insights/indexes`),
  insightBloat: (id: string) =>
    getJSON<S["BloatList"]>(`/api/v1/projects/${id}/insights/bloat`),
  insightLocks: (id: string) =>
    getJSON<S["LockList"]>(`/api/v1/projects/${id}/insights/locks`),
  // Regions and data residency (V3 §6).
  regions: () => getJSON<{ items: Region[] }>("/api/v1/regions"),
  adminRegions: () => getJSON<AdminRegionList>("/api/v1/admin/regions"),
  regionOverview: (id: string) =>
    getJSON<S["RegionOverview"]>(
      `/api/v1/admin/regions/${encodeURIComponent(id)}/overview`,
    ),
  moveAllToRegionEtcd: (id: string) =>
    request<Operation>(
      "POST",
      `/api/v1/admin/regions/${encodeURIComponent(id)}/etcd-move-all`,
    ),
  regionReadiness: (id: string) =>
    getJSON<RegionReadiness>(
      `/api/v1/admin/regions/${encodeURIComponent(id)}/readiness`,
    ),
  saveRegion: (id: string, b: AdminRegionRequest) =>
    request<AdminRegion>(
      "PUT",
      `/api/v1/admin/regions/${encodeURIComponent(id)}`,
      b,
    ),
  projectNetwork: (projectId: string) =>
    getJSON<ProjectNetwork>(`/api/v1/projects/${projectId}/network`),
  setProjectNetwork: (projectId: string, allowed_cidrs: string[]) =>
    request<ProjectNetwork>("PUT", `/api/v1/projects/${projectId}/network`, {
      allowed_cidrs,
    }),
  manageRoles: (projectId: string) =>
    getJSON<ManageRoles>(`/api/v1/projects/${projectId}/manage-roles`),
  setManageRoles: (projectId: string, enabled: boolean) =>
    request<ManageRoles>("PUT", `/api/v1/projects/${projectId}/manage-roles`, {
      enabled,
    }),
  syncManageRoles: (projectId: string) =>
    request<ManageRoles>(
      "POST",
      `/api/v1/projects/${projectId}/manage-roles/sync`,
    ),
  setResidency: (projectId: string, enabled: boolean) =>
    request<ProjectResidencyResult>(
      "PUT",
      `/api/v1/projects/${projectId}/residency`,
      { enabled },
    ),
  // Capacity automation (V3 §5).
  capacity: () => getJSON<Capacity>("/api/v1/admin/capacity"),
  saveCapacitySettings: (b: CapacitySettings) =>
    request<CapacitySettings>("PUT", "/api/v1/admin/capacity/settings", b),
  evaluateCapacity: () =>
    request<{ items: CapacityProposal[] }>(
      "POST",
      "/api/v1/admin/capacity/evaluate",
    ),
  approveProposal: (id: string) =>
    request<CapacityProposal>(
      "POST",
      `/api/v1/admin/capacity/proposals/${id}/approve`,
    ),
  rejectProposal: (id: string) =>
    request<CapacityProposal>(
      "POST",
      `/api/v1/admin/capacity/proposals/${id}/reject`,
    ),
  planRebalance: () =>
    request<{ batch?: string | null; moves: number }>(
      "POST",
      "/api/v1/admin/capacity/rebalance",
    ),
  decideBatch: (batch: string, approve: boolean) =>
    request<{ moves: number }>(
      "POST",
      `/api/v1/admin/capacity/batches/${batch}`,
      { approve },
    ),
  cloudCatalog: () =>
    getJSON<{ provider: string; items: ServerPrice[] }>(
      "/api/v1/admin/cloud/catalog",
    ),
  drainNode: (id: string) =>
    request<{ node: Node; moves: number }>("POST", `/api/v1/nodes/${id}/drain`),
  stopDrain: (id: string) =>
    request<Node>("DELETE", `/api/v1/nodes/${id}/drain`),
  setNodeCost: (id: string, b: S["NodeCost"]) =>
    request<Node>("PUT", `/api/v1/nodes/${id}/cost`, b),
  // Costs and margins (V3 §5.4, §7.2).
  costs: (month?: string) =>
    getJSON<Margins>(`/api/v1/admin/costs${qs({ month })}`),
  costsCsvUrl: (month?: string) =>
    `/api/v1/admin/costs${qs({ month, format: "csv" })}`,
  attributeCosts: (from: string, to: string) =>
    request<{ days: number }>("POST", "/api/v1/admin/costs/attribute", {
      from,
      to,
    }),
  costSettings: () => getJSON<CostSettings>("/api/v1/admin/costs/settings"),
  saveCostSettings: (b: CostSettings) =>
    request<CostSettings>("PUT", "/api/v1/admin/costs/settings", b),
  fxRates: () =>
    getJSON<{ current: FXRate[]; history: FXRate[] }>("/api/v1/admin/fx-rates"),
  setFxRate: (currency: string, ngn_per_unit: number) =>
    request<FXRate>("POST", "/api/v1/admin/fx-rates", {
      currency,
      ngn_per_unit,
    }),

  generalSettings: () => getJSON<GeneralSettings>("/api/v1/settings/general"),
  setDbHost: (db_host: string) =>
    request<GeneralSettings>("PUT", "/api/v1/settings/db-host", { db_host }),
  checkDbHost: (db_host: string) =>
    request<DnsCheck>("POST", "/api/v1/settings/db-host/check", { db_host }),
};

/** Turns any thrown value into a message fit for the UI. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiRequestError) return err.message;
  if (err instanceof Error) return err.message;
  return String(err);
}
