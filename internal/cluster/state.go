package cluster

import (
	"encoding/json"
	"fmt"
	"os"
)

// WriteState serialises the cluster state to a JSON file (mode 0600).
func WriteState(path string, state *ClusterState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	return os.WriteFile(path, b, 0600)
}

// ReadState reads and deserialises a cluster state JSON file.
func ReadState(path string) (*ClusterState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	var state ClusterState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	return &state, nil
}
