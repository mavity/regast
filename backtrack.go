// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// backtrack is a regular expression search with submatch
// tracking for small regular expressions and texts. It allocates
// a bit vector with (length of input) * (length of prog) bits,
// to make sure it never explores the same (character position, instruction)
// state multiple times. This limits the search to run in time linear in
// the length of the test.
//
// backtrack is a fast replacement for the NFA code on small
// regexps when onepass cannot be used.

package regast

import (
	"mohabbat/mohabbat/regast/syntax"
	"sync"
)

// A job is an entry on the backtracker's job stack. It holds
// the instruction pc and the position in the input.
type job struct {
	pc  uint32
	arg bool
	pos int
}

type nodeJob struct {
	pc   uint32
	arg  bool
	pos  int
	nctx nodeCtx
}

// nodeFrame is one level of node-group nesting: the bounding AST node end and
// the index of the parent frame (-1 if none). Frames live in a per-search arena
// (bitState.frames) and are interned, so identical chains share one index. This
// gives unbounded nesting depth with no per-fork heap allocation, and lets the
// visited prune key on the exact bound chain (not just the innermost end).
type nodeFrame struct {
	end    int32
	parent int32
}

// nodeCtx is the per-job node-matching cursor. frame < 0 means "not inside any
// node-group" (but per-character whitespace/comment skipping still applies when
// the input has a structural map). It is a small scalar value copied with the
// job (no array, no heap).
//
// started becomes true once the match has consumed its first significant byte.
// While it is false the engine refuses to skip leading debris (so a match never
// begins on whitespace/comment bytes) and the first significant byte is exempt
// from the token-seam rule. gapOK records that an explicit separator — a pattern
// gap (InstGap) or a literally consumed debris byte — has been seen since the
// last significant byte, which is what licenses crossing a word|word seam.
type nodeCtx struct {
	frame     int32
	started   bool
	gapOK     bool
	isComment uint8 // 0=code, 1=// line comment, 2=/* */ block comment
}

func noNode() nodeCtx { return nodeCtx{frame: -1} }

// isCommentWhitespace reports whether b is a whitespace byte for matching inside
// a comment node-group (space, tab, newline, carriage return).
func isCommentWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// isWordByte reports whether c is an ASCII identifier byte.
func isWordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// frameEnd returns the bounding node end of a frame (or +inf for frame < 0).
func (b *bitState) frameEnd(frame int32) int {
	if frame < 0 {
		return 1<<31 - 1
	}
	return int(b.frames[frame].end)
}

// internFrame returns the canonical index of the frame {end, parent}, creating
// it in the arena on first sight. Interning bounds the search: identical chains
// collapse to one index, so the visited prune can deduplicate them.
func (b *bitState) internFrame(end, parent int32) int32 {
	key := uint64(uint32(end))<<32 | uint64(uint32(parent))
	if id, ok := b.frameIntern[key]; ok {
		return id
	}
	id := int32(len(b.frames))
	b.frames = append(b.frames, nodeFrame{end: end, parent: parent})
	b.frameIntern[key] = id
	return id
}

// seamOK enforces the Token-Stream Boundary Rule when about to consume the
// significant byte at pos: two distinct word tokens may not be fused together
// unless an explicit separator licensed it. The first significant byte of a
// match, and any byte reached right after a gap or a consumed debris byte, is
// always allowed; only an unmediated word|word seam is rejected.
func (b *bitState) seamOK(pos int, nctx nodeCtx) bool {
	if !nctx.started || nctx.gapOK {
		return true
	}
	q := b.am.prevSignificantEnd(pos)
	if q == 0 {
		return true
	}
	if b.am.tokenIndexAt(q-1) == b.am.tokenIndexAt(pos) {
		return true // same token: contiguous bytes, no seam
	}
	if isWordByte(b.am.src[q-1]) && isWordByte(b.am.src[pos]) {
		return false
	}
	return true
}

