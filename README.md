# regast — node-aware `regexp`, natively integrated

`regast` is a drop-in replacement for the Go standard library's `regexp` package that adds **AST node-group** support. It allows you to write regular expressions that understand Go code structure, ignoring insignificant whitespace and comments while honoring lexical token boundaries.

## Casual Examples

### 1. Rename a qualified identifier (ignores spacing)
```go
re := regast.MustCompile(`⦅unix\.Major⦆`)
// Both "unix.Major" and "unix  .  Major" match and are replaced.
out := re.ReplaceAllString(src, `syscall.Major`)
```

### 2. Swap binary operands
```go
re := regast.MustCompile(`⦅(\w+) \+ (\w+)⦆`)
// Matches "a+b", "a + b", "a /* comment */ + b"
// $2 and $3 are the operand captures; $1 is the whole node-group text.
out := re.ReplaceAllString(src, `$3 + $2`)
```

### 3. Wrap a function call (stays inside its node)
```go
re := regast.MustCompile(`⦅log\.Printf(\(.*\))⦆`)
// Because it is wrapped in ⦅…⦆, the (.*) will not greedily 
// skip past the end of the call expression node.
out := re.ReplaceAllString(src, `logger.Log$2`)
```

### 4. Match a specific field in a struct type
```go
re := regast.MustCompile(`⦅Field int⦆`)
out := re.ReplaceAllString(src, `NewField int64`)
```

### 5. Simplify complex error checking
```go
// Replace an error check block with a single-line check function
re := regast.MustCompile(`if ⦅err != nil⦆ { return err }`)
out := re.ReplaceAllString(src, `check($1)`)
```

