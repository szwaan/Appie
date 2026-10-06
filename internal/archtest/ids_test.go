package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// idMakers are the packages that may turn raw numbers into ids. fill defines
// the ids, and ah and ahfake read AH's numbers.
var idMakers = map[string]bool{"internal/fill": true, "internal/ah": true, "internal/ahfake": true}

func TestOnlyAHAdaptersMakeIDs(t *testing.T) {
	eachGoFile(t, func(path, rel string) error {
		if idMakers[rel] {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		fill := ""
		for _, spec := range f.Imports {
			if p, err := strconv.Unquote(spec.Path.Value); err == nil && p == module+"/internal/fill" {
				fill = "fill"
				if spec.Name != nil {
					fill = spec.Name.Name
				}
			}
		}
		if fill == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == fill && (sel.Sel.Name == "NewOrderID" || sel.Sel.Name == "NewProductID") {
				t.Errorf("%s calls fill.%s; only internal/ah and internal/ahfake make ids from AH's numbers", path, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
}
