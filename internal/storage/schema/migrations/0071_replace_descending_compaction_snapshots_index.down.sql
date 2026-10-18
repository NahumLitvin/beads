-- Reverse of 0071: restore 0010's idx_comp_snap_issue (issue_id,
-- compaction_level, created_at DESC) and drop the ascending twin
-- idx_comp_snap_issue_asc. The order mirrors the up file: fk_comp_snap_issue
-- needs an issue_id-leading index at every step, so the restored index goes
-- in before the twin comes out.
--
-- On Dolt 2.3.3 and later this brings back the descending storage the up
-- file removes, so Dolt older than 2.3.3 -- and every bd release that links
-- one -- again fails on this database with "table has unknown fields".
-- That is the faithful v70 schema; roll back only as far as the bd and Dolt
-- you then run can read.
--
-- Guarded on INFORMATION_SCHEMA.STATISTICS like the up file, so a store that
-- never took 0071, or was already rolled back, no-ops. Only
-- migrations/*.up.sql is embedded into the CLI fresh bundle, so the PREPARE
-- hazard (cli_prepared_ddl.go) never reaches this file.
SET @comp_snap_has_old_index = (
    SELECT IF(COUNT(*) > 0, 1, 0)
    FROM INFORMATION_SCHEMA.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'compaction_snapshots'
      AND INDEX_NAME = 'idx_comp_snap_issue'
);
SET @comp_snap_has_twin_index = (
    SELECT IF(COUNT(*) > 0, 1, 0)
    FROM INFORMATION_SCHEMA.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'compaction_snapshots'
      AND INDEX_NAME = 'idx_comp_snap_issue_asc'
);
SET @sql = IF(@comp_snap_has_twin_index = 1 AND @comp_snap_has_old_index = 0,
    'ALTER TABLE compaction_snapshots ADD INDEX idx_comp_snap_issue (issue_id, compaction_level, created_at DESC)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
SET @sql = IF(@comp_snap_has_twin_index = 1,
    'ALTER TABLE compaction_snapshots DROP INDEX idx_comp_snap_issue_asc',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
