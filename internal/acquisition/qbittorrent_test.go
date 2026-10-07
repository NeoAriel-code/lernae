package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQBittorrentWaitLifecycle(t *testing.T) {
	for _, mode := range []string{"complete", "complete uploading", "complete queuedUP", "complete forcedUP", "complete stoppedUP", "metadata complete", "error", "missingFiles", "stoppedDL", "unknown", "disappeared", "inconsistent", "total changed", "partial complete", "metadata cancel", "cancel", "report error"} {
		t.Run(mode, func(t *testing.T) {
			hash, tag := strings.Repeat("a", 40), "lernae-"+strings.Repeat("b", 64)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/torrents/info" || r.Method != "GET" || r.URL.Query().Get("hashes") != hash || r.URL.Query().Get("tag") != tag {
					t.Error("unscoped observation/cleanup")
				}
				call := calls.Add(1)
				info := qbTorrentInfo{Hash: hash, Tags: tag, State: "downloading", Size: 4, TotalSize: 4, Completed: 2, AmountLeft: 2, Progress: 0.5}
				if call == 2 {
					info.Completed, info.AmountLeft, info.Progress = 1, 3, 0.25 // recheck regression must not regress persisted progress
				}
				if call >= 3 {
					info.State, info.Completed, info.AmountLeft, info.Progress = "stalledUP", 4, 0, 1
					if strings.HasPrefix(mode, "complete ") {
						info.State = strings.TrimPrefix(mode, "complete ")
					}
				}
				if mode == "metadata complete" && call == 1 {
					info.State, info.TotalSize, info.Size = "metaDL", 0, 0
				}
				switch mode {
				case "error", "missingFiles", "stoppedDL", "unknown":
					info.State = mode
				case "disappeared":
					_, _ = io.WriteString(w, "[]")
					return
				case "inconsistent":
					info.AmountLeft = 4
				case "total changed":
					info.TotalSize, info.Size = 8, 8
				case "partial complete":
					info.State = "uploading"
				case "metadata cancel":
					info.State, info.TotalSize, info.Size = "metaDL", 0, 0
				case "cancel":
					info.State, info.Completed, info.AmountLeft, info.Progress = "downloading", 2, 2, 0.5
				}
				_ = json.NewEncoder(w).Encode([]qbTorrentInfo{info})
			}))
			defer server.Close()
			e := &qbExecution{session: newQBSession(server.URL), hash: hash, tag: tag, total: 4, interval: time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var got []Progress
			if mode == "metadata complete" {
				e.total = 0
			}
			if mode == "metadata cancel" {
				e.total = 0
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				e.interval = 20 * time.Millisecond // real metadata polling without fabricated progress
			}
			completion, err := e.Wait(ctx, func(p Progress) error {
				got = append(got, p)
				if mode == "cancel" {
					cancel()
				}
				if mode == "report error" {
					return ErrUnavailable
				}
				return nil
			})
			if mode == "metadata complete" {
				if err != nil || completion != CompletionSucceeded || len(got) != 2 || got[0] != (Progress{Current: 1, Total: 4}) || got[1] != (Progress{Current: 4, Total: 4}) {
					t.Fatal("metadata wait fabricated progress or failed to freeze total")
				}
			} else if mode == "complete" || strings.HasPrefix(mode, "complete ") {
				if err != nil || completion != CompletionSucceeded || len(got) != 2 || got[0] != (Progress{Current: 2, Total: 4}) || got[1] != (Progress{Current: 4, Total: 4}) {
					t.Fatal("completion/progress was not fixed, monotonic and coalesced")
				}
			} else if mode == "cancel" || mode == "metadata cancel" {
				if completion != CompletionInterrupted || err == nil || (mode == "metadata cancel" && len(got) != 0) {
					t.Fatal("cancellation not observed")
				}
			} else if completion != CompletionFailed || err == nil {
				t.Fatal("invalid/terminal observation not failed")
			}
			if mode == "inconsistent" && (calls.Load() != 1 || len(got) != 0) {
				t.Fatal("inconsistent bytes emitted progress or continued polling")
			}
		})
	}
}

