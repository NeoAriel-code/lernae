package config

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProductionEnvironmentAccessStaysInConfigPackage(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	violations, err := findProductionEnvironmentAccessViolations(root)
	if err != nil {
		t.Fatalf("scan production source for environment access: %v", err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}

func TestEnvironmentAccessGuardDetectsAliasesAndUsesExactConfigAllowlist(t *testing.T) {
	root := t.TempDir()
	writeSource := func(relativePath, source string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSource("cmd/agent/main.go", `package main
import runtimeos "os"
func f() { _ = runtimeos.Getenv("EXAMPLE"); getenv := runtimeos.LookupEnv; _, _ = getenv("EXAMPLE") }
`)
	writeSource("internal/config/config.go", `package config
import "os"
func f() { _ = os.Getenv("ALLOWED") }
`)
	writeSource("internal/config/subpackage/access.go", `package subpackage
import sys "os"
func f() { _ = sys.LookupEnv("EXAMPLE") }
`)
	writeSource("internal/config-extra/access.go", `package configextra
import . "os"
func f() { _ = Environ() }
`)

	violations, err := findProductionEnvironmentAccessViolations(root)
	if err != nil {
		t.Fatalf("find environment-access violations: %v", err)
	}
	if len(violations) != 4 {
		t.Fatalf("environment-access violations = %#v, want direct call, function reference, nested-package, and sibling-package findings", violations)
	}
}

func findProductionEnvironmentAccessViolations(root string) ([]string, error) {
	var violations []string
	for _, sourceRoot := range []string{"cmd", "internal"} {
		base := filepath.Join(root, sourceRoot)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			if sourceRoot == "internal" && filepath.Dir(relative) == "config" {
				return nil
			}
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, path, nil, 0)
			if err != nil {
				return err
			}
			osAliases := make(map[string]struct{})
			dotImport := false
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil || importPath != "os" {
					continue
				}
				alias := "os"
				if spec.Name != nil {
					alias = spec.Name.Name
				}
				switch alias {
				case ".":
					dotImport = true
				case "_":
					// A blank import cannot expose package functions.
				default:
					osAliases[alias] = struct{}{}
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				var expression ast.Expr
				switch access := node.(type) {
				case *ast.SelectorExpr:
					expression = access
				case *ast.Ident:
					if dotImport {
						expression = access
					}
				}
				if expression == nil {
					return true
				}
				method, usesOS := environmentCallName(expression, osAliases, dotImport)
				if !usesOS {
					return true
				}
				switch method {
				case "Getenv", "LookupEnv", "Environ":
					position := fileSet.Position(node.Pos())
					violations = append(violations, fmt.Sprintf("%s:%d: production environment access outside internal/config (%s)", path, position.Line, method))
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", sourceRoot, err)
		}
	}
	return violations, nil
}

func environmentCallName(expression ast.Expr, aliases map[string]struct{}, dotImport bool) (string, bool) {
	if selector, ok := expression.(*ast.SelectorExpr); ok {
		pkg, ok := selector.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		_, imported := aliases[pkg.Name]
		return selector.Sel.Name, imported
	}
	if identifier, ok := expression.(*ast.Ident); ok && dotImport {
		return identifier.Name, true
	}
	return "", false
}
