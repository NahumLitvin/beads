//go:build cgo

package embeddeddolt_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// These tests pin migration 0071 (gastownhall/beads#7646 review) on the engine
// bd links, which -- unlike Dolt older than 2.3.3 -- stores 0010's
// idx_comp_snap_issue (issue_id, compaction_level, created_at DESC) with
// created_at really descending. That stored DESC is what older Dolt cannot
// decode ("table has unknown fields"), so the tests read the shape
// information_schema reports, the foreign key the index backs, and the
// table's one reader. They need BEADS_TEST_EMBEDDED_DOLT=1 (requireEmbedded,
// migrate_selfheal_test.go). The CLI-bundle half is
// TestMigration0071LeavesNoDescendingIndexThroughDoltCLI in
// internal/storage/schema.

const compSnapMigration0071 = "0071_replace_descending_compaction_snapshots_index.up.sql"

const compSnapTwinShape = "issue_id:A,compaction_level:A,created_at:A"

func TestEmbeddedMigration0071ReplacesDescendingCompactionSnapshotsIndex(t *testing.T) {
	requireEmbedded(t)
	ctx := t.Context()
	upSQL, err := schema.MigrationSQL(compSnapMigration0071)
	if err != nil {
		t.Fatalf("read 0071 migration: %v", err)
	}

	// The upgrade a v69 user takes through the production MigrateUp path, which
	// applies 0070 and then 0071.
	t.Run("upgrade from v69", func(t *testing.T) {
		dataDir := seedMainSchemaAt(t, ctx, 69)
		conn, closeConn := openPinnedConn(t, ctx, dataDir)
		defer closeConn()

		if got, want := compSnapIndexShape(t, ctx, conn, "idx_comp_snap_issue"), "issue_id:A,compaction_level:A,created_at:D"; got != want {
			t.Fatalf("v69 idx_comp_snap_issue = %q, want %q: this engine should store 0010's DESC, which is the shape 0071 removes", got, want)
		}

		// The newest snapshot of cs-keep is neither first nor last in index
		// order, so the reader below has to sort on created_at to find it.
		execFrozenGuard(t, ctx, conn, `
INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type)
VALUES ('cs-keep', 'x', '', '', '', '', 'open', 2, 'task'),
       ('cs-gone', 'x', '', '', '', '', 'open', 2, 'task');
INSERT INTO compaction_snapshots (id, issue_id, compaction_level, snapshot_json, created_at)
VALUES ('cs-snap-1', 'cs-keep', 1, '{"title":"oldest"}', '2026-10-01 00:00:00'),
       ('cs-snap-2', 'cs-keep', 2, '{"title":"middle"}', '2026-10-02 00:00:00'),
       ('cs-snap-3', 'cs-keep', 1, '{"title":"newest"}', '2026-10-03 00:00:00'),
       ('cs-snap-4', 'cs-gone', 1, '{"title":"doomed"}', '2026-10-01 00:00:00');
`)
		mustDrain(t, ctx, conn, "CALL DOLT_ADD('-A')")
		mustDrain(t, ctx, conn, "CALL DOLT_COMMIT('-m', 'test: seed compaction snapshots')")
		if got := compSnapLatestTitle(t, ctx, conn, "cs-keep"); got != "newest" {
			t.Fatalf("before 0071, latest cs-keep snapshot = %q, want %q", got, "newest")
		}

		if _, err := schema.MigrateUp(ctx, conn); err != nil {
			t.Fatalf("MigrateUp from v69: %v", err)
		}
		compSnapRequireReplaced(t, ctx, conn)
		// A clone or a pull hands the other side HEAD, not the working set.
		if tables := dirtyTableNames(t, ctx, conn); contains(tables, "compaction_snapshots") {
			t.Fatalf("compaction_snapshots still dirty after MigrateUp (%v); 0071's step commit must carry the new index into HEAD", tables)
		}
		if got := compSnapLatestTitle(t, ctx, conn, "cs-keep"); got != "newest" {
			t.Fatalf("after 0071, latest cs-keep snapshot = %q, want %q", got, "newest")
		}

		// Replaying the file onto the migrated store changes nothing.
		execFrozenGuard(t, ctx, conn, upSQL)
		compSnapRequireReplaced(t, ctx, conn)

		// fk_comp_snap_issue, now backed by the twin, still cascades a delete
		// and still refuses a snapshot of a missing issue.
		execFrozenGuard(t, ctx, conn, "DELETE FROM issues WHERE id = 'cs-gone'")
		if got := scalarInt(t, ctx, conn, "SELECT COUNT(*) FROM compaction_snapshots WHERE issue_id = 'cs-gone'"); got != 0 {
			t.Fatalf("snapshots of deleted issue cs-gone = %d, want 0 (ON DELETE CASCADE)", got)
		}
		if _, err := conn.ExecContext(ctx, `
INSERT INTO compaction_snapshots (id, issue_id, compaction_level, snapshot_json, created_at)
VALUES ('cs-snap-5', 'cs-missing', 1, '{}', '2026-10-04 00:00:00')`); err == nil {
			t.Fatal("snapshot of missing issue cs-missing was accepted; fk_comp_snap_issue must refuse it")
		}
	})

	// DDL is not transactional: a pass killed after the add holds both
	// indexes, and running the file again must skip the add and finish the
	// drop.
	t.Run("resume after the add", func(t *testing.T) {
		dataDir := seedMainSchemaAt(t, ctx, 69)
		conn, closeConn := openPinnedConn(t, ctx, dataDir)
		defer closeConn()

		execFrozenGuard(t, ctx, conn, "ALTER TABLE compaction_snapshots ADD INDEX idx_comp_snap_issue_asc (issue_id, compaction_level, created_at)")
		execFrozenGuard(t, ctx, conn, upSQL)
		compSnapRequireReplaced(t, ctx, conn)
	})

	t.Run("store without the table", func(t *testing.T) {
		dataDir := seedMainSchemaAt(t, ctx, 69)
		conn, closeConn := openPinnedConn(t, ctx, dataDir)
		defer closeConn()

		execFrozenGuard(t, ctx, conn, "DROP TABLE compaction_snapshots")
		execFrozenGuard(t, ctx, conn, upSQL)
		if got := scalarInt(t, ctx, conn, "SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'compaction_snapshots'"); got != 0 {
			t.Fatalf("compaction_snapshots tables = %d after 0071 on a store without one, want 0", got)
		}
	})

	// A store created from nothing by the open path bd init uses.
	t.Run("fresh store", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		store, err := embeddeddolt.Open(ctx, beadsDir, "testdb", "main")
		if err != nil {
			t.Fatalf("open fresh store: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close fresh store: %v", err)
		}
		conn, closeConn := openPinnedConn(t, ctx, filepath.Join(beadsDir, "embeddeddolt"))
		defer closeConn()

		compSnapRequireReplaced(t, ctx, conn)
		// One descending index column anywhere makes the database unreadable
		// for Dolt older than 2.3.3. A migration that wants one has to make
		// that trade-off on purpose and change this assertion.
		if cols := compSnapDescendingColumns(t, ctx, conn); len(cols) != 0 {
			t.Fatalf("fresh store has descending index columns %v, want none", cols)
		}
	})
}

