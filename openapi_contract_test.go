package profiles_test

import (
	"bytes"
	"encoding/json"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/sdk"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAPIWriteCSRFAndErrorResponses(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Security    []map[string][]string
			Parameters  []struct {
				Name     string `json:"name"`
				In       string `json:"in"`
				Required bool   `json:"required"`
			} `json:"parameters"`
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref        string         `json:"$ref"`
						Required   []string       `json:"required"`
						Properties map[string]any `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(p.OpenAPI(), &doc); err != nil {
		t.Fatal(err)
	}

	writeOpsChecked := 0
	for route, methods := range doc.Paths {
		for method, op := range methods {
			upper := strings.ToUpper(method)
			usesSession := false
			for _, sec := range op.Security {
				if _, ok := sec["sessionCookie"]; ok {
					usesSession = true
				}
			}
			if usesSession && upper != "GET" && upper != "HEAD" && upper != "OPTIONS" {
				hasCSRF, hasIdem := false, false
				for _, param := range op.Parameters {
					if param.In == "header" && param.Required {
						if param.Name == "X-CSRF-Token" {
							hasCSRF = true
						}
						if param.Name == "Idempotency-Key" {
							hasIdem = true
						}
					}
				}
				_, hasConflict := op.Responses["409"]
				readOnlyPreview := upper == "POST" && route == "/api/v1/profiles/me/preview" && op.OperationID == "previewOwnProfile"
				if !hasCSRF || hasIdem == readOnlyPreview || hasConflict == readOnlyPreview {
					t.Fatalf("%s %s: csrf=%v idempotency=%v conflict=%v", upper, route, hasCSRF, hasIdem, hasConflict)
				}
				if !readOnlyPreview {
					writeOpsChecked++
				}
			}
			if upper == "GET" {
				if _, has409 := op.Responses["409"]; has409 {
					t.Fatalf("GET %s (%s) declares unreachable 409 response", route, op.OperationID)
				}
			}
			for status, resp := range op.Responses {
				if status == "400" || status == "401" || status == "403" || status == "404" || status == "429" || status == "503" {
					media, ok := resp.Content["application/json"]
					if !ok || media.Schema.Ref != "#/components/schemas/Error" {
						t.Fatalf("%s %s (%s) status %s missing Error schema ref: got %q", upper, route, op.OperationID, status, media.Schema.Ref)
					}
				}
			}
		}
	}
	if writeOpsChecked != 4 {
		t.Fatalf("expected 4 session-authenticated write operations, checked %d", writeOpsChecked)
	}

	// Verify generated Go SDK builders transmit both Idempotency-Key and X-CSRF-Token headers.
	closureID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	req1, err := sdk.NewRequestClosureRequest("https://api.example", &sdk.RequestClosureParams{
		IdempotencyKey: "idem-req",
		XCSRFToken:     "csrf-req",
	}, sdk.ClosureRequest{AcknowledgePointsForfeiture: true})
	if err != nil {
		t.Fatal(err)
	}
	if req1.Header.Get("Idempotency-Key") != "idem-req" || req1.Header.Get("X-CSRF-Token") != "csrf-req" {
		t.Fatalf("requestClosure headers missing: %v", req1.Header)
	}

	req2, err := sdk.NewCancelClosureRequest("https://api.example", closureID, &sdk.CancelClosureParams{
		IdempotencyKey: "idem-cancel",
		XCSRFToken:     "csrf-cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req2.Header.Get("Idempotency-Key") != "idem-cancel" || req2.Header.Get("X-CSRF-Token") != "csrf-cancel" {
		t.Fatalf("cancelClosure headers missing: %v", req2.Header)
	}

	req3, err := sdk.NewProcessClosureRequest("https://api.example", closureID, &sdk.ProcessClosureParams{
		IdempotencyKey: "idem-proc",
		XCSRFToken:     "csrf-proc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req3.Header.Get("Idempotency-Key") != "idem-proc" || req3.Header.Get("X-CSRF-Token") != "csrf-proc" {
		t.Fatalf("processClosure headers missing: %v", req3.Header)
	}

	// Verify generated response parsers decode JSON Error payloads on 404/429/503.
	jsonResp := func(status int, code string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"code":"` + code + `"}`)),
		}
	}
	pub404, err := sdk.ParseGetPublicProfileResponse(jsonResp(404, "profile_not_found"))
	if err != nil || pub404.JSON404 == nil || pub404.JSON404.Code != "profile_not_found" {
		t.Fatalf("getPublicProfile 404 not parsed: %+v err=%v", pub404, err)
	}
	pub429, err := sdk.ParseGetPublicProfileResponse(jsonResp(429, "rate_limited"))
	if err != nil || pub429.JSON429 == nil || pub429.JSON429.Code != "rate_limited" {
		t.Fatalf("getPublicProfile 429 not parsed: %+v err=%v", pub429, err)
	}
	pub503, err := sdk.ParseGetPublicProfileResponse(jsonResp(503, "unavailable"))
	if err != nil || pub503.JSON503 == nil || pub503.JSON503.Code != "unavailable" {
		t.Fatalf("getPublicProfile 503 not parsed: %+v err=%v", pub503, err)
	}
	own404, err := sdk.ParseGetOwnClosureResponse(jsonResp(404, "closure_not_found"))
	if err != nil || own404.JSON404 == nil || own404.JSON404.Code != "closure_not_found" {
		t.Fatalf("getOwnClosure 404 not parsed: %+v err=%v", own404, err)
	}
}
