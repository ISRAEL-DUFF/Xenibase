// The atlas: the engineering guide to this repository (docs/atlas/README.md).
// What is particular to the repository (its title, where its packages live
// and what the counts on its pages count) is in atlas.json.
//
//	node docs/atlas/atlas.mjs build    write site/index.html (committed) and dist/artifact.html
//	node docs/atlas/atlas.mjs stale    list the pages whose code has moved since they were checked
//	node docs/atlas/atlas.mjs check    build in memory; fail on a reference that no longer resolves
//
// Pages are HTML fragments in pages/, each with a JSON header. The build
// reads the code they describe: excerpts and line links are resolved by
// symbol at build time, never pasted, so a rebuild follows the code and a
// symbol that disappears is reported. Nothing here needs a package.
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { dirname, extname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, "..", "..");
const config = JSON.parse(readFileSync(join(here, "atlas.json"), "utf8"));

// ---------------------------------------------------------------- git

function git(...args) {
  try {
    return execFileSync("git", args, { cwd: root, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"], maxBuffer: 64 << 20 }).trim();
  } catch {
    return null;
  }
}

const head = git("rev-parse", "HEAD") ?? "HEAD";
const headShort = head.slice(0, 7);
const headDate = git("log", "-1", "--format=%cs") ?? "";

// freshness says how far the code a page describes has moved since the
// commit the page was last checked against.
function freshness(page) {
  if (!page.verified || !page.sources?.length) return null;
  const known = git("cat-file", "-e", `${page.verified}^{commit}`) !== null;
  if (!known) return { known: false, count: 0, files: [], date: "" };
  const count = Number(git("rev-list", "--count", `${page.verified}..HEAD`, "--", ...page.sources) || 0);
  const files = count ? (git("diff", "--name-only", page.verified, "HEAD", "--", ...page.sources) ?? "").split("\n").filter(Boolean) : [];
  return { known: true, count, files, date: git("log", "-1", "--format=%cs", page.verified) ?? "" };
}

// ---------------------------------------------------------------- sources

const files = new Map();

function lines(path) {
  if (!files.has(path)) {
    const abs = join(root, path);
    files.set(path, existsSync(abs) && statSync(abs).isFile() ? readFileSync(abs, "utf8").split("\n") : null);
  }
  return files.get(path);
}

function findLine(src, needle, from = 0) {
  for (let i = from; i < src.length; i++) if (src[i].includes(needle)) return i;
  return -1;
}

function blobURL(path, line, endLine) {
  const kind = path.endsWith("/") ? "tree" : "blob";
  let url = `${config.repo}/${kind}/${head}/${path.replace(/\/$/, "")}`;
  if (line !== undefined) url += `#L${line + 1}` + (endLine !== undefined && endLine !== line ? `-L${endLine + 1}` : "");
  return url;
}

// blockEnd is the line that closes the block opened on line start: braces
// counted outside strings and comments, which is enough for Go and TS.
function blockEnd(src, start) {
  let depth = 0;
  let opened = false;
  let inRaw = false;
  for (let i = start; i < src.length; i++) {
    let l = src[i];
    if (inRaw) {
      const j = l.indexOf("`");
      if (j < 0) continue;
      inRaw = false;
      l = l.slice(j + 1);
    }
    l = l.replace(/"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`[^`]*`/g, "").replace(/\/\/.*$/, "");
    if (l.includes("`")) {
      inRaw = true;
      l = l.slice(0, l.indexOf("`"));
    }
    for (const ch of l) {
      if (ch === "{") {
        depth++;
        opened = true;
      } else if (ch === "}") depth--;
    }
    if (opened && depth <= 0) return i;
  }
  return opened ? src.length - 1 : start;
}

// ---------------------------------------------------------------- highlighting

const keywords = {
  go: "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false iota",
  ts: "async await break case catch class const continue default delete do else export extends false finally for from function if import in instanceof interface let new null return switch this throw true try type typeof undefined var void while of",
  sql: "add all alter and as begin by case conflict create declare default definer delete distinct do drop else end exception execute exists false for found from function grant if in index insert into is join key language lateral left limit locked not nothing null on or order owner partition perform primary raise range recursive references return returning returns revoke row security select set skip table then to true union unique update using values when where with",
  yaml: "true false null",
  json: "true false null",
  sh: "case do done elif else esac export fi for function if in then while",
};
const kwSets = Object.fromEntries(Object.entries(keywords).map(([k, v]) => [k, new Set(v.split(" "))]));

const tokenRes = {
  go: /(?<com>\/\/[^\n]*|\/\*[\s\S]*?\*\/)|(?<str>"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|`[^`]*`)|(?<num>\b\d[\d_.xXa-fA-F]*\b)|(?<id>[A-Za-z_]\w*)|(?<ws>\s+)|(?<p>[\s\S])/gy,
  ts: /(?<com>\/\/[^\n]*|\/\*[\s\S]*?\*\/)|(?<str>"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|`(?:[^`\\]|\\.)*`)|(?<num>\b\d[\d_.]*\b)|(?<id>[A-Za-z_$][\w$]*)|(?<ws>\s+)|(?<p>[\s\S])/gy,
  sql: /(?<com>--[^\n]*)|(?<str>'(?:[^']|'')*'|\$\$)|(?<num>\b\d[\d.]*\b)|(?<id>[A-Za-z_][\w]*)|(?<ws>\s+)|(?<p>[\s\S])/gy,
  yaml: /(?<com>#[^\n]*)|(?<key>^[ \t-]*[\w$./-]+(?=:))|(?<str>"(?:[^"\\\n]|\\.)*"|'[^'\n]*')|(?<num>\b\d[\d.]*\b)|(?<id>[A-Za-z_][\w-]*)|(?<ws>\s+)|(?<p>[\s\S])/gmy,
  json: /(?<key>"(?:[^"\\\n]|\\.)*"(?=\s*:))|(?<str>"(?:[^"\\\n]|\\.)*")|(?<num>-?\b\d[\d.eE+-]*\b)|(?<id>[A-Za-z_]\w*)|(?<ws>\s+)|(?<p>[\s\S])/gy,
  sh: /(?<com>#[^\n]*)|(?<str>"(?:[^"\\\n]|\\.)*"|'[^'\n]*')|(?<num>\b\d+\b)|(?<id>[A-Za-z_][\w-]*)|(?<ws>\s+)|(?<p>[\s\S])/gy,
};

const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

// highlight returns one HTML string per source line; a token that spans
// lines (a block comment, a raw string) is closed and reopened on each.
function highlight(text, lang) {
  const re = tokenRes[lang];
  const out = [""];
  const push = (cls, s) => {
    s.split("\n").forEach((piece, i) => {
      if (i > 0) out.push("");
      if (piece) out[out.length - 1] += cls ? `<span class="${cls}">${esc(piece)}</span>` : esc(piece);
    });
  };
  if (!re) {
    push("", text);
    return out;
  }
  re.lastIndex = 0;
  const kw = kwSets[lang];
  let m;
  while (re.lastIndex < text.length && (m = re.exec(text))) {
    const g = m.groups;
    if (g.com) push("c-com", g.com);
    else if (g.key) push("c-key", g.key);
    else if (g.str) push("c-str", g.str);
    else if (g.num) push("c-num", g.num);
    else if (g.id) {
      const word = lang === "sql" ? g.id.toLowerCase() : g.id;
      const next = text.slice(re.lastIndex).match(/^\s*\(/);
      if (kw.has(word)) push("c-kw", g.id);
      else if (next) push("c-fn", g.id);
      else if (lang === "go" && /^[A-Z]/.test(g.id)) push("c-ty", g.id);
      else push("", g.id);
    } else push("", m[0]);
  }
  return out;
}

const langOf = (path) =>
  ({ ".go": "go", ".sql": "sql", ".ts": "ts", ".tsx": "ts", ".js": "ts", ".mjs": "ts", ".yaml": "yaml", ".yml": "yaml", ".json": "json", ".sh": "sh" })[extname(path)] ?? "";

// ---------------------------------------------------------------- stats

const statCache = new Map();

function trackedFiles(...patterns) {
  return (git("ls-files", "--", ...patterns) ?? "").split("\n").filter(Boolean);
}

function countLines(paths) {
  let n = 0;
  for (const p of paths) n += (lines(p)?.length ?? 1) - 1;
  return n;
}

// The package roots are the directories whose subdirectories are the
// repository's packages ("packages": {"root": "internal"} in atlas.json, or
// "roots": [...] for more than one). A package named without a slash on a
// page is looked up under the first root.
const packageRoots = (() => {
  const p = config.packages ?? {};
  const list = p.roots ?? (p.root ? [p.root] : []);
  return list.map((r) => r.replace(/\/$/, ""));
})();

function packageDirs() {
  const out = [];
  for (const r of packageRoots) {
    if (!existsSync(join(root, r))) continue;
    for (const d of readdirSync(join(root, r), { withFileTypes: true })) {
      if (d.isDirectory() && readdirSync(join(root, r, d.name)).some((f) => f.endsWith(".go"))) out.push(`${r}/${d.name}`);
    }
  }
  return out.sort();
}

const qualify = (n) => (n.includes("/") || !packageRoots.length ? n : `${packageRoots[0]}/${n}`);
const shortName = (dir) => {
  for (const r of packageRoots) if (dir.startsWith(r + "/")) return dir.slice(r.length + 1);
  return dir;
};
const rootsLabel = () => packageRoots.map((r) => `<code>${esc(r)}/</code>`).join(" and ") || "the repository";

const filesIn = (dir, match) => (existsSync(join(root, dir)) ? readdirSync(join(root, dir)).filter((f) => new RegExp(match ?? ".").test(f)).sort() : []);
const read = (path) => readFileSync(join(root, path), "utf8");

// statKinds are the ways a stat in atlas.json can count. Each stat is
// {"kind": "...", ...} under "stats"; its name is what <x-stat name> uses.
const statKinds = {
  // Lines in the tracked files matching the git pathspecs, less those
  // matching exclude.
  lines: (d) => countLines(trackedFiles(...[].concat(d.files)).filter((f) => !d.exclude || !new RegExp(d.exclude).test(f))),
  // How many packages there are under the package roots.
  packages: () => packageDirs().length,
  // How many files in dir match the pattern.
  files: (d) => filesIn(d.dir, d.match).length,
  // The last such file by name, cut to slice characters.
  latest: (d) => filesIn(d.dir, d.match).pop()?.slice(0, d.slice ?? undefined) ?? "",
  // How many subdirectories of dir hold the file named in having.
  dirs: (d) => filesIn(d.dir).filter((n) => existsSync(join(root, d.dir, n, d.having ?? ""))).length,
  // How many times the pattern matches in the file (multiline).
  matches: (d) => (read(d.file).match(new RegExp(d.pattern, "gm")) ?? []).length,
  // The pattern's first group in the file.
  capture: (d) => read(d.file).match(new RegExp(d.pattern, "m"))?.[1] ?? "",
  // The schema left after replaying goose migrations: "tables",
  // "functions" or "definers" (SECURITY DEFINER functions).
  schema: (d) => schema(d.dir)[d.value ?? "tables"],
};

// schema replays the migrations' Up sections in order and counts what is
// left: tables created and not dropped, and functions by their last
// definition (SECURITY DEFINER or not).
function schema(dir) {
  const tables = new Set();
  const fns = new Map();
  for (const f of filesIn(dir, "\\.sql$")) {
    const up = read(`${dir}/${f}`).split("-- +goose Down")[0];
    for (const m of up.matchAll(/(CREATE TABLE(?: IF NOT EXISTS)?|DROP TABLE(?: IF EXISTS)?|ALTER TABLE (\w+) RENAME TO)\s+(\w+)/g)) {
      if (m[1].startsWith("CREATE")) tables.add(m[3]);
      else if (m[1].startsWith("DROP")) tables.delete(m[3]);
      else {
        tables.delete(m[2]);
        tables.add(m[3]);
      }
    }
    for (const m of up.matchAll(/CREATE (?:OR REPLACE )?FUNCTION (?:\w+\.)?(\w+)\s*\(([\s\S]*?)\$\$/g)) fns.set(m[1], m[2].includes("SECURITY DEFINER"));
  }
  return { tables: tables.size, functions: fns.size, definers: [...fns.values()].filter(Boolean).length };
}

function stat(name) {
  if (!statCache.has(name)) {
    const d = config.stats?.[name];
    let v = null;
    if (d && statKinds[d.kind]) {
      try {
        v = statKinds[d.kind](d);
      } catch {
        v = null;
      }
    }
    statCache.set(name, v);
  }
  return statCache.get(name);
}

// packageInfo is a Go package's size (non-test lines) and the first
// sentence of its package comment.
function packageInfo(dir) {
  const abs = join(root, dir);
  if (!existsSync(abs)) return null;
  const gofiles = readdirSync(abs).filter((f) => f.endsWith(".go") && !f.endsWith("_test.go"));
  let doc = "";
  for (const f of gofiles) {
    const src = lines(`${dir}/${f}`);
    const at = src.findIndex((l) => /^\/\/ Package \w+/.test(l));
    if (at < 0) continue;
    const parts = [];
    // Indented lines in a package comment are code; the summary skips them.
    for (let i = at; i < src.length && src[i].startsWith("//"); i++) if (!/^\/\/\t/.test(src[i])) parts.push(src[i].replace(/^\/\/ ?/, "").trim());
    doc = parts.join(" ").replace(/\s+/g, " ");
    break;
  }
  const sentence = (doc.match(/^.*?\.(?=\s+[A-Z(]|\s*$)/) ?? [doc])[0];
  return { lines: countLines(gofiles.map((f) => `${dir}/${f}`)), doc: sentence.length > 260 ? sentence.slice(0, 257) + "…" : sentence };
}

// ---------------------------------------------------------------- pages

function loadPages() {
  const dir = join(here, "pages");
  const order = new Map(config.parts.map((p, i) => [p.id, i]));
  return readdirSync(dir)
    .filter((f) => f.endsWith(".html"))
    .map((f) => {
      const raw = readFileSync(join(dir, f), "utf8");
      const m = raw.match(/^<!--atlas\s*([\s\S]*?)-->\s*/);
      if (!m) throw new Error(`pages/${f}: no <!--atlas {...} --> header`);
      const meta = JSON.parse(m[1]);
      return { status: "written", ...meta, file: f, body: raw.slice(m[0].length) };
    })
    .sort((a, b) => (a.part ? (order.get(a.part) ?? 99) : -1) - (b.part ? (order.get(b.part) ?? 99) : -1) || (a.order ?? 0) - (b.order ?? 0));
}

function attrs(s) {
  const out = {};
  for (const m of s.matchAll(/([\w-]+)(?:\s*=\s*"([^"]*)")?/g)) {
    out[m[1]] = m[2] === undefined ? true : m[2].replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
  }
  return out;
}

const slug = (s) =>
  s
    .replace(/<[^>]+>/g, "")
    .toLowerCase()
    .replace(/&[a-z]+;/g, "")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 48);

const fmt = (n) => (typeof n === "number" ? n.toLocaleString("en-GB") : String(n));

function renderPage(page, pages, problems, placed) {
  const problem = (msg) => problems.push({ page: page.id, msg });
  let body = page.body;
  let uid = 0;

  body = body.replace(/<x-include\b([^>]*?)\/>/g, (_, a) => {
    const { name } = attrs(a);
    const path = join(here, "partials", name);
    if (!existsSync(path)) {
      problem(`x-include: no partials/${name}`);
      return "";
    }
    uid++;
    return readFileSync(path, "utf8").replace(/\{\{uid\}\}/g, `${page.id}-${uid}`);
  });

  body = body.replace(/<x-page\b([^>]*?)(?:\/>|>([\s\S]*?)<\/x-page>)/g, (_, a, text) => {
    const { id } = attrs(a);
    const target = pages.find((p) => p.id === id.split("--")[0]);
    if (!target) {
      problem(`x-page: no page ${id}`);
      return `<span class="xref broken">${text ?? id}</span>`;
    }
    return `<a class="xref" href="#${id}">${text ?? esc(target.title)}</a>`;
  });

  body = body.replace(/<x-ref\b([^>]*?)(?:\/>|>([\s\S]*?)<\/x-ref>)/g, (_, a, text) => {
    const { path, symbol } = attrs(a);
    if (path.endsWith("/")) {
      if (!existsSync(join(root, path))) problem(`x-ref: no directory ${path}`);
      return `<a class="ref" href="${blobURL(path)}">${text ?? `<code>${esc(path)}</code>`}</a>`;
    }
    const src = lines(path);
    if (!src) {
      problem(`x-ref: no file ${path}`);
      return `<span class="ref broken" title="missing: ${esc(path)}">${text ?? esc(path)}</span>`;
    }
    if (!symbol) return `<a class="ref" href="${blobURL(path)}">${text ?? `<code>${esc(path)}</code>`}</a>`;
    const at = findLine(src, symbol);
    if (at < 0) {
      problem(`x-ref: "${symbol}" not found in ${path}`);
      return `<span class="ref broken" title="not found in ${esc(path)}: ${esc(symbol)}">${text ?? `<code>${esc(symbol)}</code>`}</span>`;
    }
    return `<a class="ref" href="${blobURL(path, at)}" title="${esc(path)}:${at + 1}">${text ?? `<code>${esc(path.split("/").pop())}:${at + 1}</code>`}</a>`;
  });

  body = body.replace(/<x-code\b([^>]*?)\/>/g, (_, a) => {
    const o = attrs(a);
    const src = lines(o.path);
    if (!src) {
      problem(`x-code: no file ${o.path}`);
      return `<div class="code broken">Missing file <code>${esc(o.path)}</code></div>`;
    }
    const at = o.start ? findLine(src, o.start) : 0;
    if (at < 0) {
      problem(`x-code: "${o.start}" not found in ${o.path}`);
      return `<div class="code broken"><code>${esc(o.start)}</code> is no longer in <code>${esc(o.path)}</code>; this excerpt needs a new anchor.</div>`;
    }
    let from = at;
    if (o.doc) while (from > 0 && /^\s*(\/\/|--)/.test(src[from - 1])) from--;
    let to;
    if (o.end) {
      to = findLine(src, o.end, at + 1);
      if (to < 0) {
        problem(`x-code: end "${o.end}" not found after "${o.start}" in ${o.path}`);
        to = at;
      }
    } else if (o.lines) to = Math.min(src.length - 1, at + Number(o.lines) - 1);
    else to = blockEnd(src, at);
    const max = Number(o.max ?? 80);
    const total = to - from + 1;
    const shown = Math.min(total, max);
    const hl = highlight(src.slice(from, from + shown).join("\n"), o.lang ?? langOf(o.path));
    const rows = hl.map((h, i) => `<span class="ln"><a class="n" href="${blobURL(o.path, from + i)}" tabindex="-1">${from + i + 1}</a><span class="t">${h || " "}</span></span>`).join("");
    const more = total > shown ? `<a class="more" href="${blobURL(o.path, from, to)}">${total - shown} more lines in the file</a>` : "";
    const caption = o.caption ? `<span class="cap">${esc(o.caption)}</span>` : "";
    return `<figure class="code"><figcaption><a class="path" href="${blobURL(o.path, from, to)}">${esc(o.path)}<span class="at">:${from + 1}</span></a>${caption}<button class="copy" type="button">Copy</button></figcaption><pre><code>${rows}</code></pre>${more}</figure>`;
  });

  body = body.replace(/<x-stat\b([^>]*?)\/>/g, (_, a) => {
    const { name } = attrs(a);
    const v = stat(name);
    if (v === null) problem(`x-stat: unknown stat ${name}`);
    return `<span class="stat-v">${esc(fmt(v ?? "?"))}</span>`;
  });

  body = body.replace(/<x-packages\b([^>]*?)\/>/g, (_, a) => {
    const o = attrs(a);
    let names;
    if (o.rest) {
      names = packageDirs().filter((d) => !placed.has(d));
      if (names.length) problem(`x-packages: not on the package map yet: ${names.join(", ")}`);
      if (!names.length) return `<p class="muted">Every package under ${rootsLabel()} is placed above.</p>`;
    } else names = o.names.split(",").map((n) => n.trim()).filter(Boolean).map(qualify);
    const cards = names.map((dir) => {
      placed.add(dir);
      const info = packageInfo(dir);
      if (!info) {
        problem(`x-packages: no package ${dir}`);
        return `<li class="pkg broken"><span class="pkg-name">${esc(dir)}</span><span class="pkg-doc">No longer in the repository.</span></li>`;
      }
      const doc = info.doc.replace(/^Package \S+ (is |are |holds |runs |defines )?/, "");
      return `<li class="pkg"><a class="pkg-name" href="${blobURL(dir + "/")}">${esc(shortName(dir))}</a><span class="pkg-lines">${fmt(info.lines)} lines</span><span class="pkg-doc">${esc(doc.charAt(0).toUpperCase() + doc.slice(1))}</span></li>`;
    });
    return `<ul class="pkg-grid">${cards.join("")}</ul>`;
  });

  body = body.replace(/<x-parts\s*\/>/g, () => partsGrid(pages));

  // Headings get stable ids, a link of their own, and a table of contents.
  const toc = [];
  body = body.replace(/<h([23])(\s[^>]*)?>([\s\S]*?)<\/h\1>/g, (whole, level, rest = "", inner) => {
    if (rest.includes("data-notoc")) return whole;
    const own = rest.match(/\sid="([^"]+)"/);
    const id = own ? own[1] : `${page.id}--${slug(inner)}`;
    const text = inner.replace(/<[^>]+>/g, "");
    toc.push({ level: Number(level), id, text });
    const r = own ? rest : `${rest} id="${id}"`;
    return `<h${level}${r}>${inner}<a class="anchor" href="#${id}" aria-label="Link to this section">#</a></h${level}>`;
  });
  const ids = new Set();
  for (const t of toc) {
    if (ids.has(t.id)) problem(`duplicate heading id ${t.id}`);
    ids.add(t.id);
  }
  return { body, toc };
}

function partsGrid(pages) {
  return `<div class="parts">${config.parts
    .filter((part) => part.id !== "Z")
    .map((part) => {
      const list = pages.filter((p) => p.part === part.id);
      return `<section class="part"><h3 class="part-title" data-notoc><span class="part-id">${part.id}</span>${esc(part.title)}</h3><ol class="part-pages">${list
        .map((p) => `<li><a href="#${p.id}"><span class="pp-title">${esc(p.title)}</span><span class="status s-${p.status}">${p.status === "planned" ? "Planned" : "Written"}</span></a><span class="pp-sum">${esc(p.summary ?? "")}</span></li>`)
        .join("")}</ol></section>`;
    })
    .join("")}</div>`;
}

function plannedBody(page) {
  const scope = (page.scope ?? []).map((s) => `<li>${s}</li>`).join("");
  const src = (page.sources ?? []).map((s) => `<li><a class="ref" href="${blobURL(s)}"><code>${esc(s)}</code></a></li>`).join("");
  return `<div class="planned"><p class="planned-note">This page is planned. It will be written from the code and checked against it like the others; until then, here is what it will cover.</p><h2 id="${page.id}--scope">What this page will cover</h2><ul>${scope}</ul>${src ? `<h2 id="${page.id}--sources">Code it will be checked against</h2><ul class="src-list">${src}</ul>` : ""}</div>`;
}

function pageHeader(page, fresh) {
  const part = config.parts.find((p) => p.id === page.part);
  const eyebrow = part ? `<p class="eyebrow">Part ${part.id} · ${esc(part.title)}</p>` : "";
  let stamp = "";
  if (page.status !== "planned" && fresh) {
    const at = `<a href="${config.repo}/commit/${page.verified}"><code>${esc(page.verified.slice(0, 7))}</code></a>${fresh.date ? ` · ${fresh.date}` : ""}`;
    let state;
    if (!fresh.known) state = `<span class="fresh unknown">Checked-at commit not in this clone</span>`;
    else if (fresh.count === 0) state = `<span class="fresh ok">No changes to its code since</span>`;
    else
      state = `<details class="fresh moved"><summary>The code has moved: ${fresh.count} commit${fresh.count === 1 ? "" : "s"} since</summary><ul>${fresh.files
        .slice(0, 30)
        .map((f) => `<li><code>${esc(f)}</code></li>`)
        .join("")}${fresh.files.length > 30 ? `<li>and ${fresh.files.length - 30} more</li>` : ""}</ul></details>`;
    stamp = `<div class="stamp"><span>Checked against ${at}</span>${state}</div>`;
  }
  return `<header class="page-head">${eyebrow}<h1>${esc(page.title)}</h1>${page.summary ? `<p class="lede">${esc(page.summary)}</p>` : ""}${stamp}</header>`;
}

function navHTML(pages, freshByPage) {
  const home = pages.find((p) => p.id === config.home);
  const groups = config.parts
    .map((part) => {
      const list = pages.filter((p) => p.part === part.id);
      if (!list.length) return "";
      return `<li class="nav-part"><span class="nav-part-title">${part.id !== "Z" ? `<span class="part-id">${part.id}</span>` : ""}${esc(part.title)}</span><ul>${list
        .map((p) => {
          const f = freshByPage.get(p.id);
          const dot = p.status === "planned" ? "planned" : f?.count ? "moved" : "written";
          const label = { planned: "Planned", moved: "Code has moved since this was checked", written: "Written" }[dot];
          return `<li><a href="#${p.id}" data-page="${p.id}"><span class="dot d-${dot}" title="${label}" aria-label="${label}"></span>${esc(p.title)}</a></li>`;
        })
        .join("")}</ul></li>`;
    })
    .join("");
  return `<ul class="nav-list"><li><a class="nav-home" href="#${home.id}" data-page="${home.id}">Start here</a></li>${groups}</ul>`;
}

function searchEntries(page, html) {
  const out = [];
  const parts = html.split(/(?=<h[23][\s>])/);
  for (const chunk of parts) {
    const hm = chunk.match(/^<h[23][^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/h[23]>/);
    const text = chunk
      .replace(/^<h[23][\s\S]*?<\/h[23]>/, "")
      .replace(/<svg[\s\S]*?<\/svg>/g, " ")
      .replace(/<span class="n"[^>]*>.*?<\/span>|<a class="n"[^>]*>.*?<\/a>/g, "")
      .replace(/<[^>]+>/g, " ")
      .replace(/&nbsp;/g, " ")
      .replace(/&amp;/g, "&")
      .replace(/&lt;/g, "<")
      .replace(/&gt;/g, ">")
      .replace(/&quot;/g, '"')
      .replace(/&#?\w+;/g, " ")
      .replace(/\s+/g, " ")
      .trim();
    if (!text && !hm) continue;
    out.push({ p: page.id, s: hm ? hm[1] : page.id, h: hm ? hm[2].replace(/<[^>]+>/g, "").replace(/#$/, "") : page.title, t: text.slice(0, 1200) });
  }
  return out;
}

function build() {
  const pages = loadPages();
  const problems = [];
  const placed = new Set();
  const freshByPage = new Map(pages.map((p) => [p.id, freshness(p)]));
  if (!pages.some((p) => p.id === config.home)) throw new Error(`no home page "${config.home}"`);
  const ids = new Set();
  for (const p of pages) {
    if (ids.has(p.id)) problems.push({ page: p.id, msg: "duplicate page id" });
    ids.add(p.id);
    if (!/^[a-z][a-z0-9-]*$/.test(p.id) || p.id.includes("--")) problems.push({ page: p.id, msg: "page ids are lowercase words joined by single hyphens" });
  }
  // The rest of the package map is rendered after every listed package is known.
  const rendered = [];
  for (const p of [...pages].sort((a, b) => Number(/<x-packages\s+rest/.test(a.body)) - Number(/<x-packages\s+rest/.test(b.body)))) {
    const src = p.status === "planned" && !p.body.trim() ? plannedBody(p) : p.body;
    rendered.push([p.id, renderPage({ ...p, body: src }, pages, problems, placed)]);
  }
  const byId = new Map(rendered);
  const search = [];
  const articles = pages
    .map((p, i) => {
      const { body, toc } = byId.get(p.id);
      search.push(...searchEntries(p, body));
      const prev = pages[i - 1];
      const next = pages[i + 1];
      const pager = `<nav class="pager" aria-label="Previous and next page">${prev ? `<a class="prev" href="#${prev.id}"><span>Previous</span>${esc(prev.title)}</a>` : "<span></span>"}${next ? `<a class="next" href="#${next.id}"><span>Next</span>${esc(next.title)}</a>` : ""}</nav>`;
      const tocJSON = esc(JSON.stringify(toc.filter((t) => t.level === 2 || t.level === 3)));
      return `<article class="page${p.id === config.home ? " home" : ""}" id="${p.id}" data-title="${esc(p.title)}" data-toc="${tocJSON}" hidden>${p.id === config.home ? "" : pageHeader(p, freshByPage.get(p.id))}<div class="prose">${body}</div>${pager}</article>`;
    })
    .join("\n");

  // Every in-atlas link must land on a page or a heading.
  const anchors = new Set([...articles.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1]));
  let inPage = "?";
  for (const [, page, target] of articles.matchAll(/<article class="page[^"]*" id="([^"]+)"|href="#([^"]+)"/g)) {
    if (page) inPage = page;
    else if (!anchors.has(target)) problems.push({ page: inPage, msg: `link to #${target}, which no page or heading has` });
  }

  const data = { home: config.home, title: config.title, pages: pages.map((p) => ({ id: p.id, title: p.title, part: p.part ?? "" })), search };
  const css = readFileSync(join(here, "src/atlas.css"), "utf8");
  const js = readFileSync(join(here, "src/atlas.js"), "utf8");
  const shell = readFileSync(join(here, "src/shell.html"), "utf8");
  const fill = (tpl, vars) => tpl.replace(/\{\{(\w+)\}\}/g, (_, k) => vars[k] ?? "");
  const [headTpl, bodyTpl] = shell.split("<!-- body -->");
  const vars = {
    title: esc(config.title),
    tagline: esc(config.tagline),
    css,
    js,
    nav: navHTML(pages, freshByPage),
    articles,
    data: JSON.stringify(data).replace(/</g, "\\u003c"),
    commit: headShort,
    commitURL: `${config.repo}/commit/${head}`,
    date: headDate,
    repo: config.repo,
    // The brand in the top bar: the title's last word set apart ("Xenibase Atlas").
    brandName: esc(config.title.replace(/\s+\S+$/, "")),
    brandSuffix: esc(config.title.match(/\S+$/)?.[0] ?? ""),
  };
  const headHTML = fill(headTpl, vars).trim();
  const bodyHTML = fill(bodyTpl, vars).trim();
  return {
    problems,
    pages,
    freshByPage,
    standalone: `<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">\n${headHTML}\n</head>\n<body>\n${bodyHTML}\n</body>\n</html>\n`,
    artifact: `${headHTML}\n${bodyHTML}\n`,
  };
}

function report(problems) {
  for (const p of problems) console.error(`atlas: ${p.page}: ${p.msg}`);
}

// ---------------------------------------------------------------- commands

const cmd = process.argv[2] ?? "build";
if (cmd === "build") {
  const out = build();
  mkdirSync(join(here, "site"), { recursive: true });
  mkdirSync(join(here, "dist"), { recursive: true });
  writeFileSync(join(here, "site/index.html"), out.standalone);
  writeFileSync(join(here, "dist/artifact.html"), out.artifact);
  report(out.problems);
  const kb = (s) => `${Math.round(Buffer.byteLength(s) / 1024)} KB`;
  console.log(`atlas: ${out.pages.length} pages at ${headShort}; site/index.html ${kb(out.standalone)}, dist/artifact.html ${kb(out.artifact)}; ${out.problems.length} problem(s)`);
} else if (cmd === "check") {
  const out = build();
  report(out.problems);
  if (out.problems.length) process.exit(1);
  console.log(`atlas: ${out.pages.length} pages, every reference resolves at ${headShort}`);
} else if (cmd === "stale") {
  const pages = loadPages().filter((p) => p.status !== "planned");
  const rows = pages.map((p) => ({ p, f: freshness(p) }));
  const moved = rows.filter((r) => r.f && (!r.f.known || r.f.count > 0));
  for (const { p, f } of rows) {
    const state = !f ? "no sources declared" : !f.known ? `checked at ${p.verified}, which this clone does not have` : f.count ? `${f.count} commit(s) since ${p.verified.slice(0, 7)}` : "current";
    console.log(`${p.id.padEnd(28)} ${state}`);
    if (f?.count) for (const file of f.files.slice(0, 15)) console.log(`${"".padEnd(30)}${file}`);
  }
  console.log(moved.length ? `\n${moved.length} page(s) to re-check. Update each page, then set its "verified" to ${headShort}.` : "\nEvery written page is current.");
} else {
  console.error("usage: node docs/atlas/atlas.mjs build | check | stale");
  process.exit(2);
}