### 6. Match and rewrite specific comments
```go
// Matches " // TODO: fix" but NOT " /* TODO: fix */"
re := regast.MustCompile(`⦅ // TODO: (\w+)⦆`)
out := re.ReplaceAllString(src, `// FIXED($2)`)
```

## Running Tests

To run all tests for `regast`, including the syntax fork and the node-aware engine:

```powershell
go test ./mohabbat/regast/ ./mohabbat/regast/syntax/
```

For verbose output on node-specific tests:

```powershell
go test -v ./mohabbat/regast/ -run TestNode
```

---

This package is a **vendored copy of Go's `regexp`** (cloned as a subtree),
extended so a pattern may contain **node-groups** — `⦅ … ⦆` — that match a whole
Go AST node with whitespace/comment-insensitive semantics. The proof-of-concept
in `mohabbat/regast-poc` (its executor and golden tests) is the behavioural
contract this re-implementation must satisfy.

Callers use the **exact `regexp` API** (`MustCompile`, `FindStringSubmatchIndex`,
`ReplaceAllString`, …). Plain patterns behave identically to upstream `regexp`.
Patterns containing `⦅ … ⦆` become AST-aware, and the caller never has to know
about ASTs or preprocessing — the engine owns the whole lifecycle of the input
string.

The node logic is integrated **natively**: first-class bytecode opcodes emitted
by a forked `syntax`, executed inside one of the package's existing engines. No
marker runes, no synthetic capture groups, no second external matcher.

---

## What the vendored code actually says

These are the load-bearing facts (verified in the cloned source), and the plan
is built on them.

### A. Pike VM (`exec.go`) dedup is keyed on `pc` alone

`machine.add` deduplicates threads purely by program counter:

```go
if j := q.sparse[pc]; j < uint32(len(q.dense)) && q.dense[j].pc == pc {
    return t   // dropped as a duplicate — key is ONLY pc
}
```

`q.sparse` is a flat array of length `len(prog.Inst)`. Threads recycle through
`m.pool` (zero allocation during matching). `match` drives a **global** `pos`;
`step` only ever re-queues a matched thread at `nextPos = pos+width`. Threads do
**not** own a text coordinate.

**Consequence.** Node-group state is a *dynamic text coordinate* (which node end
am I bound to; how far did I skip). Two facts make this fatal for the Pike VM:

1. **Memory collapse.** To stop `add` merging two threads that share `pc` but
   differ in that coordinate, the dedup key must become `(pc, coord)`. Since
   `coord ∈ [0,n]`, `sparse` must grow to `len(Inst) × (n+1)` — exactly the
   bitstate backtracker's `visited` vector. Expanding the key *is* becoming the
   backtracker, at `O(prog × input)` memory, forfeiting the Pike VM's only
   advantage.
2. **Silent incorrectness.** At one offset, multiple AST nodes can start
   (`foo` and `foo.bar` both start at `foo`). A node-group must fork into the
   inner pattern at the **same** inner-start `pc`, **same** `pos`, with
   **different** end targets. Dedup-by-`pc` drops one ⇒ a valid match vanishes.

So `exec.go` stays **pristine** and serves plain patterns only.

### B. Bitstate backtracker (`backtrack.go`) already owns per-branch `pos` — and already jumps

```go
type job struct { pc uint32; arg bool; pos int }
```

`tryBacktrack` is a DFS over an explicit job stack; **each job carries its own
`pos`**, and the engine already *jumps* it (`InstAltMatch` does `pos = b.end`).
Dedup is `shouldVisit(pc,pos)` over a bitvector sized `len(Inst)*(end+1)` — it is
*already* `O(prog × input)`.

This is the correct native host for node-groups:

- **Per-branch `pos`** — advancing it to skip insignificant bytes, or jumping to
  a node `end`, is local to one DFS branch and corrupts no sibling.
- **Sequential alternatives** — same-start ambiguity becomes "push one job per
  candidate end," explored one timeline at a time. No parallel merge, no dedup
  collision.
- **Nesting needs zero per-thread heap** — the **job stack itself is the nesting
  stack**. An inner node-group pushes jobs carrying the inner `end`; the outer
  `end` survives in the suspended outer jobs already on the stack. We add exactly
  **one scalar** to `job` (`nodeEnd int32`); `b.jobs` is a pooled, pre-grown
  slice, so this allocates nothing per fork. The Pike VM `thread` struct gains
  **nothing**.

### C. One dispatch seam

`func (re *Regexp) find(r, b, s, pos, ncap, dstCap)` (`exec.go`) is the single
chokepoint; every `Find*`/`Replace*`/`Match*` funnels through it
(`replaceAll`, `matches`, `doMatch`). It already chooses between
`doOnePass`, `backtrack`, and the Pike VM `m.match`. We add one more
consideration: **programs containing node opcodes go to the backtracker**.

### D. The `input` interface is the clean seam for the AST table

```go
type input interface { step(pos int) (rune, int); canCheckPrefix() bool; … }
```

We add one method, `astMap() *astMap`, returning the call-local, immutable
boundary table (nil for plain inputs).

---

## Architecture

```
            public API (unchanged signatures: the 16 Find…/Replace… + Match…)
                                     │
            string/[]byte methods preprocess ONCE per call (go/parser)
                                     │  astMap: immutable, call-local ⇒ thread-safe
                                     ▼
        re.replaceAll / re.matches / re.doMatch ── carry astMap ──► re.find
                                     │
                 ┌───────────────────┴───────────────────┐
        prog.HasNodeGroup == false              prog.HasNodeGroup == true
                 │                                        │
   stock doOnePass / backtrack / Pike VM      re.backtrack (extended, cap lifted)
        (UNTOUCHED, plain patterns)            executes InstNodeGroup/Exit/Gap
                                               natively over job.pos + astMap
