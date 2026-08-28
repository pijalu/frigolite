-- Oracle script transcript: SQL-observable hook/transaction semantics
-- exercised by dbstatus/hook/hook2/interrupt testgen packages.
.headers off
.mode list
CREATE TABLE t1(a,b);
INSERT INTO t1 VALUES(1,'one');
SELECT 'changes_after_insert', changes();
INSERT INTO t1 VALUES(2,'two');
SELECT 'changes_after_second', changes();
UPDATE t1 SET b='ONE' WHERE a=1;
SELECT 'changes_after_update', changes();
DELETE FROM t1 WHERE a=2;
SELECT 'changes_after_delete', changes();
SELECT 'total_changes', total_changes();
BEGIN;
INSERT INTO t1 VALUES(3,'three');
ROLLBACK;
SELECT 'after_rollback_count', count(*) FROM t1;
SELECT 'after_rollback_changes', changes();
CREATE TRIGGER t1_no_delete BEFORE DELETE ON t1 BEGIN SELECT RAISE(ABORT,'delete blocked'); END;
DELETE FROM t1 WHERE a=1;
SELECT 'after_abort_delete', count(*) FROM t1;
DROP TRIGGER t1_no_delete;
CREATE TRIGGER t1_rb_insert BEFORE INSERT ON t1 BEGIN SELECT RAISE(ROLLBACK,'insert blocked'); END;
BEGIN;
INSERT INTO t1 VALUES(9,'nine');
SELECT 'unreachable_after_raise', 1;
