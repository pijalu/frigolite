-- SQLite 3.51.0 source-build oracle fixture; run with STAT4 enabled.
CREATE TABLE stat4_interleaved(a INT,b INT);
CREATE INDEX stat4_interleaved_ab ON stat4_interleaved(a,b);
WITH RECURSIVE a(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM a WHERE x<9),
               b(y) AS (VALUES(0) UNION ALL SELECT y+1 FROM b WHERE y<99)
INSERT INTO stat4_interleaved SELECT a.x,b.y%5 FROM a,b ORDER BY a.x,b.y;
ANALYZE;
SELECT nEq||'|'||nLt||'|'||nDLt||'|'||hex(sample)
FROM sqlite_stat4 WHERE idx='stat4_interleaved_ab' ORDER BY rowid;
