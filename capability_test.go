package profiles_test

import (
	"encoding/json"
	p "github.com/StuHelper/stuhelper-profiles"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Keep the module's exported capability names valid for platform registration.
func TestGroupCardCapabilityRegistration(t *testing.T) {
	manifest, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatal(err)
	}
	grammar := regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z][a-z0-9.-]*$`)
	inCapabilities, found := false, false
	for _, line := range strings.Split(string(manifest), "\n") {
		if line == "capabilities:" {
			inCapabilities = true
			continue
		}
		if !inCapabilities {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		if !strings.HasPrefix(line, "  - ") {
			continue
		}
		name := strings.TrimPrefix(line, "  - ")
		if !grammar.MatchString(name) {
			t.Fatalf("platform rejects capability %q", name)
		}
		if name == "profiles.group-card.read" {
			found = true
		}
	}
	if !found {
		t.Fatal("group-card read capability is not registered")
	}
}

func TestClosureOptionalDependenciesDeclaredInManifest(t *testing.T) {
	manifest, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatal(err)
	}
	optional := map[string]bool{}
	inOptional := false
	for _, line := range strings.Split(string(manifest), "\n") {
		if line == "optional:" {
			inOptional = true
			continue
		}
		if !inOptional {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		if strings.HasPrefix(line, "  - id: ") {
			optional[strings.TrimSpace(strings.TrimPrefix(line, "  - id: "))] = true
		}
	}

	var doc struct {
		Components struct {
			Schemas struct {
				Closure struct {
					Properties struct {
						SkippedModules struct {
							Items struct {
								Enum []string `json:"enum"`
							} `json:"items"`
						} `json:"skipped_modules"`
					} `json:"properties"`
				} `json:"Closure"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(p.OpenAPI(), &doc); err != nil {
		t.Fatal(err)
	}
	modules := doc.Components.Schemas.Closure.Properties.SkippedModules.Items.Enum
	if len(modules) == 0 {
		t.Fatal("Closure.skipped_modules enum is empty")
	}
	for _, mod := range modules {
		if !optional[mod] {
			t.Fatalf("closure-coordinated module %q is missing from module.yaml optional dependencies", mod)
		}
	}
}
