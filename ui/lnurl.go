package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/bech32"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
)

// sendDestKind identifies what a pasted Send destination actually is.
type sendDestKind int

const (
	sendDestInvoice sendDestKind = iota
	sendDestLNURL
	sendDestLightningAddress
)

// classifySendDest identifies what kind of destination the user entered.
// LNURL strings ("lnurl1...") are checked first: bech32's "lnurl" HRP also
// starts with "ln", so it would otherwise look like a bolt11 invoice.
func classifySendDest(dest string) sendDestKind {
	trimmed := strings.ToLower(stripLightningPrefix(dest))
	switch {
	case strings.HasPrefix(trimmed, "lnurl1"):
		return sendDestLNURL
	case isLightningAddress(dest):
		return sendDestLightningAddress
	default:
		return sendDestInvoice
	}
}

// stripLightningPrefix removes a case-insensitive "lightning:" URI scheme
// prefix without altering the case of the remainder.
func stripLightningPrefix(s string) string {
	const prefix = "lightning:"
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):]
	}
	return s
}

// isLightningAddress reports whether s looks like a Lightning Address
// (LUD-16): exactly one "@", a non-empty local part, and a dotted domain.
func isLightningAddress(s string) bool {
	s = stripLightningPrefix(s)
	if strings.ContainsAny(s, " \t\n\r?#/") || strings.Count(s, "@") != 1 {
		return false
	}
	user, domain, _ := strings.Cut(s, "@")
	return user != "" && strings.Contains(domain, ".")
}

const (
	lnurlHTTPTimeout  = 15 * time.Second
	lnurlMaxBodyBytes = 1 << 20 // payRequest/callback responses are tiny
)

var lnurlHTTPClient = &http.Client{Timeout: lnurlHTTPTimeout}

// lnurlPayParams is a parsed LUD-06 payRequest response (also what a LUD-16
// Lightning Address's well-known endpoint returns).
type lnurlPayParams struct {
	callback        string
	minSendableMsat int64
	maxSendableMsat int64
	metadata        string // raw; its sha256 must match the invoice's description hash
	commentAllowed  int64  // LUD-12; 0 = comments not supported
	description     string // text/plain entry from metadata, for display only
}

type lnurlPayResponse struct {
	Callback       string `json:"callback"`
	MinSendable    int64  `json:"minSendable"`
	MaxSendable    int64  `json:"maxSendable"`
	Metadata       string `json:"metadata"`
	CommentAllowed int64  `json:"commentAllowed"`
	Tag            string `json:"tag"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
}

type lnurlCallbackResponse struct {
	PR     string `json:"pr"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// lnurlParamsURL resolves dest to the URL serving its LUD-06 payRequest:
// the bech32-decoded URL of an "lnurl1..." string (LUD-01), or the
// well-known URL of a Lightning Address (LUD-16).
func lnurlParamsURL(dest string, kind sendDestKind) (string, error) {
	raw := stripLightningPrefix(dest)
	switch kind {
	case sendDestLNURL:
		// LUD-01 doesn't enforce bech32's 90-char limit.
		_, data5, err := bech32.DecodeNoLimit(raw)
		if err != nil {
			return "", fmt.Errorf("decode lnurl: %w", err)
		}
		data, err := bech32.ConvertBits(data5, 5, 8, false)
		if err != nil {
			return "", fmt.Errorf("decode lnurl: %w", err)
		}
		target := string(data)
		if err := validateLNURLTargetURL(target); err != nil {
			return "", err
		}
		return target, nil

	case sendDestLightningAddress:
		user, domain, ok := strings.Cut(raw, "@")
		if !ok || user == "" || domain == "" {
			return "", fmt.Errorf("invalid lightning address")
		}
		scheme := "https"
		if strings.HasSuffix(strings.ToLower(domain), ".onion") {
			scheme = "http"
		}
		return fmt.Sprintf("%s://%s/.well-known/lnurlp/%s",
			scheme, strings.ToLower(domain), url.PathEscape(user)), nil

	default:
		return "", fmt.Errorf("not an lnurl destination")
	}
}

// validateLNURLTargetURL enforces LUD-01's transport rule: HTTPS, or plain
// HTTP only for .onion hosts.
func validateLNURLTargetURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	isOnion := strings.HasSuffix(strings.ToLower(u.Hostname()), ".onion")
	if u.Scheme == "https" || (u.Scheme == "http" && isOnion) {
		return nil
	}
	return fmt.Errorf("url must use https (or http for .onion): %s", rawURL)
}

