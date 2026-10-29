package edge

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// testCatalog: authors <- posts (author_id, editor_id) <- comments.
func testCatalog() *Catalog {
	mk := func(name string, rls bool, cols ...*Column) *Table {
		t := &Table{Schema: "public", Name: name, Kind: kindTable, RLS: rls, ByName: map[string]*Column{}}
		for _, c := range cols {
			t.Columns = append(t.Columns, c)
			t.ByName[c.Name] = c
		}
		return t
	}
	col := func(name, typ, typname string, cat byte) *Column {
		return &Column{Name: name, Type: typ, TypName: typname, Category: cat, Nullable: true}
	}
	authors := mk("authors", true, col("id", "uuid", "uuid", 'U'), col("name", "text", "text", 'S'))
	authors.PK = []string{"id"}
	posts := mk("posts", true, col("id", "bigint", "int8", 'N'), col("title", "text", "text", 'S'),
		col("author_id", "uuid", "uuid", 'U'), col("editor_id", "uuid", "uuid", 'U'), col("meta", "jsonb", "jsonb", 'U'),
		col("created_at", "timestamp with time zone", "timestamptz", 'D'), col("tags", "text[]", "_text", 'A'))
	posts.PK = []string{"id"}
	comments := mk("comments", false, col("id", "bigint", "int8", 'N'), col("post_id", "bigint", "int8", 'N'), col("body", "text", "text", 'S'))
	comments.PK = []string{"id"}
	fk := func(name string, t *Table, c string, ref *Table, rc string) {
		f := &ForeignKey{Name: name, Table: t, Cols: []string{c}, Ref: ref, RefCols: []string{rc}}
		t.Out = append(t.Out, f)
		ref.In = append(ref.In, f)
	}
	fk("posts_author_id_fkey", posts, "author_id", authors, "id")
	fk("posts_editor_id_fkey", posts, "editor_id", authors, "id")
	fk("comments_post_id_fkey", comments, "post_id", posts, "id")
	cat := &Catalog{Schemas: []string{"public"}, ByName: map[string]*Table{}}
	for _, t := range []*Table{authors, posts, comments} {
		cat.ByName["public."+t.Name] = t
		cat.Ordered = append(cat.Ordered, t)
	}
	return cat
}

