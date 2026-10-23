package fts

import "testing"

func TestFTS3ShadowNameClassification(t *testing.T) {
	fts3 := NewFTS3Module("fts3")
	fts4 := NewFTS3Module("fts4")
	for _, name := range []string{"idx_content", "idx_segments", "idx_segdir"} {
		if !fts3.ShadowName("idx", name) || !fts4.ShadowName("idx", name) {
			t.Errorf("expected common shadow suffix: %s", name)
		}
	}
	for _, name := range []string{"idx_docsize", "idx_stat"} {
		if !fts3.ShadowName("idx", name) || !fts4.ShadowName("idx", name) {
			t.Errorf("FTS3/4 shadow suffix not classified: %s", name)
		}
	}
	for _, name := range []string{"idx_segments_extra", "idxish_segments", "idx_contentx"} {
		if fts4.ShadowName("idx", name) {
			t.Errorf("lookalike classified as shadow: %s", name)
		}
	}
}
