// The M2 and M3 "done when"s, entirely through the browser (spec §14): a
// fresh install to a working database; then back up, delete data, restore
// into a new project and verify it, and import a Supabase project and serve
// its app. Run against the install bundle by `make test-e2e`, or against a
// dev server with XENIBASE_E2E_URL.
import { expect, test, type Page } from "@playwright/test";
import { execFile } from "node:child_process";
import { createHmac } from "node:crypto";
import AxeBuilder from "@axe-core/playwright";
import fs, { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import pg from "pg";
import { startTodoApp } from "./todo-app";
import { freshTotp } from "./totp";

const setupCode = process.env.XENIBASE_E2E_SETUP_CODE ?? "";
const dbHost = process.env.XENIBASE_E2E_DB_HOST ?? "127.0.0.1";
// Where the DB hostname really points (no DNS in CI): "host[:port]" pairs
// are rewritten to this address, keeping TLS hostname verification.
const dbAddr = process.env.XENIBASE_E2E_DB_ADDR;
const caFile = process.env.XENIBASE_E2E_DB_CA; // verify-full against this root
// Wait for the pooler certificate from this issuer (ACME) before connecting.
const expectIssuer = process.env.XENIBASE_E2E_EXPECT_ISSUER;
// Backup storage the wizard configures (the bundle's fake S3).
const s3Endpoint = process.env.XENIBASE_E2E_S3_ENDPOINT;
const s3Bucket = process.env.XENIBASE_E2E_S3_BUCKET ?? "xenibase-e2e";
// A Supabase project to import: seeded through seedURL, imported from
// sourceURL (the same database, as the control plane reaches it).
const supabaseSeedURL = process.env.XENIBASE_E2E_SUPABASE_SEED_URL;
const supabaseURL = process.env.XENIBASE_E2E_SUPABASE_URL;
// The bundle's catch-all mail server: SMTP for the wizard, HTTP for the tests.
const smtpHost = process.env.XENIBASE_E2E_SMTP_HOST ?? "fakesmtp";
const smtpPort = process.env.XENIBASE_E2E_SMTP_PORT ?? "2525";
const mailAPI = process.env.XENIBASE_E2E_MAIL_API ?? "http://127.0.0.1:18025";
// The bundle's webhook receiver, as the tests read it.
const hookAPI = process.env.XENIBASE_E2E_HOOK_API ?? "http://127.0.0.1:18090";
const email = "owner@example.com";
const password = "a long enough passphrase";

let totpSecret = "";
// The signed-in session from the first test, reused by the later ones:
// signing in again would spend the per-address auth rate limit (10 per 5
// minutes) that the first test already exercises.
const stateFile = "test-results/.session.json";

// Optional screenshots of key screens, in both themes, for review.
const shotDir = process.env.XENIBASE_E2E_SCREENSHOTS;
/** Checks a screen against WCAG 2.1 A and AA with axe: serious and
 * critical issues fail the run (docs/ui-redesign.md, phase 6). With
 * XENIBASE_E2E_AXE=report they are written to axe.jsonl instead. */
async function a11y(page: Page, name: string, theme: string) {
  const r = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"]).analyze();
  const bad = r.violations
    .filter((v) => v.impact === "serious" || v.impact === "critical")
    .map((v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).slice(0, 5).join(" | ")}`);
  if (process.env.XENIBASE_E2E_AXE === "report") {
    if (shotDir && bad.length) fs.appendFileSync(`${shotDir}/axe.jsonl`, JSON.stringify({ name, theme, bad }) + "\n");
    return;
  }
  expect.soft(bad, `accessibility of ${name} (${theme})`).toEqual([]);
}

async function shot(page: Page, name: string) {
  for (const theme of ["dark", "light"]) {
    await page.evaluate((t) => {
      localStorage.setItem("xenibase.theme", t);
      document.documentElement.dataset.theme = t;
    }, theme);
    await page.waitForTimeout(400); // let colour transitions finish
    await a11y(page, name, theme);
    if (shotDir) await page.screenshot({ path: `${shotDir}/${name}-${theme}.png`, fullPage: true });
  }
  await page.evaluate(() => {
    localStorage.setItem("xenibase.theme", "dark");
    document.documentElement.dataset.theme = "dark";
  });
}

/** Opens the app with the session saved by the first test. */
// The shell's navigation (docs/ui-redesign.md): a project's old tabs are
// now rail sections, some with their own sidebar.
const projectSections: Record<string, [string, string?]> = {
  Overview: ["overview"],
  Connect: ["settings", "Connection"],
  Settings: ["settings", "General"],
  Members: ["settings", "Members"],
  SQL: ["sql"],
  Tables: ["tables"],
  "Database settings": ["settings", "Database"],
  Compute: ["settings", "Compute and tier"],
  Storage: ["settings", "Backup storage"],
  Extensions: ["database", "Extensions"],
  Migrations: ["database", "Migrations"],
  Logs: ["logs"],
  Metrics: ["reports"],
  Backups: ["database", "Backups"],
  Branches: ["database", "Branches"],
  Branch: ["database", "Branch"],
  Webhooks: ["database", "Webhooks"],
  Jobs: ["database", "Scheduled jobs"],
};

async function projectTab(page: Page, tab: string) {
  const [rail, link] = projectSections[tab];
  await page.getByTestId(`rail-${rail}`).click();
  // The rail expands over the page while hovered.
  await page.mouse.move(900, 500);
  if (link) await page.getByTestId("section-sidebar").getByRole("link", { name: link, exact: true }).click();
}

/** A platform admin page, switching to the platform area first. */
async function platformNav(page: Page, label: string) {
  const nav = page.getByRole("navigation", { name: "Platform" });
  if (!(await nav.isVisible())) {
    await page.getByTestId("org-switcher").click();
    await page.getByTestId("platform-link").click();
  }
  await nav.getByRole("link", { name: label, exact: true }).click();
  await page.mouse.move(900, 500);
}

// The organisation's pages: leave the project through the logo first.
async function orgNav(page: Page, label: string) {
  const nav = page.getByRole("navigation", { name: "Organisation" });
  if (!(await nav.isVisible())) await page.getByLabel("Xenibase home").click();
  await nav.getByRole("link", { name: label, exact: true }).click();
  await page.mouse.move(900, 500);
}

async function signOut(page: Page) {
  await page.getByTestId("user-menu").click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
}

async function switchOrg(page: Page, id: string) {
  await page.getByTestId("org-switcher").click();
  await page.getByTestId(`org-option-${id}`).click();
}

async function signedIn(page: Page) {
  await page.goto("/projects");
  await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
}

/** Confirms the password and a code when a sensitive action asks for them
 * (the step-up dialog appears only when the last one has expired). */
async function stepUpIfAsked(page: Page) {
  const d = page.getByTestId("step-up");
  try {
    await d.waitFor({ timeout: 3_000 });
  } catch {
    return;
  }
  await d.getByLabel("Your password").fill(password);
  await d.getByLabel("Your authenticator code").fill(await freshTotp(totpSecret));
  await d.getByRole("button", { name: "Confirm" }).click();
  await expect(d).toBeHidden();
}

async function count(url: string, sql: string): Promise<number> {
  const c = await connect(url);
  try {
    const r = await c.query(sql);
    return Number(Object.values(r.rows[0])[0]);
  } finally {
    await c.end();
  }
}

/** Connects as a client app would, with the URL copied from the UI. */
async function connect(url: string): Promise<pg.Client> {
  const u = new URL(url);
  const client = new pg.Client({
    host: dbAddr ?? u.hostname,
    port: Number(u.port),
    user: decodeURIComponent(u.username),
    password: decodeURIComponent(u.password),
    database: u.pathname.slice(1),
    ssl: caFile
      ? { ca: readFileSync(caFile, "utf8"), servername: u.hostname, rejectUnauthorized: true }
      : { rejectUnauthorized: false }, // sslmode=require: encrypted, unverified
    connectionTimeoutMillis: 10_000,
    // A query sent as a reset or restore holds clients at the pooler can
    // outlive that window on a connection the pooler dropped; fail it so
    // the caller's retry loop tries again instead of hanging.
    query_timeout: 15_000,
  });
  // A dropped connection (e.g. the project was deleted) must not crash the run.
  client.on("error", () => {});
  await client.connect();
  return client;
}

async function psql(url: string, sql: string): Promise<string> {
  const u = new URL(url);
  const conninfo = [
    `host=${u.hostname}`,
    dbAddr ? `hostaddr=${dbAddr}` : "",
    `port=${u.port}`,
    `user=${decodeURIComponent(u.username)}`,
    `password=${decodeURIComponent(u.password)}`,
    `dbname=${u.pathname.slice(1)}`,
    caFile ? `sslmode=verify-full sslrootcert=${caFile}` : "sslmode=require",
  ]
    .filter(Boolean)
    .join(" ");
  const { stdout } = await promisify(execFile)("psql", [conninfo, "-Atc", sql]);
  return stdout.trim();
}

async function hasPsql(): Promise<boolean> {
  try {
    await promisify(execFile)("psql", ["--version"]);
    return true;
  } catch {
    return false;
  }
}

/** The link in the latest email to addr whose body contains want. */
async function mailLink(addr: string, want: string): Promise<string> {
  let link = "";
  await expect(async () => {
    const res = await fetch(`${mailAPI}/messages?to=${encodeURIComponent(addr)}`);
    const msgs = (await res.json()) as { data: string }[];
    const m = [...msgs].reverse().find((x) => x.data.includes(want));
    const url = m?.data.match(/https?:\/\/\S+token=[A-Za-z0-9_-]+/);
    expect(url, `an email to ${addr} with ${want}`).toBeTruthy();
    link = url![0];
  }).toPass({ timeout: 30_000 });
  return link;
}

/** Opens an invitation link in a fresh browser and creates the account it invites. */
async function acceptInvitation(page: Page, link: string, name: string): Promise<string> {
  await page.goto(link);
  await expect(page.getByRole("heading", { name: /^Join / })).toBeVisible();
  await page.getByLabel("Your name").fill(name);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByLabel("Confirm password").fill(password);
  await page.getByTestId("accept-terms").check();
  await page.getByRole("button", { name: "Create account and join" }).click();
  // A new account signs in with its password alone: no authenticator is asked for.
  // (Wait for the move to /projects: a heading named "Projects" also matches the invitation page.)
  await expect(page).toHaveURL(/\/projects$/);
  await expect(page.getByRole("heading", { name: "Projects", exact: true })).toBeVisible();
  // It turns two-factor on from Account → Security, as a user does.
  await page.goto("/account");
  await expect(page.getByText("Signing in needs only your password.")).toBeVisible();
  await page.getByRole("button", { name: "Turn on" }).click();
  const dlg = page.getByRole("dialog");
  await dlg.getByLabel("Password").fill(password);
  await dlg.getByRole("button", { name: "Continue" }).click();
  await expect(dlg.getByAltText("TOTP QR code")).toBeVisible();
  const secret = (await dlg.getByTestId("totp-secret").textContent())!.trim();
  await dlg.getByLabel("Code").fill(await freshTotp(secret));
  await dlg.getByRole("button", { name: "Verify and turn on" }).click();
  await expect(dlg.getByTestId("recovery-codes")).toBeVisible();
  await dlg.getByTestId("codes-saved").check();
  await dlg.getByRole("button", { name: "Done" }).click();
  await expect(page.getByText("Signing in needs your password and a code.")).toBeVisible();
  await page.goto("/projects");
  await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();
  return secret;
}

/** The status of a GET from inside the page: its cookies, its DNS mapping. */
async function apiStatus(page: Page, path: string): Promise<number> {
  return page.evaluate(async (p) => (await fetch(p)).status, path);
}

async function revealedValue(page: Page, testId: string): Promise<string> {
  const code = page.getByTestId(testId);
  await expect(code).toBeVisible(); // isVisible() below does not wait
  const row = code.locator("..");
  const show = row.getByRole("button", { name: "Show" });
  if (await show.isVisible()) await show.click();
  return (await code.textContent())!.trim();
}

// The bundle's metadata database, for seeding history the tests cannot wait
// a week for.
const metadataContainer = process.env.XENIBASE_E2E_METADATA_CONTAINER ?? "xenibase-e2e-metadata-db-1";
async function metadataSQL(sql: string): Promise<string> {
  const { stdout } = await promisify(execFile)("docker", ["exec", metadataContainer, "psql", "-U", "xenibase", "-d", "xenibase", "-v", "ON_ERROR_STOP=1", "-Atc", sql]);
  return stdout.trim();
}

test.describe.configure({ mode: "serial" });

test("fresh install to a working database, entirely in the browser", async ({ page }) => {
  // 1. A fresh install sends every page to the setup wizard.
  await page.goto("/");
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole("heading", { name: "Create the platform admin account" })).toBeVisible();
  await shot(page, "01-setup");

  await page.getByLabel("Setup code").fill(setupCode);
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByLabel("Confirm password").fill(password);
  await page.getByRole("button", { name: "Continue" }).click();

  // 2. Enrol TOTP with the key the wizard shows (an authenticator app would scan the QR).
  await expect(page.getByRole("heading", { name: "Set up two-factor authentication" })).toBeVisible();
  await expect(page.getByAltText("TOTP QR code")).toBeVisible();
  await shot(page, "02-setup-totp");
  totpSecret = (await page.getByTestId("totp-secret").textContent())!.trim();
  expect(totpSecret).toMatch(/^[A-Z2-7]{32}$/);
  await page.getByLabel("Code").fill(await freshTotp(totpSecret));
  await page.getByRole("button", { name: "Verify and create account" }).click();

  // Recovery codes, shown once.
  await expect(page.getByTestId("recovery-codes")).toBeVisible();
  await expect(page.getByTestId("recovery-codes").locator("li")).toHaveCount(10);
  await page.getByTestId("codes-saved").check();
  await page.getByRole("button", { name: "Continue" }).click();

  // Email: SMTP is required, and saved only once a test goes out.
  await expect(page.getByRole("heading", { name: "Email" })).toBeVisible();
  await page.getByLabel("SMTP host").fill(smtpHost);
  await page.getByLabel("Port").fill(smtpPort);
  await page.getByLabel("Encryption").selectOption("none");
  await page.getByLabel("From address").fill("Xenibase <xenibase@xenibase.test>");
  await expect(page.getByLabel("Send the test to")).toHaveValue(email);
  await page.getByRole("button", { name: "Send a test, save, and continue" }).click();
  await expect(async () => {
    const res = await fetch(`${mailAPI}/messages?to=${encodeURIComponent(email)}`);
    expect(((await res.json()) as { data: string }[]).some((m) => m.data.includes("Xenibase email works"))).toBe(true);
  }).toPass({ timeout: 30_000 });

  // 3. The database hostname, with a DNS check.
  await expect(page.getByRole("heading", { name: "Database hostname" })).toBeVisible();
  await page.getByLabel("Hostname").fill(dbHost);
  await page.getByRole("button", { name: "Check DNS" }).click();
  await expect(page.getByText(/DNS (points at this server|does not point here yet)/)).toBeVisible();
  await page.getByRole("button", { name: "Save and continue" }).click();

  // 4. Backup storage, with the live write/read/delete test.
  await expect(page.getByRole("heading", { name: "Backup storage" })).toBeVisible();
  if (s3Endpoint) {
    await page.getByLabel("Endpoint").fill(s3Endpoint);
    await page.getByLabel("Region").fill("us-east-1");
    await page.getByLabel("Bucket").fill(s3Bucket);
    await page.getByLabel("Access key").fill("e2e-access");
    await page.getByLabel("Secret key").fill("e2e-secret");
    await page.getByLabel(/Path-style addressing/).check();
    await page.getByRole("button", { name: "Test only" }).click();
    await expect(page.getByText("Live test passed", { exact: true })).toBeVisible();
    await expect(page.getByTestId("storage-test-steps")).toContainText("delete");
    await shot(page, "09-setup-storage");
    await page.getByRole("button", { name: "Test, save, and continue" }).click();
  } else {
    await page.getByRole("button", { name: /Skip for now/ }).click();
  }

  // 5. The backup key must be downloaded and confirmed before continuing.
  await expect(page.getByRole("heading", { name: "Backup encryption key" })).toBeVisible();
  await page.getByRole("button", { name: "Generate backup key" }).click();
  await expect(page.getByRole("button", { name: "Confirm backup key" })).toBeDisabled();
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download key file" }).click()]);
  const keyFile = readFileSync((await download.path())!, "utf8");
  expect(keyFile).toMatch(/^xenibase-backup-key-v1:[A-Za-z0-9+/=]+$/m);
  await page.getByTestId("confirm-backup-key").fill("xenibase-backup-key-v1:" + "A".repeat(43) + "=");
  await page.getByRole("button", { name: "Confirm backup key" }).click();
  await expect(page.getByText("That is not this installation's backup key.")).toBeVisible();
  await page.getByLabel("Load key file").setInputFiles({ name: "key.txt", mimeType: "text/plain", buffer: Buffer.from(keyFile) });
  await shot(page, "10-setup-key");
  await page.getByRole("button", { name: "Confirm backup key" }).click();

  // 6. The bundle's agent registers the local node by itself.
  await expect(page.getByRole("heading", { name: "Local node" })).toBeVisible();
  if (s3Endpoint) {
    await expect(page.getByText("Agent connected")).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId("agent-status").first()).toContainText("healthy");
    await shot(page, "11-setup-node");
    await page.getByRole("button", { name: "Continue", exact: true }).click();
  } else {
    await page.getByRole("button", { name: /Continue/ }).click();
  }
  await expect(page.getByRole("heading", { name: "Xenibase is ready" })).toBeVisible();

  // The pooler certificate for the hostname arrives over ACME (HTTP-01).
  if (expectIssuer) {
    await page.goto("/settings");
    await expect(async () => {
      await page.reload();
      await expect(page.getByText(expectIssuer).first()).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 120_000 });
    await expect(page.getByText(dbHost, { exact: true }).first()).toBeVisible();
    await shot(page, "08-settings-tls");
    await page.goto("/projects/new");
  } else {
    // 7. Create the first project and watch it provision.
    await page.getByRole("button", { name: "Create your first project" }).click();
  }
  await expect(page).toHaveURL(/\/projects\/new$/);
  await page.getByLabel("Name").fill("My Blog");
  await page.getByRole("button", { name: "Create project" }).click();
  await expect(page.getByTestId("credential-panel")).toBeVisible();
  await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
  await expect(page.getByTestId("operation-log")).toContainText("connected through the transaction pooler");
  await shot(page, "03-created");

  const pooledURL = await revealedValue(page, "credential-pooled-url");
  const sessionURL = await revealedValue(page, "credential-session-url");
  expect(pooledURL).toContain(`@${dbHost}:`);
  expect(pooledURL).toContain("sslmode=require");

  // 5. The copied URLs work for an app, over TLS.
  const app = await connect(pooledURL);
  await app.query("CREATE TABLE posts (id serial PRIMARY KEY, title text NOT NULL)");
  await app.query("INSERT INTO posts (title) VALUES ($1)", ["Hello from the browser"]);
  const { rows } = await app.query("SELECT current_database() AS db, (SELECT count(*) FROM posts)::int AS n");
  expect(rows[0].db).toMatch(/^p_[a-z2-7]{10}$/);
  expect(rows[0].n).toBe(1);
  await app.end();
  if (await hasPsql()) {
    expect(await psql(sessionURL, "SELECT title FROM posts")).toBe("Hello from the browser");
  }

  await page.getByLabel("I've saved the password somewhere safe").check();
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("link", { name: "Open the project" }).click();
  await expect(page.getByRole("heading", { name: /My Blog/ })).toBeVisible();
  await shot(page, "04-overview");

  // 6. Connect page snippets carry the real host and database.
  await projectTab(page, "Connect");
  await expect(page.getByText(`psql 'postgresql://${rows[0].db}_owner:YOUR_PASSWORD@${dbHost}`)).toBeVisible();
  await shot(page, "05-connect");

  // 7. Change a guardrail; it applies to the live database.
  await projectTab(page, "Database settings");
  await page.getByLabel("Statement timeout").fill("15s");
  await page.getByRole("button", { name: "Save guardrails" }).click();
  await expect(page.getByText("Saved; applying to the database and pooler.")).toBeVisible();
  await expect(async () => {
    const c = await connect(sessionURL);
    const r = await c.query("SHOW statement_timeout");
    await c.end();
    expect(r.rows[0].statement_timeout).toBe("15s");
  }).toPass({ timeout: 30_000 });

  // 8. Rotate the password: the new one works, the old one stops.
  await page.getByRole("button", { name: "Rotate password" }).click();
  await expect(page.getByTestId("credential-panel")).toBeVisible();
  const rotatedURL = await revealedValue(page, "credential-pooled-url");
  expect(rotatedURL).not.toBe(pooledURL);
  await expect(async () => {
    const c = await connect(rotatedURL);
    await c.end();
  }).toPass({ timeout: 30_000 });
  await expect(connect(pooledURL)).rejects.toThrow();
  await page.getByLabel("I've saved the password somewhere safe").check();
  await page.getByRole("button", { name: "Done" }).click();

  // 9. Operations and the audit log recorded it all.
  await orgNav(page, "Operations");
  await expect(page.getByRole("link", { name: "rotate" }).first()).toBeVisible();
  await page.getByRole("link", { name: "create" }).first().click();
  await expect(page.getByTestId("operation-log")).toContainText("is active");
  await orgNav(page, "Audit log");
  for (const action of ["project.create", "project.update", "project.rotate_password"]) {
    await expect(page.getByRole("cell", { name: action, exact: true }).first()).toBeVisible();
  }
  await shot(page, "06-audit");
  await platformNav(page, "Platform audit");
  await expect(page.getByRole("cell", { name: "setup.complete", exact: true }).first()).toBeVisible();

  // 10. Sign out and back in with password + TOTP.
  await signOut(page);
  await expect(page).toHaveURL(/\/login/);
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("wrong password, clearly");
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByRole("alert")).toContainText("invalid email, password, or code");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByLabel("Code").fill(await freshTotp(totpSecret));
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();

  // 11. Delete needs the typed name plus password and code.
  await page.getByRole("link", { name: "My Blog", exact: true }).click();
  await projectTab(page, "Settings");
  await page.getByRole("button", { name: "Delete project" }).click();
  const dialog = page.getByRole("dialog");
  const confirm = dialog.getByRole("button", { name: "Delete project" });
  await expect(confirm).toBeDisabled();
  await shot(page, "07-delete-confirm");
  await dialog.getByTestId("confirm-name").fill("My Blog");
  await dialog.getByLabel("Your password").fill(password);
  await dialog.getByTestId("confirm-code").fill(await freshTotp(totpSecret));
  await confirm.click();
  await expect(page).toHaveURL(/\/projects$/);
  // A final backup runs first, so the database disappears after a moment.
  await expect(async () => {
    let c: pg.Client;
    try {
      c = await connect(rotatedURL);
    } catch {
      return;
    }
    await c.end();
    throw new Error("the deleted project still accepts connections");
  }).toPass({ timeout: 60_000 });
  await expect(page.getByText("No projects yet")).toBeVisible({ timeout: 30_000 });
  await page.context().storageState({ path: stateFile });
});

