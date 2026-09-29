package ui

import (
	"testing"

	taprpc "github.com/lightninglabs/taproot-assets/taprpc"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
)

func TestParsePaymentAsset(t *testing.T) {
	// Shape produced by tapd's rfqmsg.Htlc.AsJson; two HTLCs of an MPP payment.
	htlcs := [][]byte{
		[]byte(`{"balances":[{"asset_id":"aabbccddeeff00112233","amount":1500}],"rfq_id":"01"}`),
		[]byte(`{"balances":[{"asset_id":"aabbccddeeff00112233","amount":500}],"rfq_id":"02"}`),
	}
	byID := map[string]assetInfo{"aabbccddeeff00112233": {name: "USDT", dd: 6}}

	name, amt, dd, ok := parsePaymentAsset(htlcs, byID)
	if !ok || name != "USDT" || amt != 2000 || dd != 6 {
		t.Errorf("got (%q, %d, %d, %v), want (USDT, 2000, 6, true)", name, amt, dd, ok)
	}

	// Unknown asset ID: amount still resolved, name falls back to the ID.
	name, amt, _, ok = parsePaymentAsset(htlcs[:1], nil)
	if !ok || name != "aabbccddeeff" || amt != 1500 {
		t.Errorf("got (%q, %d, %v)", name, amt, ok)
	}

	// Non-asset / unparsable data.
	if _, _, _, ok := parsePaymentAsset([][]byte{[]byte("\x01\x02")}, byID); ok {
		t.Error("expected no match for non-JSON data")
	}
}

func TestBuildAssetIDInfoGroupDecimalFallback(t *testing.T) {
	gk := []byte{0x02, 0xab}
	gkHex := "02ab"
	assets := []*taprpc.Asset{
		// Group anchor carries the decimal display...
		{
			AssetGenesis:   &taprpc.GenesisInfo{AssetId: []byte{0x01}, Name: "USDT"},
			AssetGroup:     &taprpc.AssetGroup{TweakedGroupKey: gk},
			DecimalDisplay: &taprpc.DecimalDisplay{DecimalDisplay: 6},
		},
		// ...a reissued tranche in the same group does not.
		{
			AssetGenesis: &taprpc.GenesisInfo{AssetId: []byte{0x02}, Name: "USDT"},
			AssetGroup:   &taprpc.AssetGroup{TweakedGroupKey: gk},
		},
	}
	metaByKey := buildGroupMetaMap(assets)
	if got := metaByKey[gkHex].decimalDisplay; got != 6 {
		t.Fatalf("group decimal display = %d, want 6", got)
	}

	// Channel-only tranche without its own decimal display.
	channels := []*lnrpc.Channel{{
		CustomChannelData: []byte(`{"group_key":"02ab","funding_assets":[{"asset_genesis":{"name":"USDT","asset_id":"03"},"decimal_display":0}]}`),
	}}

	byID := buildAssetIDInfo(assets, channels, metaByKey)
	for _, id := range []string{"01", "02", "03"} {
		if info := byID[id]; info.dd != 6 || info.name != "USDT" {
			t.Errorf("asset %s: got %+v, want {USDT 6}", id, info)
		}
	}
}
