package regast

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"mohabbat/mohabbat/regast/syntax"
)

func TestInvalidBytesAreDistinctSymbols(t *testing.T) {
	first := []byte{0x80}
	second := []byte{0x81}

	for _, tc := range []struct {
		pattern []byte
		input   []byte
		want    []int
	}{
		{first, first, []int{0, 1}},
		{first, second, nil},
		{[]byte(`.`), first, []int{0, 1}},
		{[]byte(`[^\d]`), first, []int{0, 1}},
		{[]byte(`\pL`), first, nil},
		{[]byte(`\x{110080}`), first, []int{0, 1}},
		{[]byte(`\x{110081}`), first, nil},
	} {
		re, err := CompileBytes(tc.pattern)
		if err != nil {
			t.Fatalf("CompileBytes(%q): %v", tc.pattern, err)
		}
		if got := re.FindIndex(tc.input); !equalInts(got, tc.want) {
			t.Errorf("CompileBytes(%q).FindIndex(%q) = %v, want %v", tc.pattern, tc.input, got, tc.want)
		}
	}

	dot := MustCompile(`.`)
	if got, want := dot.FindAllIndex([]byte{0x80, 0x81, '\n'}, -1), [][]int{{0, 1}, {1, 2}}; !equalIndexSlices(got, want) {
		t.Errorf("dot matches = %v, want %v", got, want)
	}
}

func TestValidRuneErrorDiffersFromInvalidByte(t *testing.T) {
	pattern := []byte("\uFFFD")
	re, err := CompileBytes(pattern)
	if err != nil {
		t.Fatal(err)
	}
	valid := []byte("\uFFFD")
	if got := re.FindIndex(valid); !equalInts(got, []int{0, len(valid)}) {
		t.Errorf("FindIndex(valid RuneError) = %v, want [0 %d]", got, len(valid))
	}
	if got := re.FindIndex([]byte{0x80}); got != nil {
		t.Errorf("FindIndex(invalid byte) = %v, want nil", got)
	}
}

