package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/providers"
	"lernae/internal/providers/wikidata"
)

type relationshipMetadataStub struct {
	candidates []wikidata.EntityCandidate
	entities   map[string]providers.RelationshipMetadata
	searches   int
	fetches    []string
	searchErr  error
	fetchErr   map[string]error
}

func (stub *relationshipMetadataStub) SearchEntities(_ context.Context, query, language string, limit int) ([]wikidata.EntityCandidate, error) {
	stub.searches++
	if stub.searchErr != nil {
		return nil, stub.searchErr
	}
	if limit > 10 {
		return nil, fmt.Errorf("unexpected search limit %d", limit)
	}
	return append([]wikidata.EntityCandidate(nil), stub.candidates...), nil
}

func (stub *relationshipMetadataStub) FetchEntity(_ context.Context, qid string, _ []string) (providers.RelationshipMetadata, error) {
	stub.fetches = append(stub.fetches, qid)
	if err := stub.fetchErr[qid]; err != nil {
		return providers.RelationshipMetadata{}, err
	}
	metadata, ok := stub.entities[qid]
	if !ok {
		return providers.RelationshipMetadata{}, fmt.Errorf("unexpected entity request %s", qid)
	}
	return metadata, nil
}

func TestRelationshipResolverAddsLocalizedAliasToExistingUniverseAndKeepsCollectionIdentitySeparate(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	universeID := domain.UniverseID("got-universe")
	createRelationshipUniverse(t, repository, universeID, "Game of Thrones")
	book := materializeRelationshipWork(t, repository, "OL100W", "A Game of Thrones")
	root := relationshipMetadata("Q100", []providers.RelationshipLocalizedValue{{Language: "en", Value: "Game of Thrones"}, {Language: "es", Value: "Juego de Tronos"}}, nil,
		claim("P527", "Q100", "Q101", nil))
	collection := relationshipMetadata("Q200", []providers.RelationshipLocalizedValue{{Language: "en", Value: "A Song of Ice and Fire"}, {Language: "es", Value: "Canción de hielo y fuego"}}, nil)
	item := relationshipMetadata("Q101", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL100W"}},
		claim("P179", "Q101", "Q200", stringPointer("1")))
	resolver.source = &relationshipMetadataStub{
		candidates: []wikidata.EntityCandidate{{ID: "Q100", Label: "Game of Thrones", Language: "en"}, {ID: "Q100", Label: "Juego de Tronos", Language: "es"}},
		entities:   map[string]providers.RelationshipMetadata{"Q100": root, "Q101": item, "Q200": collection}, fetchErr: map[string]error{},
	}
	result, err := resolver.ResolveByUniverseID(ctx, universeID, "en")
	if err != nil {
		t.Fatalf("ResolveByUniverseID() error = %v", err)
	}
	if result.UniverseID != universeID || result.AliasesAdded != 1 {
		t.Fatalf("resolution result = %#v, want existing Universe and one localized alias", result)
	}
	graph, err := repository.GetUniverse(ctx, universeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Aliases) != 1 || graph.Aliases[0].Alias != "Juego de Tronos" || graph.Aliases[0].Language == nil || *graph.Aliases[0].Language != "es" {
		t.Fatalf("Universe aliases = %#v, want Juego de Tronos on existing ID", graph.Aliases)
	}
	collectionID, err := repository.GetWorkCollectionByExternalIdentity(ctx, "wikidata", "Q200")
	if err != nil {
		t.Fatal(err)
	}
	relations, err := repository.GetWorkRelations(ctx, book.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(relations) != 1 || relations[0].Type != domain.WorkRelationPartOfSeries || relations[0].TargetCollectionID == nil || *relations[0].TargetCollectionID != collectionID.ID || relations[0].Ordinal == nil || *relations[0].Ordinal != "1" {
		t.Fatalf("book relations = %#v, want exact ordered collection membership", relations)
	}
	if _, err := resolver.ResolveByExactName(ctx, "Juego de Tronos", "es"); err != nil {
		t.Fatalf("resolve exact localized alias: %v", err)
	}
	again, err := repository.GetUniverse(ctx, universeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Aliases) != 1 || again.Universe.ID != universeID || collectionID.Title != "A Song of Ice and Fire" {
		t.Fatalf("refresh duplicated/replaced identity: graph=%#v collection=%#v", again, collectionID)
	}
	for _, alias := range again.Aliases {
		if strings.Contains(alias.Alias, "Song of Ice") || alias.Alias == "Canción de hielo y fuego" {
			t.Fatalf("series alias leaked onto umbrella Universe: %#v", alias)
		}
	}
}

func TestRelationshipResolverStoresWheelOfTimeOrdinalsAndTypedEdges(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	universeID := domain.UniverseID("wheel-universe")
	createRelationshipUniverse(t, repository, universeID, "The Wheel of Time")
	one := materializeRelationshipWork(t, repository, "OL201W", "The Eye of the World")
	two := materializeRelationshipWork(t, repository, "OL202W", "The Great Hunt")
	film := materializeRelationshipWork(t, repository, "OL203W", "The Wheel of Time Film")
	root := relationshipMetadata("Q300", nil, nil, claim("P527", "Q300", "Q301", nil), claim("P527", "Q300", "Q302", nil), claim("P527", "Q300", "Q303", nil))
	metadata := map[string]providers.RelationshipMetadata{
		"Q300": root,
		"Q301": relationshipMetadata("Q301", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL201W"}}, claim("P179", "Q301", "Q310", stringPointer("1")), claim("P144", "Q301", "Q303", nil)),
		"Q302": relationshipMetadata("Q302", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL202W"}}, claim("P179", "Q302", "Q310", stringPointer("2")), claim("P155", "Q302", "Q301", nil)),
		"Q303": relationshipMetadata("Q303", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL203W"}}, claim("P527", "Q303", "Q301", nil)),
		"Q310": relationshipMetadata("Q310", []providers.RelationshipLocalizedValue{{Language: "en", Value: "The Wheel of Time"}}, nil),
	}
	resolver.source = &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q300", Label: "The Wheel of Time", Language: "en"}}, entities: metadata, fetchErr: map[string]error{}}
	if _, err := resolver.ResolveByUniverseID(ctx, universeID, "en"); err != nil {
		t.Fatalf("ResolveByUniverseID() error = %v", err)
	}
	collection, err := repository.GetWorkCollectionByExternalIdentity(ctx, "wikidata", "Q310")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		work    domain.WorkID
		ordinal string
	}{{one.Work.ID, "1"}, {two.Work.ID, "2"}} {
		relations, err := repository.GetWorkRelations(ctx, test.work)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, relation := range relations {
			if relation.Type == domain.WorkRelationPartOfSeries && relation.TargetCollectionID != nil && *relation.TargetCollectionID == collection.ID {
				found = relation.Ordinal != nil && *relation.Ordinal == test.ordinal
			}
		}
		if !found {
			t.Errorf("work %q has relations %#v, want part_of_series ordinal %q", test.work, relations, test.ordinal)
		}
	}
	previous, err := repository.GetWorkRelations(ctx, two.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTargetRelation(previous, domain.WorkRelationFollows, one.Work.ID) {
		t.Errorf("P155 relations = %#v, want explicit follows edge to preceding Work", previous)
	}
	adaptation, err := repository.GetWorkRelations(ctx, one.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTargetRelation(adaptation, domain.WorkRelationAdaptationOf, film.Work.ID) {
		t.Errorf("P144 relations = %#v, want explicit adaptation edge to exact Work", adaptation)
	}
	for _, relation := range append(previous, adaptation...) {
		if (relation.Type == domain.WorkRelationFollows || relation.Type == domain.WorkRelationAdaptationOf) && relation.Ordinal != nil {
			t.Errorf("typed non-series relation has ordinal: %#v", relation)
		}
	}
	if suggestions, err := repository.GetUniverse(ctx, universeID); err != nil {
		t.Fatal(err)
	} else if len(suggestions.Suggestions) == 0 || suggestions.Memberships != nil && len(suggestions.Memberships) != 0 {
		t.Errorf("P527 must remain suggestions, graph = %#v", suggestions)
	}
}

func TestRelationshipResolverHonorsManualRelationsAliasesAndUniverseExclusions(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	universeID := domain.UniverseID("authority-universe")
	createRelationshipUniverse(t, repository, universeID, "Game of Thrones")
	manual := materializeRelationshipWork(t, repository, "OL300W", "Manual Series Work")
	excluded := materializeRelationshipWork(t, repository, "OL301W", "Excluded Work")
	collection, err := repository.CreateOrGetWorkCollectionByExternalIdentity(ctx, "wikidata", "Q420", "Existing Series", domain.WorkCollectionTypeSeries)
	if err != nil {
		t.Fatal(err)
	}
	manualRelation := domain.WorkRelation{WorkID: manual.Work.ID, Type: domain.WorkRelationPartOfSeries, TargetCollectionID: &collection.ID, Ordinal: stringPointer("manual-ordinal"), Provenance: domain.WorkRelationProvenanceManual, Confidence: 1, Evidence: "user-confirmed"}
	if _, err := repository.SetWorkRelation(ctx, manualRelation); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AddUniverseAlias(ctx, domain.UniverseAlias{UniverseID: universeID, Alias: "Juego de Tronos", Language: stringPointer("es"), Provenance: string(domain.UniverseMembershipProvenanceManual), Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RemoveUniverseMembership(ctx, excluded.Work.ID, universeID, "explicitly unrelated"); err != nil {
		t.Fatal(err)
	}
	root := relationshipMetadata("Q400", []providers.RelationshipLocalizedValue{{Language: "es", Value: "Juego de Tronos"}}, nil,
		claim("P527", "Q400", "Q401", nil), claim("P527", "Q400", "Q402", nil))
	manualMeta := relationshipMetadata("Q401", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL300W"}}, claim("P179", "Q401", "Q420", stringPointer("provider-ordinal")))
	excludedMeta := relationshipMetadata("Q402", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL301W"}})
	resolver.source = &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q400", Label: "Game of Thrones", Language: "en"}}, entities: map[string]providers.RelationshipMetadata{"Q400": root, "Q401": manualMeta, "Q402": excludedMeta, "Q420": relationshipMetadata("Q420", []providers.RelationshipLocalizedValue{{Language: "en", Value: "Existing Series"}}, nil)}, fetchErr: map[string]error{}}
	if _, err := resolver.ResolveByUniverseID(ctx, universeID, "en"); err != nil {
		t.Fatalf("ResolveByUniverseID() error = %v", err)
	}
	relations, err := repository.GetWorkRelations(ctx, manual.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(relations) != 1 || relations[0].Provenance != domain.WorkRelationProvenanceManual || relations[0].Ordinal == nil || *relations[0].Ordinal != "manual-ordinal" {
		t.Errorf("provider refresh replaced manual relation: %#v", relations)
	}
	graph, err := repository.GetUniverse(ctx, universeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Aliases) != 1 || graph.Aliases[0].Provenance != string(domain.UniverseMembershipProvenanceManual) {
		t.Errorf("provider refresh replaced manual localized alias: %#v", graph.Aliases)
	}
	for _, suggestion := range graph.Suggestions {
		if suggestion.WorkID == excluded.Work.ID {
			t.Errorf("provider suggestion bypassed exclusion: %#v", suggestion)
		}
	}
}

func TestRelationshipResolverBoundsRequestsNodesEdgesAndDepth(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	createRelationshipUniverse(t, repository, "bounded", "Bounded Universe")
	claims := make([]providers.RelationshipClaim, 0, 70)
	entities := make(map[string]providers.RelationshipMetadata)
	for i := 0; i < 70; i++ {
		qid := fmt.Sprintf("Q%d", 501+i)
		claims = append(claims, claim("P527", "Q500", qid, nil))
		entities[qid] = relationshipMetadata(qid, nil, nil)
	}
	entities["Q500"] = relationshipMetadata("Q500", nil, nil, claims...)
	stub := &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q500", Label: "Bounded Universe", Language: "en"}}, entities: entities, fetchErr: map[string]error{}}
	resolver.source = stub
	result, err := resolver.ResolveByUniverseID(ctx, "bounded", "en")
	if err != nil {
		t.Fatalf("ResolveByUniverseID() error = %v", err)
	}
	if stub.searches > RelationshipGraphMaxRequests || len(stub.fetches)+stub.searches > RelationshipGraphMaxRequests {
		t.Fatalf("provider requests = %d search + %d fetch, want <= %d", stub.searches, len(stub.fetches), RelationshipGraphMaxRequests)
	}
	if result.NodesVisited > RelationshipGraphMaxNodes || result.EdgesVisited > RelationshipGraphMaxEdges || result.MaxDepth > RelationshipGraphMaxDepth {
		t.Fatalf("traversal exceeded bounds: %#v", result)
	}
	if result.RequestsUsed != stub.searches+len(stub.fetches) {
		t.Fatalf("reported requests = %d, observed = %d", result.RequestsUsed, stub.searches+len(stub.fetches))
	}
	if !result.BudgetLimited || result.RequestsUsed != RelationshipGraphMaxRequests {
		t.Fatalf("provider request cap result = %#v; want exact request-limit stop", result)
	}

	cache := NewSQLiteRelationshipMetadataCache(db)
	resolver.cache = cache
	for i := 0; i < 70; i++ {
		qid := fmt.Sprintf("Q%d", 601+i)
		metadata := relationshipMetadata(qid, nil, nil)
		if err := cache.Put(ctx, metadata, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	nodeClaims := make([]providers.RelationshipClaim, 0, 70)
	for i := 0; i < 70; i++ {
		qid := fmt.Sprintf("Q%d", 601+i)
		nodeClaims = append(nodeClaims, claim("P527", "Q600", qid, nil))
	}
	nodeStub := &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q600", Label: "Bounded Universe", Language: "en"}}, entities: map[string]providers.RelationshipMetadata{"Q600": relationshipMetadata("Q600", nil, nil, nodeClaims...)}, fetchErr: map[string]error{}}
	resolver.source = nodeStub
	createRelationshipUniverse(t, repository, "node-bounded", "Node Bounded")
	// The cached graph keeps the provider request count low so the node limit is independently exercised.
	nodeStub.candidates[0].Label = "Node Bounded"
	nodeResult, err := resolver.ResolveByUniverseID(ctx, "node-bounded", "en")
	if err != nil {
		t.Fatalf("node-bounded ResolveByUniverseID() error = %v", err)
	}
	if nodeResult.NodesVisited != RelationshipGraphMaxNodes || !nodeResult.BudgetLimited || nodeResult.RequestsUsed != 2 {
		t.Fatalf("cached node cap result = %#v, requests=%d; want node stop after two provider requests", nodeResult, nodeStub.searches+len(nodeStub.fetches))
	}

	edgeClaims := make([]providers.RelationshipClaim, 0, 70)
	for i := 0; i < 70; i++ {
		edgeClaims = append(edgeClaims, claim("P527", "Q700", "Q699", nil))
	}
	if err := cache.Put(ctx, relationshipMetadata("Q699", nil, nil), time.Hour); err != nil {
		t.Fatal(err)
	}
	edgeStub := &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q700", Label: "Edge Bounded", Language: "en"}}, entities: map[string]providers.RelationshipMetadata{"Q700": relationshipMetadata("Q700", nil, nil, edgeClaims...)}, fetchErr: map[string]error{}}
	resolver.source = edgeStub
	createRelationshipUniverse(t, repository, "edge-bounded", "Edge Bounded")
	edgeResult, err := resolver.ResolveByUniverseID(ctx, "edge-bounded", "en")
	if err != nil {
		t.Fatalf("edge-bounded ResolveByUniverseID() error = %v", err)
	}
	if edgeResult.EdgesVisited != RelationshipGraphMaxEdges || !edgeResult.BudgetLimited || edgeResult.NodesVisited != 2 {
		t.Fatalf("cached edge cap result = %#v", edgeResult)
	}
}

func TestRelationshipResolverDoesNotFetchBeyondDepthLimit(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	universeID := domain.UniverseID("depth-universe")
	createRelationshipUniverse(t, repository, universeID, "Depth Saga")
	materializeRelationshipWork(t, repository, "OL801W", "Depth One")
	materializeRelationshipWork(t, repository, "OL802W", "Depth Two")
	root := relationshipMetadata("Q800", nil, nil, claim("P527", "Q800", "Q801", nil))
	first := relationshipMetadata("Q801", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL801W"}}, claim("P527", "Q801", "Q802", nil))
	second := relationshipMetadata("Q802", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL802W"}}, claim("P527", "Q802", "Q803", nil))
	third := relationshipMetadata("Q803", nil, nil)
	stub := &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q800", Label: "Depth Saga", Language: "en"}}, entities: map[string]providers.RelationshipMetadata{"Q800": root, "Q801": first, "Q802": second, "Q803": third}, fetchErr: map[string]error{}}
	resolver.source = stub
	result, err := resolver.ResolveByUniverseID(ctx, universeID, "en")
	if err != nil {
		t.Fatalf("ResolveByUniverseID() error = %v", err)
	}
	if result.MaxDepth != RelationshipGraphMaxDepth || !result.BudgetLimited {
		t.Fatalf("depth result = %#v; want bounded stop at depth %d", result, RelationshipGraphMaxDepth)
	}
	for _, qid := range stub.fetches {
		if qid == "Q803" {
			t.Fatalf("resolver fetched depth-three entity Q803: requests=%v", stub.fetches)
		}
	}
}

func TestRelationshipResolverFailsClosedForAmbiguousOrMalformedProviderCandidates(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	createRelationshipUniverse(t, repository, "ambiguous", "Juego de Tronos")
	stub := &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q601", Label: "Juego de Tronos"}, {ID: "Q602", Label: "Juego de Tronos"}}, entities: map[string]providers.RelationshipMetadata{}, fetchErr: map[string]error{}}
	resolver.source = stub
	if _, err := resolver.ResolveByExactName(ctx, "Juego de Tronos", "es"); !errors.Is(err, ErrRelationshipAnchorAmbiguous) {
		t.Fatalf("ambiguous exact provider candidates error = %v, want ErrRelationshipAnchorAmbiguous", err)
	}
	if len(stub.fetches) != 0 {
		t.Fatalf("ambiguous candidates triggered entity fetches: %#v", stub.fetches)
	}
	bad := relationshipMetadata("Q601", []providers.RelationshipLocalizedValue{{Language: "es", Value: "Juego de Tronos"}}, nil,
		claim("P999", "Q601", "Q602", nil))
	stub = &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q601", Label: "Juego de Tronos", Language: "es"}}, entities: map[string]providers.RelationshipMetadata{"Q601": bad}, fetchErr: map[string]error{}}
	resolver.source = stub
	if _, err := resolver.ResolveByExactName(ctx, "Juego de Tronos", "es"); !errors.Is(err, providers.ErrInvalidRelationshipMetadata) {
		t.Fatalf("malformed provider metadata error = %v, want validation failure", err)
	}
	graph, err := repository.GetUniverse(ctx, "ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Aliases) != 0 {
		t.Fatalf("malformed response partially wrote aliases: %#v", graph.Aliases)
	}
}

func TestRelationshipResolverUsesExactWorkIdentityAndNeverTextCollisions(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	universeID := domain.UniverseID("collision-universe")
	createRelationshipUniverse(t, repository, universeID, "The Wheel of Time")
	known := materializeRelationshipWork(t, repository, "OL701W", "The Eye of the World")
	root := relationshipMetadata("Q700", nil, nil, claim("P527", "Q700", "Q701", nil), claim("P527", "Q700", "Q702", nil))
	knownEntity := relationshipMetadata("Q701", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL701W"}})
	textCollision := relationshipMetadata("Q702", []providers.RelationshipLocalizedValue{{Language: "en", Value: "The Eye of the World Companion"}}, nil)
	resolver.source = &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q700", Label: "The Wheel of Time", Language: "en"}}, entities: map[string]providers.RelationshipMetadata{"Q700": root, "Q701": knownEntity, "Q702": textCollision}, fetchErr: map[string]error{}}
	if _, err := resolver.ResolveByUniverseID(ctx, universeID, "en"); err != nil {
		t.Fatalf("ResolveByUniverseID() error = %v", err)
	}
	graph, err := repository.GetUniverse(ctx, universeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Suggestions) != 1 || graph.Suggestions[0].WorkID != known.Work.ID {
		t.Fatalf("text collision created a local suggestion: %#v", graph.Suggestions)
	}
}

func TestRelationshipResolverFailsClosedWhenSeriesCollectionIDCollides(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	universeID := domain.UniverseID("collision-series-universe")
	createRelationshipUniverse(t, repository, universeID, "Collision Saga")
	work := materializeRelationshipWork(t, repository, "OL901W", "Collision Saga Book")
	const seriesQID = "Q901"
	collidingID := domain.WorkCollectionID(stableCatalogID("collection", "wikidata", seriesQID))
	if err := repository.CreateWorkCollection(ctx, domain.WorkCollection{ID: collidingID, Title: "Unrelated manual collection", Type: domain.WorkCollectionTypeSeries}); err != nil {
		t.Fatal(err)
	}
	root := relationshipMetadata("Q900", nil, nil, claim("P527", "Q900", "Q902", nil))
	book := relationshipMetadata("Q902", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL901W"}}, claim("P179", "Q902", seriesQID, stringPointer("1")))
	series := relationshipMetadata(seriesQID, []providers.RelationshipLocalizedValue{{Language: "en", Value: "Actual Series"}}, nil)
	resolver.source = &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q900", Label: "Collision Saga", Language: "en"}}, entities: map[string]providers.RelationshipMetadata{"Q900": root, "Q902": book, seriesQID: series}, fetchErr: map[string]error{}}
	if _, err := resolver.ResolveByUniverseID(ctx, universeID, "en"); err != nil {
		t.Fatalf("ResolveByUniverseID() error = %v", err)
	}
	if _, err := repository.GetWorkCollectionByExternalIdentity(ctx, "wikidata", seriesQID); !errors.Is(err, ErrWorkCollectionNotFound) {
		t.Fatalf("colliding local collection acquired a Wikidata identity: error = %v", err)
	}
	relations, err := repository.GetWorkRelations(ctx, work.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(relations) != 0 {
		t.Fatalf("series membership targeted unrelated colliding collection: %#v", relations)
	}
}

func TestRelationshipResolverDoesNotUseTitlesWhenExactProviderIdentityIsAmbiguous(t *testing.T) {
	ctx := context.Background()
	db, repository, resolver := newRelationshipFixture(t)
	defer db.Close()
	universeID := domain.UniverseID("ambiguous-work-universe")
	createRelationshipUniverse(t, repository, universeID, "Ambiguous Work Saga")
	materializeRelationshipWork(t, repository, "OL1001W", "Same Candidate Title")
	second, err := repository.MaterializeExternalWork(ctx, domain.WorkMaterialization{
		Provider: "tvmaze", ExternalID: "9001", Title: "Different Local Title", Medium: domain.MediumVideo, WorkType: "series",
	})
	if err != nil {
		t.Fatal(err)
	}
	root := relationshipMetadata("Q1000", nil, nil, claim("P527", "Q1000", "Q1001", nil))
	ambiguous := relationshipMetadata("Q1001", nil, []providers.RelationshipIdentifier{{Property: "P648", Value: "OL1001W"}, {Property: "P8600", Value: "9001"}})
	resolver.source = &relationshipMetadataStub{candidates: []wikidata.EntityCandidate{{ID: "Q1000", Label: "Ambiguous Work Saga", Language: "en"}}, entities: map[string]providers.RelationshipMetadata{"Q1000": root, "Q1001": ambiguous}, fetchErr: map[string]error{}}
	if _, err := resolver.ResolveByUniverseID(ctx, universeID, "en"); err != nil {
		t.Fatalf("ResolveByUniverseID() error = %v", err)
	}
	graph, err := repository.GetUniverse(ctx, universeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Suggestions) != 0 {
		t.Fatalf("ambiguous exact identities created a title-based suggestion: %#v; second local Work %q", graph.Suggestions, second.Work.ID)
	}
}

func newRelationshipFixture(t *testing.T) (*sql.DB, *SQLiteRepository, *UniverseRelationshipResolver) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "relationship-graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteRepository(db)
	resolver := NewUniverseRelationshipResolver(repository, nil, nil)
	return db, repository, resolver
}

func createRelationshipUniverse(t *testing.T, repository *SQLiteRepository, id domain.UniverseID, title string) {
	t.Helper()
	if err := repository.CreateUniverse(context.Background(), domain.Universe{ID: id, Title: title}); err != nil {
		t.Fatal(err)
	}
}

func materializeRelationshipWork(t *testing.T, repository *SQLiteRepository, externalID, title string) WorkGraph {
	t.Helper()
	graph, err := repository.MaterializeExternalWork(context.Background(), domain.WorkMaterialization{
		Provider: "openlibrary", ExternalID: externalID, Title: title, Medium: domain.MediumLiterature, WorkType: "novel",
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func relationshipMetadata(qid string, labels []providers.RelationshipLocalizedValue, identifiers []providers.RelationshipIdentifier, claims ...providers.RelationshipClaim) providers.RelationshipMetadata {
	return providers.RelationshipMetadata{
		Provider: "wikidata", ExternalID: qid, EntityRevision: "123", ParserVersion: "wikidata-relationship-v3", FetchedAt: time.Now().UTC(),
		Labels: labels, Identifiers: identifiers, Claims: claims,
	}
}

func claim(property, subject, object string, ordinal *string) providers.RelationshipClaim {
	return providers.RelationshipClaim{Property: property, SubjectExternalID: subject, ObjectExternalID: object, Ordinal: ordinal}
}

func stringPointer(value string) *string { return &value }

func hasTargetRelation(relations []domain.WorkRelation, kind domain.WorkRelationType, target domain.WorkID) bool {
	for _, relation := range relations {
		if relation.Type == kind && relation.TargetWorkID != nil && *relation.TargetWorkID == target {
			return true
		}
	}
	return false
}
