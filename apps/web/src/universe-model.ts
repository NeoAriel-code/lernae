export type UniverseWorkInput = {
  provider: string;
  external_id: string;
  medium: string;
  work_type: string;
  title: string;
};

export type UniverseSummary = { id: string; title: string };

export type UniverseDiscovery = {
  state: "not_eligible" | "created" | "reused" | "ambiguous";
  universe_id?: string;
  title?: string;
  existence_confidence?: number;
  memberships_added: number;
  provenance?: "manual" | "automatic";
  confirmed_by_user: boolean;
};

export type UniverseMembership = UniverseWorkInput & {
  provenance: string;
  confidence: number;
  confirmed_by_user: boolean;
};

export type UniverseSuggestion = UniverseWorkInput & {
  media_type: string;
  provenance: string;
  confidence: number;
  evidence: string;
  reason: string;
  confirmed_by_user: false;
};

export type UniverseExclusion = UniverseWorkInput & { reason: string };

export type UniverseAlias = { value: string; language: string | null };

export type UniverseDetail = {
  id: string;
  title: string;
  existence_confidence?: number;
  provenance?: "manual" | "automatic";
  confirmed_by_user?: boolean;
  memberships: UniverseMembership[];
  aliases: UniverseAlias[];
  aliases_truncated: boolean;
  suggestions: UniverseSuggestion[];
  exclusions: UniverseExclusion[];
  memberships_truncated: boolean;
  suggestions_truncated: boolean;
  exclusions_truncated: boolean;
};

export type UniverseProposal = {
  universe_id?: string;
  universe_title: string;
  provider: string;
  external_id: string;
  media_type: string;
  title: string;
  existing_membership_universe_id?: string;
  existing_state: "none" | "accepted" | "accepted_elsewhere" | "excluded";
  confirmed_by_user: boolean;
  evidence: string;
  reason: string;
  confidence: number;
  confidence_level: "none" | "moderate" | "high";
  suppressed: boolean;
  requires_confirmation: boolean;
  medium: string;
  work_type: string;
};

export type UniverseCandidate = {
  id?: string;
  title: string;
  existing: boolean;
  evidence: string;
  reason: string;
  confidence: number;
  confidence_level: "none" | "moderate" | "high";
  memberships: UniverseProposal[];
};

export type UniverseResolverState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "error" }
  | { kind: "ready"; candidates: UniverseCandidate[] };

export type UniverseDetailState =
  | { kind: "loading" }
  | { kind: "error" }
  | { kind: "ready"; detail: UniverseDetail };

export function parseUniverseDiscovery(value: unknown): UniverseDiscovery | null {
  if (!isRecord(value) ||
    !["not_eligible", "created", "reused", "ambiguous"].includes(String(value.state)) ||
    !Number.isInteger(value.memberships_added) || (value.memberships_added as number) < 0 ||
    typeof value.confirmed_by_user !== "boolean" ||
    (value.universe_id !== undefined && !bounded(value.universe_id, 128)) ||
    (value.title !== undefined && !bounded(value.title, 512)) ||
    (value.existence_confidence !== undefined && !isConfidence(value.existence_confidence)) ||
    (value.provenance !== undefined && value.provenance !== "manual" && value.provenance !== "automatic")) return null;

  const state = value.state as UniverseDiscovery["state"];
  if ((state === "created" || state === "reused") &&
    (typeof value.universe_id !== "string" || typeof value.title !== "string" || typeof value.provenance !== "string")) return null;

  return {
    state,
    universe_id: typeof value.universe_id === "string" ? value.universe_id : undefined,
    title: typeof value.title === "string" ? value.title : undefined,
    existence_confidence: typeof value.existence_confidence === "number" ? value.existence_confidence : undefined,
    memberships_added: value.memberships_added as number,
    provenance: value.provenance as UniverseDiscovery["provenance"],
    confirmed_by_user: value.confirmed_by_user,
  };
}

