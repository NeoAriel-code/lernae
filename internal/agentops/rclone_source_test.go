package agentops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRcloneSourceLocator(t *testing.T) {
	tests := []struct {
		name       string
		locator    string
		wantRemote string
		wantPath   string
		wantError  bool
	}{
		{name: "exact remote object", locator: "archive:games/game.iso", wantRemote: "archive", wantPath: "games/game.iso"},
		{name: "spaces and unicode stay data", locator: "archive:games/東京 game.iso", wantRemote: "archive", wantPath: "games/東京 game.iso"},
		{name: "empty locator", locator: "", wantError: true},
		{name: "missing remote", locator: ":games/game.iso", wantError: true},
		{name: "missing object path", locator: "archive:", wantError: true},
		{name: "option-like remote", locator: "--config:games/game.iso", wantError: true},
		{name: "dash-prefixed remote", locator: "-archive:games/game.iso", wantError: true},
		{name: "malformed remote", locator: "bad remote:games/game.iso", wantError: true},
		{name: "absolute object path", locator: "archive:/games/game.iso", wantError: true},
		{name: "directory suffix", locator: "archive:games/", wantError: true},
		{name: "current directory target", locator: "archive:.", wantError: true},
		{name: "parent traversal segment", locator: "archive:games/../game.iso", wantError: true},
		{name: "empty path segment", locator: "archive:games//game.iso", wantError: true},
		{name: "backslash separator", locator: `archive:games\game.iso`, wantError: true},
		{name: "NUL control", locator: "archive:games/\x00game.iso", wantError: true},
		{name: "newline control", locator: "archive:games/\ngame.iso", wantError: true},
		{name: "invalid UTF-8", locator: "archive:games/" + string([]byte{0xff}), wantError: true},
		{name: "oversized locator", locator: "archive:" + strings.Repeat("a", maxRcloneLocatorBytes), wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseRcloneSourceLocator(test.locator)
			if test.wantError {
				assertRcloneCategory(t, err, RcloneFailureInvalidLocator)
				return
			}
			if err != nil {
				t.Fatalf("ParseRcloneSourceLocator() error = %v", err)
			}
			if got.RemoteName() != test.wantRemote || got.ObjectPath() != test.wantPath || got.Locator() != test.locator {
				t.Fatalf("parsed source = (%q, %q, %q), want (%q, %q, %q)", got.RemoteName(), got.ObjectPath(), got.Locator(), test.wantRemote, test.wantPath, test.locator)
			}
		})
	}
}

func TestRcloneStatArgumentsKeepLocatorAsOneDataArgument(t *testing.T) {
	const locator = "archive:games/--deletefile; echo marker.iso"
	source, err := ParseRcloneSourceLocator(locator)
	if err != nil {
		t.Fatalf("ParseRcloneSourceLocator() error = %v", err)
	}

	got, err := BuildRcloneStatArgs(source)
	if err != nil {
		t.Fatalf("BuildRcloneStatArgs() error = %v", err)
	}
	want := []string{"lsjson", "--stat", locator}
	if len(got) != len(want) {
		t.Fatalf("stat args = %#v, want exactly %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("stat args = %#v, want exactly %#v", got, want)
		}
	}
	if got[0] != "lsjson" {
		t.Fatalf("stat subcommand = %q, want allowlisted lsjson", got[0])
	}
	for _, arg := range got {
		if arg == "deletefile" || arg == "purge" || arg == "rmdir" || arg == "rcat" {
			t.Fatalf("stat args exposed a destructive or write subcommand: %#v", got)
		}
	}
}

func TestRcloneStatArgumentsRejectUnparsedOrForgedSource(t *testing.T) {
	for name, source := range map[string]RcloneSourceDescriptor{
		"zero value":         {},
		"option-like remote": {remoteName: "--config", objectPath: "archive/game.iso"},
		"directory target":   {remoteName: "archive", objectPath: "games/"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildRcloneStatArgs(source); err == nil {
				t.Fatal("invalid source unexpectedly produced stat arguments")
			}
		})
	}
}

