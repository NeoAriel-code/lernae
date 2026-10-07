package acquisition

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
)

func testTorrent() []byte {
	return []byte("d4:infod6:lengthi4e4:name4:test12:piece lengthi16384e6:pieces20:12345678901234567890ee")
}

func TestTorrentRawInfoAuthorityAndBounds(t *testing.T) {
	data := testTorrent()
	payload, err := parseTorrent(data)
	start := bytes.Index(data, []byte("d6:length"))
	hash := sha1.Sum(data[start : len(data)-1])
	if err != nil || payload.hash != hex.EncodeToString(hash[:]) || payload.total != 4 || !bytes.Equal(payload.torrent, data) {
		t.Fatal("valid v1 exact raw info was not preserved")
	}
	for _, data := range [][]byte{
		nil, []byte("de"), append(testTorrent(), 'x'),
		[]byte(strings.Repeat("l", 18) + strings.Repeat("e", 18)),
		bytes.Replace(testTorrent(), []byte("4:test"), []byte("2:.."), 1),
		bytes.Replace(testTorrent(), []byte("i4e"), []byte("i-4e"), 1),
		bytes.Replace(testTorrent(), []byte("20:12345678901234567890"), []byte("1:x"), 1),
		bytes.Replace(testTorrent(), []byte("4:name"), []byte("12:meta versioni2e4:name"), 1),
		bytes.Replace(testTorrent(), []byte("12:piece length"), []byte("10:name.utf-82:..12:piece length"), 1),
		bytes.Replace(testTorrent(), []byte("6:pieces"), []byte("7:privatei2e6:pieces"), 1),
		[]byte("d4:infod5:filesld6:lengthi4e4:pathl1:aeeed6:lengthi4e4:pathl1:aeeee4:name4:test12:piece lengthi16384e6:pieces20:12345678901234567890ee"),
		bytes.Repeat([]byte("x"), torrentMaxBytes+1),
	} {
		if _, err := parseTorrent(data); err != ErrInvalidResponse {
			t.Fatal("invalid/unsupported torrent accepted or unsafe error")
		}
	}
}

func TestMagnetNarrowAuthority(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, value := range []string{
		"magnet:?xt=urn:btih:" + hash,
		"magnet:?xt=urn:btih:" + hash + "&tr=https%3A%2F%2Ftracker.invalid%2Fannounce",
	} {
		payload, err := parseMagnet(value)
		if err != nil || payload.hash != hash || payload.magnet != value {
			t.Fatal("exact supported magnet rejected")
		}
	}
	for _, value := range []string{
		"https://private.invalid/torrent", "magnet:?xt=urn:btmh:1220" + hash,
		"magnet:?xt=urn:btih:" + hash + "&xt=urn:btih:" + hash,
		"magnet:?xt=urn:btih:" + hash + "&dn=private", "magnet:?xt=urn:btih:" + hash + "&tr=file:///secret",
	} {
		if _, err := parseMagnet(value); err != ErrInvalidResponse {
			t.Fatal("unsupported magnet accepted")
		}
	}
}
