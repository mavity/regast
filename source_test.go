package regexp

import "testing"

func TestASTMapTokenBoundary(t *testing.T) {
	// Offsets: "a + b" inside an expression statement.
	src := "package p\n\nvar _ = a + b\n"
	m, err := newASTMap("t.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	ia := indexOf(src, "a + b")
	a := ia        // 'a'
	plus := ia + 2 // '+'
	b := ia + 4    // 'b'

	if !m.significant(a) || !m.significant(plus) || !m.significant(b) {
		t.Fatalf("a/+/b must all be significant")
	}
	if m.significant(a + 1) { // the space after 'a'
		t.Errorf("space after 'a' should be insignificant")
	}
	// a|+ is a real token seam; +|b is a real token seam.
	if !m.gapBoundary(a, plus) {
		t.Errorf("a|+ should be a token boundary")
	}
	if !m.gapBoundary(plus, b) {
		t.Errorf("+|b should be a token boundary")
	}
}

func TestASTMapSameTokenNoBoundary(t *testing.T) {
	// "xy" is a single identifier token: x|y is NOT a boundary.
	src := "package p\n\nvar xy = 0\n"
	m, err := newASTMap("t.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	ix := indexOf(src, "xy")
	if m.gapBoundary(ix, ix+1) {
		t.Errorf("x|y inside identifier 'xy' must NOT be a boundary")
	}
}

func TestASTMapEndsForStart(t *testing.T) {
	// foo.bar: at 'foo' two nodes start — Ident foo and SelectorExpr foo.bar.
	src := "package p\n\nvar _ = foo.bar\n"
	m, err := newASTMap("t.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	start := indexOf(src, "foo")
	ends := m.endsForStart(start)
	if len(ends) < 2 {
		t.Fatalf("expected >=2 node ends at 'foo', got %v", ends)
	}
	if ends[len(ends)-1] != start+len("foo.bar") {
		t.Errorf("largest end = %d, want %d", ends[len(ends)-1], start+len("foo.bar"))
	}
}

func TestASTMapNextSignificant(t *testing.T) {
	src := "package p\n\nvar _ = a /*c*/ . b\n"
	m, err := newASTMap("t.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	ia := indexOf(src, "a /*c*/ . b")
	// Right after 'a', the next significant byte is the '.'.
	dot := indexOf(src, ".")
	if got := m.nextSignificant(ia + 1); got != dot {
		t.Errorf("nextSignificant after 'a' = %d, want '.' at %d", got, dot)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
