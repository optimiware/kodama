package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// parsePackage parses the adapter's non-test source files.
func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, p := range pkgs {
		for _, f := range p.Files {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("no adapter source found")
	}
	return files
}

// statementsAreTenantScoped is the infrastructure-free half of the scope proof:
// every statement the adapter can send names the tenant, and nothing sends SQL
// that is not one of those statements.
func statementsAreTenantScoped(t *testing.T) {
	files := parsePackage(t)

	// Every stmt constant is listed, so none escapes the checks below.
	declared := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				for _, n := range spec.(*ast.ValueSpec).Names {
					if strings.HasPrefix(n.Name, "stmt") {
						declared[n.Name] = true
					}
				}
			}
		}
	}
	listed := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "allStatements" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "stmt") {
					listed[id.Name] = true
				}
				return true
			})
		}
	}
	for name := range declared {
		if !listed[name] {
			t.Errorf("%s is not in allStatements, so it escapes the scope checks", name)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("allStatements lists %d statements, %d are declared", len(listed), len(declared))
	}

	// The only SQL sent from a transaction is a stmt constant.
	sends := map[string]bool{"Exec": true, "Query": true, "QueryRow": true}
	calls := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !sends[sel.Sel.Name] {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "tx" || len(call.Args) < 2 {
				return true
			}
			calls++
			switch sql := call.Args[1].(type) {
			case *ast.Ident:
				if !declared[sql.Name] && sql.Name != "q" {
					t.Errorf("tx.%s sends %s, which is not a declared statement", sel.Sel.Name, sql.Name)
				}
			default:
				t.Errorf("tx.%s at %v sends SQL that is not a named statement", sel.Sel.Name, call.Pos())
			}
			return true
		})
	}
	if calls < 20 {
		t.Errorf("found only %d statements sent from a transaction; the scan is not looking at the adapter", calls)
	}

	tenantRead := regexp.MustCompile(`(?s)\bWHERE\b.*\btenant_id = \$1\b`)
	tenantInsert := regexp.MustCompile(`(?s)^INSERT INTO \w+ \(tenant_id,.*\bVALUES \(\$1,`)
	// Statements with a reason not to carry a tenant predicate of their own.
	exempt := map[string]string{
		stmtLockSignalIdentity: "an advisory lock keyed by a digest of the tenant, project, caller and key",
	}
	for _, sql := range allStatements() {
		flat := strings.Join(strings.Fields(sql), " ")
		if _, ok := exempt[sql]; ok {
			continue
		}
		switch {
		case strings.HasPrefix(flat, "INSERT"):
			if !tenantInsert.MatchString(flat) {
				t.Errorf("an insert does not write the tenant from $1: %.80s", flat)
			}
		case !tenantRead.MatchString(flat):
			t.Errorf("a statement has no tenant predicate: %.80s", flat)
		}
		if strings.HasPrefix(flat, "SELECT") || strings.HasPrefix(flat, "UPDATE") {
			if !strings.Contains(flat, "project_id = $2") && !strings.Contains(flat, "tenant_id = $1 AND NOT") && !strings.Contains(flat, "tenant_id = $1 AND id = $2") {
				t.Errorf("a statement is not project scoped: %.80s", flat)
			}
		}
		for _, banned := range []string{"DELETE", "TRUNCATE", "DROP ", "ALTER ", "GRANT ", "SET ROLE", "row_security"} {
			if strings.Contains(strings.ToUpper(flat), banned) {
				t.Errorf("the runtime adapter sends %q: %.80s", banned, flat)
			}
		}
	}
	// The lock statements are the only ones that lock rows.
	locking := 0
	for _, sql := range allStatements() {
		if strings.Contains(sql, "FOR UPDATE") {
			locking++
		}
	}
	if locking != 1 || !strings.Contains(stmtLockRun, "FOR UPDATE") {
		t.Errorf("only stmtLockRun locks rows, found %d locking statements", locking)
	}
	if !slices.Contains(allStatements(), stmtLockRun) {
		t.Error("stmtLockRun is not listed")
	}
}
