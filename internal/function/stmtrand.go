package function

// stmtrand.go ports ext/misc/stmtrand.c: the stmtrand([SEED]) test function,
// a pseudo-random integer generator whose sequence is repeatable per
// statement. The seed is used by the first invocation only and ignored for
// subsequent calls in the same statement; resetting the statement restarts
// the sequence.

// StmtrandState is the LCG state of one statement's stmtrand() sequence
// (stmtrand.c Stmtrand).
type StmtrandState struct {
	X, Y uint32
}

// NewStmtrandState seeds a fresh sequence (stmtrandFunc first-call init:
// x = seed|1, y = seed).
func NewStmtrandState(seed uint32) *StmtrandState {
	return &StmtrandState{X: seed | 1, Y: seed}
}

// StmtrandStep advances the LCG once and returns the result value
// (stmtrandFunc body):
//
//	x = (x>>1) ^ ((1+~(x&1)) & 0xd0000001)
//	y = y*1103515245 + 12345
//	result = (x ^ y) & 0x7fffffff
func StmtrandStep(st *StmtrandState) int64 {
	st.X = (st.X >> 1) ^ ((1 + ^(st.X & 1)) & 0xd0000001)
	st.Y = st.Y*1103515245 + 12345
	return int64((st.X ^ st.Y) & 0x7fffffff)
}

// fnSTMTRAND is the fallback scalar used when a call is not routed through
// the statement-scoped engine dispatch (execexpr evalEngineFunc): each call
// seeds a fresh LCG from its argument (or 0) and advances once, mirroring
// the C function's first-call behavior.
func fnSTMTRAND(args []interface{}) (interface{}, error) {
	var seed uint32
	if len(args) >= 1 && args[0] != nil {
		seed = uint32(toInt64(args[0]))
	}
	return StmtrandStep(NewStmtrandState(seed)), nil
}
