package egress

import "testing"

func TestModeCompatibilityAndRevision(t *testing.T) {
	old, err := Decode([]byte(`{"ips":[],"domains":[],"services":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if old.EffectiveMode() != ModeAllowlist {
		t.Fatal("historical configuration changed behavior")
	}
	explicit := old
	explicit.Mode = ModeAllowlist
	if old.Revision() != explicit.Revision() {
		t.Fatal("explicit allowlist invalidates historical loaded revision")
	}
	revisions := map[string]bool{old.Revision(): true}
	for _, mode := range []Mode{ModeLegacy, ModeUnrestricted} {
		changed := old
		changed.Mode = mode
		if err := changed.Validate(); err != nil {
			t.Fatal(err)
		}
		if revisions[changed.Revision()] {
			t.Fatal("distinct modes share revision")
		}
		revisions[changed.Revision()] = true
	}
}

func TestInvalidModeRejected(t *testing.T) {
	for _, mode := range []string{"ALLOWLIST", "open", "0.0.0.0/0", " legacy "} {
		if _, err := Decode([]byte(`{"mode":"` + mode + `"}`)); err == nil {
			t.Fatalf("accepted %q", mode)
		}
	}
}

func TestUnsupportedModeCannotProduceAllowlistDataPlane(t *testing.T) {
	for _, mode := range []Mode{ModeLegacy, ModeUnrestricted} {
		c := Config{Mode: mode}
		if _, err := c.Routes(); err != nil {
			t.Fatalf("%s cannot keep old proxy Pods served during rollout: %v", mode, err)
		}
		if _, err := c.WorkspacePolicy("work", "system", []string{"10.0.0.0/8"}); err == nil {
			t.Fatalf("%s falsely produced strict policy", mode)
		}
	}
}
