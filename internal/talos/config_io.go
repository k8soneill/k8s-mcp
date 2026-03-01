package talos

import (
	"fmt"
	"os"

	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// SaveTalosconfig serialises a Talos client config to a file (mode 0600).
func SaveTalosconfig(path string, tc *clientconfig.Config) error {
	b, err := tc.Bytes()
	if err != nil {
		return fmt.Errorf("marshal talosconfig: %w", err)
	}
	return os.WriteFile(path, b, 0600)
}

// LoadTalosconfig reads and parses a Talos client config file.
func LoadTalosconfig(path string) (*clientconfig.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read talosconfig: %w", err)
	}
	tc, err := clientconfig.FromBytes(b)
	if err != nil {
		return nil, fmt.Errorf("parse talosconfig: %w", err)
	}
	return tc, nil
}