func TestResolveRcloneExecutableUsesOnlyTrustedNameLookup(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)

	if _, err := ResolveRcloneExecutable(); err == nil {
		t.Fatal("missing rclone executable unexpectedly resolved")
	} else {
		assertRcloneCategory(t, err, RcloneFailureExecutableUnavailable)
	}

	executable := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(executable, []byte("test-only placeholder"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRcloneExecutable()
	if err != nil {
		t.Fatalf("ResolveRcloneExecutable() error = %v", err)
	}
	if got != executable {
		t.Fatalf("resolved executable = %q, want %q", got, executable)
	}
}

func TestParseRcloneStatJSONValidatesRegularKnownMatchingSize(t *testing.T) {
	got, err := ParseRcloneStatJSON([]byte(`{"Path":"games/game.iso","Name":"game.iso","Size":64,"IsDir":false}`), 64)
	if err != nil {
		t.Fatalf("ParseRcloneStatJSON() error = %v", err)
	}
	if got.SizeBytes != 64 {
		t.Fatalf("stat size = %d, want 64", got.SizeBytes)
	}
}

func TestParseRcloneStatJSONFailsClosed(t *testing.T) {
	tests := []struct {
		name         string
		output       string
		expectedSize int64
		category     RcloneFailureCategory
	}{
		{name: "array instead of stat object", output: `[{"Size":64,"IsDir":false}]`, expectedSize: 64, category: RcloneFailureInvalidStat},
		{name: "malformed JSON", output: `{"Size":`, expectedSize: 64, category: RcloneFailureInvalidStat},
		{name: "trailing JSON value", output: `{"Size":64,"IsDir":false} {}`, expectedSize: 64, category: RcloneFailureInvalidStat},
		{name: "duplicate size field", output: `{"Size":64,"Size":64,"IsDir":false}`, expectedSize: 64, category: RcloneFailureInvalidStat},
		{name: "missing directory bit", output: `{"Size":64}`, expectedSize: 64, category: RcloneFailureInvalidStat},
		{name: "directory target", output: `{"Size":-1,"IsDir":true}`, expectedSize: 64, category: RcloneFailureNotRegularFile},
		{name: "unknown size", output: `{"IsDir":false}`, expectedSize: 64, category: RcloneFailureUnknownSize},
		{name: "null size", output: `{"Size":null,"IsDir":false}`, expectedSize: 64, category: RcloneFailureUnknownSize},
		{name: "non-integer size", output: `{"Size":64.5,"IsDir":false}`, expectedSize: 64, category: RcloneFailureUnknownSize},
		{name: "zero size", output: `{"Size":0,"IsDir":false}`, expectedSize: 64, category: RcloneFailureUnknownSize},
		{name: "negative size", output: `{"Size":-1,"IsDir":false}`, expectedSize: 64, category: RcloneFailureUnknownSize},
		{name: "string directory bit", output: `{"Size":64,"IsDir":"false"}`, expectedSize: 64, category: RcloneFailureInvalidStat},
		{name: "unknown directory bit", output: `{"Size":64,"IsDir":null}`, expectedSize: 64, category: RcloneFailureInvalidStat},
		{name: "size mismatch", output: `{"Size":63,"IsDir":false}`, expectedSize: 64, category: RcloneFailureSizeMismatch},
		{name: "unknown expected size", output: `{"Size":64,"IsDir":false}`, expectedSize: 0, category: RcloneFailureExpectedSizeInvalid},
		{name: "oversized output", output: strings.Repeat(" ", maxRcloneStatOutputBytes+1), expectedSize: 64, category: RcloneFailureInvalidStat},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseRcloneStatJSON([]byte(test.output), test.expectedSize)
			assertRcloneCategory(t, err, test.category)
		})
	}
}

func TestRcloneErrorsAreBoundedAndDoNotExposeInput(t *testing.T) {
	const privateLocator = "private-config-token/rom.iso"
	_, err := ParseRcloneSourceLocator("--private-config-token:" + privateLocator)
	assertRcloneCategory(t, err, RcloneFailureInvalidLocator)
	if strings.Contains(err.Error(), privateLocator) || len(err.Error()) > maxRcloneErrorMessageBytes {
		t.Fatalf("locator error is not bounded/sanitized: %q", err)
	}

	_, err = ParseRcloneStatJSON([]byte(`{"Size":1,"IsDir":false,"secret":"private-config-token"}`), 2)
	assertRcloneCategory(t, err, RcloneFailureSizeMismatch)
	if strings.Contains(err.Error(), "private-config-token") || len(err.Error()) > maxRcloneErrorMessageBytes {
		t.Fatalf("stat error is not bounded/sanitized: %q", err)
	}
}

func assertRcloneCategory(t *testing.T, err error, want RcloneFailureCategory) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want rclone category %q", want)
	}
	var got *RcloneError
	if !errors.As(err, &got) {
		t.Fatalf("error = %T %v, want *RcloneError", err, err)
	}
	if got.Category() != want {
		t.Fatalf("error category = %q, want %q", got.Category(), want)
	}
}
