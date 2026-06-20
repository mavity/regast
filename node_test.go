package regexp

import (
	"strings"
	"testing"
)

// TestNodeGolden is the acceptance wall: each case is an input string, a regast
// pattern, a replacement template, and the exact expected output. Ported from
// the regast-poc golden table to the public regexp-compatible API.
func TestNodeGolden(t *testing.T) {
	cases := []struct {
		name string
		in   string
		pat  string
		repl string
		want string
	}{
		// --- Plain regex (no node-groups): identical to upstream regexp. ---
		{
			name: "plain literal replace",
			in:   "package p\n\nvar x = 1\n",
			pat:  `x = 1`,
			repl: `y = 2`,
			want: "package p\n\nvar y = 2\n",
		},
		{
			name: "plain regex replaces every match",
			in:   "package p\n\nvar a = 12\nvar b = 345\n",
			pat:  `\d+`,
			repl: `N`,
			want: "package p\n\nvar a = N\nvar b = N\n",
		},
		{
			name: "plain regex group swap",
			in:   "package p\n\nvar _ = \"a-b\"\n",
			pat:  `(\w)-(\w)`,
			repl: `$2-$1`,
			want: "package p\n\nvar _ = \"b-a\"\n",
		},

		// --- Node-groups: match a whole AST node, spacing-insensitive. ---
		{
			name: "rename qualified identifier",
			in:   "package p\n\nvar _ = unix.Major\n",
			pat:  `⦃unix\.Major⦄`,
			repl: `syscall.Major`,
			want: "package p\n\nvar _ = syscall.Major\n",
		},
		{
			name: "rename ignores spacing around dot",
			in:   "package p\n\nvar _ = unix . Major\n",
			pat:  `⦃unix\.Major⦄`,
			repl: `syscall.Major`,
			want: "package p\n\nvar _ = syscall.Major\n",
		},
		{
			name: "rename ignores a comment in the selector",
			in:   "package p\n\nvar _ = unix /*c*/ . Major\n",
			pat:  `⦃unix\.Major⦄`,
			repl: `syscall.Major`,
			want: "package p\n\nvar _ = syscall.Major\n",
		},
		{
			name: "wrap a node, $1 is its raw text",
			in:   "package p\n\nvar _ = foo.bar\n",
			pat:  `⦃\w+\.\w+⦄`,
			repl: `wrap($1)`,
			want: "package p\n\nvar _ = wrap(foo.bar)\n",
		},
		{
			name: "node capture preserves original spacing",
			in:   "package p\n\nvar _ = foo . bar\n",
			pat:  `⦃\w+\.\w+⦄`,
			repl: `wrap($1)`,
			want: "package p\n\nvar _ = wrap(foo . bar)\n",
		},
		{
			name: "swap binary operands",
			in:   "package p\n\nvar _ = a + b\n",
			pat:  `⦃(\w+) \+ (\w+)⦄`,
			repl: `$3 + $2`,
			want: "package p\n\nvar _ = b + a\n",
		},
		{
			name: "swap operands matches tight source too",
			in:   "package p\n\nvar _ = a+b\n",
			pat:  `⦃(\w+) \+ (\w+)⦄`,
			repl: `$3 + $2`,
			want: "package p\n\nvar _ = b + a\n",
		},
		{
			name: "nested node-group capture",
			in:   "package p\n\nvar _ = foo.bar\n",
			pat:  `⦃⦃\w+⦄\.\w+⦄`,
			repl: `$2`,
			want: "package p\n\nvar _ = foo\n",
		},
		{
			name: "same-start picks the identifier, not the selector",
			in:   "package p\n\nvar _ = foo.bar\n",
			pat:  `⦃foo⦄`,
			repl: `X`,
			want: "package p\n\nvar _ = X.bar\n",
		},
		{
			name: "replace all selectors",
			in:   "package p\n\nvar _ = a.b\nvar _ = c.d\n",
			pat:  `⦃\w+\.\w+⦄`,
			repl: `X`,
			want: "package p\n\nvar _ = X\nvar _ = X\n",
		},
		{
			name: "rename a typed field, word spacing required",
			in:   "package p\n\nfunc f(x int) {}\n",
			pat:  `⦃x int⦄`,
			repl: `y int64`,
			want: "package p\n\nfunc f(y int64) {}\n",
		},

		// --- No match leaves the input untouched. ---
		{
			name: "no match is a no-op",
			in:   "package p\n\nvar _ = a.b\n",
			pat:  `⦃x\.y⦄`,
			repl: `Z`,
			want: "package p\n\nvar _ = a.b\n",
		},
		{
			name: "word pattern does not merge two identifiers",
			in:   "package p\n\nvar xy = 0\n",
			pat:  `⦃x y⦄`,
			repl: `Z`,
			want: "package p\n\nvar xy = 0\n",
		},
		{
			name: "word pattern does not split a single identifier",
			in:   "package p\n\nfunc f(a b) {}\n",
			pat:  `⦃ab⦄`,
			repl: `Z`,
			want: "package p\n\nfunc f(a b) {}\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			re, err := Compile(c.pat)
			if err != nil {
				t.Fatalf("compile %q: %v", c.pat, err)
			}
			got := re.ReplaceAllString(c.in, c.repl)
			if got != c.want {
				t.Errorf("in=%q pat=%q repl=%q\n got=%q\nwant=%q", c.in, c.pat, c.repl, got, c.want)
			}
		})
	}
}

