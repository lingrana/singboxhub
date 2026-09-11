package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIReferencesAndRegisteredRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if document["openapi"] != "3.2.0" {
		t.Fatal("expected OpenAPI 3.2.0")
	}
	var checkRefs func(any)
	checkRefs = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if ref, ok := value["$ref"].(string); ok {
				if !strings.HasPrefix(ref, "#/") {
					t.Errorf("non-local reference: %s", ref)
					return
				}
				var target any = document
				for _, segment := range strings.Split(ref[2:], "/") {
					segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
					object, ok := target.(map[string]any)
					if !ok {
						target = nil
						break
					}
					target = object[segment]
				}
				if target == nil {
					t.Errorf("unresolved reference: %s", ref)
				}
			}
			for _, child := range value {
				checkRefs(child)
			}
		case []any:
			for _, child := range value {
				checkRefs(child)
			}
		}
	}
	checkRefs(document)
	paths := document["paths"].(map[string]any)
	operations := map[string]string{}
	for path, entry := range paths {
		for method, item := range entry.(map[string]any) {
			if !strings.Contains(" get post put patch delete head options ", " "+method+" ") {
				continue
			}
			op := item.(map[string]any)
			id, _ := op["operationId"].(string)
			if id == "" {
				t.Errorf("missing operationId: %s %s", method, path)
			}
			if old, exists := operations[id]; exists {
				t.Errorf("duplicate operationId %s: %s and %s", id, old, path)
			}
			operations[id] = path
			if responses, ok := op["responses"].(map[string]any); !ok || len(responses) == 0 {
				t.Errorf("missing responses: %s %s", method, path)
			}
		}
	}
	source, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(source, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		pattern, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		method, path, found := strings.Cut(pattern, " ")
		if !found {
			t.Errorf("unrecognized route: %s", pattern)
			return true
		}
		entry, ok := paths[path].(map[string]any)
		if !ok || entry[strings.ToLower(method)] == nil {
			t.Errorf("route missing from contract: %s", pattern)
		}
		return true
	})
}
