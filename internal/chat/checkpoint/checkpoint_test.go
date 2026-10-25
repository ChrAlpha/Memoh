package checkpoint

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

const (
	testCheckpointID = "00000000-0000-0000-0000-0000000000c1"
	testSessionID    = "00000000-0000-0000-0000-0000000000a1"
	testRowID        = "00000000-0000-0000-0000-0000000000b1"
	testSummaryID    = "00000000-0000-0000-0000-0000000000d1"
)

func TestFromRowReadsItemsInOrder(t *testing.T) {
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	row := sqlc.BotHistoryCheckpoint{
		ID:             dbpkg.ParseUUIDOrEmpty(testCheckpointID),
		SessionID:      dbpkg.ParseUUIDOrEmpty(testSessionID),
		TurnPosition:   4,
		TurnMessageSeq: 2,
		OmittedCount:   3,
		OmittedFirstAt: pgtype.Timestamptz{Time: first, Valid: true},
		OmittedLastAt:  pgtype.Timestamptz{Time: first.Add(time.Hour), Valid: true},
		Items:          []byte(`[{"row":" ` + testRowID + ` "},{"summary":"` + testSummaryID + `"}]`),
	}
	checkpoint, err := fromRow(row)
	if err != nil {
		t.Fatalf("fromRow: %v", err)
	}
	if checkpoint.ID != testCheckpointID || checkpoint.ParentID != "" || checkpoint.Position.Turn != 4 || checkpoint.Position.Seq != 2 {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
	if rows, summaries := checkpoint.RowIDs(), checkpoint.SummaryIDs(); len(rows) != 1 || rows[0] != testRowID || len(summaries) != 1 || summaries[0] != testSummaryID {
		t.Fatalf("rows %v summaries %v, want the trimmed row then the summary", rows, summaries)
	}
	if !checkpoint.Omitted.Omits() || checkpoint.Omitted.Count != 3 || !checkpoint.Omitted.First.Equal(first) {
		t.Fatalf("omitted = %+v", checkpoint.Omitted)
	}
}

func TestFromRowRefusesAnItemNamingNeitherOrBoth(t *testing.T) {
	for _, items := range []string{
		`[{}]`,
		`[{"row":"   "}]`,
		`[{"row":"` + testRowID + `","summary":"` + testSummaryID + `"}]`,
	} {
		_, err := fromRow(sqlc.BotHistoryCheckpoint{ID: dbpkg.ParseUUIDOrEmpty(testCheckpointID), Items: []byte(items)})
		if err == nil || !strings.Contains(err.Error(), "neither one row nor one summary") {
			t.Fatalf("fromRow(%s) = %v, want a refusal", items, err)
		}
	}
}

func TestEncodeItemsWritesAnArrayForNoItems(t *testing.T) {
	encoded, err := encodeItems(nil)
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("encodeItems(nil) = %s, %v", encoded, err)
	}
	encoded, err = encodeItems([]Item{{RowID: testRowID}, {SummaryID: testSummaryID, Summary: "not stored"}})
	if err != nil || string(encoded) != `[{"row":"`+testRowID+`"},{"summary":"`+testSummaryID+`"}]` {
		t.Fatalf("encodeItems = %s, %v", encoded, err)
	}
}

func TestOmittedOmitsOnlyWhatItCounts(t *testing.T) {
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		omitted Omitted
		want    bool
	}{
		{Omitted{}, false},
		{Omitted{Count: 2}, false},
		{Omitted{Count: 0, First: at, Last: at}, false},
		{Omitted{Count: 2, First: at, Last: at}, true},
	} {
		if got := tc.omitted.Omits(); got != tc.want {
			t.Fatalf("%+v.Omits() = %v, want %v", tc.omitted, got, tc.want)
		}
	}
}
