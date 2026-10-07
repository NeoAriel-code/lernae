package catalog

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"lernae/internal/database"
	"lernae/internal/domain"
)

func TestGetWorkSeriesDetailsUsesExactIdentityAndReturnsOrderedCappedPeers(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "series-details.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	collection := domain.WorkCollection{ID: "opaque-local-series", Title: "A Different Series Label", Type: domain.WorkCollectionTypeSeries}
	if err := repository.CreateWorkCollection(ctx, collection); err != nil {
		t.Fatal(err)
	}
	materialize := func(provider, externalID, title string) domain.WorkID {
		t.Helper()
		work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
			Provider: provider, ExternalID: externalID, Medium: domain.MediumLiterature, WorkType: "book", Title: title,
		})
		if err != nil {
			t.Fatal(err)
		}
		return work.Work.ID
	}
	setMembership := func(workID domain.WorkID, ordinal *string, provenance domain.WorkRelationProvenance) {
		t.Helper()
		_, err := repository.SetWorkRelation(ctx, domain.WorkRelation{
			WorkID: workID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID,
			Ordinal: ordinal, Provenance: provenance, Confidence: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	currentID := materialize("openlibrary", "OL-currentW", "Same Author Book 15")
	currentOrdinal := "15"
	setMembership(currentID, &currentOrdinal, domain.WorkRelationProvenanceManual)
	for ordinal := 1; ordinal <= 22; ordinal++ {
		value := strconv.Itoa(ordinal)
		materialize("openlibrary", "OL-peer"+value+"W", "Series Peer "+value)
		peer, err := repository.GetWorkByExternalIdentity(ctx, "openlibrary", "OL-peer"+value+"W")
		if err != nil {
			t.Fatal(err)
		}
		setMembership(peer.Work.ID, &value, domain.WorkRelationProvenanceProvider)
	}
	tiedID := materialize("openlibrary", "OL-peer2bW", "Second Series Peer")
	tiedOrdinal := "2"
	setMembership(tiedID, &tiedOrdinal, domain.WorkRelationProvenanceProvider)
	unordinalID := materialize("openlibrary", "OL-unordinalW", "Unnumbered Series Peer")
	setMembership(unordinalID, nil, domain.WorkRelationProvenanceProvider)
	unrelatedID := materialize("openlibrary", "OL-unrelatedW", "Same Author Book 16")
	otherSeriesRelation := domain.WorkRelation{
		WorkID: unrelatedID, Type: domain.WorkRelationFranchiseMember, TargetCollectionID: &collection.ID,
		Provenance: domain.WorkRelationProvenanceProvider, Confidence: 1,
	}
	if _, err := repository.SetWorkRelation(ctx, otherSeriesRelation); err != nil {
		t.Fatal(err)
	}

	got, truncated, err := repository.GetWorkSeriesDetails(ctx, "openlibrary", "OL-currentW")
	if err != nil {
		t.Fatalf("GetWorkSeriesDetails() error = %v", err)
	}
	if len(got) != 1 || got[0].CollectionID != collection.ID || got[0].Title != collection.Title {
		t.Fatalf("series identity = %#v, want the opaque local collection identity and title", got)
	}
	if truncated || got[0].Ordinal == nil || *got[0].Ordinal != currentOrdinal || got[0].KnownTotal == nil || *got[0].KnownTotal != 25 {
		t.Fatalf("series ordinal/known total/truncation = %#v/%t, want 15/25/false", got[0], truncated)
	}
	if len(got[0].MoreInSeries) != maxSeriesDetailMembers || !got[0].MoreInSeriesTruncated {
		t.Fatalf("bounded peer list = %d truncated=%t, want cap %d and truncated", len(got[0].MoreInSeries), got[0].MoreInSeriesTruncated, maxSeriesDetailMembers)
	}
	if got[0].MoreInSeries[0].Ordinal == nil || *got[0].MoreInSeries[0].Ordinal != "1" ||
		got[0].MoreInSeries[1].Ordinal == nil || *got[0].MoreInSeries[1].Ordinal != "2" ||
		got[0].MoreInSeries[1].ExternalID != "OL-peer2W" || got[0].MoreInSeries[2].ExternalID != "OL-peer2bW" {
		t.Fatalf("series peers are not ordered by ordinal and stable identity: %#v", got[0].MoreInSeries[:3])
	}
	for _, member := range got[0].MoreInSeries {
		if member.ExternalID == "OL-currentW" || member.ExternalID == "OL-unrelatedW" {
			t.Fatalf("series peer list included current or same-author-only work: %#v", member)
		}
	}
	relations, err := repository.GetWorkRelations(ctx, currentID)
	if err != nil || len(relations) != 1 || !relations[0].ConfirmedByUser || relations[0].Provenance != domain.WorkRelationProvenanceManual {
		t.Fatalf("reading series details changed manual membership: %#v, %v", relations, err)
	}

	missing, missingTruncated, err := repository.GetWorkSeriesDetails(ctx, "openlibrary", "OL-unknownW")
	if err != nil || missingTruncated || len(missing) != 0 {
		t.Fatalf("unknown exact identity got series details %#v (truncated=%t), err=%v; want none", missing, missingTruncated, err)
	}
}

func TestGetWorkSeriesDetailsOmitsPeersWithoutExternalIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "series-details-identity-less-peer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	collection := domain.WorkCollection{ID: "identity-safe-series", Title: "Identity Safe Series", Type: domain.WorkCollectionTypeSeries}
	if err := repository.CreateWorkCollection(ctx, collection); err != nil {
		t.Fatal(err)
	}
	materialize := func(provider, externalID, title string) domain.WorkID {
		t.Helper()
		work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
			Provider: provider, ExternalID: externalID, Medium: domain.MediumLiterature, WorkType: "book", Title: title,
		})
		if err != nil {
			t.Fatal(err)
		}
		return work.Work.ID
	}
	setMembership := func(workID domain.WorkID) {
		t.Helper()
		_, err := repository.SetWorkRelation(ctx, domain.WorkRelation{
			WorkID: workID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID,
			Provenance: domain.WorkRelationProvenanceProvider, Confidence: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	currentWorkID := materialize("openlibrary", "OL-currentIdentityW", "Current Series Work")
	setMembership(currentWorkID)
	identityLessPeerID := domain.WorkID("identity-less-series-peer")
	if err := repository.CreateGraph(ctx, WorkGraph{Work: domain.Work{
		ID: identityLessPeerID, Title: "Unaddressable Series Work", Medium: domain.MediumLiterature, WorkType: "book",
	}}); err != nil {
		t.Fatalf("create identity-less Series peer: %v", err)
	}
	setMembership(identityLessPeerID)
	addressablePeerID := materialize("openlibrary", "OL-addressablePeerW", "Addressable Series Work")
	setMembership(addressablePeerID)

	got, truncated, err := repository.GetWorkSeriesDetails(ctx, "openlibrary", "OL-currentIdentityW")
	if err != nil {
		t.Fatalf("GetWorkSeriesDetails() error = %v", err)
	}
	if truncated || len(got) != 1 || got[0].KnownTotal == nil || *got[0].KnownTotal != 3 {
		t.Fatalf("Series count/truncation = %#v/%t, want three persisted memberships and no collection truncation", got, truncated)
	}
	if len(got[0].MoreInSeries) != 1 || got[0].MoreInSeriesTruncated {
		t.Fatalf("displayable peers = %#v truncated=%t, want only the one identity-bearing peer", got[0].MoreInSeries, got[0].MoreInSeriesTruncated)
	}
	member := got[0].MoreInSeries[0]
	if member.Provider != "openlibrary" || member.ExternalID != "OL-addressablePeerW" {
		t.Fatalf("Series peer identity = %#v, want the exact addressable peer", member)
	}
	for _, member := range got[0].MoreInSeries {
		if member.Provider == "" || member.ExternalID == "" {
			t.Fatalf("Series Details exposed an empty external identity: %#v", member)
		}
	}
}

func TestGetWorkSeriesDetailsOrdersExplicitTextOrdinalBeforeUnknownOrdinal(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "series-details-unknown-ordinal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository := NewSQLiteRepository(db)
	collection := domain.WorkCollection{ID: "series-with-unknown-ordinal", Title: "Ordinal Series", Type: domain.WorkCollectionTypeSeries}
	if err := repository.CreateWorkCollection(ctx, collection); err != nil {
		t.Fatal(err)
	}
	materialize := func(externalID string) domain.WorkID {
		t.Helper()
		work, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
			Provider: "fixture", ExternalID: externalID, Title: externalID, Medium: domain.MediumLiterature, WorkType: "book",
		})
		if err != nil {
			t.Fatal(err)
		}
		return work.Work.ID
	}
	currentID := materialize("current")
	textID := materialize("text-ordinal")
	nilID := materialize("nil-ordinal")
	textOrdinal := "Special"
	for _, relation := range []domain.WorkRelation{
		{WorkID: currentID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID, Provenance: domain.WorkRelationProvenanceManual, Confidence: 1},
		{WorkID: textID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID, Ordinal: &textOrdinal, Provenance: domain.WorkRelationProvenanceProvider, Confidence: 1},
		{WorkID: nilID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID, Provenance: domain.WorkRelationProvenanceProvider, Confidence: 1},
	} {
		if _, err := repository.SetWorkRelation(ctx, relation); err != nil {
			t.Fatal(err)
		}
	}
	series, _, err := repository.GetWorkSeriesDetails(ctx, "fixture", "current")
	if err != nil || len(series) != 1 || series[0].Ordinal != nil || series[0].KnownTotal == nil || *series[0].KnownTotal != 3 {
		t.Fatalf("nullable series summary = %#v, err=%v", series, err)
	}
	if len(series[0].MoreInSeries) != 2 || series[0].MoreInSeries[0].ExternalID != "text-ordinal" ||
		series[0].MoreInSeries[0].Ordinal == nil || *series[0].MoreInSeries[0].Ordinal != textOrdinal ||
		series[0].MoreInSeries[1].ExternalID != "nil-ordinal" || series[0].MoreInSeries[1].Ordinal != nil {
		t.Fatalf("explicit/unknown ordinal order = %#v", series[0].MoreInSeries)
	}
}
