// Package archtest fails the build when a package imports across Appie's
// boundaries.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/szwaan/appie"

// allowed lists, per package directory, the non-standard imports its files
// may use. A package missing here fails the test, so a new package has to
// state its place. Test files may also import their own package and ahfake.
var allowed = map[string][]string{
	"internal/fill":     {},
	"internal/ah":       {"github.com/gwillem/appie-go", module + "/internal/fill"},
	"internal/ahfake":   {module + "/internal/fill"},
	"internal/tui":      {"charm.land/", module + "/internal/fill"},
	"cmd/appie-fill":    {module + "/internal/ah", module + "/internal/fill", module + "/internal/tui"},
	"internal/archtest": {},
}

func TestImportsStayInsideTheirBoundaries(t *testing.T) {
	eachGoFile(t, func(path, rel string) error {
		rules, ok := allowed[rel]
		if !ok {
			t.Errorf("%s: package %q has no entry in archtest's allowed imports", path, rel)
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			rules = append(rules[:len(rules):len(rules)], module+"/"+rel, module+"/internal/ahfake")
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if !stdlib(imp) && !permitted(imp, rules) {
				t.Errorf("%s imports %s; %s may import only %v", path, imp, rel, rules)
			}
		}
		return nil
	})
}

// eachGoFile calls f with every Go file in the module and the file's package
// directory relative to the module root.
func eachGoFile(t *testing.T, f func(path, rel string) error) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		return f(path, filepath.ToSlash(rel))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func stdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// permitted matches whole path elements: "charm.land/" covers everything
// under charm.land, "github.com/gwillem/appie-go" covers that module and its
// packages but not "github.com/gwillem/appie-gox".
func permitted(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
