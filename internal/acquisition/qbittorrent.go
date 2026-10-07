package acquisition

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	qbResponseLimit   = 256 << 10
	qbRequestTimeout  = 15 * time.Second
	qbDispatchTimeout = 90 * time.Second
	qbWaitTimeout     = 24 * time.Hour
)

// All endpoint/session/payload fields are trusted private composition input.
type QBittorrentConfig struct {
	Enabled  bool   `json:"-"`
	BaseURL  string `json:"-"`
	Username string `json:"-"`
	Password string `json:"-"`
}

func (QBittorrentConfig) String() string     { return "QBittorrentConfig{values:<private>}" }
func (c QBittorrentConfig) GoString() string { return c.String() }

func ValidQBittorrentCredential(value string) bool {
	if strings.TrimSpace(value) == "" || len(value) > 1024 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func NormalizeQBittorrentBaseURL(value string) (string, error) {
	return NormalizeProwlarrBaseURL(value)
}

// QBittorrent has no startup probes, background work, retries, or disk state.
// Each claimed dispatch has its own private session (safe under concurrency).
type QBittorrent struct {
	config       QBittorrentConfig
	provider     *Prowlarr
	pollInterval time.Duration
}

func (*QBittorrent) String() string     { return "QBittorrent{values:<private>}" }
func (q *QBittorrent) GoString() string { return q.String() }

func NewQBittorrent(config QBittorrentConfig, provider *Prowlarr) (*QBittorrent, error) {
	if config.BaseURL != "" {
		base, err := NormalizeQBittorrentBaseURL(config.BaseURL)
		if err != nil {
			return nil, ErrExecutor
		}
		config.BaseURL = base
	}
	if (config.Username != "" && !ValidQBittorrentCredential(config.Username)) || (config.Password != "" && !ValidQBittorrentCredential(config.Password)) {
		return nil, ErrExecutor
	}
	return &QBittorrent{config: config, provider: provider, pollInterval: 2 * time.Second}, nil
}

func (q *QBittorrent) Configured() bool {
	return q != nil && q.config.Enabled && q.config.BaseURL != "" && ValidQBittorrentCredential(q.config.Username) && ValidQBittorrentCredential(q.config.Password) && q.provider.configured()
}

func (*QBittorrent) ContentVerificationTimeouts() ContentVerificationPolicy {
	return ContentVerificationPolicy{Dispatch: qbDispatchTimeout}
}

type qbSession struct {
	client *http.Client
	base   string
	origin string
}

func (qbSession) String() string     { return "qbSession{values:<private>}" }
func (s qbSession) GoString() string { return s.String() }

func newQBSession(base string) *qbSession {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.DialContext = (&net.Dialer{Timeout: qbRequestTimeout}).DialContext
	transport.TLSHandshakeTimeout = qbRequestTimeout
	transport.ResponseHeaderTimeout = qbRequestTimeout
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(base)
	return &qbSession{base: base, origin: u.Scheme + "://" + u.Host, client: &http.Client{Transport: transport, Jar: jar, Timeout: qbRequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (s *qbSession) request(ctx context.Context, method, route, contentType string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, s.base+"/api/v2/"+route, bytes.NewReader(body))
	if err != nil {
		return nil, ErrExecutor
	}
	request.Header.Set("Origin", s.origin)
	request.Header.Set("Referer", s.base+"/")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, ErrExecutor
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > qbResponseLimit {
		return nil, ErrInvalidResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, qbResponseLimit+1))
	if err != nil || len(data) > qbResponseLimit {
		return nil, ErrInvalidResponse
	}
	return data, nil
}

func (q *QBittorrent) authenticate(ctx context.Context, s *qbSession) error {
	data, err := s.request(ctx, http.MethodPost, "auth/login", "application/x-www-form-urlencoded", []byte(url.Values{"username": {q.config.Username}, "password": {q.config.Password}}.Encode()))
	if err != nil || string(data) != "Ok." {
		return ErrExecutor
	}
	u, _ := url.Parse(s.base + "/api/v2/torrents/info")
	if len(s.client.Jar.Cookies(u)) != 1 || s.client.Jar.Cookies(u)[0].Value == "" {
		return ErrExecutor // SID's configured name is not hardcoded
	}
	// Source-envelope enforcement is execute-time, never a startup dependency.
	version, err := s.request(ctx, http.MethodGet, "app/version", "", nil)
	if err != nil || string(version) != "v5.0.0" {
		return ErrExecutor
	}
	apiVersion, err := s.request(ctx, http.MethodGet, "app/webapiVersion", "", nil)
	if err != nil || string(apiVersion) != "2.11.2" {
		return ErrExecutor
	}
	return nil
}

func (q *QBittorrent) Dispatch(ctx context.Context, plan ExecutionPlan) (DispatchResult, error) {
	reject := func() (DispatchResult, error) { return DispatchResult{Decision: DispatchRejected}, nil }
	ambiguous := func() (DispatchResult, error) { return DispatchResult{Decision: DispatchUnconfirmed}, nil }
	if !q.Configured() || !handlePattern.MatchString(plan.ExecutionID) || ctx.Err() != nil {
		return reject()
	}
	ctx, cancel := context.WithTimeout(ctx, qbDispatchTimeout)
	defer cancel()
	// This exact remote fetch can emit Prowlarr accounting events. It is AFTER
	// the core's claim even if qBit later rejects. Never fallback or retry.
	payload, err := q.provider.fetchTorrent(ctx, plan)
	if err != nil {
		return reject() // no qBit add; the already reserved claim remains consumed
	}
	session := newQBSession(q.config.BaseURL)
	if err := q.authenticate(ctx, session); err != nil {
		return reject()
	}
	tag := "lernae-" + plan.ExecutionID
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if writer.WriteField("tags", tag) != nil || writer.WriteField("stopped", "false") != nil {
		return reject()
	}
	if payload.magnet != "" {
		if writer.WriteField("urls", payload.magnet) != nil {
			return reject()
		}
	} else {
		part, err := writer.CreateFormFile("torrents", "selection.torrent")
		if err != nil {
			return reject()
		}
		if _, err := part.Write(payload.torrent); err != nil {
			return reject()
		}
	}
	if writer.Close() != nil {
		return reject()
	}
	// Once add is attempted, all errors (including Fails./415) are treated as
	// potentially effectful. No tagging/adoption of duplicates, no second add.
	data, err := session.request(ctx, http.MethodPost, "torrents/add", writer.FormDataContentType(), body.Bytes())
	if err != nil || string(data) != "Ok." {
		return ambiguous()
	}
	for {
		_, exists, err := session.ownedInfo(ctx, payload.hash, tag)
		if err != nil {
			return ambiguous()
		}
		if exists {
			return DispatchResult{Decision: DispatchAccepted, Execution: &qbExecution{session: session, hash: payload.hash, tag: tag, total: payload.total, interval: q.pollInterval}}, nil
		}
		if !qbDelay(ctx, q.pollInterval) {
			return ambiguous()
		}
	}
}

type qbTorrentInfo struct {
	Hash       string  `json:"hash"`
	Tags       string  `json:"tags"`
	State      string  `json:"state"`
	TotalSize  int64   `json:"total_size"`
	Size       int64   `json:"size"`
	Completed  int64   `json:"completed"`
	AmountLeft int64   `json:"amount_left"`
	Progress   float64 `json:"progress"`
}

func (qbTorrentInfo) String() string     { return "qbTorrentInfo{values:<private>}" }
func (i qbTorrentInfo) GoString() string { return i.String() }

func (s *qbSession) ownedInfo(ctx context.Context, hash, tag string) (qbTorrentInfo, bool, error) {
	query := url.Values{"hashes": {hash}, "tag": {tag}}.Encode()
	data, err := s.request(ctx, http.MethodGet, "torrents/info?"+query, "", nil)
	if err != nil {
		return qbTorrentInfo{}, false, err
	}
	var documents []map[string]json.RawMessage
	if !utf8.Valid(data) || strictProwlarrJSON(data) != nil || !prowlarrUnicodeEscapes(data) || json.Unmarshal(data, &documents) != nil || documents == nil || len(documents) > 1 {
		return qbTorrentInfo{}, false, ErrInvalidResponse
	}
	if len(documents) == 0 {
		return qbTorrentInfo{}, false, nil
	}
	for _, key := range []string{"hash", "tags", "state", "total_size", "size", "completed", "amount_left", "progress"} {
		if raw := documents[0][key]; len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return qbTorrentInfo{}, false, ErrInvalidResponse
		}
		for name := range documents[0] {
			if name != key && strings.EqualFold(name, key) {
				return qbTorrentInfo{}, false, ErrInvalidResponse
			}
		}
	}
	var infos []qbTorrentInfo
	if json.Unmarshal(data, &infos) != nil || infos[0].Hash != hash {
		return qbTorrentInfo{}, false, ErrInvalidResponse
	}
	matches := 0
	for _, value := range strings.Split(infos[0].Tags, ",") {
		if strings.TrimSpace(value) == tag {
			matches++
		}
	}
	if matches != 1 {
		return qbTorrentInfo{}, false, ErrInvalidResponse
	}
	return infos[0], true, nil
}

type qbExecution struct {
	session  *qbSession
	hash     string
	tag      string
	total    int64
	interval time.Duration
}

func (*qbExecution) String() string     { return "qbExecution{values:<private>}" }
func (e *qbExecution) GoString() string { return e.String() }

func qbDelay(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (e *qbExecution) Wait(ctx context.Context, report func(Progress) error) (Completion, error) {
	ctx, cancel := context.WithTimeout(ctx, qbWaitTimeout)
	defer cancel()
	var current int64
	last := Progress{Current: -1}
	for {
		if ctx.Err() != nil {
			return CompletionInterrupted, ErrCancelled
		}
		info, exists, err := e.session.ownedInfo(ctx, e.hash, e.tag)
		if err != nil || !exists {
			if ctx.Err() != nil {
				return CompletionInterrupted, ErrCancelled
			}
			return CompletionFailed, ErrExecutor
		}
		success := false
		switch info.State {
		case "metaDL", "forcedMetaDL":
			// Unknown metadata size is not fabricated progress.
			if !qbDelay(ctx, e.interval) {
				return CompletionInterrupted, ErrCancelled
			}
			continue
		case "uploading", "stalledUP", "queuedUP", "forcedUP", "stoppedUP":
			success = true
		case "downloading", "forcedDL", "stalledDL", "queuedDL", "checkingDL", "checkingUP", "checkingResumeData", "moving":
		default:
			return CompletionFailed, ErrExecutor // error, missingFiles, stoppedDL, unknown
		}
		if info.TotalSize <= 0 || info.TotalSize > 1<<60 || info.Size != info.TotalSize || info.Completed < 0 || info.Completed > info.TotalSize || info.AmountLeft < 0 || info.AmountLeft > info.TotalSize || info.Completed+info.AmountLeft != info.TotalSize || info.Progress < 0 || info.Progress > 1 {
			return CompletionFailed, ErrInvalidResponse
		}
		if e.total == 0 {
			e.total = info.TotalSize // magnet total freezes only after metadata
		}
		if info.TotalSize != e.total || (success && (info.Completed != e.total || info.AmountLeft != 0 || info.Progress != 1)) {
			return CompletionFailed, ErrInvalidResponse
		}
		if info.Completed > current {
			current = info.Completed
		}
		// Remote checking/moving/full-byte ticks are not terminal proof. Reserve
		// the final byte for verified completion; emit only changed byte values.
		value := current
		if !success && value == e.total {
			value--
		}
		progress := Progress{Current: value, Total: e.total}
		if progress != last {
			if report == nil || report(progress) != nil {
				return CompletionFailed, ErrExecutor
			}
			last = progress
		}
		if success {
			return CompletionSucceeded, nil
		}
		if !qbDelay(ctx, e.interval) {
			return CompletionInterrupted, ErrCancelled
		}
	}
}