// consumed updates the cursor after a rune is matched at pos. Consuming a
// significant byte marks the match as started and clears the gap credit;
// consuming a debris byte literally counts as an explicit separator.
func (b *bitState) consumed(pos int, nctx nodeCtx) nodeCtx {
	if nctx.isComment != 0 {
		// Inside a comment node-group: only whitespace is "insignificant".
		if pos < len(b.am.src) && isCommentWhitespace(b.am.src[pos]) {
			nctx.gapOK = true
			return nctx
		}
		nctx.started = true
		nctx.gapOK = false
		return nctx
	}
	if b.am.insignificant(pos) {
		nctx.gapOK = true
		return nctx
	}
	nctx.started = true
	nctx.gapOK = false
	return nctx
}

const (
	visitedBits        = 32
	maxBacktrackProg   = 500        // len(prog.Inst) <= max
	maxBacktrackVector = 256 * 1024 // bit vector size <= max (bits)
)

// nodeVisitKey uniquely identifies a node-region execution state for the
// deduplicating prune: instruction, position, the exact bound chain (frame),
// and the two transient cursor flags.
type nodeVisitKey struct {
	pc    uint32
	pos   int32
	frame int32
	fl    uint8
}

// bitState holds state for the backtracker.
type bitState struct {
	end      int
	cap      []int
	matchcap []int
	jobs     []job
	nodeJobs []nodeJob
	visited  []uint32

	// regast node-group extension:
	am          *astMap                   // structural map of the input (nil for plain matching)
	frames      []nodeFrame               // per-search arena of node-bound frames
	frameIntern map[uint64]int32          // canonicalize {end,parent} -> frame index
	nodeVisited map[nodeVisitKey]struct{} // dedup of node-region states (replaces the bitvector there)
	steps       int                       // per-start work counter
	maxSteps    int                       // per-start backstop against adversarial exponential nesting

	inputs inputs
}

var bitStatePool sync.Pool

func newBitState() *bitState {
	b, ok := bitStatePool.Get().(*bitState)
	if !ok {
		b = new(bitState)
	}
	return b
}

func freeBitState(b *bitState) {
	b.inputs.clear()
	b.am = nil
	bitStatePool.Put(b)
}

// maxBitStateLen returns the maximum length of a string to search with
// the backtracker using prog.
func maxBitStateLen(prog *syntax.Prog) int {
	if !shouldBacktrack(prog) {
		return 0
	}
	return maxBacktrackVector / len(prog.Inst)
}

// shouldBacktrack reports whether the program is too
// long for the backtracker to run.
func shouldBacktrack(prog *syntax.Prog) bool {
	return len(prog.Inst) <= maxBacktrackProg
}

// reset resets the state of the backtracker.
// end is the end position in the input.
// ncap is the number of captures.
func (b *bitState) reset(prog *syntax.Prog, end int, ncap int) {
	b.end = end

	if prog.HasNodeGroup {
		if cap(b.nodeJobs) == 0 {
			b.nodeJobs = make([]nodeJob, 0, 256)
		} else {
			b.nodeJobs = b.nodeJobs[:0]
		}
	} else if cap(b.jobs) == 0 {
		b.jobs = make([]job, 0, 256)
	} else {
		b.jobs = b.jobs[:0]
	}

	if prog.HasNodeGroup {
		b.visited = b.visited[:0]
	} else {
		visitedSize := (len(prog.Inst)*(end+1) + visitedBits - 1) / visitedBits
		if cap(b.visited) < visitedSize {
			b.visited = make([]uint32, visitedSize, maxBacktrackVector/visitedBits)
		} else {
			b.visited = b.visited[:visitedSize]
			clear(b.visited)
		}
	}

	if cap(b.cap) < ncap {
		b.cap = make([]int, ncap)
	} else {
		b.cap = b.cap[:ncap]
	}
	for i := range b.cap {
		b.cap[i] = -1
	}

	if cap(b.matchcap) < ncap {
		b.matchcap = make([]int, ncap)
	} else {
		b.matchcap = b.matchcap[:ncap]
	}
	for i := range b.matchcap {
		b.matchcap[i] = -1
	}

	if prog.HasNodeGroup {
		b.frames = b.frames[:0]
		if b.frameIntern == nil {
			b.frameIntern = make(map[uint64]int32)
		} else {
			clear(b.frameIntern)
		}
		if b.nodeVisited == nil {
			b.nodeVisited = make(map[nodeVisitKey]struct{})
		} else {
			clear(b.nodeVisited)
		}
	}
}

