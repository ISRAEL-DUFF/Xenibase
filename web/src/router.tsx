import { QueryClient } from "@tanstack/react-query";
import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  lazyRouteComponent,
  Outlet,
  redirect,
} from "@tanstack/react-router";
import { AppShell } from "./components/shell/AppShell";
import { ToastProvider } from "./components/Toasts";
import { StepUpHost } from "./components/StepUp";
import { parseAuthorizeSearch } from "./lib/oauth";
import { sessionQuery } from "./lib/session";
import { LoginPage } from "./pages/Login";
import { SetupPage } from "./pages/Setup";
import { NotFound } from "./pages/NotFound";

// Pages load on first visit; the shell and sign-in pages are in the main bundle.
const AlertsPage = lazyRouteComponent(
  () => import("./pages/Alerts"),
  "AlertsPage",
);
const IncidentsPage = lazyRouteComponent(
  () => import("./pages/Incidents"),
  "IncidentsPage",
);
const AccountPage = lazyRouteComponent(
  () => import("./pages/Account"),
  "AccountPage",
);
const DevicePage = lazyRouteComponent(
  () => import("./pages/Device"),
  "DevicePage",
);
const OAuthAuthorizePage = lazyRouteComponent(
  () => import("./pages/OAuthAuthorize"),
  "OAuthAuthorizePage",
);
const AdminUsersPage = lazyRouteComponent(
  () => import("./pages/AdminUsers"),
  "AdminUsersPage",
);
const AdminOrgPage = lazyRouteComponent(
  () => import("./pages/AdminOrgs"),
  "AdminOrgPage",
);
const AdminOrgsPage = lazyRouteComponent(
  () => import("./pages/AdminOrgs"),
  "AdminOrgsPage",
);
const AdminPlansPage = lazyRouteComponent(
  () => import("./pages/AdminOrgs"),
  "AdminPlansPage",
);
const DedicatedRequestsPage = lazyRouteComponent(
  () => import("./pages/AdminOrgs"),
  "DedicatedRequestsPage",
);
const UsagePage = lazyRouteComponent(
  () => import("./pages/Usage"),
  "UsagePage",
);
const BillingPage = lazyRouteComponent(
  () => import("./pages/Billing"),
  "BillingPage",
);
const PricingPage = lazyRouteComponent(
  () => import("./pages/Pricing"),
  "PricingPage",
);
const AdminBillingPage = lazyRouteComponent(
  () => import("./pages/AdminBilling"),
  "AdminBillingPage",
);
const AdminRevenuePage = lazyRouteComponent(
  () => import("./pages/AdminRevenue"),
  "AdminRevenuePage",
);
const AdminCostsPage = lazyRouteComponent(
  () => import("./pages/AdminCosts"),
  "AdminCostsPage",
);
const AdminCapacityPage = lazyRouteComponent(
  () => import("./pages/AdminCapacity"),
  "AdminCapacityPage",
);
const AdminPgVersionsPage = lazyRouteComponent(
  () => import("./pages/AdminPgVersions"),
  "AdminPgVersionsPage",
);
const AdminRegionsPage = lazyRouteComponent(
  () => import("./pages/AdminRegions"),
  "AdminRegionsPage",
);
const SupportPage = lazyRouteComponent(
  () => import("./pages/Support"),
  "SupportPage",
);
const SupportConsolePage = lazyRouteComponent(
  () => import("./pages/Support"),
  "SupportConsolePage",
);
const OrgLegalPage = lazyRouteComponent(
  () => import("./pages/Legal"),
  "OrgLegalPage",
);
const AdminLegalPage = lazyRouteComponent(
  () => import("./pages/Legal"),
  "AdminLegalPage",
);
const AuditPage = lazyRouteComponent(
  () => import("./pages/Audit"),
  "AuditPage",
);
const OrgAuditPage = lazyRouteComponent(
  () => import("./pages/Audit"),
  "OrgAuditPage",
);
const OrgMembersPage = lazyRouteComponent(
  () => import("./pages/Org"),
  "OrgMembersPage",
);
const OrgSettingsPage = lazyRouteComponent(
  () => import("./pages/Org"),
  "OrgSettingsPage",
);
const ProjectMembersPage = lazyRouteComponent(
  () => import("./pages/ProjectMembers"),
  "ProjectMembersPage",
);
const InvitePage = lazyRouteComponent(
  () => import("./pages/Public"),
  "InvitePage",
);
const ResetPasswordPage = lazyRouteComponent(
  () => import("./pages/Public"),
  "ResetPasswordPage",
);
const SignupPage = lazyRouteComponent(
  () => import("./pages/Public"),
  "SignupPage",
);
const VerifyEmailPage = lazyRouteComponent(
  () => import("./pages/Public"),
  "VerifyEmailPage",
);
const ConfirmContactPage = lazyRouteComponent(
  () => import("./pages/Public"),
  "ConfirmContactPage",
);
const NewProjectPage = lazyRouteComponent(
  () => import("./pages/NewProject"),
  "NewProjectPage",
);
const OperationDetailPage = lazyRouteComponent(
  () => import("./pages/Operations"),
  "OperationDetailPage",
);
const OperationsPage = lazyRouteComponent(
  () => import("./pages/Operations"),
  "OperationsPage",
);
const ImportProjectPage = lazyRouteComponent(
  () => import("./pages/ImportProject"),
  "ImportProjectPage",
);
const NodeDetailPage = lazyRouteComponent(
  () => import("./pages/Nodes"),
  "NodeDetailPage",
);
const NodesPage = lazyRouteComponent(
  () => import("./pages/Nodes"),
  "NodesPage",
);
const ProjectBackupsPage = lazyRouteComponent(
  () => import("./pages/ProjectBackups"),
  "ProjectBackupsPage",
);
const ProjectBranchesPage = lazyRouteComponent(
  () => import("./pages/ProjectBranches"),
  "ProjectBranchesPage",
);
const ProjectJobsPage = lazyRouteComponent(
  () => import("./pages/ProjectJobs"),
  "ProjectJobsPage",
);
const ProjectWebhooksPage = lazyRouteComponent(
  () => import("./pages/ProjectWebhooks"),
  "ProjectWebhooksPage",
);
const ProjectConnectPage = lazyRouteComponent(
  () => import("./pages/ProjectConnect"),
  "ProjectConnectPage",
);
const ProjectLayout = lazyRouteComponent(
  () => import("./pages/ProjectOverview"),
  "ProjectLayout",
);
const ProjectOverviewPage = lazyRouteComponent(
  () => import("./pages/ProjectOverview"),
  "ProjectOverviewPage",
);
const ProjectMetricsPage = lazyRouteComponent(
  () => import("./pages/ProjectMetrics"),
  "ProjectMetricsPage",
);
const ProjectInsightsPage = lazyRouteComponent(
  () => import("./pages/ProjectInsights"),
  "ProjectInsightsPage",
);
const ProjectSettingsPage = lazyRouteComponent(
  () => import("./pages/ProjectSettings"),
  "ProjectSettingsPage",
);
const ProjectDatabaseSettingsPage = lazyRouteComponent(
  () => import("./pages/ProjectSettings"),
  "ProjectDatabaseSettingsPage",
);
const ProjectComputePage = lazyRouteComponent(
  () => import("./pages/ProjectSettings"),
  "ProjectComputePage",
);
const ProjectAuthPage = lazyRouteComponent(
  () => import("./pages/ProjectAuth"),
  "ProjectAuthPage",
);
const ProjectFilesPage = lazyRouteComponent(
  () => import("./pages/ProjectFiles"),
  "ProjectFilesPage",
);
const ProjectRealtimePage = lazyRouteComponent(
  () => import("./pages/ProjectRealtime"),
  "ProjectRealtimePage",
);
const ProjectAPIPage = lazyRouteComponent(
  () => import("./pages/ProjectAPI"),
  "ProjectAPIPage",
);
const ProjectStorageSettingsPage = lazyRouteComponent(
  () => import("./pages/ProjectSettings"),
  "ProjectStorageSettingsPage",
);
const ProjectExtensionsPage = lazyRouteComponent(
  () => import("./pages/ProjectExtensions"),
  "ProjectExtensionsPage",
);
const ProjectMigrationsPage = lazyRouteComponent(
  () => import("./pages/ProjectMigrations"),
  "ProjectMigrationsPage",
);
const ProjectLogsPage = lazyRouteComponent(
  () => import("./pages/ProjectLogs"),
  "ProjectLogsPage",
);
const ProjectSqlPage = lazyRouteComponent(
  () => import("./pages/ProjectSql"),
  "ProjectSqlPage",
);
const ProjectTablesPage = lazyRouteComponent(
  () => import("./pages/ProjectTables"),
  "ProjectTablesPage",
);
const ProjectsPage = lazyRouteComponent(
  () => import("./pages/Projects"),
  "ProjectsPage",
);
const SettingsPage = lazyRouteComponent(
  () => import("./pages/Settings"),
  "SettingsPage",
);

