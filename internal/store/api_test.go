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
// field or interface method fails this test: if it does not write, add it here; if
// it writes, make it part of Append instead.
var allowedAPI = []string{
	// types
	"Change", "Cursor", "Event", "Filter", "Record", "SQLite", "Store",
	// functions
	"Open",
	// variables
	"ErrConflict", "ErrNotFound", "ErrInvalidCursor",
	// fields: plain data, no handles
	"Change.Kind", "Change.ID", "Change.Data", "Change.IfVersion",
	"Record.Kind", "Record.ID", "Record.Data", "Record.Version",
	"Filter.Project", "Filter.Ticket", "Filter.TypePrefix",
	"Event.Cursor", "Event.Type", "Event.At", "Event.Actor", "Event.Project",
	"Event.Ticket", "Event.Task", "Event.Run", "Event.Payload",
	// methods of SQLite and Store
	"SQLite.Append", "SQLite.Close", "SQLite.Get", "SQLite.List", "SQLite.Read",
	"Store.Append", "Store.Close", "Store.Get", "Store.List", "Store.Read",
}

func TestAppendIsTheOnlyWritePath(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var exported []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			exported = append(exported, exportedNames(decl)...)
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
		return []string{receiverName(d.Recv.List[0].Type) + "." + d.Name.Name}
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
				var members []*ast.Field
				switch typ := s.Type.(type) {
				case *ast.InterfaceType:
					members = typ.Methods.List
				case *ast.StructType:
					members = typ.Fields.List
				}
				for _, m := range members {
					for _, n := range m.Names {
						if n.IsExported() {
							names = append(names, s.Name.Name+"."+n.Name)
						}
					}
					if len(m.Names) == 0 { // embedded: its exported members are promoted
						names = append(names, s.Name.Name+"."+receiverName(m.Type))
					}
				}
			}
		}
	}
	return names
}

// receiverName returns the type name of a receiver or embedded field,
// without pointers and type parameters.
func receiverName(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.SelectorExpr:
			return e.Sel.Name
		case *ast.Ident:
			return e.Name
		default:
			return "?"
		}
	}
}
