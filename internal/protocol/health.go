package protocol

import "fmt"

const VersionHeader = "X-Pika-Protocol-Version"

const Version = 2

type Health struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
	BinaryDigest    string `json:"binary_digest,omitempty"`
	PID             int    `json:"pid,omitempty"`
	HandoffProtocol int    `json:"handoff_protocol,omitempty"`
}

func CheckVersion(remote int) error {
	if remote != Version {
		return fmt.Errorf("incompatible daemon protocol %d (client requires %d)", remote, Version)
	}
	return nil
}
