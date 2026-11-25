package tcl

import "testing"

func TestHasInterleavedRowCallback(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   bool
	}{
		{
			name:   "wal6",
			script: "set res [list]\ndb eval { SELECT * FROM t1 } { if {$x==1} { db2 eval { INSERT INTO t1 VALUES(5, 6) } } }\nset res",
			want:   true,
		},
		{
			name:   "interrupt2",
			script: "set i 10\nset res [list [catch { db eval {SELECT * FROM z1} { incr i -1; if {$i==0} { sqlite3_wal_checkpoint_v2 db truncate } } } msg] $msg]\nlist $cres $res",
			want:   true,
		},
		{
			name:   "first column",
			script: "db eval {SELECT x FROM t} {lappend res $x}\nset res",
			want:   false,
		},
		{
			name:   "plain query",
			script: "set res [db eval {SELECT x FROM t}]\nset res",
			want:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasInterleavedRowCallback(c.script); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
