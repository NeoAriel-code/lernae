package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lernae/internal/domain"
)

const (
	ProwlarrProviderID       = "prowlarr"
	ProwlarrDiscoveryTimeout = 30 * time.Second
	prowlarrMaxResponseBytes = 2 << 20
	prowlarrMaxReleases      = 4096
	prowlarrMaxRecordBytes   = 32 << 10
)

// ProwlarrConfig is trusted composition input, never a public configuration DTO.
// The reference directory is derived from the existing private XDG config root.
// Construction validates locally, without filesystem or network probes.
type ProwlarrConfig struct {
	Enabled            bool   `json:"-"`
	BaseURL            string `json:"-"`
	APIKey             string `json:"-"`
	ReferenceDirectory string `json:"-"`
}

func (ProwlarrConfig) String() string     { return "ProwlarrConfig{values:<private>}" }
func (c ProwlarrConfig) GoString() string { return c.String() }

// Prowlarr implements Discovery ONLY. Lookup reopens provider-owned exact state
// for future trusted consumers, but is deliberately not ExecutionResolver.
type Prowlarr struct {
	config   ProwlarrConfig
	base     *url.URL
	instance string
	client   *http.Client
	// Narrow durability fault seams; production uses real file/directory sync.
	syncFile      func(*os.File) error
	syncDirectory func(*os.File) error
}

var _ Provider = (*Prowlarr)(nil)

func (*Prowlarr) String() string     { return "Prowlarr{configuration:<private>}" }
func (p *Prowlarr) GoString() string { return p.String() }
func (*Prowlarr) ID() string         { return ProwlarrProviderID }
func (*Prowlarr) ContentVerificationTimeouts() ContentVerificationPolicy {
	return ContentVerificationPolicy{Discovery: ProwlarrDiscoveryTimeout}
}

// NormalizeProwlarrBaseURL accepts only HTTP(S) origins with an optional clean
// UrlBase. It rejects embedded credentials, queries, fragments, escaping and
// traversal rather than repairing ambiguous paths. No input appears in errors.
func NormalizeProwlarrBaseURL(value string) (string, error) {
	if value == "" || len(value) > 2048 || strings.TrimSpace(value) != value || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") {
		return "", ErrProvider
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", ErrProvider
		}
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") || u.Opaque != "" || u.RawPath != "" || strings.Contains(value, "%") {
		return "", ErrProvider
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", ErrProvider
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", ErrProvider
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) == nil {
		for _, part := range strings.Split(host, ".") {
			if part == "" || strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
				return "", ErrProvider
			}
			for _, r := range part {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					return "", ErrProvider
				}
			}
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	prefix := strings.TrimSuffix(u.Path, "/")
	if prefix != "" && (prefix[0] != '/' || path.Clean(prefix) != prefix || strings.Contains(prefix, "//")) {
		return "", ErrProvider
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "." || segment == ".." {
			return "", ErrProvider
		}
		for _, r := range segment {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return "", ErrProvider
			}
		}
	}
	u.Path = prefix
	return u.String(), nil
}

func NewProwlarr(config ProwlarrConfig) (*Prowlarr, error) {
	p := &Prowlarr{
		config:        config,
		syncFile:      func(file *os.File) error { return file.Sync() },
		syncDirectory: func(file *os.File) error { return file.Sync() },
	}
	if config.BaseURL != "" {
		normalized, err := NormalizeProwlarrBaseURL(config.BaseURL)
		if err != nil {
			return nil, ErrProvider
		}
		p.config.BaseURL = normalized
		p.base, _ = url.Parse(normalized)
		p.instance = prowlarrHash("instance-v1", []byte(normalized))
	}
	if config.APIKey != "" && !ValidProwlarrAPIKey(config.APIKey) {
		return nil, ErrProvider
	}
	if config.ReferenceDirectory != "" && (!filepath.IsAbs(config.ReferenceDirectory) || filepath.Clean(config.ReferenceDirectory) != config.ReferenceDirectory || config.ReferenceDirectory == "/" || strings.ContainsAny(config.ReferenceDirectory, "\\\x00")) {
		return nil, ErrProvider
	}
	// A transport per adapter has normal TLS verification and no environment
	// proxy credential routing. No retry logic or auxiliary remote probes exist.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// A fresh HTTP/1 connection avoids net/http's transparent retries on a
	// reused idempotent connection or HTTP/2 stream. Search has external cost.
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = ProwlarrDiscoveryTimeout
	p.client = &http.Client{Transport: transport, Timeout: ProwlarrDiscoveryTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return p, nil
}

func ValidProwlarrAPIKey(value string) bool {
	if value == "" || len(value) > 512 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func (p *Prowlarr) configured() bool {
	return p != nil && p.config.Enabled && p.base != nil && ValidProwlarrAPIKey(p.config.APIKey) && p.config.ReferenceDirectory != ""
}
func (p *Prowlarr) Eligible(target Target) bool {
	return p.configured() && target.Work.Medium.Valid() && localIDPattern.MatchString(string(target.Edition.ID)) && localIDPattern.MatchString(string(target.Work.ID)) && target.Edition.WorkID == target.Work.ID && strings.TrimSpace(target.Work.Title) != "" && len(target.Work.Title) <= 4096 && utf8.ValidString(target.Work.Title)
}

// prowlarrQuery runs only after Eligible bounds valid UTF-8 titles to 4096
// bytes. Insert spaces at adjacent Unicode lower/upper boundaries and acronym
// word starts (XMLParser -> XML Parser), preserving all original runes/casing.
// Punctuation, digits and combining marks are not case boundaries; no Unicode
// normalization or aliases are applied. At most one byte is added per boundary.
func prowlarrQuery(title string) string {
	title = strings.TrimSpace(title)
	runes := []rune(title)
	var query strings.Builder
	query.Grow(len(title))
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[i-1]) ||
			(unicode.IsUpper(runes[i-1]) && i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
			query.WriteByte(' ')
		}
		query.WriteRune(r)
	}
	return query.String()
}

