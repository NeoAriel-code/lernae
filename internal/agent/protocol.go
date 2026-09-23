// Package agent defines the closed set of typed commands that may cross the
// future Server/Agent boundary. It does not provide transport or execution.
package agent

import "lernae/internal/domain"

// Command can only be implemented inside this package, keeping the protocol
// explicit rather than exposing a generic command or shell string.
type Command interface {
	isAgentCommand()
}

type RestoreAsset struct {
	AssetID domain.AssetID
}

func (RestoreAsset) isAgentCommand() {}

type LaunchAsset struct {
	AssetID domain.AssetID
}

func (LaunchAsset) isAgentCommand() {}

type GetStatus struct{}

func (GetStatus) isAgentCommand() {}
