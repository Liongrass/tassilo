package main

import (
	"fmt"
	"os"

	"github.com/lightninglabs/tassilo/client"
	"github.com/lightninglabs/tassilo/config"
	"github.com/lightninglabs/tassilo/ui"
	"github.com/urfave/cli/v2"
)

var version = "dev"

func main() {
	cfg := config.DefaultConfig()

	app := &cli.App{
		Name:    "tassilo",
		Usage:   "Taproot Assets CLI wallet",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "rpcserver",
				Usage:       "litd gRPC host:port",
				Value:       cfg.RPCServer,
				Destination: &cfg.RPCServer,
				EnvVars:     []string{"TASSILO_RPCSERVER"},
			},
			&cli.StringFlag{
				Name:        "tlscertpath",
				Usage:       "path to litd TLS certificate",
				Value:       cfg.TLSCertPath,
				Destination: &cfg.TLSCertPath,
				EnvVars:     []string{"TASSILO_TLSCERTPATH"},
			},
			&cli.StringFlag{
				Name:        "macaroonpath",
				Usage:       "path to lit.macaroon",
				Value:       cfg.MacaroonPath,
				Destination: &cfg.MacaroonPath,
				EnvVars:     []string{"TASSILO_MACAROONPATH"},
			},
			&cli.StringFlag{
				Name:        "network",
				Usage:       "network: mainnet, testnet, regtest, simnet",
				Value:       cfg.Network,
				Destination: &cfg.Network,
				EnvVars:     []string{"TASSILO_NETWORK"},
			},
		},
		Action: func(c *cli.Context) error {
			if !c.IsSet("macaroonpath") {
				cfg.MacaroonPath = config.MacaroonPath(cfg.Network)
			}
			return runTUI(cfg)
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runTUI(cfg *config.Config) error {
	fmt.Println("Connecting to litd…")

	clients, err := client.Connect(cfg)
	if err != nil {
		return fmt.Errorf("connect: %w\n\nCheck that litd is running and that --tlscertpath / --macaroonpath are correct.", err)
	}
	defer clients.Close()

	return ui.Run(clients)
}