func prowlarrCategories(medium domain.Medium) []string {
	switch medium {
	case domain.MediumGame:
		return []string{"1000", "4050"}
	case domain.MediumVideo:
		return []string{"2000", "5000"}
	case domain.MediumLiterature:
		return []string{"7000"}
	case domain.MediumAudio:
		return []string{"3000"}
	default:
		return nil
	}
}

func prowlarrContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	if ctx.Err() != nil {
		return ErrCancelled
	}
	return nil
}

func (p *Prowlarr) Discover(ctx context.Context, target Target) ([]Option, error) {
	if err := prowlarrContextError(ctx); err != nil {
		return nil, err
	}
	if !p.Eligible(target) {
		return nil, ErrProvider
	}
	ctx, cancel := context.WithTimeout(ctx, ProwlarrDiscoveryTimeout)
	defer cancel()
	u := *p.base
	u.Path += "/api/v1/search"
	q := url.Values{"query": {prowlarrQuery(target.Work.Title)}, "type": {"search"}, "limit": {"32"}, "offset": {"0"}, "categories": prowlarrCategories(target.Work.Medium)}
	u.RawQuery = q.Encode() // OpenAPI form/explode: repeat array parameter names.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrProvider
	}
	request.Header.Set("X-Api-Key", p.config.APIKey)
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		if safe := prowlarrContextError(ctx); safe != nil {
			return nil, safe
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, ErrTimeout
		}
		return nil, ErrProvider
	}
	defer response.Body.Close()
	// All non-200 statuses, including every redirect, are safe unavailability.
	// Do not read, wrap, log or copy their payload or authenticate another request.
	if response.StatusCode != http.StatusOK {
		return nil, ErrProvider
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, prowlarrMaxResponseBytes+1))
	if safe := prowlarrContextError(ctx); safe != nil {
		return nil, safe
	}
	if err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, ErrTimeout
		}
		return nil, ErrProvider
	}
	if len(data) > prowlarrMaxResponseBytes {
		return nil, ErrInvalidResponse
	}
	releases, err := decodeProwlarrReleases(data)
	if err != nil {
		return nil, ErrInvalidResponse
	}
	records := make(map[string]prowlarrRecord)
	// Compare the consumed upstream snapshot as well as normalized records. An
	// unsuitable title must not mask conflicting duplicate release metadata.
	seen := make(map[string]prowlarrRelease)
	for _, release := range releases {
		record, err := p.recordForRelease(release)
		if err != nil {
			return nil, ErrInvalidResponse
		}
		id := record.candidate()
		if old, ok := seen[id]; ok {
			if !old.sameOriginalTitle(release) || records[id] != record {
				return nil, ErrInvalidResponse
			}
			continue
		}
		seen[id], records[id] = release, record
	}
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Upstream sometimes ignores limit. Choose the first 32 stable identities
	// only AFTER validating all bounded releases and deduplicating. No ranking,
	// title, language, size, or locator-dependent silent exclusion occurs.
	if len(ids) > MaxProviderCandidates {
		ids = ids[:MaxProviderCandidates]
	}
	options := make([]Option, 0, len(ids))
	if len(ids) == 0 {
		return options, nil
	}
	store, err := p.openRecords(true)
	if err != nil {
		return nil, ErrProvider
	}
	defer store.close()
	for _, id := range ids {
		if safe := prowlarrContextError(ctx); safe != nil {
			return nil, safe
		}
		record := records[id]
		if err := p.saveRecord(ctx, store, record); err != nil {
			return nil, err
		}
		options = append(options, Option{ID: id, Metadata: record.metadata(), ExecutionRef: record.reference()})
	}
	if safe := prowlarrContextError(ctx); safe != nil {
		return nil, safe
	}
	return options, nil
}