type Ctx = { queryClient: QueryClient };

const root = createRootRouteWithContext<Ctx>()({
  component: () => (
    <ToastProvider>
      <Outlet />
      <StepUpHost />
    </ToastProvider>
  ),
  notFoundComponent: NotFound,
});

// Every navigation checks the session: first-run setup, then sign-in.
const loadSession = (qc: QueryClient) => qc.ensureQueryData(sessionQuery);

const setup = createRoute({
  getParentRoute: () => root,
  path: "/setup",
  component: SetupPage,
});

const login = createRoute({
  getParentRoute: () => root,
  path: "/login",
  validateSearch: (s: Record<string, unknown>): { next?: string } => ({
    next:
      typeof s.next === "string" &&
      s.next.startsWith("/") &&
      !s.next.startsWith("//")
        ? s.next
        : undefined,
  }),
  beforeLoad: async ({ context }) => {
    const s = await loadSession(context.queryClient);
    if (s.setup_required) throw redirect({ to: "/setup" });
    if (s.authenticated) throw redirect({ to: "/projects" });
  },
  component: LoginPage,
});

const token = (s: Record<string, unknown>): { token?: string } => ({
  token: typeof s.token === "string" ? s.token : undefined,
});

// Public pages: they work signed in or not.
const signup = createRoute({
  getParentRoute: () => root,
  path: "/signup",
  component: SignupPage,
});
const verifyEmail = createRoute({
  getParentRoute: () => root,
  path: "/verify-email",
  validateSearch: token,
  component: VerifyEmailPage,
});
const confirmContact = createRoute({
  getParentRoute: () => root,
  path: "/confirm-contact",
  validateSearch: token,
  component: ConfirmContactPage,
});
const resetPassword = createRoute({
  getParentRoute: () => root,
  path: "/reset-password",
  validateSearch: token,
  component: ResetPasswordPage,
});
const invite = createRoute({
  getParentRoute: () => root,
  path: "/invite",
  validateSearch: token,
  component: InvitePage,
});

