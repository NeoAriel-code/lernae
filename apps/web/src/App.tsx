import { useEffect, useLayoutEffect, useRef, useState, type FormEvent, type RefObject } from "react";
import {
  UniverseExactNameMatch,
  UniverseDetailView,
  UniverseMatches,
  UniverseResolverReview,
} from "./Universe";
import {
  parseUniverseCandidates,
  parseUniverseDetail,
  parseUniverseDiscovery,
  searchResultUniverseWorkList,
  universeWorkFromResult,
  workIdentity,
  type UniverseCandidate,
  type UniverseDetailState,
  type UniverseDiscovery,
  type UniverseProposal,
  type UniverseResolverState,
  type UniverseSummary,
  type UniverseWorkInput,
} from "./universe-model";

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
  relevance?: SearchRelevance;
};

type SearchRelevance = "best" | "related" | "weak";

type SearchSourceStatus = {
  provider: string;
  state: "fresh" | "available" | "empty" | "throttled" | "unavailable" | "stale";
  result_count: number;
  error_code?: string;
};

type SearchEnvelope = {
  results: SearchResult[];
  sources: SearchSourceStatus[];
  universeDiscovery?: UniverseDiscovery;
};

type WorkDetails = Omit<SearchResult, "platforms"> & {
  language?: string;
  status?: string;
  genres?: string[];
  network?: string;
  web_channel?: string;
  season_count?: number | null;
  series?: WorkSeries[];
  series_truncated?: boolean;
  fallback_cover_reference?: string;
  representative_edition?: RepresentativeEdition;
  platform_candidates: PlatformCandidate[];
};

type WorkSeriesPeer = {
  provider: string;
  external_id: string;
  title: string;
  ordinal: string | null;
};

type WorkSeries = {
  collection_id: string;
  title: string;
  ordinal: string | null;
  known_total: number | null;
  more_in_series: WorkSeriesPeer[];
  more_in_series_truncated: boolean;
};

type ExactUniverseAliasState =
  | { kind: "idle" }
  | { kind: "loading"; query: string }
  | { kind: "matched"; query: string; universe: UniverseSummary }
  | { kind: "not_found"; query: string }
  | { kind: "error"; query: string };

type RepresentativeEdition = {
  external_id?: string;
  title?: string;
  language?: string;
  publication_date?: string;
  cover_reference?: string;
};

type PlatformCandidate = {
  platform: Platform;
  provenance: Array<{ provider: string; external_id: string; relation: string }>;
};

type SearchState =
  | { kind: "idle" }
  | { kind: "loading"; query: string }
  | { kind: "results"; query: string; results: SearchResult[]; sources: SearchSourceStatus[]; universeDiscovery?: UniverseDiscovery }
  | { kind: "empty"; query: string; sources: SearchSourceStatus[]; universeDiscovery?: UniverseDiscovery }
  | { kind: "error"; query: string; message: string };

type ComponentStatus = { status: string };

type SystemStatus = {
  server: ComponentStatus;
  database: ComponentStatus;
  agent: ComponentStatus;
};

type SystemLoadState =
  | { kind: "loading" }
  | { kind: "unreachable" }
  | { kind: "online"; status: SystemStatus };

type MetadataLanguage = "en" | "es" | "original";

type InventoryAvailability = "unavailable" | "archived" | "ready";

type InventoryResolution = {
  owned: boolean;
  availability: InventoryAvailability;
};

type InventoryResolutionResponse = InventoryResolution & {
  work_id?: string;
  edition_id?: string;
  asset_ids?: string[];
};

type InventoryResolutionState =
  | { kind: "idle" }
  | { kind: "unsupported" }
  | { kind: "loading" }
  | { kind: "resolved"; resolution: InventoryResolution }
  | { kind: "unconfigured" }
  | { kind: "error" };

type PlayOperation = {
  operationID: string;
  status: "queued" | "running" | "waiting_on_agent" | "succeeded" | "failed" | "interrupted" | "cancelled" | "unknown";
  phase?: "restore" | "launch" | "playing";
  restoreProgress?: { currentBytes: number; totalBytes: number };
  sessionID?: string;
  errorCategory?: string;
};

type PlayState =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "active"; operationID: string; operation: PlayOperation | null; pollingError: boolean }
  | { kind: "completed" }
  | { kind: "error"; message: string };

type WorkDetailsState =
  | { kind: "loading" }
  | { kind: "error" }
  | { kind: "ready"; details: WorkDetails };

const unavailableMessage = "Search is temporarily unavailable";
const invalidResponseMessage = "Search returned an unexpected response";

