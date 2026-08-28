package protocol

import "fmt"

const VersionHeader = "X-Pika-Protocol-Version"

const Version = 2

type Health struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
}

func CheckVersion(remote int) error {
	if remote != Version {
		return fmt.Errorf("incompatible daemon protocol %d (client requires %d)", remote, Version)
	}
	return nil
}