const app = createRoute({
  getParentRoute: () => root,
  id: "app",
  beforeLoad: async ({ context, location }) => {
    const s = await loadSession(context.queryClient);
    if (s.setup_required) throw redirect({ to: "/setup" });
    if (!s.authenticated)
      throw redirect({ to: "/login", search: { next: location.href } });
  },
  component: AppShell,
});

const index = createRoute({
  getParentRoute: () => app,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/projects" });
  },
});

const projects = createRoute({
  getParentRoute: () => app,
  path: "/projects",
  component: ProjectsPage,
});
const newProject = createRoute({
  getParentRoute: () => app,
  path: "/projects/new",
  component: NewProjectPage,
});
const importProject = createRoute({
  getParentRoute: () => app,
  path: "/projects/import",
  component: ImportProjectPage,
});
const project = createRoute({
  getParentRoute: () => app,
  path: "/projects/$id",
  component: ProjectLayout,
});
const projectOverview = createRoute({
  getParentRoute: () => project,
  path: "/",
  component: ProjectOverviewPage,
});
const projectConnect = createRoute({
  getParentRoute: () => project,
  path: "/connect",
  component: ProjectConnectPage,
});
const projectSql = createRoute({
  getParentRoute: () => project,
  path: "/sql",
  component: ProjectSqlPage,
});
const projectTables = createRoute({
  getParentRoute: () => project,
  path: "/tables",
  validateSearch: (
    s: Record<string, unknown>,
  ): { schema?: string; table?: string } => ({
    schema: typeof s.schema === "string" && s.schema ? s.schema : undefined,
    table: typeof s.table === "string" && s.table ? s.table : undefined,
  }),
  component: ProjectTablesPage,
});
const projectMetrics = createRoute({
  getParentRoute: () => project,
  path: "/metrics",
  component: ProjectMetricsPage,
});
const projectInsights = createRoute({
  getParentRoute: () => project,
  path: "/insights",
  component: ProjectInsightsPage,
});
const projectBackups = createRoute({
  getParentRoute: () => project,
  path: "/backups",
  component: ProjectBackupsPage,
});
const projectBranches = createRoute({
  getParentRoute: () => project,
  path: "/branches",
  component: ProjectBranchesPage,
});
const projectWebhooks = createRoute({
  getParentRoute: () => project,
  path: "/webhooks",
  component: ProjectWebhooksPage,
});
const projectJobs = createRoute({
  getParentRoute: () => project,
  path: "/jobs",
  component: ProjectJobsPage,
});
const projectSettings = createRoute({
  getParentRoute: () => project,
  path: "/settings",
  component: ProjectSettingsPage,
});
const projectDatabaseSettings = createRoute({
  getParentRoute: () => project,
  path: "/settings/database",
  component: ProjectDatabaseSettingsPage,
});
const projectCompute = createRoute({
  getParentRoute: () => project,
  path: "/settings/compute",
  component: ProjectComputePage,
});
const projectAuth = createRoute({
  getParentRoute: () => project,
  path: "/auth",
  component: ProjectAuthPage,
});
const projectRealtime = createRoute({
  getParentRoute: () => project,
  path: "/realtime",
  component: ProjectRealtimePage,
});
const projectFiles = createRoute({
  getParentRoute: () => project,
  path: "/files",
  component: ProjectFilesPage,
});
const projectAPISettings = createRoute({
  getParentRoute: () => project,
  path: "/settings/api",
  component: ProjectAPIPage,
});
const projectStorageSettings = createRoute({
  getParentRoute: () => project,
  path: "/settings/storage",
  component: ProjectStorageSettingsPage,
});
const projectExtensions = createRoute({
  getParentRoute: () => project,
  path: "/extensions",
  component: ProjectExtensionsPage,
});
const projectMigrations = createRoute({
  getParentRoute: () => project,
  path: "/migrations",
  component: ProjectMigrationsPage,
});
const projectLogs = createRoute({
  getParentRoute: () => project,
  path: "/logs",
  component: ProjectLogsPage,
});
const projectMembers = createRoute({
  getParentRoute: () => project,
  path: "/members",
  component: ProjectMembersPage,
});
const account = createRoute({
  getParentRoute: () => app,
  path: "/account",
  component: AccountPage,
});
const device = createRoute({
  getParentRoute: () => app,
  path: "/device",
  validateSearch: (s: Record<string, unknown>): { code?: string } => ({
    code: typeof s.code === "string" && s.code ? s.code : undefined,
  }),
  component: DevicePage,
});
// A third-party app's consent page; signed out, the app route sends the
// user to log in and back here with the app's query intact.
const oauthAuthorize = createRoute({
  getParentRoute: () => app,
  path: "/oauth/authorize",
  validateSearch: parseAuthorizeSearch,
  component: OAuthAuthorizePage,
});
const orgMembers = createRoute({
  getParentRoute: () => app,
  path: "/org/members",
  component: OrgMembersPage,
});
const orgSettings = createRoute({
  getParentRoute: () => app,
  path: "/org/settings",
  component: OrgSettingsPage,
});
const orgAudit = createRoute({
  getParentRoute: () => app,
  path: "/org/audit",
  component: OrgAuditPage,
});
const adminUsers = createRoute({
  getParentRoute: () => app,
  path: "/admin/users",
  component: AdminUsersPage,
});
const nodes = createRoute({
  getParentRoute: () => app,
  path: "/nodes",
  component: NodesPage,
});
const nodeDetail = createRoute({
  getParentRoute: () => app,
  path: "/nodes/$id",
  component: NodeDetailPage,
});
const operations = createRoute({
  getParentRoute: () => app,
  path: "/operations",
  component: OperationsPage,
});
const operation = createRoute({
  getParentRoute: () => app,
  path: "/operations/$id",
  component: OperationDetailPage,
});
const alertsRoute = createRoute({
  getParentRoute: () => app,
  path: "/alerts",
  component: AlertsPage,
});
const incidentsRoute = createRoute({
  getParentRoute: () => app,
  path: "/admin/incidents",
  component: IncidentsPage,
});
const audit = createRoute({
  getParentRoute: () => app,
  path: "/audit",
  component: AuditPage,
});
const settings = createRoute({
  getParentRoute: () => app,
  path: "/settings",
  component: SettingsPage,
});
const orgUsage = createRoute({
  getParentRoute: () => app,
  path: "/org/usage",
  component: UsagePage,
});
// ?org= and ?invoice= come from billing emails.
const orgBilling = createRoute({
  getParentRoute: () => app,
  path: "/org/billing",
  validateSearch: (
    s: Record<string, unknown>,
  ): { org?: string; invoice?: string } => ({
    org: typeof s.org === "string" ? s.org : undefined,
    invoice: typeof s.invoice === "string" ? s.invoice : undefined,
  }),
  component: BillingPage,
});
const pricing = createRoute({
  getParentRoute: () => app,
  path: "/pricing",
  component: PricingPage,
});
const adminBilling = createRoute({
  getParentRoute: () => app,
  path: "/admin/billing",
  component: AdminBillingPage,
});
const adminOrgs = createRoute({
  getParentRoute: () => app,
  path: "/admin/orgs",
  component: AdminOrgsPage,
});
const adminOrg = createRoute({
  getParentRoute: () => app,
  path: "/admin/orgs/$id",
  component: AdminOrgPage,
});
const adminPlans = createRoute({
  getParentRoute: () => app,
  path: "/admin/plans",
  component: AdminPlansPage,
});
const adminCosts = createRoute({
  getParentRoute: () => app,
  path: "/admin/costs",
  component: AdminCostsPage,
});
const adminCapacity = createRoute({
  getParentRoute: () => app,
  path: "/admin/capacity",
  component: AdminCapacityPage,
});
const adminPgVersions = createRoute({
  getParentRoute: () => app,
  path: "/admin/pg-versions",
  component: AdminPgVersionsPage,
});
const adminRegions = createRoute({
  getParentRoute: () => app,
  path: "/admin/regions",
  component: AdminRegionsPage,
});
const adminRevenue = createRoute({
  getParentRoute: () => app,
  path: "/admin/revenue",
  component: AdminRevenuePage,
});
const adminSupport = createRoute({
  getParentRoute: () => app,
  path: "/admin/support",
  component: SupportConsolePage,
});
const adminLegal = createRoute({
  getParentRoute: () => app,
  path: "/admin/legal",
  component: AdminLegalPage,
});
const orgSupport = createRoute({
  getParentRoute: () => app,
  path: "/org/support",
  component: SupportPage,
});
const orgLegal = createRoute({
  getParentRoute: () => app,
  path: "/org/legal",
  component: OrgLegalPage,
});
const adminRequests = createRoute({
  getParentRoute: () => app,
  path: "/admin/dedicated-requests",
  component: DedicatedRequestsPage,
});

