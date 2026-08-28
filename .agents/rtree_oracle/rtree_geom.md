# rtree geometry MATCH — parity evidence (slice9, t5)

Engine functions `cube`/`circle` port src/test_rtree.c callbacks; SQLite's
/usr/bin/sqlite3 CLI does not ship them (test-harness only), so ground truth
is the upstream TCL expectation corpus now exercised by:
  testgen/rtree9 (cube 3-D, rt32 i32, error paths, circle 2-D)
  testgen/rtree8 (x1 MATCH <literal> -> {1 {SQL logic error}})

Transcript (frigolite):
    SELECT * FROM rt WHERE id MATCH cube(0,0,0,2,2,2);   -> 1 | 1.0 2.0 1.0 2.0 1.0 2.0
    SELECT * FROM rt WHERE id MATCH cube(3,3,3,2,2,2);   -> (empty)
    SELECT id FROM rt WHERE id MATCH cube(5.5,1,1);      -> ERROR: SQL logic error

MATCH plumb: conjunct consumed into vtab.RtreeMatchSink at materialization
(argvConsumed analogue); callback invoked per cell incl. interior MBRs
(res!=0 keeps); non-marker values / lazy param validation surface
{1 {SQL logic error}} like SQLITE_ERROR from xGeom.