export function parseUniverseDetail(value: unknown): UniverseDetail | null {
  if (!isRecord(value) || !bounded(value.id, 128) || !bounded(value.title, 512) ||
    (value.existence_confidence !== undefined && !isConfidence(value.existence_confidence)) ||
    (value.provenance !== undefined && value.provenance !== "manual" && value.provenance !== "automatic") ||
    (value.confirmed_by_user !== undefined && typeof value.confirmed_by_user !== "boolean") ||
    !Array.isArray(value.memberships) || value.memberships.length > 100 ||
    !Array.isArray(value.suggestions) || value.suggestions.length > 100 ||
    !Array.isArray(value.exclusions) || value.exclusions.length > 100 ||
    typeof value.memberships_truncated !== "boolean" || typeof value.suggestions_truncated !== "boolean" ||
    typeof value.exclusions_truncated !== "boolean") return null;
  const memberships: UniverseMembership[] = [];
  for (const item of value.memberships) {
    if (!isRecord(item) || !isUniverseWork(item) ||
      !["provider", "rule", "manual", "automatic", "unknown"].includes(String(item.provenance)) ||
      !isConfidence(item.confidence) || typeof item.confirmed_by_user !== "boolean") return null;
    memberships.push({
      ...asUniverseWork(item),
      provenance: item.provenance as string,
      confidence: item.confidence,
      confirmed_by_user: item.confirmed_by_user,
    });
  }
  const suggestions: UniverseSuggestion[] = [];
  for (const item of value.suggestions) {
    if (!isRecord(item) || !isUniverseWork(item) ||
      !bounded(item.media_type, 64) ||
      !["provider", "rule", "manual", "automatic", "unknown"].includes(String(item.provenance)) ||
      !isConfidence(item.confidence) || !bounded(item.evidence, 64) || !bounded(item.reason, 512) ||
      item.confirmed_by_user !== false) return null;
    suggestions.push({
      ...asUniverseWork(item),
      media_type: item.media_type as string,
      provenance: item.provenance as string,
      confidence: item.confidence,
      evidence: item.evidence,
      reason: item.reason,
      confirmed_by_user: false,
    });
  }
  const exclusions: UniverseExclusion[] = [];
  for (const item of value.exclusions) {
    if (!isRecord(item) || !isUniverseWork(item) || !bounded(item.reason, 512)) return null;
    exclusions.push({ ...asUniverseWork(item), reason: item.reason });
  }
  const aliases = parseUniverseAliases(value.aliases);
  return {
    id: value.id,
    title: value.title,
    existence_confidence: typeof value.existence_confidence === "number" ? value.existence_confidence : undefined,
    provenance: value.provenance as UniverseDetail["provenance"],
    confirmed_by_user: typeof value.confirmed_by_user === "boolean" ? value.confirmed_by_user : undefined,
    memberships,
    aliases: aliases.values,
    aliases_truncated: value.aliases_truncated === true || aliases.truncated,
    suggestions,
    exclusions,
    memberships_truncated: value.memberships_truncated,
    suggestions_truncated: value.suggestions_truncated,
    exclusions_truncated: value.exclusions_truncated,
  };
}

function parseUniverseAliases(value: unknown): { values: UniverseAlias[]; truncated: boolean } {
  if (value === undefined) return { values: [], truncated: false };
  if (!Array.isArray(value)) return { values: [], truncated: true };
  const values: UniverseAlias[] = [];
  let truncated = value.length > 100;
  for (const item of value.slice(0, 100)) {
    if (!isRecord(item) || !bounded(item.value, 512) ||
      (item.language !== null && item.language !== undefined && !bounded(item.language, 35))) {
      truncated = true;
      continue;
    }
    values.push({ value: item.value, language: typeof item.language === "string" ? item.language : null });
  }
  return { values, truncated };
}

export function parseUniverseCandidates(value: unknown): UniverseCandidate[] | null {
  if (!isRecord(value) || !Array.isArray(value.candidates) || value.candidates.length > 20) return null;
  const candidates: UniverseCandidate[] = [];
  for (const item of value.candidates) {
    if (!isRecord(item) || !bounded(item.title, 512) || typeof item.existing !== "boolean" ||
      !bounded(item.evidence, 64) || !bounded(item.reason, 512) || !isConfidence(item.confidence) ||
      !isConfidenceLevel(item.confidence_level) || !Array.isArray(item.memberships) || item.memberships.length > 20 ||
      (item.id !== undefined && !bounded(item.id, 128))) return null;
    const memberships: UniverseProposal[] = [];
    for (const proposal of item.memberships) {
      if (!isRecord(proposal) || !bounded(proposal.universe_title, 512) || !bounded(proposal.provider, 128) ||
        !bounded(proposal.external_id, 512) || !bounded(proposal.media_type, 64) || !bounded(proposal.title, 512) ||
        !isExistingState(proposal.existing_state) || typeof proposal.confirmed_by_user !== "boolean" ||
        !bounded(proposal.evidence, 64) || !bounded(proposal.reason, 512) || !isConfidence(proposal.confidence) ||
        !isConfidenceLevel(proposal.confidence_level) || typeof proposal.suppressed !== "boolean" ||
        typeof proposal.requires_confirmation !== "boolean" ||
        (proposal.universe_id !== undefined && !bounded(proposal.universe_id, 128)) ||
        (proposal.existing_membership_universe_id !== undefined && !bounded(proposal.existing_membership_universe_id, 128))) return null;
      const medium = mediumForMediaType(proposal.media_type);
      const workType = workTypeForMediaType(proposal.media_type);
      memberships.push({
        universe_title: proposal.universe_title,
        provider: proposal.provider,
        external_id: proposal.external_id,
        media_type: proposal.media_type,
        title: proposal.title,
        universe_id: typeof proposal.universe_id === "string" ? proposal.universe_id : undefined,
        existing_membership_universe_id: typeof proposal.existing_membership_universe_id === "string"
          ? proposal.existing_membership_universe_id
          : undefined,
        existing_state: proposal.existing_state,
        confirmed_by_user: proposal.confirmed_by_user,
        evidence: proposal.evidence,
        reason: proposal.reason,
        confidence: proposal.confidence,
        confidence_level: proposal.confidence_level,
        suppressed: proposal.suppressed,
        requires_confirmation: proposal.requires_confirmation,
        medium,
        work_type: workType,
      });
    }
    candidates.push({
      id: typeof item.id === "string" ? item.id : undefined,
      title: item.title,
      existing: item.existing,
      evidence: item.evidence,
      reason: item.reason,
      confidence: item.confidence,
      confidence_level: item.confidence_level,
      memberships,
    });
  }
  return candidates;
}