// The M3 "done when" (spec §14): delete data, restore to a new project, and
// verify it; then import a Supabase project and serve its app from Xenibase.
// (A hosted Supabase project is not reachable from CI, so the import source
// is the supabase/postgres image with a Supabase-shaped app in it.)
test.describe("with the saved session", () => {
  test.use({ storageState: stateFile });

  test("back up, delete data, restore and verify; import a Supabase project and serve its app", async ({ page }) => {
    test.skip(!s3Endpoint || !supabaseSeedURL || !supabaseURL, "needs the e2e bundle's fake S3 and Supabase source");
    await signedIn(page);

    // 1. A project with data.
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Notes");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const notesURL = await revealedValue(page, "credential-pooled-url");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    const app = await connect(notesURL);
    await app.query("CREATE TABLE notes (id serial PRIMARY KEY, body text NOT NULL)");
    await app.query("INSERT INTO notes (body) SELECT 'note ' || g FROM generate_series(1, 50) g");
    await app.end();

    // 2. Back it up from the Backups tab.
    await page.getByRole("link", { name: "Open the project" }).click();
    await projectTab(page, "Backups");
    await expect(page.getByTestId("last-backup")).toHaveText("never");
    await page.getByRole("button", { name: "Back up now" }).click();
    await expect(page.getByTestId("backup-row").first()).toContainText("succeeded", { timeout: 60_000 });
    await expect(page.getByTestId("backup-row").first()).toContainText("Nightly / manual");
    await page.reload();
    await expect(page.getByTestId("last-backup")).toContainText(/just now|s ago/);
    await shot(page, "12-backups");

    // 3. Delete data.
    expect(await count(notesURL, "DELETE FROM notes WHERE id > 5 RETURNING 1")).toBe(1);
    expect(await count(notesURL, "SELECT count(*) FROM notes")).toBe(5);

    // 4. Restore into a new project and verify it.
    await page.getByTestId("backup-row").first().getByRole("button", { name: "Restore" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByLabel("New project name")).toHaveValue("Notes restored");
    await shot(page, "13-restore-dialog");
    await dialog.getByRole("button", { name: "Restore into new project" }).click();
    await expect(page.getByTestId("credential-panel")).toBeVisible();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId("operation-log")).toContainText("restored in");
    const restoredURL = await revealedValue(page, "credential-pooled-url");
    expect(await count(restoredURL, "SELECT count(*) FROM notes")).toBe(50);
    expect(await count(restoredURL, "SELECT max(id) FROM notes")).toBe(50);
    expect(await count(notesURL, "SELECT count(*) FROM notes")).toBe(5); // the original is untouched
    await shot(page, "14-restored");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();

    // 5. Restore in place: typed name, password and code; same URL afterwards.
    await page.goto("/projects");
    await page.getByRole("link", { name: "Notes", exact: true }).click();
    await projectTab(page, "Backups");
    await page.getByTestId("backup-row").last().getByRole("button", { name: "Restore" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Restore in place instead…" }).click();
    const confirm = page.getByRole("dialog").getByRole("button", { name: "Restore in place" });
    await expect(confirm).toBeDisabled();
    await page.getByRole("dialog").getByTestId("confirm-name").fill("Notes");
    await page.getByRole("dialog").getByLabel("Your password").fill(password);
    await page.getByRole("dialog").getByTestId("confirm-code").fill(await freshTotp(totpSecret));
    await confirm.click();
    await expect(async () => {
      expect(await count(notesURL, "SELECT count(*) FROM notes")).toBe(50);
    }).toPass({ timeout: 60_000 });
    // A safety backup of the 5-row state was taken first.
    await expect(page.getByText("Safety (before restore)")).toBeVisible({ timeout: 30_000 });

    // 6. The projects list shows the last backup.
    await page.goto("/projects");
    const row = page.getByTestId("project-card").filter({ has: page.getByRole("link", { name: "Notes", exact: true }) });
    await expect(row.getByTestId("last-backup-cell")).not.toContainText("never");
    await shot(page, "15-projects-last-backup");

    // 7. Import a Supabase project.
    const seed = new pg.Client({ connectionString: supabaseSeedURL });
    await seed.connect();
    await seed.query(readFileSync(fileURLToPath(new URL("./supabase-fixture.sql", import.meta.url)), "utf8"));
    await seed.end();

    // The source is on the bundle's private network, which imports refuse
    // unless the platform admin allow-lists the host for the organisation.
    const mine = (await page.evaluate(async () => (await fetch("/api/v1/projects")).json())) as { items: { name: string; org_id: string }[] };
    const notesOrg = mine.items.find((p) => p.name === "Notes")!.org_id;
    await page.goto(`/admin/orgs/${notesOrg}`);
    await page.getByTestId("outbound-allowlist").fill(new URL(supabaseURL!).hostname);
    await page.getByRole("button", { name: "Save allow-list" }).click();
    await expect(page.getByRole("button", { name: "Save allow-list" })).toBeDisabled();
    await page.goto("/projects");

    await page.getByRole("link", { name: "Import" }).click();
    await page.getByLabel("Source connection string").fill(supabaseURL!);
    await page.getByRole("button", { name: "Run preflight" }).click();
    await expect(page.getByText("Supabase project")).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId("preflight-version")).toContainText("17");
    await expect(page.getByLabel("Import schema public")).toBeChecked();
    await expect(page.getByLabel("Import schema app")).toBeChecked();
    await expect(page.getByLabel("Import schema auth")).not.toBeChecked();
    await expect(page.getByLabel("Import schema storage")).not.toBeChecked();
    await expect(page.getByRole("cell", { name: "own todos" })).toBeVisible();
    await shot(page, "16-import-preflight");
    await page.getByLabel("Project name").fill("Todo app");
    await page.getByRole("button", { name: "Import 2 schemas" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 120_000 });
    await expect(page.getByTestId("operation-log")).toContainText("verified: 2 table(s), 122 row(s)");
    await shot(page, "17-imported");
    const todoURL = await revealedValue(page, "credential-pooled-url");
    expect(todoURL).toContain(`@${dbHost}:`);

    // 8. The app runs against Xenibase with nothing but the new URL.
    const todo = await startTodoApp(() => connect(todoURL));
    try {
      const ada = "00000000-0000-0000-0000-000000000001";
      const list = (await (await fetch(`${todo.url}/todos?user=${ada}`)).json()) as { id: number }[];
      expect(list).toHaveLength(60);
      expect(await (await fetch(`${todo.url}/profile?user=${ada}`)).json()).toEqual({ username: "ada" });
      const created = (await (await fetch(`${todo.url}/todos`, { method: "POST", body: JSON.stringify({ user: ada, task: "written on Xenibase" }) })).json()) as {
        id: number;
        ref: string;
      };
      expect(created.id).toBe(121); // the identity sequence carried over
      expect(created.ref).toMatch(/^[0-9a-f-]{36}$/); // extensions.uuid_generate_v4() works
      expect(await (await fetch(`${todo.url}/open`)).json()).toEqual([{ open: 81 }]);
    } finally {
      await todo.close();
    }
    const src = new pg.Client({ connectionString: supabaseSeedURL });
    await src.connect();
    expect((await src.query("SELECT count(*)::int AS n FROM public.todos")).rows[0].n).toBe(120); // the source was not modified
    await src.end();
  });

  // The M4 "done when" (spec §14): a dedicated project works through the
  // same pooler URL format, and a point-in-time restore succeeds, through
  // the browser. The bundle's local node runs dedicated instances too.
  test("a dedicated project through the same pooler URL format, and a point-in-time restore", async ({ page }) => {
    test.skip(!s3Endpoint, "needs the e2e bundle (Docker for instances, fake S3 for WAL-G)");
    await signedIn(page);

    // A shared project's URL, for comparison.
    const urlShape = (u: string) => {
      const x = new URL(u);
      return `${x.protocol}//${x.host}?${x.searchParams.toString()}`;
    };
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Shape check");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const sharedURL = await revealedValue(page, "credential-pooled-url");

    // 1. Create on the dedicated tier.
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Orders Pro");
    await page.getByLabel("Dedicated").check();
    await expect(page.getByLabel("Size")).toContainText("small");
    await page.getByLabel("Volume (GB)").fill("5");
    await shot(page, "18-new-dedicated");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 180_000 });
    await expect(page.getByTestId("operation-log")).toContainText("archiving WAL to s3://");
    await expect(page.getByTestId("operation-log")).toContainText("base backup base_");
    const pooledURL = await revealedValue(page, "credential-pooled-url");
    expect(urlShape(pooledURL)).toBe(urlShape(sharedURL)); // same host, ports, sslmode
    expect(new URL(pooledURL).pathname).toMatch(/^\/p_[a-z2-7]{10}$/);
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();

    // 2. The app writes through the pooler; remember a moment, then damage.
    const app = await connect(pooledURL);
    await app.query("CREATE TABLE orders (id bigserial PRIMARY KEY, item text NOT NULL)");
    await app.query("INSERT INTO orders (item) SELECT 'order ' || g FROM generate_series(1, 30) g");
    await new Promise((r) => setTimeout(r, 2_000));
    const target = new Date(Math.floor(Date.now() / 1000) * 1000); // datetime-local has second precision
    await new Promise((r) => setTimeout(r, 2_000));
    await app.query("DELETE FROM orders WHERE id > 3");
    await app.end();
    expect(await count(pooledURL, "SELECT count(*) FROM orders")).toBe(3);

    // 3. The instance card and the node it runs on.
    await page.getByRole("link", { name: "Open the project" }).click();
    await expect(page.getByTestId("instance-card")).toContainText("small");
    await expect(page.getByTestId("instance-card")).toContainText("5 GB volume");
    await shot(page, "19-dedicated-overview");
    // V4-M35: the read replicas card (none yet: a replica needs another node).
    await projectTab(page, "Compute");
    await expect(page.getByTestId("replicas")).toContainText("Copies on other nodes that serve reads");
    await expect(page.getByRole("button", { name: "Add replica…" })).toBeVisible();

    // 4. Point-in-time recovery into a new project, from the Backups tab.
    await projectTab(page, "Backups");
    await expect(page.getByTestId("backup-row").first()).toContainText("Base backup (WAL-G)");
    await expect(page.getByTestId("pitr-window")).toBeVisible();
    const pad = (n: number) => String(n).padStart(2, "0");
    const local = `${target.getFullYear()}-${pad(target.getMonth() + 1)}-${pad(target.getDate())}T${pad(target.getHours())}:${pad(target.getMinutes())}:${pad(target.getSeconds())}`;
    // Chromium serializes whole minutes without ":00", and fill() checks
    // the value reads back the same.
    await page.getByLabel("Restore to (your local time)").fill(local.replace(/:00$/, ""));
    await page.getByLabel("New project name").fill("Orders Pro restored");
    await shot(page, "20-pitr");
    await page.getByRole("button", { name: "Restore to this point" }).click();
    await expect(page.getByTestId("credential-panel")).toBeVisible();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 240_000 });
    await expect(page.getByTestId("operation-log")).toContainText("recovery finished and promoted");
    const restoredURL = await revealedValue(page, "credential-pooled-url");
    expect(urlShape(restoredURL)).toBe(urlShape(sharedURL));
    expect(await count(restoredURL, "SELECT count(*) FROM orders")).toBe(30);
    expect(await count(pooledURL, "SELECT count(*) FROM orders")).toBe(3); // the source is unchanged
    await shot(page, "21-pitr-restored");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();

    // 5. The node page lists both dedicated instances.
    await platformNav(page, "Nodes");
    await page.getByRole("link", { name: "local" }).click();
    await expect(page.getByTestId("node-docker")).toHaveText("ok");
    await expect(page.getByTestId("instance-row").filter({ hasText: "dedicated" })).toHaveCount(2);
    // M18: each instance shows its Postgres release (V3 §2.4).
    await expect(page.getByTestId("instance-version").first()).toContainText("18");
    await shot(page, "22-node");
    // V3.1-M1: the node's failure domain (rack), shown on the Nodes page.
    await page.getByLabel("Failure domain").fill("rack-a");
    await page.getByRole("button", { name: "Save" }).first().click();
    await expect(page.getByText("Now: rack-a.")).toBeVisible();
    // The maintenance window for minor releases, on the Nodes page.
    await platformNav(page, "Nodes");
    await expect(page.getByTestId("node-domain").first()).toHaveText("rack-a");
    await expect(page.getByTestId("maintenance")).toBeVisible();
  });
  // The M5 "done when" (spec §14): a hobby project is promoted with its URL
  // unchanged and no lost commits, while an app keeps writing; then (M14,
  // V2 §5) demoted back the same way.
  test("promote a hobby project with a live writer, then demote it: same URL, no lost commits", async ({ page }) => {
    test.skip(!s3Endpoint, "needs the e2e bundle (Docker for instances, fake S3 for WAL-G)");
    await signedIn(page);

    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Hobby promo");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const pooledURL = await revealedValue(page, "credential-pooled-url");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    const seed = await connect(pooledURL);
    await seed.query("CREATE TABLE events (id bigserial PRIMARY KEY, n int UNIQUE NOT NULL)");
    await seed.query("CREATE TABLE notes AS SELECT g AS id, 'note ' || g AS body FROM generate_series(1, 5000) g");
    await seed.end();

    // A live writer on the pooled URL, as an app would be.
    const acked: number[] = [];
    const errors: string[] = [];
    let stop = false;
    const writer = (async () => {
      let c: pg.Client | null = null;
      for (let n = 1; !stop; n++) {
        try {
          c ??= await connect(pooledURL);
          await c.query("INSERT INTO events (n) VALUES ($1)", [n]);
          acked.push(n);
        } catch (e) {
          errors.push(String(e));
          await c?.end().catch(() => {});
          c = null;
          await new Promise((r) => setTimeout(r, 100));
        }
        await new Promise((r) => setTimeout(r, 10));
      }
      await c?.end().catch(() => {});
    })();
    await expect.poll(() => acked.length).toBeGreaterThan(20);

    // The wizard: target, size, the estimate, then live progress.
    await page.getByRole("link", { name: "Open the project" }).click();
    await projectTab(page, "Compute");
    await expect(page.getByTestId("pg-version")).toHaveText("Postgres 18"); // M18: the newest supported, nothing to upgrade
    await page.getByRole("button", { name: "Promote…" }).click();
    await expect(page.getByText(/Estimated write freeze: about \d+ s/)).toBeVisible();
    await expect(page.getByTestId("promote-estimate")).toContainText("The database is");
    await page.getByLabel("Volume size (GB)").fill("5");
    await shot(page, "23-promote-wizard");
    await page.getByRole("button", { name: "Promote now" }).click();
    await expect(page.getByTestId("promote-done")).toBeVisible({ timeout: 240_000 });
    await expect(page.getByTestId("operation-log")).toContainText("logical replication");
    await expect(page.getByTestId("operation-log")).toContainText("writes were paused for");
    await shot(page, "24-promoted");

    // The writer keeps going on the new instance, then stops.
    const after = acked.length;
    await expect.poll(() => acked.length, { timeout: 30_000 }).toBeGreaterThan(after + 20);
    stop = true;
    await writer;

    // Same URL, now on the dedicated instance, with every acknowledged commit.
    const c = await connect(pooledURL);
    const archive = (await c.query("SELECT current_setting('archive_mode') AS a")).rows[0].a;
    expect(archive).toBe("on");
    const have = new Set((await c.query("SELECT n FROM events")).rows.map((r: { n: number }) => r.n));
    expect(acked.filter((n) => !have.has(n))).toEqual([]);
    expect(Number((await c.query("SELECT count(*) AS n FROM notes")).rows[0].n)).toBe(5000);
    await c.end();
    expect(errors).toEqual([]); // the pooled URL waited out the freeze

    await projectTab(page, "Overview");
    await expect(page.getByText("Promoted to the dedicated tier")).toBeVisible();
    await expect(page.getByTestId("instance-card")).toBeVisible();
    await shot(page, "25-promoted-overview");

    // Demotion: the preflight checklist, what resets, then live progress,
    // with the writer going again.
    stop = false;
    const before = acked.length;
    const writer2 = (async () => {
      let w: pg.Client | null = null;
      for (let n = 1_000_000; !stop; n++) {
        try {
          w ??= await connect(pooledURL);
          await w.query("INSERT INTO events (n) VALUES ($1)", [n]);
          acked.push(n);
        } catch (e) {
          errors.push(String(e));
          await w?.end().catch(() => {});
          w = null;
          await new Promise((r) => setTimeout(r, 100));
        }
        await new Promise((r) => setTimeout(r, 10));
      }
      await w?.end().catch(() => {});
    })();
    await expect.poll(() => acked.length).toBeGreaterThan(before + 20);
    await projectTab(page, "Compute");
    await page.getByRole("button", { name: "Demote…" }).click();
    await expect(page.getByTestId("demote-checks").locator("li")).toHaveCount(7);
    for (const name of ["size", "extensions", "roles", "allowance", "capacity"]) {
      await expect(page.getByTestId(`demote-check-${name}`)).toHaveAttribute("data-status", "ok");
    }
    await expect(page.getByTestId("demote-resets")).toContainText("connection limit 90 → 20");
    await expect(page.getByText("Point-in-time recovery ends at the demotion")).toBeVisible();
    await shot(page, "26-demote-wizard");
    await page.getByRole("button", { name: "Demote now" }).click();
    await expect(page.getByTestId("demote-done")).toBeVisible({ timeout: 240_000 });
    await expect(page.getByTestId("operation-log")).toContainText("route switched to the shared cluster");
    await expect(page.getByTestId("operation-log")).toContainText("dedicated instance stopped");
    await shot(page, "27-demoted");
    const after2 = acked.length;
    await expect.poll(() => acked.length, { timeout: 30_000 }).toBeGreaterThan(after2 + 20);
    stop = true;
    await writer2;

    // Same URL, back on the shared cluster, with every acknowledged commit.
    const d = await connect(pooledURL);
    expect((await d.query("SELECT current_setting('archive_mode') AS a")).rows[0].a).not.toBe("on");
    const all = new Set((await d.query("SELECT n FROM events")).rows.map((r: { n: number }) => r.n));
    expect(acked.filter((n) => !all.has(n))).toEqual([]);
    await d.end();
    expect(errors).toEqual([]);

    await projectTab(page, "Overview");
    await expect(page.getByText("Demoted to the shared tier")).toBeVisible();
    await projectTab(page, "Backups");
    await expect(page.getByTestId("backup-row").filter({ hasText: "Dedicated (pre-demotion)" }).first()).toBeVisible();
    await shot(page, "28-demoted-backups");
  });
  test("inspect and query a project, and see its size and connection trends, without leaving the UI", async ({ page }) => {
    await signedIn(page);
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Insight");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const pooledURL = await revealedValue(page, "credential-pooled-url");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    // An app holding a pooled connection, for the connection charts.
    const app = await connect(pooledURL);
    await page.getByRole("link", { name: "Open the project" }).click();

    // SQL Editor: a new saved query, run as the project role (Ctrl+Enter).
    const editor = page.getByTestId("sql-editor-host");
    const runSQL = async (sql: string) => {
      await editor.click();
      await page.keyboard.press("ControlOrMeta+a");
      await page.keyboard.insertText(sql);
      await page.keyboard.press("ControlOrMeta+Enter");
    };
    await projectTab(page, "SQL");
    await expect(page.getByText("Queries run against the live database")).toBeVisible();
    await page.getByTestId("create-query").click();
    await expect(page.getByTestId("query-name")).toHaveText("Untitled query");
    await runSQL(
      "CREATE TABLE notes (id serial PRIMARY KEY, body text NOT NULL);\n" +
        "INSERT INTO notes (body) SELECT 'note ' || g FROM generate_series(1, 120) g;\n" +
        "SELECT current_user AS who, count(*) AS n FROM notes;",
    );
    const results = page.getByTestId("sql-results");
    await expect(results).toContainText("3 statements");
    await expect(results).toContainText("INSERT 0 120");
    await expect(page.getByTestId("sql-grid").last()).toContainText("120");
    await expect(page.getByTestId("sql-grid").last()).toContainText("_owner");
    await shot(page, "26-sql-console");

    // CSV export of the displayed rows.
    await runSQL("SELECT id, body FROM notes ORDER BY id LIMIT 3");
    await expect(page.getByTestId("sql-grid")).toContainText("note 3");
    await page.getByRole("button", { name: "Export" }).click();
    const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("menuitem", { name: "Download CSV" }).click()]);
    expect(readFileSync((await dl.path())!, "utf8")).toBe("id,body\r\n1,note 1\r\n2,note 2\r\n3,note 3\r\n");

    // Errors point at the problem; Cancel stops a running query.
    await runSQL("SELECT * FROM missing_table");
    await expect(page.getByTestId("sql-error")).toContainText('relation "missing_table" does not exist');
    await expect(page.getByTestId("sql-error")).toContainText("LINE 1, COLUMN 15");
    await runSQL("SELECT pg_sleep(60)");
    await page.getByTestId("sql-cancel").click();
    await expect(page.getByTestId("sql-error")).toContainText("canceling statement due to user request", { timeout: 15_000 });
    // The history is this browser's own.
    await page.getByRole("button", { name: "History" }).click();
    await expect(page.getByRole("button", { name: "SELECT pg_sleep(60)" })).toBeVisible();

    // The query is saved as you type: rename it, share it, and it is there
    // after a reload.
    await expect(page.getByTestId("save-status")).toHaveText("Saved");
    await page.getByTestId("query-name").click();
    await page.getByTestId("rename-input").fill("Sleepy report");
    await page.getByTestId("rename-save").click();
    await page.getByRole("button", { name: "Sleepy report actions" }).click();
    await page.getByTestId("share-query").click();
    await expect(page.getByTestId("shared-queries")).toContainText("Sleepy report");
    await page.reload();
    await expect(page.getByTestId("query-name")).toHaveText("Sleepy report");
    await expect(editor).toContainText("pg_sleep(60)");

    // The read-only toggle (project settings) refuses writes in the console.
    await projectTab(page, "Database settings");
    await page.getByLabel("SQL console is read-only").click();
    await page.getByRole("button", { name: "Save guardrails" }).click();
    await expect(page.getByText("Saved")).toBeVisible({ timeout: 30_000 });
    await projectTab(page, "SQL");
    await expect(page.getByText("read-only", { exact: true }).first()).toBeVisible();
    await runSQL("DELETE FROM notes");
    await expect(page.getByTestId("sql-error")).toContainText("cannot execute DELETE in a read-only transaction");
    expect(await count(pooledURL, "SELECT count(*) FROM notes")).toBe(120);

    // Extensions: enable pg_stat_statements for the top-queries table.
    await projectTab(page, "Extensions");
    await page.getByTestId("ext-pg_stat_statements").getByRole("button", { name: "Enable" }).click();
    await expect(page.getByTestId("ext-pg_stat_statements")).toContainText("enabled");
    await expect(page.getByTestId("ext-postgis")).toContainText("dedicated tier only");
    await shot(page, "27-extensions");

    // Table editor: the sidebar, the record count, numbered pages, and
    // the table's definition.
    await projectTab(page, "Tables");
    await page.getByTestId("schema-tree").getByRole("button", { name: "notes", exact: true }).click();
    await expect(page.getByTestId("record-count")).toHaveText("120 records");
    const grid = page.getByTestId("table-grid");
    await expect(grid).toContainText("note 1");
    await expect(page.getByTestId("page-number")).toHaveText("of 2");
    await page.getByRole("button", { name: "Next page" }).click();
    await expect(page.getByTestId("page-input")).toHaveValue("2");
    await expect(grid).toContainText("note 101");
    // The grid draws only the rows in view: scroll to the last one.
    await grid.getByRole("grid").evaluate((el) => (el.scrollTop = el.scrollHeight));
    await expect(grid).toContainText("note 120");
    await expect(page.getByRole("button", { name: "Next page" })).toBeDisabled();
    await shot(page, "28-table-browser");
    await page.getByRole("button", { name: "Previous page" }).click();
    await expect(page.getByTestId("page-input")).toHaveValue("1");
    await page.getByRole("radio", { name: "Definition" }).click();
    await expect(page.getByTestId("table-definition-host")).toContainText('CREATE TABLE "public"."notes"');
    await page.getByRole("radio", { name: "Data" }).click();

    // Traffic, then the metrics charts: size and connection trends.
    for (let i = 0; i < 30; i++) await app.query("SELECT count(*) FROM notes");
    await projectTab(page, "Metrics");
    await expect(page.getByRole("img", { name: "Database size" })).toBeVisible();
    await expect(async () => {
      await page.reload();
      await expect(page.getByTestId("latest-size_bytes")).toContainText(/\d+(\.\d+)? (KiB|MiB)/, { timeout: 2_000 });
      await expect(page.getByTestId("latest-connections_active")).toContainText(/pooler clients [1-9]/, { timeout: 2_000 });
    }).toPass({ timeout: 60_000 });
    await expect(page.getByRole("img", { name: "Connections" })).toBeVisible();
    await expect(async () => {
      await page.reload();
      await expect(page.getByRole("cell", { name: /SELECT count\(\*\) FROM notes/ })).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 30_000 });
    await page.getByRole("radio", { name: "Last 24 hours" }).click();
    await expect(page.getByTestId("latest-size_bytes")).toContainText(/(KiB|MiB)/);
    await shot(page, "29-metrics");
    await app.end();

    // The node behind it has its own charts.
    await page.goto("/nodes");
    await page.getByRole("link", { name: "local" }).first().click();
    await expect(async () => {
      await page.reload();
      await expect(page.getByTestId("latest-cpu_percent")).toContainText("%", { timeout: 2_000 });
    }).toPass({ timeout: 60_000 });
    await shot(page, "30-node-metrics");

    // The tenant-isolation check against the bundle's live shared cluster
    // (its pg_hba.conf, every project, two throwaway tenants), and alerts.
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Alerts", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Check now" }).click();
    await expect(page.getByTestId("isolation-row").first()).toContainText("succeeded", { timeout: 90_000 });
    await shot(page, "31-settings-alerts");
    await platformNav(page, "Alerts");
    await expect(page.getByRole("heading", { name: "Alerts", exact: true })).toBeVisible();
    // The poolers starting after the server is not "pooler down" (other
    // alerts, like a full disk on the test host, may be real).
    await page.getByRole("radio", { name: "firing" }).click();
    await expect(page.getByText(/Nothing is wrong|Alert/).first()).toBeVisible();
    await expect(page.getByText("Pooler down")).toHaveCount(0);
    await shot(page, "32-alerts");
  });

  // The M8 "done when" (V2 §16): two users in two organisations see only
  // their own projects; an invited read-only member gets working read-only
  // credentials; removing them revokes everything at once.
  test("organisations: invite a read-only member, keep orgs apart, remove the member", async ({ page, browser }) => {
    await signedIn(page);
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Team data");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const appURL = await revealedValue(page, "credential-pooled-url");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    await page.getByRole("link", { name: "Open the project" }).click();
    await expect(page).toHaveURL(/\/projects\/[0-9a-f-]{36}/);
    const teamID = new URL(page.url()).pathname.split("/")[2];
    const app = await connect(appURL);
    await app.query("CREATE TABLE facts (id int PRIMARY KEY, body text); INSERT INTO facts VALUES (1, 'shared')");
    await app.end();

    // Invite carol into the organisation, read-only on this one project.
    const carolEmail = "carol@example.com";
    await page.goto("/org/members");
    await page.getByTestId("invite-member").click();
    await page.getByLabel("Email").fill(carolEmail);
    await page.getByLabel("Organisation role").selectOption("member");
    await page.getByLabel("Access to Team data").selectOption("read_only");
    await page.getByRole("button", { name: "Send invitation" }).click();
    await expect(page.getByTestId("invitation-created")).toBeVisible();
    await shot(page, "33-invitation");
    await page.getByRole("button", { name: "Done" }).click();

    const carolCtx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const carol = await carolCtx.newPage();
    await acceptInvitation(carol, await mailLink(carolEmail, "invitation"), "Carol");
    await expect(carol.getByRole("link", { name: "Team data" })).toBeVisible();
    await carol.goto(`/projects/${teamID}/members`);
    await expect(carol.getByText("read-only", { exact: true }).first()).toBeVisible();
    await carol.getByTestId("get-credentials").click();
    const carolURL = await revealedValue(carol, "my-pooled-url");
    expect(decodeURIComponent(new URL(carolURL).username)).toMatch(/_u_/);
    await shot(carol, "34-my-credentials");
    const carolDB = await connect(carolURL);
    expect((await carolDB.query("SELECT body FROM facts")).rows).toEqual([{ body: "shared" }]);
    await expect(carolDB.query("INSERT INTO facts VALUES (2, 'nope')")).rejects.toThrow(/read-only transaction/);

    // Bob, invited to the platform only, has his own organisation.
    const bobEmail = "bob@example.com";
    await page.goto("/admin/users");
    await page.getByTestId("invite-user").click();
    await page.getByLabel("Email").fill(bobEmail);
    await page.getByRole("button", { name: "Send invitation" }).click();
    await expect(page.getByTestId("invitation-created")).toBeVisible();
    await page.getByRole("button", { name: "Done" }).click();
    const bobCtx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const bob = await bobCtx.newPage();
    await acceptInvitation(bob, await mailLink(bobEmail, "invitation"), "Bob");
    await expect(bob.getByRole("link", { name: "Team data" })).toHaveCount(0);
    await bob.goto("/projects/new");
    await bob.getByLabel("Name").fill("Bobs app");
    await bob.getByRole("button", { name: "Create project" }).click();
    await expect(bob.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    await bob.getByLabel("I've saved the password somewhere safe").check();
    await bob.getByRole("button", { name: "Done" }).click();
    await bob.getByRole("link", { name: "Open the project" }).click();
    await expect(bob).toHaveURL(/\/projects\/[0-9a-f-]{36}/);
    const bobID = new URL(bob.url()).pathname.split("/")[2];

    // Neither sees the other's project, in the UI or the API: 404, not 403.
    await page.goto("/projects");
    await expect(page.getByRole("link", { name: "Team data" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Bobs app" })).toHaveCount(0);
    await page.goto(`/projects/${bobID}`);
    await expect(page.getByTestId("project-not-found")).toBeVisible();
    expect(await apiStatus(page, `/api/v1/projects/${bobID}`)).toBe(404);
    await bob.goto(`/projects/${teamID}`);
    await expect(bob.getByTestId("project-not-found")).toBeVisible();
    expect(await apiStatus(bob, `/api/v1/projects/${teamID}`)).toBe(404);
    expect(await apiStatus(bob, `/api/v1/projects/${teamID}/members`)).toBe(404);
    await shot(bob, "35-not-found");

    // Removing carol from the organisation revokes everything immediately.
    await page.goto("/org/members");
    await page.getByTestId(`remove-${carolEmail}`).click();
    await expect(page.getByTestId(`member-${carolEmail}`)).toHaveCount(0);
    await expect(carolDB.query("SELECT 1")).rejects.toThrow();
    await expect(connect(carolURL).then((c) => c.query("SELECT 1"))).rejects.toThrow();
    await carol.goto(`/projects/${teamID}`);
    await expect(carol.getByTestId("project-not-found")).toBeVisible();
    expect(await apiStatus(carol, `/api/v1/projects/${teamID}`)).toBe(404);
    await carolCtx.close();
    await bobCtx.close();
  });

  test("usage: a week of hourly storage on the Usage page", async ({ page }) => {
    await signedIn(page);
    // A new organisation, so only this project's storage is counted.
    await page.getByTestId("org-switcher").click();
    await page.getByTestId("new-org").click();
    await page.getByLabel("Name").fill("Metered team");
    await page.getByRole("button", { name: "Create organisation" }).click();
    await expect(page.getByTestId("org-switcher-name")).toHaveText("Metered team");
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Metered");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    expect(decodeURIComponent(new URL(await revealedValue(page, "credential-pooled-url")).username)).toMatch(/^p_[a-z2-7]{10}_owner$/);
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    await page.getByRole("link", { name: "Open the project" }).click();
    await expect(page).toHaveURL(/\/projects\/[0-9a-f-]{36}/);
    const id = new URL(page.url()).pathname.split("/")[2];

    // A week of size samples ending at the current hour: hour i averages
    // (i+1) x 100 MB. Older hours exist only as hourly points; the last day
    // has minute points. Don't let the hour turn between seeding and looking.
    while (Number(await metadataSQL("SELECT extract(minute FROM now())::int")) >= 57) await page.waitForTimeout(10_000);
    await metadataSQL(`
      BEGIN;
      DELETE FROM metric_points WHERE scope = 'project' AND scope_id = '${id}' AND ts < date_trunc('hour', now());
      DELETE FROM usage_records WHERE project_id = '${id}';
      INSERT INTO metric_points (scope, scope_id, metric, ts, resolution, value)
        SELECT 'project', '${id}', 'size_bytes', date_trunc('hour', now()) - interval '168 hours' + g * interval '1 hour', '1h', (g + 1) * 1e8
        FROM generate_series(0, 143) g;
      INSERT INTO metric_points (scope, scope_id, metric, ts, resolution, value)
        SELECT 'project', '${id}', 'size_bytes', date_trunc('hour', now()) - interval '24 hours' + m * interval '1 minute', '1m',
               (144 + m / 60 + 1) * 1e8 + CASE WHEN m % 2 = 0 THEN 5e6 ELSE -5e6 END
        FROM generate_series(0, 24 * 60 - 1) m;
      DELETE FROM settings WHERE key = 'usage.watermark';
      COMMIT;`);

    // The tenancy sweep (every 5 s in the bundle) records the week.
    await page.goto("/org/usage");
    await expect(page.getByRole("heading", { name: "Usage & quotas" })).toBeVisible();
    await expect(page.getByTestId("quota-projects")).toContainText("1 of");
    await page.getByTestId("usage-range").selectOption("30d");
    await expect(async () => {
      await page.reload();
      await page.getByTestId("usage-range").selectOption("30d");
      await expect(page.getByTestId("usage-hours-toggle")).toHaveText("168 hours recorded", { timeout: 2_000 });
    }).toPass({ timeout: 60_000 });
    await page.getByTestId("usage-hours-toggle").click();
    const totals = page.getByTestId("usage-hour-total");
    await expect(totals).toHaveCount(168);
    await expect(totals.first()).toHaveText("0.1");
    await expect(totals.nth(99)).toHaveText("10");
    await expect(totals.last()).toHaveText("16.8");
    // 0.1 x (1 + 2 + ... + 168) GB-hours.
    await expect(page.getByTestId("usage-total-shared_storage_gb_hours")).toContainText("1420 GB-hours");
    await expect(page.getByTestId("usage-csv")).toHaveAttribute("href", /format=csv/);
    await shot(page, "36-usage");

    // The platform admin sees the organisation in the admin console.
    await page.goto("/admin/orgs");
    await page.getByRole("link", { name: "Metered team" }).click();
    await expect(page.getByRole("heading", { name: "Metered team" })).toBeVisible();
  });

  test("billing: upgrade with a prorated preview, spend controls, an invoice issued and downloaded", async ({ page }) => {
    await signedIn(page);
    // The organisation from the usage journey.
    await page.getByTestId("org-switcher").click();
    await page.getByRole("menuitem", { name: /Metered team/ }).click();
    await expect(page.getByTestId("org-switcher-name")).toHaveText("Metered team");
    await page.goto("/org/billing");
    await expect(page.getByRole("heading", { name: "Billing", exact: true })).toBeVisible();
    await expect(page.getByTestId("plan-name")).toHaveText("Free");

    // Free → Pro: priced first (the rest of the month), then applied.
    await page.getByRole("button", { name: "Change plan…" }).click();
    await page.getByRole("radio", { name: /Pro/ }).check();
    const preview = page.getByTestId("plan-preview");
    await expect(preview).toContainText("Pro (monthly)");
    await expect(preview).toContainText("On the next invoice");
    // The owner accepts the SLA and DPA with the upgrade (V3 §7.3).
    await page.getByTestId("accept-legal").getByRole("checkbox").check();
    await shot(page, "50-billing-plan");
    await page.getByRole("button", { name: "Change plan now" }).click();
    await expect(page.getByTestId("plan-name")).toHaveText("Pro");

    // A budget, business details and a billing contact.
    await page.getByLabel("Monthly budget (₦)").fill("50,000");
    await page.getByTestId("spend-controls").getByRole("button", { name: "Save" }).click();
    await expect(page.getByTestId("spend-controls")).toContainText("Saved.");
    await expect(page.getByTestId("forecast")).toContainText("Budget ₦50,000.00");
    // The month so far, by service (V4 §12): at least the plan's fee.
    await expect(page.locator('[data-testid="service-spend"][data-service="plan"]')).toContainText("Plan");
    await page.getByLabel("Legal name").fill("Metered Team Ltd");
    await page.getByTestId("business-details").getByRole("button", { name: "Save" }).click();
    await expect(page.getByTestId("business-details")).toContainText("Saved.");
    await page.getByLabel("Contact email").fill("accounts@metered.example");
    await page.getByRole("button", { name: "Add", exact: true }).click();
    await expect(page.getByTestId("billing-contact")).toHaveText(/accounts@metered.example/);

    // The platform admin drafts this month's invoices and issues the team's.
    await page.goto("/admin/billing");
    await page.getByRole("button", { name: /^Draft / }).click();
    const row = page.getByTestId("admin-invoice-row").filter({ hasText: "Metered team" });
    await expect(row).toContainText("Draft");
    await row.getByRole("button", { name: "Issue" }).click();
    await expect(row).toContainText(/XB-\d{4}-\d{6}/);
    await page.getByRole("radio", { name: "Ledger" }).click();
    await expect(page.getByTestId("ledger")).toContainText("Balanced");
    await shot(page, "51-admin-billing");

    // The team sees it, with the details frozen on it, and downloads it.
    await page.goto("/org/billing");
    const inv = page.getByTestId("invoice-row").first();
    await expect(inv).toContainText(/XB-/);
    await inv.getByRole("button").click();
    await expect(page.getByTestId("invoice-panel")).toContainText("Pro (monthly)");
    await expect(page.getByTestId("invoice-total")).toContainText("₦");
    const [pdf] = await Promise.all([page.waitForEvent("download"), page.getByRole("link", { name: /Download PDF/ }).click()]);
    expect(pdf.suggestedFilename()).toMatch(/^XB-\d{4}-\d{6}\.pdf$/);
    expect(readFileSync((await pdf.path())!).subarray(0, 4).toString()).toBe("%PDF");
    await shot(page, "52-invoice");
    const total = ((await page.getByTestId("invoice-total").textContent()) ?? "").replace(/[^\d.,]/g, "");
    await page.keyboard.press("Escape");

    // The team pays by transfer to Xenibase's account; the admin records it
    // and the invoice is settled, with a receipt.
    await page.goto("/admin/billing");
    await page.getByRole("radio", { name: "Payments" }).click();
    await page.getByRole("button", { name: "Record a payment" }).click();
    const rec = page.getByTestId("record-payment");
    await rec.getByLabel("Organisation").selectOption({ label: "Metered team" });
    await rec.getByLabel("Amount received (₦)").fill(total);
    await rec.getByLabel("Bank reference").fill("GTB-E2E-0001");
    await rec.getByRole("button", { name: "Record" }).click();
    await stepUpIfAsked(page);
    const paid = page.getByTestId("admin-payment-row").filter({ hasText: "Metered team" });
    await expect(paid).toContainText("GTB-E2E-0001");
    await expect(paid).toContainText("manual");
    await shot(page, "53-admin-payments");

    // The org's billing terms, on its admin page.
    await page.goto("/admin/orgs");
    await page.getByRole("link", { name: "Metered team" }).click();
    const terms = page.getByTestId("org-billing");
    await expect(terms).toContainText("In good standing");
    await expect(terms).toContainText("Pro (monthly)");
    await terms.getByLabel("Payment terms (days)").fill("30");
    await terms.getByRole("button", { name: "Save", exact: true }).click();
    await stepUpIfAsked(page);
    await expect(terms).toContainText("Saved.");
    await expect(terms.getByLabel("Payment terms (days)")).toHaveValue("30");
    await page.goto("/org/billing");
    await expect(page.getByTestId("invoice-row").first()).toContainText("Paid");
    await expect(page.getByTestId("payment-row").first()).toContainText("GTB-E2E-0001");
    const [receipt] = await Promise.all([page.waitForEvent("download"), page.getByTestId("payment-row").first().getByRole("link", { name: /Receipt/ }).click()]);
    expect(readFileSync((await receipt.path())!).subarray(0, 4).toString()).toBe("%PDF");
    await shot(page, "54-billing-paid");

    // Pricing: plans side by side and a month's estimate (V3 §11).
    await page.goto("/pricing");
    await expect(page.getByRole("heading", { name: "Pricing", exact: true })).toBeVisible();
    await expect(page.getByTestId("plan-monthly-pro")).toContainText("₦");
    await page.getByLabel("Dedicated vCPUs").fill("2");
    await page.getByLabel("RAM (GB)").fill("4");
    await expect(page.getByTestId("estimate-total")).toContainText("₦");
    await shot(page, "55-pricing");
  });

  test("support, legal and revenue: a ticket answered from the console, agreements accepted, the month's MRR", async ({ page }) => {
    await signedIn(page);
    await page.getByTestId("org-switcher").click();
    await page.getByRole("menuitem", { name: /Metered team/ }).click();
    await expect(page.getByTestId("org-switcher-name")).toHaveText("Metered team");

    // Accepted with the upgrade.
    await page.goto("/org/legal");
    await expect(page.getByTestId("legal-sla")).toContainText("Accepted ");
    await expect(page.getByTestId("legal-dpa")).toContainText("Accepted ");
    // The documents are read as documents: a heading and a table, not Markdown source.
    await expect(page.getByTestId("legal-text-sla").getByRole("heading", { name: "Service level agreement" })).toBeVisible();
    await expect(page.getByTestId("legal-text-sla").getByRole("table").first()).toBeVisible();
    await expect(page.getByTestId("legal-text-sla")).not.toContainText("| ---");

    // A ticket from the dashboard.
    await page.goto("/org/support");
    await page.getByTestId("new-ticket").click();
    await page.getByTestId("ticket-subject").fill("Slow monthly report");
    await page.getByTestId("ticket-body").fill("The report query takes a minute since Tuesday.");
    await page.getByTestId("ticket-submit").click();
    await expect(page.getByTestId("ticket-panel")).toContainText("Slow monthly report");
    await expect(page.getByTestId("ticket-panel").getByTestId("message-in")).toContainText("takes a minute");
    await page.keyboard.press("Escape");

    // The console: the organisation beside the thread; a note and a reply.
    await page.goto("/admin/support");
    await page.getByRole("row").filter({ hasText: "Slow monthly report" }).click();
    const t = page.getByTestId("console-ticket");
    await expect(t.getByTestId("console-context")).toContainText("Metered team");
    await expect(t.getByTestId("console-context")).toContainText("pro");
    await t.getByTestId("console-body").fill("Likely the missing index on created_at.");
    await t.getByTestId("console-note").click();
    await expect(t.getByTestId("message-note")).toContainText("missing index");
    await t.getByTestId("console-body").fill("Could you add an index on orders(created_at)?");
    await t.getByTestId("console-reply").click();
    await expect(t.getByTestId("message-out")).toContainText("add an index");
    await t.getByTestId("console-status").selectOption("pending");
    await expect(t.getByTestId("console-status")).toHaveValue("pending");
    await shot(page, "56-support-console");
    await page.keyboard.press("Escape");

    // The customer sees the reply, not the note.
    await page.goto("/org/support");
    await page.getByRole("row").filter({ hasText: "Slow monthly report" }).click();
    const mine = page.getByTestId("ticket-panel");
    await expect(mine.getByTestId("message-out")).toContainText("add an index");
    await expect(mine.getByTestId("message-note")).toHaveCount(0);
    await shot(page, "57-support-ticket");
    await page.keyboard.press("Escape");

    // The month's recurring revenue includes the team's Pro plan.
    await page.goto("/admin/revenue");
    await expect(page.getByTestId("revenue-mrr")).toContainText("₦");
    await expect(page.getByTestId("revenue-mrr")).not.toContainText("₦0.00");
    await expect(page.getByTestId("revenue-csv")).toHaveAttribute("href", /format=csv/);
    await shot(page, "58-revenue");
  });

  test("capacity and costs: a node's cost, an exchange rate, and the month's margins", async ({ page }) => {
    await signedIn(page);
    await page.goto("/admin/capacity");
    await expect(page.getByTestId("admin-capacity")).toContainText("registered by hand");
    const row = page.getByTestId("capacity-node-local");
    await expect(row).toContainText("manual");
    await row.getByRole("button", { name: "Cost", exact: true }).click();
    await page.getByLabel("Monthly cost").fill("40");
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(row).toContainText("EUR 40.00");
    await page.getByTestId("capacity-evaluate").click();
    await expect(page.getByText("Thresholds checked.")).toBeVisible();
    await shot(page, "59-capacity");

    // Naira per euro, then today's costs attributed: the node's day, and
    // the floating IP.
    await page.goto("/admin/costs");
    await page.getByTestId("fx-rate").fill("1700");
    await page.getByTestId("fx-save").click();
    await expect(page.getByTestId("fx-rates")).toContainText("1 EUR = ₦1,700");
    await page.getByRole("button", { name: "Recompute" }).click();
    await expect(page.getByTestId("costs-total")).not.toContainText("₦0.00");
    await expect(page.getByTestId("cost-floating_ip")).toBeVisible();
    await expect(page.getByTestId("costs-csv")).toHaveAttribute("href", /format=csv/);
    await shot(page, "60-costs");
  });

  test("regions: a Lagos region, offered at project creation", async ({ page }) => {
    await signedIn(page);
    await page.goto("/admin/regions");
    await expect(page.getByTestId("region-eu-central")).toContainText("home");
    await page.getByRole("button", { name: "Add region" }).click();
    await page.getByLabel("ID", { exact: true }).fill("ng-lagos");
    await page.getByLabel("Name", { exact: true }).fill("Lagos");
    await page.getByLabel("Country (two letters)").fill("NG");
    await page.getByLabel("Pooler hostname").fill("db.ng.example.test");
    await page.getByRole("button", { name: "Save", exact: true }).click();
    const lagos = page.getByTestId("region-ng-lagos");
    await expect(lagos).toContainText("Lagos");
    await expect(lagos).toContainText("db.ng.example.test");
    await expect(lagos).toContainText("served by the home poolers");
    await shot(page, "61-regions");

    // New projects can choose it; the default stays the home region.
    await page.goto("/projects/new");
    const picker = page.getByLabel("Region");
    await expect(picker).toContainText("Lagos (NG)");
    await expect(picker).toContainText("(default)");

    // An existing project shows its region in Settings.
    const mine = (await page.evaluate(async () => (await fetch("/api/v1/projects")).json())) as { items: { id: string; region: string }[] };
    await page.goto(`/projects/${mine.items[0].id}/settings`);
    await expect(page.getByTestId("project-region")).toContainText(mine.items[0].region);
  });

  test("query insights: a slow lookup, its plan, and the index that fixes it", async ({ page }) => {
    await signedIn(page);
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Catalogue");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const appURL = await revealedValue(page, "credential-pooled-url");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    await page.getByRole("link", { name: "Open the project" }).click();
    await expect(page).toHaveURL(/\/projects\/[0-9a-f-]{36}/);
    const id = new URL(page.url()).pathname.split("/")[2];
    const app = await connect(appURL);
    await app.query(
      "CREATE TABLE items (id serial PRIMARY KEY, category int NOT NULL, price numeric NOT NULL); " +
        "INSERT INTO items (category, price) SELECT g % 500, g FROM generate_series(1, 60000) g; ANALYZE items",
    );
    // Statistics are read every 5 seconds here (every 5 minutes in production).
    await expect(async () => {
      for (let i = 0; i < 20; i++) await app.query(`SELECT avg(price) FROM items WHERE category = ${i}`);
      await page.goto(`/projects/${id}/insights`);
      await expect(page.getByTestId("project-insights").locator("tr", { hasText: "FROM items WHERE category" })).toBeVisible({ timeout: 3_000 });
    }).toPass({ timeout: 60_000 });
    await app.end();
    await page.getByTestId("project-insights").locator("tr", { hasText: "FROM items WHERE category" }).click();
    await page.getByTestId("insight-explain").click();
    await expect(page.getByTestId("insight-plan")).toContainText("Seq Scan on public.items");
    await shot(page, "62-query-insights");
    await page.keyboard.press("Escape");
    await page.getByRole("tab", { name: "Indexes" }).click();
    const sg = page.getByTestId("suggestion-items-category");
    await expect(sg).toContainText("CREATE INDEX CONCURRENTLY items_category_idx ON public.items (category);");
    await shot(page, "63-index-suggestion");
  });

  test("API tokens: a restricted write token for CI, and a CLI device login", async ({ page }) => {
    await signedIn(page);
    // Calls the API as a CI job would: a bearer token, no cookies.
    const bearer = (token: string, method: string, path: string, body?: unknown) =>
      page.evaluate(
        async ([token, method, path, body]) => {
          const r = await fetch(path as string, {
            method: method as string,
            credentials: "omit",
            headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
            body: body === undefined ? undefined : JSON.stringify(body),
          });
          return { status: r.status, body: await r.json().catch(() => null) };
        },
        [token, method, path, body] as const,
      );
    const sessionGet = (path: string) => page.evaluate(async (p) => (await fetch(p)).json(), path);
    const mine = (await sessionGet("/api/v1/projects")) as { items: { id: string; name: string }[] };
    const team = mine.items.find((p) => p.name === "Team data")!;
    const orgs = (await sessionGet("/api/v1/orgs")) as { items: { id: string; name: string; personal: boolean }[] };
    const metered = orgs.items.find((o) => o.name === "Metered team")!;
    const other = ((await sessionGet(`/api/v1/projects?org=${metered.id}`)) as { items: { id: string }[] }).items[0];

    // Account → API tokens: write scope, only "Team data".
    await page.goto("/account");
    await page.getByTestId("new-token").click();
    await page.getByRole("dialog").getByLabel("Name").fill("GitHub Actions — team data");
    await page.getByTestId("scope-write").check();
    await page.getByTestId("token-some-projects").check();
    await page.getByTestId("token-project-Team data").check();
    await page.getByTestId("create-token").click();
    const token = await revealedValue(page, "token-secret");
    expect(token).toMatch(/^xb_[0-9A-Za-z]{43}$/);
    await shot(page, "37-token-created");
    await page.getByRole("button", { name: "Done" }).click();
    await expect(page.getByTestId("token-GitHub Actions — team data")).toContainText("1 only");

    // It runs SQL on its project...
    const sql = await bearer(token, "POST", `/api/v1/projects/${team.id}/sql`, { query: "SELECT body FROM facts", query_id: crypto.randomUUID() });
    expect(sql.status).toBe(200);
    expect(sql.body.results[0].rows).toEqual([["shared"]]);
    // ...gets 404 for a project in another organisation...
    expect((await bearer(token, "GET", `/api/v1/projects/${other.id}`)).status).toBe(404);
    // ...and is refused deleting its own project.
    const del = await bearer(token, "DELETE", `/api/v1/projects/${team.id}?confirm=Team%20data`);
    expect(del.status).toBe(403);
    expect(del.body.code).toBe("insufficient_scope");

    // The CLI's device login, approved here for the Metered team org.
    const start = await page.evaluate(async () => {
      const r = await fetch("/api/v1/auth/device", { method: "POST", credentials: "omit", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ client_name: "xenibase CLI on ci-runner" }) });
      return r.json();
    });
    await page.goto(new URL(start.verification_uri_complete).pathname + new URL(start.verification_uri_complete).search);
    await expect(page.getByText("ci-runner")).toBeVisible();
    await page.getByTestId("device-org").selectOption(metered.id);
    await shot(page, "38-device-login");
    await page.getByTestId("device-approve").click();
    await expect(page.getByTestId("device-approved")).toBeVisible();
    const collected = await page.evaluate(async (device) => {
      const r = await fetch("/api/v1/auth/device/token", { method: "POST", credentials: "omit", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ device_code: device }) });
      return { status: r.status, body: await r.json() };
    }, start.device_code);
    expect(collected.status).toBe(200);
    const me = await bearer(collected.body.secret, "GET", "/api/v1/me");
    expect(me.body.token.org_id).toBe(metered.id);

    // The org's owners see every token scoped to it, and can revoke one.
    await switchOrg(page, metered.id);
    await page.goto("/org/settings");
    await expect(page.getByTestId("org-tokens")).toContainText("xenibase CLI on ci-runner");
    page.once("dialog", (d) => void d.accept());
    await page.getByTestId("revoke-token-xenibase CLI on ci-runner").click();
    await expect(page.getByTestId("org-tokens")).toContainText("revoked");
    expect((await bearer(collected.body.secret, "GET", "/api/v1/me")).status).toBe(401);
  });

  test("table editor: edit rows, see a conflict, change the schema and export a migration", async ({ page, browser }) => {
    await signedIn(page);
    const mine = (await page.evaluate(async () => (await fetch("/api/v1/projects")).json())) as { items: { id: string; name: string }[] };
    const team = mine.items.find((p) => p.name === "Team data")!;
    const openTable = async (p: Page, name: string) => {
      await p.goto(`/projects/${team.id}/tables`);
      await p.getByTestId("schema-tree").getByRole("button", { name, exact: true }).click();
      await expect(p.getByTestId(`tab-${name}`)).toBeVisible();
    };
    // Double-click a cell, type, Enter: saved straight away, as in Studio.
    const editBody = async (p: Page, from: string, to: string) => {
      await p.getByTestId("grid-row").filter({ hasText: from }).getByTestId("cell-body").dblclick();
      await p.getByTestId("cell-input-body").fill(to);
      // The grid shows the new value at once; wait for the save itself.
      const saved = p.waitForResponse((r) => r.request().method() === "POST" && r.url().endsWith("/changes"));
      await p.getByTestId("cell-input-body").press("Enter");
      await saved;
    };
    await openTable(page, "facts");
    await expect(page.getByTestId("table-grid")).toContainText("shared");

    await editBody(page, "shared", "shared (mine)");
    await expect(page.getByTestId("table-grid")).toContainText("shared (mine)");
    await page.reload();
    await expect(page.getByTestId("table-grid")).toContainText("shared (mine)");

    // Two sessions edit the same row: the second is told, and nothing of
    // theirs is overwritten.
    const otherCtx = await browser.newContext({ storageState: stateFile });
    const other = await otherCtx.newPage();
    await openTable(other, "facts");
    await expect(other.getByTestId("table-grid")).toContainText("shared (mine)");
    await editBody(other, "shared (mine)", "theirs");
    await expect(other.getByTestId("table-grid")).toContainText("theirs");
    await editBody(page, "shared (mine)", "mine again");
    await expect(page.getByText("Someone changed this row since you loaded it")).toBeVisible();
    await shot(page, "39-row-conflict");
    await expect(page.getByTestId("table-grid")).toContainText("theirs");
    await otherCtx.close();

    // Insert a row from the side panel, filter to it, then delete it.
    await page.getByTestId("insert-menu").click();
    await page.getByTestId("insert-row").click();
    await page.getByTestId("row-field-id").fill("2");
    await page.getByTestId("row-field-body").fill("from the panel");
    await page.getByTestId("row-panel-save").click();
    await expect(page.getByTestId("record-count")).toHaveText("2 records");
    await page.getByTestId("filter-button").click();
    await page.getByTestId("add-filter").click();
    await page.getByLabel("Filter column").selectOption("body");
    await page.getByLabel("Filter value").fill("from the panel");
    await page.getByTestId("apply-filter").click();
    await expect(page.getByTestId("record-count")).toHaveText("1 record");
    await expect(page.getByTestId("table-grid")).not.toContainText("theirs");
    await page.getByTestId("grid-row").getByRole("checkbox").check();
    await page.getByTestId("delete-rows").click();
    await page.getByTestId("confirm-delete-rows").click();
    await expect(page.getByTestId("record-count")).toHaveText("0 records");
    await page.getByTestId("filter-button").click();
    await page.getByRole("button", { name: "Remove filter" }).click();
    await page.getByTestId("apply-filter").click();
    await expect(page.getByTestId("record-count")).toHaveText("1 record");

    // Add a column: review the SQL, save it as a goose migration, run it.
    await page.getByTestId("insert-menu").click();
    await page.getByTestId("insert-column").click();
    await page.getByTestId("column-name").fill("summary");
    await page.getByTestId("column-panel").getByLabel("Default value").fill("''");
    await page.getByTestId("column-panel-save").click();
    await expect(page.getByTestId("schema-preview")).toContainText('ADD COLUMN "summary" text DEFAULT');
    await page.getByTestId("migration-format").selectOption("goose");
    const [download] = await Promise.all([page.waitForEvent("download"), page.getByTestId("save-migration").click()]);
    expect(download.suggestedFilename()).toMatch(/^\d{14}_add_column_facts_summary\.sql$/);
    const migration = readFileSync(await download.path(), "utf8");
    expect(migration).toContain("-- +goose Up");
    expect(migration).toContain('ALTER TABLE "public"."facts" DROP COLUMN "summary";');
    await shot(page, "40-schema-preview");
    await page.getByTestId("schema-run").click();
    await expect(page.getByTestId("column-summary")).toBeVisible();

    // A type change that rewrites the table says so before anything runs.
    await page.getByTestId("column-menu-id").click();
    await page.getByRole("menuitem", { name: "Edit column" }).click();
    await page.getByTestId("column-panel").getByTestId("type-picker").click();
    await page.getByLabel("Search types").fill("int8");
    await page.getByTestId("type-int8").click();
    await page.getByTestId("column-panel-save").click();
    await expect(page.getByTestId("schema-risks")).toContainText("Rewrites the table and blocks writes. Estimated size:");
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("column-id")).toContainText("integer");

    // A new table with a foreign key to facts, then a row that links to it.
    await page.getByTestId("new-table").click();
    await page.getByTestId("table-name").fill("fact_notes");
    await page.getByTestId("table-panel-add-column").click();
    const fkRow = page.getByTestId("column-row").last();
    await fkRow.getByLabel("Column name").fill("fact_id");
    await fkRow.getByTestId("type-picker").click();
    await page.getByTestId("type-int4").click();
    await fkRow.getByTestId("add-foreign-key").click();
    await fkRow.getByLabel("Referenced table").selectOption("facts");
    await fkRow.getByLabel("Referenced column").selectOption("id");
    await fkRow.getByLabel("On delete").selectOption("CASCADE");
    await shot(page, "40b-new-table-panel");
    await page.getByTestId("table-panel-save").click();
    await expect(page.getByTestId("schema-preview")).toContainText('REFERENCES "public"."facts" ("id") ON DELETE CASCADE');
    await page.getByTestId("schema-run").click();
    await expect(page.getByTestId("tab-fact_notes")).toBeVisible();
    await expect(page.getByText("This table is empty")).toBeVisible();
    await page.getByTestId("insert-menu").click();
    await page.getByTestId("insert-row").click();
    await page.getByTestId("row-field-fact_id").fill("1");
    await page.getByTestId("row-panel-save").click();
    await expect(page.getByTestId("record-count")).toHaveText("1 record");
    await page.getByTestId("fk-fact_id").click();
    await expect(page.getByTestId("fk-panel")).toContainText("theirs");
    await shot(page, "40c-foreign-row");
  });
  // The M12 "done when", through the browser: the organisation brings its
  // own bucket, a project gets its own key and sends its backups there, and
  // the key downloads (after re-authentication) with the restore README.
  // Restoring from that file with gpg and pg_restore alone is covered by
  // the integration suite.
  test("backup storage: the organisation's own bucket and a project key", async ({ page }) => {
    test.skip(!s3Endpoint, "needs the e2e bundle's fake S3");
    await signedIn(page);

    // An org target, saved only once its live test passes.
    await page.goto("/org/settings");
    const panel = page.getByTestId("org-storage-targets");
    await panel.getByRole("button", { name: "Add a target" }).click();
    await panel.getByLabel("Name").fill("Our bucket");
    await panel.getByLabel("Endpoint").fill(s3Endpoint!);
    await panel.getByLabel("Region").fill("us-east-1");
    await panel.getByLabel("Bucket").fill("no-such-bucket");
    await panel.getByLabel("Access key").fill("e2e-access");
    await panel.getByLabel("Secret key").fill("e2e-secret");
    await panel.getByLabel(/Path-style addressing/).check();
    await panel.getByRole("button", { name: "Test and save" }).click();
    await expect(panel.getByText("Live test failed; not saved")).toBeVisible();
    await panel.getByLabel("Bucket").fill("org-e2e");
    await panel.getByRole("button", { name: "Test and save" }).click();
    await expect(panel.getByTestId("storage-target-row").filter({ hasText: "Our bucket" })).toBeVisible();
    await shot(page, "41-org-storage-targets");

    // A project of that organisation, with its own key, backing up there.
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Vault");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    await page.getByRole("link", { name: "Open the project" }).click();
    await projectTab(page, "Storage");
    const storage = page.getByTestId("project-storage");
    await expect(storage.getByTestId("project-storage-target")).toContainText("counts toward your backup quota");
    await storage.getByRole("button", { name: "Use a project key" }).click();
    await expect(storage.getByTestId("project-backup-key")).toContainText("this project's own key");
    await storage.getByTestId("storage-choice").selectOption({ label: "Our bucket (organisation)" });
    await storage.getByLabel("Also copy existing backups there").check();
    await storage.getByRole("button", { name: "Switch storage" }).click();
    await expect(storage.getByTestId("project-storage-target")).toContainText("Our bucket");
    await expect(storage.getByTestId("project-storage-target")).toContainText("doesn't count toward your backup quota");

    await shot(page, "42-project-storage");
    await projectTab(page, "Backups");
    await page.getByRole("button", { name: "Back up now" }).click();
    const row = page.getByTestId("backup-row").first();
    await expect(row).toContainText("succeeded", { timeout: 60_000 });
    await expect(row.getByTestId("backup-storage")).toContainText("Our bucket");
    await expect(row.getByTestId("backup-storage")).toContainText("project key");

    // The key file, after confirming it's you.
    await projectTab(page, "Storage");
    await storage.getByRole("button", { name: "Download key…" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel("Your password").fill(password);
    await dialog.getByLabel("Authenticator code").fill(await freshTotp(totpSecret));
    const [download] = await Promise.all([page.waitForEvent("download"), dialog.getByRole("button", { name: "Download" }).click()]);
    const keyFile = readFileSync((await download.path())!, "utf8");
    expect(keyFile).toContain("-----BEGIN PGP PRIVATE KEY BLOCK-----");
    expect(keyFile).toContain("gpg --batch --decrypt backup.dump.gpg > backup.dump");
    expect(keyFile).toContain("pg_restore --no-owner --no-acl");
  });
  // The M13 "done when", through the browser: branch a project, reset the
  // branch from its parent with its URL unchanged, keep it, and delete it.
  // (The per-pull-request workflow and the branch quota run in the
  // integration suite.)
  test("branches: create, reset with the same URL, keep, and delete", async ({ page }) => {
    await signedIn(page);
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Ledger");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const ledgerURL = await revealedValue(page, "credential-pooled-url");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    let db = await connect(ledgerURL);
    await db.query("CREATE TABLE entries (id serial PRIMARY KEY, memo text NOT NULL)");
    await db.query("INSERT INTO entries (memo) SELECT 'entry ' || g FROM generate_series(1, 30) g");
    await db.end();

    // A branch, copied live (there is no backup yet), kept 3 days.
    await page.getByRole("link", { name: "Open the project" }).click();
    await projectTab(page, "Branches");
    await page.getByRole("button", { name: "New branch" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel("Branch name").fill("try-migration");
    await expect(dialog.getByLabel(/Live: a fresh dump/)).toBeChecked();
    await dialog.getByTestId("branch-ttl").selectOption("72");
    await dialog.getByRole("button", { name: "Create branch" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const branchURL = await revealedValue(page, "credential-pooled-url");
    expect(branchURL).not.toBe(ledgerURL);
    expect(await count(branchURL, "SELECT count(*) FROM entries")).toBe(30);
    await page.getByLabel("I've saved the password somewhere safe").check();
    await shot(page, "43-branch-created");
    await page.getByRole("button", { name: "Done" }).click();
    await page.getByRole("link", { name: "Open the project" }).click();

    // Diverge, then reset from the parent: the same URL sees the parent's data.
    db = await connect(branchURL);
    await db.query("DELETE FROM entries");
    await db.end();
    await projectTab(page, "Branch");
    const controls = page.getByTestId("branch-controls");
    await expect(controls.getByTestId("branch-expiry")).toContainText("in ");
    await controls.getByTestId("reset-branch").click();
    await page.getByTestId("confirm-reset").click();
    await expect(async () => {
      expect(await count(branchURL, "SELECT count(*) FROM entries")).toBe(30);
    }).toPass({ timeout: 60_000 });
    await shot(page, "44-branch-controls");

    // Keep it, and see it nested under its parent.
    await controls.getByTestId("extend-ttl").selectOption("0");
    await controls.getByTestId("extend-branch").click();
    await expect(controls.getByText("never")).toBeVisible();
    await page.goto("/projects");
    const nested = page.getByTestId("branch-list-row").filter({ hasText: "try-migration" });
    await expect(nested).toContainText("kept");
    await shot(page, "45-projects-with-branches");

    // Delete it (re-authentication, like any delete).
    await nested.getByRole("link", { name: "try-migration" }).click();
    await projectTab(page, "Branch");
    await page.getByRole("button", { name: "Delete branch…" }).click();
    const confirm = page.getByRole("dialog");
    await confirm.getByTestId("confirm-name").fill("try-migration");
    await confirm.getByLabel("Your password").fill(password);
    await confirm.getByTestId("confirm-code").fill(await freshTotp(totpSecret));
    await confirm.getByRole("button", { name: "Delete branch" }).click();
    // The dialog closes once the delete is queued; navigating sooner
    // abandons the request.
    await expect(confirm).toBeHidden({ timeout: 30_000 });
    await expect(async () => {
      await page.goto("/projects");
      await expect(page.getByTestId("branch-list-row").filter({ hasText: "try-migration" })).toHaveCount(0);
    }).toPass({ timeout: 60_000 });
  });
  // The M15 "done when" in the browser: a committed insert reaches a
  // receiver, signed; a scheduled job runs on demand. (Rollbacks, a receiver
  // down for an hour, the metadata address and the delivery rate run in the
  // integration suite.)
  test("webhooks and jobs: an insert reaches a receiver, signed; a job runs", async ({ page }) => {
    await signedIn(page);
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Storefront");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const url = await revealedValue(page, "credential-pooled-url");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    const db = await connect(url);
    await db.query("CREATE TABLE orders (id serial PRIMARY KEY, item text NOT NULL)");
    await db.query("CREATE TABLE order_counts (at timestamptz NOT NULL DEFAULT now(), n bigint NOT NULL)");
    await page.getByRole("link", { name: "Open the project" }).click();
    const id = page.url().match(/projects\/([0-9a-f-]{36})/)![1];
    const proj = await page.evaluate(async (pid) => (await (await fetch(`/api/v1/projects/${pid}`)).json()) as { org_id: string }, id);

    // The receiver is an internal host: the platform admin allow-lists it.
    await page.goto(`/admin/orgs/${proj.org_id}`);
    await page.getByTestId("outbound-allowlist").fill("fakehook");
    await page.getByRole("button", { name: "Save allow-list" }).click();
    await expect(page.getByRole("button", { name: "Save allow-list" })).toBeDisabled();

    // A webhook on orders.
    await page.goto(`/projects/${id}/webhooks`);
    await page.getByRole("button", { name: "New webhook" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel("Name").fill("orders-to-shop");
    await dialog.getByLabel("Tables").fill("orders");
    await dialog.getByLabel("URL").fill("http://fakehook:8080/orders");
    await dialog.getByRole("button", { name: "Create webhook" }).click();
    const secret = await revealedValue(page, "webhook-secret-value");
    expect(secret).toMatch(/^whsec_/);
    await shot(page, "46-webhook-created");
    await page.getByRole("button", { name: /stored it/ }).click();

    // A committed insert arrives, signed with the secret.
    await db.query("INSERT INTO orders (item) VALUES ('kettle')");
    type Hook = { path: string; header: Record<string, string>; body: string };
    let hook: Hook | undefined;
    await expect(async () => {
      const got = (await (await fetch(`${hookAPI}/requests`)).json()) as Hook[];
      hook = got.find((h) => h.path === "/orders" && h.body.includes("kettle"));
      expect(hook).toBeTruthy();
    }).toPass({ timeout: 15_000 });
    const sig = Object.fromEntries(hook!.header["Xenibase-Signature"].split(",").map((kv) => kv.split("=") as [string, string]));
    expect(createHmac("sha256", secret).update(`${sig.t}.${hook!.body}`).digest("hex")).toBe(sig.v1);
    expect(JSON.parse(hook!.body)).toMatchObject({ webhook: "orders-to-shop", table: "public.orders", type: "INSERT", record: { item: "kettle" } });
    expect(hook!.header["Xenibase-Event-Id"]).toMatch(/^evt_/);

    // The delivery log, and a test event.
    // The new webhook's details are open.
    await expect(page.getByTestId("delivery-row").first()).toContainText("delivered");
    await page.getByTestId("webhook-test").click();
    await expect(page.getByTestId("webhook-message")).toContainText("Test event delivered");
    await shot(page, "47-webhook-log");
    await page.keyboard.press("Escape");

    // A scheduled job, run now.
    await projectTab(page, "Jobs");
    await page.getByRole("button", { name: "New job" }).click();
    const jd = page.getByRole("dialog");
    await jd.getByLabel("Name").fill("count-orders");
    await jd.getByRole("button", { name: "Every day at 03:00" }).click();
    await jd.getByRole("textbox", { name: "SQL" }).fill("INSERT INTO order_counts (n) SELECT count(*) FROM orders");
    await jd.getByRole("button", { name: "Create job" }).click();
    await expect(page.getByTestId("job-row").filter({ hasText: "count-orders" })).toBeVisible();
    await expect(page.getByTestId("job-upcoming").locator("li")).toHaveCount(5);
    await page.getByTestId("job-run-now").click();
    await expect(page.getByTestId("job-run-row").first()).toContainText("succeeded", { timeout: 30_000 });
    expect(await count(url, "SELECT n FROM order_counts")).toBe(1);
    await shot(page, "48-job-run");
    await db.end();
  });

  test("backend services: enable a project's API, keys shown once, a key made and revoked", async ({ page }) => {
    await signedIn(page);
    await page.goto("/projects/new");
    await page.getByLabel("Name").fill("Storefront API");
    await page.getByRole("button", { name: "Create project" }).click();
    await expect(page.getByTestId("provision-ready")).toBeVisible({ timeout: 60_000 });
    const dbURL = await revealedValue(page, "credential-pooled-url");
    await page.getByLabel("I've saved the password somewhere safe").check();
    await page.getByRole("button", { name: "Done" }).click();
    await page.getByRole("link", { name: "Open the project" }).click();
    await expect(page).toHaveURL(/\/projects\/[0-9a-f-]{36}/);
    const id = new URL(page.url()).pathname.split("/")[2];

    await page.goto(`/projects/${id}/settings/api`);
    await page.getByRole("button", { name: "Enable backend services" }).click();
    const keys = page.getByTestId("new-api-keys");
    await expect(keys.getByTestId("key-secret")).toContainText("xb_sec_");
    await expect(keys.getByTestId("key-publishable")).toContainText("xb_pub_");
    await shot(page, "70-api-keys-once");
    await keys.getByRole("button", { name: "I've copied them" }).click();
    await expect(page.getByTestId("services-url")).toContainText(/[a-z][a-z0-9]{7}/, { timeout: 30_000 });
    const rows = page.getByTestId("api-key-row");
    await expect(rows).toHaveCount(2);

    // Rotation: a second secret key, then the first one revoked.
    await page.getByTestId("services-keys").getByRole("button", { name: "New key" }).click();
    await page.getByLabel("Name").fill("rotated");
    await page.getByLabel("Kind").selectOption("secret");
    await page.getByRole("button", { name: "Create" }).click();
    await expect(page.getByTestId("new-api-keys").getByTestId("key-secret")).toContainText("xb_sec_");
    await page.getByRole("button", { name: "I've copied them" }).click();
    await expect(rows).toHaveCount(3);
    const first = rows.filter({ hasText: "default" }).filter({ hasText: "secret" });
    await first.getByRole("button", { name: "Revoke" }).click();
    await expect(first).toContainText("revoked");
    await shot(page, "71-api-settings");

    // M30: the advisor flags a table without row-level security, the
    // explorer shows anon is refused, and the policy helper fixes it.
    const db = await connect(dbURL);
    await db.query("CREATE TABLE notes (id serial PRIMARY KEY, owner_id uuid NOT NULL, body text NOT NULL)");
    await db.end();
    const advisor = page.getByTestId("services-advisor");
    await expect(async () => {
      await advisor.getByRole("button", { name: "Check again" }).click();
      await expect(advisor.getByTestId("advisor-finding").filter({ hasText: "public.notes" })).toContainText(/row-level security is off/i, { timeout: 2_000 });
    }).toPass({ timeout: 30_000 });
    const explorer = page.getByTestId("services-explorer");
    await explorer.getByLabel("Path").fill("/data/v1/notes");
    await explorer.getByRole("button", { name: "Send" }).click();
    await expect(explorer.getByTestId("explorer-result")).toContainText("rls_required");
    await shot(page, "72-api-advisor-explorer");

    await page.goto(`/projects/${id}/tables`);
    await page.getByTestId("schema-tree").getByRole("button", { name: "notes", exact: true }).click();
    await page.getByTestId("policy-helper").click();
    const pd = page.getByTestId("policy-dialog");
    await expect(pd.getByTestId("policy-sql")).toContainText("owner_id = xb_auth.uid()");
    await shot(page, "73-policy-helper");
    await pd.getByTestId("policy-apply").click();
    await expect(pd).toBeHidden();

    await page.goto(`/projects/${id}/settings/api`);
    await expect(page.getByTestId("services-advisor")).toBeVisible();
    await expect(page.getByTestId("advisor-finding").filter({ hasText: "public.notes" })).toHaveCount(0);
    await explorer.getByLabel("Path").fill("/data/v1/notes");
    await explorer.getByRole("button", { name: "Send" }).click();
    await expect(explorer.getByTestId("explorer-result")).toContainText("200");

    // V4.1-M8: the quick start and a table's docs carry the project's URL
    // and publishable key, and "Try it" runs in the explorer.
    const apiURL = (await page.getByTestId("services-url").innerText()).match(/https?:\/\/\S+/)![0];
    const quick = page.getByTestId("services-quickstart");
    await expect(quick.getByTestId("quickstart-values")).toContainText(apiURL);
    await expect(quick.getByTestId("quickstart-values")).toContainText("xb_pub_");
    await expect(quick.locator("pre").last()).toContainText(`createClient("${apiURL}", "xb_pub_`);
    await quick.getByRole("radio", { name: "Dart" }).click();
    await expect(quick.locator("pre").last()).toContainText(`XenibaseClient('${apiURL}', 'xb_pub_`);
    await quick.getByRole("button", { name: "Hide" }).click();
    await page.reload();
    await expect(page.getByTestId("services-quickstart").getByRole("button", { name: "Show" })).toBeVisible();
    const docs = page.getByTestId("services-docs");
    await docs.getByLabel("Table or function").selectOption("t:notes");
    await expect(docs.getByTestId("docs-table")).toContainText("owner_id");
    await expect(docs.getByTestId("docs-table")).toContainText(/Row-level security\s*on/);
    const read = docs.getByTestId("docs-example-read");
    await expect(read).toContainText(`${apiURL}/data/v1/notes`);
    await expect(read).toContainText("apikey: xb_pub_");
    await shot(page, "75-api-docs");
    await read.getByRole("button", { name: "Try it" }).click();
    await expect(explorer.getByLabel("Path")).toHaveValue(/^\/data\/v1\/notes\?select=/);
    await explorer.getByRole("button", { name: "Send" }).click();
    await expect(explorer.getByTestId("explorer-result")).toContainText("200");
    await expect(page.getByTestId("services-usage").getByTestId("usage-row").first()).toContainText("Requests");

    // M36: the Supabase migration helper. The users and files steps need the
    // Supabase project's credentials; the policies step runs on its own.
    const mig = page.getByTestId("migrate-supabase");
    await expect(mig).toContainText("Migrate from Supabase");
    await expect(mig.getByTestId("migrate-users")).toBeDisabled();
    await expect(mig.getByTestId("migrate-storage")).toBeDisabled();
    await mig.getByTestId("migrate-policies").click();
    await expect(mig.getByTestId("operation-log")).toContainText("policies migrated", { timeout: 60_000 });
    await shot(page, "74-migrate-supabase");

    // M31: the app's users on Project → Authentication.
    await page.goto(`/projects/${id}/auth`);
    await expect(page.getByTestId("auth-users-empty")).toBeVisible();
    await page.getByRole("button", { name: "Add user" }).click();
    const add = page.getByTestId("add-auth-user");
    await add.getByLabel("Email").fill("ada@example.com");
    await add.getByRole("checkbox").first().click(); // a password instead of an invitation
    await add.getByLabel("Password").fill("ada-password-1");
    await add.getByRole("button", { name: "Create user" }).click();
    const userRow = page.getByTestId("auth-user-row").filter({ hasText: "ada@example.com" });
    await expect(userRow).toContainText("confirmed");
    await userRow.click();
    const panel = page.getByTestId("auth-user-panel");
    await panel.getByRole("button", { name: "Ban for 24 hours" }).click();
    await expect(panel).toContainText("Banned until");
    await shot(page, "74-auth-user");
    await page.keyboard.press("Escape");
    await expect(userRow).toContainText("banned");
    await page.getByRole("tab", { name: "Signing keys" }).click();
    await expect(page.getByTestId("signing-key-row")).toHaveCount(1);
    await page.getByRole("tab", { name: "Emails" }).click();
    await expect(page.getByTestId("auth-email-sending")).toContainText("Platform email");
    await shot(page, "75-auth-emails");

    // M32: WhatsApp codes, Google, MFA and hooks.
    await page.getByRole("tab", { name: "Phone" }).click();
    const phone = page.getByTestId("auth-phone");
    await phone.getByRole("switch", { name: "Codes by WhatsApp" }).click();
    await phone.getByLabel("Daily cap").fill("50");
    await phone.getByRole("button", { name: "Save" }).click();
    await expect(phone).toContainText("Saved");
    await expect(page.getByTestId("auth-phone-today")).toContainText("0 / 50");
    await shot(page, "76-auth-phone");
    await page.getByRole("tab", { name: "Providers" }).click();
    const google = page.getByTestId("auth-oauth-google");
    await google.getByRole("switch", { name: "Sign in with Google" }).click();
    await google.getByLabel("Google client ID").fill("e2e-client.apps.googleusercontent.com");
    await google.getByLabel("Google client secret").fill("e2e-secret");
    await page.getByTestId("auth-providers").getByRole("button", { name: "Save" }).click();
    await expect(google).toContainText("On");
    await expect(google).toContainText("Stored; leave empty to keep it.");
    await shot(page, "77-auth-providers");
    await page.getByRole("tab", { name: "MFA and captcha" }).click();
    await page.getByLabel("MFA policy").selectOption("required");
    await page.getByTestId("auth-security").getByRole("button", { name: "Save" }).click();
    await expect(page.getByTestId("auth-security")).toContainText("Saved");
    await page.getByRole("tab", { name: "Hooks" }).click();
    const hooks = page.getByTestId("auth-hooks");
    await expect(hooks).toContainText("_auth_hook");
    await hooks.getByLabel("Custom claims function").fill("public.not_a_real_hook");
    await hooks.getByRole("button", { name: "Save" }).click();
    await expect(hooks).toContainText("Saved");
    await expect(page.getByTestId("auth-hook-deliveries")).toContainText("No webhook deliveries yet.");
    await shot(page, "78-auth-hooks");

    // M33: a bucket, a file uploaded into a folder, and the list.
    await page.goto(`/projects/${id}/files`);
    await page.getByTestId("new-bucket").click();
    const bd = page.getByTestId("bucket-dialog");
    await bd.getByLabel("Name").fill("docs");
    await bd.getByLabel("Allowed file types").fill("text/*");
    await bd.getByRole("button", { name: "Create bucket" }).click();
    await expect(bd).toBeHidden();
    await expect(page.getByTestId("bucket-row").filter({ hasText: "docs" })).toBeVisible();
    await expect(page.getByTestId("files-empty")).toBeVisible();
    await page.getByTestId("upload-input").setInputFiles({ name: "hello.txt", mimeType: "text/plain", buffer: Buffer.from("hello, files") });
    const fileRow = page.getByTestId("file-row").filter({ hasText: "hello.txt" });
    await expect(fileRow).toContainText("text/plain");
    await expect(fileRow).toContainText("12 B");
    await expect(page.getByTestId("files-stat-bytes")).toContainText("12 B", { timeout: 10_000 });
    // A private bucket's file gets a signed link on the project's API URL.
    await fileRow.getByRole("button", { name: "Signed URL" }).click();
    const signed = page.getByRole("dialog", { name: "Signed URL" });
    await expect(signed.getByLabel("Signed URL")).toHaveValue(/\/storage\/v1\/object\/sign\/docs\/hello\.txt\?token=.+/);
    expect(await signed.getByLabel("Signed URL").inputValue()).toMatch(new RegExp("^" + apiURL.replaceAll(".", "\\.") + "/"));
    await page.keyboard.press("Escape");
    await expect(signed).toBeHidden();
    await expect(fileRow.getByRole("link", { name: "Download" })).toHaveAttribute("href", /\/files\/buckets\/docs\/object\?path=hello\.txt$/);
    await shot(page, "79-storage");
    page.once("dialog", (d) => void d.accept());
    await fileRow.getByTestId("delete-file").click();
    await expect(page.getByTestId("files-empty")).toBeVisible();

    // M34: realtime on for the notes table (it has row-level security now).
    await page.goto(`/projects/${id}/realtime`);
    const notesRow = page.getByTestId("realtime-table").filter({ hasText: "public.notes" });
    await expect(notesRow).toContainText("on");
    const sw = notesRow.getByRole("switch", { name: "Realtime for public.notes" });
    await expect(sw).not.toBeChecked();
    await sw.click();
    await expect(sw).toBeChecked();
    await page.getByLabel("Topics").fill("lobby");
    await page.getByRole("button", { name: "Save" }).click();
    await page.reload();
    await expect(page.getByLabel("Topics")).toHaveValue("lobby");
    await expect(page.getByTestId("realtime-table").filter({ hasText: "public.notes" }).getByRole("switch")).toBeChecked();
    await shot(page, "80-realtime");
  });

  test("banners: a member sees the overdue invoice without amounts, and an incident affecting the projects", async ({ page, browser }) => {
    await signedIn(page);
    // The owner's personal organisation, where the earlier projects are.
    const orgs = await page.evaluate(async () => (await (await fetch("/api/v1/orgs")).json()).items as { id: string; personal: boolean }[]);
    const orgID = orgs.find((o) => o.personal)!.id;
    await page.evaluate((id) => localStorage.setItem("xenibase.org", id), orgID);

    // Dana joins as a plain member: no billing access.
    const danaEmail = "dana@example.com";
    await page.goto("/org/members");
    await page.getByTestId("invite-member").click();
    await page.getByLabel("Email").fill(danaEmail);
    await page.getByLabel("Organisation role").selectOption("member");
    await page.getByRole("button", { name: "Send invitation" }).click();
    await expect(page.getByTestId("invitation-created")).toBeVisible();
    await page.getByRole("button", { name: "Done" }).click();
    const danaCtx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const dana = await danaCtx.newPage();
    await acceptInvitation(dana, await mailLink(danaEmail, "invitation"), "Dana");
    await dana.evaluate((id) => localStorage.setItem("xenibase.org", id), orgID);

    // An overdue invoice: every member sees it; only the owner gets the pay link, and nobody sees an amount here.
    expect(await apiStatus(page, `/api/v1/orgs/${orgID}/billing`)).toBe(200); // opens the billing account
    await metadataSQL(`UPDATE billing_accounts SET dunning_state = 'overdue' WHERE org_id = '${orgID}'`);
    await dana.goto("/projects");
    const danaBanner = dana.getByTestId("billing-banner");
    await expect(danaBanner).toContainText("has an overdue invoice");
    await expect(danaBanner).toContainText("Ask an owner or a billing member");
    await expect(danaBanner).not.toContainText("₦");
    await expect(danaBanner.getByRole("link", { name: "Pay now" })).toHaveCount(0);
    await page.goto("/projects");
    await expect(page.getByTestId("billing-banner").getByRole("link", { name: "Pay now" })).toBeVisible();

    // An open incident on the components the organisation's projects use.
    const incidentID = await metadataSQL(`WITH i AS (INSERT INTO incidents (title, components, severity, status)
      VALUES ('Connections are slow', ARRAY['shared-tier', 'dedicated'], 'major', 'investigating') RETURNING id),
      u AS (INSERT INTO incident_updates (incident_id, status, body) SELECT id, 'investigating', 'We are looking into it.' FROM i)
      SELECT id FROM i`);
    await dana.reload();
    await expect(dana.getByTestId("incident-banner")).toContainText("Connections are slow");
    await expect(dana.getByTestId("incident-banner")).toContainText("We are looking into it.");
    await shot(dana, "81-banners");

    await metadataSQL(`UPDATE incidents SET status = 'resolved', resolved_at = now() WHERE id = '${incidentID}'`);
    await metadataSQL(`UPDATE billing_accounts SET dunning_state = 'ok' WHERE org_id = '${orgID}'`);
    await dana.reload();
    await expect(dana.getByTestId("incident-banner")).toHaveCount(0);
    await expect(dana.getByTestId("billing-banner")).toHaveCount(0);
    await danaCtx.close();
  });

  test("the shell: keyboard shortcuts, and the menu on a narrow screen", async ({ page }) => {
    await signedIn(page);
    await page.goto("/projects");
    await page.getByTestId("project-card").first().getByRole("link").first().click();
    await expect(page).toHaveURL(/\/projects\/[0-9a-f-]{36}$/);
    const id = new URL(page.url()).pathname.split("/")[2];
    const unfocus = () => page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());

    // "?" lists every shortcut.
    await unfocus();
    await page.keyboard.press("?");
    await expect(page.getByTestId("shortcuts-sheet")).toContainText("Format the SQL");
    await shot(page, "49-shortcuts");
    await page.keyboard.press("Escape");

    // "g" then a letter jumps to a section.
    await unfocus();
    await page.keyboard.press("g");
    await page.keyboard.press("l");
    await expect(page).toHaveURL(new RegExp(`/projects/${id}/logs$`));
    await unfocus();
    await page.keyboard.press("g");
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/projects/${id}$`));

    // On a phone the rail and the section menu are one drawer.
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`/projects/${id}/backups`);
    await expect(page.getByTestId("rail")).toBeHidden();
    await page.getByTestId("mobile-menu").click();
    const drawer = page.getByTestId("mobile-nav");
    await expect(drawer.getByRole("link", { name: "Backups", exact: true })).toHaveAttribute("aria-current", "page");
    await shot(page, "50-mobile-menu");
    await drawer.getByRole("link", { name: "SQL Editor" }).click();
    await expect(page).toHaveURL(new RegExp(`/projects/${id}/sql$`));
    await expect(drawer).toBeHidden();
    await page.goto("/projects");
    await expect(page.getByTestId("project-card").first()).toBeVisible();
    await shot(page, "51-mobile-projects");
  });
});