// shouldVisit reports whether the combination of (pc, pos) has not
// been visited yet.
func (b *bitState) shouldVisit(pc uint32, pos int) bool {
	n := uint(int(pc)*(b.end+1) + pos)
	if b.visited[n/visitedBits]&(1<<(n&(visitedBits-1))) != 0 {
		return false
	}
	b.visited[n/visitedBits] |= 1 << (n & (visitedBits - 1))
	return true
}

// shouldVisitNode reports whether a node-region state (pc, pos, exact bound
// chain, cursor flags) has not been seen yet, recording it if new. Because the
// frame index encodes the whole nesting chain, this prunes duplicate branches
// correctly and guarantees termination without a step cap.
func (b *bitState) shouldVisitNode(pc uint32, pos int, nctx nodeCtx) bool {
	var fl uint8
	if nctx.started {
		fl |= 1
	}
	if nctx.gapOK {
		fl |= 2
	}
	fl |= (nctx.isComment & 3) << 2
	k := nodeVisitKey{pc: pc, pos: int32(pos), frame: nctx.frame, fl: fl}
	if _, ok := b.nodeVisited[k]; ok {
		return false
	}
	b.nodeVisited[k] = struct{}{}
	return true
}

// push pushes an AST-aware job if it has not already been visited.
func (b *bitState) push(re *Regexp, pc uint32, pos int, arg bool, nctx nodeCtx) {
	if re.prog.Inst[pc].Op == syntax.InstFail {
		return
	}
	if !arg && !b.shouldVisitNode(pc, pos, nctx) {
		return
	}
	b.nodeJobs = append(b.nodeJobs, nodeJob{pc: pc, arg: arg, pos: pos, nctx: nctx})
}

func isPatternSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

// tryBacktrack runs the plain or AST-aware backtracking path.
func (re *Regexp) tryBacktrack(b *bitState, i input, pc uint32, pos int) bool {
	if b.am != nil {
		return re.tryBacktrackNode(b, i, pc, pos)
	}
	return re.tryBacktrackPlain(b, i, pc, pos)
}

func (b *bitState) pushPlain(re *Regexp, pc uint32, pos int, arg bool) {
	if re.prog.Inst[pc].Op != syntax.InstFail && (arg || b.shouldVisit(pc, pos)) {
		b.jobs = append(b.jobs, job{pc: pc, arg: arg, pos: pos})
	}
}

func (re *Regexp) tryBacktrackPlain(b *bitState, i input, pc uint32, pos int) bool {
	longest := re.longest

	b.pushPlain(re, pc, pos, false)
	for len(b.jobs) > 0 {
		l := len(b.jobs) - 1
		pc := b.jobs[l].pc
		pos := b.jobs[l].pos
		arg := b.jobs[l].arg
		b.jobs = b.jobs[:l]

		goto Skip
	CheckAndLoop:
		if !b.shouldVisit(pc, pos) {
			continue
		}
	Skip:

		inst := &re.prog.Inst[pc]
		switch inst.Op {
		default:
			panic("bad inst")
		case syntax.InstFail:
			panic("unexpected InstFail")
		case syntax.InstAlt:
			if arg {
				arg = false
				pc = inst.Arg
				goto CheckAndLoop
			}
			b.pushPlain(re, pc, pos, true)
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstAltMatch:
			switch re.prog.Inst[inst.Out].Op {
			case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
				b.pushPlain(re, inst.Arg, pos, false)
				pc = inst.Arg
				pos = b.end
				goto CheckAndLoop
			}
			b.pushPlain(re, inst.Out, b.end, false)
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstRune:
			r, width := i.step(pos)
			if !inst.MatchRune(r) {
				continue
			}
			pos += width
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstRune1:
			r, width := i.step(pos)
			if r != inst.Rune[0] {
				continue
			}
			pos += width
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstRuneAnyNotNL:
			r, width := i.step(pos)
			if r == '\n' || r == endOfText {
				continue
			}
			pos += width
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstRuneAny:
			r, width := i.step(pos)
			if r == endOfText {
				continue
			}
			pos += width
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstCapture:
			if arg {
				b.cap[inst.Arg] = pos
				continue
			}
			if inst.Arg < uint32(len(b.cap)) {
				b.pushPlain(re, pc, b.cap[inst.Arg], true)
				b.cap[inst.Arg] = pos
			}
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstEmptyWidth:
			flag := i.context(pos)
			if !flag.match(syntax.EmptyOp(inst.Arg)) {
				continue
			}
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstNop:
			pc = inst.Out
			goto CheckAndLoop
		case syntax.InstMatch:
			if len(b.cap) == 0 {
				return true
			}
			if len(b.cap) > 1 {
				b.cap[1] = pos
			}
			if old := b.matchcap[1]; old == -1 || (longest && pos > 0 && pos > old) {
				copy(b.matchcap, b.cap)
			}
			if !longest || pos == b.end {
				return true
			}
		}
	}

	return longest && len(b.matchcap) > 1 && b.matchcap[1] >= 0
}

