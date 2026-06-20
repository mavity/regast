package regexp

import (
	"testing"
)

func TestNodeGolden(t *testing.T) {
	cases := []struct {
		name string
		in   string
		pat  string
		repl string
		want string
	}{
		// --- Plain regex: identical to upstream regexp ---
		{"plain literal replace", "package p\n\nvar x = 1\n", `x = 1`, `y = 2`, "package p\n\nvar y = 2\n"},
		{"plain regex group swap", "package p\n\nvar _ = \"a-b\"\n", `(\w)-(\w)`, `$2-$1`, "package p\n\nvar _ = \"b-a\"\n"},

		// --- Basic Node Groups ---
		{"rename qualified identifier", "package p\n\nvar _ = unix.Major\n", `⦃unix\.Major⦄`, `syscall.Major`, "package p\n\nvar _ = syscall.Major\n"},
		{"rename with internal spacing", "package p\n\nvar _ = unix  .  Major\n", `⦃unix\.Major⦄`, `syscall.Major`, "package p\n\nvar _ = syscall.Major\n"},
		{"wrap node with $1", "package p\n\nvar _ = foo.bar\n", `⦃\w+\.\w+⦄`, `wrap($1)`, "package p\n\nvar _ = wrap(foo.bar)\n"},
		{"nested node groups numbering", "package p\n\nvar _ = foo.bar\n", `⦃⦃\w+⦄\.\w+⦄`, `$2`, "package p\n\nvar _ = foo\n"},
		{"swap binary operands", "package p\n\nfunc f() { _ = a + b }", `⦃(\w+) \+ (\w+)⦄`, `$3 + $2`, "package p\n\nfunc f() { _ = b + a }"},

		// --- Function and Call Refactorings ---
		{"replace call name", "package p\n\nfunc main() {\n\tlog.Printf(\"hello\")\n}\n", `⦃log\.Printf(\(.*\))⦄`, `logger.Log$2`, "package p\n\nfunc main() {\n\tlogger.Log(\"hello\")\n}\n"},
		{"swap call arguments", "package p\n\nfunc f() { move(a, b) }", `⦃move\(⦃\w+⦄, ⦃\w+⦄\)⦄`, `move($3, $2)`, "package p\n\nfunc f() { move(b, a) }"},
		{"rename method and receiver", "package p\n\nfunc (r *S) Old() {}\n", `func\s+\(⦃\w+⦄\s+⦃\*S⦄\)\s+⦃Old⦄`, `func ($1 $2) New`, "package p\n\nfunc (r *S) New() {}\n"},

		// --- Types and Structs ---
		{"struct field swap", "package p\ntype S struct {\n\tF1 int\n\tF2 string\n}\n", `⦃F1 int⦄\n\t⦃F2 string⦄`, "$2\n\t$1", "package p\ntype S struct {\n\tF2 string\n\tF1 int\n}\n"},
		{"swap struct tags", "package p\ntype S struct {\n\tF int `json:\"f\" xml:\"f\"` \n}\n", "`" + `(json:"f") (xml:"f")` + "`", "`$2 $1`", "package p\ntype S struct {\n\tF int `xml:\"f\" json:\"f\"` \n}\n"},
		{"generic func numbering", "package p\nfunc F[T any](x T) {}\n", `⦃func F\[⦃T any⦄\] \(⦃x T⦄\)⦄`, `func F[$2, U any]($3, y U)`, "package p\nfunc F[T any, U any](x T, y U) {}\n"},

		// --- Loops and Flow ---
		{"for range simplify", "package p\nfunc f() {\n\tfor i := range src {\n\t\tdst = append(dst, src[i])\n\t}\n}\n", `⦃for ⦃i⦄ := range ⦃src⦄ \{ ⦃dst = append\(dst, src\[i\]\)⦄ \}⦄`, `for _, v := range $3 { dst = append(dst, v) }`, "package p\nfunc f() {\n\tfor _, v := range src { dst = append(dst, v) }\n}\n"},
		{"if with init split", "package p\nfunc f() {\n\tif err := work(); err != nil {\n\t\treturn err\n\t}\n}\n", `if\s+⦃err := work\(\)⦄\s*;\s*⦃(\w+) != nil⦄`, `if $1; isBad($3)`, "package p\nfunc f() {\n\tif err := work(); isBad(err) {\n\t\treturn err\n\t}\n}\n"},
		{"switch case rename", "package p\nfunc f(v any) {\n\tswitch v := v.(type) {\n\tcase int:\n\t}\n}\n", `case ⦃int⦄`, `case int64`, "package p\nfunc f(v any) {\n\tswitch v := v.(type) {\n\tcase int64:\n\t}\n}\n"},

		// --- Declarations and Imports ---
		{"multiple imports literal", "package p\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n", `⦃"fmt"⦄`, `"log"`, "package p\nimport (\n\t\"log\"\n\t\"os\"\n)\n"},
		{"var to const", "package p\nvar X = 1\n", `var ⦃X = 1⦄`, `const $1`, "package p\nconst X = 1\n"},
		{"empty interface to any", "package p\nfunc f(x interface{}) {}\n", `⦃interface\{\}⦄`, `any`, "package p\nfunc f(x any) {}\n"},

		// --- Advanced / Edge Cases ---
		{"cross-line binary", "package p\nfunc f() {\n\t_ = a +\n\t\tb\n}\n", `⦃a \+ b⦄`, `(a+b)`, "package p\nfunc f() {\n\t_ = (a+b)\n}\n"},
		{"chan send rename", "package p\nfunc f(ch chan int) { ch  <-  1 }", `⦃ch <- 1⦄`, `ch.Send(1)`, "package p\nfunc f(ch chan int) { ch.Send(1) }"},
		{"variadic to slice", "package p\nfunc f(args ...int) {}", `⦃\( ⦃args \.\.\.int⦄ \)⦄`, `(args []int)`, "package p\nfunc f(args []int) {}"},
		{"map key swap", "package p\nvar _ map[string]int", `map\[⦃string⦄\]⦃int⦄`, `map[$2]$1`, "package p\nvar _ map[int]string"},
		{"comment preservation", "package p\nvar _ = x // stay\n", `⦃x⦄`, `y`, "package p\nvar _ = y // stay\n"},

		// --- Additional Practical / Advanced Cases ---
		{"context background to todo", "package p\nvar ctx = context.Background()\n", `⦃context\.Background\(\)⦄`, `context.TODO()`, "package p\nvar ctx = context.TODO()\n"},
		{"add context timeout", "package p\nvar ctx = context.TODO()\n", `⦃context\.TODO\(\)⦄`, `context.WithTimeout(context.Background(), delay)`, "package p\nvar ctx = context.WithTimeout(context.Background(), delay)\n"},
		{"standardize fmt errorwrapping", "package p\nfunc f() { fmt.Errorf(\"msg: %v\", err) }\n", `fmt\.Errorf\("([^"]+)%v", ⦃err⦄\)`, `fmt.Errorf("$1%w", $2)`, "package p\nfunc f() { fmt.Errorf(\"msg: %w\", err) }\n"},
		{"convert for loop to range", "package p\nfunc f() { for i := 0; i < len(s); i++ { _ = s[i] } }\n", `for ⦃i := 0⦄; ⦃i < len\(s\)⦄; ⦃i\+\+⦄ \{ ⦃_ = s\[i\]⦄ \}`, `for i := range s { $4 }`, "package p\nfunc f() { for i := range s { _ = s[i] } }\n"},
		{"extract error handling to func", "package p\nfunc f() {\n\tif err != nil {\n\t\treturn err\n\t}\n}\n", `if ⦃err != nil⦄ ⦃\{ ⦃return err⦄ \}⦄`, `check($1)`, "package p\nfunc f() {\n\tcheck(err != nil)\n}\n"},
		{"rename receiver in method", "package p\nfunc (this *S) Foo() {}\n", `func \(⦃this \*S⦄\) Foo\(\)`, `func (s *S) Foo()`, "package p\nfunc (s *S) Foo() {}\n"},
		{"wrap defer call in closure", "package p\nfunc f() { defer f.Close() }\n", `defer ⦃f\.Close\(\)⦄`, `defer func() { _ = $1 }()`, "package p\nfunc f() { defer func() { _ = f.Close() }() }\n"},
		{"change slice initialization", "package p\nfunc f() { s := make([]int, 0, 10); _ = s }\n", `make\(\[\]⦃int⦄, 0, ⦃10⦄\)`, `make([]$1, $2)`, "package p\nfunc f() { s := make([]int, 10); _ = s }\n"},
		{"swap coordinates struct", "package p\nvar p = Point{X: 1, Y: 2}\n", `Point\{X: ⦃1⦄, Y: ⦃2⦄\}`, `Point{X: $2, Y: $1}`, "package p\nvar p = Point{X: 2, Y: 1}\n"},
		{"rename field in struct init", "package p\nvar s = S{Old: value}\n", `⦃Old⦄: value`, `New: value`, "package p\nvar s = S{New: value}\n"},
		{"convert type assertion to switch", "package p\nfunc f() { v, ok := x.(int); _, _ = v, ok }\n", `⦃v, ok := x\.\(int\)⦄`, `switch v := x.(type) { case int: }`, "package p\nfunc f() { switch v := x.(type) { case int: }; _, _ = v, ok }\n"},
		{"change varied args call", "package p\nfunc f() { f(a, b, c...) }\n", `⦃f\(⦃a⦄, ⦃b⦄, ⦃c⦄\.\.\.\)⦄`, `f([]any{$2, $3, $4}...)`, "package p\nfunc f() { f([]any{a, b, c}...) }\n"},
		{"strip log prefix from call", "package p\nfunc f() { log.Printf(\"msg\", args) }\n", `log\.⦃Printf⦄\(⦃"[^"]+"⦄, ⦃args⦄\)`, `Log($2, $3)`, "package p\nfunc f() { Log(\"msg\", args) }\n"},
		{"fix error message template", "package p\nfunc f() { panic(fmt.Sprintf(\"fail: %s\", msg)) }\n", `panic\(fmt\.Sprintf\("fail: %s", ⦃msg⦄\)\)`, `panic("fail: " + $1)`, "package p\nfunc f() { panic(\"fail: \" + msg) }\n"},
		{"change map retrieval", "package p\nfunc f() { v, ok := m[key]; _, _ = v, ok }\n", `⦃v, ok := m\[key\]⦄`, `v := m.Get(key)`, "package p\nfunc f() { v := m.Get(key); _, _ = v, ok }\n"},
		{"rewrite select case", "package p\nfunc f() { select { case <-ch: } }\n", `case ⦃<-ch⦄:`, `case v := $1:`, "package p\nfunc f() { select { case v := <-ch: } }\n"},
		{"type switch case rename", "package p\nfunc f(x any) { switch x.(type) { case int: } }\n", `case ⦃int⦄`, `case float64`, "package p\nfunc f(x any) { switch x.(type) { case float64: } }\n"},
		{"labeled loop break", "package p\nfunc f() {\nLoop: for { break Loop }\n}\n", `⦃Loop⦄:`, `Top:`, "package p\nfunc f() {\nTop: for { break Loop }\n}\n"},
		{"rename method on type", "package p\ntype S struct {}\nfunc (S) Old() {}\n", `func \(S\) ⦃Old⦄\(\)`, `func (S) New()`, "package p\ntype S struct {}\nfunc (S) New() {}\n"},
		{"composite literal to constructor", "package p\nvar s = &S{1, 2}\n", `&S\{⦃1⦄, ⦃2⦄\}`, `NewS($1, $2)`, "package p\nvar s = NewS(1, 2)\n"},
		{"slice length check to empty check", "package p\nfunc f() { if len(s) == 0 {} }\n", `if ⦃len\(s\) == 0⦄`, `if s == nil`, "package p\nfunc f() { if s == nil {} }\n"},
		{"remove outer parens", "package p\nfunc f() { x := (1 + 1); _ = x }\n", `⦃\( ⦃1 \+ 1⦄ \)⦄`, `$2`, "package p\nfunc f() { x := 1 + 1; _ = x }\n"},
		{"change operator precedence", "package p\nfunc f() { x := a + b * c; _ = x }\n", `⦃a \+ b \* c⦄`, `(a + b) * c`, "package p\nfunc f() { x := (a + b) * c; _ = x }\n"},
		{"interface method rename", "package p\ntype I interface { Old() }\n", `interface \{ ⦃Old⦄\(\) \}`, `interface { New() }`, "package p\ntype I interface { New() }\n"},
		{"go func to runner", "package p\nfunc f() { go func() { work() }() }\n", `go ⦃func\(\) \{ work\(\) \}\(\)⦄`, `go runner()`, "package p\nfunc f() { go runner() }\n"},
		{"rename multiple variables in decl", "package p\nvar x, y int\n", `var ⦃a, b⦄ int`, `var x, y int`, "package p\nvar x, y int\n"},
		{"simplify if else boolean", "package p\nfunc f() { if x { return true } else { return false } }\n", `if ⦃x⦄ \{ return true \} else \{ return false \}`, `return $1`, "package p\nfunc f() { return x }\n"},
		{"add error return to func", "package p\nfunc F() {\n\treturn\n}\n", `func F\(\) ⦃\{ ⦃return⦄ \}⦄`, `func F() error { return nil }`, "package p\nfunc F() error { return nil }\n"},
		{"change constant string prefix", "package p\nconst prefix = \"new_\"\n", `const ⦃prefix = "old_"⦄`, `const prefix = "new_"`, "package p\nconst prefix = \"new_\"\n"},
		{"swap generic params", "package p\ntype M[K any, V any] struct {}\n", `\[⦃K any⦄, ⦃V any⦄\]`, `[$2, $1]`, "package p\ntype M[V any, K any] struct {}\n"},
		{"variadic to append", "package p\nfunc f() { s = append(s, 1, 2, 3) }\n", `append\(s, ⦃1⦄, ⦃2⦄, ⦃3⦄\)`, `append(s, []int{$1, $2, $3}...)`, "package p\nfunc f() { s = append(s, []int{1, 2, 3}...) }\n"},
		{"rename type alias", "package p\ntype Alias = Old\n", `type ⦃New = Old⦄`, `type Alias = Old`, "package p\ntype Alias = Old\n"},
		{"remove blank identifier", "package p\nfunc f() { _ = work() }\n", `_ = ⦃work\(\)⦄`, `$1`, "package p\nfunc f() { work() }\n"},
		{"wrap block in if", "package p\nfunc f() { { work() } }\n", `⦃\{ ⦃work\(\)⦄ \}⦄`, `if true { $2 }`, "package p\nfunc f() { if true { work() } }\n"},
		{"rename method chaining", "package p\nfunc f() { obj.A().B().C() }\n", `⦃obj\.A\(\)\.B\(\)⦄\.C\(\)`, `$1.X()`, "package p\nfunc f() { obj.A().B().X() }\n"},
		{"change map literalkey", "package p\nvar m = map[string]int{\"k\": 1}\n", `\{⦃"k"⦄: 1\}`, `{"new_k": 1}`, "package p\nvar m = map[string]int{\"new_k\": 1}\n"},
		{"convert type assertion with else", "package p\nfunc f() { if v, ok := x.(int); ok { return v } }\n", `if ⦃v, ok := x\.\(int\)⦄; ok \{ return ⦃v⦄ \}`, `if i, ok := x.(int); ok { return i }`, "package p\nfunc f() { if i, ok := x.(int); ok { return i } }\n"},
		{"simplify slice expression", "package p\nfunc f() { _ = s[0:len(s)] }\n", `⦃s\[0:len\(s\)\]⦄`, `s[:]`, "package p\nfunc f() { _ = s[:] }\n"},
		{"rename channel variable", "package p\nvar msgCh chan int\n", `var ⦃ch⦄ chan int`, `var msgCh chan int`, "package p\nvar msgCh chan int\n"},
		{"add trailing comma to struct", "package p\nvar s = S{F1: 1}\n", `S\{⦃F1: 1⦄\}`, `S{F1: 1,}`, "package p\nvar s = S{F1: 1,}\n"},
		{"rewrite bitwise op", "package p\nfunc f() { x := a &^ b; _ = x }\n", `⦃a &\^ b⦄`, `a & (^b)`, "package p\nfunc f() { x := a & (^b); _ = x }\n"},
		{"rename complex number", "package p\nfunc f() { _ = 1 + 2i }\n", `⦃1 \+ 2i⦄`, `complex(1, 2)`, "package p\nfunc f() { _ = complex(1, 2) }\n"},

		// --- Call expression robustness (nested, greedy match avoidance) ---
		{"call with nested parens", "package p\nfunc f() { log.Printf(\"fmt\", get(\"nested\")) }\n", `⦃log\.Printf(\(.*\))⦄`, `logger.Log$2`, "package p\nfunc f() { logger.Log(\"fmt\", get(\"nested\")) }\n"},
		{"nested call in expression", "package p\nfunc f() { check(log.Printf(\"a\")) }\n", `⦃log\.Printf(\(.*\))⦄`, `logger.Log$2`, "package p\nfunc f() { check(logger.Log(\"a\")) }\n"},
		{"multiple calls same line", "package p\nfunc f() { a(1); b(2) }\n", `⦃a(\(.*\))⦄`, `x$2`, "package p\nfunc f() { x(1); b(2) }\n"},
		{"call with multi-line args", "package p\nfunc f() {\n\tlog.Printf(\n\t\t\"msg\",\n\t\targ,\n\t)\n}\n", `⦃log\.Printf(\(.*\))⦄`, `logger.Log$2`, "package p\nfunc f() {\n\tlogger.Log(\n\t\t\"msg\",\n\t\targ,\n\t)\n}\n"},
		{"greedy match avoidance", "package p\nfunc f() {\n\tlog.Printf(\"a\")\n\tfmt.Println(\"b\")\n}\n", `⦃log\.Printf(\(.*\))⦄`, `Log$2`, "package p\nfunc f() {\n\tLog(\"a\")\n\tfmt.Println(\"b\")\n}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			re, err := Compile(c.pat)
			if err != nil {
				t.Fatalf("%s: compile fail: %v", c.name, err)
			}
			got := re.ReplaceAllString(c.in, c.repl)
			if got != c.want {
				t.Errorf("%s:\n got: %q\nwant: %q", c.name, got, c.want)
			}
		})
	}
}
