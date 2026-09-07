.dbconfig defensive off
BEGIN;
PRAGMA writable_schema = on;
PRAGMA foreign_keys = off;
PRAGMA encoding = 'UTF-8';
PRAGMA page_size = '4096';
PRAGMA auto_vacuum = '0';
PRAGMA user_version = '0';
PRAGMA application_id = '0';
CREATE TABLE lost_and_found(rootpgno INTEGER, pgno INTEGER, nfield INTEGER, id INTEGER, c0, c1, c2);
INSERT INTO lost_and_found VALUES(2, 2, 3, NULL, 2, 3, 1);
INSERT INTO lost_and_found VALUES(2, 2, 3, NULL, 5, 6, 4);
INSERT INTO lost_and_found VALUES(2, 2, 3, NULL, 8, 9, 7);
PRAGMA writable_schema = off;
COMMIT;