func (re *Regexp) tryBacktrackNode(b *bitState, i input, pc uint32, pos int) bool {
	longest := re.longest

	// Per-start work counter. Frame-keyed dedup makes realistic node patterns
	// polynomial, so this is only a backstop: deeply-nested same-start
	// node-groups are inherently exponential (back-to-back ⦅ opens multiply
	// candidate ends before any consume narrows them), and without a cap such
	// adversarial patterns would hang. It is reset per start, so it bounds work
	// per start position rather than across the whole (linear) start sweep.
	b.steps = 0

	b.push(re, pc, pos, false, noNode())
	for len(b.nodeJobs) > 0 {
		l := len(b.nodeJobs) - 1
		// Pop job off the stack.
		pc := b.nodeJobs[l].pc
		pos := b.nodeJobs[l].pos
		arg := b.nodeJobs[l].arg
		nctx := b.nodeJobs[l].nctx
		b.nodeJobs = b.nodeJobs[:l]

		if b.maxSteps > 0 {
			b.steps++
			if b.steps > b.maxSteps {
				return false
			}
		}

		// Optimization: rather than push and pop,
		// code that is going to Push and continue
		// the loop simply updates ip, p, and arg
		// and jumps to CheckAndLoop. We have to
		// do the ShouldVisit check that Push
		// would have, but we avoid the stack
		// manipulation.
		goto Skip
	CheckAndLoop:
		// Plain programs use the flat (pc,pos) bitvector; structural programs
		// (b.am != nil) carry per-character cursor flags, so they dedup through
		// the frame-keyed node-visited set. Both terminate without a step cap.
		if b.am != nil {
			if !b.shouldVisitNode(pc, pos, nctx) {
				continue
			}
		} else if !b.shouldVisit(pc, pos) {
			continue
		}
	Skip:

		inst := &re.prog.Inst[pc]

		switch inst.Op {
		default:
			panic("bad inst")
		case syntax.InstFail:
			panic("unexpected InstFail")
		case syntax.InstAlt:
			// Cannot just
			//   b.push(inst.Out, pos, false)
			//   b.push(inst.Arg, pos, false)
			// If during the processing of inst.Out, we encounter
			// inst.Arg via another path, we want to process it then.
			// Pushing it here will inhibit that. Instead, re-push
			// inst with arg==true as a reminder to push inst.Arg out
			// later.
			if arg {
				// Finished inst.Out; try inst.Arg.
				arg = false
				pc = inst.Arg
				goto CheckAndLoop
			} else {
				b.push(re, pc, pos, true, nctx)
				pc = inst.Out
				goto CheckAndLoop
			}

		case syntax.InstAltMatch:
			// One opcode consumes runes; the other leads to match.
			switch re.prog.Inst[inst.Out].Op {
			case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
				// inst.Arg is the match.
				b.push(re, inst.Arg, pos, false, nctx)
				pc = inst.Arg
				pos = b.end
				goto CheckAndLoop
			}
			// inst.Out is the match - non-greedy
			b.push(re, inst.Out, b.end, false, nctx)
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstRune:
			if b.am != nil {
				if pos >= b.frameEnd(nctx.frame) {
					continue
				}
				if nctx.isComment != 0 {
					// Inside comment: skip only whitespace; no seam check.
					if len(inst.Rune) == 2 && inst.Rune[0] == inst.Rune[1] && isPatternSpace(inst.Rune[0]) {
						b.push(re, inst.Out, pos, false, nctx)
					}
					if nctx.started && pos < b.end && isCommentWhitespace(b.am.src[pos]) {
						b.push(re, pc, pos+1, false, nctx) // Branch B: skip source whitespace
					}
				} else {
					// Code context: skip insignificant bytes with seam check.
					if len(inst.Rune) == 2 && inst.Rune[0] == inst.Rune[1] && isPatternSpace(inst.Rune[0]) {
						b.push(re, inst.Out, pos, false, nctx)
					}
					if b.am.insignificant(pos) {
						if nctx.started {
							b.push(re, pc, pos+1, false, nctx) // Branch B: skip one debris byte
						}
					} else if !b.seamOK(pos, nctx) {
						continue
					}
				}
			}
			r, width := i.step(pos)
			if !inst.MatchRune(r) {
				continue
			}
			if b.am != nil {
				nctx = b.consumed(pos, nctx)
			}
			pos += width
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstRune1:
			if b.am != nil {
				if pos >= b.frameEnd(nctx.frame) {
					continue
				}
				if nctx.isComment != 0 {
					// Inside comment: skip only whitespace; no seam check.
					if isPatternSpace(inst.Rune[0]) {
						b.push(re, inst.Out, pos, false, nctx)
					}
					if nctx.started && pos < b.end && isCommentWhitespace(b.am.src[pos]) {
						b.push(re, pc, pos+1, false, nctx) // Branch B: skip source whitespace
					}
				} else {
					// Code context: skip insignificant bytes with seam check.
					if isPatternSpace(inst.Rune[0]) {
						b.push(re, inst.Out, pos, false, nctx)
					}
					if b.am.insignificant(pos) {
						if nctx.started {
							b.push(re, pc, pos+1, false, nctx) // Branch B: skip one debris byte
						}
					} else if !b.seamOK(pos, nctx) {
						continue
					}
				}
			}
			r, width := i.step(pos)
			if r != inst.Rune[0] {
				continue
			}
			if b.am != nil {
				nctx = b.consumed(pos, nctx)
			}
			pos += width
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstRuneAnyNotNL:
			if b.am != nil {
				if pos >= b.frameEnd(nctx.frame) {
					continue
				}
				if nctx.isComment != 0 {
					// Inside comment: skip only whitespace; no seam check.
					if nctx.started && pos < b.end && isCommentWhitespace(b.am.src[pos]) {
						b.push(re, pc, pos+1, false, nctx) // Branch B: skip source whitespace
					}
				} else {
					// Code context: skip insignificant bytes with seam check.
					if b.am.insignificant(pos) {
						if nctx.started {
							b.push(re, pc, pos+1, false, nctx) // Branch B: skip one debris byte
						}
					} else if !b.seamOK(pos, nctx) {
						continue
					}
				}
			}
			r, width := i.step(pos)
			if r == '\n' || r == endOfText {
				continue
			}
			if b.am != nil {
				nctx = b.consumed(pos, nctx)
			}
			pos += width
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstRuneAny:
			if b.am != nil {
				if pos >= b.frameEnd(nctx.frame) {
					continue
				}
				if nctx.isComment != 0 {
					// Inside comment: skip only whitespace; no seam check.
					if nctx.started && pos < b.end && isCommentWhitespace(b.am.src[pos]) {
						b.push(re, pc, pos+1, false, nctx) // Branch B: skip source whitespace
					}
				} else {
					// Code context: skip insignificant bytes with seam check.
					if b.am.insignificant(pos) {
						if nctx.started {
							b.push(re, pc, pos+1, false, nctx) // Branch B: skip one debris byte
						}
					} else if !b.seamOK(pos, nctx) {
						continue
					}
				}
			}
			r, width := i.step(pos)
			if r == endOfText {
				continue
			}
			if b.am != nil {
				nctx = b.consumed(pos, nctx)
			}
			pos += width
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstNodeGroup:
			if b.am == nil {
				continue
			}
			commentKind := uint8(inst.Arg)
			if commentKind != 0 {
				// Comment node-group: skip only whitespace to reach comment start.
				if nctx.started && pos < b.frameEnd(nctx.frame) && pos < b.end && isCommentWhitespace(b.am.src[pos]) {
					b.push(re, pc, pos+1, false, nctx) // Branch B: skip whitespace
				}
			} else {
				// Code node-group: skip all insignificant bytes to reach node start.
				if nctx.started && pos < b.frameEnd(nctx.frame) && b.am.insignificant(pos) {
					b.push(re, pc, pos+1, false, nctx) // Branch B
				}
			}
			ends := b.am.endsForStartKind(pos, commentKind)
			// Push one branch per candidate node end (ascending, so the largest
			// is on top of the stack and tried first — greedy). Each branch gets
			// a fresh interned frame chained to the current one; the exact-landing
			// check at InstNodeExit decides which actually fits. Nesting depth is
			// unbounded: the chain lives in the frame arena, not on the job. The
			// outer cursor flags carry inward so a seam at the node's first byte is
			// judged against the token that preceded the node-group.
			for k := 0; k < len(ends); k++ {
				nf := b.internFrame(int32(ends[k]), nctx.frame)
				b.push(re, inst.Out, pos, false, nodeCtx{
					frame:     nf,
					started:   nctx.started,
					gapOK:     nctx.gapOK,
					isComment: commentKind,
				})
			}
			continue

		case syntax.InstNodeExit:
			if nctx.frame < 0 || b.am == nil {
				continue
			}
			end := int(b.frames[nctx.frame].end)
			if nctx.isComment != 0 {
				// Comment node-group: all non-whitespace bytes before end must be consumed.
				p2 := pos
				for p2 < end && isCommentWhitespace(b.am.src[p2]) {
					p2++
				}
				if p2 < end {
					continue // non-whitespace comment content still remains
				}
			} else {
				// Code node-group: no significant (code token) bytes may remain before end.
				if b.am.nextSignificant(pos) < end {
					continue
				}
			}
			// Restore the parent frame (correct unwind of one nesting level). The
			// matched node counts as a consumed significant unit: the match has
			// started and any following word|word seam needs a fresh separator.
			nctx = nodeCtx{frame: b.frames[nctx.frame].parent, started: true, gapOK: false}
			pos = end
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstGap:
			if b.am == nil {
				continue
			}
			if nctx.isComment != 0 {
				// Inside a comment node-group: gap always succeeds; whitespace is
				// stepped over by Branch B in the rune instructions.
				nctx.gapOK = true
				pc = inst.Out
				goto CheckAndLoop
			}
			// A gap is a zero-width separator marker: the whitespace/comment bytes
			// themselves are stepped over by the per-character skip branches. It is
			// illegal only if it falls strictly inside one significant token (which
			// would split that token across the gap).
			q := b.am.prevSignificantEnd(pos)
			r := b.am.nextSignificant(pos)
			if q > 0 && r < len(b.am.src) && b.am.tokenIndexAt(q-1) == b.am.tokenIndexAt(r) {
				continue
			}
			nctx.gapOK = true
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstCapture:
			if arg {
				// Finished inst.Out; restore the old value.
				b.cap[inst.Arg] = pos
				continue
			} else {
				if inst.Arg < uint32(len(b.cap)) {
					// Capture pos to register, but save old value.
					// Inside a node region, snap the recorded boundary to a
					// significant byte (open forward, close back) so a group
					// never includes surrounding whitespace/comments.
					val := pos
					if nctx.frame >= 0 && b.am != nil {
						if nctx.isComment != 0 {
							// Inside comment: snap to non-whitespace boundaries.
							if inst.Arg&1 == 0 {
								val = b.am.nextNonCommentWhitespace(pos)
							} else {
								val = b.am.prevNonCommentWhitespaceEnd(pos)
							}
						} else {
							// Code context: snap to code-token boundaries.
							if inst.Arg&1 == 0 {
								val = b.am.nextSignificant(pos)
							} else {
								val = b.am.prevSignificantEnd(pos)
							}
						}
					}
					b.push(re, pc, b.cap[inst.Arg], true, nctx) // come back when we're done.
					b.cap[inst.Arg] = val
				}
				pc = inst.Out
				goto CheckAndLoop
			}

		case syntax.InstEmptyWidth:
			flag := i.context(pos)
			if !flag.match(syntax.EmptyOp(inst.Arg)) {
				continue
			}
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstNop:
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstMatch:
			// We found a match. If the caller doesn't care
			// where the match is, no point going further.
			if len(b.cap) == 0 {
				return true
			}

			// Record best match so far.
			// Only need to check end point, because this entire
			// call is only considering one start position.
			if len(b.cap) > 1 {
				b.cap[1] = pos
			}
			if old := b.matchcap[1]; old == -1 || (longest && pos > 0 && pos > old) {
				copy(b.matchcap, b.cap)
			}

			// If going for first match, we're done.
			if !longest {
				return true
			}

			// If we used the entire text, no longer match is possible.
			if pos == b.end {
				return true
			}

			// Otherwise, continue on in hope of a longer match.
			continue
		}
	}

	return longest && len(b.matchcap) > 1 && b.matchcap[1] >= 0
}