func TestInvalidByteCapturesAndReplacementPreserveBytes(t *testing.T) {
	input := []byte{'a', 0x80, 'b', 0x81, 'c'}
	re, err := CompileBytes([]byte(`(.)b(.)`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := re.FindSubmatchIndex(input), []int{1, 4, 1, 2, 3, 4}; !equalInts(got, want) {
		t.Fatalf("FindSubmatchIndex = %v, want %v", got, want)
	}
	if got, want := re.ReplaceAll(input, []byte("$2$1")), []byte{'a', 0x81, 0x80, 'c'}; !bytes.Equal(got, want) {
		t.Errorf("ReplaceAll = %v, want %v", got, want)
	}
	if got, want := re.ReplaceAll(input, []byte("X")), []byte{'a', 'X', 'c'}; !bytes.Equal(got, want) {
		t.Errorf("ReplaceAll literal = %v, want %v", got, want)
	}
}

func TestByteMatchesAgreeWithRegexpOnValidUTF8(t *testing.T) {
	patterns := []string{``, `a*`, `a+`, `(\w+)-(\d*)`, `^.*?$`, `x|xy`, `(?:ab)?`}
	inputs := []string{"aéaa", "café-42!", "line\n", "xy", "é"}
	for _, pattern := range patterns {
		gotRE, err := Compile(pattern)
		if err != nil {
			t.Fatalf("Compile(%q): %v", pattern, err)
		}
		wantRE, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("regexp.Compile(%q): %v", pattern, err)
		}
		for _, input := range inputs {
			got := gotRE.FindSubmatchIndex([]byte(input))
			want := wantRE.FindSubmatchIndex([]byte(input))
			if !equalInts(got, want) {
				t.Errorf("%q on %q: got %v, want %v", pattern, input, got, want)
			}
			gotAll := gotRE.FindAllSubmatchIndex([]byte(input), -1)
			wantAll := wantRE.FindAllSubmatchIndex([]byte(input), -1)
			if !equalIndexSlices(gotAll, wantAll) {
				t.Errorf("%q on %q all matches: got %v, want %v", pattern, input, gotAll, wantAll)
			}
		}
	}
}

func TestBitStateReusesFlatBitsAcrossSearches(t *testing.T) {
	b := &bitState{}
	prog := &syntax.Prog{Inst: make([]syntax.Inst, 8)}
	b.reset(prog, 0, 0)
	if !b.shouldVisit(0, 0) || b.shouldVisit(0, 0) {
		t.Fatal("first search did not mark a visited bit")
	}
	visited := &b.visited[0]

	b.reset(prog, 0, 0)
	if &b.visited[0] != visited {
		t.Fatal("flat visited vector was not reused")
	}
	if !b.shouldVisit(0, 0) || b.shouldVisit(0, 0) {
		t.Fatal("visited bits were not reset for the next search")
	}
}

func TestNodeGroupStateIsConditional(t *testing.T) {
	plain := MustCompile(`a+`).prog
	nodeGroup := MustCompile(`⦅a+⦆`).prog
	if plain.HasNodeGroup || !nodeGroup.HasNodeGroup {
		t.Fatal("test programs do not distinguish plain and node-group patterns")
	}

	b := &bitState{}
	b.reset(plain, 1, 0)
	if b.frameIntern != nil || b.nodeVisited != nil || cap(b.nodeJobs) != 0 {
		t.Fatal("plain pattern initialized AST state")
	}
	if len(b.visited) == 0 {
		t.Fatal("plain pattern did not allocate its flat visited vector")
	}

	b.reset(nodeGroup, 100_000, 0)
	if len(b.visited) != 0 {
		t.Fatalf("node-group pattern allocated %d plain visited words", len(b.visited))
	}
	if b.frameIntern == nil || b.nodeVisited == nil || cap(b.nodeJobs) == 0 {
		t.Fatal("node-group pattern did not initialize AST state")
	}
	b.frameIntern[1] = 2
	b.nodeVisited[nodeVisitKey{pc: 1, pos: 2}] = struct{}{}
	b.frames = append(b.frames, nodeFrame{end: 2, parent: -1})

	b.reset(plain, 1, 0)
	if len(b.frameIntern) != 1 || len(b.nodeVisited) != 1 || len(b.frames) != 1 {
		t.Fatal("plain pattern reset AST state")
	}

	b.reset(nodeGroup, 1, 0)
	if len(b.frameIntern) != 0 || len(b.nodeVisited) != 0 || len(b.frames) != 0 {
		t.Fatal("node-group reset retained state from the previous search")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return len(a) == 0 && len(b) == 0
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalIndexSlices(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalInts(a[i], b[i]) {
			return false
		}
	}
	return true
}

func BenchmarkByteSubject(b *testing.B) {
	cases := []struct {
		name    string
		pattern string
		input   []byte
	}{
		{
			name:    "valid-utf8",
			pattern: `\pL+-[0-9]+$`,
			input:   []byte(strings.Repeat("café-1234/", 128) + "café-1234"),
		},
		{
			name:    "malformed-utf8",
			pattern: `.+-[0-9]+$`,
			input:   append(bytes.Repeat([]byte{'x', 0x80, 'y', '/'}, 128), []byte("tail-1234")...),
		},
	}
	for _, tc := range cases {
		b.Run(tc.name+"/regast", func(b *testing.B) {
			re, err := Compile(tc.pattern)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(tc.input)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !re.Match(tc.input) {
					b.Fatal("no match")
				}
			}
		})
		b.Run(tc.name+"/stdlib", func(b *testing.B) {
			re, err := regexp.Compile(tc.pattern)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(tc.input)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !re.Match(tc.input) {
					b.Fatal("no match")
				}
			}
		})
	}
}
