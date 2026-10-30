package syntax

import "testing"

func TestBackrefParsingAndCompilation(t *testing.T) {
	re, err := Parse(`(a)\10`, Perl|Backref)
	if err != nil {
		t.Fatal(err)
	}
	if got := re.MaxBackref(); got != 1 {
		t.Fatalf("MaxBackref() = %d, want 1", got)
	}
	if re.String() != `(a)\10` {
		t.Fatalf("String() = %q, want %q", re.String(), `(a)\10`)
	}
	prog, err := Compile(re.Simplify())
	if err != nil {
		t.Fatal(err)
	}
	if !prog.HasBackref || prog.BackrefMask != 1<<1 {
		t.Fatalf("compiled backref metadata = (%v, %#x), want (true, %#x)", prog.HasBackref, prog.BackrefMask, 1<<1)
	}
}

func TestBackrefRejectsOutOfRangeCapture(t *testing.T) {
	if _, err := Parse(`(a)\2`, Perl|Backref); err == nil {
		t.Fatal("Parse accepted a reference beyond the final capture count")
	}
}

func TestBackrefDisabledWithoutDialectFlag(t *testing.T) {
	if _, err := Parse(`(a)\1`, Perl); err == nil {
		t.Fatal("Parse accepted a backreference without the Backref flag")
	}
}

func TestGNUWordBoundaryAssertions(t *testing.T) {
	for _, pattern := range []string{`\<word`, `word\>`} {
		re, err := Parse(pattern, Perl|Backref)
		if err != nil {
			t.Fatalf("Parse(%q): %v", pattern, err)
		}
		if got := re.String(); got != pattern {
			t.Errorf("String() = %q, want %q", got, pattern)
		}
	}
}