func TestParseSelect(t *testing.T) {
	items, err := ParseSelect("id, t:title, author(name), editor:authors!editor_id(name), comments(id,body), meta->plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 || items[1].Alias != "t" || items[2].Embed == nil || items[3].Embed.Hint != "editor_id" || items[3].Alias != "editor" ||
		items[5].Column != "meta" || items[5].Path[0] != "plan" {
		t.Fatalf("items: %+v", items)
	}
	// Odd names parse (quoted identifiers allow them) and are refused unless
	// the catalog has them; these don't parse at all.
	for _, bad := range []string{"a(b", "a)", "x:*", "a(b(c(d(e))))", "a'b", "a b"} {
		if _, err := ParseSelect(bad, 0); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestParseConditions(t *testing.T) {
	f, err := ParseCondition("created_at:gte:2026-10-01T10:00:00Z")
	if err != nil || f.Column != "created_at" || f.Op != "gte" || f.Value != "2026-10-01T10:00:00Z" {
		t.Fatalf("%+v %v", f, err)
	}
	f, err = ParseCondition(`tags:in:a,"b,c",d`)
	if err != nil || len(f.Values) != 3 || f.Values[1] != "b,c" {
		t.Fatalf("in: %+v %v", f, err)
	}
	g, err := ParseOr("status:eq:draft,owner_id:in:1,2,3,meta->plan:eq:pro")
	if err != nil || len(g.Or) != 3 || len(g.Or[1].Values) != 3 || g.Or[2].Path[0] != "plan" {
		t.Fatalf("or: %+v %v", g, err)
	}
	for _, bad := range []string{"a:eq", "a:nope:1", "a:is:maybe", "x y:eq:1"} {
		if _, err := ParseCondition(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestReadSQL(t *testing.T) {
	cat := testCatalog()
	posts := cat.Find("posts")
	sel, _ := ParseSelect("id,title,author(name),comments(body),meta->plan", 0)
	w1, _ := ParseCondition("title:ilike:%go%")
	w2, _ := ParseCondition("meta->plan:eq:pro")
	b := &builder{cat: cat}
	st, err := b.read(posts, Query{Select: sel, Where: Filter{And: []Filter{w1, w2}}, Order: []Order{{Column: "created_at", Desc: true}}, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`FROM "public"."posts" t1`,
		`t1."title"::text ILIKE $1`,
		`t1."meta"->>'plan' = $2`,
		`ORDER BY t1."created_at" DESC, t1."id"`,
		`LIMIT 20`,
		`(SELECT row_to_json(r) FROM (SELECT t2."name" AS "name" FROM "public"."authors" t2 WHERE t2."id" = t1."author_id" LIMIT 1) r) AS "author"`,
		`FROM "public"."comments" t3 WHERE t3."post_id" = t1."id" ORDER BY t3."id" LIMIT 1000`,
		`t1."meta"->'plan' AS "plan"`,
	} {
		if !strings.Contains(st.SQL, want) {
			t.Errorf("SQL lacks %s:\n%s", want, st.SQL)
		}
	}
	if len(st.Args) != 2 || st.Args[0] != "%go%" {
		t.Fatalf("args: %v", st.Args)
	}
	if !strings.Contains(st.CountSQL, `count(*) FROM "public"."posts" t1 WHERE`) {
		t.Fatalf("count: %s", st.CountSQL)
	}

	// Two foreign keys to authors: "authors" is ambiguous, a hint picks one.
	sel, _ = ParseSelect("id,authors(name)", 0)
	if _, err := (&builder{cat: cat}).read(posts, Query{Select: sel}); err == nil || asAPIError(err).Code != "ambiguous_relation" {
		t.Fatalf("ambiguous: %v", err)
	}
	sel, _ = ParseSelect("id,authors!editor_id(name)", 0)
	if _, err := (&builder{cat: cat}).read(posts, Query{Select: sel}); err != nil {
		t.Fatalf("hinted: %v", err)
	}
	// Names come from the catalog only.
	w, _ := ParseCondition("nope:eq:1")
	if _, err := (&builder{cat: cat}).read(posts, Query{Select: []SelectItem{{Star: true}}, Where: Filter{And: []Filter{w}}}); err == nil ||
		asAPIError(err).Code != "unknown_column" {
		t.Fatalf("unknown column: %v", err)
	}
	// The gate applies to embedded tables.
	p := &project{}
	sel, _ = ParseSelect("id,comments(body)", 0)
	if _, err := (&builder{cat: cat, gate: p.gateFor(cat, "anon")}).read(posts, Query{Select: sel}); err == nil || asAPIError(err).Code != "rls_required" {
		t.Fatalf("gate on an embed: %v", err)
	}
}

func TestCursor(t *testing.T) {
	cat := testCatalog()
	posts := cat.Find("posts")
	keys, _ := orderKeys(posts, []Order{{Column: "created_at", Desc: true}})
	ts, id := "2026-10-01 10:00:00+00", "7"
	cur := encodeCursor(posts, keys, []*string{&ts, &id})
	b := &builder{cat: cat}
	cond, err := b.after(posts, "t1", keys, cur)
	if err != nil {
		t.Fatal(err)
	}
	want := `((t1."created_at" < $1::timestamp with time zone) OR (t1."created_at" = $2::timestamp with time zone AND (t1."id" > $3::bigint OR t1."id" IS NULL)))`
	if cond != want {
		t.Fatalf("after:\n got %s\nwant %s", cond, want)
	}
	// A NULL descending sorts first: everything not NULL follows it.
	cur = encodeCursor(posts, keys, []*string{nil, &id})
	if cond, err = (&builder{cat: cat}).after(posts, "t1", keys, cur); err != nil || !strings.Contains(cond, `t1."created_at" IS NOT NULL`) {
		t.Fatalf("null cursor: %s %v", cond, err)
	}
	// Another order's cursor is refused.
	other, _ := orderKeys(posts, []Order{{Column: "title"}})
	if _, err := (&builder{cat: cat}).after(posts, "t1", other, cur); err == nil {
		t.Fatal("a cursor for another order was accepted")
	}
}

func TestExploreMalformedPath(t *testing.T) {
	e := New(Config{Name: "t", Domain: "x.invalid"})
	code, _, body := e.Explore(context.Background(), edgeapi.Project{Ref: "k7f3m2q9", State: edgeapi.StateActive}, "anon", nil, "GET", "/data/v1/%zz", nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "invalid_path") {
		t.Fatalf("%d %s", code, body)
	}
}