function apiBaseUrl(): string {
  return (import.meta.env.VITE_LERNAE_API_BASE_URL ?? "").trim().replace(/\/+$/, "");
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isMetadataLanguage(value: unknown): value is MetadataLanguage {
  return value === "en" || value === "es" || value === "original";
}

function parseMetadataLanguageResponse(value: unknown): MetadataLanguage | null {
  return isRecord(value) && isMetadataLanguage(value.metadata_language) ? value.metadata_language : null;
}

function isPlatform(value: unknown): value is Platform {
  return isRecord(value) && typeof value.name === "string" &&
    (value.slug === undefined || typeof value.slug === "string");
}

function isSearchResult(value: unknown): value is SearchResult {
  if (!isRecord(value)) return false;

  return (
    typeof value.provider === "string" &&
    typeof value.external_id === "string" &&
    typeof value.title === "string" &&
    typeof value.medium === "string" &&
    typeof value.work_type === "string" &&
    (value.release_date === undefined || typeof value.release_date === "string") &&
    (value.release_year === undefined || typeof value.release_year === "number") &&
    (value.summary === undefined || typeof value.summary === "string") &&
    (value.cover_reference === undefined || typeof value.cover_reference === "string") &&
    (value.media_type === undefined || typeof value.media_type === "string") &&
    (value.subtitle === undefined || typeof value.subtitle === "string") &&
    (value.creators === undefined || (Array.isArray(value.creators) && value.creators.every((creator) => typeof creator === "string"))) &&
    (value.year === undefined || typeof value.year === "number") &&
    (value.network === undefined || typeof value.network === "string") &&
    (value.web_channel === undefined || typeof value.web_channel === "string") &&
    (value.artwork_url === undefined || typeof value.artwork_url === "string") &&
    (value.source_url === undefined || typeof value.source_url === "string") &&
    (value.source_rank === undefined || typeof value.source_rank === "number") &&
    (value.score === undefined || typeof value.score === "number") &&
    (value.relevance === undefined || ["best", "related", "weak"].includes(String(value.relevance))) &&
    (value.platforms === undefined ||
      (Array.isArray(value.platforms) && value.platforms.every(isPlatform)))
  );
}

function isSearchResults(value: unknown): value is SearchResult[] {
  return Array.isArray(value) && value.every(isSearchResult);
}

function isSearchSourceStatus(value: unknown): value is SearchSourceStatus {
  return isRecord(value) && typeof value.provider === "string" &&
    ["fresh", "available", "empty", "throttled", "unavailable", "stale"].includes(String(value.state)) &&
    typeof value.result_count === "number" && Number.isInteger(value.result_count) && value.result_count >= 0 &&
    (value.error_code === undefined || typeof value.error_code === "string");
}

function parseSearchEnvelope(value: unknown): SearchEnvelope | null {
  // Arrays remain accepted during rollout for older local servers and fixtures.
  if (isSearchResults(value)) return { results: value, sources: [] };
  if (!isRecord(value) || !isSearchResults(value.results) || !Array.isArray(value.sources) ||
    !value.sources.every(isSearchSourceStatus)) return null;
  const universeDiscovery = value.universe_discovery === undefined || value.universe_discovery === null
    ? undefined
    : parseUniverseDiscovery(value.universe_discovery);
  if (value.universe_discovery !== undefined && value.universe_discovery !== null && !universeDiscovery) return null;
  return {
    results: value.results,
    sources: value.sources,
    universeDiscovery: universeDiscovery ?? undefined,
  };
}

function isWorkDetails(value: unknown): value is WorkDetails {
  if (!isRecord(value)) return false;
  return typeof value.provider === "string" && typeof value.external_id === "string" &&
    typeof value.title === "string" && typeof value.medium === "string" &&
    typeof value.work_type === "string" &&
    (value.release_date === undefined || typeof value.release_date === "string") &&
    (value.release_year === undefined || typeof value.release_year === "number") &&
    (value.summary === undefined || typeof value.summary === "string") &&
    (value.cover_reference === undefined || typeof value.cover_reference === "string") &&
    (value.fallback_cover_reference === undefined ||
      (value.provider === "openlibrary" && isSafeOpenLibraryCoverReference(value.fallback_cover_reference))) &&
    (value.source_url === undefined || typeof value.source_url === "string") &&
    (value.language === undefined || typeof value.language === "string") &&
    (value.status === undefined || typeof value.status === "string") &&
    (value.genres === undefined || (Array.isArray(value.genres) && value.genres.every((genre) => typeof genre === "string"))) &&
    (value.network === undefined || typeof value.network === "string") &&
    (value.web_channel === undefined || typeof value.web_channel === "string") &&
    (value.representative_edition === undefined || isRepresentativeEdition(value.representative_edition)) &&
    Array.isArray(value.platform_candidates) && value.platform_candidates.every(isPlatformCandidate);
}

function sanitizeWorkDetails(value: WorkDetails): WorkDetails {
  const candidate = value as WorkDetails & Record<string, unknown>;
  const series = Array.isArray(candidate.series) && candidate.series.length <= 20 &&
    candidate.series.every(isWorkSeries)
    ? candidate.series
    : undefined;
  const seasonCount = Number.isInteger(candidate.season_count) &&
    typeof candidate.season_count === "number" && candidate.season_count >= 0 && candidate.season_count <= 1000
    ? candidate.season_count
    : undefined;
  return {
    ...value,
    series,
    series_truncated: candidate.series_truncated === true,
    season_count: seasonCount,
  };
}

function isWorkSeries(value: unknown): value is WorkSeries {
  if (!isRecord(value) || !boundedText(value.collection_id, 128) || !boundedText(value.title, 512) ||
    !isNullableOrdinal(value.ordinal) || !isNullableCount(value.known_total) ||
    !Array.isArray(value.more_in_series) || value.more_in_series.length > 20 ||
    typeof value.more_in_series_truncated !== "boolean") return false;
  return value.more_in_series.every((peer) => isRecord(peer) &&
    boundedText(peer.provider, 128) && boundedText(peer.external_id, 512) && boundedText(peer.title, 512) &&
    isNullableOrdinal(peer.ordinal));
}

function isNullableOrdinal(value: unknown): value is string | null {
  return value === null || boundedText(value, 64);
}

function isNullableCount(value: unknown): value is number | null {
  return value === null || (Number.isInteger(value) && typeof value === "number" && value > 0 && value <= 1000);
}

function boundedText(value: unknown, maxLength: number): value is string {
  return typeof value === "string" && value.trim() !== "" && value.length <= maxLength;
}

function isSafeOpenLibraryCoverReference(value: unknown): value is string {
  return typeof value === "string" && /^https:\/\/covers\.openlibrary\.org\/b\/id\/[1-9]\d*-L\.jpg$/.test(value);
}

function isRepresentativeEdition(value: unknown): value is RepresentativeEdition {
  if (!isRecord(value)) return false;
  return ["external_id", "title", "language", "publication_date", "cover_reference"]
    .every((key) => value[key] === undefined || typeof value[key] === "string");
}

function isWorkDetailsForResult(value: unknown, result: SearchResult): value is WorkDetails {
  if (!isWorkDetails(value)) return false;
  if (!isBookMedia(result) && !isTVMedia(result)) return true;
  return value.provider === result.provider && value.external_id === result.external_id;
}

function isPlatformCandidate(value: unknown): value is PlatformCandidate {
  if (!isRecord(value) || !isPlatform(value.platform) || !Array.isArray(value.provenance)) return false;
  return value.provenance.every((source) => isRecord(source) &&
    typeof source.provider === "string" && typeof source.external_id === "string" &&
    typeof source.relation === "string");
}

function isInventoryResolution(value: unknown): value is InventoryResolutionResponse {
  if (!isRecord(value) || typeof value.owned !== "boolean") return false;
  if (value.availability !== "unavailable" && value.availability !== "archived" && value.availability !== "ready") {
    return false;
  }
  if (!value.owned) return value.availability === "unavailable";

  return typeof value.work_id === "string" && value.work_id.length > 0 &&
    typeof value.edition_id === "string" && value.edition_id.length > 0 &&
    Array.isArray(value.asset_ids) && value.asset_ids.length > 0 &&
    value.asset_ids.every((assetId) => typeof assetId === "string" && assetId.length > 0);
}

function isUniverseRelationshipResolution(value: unknown): value is { universe_id: string } {
  return isRecord(value) && boundedText(value.universe_id, 128);
}

function isInventoryNotConfigured(value: unknown): boolean {
  return typeof value === "object" && value !== null &&
    "code" in value && value.code === "inventory_not_configured";
}

function isOpaqueOperationID(value: unknown): value is string {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/.test(value);
}

function parsePlayOperation(value: unknown, expectedOperationID: string): PlayOperation | null {
  if (!isRecord(value) || value.operation_id !== expectedOperationID ||
    typeof value.status !== "string" ||
    !["queued", "running", "waiting_on_agent", "succeeded", "failed", "interrupted", "cancelled", "unknown"].includes(value.status)) {
    return null;
  }

  const phase = value.phase;
  if (phase !== undefined && phase !== "restore" && phase !== "launch" && phase !== "playing") return null;

  let restoreProgress: PlayOperation["restoreProgress"];
  if (value.restore_progress !== undefined) {
    if (!isRecord(value.restore_progress) || !Number.isSafeInteger(value.restore_progress.current_bytes) ||
      !Number.isSafeInteger(value.restore_progress.total_bytes) ||
      Number(value.restore_progress.current_bytes) < 0 || Number(value.restore_progress.total_bytes) <= 0 ||
      Number(value.restore_progress.current_bytes) > Number(value.restore_progress.total_bytes)) return null;
    restoreProgress = {
      currentBytes: Number(value.restore_progress.current_bytes),
      totalBytes: Number(value.restore_progress.total_bytes),
    };
  }

  let sessionID: string | undefined;
  if (value.session_id !== undefined) {
    if (!isOpaqueOperationID(value.session_id)) return null;
    sessionID = value.session_id;
  }

  let errorCategory: string | undefined;
  if (value.error !== undefined) {
    if (!isRecord(value.error) || typeof value.error.category !== "string" || typeof value.error.message !== "string") return null;
    errorCategory = value.error.category;
  }

  return {
    operationID: value.operation_id,
    status: value.status as PlayOperation["status"],
    ...(phase === undefined ? {} : { phase }),
    ...(restoreProgress ? { restoreProgress } : {}),
    ...(sessionID ? { sessionID } : {}),
    ...(errorCategory ? { errorCategory } : {}),
  };
}

const safePlayErrorMessages: Record<string, string> = {
  inventory_unavailable: "No exact owned game is available for this Edition.",
  inventory_resolution_failed: "Inventory resolution failed. Try again shortly.",
  inventory_conflict: "More than one owned match prevents safe playback.",
  launch_source_unavailable: "No safe launch source is available.",
  restore_failed: "The game could not be restored safely.",
  local_cache_unavailable: "The local game cache could not be updated safely.",
  local_cache_invalid: "The local game copy did not pass validation.",
  game_exit: "The game did not exit normally.",
  session_unconfirmed: "PLAY could not confirm the game Session safely.",
  operation_interrupted: "PLAY ended before normal completion was confirmed.",
  operation_cancelled: "PLAY was cancelled before completion.",
  operation_failed: "PLAY could not complete safely.",
};

function safePlayErrorMessage(category: string | undefined): string {
  return safePlayErrorMessages[category ?? ""] ?? safePlayErrorMessages.operation_failed!;
}

function inventoryEditionForPlatform(platform: Platform | string): { platform: string; format: string } | null {
  const identities = typeof platform === "string" ? [platform] : [platform.name, platform.slug ?? ""];
  const normalizedIdentities = identities.map((identity) => identity.trim().toLowerCase().replace(/[_-]+/g, " "));
  if (normalizedIdentities.some((identity) => identity === "nintendo gamecube" || identity === "gamecube")) {
    return { platform: "gamecube", format: "disc_image" };
  }

  return null;
}

function availabilityLabel(availability: InventoryAvailability): string {
  return availability[0].toUpperCase() + availability.slice(1);
}

function releaseLabel(result: SearchResult): string | undefined {
  if (result.release_year) return String(result.release_year);
  if (!result.release_date) return undefined;

  const date = new Date(result.release_date);
  return Number.isNaN(date.getTime()) ? undefined : String(date.getUTCFullYear());
}

function platformIdentity(platform: Platform): string {
  return platform.slug ?? platform.name;
}

function displayLabel(value: string): string {
  return value
    .split(/[_-]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
}

function mediaTypeLabel(result: Pick<SearchResult, "medium" | "work_type" | "media_type">): string {
  const type = (result.media_type ?? "").toLowerCase();
  const workType = result.work_type.toLowerCase();
  const typeLabels: Record<string, string> = {
    game: "Game",
    book: "Book",
    tv: "TV",
    film: "Film",
    series: "Series",
    anime: "Anime",
    manga: "Manga",
    music: "Music",
    album: "Album",
  };
  if (type) {
    const label = typeLabels[type] ?? displayLabel(type);
    const detail = type === "tv" && workType && workType !== "tv" ? displayLabel(workType) : "";
    return detail ? `${label} · ${detail}` : label;
  }

  const medium = result.medium.toLowerCase();
  if (medium === "literature" && workType === "book") return "Book";
  if (medium === "video" && workType === "series") return "TV · Series";
  const mediumLabel = displayLabel(result.medium);
  const workTypeLabel = displayLabel(result.work_type);
  return mediumLabel.toLowerCase() === workTypeLabel.toLowerCase()
    ? mediumLabel
    : `${mediumLabel} · ${workTypeLabel}`;
}

function seriesHeadingID(collectionID: string): string {
  return `series-more-title-${encodeURIComponent(collectionID)}`;
}

type SearchFilter = "all" | "games" | "books" | "tv";

const searchFilters: Array<{ value: SearchFilter; label: string }> = [
  { value: "all", label: "All" },
  { value: "games", label: "Games" },
  { value: "books", label: "Books" },
  { value: "tv", label: "TV" },
];

function matchesSearchFilter(result: SearchResult, filter: SearchFilter): boolean {
  if (filter === "all") return true;
  const mediaType = (result.media_type ?? "").toLowerCase();
  if (filter === "games") return mediaType === "game" || (!mediaType && result.medium.toLowerCase() === "game");
  if (filter === "books") return mediaType === "book" || (!mediaType && result.medium.toLowerCase() === "literature");
  return mediaType === "tv" || (!mediaType && result.medium.toLowerCase() === "video" && result.work_type.toLowerCase() === "series");
}

type MediaIdentity = Pick<SearchResult, "provider" | "external_id" | "source_url" | "media_type" | "medium" | "work_type">;

function normalizedMediaType(result: MediaIdentity): string {
  const mediaType = (result.media_type ?? "").toLowerCase();
  if (mediaType) return mediaType;
  const medium = result.medium.toLowerCase();
  const workType = result.work_type.toLowerCase();
  if (medium === "literature" && workType === "book") return "book";
  if (medium === "video" && workType === "series") return "tv";
  return "";
}

function isBookMedia(result: Pick<SearchResult, "medium" | "work_type" | "media_type">): boolean {
  return normalizedMediaType({ ...result, provider: "", external_id: "", source_url: undefined }) === "book";
}

function isTVMedia(result: Pick<SearchResult, "medium" | "work_type" | "media_type">): boolean {
  return normalizedMediaType({ ...result, provider: "", external_id: "", source_url: undefined }) === "tv";
}

function mediaSource(result: MediaIdentity): { label: string; href: string } | null {
  const source = result.source_url;
  if (!source) return null;

  let url: URL;
  try {
    url = new URL(source);
  } catch {
    return null;
  }
  if (url.protocol !== "https:" || url.username !== "" || url.password !== "" || url.port !== "" || url.search !== "" || url.hash !== "") {
    return null;
  }

  const mediaType = normalizedMediaType(result);
  if (mediaType === "book" && result.provider.toLowerCase() === "openlibrary") {
    const externalID = result.external_id.replace(/^\/works\//, "");
    if (url.origin === "https://openlibrary.org" && externalID !== "" && url.pathname === `/works/${externalID}` && /^OL\d+W$/.test(externalID)) {
      return { label: "Open Library", href: url.href };
    }
  }
  if (mediaType === "tv" && result.provider.toLowerCase() === "tvmaze") {
    const showID = result.external_id;
    if (url.origin === "https://www.tvmaze.com" && /^\d+$/.test(showID) && new RegExp(`^/shows/${showID}(?:/[A-Za-z0-9-]+)?$`).test(url.pathname)) {
      return { label: "TVMaze", href: url.href };
    }
  }
  return null;
}

function providerLabel(provider: string): string {
  switch (provider.toLowerCase()) {
    case "igdb": return "IGDB";
    case "openlibrary": return "Open Library";
    case "tvmaze": return "TVMaze";
    default: return displayLabel(provider);
  }
}

function sourceAvailabilityMessage(source: SearchSourceStatus): string {
  const name = providerLabel(source.provider);
  switch (source.state) {
    case "stale": return `${name} is showing cached results${source.result_count > 0 ? ` (${source.result_count})` : ""}.`;
    case "throttled": return `${name} is temporarily rate-limited.`;
    case "unavailable": return `${name} is temporarily unavailable.`;
    default: return "";
  }
}

function mediaActionLabel(medium: string): "PLAY" | "WATCH" | "READ" | "LISTEN" | "Unavailable" {
  switch (medium.toLowerCase()) {
    case "game":
      return "PLAY";
    case "film":
    case "series":
    case "anime":
      return "WATCH";
    case "book":
    case "manga":
      return "READ";
    case "music":
      return "LISTEN";
    default:
      return "Unavailable";
  }
}

function isSystemStatus(value: unknown): value is SystemStatus {
  if (typeof value !== "object" || value === null) return false;
  const candidate = value as Record<string, unknown>;
  return ["server", "database", "agent"].every((key) => {
    const component = candidate[key];
    return (
      typeof component === "object" &&
      component !== null &&
      typeof (component as Record<string, unknown>).status === "string"
    );
  });
}

function statusLabel(component: ComponentStatus | undefined, expected: string, unavailable: string) {
  if (!component) return "Unknown";
  return component.status === expected ? expected[0].toUpperCase() + expected.slice(1) : unavailable;
}

function StatusRow({ label, status }: { label: string; status: string }) {
  const online = status === "Online" || status === "Ready" || status === "Connected";
  return (
    <li className="status-row">
      <span>{label}</span>
      <span className={`status-value ${online ? "is-online" : "is-offline"}`}>
        <span className="status-dot" aria-hidden="true" />
        {status}
      </span>
    </li>
  );
}

function MediaArtwork({ title, reference, fit = "cover" }: { title: string; reference?: string; fit?: "cover" | "contain" }) {
  const [failedReference, setFailedReference] = useState<string | null>(null);
  const imageFailed = reference !== undefined && failedReference === reference;

  return (
    <div className={`media-artwork${fit === "contain" ? " media-artwork-contain" : ""}`} aria-hidden="true">
      {reference && !imageFailed ? (
        <img alt="" onError={() => setFailedReference(reference)} src={reference} />
      ) : (
        <span>{title.slice(0, 1).toUpperCase()}</span>
      )}
    </div>
  );
}

function MediaCard({ result, onOpen }: { result: SearchResult; onOpen: (result: SearchResult) => void }) {
  const isBook = isBookMedia(result);
  const isTV = isTVMedia(result);
  const creatorLine = (isBook ? result.creators?.slice(0, 2).join(", ") : result.subtitle?.trim()) ||
    result.creators?.slice(0, 2).join(", ");
  const channels = [
    result.network ? `Network: ${result.network}` : "",
    result.web_channel ? `Web channel: ${result.web_channel}` : "",
  ].filter(Boolean).join(" · ");
  const artwork = result.artwork_url || result.cover_reference;
  const cardContent = (
    <>
      <MediaArtwork fit={isBook ? "contain" : "cover"} reference={artwork} title={result.title} />
      <span className="media-card-copy">
        <span className="media-card-title">{result.title}</span>
        <span className="media-type-badge">{mediaTypeLabel(result)}</span>
        {creatorLine && <span className="media-card-meta">{creatorLine}</span>}
        {(result.year || releaseLabel(result)) && <span className="media-card-year">{result.year || releaseLabel(result)}</span>}
        {isTV && channels && <span className="media-card-channels">{channels}</span>}
        {result.platforms && result.platforms.length > 0 && (
          <span className="media-card-platforms">{result.platforms.map((platform) => platform.name).join(" · ")}</span>
        )}
      </span>
    </>
  );

  return (
    <li className="media-card-item">
      <button
        data-result-key={`${result.provider}:${result.external_id}`}
        data-search-focus-key={`result:${result.provider}:${result.external_id}`}
        aria-label={`View details for ${result.title}`}
        className="media-card"
        onClick={() => onOpen(result)}
        type="button"
      >
        {cardContent}
      </button>
    </li>
  );
}

function HomeDiscovery({
  items,
  onSearch,
  onOpen,
}: {
  items: SearchResult[];
  onSearch: () => void;
  onOpen: (result: SearchResult) => void;
}) {
  return (
    <section className="home-discovery" aria-labelledby="home-title">
      <div className="discovery-empty">
        <p className="eyebrow">One universe. Every story.</p>
        <h2 id="home-title">Discover something you love.</h2>
        <p>Find your next game, film, series, book, manga, or album.</p>
        <button className="primary-button" onClick={onSearch} type="button">Explore titles</button>
      </div>
      {items.length > 0 && (
        <>
          <div className="home-section-heading">
            <div>
              <p className="eyebrow">Your session</p>
              <h2>Recently discovered</h2>
            </div>
            <button className="quiet-button" onClick={onSearch} type="button">Search more titles</button>
          </div>
          <ul className="media-grid" aria-label="Recently discovered titles">
            {items.map((result) => (
              <MediaCard key={`${result.provider}:${result.external_id}`} onOpen={onOpen} result={result} />
            ))}
          </ul>
        </>
      )}
    </section>
  );
}

function SettingsView({
  onSystemStatus,
  onMetadataLanguageChange,
}: {
  onSystemStatus: () => void;
  onMetadataLanguageChange: (language: MetadataLanguage) => void;
}) {
  const [metadataLanguage, setMetadataLanguage] = useState<MetadataLanguage | null>(null);
  const [languageUnavailable, setLanguageUnavailable] = useState(false);
  const [languageSaving, setLanguageSaving] = useState(false);
  const [languageMessage, setLanguageMessage] = useState("");

  useEffect(() => {
    const controller = new AbortController();
    let active = true;
    void (async () => {
      try {
        const response = await fetch(`${apiBaseUrl()}/api/v1/settings/metadata-language`, {
          cache: "no-store",
          signal: controller.signal,
        });
        if (!response.ok) throw new Error("metadata language unavailable");
        const language = parseMetadataLanguageResponse(await response.json());
        if (!language) throw new Error("invalid metadata language response");
        if (active) {
          setMetadataLanguage(language);
          onMetadataLanguageChange(language);
          setLanguageUnavailable(false);
        }
      } catch {
        if (active && !controller.signal.aborted) setLanguageUnavailable(true);
      }
    })();
    return () => {
      active = false;
      controller.abort();
    };
  }, [onMetadataLanguageChange]);

  async function updateMetadataLanguage(language: MetadataLanguage) {
    setLanguageSaving(true);
    setLanguageMessage("");
    try {
      const response = await fetch(`${apiBaseUrl()}/api/v1/settings/metadata-language`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ metadata_language: language }),
      });
      if (!response.ok) throw new Error("metadata language could not be saved");
      const savedLanguage = parseMetadataLanguageResponse(await response.json());
      if (!savedLanguage) throw new Error("invalid metadata language response");
      setMetadataLanguage(savedLanguage);
      onMetadataLanguageChange(savedLanguage);
      setLanguageMessage("Metadata language saved.");
    } catch {
      setLanguageMessage("Metadata language could not be saved. Try again.");
    } finally {
      setLanguageSaving(false);
    }
  }

  return (
    <section className="settings-panel" aria-labelledby="settings-title">
      <p className="eyebrow">Your space</p>
      <h2 id="settings-title">Settings</h2>
      <div className="settings-group settings-language-preference">
        <div className="settings-status-copy">
          <label className="settings-group-title" htmlFor="metadata-language">Metadata language</label>
          <p className="settings-group-description" id="metadata-language-description">
            English and Spanish prefer a matching Open Library edition when available. Original uses Open Library’s default edition order.
            This affects metadata only, not the language you consume in.
          </p>
        </div>
        <div className="settings-language-control">
          <select
            aria-describedby="metadata-language-description"
            disabled={metadataLanguage === null || languageSaving || languageUnavailable}
            id="metadata-language"
            onChange={(event) => {
              const language = event.currentTarget.value;
              if (isMetadataLanguage(language)) void updateMetadataLanguage(language);
            }}
            value={metadataLanguage ?? "en"}
          >
            <option value="en">English</option>
            <option value="es">Español</option>
            <option value="original">Original (provider default)</option>
          </select>
          {metadataLanguage === null && !languageUnavailable && <span role="status">Loading metadata language…</span>}
          {languageUnavailable && <span className="settings-language-error" role="alert">Metadata language settings are unavailable.</span>}
          {languageMessage && <span role={languageMessage.includes("could not") ? "alert" : "status"}>{languageMessage}</span>}
          {languageSaving && <span role="status">Saving metadata language…</span>}
        </div>
      </div>
      <button
        aria-describedby="system-status-row-description"
        aria-labelledby="system-status-row-title"
        className="settings-group settings-status-row"
        onClick={onSystemStatus}
        type="button"
      >
        <span className="settings-status-copy">
          <span className="settings-group-title" id="system-status-row-title">System status</span>
          <span className="settings-group-description" id="system-status-row-description">
            Check the status of your Lernae services.
          </span>
        </span>
        <span aria-hidden="true" className="settings-status-chevron">›</span>
      </button>
    </section>
  );
}

