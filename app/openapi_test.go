package app

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/pillar/gostack/adapters/pgtest"
)

// The OpenAPI document (docs/openapi/gostack.yaml) must describe exactly the
// routes the code registers: every documented operation reaches a real route,
// every route pattern in the Go source is documented, and every error code
// literal in the source is listed in the ErrorCode enum.

const openAPIPath = "../docs/openapi/gostack.yaml"

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch"}

type apiOperation struct {
	Method, Path, Module string
}

func loadOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", openAPIPath, err)
	}
	return doc
}

func apiOperations(t *testing.T, doc map[string]any) []apiOperation {
	t.Helper()
	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("document has no paths")
	}
	var ops []apiOperation
	for p, item := range paths {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("path %s is not an object", p)
		}
		for _, method := range httpMethods {
			op, ok := m[method].(map[string]any)
			if !ok {
				continue
			}
			mod, _ := op["x-gostack-module"].(string)
			ops = append(ops, apiOperation{Method: strings.ToUpper(method), Path: p, Module: mod})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Path+ops[i].Method < ops[j].Path+ops[j].Method })
	return ops
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

// The document is structurally sound: unique operation ids, known modules,
// declared path parameters and resolvable local $refs.
func TestOpenAPIDocumentWellFormed(t *testing.T) {
	doc := loadOpenAPI(t)
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", doc["openapi"])
	}
	modules := []string{"system", "core", "ops", "experiments", "social", "match", "commerce", "realtime", "webauth", "billing", "storage-local"}
	ids := map[string]string{}
	paths := doc["paths"].(map[string]any)
	for p, item := range paths {
		pi := item.(map[string]any)
		for _, method := range httpMethods {
			op, ok := pi[method].(map[string]any)
			if !ok {
				continue
			}
			where := strings.ToUpper(method) + " " + p
			id, _ := op["operationId"].(string)
			if id == "" {
				t.Errorf("%s: missing operationId", where)
			} else if prev, dup := ids[id]; dup {
				t.Errorf("%s: operationId %q also used by %s", where, id, prev)
			}
			ids[id] = where
			if mod, _ := op["x-gostack-module"].(string); !slices.Contains(modules, mod) {
				t.Errorf("%s: x-gostack-module %q not one of %v", where, mod, modules)
			}
			if _, ok := op["responses"].(map[string]any); !ok {
				t.Errorf("%s: no responses", where)
			}
			declared := map[string]bool{}
			for _, list := range []any{pi["parameters"], op["parameters"]} {
				ps, _ := list.([]any)
				for _, x := range ps {
					pm, _ := x.(map[string]any)
					if ref, ok := pm["$ref"].(string); ok {
						pm, _ = resolveRef(doc, ref).(map[string]any)
					}
					if pm != nil && pm["in"] == "path" {
						declared[pm["name"].(string)] = true
					}
				}
			}
			for _, m := range pathParam.FindAllStringSubmatch(p, -1) {
				if !declared[m[1]] {
					t.Errorf("%s: path parameter {%s} not declared", where, m[1])
				}
			}
		}
	}
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if k == "$ref" {
					ref, _ := c.(string)
					if !strings.HasPrefix(ref, "#/") || resolveRef(doc, ref) == nil {
						t.Errorf("%s: unresolvable $ref %q", at, ref)
					}
					continue
				}
				walk(c, at+"/"+k)
			}
		case []any:
			for i, c := range x {
				walk(c, at+"/"+strconv.Itoa(i))
			}
		}
	}
	walk(doc, "#")
}

func resolveRef(doc map[string]any, ref string) any {
	var cur any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
		if cur, ok = m[part]; !ok {
			return nil
		}
	}
	return cur
}

// Every documented operation is served by a real route: the app (with every
// module enabled) must not answer with app.Handler's "route not found" fallback.
func TestOpenAPIRoutesExist(t *testing.T) {
	doc := loadOpenAPI(t)
	c := allModulesConfig(t)
	c.Auth.Web.Enabled = true
	a := newTestApp(t, c)
	srv := httptest.NewServer(a.Handler(Hooks{}))
	defer srv.Close()

	ops := apiOperations(t, doc)
	checked := 0
	for _, op := range ops {
		if pgtest.Enabled() && op.Module == "storage-local" {
			continue // rejected by configuration validation under profile scale
		}
		checked++
		path := pathParam.ReplaceAllString(op.Path, "x1")
		var body *strings.Reader
		if op.Method == "GET" || op.Method == "OPTIONS" || op.Method == "DELETE" {
			body = strings.NewReader("")
		} else {
			body = strings.NewReader("{}")
		}
		req, err := http.NewRequest(op.Method, srv.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", op.Method, op.Path, err)
		}
		var env struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&env)
		res.Body.Close()
		if res.StatusCode == http.StatusNotFound && env.Error.Code == "not_found" && env.Error.Message == "route not found" {
			t.Errorf("%s %s (module %s): documented but not routed", op.Method, op.Path, op.Module)
		}
	}
	if len(ops) < 100 {
		t.Fatalf("only %d operations documented; document incomplete or parse error", len(ops))
	}
	t.Logf("%d of %d documented operations checked against the router", checked, len(ops))
}