// The component gallery, in development builds only.
const uiGallery = createRoute({
  getParentRoute: () => root,
  path: "/_ui",
  component: lazyRouteComponent(
    () => import("./pages/UiGallery"),
    "UiGalleryPage",
  ),
});

const routeTree = root.addChildren([
  ...(import.meta.env.DEV ? [uiGallery] : []),
  setup,
  login,
  signup,
  verifyEmail,
  confirmContact,
  resetPassword,
  invite,
  app.addChildren([
    index,
    projects,
    newProject,
    importProject,
    project.addChildren([
      projectOverview,
      projectConnect,
      projectSql,
      projectTables,
      projectBackups,
      projectBranches,
      projectWebhooks,
      projectJobs,
      projectMetrics,
      projectInsights,
      projectMembers,
      projectSettings,
      projectDatabaseSettings,
      projectCompute,
      projectStorageSettings,
      projectAPISettings,
      projectAuth,
      projectFiles,
      projectRealtime,
      projectExtensions,
      projectMigrations,
      projectLogs,
    ]),
    account,
    device,
    oauthAuthorize,
    orgMembers,
    orgSettings,
    orgAudit,
    adminUsers,
    nodes,
    nodeDetail,
    operations,
    operation,
    alertsRoute,
    incidentsRoute,
    audit,
    settings,
    orgUsage,
    orgBilling,
    pricing,
    adminBilling,
    adminOrgs,
    adminOrg,
    adminPlans,
    adminRequests,
    adminRevenue,
    adminCosts,
    adminRegions,
    adminPgVersions,
    adminCapacity,
    adminSupport,
    adminLegal,
    orgSupport,
    orgLegal,
  ]),
]);

export function makeRouter(queryClient: QueryClient) {
  return createRouter({
    routeTree,
    context: { queryClient },
    defaultPreload: "intent",
    defaultPreloadStaleTime: 0,
  });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof makeRouter>;
  }
}
