package syntax

import (
	"strings"
	"testing"
)

func TestParseNodeGroup(t *testing.T) {
	re, err := Parse(`⦃a\.b⦄`, Perl)
	if err != nil {
		t.Fatal(err)
	}
	// Top level should be a capture wrapping a node group.
	if re.Op != OpCapture {
		t.Fatalf("top op = %v, want OpCapture", re.Op)
	}
	if re.Sub[0].Op != OpNodeGroup {
		t.Fatalf("inner op = %v, want OpNodeGroup", re.Sub[0].Op)
	}
	prog, err := Compile(re.Simplify())
	if err != nil {
		t.Fatal(err)
	}
	if !prog.HasNodeGroup {
		t.Errorf("prog.HasNodeGroup = false, want true")
	}
	dump := prog.String()
	if !strings.Contains(dump, "nodegroup") || !strings.Contains(dump, "nodeexit") {
		t.Errorf("prog missing node instructions:\n%s", dump)
	}
}

func TestParseNodeGap(t *testing.T) {
	// In-node whitespace becomes a gap; outside, it stays literal.
	re, err := Parse(`⦃a b⦄`, Perl)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := Compile(re.Simplify())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prog.String(), "gap") {
		t.Errorf("expected a gap instruction in:\n%s", prog.String())
	}

	// Plain pattern: no node ops, whitespace literal.
	re2, _ := Parse(`a b`, Perl)
	prog2, _ := Compile(re2.Simplify())
	if prog2.HasNodeGroup {
		t.Errorf("plain pattern must not set HasNodeGroup")
	}
	if strings.Contains(prog2.String(), "gap") {
		t.Errorf("plain pattern must not contain a gap instruction")
	}
}

func TestParseNodeNested(t *testing.T) {
	re, err := Parse(`⦃⦃\w+⦄\.\w+⦄`, Perl)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := Compile(re.Simplify())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(prog.String(), "nodegroup") != 2 {
		t.Errorf("expected 2 nodegroup instructions:\n%s", prog.String())
	}
}
