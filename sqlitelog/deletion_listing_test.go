package sqlitelog

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func listedUIDs(page SessionPage) []string {
	out := make([]string, 0, len(page.Sessions))
	for _, info := range page.Sessions {
		out = append(out, info.UID)
	}
	return out
}

// Filtering after LIMIT would produce holes, truncated pages and missing continuation tokens.
func TestDeletedListingFiltersBeforePagination(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hidden, want []string
	}{
		{"none", nil, []string{"a", "b", "c", "d", "e"}},
		{"first", []string{"a"}, []string{"b", "c", "d", "e"}},
		{"middle", []string{"b", "c"}, []string{"a", "d", "e"}},
		{"last", []string{"e"}, []string{"a", "b", "c", "d"}},
		{"all", []string{"a", "b", "c", "d", "e"}, []string{}},
		{"alternating", []string{"a", "c", "e"}, []string{"b", "d"}},
	} {
		for _, tied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tied=%v", tc.name, tied), func(t *testing.T) {
				s := deletionStore(t, ":memory:")
				base := time.Unix(1700000000, 123)
				for i, uid := range []string{"a", "b", "c", "d", "e"} {
					created := base
					if !tied {
						created = base.Add(-time.Duration(i) * time.Minute)
					}
					if err := s.PutSession(SessionMeta{UID: uid, CreatedAt: created}); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.PutSession(SessionMeta{UID: "foreign", Project: "other", CreatedAt: base.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				for _, uid := range tc.hidden {
					if _, err := s.DeleteSession(context.Background(), uid); err != nil {
						t.Fatal(err)
					}
				}
				// Future COMPLETE rows are hidden by the same ACTIVE-only listing contract.
				if len(tc.hidden) > 0 {
					if _, err := s.db.Exec(`UPDATE sessions SET deletion_state = 'COMPLETE' WHERE session = ?`, tc.hidden[0]); err != nil {
						t.Fatal(err)
					}
				}
				for _, size := range []int{1, 2, 3, 10} {
					seen := []string{}
					token := ""
					for n := 0; ; n++ {
						if n > 5 {
							t.Fatal("pagination did not terminate")
						}
						page, err := s.ListSessions(ListOptions{PageSize: size, PageToken: token})
						if err != nil {
							t.Fatal(err)
						}
						seen = append(seen, listedUIDs(page)...)
						if page.NextPageToken == "" {
							break
						}
						if len(page.Sessions) != size {
							t.Fatalf("intermediate page has hole: %+v", page)
						}
						token = page.NextPageToken
					}
					if !reflect.DeepEqual(seen, tc.want) {
						t.Fatalf("size %d: %v, want %v", size, seen, tc.want)
					}
				}
				foreign, err := s.ListSessions(ListOptions{Project: "other"})
				if err != nil || !reflect.DeepEqual(listedUIDs(foreign), []string{"foreign"}) {
					t.Fatalf("exact project = %+v, %v", foreign, err)
				}
			})
		}
	}
}

func TestDeletedBoundaryTokenStillResumesAcrossInserts(t *testing.T) {
	s := deletionStore(t, ":memory:")
	base := time.Unix(1700000000, 0)
	for i, uid := range []string{"a", "b", "c", "d", "e"} {
		if err := s.PutSession(SessionMeta{UID: uid, CreatedAt: base.Add(-time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListSessions(ListOptions{PageSize: 2})
	if err != nil || !reflect.DeepEqual(listedUIDs(first), []string{"a", "b"}) || first.NextPageToken == "" {
		t.Fatalf("first = %+v,%v", first, err)
	}
	for _, uid := range []string{"b", "c", "e"} {
		if _, err := s.DeleteSession(context.Background(), uid); err != nil {
			t.Fatal(err)
		}
	}
	// Newer insert must not repeat earlier rows; an older insert after the keyset belongs here.
	for _, m := range []SessionMeta{{UID: "newer", CreatedAt: base.Add(time.Hour)}, {UID: "between", CreatedAt: base.Add(-150 * time.Second)}} {
		if err := s.PutSession(m); err != nil {
			t.Fatal(err)
		}
	}
	second, err := s.ListSessions(ListOptions{PageSize: 2, PageToken: first.NextPageToken})
	if err != nil || !reflect.DeepEqual(listedUIDs(second), []string{"between", "d"}) || second.NextPageToken != "" {
		t.Fatalf("resume deleted cursor = %+v,%v", second, err)
	}
}

func TestDeletedListingDefaultAndMaximumSizes(t *testing.T) {
	s := deletionStore(t, ":memory:")
	base := time.Unix(1700000000, 0)
	for i := 0; i < 521; i++ {
		uid := fmt.Sprintf("s-%03d", i)
		if err := s.PutSession(SessionMeta{UID: uid, CreatedAt: base}); err != nil {
			t.Fatal(err)
		}
		if i < 20 {
			if _, err := s.DeleteSession(context.Background(), uid); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct{ size, want int }{{0, 50}, {-1, 50}, {501, 500}, {5000, 500}, {3, 3}} {
		page, err := s.ListSessions(ListOptions{PageSize: tc.size})
		if err != nil || len(page.Sessions) != tc.want || page.NextPageToken == "" {
			t.Fatalf("size %d = %d/%q,%v", tc.size, len(page.Sessions), page.NextPageToken, err)
		}
		if page.Sessions[0].UID != "s-020" || page.Sessions[tc.want-1].UID != fmt.Sprintf("s-%03d", 19+tc.want) {
			t.Fatalf("size %d didn't fill with active rows: %v", tc.size, listedUIDs(page))
		}
		last, err := s.ListSessions(ListOptions{PageSize: 5000, PageToken: page.NextPageToken})
		if err != nil || len(last.Sessions) != 501-tc.want || last.NextPageToken != "" {
			t.Fatalf("tail = %+v,%v", last, err)
		}
	}
}
