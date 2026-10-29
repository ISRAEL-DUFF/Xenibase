package typegen

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/datacat"
)

func testCatalog() *datacat.Catalog {
	todos := &datacat.Table{Schema: "public", Name: "todos", Kind: datacat.KindTable, ByName: map[string]*datacat.Column{}}
	for _, c := range []*datacat.Column{
		{Name: "id", TypName: "int8", Category: 'N', HasDefault: true},
		{Name: "owner_id", TypName: "uuid", Category: 'U'},
		{Name: "title", TypName: "text", Category: 'S'},
		{Name: "done", TypName: "bool", Category: 'B', HasDefault: true},
		{Name: "meta", TypName: "jsonb", Category: 'U', Nullable: true},
		{Name: "tags", TypName: "_text", Category: 'A', Nullable: true},
		{Name: "status", TypName: "todo_status", Category: 'E', Enum: []string{"open", "closed"}, HasDefault: true},
		{Name: "created_at", TypName: "timestamptz", Category: 'D', HasDefault: true},
		{Name: "search", TypName: "tsvector", Category: 'U', Nullable: true, Generated: true},
	} {
		todos.Columns = append(todos.Columns, c)
		todos.ByName[c.Name] = c
	}
	view := &datacat.Table{Schema: "public", Name: "open_todos", Kind: datacat.KindView, Columns: todos.Columns[:3]}
	cat := &datacat.Catalog{Schemas: []string{"public"}, ByName: map[string]*datacat.Table{"public.todos": todos, "public.open_todos": view},
		Ordered: []*datacat.Table{view, todos}, Functions: map[string][]*datacat.Function{
			"public.search_todos": {{Schema: "public", Name: "search_todos", RetSet: true, Volatile: 's',
				Args:    []datacat.Arg{{Name: "q", TypName: "text", Category: 'S'}, {Name: "max", TypName: "int4", Category: 'N', HasDefault: true}},
				Columns: todos.Columns[:3]}},
		}}
	return cat
}

