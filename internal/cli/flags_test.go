package cli

import "testing"

// Regression: `search q -n --json` used to consume "--json" as the value of
// -n, silently discarding the JSON output flag.
func TestParseKnownDoesNotSwallowFollowingFlag(t *testing.T) {
	if _, _, bad := parseKnown([]string{"q", "-n", "--json"}, searchFlags); !bad {
		t.Fatal("expected a usage error for a missing -n value")
	}
	pv, pos, bad := parseKnown([]string{"q", "-n", "5", "--json"}, searchFlags)
	if bad {
		t.Fatal("valid argument list rejected")
	}
	if n := pv.int("n", 10); n != 5 {
		t.Fatalf("n=%d, want 5", n)
	}
	if !pv.boolean("json") {
		t.Fatal("--json was lost")
	}
	if len(pos) != 1 || pos[0] != "q" {
		t.Fatalf("positional args = %v", pos)
	}
}

// A negative numeric value is not a flag name and must remain a value.
func TestParseKnownAllowsNegativeNumericValue(t *testing.T) {
	pv, _, bad := parseKnown([]string{"q", "--recency", "-0.2"}, searchFlags)
	if bad {
		t.Fatal("negative numeric value rejected")
	}
	if got := pv.float("recency", 0); got != -0.2 {
		t.Fatalf("recency=%v", got)
	}
}

// Invalid or out-of-range integers must be usage errors, not silent defaults.
func TestIntInRange(t *testing.T) {
	pv, _, _ := parseKnown([]string{"-n", "0"}, searchFlags)
	if _, err := pv.intInRange("n", 10, 1, 1000); err == nil {
		t.Fatal("n=0 must be rejected")
	}
	pv, _, _ = parseKnown([]string{"-n", "abc"}, searchFlags)
	if _, err := pv.intInRange("n", 10, 1, 1000); err == nil {
		t.Fatal("n=abc must be rejected")
	}
	pv, _, _ = parseKnown([]string{"-n", "20"}, searchFlags)
	if n, err := pv.intInRange("n", 10, 1, 1000); err != nil || n != 20 {
		t.Fatalf("n=20 -> %d, %v", n, err)
	}
	empty := &parsedValues{m: map[string][]string{}}
	if n, err := empty.intInRange("n", 10, 1, 1000); err != nil || n != 10 {
		t.Fatalf("default -> %d, %v", n, err)
	}
}

func TestWatchFlagsIncludeMaxSize(t *testing.T) {
	if _, ok := watchFlags["max-size"]; !ok {
		t.Fatal("watch must accept --max-size so it matches index limits")
	}
}
