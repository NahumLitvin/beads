-- Migration 0071: replace compaction_snapshots' idx_comp_snap_issue
-- (issue_id, compaction_level, created_at DESC), which 0010 declares, with
-- the ascending twin idx_comp_snap_issue_asc (issue_id, compaction_level,
-- created_at).
--
-- Dolt 2.3.3 and later store a descending index column as descending
-- (dolthub/dolt#11746); earlier releases parsed the DESC and stored the
-- column ascending. bd's embedded engine is Dolt 2.4.1, so a database it
-- creates holds 0010's created_at DESC for real, and Dolt older than 2.3.3
-- cannot decode that table's schema: a plain INSERT into any table, and any
-- information_schema query, fails with "table has unknown fields". Released
-- bd (v1.3.x links a Dolt 2.2.0 driver) hits that engine error on open
-- instead of its structured schema-skew refusal, and BD_IGNORE_SCHEMA_SKEW=1
-- does not get past it. The same goes for a database whose schema a dolt
-- sql-server 2.3.3 or newer created, whatever the bd version.
--
-- What 0071 cannot reach is history. bd init commits each migration on its
-- own, so on a database created by Dolt 2.3.3 or later the commits from
-- 0010's through 0070's still hold the descending column. Dolt older than
-- 2.3.3 fails the same way reading compaction_snapshots at one of them (AS
-- OF, dolt_history_compaction_snapshots, dolt_diff_compaction_snapshots) and
-- on any dolt diff with one as an endpoint. Reads and writes at the current
-- root, other tables' AS OF and dolt_history_ reads, dolt log, push, clone
-- and pull all work (measured with dolt 2.1.8 and 2.2.0 on 2026-10-18).
--
-- The DESC buys nothing. The table's one reader, GetLatestSnapshotInTx
-- (internal/storage/issueops/compaction.go), filters on issue_id alone and
-- orders by created_at DESC LIMIT 1; compaction_level sits between the two in
-- the index, so the index cannot supply that order in either direction and
-- the plan is the same TopN over an indexed lookup with or without the DESC.
-- An index declared with no explicit order carries no order vectors, so
-- every Dolt release can read the twin.
--
-- 0010 is frozen (scripts/check-migration-hygiene.sh check C), hence a new
-- migration. Two shapes that look simpler fail on dolt 2.4.1:
--   * Dropping idx_comp_snap_issue first, or swapping it in one ALTER,
--     fails with "cannot drop index: idx_comp_snap_issue is used by foreign
--     key fk_comp_snap_issue" -- the twin must exist before the drop, and
--     Dolt then backs the foreign key with it.
--   * Renaming the twin back to idx_comp_snap_issue fails at commit:
--     "foreign key fk_comp_snap_issue has entered an invalid state". The
--     new name stays; nothing reads the index by name.
--
-- DDL is not transactional (migrations/README.md), so each step is guarded
-- on INFORMATION_SCHEMA.STATISTICS the way 0042 guards its foreign-key swap.
-- Run again after a pass killed between the two steps, the file skips the
-- add and does the drop, and a replay onto a store that already took this
-- migration is a no-op (internal/storage/dolt's pr4107 replay harness
-- requires that of every migration >= 0046). A missing table holds neither
-- index, so both guards take the SELECT 1 branch.
--
-- The guards key on the index name, not on whether created_at is stored
-- descending. A database Dolt older than 2.3.3 created holds an ascending
-- idx_comp_snap_issue, and replacing that one too keeps every clone on one
-- schema, so a later merge between clones never meets two index sets.
--
-- No CLI-bundle override: both prepared forms here, ALTER TABLE ... ADD
-- INDEX and ... DROP INDEX, execute through the dolt CLI batch path on 2.1.8
-- as well as 2.4.1 (measured; see preparedALTERSafeOnFreshBundle in
-- cli_prepared_ddl.go). compaction_snapshots replicates -- it is main-plane,
-- not clone-local -- so there is no ignored twin either.
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
SET @sql = IF(@comp_snap_has_old_index = 1 AND @comp_snap_has_twin_index = 0,
    'ALTER TABLE compaction_snapshots ADD INDEX idx_comp_snap_issue_asc (issue_id, compaction_level, created_at)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
SET @sql = IF(@comp_snap_has_old_index = 1,
    'ALTER TABLE compaction_snapshots DROP INDEX idx_comp_snap_issue',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
