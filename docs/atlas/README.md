# Xenibase Atlas

The engineering guide to this repository: how Xenibase is built, feature by feature and component by component, checked against the code. Open [site/index.html](site/index.html) in a browser. It is one self-contained file.

`dist/artifact.html` is the same atlas without the document wrapper, for publishing to a host that adds its own (such as a claude.ai artifact). Republish it after a rebuild.

Each page records the commit it was last checked against. Code excerpts, line links, package summaries and counts are read from the repository every time the atlas is built, so they follow the code. Prose and diagrams need a person. `make atlas-stale` says which pages need one.

## Commands

| Command | What it does |
| --- | --- |
| `make atlas` | Build `site/index.html` (commit it) and `dist/artifact.html` (the same atlas without the document wrapper, for hosts that add their own). Prints any reference that no longer resolves. |
| `make atlas-stale` | List each written page with the number of commits that have touched its sources since it was checked, and the files. |
| `node docs/atlas/atlas.mjs check` | Build in memory and exit non-zero if any reference is broken. |

Node 22 and git are all it needs.

## Updating a page

1. Run `make atlas-stale` and read the diff of the listed files since the page's `verified` commit.
2. Change what the page says in `pages/<page>.html`.
3. Set `"verified"` in the page's header to the commit you checked.
4. Run `make atlas` and commit the page with `site/index.html`.

## Layout

| Path | What it is |
| --- | --- |
| [pages/](pages/) | One HTML fragment per page, starting with a `<!--atlas {...} -->` JSON header: `id`, `title`, `part`, `order`, `summary`, `verified`, `sources`, and for a planned page `status: "planned"` and `scope`. |
| [partials/](partials/) | Diagrams included with `<x-include name="…"/>`. |
| [src/](src/) | The shell, stylesheet and page script shared by every page. |
| [atlas.json](atlas.json) | Title, repository URL, parts, the package root (`internal`) and the definitions of the counts pages show (`stats`). |
| [atlas.mjs](atlas.mjs) | The build. |

The elements a page can use (`x-ref`, `x-code`, `x-packages`, `x-stat`, `x-page`, `x-include`) and the classes the page script understands are described on the atlas's own page, *How the atlas is maintained*.
