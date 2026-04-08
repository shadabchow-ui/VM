package idgen_test

import (
	"strings"
	"testing"

	"github.com/compute-platform/compute-platform/packages/idgen"
)

func TestNew_HasPrefix(t *testing.T) {
	id := idgen.New(idgen.PrefixInstance)
	if !strings.HasPrefix(id, idgen.PrefixInstance+"_") {
		t.Errorf("expected prefix %q, got %q", idgen.PrefixInstance+"_", id)
	}
}

func TestNew_Unique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := idgen.New(idgen.PrefixInstance)
		if seen[id] {
			t.Fatalf("duplicate ID generated after %d iterations: %s", i, id)
		}
		seen[id] = true
	}
}

func TestNew_AllPrefixes(t *testing.T) {
	prefixes := []string{
		idgen.PrefixInstance, idgen.PrefixJob, idgen.PrefixDisk,
		idgen.PrefixPrincipal, idgen.PrefixAccount, idgen.PrefixHost,
		idgen.PrefixSSHKey, idgen.PrefixAPIKey, idgen.PrefixEvent,
	}
	for _, p := range prefixes {
		id := idgen.New(p)
		if !idgen.IsValid(id, p) {
			t.Errorf("IsValid(%q, %q) = false", id, p)
		}
	}
}

func TestNew_NonEmpty(t *testing.T) {
	id := idgen.New(idgen.PrefixJob)
	parts := strings.SplitN(id, "_", 2)
	if len(parts) != 2 || parts[1] == "" {
		t.Errorf("ID has empty body: %q", id)
	}
}
