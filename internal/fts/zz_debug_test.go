package fts

import "testing"

func TestW5DebugLegacy2(t *testing.T) {
	p := &legacyParser{input: "one OR two"}
	st := &legacyParseState{p: p, require: true, syntaxErr: error(nil)}
	pos := 0
	for i := 0; i < 6; i++ {
		node, consumed, done, nerr := p.legacyNextNode(pos)
		if nerr != nil {
			t.Logf("round %d: err %v", i, nerr)
			break
		}
		if done {
			t.Logf("round %d: done", i)
			break
		}
		pos += consumed
		if node == nil {
			t.Logf("round %d: nil node", i)
			continue
		}
		t.Logf("round %d: eType=%d pos=%d require=%v", i, node.eType, pos, st.require)
		st.attach(node)
		t.Logf("  after: require=%v err=%v", st.require, st.err)
	}
}
