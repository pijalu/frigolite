.dbconfig defensive off
BEGIN;
PRAGMA writable_schema = on;
PRAGMA foreign_keys = off;
PRAGMA encoding = 'UTF-8';
PRAGMA page_size = '4096';
PRAGMA auto_vacuum = '0';
PRAGMA user_version = '0';
PRAGMA application_id = '0';
CREATE TABLE t3(g, h, i);
INSERT OR IGNORE INTO 't3'(_rowid_, 'g', 'h', 'i') VALUES (1, 'one', 'two', 'three');
CREATE TABLE lost_and_found(rootpgno INTEGER, pgno INTEGER, nfield INTEGER, id INTEGER, c0, c1, c2);
INSERT INTO lost_and_found VALUES(3, 3, 3, 1, 1, 2, 3);
INSERT INTO lost_and_found VALUES(3, 3, 3, 2, 'a', 'b', 'c');
PRAGMA writable_schema = off;
COMMIT;
