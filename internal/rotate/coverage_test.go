package rotate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryEncryptIsRotated reads every keyring Encrypt call in the module
// and checks that rotate re-encrypts what it seals: each call's associated
// data (a string, a constant, or a function returning one) must start with
// a label in columns, settingBlobs, opAADPrefix or clearedAADPrefix. A new
// sealed value fails here until collect learns about it.
func TestEveryEncryptIsRotated(t *testing.T) {
	root := moduleRoot(t)
	labels := map[string][]string{} // label -> call sites
	for _, dir := range []string{"internal", "cmd"} {
		pkgs := map[string][]*ast.File{}
		fset := token.NewFileSet()
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			pkgs[filepath.Dir(path)] = append(pkgs[filepath.Dir(path)], f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for pkg, files := range pkgs {
			if pkg == filepath.Join(root, "internal", "crypto") { // the keyring itself (Reencrypt)
				continue
			}
			for label, sites := range encryptLabels(t, fset, root, files) {
				labels[label] = append(labels[label], sites...)
			}
		}
	}
	if len(labels) < 15 {
		t.Fatalf("found only %d labels; is the scan still finding the Encrypt calls?", len(labels))
	}
	var missing []string
	for label, sites := range labels {
		if !covered(label) {
			missing = append(missing, label+" ("+strings.Join(sites, ", ")+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("sealed with the master key but not rotated: %s", m)
	}
}

func covered(label string) bool {
	if strings.HasPrefix(label, clearedAADPrefix) || label == opAADPrefix {
		return true
	}
	for _, c := range columns {
		if label == c.aad {
			return true
		}
	}
	for _, s := range settingBlobs {
		if label == s.aad {
			return true
		}
	}
	return false
}

// encryptLabels returns the leading string literal of the associated data
// of every x.Encrypt(plaintext, aad) call in one package.
func encryptLabels(t *testing.T, fset *token.FileSet, root string, files []*ast.File) map[string][]string {
	consts := map[string]string{}
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, sp := range d.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								consts[n.Name], _ = strconv.Unquote(lit.Value)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil {
					funcs[d.Name.Name] = d
				}
			}
		}
	}
	var resolve func(e ast.Expr) (string, bool)
	resolve = func(e ast.Expr) (string, bool) {
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				s, err := strconv.Unquote(e.Value)
				return s, err == nil
			}
		case *ast.Ident:
			s, ok := consts[e.Name]
			return s, ok
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				return resolve(e.X)
			}
		case *ast.ParenExpr:
			return resolve(e.X)
		case *ast.CallExpr:
			if _, ok := e.Fun.(*ast.ArrayType); ok && len(e.Args) == 1 { // []byte(...)
				return resolve(e.Args[0])
			}
			if id, ok := e.Fun.(*ast.Ident); ok && funcs[id.Name] != nil {
				body := funcs[id.Name].Body
				if len(body.List) == 1 {
					if r, ok := body.List[0].(*ast.ReturnStmt); ok && len(r.Results) == 1 {
						return resolve(r.Results[0])
					}
				}
			}
		}
		return "", false
	}
	out := map[string][]string{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Encrypt" || len(call.Args) != 2 {
				return true
			}
			// OpenPGP streams for backup objects, not the master key.
			if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "pgpstream" || id.Name == "openpgp") {
				return true
			}
			pos := fset.Position(call.Pos())
			rel, _ := filepath.Rel(root, pos.Filename)
			site := rel + ":" + strconv.Itoa(pos.Line)
			label, ok := resolve(call.Args[1])
			if !ok || label == "" {
				t.Errorf("%s: cannot read the associated data of this Encrypt call; seal with a string, a constant or a one-line function returning one", site)
				return true
			}
			out[label] = append(out[label], site)
			return true
		})
	}
	return out
}

func moduleRoot(t *testing.T) string {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
