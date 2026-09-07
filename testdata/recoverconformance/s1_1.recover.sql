.dbconfig defensive off
BEGIN;
PRAGMA writable_schema = on;
PRAGMA foreign_keys = off;
PRAGMA encoding = 'UTF-8';
PRAGMA page_size = '4096';
PRAGMA auto_vacuum = '0';
PRAGMA user_version = '0';
PRAGMA application_id = '0';
CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c);
INSERT OR IGNORE INTO 't1'('a', 'b', 'c') VALUES (1, 4, X'1234567800');
INSERT OR IGNORE INTO 't1'('a', 'b', 'c') VALUES (2, 'test', 8.1);
INSERT OR IGNORE INTO 't1'('a', 'b', 'c') VALUES (3, replace('hello\nworld','\n', char(10)), 8.4);
PRAGMA writable_schema = off;
COMMIT;
