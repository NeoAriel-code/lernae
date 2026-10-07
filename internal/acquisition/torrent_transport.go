package acquisition

import (
	"bytes"
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const torrentMaxBytes = 4 << 20

// Payloads and their remote identity exist only in private adapter memory.
type torrentPayload struct {
	torrent []byte
	magnet  string
	hash    string
	total   int64
}

func (torrentPayload) String() string     { return "torrentPayload{values:<private>}" }
func (p torrentPayload) GoString() string { return p.String() }

// A small bounded bencode reader retains RAW spans: hashing a re-encoded info
// dictionary could silently change the selected torrent's identity.
type torrentNode struct {
	kind       byte
	start, end int
	text       string
	number     int64
	list       []torrentNode
	dict       map[string]torrentNode
}

type torrentParser struct {
	data  []byte
	pos   int
	nodes int
}

func (p *torrentParser) node(depth int) (torrentNode, error) {
	fail := func() (torrentNode, error) { return torrentNode{}, ErrInvalidResponse }
	p.nodes++
	if depth > 16 || p.nodes > 8192 || p.pos >= len(p.data) {
		return fail()
	}
	n := torrentNode{start: p.pos, kind: p.data[p.pos]}
	switch n.kind {
	case 'i':
		p.pos++
		end := bytes.IndexByte(p.data[p.pos:], 'e')
		if end < 1 || end > 20 {
			return fail()
		}
		value := string(p.data[p.pos : p.pos+end])
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil || strconv.FormatInt(v, 10) != value {
			return fail()
		}
		n.number = v
		p.pos += end + 1
	case 'l', 'd':
		p.pos++
		if n.kind == 'd' {
			n.dict = make(map[string]torrentNode)
		}
		previous := ""
		for p.pos < len(p.data) && p.data[p.pos] != 'e' {
			key := ""
			if n.kind == 'd' {
				k, err := p.node(depth + 1)
				if err != nil || k.kind != 's' || k.text == "" || (previous != "" && k.text <= previous) {
					return fail()
				}
				key, previous = k.text, k.text
			}
			child, err := p.node(depth + 1)
			if err != nil {
				return fail()
			}
			if n.kind == 'd' {
				n.dict[key] = child
			} else {
				n.list = append(n.list, child)
			}
		}
		if p.pos >= len(p.data) {
			return fail()
		}
		p.pos++
	default:
		colon := bytes.IndexByte(p.data[p.pos:], ':')
		if colon < 1 || colon > 8 {
			return fail()
		}
		value := string(p.data[p.pos : p.pos+colon])
		size, err := strconv.Atoi(value)
		if err != nil || size < 0 || strconv.Itoa(size) != value || size > len(p.data)-p.pos-colon-1 {
			return fail()
		}
		p.pos += colon + 1
		n.kind, n.text = 's', string(p.data[p.pos:p.pos+size])
		p.pos += size
	}
	n.end = p.pos
	return n, nil
}

func torrentPathPart(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 255 || !utf8.ValidString(value) || strings.ContainsAny(value, "/\\:") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func parseTorrent(data []byte) (torrentPayload, error) {
	fail := func() (torrentPayload, error) { return torrentPayload{}, ErrInvalidResponse }
	if len(data) == 0 || len(data) > torrentMaxBytes {
		return fail()
	}
	parser := torrentParser{data: data}
	root, err := parser.node(0)
	if err != nil || root.kind != 'd' || parser.pos != len(data) {
		return fail()
	}
	info := root.dict["info"]
	if info.kind != 'd' || info.dict["meta version"].kind != 0 || root.dict["piece layers"].kind != 0 || info.dict["file tree"].kind != 0 {
		return fail() // BEP52 v2/hybrid deliberately unsupported
	}
	if info.dict["name.utf-8"].kind != 0 || (info.dict["private"].kind != 0 && (info.dict["private"].kind != 'i' || (info.dict["private"].number != 0 && info.dict["private"].number != 1))) {
		return fail() // alternate name/path authority is outside this envelope
	}
	name, pieceLength, pieces := info.dict["name"], info.dict["piece length"], info.dict["pieces"]
	if name.kind != 's' || !torrentPathPart(name.text) || pieceLength.kind != 'i' || pieceLength.number < 16384 || pieceLength.number > 16<<20 || pieceLength.number&(pieceLength.number-1) != 0 || pieces.kind != 's' {
		return fail()
	}
	var total int64
	files, length := info.dict["files"], info.dict["length"]
	if files.kind == 0 {
		if length.kind != 'i' || length.number <= 0 || length.number > 1<<60 {
			return fail()
		}
		total = length.number
	} else {
		if files.kind != 'l' || len(files.list) == 0 || len(files.list) > 1024 || length.kind != 0 {
			return fail()
		}
		paths := make(map[string]bool)
		for _, file := range files.list {
			length, path := file.dict["length"], file.dict["path"]
			if file.kind != 'd' || length.kind != 'i' || length.number < 0 || length.number > 1<<60 || path.kind != 'l' || len(path.list) == 0 || len(path.list) > 16 || file.dict["attr"].kind != 0 || file.dict["symlink path"].kind != 0 || file.dict["path.utf-8"].kind != 0 {
				return fail()
			}
			parts := make([]string, 0, len(path.list))
			for _, part := range path.list {
				if part.kind != 's' || !torrentPathPart(part.text) {
					return fail()
				}
				parts = append(parts, part.text)
			}
			key := strings.Join(parts, "/")
			for existing := range paths {
				if strings.HasPrefix(existing, key+"/") || strings.HasPrefix(key, existing+"/") {
					return fail()
				}
			}
			if paths[key] || total > (1<<60)-length.number {
				return fail()
			}
			paths[key] = true
			total += length.number
		}
	}
	if total <= 0 || int64(len(pieces.text)) != ((total+pieceLength.number-1)/pieceLength.number)*20 || info.dict["attr"].kind != 0 || info.dict["symlink path"].kind != 0 {
		return fail()
	}
	hash := sha1.Sum(data[info.start:info.end])
	return torrentPayload{torrent: data, hash: hex.EncodeToString(hash[:]), total: total}, nil
}

// Narrow BEP9 transport: exactly one v1 btih (hex or unpadded base32), optional
// bounded HTTP(S) trackers only. No btmh/hybrid, display names, web seeds, peers,
// select-only files or arbitrary extension parameters. Preserve exact input.
func parseMagnet(value string) (torrentPayload, error) {
	fail := func() (torrentPayload, error) { return torrentPayload{}, ErrInvalidResponse }
	if len(value) > 16384 || strings.ContainsAny(value, "\r\n\x00#") {
		return fail()
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "magnet" || u.Host != "" || u.Path != "" || u.Opaque != "" || u.Fragment != "" || u.User != nil {
		return fail()
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["xt"]) != 1 || !strings.HasPrefix(q.Get("xt"), "urn:btih:") {
		return fail()
	}
	for key := range q {
		if key != "xt" && key != "tr" {
			return fail()
		}
	}
	hash := strings.TrimPrefix(q.Get("xt"), "urn:btih:")
	var raw []byte
	if len(hash) == 40 {
		raw, err = hex.DecodeString(hash)
	} else if len(hash) == 32 {
		raw, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(hash))
	} else {
		return fail()
	}
	if err != nil || len(raw) != 20 || len(q["tr"]) > 16 {
		return fail()
	}
	for _, tracker := range q["tr"] {
		u, err := url.Parse(tracker)
		if err != nil || len(tracker) > 2048 || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(tracker, "\r\n\x00\\") {
			return fail()
		}
	}
	return torrentPayload{magnet: value, hash: hex.EncodeToString(raw)}, nil
}