func prowlarrHash(domain string, value []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain + "\x00"))
	_, _ = h.Write(value)
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Prowlarr) safeTitle(value, id string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if !validText(value, 256, true) || p.containsKey(value) || strings.Contains(lower, strings.ToLower(p.base.Hostname())) || strings.Contains(lower, strings.ToLower(p.config.BaseURL)) {
		return "Prowlarr release " + id[:12]
	}
	return value
}

func (p *Prowlarr) containsKey(value string) bool {
	// Treat the current key as forbidden in every persisted string, including
	// decoded query fields. Historical records never need rewriting on rotation.
	if p.config.APIKey == "" {
		return false
	}
	for i := 0; i < 3; i++ {
		if strings.Contains(value, p.config.APIKey) {
			return true
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil || decoded == value {
			break
		}
		value = decoded
	}
	return false
}

func (p *Prowlarr) recordForRelease(r prowlarrRelease) (prowlarrRecord, error) {
	if r.IndexerID == nil || *r.IndexerID <= 0 || *r.IndexerID > 2147483647 || r.GUID == nil || *r.GUID == "" || len(*r.GUID) > 8192 || !utf8.ValidString(*r.GUID) || strings.ContainsRune(*r.GUID, '\x00') || p.containsKey(*r.GUID) {
		return prowlarrRecord{}, ErrInvalidResponse
	}
	record := prowlarrRecord{Version: 1, Instance: p.instance, IndexerID: *r.IndexerID, GUID: *r.GUID, Protocol: ProwlarrProtocolUnknown}
	if r.Protocol != nil {
		record.Protocol = *r.Protocol
	}
	if !record.Protocol.valid() {
		return prowlarrRecord{}, ErrInvalidResponse
	}
	if r.Size != nil {
		if *r.Size < 0 || *r.Size > 1<<60 {
			return prowlarrRecord{}, ErrInvalidResponse
		}
		record.HasSize, record.Size = true, *r.Size
	}
	title := ""
	if r.Title != nil {
		title = *r.Title
	}
	record.Title = p.safeTitle(title, record.candidate())
	for _, pair := range []struct {
		value  *string
		target *prowlarrLocator
	}{{r.DownloadURL, &record.Download}, {r.MagnetURL, &record.Magnet}} {
		if pair.value != nil && *pair.value != "" {
			locator, err := p.normalizeLocator(*pair.value, record.IndexerID)
			if err != nil {
				return prowlarrRecord{}, ErrInvalidResponse
			}
			*pair.target = locator
		}
	}
	data := record.bytes()
	if len(data) > prowlarrMaxRecordBytes || p.containsKey(string(data)) {
		return prowlarrRecord{}, ErrInvalidResponse
	}
	return record, nil
}

// Only documented same-origin/same-UrlBase proxy routes are retained. No URL,
// userinfo, query extras or active API key survive into the private record.
func (p *Prowlarr) normalizeLocator(value string, indexer int64) (prowlarrLocator, error) {
	fail := func() (prowlarrLocator, error) { return prowlarrLocator{}, ErrInvalidResponse }
	if len(value) > 16384 || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00\r\n") {
		return fail()
	}
	u, err := url.Parse(value)
	if err != nil || !u.IsAbs() || u.User != nil || u.Fragment != "" || strings.Contains(value, "#") || u.Opaque != "" || u.RawPath != "" {
		return fail()
	}
	origin, err := NormalizeProwlarrBaseURL(u.Scheme + "://" + u.Host)
	if err != nil || origin != p.base.Scheme+"://"+p.base.Host {
		return fail()
	}
	id := strconv.FormatInt(indexer, 10)
	if u.Path != p.base.Path+"/"+id+"/download" && u.Path != p.base.Path+"/api/v1/indexer/"+id+"/download" {
		return fail()
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fail()
	}
	for name, values := range q {
		if (name != "apikey" && name != "link" && name != "file") || len(values) != 1 {
			return fail()
		}
	}
	if values, exists := q["apikey"]; exists && values[0] != p.config.APIKey {
		return fail()
	}
	link := q.Get("link")
	// The official protected link is Base64URL custom AES data. Keep it opaque;
	// validate only bounded transport spelling, never decrypt or follow it.
	if link == "" || len(link) > 12288 {
		return fail()
	}
	for _, r := range link {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '=') {
			return fail()
		}
	}
	file := q.Get("file")
	if len(file) > 2048 || !utf8.ValidString(file) {
		return fail()
	}
	for _, r := range file {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fail()
		}
	}
	locator := prowlarrLocator{Route: u.Path, Link: link, File: file}
	if p.containsKey(locator.Route) || p.containsKey(link) || p.containsKey(file) {
		return fail()
	}
	return locator, nil
}

// The common Candidate remains neutral; optional bounded size uses its existing
// Label. No indexer name, provider GUID, URL or protocol enters Metadata.
func prowlarrLabel(hasSize bool, size int64) string {
	if !hasSize {
		return ""
	}
	return fmt.Sprintf("%d bytes", size)
}
