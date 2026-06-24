// Copyright 2026 The regast Authors.
// Node-aware extension to the vendored Go regexp engine.

package regast

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"sort"
)

// astMap is the call-local, immutable structural map of one input source string.
// It carries two flat, sorted, binary-searchable tables:
//
//   - a Unified Significant-Token Map: the byte spans of every meaningful
//     lexical token (identifiers, keywords, literals, and punctuation/operators),
//     with whitespace, comments, and synthetic auto-semicolons discarded; and
//   - the AST node table: the byte spans of every AST node, for resolving
//     node-groups.
//
// Both come from a single up-front pass over the source.
type astMap struct {
	src []byte

	// significant token spans, sorted by start, non-overlapping
	tokStart []int
	tokEnd   []int

	// AST node spans, sorted by (start, end); nodeFlags: 0=code, 1=// comment, 2=/* comment
	nodeStart []int
	nodeEnd   []int
	nodeFlags []byte
}

// newASTMap scans and parses src once, building both tables. Parsing is
// error-tolerant: a best-effort partial AST is used; only a wholly unusable
// parse returns an error.
func newASTMap(filename string, src []byte) (*astMap, error) {
	m := &astMap{src: src}

	// Pass 1: the significant-token map (also yields comments, which we drop).
	fset := token.NewFileSet()
	file := fset.AddFile(filename, fset.Base(), len(src))
	var sc scanner.Scanner
	sc.Init(file, src, nil, scanner.ScanComments)
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		// Insignificant debris: comments, illegal runes, and auto-inserted
		// semicolons (their literal is "\n"). Explicit ';' (lit == ";") is kept.
		if tok == token.COMMENT || tok == token.ILLEGAL {
			continue
		}
		if tok == token.SEMICOLON && lit != ";" {
			continue
		}
		start := fset.Position(pos).Offset
		n := len(lit)
		if n == 0 {
			n = len(tok.String()) // operators/delimiters carry no literal
		}
		if n <= 0 {
			continue
		}
		end := start + n
		if end > len(src) {
			end = len(src)
		}
		if end <= start {
			continue
		}
		m.tokStart = append(m.tokStart, start)
		m.tokEnd = append(m.tokEnd, end)
	}

	// Pass 2: the AST node table.
	pfset := token.NewFileSet()
	f, err := parser.ParseFile(pfset, filename, src,
		parser.ParseComments|parser.AllErrors|parser.SkipObjectResolution)
	if f == nil {
		return nil, fmt.Errorf("regast: cannot parse %s: %w", filename, err)
	}

	type se struct {
		s, e int
		flag byte
	}
	var nodes []se
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if !n.Pos().IsValid() || !n.End().IsValid() {
			return true
		}
		st := pfset.Position(n.Pos()).Offset
		en := pfset.Position(n.End()).Offset
		if en > st && st >= 0 && en <= len(src) {
			var flag byte
			if c, ok := n.(*ast.Comment); ok {
				if len(c.Text) >= 2 && c.Text[0] == '/' {
					if c.Text[1] == '/' {
						flag = 1
					} else if c.Text[1] == '*' {
						flag = 2
					}
				}
			}
			nodes = append(nodes, se{st, en, flag})
		}
		return true
	})
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].s != nodes[j].s {
			return nodes[i].s < nodes[j].s
		}
		return nodes[i].e < nodes[j].e
	})
	for _, nd := range nodes {
		m.nodeStart = append(m.nodeStart, nd.s)
		m.nodeEnd = append(m.nodeEnd, nd.e)
		m.nodeFlags = append(m.nodeFlags, nd.flag)
	}
	return m, nil
}