```

- **Plain patterns**: stock dispatch, byte-for-byte upstream behaviour.
- **Node-group patterns**: always routed to the **extended bitstate
  backtracker**, with the `maxBitStateLen` size cap lifted (node matching is
  inherently `O(prog × input)`; source files are bounded). The Pike VM is never
  asked to run them.

---

## Component plan (substantiated per file)

### 1. Fork `syntax/` and emit real opcodes

We now depend on the **local** `syntax/` (re-point the vendored `regexp`'s
import from stdlib `regexp/syntax` to `…/regast/syntax`). We only **add**
opcodes, so every existing `syntax` test stays green.

- **`syntax/regexp.go`**: add `OpNodeGroup`, `OpGap` to `Op`.
- **`syntax/prog.go`**: add `InstNodeGroup`, `InstNodeExit`, `InstGap` to
  `InstOp` and `instOpNames`; add `Prog.HasNodeGroup bool`.
- **`syntax/parse.go`**: scan `⦅` and `⦆` as native operators producing an
  `OpNodeGroup` subtree; a run of whitespace **inside** a node-group becomes an
  `OpGap` node (outside a node-group, whitespace is literal as today). Bracket
  scanning is escape- and char-class-aware. **Comment nodes** are identified by
  peeking for `//` or `/*` immediately after `⦅`.
- **`syntax/compile.go`**: compile `OpNodeGroup` to
  `InstNodeGroup{Arg: flags, Out: continuation pc}` where `Arg` carries the
  node kind (Code/LineComment/BlockComment); terminated by an `InstNodeExit`.
  Compile `OpGap` to `InstGap`. Set `Prog.HasNodeGroup`.
  - If `$1 == node text` is wanted, the group is assigned a **real `Cap` index**
    in normal left-to-right order. Because these are first-class instructions and
    the only captures are real ones, submatch indices need **zero** correction —
    this is the concrete fix to the index-corruption objection.

### 2. Source preprocessing (`source.go`)

Built from a **single up-front pass** that harvests *both* structures the Go
front end already produces — no second tokenizer, no raw-character heuristics:

- A **Unified Significant-Token Map**, from `go/scanner` (the same pass that
  finds comments): the byte spans `[start,end)` of every *significant* lexical
  token — identifiers, keywords, literals, **and** punctuation/operators
  (`+`, `.`, `(`, `;`, …) — while **discarding insignificant debris**
  (whitespace, comments, and synthetic auto-inserted semicolons whose literal is
  `"\n"`). Token end is `start + len(lit)` for literal tokens, else
  `start + len(tok.String())` for operators/delimiters.
- The **AST node table**, from `go/parser` (`ParseComments | AllErrors |
  SkipObjectResolution`): byte spans of every AST node, for resolving
  node-groups. Error-tolerant; partial AST is used; only a wholly unusable parse
  errors out.

Each entry in the table is tagged with `nodeFlags` (Code, LineComment, or
BlockComment), allowing node-groups to distinguish between code and specific
comment types while residing in a single, non-redundant data structure.

`type astMap struct { … }` holds both tables (flat, sorted, binary-searchable).
Methods used by the engine:
  - `endsForStartKind(pos, kind) []int` — candidate AST node ends starting
    exactly at `pos` with matching kind (tried largest-first).
  - `significant(i) bool` — byte `i` lies inside a significant token.
  - `nextSignificant(pos) int` — advance over insignificant debris (WS + comments).
  - `nextNonCommentWhitespace(pos) int` — advance over raw whitespace only (retains comments).
  - `gapBoundary(L, R int) bool` — the gap invariant (below).

---

### 2a. Comment node-groups

Patterns can target specific comment types using the `⦅ // … ⦆` and `⦅ /* … */ ⦆`
syntax. These are **strict lexical matches**:

1. **One-to-one mapping**: Each comment node-group must match exactly one
   comment as identified by the Go parser. It cannot span multiple comments
   or match a subset of a single comment.
2. **Kind isolation**: A line comment group (`//`) will never match a block
   comment (`/* */`) and vice versa.
3. **Context-aware snapping**: While code node-groups skip whitespace and
   comments to find the next token, comment node-groups **snap to row whitespace
   only**. This prevents a comment match from accidentally "consuming" adjacent
   code tokens due to boundary alignment.