func TestQBittorrentHTTPBoundsRedirectTLSAndTimeout(t *testing.T) {
	for _, mode := range []string{"oversize", "redirect", "unavailable", "login timeout", "add timeout", "TLS"} {
		t.Run(mode, func(t *testing.T) {
			var adds, calls atomic.Int32
			release := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "redirect" {
					w.Header().Set("Location", "/must-not-follow")
					w.WriteHeader(302)
					return
				}
				if mode == "oversize" {
					_, _ = io.WriteString(w, strings.Repeat("x", qbResponseLimit+1))
					return
				}
				if mode == "unavailable" {
					w.WriteHeader(503)
					_, _ = io.WriteString(w, "PRIVATE_REMOTE_ERROR")
					return
				}
				switch r.URL.Path {
				case "/api/v2/auth/login":
					if mode == "login timeout" {
						<-release
						return
					}
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "private", Path: "/"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/app/version":
					_, _ = io.WriteString(w, "v5.0.0")
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.2")
				case "/api/v2/torrents/add":
					adds.Add(1)
					<-release
				default:
					t.Error("unexpected request/redirect/retry")
				}
			})
			server := httptest.NewServer(handler)
			if mode == "TLS" {
				server.Close()
				server = httptest.NewTLSServer(handler)
			}
			defer server.Close()
			pServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(testTorrent()) }))
			defer pServer.Close()
			p := newTestProwlarr(t, pServer.URL)
			selection := storedProwlarrSelection(t, p, nil)
			q, err := NewQBittorrent(QBittorrentConfig{Enabled: true, BaseURL: server.URL, Username: "operator", Password: "PRIVATE_PASSWORD"}, p)
			if err != nil {
				t.Fatal(err)
			}
			plan := PlanForSelection(selection)
			plan.ExecutionID = strings.Repeat("e", 64)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			result, err := q.Dispatch(ctx, plan)
			close(release) // fixture handlers are joined by Close, never detached work
			want := DispatchRejected
			if mode == "add timeout" {
				want = DispatchUnconfirmed
			}
			if err != nil || result.Decision != want || result.Execution != nil || adds.Load() > 1 {
				t.Fatal("unsafe HTTP outcome or add retry")
			}
			if mode == "redirect" && calls.Load() != 1 {
				t.Fatal("HTTP redirect followed")
			}
			transport := newQBSession(server.URL).client.Transport.(*http.Transport)
			if transport.Proxy != nil || (transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify) || !transport.DisableKeepAlives {
				t.Fatal("proxy/TLS/transparent retry policy unsafe")
			}
		})
	}
}

func TestQBittorrentDuplicateJSONCannotConfirmOwnership(t *testing.T) {
	hash, tag := strings.Repeat("a", 40), "lernae-test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"hash":"wrong","hash":%q,"tags":%q,"state":"downloading","total_size":4,"size":4,"completed":0,"amount_left":4,"progress":0}]`, hash, tag)
	}))
	defer server.Close()
	if _, exists, err := newQBSession(server.URL).ownedInfo(context.Background(), hash, tag); err == nil || exists {
		t.Fatal("ambiguous duplicate field confirmed ownership")
	}
}