// tokenIndexAt returns the index of the significant token containing byte i, or
// -1 if i lies in insignificant debris (or out of range).
func (m *astMap) tokenIndexAt(i int) int {
	if i < 0 || i >= len(m.src) {
		return -1
	}
	// last token whose start <= i
	lo, hi := 0, len(m.tokStart)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if m.tokStart[mid] <= i {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo > 0 && i < m.tokEnd[lo-1] {
		return lo - 1
	}
	return -1
}

// significant reports whether byte i lies inside a significant token.
func (m *astMap) significant(i int) bool {
	return m.tokenIndexAt(i) >= 0
}

// insignificant reports whether byte i is real input that lies outside every
// significant token — i.e. whitespace, a comment, or an auto-inserted
// semicolon. These are the freely skippable "Insignificant Text Spans": the
// per-character backtracker may step over any one of them. End-of-text is not
// insignificant (there is no byte there to skip).
func (m *astMap) insignificant(i int) bool {
	return i >= 0 && i < len(m.src) && m.tokenIndexAt(i) < 0
}

// nextSignificant advances pos forward over insignificant debris and returns
// the first significant byte index (or len(src) if none remains).
func (m *astMap) nextSignificant(pos int) int {
	if pos < 0 {
		pos = 0
	}
	for pos < len(m.src) && m.tokenIndexAt(pos) < 0 {
		pos++
	}
	return pos
}

// prevSignificantEnd returns the largest q <= pos such that the byte at q-1 is
// significant (i.e. trailing insignificant bytes before pos are trimmed). Used
// to snap a capture-close boundary back onto the token stream.
func (m *astMap) prevSignificantEnd(pos int) int {
	if pos > len(m.src) {
		pos = len(m.src)
	}
	for pos > 0 && m.tokenIndexAt(pos-1) < 0 {
		pos--
	}
	return pos
}

// nextNonCommentWhitespace advances pos past whitespace-only bytes (space, tab,
// newline, CR). Used for capture-open snapping inside comment node-groups.
func (m *astMap) nextNonCommentWhitespace(pos int) int {
	for pos < len(m.src) {
		b := m.src[pos]
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			pos++
		} else {
			break
		}
	}
	return pos
}

// prevNonCommentWhitespaceEnd returns the largest q <= pos such that src[q-1]
// is not whitespace. Used for capture-close snapping inside comment node-groups.
func (m *astMap) prevNonCommentWhitespaceEnd(pos int) int {
	if pos > len(m.src) {
		pos = len(m.src)
	}
	for pos > 0 {
		b := m.src[pos-1]
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			pos--
		} else {
			break
		}
	}
	return pos
}

// gapBoundary implements the Token-Stream Boundary Rule: it reports whether L
// and R fall in *different* significant tokens, i.e. the cursor between them is
// a crisp token seam rather than the middle of one token.
func (m *astMap) gapBoundary(l, r int) bool {
	tl := m.tokenIndexAt(l)
	tr := m.tokenIndexAt(r)
	if tl < 0 || tr < 0 {
		return false
	}
	return tl != tr
}

// astMapFor builds the structural map for a node-group match over b or s,
// returning nil when the regexp has no node-groups or the source cannot be
// parsed at all (in which case node-groups simply find no matches).
func (re *Regexp) astMapFor(b []byte, s string) *astMap {
	if !re.prog.HasNodeGroup {
		return nil
	}
	src := b
	if src == nil {
		src = []byte(s)
	}
	am, err := newASTMap("", src)
	if err != nil {
		return nil
	}
	return am
}

// endsForStart returns the end offsets of all code (non-comment) AST nodes
// beginning exactly at pos, ascending and de-duplicated.
func (m *astMap) endsForStart(pos int) []int {
	return m.endsForStartKind(pos, 0)
}

// endsForStartKind returns the end offsets of AST nodes with the specified kind
// that begin exactly at pos, ascending and de-duplicated.
// kind: 0=code node, 1=// line comment, 2=/* */ block comment.
func (m *astMap) endsForStartKind(pos int, kind byte) []int {
	lo, hi := 0, len(m.nodeStart)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if m.nodeStart[mid] < pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	var ends []int
	for lo < len(m.nodeStart) && m.nodeStart[lo] == pos {
		if m.nodeFlags[lo] == kind {
			e := m.nodeEnd[lo]
			if len(ends) == 0 || ends[len(ends)-1] != e {
				ends = append(ends, e)
			}
		}
		lo++
	}
	return ends
}
