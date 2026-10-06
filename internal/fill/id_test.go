package fill

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"
)

func TestIDsDoNotConvertIntoEachOther(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			f, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
	}
	imports := importer.ForCompiler(fset, "gc", nil)
	check := func(decl string) error {
		f, err := parser.ParseFile(fset, "conversion.go", "package fill\n"+decl, 0)
		if err != nil {
			t.Fatal(err)
		}
		conf := types.Config{Importer: imports}
		_, err = conf.Check("github.com/szwaan/appie/internal/fill", fset, append(files[:len(files):len(files)], f), nil)
		return err
	}

	if err := check("var _ = OrderID(OrderID{})"); err != nil {
		t.Fatalf("package fill with a valid conversion does not type-check: %v", err)
	}
	for _, decl := range []string{
		"var _ = OrderID(ProductID{})",
		"var _ = ProductID(OrderID{})",
		"var _ = OrderID(7)",
		"var _ = ProductID(1525)",
	} {
		if err := check(decl); err == nil || !strings.Contains(err.Error(), "cannot convert") {
			t.Errorf("%s: type-check error %v, want cannot convert", decl, err)
		}
	}
}
