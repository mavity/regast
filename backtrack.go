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

// nodeCtx is the per-job node-matching cursor: which frame bounds it, plus two
// transient flags. frame < 0 means "not inside any node-group". It is a small
// scalar value copied with the job (no array, no heap).
//
// fresh marks that no significant character has been consumed yet at the current
// node level (so the first atom skips the junction rule); pendGap marks that an
// InstGap just validated and crossed a token seam.
type nodeCtx struct {
	frame   int32
	fresh   bool
	pendGap bool
}

func noNode() nodeCtx { return nodeCtx{frame: -1} }

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

// nodeMatchPos applies the node-group whitespace rules to decide where the next
// significant rune to match begins. It returns the byte position p, the updated
// context, and ok=false if no legal position exists (the branch must die).
func (b *bitState) nodeMatchPos(pos int, nctx nodeCtx) (p int, nn nodeCtx, ok bool) {
	end := b.frameEnd(nctx.frame)
	switch {
	case nctx.fresh:
		// First atom in this node: no junction constraint.
		nctx.fresh = false
		nctx.pendGap = false
		p = b.am.nextSignificant(pos)
	case nctx.pendGap:
		// A gap already validated the token seam and advanced pos to it.
		nctx.pendGap = false
		p = pos
	default:
		// No gap: a word|word token seam may not be crossed (that would merge
		// or split two identifier tokens). Other seams are free.
		p = b.am.nextSignificant(pos)
		if pos > 0 && p < len(b.am.src) &&
			b.am.tokenIndexAt(pos-1) != b.am.tokenIndexAt(p) &&
			isWordByte(b.am.src[pos-1]) && isWordByte(b.am.src[p]) {
			return 0, nctx, false
		}
	}
	if p >= end {
		return 0, nctx, false
	}
	return p, nctx, true
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

	if cap(b.jobs) == 0 {
		b.jobs = make([]job, 0, 256)
	} else {
		b.jobs = b.jobs[:0]
	}

	visitedSize := (len(prog.Inst)*(end+1) + visitedBits - 1) / visitedBits
	if cap(b.visited) < visitedSize {
		b.visited = make([]uint32, visitedSize, maxBacktrackVector/visitedBits)
	} else {
		b.visited = b.visited[:visitedSize]
		clear(b.visited) // set to 0
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

	// Node-group state: a pooled, append-only frame arena plus interning and a
	// node-region visited set. All reused across searches (capacity retained).
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
	if nctx.fresh {
		fl |= 1
	}
	if nctx.pendGap {
		fl |= 2
	}
	k := nodeVisitKey{pc: pc, pos: int32(pos), frame: nctx.frame, fl: fl}
	if _, ok := b.nodeVisited[k]; ok {
		return false
	}
	b.nodeVisited[k] = struct{}{}
	return true
}

// push pushes (pc, pos, arg) onto the job stack if it should be visited. Plain
// (frame < 0) states use the flat (pc,pos) bitvector; node-region states use the
// frame-keyed node-visited set, which deduplicates them correctly.
func (b *bitState) push(re *Regexp, pc uint32, pos int, arg bool, nctx nodeCtx) {
	// Only check the visited prune when arg is false.
	// When arg is true, we are continuing a previous visit.
	if re.prog.Inst[pc].Op == syntax.InstFail {
		return
	}
	if !arg {
		if nctx.frame >= 0 {
			if !b.shouldVisitNode(pc, pos, nctx) {
				return
			}
		} else if !b.shouldVisit(pc, pos) {
			return
		}
	}
	b.jobs = append(b.jobs, job{pc: pc, arg: arg, pos: pos, nctx: nctx})
}

// tryBacktrack runs a backtracking search starting at pos.
func (re *Regexp) tryBacktrack(b *bitState, i input, pc uint32, pos int) bool {
	longest := re.longest

	// Per-start work counter. Frame-keyed dedup makes realistic node patterns
	// polynomial, so this is only a backstop: deeply-nested same-start
	// node-groups are inherently exponential (back-to-back ⦃ opens multiply
	// candidate ends before any consume narrows them), and without a cap such
	// adversarial patterns would hang. It is reset per start, so it bounds work
	// per start position rather than across the whole (linear) start sweep.
	b.steps = 0

	b.push(re, pc, pos, false, noNode())
	for len(b.jobs) > 0 {
		l := len(b.jobs) - 1
		// Pop job off the stack.
		pc := b.jobs[l].pc
		pos := b.jobs[l].pos
		arg := b.jobs[l].arg
		nctx := b.jobs[l].nctx
		b.jobs = b.jobs[:l]

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
		// Plain states use the flat (pc,pos) bitvector; node-region states use
		// the frame-keyed node-visited set. Both deduplicate correctly, so the
		// search terminates without any step cap.
		if nctx.frame >= 0 {
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
			p := pos
			if nctx.frame >= 0 {
				var ok bool
				if p, nctx, ok = b.nodeMatchPos(pos, nctx); !ok {
					continue
				}
			}
			r, width := i.step(p)
			if !inst.MatchRune(r) {
				continue
			}
			pos = p + width
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstRune1:
			p := pos
			if nctx.frame >= 0 {
				var ok bool
				if p, nctx, ok = b.nodeMatchPos(pos, nctx); !ok {
					continue
				}
			}
			r, width := i.step(p)
			if r != inst.Rune[0] {
				continue
			}
			pos = p + width
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstRuneAnyNotNL:
			p := pos
			if nctx.frame >= 0 {
				var ok bool
				if p, nctx, ok = b.nodeMatchPos(pos, nctx); !ok {
					continue
				}
			}
			r, width := i.step(p)
			if r == '\n' || r == endOfText {
				continue
			}
			pos = p + width
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstRuneAny:
			p := pos
			if nctx.frame >= 0 {
				var ok bool
				if p, nctx, ok = b.nodeMatchPos(pos, nctx); !ok {
					continue
				}
			}
			r, width := i.step(p)
			if r == endOfText {
				continue
			}
			pos = p + width
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstNodeGroup:
			if b.am == nil {
				continue
			}
			ends := b.am.endsForStart(pos)
			// Push one branch per candidate node end (ascending, so the largest
			// is on top of the stack and tried first — greedy). Each branch gets
			// a fresh interned frame chained to the current one; the exact-landing
			// check at InstNodeExit decides which actually fits. Nesting depth is
			// unbounded: the chain lives in the frame arena, not on the job.
			for k := 0; k < len(ends); k++ {
				nf := b.internFrame(int32(ends[k]), nctx.frame)
				b.push(re, inst.Out, pos, false, nodeCtx{frame: nf, fresh: true})
			}
			continue

		case syntax.InstNodeExit:
			if nctx.frame < 0 || b.am == nil {
				continue
			}
			end := int(b.frames[nctx.frame].end)
			// The inner pattern must have consumed the node's significant
			// content: no significant byte may remain before end.
			if b.am.nextSignificant(pos) < end {
				continue
			}
			// Restore the parent frame (correct unwind of one nesting level).
			nctx = nodeCtx{frame: b.frames[nctx.frame].parent}
			pos = end
			pc = inst.Out
			goto CheckAndLoop

		case syntax.InstGap:
			if b.am == nil {
				continue
			}
			r := b.am.nextSignificant(pos)
			if !b.am.gapBoundary(pos-1, r) {
				continue
			}
			pos = r
			nctx.pendGap = true
			nctx.fresh = false
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
						if inst.Arg&1 == 0 {
							val = b.am.nextSignificant(pos)
						} else {
							val = b.am.prevSignificantEnd(pos)
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
