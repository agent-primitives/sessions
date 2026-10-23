package sqlitelog

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Late pages must seek past their cursor, not scan and sort the same inventory again.
func TestAllocationPageSeeksCursor(t *testing.T) {
	s := deletionStore(t, filepath.Join(t.TempDir(), "pages.db"))
	if _, err := s.db.Exec(`WITH RECURSIVE attempts(n) AS (
		VALUES(0) UNION ALL SELECT n+1 FROM attempts WHERE n<4095
	) INSERT INTO allocations(id,session,backend,operation,released)
	SELECT printf('%08d',n),'target','route','create',n%17=0 FROM attempts`); err != nil {
		t.Fatal(err)
	}
	// Explain AllocationPage's query against a populated store so a missing cursor index cannot
	// hide behind a small functional fixture. Avoid timing thresholds that depend on host load.
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN
		SELECT id,session,backend,operation,resource_id,confirmed FROM allocations
		WHERE session=? AND released=0 AND id>? ORDER BY id LIMIT ?`, "target", "00004000", 10)
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "; ")
	t.Logf("late-page plan: %s", plan)
	if strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "id>?") {
		t.Errorf("allocation page does not seek its cursor without sorting: %s", plan)
	}
	page, err := s.AllocationPage(t.Context(), "target", "00004000", 10)
	if err != nil || len(page) != 10 {
		t.Fatalf("late page = %+v, %v", page, err)
	}
	for i, a := range page {
		if want := fmt.Sprintf("%08d", 4001+i); a.ID != want {
			t.Fatalf("page[%d].ID = %q, want %q", i, a.ID, want)
		}
	}
}
