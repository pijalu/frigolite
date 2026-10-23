package vtab

import "testing"

func TestRtreeShadowNameClassification(t *testing.T) {
	module := NewRtreeModule[float32](nil)
	for _, suffix := range []string{"node", "parent", "rowid"} {
		name := "tree_" + suffix
		if !module.ShadowName("tree", name) {
			t.Errorf("expected RTree shadow %q", name)
		}
	}
	for _, name := range []string{"tree_node_extra", "treehouse_node", "tree_content"} {
		if module.ShadowName("tree", name) {
			t.Errorf("lookalike classified as shadow: %q", name)
		}
	}
}
