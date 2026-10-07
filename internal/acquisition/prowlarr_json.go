package acquisition

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ProwlarrProtocol is the pinned OpenAPI DownloadProtocol string enum. It is
// provider-owned private state, not a common Candidate field or numeric CLR enum.
type ProwlarrProtocol string

const (
	ProwlarrProtocolUnknown ProwlarrProtocol = "unknown"
	ProwlarrProtocolUsenet  ProwlarrProtocol = "usenet"
	ProwlarrProtocolTorrent ProwlarrProtocol = "torrent"
)

func (p ProwlarrProtocol) valid() bool {
	return p == ProwlarrProtocolUnknown || p == ProwlarrProtocolUsenet || p == ProwlarrProtocolTorrent
}

// Only documented fields needed for the exact snapshot are consumed. Remaining
// documented optional ReleaseResource fields may be absent/null and do not act
// as filters. Raw JSON never leaves this bounded parser or becomes a record.
type prowlarrRelease struct {
	ID          *int64            `json:"id"`
	IndexerID   *int64            `json:"indexerId"`
	GUID        *string           `json:"guid"`
	Title       *string           `json:"title"`
	Size        *int64            `json:"size"`
	Protocol    *ProwlarrProtocol `json:"protocol"`
	DownloadURL *string           `json:"downloadUrl"`
	MagnetURL   *string           `json:"magnetUrl"`
}

// CategoryResource's documented nullable shape is validated, never used to
// infer identities, filter possibilities, or add provider-specific metadata.
type prowlarrCategory struct {
	ID            *int32              `json:"id"`
	Name          *string             `json:"name"`
	SubCategories []*prowlarrCategory `json:"subCategories"`
}

func (r prowlarrRelease) sameOriginalTitle(other prowlarrRelease) bool {
	// Normalized records are compared separately, including protocol, optional
	// size and keyless locators. Transient id and equivalent missing/null/default
	// values are not authority. Different unsuitable titles must still conflict
	// rather than being hidden by identical safe generic presentation.
	text := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	return text(r.Title) == text(other.Title)
}

func decodeProwlarrReleases(data []byte) ([]prowlarrRelease, error) {
	if !utf8.Valid(data) || strictProwlarrJSON(data) != nil || !prowlarrUnicodeEscapes(data) {
		return nil, ErrInvalidResponse
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, ErrInvalidResponse
	}
	var raw []json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil || len(raw) > prowlarrMaxReleases {
		return nil, ErrInvalidResponse
	}
	releases := make([]prowlarrRelease, 0, len(raw))
	for _, value := range raw {
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil || fields == nil {
			return nil, ErrInvalidResponse
		}
		// encoding/json matches case-insensitively; reject ambiguous lookalikes
		// instead of allowing them to replace documented exact field spellings.
		for name := range fields {
			for _, known := range []string{"id", "indexerId", "guid", "title", "size", "protocol", "downloadUrl", "magnetUrl", "indexer", "categories"} {
				if name != known && strings.EqualFold(name, known) {
					return nil, ErrInvalidResponse
				}
			}
		}
		var release struct {
			prowlarrRelease
			Indexer    *string             `json:"indexer"`
			Categories []*prowlarrCategory `json:"categories"`
		}
		if json.Unmarshal(value, &release) != nil || (release.Protocol != nil && !release.Protocol.valid()) {
			return nil, ErrInvalidResponse
		}
		releases = append(releases, release.prowlarrRelease)
	}
	return releases, nil
}

// Token traversal checks duplicate keys at every depth, one complete value,
// finite nesting and valid numbers, while accepting undocumented optional
// fields without making arbitrary raw payloads part of the provider model.
func strictProwlarrJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if consumeProwlarrJSON(decoder, 0) != nil {
		return ErrInvalidResponse
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalidResponse
	}
	return nil
}

func consumeProwlarrJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return ErrInvalidResponse
	}
	token, err := d.Token()
	if err != nil {
		return ErrInvalidResponse
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return ErrInvalidResponse
			}
			seen[name] = true
			if consumeProwlarrJSON(d, depth+1) != nil {
				return ErrInvalidResponse
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalidResponse
		}
	case '[':
		for d.More() {
			if consumeProwlarrJSON(d, depth+1) != nil {
				return ErrInvalidResponse
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalidResponse
		}
	default:
		return ErrInvalidResponse
	}
	return nil
}

// encoding/json replaces unmatched UTF-16 surrogates with U+FFFD. That is not
// acceptable for exact GUID authority. Validate escapes after syntax checking;
// real U+FFFD and literal escaped backslashes remain valid distinct strings.
func prowlarrUnicodeEscapes(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		value, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value < 0xd800 || value > 0xdbff {
			continue
		}
		if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