// compSnapIndexShape returns an index on compaction_snapshots as
// "column:collation,..." in index order, or "" when the index does not exist.
func compSnapIndexShape(t *testing.T, ctx context.Context, conn *sql.Conn, index string) string {
	t.Helper()
	var shape sql.NullString
	if err := conn.QueryRowContext(ctx, `
SELECT GROUP_CONCAT(CONCAT(COLUMN_NAME, ':', COALESCE(COLLATION, '?')) ORDER BY SEQ_IN_INDEX)
FROM INFORMATION_SCHEMA.STATISTICS
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'compaction_snapshots' AND INDEX_NAME = ?`, index).Scan(&shape); err != nil {
		t.Fatalf("read %s shape: %v", index, err)
	}
	return shape.String
}

// compSnapRequireReplaced asserts 0071's end state: the all-ascending twin in
// place of idx_comp_snap_issue, and fk_comp_snap_issue as 0042 left it.
func compSnapRequireReplaced(t *testing.T, ctx context.Context, conn *sql.Conn) {
	t.Helper()
	if got := compSnapIndexShape(t, ctx, conn, "idx_comp_snap_issue"); got != "" {
		t.Fatalf("idx_comp_snap_issue = %q after 0071, want it dropped", got)
	}
	if got := compSnapIndexShape(t, ctx, conn, "idx_comp_snap_issue_asc"); got != compSnapTwinShape {
		t.Fatalf("idx_comp_snap_issue_asc = %q after 0071, want %q", got, compSnapTwinShape)
	}
	var refTable, updateRule, deleteRule string
	if err := conn.QueryRowContext(ctx, `
SELECT REFERENCED_TABLE_NAME, UPDATE_RULE, DELETE_RULE
FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS
WHERE CONSTRAINT_SCHEMA = DATABASE() AND CONSTRAINT_NAME = 'fk_comp_snap_issue'`).Scan(&refTable, &updateRule, &deleteRule); err != nil {
		t.Fatalf("read fk_comp_snap_issue: %v", err)
	}
	if refTable != "issues" || updateRule != "CASCADE" || deleteRule != "CASCADE" {
		t.Fatalf("fk_comp_snap_issue references %s ON UPDATE %s ON DELETE %s, want issues ON UPDATE CASCADE ON DELETE CASCADE", refTable, updateRule, deleteRule)
	}
}

// compSnapLatestTitle reads an issue's newest snapshot through the table's
// production reader.
func compSnapLatestTitle(t *testing.T, ctx context.Context, conn *sql.Conn, issueID string) string {
	t.Helper()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	snap, err := issueops.GetLatestSnapshotInTx(ctx, tx, issueID)
	if err != nil {
		t.Fatalf("GetLatestSnapshotInTx(%s): %v", issueID, err)
	}
	if snap == nil {
		t.Fatalf("GetLatestSnapshotInTx(%s) found no snapshot", issueID)
	}
	return snap.Title
}

// compSnapDescendingColumns lists every index column in the database that
// information_schema reports as stored descending.
func compSnapDescendingColumns(t *testing.T, ctx context.Context, conn *sql.Conn) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, `
SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME
FROM INFORMATION_SCHEMA.STATISTICS
WHERE TABLE_SCHEMA = DATABASE() AND COLLATION = 'D'
ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`)
	if err != nil {
		t.Fatalf("read descending index columns: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table, index, column string
		if err := rows.Scan(&table, &index, &column); err != nil {
			t.Fatalf("scan descending index column: %v", err)
		}
		out = append(out, strings.Join([]string{table, index, column}, "."))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read descending index columns: %v", err)
	}
	return out
}
