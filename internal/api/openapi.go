package api

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/version"
)

// The OpenAPI document is generated from the route table in server.go, not
// written by hand.
//
// A hand-maintained spec drifts the first time someone adds an endpoint in a
// hurry, and from then on it is worse than no spec: it tells you an endpoint
// exists that does not, or omits the one you need. Generating from the same
// slice the mux is built from makes that impossible -- if it is served, it is
// documented, and if it is documented, it is served.

var pathParamRE = regexp.MustCompile(`\{([^}]+)\}`)

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, s.OpenAPI())
	return nil
}

// OpenAPI builds the 3.1 document.
func (s *Server) OpenAPI() map[string]any {
	paths := map[string]any{}

	// Group routes by path so several methods share one path item.
	byPath := map[string][]route{}
	for _, rt := range s.routes {
		byPath[rt.Pattern] = append(byPath[rt.Pattern], rt)
	}

	for pattern, routes := range byPath {
		item := map[string]any{}

		params := []any{}
		for _, m := range pathParamRE.FindAllStringSubmatch(pattern, -1) {
			params = append(params, map[string]any{
				"name":     m[1],
				"in":       "path",
				"required": true,
				"schema":   map[string]any{"type": "string"},
			})
		}
		if len(params) > 0 {
			item["parameters"] = params
		}

		for _, rt := range routes {
			op := map[string]any{
				"summary":     rt.Summary,
				"operationId": operationID(rt),
				"tags":        []string{rt.Tag},
				"responses":   responsesFor(rt),
			}
			if rt.Auth == authNone {
				// An empty security array means "no auth required" and
				// overrides the document-level default.
				op["security"] = []any{}
			}
			if rt.Method == http.MethodPost || rt.Method == http.MethodPatch || rt.Method == http.MethodPut {
				op["requestBody"] = map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"type": "object"},
						},
					},
				}
			}
			if q := queryParamsFor(rt); len(q) > 0 {
				existing, _ := item["parameters"].([]any)
				item["parameters"] = append(existing, q...)
			}
			item[strings.ToLower(rt.Method)] = op
		}
		paths[pattern] = item
	}

	tags := []any{}
	seen := map[string]bool{}
	names := []string{}
	for _, rt := range s.routes {
		if !seen[rt.Tag] {
			seen[rt.Tag] = true
			names = append(names, rt.Tag)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		tags = append(tags, map[string]any{"name": n})
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "DevKuong Memories",
			"version":     version.Short(),
			"description": "Self-hosted shared memory for AI coding agents. Generated from the server's route table, so it cannot drift from the implementation.",
			"license":     map[string]any{"name": "Apache-2.0", "identifier": "Apache-2.0"},
		},
		"servers": []any{
			map[string]any{"url": strings.TrimRight(s.cfg.Server.PublicURL, "/")},
		},
		"tags": tags,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type":        "http",
					"scheme":      "bearer",
					"description": "A key issued by `dkm admin key issue`, in the form pmk_<prefix>_<secret>.",
				},
			},
			"schemas": map[string]any{
				"Error": map[string]any{
					"type": "object",
					"description": "Every error response has this shape, including 404 and 500. " +
						"A response with no body means the request never reached this server.",
					"required": []string{"error", "message"},
					"properties": map[string]any{
						"error":      map[string]any{"type": "string", "description": "stable machine-readable code"},
						"message":    map[string]any{"type": "string"},
						"request_id": map[string]any{"type": "string"},
						"hint":       map[string]any{"type": "string"},
					},
				},
			},
		},
		"security": []any{map[string]any{"bearerAuth": []any{}}},
		"paths":    paths,
	}
}

func operationID(rt route) string {
	id := strings.ToLower(rt.Method) + pathParamRE.ReplaceAllString(rt.Pattern, "By_$1")
	id = strings.ReplaceAll(id, "/v1/", "_")
	id = strings.ReplaceAll(id, "/", "_")
	return strings.Trim(id, "_")
}

func responsesFor(rt route) map[string]any {
	errRef := map[string]any{
		"description": "error",
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]any{"$ref": "#/components/schemas/Error"},
			},
		},
	}

	ok := "200"
	if rt.Method == http.MethodPost && !strings.HasSuffix(rt.Pattern, "/reinforce") &&
		!strings.HasSuffix(rt.Pattern, "/supersede") && !strings.Contains(rt.Pattern, "/search") &&
		!strings.Contains(rt.Pattern, "/context") && !strings.Contains(rt.Pattern, "/import") &&
		!strings.Contains(rt.Pattern, "/rebuild") && !strings.Contains(rt.Pattern, "/share") {
		ok = "201"
	}

	out := map[string]any{
		ok: map[string]any{
			"description": "success",
			"content": map[string]any{
				"application/json": map[string]any{"schema": map[string]any{"type": "object"}},
			},
		},
		"400": errRef,
		"500": errRef,
	}
	if rt.Auth != authNone {
		out["401"] = errRef
		out["404"] = errRef
		out["429"] = errRef
	}
	if rt.Auth == authAdmin {
		out["403"] = errRef
	}
	return out
}

// queryParamsFor documents the query parameters each listing route accepts.
func queryParamsFor(rt route) []any {
	str := func(name, desc string) any {
		return map[string]any{"name": name, "in": "query", "schema": map[string]any{"type": "string"}, "description": desc}
	}
	num := func(name, desc string) any {
		return map[string]any{"name": name, "in": "query", "schema": map[string]any{"type": "integer"}, "description": desc}
	}

	switch rt.Pattern {
	case "/v1/memories":
		if rt.Method == http.MethodGet {
			return []any{
				str("project", "project identity, e.g. github.com/org/repo"),
				str("kind", "comma-separated: fact, decision, lesson, preference"),
				str("cursor", "next_cursor from the previous page"),
				num("limit", "page size, default 50, max 500"),
				str("mine", "true to exclude teammates' shared memories"),
				str("include_superseded", "true to include replaced memories"),
			}
		}
	case "/v1/sessions", "/v1/lessons", "/v1/feed":
		return []any{str("project", "project identity"), num("limit", "maximum rows")}
	case "/v1/sync":
		return []any{str("since", "cursor from the previous response"), num("limit", "maximum changes")}
	case "/v1/export":
		return []any{str("scope", "me or team"), str("project", "restrict to one project")}
	case "/v1/graph":
		return []any{
			str("project", "project identity"),
			str("node", "seed label; omit for the whole project graph"),
			num("depth", "traversal depth from the seed, 1-6"),
			num("limit", "maximum nodes"),
		}
	case "/v1/graph/rebuild":
		return []any{str("project", "project identity; required")}
	case "/v1/admin/audit":
		return []any{
			str("user", "filter by user id"),
			str("action", "filter by action, e.g. memory.delete"),
			str("since", "RFC3339 timestamp"),
			num("limit", "maximum entries"),
		}
	}
	return nil
}
