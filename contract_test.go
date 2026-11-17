package profiles_test

import (
	"bytes"
	"encoding/json"
	p "github.com/StuHelper/stuhelper-profiles"
	"os"
	"strings"
	"testing"
)

func TestOpenAPIContractConsistencyWithoutDatabase(t *testing.T) {
	embedded := p.OpenAPI()
	disk, err := os.ReadFile("contracts/profiles.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(embedded, disk) {
		t.Fatal("embedded OpenAPI differs from contracts/profiles.openapi.json")
	}

	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(embedded, &doc); err != nil {
		t.Fatalf("invalid OpenAPI JSON: %v", err)
	}
	if doc.OpenAPI != "3.0.3" || doc.Info.Version == "" || len(doc.Paths) == 0 || len(doc.Components.Schemas) == 0 {
		t.Fatalf("incomplete OpenAPI metadata: openapi=%q version=%q", doc.OpenAPI, doc.Info.Version)
	}

	// Verify module.yaml version matches OpenAPI info.version.
	manifest, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var manifestVersion string
	for _, line := range strings.Split(string(manifest), "\n") {
		if strings.HasPrefix(line, "version: ") {
			manifestVersion = strings.TrimSpace(strings.TrimPrefix(line, "version: "))
			break
		}
	}
	if manifestVersion != doc.Info.Version {
		t.Fatalf("module.yaml version %q does not match OpenAPI version %q", manifestVersion, doc.Info.Version)
	}

	// Verify every $ref in the OpenAPI document resolves to an existing component schema.
	var walkRefs func(v any)
	walkRefs = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if ref, ok := node["$ref"].(string); ok {
				const prefix = "#/components/schemas/"
				if !strings.HasPrefix(ref, prefix) {
					t.Fatalf("unexpected $ref prefix: %q", ref)
				}
				name := strings.TrimPrefix(ref, prefix)
				if _, exists := doc.Components.Schemas[name]; !exists {
					t.Fatalf("unresolved schema $ref %q", ref)
				}
			}
			for _, child := range node {
				walkRefs(child)
			}
		case []any:
			for _, child := range node {
				walkRefs(child)
			}
		}
	}
	var generic any
	if err := json.Unmarshal(embedded, &generic); err != nil {
		t.Fatal(err)
	}
	walkRefs(generic)

	// Verify operationId uniqueness and PUT /api/v1/profiles/me Idempotency-Key + X-CSRF-Token headers.
	seenOps := map[string]bool{}
	for pathKey, methods := range doc.Paths {
		for method, rawOp := range methods {
			var op struct {
				OperationID string `json:"operationId"`
				Parameters  []struct {
					Name     string `json:"name"`
					In       string `json:"in"`
					Required bool   `json:"required"`
				} `json:"parameters"`
			}
			if err := json.Unmarshal(rawOp, &op); err != nil {
				t.Fatalf("%s %s: %v", method, pathKey, err)
			}
			if op.OperationID == "" || seenOps[op.OperationID] {
				t.Fatalf("%s %s: missing or duplicate operationId %q", method, pathKey, op.OperationID)
			}
			seenOps[op.OperationID] = true
			if pathKey == "/api/v1/profiles/me" && method == "put" {
				hasIdem, hasCSRF := false, false
				for _, param := range op.Parameters {
					if param.In == "header" && param.Required && param.Name == "Idempotency-Key" {
						hasIdem = true
					}
					if param.In == "header" && param.Required && param.Name == "X-CSRF-Token" {
						hasCSRF = true
					}
				}
				if !hasIdem || !hasCSRF {
					t.Fatalf("PUT /api/v1/profiles/me missing required headers: idempotency=%v csrf=%v", hasIdem, hasCSRF)
				}
			}
		}
	}
}
