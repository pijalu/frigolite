.dbconfig defensive off
BEGIN;
PRAGMA writable_schema = on;
PRAGMA foreign_keys = off;
PRAGMA encoding = 'UTF-8';
PRAGMA page_size = '4096';
PRAGMA auto_vacuum = '0';
PRAGMA user_version = '0';
PRAGMA application_id = '0';
CREATE TABLE t1(a, b, c, PRIMARY KEY(b, c)) WITHOUT ROWID;
INSERT OR IGNORE INTO 't1'('a', 'b', 'c') VALUES (1, 2, 3);
INSERT OR IGNORE INTO 't1'('a', 'b', 'c') VALUES (4, 5, 6);
INSERT OR IGNORE INTO 't1'('a', 'b', 'c') VALUES (7, 8, 9);
PRAGMA writable_schema = off;
COMMIT;
