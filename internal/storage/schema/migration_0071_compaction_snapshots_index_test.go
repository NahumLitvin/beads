package schema

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

// Migration 0071 (gastownhall/beads#7646 review): 0010 declares
// compaction_snapshots' idx_comp_snap_issue with created_at DESC. Dolt 2.3.3
// and later store that column descending (dolthub/dolt#11746), and Dolt older
// than 2.3.3 cannot decode a table that holds one -- every INSERT and every
// information_schema query fails with "table has unknown fields", which takes
// released bd (Dolt 2.2.0 driver) down on open, past its schema-skew refusal
// and past BD_IGNORE_SCHEMA_SKEW=1. The DESC never helped a query, so 0071
// replaces the index with the all-ascending twin idx_comp_snap_issue_asc. The
// real-engine half of the pin (guards firing, a resumed pass, a replay, the
// foreign key still cascading) is
// TestEmbeddedMigration0071ReplacesDescendingCompactionSnapshotsIndex in
// internal/storage/embeddeddolt.
const migration0071Up = "0071_replace_descending_compaction_snapshots_index.up.sql"
const migration0071Down = "0071_replace_descending_compaction_snapshots_index.down.sql"

const migration0071OldIndexGuard = "@comp_snap_has_old_index"
const migration0071TwinIndexGuard = "@comp_snap_has_twin_index"

// TestLatestVersionIncludesMigration0071 pins the slot this migration claims,
// superseding 0070's version of this test. It is a hardcoded literal for the
// reason 0067's through 0070's were: LatestVersion() reaching 71 because some
// other migration landed there first should fail here, not pass silently.
// Migrations take slots in landing order: gastownhall/beads#6943 landed 0070
// first, so this one renumbered to 0071, and of two open PRs that both number
// 0071 the second to land renumbers again (versions must stay gap-free, see
// TestAllMigrationsSQLAppliesThroughDoltCLIAndRecordsLatestVersion).
func TestLatestVersionIncludesMigration0071(t *testing.T) {
	const want = 71
	if got := LatestVersion(); got != want {
		t.Fatalf("LatestVersion() = %d, want %d (compaction_snapshots ascending-index migration slot claimed by gastownhall/beads#7646)", got, want)
	}
}

