package regast

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"mohabbat/mohabbat/regast/syntax"
)

func compileBackref(t *testing.T, pattern string) *Regexp {
	t.Helper()
	re, err := CompileWithFlags(pattern, syntax.Perl|syntax.Backref)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

func TestBackrefMatchingAndCaptureResults(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    []int
	}{
		{`([a-z]+)-\1`, "foo-foo", []int{0, 7, 0, 3}},
		{`(a){2,3}\1`, "aaa", []int{0, 3, 1, 2}},
		{`(?:(a)\1){2}`, "aaaa", []int{0, 4, 2, 3}},
		{`((a)|b)+\2`, "aba", []int{0, 3, 1, 2, 0, 1}},
		{`(a)\10`, "aa0", []int{0, 3, 0, 1}},
		{`(a*)\1`, "", []int{0, 0, 0, 0}},
		{`(a){0}\1`, "", nil},
		{`\1(a)`, "aa", nil},
		{`(a\1)`, "aa", nil},
		{`(a)?b\1`, "b", nil},
	}

	for _, test := range tests {
		re := compileBackref(t, test.pattern)
		if got := re.FindSubmatchIndex([]byte(test.input)); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%q on %q: got %v, want %v", test.pattern, test.input, got, test.want)
		}
	}
}

func TestBackrefCaseFoldingAndMalformedBytes(t *testing.T) {
	re := compileBackref(t, `(?i:(K)\1)`)
	if got, want := re.FindSubmatchIndex([]byte("Kk")), []int{0, 4, 0, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("case-folded match indexes = %v, want %v", got, want)
	}

	bytesRE, err := CompileBytesWithFlags([]byte(`(.)\1`), syntax.Perl|syntax.Backref)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bytesRE.FindSubmatchIndex([]byte{0x80, 0x80}), []int{0, 2, 0, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("malformed-byte match indexes = %v, want %v", got, want)
	}
	if got := bytesRE.FindIndex([]byte{0x80, 0x81}); got != nil {
		t.Fatalf("distinct malformed bytes matched: %v", got)
	}

	foldedBytes, err := CompileBytesWithFlags([]byte(`(?i:(.)\1)`), syntax.Perl|syntax.Backref)
	if err != nil {
		t.Fatal(err)
	}
	if got := foldedBytes.FindIndex([]byte{0x80, 0x81}); got != nil {
		t.Fatalf("case folding equated distinct malformed bytes: %v", got)
	}
}

func TestBackrefWordBoundaries(t *testing.T) {
	re := compileBackref(t, `\<foo\>`)
	if got, want := re.FindIndex([]byte("xfoo foo!")), []int{5, 8}; !reflect.DeepEqual(got, want) {
		t.Fatalf("word-boundary match = %v, want %v", got, want)
	}
}

func TestBackrefLongestMatch(t *testing.T) {
	re := compileBackref(t, `^(a|aa)\1$`)
	re.Longest()
	if got, want := re.FindSubmatchIndex([]byte("aaaa")), []int{0, 4, 0, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("longest match indexes = %v, want %v", got, want)
	}
}

func TestBackrefRejectsNodeGroups(t *testing.T) {
	if _, err := CompileWithFlags(`⦅(a)\1⦆`, syntax.Perl|syntax.Backref); err == nil {
		t.Fatal("CompileWithFlags accepted a pattern combining backreferences and node-groups")
	}
}

func TestBackrefTryAPIsAndReaderPolicy(t *testing.T) {
	re := compileBackref(t, `([a-z]+)-\1`)
	if _, err := re.TryFindSubmatchIndex([]byte("foo-foo"), 1); !errors.Is(err, ErrStepLimit) {
		t.Fatalf("TryFindSubmatchIndex error = %v, want ErrStepLimit", err)
	}
	if _, err := re.TryReplaceAll([]byte("foo-foo"), []byte("$1"), 1); !errors.Is(err, ErrStepLimit) {
		t.Fatalf("TryReplaceAll error = %v, want ErrStepLimit", err)
	}
	if got, err := re.TryReplaceAll([]byte("foo-foo"), []byte("$1"), 1000); err != nil || !bytes.Equal(got, []byte("foo")) {
		t.Fatalf("TryReplaceAll = %q, %v; want foo, nil", got, err)
	}
	pair := compileBackref(t, `(a)\1`)
	if _, err := pair.TryReplaceAll(bytes.Repeat([]byte("aa "), 10), []byte("x"), 20); !errors.Is(err, ErrStepLimit) {
		t.Fatalf("TryReplaceAll total budget error = %v, want ErrStepLimit", err)
	}
	prefixed := compileBackref(t, `abc(a)\1`)
	if _, err := prefixed.TryFindSubmatchIndex(bytes.Repeat([]byte{'x'}, 1000), 10); !errors.Is(err, ErrStepLimit) {
		t.Fatalf("prefix search limit error = %v, want ErrStepLimit", err)
	}
	if got := re.FindReaderIndex(strings.NewReader("foo-foo")); got != nil {
		t.Fatalf("FindReaderIndex with backref = %v, want nil", got)
	}

	matched, err := re.TryMatch([]byte("foo-foo"), 1000)
	if err != nil || !matched {
		t.Fatalf("TryMatch = %v, %v; want true, nil", matched, err)
	}
}

func TestBackrefLargeInputDoesNotAllocateFlatVisitedVector(t *testing.T) {
	re := compileBackref(t, `^(a)\1$`)
	input := bytes.Repeat([]byte{'x'}, 40_000)
	if re.Match(input) {
		t.Fatal("unexpected match")
	}
}