function CatalogSearchForm({
  compact = false,
  inputId = "catalog-query",
  inputLabel = "Search the catalog",
  inputRef,
  onChange,
  onSubmit,
  query,
}: {
  compact?: boolean;
  inputId?: string;
  inputLabel?: string;
  inputRef?: RefObject<HTMLInputElement | null>;
  onChange: (value: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  query: string;
}) {
  return (
    <form className={compact ? "search-form header-search-form" : "search-form"} onSubmit={onSubmit}>
      <label htmlFor={inputId}>{inputLabel}</label>
      <div className="search-controls">
        <input
          autoComplete="off"
          id={inputId}
          name="q"
          onChange={(event) => onChange(event.currentTarget.value)}
          placeholder={compact ? "Search titles…" : "Titles, creators, and more…"}
          ref={inputRef}
          type="search"
          value={query}
        />
        <button
          aria-label={compact ? "Submit quick search" : undefined}
          className="primary-button"
          type="submit"
        >{compact ? "Search" : "Search titles"}</button>
      </div>
    </form>
  );
}

function SearchSourceStatusView({ sources }: { sources: SearchSourceStatus[] }) {
  const degradedSources = sources.filter((source) =>
    source.state === "stale" || source.state === "throttled" || source.state === "unavailable",
  );
  if (degradedSources.length === 0) return null;

  const hasVisibleResults = sources.some((source) => source.result_count > 0);
  const details = degradedSources.map(sourceAvailabilityMessage).filter(Boolean).join(" ");

  return (
    <p aria-label="Search source status" className="search-source-status" role="status">
      {details} {hasVisibleResults ? "Available results remain visible." : "Try again shortly."}
    </p>
  );
}

const searchDataSources = [
  { provider: "igdb", label: "IGDB", href: "https://www.igdb.com/" },
  { provider: "openlibrary", label: "Open Library", href: "https://openlibrary.org/" },
  { provider: "tvmaze", label: "TVMaze", href: "https://www.tvmaze.com/" },
] as const;

function SearchDataSources({ results }: { results: SearchResult[] }) {
  const visibleProviders = new Set(results.map((result) => result.provider.trim().toLowerCase()));
  const sources = searchDataSources.filter((source) => visibleProviders.has(source.provider));
  if (sources.length === 0) return null;

  return (
    <aside aria-label="Data sources" className="search-data-sources">
      <span className="search-data-sources-label">Data sources:</span>
      <span className="search-data-source-links">
        {sources.map((source, index) => (
          <span className="search-data-source-item" key={source.provider}>
            {index > 0 && <span aria-hidden="true"> · </span>}
            <a href={source.href} rel="noopener noreferrer" target="_blank">{source.label}</a>
          </span>
        ))}
      </span>
    </aside>
  );
}

function SearchView({
  activeFilter,
  inputRef,
  universeDiscovery,
  exactUniverseAlias,
  resolverState,
  selectedUniverseWorkIDs,
  resolverBusyIdentity,
  resolverStatusMessage,
  onFilterChange,
  onChange,
  onGroupRelated,
  onOpen,
  onOpenUniverse,
  onUniverseSelectionChange,
  onConfirmUniverseCandidate,
  onExcludeUniverseProposal,
  onResetUniverseExclusion,
  onRetry,
  onSubmit,
  query,
  searchState,
}: {
  activeFilter: SearchFilter;
  inputRef: RefObject<HTMLInputElement | null>;
  universeDiscovery?: UniverseDiscovery;
  exactUniverseAlias: ExactUniverseAliasState;
  resolverState: UniverseResolverState;
  selectedUniverseWorkIDs: Set<string>;
  resolverBusyIdentity: string | null;
  resolverStatusMessage: string;
  onFilterChange: (filter: SearchFilter) => void;
  onChange: (value: string) => void;
  onGroupRelated: () => void;
  onOpen: (result: SearchResult) => void;
  onOpenUniverse: (universe: UniverseSummary) => void;
  onUniverseSelectionChange: (identity: string, checked: boolean) => void;
  onConfirmUniverseCandidate: (candidate: UniverseCandidate, proposals: UniverseProposal[]) => void;
  onExcludeUniverseProposal: (universeID: string, proposal: UniverseProposal) => void;
  onResetUniverseExclusion: (universeID: string, proposal: UniverseProposal) => void;
  onRetry: (query: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  query: string;
  searchState: SearchState;
}) {
  const activeFilterLabel = searchFilters.find((filter) => filter.value === activeFilter)?.label ?? "All";
  const visibleResults = searchState.kind === "results"
    ? searchState.results.filter((result) => matchesSearchFilter(result, activeFilter))
    : [];
  const relevanceGroups = groupSearchResults(visibleResults);
  const hasAmbiguousGrouping = universeDiscovery?.state === "ambiguous";
  const allSourcesUnavailable = searchState.kind === "empty" && searchState.sources.length > 0 &&
    searchState.sources.every((source) => source.state === "unavailable" || source.state === "throttled");
  const exactAliasAlreadySurfaced = exactUniverseAlias.kind === "matched" &&
    universeDiscovery?.universe_id === exactUniverseAlias.universe.id;

  return (
    <section className="search-panel" aria-labelledby="search-title">
      <div className="section-heading">
        <p className="eyebrow">Find your next favorite</p>
        <h2 id="search-title">Search</h2>
        <p>Explore titles from across your entertainment universe.</p>
      </div>
      <CatalogSearchForm inputRef={inputRef} onChange={onChange} onSubmit={onSubmit} query={query} />

      <div aria-label="Filter search results by media type" className="search-filters" role="group">
        {searchFilters.map((filter) => (
          <button
            aria-pressed={activeFilter === filter.value}
            className="search-filter"
            key={filter.value}
            onClick={() => onFilterChange(filter.value)}
            type="button"
          >{filter.label}</button>
        ))}
      </div>

      {searchState.kind === "loading" && (
        <p className="search-message" role="status">Searching for “{searchState.query}”…</p>
      )}
      {searchState.kind === "empty" && (
        <>
          <p className="search-message" role="status">
            {allSourcesUnavailable
              ? "Search sources are temporarily unavailable. Try again shortly."
              : `No ${activeFilter === "all" ? "titles" : `${activeFilterLabel.toLowerCase()} titles`} found. Try another search.`}
          </p>
          <SearchSourceStatusView sources={searchState.sources} />
          <UniverseMatches discovery={universeDiscovery} onOpen={onOpenUniverse} />
          {exactUniverseAlias.kind === "matched" && !exactAliasAlreadySurfaced && (
            <UniverseExactNameMatch
              onOpen={onOpenUniverse}
              query={exactUniverseAlias.query}
              universe={exactUniverseAlias.universe}
            />
          )}
        </>
      )}
      {searchState.kind === "error" && (
        <div className="search-error" role="alert">
          <p>{searchState.message}</p>
          <button type="button" onClick={() => onRetry(searchState.query)}>Try again</button>
        </div>
      )}
      {searchState.kind === "results" && (
        <>
          <SearchDataSources results={visibleResults} />
          <SearchSourceStatusView sources={searchState.sources} />
          <UniverseMatches discovery={universeDiscovery} onOpen={onOpenUniverse} />
          {exactUniverseAlias.kind === "loading" && (
            <p className="search-message" role="status">Checking for an exact Universe name…</p>
          )}
          {exactUniverseAlias.kind === "matched" && !exactAliasAlreadySurfaced && (
            <UniverseExactNameMatch
              onOpen={onOpenUniverse}
              query={exactUniverseAlias.query}
              universe={exactUniverseAlias.universe}
            />
          )}
          {exactUniverseAlias.kind === "error" && (
            <p className="search-message" role="status">Exact Universe lookup is unavailable. Search results are unchanged.</p>
          )}
          {hasAmbiguousGrouping && visibleResults.length > 0 && (
            <section aria-labelledby="possible-collection-title" className="possible-collection">
              <div>
                <p className="eyebrow">Possible collection</p>
                <h3 id="possible-collection-title">These results may belong together</h3>
                <p>Review the suggested grouping before confirming any Works.</p>
              </div>
              <button
                className="quiet-button group-results-button"
                data-search-focus-key="group-related-results"
                disabled={resolverState.kind === "loading"}
                onClick={onGroupRelated}
                type="button"
              >{resolverState.kind === "loading" ? "Reviewing grouping…" : "Review grouping"}</button>
            </section>
          )}
          <div className="results-heading">
            <p>
              {visibleResults.length} {visibleResults.length === 1 ? "title" : "titles"}
              {activeFilter === "all" ? "" : ` in ${activeFilterLabel}`} for “{searchState.query}”
            </p>
            {visibleResults.length > 0 && !hasAmbiguousGrouping && (
              <button
                className="quiet-button group-results-button"
                data-search-focus-key="group-related-results"
                disabled={resolverState.kind === "loading"}
                onClick={onGroupRelated}
                type="button"
              >{resolverState.kind === "loading" ? "Reviewing grouping…" : "Review grouping"}</button>
            )}
          </div>
          {searchState.results.length > 20 && (
            <p className="resolver-evidence">Grouping review is limited to the first 20 Search results.</p>
          )}
          {visibleResults.length > 0 ? (
            <div className="search-result-groups">
              <SearchResultList label="Best matches" results={relevanceGroups.best} onOpen={onOpen} />
              <SearchResultList label="Related results" results={relevanceGroups.related} onOpen={onOpen} />
              {relevanceGroups.weak.length > 0 && (
                <details className="weak-results">
                  <summary>
                    <span>Show more related results</span>
                    <span aria-hidden="true" className="weak-result-count">({relevanceGroups.weak.length})</span>
                  </summary>
                  <p>These titles have weaker search evidence, but remain available to explore.</p>
                  <SearchResultList label="More related results" results={relevanceGroups.weak} onOpen={onOpen} />
                </details>
              )}
            </div>
          ) : (
            <p className="search-message" role="status">
              No {activeFilter === "all" ? "titles" : activeFilterLabel.toLowerCase()} found for “{searchState.query}”.
            </p>
          )}
          <UniverseResolverReview
            busyIdentity={resolverBusyIdentity}
            onConfirm={onConfirmUniverseCandidate}
            onExclude={onExcludeUniverseProposal}
            onOpenUniverse={onOpenUniverse}
            onResetExclusion={onResetUniverseExclusion}
            onSelect={onUniverseSelectionChange}
            selected={selectedUniverseWorkIDs}
            state={resolverState}
            statusMessage={resolverStatusMessage}
          />
        </>
      )}
    </section>
  );
}

function SystemStatusView() {
  const [loadState, setLoadState] = useState<SystemLoadState>({ kind: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    let active = true;
    let requestIdentity = 0;

    const refreshStatus = async () => {
      const identity = ++requestIdentity;
      try {
        const response = await fetch(`${apiBaseUrl()}/api/v1/system/status`, {
          cache: "no-store",
          signal: controller.signal,
        });
        if (!response.ok) throw new Error("status request failed");
        const payload: unknown = await response.json();
        if (!isSystemStatus(payload)) throw new Error("invalid status response");
        if (active && identity === requestIdentity) setLoadState({ kind: "online", status: payload });
      } catch {
        if (active && identity === requestIdentity && !controller.signal.aborted) {
          setLoadState({ kind: "unreachable" });
        }
      }
    };

    void refreshStatus();
    const interval = window.setInterval(() => void refreshStatus(), 5000);
    return () => {
      active = false;
      window.clearInterval(interval);
      controller.abort();
    };
  }, []);

  const status = loadState.kind === "online" ? loadState.status : undefined;

  return (
    <section className="system-panel" aria-labelledby="system-status-title" aria-live="polite">
      <p className="eyebrow">Settings · Runtime</p>
      <h2 id="system-status-title">System status</h2>
      {loadState.kind === "loading" && <p className="notice" role="status">Checking local runtime…</p>}
      {loadState.kind === "unreachable" && (
        <p className="notice error" role="status">
          Server unreachable. Start the Lernae Server to check runtime status.
        </p>
      )}
      <ul className="status-list">
        <StatusRow
          label="Server"
          status={
            loadState.kind === "unreachable"
              ? "Unreachable"
              : loadState.kind === "loading"
                ? "Checking"
                : statusLabel(status?.server, "online", "Offline")
          }
        />
        <StatusRow
          label="Database"
          status={
            loadState.kind === "online"
              ? statusLabel(status?.database, "ready", "Unavailable")
              : "Unknown"
          }
        />
        <StatusRow
          label="Agent"
          status={
            loadState.kind === "online"
              ? statusLabel(status?.agent, "connected", "Offline")
              : "Unknown"
          }
        />
      </ul>
      <p className="refresh-note">Status refreshes automatically every 5 seconds.</p>
    </section>
  );
}

function groupSearchResults(results: SearchResult[]): Record<SearchRelevance, SearchResult[]> {
  const groups: Record<SearchRelevance, SearchResult[]> = { best: [], related: [], weak: [] };
  for (const result of results) {
    // Older local fixtures/servers do not send relevance yet; keep those visible in the middle band.
    groups[result.relevance ?? "related"].push(result);
  }
  return groups;
}

function SearchResultList({
  label,
  results,
  onOpen,
}: {
  label: string;
  results: SearchResult[];
  onOpen: (result: SearchResult) => void;
}) {
  if (results.length === 0) return null;
  return (
    <section aria-labelledby={`search-band-${label.toLowerCase().replaceAll(" ", "-")}`} className="search-result-band">
      <h3 id={`search-band-${label.toLowerCase().replaceAll(" ", "-")}`}>{label}</h3>
      <ul className="media-grid" aria-label={label}>
        {results.map((result) => (
          <MediaCard key={`${result.provider}:${result.external_id}`} onOpen={onOpen} result={result} />
        ))}
      </ul>
    </section>
  );
}

function MediaDetails({ result, onOpenSeriesMember }: {
  result: SearchResult;
  onOpenSeriesMember: (result: SearchResult) => void;
}) {
  const [failedCoverReferences, setFailedCoverReferences] = useState<string[]>([]);
  const [descriptionExpanded, setDescriptionExpanded] = useState(false);
  const [selectedPlatform, setSelectedPlatform] = useState<string | null>(null);
  const [inventoryState, setInventoryState] = useState<InventoryResolutionState>({ kind: "idle" });
  const [detailsState, setDetailsState] = useState<WorkDetailsState>({ kind: "loading" });
  const [playState, setPlayState] = useState<PlayState>({ kind: "idle" });
  const [detailsRetry, setDetailsRetry] = useState(0);
  const [inventoryRetry, setInventoryRetry] = useState(0);
  const inventoryRequestIdentity = useRef(0);
  const activeInventoryRequest = useRef<AbortController | null>(null);
  const playSubmissionPending = useRef(false);
  const platforms = detailsState.kind === "ready"
    ? detailsState.details.platform_candidates.map((candidate) => candidate.platform)
    : [];
  const selectedPlatformIsAvailable = platforms.some(
    (platform) => platformIdentity(platform) === selectedPlatform,
  );
  const resolvedInventory = inventoryState.kind === "resolved" ? inventoryState.resolution : null;
  const readyDetails = detailsState.kind === "ready" ? detailsState.details : null;
  const isBookDetails = readyDetails !== null && isBookMedia(readyDetails);
  const isTVDetails = readyDetails !== null && isTVMedia(readyDetails);
  const coverCandidates = [...new Set([
    readyDetails?.cover_reference,
    isBookDetails ? readyDetails?.fallback_cover_reference : undefined,
  ].filter((reference): reference is string => typeof reference === "string" && reference !== ""))];
  const visibleCoverReference = coverCandidates.find((reference) => !failedCoverReferences.includes(reference));
  const hidesUnavailableAction = isBookDetails || isTVDetails;
  const bookAuthors = result.creators?.join(", ") || result.subtitle?.trim();
  const representativeEdition = readyDetails?.representative_edition;
  const yearFromEdition = representativeEdition?.publication_date?.match(/\b(?:1[5-9]\d{2}|20\d{2}|21\d{2})\b/)?.[0];
  const publicationYear = readyDetails
    ? readyDetails.release_year || releaseLabel(readyDetails) || result.year || yearFromEdition
    : undefined;
  const detailsSource = readyDetails
    ? mediaSource({ ...result, provider: readyDetails.provider, external_id: readyDetails.external_id, source_url: readyDetails.source_url })
    : null;
  const canPlay = Boolean(detailsState.kind === "ready" && detailsState.details.medium.toLowerCase() === "game" &&
    selectedPlatformIsAvailable && resolvedInventory?.owned &&
    (resolvedInventory.availability === "archived" || resolvedInventory.availability === "ready"));
  const activeOperationID = playState.kind === "active" ? playState.operationID : null;

  useEffect(() => {
    const controller = new AbortController();
    let active = true;

    const loadDetails = async () => {
      try {
        const query = new URLSearchParams({ provider: result.provider, external_id: result.external_id });
        const response = await fetch(`${apiBaseUrl()}/api/v1/work-details?${query.toString()}`, {
          cache: "no-store",
          signal: controller.signal,
        });
        if (!response.ok) throw new Error("work details unavailable");
        const payload: unknown = await response.json();
        if (!isWorkDetailsForResult(payload, result)) throw new Error("invalid work details response");
        const safeDetails = sanitizeWorkDetails(payload);
        if (active && !controller.signal.aborted) {
          const initialSupportedPlatform = safeDetails.medium.toLowerCase() === "game"
            ? safeDetails.platform_candidates
              .map((candidate) => candidate.platform)
              .find((platform) => inventoryEditionForPlatform(platform) !== null)
            : undefined;
          setSelectedPlatform(initialSupportedPlatform ? platformIdentity(initialSupportedPlatform) : null);
          if (initialSupportedPlatform) setInventoryState({ kind: "loading" });
          setDetailsState({ kind: "ready", details: safeDetails });
        }
      } catch {
        if (active && !controller.signal.aborted) setDetailsState({ kind: "error" });
      }
    };

    void loadDetails();
    return () => {
      active = false;
      controller.abort();
    };
  }, [result, detailsRetry]);

  function retryDetails() {
    setDetailsState({ kind: "loading" });
    setSelectedPlatform(null);
    setInventoryState({ kind: "idle" });
    setPlayState({ kind: "idle" });
    inventoryRequestIdentity.current += 1;
    activeInventoryRequest.current?.abort();
    activeInventoryRequest.current = null;
    setDetailsRetry((retry) => retry + 1);
  }

  useEffect(() => () => {
    inventoryRequestIdentity.current += 1;
    activeInventoryRequest.current?.abort();
  }, []);

  function selectPlatform(platform: Platform) {
    if (detailsState.kind !== "ready") return;
    if (playState.kind === "submitting" || playState.kind === "active") return;
    const identity = platformIdentity(platform);
    if (selectedPlatform === identity) {
      if (inventoryState.kind === "error") {
        setInventoryState({ kind: "loading" });
        setInventoryRetry((retry) => retry + 1);
      }
      return;
    }
    inventoryRequestIdentity.current += 1;
    activeInventoryRequest.current?.abort();
    activeInventoryRequest.current = null;
    setSelectedPlatform(identity);
    setPlayState({ kind: "idle" });
    const edition = detailsState.details.medium.toLowerCase() === "game"
      ? inventoryEditionForPlatform(platform)
      : null;
    setInventoryState(edition ? { kind: "loading" } : { kind: "unsupported" });
  }

  useEffect(() => {
    const requestIdentity = ++inventoryRequestIdentity.current;
    activeInventoryRequest.current?.abort();
    activeInventoryRequest.current = null;
    if (detailsState.kind !== "ready" || selectedPlatform === null) return;

    const workDetails = detailsState.details;
    const platform = workDetails.platform_candidates
      .map((candidate) => candidate.platform)
      .find((candidate) => platformIdentity(candidate) === selectedPlatform);
    const edition = platform && workDetails.medium.toLowerCase() === "game"
      ? inventoryEditionForPlatform(platform)
      : null;
    if (!edition) return;

    const controller = new AbortController();
    activeInventoryRequest.current = controller;

    const resolveInventory = async () => {
      try {
        const response = await fetch(`${apiBaseUrl()}/api/v1/inventory/resolve`, {
          method: "POST",
          cache: "no-store",
          headers: { "Content-Type": "application/json" },
          signal: controller.signal,
          body: JSON.stringify({
            work_identity: { provider: workDetails.provider, external_id: workDetails.external_id },
            edition,
          }),
        });
        if (!response.ok) {
          const errorPayload: unknown = await response.json().catch(() => null);
          if (isInventoryNotConfigured(errorPayload)) {
            if (requestIdentity === inventoryRequestIdentity.current && !controller.signal.aborted) {
              setInventoryState({ kind: "unconfigured" });
            }
            return;
          }
          throw new Error("inventory resolution unavailable");
        }
        const payload: unknown = await response.json();
        if (!isInventoryResolution(payload)) throw new Error("invalid inventory response");
        if (requestIdentity === inventoryRequestIdentity.current && !controller.signal.aborted) {
          setInventoryState({
            kind: "resolved",
            resolution: { owned: payload.owned, availability: payload.availability },
          });
        }
      } catch {
        if (requestIdentity === inventoryRequestIdentity.current && !controller.signal.aborted) {
          setInventoryState({ kind: "error" });
        }
      } finally {
        if (requestIdentity === inventoryRequestIdentity.current) activeInventoryRequest.current = null;
      }
    };

    void resolveInventory();
    return () => {
      controller.abort();
      if (requestIdentity === inventoryRequestIdentity.current) {
        inventoryRequestIdentity.current += 1;
      }
      if (activeInventoryRequest.current === controller) activeInventoryRequest.current = null;
    };
  }, [detailsState, selectedPlatform, inventoryRetry]);

  async function startPlay() {
    if (!canPlay || detailsState.kind !== "ready" || playSubmissionPending.current ||
      playState.kind === "submitting" || playState.kind === "active") return;
    const selectedPlatformDetails = platforms.find((platform) => platformIdentity(platform) === selectedPlatform);
    const edition = selectedPlatformDetails ? inventoryEditionForPlatform(selectedPlatformDetails) : null;
    if (!edition) return;

    playSubmissionPending.current = true;
    setPlayState({ kind: "submitting" });
    try {
      const response = await fetch(`${apiBaseUrl()}/api/v1/play`, {
        method: "POST",
        cache: "no-store",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          work_identity: {
            provider: detailsState.details.provider,
            external_id: detailsState.details.external_id,
          },
          edition,
        }),
      });
      if (response.status !== 202) throw new Error("PLAY was not accepted");
      const payload: unknown = await response.json();
      if (!isRecord(payload) || !isOpaqueOperationID(payload.operation_id)) {
        throw new Error("PLAY returned an invalid operation reference");
      }
      setPlayState({ kind: "active", operationID: payload.operation_id, operation: null, pollingError: false });
    } catch {
      setPlayState({ kind: "error", message: "PLAY could not be started safely. Try again shortly." });
    } finally {
      playSubmissionPending.current = false;
    }
  }

  useEffect(() => {
    if (!activeOperationID) return;
    let active = true;
    let requestInFlight = false;
    const controller = new AbortController();

    const pollOperation = async () => {
      if (requestInFlight) return;
      requestInFlight = true;
      try {
        const response = await fetch(`${apiBaseUrl()}/api/v1/play/${encodeURIComponent(activeOperationID)}`, {
          cache: "no-store",
          signal: controller.signal,
        });
        if (!response.ok) throw new Error("PLAY status unavailable");
        const payload: unknown = await response.json();
        const operation = parsePlayOperation(payload, activeOperationID);
        if (!operation) throw new Error("invalid PLAY status");
        if (!active || controller.signal.aborted) return;
        if (operation.status === "succeeded") {
          setPlayState({ kind: "completed" });
        } else if (["failed", "interrupted", "cancelled"].includes(operation.status)) {
          setPlayState({ kind: "error", message: safePlayErrorMessage(operation.errorCategory) });
        } else {
          setPlayState({ kind: "active", operationID: activeOperationID, operation, pollingError: false });
        }
      } catch {
        if (active && !controller.signal.aborted) {
          setPlayState((current) => current.kind === "active" && current.operationID === activeOperationID
            ? { ...current, pollingError: true }
            : current);
        }
      } finally {
        requestInFlight = false;
      }
    };

    const interval = window.setInterval(() => void pollOperation(), 1000);
    return () => {
      active = false;
      window.clearInterval(interval);
      controller.abort();
    };
  }, [activeOperationID]);

  let playStatusMessage: string | undefined;
  let playStatusRole: "status" | "alert" = "status";
  if (playState.kind === "submitting") {
    playStatusMessage = "Starting PLAY…";
  } else if (playState.kind === "active") {
    if (playState.pollingError) {
      playStatusMessage = "PLAY status is temporarily unavailable. Retrying automatically…";
    } else if (!playState.operation) {
      playStatusMessage = "PLAY accepted. Preparing your game…";
    } else if (playState.operation.phase === "restore") {
      playStatusMessage = "Restoring game…";
    } else if (playState.operation.phase === "launch") {
      playStatusMessage = "Launching game…";
    } else if (playState.operation.phase === "playing" && playState.operation.sessionID) {
      playStatusMessage = "Playing game…";
    } else {
      playStatusMessage = "Preparing your game…";
    }
  } else if (playState.kind === "completed") {
    playStatusMessage = "PLAY completed.";
  } else if (playState.kind === "error") {
    playStatusMessage = playState.message;
    playStatusRole = "alert";
  } else if (detailsState.kind === "ready" && detailsState.details.medium.toLowerCase() !== "game" && !hidesUnavailableAction) {
    const actionLabel = mediaActionLabel(detailsState.details.medium);
    playStatusMessage = actionLabel === "Unavailable"
      ? "This media type is not supported yet."
      : `${actionLabel} is not available for this title yet.`;
  } else if (inventoryState.kind === "unconfigured") {
    playStatusMessage = "";
  } else if (inventoryState.kind === "unsupported") {
    playStatusMessage = "Playback is not supported for this platform.";
  } else if (inventoryState.kind === "resolved" && !canPlay) {
    playStatusMessage = "This Edition is unavailable for playback.";
  } else if (inventoryState.kind === "resolved" && canPlay) {
    playStatusMessage = "This Edition is ready to play.";
  } else if (detailsState.kind === "ready" && platforms.length > 0) {
    playStatusMessage = "Select a platform to check availability.";
  } else if (detailsState.kind === "ready") {
    playStatusMessage = "No supported platform was returned for this title.";
  }

  const restoreProgress = playState.kind === "active" && playState.operation?.phase === "restore"
    ? playState.operation.restoreProgress
    : undefined;

  return (
    <section
      className={["detail-panel", isBookDetails ? "book-details" : "", isTVDetails ? "tv-details" : ""].filter(Boolean).join(" ")}
      aria-label={`Details for ${result.title}`}
    >
      {detailsState.kind === "loading" && <p className="notice" role="status">Loading work details…</p>}
      {detailsState.kind === "error" && (
        <div className="search-error" role="alert">
          <p>Work details are temporarily unavailable.</p>
          <button type="button" onClick={retryDetails}>Try again</button>
        </div>
      )}
      {detailsState.kind === "ready" && (
        <div className="detail-layout">
          <div className="details-backdrop" aria-hidden="true">
            {detailsState.details.cover_reference && !isBookDetails ? (
              <MediaArtwork reference={detailsState.details.cover_reference} title={detailsState.details.title} />
            ) : (
              <div className="details-backdrop-placeholder" />
            )}
          </div>
          <div className={[
            "cover-frame",
            isBookDetails ? "book-cover-frame" : "",
            visibleCoverReference ? "cover-frame-with-artwork" : "",
          ].filter(Boolean).join(" ")}>
            {visibleCoverReference ? (
              <img
                alt={`${detailsState.details.title} cover`}
                onError={() => setFailedCoverReferences((failed) => failed.includes(visibleCoverReference)
                  ? failed
                  : [...failed, visibleCoverReference])}
                src={visibleCoverReference}
              />
            ) : (
              <div className="cover-placeholder" role="img" aria-label="Cover art unavailable">
                <span>Cover art unavailable</span>
              </div>
            )}
          </div>

          <div className="work-details">
            <h2 id="details-title">{detailsState.details.title}</h2>
            {!isBookDetails && !isTVDetails && detailsState.details.release_date &&
              <p>Release date: {detailsState.details.release_date.slice(0, 10)}</p>}
            {!isBookDetails && !isTVDetails && detailsState.details.release_year &&
              <p>Release year: {detailsState.details.release_year}</p>}
            <p className="work-type"><span className="media-type-badge">{mediaTypeLabel(detailsState.details)}</span></p>
            {isBookDetails && (
              <div aria-label="Book metadata" className="book-metadata">
                {bookAuthors && <p className="book-authors">By {bookAuthors}</p>}
                {publicationYear && <p className="book-publication-year">Published {publicationYear}</p>}
                {representativeEdition && (representativeEdition.title || representativeEdition.external_id) && (
                  <p className="representative-edition">
                    Edition: {representativeEdition.title || representativeEdition.external_id}
                    {representativeEdition.language ? ` · ${representativeEdition.language.toUpperCase()}` : ""}
                  </p>
                )}
              </div>
            )}
            {isTVDetails && (
              <dl aria-label="TV metadata" className="tv-metadata">
                {publicationYear && <div><dt>Premiered</dt><dd className="tv-premiered">{publicationYear}</dd></div>}
                {detailsState.details.network && <div><dt>Network</dt><dd className="tv-network">{detailsState.details.network}</dd></div>}
                {detailsState.details.web_channel && <div><dt>Web channel</dt><dd className="tv-web-channel">{detailsState.details.web_channel}</dd></div>}
                {detailsState.details.status && <div><dt>Status</dt><dd className="tv-status">{detailsState.details.status}</dd></div>}
                {detailsState.details.genres && detailsState.details.genres.length > 0 && (
                  <div><dt>Genres</dt><dd className="tv-genres">{detailsState.details.genres.join(" · ")}</dd></div>
                )}
                {detailsState.details.language && <div><dt>Language</dt><dd>{detailsState.details.language.toUpperCase()}</dd></div>}
                {typeof detailsState.details.season_count === "number" && (
                  <div><dt>Seasons</dt><dd className="tv-season-count">{detailsState.details.season_count}</dd></div>
                )}
              </dl>
            )}
            {readyDetails?.series?.map((series) => (
              <section
                aria-labelledby={seriesHeadingID(series.collection_id)}
                className="series-details"
                data-series-id={series.collection_id}
                key={series.collection_id}
              >
                <p className="series-identity">
                  {isBookDetails && series.ordinal !== null && series.known_total !== null
                    ? `Series · Book ${series.ordinal} of ${series.known_total}`
                    : "Series"}
                  <span className="series-title">{series.title}</span>
                </p>
                {series.more_in_series.length > 0 && (
                  <div className="series-more">
                    <h3 id={seriesHeadingID(series.collection_id)}>More in this series</h3>
                    <ul className="series-member-list">
                      {series.more_in_series.map((member) => (
                        <li key={`${member.provider}:${member.external_id}`}>
                          <button
                            aria-label={`View details for ${member.title}`}
                            className="series-member"
                            data-series-member-external-id={member.external_id}
                            data-series-member-provider={member.provider}
                            onClick={() => onOpenSeriesMember({
                              provider: member.provider,
                              external_id: member.external_id,
                              title: member.title,
                              medium: result.medium,
                              work_type: result.work_type,
                              media_type: result.media_type,
                            })}
                            type="button"
                          >
                            <span>{member.title}</span>
                            {isBookDetails && member.ordinal !== null && (
                              <span className="series-member-ordinal">Book {member.ordinal}</span>
                            )}
                          </button>
                        </li>
                      ))}
                    </ul>
                    {series.more_in_series_truncated && (
                      <p className="series-truncated">More titles are available in this series.</p>
                    )}
                  </div>
                )}
              </section>
            ))}
            {detailsSource && (isBookDetails || isTVDetails) && (
              <p className="details-provider-link">
                <a href={detailsSource.href} rel="noopener noreferrer" target="_blank">{detailsSource.label} record</a>
              </p>
            )}
            {detailsState.details.summary && <p className="detail-summary">{detailsState.details.summary}</p>}

            {!hidesUnavailableAction && (
              <div className="play-panel" aria-live="polite" aria-atomic="true">
                <div className="play-feedback">
                  {playStatusMessage && <p role={playStatusRole}>{playStatusMessage}</p>}
                  {restoreProgress && (
                    <>
                      <progress
                        aria-label="Restore progress"
                        max={restoreProgress.totalBytes}
                        value={restoreProgress.currentBytes}
                      />
                      <p>Restored {restoreProgress.currentBytes} of {restoreProgress.totalBytes} bytes.</p>
                    </>
                  )}
                </div>
                <button
                  className="primary-button"
                  disabled={!canPlay || playState.kind === "submitting" || playState.kind === "active"}
                  onClick={() => void startPlay()}
                  type="button"
                >{mediaActionLabel(detailsState.details.medium)}</button>
              </div>
            )}

            {detailsState.details.summary && (
              <div className="description-disclosure">
                <button
                  aria-controls="full-description"
                  aria-expanded={descriptionExpanded}
                  onClick={() => setDescriptionExpanded((expanded) => !expanded)}
                  type="button"
                >{descriptionExpanded ? "Hide full description" : "Read full description"}</button>
                <p className="full-description" hidden={!descriptionExpanded} id="full-description">
                  {detailsState.details.summary}
                </p>
              </div>
            )}

            {!hidesUnavailableAction && <fieldset className="platform-options">
              <legend>Edition &amp; platform</legend>
              {platforms.length > 0 ? (
                <div className="platform-picker">
                  {platforms.map((platform) => {
                    const identity = platformIdentity(platform);
                    return (
                      <button
                        aria-pressed={selectedPlatform === identity}
                        className="platform-option"
                        disabled={playState.kind === "submitting" || playState.kind === "active"}
                        key={identity}
                        onClick={() => selectPlatform(platform)}
                        type="button"
                      >
                        {platform.name}
                      </button>
                    );
                  })}
                </div>
              ) : (
                <p>No platforms are listed for this title.</p>
              )}
            </fieldset>}

            {selectedPlatformIsAvailable && (
              <>
                <section className="inventory-status" aria-labelledby="inventory-title" aria-live="polite">
                  <h3 id="inventory-title">
                    {detailsState.kind === "ready" && detailsState.details.medium.toLowerCase() !== "game"
                      ? "Availability"
                      : "Inventory status"}
                  </h3>
                  {inventoryState.kind === "loading" && <p role="status">Checking inventory…</p>}
                  {inventoryState.kind === "unsupported" && (
                    <p role="status">
                      {detailsState.kind === "ready" && detailsState.details.medium.toLowerCase() !== "game"
                        ? "Actions are not available for this media yet."
                        : "Inventory lookup is not supported for this platform."}
                    </p>
                  )}
                  {inventoryState.kind === "unconfigured" && (
                    <p className="inventory-error" role="alert">
                      Your library inventory is not configured.
                    </p>
                  )}
                  {inventoryState.kind === "error" && (
                    <p className="inventory-error" role="alert">
                      Inventory status is temporarily unavailable. Select the platform again to retry.
                    </p>
                  )}
                  {inventoryState.kind === "resolved" && (
                    <>
                      <p role="status">Availability: {availabilityLabel(inventoryState.resolution.availability)}</p>
                      <p className="inventory-ownership">
                        {inventoryState.resolution.owned ? "Listed in inventory." : "Not listed in inventory."}
                      </p>
                    </>
                  )}
                </section>
              </>
            )}
          </div>
        </div>
      )}
    </section>
  );
}