// sourceRoutes parses the non-test Go files of the framework packages and
// returns every literal route pattern as "METHOD /openapi/path", plus every
// error code literal passed to httpx.NewError / httpx.Conflict.
func sourceRoutes(t *testing.T) (routes map[string]string, codes map[string]string) {
	t.Helper()
	routes, codes = map[string]string{}, map[string]string{}
	routeLit := regexp.MustCompile(`^(?:([A-Z]+) )?(/\S*)$`)
	fset := token.NewFileSet()
	for _, dir := range []string{"../kits", "../modules", "../core", "."} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				default:
					return true
				}
				lit := func(i int) (string, bool) {
					if i >= len(call.Args) {
						return "", false
					}
					bl, ok := call.Args[i].(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						return "", false
					}
					s, err := strconv.Unquote(bl.Value)
					return s, err == nil
				}
				at := fset.Position(call.Pos()).String()
				switch name {
				case "NewError":
					if s, ok := lit(1); ok {
						codes[s] = at
					}
					return true
				case "Conflict":
					if s, ok := lit(0); ok {
						codes[s] = at
					}
					return true
				case "Handle", "HandleFunc", "HandleAdmin", "g", "h", "mutate":
				default:
					return true
				}
				prefix := ""
				s, ok := lit(0)
				if ok {
					// mux.Handle/HandleFunc take absolute patterns; the g/h/mutate
					// helpers take either absolute /v1 paths or game-relative ones.
					if name != "Handle" && name != "HandleFunc" && !strings.Contains(s, " /v1/") {
						prefix = "/v1/games/{game}"
					}
				} else if s, ok = lit(1); ok { // Kit.Handle(mux, p, ...) / Kit.HandleAdmin(mux, p, ...)
					prefix = "/v1/games/{game}"
					if name == "HandleAdmin" {
						prefix = "/v1/admin/games/{game}"
					}
				} else {
					return true
				}
				m := routeLit.FindStringSubmatch(s)
				if m == nil {
					return true
				}
				routes[normalizeRoute(m[1], prefix+m[2])] = at
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return routes, codes
}

// normalizeRoute turns a ServeMux pattern into "METHOD /openapi/path":
// {name...} becomes {name}; a subtree pattern ("/x/") gets a {path} segment.
func normalizeRoute(method, path string) string {
	path = strings.ReplaceAll(path, "...}", "}")
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path += "{path}"
	}
	return method + " " + path
}

// Every route registered by the framework source is documented, so a new
// route without OpenAPI documentation fails this test.
func TestOpenAPICoversRegisteredRoutes(t *testing.T) {
	doc := loadOpenAPI(t)
	documented := map[string]bool{}
	for _, op := range apiOperations(t, doc) {
		documented[op.Method+" "+op.Path] = true
	}
	routes, _ := sourceRoutes(t)
	// Not on the public handler: served by the internal listener (http.internal_listen).
	internal := []string{"GET /metrics", " /debug/pprof/{path}", " /debug/pprof/profile", " /debug/pprof/trace"}
	// The catch-all fallback in app.Handler.
	internal = append(internal, " /")
	for _, r := range internal {
		if _, ok := routes[r]; !ok {
			t.Errorf("expected source route %q not found; update this test", r)
		}
		delete(routes, r)
	}
	// The local storage presign handler is mounted at a configured path
	// (storage.local.public_url, documented as /v1/files) for GET and PUT.
	routes["GET /v1/files/{key}"] = "app/modules.go (LocalStorage.Handler)"
	routes["PUT /v1/files/{key}"] = "app/modules.go (LocalStorage.Handler)"
	if len(routes) < 100 {
		t.Fatalf("only %d routes found in source; parser heuristics broken", len(routes))
	}
	for r, at := range routes {
		if strings.HasPrefix(r, " ") {
			t.Errorf("%s: route %q has no method; document it or extend this test", at, r)
			continue
		}
		if !documented[r] {
			t.Errorf("%s: route %q is not documented in %s", at, r, openAPIPath)
		}
	}
	for r := range documented {
		if _, ok := routes[r]; !ok {
			t.Errorf("documented operation %q has no literal route pattern in the source", r)
		}
	}
}

// The ErrorCode enum lists exactly the error codes the source can produce.
func TestOpenAPIErrorCodes(t *testing.T) {
	doc := loadOpenAPI(t)
	enum, _ := resolveRef(doc, "#/components/schemas/ErrorCode").(map[string]any)["enum"].([]any)
	listed := map[string]bool{}
	for _, v := range enum {
		listed[v.(string)] = true
	}
	_, codes := sourceRoutes(t)
	if len(codes) < 50 {
		t.Fatalf("only %d error codes found in source; parser heuristics broken", len(codes))
	}
	for c, at := range codes {
		if !listed[c] {
			t.Errorf("%s: error code %q missing from components.schemas.ErrorCode", at, c)
		}
	}
	for c := range listed {
		if _, ok := codes[c]; !ok {
			t.Errorf("ErrorCode %q is listed but never produced by the source", c)
		}
	}
}
