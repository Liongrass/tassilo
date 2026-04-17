package config

import (
	"os"
	"path/filepath"
	"runtime"
)

const (
	DefaultRPCPort = "8443"
	DefaultHost    = "127.0.0.1"
	DefaultNetwork = "mainnet"
)

// Config holds all runtime configuration for Tassilo.
type Config struct {
	RPCServer    string
	TLSCertPath  string
	MacaroonPath string
	Network      string
}

// DefaultLitDir returns the platform-specific default litd data directory.
func DefaultLitDir() string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Lit")
	case "windows":
		appData := os.Getenv("LOCALAPPDATA")
		if appData == "" {
			home, _ := os.UserHomeDir()
			appData = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(appData, "Lit")
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".lit")
	}
}

// MacaroonPath returns the default lit.macaroon path for the given network.
func MacaroonPath(network string) string {
	return filepath.Join(DefaultLitDir(), network, "lit.macaroon")
}

// DefaultConfig returns a Config with platform-appropriate defaults.
func DefaultConfig() *Config {
	network := DefaultNetwork
	if n := os.Getenv("TASSILO_NETWORK"); n != "" {
		network = n
	}
	return &Config{
		RPCServer:    DefaultHost + ":" + DefaultRPCPort,
		TLSCertPath:  filepath.Join(DefaultLitDir(), "tls.cert"),
		MacaroonPath: MacaroonPath(network),
		Network:      network,
	}
}