export function universeWorkFromResult(result: Pick<UniverseWorkInput, "provider" | "external_id" | "medium" | "work_type" | "title">): UniverseWorkInput {
  return {
    provider: result.provider,
    external_id: result.external_id,
    medium: result.medium,
    work_type: result.work_type,
    title: result.title,
  };
}

export function workIdentity(provider: string, externalID: string): string {
  return `${provider}\u0000${externalID}`;
}

export function proposalSelectionIdentity(candidate: UniverseCandidate, index: number, proposal: UniverseProposal): string {
  return `${candidate.id ?? candidate.title}\u0001${index}\u0001${workIdentity(proposal.provider, proposal.external_id)}`;
}

export function universeMediaLabel(medium: string, workType: string): string {
  if (medium.toLowerCase() === "literature" && workType.toLowerCase() === "book") return "Book";
  if (medium.toLowerCase() === "video" && ["series", "tv"].includes(workType.toLowerCase())) return "TV";
  if (medium.toLowerCase() === "game" && workType.toLowerCase() === "game") return "Game";
  return titleCase(workType || medium);
}

export function universeProposalLabel(proposal: UniverseProposal): string {
  if (proposal.media_type === "book") return "Book";
  if (proposal.media_type === "tv") return "TV";
  if (proposal.media_type === "game") return "Game";
  return titleCase(proposal.media_type);
}

function titleCase(value: string): string {
  return value.split(/[_-]+/).filter(Boolean).map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ");
}

function mediumForMediaType(mediaType: string): string {
  switch (mediaType) {
    case "book": return "literature";
    case "tv": return "video";
    case "game": return "game";
    default: return mediaType;
  }
}

function workTypeForMediaType(mediaType: string): string {
  switch (mediaType) {
    case "book": return "book";
    case "tv": return "series";
    case "game": return "game";
    default: return mediaType;
  }
}

function isUniverseWork(value: Record<string, unknown>): value is Record<string, unknown> & UniverseWorkInput {
  return bounded(value.provider, 128) && bounded(value.external_id, 512) && bounded(value.title, 512) &&
    bounded(value.medium, 64) && bounded(value.work_type, 128);
}

function asUniverseWork(value: Record<string, unknown>): UniverseWorkInput {
  return {
    provider: value.provider as string,
    external_id: value.external_id as string,
    title: value.title as string,
    medium: value.medium as string,
    work_type: value.work_type as string,
  };
}

function isExistingState(value: unknown): value is UniverseProposal["existing_state"] {
  return value === "none" || value === "accepted" || value === "accepted_elsewhere" || value === "excluded";
}

function isConfidenceLevel(value: unknown): value is UniverseCandidate["confidence_level"] {
  return value === "none" || value === "moderate" || value === "high";
}

function isConfidence(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value) && value >= 0 && value <= 1;
}

function bounded(value: unknown, max: number): value is string {
  if (typeof value !== "string" || value.trim() === "" || value.length > max) return false;
  return !Array.from(value).some((character) => {
    const codePoint = character.codePointAt(0) ?? 0;
    return codePoint <= 0x1f || (codePoint >= 0x7f && codePoint <= 0x9f);
  });
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function searchResultUniverseWorkList(results: UniverseWorkInput[]): UniverseWorkInput[] {
  const seen = new Set<string>();
  return results.filter((result) => {
    const identity = workIdentity(result.provider, result.external_id);
    if (seen.has(identity)) return false;
    seen.add(identity);
    return true;
  });
}
