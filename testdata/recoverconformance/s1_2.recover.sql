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
INSERT OR IGNORE INTO 't1'('a', 'b', 'c') VALUES (13, 'hello\r\nworld', 13);
PRAGMA writable_schema = off;
COMMIT;