export default function App() {
  const [query, setQuery] = useState("");
  const [searchState, setSearchState] = useState<SearchState>({ kind: "idle" });
  const [sessionResults, setSessionResults] = useState<SearchResult[]>([]);
  const [currentView, setCurrentView] = useState<"home" | "search" | "settings" | "system" | "universe">("home");
  const [searchFilter, setSearchFilter] = useState<SearchFilter>("all");
  const [selectedResult, setSelectedResult] = useState<SearchResult | null>(null);
  const [detailOrigin, setDetailOrigin] = useState<"home" | "search">("search");
  const [universeResolver, setUniverseResolver] = useState<UniverseResolverState>({ kind: "idle" });
  const [selectedUniverseWorkIDs, setSelectedUniverseWorkIDs] = useState<Set<string>>(new Set());
  const [resolverBusyIdentity, setResolverBusyIdentity] = useState<string | null>(null);
  const [resolverStatusMessage, setResolverStatusMessage] = useState("");
  const [activeUniverse, setActiveUniverse] = useState<UniverseSummary | null>(null);
  const [universeViewOrigin, setUniverseViewOrigin] = useState<"home" | "search">("search");
  const [universeDetailState, setUniverseDetailState] = useState<UniverseDetailState>({ kind: "loading" });
  const [universeManaging, setUniverseManaging] = useState(false);
  const [universeBusyIdentity, setUniverseBusyIdentity] = useState<string | null>(null);
  const [universeActionStatus, setUniverseActionStatus] = useState("");
  const [universeReload, setUniverseReload] = useState(0);
  const [metadataLanguageHint, setMetadataLanguageHint] = useState<MetadataLanguage | null>(null);
  const [exactUniverseAlias, setExactUniverseAlias] = useState<ExactUniverseAliasState>({ kind: "idle" });
  const requestIdentity = useRef(0);
  const exactAliasRequestIdentity = useRef(0);
  const exactAliasRequestKeys = useRef(new Set<string>());
  const activeExactAliasRequest = useRef<AbortController | null>(null);
  const activeExactAliasKey = useRef<string | null>(null);
  const activeSearch = useRef<{ query: string; controller: AbortController } | null>(null);
  const searchInputRef = useRef<HTMLInputElement | null>(null);
  const searchScrollPosition = useRef(0);
  const searchReturnFocusKey = useRef<string | null>(null);
  const pendingSearchRestore = useRef(false);
  const resolverIdentity = useRef(0);
  const createOperationIDs = useRef(new Map<string, string>());
  const skipSearchInputFocus = useRef(false);

  useEffect(() => {
    if (currentView !== "search") return;
    if (skipSearchInputFocus.current) {
      skipSearchInputFocus.current = false;
      return;
    }
    searchInputRef.current?.focus();
  }, [currentView]);

  useLayoutEffect(() => {
    if (selectedResult || currentView !== "search" || !pendingSearchRestore.current) return;
    pendingSearchRestore.current = false;
    if (window.scrollY !== searchScrollPosition.current) {
      window.scrollTo(0, searchScrollPosition.current);
    }
    const resultKey = searchReturnFocusKey.current;
    const focusTarget = resultKey === null
      ? null
      : [...document.querySelectorAll<HTMLButtonElement>("[data-search-focus-key]")]
        .find((button) => button.dataset.searchFocusKey === resultKey) ?? null;
    (focusTarget ?? searchInputRef.current)?.focus({ preventScroll: true });
  }, [selectedResult, currentView]);

  useEffect(() => () => {
    requestIdentity.current += 1;
    resolverIdentity.current += 1;
    activeSearch.current?.controller.abort();
    activeExactAliasRequest.current?.abort();
  }, []);

  useEffect(() => {
    if (currentView !== "universe" || !activeUniverse) return;
    const controller = new AbortController();
    let active = true;
    void (async () => {
      try {
        const response = await fetch(`${apiBaseUrl()}/api/v1/universes/${encodeURIComponent(activeUniverse.id)}`, {
          cache: "no-store",
          signal: controller.signal,
        });
        if (!response.ok) throw new Error("Universe unavailable");
        const detail = parseUniverseDetail(await response.json());
        if (!detail || detail.id !== activeUniverse.id) throw new Error("Invalid Universe response");
        if (active) setUniverseDetailState({ kind: "ready", detail });
      } catch {
        if (active && !controller.signal.aborted) setUniverseDetailState({ kind: "error" });
      }
    })();
    return () => {
      active = false;
      controller.abort();
    };
  }, [activeUniverse, currentView, universeReload]);

  async function search(normalizedQuery: string) {
    if (activeSearch.current?.query === normalizedQuery) return;
    const language = metadataLanguageHint === "original" ? "" : metadataLanguageHint ?? "";
    const aliasKey = JSON.stringify([normalizedQuery, language]);
    if (activeExactAliasRequest.current && activeExactAliasKey.current !== aliasKey) {
      activeExactAliasRequest.current.abort();
      if (activeExactAliasKey.current) exactAliasRequestKeys.current.delete(activeExactAliasKey.current);
      activeExactAliasRequest.current = null;
      activeExactAliasKey.current = null;
      exactAliasRequestIdentity.current += 1;
    }
    setExactUniverseAlias((current) => current.kind !== "idle" && current.query === normalizedQuery
      ? current
      : { kind: "idle" });

    resolverIdentity.current += 1;
    setUniverseResolver({ kind: "idle" });
    setSelectedUniverseWorkIDs(new Set());
    setResolverStatusMessage("");
    activeSearch.current?.controller.abort();
    const controller = new AbortController();
    activeSearch.current = { query: normalizedQuery, controller };
    const identity = ++requestIdentity.current;
    setSearchState({ kind: "loading", query: normalizedQuery });

    try {
      const searchResponse = fetch(`${apiBaseUrl()}/api/v1/search?q=${encodeURIComponent(normalizedQuery)}`, {
        cache: "no-store",
        signal: controller.signal,
      });
      void resolveExactUniverseAlias(normalizedQuery, metadataLanguageHint);
      const response = await searchResponse;
      if (!response.ok) throw new Error("search unavailable");

      let payload: unknown;
      try {
        payload = await response.json();
      } catch {
        throw new Error("invalid search response");
      }
      const searchEnvelope = parseSearchEnvelope(payload);
      if (!searchEnvelope) throw new Error("invalid search response");
      if (identity !== requestIdentity.current) return;

      setSessionResults(searchEnvelope.results);
      setSearchState(
        searchEnvelope.results.length > 0
          ? {
              kind: "results",
              query: normalizedQuery,
              results: searchEnvelope.results,
              sources: searchEnvelope.sources,
              universeDiscovery: searchEnvelope.universeDiscovery,
            }
          : {
              kind: "empty",
              query: normalizedQuery,
              sources: searchEnvelope.sources,
              universeDiscovery: searchEnvelope.universeDiscovery,
            },
      );
    } catch (error) {
      if (identity !== requestIdentity.current || controller.signal.aborted) return;

      setSearchState({
        kind: "error",
        query: normalizedQuery,
        message: error instanceof Error && error.message === "invalid search response"
          ? invalidResponseMessage
          : unavailableMessage,
      });
    } finally {
      if (identity === requestIdentity.current) activeSearch.current = null;
    }
  }

  async function resolveExactUniverseAlias(query: string, languageHint: MetadataLanguage | null) {
    const language = languageHint === "original" ? undefined : languageHint ?? undefined;
    const key = JSON.stringify([query, language ?? ""]);
    if (exactAliasRequestKeys.current.has(key)) return;
    exactAliasRequestKeys.current.add(key);
    if (exactAliasRequestKeys.current.size > 100) {
      const oldestKey = exactAliasRequestKeys.current.values().next().value;
      if (oldestKey) exactAliasRequestKeys.current.delete(oldestKey);
    }

    activeExactAliasRequest.current?.abort();
    const controller = new AbortController();
    activeExactAliasRequest.current = controller;
    activeExactAliasKey.current = key;
    const identity = ++exactAliasRequestIdentity.current;
    setExactUniverseAlias({ kind: "loading", query });

    try {
      const request: { alias: string; language?: string } = { alias: query };
      if (language) request.language = language;
      const response = await fetch(`${apiBaseUrl()}/api/v1/universes/relationships/resolve`, {
        method: "POST",
        cache: "no-store",
        headers: { "Content-Type": "application/json" },
        signal: controller.signal,
        body: JSON.stringify(request),
      });
      if (response.status === 404) {
        if (identity === exactAliasRequestIdentity.current && !controller.signal.aborted) {
          setExactUniverseAlias({ kind: "not_found", query });
        }
        return;
      }
      if (!response.ok) throw new Error("exact Universe name lookup unavailable");
      const resolution: unknown = await response.json();
      if (!isUniverseRelationshipResolution(resolution)) throw new Error("invalid relationship resolution response");

      const universeResponse = await fetch(
        `${apiBaseUrl()}/api/v1/universes/${encodeURIComponent(resolution.universe_id)}`,
        { cache: "no-store", signal: controller.signal },
      );
      if (!universeResponse.ok) throw new Error("resolved Universe unavailable");
      const detail = parseUniverseDetail(await universeResponse.json());
      if (!detail || detail.id !== resolution.universe_id) throw new Error("invalid resolved Universe");
      if (identity === exactAliasRequestIdentity.current && !controller.signal.aborted) {
        setExactUniverseAlias({
          kind: "matched",
          query,
          universe: { id: detail.id, title: detail.title },
        });
      }
    } catch {
      if (identity === exactAliasRequestIdentity.current && !controller.signal.aborted) {
        setExactUniverseAlias({ kind: "error", query });
      }
    } finally {
      if (identity === exactAliasRequestIdentity.current) {
        activeExactAliasRequest.current = null;
        activeExactAliasKey.current = null;
      }
    }
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const normalizedQuery = query.trim();
    if (!normalizedQuery) {
      activeSearch.current?.controller.abort();
      activeSearch.current = null;
      requestIdentity.current += 1;
      resolverIdentity.current += 1;
      setUniverseResolver({ kind: "idle" });
      setSearchState({ kind: "idle" });
      return;
    }

    pendingSearchRestore.current = false;
    searchReturnFocusKey.current = null;
    setSelectedResult(null);
    setActiveUniverse(null);
    setCurrentView("search");
    void search(normalizedQuery);
  }

  function openDetails(result: SearchResult, origin: "home" | "search") {
    if (origin === "search") {
      searchScrollPosition.current = window.scrollY;
      searchReturnFocusKey.current = `result:${result.provider}:${result.external_id}`;
      pendingSearchRestore.current = true;
    }
    setActiveUniverse(null);
    setDetailOrigin(origin);
    setSelectedResult(result);
  }

  function openUniverse(universe: UniverseSummary, origin: "home" | "search" = "search") {
    if (origin === "search") {
      searchScrollPosition.current = window.scrollY;
      searchReturnFocusKey.current = `universe:${universe.id}`;
      pendingSearchRestore.current = true;
    }
    setUniverseViewOrigin(origin);
    setSelectedResult(null);
    setActiveUniverse(universe);
    setUniverseDetailState({ kind: "loading" });
    setUniverseManaging(false);
    setUniverseActionStatus("");
    setCurrentView("universe");
  }

  function closeUniverse() {
    skipSearchInputFocus.current = universeViewOrigin === "search";
    setActiveUniverse(null);
    setCurrentView(universeViewOrigin);
  }

  function leaveSearchFor(view: "home" | "search" | "settings") {
    pendingSearchRestore.current = false;
    searchReturnFocusKey.current = null;
    setCurrentView(view);
    setSelectedResult(null);
    setActiveUniverse(null);
  }

  async function groupRelatedResults() {
    if (searchState.kind !== "results") return;
    const identity = ++resolverIdentity.current;
    setUniverseResolver({ kind: "loading" });
    setSelectedUniverseWorkIDs(new Set());
    setResolverStatusMessage("");
    const results = searchState.results.slice(0, 20).map(universeWorkFromResult);
    try {
      const response = await fetch(`${apiBaseUrl()}/api/v1/universes/resolve`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ query: searchState.query, results }),
      });
      if (!response.ok) throw new Error("Universe proposal unavailable");
      const candidates = parseUniverseCandidates(await response.json());
      if (!candidates) throw new Error("Invalid Universe proposal response");
      if (identity === resolverIdentity.current) setUniverseResolver({ kind: "ready", candidates });
    } catch {
      if (identity === resolverIdentity.current) setUniverseResolver({ kind: "error" });
    }
  }

  function selectUniverseWork(identity: string, checked: boolean) {
    setSelectedUniverseWorkIDs((current) => {
      const next = new Set(current);
      if (checked) next.add(identity);
      else next.delete(identity);
      return next;
    });
  }

  function workForProposal(proposal: UniverseProposal): UniverseWorkInput | null {
    const result = sessionResults.find((item) => item.provider === proposal.provider && item.external_id === proposal.external_id);
    return result ? universeWorkFromResult(result) : null;
  }

  async function confirmUniverseCandidate(candidate: UniverseCandidate, proposals: UniverseProposal[]) {
    if (proposals.length === 0) return;
    const works = proposals.map(workForProposal);
    if (works.some((work) => work === null)) {
      setResolverStatusMessage("A selected exact Work is no longer in the current Search results. Run Search again before confirming.");
      return;
    }
    const confirmedWorks = works as UniverseWorkInput[];
    setResolverBusyIdentity(`candidate:${candidate.id ?? candidate.title}`);
    setResolverStatusMessage("");
    try {
      let detail = null as ReturnType<typeof parseUniverseDetail>;
      if (candidate.existing && candidate.id) {
        for (const work of confirmedWorks) {
          const response = await fetch(`${apiBaseUrl()}/api/v1/universes/${encodeURIComponent(candidate.id)}/memberships`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(work),
          });
          if (!response.ok) throw new Error("Work confirmation failed");
          detail = parseUniverseDetail(await response.json());
          if (!detail || detail.id !== candidate.id) throw new Error("Invalid Universe response");
        }
      } else {
        const operationKey = JSON.stringify({
          title: candidate.title,
          works: [...confirmedWorks].sort((left, right) => workIdentity(left.provider, left.external_id).localeCompare(workIdentity(right.provider, right.external_id))),
        });
        let operationID = createOperationIDs.current.get(operationKey);
        if (!operationID) {
          const bytes = new Uint8Array(16);
          if (!globalThis.crypto?.getRandomValues) throw new Error("Secure operation identity is unavailable");
          globalThis.crypto.getRandomValues(bytes);
          operationID = `ui-${Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
          createOperationIDs.current.set(operationKey, operationID);
        }
        const response = await fetch(`${apiBaseUrl()}/api/v1/universes`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ operation_id: operationID, title: candidate.title, works: confirmedWorks }),
        });
        if (!response.ok) throw new Error("Universe creation failed");
        detail = parseUniverseDetail(await response.json());
        if (!detail) throw new Error("Invalid Universe response");
        createOperationIDs.current.delete(operationKey);
      }
      if (!detail) throw new Error("Universe response unavailable");
      setActiveUniverse({ id: detail.id, title: detail.title });
      setUniverseViewOrigin("search");
      setUniverseDetailState({ kind: "loading" });
      setUniverseManaging(false);
      setUniverseActionStatus("");
      setCurrentView("universe");
      setUniverseResolver({ kind: "idle" });
    } catch {
      setResolverStatusMessage("The selected Works could not all be confirmed. No suggested group was accepted automatically; review the Universe before trying again.");
    } finally {
      setResolverBusyIdentity(null);
    }
  }

  async function saveUniverseDecision(
    identity: string,
    path: string,
    method: "POST" | "DELETE",
    body: unknown,
    successMessage: string,
  ): Promise<boolean> {
    if (!activeUniverse) return false;
    setUniverseBusyIdentity(identity);
    setUniverseActionStatus("");
    try {
      const response = await fetch(`${apiBaseUrl()}${path}`, {
        method,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!response.ok) throw new Error("Universe decision failed");
      const detail = parseUniverseDetail(await response.json());
      if (!detail || detail.id !== activeUniverse.id) throw new Error("Invalid Universe response");
      setUniverseDetailState({ kind: "ready", detail });
      setUniverseActionStatus(successMessage);
      return true;
    } catch {
      setUniverseActionStatus("That Universe change could not be saved. Existing membership and exclusion state remain unchanged.");
      return false;
    } finally {
      setUniverseBusyIdentity(null);
    }
  }

  async function saveProposalDecision(
    universeID: string,
    identity: string,
    route: "reject" | "reset",
    body: unknown,
    successMessage: string,
  ): Promise<boolean> {
    setResolverBusyIdentity(identity);
    setResolverStatusMessage("");
    try {
      const response = await fetch(`${apiBaseUrl()}/api/v1/universes/${encodeURIComponent(universeID)}/${route}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!response.ok) throw new Error("Universe proposal decision failed");
      const detail = parseUniverseDetail(await response.json());
      if (!detail || detail.id !== universeID) throw new Error("Invalid Universe response");
      if (activeUniverse?.id === universeID) setUniverseDetailState({ kind: "ready", detail });
      setResolverStatusMessage(successMessage);
      return true;
    } catch {
      setResolverStatusMessage("That explicit Universe decision could not be saved. Search results and suggestions remain unchanged.");
      return false;
    } finally {
      setResolverBusyIdentity(null);
    }
  }

  async function confirmUniverseWork(work: UniverseWorkInput) {
    if (!activeUniverse) return;
    const identity = workIdentity(work.provider, work.external_id);
    await saveUniverseDecision(identity, `/api/v1/universes/${encodeURIComponent(activeUniverse.id)}/memberships`, "POST", work, `${work.title} was confirmed in this Universe.`);
  }

  async function confirmUniverse() {
    if (!activeUniverse) return;
    await saveUniverseDecision(
      "universe-confirm",
      `/api/v1/universes/${encodeURIComponent(activeUniverse.id)}/confirm`,
      "POST",
      {},
      `${activeUniverse.title} was confirmed by you. Confirm Works individually.`,
    );
  }

  async function removeUniverseWork(work: UniverseWorkInput) {
    if (!activeUniverse) return;
    const identity = workIdentity(work.provider, work.external_id);
    await saveUniverseDecision(identity, `/api/v1/universes/${encodeURIComponent(activeUniverse.id)}/memberships`, "DELETE", {
      provider: work.provider,
      external_id: work.external_id,
      reason: "Removed from this Universe by user",
    }, `${work.title} was removed and excluded from this Universe.`);
  }

  async function rejectUniverseWork(work: UniverseWorkInput) {
    if (!activeUniverse) return;
    const identity = workIdentity(work.provider, work.external_id);
    await saveUniverseDecision(identity, `/api/v1/universes/${encodeURIComponent(activeUniverse.id)}/reject`, "POST", {
      work,
      reason: "Excluded from this Universe by user",
    }, `${work.title} was excluded from this Universe.`);
  }

  async function resetUniverseExclusion(work: UniverseWorkInput) {
    if (!activeUniverse) return;
    const identity = workIdentity(work.provider, work.external_id);
    await saveUniverseDecision(identity, `/api/v1/universes/${encodeURIComponent(activeUniverse.id)}/reset`, "POST", {
      action: "clear_exclusion",
      provider: work.provider,
      external_id: work.external_id,
    }, `The exclusion for ${work.title} was cleared. Confirm it separately to add it.`);
  }

  async function excludeUniverseProposal(universeID: string, proposal: UniverseProposal) {
    const work = workForProposal(proposal);
    if (!work) return;
    const identity = workIdentity(work.provider, work.external_id);
    const success = await saveProposalDecision(universeID, identity, "reject", {
      work,
      reason: "Excluded from this Universe by user",
    }, `${work.title} was excluded from this Universe.`);
    if (success) await groupRelatedResults();
  }

  async function resetUniverseProposalExclusion(universeID: string, proposal: UniverseProposal) {
    const work = workForProposal(proposal);
    if (!work) return;
    const identity = workIdentity(work.provider, work.external_id);
    const success = await saveProposalDecision(universeID, identity, "reset", {
      action: "clear_exclusion",
      provider: work.provider,
      external_id: work.external_id,
    }, `The exclusion for ${work.title} was cleared. Confirm it separately to add it.`);
    if (success) await groupRelatedResults();
  }

  function openExistingUniverse(universe: UniverseSummary) {
    openUniverse(universe, "search");
  }

  function retryUniverse() {
    setUniverseDetailState({ kind: "loading" });
    setUniverseReload((value) => value + 1);
  }

  return (
    <div className="app-shell">
      <header className="app-header">
        <a className="brand-lockup" href="#home" onClick={(event) => {
          event.preventDefault();
          leaveSearchFor("home");
        }}>
          <span className="brand-mark" aria-hidden="true">L</span>
          <h1 className="brand-name">Lernae</h1>
        </a>
        {selectedResult && (
          <button className="header-back-button" type="button" onClick={() => setSelectedResult(null)}>
            {detailOrigin === "home" ? "Back to Home" : "Back to results"}
          </button>
        )}
        {!selectedResult && currentView === "universe" && activeUniverse && (
          <button className="header-back-button" type="button" onClick={closeUniverse}>
            {universeViewOrigin === "home" ? "Back to Home" : "Back to results"}
          </button>
        )}
        <p className="brand-tagline">One library. Your universe.</p>
        <CatalogSearchForm
          compact
          inputId="header-catalog-query"
          inputLabel="Quick catalog search"
          onChange={setQuery}
          onSubmit={submit}
          query={query}
        />
        <nav className="primary-nav" aria-label="Primary navigation">
          <button
            aria-current={currentView === "home" ? "page" : undefined}
            onClick={() => leaveSearchFor("home")}
            type="button"
          >Home</button>
          <button
            aria-current={currentView === "search" ? "page" : undefined}
            onClick={() => leaveSearchFor("search")}
            type="button"
          >Search</button>
          <button
            aria-current={currentView === "settings" || currentView === "system" ? "page" : undefined}
            onClick={() => leaveSearchFor("settings")}
            type="button"
          >Settings</button>
        </nav>
      </header>

      <main className="page-content">
        {currentView === "system" ? (
          <>
            <button className="back-button" type="button" onClick={() => setCurrentView("settings")}>Back to settings</button>
            <SystemStatusView />
          </>
        ) : currentView === "settings" ? (
          <SettingsView
            onMetadataLanguageChange={setMetadataLanguageHint}
            onSystemStatus={() => setCurrentView("system")}
          />
        ) : currentView === "universe" && activeUniverse ? (
          <UniverseDetailView
            busyIdentity={universeBusyIdentity}
            managing={universeManaging}
            onConfirmUniverse={() => void confirmUniverse()}
            onConfirmWork={confirmUniverseWork}
            onRejectWork={rejectUniverseWork}
            onRemoveWork={removeUniverseWork}
            onResetExclusion={resetUniverseExclusion}
            onRetry={retryUniverse}
            onToggleManage={() => setUniverseManaging((value) => !value)}
            state={universeDetailState}
            statusMessage={universeActionStatus}
            workResults={searchResultUniverseWorkList(sessionResults.map(universeWorkFromResult))}
          />
        ) : selectedResult ? (
          <MediaDetails
            key={`${selectedResult.provider}:${selectedResult.external_id}`}
            onOpenSeriesMember={setSelectedResult}
            result={selectedResult}
          />
        ) : currentView === "home" ? (
          <>
            <HomeDiscovery
              items={sessionResults}
              onOpen={(result) => openDetails(result, "home")}
              onSearch={() => setCurrentView("search")}
            />
            <section className="home-search" aria-label="Search the catalog">
              <p className="eyebrow">Looking for something specific?</p>
              <CatalogSearchForm
                inputRef={searchInputRef}
                onChange={setQuery}
                onSubmit={submit}
                query={query}
              />
            </section>
          </>
        ) : (
          <SearchView
            activeFilter={searchFilter}
            inputRef={searchInputRef}
            universeDiscovery={searchState.kind === "results" || searchState.kind === "empty"
              ? searchState.universeDiscovery
              : undefined}
            exactUniverseAlias={exactUniverseAlias}
            resolverState={universeResolver}
            selectedUniverseWorkIDs={selectedUniverseWorkIDs}
            resolverBusyIdentity={resolverBusyIdentity}
            resolverStatusMessage={resolverStatusMessage}
            onFilterChange={setSearchFilter}
            onChange={setQuery}
            onGroupRelated={() => void groupRelatedResults()}
            onOpen={(result) => openDetails(result, "search")}
            onOpenUniverse={openExistingUniverse}
            onUniverseSelectionChange={selectUniverseWork}
            onConfirmUniverseCandidate={(candidate, proposals) => void confirmUniverseCandidate(candidate, proposals)}
            onExcludeUniverseProposal={(universeID, proposal) => void excludeUniverseProposal(universeID, proposal)}
            onResetUniverseExclusion={(universeID, proposal) => void resetUniverseProposalExclusion(universeID, proposal)}
            onRetry={(retryQuery) => void search(retryQuery)}
            onSubmit={submit}
            query={query}
            searchState={searchState}
          />
        )}
      </main>
    </div>
  );
}