// TestNodeGapWordRule checks the token-stream boundary rule directly.
func TestNodeGapWordRule(t *testing.T) {
	cases := []struct {
		name string
		src  string
		pat  string
		want bool
	}{
		{"dot-tight-pat-tight", "package p\n\nvar _ = a.b\n", `⦃a\.b⦄`, true},
		{"dot-tight-pat-spaced", "package p\n\nvar _ = a.b\n", `⦃a \. b⦄`, true},
		{"dot-spaced-pat-tight", "package p\n\nvar _ = a . b\n", `⦃a\.b⦄`, true},
		{"dot-spaced-pat-spaced", "package p\n\nvar _ = a . b\n", `⦃a \. b⦄`, true},
		{"dot-comment", "package p\n\nvar _ = a /*x*/ . b\n", `⦃a\.b⦄`, true},
		{"plus-tight-pat-spaced", "package p\n\nvar _ = a+b\n", `⦃a \+ b⦄`, true},
		{"plus-spaced-pat-tight", "package p\n\nvar _ = a + b\n", `⦃a\+b⦄`, true},
		{"word-space-needs-space", "package p\n\nfunc f(x int) {}\n", `⦃x int⦄`, true},
		{"word-extra-space-ok", "package p\n\nfunc f(x  int) {}\n", `⦃x int⦄`, true},
		{"word-no-merge", "package p\n\nvar xy = 0\n", `⦃x y⦄`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			re, err := Compile(c.pat)
			if err != nil {
				t.Fatal(err)
			}
			if got := re.MatchString(c.src); got != c.want {
				t.Errorf("src=%q pat=%q: got %v want %v", c.src, c.pat, got, c.want)
			}
		})
	}
}

// TestNodeDeepNesting proves node-group nesting is unbounded (depth 12, far
// past the old fixed cap of 8). Uses nested parentheses so each level has only
// one candidate node end — i.e. genuine deep nesting without the (inherent)
// exponential blow-up of deeply-nested *same-start* node-groups.
func TestNodeDeepNesting(t *testing.T) {
	const n = 12
	expr := strings.Repeat("(", n) + "x" + strings.Repeat(")", n)
	src := "package p\n\nvar _ = " + expr + "\n"

	// n node-groups deep, each matching one ParenExpr: ⦃\(⦃\(…x…\)⦄\)⦄
	pat := strings.Repeat(`⦃\(`, n) + "x" + strings.Repeat(`\)⦄`, n)

	re, err := Compile(pat)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no match at nesting depth %d (cap regression?)", n)
	}
	if got := src[loc[0]:loc[1]]; got != expr {
		t.Errorf("match = %q, want %q", got, expr)
	}
}

func TestNodeSubmatchIndex(t *testing.T) {
	src := "package p\n\nvar _ = foo.bar\n"
	re := MustCompile(`⦃(\w+)\.(\w+)⦄`)
	m := re.FindStringSubmatchIndex(src)
	if m == nil {
		t.Fatal("no match")
	}
	// group0 = foo.bar, group1 = node text (foo.bar), group2 = foo, group3 = bar.
	whole := src[m[0]:m[1]]
	if whole != "foo.bar" {
		t.Errorf("group0 = %q, want foo.bar", whole)
	}
	if g2 := src[m[4]:m[5]]; g2 != "foo" {
		t.Errorf("group2 = %q, want foo", g2)
	}
	if g3 := src[m[6]:m[7]]; g3 != "bar" {
		t.Errorf("group3 = %q, want bar", g3)
	}
}