// TestMigration0071ReplacesDescendingCompactionSnapshotsIndex is the pure-Go
// half of the pin: the frozen-to-be migration bytes, the absence of a CLI
// override, the inventory entry that justifies leaving the prepared DDL in
// the fresh bundle, and the down file. No `dolt` binary required.
func TestMigration0071ReplacesDescendingCompactionSnapshotsIndex(t *testing.T) {
	upSQL, err := MigrationSQL(migration0071Up)
	if err != nil {
		t.Fatalf("MigrationSQL(%s) error = %v, want the migration file to exist", migration0071Up, err)
	}
	for _, want := range []string{
		// The closing quote pins the twin's column list: no DESC, the
		// descending column Dolt older than 2.3.3 cannot read.
		"ALTER TABLE compaction_snapshots ADD INDEX idx_comp_snap_issue_asc (issue_id, compaction_level, created_at)'",
		"ALTER TABLE compaction_snapshots DROP INDEX idx_comp_snap_issue'",
		migration0071OldIndexGuard,
		migration0071TwinIndexGuard,
		"INFORMATION_SCHEMA.STATISTICS",
	} {
		if !strings.Contains(upSQL, want) {
			t.Errorf("0071 up migration missing %q\nfull SQL:\n%s", want, upSQL)
		}
	}
	if add, drop := strings.Index(upSQL, "ADD INDEX idx_comp_snap_issue_asc"), strings.Index(upSQL, "DROP INDEX idx_comp_snap_issue'"); add < 0 || drop < 0 || add > drop {
		t.Error("0071 up migration must add the twin before dropping idx_comp_snap_issue: fk_comp_snap_issue refuses to lose its only issue_id-leading index")
	}
	if !strings.Contains(strings.ToUpper(upSQL), "PREPARE STMT FROM @SQL") {
		t.Error("0071 up migration must keep its guarded PREPARE blocks -- they are what make a resumed pass and a raw .up.sql replay no-ops")
	}

	if got := cliCompatibleMigrationSQL(migration0071Up, upSQL); got != upSQL {
		t.Error("0071 has no CLI-bundle override: its prepared ADD INDEX and DROP INDEX execute through the dolt CLI batch path, so the bundle must carry the migration text unchanged")
	}
	if _, ok := preparedALTERSafeOnFreshBundle[migration0071Up]; !ok {
		t.Errorf("preparedALTERSafeOnFreshBundle must justify %s's prepared ALTERs staying in the fresh bundle", migration0071Up)
	}
	if cliSubstituteAssumesWispTables(migration0071Up) {
		t.Error("0071 touches only compaction_snapshots, which has no wisps-side twin -- it must not be listed in cliSubstituteAssumesWispTables")
	}

	// Only migrations/*.up.sql is //go:embed'd, so the down file is read
	// from disk by package-relative path.
	downBytes, err := os.ReadFile("migrations/" + migration0071Down)
	if err != nil {
		t.Fatalf("read %s: %v, want the migration file to exist", migration0071Down, err)
	}
	downSQL := string(downBytes)
	for _, want := range []string{
		"ALTER TABLE compaction_snapshots ADD INDEX idx_comp_snap_issue (issue_id, compaction_level, created_at DESC)'",
		"ALTER TABLE compaction_snapshots DROP INDEX idx_comp_snap_issue_asc'",
		migration0071OldIndexGuard,
		migration0071TwinIndexGuard,
	} {
		if !strings.Contains(downSQL, want) {
			t.Errorf("0071 down migration missing %q\nfull SQL:\n%s", want, downSQL)
		}
	}
	if add, drop := strings.Index(downSQL, "ADD INDEX idx_comp_snap_issue "), strings.Index(downSQL, "DROP INDEX idx_comp_snap_issue_asc"); add < 0 || drop < 0 || add > drop {
		t.Error("0071 down migration must restore idx_comp_snap_issue before dropping the twin, for the same foreign-key reason as the up file")
	}
}

// TestMigration0071LeavesNoDescendingIndexThroughDoltCLI builds the fresh
// bundle with the `dolt` on PATH (skipped without one) and reads the result
// from information_schema. On a CLI that persists DESC (2.3.3 and later) this
// is the case 0071 exists for; on an older CLI it shows the prepared ADD and
// DROP INDEX really executed, which is what preparedALTERSafeOnFreshBundle's
// entry claims.
func TestMigration0071LeavesNoDescendingIndexThroughDoltCLI(t *testing.T) {
	testutil.RequireDoltBinary(t)

	dir := t.TempDir()
	runDoltCommand(t, dir, "init", "--name", "test", "--email", "test@example.com")
	runDoltSQL(t, dir, AllMigrationsSQL())

	rows := queryDoltCSV(t, dir, `
SELECT index_name AS index_name, column_name AS column_name, collation AS collation
FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name = 'compaction_snapshots'
ORDER BY index_name, seq_in_index`)
	var twin []string
	for _, row := range rows {
		switch row["index_name"] {
		case "idx_comp_snap_issue":
			t.Errorf("idx_comp_snap_issue still exists after the bundle (column %s, collation %s); 0071 must drop it", row["column_name"], row["collation"])
		case "idx_comp_snap_issue_asc":
			twin = append(twin, row["column_name"]+":"+row["collation"])
		}
	}
	if got, want := strings.Join(twin, ","), "issue_id:A,compaction_level:A,created_at:A"; got != want {
		t.Errorf("idx_comp_snap_issue_asc columns = %q, want %q", got, want)
	}
	requireDoltFKRules(t, dir, "fk_comp_snap_issue", "CASCADE", "CASCADE")

	// The fresh schema as a whole: one descending index column anywhere makes
	// the database unreadable for Dolt older than 2.3.3, which is the failure
	// this migration removes. A new migration that wants DESC has to make that
	// trade-off on purpose and change this assertion.
	requireDoltNoRows(t, dir, `
SELECT table_name, index_name, column_name
FROM information_schema.statistics
WHERE table_schema = DATABASE() AND collation = 'D'`, "descending index columns in the fresh schema")
}
