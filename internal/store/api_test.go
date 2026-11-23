package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// allowedAPI is the whole exported surface of package store. Append is the
// only identifier in it that writes. A new exported function, method, variable
// or interface method fails this test: if it does not write, add it here; if
// it writes, make it part of Append instead.
var allowedAPI = []string{
	// types
	"Change", "Cursor", "Event", "Filter", "Record", "SQLite", "Store",
	// functions
	"Open",
	// variables
	"ErrConflict", "ErrNotFound",
	// methods of SQLite and Store
	"SQLite.Append", "SQLite.Close", "SQLite.Get", "SQLite.List", "SQLite.Read",
	"Store.Append", "Store.Close", "Store.Get", "Store.List", "Store.Read",
}

func TestAppendIsTheOnlyWritePath(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var exported []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				exported = append(exported, exportedNames(decl)...)
			}
		}
	}
	for _, name := range exported {
		if !slices.Contains(allowedAPI, name) {
			t.Errorf("package store exports %s, which is not in allowedAPI", name)
		}
	}
}

func exportedNames(decl ast.Decl) []string {
	var names []string
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if !d.Name.IsExported() {
			return nil
		}
		if d.Recv == nil {
			return []string{d.Name.Name}
		}
		recv := d.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		return []string{recv.(*ast.Ident).Name + "." + d.Name.Name}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.IsExported() {
						names = append(names, n.Name)
					}
				}
			case *ast.TypeSpec:
				if !s.Name.IsExported() {
					continue
				}
				names = append(names, s.Name.Name)
				if iface, ok := s.Type.(*ast.InterfaceType); ok {
					for _, m := range iface.Methods.List {
						for _, n := range m.Names {
							names = append(names, s.Name.Name+"."+n.Name)
						}
					}
				}
			}
		}
	}
	return names
}