4. **Internal whitespace**: Insignificant whitespace *inside* the comment guards
   (e.g. between `//` and the first word) is handled via the Token-Stream 
   Boundary Rule, allowing `⦅ // TODO ⦆` to match `//    TODO`.

---

### 3. The gap rule as a native instruction — the Token-Stream Boundary Rule

A pattern gap is a **single, rigid invariant**: it is valid *iff the text cursor
sits exactly on a seam between two distinct significant tokens.* This unifies AST
leaves and punctuation under one language-precise timeline, so it is correct
around operators (where AST nodes don't exist) yet still forbids splitting a
single token.

`InstGap` at branch-local `pos`:

```
R := astMap.nextSignificant(pos)        // swallow ws/comments/debris forward
if !astMap.gapBoundary(pos-1, R)        // are L and R the same token? (mid-token)
    → this DFS branch dies (continue)
pos = R; pc = inst.Out                  // crisp token seam; advance
```

`gapBoundary(L, R)` is true iff `L` and `R` fall in **different** significant
tokens (`tokenIndexAt(L) != tokenIndexAt(R)`). Worked cases:
- `x int`: `Ident x` | `Ident int` — distinct tokens ⇒ **valid**.
- `a + b`: `Ident a` | `token.ADD "+"` | `Ident b` — distinct tokens ⇒ **valid**.
- `va|riable`: both sides inside one `Ident` ⇒ same token ⇒ **invalid**, backtrack.

For in-node `InstRune`, before `i.step(pos)` the branch advances `pos` over
insignificant bytes and checks `pos < job.nodeEnd`. (Legal: the job owns `pos`.)

### 4. Extend `backtrack.go`

- `InstNodeGroup` at `pos`: `ends := astMap.endsForStartKind(pos, inst.Arg)`; 
  empty ⇒ branch dies. Else push one job per `end` (largest-first), 
  `pc = inst.Sub`, `nodeEnd = end`, `isComment = (kind != NodeCode)`.
- `InstNodeExit`: succeed iff `snap(pos) == job.nodeEnd`; then continue
  at `inst.Out` with the parent frame (already on the stack). `snap` is 
  `nextSignificant` for code and `nextNonCommentWhitespace` for comments.
- `InstGap`: as in §3.
- `shouldVisit(pc,pos)` collides with node bounds (same `(pc,pos)` under
  different `nodeEnd`). Inside node regions, **disable the visited prune** and
  rely on the fixed, properly-nested AST bounds plus a **step cap** for
  termination — exactly the POC's proven model. Plain-region pruning is
  unchanged.
- Add `nodeEnd int32` and `isComment bool` to `job`. No heap-per-fork.

### 5. Dispatch + `input` seam (`exec.go`, `regexp.go`)

- Add `astMap()` to the `input` interface; `inputBytes`/`inputString` return nil,
  a new `inputNode` (string + `*astMap`) returns the table.
- `find`: when `re.prog.HasNodeGroup`, build the `inputNode`, call
  `re.backtrack` directly, and **bypass the `maxBitStateLen` cap**. Otherwise the
  existing dispatch, verbatim.
- Public **string/[]byte** methods preprocess once when `re.prog.HasNodeGroup`
  and thread the `astMap` down via `find`. **Reader** methods can't AST-parse a
  stream ⇒ they reject node-group patterns (documented).
- Preprocess happens once per **public call**, never inside the per-position
  loop.

### 6. `compile` hook (`regexp.go`)

- `compile()` calls the forked `syntax.Parse`; `Prog.HasNodeGroup` flows onto
  `Regexp`. No marker lowering anywhere.

### 7. Package identity

- Rename `package regexp` → `package regast` (and `_test` packages). Pure clause
  rename; tests keep compiling. Import path `mohabbat/mohabbat/regast`; callers
  use `regast.MustCompile`.

---

## Zero-allocation accounting (explicit)

- **Pike VM (`exec.go`) `thread`**: unchanged — no new fields; still pooled and
  alloc-free on the plain path.
- **Backtracker (`backtrack.go`) `job`**: +1 scalar (`nodeEnd int32`). `b.jobs`
  is a pooled, pre-grown slice; pushing candidate-end jobs is amortized O(1) with
  no per-fork heap allocation and no `[]int` payload.
- **Nesting**: carried by the job stack, not by any thread payload.

---

## Testing strategy (the contract)

Three layers, all must pass:

1. **Upstream `regexp` tests — kept, unchanged.** `all_test.go`, `find_test.go`,
   `exec_test.go`, `exec2_test.go`, `onepass_test.go`, `example_test.go`,
   `testdata/`. The regression wall proving the plain path is byte-for-byte
   stdlib. (Survives the package rename untouched.)
2. **`syntax/` tests — kept, unchanged.** We only add opcodes, so they pass by
   construction; they guard the forked parser/compiler.
3. **Golden table — ported from `regast-poc`, re-expressed against the public
   API** (`regast.MustCompile(pat).ReplaceAllString(in,repl) == want`). This is
   the primary evidence node-awareness lives entirely inside the engine. Carry
   over `TestGolden`, `TestGapWordRule`, and the structural tests (no-spill,
   nested groups, same-start, CRLF, partial-parse, capture fidelity), plus
   plain-pattern **parity** cases cross-checked against the standard library.

Acceptance bar: **every** POC golden/word-rule/structural case passes against the
new API, and **no** upstream test regresses.

---

## Build order (with verification gates)

1. **Rename + build.** Rename package; vendored package builds in the mohabbat
   module; **upstream suite green**. *Gate.*
2. **Fork `syntax`.** Re-point the import to local `syntax`; add `OpNodeGroup`,
   `OpGap`, `InstNodeGroup`, `InstNodeExit`, `InstGap`, `Prog.HasNodeGroup` as
   inert additions. *Gate: `syntax` + `regexp` suites green.*
3. **Parser.** `⦅ … ⦆` and in-node whitespace produce `OpNodeGroup`/`OpGap`;
   plain patterns parse identically. *Gate: stock suites green; AST dumps for new
   ops match expectations.*
4. **Compiler.** Emit the new instructions; set `HasNodeGroup`; correct, real
   capture numbering. *Gate: a node pattern compiles; a plain pattern's prog is
   unchanged.*
5. **`source.go`.** Port preprocessing; unit-test `endsForStart`,
   `significant`/`nextSignificant`, `hasStructuralDivergence`. *Gate: unit tests.*
6. **`input` seam + dispatch.** `astMap()`, `inputNode`, `find` routing + cap
   bypass for node programs. *Gate: stock suites green (plain path untouched).*
7. **Backtracker execution.** Implement `InstNodeGroup`/`InstNodeExit`/`InstGap`,
   in-node skipping, visited-prune disabled in node regions, step cap. *Gate: the
   full ported golden table passes.*
8. **Parity sweep** against stdlib `regexp` for node-free patterns. *Gate.*
9. **Wire `mohabbat` callers** (`patch_regast.go`) to the new API; retire the POC
   once trusted. *Gate: a real JIT build succeeds.*

---

## Risks & limitations (tracked deliberately)

- **Node-group patterns forfeit the linear-time guarantee** — they run on bounded
  backtracking with a step cap. Intrinsic: same-start AST ambiguity is not a
  regular-language operation. Plain patterns keep every guarantee.
- **`O(prog × input)` memory for node programs** (visited vector / lifted cap) —
  the unavoidable price; bounded by source-file size.
- **Reader/stream inputs** can't be AST-parsed without a full buffer ⇒ node
  patterns rejected there; plain patterns unaffected.
- **Forking `syntax`** re-points the vendored `regexp` import; the `syntax` test
  suite is the regression wall (additions only).
- **Termination inside node regions** relies on fixed AST bounds + step cap
  rather than the `(pc,pos)` visited prune; the cap must be generous but finite.

