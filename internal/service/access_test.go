package service

import (
	"net/netip"
	"testing"
)

func TestActionAllowListDefaultsToLoopback(t *testing.T) {
	allow := Config{}.ActionAllowList()

	for _, addr := range []string{"127.0.0.1", "127.0.0.2", "::1"} {
		if !ActionsAllowedFrom(allow, netip.MustParseAddr(addr)) {
			t.Errorf("ActionsAllowedFrom(default, %s) = false, want true", addr)
		}
	}
	for _, addr := range []string{"192.168.1.20", "10.0.0.5", "fe80::1", "2001:db8::1"} {
		if ActionsAllowedFrom(allow, netip.MustParseAddr(addr)) {
			t.Errorf("ActionsAllowedFrom(default, %s) = true, want false", addr)
		}
	}
}

// A dual-stack listener - what binding ":8091" produces on Linux - reports an
// IPv4 peer as the IPv4-mapped ::ffff:127.0.0.1. netip.Prefix.Contains never
// matches a 4-in-6 address against an IPv4 prefix, so without the Unmap in
// ActionsAllowedFrom the operator's own browser would be refused.
func TestActionsAllowedFromUnmapsIPv4In6(t *testing.T) {
	allow := Config{}.ActionAllowList()

	if !ActionsAllowedFrom(allow, netip.MustParseAddr("::ffff:127.0.0.1")) {
		t.Error("ActionsAllowedFrom(default, ::ffff:127.0.0.1) = false, want true")
	}
	if ActionsAllowedFrom(allow, netip.MustParseAddr("::ffff:192.168.1.20")) {
		t.Error("ActionsAllowedFrom(default, ::ffff:192.168.1.20) = true, want false")
	}
}

func TestActionAllowListHonoursConfiguredEntries(t *testing.T) {
	c := Config{AllowActionsFrom: []string{"192.168.1.0/24", "10.0.0.7"}}
	allow := c.ActionAllowList()

	for _, addr := range []string{"192.168.1.1", "192.168.1.254", "10.0.0.7"} {
		if !ActionsAllowedFrom(allow, netip.MustParseAddr(addr)) {
			t.Errorf("ActionsAllowedFrom(%v, %s) = false, want true", c.AllowActionsFrom, addr)
		}
	}
	// An explicit list replaces the loopback default rather than adding to it,
	// and a bare address is a single host, not its whole /24.
	for _, addr := range []string{"127.0.0.1", "::1", "192.168.2.1", "10.0.0.8"} {
		if ActionsAllowedFrom(allow, netip.MustParseAddr(addr)) {
			t.Errorf("ActionsAllowedFrom(%v, %s) = true, want false", c.AllowActionsFrom, addr)
		}
	}
}

func TestActionAllowListNormalisesHostBits(t *testing.T) {
	// "127.0.0.1/8" is the form a reader is likely to write by hand; it must
	// mean the 127.0.0.0/8 network it describes.
	allow := Config{AllowActionsFrom: []string{"127.0.0.1/8"}}.ActionAllowList()

	if !ActionsAllowedFrom(allow, netip.MustParseAddr("127.0.0.9")) {
		t.Error("ActionsAllowedFrom(127.0.0.1/8, 127.0.0.9) = false, want true")
	}
}

func TestParseAllowEntryRejectsGarbage(t *testing.T) {
	for _, entry := range []string{"", "  ", "localhost", "127.0.0.1/33", "not-an-ip", "1.2.3.4/", "/8"} {
		if _, err := parseAllowEntry(entry); err == nil {
			t.Errorf("parseAllowEntry(%q) = nil error, want an error", entry)
		}
	}
}

func TestValidateReportsBadAllowActionsFromEntry(t *testing.T) {
	c := Config{
		Dir:              t.TempDir(),
		AllowActionsFrom: []string{"127.0.0.0/8", "localhost"},
	}

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error for the unparseable entry")
	}
	verr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("Validate() returned %T, want *ValidationError", err)
	}
	if len(verr.Fields) != 1 || verr.Fields[0].Field != "allowActionsFrom[1]" {
		t.Fatalf("Validate() fields = %+v, want one error on allowActionsFrom[1]", verr.Fields)
	}
}

func TestEffectiveAllowActionsFromReportsDefaults(t *testing.T) {
	got := Config{}.EffectiveAllowActionsFrom()
	want := []string{"127.0.0.0/8", "::1/128"}

	if len(got) != len(want) {
		t.Fatalf("EffectiveAllowActionsFrom() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EffectiveAllowActionsFrom() = %v, want %v", got, want)
		}
	}

	// The caller must not be able to edit the package-level default through
	// the returned slice.
	got[0] = "0.0.0.0/0"
	if defaultAllowActionsFrom[0] != "127.0.0.0/8" {
		t.Fatalf("defaultAllowActionsFrom was mutated: %v", defaultAllowActionsFrom)
	}
}
