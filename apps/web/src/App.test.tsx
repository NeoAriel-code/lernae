import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "./App";

type Platform = { name: string; slug?: string };

type SearchResult = {
  provider: string;
  external_id: string;
  title: string;
  release_date?: string;
  release_year?: number;
  summary?: string;
  cover_reference?: string;
  platforms?: Platform[];
  medium: string;
  work_type: string;
  media_type?: string;
  subtitle?: string;
  creators?: string[];
  year?: number;
  network?: string;
  web_channel?: string;
  artwork_url?: string;
  source_url?: string;
  source_rank?: number;
  score?: number;
  relevance?: "best" | "related" | "weak";
};

type WorkDetails = Omit<SearchResult, "platforms"> & {
  language?: string;
  status?: string;
  genres?: string[];
  network?: string;
  web_channel?: string;
  season_count?: number | null;
  series?: Array<{
    collection_id: string;
    title: string;
    ordinal: string | null;
    known_total: number | null;
    more_in_series: Array<{
      provider: string;
      external_id: string;
      title: string;
      ordinal: string | null;
    }>;
    more_in_series_truncated: boolean;
  }>;
  fallback_cover_reference?: string;
  representative_edition?: {
    external_id?: string;
    title?: string;
    language?: string;
    publication_date?: string;
    cover_reference?: string;
  };
  platform_candidates: Array<{
    platform: Platform;
    provenance: Array<{ provider: string; external_id: string; relation: string }>;
  }>;
};

const soulcalibur: SearchResult = {
  provider: "igdb",
  external_id: "1001",
  title: "Soulcalibur II",
  release_date: "2002-08-26T00:00:00Z",
  release_year: 2002,
  summary: "The synthetic GameCube fixture.",
  cover_reference: "https://example.test/soulcalibur-cover.jpg",
  platforms: [{ name: "Nintendo GameCube", slug: "nintendo-gamecube" }],
  medium: "game",
  work_type: "game",
};

const syntheticGame: SearchResult = {
  provider: "test-catalog",
  external_id: "record-42",
  title: "Synthetic Game",
  platforms: [{ name: "Nintendo GameCube", slug: "nintendo-gamecube" }],
  medium: "game",
  work_type: "game",
};

const archivedInventory = {
  owned: true,
  availability: "archived",
  work_id: "work-synthetic-42",
  edition_id: "edition-synthetic-42",
  asset_ids: ["asset-synthetic-42"],
};

const runtimeStatus = {
  server: { status: "online" },
  database: { status: "ready" },
  agent: { status: "connected" },
};

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

