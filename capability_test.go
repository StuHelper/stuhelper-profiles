package profiles_test

import (
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