func TestQBittorrentAcceptanceRequiresAddAndExactOwnership(t *testing.T) {
	for _, mode := range []string{"owned", "owned magnet", "login fails", "login no cookie", "add fails", "add malformed", "empty", "wrong hash", "wrong tag", "multiple", "malformed", "version"} {
		t.Run(mode, func(t *testing.T) {
			var adds atomic.Int32
			payload, _ := parseTorrent(testTorrent())
			tag := "lernae-" + strings.Repeat("e", 64)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Origin") == "" || r.Header.Get("Referer") == "" {
					t.Error("missing same-origin security headers")
				}
				switch r.URL.Path {
				case "/api/v2/auth/login":
					_ = r.ParseForm()
					if r.Form.Get("username") != "operator" || r.Form.Get("password") != "PRIVATE_PASSWORD" {
						t.Error("wrong credentials")
					}
					if mode == "login fails" {
						_, _ = io.WriteString(w, "Fails.")
						return
					}
					if mode != "login no cookie" {
						http.SetCookie(w, &http.Cookie{Name: "CustomSID", Value: "PRIVATE_COOKIE", Path: "/", HttpOnly: true})
					}
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/app/version":
					if mode == "version" {
						_, _ = io.WriteString(w, "v5.1.0")
					} else {
						_, _ = io.WriteString(w, "v5.0.0")
					}
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.2")
				case "/api/v2/torrents/add":
					adds.Add(1)
					cookie, err := r.Cookie("CustomSID")
					if err != nil || cookie.Value != "PRIVATE_COOKIE" {
						t.Error("missing configured-name session cookie")
					}
					mr, err := r.MultipartReader()
					if err != nil {
						t.Error("not multipart")
						return
					}
					fields := map[string]string{}
					for {
						part, err := mr.NextPart()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Error(err)
							return
						}
						data, _ := io.ReadAll(part)
						fields[part.FormName()] = string(data)
					}
					validPayload := fields["torrents"] == string(testTorrent()) && fields["urls"] == ""
					if mode == "owned magnet" {
						validPayload = fields["urls"] == "magnet:?xt=urn:btih:"+payload.hash && fields["torrents"] == ""
					}
					if fields["tags"] != tag || !validPayload || fields["stopped"] != "false" || len(fields) != 3 {
						t.Error("substituted torrent/tag or unsafe add fields")
					}
					if mode == "add malformed" {
						_, _ = io.WriteString(w, "PRIVATE_REMOTE_ERROR")
					} else if mode == "add fails" {
						_, _ = io.WriteString(w, "Fails.")
					} else {
						_, _ = io.WriteString(w, "Ok.")
					}
				case "/api/v2/torrents/info":
					if r.URL.Query().Get("hashes") != payload.hash || r.URL.Query().Get("tag") != tag {
						t.Error("unscoped correlation")
					}
					if mode == "empty" {
						_, _ = io.WriteString(w, "[]")
						return
					}
					info := qbTorrentInfo{Hash: payload.hash, Tags: tag, State: "downloading", TotalSize: 4, Size: 4, AmountLeft: 4}
					if mode == "wrong hash" {
						info.Hash = strings.Repeat("f", 40)
					}
					if mode == "wrong tag" {
						info.Tags = "untagged"
					}
					if mode == "malformed" {
						_, _ = io.WriteString(w, "private raw error")
						return
					}
					infos := []qbTorrentInfo{info}
					if mode == "multiple" {
						infos = append(infos, info)
					}
					_ = json.NewEncoder(w).Encode(infos)
				default:
					t.Error("unexpected request")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			pServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "owned magnet" {
					w.Header().Set("Location", "magnet:?xt=urn:btih:"+payload.hash)
					w.WriteHeader(301)
				} else {
					_, _ = w.Write(testTorrent())
				}
			}))
			defer pServer.Close()
			p := newTestProwlarr(t, pServer.URL)
			selection := storedProwlarrSelection(t, p, nil)
			plan := PlanForSelection(selection)
			plan.ExecutionID = strings.Repeat("e", 64)
			q, err := NewQBittorrent(QBittorrentConfig{Enabled: true, BaseURL: server.URL, Username: "operator", Password: "PRIVATE_PASSWORD"}, p)
			if err != nil {
				t.Fatal(err)
			}
			q.pollInterval = time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			result, err := q.Dispatch(ctx, plan)
			if mode == "owned" || mode == "owned magnet" {
				if err != nil || result.Decision != DispatchAccepted || result.Execution == nil {
					t.Fatal("actual add+ownership not accepted")
				}
			} else if mode == "login fails" || mode == "login no cookie" || mode == "version" {
				if err != nil || result.Decision != DispatchRejected || adds.Load() != 0 {
					t.Fatal("preadd error not effect-free rejection")
				}
			} else if result.Decision != DispatchUnconfirmed || result.Execution != nil {
				t.Fatal("ambiguous add/correlation not fail closed")
			}
			if adds.Load() > 1 {
				t.Fatal("add retried")
			}
		})
	}
}
