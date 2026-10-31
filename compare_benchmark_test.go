package regast

import (
	"bytes"
	"reflect"
	stdregexp "regexp"
	"strconv"
	"testing"
)

type findAllIndexer interface {
	FindAllIndex([]byte, int) [][]int
}

var findAllIndexBenchmarkSink [][]int

func BenchmarkFindAllIndexScaling(b *testing.B) {
	engines := []struct {
		name    string
		compile func(string) (findAllIndexer, error)
	}{
		{
			name: "regast",
			compile: func(pattern string) (findAllIndexer, error) {
				return Compile(pattern)
			},
		},
		{
			name: "stdlib",
			compile: func(pattern string) (findAllIndexer, error) {
				return stdregexp.Compile(pattern)
			},
		},
	}
	cases := []struct {
		name      string
		pattern   string
		makeInput func(int) []byte
	}{
		{
			name:    "prefix_hit",
			pattern: "needle",
			makeInput: func(size int) []byte {
				return append(bytes.Repeat([]byte("x"), size), "needle"...)
			},
		},
		{
			name:    "rune_scan_miss",
			pattern: `[^x]`,
			makeInput: func(size int) []byte {
				return bytes.Repeat([]byte("x"), size)
			},
		},
		{
			name:    "non_onepass_scan",
			pattern: `.bc(d|e)*$`,
			makeInput: func(size int) []byte {
				return append([]byte("abc"), bytes.Repeat([]byte("d"), size-3)...)
			},
		},
	}
	sizes := []int{32, 4096, 1 << 20}

	for _, tc := range cases {
		for _, size := range sizes {
			input := tc.makeInput(size)
			for _, engine := range engines {
				b.Run(engine.name+"/"+tc.name+"/"+strconv.Itoa(size), func(b *testing.B) {
					re, err := engine.compile(tc.pattern)
					if err != nil {
						b.Fatal(err)
					}
					std, err := stdregexp.Compile(tc.pattern)
					if err != nil {
						b.Fatal(err)
					}
					if got, want := re.FindAllIndex(input, -1), std.FindAllIndex(input, -1); !reflect.DeepEqual(got, want) {
						b.Fatalf("FindAllIndex() = %v, stdlib wants %v", got, want)
					}

					b.ReportAllocs()
					b.SetBytes(int64(len(input)))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						findAllIndexBenchmarkSink = re.FindAllIndex(input, -1)
					}
				})
			}
		}
	}
}
