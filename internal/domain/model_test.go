package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkDetailsSerializesSeriesAndOptionalSeasonSummary(t *testing.T) {
	seasonCount := 0
	details := WorkDetails{
		Provider: "tvmaze", ExternalID: "42", Title: "Example", Medium: MediumVideo, WorkType: "series",
		SeasonCount: &seasonCount,
		Series: []WorkSeriesDetails{{
			CollectionID: "opaque-local-series", Title: "Example Series", Ordinal: nil, KnownTotal: nil,
			MoreInSeries: []WorkSeriesMember{},
		}},
	}
	body, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	series, ok := decoded["series"].([]any)
	if !ok || len(series) != 1 {
		t.Fatalf("series JSON = %#v, want one series record", decoded["series"])
	}
	entry := series[0].(map[string]any)
	if entry["collection_id"] != "opaque-local-series" || entry["ordinal"] != nil || entry["known_total"] != nil {
		t.Fatalf("series identity/nullable values = %#v", entry)
	}
	if members, ok := entry["more_in_series"].([]any); !ok || len(members) != 0 {
		t.Fatalf("empty More in this series list = %#v, want []", entry["more_in_series"])
	}
	if decoded["season_count"] != float64(0) {
		t.Fatalf("season_count = %#v, want valid zero count", decoded["season_count"])
	}
}

func TestMediumValid(t *testing.T) {
	tests := []struct {
		name   string
		medium Medium
		valid  bool
	}{
		{name: "game", medium: MediumGame, valid: true},
		{name: "video", medium: MediumVideo, valid: true},
		{name: "literature", medium: MediumLiterature, valid: true},
		{name: "audio", medium: MediumAudio, valid: true},
		{name: "edition format is not a broad medium", medium: "epub"},
		{name: "unknown", medium: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.medium.Valid(); got != tt.valid {
				t.Fatalf("Medium(%q).Valid() = %t, want %t", tt.medium, got, tt.valid)
			}
		})
	}
}

func TestWorkRelationValidRequiresOneTypedTargetAndPreservesTextOrdinal(t *testing.T) {
	workTarget := WorkID("work-two")
	seriesTarget := WorkCollectionID("series-a")
	ordinal := "5.0"
	tests := []struct {
		name     string
		relation WorkRelation
		valid    bool
	}{
		{
			name: "series membership retains provider ordinal text",
			relation: WorkRelation{
				WorkID: "work-one", Type: WorkRelationPartOfSeries, TargetCollectionID: &seriesTarget,
				Ordinal: &ordinal, Provenance: WorkRelationProvenanceProvider, Confidence: 0.9,
			},
			valid: true,
		},
		{
			name: "work relation can target a Work",
			relation: WorkRelation{
				WorkID: "work-one", Type: WorkRelationSequelOf, TargetWorkID: &workTarget,
				Provenance: WorkRelationProvenanceManual, Confidence: 1,
			},
			valid: true,
		},
		{
			name:     "missing target",
			relation: WorkRelation{WorkID: "work-one", Type: WorkRelationAdaptationOf, Provenance: WorkRelationProvenanceProvider},
		},
		{
			name: "two targets",
			relation: WorkRelation{
				WorkID: "work-one", Type: WorkRelationAdaptationOf, TargetWorkID: &workTarget,
				TargetCollectionID: &seriesTarget, Provenance: WorkRelationProvenanceProvider,
			},
		},
		{
			name:     "series membership must target a collection",
			relation: WorkRelation{WorkID: "work-one", Type: WorkRelationPartOfSeries, TargetWorkID: &workTarget, Provenance: WorkRelationProvenanceProvider},
		},
		{
			name:     "unknown relation type",
			relation: WorkRelation{WorkID: "work-one", Type: "related_to", TargetWorkID: &workTarget, Provenance: WorkRelationProvenanceProvider},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.relation.Valid(); got != test.valid {
				t.Fatalf("WorkRelation.Valid() = %t, want %t", got, test.valid)
			}
		})
	}
}

func TestWorkDetailsProvidesProviderNeutralBookAndTVPresentationFields(t *testing.T) {
	details := WorkDetails{
		Provider: "openlibrary", ExternalID: "OL123W", Title: "A Work", Medium: MediumLiterature, WorkType: "book",
		Language: "en", SourceURL: "https://openlibrary.org/works/OL123W",
		FallbackCoverReference: "https://covers.openlibrary.org/b/id/98-L.jpg",
		RepresentativeEdition: &RepresentativeEdition{
			ExternalID: "OL321M", Title: "A Work", Language: "en", PublicationDate: "1990",
			CoverReference: "https://covers.openlibrary.org/b/id/99-L.jpg",
		},
		Genres: []string{"Fantasy"}, Status: "Running", Network: "Public Network", WebChannel: "Public Channel",
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if normalized["external_id"] != "OL123W" || normalized["language"] != "en" || normalized["source_url"] != details.SourceURL {
		t.Fatalf("normalized top-level details = %s", encoded)
	}
	if normalized["fallback_cover_reference"] != details.FallbackCoverReference {
		t.Fatalf("normalized fallback cover reference = %s", encoded)
	}
	edition, ok := normalized["representative_edition"].(map[string]any)
	if !ok || edition["external_id"] != "OL321M" || edition["publication_date"] != "1990" {
		t.Fatalf("normalized edition metadata = %s", encoded)
	}
	if strings.Contains(string(encoded), "provider_payload") || strings.Contains(string(encoded), "raw_json") || strings.Contains(string(encoded), "html") {
		t.Fatalf("provider-specific payload leaked in normalized details: %s", encoded)
	}
}