// backtrack runs a backtracking search of prog on the input starting at pos.
func (re *Regexp) backtrack(am *astMap, ib []byte, is string, pos int, ncap int, dstCap []int) []int {
	startCond := re.cond
	if startCond == ^syntax.EmptyOp(0) { // impossible
		return nil
	}
	if startCond&syntax.EmptyBeginText != 0 && pos != 0 {
		// Anchored match, past beginning of text.
		return nil
	}

	b := newBitState()
	i, end := b.inputs.init(nil, ib, is)
	b.reset(re.prog, end, ncap)
	b.am = am
	// The work cap applies only to node-group programs (plain matching is
	// already bounded by the bitvector visited prune). Generous: realistic node
	// patterns stay far under it; it only catches adversarial deep nesting.
	if am != nil {
		b.maxSteps = 64*(end+1)*len(re.prog.Inst) + (1 << 20)
	} else {
		b.maxSteps = 0
	}

	// Anchored search must start at the beginning of the input
	if startCond&syntax.EmptyBeginText != 0 {
		if len(b.cap) > 0 {
			b.cap[0] = pos
		}
		if !re.tryBacktrack(b, i, uint32(re.prog.Start), pos) {
			freeBitState(b)
			return nil
		}
	} else {

		// Unanchored search, starting from each possible text position.
		// Notice that we have to try the empty string at the end of
		// the text, so the loop condition is pos <= end, not pos < end.
		// This looks like it's quadratic in the size of the text,
		// but we are not clearing visited between calls to TrySearch,
		// so no work is duplicated and it ends up still being linear.
		width := -1
		for ; pos <= end && width != 0; pos += width {
			if len(re.prefix) > 0 {
				// Match requires literal prefix; fast search for it.
				advance := i.index(re, pos)
				if advance < 0 {
					freeBitState(b)
					return nil
				}
				pos += advance
			}

			if len(b.cap) > 0 {
				b.cap[0] = pos
			}
			if re.tryBacktrack(b, i, uint32(re.prog.Start), pos) {
				// Match must be leftmost; done.
				goto Match
			}
			_, width = i.step(pos)
		}
		freeBitState(b)
		return nil
	}

Match:
	dstCap = append(dstCap, b.matchcap...)
	freeBitState(b)
	return dstCap
}
