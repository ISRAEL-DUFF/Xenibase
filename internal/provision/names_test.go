package provision

import (
	"errors"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"blog":                         "blog",
		"My Blog!":                     "my_blog",
		"  todo -- app  ":              "todo_app",
		"2048 game":                    "p_2048_game",
		"Café Olé":                     "caf_ol",
		strings.Repeat("a", 80):        strings.Repeat("a", 40),
		"x" + strings.Repeat("_y", 30): "x_y_y_y_y_y_y_y_y_y_y_y_y_y_y_y_y_y_y_y",
	} {
		got, err := Slugify(in)
		if err != nil || got != want {
			t.Errorf("Slugify(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "!!!", "日本"} {
		if _, err := Slugify(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("Slugify(%q) should fail", in)
		}
	}
}

func TestValidateName(t *testing.T) {
	if got, err := ValidateName("  Blog  "); err != nil || got != "Blog" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, in := range []string{"", "   ", strings.Repeat("x", 65), "a\nb"} {
		if _, err := ValidateName(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateName(%q) should fail", in)
		}
	}
}

func TestOpaqueNames(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		db, owner, err := Names()
		if err != nil {
			t.Fatal(err)
		}
		if !IsOpaque(db) || owner != db+"_owner" {
			t.Fatalf("Names() = %q, %q", db, owner)
		}
		if seen[db] {
			t.Fatalf("duplicate %q", db)
		}
		seen[db] = true
	}
	for _, name := range []string{"blog_k2f9", "p_abc", "p_ABCDEFGHIJ", "p_abcdefghij1", "p_abcdefgh01"} {
		if IsOpaque(name) {
			t.Errorf("IsOpaque(%q) = true", name)
		}
	}
}

func TestOpaqueRoles(t *testing.T) {
	for _, s := range []string{"_owner", "_ro", "_console", "_console_ro", "_u_ab12cd", "_anon", "_user", "_service", "_edge", "_auth_hook"} {
		if !IsOpaqueRole("p_abcdefghij" + s) {
			t.Errorf("IsOpaqueRole(%q) = false", "p_abcdefghij"+s)
		}
	}
	for _, r := range []string{"p_abcdefghij", "p_abcdefghij_admin", "blog_anon", "p_abcdefghij_anonx"} {
		if IsOpaqueRole(r) {
			t.Errorf("IsOpaqueRole(%q) = true", r)
		}
	}
}