describe("Home game search", () => {
  it("presents Lernae search instead of runtime system status", () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse({}));
    vi.stubGlobal("fetch", fetchMock);

    render(<App />);

    expect(screen.getByRole("heading", { name: "Lernae" })).toBeTruthy();
    expect(screen.getByRole("searchbox", { name: "Search the catalog" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Discover something you love." })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "System status" })).toBeNull();
    expect(screen.getByRole("button", { name: "Home" }).getAttribute("aria-current")).toBe("page");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("opens the dedicated Search view from the empty discovery hero", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("fetch", vi.fn<typeof fetch>());
    render(<App />);

    await user.click(screen.getByRole("button", { name: "Explore titles" }));

    expect(screen.getByRole("heading", { name: "Search" })).toBeTruthy();
    expect(screen.getByRole("searchbox", { name: "Search the catalog" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Search" }).getAttribute("aria-current")).toBe("page");
  });

  it("V11-03 submits the persistent header search through the shared pipeline and focuses Search", async () => {
    const user = userEvent.setup();
    const fetchMock = mockAppFetch([soulcalibur]);
    render(<App />);

    const headerSearch = screen.getByRole("searchbox", { name: "Quick catalog search" });
    await user.type(headerSearch, "Soul Calibur");
    await user.keyboard("{Enter}");

    expect(await screen.findByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    const searchViewInput = screen.getByRole("searchbox", { name: "Search the catalog" }) as HTMLInputElement;
    expect(searchViewInput.value).toBe("Soul Calibur");
    expect(document.activeElement).toBe(searchViewInput);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/search?q=Soul%20Calibur");
  });

  it("accepts the universal-search envelope while retaining array compatibility", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "/works/OL1W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      creators: ["Robert Jordan"],
      year: 1990,
      source_url: "https://openlibrary.org/works/OL1W",
    };
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse({
      results: [book],
      sources: [{ provider: "openlibrary", state: "available", result_count: 1 }],
    }));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(await screen.findByRole("button", { name: "View details for The Eye of the World" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: /The Eye of the World/ })).toBeNull();
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/search?q=Wheel%20of%20Time");
  });

  it("filters universal results by All, Games, Books, and TV without losing card identity", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL123W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      creators: ["Robert Jordan"],
      year: 1990,
      artwork_url: "https://covers.openlibrary.org/b/id/123-M.jpg",
      source_url: "https://openlibrary.org/works/OL123W",
    };
    const series: SearchResult = {
      provider: "tvmaze",
      external_id: "42",
      title: "The Wheel of Time",
      medium: "video",
      work_type: "series",
      media_type: "tv",
      creators: ["Rafe Judkins"],
      year: 2021,
      network: "Prime Video",
      web_channel: "Amazon",
      artwork_url: "https://static.tvmaze.com/uploads/images/medium_portrait/wot.jpg",
      source_url: "https://www.tvmaze.com/shows/42/the-wheel-of-time",
    };
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse({
      results: [soulcalibur, book, series],
      sources: [
        { provider: "igdb", state: "fresh", result_count: 1 },
        { provider: "openlibrary", state: "fresh", result_count: 1 },
        { provider: "tvmaze", state: "fresh", result_count: 1 },
      ],
    }));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(await screen.findByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    const bookCard = screen.getByRole("button", { name: "View details for The Eye of the World" });
    const seriesCard = screen.getByRole("button", { name: "View details for The Wheel of Time" });
    expect(bookCard.hasAttribute("href")).toBe(false);
    expect(seriesCard.hasAttribute("href")).toBe(false);
    expect(bookCard.textContent).toContain("Robert Jordan");
    expect(bookCard.textContent).toContain("1990");
    expect(bookCard.querySelector(".media-type-badge")?.textContent).toBe("Book");
    expect(seriesCard.querySelector(".media-type-badge")?.textContent).toBe("TV · Series");
    expect(bookCard.querySelector(".media-card-provider")).toBeNull();
    expect(seriesCard.querySelector(".media-card-provider")).toBeNull();
    expect(seriesCard.querySelector(".media-card-channels")?.textContent).toContain("Prime Video");
    expect(seriesCard.querySelector(".media-card-channels")?.textContent).toContain("Amazon");
    const dataSources = screen.getByRole("complementary", { name: "Data sources" });
    expect(screen.getAllByRole("complementary", { name: "Data sources" })).toHaveLength(1);
    const dataSourceLinks = Array.from(dataSources.querySelectorAll("a"));
    expect(dataSourceLinks.map((link) => link.textContent)).toEqual(["IGDB", "Open Library", "TVMaze"]);
    expect(dataSourceLinks.map((link) => link.getAttribute("href"))).toEqual([
      "https://www.igdb.com/",
      "https://openlibrary.org/",
      "https://www.tvmaze.com/",
    ]);
    for (const link of dataSourceLinks) {
      expect(link.getAttribute("href")).toMatch(/^https:\/\//);
      expect(link.getAttribute("target")).toBe("_blank");
      expect(link.getAttribute("rel")).toContain("noopener");
      expect(link.getAttribute("rel")).toContain("noreferrer");
    }
    expect(screen.queryByRole("link", { name: /The Eye of the World|The Wheel of Time/ })).toBeNull();
    const bookArtwork = bookCard.querySelector(".media-artwork img");
    expect(bookArtwork?.getAttribute("src")).toBe("https://covers.openlibrary.org/b/id/123-M.jpg");
    expect(bookCard.querySelector(".media-artwork")?.className).toContain("media-artwork-contain");
    fireEvent.error(bookArtwork!);
    expect(bookCard.querySelector(".media-artwork")?.textContent).toBe("T");
    expect(screen.queryByRole("button", { name: "READ" })).toBeNull();
    expect(screen.queryByRole("button", { name: "WATCH" })).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/universes/relationships/resolve")).toHaveLength(1);

    const booksFilter = screen.getByRole("button", { name: "Books" });
    await user.click(booksFilter);
    expect(booksFilter.getAttribute("aria-pressed")).toBe("true");
    expect(screen.getAllByRole("complementary", { name: "Data sources" })[0]?.querySelector("a")?.textContent)
      .toBe("Open Library");
    expect(screen.getByRole("complementary", { name: "Data sources" }).querySelectorAll("a")).toHaveLength(1);
    expect(screen.getByRole("button", { name: "View details for The Eye of the World" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "View details for Soulcalibur II" })).toBeNull();
    expect(screen.queryByRole("button", { name: "View details for The Wheel of Time" })).toBeNull();

    const tvFilter = screen.getByRole("button", { name: "TV" });
    tvFilter.focus();
    await user.keyboard("{Enter}");
    expect(tvFilter.getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("complementary", { name: "Data sources" }).querySelectorAll("a")).toHaveLength(1);
    expect(screen.getByRole("complementary", { name: "Data sources" }).querySelector("a")?.textContent).toBe("TVMaze");
    expect(screen.getByRole("button", { name: "View details for The Wheel of Time" }).textContent)
      .toContain("2021");
    expect(screen.getByRole("button", { name: "View details for The Wheel of Time" }).textContent)
      .toContain("Prime Video");
    expect(screen.getByRole("button", { name: "View details for The Wheel of Time" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "View details for The Eye of the World" })).toBeNull();

    await user.click(screen.getByRole("button", { name: "Games" }));
    expect(screen.getByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "View details for The Wheel of Time" })).toBeNull();
  });

  it("groups results by backend relevance and keeps weak matches keyboard-accessible", async () => {
    const user = userEvent.setup();
    const results: SearchResult[] = [
      { ...soulcalibur, title: "Best first", relevance: "best" },
      { ...syntheticGame, external_id: "weak-1", title: "Weak result", relevance: "weak" },
      { ...syntheticGame, external_id: "related-1", title: "Related first", relevance: "related" },
      { ...syntheticGame, external_id: "best-2", title: "Best second", relevance: "best" },
      { ...syntheticGame, external_id: "weak-2", title: "Weak result two", relevance: "weak" },
      { ...syntheticGame, external_id: "related-2", title: "Related second", relevance: "related" },
    ];
    mockAppFetch(results);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "synthetic titles");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    const bestList = await screen.findByRole("list", { name: "Best matches" });
    const relatedList = screen.getByRole("list", { name: "Related results" });
    expect(Array.from(bestList.querySelectorAll(".media-card")).map((card) => card.textContent)).toEqual([
      expect.stringContaining("Best first"), expect.stringContaining("Best second"),
    ]);
    expect(Array.from(relatedList.querySelectorAll(".media-card")).map((card) => card.textContent)).toEqual([
      expect.stringContaining("Related first"), expect.stringContaining("Related second"),
    ]);
    const weakToggle = screen.getByText("Show more related results").closest("summary");
    expect(weakToggle).toBeTruthy();
    expect(weakToggle?.closest("details")?.hasAttribute("open")).toBe(false);
    (weakToggle as HTMLElement).focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("button", { name: "View details for Weak result" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "View details for Weak result two" })).toBeTruthy();
  });

  it("surfaces confirmed state and persisted aliases for an automatically discovered Universe", async () => {
    const user = userEvent.setup();
    const work = { ...soulcalibur, title: "Neon Genesis Evangelion", relevance: "best" as const };
    const autoUniverse = {
      ...universeDetail("universe-evangelion", "Evangelion", [work]),
      provenance: "automatic",
      existence_confidence: 0.98,
      confirmed_by_user: false,
      aliases: [
        { value: "Evangelion", language: "ja", provenance: "provider", confidence: 0.9, confirmed_by_user: false },
        { value: "Shinseiki Evangelion", language: null, provenance: "manual", confidence: 1, confirmed_by_user: true },
      ],
      aliases_truncated: false,
      memberships: [{ ...membershipFrom(work), provenance: "automatic", confidence: 0.9, confirmed_by_user: false }],
    };
    let currentUniverse = autoUniverse;
    const fetchMock = vi.fn<typeof fetch>((input, init) => {
      const url = String(input);
      if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse({
        results: [work],
        sources: [],
        universe_discovery: {
          state: "reused",
          universe_id: autoUniverse.id,
          title: autoUniverse.title,
          existence_confidence: autoUniverse.existence_confidence,
          memberships_added: 0,
          provenance: "automatic",
          confirmed_by_user: false,
        },
      }));
      if (url === `/api/v1/universes/${autoUniverse.id}`) return Promise.resolve(jsonResponse(currentUniverse));
      if (url === `/api/v1/universes/${autoUniverse.id}/confirm` && init?.method === "POST") {
        currentUniverse = { ...currentUniverse, confirmed_by_user: true };
        return Promise.resolve(jsonResponse(currentUniverse));
      }
      if (url === `/api/v1/universes/${autoUniverse.id}/memberships` && init?.method === "POST") {
        const request = JSON.parse(String(init.body)) as SearchResult;
        currentUniverse = {
          ...currentUniverse,
          memberships: currentUniverse.memberships.map((membership) => membership.external_id === request.external_id
            ? { ...membership, confirmed_by_user: true }
            : membership),
        };
        return Promise.resolve(jsonResponse(currentUniverse));
      }
      return Promise.reject(new Error(`unexpected test request: ${url}`));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Evangelion");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    const openUniverse = await screen.findByRole("button", { name: "Open Universe: Evangelion" });
    expect(screen.getByText("Discovered Universe")).toBeTruthy();
    expect(screen.getByText(/discovered automatically/i)).toBeTruthy();
    expect(fetchMock.mock.calls.filter(([url]) => String(url).startsWith("/api/v1/universes?q=")).length).toBe(0);

    await user.click(openUniverse);
    expect(await screen.findByRole("heading", { name: "Evangelion" })).toBeTruthy();
    expect(screen.getByText("Discovered Universe")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Aliases" })).toBeTruthy();
    expect(screen.getByText("Evangelion", { selector: ".universe-alias-value" })).toBeTruthy();
    expect(screen.getByText("ja", { selector: ".universe-alias-language" })).toBeTruthy();
    expect(screen.getByText("Shinseiki Evangelion", { selector: ".universe-alias-value" })).toBeTruthy();
    expect(screen.getByText("Language not specified", { selector: ".universe-alias-language" })).toBeTruthy();
    expect(screen.getByText(/Works remain individually unconfirmed/i)).toBeTruthy();
    expect(screen.getByText(/1 Work in this Universe/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Manage Works" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Confirm Universe" })).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url]) => String(url) === `/api/v1/universes/${autoUniverse.id}/confirm`)).toBe(false);

    await user.click(screen.getByRole("button", { name: "Confirm Universe" }));
    expect(await screen.findByText("Universe confirmed by you. Works still need separate confirmation.")).toBeTruthy();
    expect(screen.getByText("Confirmed Universe")).toBeTruthy();
    expect(screen.queryByText("Discovered Universe")).toBeNull();
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === `/api/v1/universes/${autoUniverse.id}/confirm` && init?.method === "POST")).toBe(true);

    await user.click(screen.getByRole("button", { name: "Manage Works" }));
    const confirmWork = screen.getByRole("button", { name: "Confirm Neon Genesis Evangelion membership in this Universe (igdb 1001)" });
    expect(confirmWork).toBeTruthy();
    await user.click(confirmWork);
    expect(await screen.findByText(/Confirmed by you/)).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === `/api/v1/universes/${autoUniverse.id}/memberships` && init?.method === "POST")).toBe(true);
  });

  it("shows review-needed Universe suggestions separately and confirms only on explicit action", async () => {
    const user = userEvent.setup();
    const work: SearchResult = {
      ...soulcalibur,
      external_id: "3555",
      title: "Neon Genesis Evangelion",
      media_type: "game",
      relevance: "weak",
    };
    const suggestion = {
      ...membershipFrom(work),
      media_type: work.media_type,
      provenance: "automatic",
      confidence: 0.9,
      evidence: "unknown",
      reason: "No supported matching evidence was found.",
      confirmed_by_user: false,
    };
    const autoUniverse = {
      ...universeDetail("universe-evangelion-review", "Evangelion", []),
      provenance: "automatic",
      existence_confidence: 0.98,
      confirmed_by_user: false,
      suggestions: [suggestion],
      suggestions_truncated: false,
    };
    let currentUniverse = autoUniverse;
    const fetchMock = vi.fn<typeof fetch>((input, init) => {
      const url = String(input);
      if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse({
        results: [work],
        sources: [],
        universe_discovery: {
          state: "reused",
          universe_id: autoUniverse.id,
          title: autoUniverse.title,
          existence_confidence: autoUniverse.existence_confidence,
          memberships_added: 0,
          provenance: "automatic",
          confirmed_by_user: false,
        },
      }));
      if (url === `/api/v1/universes/${autoUniverse.id}`) return Promise.resolve(jsonResponse(currentUniverse));
      if (url === `/api/v1/universes/${autoUniverse.id}/memberships` && init?.method === "POST") {
        currentUniverse = {
          ...currentUniverse,
          memberships: [{ ...suggestion, confirmed_by_user: true }],
          suggestions: [],
        };
        return Promise.resolve(jsonResponse(currentUniverse));
      }
      return Promise.reject(new Error(`unexpected test request: ${url}`));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Evangelion");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "Open Universe: Evangelion" }));

    expect(await screen.findByRole("heading", { name: "Review-needed suggestions" })).toBeTruthy();
    expect(screen.getByText("Neon Genesis Evangelion")).toBeTruthy();
    expect(screen.getByText("igdb · 3555")).toBeTruthy();
    expect(screen.getByText("Media: game")).toBeTruthy();
    expect(screen.getByText("Automatic suggestion · Not confirmed by you")).toBeTruthy();
    expect(screen.getByText("90% stored confidence · evidence unknown")).toBeTruthy();
    expect(screen.getByText("No supported matching evidence was found.")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Game" })).toBeNull();
    expect(screen.getByText("This Universe has no accepted Works yet.")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Manage Works" }));
    expect(screen.queryByRole("button", { name: "Add and confirm Neon Genesis Evangelion from igdb (3555)" })).toBeNull();
    const confirm = screen.getByRole("button", {
      name: "Confirm Neon Genesis Evangelion membership in this Universe (igdb 3555)",
    });
    await user.click(confirm);

    expect(await screen.findByText("Neon Genesis Evangelion was confirmed in this Universe.")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Review-needed suggestions" })).toBeNull();
    expect(screen.getByText("Confirmed by you · 90% evidence")).toBeTruthy();
    const confirmationCall = fetchMock.mock.calls.find(([url, init]) =>
      String(url) === `/api/v1/universes/${autoUniverse.id}/memberships` && init?.method === "POST");
    expect(confirmationCall).toBeTruthy();
    expect(JSON.parse(String(confirmationCall?.[1]?.body))).toMatchObject({
      provider: "igdb",
      external_id: "3555",
    });
  });

  it("explains an ambiguous grouping in plain language and opens review by keyboard", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn<typeof fetch>((input) => {
      const url = String(input);
      if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse({
        results: [{ ...soulcalibur, title: "Conan", relevance: "best" }],
        sources: [],
        universe_discovery: { state: "ambiguous", memberships_added: 0, confirmed_by_user: false },
      }));
      if (url === "/api/v1/universes/resolve") return Promise.resolve(jsonResponse({ candidates: [] }));
      return Promise.reject(new Error(`unexpected test request: ${url}`));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "conan");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(await screen.findByText("Possible collection")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "These results may belong together" })).toBeTruthy();
    const reviewButton = screen.getByRole("button", { name: "Review grouping" });
    reviewButton.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("heading", { name: "Possible groups" })).toBeTruthy();
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/universes/resolve")).toHaveLength(1);
    expect(fetchMock.mock.calls.some(([url]) => String(url).startsWith("/api/v1/universes?q="))).toBe(false);
  });

  it("resolves a localized alias once per explicit query without changing Search ranking or Universe identity", async () => {
    const user = userEvent.setup();
    const show: SearchResult = {
      provider: "tvmaze",
      external_id: "139",
      title: "Game of Thrones",
      medium: "video",
      work_type: "series",
      media_type: "tv",
      relevance: "related",
    };
    const universe = {
      ...universeDetail("opaque-universe-got", "Game of Thrones", []),
      aliases: [{
        value: "Juego de Tronos",
        language: "es",
        provenance: "manual",
        confidence: 1,
        confirmed_by_user: true,
      }],
      aliases_truncated: false,
    };
    const fetchMock = vi.fn<typeof fetch>((input, init) => {
      const url = String(input);
      if (url === "/api/v1/settings/metadata-language" && init?.method === "PATCH") {
        const body = JSON.parse(String(init.body)) as { metadata_language: string };
        return Promise.resolve(jsonResponse(body));
      }
      if (url === "/api/v1/settings/metadata-language") return Promise.resolve(jsonResponse({ metadata_language: "en" }));
      if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse({ results: [show], sources: [] }));
      if (url === "/api/v1/universes/relationships/resolve" && init?.method === "POST") {
        return Promise.resolve(jsonResponse({
          universe_id: "opaque-universe-got",
          nodes_visited: 1,
          edges_visited: 0,
          requests_used: 0,
          max_depth: 0,
          aliases_added: 0,
          relations_set: 0,
          suggestions_added: 0,
          unresolved_edges: 0,
          budget_limited: false,
        }));
      }
      if (url === "/api/v1/universes/opaque-universe-got") return Promise.resolve(jsonResponse(universe));
      return Promise.reject(new Error(`unexpected test request: ${url}`));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.click(screen.getByRole("button", { name: "Settings" }));
    const language = await screen.findByRole("combobox", { name: "Metadata language" });
    await user.selectOptions(language, "es");
    expect(await screen.findByText("Metadata language saved.")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: /^Search$/ }));
    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Juego de Tronos");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(await screen.findByRole("button", { name: "Open Universe: Game of Thrones" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "View details for Game of Thrones" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Related results" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "View details for Game of Thrones" }).closest("ul")?.getAttribute("aria-label"))
      .toBe("Related results");
    const aliasCalls = fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/universes/relationships/resolve");
    expect(aliasCalls).toHaveLength(1);
    expect(JSON.parse(String(aliasCalls[0]?.[1]?.body))).toEqual({ alias: "Juego de Tronos", language: "es" });
    expect(fetchMock.mock.calls.filter(([url]) => String(url).startsWith("/api/v1/search"))).toHaveLength(1);

    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await screen.findByRole("button", { name: "View details for Game of Thrones" });
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/universes/relationships/resolve")).toHaveLength(1);
    expect(fetchMock.mock.calls.filter(([url]) => String(url).startsWith("/api/v1/search"))).toHaveLength(2);

    await user.click(screen.getByRole("button", { name: "Open Universe: Game of Thrones" }));
    expect(await screen.findByRole("heading", { name: "Game of Thrones" })).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url]) => String(url) === "/api/v1/universes/opaque-universe-got")).toBe(true);
    expect(screen.getByText("Juego de Tronos", { selector: ".universe-alias-value" })).toBeTruthy();
  });

  it("opens normalized Book Details internally with contained cover, authors, year, and secondary attribution", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL123W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      creators: ["Robert Jordan"],
      year: 1990,
      artwork_url: "https://covers.openlibrary.org/b/id/123-M.jpg",
      source_url: "https://openlibrary.org/works/OL123W",
    };
    const bookDetails: WorkDetails = {
      ...workDetailsFor(book),
      release_year: 1990,
      summary: "A synthetic normalized Book description.",
      cover_reference: "https://covers.openlibrary.org/b/id/123-L.jpg",
      source_url: "https://openlibrary.org/works/OL123W",
      representative_edition: {
        external_id: "OL321M",
        title: "The Eye of the World",
        language: "en",
        publication_date: "1990",
        cover_reference: "https://covers.openlibrary.org/b/id/123-L.jpg",
      },
      series: [{
        collection_id: "collection-wheel-of-time",
        title: "The Wheel of Time",
        ordinal: "1",
        known_total: 14,
        more_in_series: [{
          provider: "openlibrary",
          external_id: "OL200W",
          title: "The Great Hunt",
          ordinal: "2",
        }],
        more_in_series_truncated: true,
      }],
    };
    const sequel: SearchResult = {
      ...book,
      external_id: "OL200W",
      title: "The Great Hunt",
      source_url: "https://openlibrary.org/works/OL200W",
    };
    const fetchMock = mockAppFetch([book, sequel], { detailsByRequestedID: { OL123W: bookDetails } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    const bookCard = await screen.findByRole("button", { name: "View details for The Eye of the World" });
    expect(bookCard.textContent).toContain("Robert Jordan");
    expect(bookCard.textContent).toContain("1990");
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await user.click(bookCard);

    const details = await screen.findByRole("region", { name: "Details for The Eye of the World" });
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/work-details?provider=openlibrary&external_id=OL123W");
    expect(screen.getByRole("heading", { name: "The Eye of the World" })).toBeTruthy();
    expect(details.querySelector(".media-type-badge")?.textContent).toBe("Book");
    expect(details.querySelector(".book-authors")?.textContent).toBe("By Robert Jordan");
    expect(details.querySelector(".book-publication-year")?.textContent).toContain("1990");
    expect(details.querySelector(".representative-edition")?.textContent).toContain("The Eye of the World");
    expect(details.querySelector(".representative-edition")?.textContent).toContain("EN");
    expect(details.querySelector(".series-identity")?.textContent).toContain("Series · Book 1 of 14");
    expect(details.querySelector(".series-identity")?.textContent).toContain("The Wheel of Time");
    expect(details.querySelector(".series-details")?.getAttribute("data-series-id")).toBe("collection-wheel-of-time");
    expect(screen.getByRole("heading", { name: "More in this series" })).toBeTruthy();
    const seriesMember = screen.getByRole("button", { name: "View details for The Great Hunt" });
    expect(seriesMember.textContent).toContain("Book 2");
    expect(screen.getByText(/More titles are available in this series/i)).toBeTruthy();
    expect(details.querySelector(".cover-frame")?.className).toContain("book-cover-frame");
    expect(details.querySelector(".cover-frame img")?.getAttribute("src")).toBe(bookDetails.cover_reference);
    const providerLink = screen.getByRole("link", { name: "Open Library record" });
    expect(providerLink.getAttribute("href")).toBe("https://openlibrary.org/works/OL123W");
    expect(providerLink.getAttribute("target")).toBe("_blank");
    expect(providerLink.getAttribute("rel")).toContain("noopener");
    expect(details.querySelector(".platform-options")).toBeNull();
    expect(screen.queryByRole("button", { name: "READ" })).toBeNull();
    expect(screen.queryByRole("button", { name: "WATCH" })).toBeNull();
    await user.click(seriesMember);
    expect(await screen.findByRole("heading", { name: "The Great Hunt" })).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url]) => String(url) ===
      "/api/v1/work-details?provider=openlibrary&external_id=OL200W")).toBe(true);
  });

  it("shows one canonical Data sources line for visible Book and TV cards without card-level provider labels", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL88W",
      title: "A Visible Book",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      creators: ["A. Author"],
      year: 2020,
    };
    const show: SearchResult = {
      provider: "tvmaze",
      external_id: "88",
      title: "A Visible Series",
      medium: "video",
      work_type: "series",
      media_type: "tv",
      year: 2024,
      network: "A Network",
      web_channel: "A Streamer",
    };
    const fetchMock = mockAppFetch([book, show]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Visible titles");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    const bookCard = await screen.findByRole("button", { name: "View details for A Visible Book" });
    const showCard = await screen.findByRole("button", { name: "View details for A Visible Series" });
    expect(bookCard.querySelector(".media-card-provider")).toBeNull();
    expect(showCard.querySelector(".media-card-provider")).toBeNull();

    const dataSources = screen.getByRole("complementary", { name: "Data sources" });
    expect(screen.getAllByRole("complementary", { name: "Data sources" })).toHaveLength(1);
    const links = Array.from(dataSources.querySelectorAll("a"));
    expect(links.map((link) => link.textContent)).toEqual(["Open Library", "TVMaze"]);
    expect(links.map((link) => link.getAttribute("href"))).toEqual([
      "https://openlibrary.org/",
      "https://www.tvmaze.com/",
    ]);
    for (const link of links) {
      expect(link.getAttribute("target")).toBe("_blank");
      expect(link.getAttribute("rel")).toContain("noopener");
      expect(link.getAttribute("rel")).toContain("noreferrer");
    }
    expect(showCard.textContent).toContain("2024");
    expect(showCard.textContent).toContain("A Network");
    expect(showCard.textContent).toContain("A Streamer");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("renders normalized TV premiere, network, web channel, and safe provider attribution without WATCH", async () => {
    const user = userEvent.setup();
    const show: SearchResult = {
      provider: "tvmaze",
      external_id: "42",
      title: "The Wheel of Time",
      medium: "video",
      work_type: "series",
      media_type: "tv",
      creators: ["Rafe Judkins"],
      year: 2021,
      source_url: "https://www.tvmaze.com/shows/42/the-wheel-of-time",
    };
    const showDetails: WorkDetails = {
      ...workDetailsFor(show),
      release_year: 2021,
      language: "en",
      status: "Running",
      genres: ["Adventure", "Fantasy"],
      network: "Synthetic Network",
      web_channel: "Synthetic Streamer",
      season_count: 3,
      source_url: "https://www.tvmaze.com/shows/42/the-wheel-of-time",
    };
    const fetchMock = mockAppFetch([show], { detailsByRequestedID: { "42": showDetails } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    const showCard = await screen.findByRole("button", { name: "View details for The Wheel of Time" });
    expect(showCard.querySelector(".media-type-badge")?.textContent).toBe("TV · Series");
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await user.click(showCard);

    const details = await screen.findByRole("region", { name: "Details for The Wheel of Time" });
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/work-details?provider=tvmaze&external_id=42");
    expect(details.querySelector(".media-type-badge")?.textContent).toBe("TV · Series");
    expect(details.querySelector(".tv-premiered")?.textContent).toContain("2021");
    expect(details.querySelector(".tv-network")?.textContent).toBe("Synthetic Network");
    expect(details.querySelector(".tv-web-channel")?.textContent).toBe("Synthetic Streamer");
    expect(details.querySelector(".tv-status")?.textContent).toBe("Running");
    expect(details.querySelector(".tv-genres")?.textContent).toContain("Adventure");
    expect(details.querySelector(".tv-season-count")?.textContent).toBe("3");
    expect(details.textContent).not.toContain("Episodes");
    expect(screen.getByRole("link", { name: "TVMaze record" }).getAttribute("href"))
      .toBe("https://www.tvmaze.com/shows/42/the-wheel-of-time");
    expect(details.querySelector(".platform-options")).toBeNull();
    expect(screen.queryByRole("button", { name: "READ" })).toBeNull();
    expect(screen.queryByRole("button", { name: "WATCH" })).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("keeps valid TV Work Details visible when optional season metadata is malformed", async () => {
    const user = userEvent.setup();
    const show: SearchResult = {
      provider: "tvmaze",
      external_id: "777",
      title: "A Valid Show",
      medium: "video",
      work_type: "series",
      media_type: "tv",
    };
    const malformedDetails = {
      ...workDetailsFor(show),
      status: "Running",
      season_count: "unknown",
    } as unknown as WorkDetails;
    mockAppFetch([show], { detailsByRequestedID: { "777": malformedDetails } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "A Valid Show");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for A Valid Show" }));

    const details = await screen.findByRole("region", { name: "Details for A Valid Show" });
    expect(screen.getByRole("heading", { name: "A Valid Show" })).toBeTruthy();
    expect(details.querySelector(".tv-status")?.textContent).toBe("Running");
    expect(details.querySelector(".tv-season-count")).toBeNull();
    expect(details.textContent).not.toContain("Episodes");
  });

  it("uses a missing or broken Book cover fallback without cropping the cover", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL456W",
      title: "A Cover Test",
      medium: "literature",
      work_type: "book",
      media_type: "book",
    };
    const missingDetails: WorkDetails = { ...workDetailsFor(book), source_url: "https://openlibrary.org/works/OL456W" };
    mockAppFetch([book], { detailsByRequestedID: { OL456W: missingDetails } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Cover Test");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for A Cover Test" }));
    expect(await screen.findByRole("img", { name: "Cover art unavailable" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Details for A Cover Test" }).querySelector(".cover-frame img")).toBeNull();
  });

  it("replaces a broken Book cover with the Nocturne placeholder", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL457W",
      title: "A Broken Cover Test",
      medium: "literature",
      work_type: "book",
      media_type: "book",
    };
    const details: WorkDetails = {
      ...workDetailsFor(book),
      cover_reference: "https://covers.openlibrary.org/b/id/457-L.jpg",
    };
    mockAppFetch([book], { detailsByRequestedID: { OL457W: details } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Broken Cover");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for A Broken Cover Test" }));
    fireEvent.error(await screen.findByRole("img", { name: "A Broken Cover Test cover" }));

    expect(await screen.findByRole("img", { name: "Cover art unavailable" })).toBeTruthy();
  });

  it("retries a broken representative Book cover with the Work cover, then shows the safe placeholder", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL458W",
      title: "A Two-Cover Test",
      medium: "literature",
      work_type: "book",
      media_type: "book",
    };
    const editionCover = "https://covers.openlibrary.org/b/id/458-L.jpg";
    const workCover = "https://covers.openlibrary.org/b/id/459-L.jpg";
    const details: WorkDetails = {
      ...workDetailsFor(book),
      cover_reference: editionCover,
      fallback_cover_reference: workCover,
    };
    const fetchMock = mockAppFetch([book], { detailsByRequestedID: { OL458W: details } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Two-Cover");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for A Two-Cover Test" }));

    let cover = await screen.findByRole("img", { name: "A Two-Cover Test cover" });
    expect(cover.getAttribute("src")).toBe(editionCover);
    fireEvent.error(cover);
    cover = screen.getByRole("img", { name: "A Two-Cover Test cover" });
    expect(cover.getAttribute("src")).toBe(workCover);
    expect(screen.queryByRole("img", { name: "Cover art unavailable" })).toBeNull();

    fireEvent.error(cover);
    expect(await screen.findByRole("img", { name: "Cover art unavailable" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Details for A Two-Cover Test" }).querySelector(".cover-frame img")).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("does not retry an identical Book cover fallback", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL459W",
      title: "An Identical-Cover Test",
      medium: "literature",
      work_type: "book",
      media_type: "book",
    };
    const coverReference = "https://covers.openlibrary.org/b/id/460-L.jpg";
    const details: WorkDetails = {
      ...workDetailsFor(book),
      cover_reference: coverReference,
      fallback_cover_reference: coverReference,
    };
    const fetchMock = mockAppFetch([book], { detailsByRequestedID: { OL459W: details } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Identical-Cover");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for An Identical-Cover Test" }));
    fireEvent.error(await screen.findByRole("img", { name: "An Identical-Cover Test cover" }));

    expect(await screen.findByRole("img", { name: "Cover art unavailable" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Details for An Identical-Cover Test" }).querySelector(".cover-frame img")).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("rejects an unsafe Work-cover fallback URL", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL460W",
      title: "An Unsafe-Fallback Test",
      medium: "literature",
      work_type: "book",
      media_type: "book",
    };
    const details: WorkDetails = {
      ...workDetailsFor(book),
      cover_reference: "https://covers.openlibrary.org/b/id/461-L.jpg",
      fallback_cover_reference: "https://attacker.example/book.jpg",
    };
    const fetchMock = mockAppFetch([book], { detailsByRequestedID: { OL460W: details } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Unsafe-Fallback");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for An Unsafe-Fallback Test" }));

    expect(await screen.findByText("Work details are temporarily unavailable.")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Details for An Unsafe-Fallback Test" }).querySelector(".cover-frame img")).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("rejects mismatched normalized Book identity and does not expose its provider link", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL123W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
    };
    const mismatchedDetails: WorkDetails = {
      ...workDetailsFor(book),
      external_id: "OL999W",
      title: "Different Work",
      source_url: "https://openlibrary.org/works/OL999W",
    };
    const fetchMock = mockAppFetch([book], { detailsByRequestedID: { OL123W: mismatchedDetails } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for The Eye of the World" }));

    expect(await screen.findByText("Work details are temporarily unavailable.")).toBeTruthy();
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/work-details?provider=openlibrary&external_id=OL123W");
    expect(screen.queryByRole("heading", { name: "Different Work" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Open Library record" })).toBeNull();
  });

  it("preserves the selected search filter after opening Details and returning", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL123W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      source_url: "https://openlibrary.org/works/OL123W",
    };
    mockAppFetch([soulcalibur, book]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "Games" }));
    expect(screen.getByRole("button", { name: "Games" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "View The Eye of the World on Open Library" })).toBeNull();

    await user.click(screen.getByRole("button", { name: "View details for Soulcalibur II" }));
    expect(await screen.findByRole("heading", { name: "Soulcalibur II" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Back to results" }));

    expect(screen.getByRole("button", { name: "Games" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByText("1 title in Games for “Wheel of Time”")).toBeTruthy();
    expect(screen.getByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "View The Eye of the World on Open Library" })).toBeNull();
  });

  it("restores Search query, selected filter, scroll position, and card focus after Book Details", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL123W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      source_url: "https://openlibrary.org/works/OL123W",
    };
    const scrollY = vi.spyOn(window, "scrollY", "get").mockReturnValueOnce(384).mockReturnValue(0);
    const scrollTo = vi.spyOn(window, "scrollTo").mockImplementation(() => undefined);
    mockAppFetch([soulcalibur, book]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "Books" }));
    const bookCard = screen.getByRole("button", { name: "View details for The Eye of the World" });
    bookCard.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("heading", { name: "The Eye of the World" })).toBeTruthy();

    const backButton = screen.getByRole("button", { name: "Back to results" });
    backButton.focus();
    await user.keyboard("{Enter}");

    const restoredBookCard = screen.getByRole("button", { name: "View details for The Eye of the World" });
    expect(screen.getByRole("button", { name: "Books" }).getAttribute("aria-pressed")).toBe("true");
    expect((screen.getByRole("searchbox", { name: "Search the catalog" }) as HTMLInputElement).value)
      .toBe("Wheel of Time");
    expect(scrollTo).toHaveBeenCalledWith(0, 384);
    expect(document.activeElement).toBe(restoredBookCard);
    scrollY.mockRestore();
  });

  it("keeps successful categories visible while identifying stale and unavailable sources", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL123W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      source_url: "https://openlibrary.org/works/OL123W",
    };
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse({
      results: [soulcalibur, book],
      sources: [
        { provider: "igdb", state: "fresh", result_count: 1 },
        { provider: "openlibrary", state: "stale", result_count: 1, error_code: "stale_cache" },
        { provider: "tvmaze", state: "unavailable", result_count: 0, error_code: "provider_timeout" },
      ],
    }));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(await screen.findByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "View details for The Eye of the World" })).toBeTruthy();
    const sourceStatus = screen.getByRole("status", { name: "Search source status" });
    expect(sourceStatus.textContent).toContain("Open Library is showing cached results");
    expect(sourceStatus.textContent).toContain("TVMaze is temporarily unavailable");
    expect(sourceStatus.textContent).not.toContain("provider_timeout");
  });

  it("does not make an unsafe Book or TV source URL clickable", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "OL123W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      source_url: "https://attacker.example/collect",
    };
    const fetchMock = mockAppFetch([book]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    const bookCard = await screen.findByRole("button", { name: "View details for The Eye of the World" });
    expect(bookCard.querySelector(".media-card-provider")).toBeNull();
    expect(screen.getByRole("complementary", { name: "Data sources" }).querySelector("a")?.getAttribute("href"))
      .toBe("https://openlibrary.org/");
    expect(screen.queryByRole("link", { name: /The Eye of the World/ })).toBeNull();
    await user.click(bookCard);
    expect(await screen.findByRole("heading", { name: "The Eye of the World" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Open Library record" })).toBeNull();
    expect(screen.queryByRole("link", { name: /attacker\.example/ })).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("shows a safe secondary TVMaze attribution only after opening exact TV Details", async () => {
    const user = userEvent.setup();
    const validSeries: SearchResult = {
      provider: "tvmaze",
      external_id: "42",
      title: "The Wheel of Time",
      medium: "video",
      work_type: "series",
      media_type: "tv",
      source_url: "https://www.tvmaze.com/shows/42/the-wheel-of-time",
    };
    const invalidSeries: SearchResult[] = [
      {
        ...validSeries,
        external_id: "43",
        title: "Untrusted host show",
        source_url: "https://attacker.example/shows/43/the-wheel-of-time",
      },
      {
        ...validSeries,
        external_id: "44",
        title: "Mismatched show ID",
        source_url: "https://www.tvmaze.com/shows/43/the-wheel-of-time",
      },
      {
        ...validSeries,
        external_id: "45",
        title: "Wrong TVMaze path",
        source_url: "https://www.tvmaze.com/episodes/45/the-wheel-of-time",
      },
    ];
    const fetchMock = mockAppFetch([validSeries, ...invalidSeries]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    const showCard = await screen.findByRole("button", { name: "View details for The Wheel of Time" });
    expect(screen.queryByRole("link", { name: /The Wheel of Time/ })).toBeNull();
    await user.click(showCard);
    expect(await screen.findByRole("heading", { name: "The Wheel of Time" })).toBeTruthy();
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/work-details?provider=tvmaze&external_id=42");
    const normalizedRoute = screen.getByRole("link", { name: "TVMaze record" });
    expect(normalizedRoute.getAttribute("href")).toBe("https://www.tvmaze.com/shows/42/the-wheel-of-time");
    expect(normalizedRoute.getAttribute("target")).toBe("_blank");
    expect(normalizedRoute.getAttribute("rel")).toContain("noopener");
    expect(screen.queryByText(/^Source$/)).toBeNull();
    await user.click(screen.getByRole("button", { name: "Back to results" }));
    for (const invalidShow of invalidSeries) {
      await user.click(await screen.findByRole("button", { name: `View details for ${invalidShow.title}` }));
      expect(await screen.findByRole("heading", { name: invalidShow.title })).toBeTruthy();
      expect(screen.queryByRole("link", { name: "TVMaze record" })).toBeNull();
      await user.click(screen.getByRole("button", { name: "Back to results" }));
    }
  });

  it("distinguishes total provider failure from a successful empty search", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse({
      results: [],
      sources: [
        { provider: "igdb", state: "unavailable", result_count: 0 },
        { provider: "openlibrary", state: "throttled", result_count: 0 },
        { provider: "tvmaze", state: "unavailable", result_count: 0 },
      ],
    }));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(await screen.findByText("Search sources are temporarily unavailable. Try again shortly.")).toBeTruthy();
    const sourceStatus = screen.getByRole("status", { name: "Search source status" });
    expect(sourceStatus.textContent).toContain("IGDB is temporarily unavailable");
    expect(sourceStatus.textContent).toContain("Open Library is temporarily rate-limited");
    expect(screen.queryByText("No titles found. Try another search.")).toBeNull();
  });

  it("V11-04 makes the complete System status row visible and keyboard actionable", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("fetch", vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(runtimeStatus)));
    render(<App />);

    await user.click(screen.getByRole("button", { name: "Settings" }));
    const systemStatusRow = screen.getByRole("button", { name: "System status" });
    expect(systemStatusRow.querySelector(".settings-status-chevron")?.textContent).toBe("›");
    systemStatusRow.focus();
    await user.keyboard("{Enter}");

    expect(await screen.findByRole("heading", { name: "System status" })).toBeTruthy();
  });

  it("loads, saves, and reopens the persistent metadata-language preference", async () => {
    const user = userEvent.setup();
    let savedLanguage: "en" | "es" | "original" = "en";
    const fetchMock = vi.fn<typeof fetch>(async (input, init) => {
      if (String(input) === "/api/v1/settings/metadata-language" && init?.method === "PATCH") {
        const request = JSON.parse(String(init.body)) as { metadata_language: "en" | "es" | "original" };
        savedLanguage = request.metadata_language;
        return jsonResponse({ metadata_language: savedLanguage });
      }
      if (String(input) === "/api/v1/settings/metadata-language") {
        return jsonResponse({ metadata_language: savedLanguage });
      }
      throw new Error(`unexpected test request: ${String(input)}`);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.click(screen.getByRole("button", { name: "Settings" }));
    const languageSelect = await screen.findByLabelText("Metadata language") as HTMLSelectElement;
    expect(languageSelect.value).toBe("en");
    expect(screen.getByRole("option", { name: "English" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Español" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Original (provider default)" })).toBeTruthy();
    expect(screen.getByText(/affects metadata only, not the language you consume in/i)).toBeTruthy();

    languageSelect.focus();
    await user.selectOptions(languageSelect, "es");
    expect(await screen.findByText("Metadata language saved.")).toBeTruthy();
    expect(languageSelect.value).toBe("es");
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1]?.[1]?.method).toBe("PATCH");

    await user.click(screen.getByRole("button", { name: "Home" }));
    await user.click(screen.getByRole("button", { name: "Settings" }));
    expect((await screen.findByLabelText("Metadata language") as HTMLSelectElement).value).toBe("es");
  });

  it("V11-05 keeps session results factual instead of elevating the first result", async () => {
    const user = userEvent.setup();
    const recentResults = [
      soulcalibur,
      { ...syntheticGame, title: "Night Atlas", medium: "book", work_type: "book" },
    ];
    mockAppFetch(recentResults);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "recent titles");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    expect(await screen.findByRole("button", { name: "View details for Night Atlas" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Home" }));

    expect(screen.getByRole("heading", { name: "Discover something you love." })).toBeTruthy();
    expect(screen.queryByText("From your recent discovery")).toBeNull();
    expect(screen.getByRole("list", { name: "Recently discovered titles" })).toBeTruthy();

    expect(screen.getAllByRole("button", { name: "View details for Soulcalibur II" })).toHaveLength(1);
    const nightAtlasCard = screen.getByRole("button", { name: "View details for Night Atlas" });
    nightAtlasCard.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("heading", { name: "Night Atlas" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Back to Home" })).toBeTruthy();
  });

  it("opens System by keyboard and renders returned Server, Database, and Agent status", async () => {
    vi.stubEnv("VITE_LERNAE_API_BASE_URL", "");
    const user = userEvent.setup();
    const fetchMock = vi.fn<typeof fetch>((input) => Promise.resolve(
      String(input) === "/api/v1/settings/metadata-language"
        ? jsonResponse({ metadata_language: "en" })
        : jsonResponse(runtimeStatus),
    ));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    const settingsButton = screen.getByRole("button", { name: "Settings" });
    settingsButton.focus();
    await user.keyboard("{Enter}");
    const systemButton = screen.getByRole("button", { name: "System status" });
    expect(screen.getByRole("heading", { name: "Settings" })).toBeTruthy();
    systemButton.focus();
    await user.keyboard("{Enter}");

    expect(settingsButton.getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("heading", { name: "System status" })).toBeTruthy();
    expect(await screen.findByText("Online")).toBeTruthy();
    expect(screen.getByText("Ready")).toBeTruthy();
    expect(screen.getByText("Connected")).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/settings/metadata-language");
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/system/status");

    await user.click(screen.getByRole("button", { name: "Home" }));
    expect(screen.getByRole("searchbox", { name: "Search the catalog" })).toBeTruthy();
  });

  it("shows loading and safe unreachable status when the System request fails", async () => {
    const pending = deferred<Response>();
    const fetchMock = vi.fn<typeof fetch>().mockReturnValue(pending.promise);
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    fireEvent.click(screen.getByRole("button", { name: "System status" }));
    expect(screen.getByRole("status").textContent).toContain("Checking local runtime");
    await act(async () => pending.resolve(jsonResponse({ error: "private server detail" }, 503)));

    expect(await screen.findByText("Server unreachable. Start the Lernae Server to check runtime status.")).toBeTruthy();
    expect(screen.getByText("Unreachable")).toBeTruthy();
    expect(screen.queryByText("private server detail")).toBeNull();
  });

  it("refreshes System status every five seconds and stops polling when leaving", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn<typeof fetch>((input) => Promise.resolve(
      String(input) === "/api/v1/settings/metadata-language"
        ? jsonResponse({ metadata_language: "en" })
        : jsonResponse(runtimeStatus),
    ));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    fireEvent.click(screen.getByRole("button", { name: "System status" }));
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await act(async () => vi.advanceTimersByTimeAsync(5000));
    expect(fetchMock).toHaveBeenCalledTimes(3);

    fireEvent.click(screen.getByRole("button", { name: "Home" }));
    await act(async () => vi.advanceTimersByTimeAsync(10000));
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("trims and submits a query with Enter, then renders normalized results", async () => {
    vi.stubEnv("VITE_LERNAE_API_BASE_URL", "");
    const user = userEvent.setup();
    const fetchMock = withoutRelationshipResolutionCalls(
      vi.fn<typeof fetch>().mockResolvedValue(jsonResponse([soulcalibur])),
    );
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    const searchInput = screen.getByRole("searchbox", { name: "Search the catalog" });
    await user.type(searchInput, "  Soul Calibur  ");
    await user.keyboard("{Enter}");

    expect(await screen.findByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/search?q=Soul%20Calibur");
    expect(screen.getByText("2002")).toBeTruthy();
    expect(screen.getByText("Game")).toBeTruthy();
    expect(screen.queryByText("The synthetic GameCube fixture.")).toBeNull();
  });

  it("uses the configured API base URL without a trailing slash", async () => {
    vi.stubEnv("VITE_LERNAE_API_BASE_URL", " https://api.example.test/ ");
    const user = userEvent.setup();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse([soulcalibur]));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(fetchMock.mock.calls[0]?.[0]).toBe("https://api.example.test/api/v1/search?q=Soul%20Calibur");
  });

  it("does not send an empty or whitespace-only query", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse([soulcalibur]));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    const searchInput = screen.getByRole("searchbox", { name: "Search the catalog" });
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.type(searchInput, "   ");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("shows loading while the Server request is pending", async () => {
    const pending = deferred<Response>();
    const fetchMock = withoutRelationshipResolutionCalls(
      vi.fn<typeof fetch>().mockReturnValue(pending.promise),
    );
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(screen.getByRole("status").textContent).toContain("Searching");
    await act(async () => pending.resolve(jsonResponse([])));
  });

  it("does not start a duplicate request for the same pending query", async () => {
    const pending = deferred<Response>();
    const fetchMock = withoutRelationshipResolutionCalls(
      vi.fn<typeof fetch>().mockReturnValue(pending.promise),
    );
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();
    render(<App />);

    const searchInput = screen.getByRole("searchbox", { name: "Search the catalog" });
    let searchButton = screen.getByRole("button", { name: "Search titles" });
    await user.type(searchInput, "Soul Calibur");
    await user.click(searchButton);
    searchButton = screen.getByRole("button", { name: "Search titles" });
    const firstSignal = fetchMock.mock.calls[0]?.[1]?.signal as AbortSignal;
    await user.click(searchButton);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(firstSignal.aborted).toBe(false);
    expect(screen.getByRole("status").textContent).toContain("Searching");
    await act(async () => pending.resolve(jsonResponse([soulcalibur])));
  });

  it("shows an empty state when the Server returns no results", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse([]));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Unknown game");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(await screen.findByText("No titles found. Try another search.")).toBeTruthy();
  });

  it("shows a safe error and retries the same query", async () => {
    const user = userEvent.setup();
    let searchRequestCount = 0;
    const fetchMock = withoutRelationshipResolutionCalls(vi.fn<typeof fetch>((input) => {
      if (String(input).includes("/api/v1/universes/relationships/resolve")) {
        return Promise.resolve(jsonResponse({ error: "no exact Universe name" }, 404));
      }
      searchRequestCount += 1;
      return Promise.resolve(searchRequestCount === 1
        ? jsonResponse({ error: "private server detail" }, 503)
        : jsonResponse([soulcalibur]));
    }));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    const error = await screen.findByRole("alert");
    expect(error.textContent).toContain("Search is temporarily unavailable");
    expect(error.textContent).not.toContain("private server detail");

    await user.click(screen.getByRole("button", { name: "Try again" }));

    expect(await screen.findByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/search?q=Soul%20Calibur");
  });

  it("treats malformed responses as safe errors", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse({ private: "raw body" }));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    const error = await screen.findByRole("alert");
    expect(error.textContent).toContain("Search returned an unexpected response");
    expect(error.textContent).not.toContain("raw body");
  });

  it("prevents a late response from replacing results for a newer query", async () => {
    const first = deferred<Response>();
    const second = deferred<Response>();
    const requests: Array<{ url: string; signal: AbortSignal | null | undefined }> = [];
    const fetchMock = vi.fn<typeof fetch>((input, init) => {
      const url = String(input);
      if (url.includes("/api/v1/universes/relationships/resolve")) {
        return Promise.resolve(jsonResponse({ error: "no exact Universe name" }, 404));
      }
      requests.push({ url, signal: init?.signal });
      return requests.length === 1 ? first.promise : second.promise;
    });
    const filteredFetchMock = withoutRelationshipResolutionCalls(fetchMock);
    vi.stubGlobal("fetch", filteredFetchMock);
    const user = userEvent.setup();
    render(<App />);
    let searchInput = screen.getByRole("searchbox", { name: "Search the catalog" });

    await user.type(searchInput, "Older title");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    searchInput = screen.getByRole("searchbox", { name: "Search the catalog" });
    await user.clear(searchInput);
    await user.type(searchInput, "Newer title");
    await user.click(screen.getByRole("button", { name: "Search titles" }));

    expect(filteredFetchMock).toHaveBeenCalledTimes(2);
    expect(requests[0]?.signal?.aborted).toBe(true);
    await act(async () => second.resolve(jsonResponse([{ ...soulcalibur, title: "Newer result" }])));
    expect(await screen.findByRole("button", { name: "View details for Newer result" })).toBeTruthy();
    await act(async () => first.resolve(jsonResponse([{ ...soulcalibur, title: "Older result" }])));

    expect(screen.getByRole("button", { name: "View details for Newer result" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "View details for Older result" })).toBeNull();
  });

  it("V11-06 returns to Search results from Search-origin Details by keyboard", async () => {
    const user = userEvent.setup();
    mockAppFetch([soulcalibur]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    const selectResult = await screen.findByRole("button", { name: "View details for Soulcalibur II" });
    expect(screen.getByRole("list", { name: "Related results" }).className).toContain("media-grid");
    expect(selectResult.className).toContain("media-card");
    selectResult.focus();
    await user.keyboard("{Enter}");

    expect(screen.getByRole("heading", { name: "Soulcalibur II" })).toBeTruthy();
    expect(document.querySelector(".details-backdrop img")?.getAttribute("src")).toBe(soulcalibur.cover_reference);
    expect(screen.getByText("Release date: 2002-08-26")).toBeTruthy();
    expect(screen.getByText("Release year: 2002")).toBeTruthy();
    expect(screen.getByText("Game")).toBeTruthy();
    expect(screen.queryByText("game · game")).toBeNull();
    expect(screen.getByText("The synthetic GameCube fixture.", { selector: ".detail-summary" })).toBeTruthy();

    const backButton = screen.getByRole("button", { name: "Back to results" });
    expect(backButton.closest(".app-header")).toBeTruthy();
    backButton.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "View details for Soulcalibur II" })).toBeTruthy();
    expect((screen.getByRole("searchbox", { name: "Search the catalog" }) as HTMLInputElement).value)
      .toBe("Soul Calibur");
  });

  it("V11-02 keeps Details artwork, poster, one selector, and action in compact layout", async () => {
    const user = userEvent.setup();
    mockAppFetch([soulcalibur]);
    render(<App />);

    await user.click(screen.getByRole("button", { name: "Search" }));
    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));

    const details = await screen.findByRole("region", { name: "Details for Soulcalibur II" });
    expect(details.querySelector(".details-backdrop img")?.getAttribute("src")).toBe(soulcalibur.cover_reference);
    expect(details.querySelector(".cover-frame img")?.getAttribute("src")).toBe(soulcalibur.cover_reference);
    expect(details.querySelectorAll(".platform-options")).toHaveLength(1);
    const primaryAction = screen.getByRole("button", { name: "PLAY" });
    const platformSelector = details.querySelector(".platform-options");
    expect(primaryAction.compareDocumentPosition(platformSelector!)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
  });

  it("V11-06 returns Home-origin Details to Home with the search session intact", async () => {
    const user = userEvent.setup();
    mockAppFetch([soulcalibur]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await screen.findByRole("button", { name: "View details for Soulcalibur II" });
    await user.click(screen.getByRole("button", { name: "Home" }));
    await user.click(screen.getByRole("button", { name: "View details for Soulcalibur II" }));

    const backButton = screen.getByRole("button", { name: "Back to Home" });
    backButton.focus();
    await user.keyboard("{Enter}");

    expect(screen.getByRole("heading", { name: "Discover something you love." })).toBeTruthy();
    expect(screen.getByRole("list", { name: "Recently discovered titles" })).toBeTruthy();
    expect((screen.getByRole("searchbox", { name: "Search the catalog" }) as HTMLInputElement).value)
      .toBe("Soul Calibur");
  });

  it("V11-07 renders only returned platform labels on Search cards", async () => {
    const user = userEvent.setup();
    const resultWithoutPlatforms = { ...syntheticGame, platforms: undefined };
    const fetchMock = mockAppFetch([soulcalibur, resultWithoutPlatforms]);
    render(<App />);

    const headerSearch = screen.getByRole("searchbox", { name: "Quick catalog search" });
    await user.type(headerSearch, "Soul");
    await user.keyboard("{Enter}");
    const gameCard = await screen.findByRole("button", { name: "View details for Soulcalibur II" });
    const noPlatformCard = screen.getByRole("button", { name: "View details for Synthetic Game" });

    expect(gameCard.querySelector(".media-card-platforms")?.textContent).toBe("Nintendo GameCube");
    expect(noPlatformCard.querySelector(".media-card-platforms")).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("provides a full description disclosure by keyboard", async () => {
    const user = userEvent.setup();
    const fullDescription = Array.from({ length: 12 }, (_, index) =>
      `Chapter ${index + 1} follows the crew as they trace a signal across the outer rim and uncover its origin.`,
    ).join(" ");
    const result = { ...soulcalibur, summary: fullDescription };
    mockAppFetch([result]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), result.title);
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: `View details for ${result.title}` }));
    await screen.findByRole("heading", { name: result.title });

    const disclosureButton = screen.getByRole("button", { name: "Read full description" });
    const fullDescriptionContent = document.getElementById("full-description");
    expect(disclosureButton.getAttribute("aria-expanded")).toBe("false");
    expect(fullDescriptionContent?.hidden).toBe(true);
    disclosureButton.focus();
    await user.keyboard("{Enter}");

    const expandedButton = screen.getByRole("button", { name: "Hide full description" });
    expect(expandedButton.getAttribute("aria-expanded")).toBe("true");
    expect(document.getElementById("full-description")?.hidden).toBe(false);
    expect(document.getElementById("full-description")?.textContent).toBe(fullDescription);
  });

  it("keeps unsupported and unknown media actions unavailable without reaching GameCube PLAY", async () => {
    const user = userEvent.setup();
    for (const [medium, action, message] of [
      ["film", "WATCH", "WATCH is not available for this title yet."],
      ["podcast", "Unavailable", "This media type is not supported yet."],
    ] as const) {
      const result = { ...syntheticGame, title: `Synthetic ${medium}`, medium, work_type: medium };
      const fetchMock = mockAppFetch([result]);
      const { unmount } = render(<App />);

      await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), result.title);
      await user.click(screen.getByRole("button", { name: "Search titles" }));
      await user.click(await screen.findByRole("button", { name: `View details for ${result.title}` }));

      expect(await screen.findByRole("heading", { name: result.title })).toBeTruthy();
      expect(screen.getByRole("button", { name: action }).hasAttribute("disabled")).toBe(true);
      expect(screen.getByText(message)).toBeTruthy();
      await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));
      expect(screen.getByText("Actions are not available for this media yet.")).toBeTruthy();
      expect(fetchMock).toHaveBeenCalledTimes(2);
      unmount();
    }
  });

  it("offers only returned platforms and keeps PLAY disabled for unavailable content", async () => {
    const user = userEvent.setup();
    const fetchMock = mockAppFetch([soulcalibur], {
      inventoryResponses: [jsonResponse({ owned: false, availability: "unavailable" })],
    });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));

    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(true);
    const gameCube = screen.getByRole("button", { name: "Nintendo GameCube" });
    expect(screen.queryByRole("button", { name: "Dreamcast" })).toBeNull();
    gameCube.focus();
    await user.keyboard(" ");

    expect(gameCube.getAttribute("aria-pressed")).toBe("true");
    const playButton = screen.getByRole("button", { name: "PLAY" });
    expect(playButton.hasAttribute("disabled")).toBe(true);
    expect(screen.getByText("This Edition is unavailable for playback.")).toBeTruthy();
    await user.click(playButton);
    expect(await screen.findByText("Availability: Unavailable")).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("does not invent platforms and keeps the primary action disabled when none are returned", async () => {
    const user = userEvent.setup();
    mockAppFetch([{ ...soulcalibur, platforms: [] }]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));

    expect(screen.getByText("No platforms are listed for this title.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Nintendo GameCube" })).toBeNull();
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(true);
  });

  it("resolves the selected work and GameCube edition and enables PLAY for owned archived content", async () => {
    const user = userEvent.setup();
    const fetchMock = mockAppFetch([syntheticGame], { inventoryResponses: [jsonResponse(archivedInventory)] });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic Game");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Synthetic Game" }));
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));

    expect(await screen.findByText("Availability: Archived")).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(3);
    const [url, options] = fetchMock.mock.calls[2] ?? [];
    expect(url).toBe("/api/v1/inventory/resolve");
    expect(options?.method).toBe("POST");
    expect(JSON.parse(String(options?.body))).toEqual({
      work_identity: { provider: "test-catalog", external_id: "record-42" },
      edition: { platform: "gamecube", format: "disc_image" },
    });
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("shows Unavailable when the exact selected edition is not in inventory", async () => {
    const user = userEvent.setup();
    mockAppFetch([syntheticGame], {
      inventoryResponses: [jsonResponse({ owned: false, availability: "unavailable" })],
    });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic Game");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Synthetic Game" }));
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));

    expect(await screen.findByText("Availability: Unavailable")).toBeTruthy();
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(true);
  });

  it("shows loading while inventory resolution is pending, then enables PLAY for owned ready content", async () => {
    const user = userEvent.setup();
    const pending = deferred<Response>();
    const fetchMock = vi.fn<typeof fetch>((input) => {
      const url = String(input);
      return url.includes("/api/v1/search")
        ? Promise.resolve(jsonResponse([syntheticGame]))
        : url.includes("/api/v1/work-details")
          ? Promise.resolve(jsonResponse(workDetailsFor(syntheticGame)))
          : pending.promise;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic Game");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Synthetic Game" }));
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));

    expect(screen.getByText("Checking inventory…")).toBeTruthy();
    await act(async () => pending.resolve(jsonResponse({
      ...archivedInventory,
      availability: "ready",
    })));
    expect(await screen.findByText("Availability: Ready")).toBeTruthy();
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(false);
  });

  it("shows a safe inventory error and never enables PLAY after a failed resolution", async () => {
    const user = userEvent.setup();
    const fetchMock = mockAppFetch([syntheticGame], {
      inventoryResponses: [jsonResponse({ error: "private manifest locator" }, 503)],
    });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic Game");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Synthetic Game" }));

    const error = await screen.findByRole("alert");
    expect(error.textContent).toBe(
      "Inventory status is temporarily unavailable. Select the platform again to retry.",
    );
    expect(error.textContent).not.toContain("private manifest locator");
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/inventory/resolve")).toHaveLength(1);
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(true);
  });

  it("V11-01 keeps GameCube missing inventory configuration distinct from unsupported playback", async () => {
    const user = userEvent.setup();
    const fetchMock = mockAppFetch([soulcalibur], {
      inventoryResponses: [jsonResponse({
        code: "inventory_not_configured",
        error: "inventory resolution is not configured",
      }, 503)],
    });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soulcalibur II");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));

    expect((await screen.findByRole("alert")).textContent).toBe(
      "Your library inventory is not configured.",
    );
    expect(screen.queryByText("Playback is not supported for this platform.")).toBeNull();
    expect(screen.queryByText("Select a platform to check availability.")).toBeNull();
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(true);
    expect(fetchMock.mock.calls[2]?.[0]).toBe("/api/v1/inventory/resolve");
    expect(JSON.parse(String(fetchMock.mock.calls[2]?.[1]?.body))).toEqual({
      work_identity: { provider: "igdb", external_id: "1001" },
      edition: { platform: "gamecube", format: "disc_image" },
    });
  });

  it("V11-01a auto-selects the returned GameCube candidate and resolves canonical Work identity once", async () => {
    const user = userEvent.setup();
    const result = { ...soulcalibur, external_id: "227987" };
    const canonicalDetails: WorkDetails = {
      ...workDetailsFor(result),
      external_id: "1565",
      platform_candidates: [
        platformCandidate("Xbox Series X", "xbox-series-x", "1565", "alternate_platform"),
        platformCandidate("Nintendo GameCube", "nintendo-game-cube", "1565", "canonical_work"),
      ],
    };
    const fetchMock = mockAppFetch([result], {
      detailsByRequestedID: { "227987": canonicalDetails },
      inventoryResponses: [jsonResponse({ owned: false, availability: "unavailable" })],
    });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soulcalibur II");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));

    const gameCube = screen.getByRole("button", { name: "Nintendo GameCube" });
    expect(await screen.findByText("Availability: Unavailable")).toBeTruthy();
    expect(gameCube.getAttribute("aria-pressed")).toBe("true");

    const resolutions = fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/inventory/resolve");
    expect(resolutions).toHaveLength(1);
    expect(resolutions[0]?.[1]?.method).toBe("POST");
    expect(JSON.parse(String(resolutions[0]?.[1]?.body))).toEqual({
      work_identity: { provider: "igdb", external_id: "1565" },
      edition: { platform: "gamecube", format: "disc_image" },
    });
  });

  it("V11-01a avoids Xbox resolution and starts a fresh GameCube request after switching back", async () => {
    const user = userEvent.setup();
    const result = { ...soulcalibur, external_id: "227987" };
    const details: WorkDetails = {
      ...workDetailsFor(result),
      external_id: "1565",
      platform_candidates: [
        platformCandidate("Nintendo GameCube", "nintendo-game-cube", "1565", "canonical_work"),
        platformCandidate("Xbox Series X", "xbox-series-x", "1565", "alternate_platform"),
      ],
    };
    const inventoryRequests: Array<{ pending: ReturnType<typeof deferred<Response>>; signal: AbortSignal }> = [];
    const fetchMock = vi.fn<typeof fetch>((input, init) => {
      const url = String(input);
      if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse([result]));
      if (url.startsWith("/api/v1/work-details")) return Promise.resolve(jsonResponse(details));
      if (url === "/api/v1/inventory/resolve") {
        const pending = deferred<Response>();
        inventoryRequests.push({ pending, signal: init?.signal as AbortSignal });
        return pending.promise;
      }
      return Promise.reject(new Error(`unexpected test request: ${url}`));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soulcalibur II");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));
    expect(await screen.findByText("Checking inventory…")).toBeTruthy();
    expect(inventoryRequests).toHaveLength(1);

    await user.click(screen.getByRole("button", { name: "Xbox Series X" }));
    expect(await screen.findByText("Inventory lookup is not supported for this platform.")).toBeTruthy();
    expect(inventoryRequests[0]?.signal.aborted).toBe(true);
    expect(inventoryRequests).toHaveLength(1);

    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));
    expect(await screen.findByText("Checking inventory…")).toBeTruthy();
    expect(inventoryRequests).toHaveLength(2);

    await act(async () => inventoryRequests[1]?.pending.resolve(jsonResponse({
      owned: false,
      availability: "unavailable",
    })));
    expect(await screen.findByText("Availability: Unavailable")).toBeTruthy();
    await act(async () => inventoryRequests[0]?.pending.resolve(jsonResponse(archivedInventory)));

    expect(screen.getByText("Availability: Unavailable")).toBeTruthy();
    expect(screen.queryByText("Availability: Archived")).toBeNull();
    expect(screen.queryByText("Listed in inventory.")).toBeNull();
    expect(inventoryRequests).toHaveLength(2);
  });

  it("V11-01a never resolves or invents ownership for an unsupported display-only Xbox platform", async () => {
    const user = userEvent.setup();
    const result = { ...soulcalibur, external_id: "227987" };
    const details: WorkDetails = {
      ...workDetailsFor(result),
      external_id: "1565",
      platform_candidates: [platformCandidate("Xbox Series X", "xbox-series-x", "1565", "alternate_platform")],
    };
    const fetchMock = mockAppFetch([result], { detailsByRequestedID: { "227987": details } });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soulcalibur II");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));
    await user.click(await screen.findByRole("button", { name: "Xbox Series X" }));

    expect(screen.getByText("Inventory lookup is not supported for this platform.")).toBeTruthy();
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/inventory/resolve")).toHaveLength(0);
    expect(screen.queryByText("Listed in inventory.")).toBeNull();
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(true);
  });

  it("does not guess an edition format for unsupported platforms outside the current GameCube flow", async () => {
    const user = userEvent.setup();
    const unsupportedGame = {
      ...syntheticGame,
      platforms: [{ name: "Dreamcast", slug: "dreamcast" }],
    };
    const fetchMock = mockAppFetch([unsupportedGame]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic Game");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Synthetic Game" }));
    await user.click(screen.getByRole("button", { name: "Dreamcast" }));

    expect(screen.getByText("Inventory lookup is not supported for this platform.")).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(true);
  });

  it("submits canonical Work plus GameCube Edition only, polls accessible lifecycle progress, and rearms after completion", async () => {
    const user = userEvent.setup();
    const fetchMock = mockAppFetch([syntheticGame], {
      inventoryResponses: [jsonResponse(archivedInventory)],
      playResponses: [jsonResponse({ operation_id: "operation-123" }, 202)],
      playPollResponses: [
        jsonResponse({ error: "private polling path /mnt/roms" }, 503),
        jsonResponse({ operation_id: "operation-123", status: "waiting_on_agent", phase: "restore", restore_progress: { current_bytes: 25, total_bytes: 100 } }),
        jsonResponse({ operation_id: "operation-123", status: "running", phase: "launch" }),
        jsonResponse({ operation_id: "operation-123", status: "waiting_on_agent", phase: "playing", session_id: "session-456" }),
        jsonResponse({ operation_id: "operation-123", status: "succeeded", phase: "playing", session_id: "session-456" }),
      ],
    });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic Game");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Synthetic Game" }));
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));
    screen.getByText("Availability: Archived");

    const play = screen.getByRole("button", { name: "PLAY" });
    expect(play.hasAttribute("disabled")).toBe(false);
    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(play);
      fireEvent.click(play);
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(screen.getByText("PLAY accepted. Preparing your game…")).toBeTruthy();
    expect(play.hasAttribute("disabled")).toBe(true);
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/play")).toHaveLength(1);
    const post = fetchMock.mock.calls.find(([url]) => String(url) === "/api/v1/play");
    expect(post?.[1]?.method).toBe("POST");
    expect(JSON.parse(String(post?.[1]?.body))).toEqual({
      work_identity: { provider: "test-catalog", external_id: "record-42" },
      edition: { platform: "gamecube", format: "disc_image" },
    });
    expect(JSON.stringify(post?.[1]?.body)).not.toMatch(/asset|path|locator|rclone|dolphin|shell|size|flags/i);
    expect(screen.queryByRole("button", { name: /stop|kill/i })).toBeNull();

    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByText("PLAY status is temporarily unavailable. Retrying automatically…")).toBeTruthy();
    expect(screen.queryByText(/private polling path/i)).toBeNull();

    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByText("Restoring game…")).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Restore progress" })).toBeTruthy();
    expect(screen.getByText("Restored 25 of 100 bytes.")).toBeTruthy();

    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByText("Launching game…")).toBeTruthy();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByText("Playing game…")).toBeTruthy();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByText("PLAY completed.")).toBeTruthy();
    expect(play.hasAttribute("disabled")).toBe(false);
    expect(screen.queryByRole("button", { name: /stop|kill/i })).toBeNull();
    expect(fetchMock.mock.calls.filter(([url]) => String(url).startsWith("/api/v1/play/operation-123"))).toHaveLength(5);
  });

  it("shows safe retryable submission and terminal errors without leaking API details", async () => {
    const user = userEvent.setup();
    const fetchMock = mockAppFetch([syntheticGame], {
      inventoryResponses: [jsonResponse(archivedInventory)],
      playResponses: [
        jsonResponse({ error: "private path /mnt/roms/game.iso rclone token" }, 503),
        jsonResponse({ operation_id: "operation-456" }, 202),
      ],
      playPollResponses: [
        jsonResponse({
          operation_id: "operation-456", status: "failed", phase: "restore",
          error: { category: "restore_failed", message: "private restore path /mnt/secret" },
        }),
      ],
    });
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic Game");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Synthetic Game" }));
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));
    screen.getByText("Availability: Archived");

    const play = screen.getByRole("button", { name: "PLAY" });
    await user.click(play);
    expect(screen.getByRole("alert").textContent).toContain("PLAY could not be started safely. Try again shortly.");
    expect(screen.queryByText(/private path|rclone token/i)).toBeNull();
    expect(play.hasAttribute("disabled")).toBe(false);

    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(play);
      fireEvent.click(play);
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(screen.getByText("PLAY accepted. Preparing your game…")).toBeTruthy();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByRole("alert").textContent).toContain("The game could not be restored safely.");
    expect(screen.queryByText(/private restore path/i)).toBeNull();
    expect(play.hasAttribute("disabled")).toBe(false);
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/play")).toHaveLength(2);
    expect(screen.queryByRole("button", { name: /stop|kill/i })).toBeNull();
  });

  it("aborts a stale inventory request when selection changes and ignores its late response", async () => {
    const user = userEvent.setup();
    const firstInventory = deferred<Response>();
    const secondInventory = deferred<Response>();
    let inventoryRequestCount = 0;
    const secondGame = { ...syntheticGame, external_id: "record-43", title: "Synthetic Sequel" };
    const fetchMock = vi.fn<typeof fetch>((input) => {
      const url = String(input);
      if (url.includes("/api/v1/search")) return Promise.resolve(jsonResponse([syntheticGame, secondGame]));
      if (url.includes("/api/v1/universes/relationships/resolve")) {
        return Promise.resolve(jsonResponse({ error: "no exact Universe name" }, 404));
      }
      if (url.includes("/api/v1/work-details")) {
        const externalID = new URL(url, "http://lernae.test").searchParams.get("external_id");
        const result = externalID === secondGame.external_id ? secondGame : syntheticGame;
        return Promise.resolve(jsonResponse(workDetailsFor(result)));
      }
      inventoryRequestCount += 1;
      return inventoryRequestCount === 1 ? firstInventory.promise : secondInventory.promise;
    });
    const filteredFetchMock = withoutRelationshipResolutionCalls(fetchMock);
    vi.stubGlobal("fetch", filteredFetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Synthetic Game" }));
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));
    const firstSignal = filteredFetchMock.mock.calls[2]?.[1]?.signal as AbortSignal;

    await user.click(screen.getByRole("button", { name: "Back to results" }));
    await user.click(screen.getByRole("button", { name: "View details for Synthetic Sequel" }));
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));

    expect(firstSignal.aborted).toBe(true);
    expect(JSON.parse(String(filteredFetchMock.mock.calls[4]?.[1]?.body)).work_identity.external_id).toBe("record-43");
    await act(async () => secondInventory.resolve(jsonResponse(archivedInventory)));
    expect(await screen.findByText("Availability: Archived")).toBeTruthy();
    await act(async () => firstInventory.resolve(jsonResponse({ owned: false, availability: "unavailable" })));

    expect(screen.getByRole("heading", { name: "Synthetic Sequel" })).toBeTruthy();
    expect(screen.getByText("Availability: Archived")).toBeTruthy();
    expect(screen.queryByText("Availability: Unavailable")).toBeNull();
  });

  it("shows a stable cover placeholder when artwork fails to load", async () => {
    const user = userEvent.setup();
    mockAppFetch([soulcalibur]);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));
    const cover = screen.getByRole("img", { name: "Soulcalibur II cover" });
    fireEvent.error(cover);

    expect(await screen.findByRole("img", { name: "Cover art unavailable" })).toBeTruthy();
  });

  it("loads canonical details only after opening a child result and hands GameCube to inventory by parent identity", async () => {
    const user = userEvent.setup();
    const childResult = { ...soulcalibur, external_id: "227987", title: "Soulcalibur II (GameCube)" };
    const canonicalDetails: WorkDetails = {
      ...workDetailsFor(soulcalibur),
      external_id: "1565",
      platform_candidates: [
        platformCandidate("Arcade", "arcade", "1565", "canonical_work"),
        platformCandidate("Nintendo GameCube", "nintendo-gamecube", "227987", "parent_game_child"),
        platformCandidate("PlayStation 2", "ps2", "227986", "parent_game_child"),
        platformCandidate("Xbox", "xbox", "227989", "parent_game_child"),
      ],
    };
    const fetchMock = mockAppFetch([childResult], {
      detailsByRequestedID: { "227987": canonicalDetails },
      inventoryResponses: [jsonResponse(archivedInventory)],
    });
    render(<App />);
    const searchInput = screen.getByRole("searchbox", { name: "Search the catalog" });
    await user.type(searchInput, "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await screen.findByRole("button", { name: "View details for Soulcalibur II (GameCube)" });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/search?q=Soul%20Calibur");
    await user.click(screen.getByRole("button", { name: "View details for Soulcalibur II (GameCube)" }));

    expect(await screen.findByRole("heading", { name: "Soulcalibur II" })).toBeTruthy();
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/work-details?provider=igdb&external_id=227987");
    for (const platform of ["Arcade", "Nintendo GameCube", "PlayStation 2", "Xbox"]) {
      expect(screen.getByRole("button", { name: platform })).toBeTruthy();
    }
    await user.click(screen.getByRole("button", { name: "Nintendo GameCube" }));
    expect(await screen.findByText("Availability: Archived")).toBeTruthy();
    expect(JSON.parse(String(fetchMock.mock.calls[2]?.[1]?.body))).toEqual({
      work_identity: { provider: "igdb", external_id: "1565" },
      edition: { platform: "gamecube", format: "disc_image" },
    });
    expect(screen.getByRole("button", { name: "PLAY" }).hasAttribute("disabled")).toBe(false);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "PLAY" }));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(JSON.parse(String(fetchMock.mock.calls[3]?.[1]?.body))).toEqual({
      work_identity: { provider: "igdb", external_id: "1565" },
      edition: { platform: "gamecube", format: "disc_image" },
    });

  });

  it("shows Work Details loading and aborts/ignores a stale result after changing selection", async () => {
    const user = userEvent.setup();
    const firstDetails = deferred<Response>();
    const secondDetails = deferred<Response>();
    let detailsRequestCount = 0;
    const fetchMock = vi.fn<typeof fetch>((input) => {
      const url = String(input);
      if (url.includes("/api/v1/search")) return Promise.resolve(jsonResponse([soulcalibur, syntheticGame]));
      if (url.includes("/api/v1/universes/relationships/resolve")) {
        return Promise.resolve(jsonResponse({ error: "no exact Universe name" }, 404));
      }
      if (url.includes("/api/v1/work-details")) {
        detailsRequestCount += 1;
        return detailsRequestCount === 1 ? firstDetails.promise : secondDetails.promise;
      }
      return Promise.resolve(jsonResponse({ owned: false, availability: "unavailable" }));
    });
    const filteredFetchMock = withoutRelationshipResolutionCalls(fetchMock);
    vi.stubGlobal("fetch", filteredFetchMock);
    render(<App />);
    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Synthetic");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));
    expect(screen.getByRole("status").textContent).toContain("Loading work details");
    const firstSignal = filteredFetchMock.mock.calls[1]?.[1]?.signal as AbortSignal;

    await user.click(screen.getByRole("button", { name: "Back to results" }));
    await user.click(screen.getByRole("button", { name: "View details for Synthetic Game" }));
    expect(firstSignal.aborted).toBe(true);
    await act(async () => secondDetails.resolve(jsonResponse(workDetailsFor(syntheticGame))));
    expect(await screen.findByRole("heading", { name: "Synthetic Game" })).toBeTruthy();
    await act(async () => firstDetails.resolve(jsonResponse(workDetailsFor(soulcalibur))));

    expect(screen.getByRole("heading", { name: "Synthetic Game" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Soulcalibur II" })).toBeNull();
  });

  it("shows a safe Work Details error and never renders untrusted provider content", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn<typeof fetch>((input) => String(input).includes("/api/v1/search")
      ? Promise.resolve(jsonResponse([soulcalibur]))
      : Promise.resolve(jsonResponse({ error: "private oauth and access token" }, 503)));
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Soul Calibur");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(await screen.findByRole("button", { name: "View details for Soulcalibur II" }));

    const error = await screen.findByRole("alert");
    expect(error.textContent).toContain("Work details are temporarily unavailable");
    expect(error.textContent).not.toContain("private oauth");
    expect(screen.queryByRole("button", { name: "PLAY" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Nintendo GameCube" })).toBeNull();
  });

  it("requires an explicit resolver action and selection before creating a Universe", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary",
      external_id: "/works/OL100W",
      title: "The Eye of the World",
      medium: "literature",
      work_type: "book",
      media_type: "book",
      artwork_url: "https://covers.openlibrary.org/b/id/100-M.jpg",
    };
    const series: SearchResult = {
      provider: "tvmaze",
      external_id: "200",
      title: "The Wheel of Time",
      medium: "video",
      work_type: "series",
      media_type: "tv",
      artwork_url: "https://static.tvmaze.com/example.jpg",
    };
    const candidates = {
      candidates: [{
        title: "The Wheel of Time",
        existing: false,
        evidence: "exact_title_anchor",
        reason: "The normalized user query exactly matches the result title and supplies this candidate's title anchor.",
        confidence: 0.9,
        confidence_level: "high",
        memberships: [
          universeProposal(book, "book"),
          universeProposal(series, "tv"),
        ],
      }],
    };
    const created = universeDetail("universe-new", "The Wheel of Time", [book]);
    const createBodies: Record<string, unknown>[] = [];
    const fetchMock = vi.fn<typeof fetch>((input, init) => {
      const url = String(input);
      if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse({
        results: [book, series],
        sources: [],
        universe_discovery: { state: "not_eligible", memberships_added: 0, confirmed_by_user: false },
      }));
      if (url === "/api/v1/universes/resolve") return Promise.resolve(jsonResponse(candidates));
      if (url === "/api/v1/universes/universe-new") return Promise.resolve(jsonResponse(created));
      if (url === "/api/v1/universes" && init?.method === "POST") {
        createBodies.push(JSON.parse(String(init.body)) as Record<string, unknown>);
        return Promise.resolve(createBodies.length === 1
          ? jsonResponse({ error: "safe retry fixture" }, 503)
          : jsonResponse(created, 201));
      }
      return Promise.reject(new Error(`unexpected test request: ${url}`));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    expect(await screen.findByRole("button", { name: "View details for The Eye of the World" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Review grouping" })).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url]) => String(url) === "/api/v1/universes/resolve")).toBe(false);

    await user.click(screen.getByRole("button", { name: "Review grouping" }));
    expect(await screen.findByRole("heading", { name: "Possible groups" })).toBeTruthy();
    expect(screen.getByText("Book", { selector: ".resolver-media-type" })).toBeTruthy();
    expect(screen.getByText("TV", { selector: ".resolver-media-type" })).toBeTruthy();
    expect(screen.getByText("openlibrary · /works/OL100W")).toBeTruthy();
    expect(screen.getByText("tvmaze · 200")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "READ" })).toBeNull();
    expect(screen.queryByRole("button", { name: "WATCH" })).toBeNull();

    const includeBook = screen.getByRole("checkbox", { name: "Include The Eye of the World from openlibrary (/works/OL100W)" });
    includeBook.focus();
    await user.keyboard(" ");
    expect(includeBook.getAttribute("checked")).not.toBe("true");
    expect((includeBook as HTMLInputElement).checked).toBe(true);
    const createButton = screen.getByRole("button", { name: "Create Universe with selected Works" });
    createButton.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByText(/could not all be confirmed/)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Create Universe with selected Works" }));

    expect(await screen.findByRole("heading", { name: "The Wheel of Time" })).toBeTruthy();
    expect(createBodies).toHaveLength(2);
    expect(createBodies[0]?.operation_id).toMatch(/^ui-[a-f0-9]{32}$/);
    expect(createBodies[1]?.operation_id).toBe(createBodies[0]?.operation_id);
    expect(createBodies[1]?.title).toBe("The Wheel of Time");
    expect(createBodies[1]?.works).toEqual([{
      provider: "openlibrary",
      external_id: "/works/OL100W",
      medium: "literature",
      work_type: "book",
      title: "The Eye of the World",
    }]);
    expect(JSON.stringify(createBodies[1])).not.toContain("artwork_url");
    expect(screen.getByRole("heading", { name: "Book" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Manga" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "TV" })).toBeNull();
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/universes/resolve")).toHaveLength(1);
  });

  it("opens Universe matches, manages exact Works, and keeps removals/rejections explicit", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary", external_id: "/works/OL300W", title: "The Eye of the World",
      medium: "literature", work_type: "book", media_type: "book",
    };
    const series: SearchResult = {
      provider: "tvmaze", external_id: "301", title: "A Connected Series",
      medium: "video", work_type: "series", media_type: "tv",
    };
    const otherBook: SearchResult = {
      provider: "openlibrary", external_id: "/works/OL302W", title: "The Great Hunt",
      medium: "literature", work_type: "book", media_type: "book",
    };
    let detail = universeDetail("universe-shared", "The Wheel of Time", [book]);
    const fetchMock = vi.fn<typeof fetch>((input, init) => {
      const url = String(input);
      if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse({
        results: [book, series, otherBook],
        sources: [],
        universe_discovery: {
          state: "reused",
          universe_id: detail.id,
          title: detail.title,
          existence_confidence: 1,
          memberships_added: 0,
          provenance: "manual",
          confirmed_by_user: true,
        },
      }));
      if (url === `/api/v1/universes/${detail.id}` && (!init?.method || init.method === "GET")) return Promise.resolve(jsonResponse(detail));
      if (url === `/api/v1/universes/${detail.id}/memberships` && init?.method === "DELETE") {
        const request = JSON.parse(String(init.body)) as { provider: string; external_id: string };
        const removed = detail.memberships.find((work) => work.provider === request.provider && work.external_id === request.external_id);
        detail = {
          ...detail,
          memberships: detail.memberships.filter((work) => work !== removed),
          exclusions: removed ? [...detail.exclusions, { ...removed, reason: "manual exclusion" }] : detail.exclusions,
        };
        return Promise.resolve(jsonResponse(detail));
      }
      if (url === `/api/v1/universes/${detail.id}/reject` && init?.method === "POST") {
        const request = JSON.parse(String(init.body)) as { work: SearchResult };
        detail = {
          ...detail,
          exclusions: [...detail.exclusions, { ...request.work, reason: "manual exclusion" }],
        };
        return Promise.resolve(jsonResponse(detail));
      }
      if (url === `/api/v1/universes/${detail.id}/reset` && init?.method === "POST") {
        const request = JSON.parse(String(init.body)) as { external_id: string };
        detail = { ...detail, exclusions: detail.exclusions.filter((work) => work.external_id !== request.external_id) };
        return Promise.resolve(jsonResponse(detail));
      }
      if (url === `/api/v1/universes/${detail.id}/memberships` && init?.method === "POST") {
        const request = JSON.parse(String(init.body)) as SearchResult;
        detail = { ...detail, memberships: [...detail.memberships, { ...membershipFrom(request), confirmed_by_user: true, provenance: "manual", confidence: 1 }] };
        return Promise.resolve(jsonResponse(detail, 201));
      }
      return Promise.reject(new Error(`unexpected test request: ${url}`));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Wheel of Time");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    const universeMatch = await screen.findByRole("button", { name: "Open Universe: The Wheel of Time" });
    await user.click(universeMatch);
    expect(await screen.findByRole("heading", { name: "The Wheel of Time" })).toBeTruthy();
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/universes/universe-shared")).toHaveLength(1);
    expect(screen.getByRole("heading", { name: "Book" })).toBeTruthy();
    expect(screen.getByText(/Confirmed by you/)).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Manage Works" }));
    expect(screen.getByRole("heading", { name: "Manage Works" })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Exclude The Great Hunt/ })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: /Exclude The Great Hunt/ }));
    expect(await screen.findByText("The Great Hunt — Excluded")).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === "/api/v1/universes/universe-shared/reject" && init?.method === "POST")).toBe(true);
    expect(screen.queryByRole("button", { name: /Add and confirm The Great Hunt/ })).toBeNull();
    await user.click(screen.getByRole("button", { name: /Reset exclusion for The Great Hunt/ }));
    expect(await screen.findByRole("button", { name: /Add and confirm The Great Hunt/ })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: /Add and confirm A Connected Series/ }));
    expect(await screen.findByText("A Connected Series")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "TV" })).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === "/api/v1/universes/universe-shared/memberships" && init?.method === "POST")).toBe(true);
    await user.click(screen.getByRole("button", { name: /Remove The Eye of the World from this Universe/ }));
    expect(await screen.findByText("The Eye of the World — Excluded")).toBeTruthy();
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === "/api/v1/universes/universe-shared/memberships" && init?.method === "DELETE")).toBe(true);
  });

  it("returns from a Universe match to the same Search filter and focus target", async () => {
    const user = userEvent.setup();
    const book: SearchResult = {
      provider: "openlibrary", external_id: "/works/OL500W", title: "A Search Book",
      medium: "literature", work_type: "book", media_type: "book",
    };
    const detail = universeDetail("universe-focus", "A Shared Story", [book]);
    const fetchMock = vi.fn<typeof fetch>((input) => {
      const url = String(input);
      if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse({
        results: [book],
        sources: [],
        universe_discovery: {
          state: "reused",
          universe_id: detail.id,
          title: detail.title,
          existence_confidence: 1,
          memberships_added: 0,
          provenance: "manual",
          confirmed_by_user: true,
        },
      }));
      if (url === `/api/v1/universes/${detail.id}`) return Promise.resolve(jsonResponse(detail));
      return Promise.reject(new Error(`unexpected test request: ${url}`));
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    await user.type(screen.getByRole("searchbox", { name: "Search the catalog" }), "Shared Story");
    await user.click(screen.getByRole("button", { name: "Search titles" }));
    await user.click(screen.getByRole("button", { name: "Books" }));
    const universeMatch = await screen.findByRole("button", { name: "Open Universe: A Shared Story" });
    await user.click(universeMatch);
    expect(await screen.findByRole("heading", { name: "A Shared Story" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Back to results" }));

    expect(screen.getByRole("button", { name: "Books" }).getAttribute("aria-pressed")).toBe("true");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Open Universe: A Shared Story" }));
    await user.click(screen.getByRole("button", { name: "Open Universe: A Shared Story" }));
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/universes/universe-focus")).toHaveLength(2);
  });
});

function membershipFrom(result: SearchResult) {
  return {
    provider: result.provider,
    external_id: result.external_id,
    title: result.title,
    medium: result.medium,
    work_type: result.work_type,
  };
}

function universeProposal(result: SearchResult, mediaType: string) {
  return {
    universe_title: "The Wheel of Time",
    provider: result.provider,
    external_id: result.external_id,
    media_type: mediaType,
    title: result.title,
    existing_state: "none",
    confirmed_by_user: false,
    evidence: "exact_title_anchor",
    reason: "The normalized user query exactly matches the result title and supplies this candidate's title anchor.",
    confidence: 0.9,
    confidence_level: "high",
    suppressed: false,
    requires_confirmation: true,
  };
}

function universeDetail(id: string, title: string, works: SearchResult[]) {
  return {
    id,
    title,
    existence_confidence: 1,
    provenance: "manual",
    confirmed_by_user: true,
    memberships: works.map((result) => ({
      ...membershipFrom(result),
      provenance: "manual",
      confidence: 1,
      confirmed_by_user: true,
    })),
    suggestions: [],
    exclusions: [] as Array<ReturnType<typeof membershipFrom> & { reason: string }>,
    aliases: [] as Array<{
      value: string;
      language: string | null;
      provenance: string;
      confidence: number;
      confirmed_by_user: boolean;
    }>,
    memberships_truncated: false,
    aliases_truncated: false,
    suggestions_truncated: false,
    exclusions_truncated: false,
  };
}

function jsonResponse(payload: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: vi.fn().mockResolvedValue(payload),
  } as unknown as Response;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

function workDetailsFor(result: SearchResult): WorkDetails {
  return {
    provider: result.provider,
    external_id: result.external_id,
    title: result.title,
    release_date: result.release_date,
    release_year: result.release_year,
    summary: result.summary,
    cover_reference: result.cover_reference,
    source_url: result.source_url,
    medium: result.medium,
    work_type: result.work_type,
    platform_candidates: (result.platforms ?? []).map((platform) => platformCandidate(
      platform.name,
      platform.slug ?? "",
      result.external_id,
      "canonical_work",
      result.provider,
    )),
  };
}

function platformCandidate(name: string, slug: string, externalID: string, relation: string, provider = "igdb") {
  return {
    platform: { name, slug },
    provenance: [{ provider, external_id: externalID, relation }],
  };
}

function mockAppFetch(results: SearchResult[], options: {
  detailsByRequestedID?: Record<string, WorkDetails>;
  inventoryResponses?: Response[];
  playResponses?: Response[];
  playPollResponses?: Response[];
} = {}) {
  let inventoryIndex = 0;
  let playIndex = 0;
  let playPollIndex = 0;
  const fetchMock = vi.fn<typeof fetch>((input) => {
    const url = String(input);
    if (url.startsWith("/api/v1/search")) return Promise.resolve(jsonResponse(results));
    if (url === "/api/v1/universes/relationships/resolve") {
      return Promise.resolve(jsonResponse({ error: "no exact Universe name" }, 404));
    }
    if (url.startsWith("/api/v1/work-details")) {
      const externalID = new URL(url, "http://lernae.test").searchParams.get("external_id") ?? "";
      const result = results.find((candidate) => candidate.external_id === externalID);
      const details = options.detailsByRequestedID?.[externalID] ?? (result && workDetailsFor(result));
      return Promise.resolve(details
        ? jsonResponse(details)
        : jsonResponse({ error: "not found" }, 404));
    }
    if (url === "/api/v1/inventory/resolve") {
      const response = options.inventoryResponses?.[inventoryIndex] ?? jsonResponse({ owned: false, availability: "unavailable" });
      inventoryIndex += 1;
      return Promise.resolve(response);
    }
    if (url === "/api/v1/play") {
      const response = options.playResponses?.[playIndex] ?? jsonResponse({ operation_id: "operation-default" }, 202);
      playIndex += 1;
      return Promise.resolve(response);
    }
    if (url.startsWith("/api/v1/play/")) {
      const responses = options.playPollResponses ?? [];
      const response = responses[Math.min(playPollIndex, responses.length - 1)] ?? jsonResponse({
        operation_id: url.slice("/api/v1/play/".length), status: "running", phase: "launch",
      });
      playPollIndex += 1;
      return Promise.resolve(response);
    }
    return Promise.reject(new Error(`unexpected test request: ${url}`));
  });
  const filteredFetchMock = withoutRelationshipResolutionCalls(fetchMock);
  vi.stubGlobal("fetch", filteredFetchMock);
  return filteredFetchMock;
}

function withoutRelationshipResolutionCalls<T>(fetchMock: T): T {
  const original = fetchMock as unknown as {
    (...args: unknown[]): unknown;
    mock: { calls: unknown[][]; [key: string]: unknown };
    getMockName?: () => string;
  };
  const wrapped = (...args: unknown[]) => original(...args);
  Object.defineProperties(wrapped, {
    _isMockFunction: { value: true },
    getMockName: { value: () => original.getMockName?.() ?? "fetchMock" },
    mock: {
      get: () => {
        const state = original.mock;
        return {
          ...state,
          calls: state.calls.filter((call) => !String(call[0]).includes("/api/v1/universes/relationships/resolve")),
        };
      },
    },
  });
  return wrapped as unknown as T;
}