// fetchLNURLPayParams fetches and validates the payRequest at targetURL.
func fetchLNURLPayParams(ctx context.Context, targetURL string) (*lnurlPayParams, error) {
	body, err := lnurlHTTPGet(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	var parsed lnurlPayResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if strings.EqualFold(parsed.Status, "ERROR") {
		return nil, fmt.Errorf("%s", lnurlReasonOrDefault(parsed.Reason))
	}
	if !strings.EqualFold(parsed.Tag, "payRequest") {
		return nil, fmt.Errorf("unsupported lnurl tag %q (expected payRequest)", parsed.Tag)
	}
	if parsed.Callback == "" {
		return nil, fmt.Errorf("response is missing a callback url")
	}
	if parsed.MinSendable <= 0 || parsed.MaxSendable <= 0 || parsed.MinSendable > parsed.MaxSendable {
		return nil, fmt.Errorf("invalid sendable range (min=%d, max=%d msat)",
			parsed.MinSendable, parsed.MaxSendable)
	}
	if err := validateLNURLTargetURL(parsed.Callback); err != nil {
		return nil, fmt.Errorf("callback: %w", err)
	}
	return &lnurlPayParams{
		callback:        parsed.Callback,
		minSendableMsat: parsed.MinSendable,
		maxSendableMsat: parsed.MaxSendable,
		metadata:        parsed.Metadata,
		commentAllowed:  parsed.CommentAllowed,
		description:     lnurlMetadataPlainText(parsed.Metadata),
	}, nil
}

// fetchLNURLInvoice requests a bolt11 invoice for amountMsat (plus an
// optional LUD-12 comment) from the payRequest callback.
func fetchLNURLInvoice(ctx context.Context, params *lnurlPayParams, amountMsat int64, comment string) (string, error) {
	if params.commentAllowed > 0 && int64(len(comment)) > params.commentAllowed {
		return "", fmt.Errorf("comment exceeds %d characters", params.commentAllowed)
	}
	cbURL, err := url.Parse(params.callback)
	if err != nil {
		return "", fmt.Errorf("invalid callback url: %w", err)
	}
	q := cbURL.Query()
	q.Set("amount", strconv.FormatInt(amountMsat, 10))
	if params.commentAllowed > 0 && comment != "" {
		q.Set("comment", comment)
	}
	cbURL.RawQuery = q.Encode()

	body, err := lnurlHTTPGet(ctx, cbURL.String())
	if err != nil {
		return "", err
	}
	var parsed lnurlCallbackResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if strings.EqualFold(parsed.Status, "ERROR") {
		return "", fmt.Errorf("%s", lnurlReasonOrDefault(parsed.Reason))
	}
	if parsed.PR == "" {
		return "", fmt.Errorf("response carried no invoice")
	}
	return parsed.PR, nil
}

// verifyLNURLInvoice checks the callback's invoice against what was
// requested. LUD-06 requires the description hash to equal sha256(metadata),
// binding the invoice to the terms shown to the user; the amount check
// guards against a service swapping in a different amount.
func verifyLNURLInvoice(decoded *lnrpc.PayReq, params *lnurlPayParams, amountMsat int64) error {
	if decoded.NumMsat != amountMsat {
		return fmt.Errorf("invoice amount (%d msat) does not match the requested amount (%d msat)",
			decoded.NumMsat, amountMsat)
	}
	want := sha256.Sum256([]byte(params.metadata))
	if !strings.EqualFold(decoded.DescriptionHash, hex.EncodeToString(want[:])) {
		return fmt.Errorf("invoice description hash does not match the lnurl metadata")
	}
	return nil
}

// lnurlMetadataPlainText best-effort extracts the "text/plain" entry from a
// LUD-06 metadata string (a JSON array of [mime, content] pairs).
func lnurlMetadataPlainText(metadata string) string {
	var pairs [][]string
	if err := json.Unmarshal([]byte(metadata), &pairs); err != nil {
		return ""
	}
	for _, pair := range pairs {
		if len(pair) >= 2 && pair[0] == "text/plain" {
			return pair[1]
		}
	}
	return ""
}

func lnurlReasonOrDefault(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "request rejected"
	}
	return reason
}

// lnurlHTTPGet GETs targetURL and returns its body, capped at
// lnurlMaxBodyBytes.
func lnurlHTTPGet(ctx context.Context, targetURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, lnurlHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := lnurlHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, lnurlMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Many services return a LUD-06 error JSON with a non-200 status.
		var e struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(body, &e) == nil && strings.EqualFold(e.Status, "ERROR") {
			return nil, fmt.Errorf("%s", lnurlReasonOrDefault(e.Reason))
		}
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return body, nil
}
