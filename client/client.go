package client

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/lightninglabs/tassilo/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"

	litrpc "github.com/lightninglabs/lightning-terminal/litrpc"
	taprpc "github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
)

const maxMsgSize = 200 * 1024 * 1024 // 200 MB

// Clients bundles all gRPC service clients used by Tassilo.
type Clients struct {
	conn          *grpc.ClientConn
	LN            lnrpc.LightningClient
	Tap           taprpc.TaprootAssetsClient
	TapChannel    tapchannelrpc.TaprootAssetChannelsClient
	superMacaroon string
}

// Connect dials litd, bakes a supermacaroon, then returns Clients wired with
// the supermacaroon on every call.
func Connect(cfg *config.Config) (*Clients, error) {
	// First connection uses the lit.macaroon just to bake the supermacaroon.
	litMac, err := loadMacaroon(cfg.MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("load lit.macaroon: %w", err)
	}

	tlsCreds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("load tls.cert: %w", err)
	}

	macCred, err := newMacaroonCredential(litMac)
	if err != nil {
		return nil, fmt.Errorf("macaroon credential: %w", err)
	}

	conn, err := grpc.Dial(cfg.RPCServer,
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithPerRPCCredentials(macCred),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial litd: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proxy := litrpc.NewProxyClient(conn)
	resp, err := proxy.BakeSuperMacaroon(ctx, &litrpc.BakeSuperMacaroonRequest{
		RootKeyIdSuffix: 0,
	})
	conn.Close()
	if err != nil {
		return nil, fmt.Errorf("bake supermacaroon: %w", err)
	}

	superMac, err := parseMacaroonHex(resp.Macaroon)
	if err != nil {
		return nil, fmt.Errorf("parse supermacaroon: %w", err)
	}

	superCred, err := newMacaroonCredential(superMac)
	if err != nil {
		return nil, fmt.Errorf("supermacaroon credential: %w", err)
	}

	mainConn, err := grpc.Dial(cfg.RPCServer,
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithPerRPCCredentials(superCred),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial litd with supermacaroon: %w", err)
	}

	return &Clients{
		conn:          mainConn,
		LN:            lnrpc.NewLightningClient(mainConn),
		Tap:           taprpc.NewTaprootAssetsClient(mainConn),
		TapChannel:    tapchannelrpc.NewTaprootAssetChannelsClient(mainConn),
		superMacaroon: resp.Macaroon,
	}, nil
}

// Conn returns the underlying gRPC connection (for constructing additional clients).
func (c *Clients) Conn() *grpc.ClientConn { return c.conn }

// Close releases the underlying gRPC connection.
func (c *Clients) Close() {
	c.conn.Close()
}

func loadMacaroon(path string) (*macaroon.Macaroon, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(data); err != nil {
		return nil, err
	}
	return mac, nil
}

func parseMacaroonHex(hexStr string) (*macaroon.Macaroon, error) {
	data, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, err
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(data); err != nil {
		return nil, err
	}
	return mac, nil
}

type macaroonCredential struct {
	mac *macaroon.Macaroon
}

func newMacaroonCredential(mac *macaroon.Macaroon) (*macaroonCredential, error) {
	return &macaroonCredential{mac: mac}, nil
}

func (m *macaroonCredential) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	data, err := m.mac.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"macaroon": hex.EncodeToString(data),
	}, nil
}

func (m *macaroonCredential) RequireTransportSecurity() bool {
	return true
}
