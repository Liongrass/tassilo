package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil/bech32"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
)

func TestClassifySendDest(t *testing.T) {
	tests := []struct {
		dest string
		want sendDestKind
	}{
		{"lnbc10u1pjexample", sendDestInvoice},
		{"lightning:lnbc10u1pjexample", sendDestInvoice},
		{"LNURL1DP68GURN8GHJ7UM9WFMXJCM99E3K7MF0V9CXJ0M385EKVCENXC6R2C35XVUKXEFCV5MKVV34X5EKZD3EV56NYD3HXQURZEPEXEJXXEPNXSCRVWFNV9NXZCN9XQ6XYEFHVGCXXCMYXYMNSERXFQ5FNS", sendDestLNURL},
		{"lightning:lnurl1dp68gurn8ghj7", sendDestLNURL},
		{"satoshi@example.com", sendDestLightningAddress},
		{"lightning:satoshi@example.com", sendDestLightningAddress},
		{"satoshi@localhost", sendDestInvoice},
		{"a@b@example.com", sendDestInvoice},
	}
	for _, tc := range tests {
		if got := classifySendDest(tc.dest); got != tc.want {
			t.Errorf("classifySendDest(%q) = %d, want %d", tc.dest, got, tc.want)
		}
	}
}

func TestLNURLParamsURL(t *testing.T) {
	// LUD-01 example vector.
	const lud01 = "LNURL1DP68GURN8GHJ7UM9WFMXJCM99E3K7MF0V9CXJ0M385EKVCENXC6R2C35XVUKXEFCV5MKVV34X5EKZD3EV56NYD3HXQURZEPEXEJXXEPNXSCRVWFNV9NXZCN9XQ6XYEFHVGCXXCMYXYMNSERXFQ5FNS"
	got, err := lnurlParamsURL(lud01, sendDestLNURL)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://service.com/api?q=3fc3645b439ce8e7f2553a69e5267081d96dcd340693afabe04be7b0ccd178df"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	got, err = lnurlParamsURL("lightning:Satoshi@Example.com", sendDestLightningAddress)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://example.com/.well-known/lnurlp/Satoshi"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Plain-HTTP clearnet LNURLs must be rejected.
	insecure, err := bech32.EncodeFromBase256("lnurl", []byte("http://example.com/pay"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lnurlParamsURL(insecure, sendDestLNURL); err == nil {
		t.Error("expected http clearnet lnurl to be rejected")
	}

	onion, err := bech32.EncodeFromBase256("lnurl", []byte("http://abc.onion/pay"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lnurlParamsURL(onion, sendDestLNURL); err != nil {
		t.Errorf("expected http onion lnurl to be accepted: %v", err)
	}
}

func TestVerifyLNURLInvoice(t *testing.T) {
	params := &lnurlPayParams{metadata: `[["text/plain","hi"]]`}
	h := sha256.Sum256([]byte(params.metadata))
	good := &lnrpc.PayReq{NumMsat: 21000, DescriptionHash: hex.EncodeToString(h[:])}

	if err := verifyLNURLInvoice(good, params, 21000); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := verifyLNURLInvoice(good, params, 22000); err == nil {
		t.Error("expected amount mismatch error")
	}
	bad := &lnrpc.PayReq{NumMsat: 21000, DescriptionHash: hex.EncodeToString(make([]byte, 32))}
	if err := verifyLNURLInvoice(bad, params, 21000); err == nil {
		t.Error("expected description hash mismatch error")
	}
}

func TestLNURLMetadataPlainText(t *testing.T) {
	if got := lnurlMetadataPlainText(`[["text/identifier","a@b.c"],["text/plain","Pay Bob"]]`); got != "Pay Bob" {
		t.Errorf("got %q", got)
	}
	if got := lnurlMetadataPlainText("not json"); got != "" {
		t.Errorf("got %q", got)
	}
}
