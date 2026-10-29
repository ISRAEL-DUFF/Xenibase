// The atlas's page script: one HTML file, pages chosen by the hash
// (#page or #page--section), search over an index the build embeds, the
// colour theme, the "on this page" rail, the run stepper, and diagrams
// whose boxes open the page about them.
(function () {
  "use strict";
  const data = JSON.parse(document.getElementById("atlas-data").textContent);
  const pages = new Map(data.pages.map((p) => [p.id, p]));
  const main = document.getElementById("main");
  const tocList = document.getElementById("toc-list");
  let current = null;
  let spy = null;

  // ------------------------------------------------------------ routing

  function parseHash() {
    const h = decodeURIComponent(location.hash.replace(/^#/, ""));
    const page = h.split("--")[0];
    return { page: pages.has(page) ? page : data.home, section: h.includes("--") ? h : "" };
  }

  function route() {
    const { page, section } = parseHash();
    if (page !== current) {
      document.querySelectorAll("article.page").forEach((a) => {
        a.hidden = a.id !== page;
      });
      current = page;
      const article = document.getElementById(page);
      document.title = page === data.home ? data.title : `${article.dataset.title} · ${data.title}`;
      document.querySelectorAll(".nav a[data-page]").forEach((a) => {
        if (a.dataset.page === page) a.setAttribute("aria-current", "page");
        else a.removeAttribute("aria-current");
      });
      buildToc(article);
      closeNav();
    }
    if (section) {
      const el = document.getElementById(section);
      if (el) {
        el.scrollIntoView();
        return;
      }
    }
    window.scrollTo(0, 0);
    main.focus({ preventScroll: true });
  }

  function buildToc(article) {
    let toc;
    try {
      toc = JSON.parse(article.dataset.toc || "[]");
    } catch {
      toc = [];
    }
    tocList.textContent = "";
    for (const t of toc) {
      const li = document.createElement("li");
      li.className = `l${t.level}`;
      const a = document.createElement("a");
      a.href = `#${t.id}`;
      a.textContent = t.text;
      li.appendChild(a);
      tocList.appendChild(li);
    }
    tocList.parentElement.hidden = toc.length === 0;
    if (spy) spy.disconnect();
    if (!("IntersectionObserver" in window) || toc.length === 0) return;
    const links = new Map([...tocList.querySelectorAll("a")].map((a) => [a.hash.slice(1), a]));
    spy = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (!e.isIntersecting) continue;
          links.forEach((a) => a.classList.remove("on"));
          const a = links.get(e.target.id);
          if (a) a.classList.add("on");
        }
      },
      { rootMargin: "-15% 0px -70% 0px" },
    );
    toc.forEach((t) => {
      const el = document.getElementById(t.id);
      if (el) spy.observe(el);
    });
  }

  // ------------------------------------------------------------ contents drawer

  const menuButton = document.getElementById("menu-button");
  const scrim = document.getElementById("scrim");
  function openNav() {
    document.body.classList.add("nav-open");
    menuButton.setAttribute("aria-expanded", "true");
    scrim.hidden = false;
  }
  function closeNav() {
    document.body.classList.remove("nav-open");
    menuButton.setAttribute("aria-expanded", "false");
    scrim.hidden = true;
  }
  menuButton.addEventListener("click", () => (document.body.classList.contains("nav-open") ? closeNav() : openNav()));
  scrim.addEventListener("click", closeNav);

  // ------------------------------------------------------------ theme

  const themes = ["system", "light", "dark"];
  const themeButton = document.getElementById("theme-button");
  const themeLabel = document.getElementById("theme-label");
  function applyTheme(t) {
    if (t === "system") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.setAttribute("data-theme", t);
    const label = t.charAt(0).toUpperCase() + t.slice(1);
    themeLabel.textContent = label;
    themeButton.setAttribute("aria-label", `Colour theme: ${label}. Change it`);
  }
  let theme = "system";
  try {
    const saved = localStorage.getItem("atlas-theme");
    if (themes.includes(saved)) theme = saved;
  } catch {
    // Storage may be unavailable; the system theme applies.
  }
  applyTheme(theme);
  themeButton.addEventListener("click", () => {
    theme = themes[(themes.indexOf(theme) + 1) % themes.length];
    applyTheme(theme);
    try {
      localStorage.setItem("atlas-theme", theme);
    } catch {
      // Not remembered; it still applies now.
    }
  });

  // ------------------------------------------------------------ search

  const input = document.getElementById("search-input");
  const results = document.getElementById("search-results");
  const index = data.search.map((e) => ({ ...e, lh: e.h.toLowerCase(), lt: e.t.toLowerCase(), lp: (pages.get(e.p)?.title ?? "").toLowerCase() }));
  let selected = -1;

  function snippet(text, terms) {
    const lower = text.toLowerCase();
    let at = -1;
    for (const t of terms) {
      const i = lower.indexOf(t);
      if (i >= 0 && (at < 0 || i < at)) at = i;
    }
    const start = Math.max(0, at - 60);
    let s = (start > 0 ? "…" : "") + text.slice(start, start + 170) + (start + 170 < text.length ? "…" : "");
    s = s.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[c]);
    for (const t of terms) {
      const safe = t.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[c]).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      s = s.replace(new RegExp(`(${safe})`, "gi"), "<mark>$1</mark>");
    }
    return s;
  }

  function search(q) {
    const terms = q.toLowerCase().split(/\s+/).filter((t) => t.length > 1);
    if (!terms.length) return [];
    const scored = [];
    for (const e of index) {
      let score = 0;
      let all = true;
      for (const t of terms) {
        const inH = e.lh.includes(t);
        const inP = e.lp.includes(t);
        const inT = e.lt.includes(t);
        if (!inH && !inP && !inT) {
          all = false;
          break;
        }
        score += (inH ? 6 : 0) + (inP ? 2 : 0) + (inT ? 1 : 0);
      }
      if (all) scored.push({ e, score });
    }
    return scored.sort((a, b) => b.score - a.score).slice(0, 12);
  }

  function renderResults() {
    const q = input.value.trim();
    results.textContent = "";
    selected = -1;
    if (!q) {
      results.hidden = true;
      input.setAttribute("aria-expanded", "false");
      return;
    }
    const found = search(q);
    const terms = q.toLowerCase().split(/\s+/).filter((t) => t.length > 1);
    if (!found.length) {
      const li = document.createElement("li");
      li.className = "sr-empty";
      li.textContent = `Nothing in the atlas mentions “${q}”.`;
      results.appendChild(li);
    }
    for (const { e } of found) {
      const li = document.createElement("li");
      const a = document.createElement("a");
      a.href = `#${e.s}`;
      const where = document.createElement("span");
      where.className = "sr-where";
      where.textContent = pages.get(e.p)?.title ?? "";
      const head = document.createElement("span");
      head.className = "sr-head";
      head.textContent = e.h;
      const snip = document.createElement("span");
      snip.className = "sr-snip";
      snip.innerHTML = snippet(e.t, terms);
      a.append(where, head, snip);
      a.addEventListener("click", () => {
        results.hidden = true;
        input.value = "";
      });
      li.appendChild(a);
      results.appendChild(li);
    }
    results.hidden = false;
    input.setAttribute("aria-expanded", "true");
  }

  function move(delta) {
    const links = [...results.querySelectorAll("a")];
    if (!links.length) return;
    selected = (selected + delta + links.length) % links.length;
    links.forEach((a, i) => a.setAttribute("aria-selected", String(i === selected)));
    links[selected].scrollIntoView({ block: "nearest" });
  }

  input.addEventListener("input", renderResults);
  input.addEventListener("keydown", (ev) => {
    if (ev.key === "ArrowDown") {
      ev.preventDefault();
      move(1);
    } else if (ev.key === "ArrowUp") {
      ev.preventDefault();
      move(-1);
    } else if (ev.key === "Enter") {
      const links = results.querySelectorAll("a");
      const a = links[selected >= 0 ? selected : 0];
      if (a) {
        location.hash = a.hash;
        results.hidden = true;
        input.value = "";
        input.blur();
      }
    } else if (ev.key === "Escape") {
      input.value = "";
      renderResults();
      input.blur();
    }
  });
  document.addEventListener("keydown", (ev) => {
    const t = ev.target;
    const typing = t instanceof HTMLElement && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
    if (ev.key === "/" && !typing) {
      ev.preventDefault();
      input.focus();
    }
    if (ev.key === "Escape") closeNav();
  });
  document.addEventListener("click", (ev) => {
    if (!(ev.target instanceof Node) || !input.parentElement.contains(ev.target)) results.hidden = true;
  });

  // ------------------------------------------------------------ code: copy

  document.querySelectorAll("figure.code .copy").forEach((b) => {
    b.addEventListener("click", () => {
      const fig = b.closest("figure");
      const text = [...fig.querySelectorAll(".ln .t")].map((t) => t.textContent).join("\n");
      const done = (label) => {
        b.textContent = label;
        setTimeout(() => (b.textContent = "Copy"), 1600);
      };
      const select = () => {
        const range = document.createRange();
        range.selectNodeContents(fig.querySelector("code"));
        const sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
        done("Selected");
      };
      try {
        navigator.clipboard.writeText(text).then(() => done("Copied"), select);
      } catch {
        select();
      }
    });
  });

  // ------------------------------------------------------------ diagrams

  document.querySelectorAll("[data-go]").forEach((g) => {
    g.setAttribute("tabindex", "0");
    g.setAttribute("role", "link");
    const go = () => {
      location.hash = `#${g.getAttribute("data-go")}`;
    };
    g.addEventListener("click", go);
    g.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter" || ev.key === " ") {
        ev.preventDefault();
        go();
      }
    });
  });

  // ------------------------------------------------------------ stepper

  document.querySelectorAll(".stepper").forEach((st) => {
    const svg = st.querySelector("svg");
    const stages = [...st.querySelectorAll(".stage")];
    const count = st.querySelector(".count");
    let at = 0;
    function select(i, scroll) {
      at = (i + stages.length) % stages.length;
      stages.forEach((s, j) => {
        s.classList.toggle("on", j === at);
        s.setAttribute("aria-current", j === at ? "step" : "false");
      });
      const lit = (stages[at].dataset.lit || "").split(/\s+/);
      if (svg) {
        svg.classList.add("stepping");
        svg.querySelectorAll("[data-stage]").forEach((g) => g.classList.toggle("lit", lit.includes(g.getAttribute("data-stage"))));
      }
      if (count) count.textContent = `Stage ${at + 1} of ${stages.length}`;
      if (scroll) stages[at].scrollIntoView({ block: "nearest", behavior: "smooth" });
    }
    stages.forEach((s, i) => {
      s.setAttribute("tabindex", "0");
      s.addEventListener("click", (ev) => {
        if (ev.target instanceof Element && ev.target.closest("a")) return;
        select(i, false);
      });
      s.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter" || ev.key === " ") {
          ev.preventDefault();
          select(i, false);
        }
      });
    });
    st.querySelector(".prev")?.addEventListener("click", () => select(at - 1, true));
    st.querySelector(".next")?.addEventListener("click", () => select(at + 1, true));
    select(0, false);
  });

  window.addEventListener("hashchange", route);
  route();
})();
