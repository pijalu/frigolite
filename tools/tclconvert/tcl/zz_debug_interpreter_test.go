package tcl

import "testing"

func TestDebugInterpreter(t *testing.T) {
	src := `do_test func-4.39 { string tolower [db eval {SELECT round(1e500), round(-1e500);}] } {inf -inf}`
	i := NewInterp()
	cmds := parseCommands(src)
	t.Logf("cmds=%#v", cmds)
	for _, cmd := range cmds {
		for _, w := range cmd {
			t.Logf("word=%+v", w)
		}
	}
	err := i.Execute(src)
	t.Logf("err=%v stmts=%#v", err, i.Stmts())
}
