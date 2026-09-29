package ui

import "testing"

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
