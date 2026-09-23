package agent

import "testing"

func TestFoundationCommandsImplementClosedProtocol(t *testing.T) {
	commands := []Command{
		RestoreAsset{},
		LaunchAsset{},
		GetStatus{},
	}
	if len(commands) != 3 {
		t.Fatalf("got %d typed command examples, want 3", len(commands))
	}
}