func TestTS(t *testing.T) {
	out, err := Generate(testCatalog(), TS, Options{Ref: "k7f3m2q9"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export interface Database {",
		"title: string;",
		"meta: Json | null;",
		"tags: string[] | null;",
		`status: "open" | "closed";`,
		"id?: number;",      // insert: has a default
		"owner_id: string;", // insert: required
		"search_todos: {",
		"Args: { q: string; max?: number; };",
		"export type Todos = Database[\"public\"][\"Tables\"][\"todos\"][\"Row\"];",
		"export type OpenTodos = Database[\"public\"][\"Views\"][\"open_todos\"][\"Row\"];",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("TS lacks %q:\n%s", want, out)
		}
	}
	// A generated column is in the row, never in an insert.
	ins := out[strings.Index(out, "Insert: {"):strings.Index(out, "Update: {")]
	if strings.Contains(ins, "search") {
		t.Error("the generated column is insertable")
	}
}

func TestGo(t *testing.T) {
	out, err := Generate(testCatalog(), Go, Options{Ref: "k7f3m2q9", Package: "todo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := format.Source([]byte(out)); err != nil {
		t.Fatalf("generated Go doesn't parse: %v\n%s", err, out)
	}
	for _, want := range []string{"package todo", "type Todos struct", "OwnerID string `json:\"owner_id\"`",
		"ID *int64 `json:\"id,omitempty\"`", "CreatedAt time.Time `json:\"created_at\"`", "const TodosTable = \"todos\"", "type TodosUpdate struct"} {
		if !strings.Contains(out, want) {
			t.Errorf("Go lacks %q:\n%s", want, out)
		}
	}
}

func TestDart(t *testing.T) {
	out, err := Generate(testCatalog(), Dart, Options{Ref: "k7f3m2q9"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"class Todos {", "final DateTime createdAt;", "createdAt: DateTime.parse(json['created_at'] as String),",
		"factory Todos.fromJson(Map<String, dynamic> json)", "'created_at': createdAt.toIso8601String(),", "final List<String>? tags;"} {
		if !strings.Contains(out, want) {
			t.Errorf("Dart lacks %q:\n%s", want, out)
		}
	}
	if _, err := Generate(testCatalog(), "cobol", Options{}); err == nil {
		t.Fatal("an unknown language")
	}
}

// evil is the 37-byte column name from the atlas (Part D, typed
// definitions): pasted into Go as it was, it closed the struct and declared
// func init() { panic("ran") }.
const evil = "x\tint\n}\nfunc\tinit()\t{\tpanic(\"ran\")\n//"

// hostile is a catalog whose table and column names are source code.
func hostile(column, price string) *datacat.Catalog {
	t := &datacat.Table{Schema: "public", Name: evil, Kind: datacat.KindTable, ByName: map[string]*datacat.Column{}}
	for _, c := range []*datacat.Column{
		{Name: "id", TypName: "int8", Category: 'N'},
		{Name: column, TypName: "text", Category: 'S', Nullable: true},
		{Name: price, TypName: "int4", Category: 'N', Nullable: true},
		{Name: "a b", TypName: "text", Category: 'S', Nullable: true},
		{Name: "a_b", TypName: "text", Category: 'S', Nullable: true},
		{Name: "class", TypName: "text", Category: 'S', Nullable: true},
	} {
		t.Columns = append(t.Columns, c)
		t.ByName[c.Name] = c
	}
	list := &datacat.Table{Schema: "public", Name: "list", Kind: datacat.KindTable, ByName: map[string]*datacat.Column{}}
	return &datacat.Catalog{Schemas: []string{"public"}, ByName: map[string]*datacat.Table{"public." + evil: t, "public.list": list},
		Ordered: []*datacat.Table{t, list}, Functions: map[string][]*datacat.Function{}}
}

func TestNamesAreNotCode(t *testing.T) {
	if len(evil) != 37 {
		t.Fatalf("the name is %d bytes", len(evil))
	}
	// Go: the table's name is only a string and a comment; a column
	// encoding/json can't name is refused, with the name quoted.
	if _, err := Generate(hostile(evil, "price$usd"), Go, Options{}); err == nil || !strings.Contains(err.Error(), strconv.Quote(evil)) ||
		!strings.Contains(err.Error(), "can't be a Go JSON field") {
		t.Fatalf("a column Go can't tag: %v", err)
	}
	out, err := Generate(hostile("plain", "price$usd"), Go, Options{Package: "db"})
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", out, parser.ParseComments)
	if err != nil {
		t.Fatalf("doesn't parse: %v\n%s", err, out)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			t.Fatalf("declares func %s:\n%s", fd.Name.Name, out)
		}
	}
	if _, err := (&types.Config{}).Check("db", fset, []*ast.File{f}, nil); err != nil {
		t.Fatalf("doesn't type-check: %v\n%s", err, out)
	}
	for _, want := range []string{"type XIntFuncInitPanicRan struct", "AB *string `json:\"a b\"`", "AB2 *string `json:\"a_b\"`",
		"PriceUsd *int32 `json:\"price$usd\"`", "const XIntFuncInitPanicRanTable = " + strconv.Quote(evil), "type List struct"} {
		if !strings.Contains(out, want) {
			t.Errorf("Go lacks %q:\n%s", want, out)
		}
	}
	if _, err := Generate(hostile("plain", "price$usd"), Go, Options{Package: "db\nfunc"}); err == nil {
		t.Error("a package name that isn't one")
	}

	// TypeScript and Dart: the names only in string literals, and the file
	// has the shape a plain name gives it.
	for _, lang := range []string{TS, Dart} {
		out, err := Generate(hostile(evil, "price$usd'"), lang, Options{})
		if err != nil {
			t.Fatal(err)
		}
		plain, err := Generate(hostile("plain", "price$usd'"), lang, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, evil) || strings.Contains(out, "\tinit") {
			t.Errorf("%s has the name as it is:\n%s", lang, out)
		}
		if n, m := strings.Count(out, "\n"), strings.Count(plain, "\n"); n != m {
			t.Errorf("%s: %d lines with the name, %d without:\n%s", lang, n, m, out)
		}
	}
	out, _ = Generate(hostile(evil, "price$usd'"), Dart, Options{})
	for _, want := range []string{`json['price\$usd\'']`, `'x\tint\n}\nfunc\tinit()\t{\tpanic("ran")\n//'`, "final String? class_;",
		"final String? aB;", "final String? aB2;", "class List2 {", `/// A row of "public.x\tint\n}\nfunc\tinit()\t{\tpanic(\"ran\")\n//".`} {
		if !strings.Contains(out, want) {
			t.Errorf("Dart lacks %q:\n%s", want, out)
		}
	}
	out, _ = Generate(hostile(evil, "price$usd'"), TS, Options{})
	for _, want := range []string{`"x\tint\n}\nfunc\tinit()\t{\tpanic(\"ran\")\n//": {`, `"price$usd'"?: number | null;`,
		"export type XIntFuncInitPanicRan = "} {
		if !strings.Contains(out, want) {
			t.Errorf("TS lacks %q:\n%s", want, out)
		}
	}
}
