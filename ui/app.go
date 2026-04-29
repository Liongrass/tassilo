package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/gdamore/tcell/v2"
	"github.com/lightninglabs/tassilo/client"
	"github.com/rivo/tview"

	taprpc "github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/zpay32"
)

// App is the root TUI application.
type App struct {
	tapp     *tview.Application
	clients  *client.Clients
	pages    *tview.Pages
	nodeInfo *lnrpc.GetInfoResponse
}

// Run starts the TUI and blocks until the user quits.
func Run(clients *client.Clients) error {
	a := &App{
		tapp:    tview.NewApplication(),
		clients: clients,
		pages:   tview.NewPages(),
	}

	if err := a.loadInitialData(); err != nil {
		return err
	}

	a.tapp.SetRoot(a.pages, true)
	a.showDashboard()

	// Ctrl+L forces a full redraw on any screen.
	a.tapp.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlL {
			a.tapp.Sync()
			return nil
		}
		return event
	})

	// SIGCONT: redraws the screen after resuming from a suspend or switching
	// back from another terminal app / tmux pane.
	contCh := make(chan os.Signal, 1)
	signal.Notify(contCh, syscall.SIGCONT)
	go func() {
		for range contCh {
			a.tapp.Draw()
		}
	}()

	// SIGTSTP (Ctrl+Z): let tview release the terminal cleanly before
	// stopping, then reinitialise when the process is resumed with fg.
	tstpCh := make(chan os.Signal, 1)
	signal.Notify(tstpCh, syscall.SIGTSTP)
	go func() {
		for range tstpCh {
			a.tapp.Suspend(func() {
				signal.Stop(tstpCh)
				// SIGSTOP cannot be caught or ignored, so fg will always resume.
				_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
				signal.Notify(tstpCh, syscall.SIGTSTP)
			})
		}
	}()

	err := a.tapp.Run()
	signal.Stop(contCh)
	signal.Stop(tstpCh)
	return err
}

func (a *App) loadInitialData() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := a.clients.LN.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("get node info: %w", err)
	}
	a.nodeInfo = info
	return nil
}

// withEsc wraps a primitive so that pressing Esc calls back.
func withEsc(p tview.Primitive, back func()) tview.Primitive {
	type inputCapturer interface {
		tview.Primitive
		SetInputCapture(func(*tcell.EventKey) *tcell.EventKey) *tview.Box
	}
	if ic, ok := p.(inputCapturer); ok {
		ic.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyEscape {
				back()
				return nil
			}
			return event
		})
	}
	return p
}

func (a *App) showDashboard() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	chanBal, _ := a.clients.LN.ChannelBalance(ctx, &lnrpc.ChannelBalanceRequest{})
	walletBal, _ := a.clients.LN.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})

	chanList, _ := a.clients.LN.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	assetList, _ := a.clients.Tap.ListAssets(ctx, &taprpc.ListAssetRequest{
		ScriptKeyType: &taprpc.ScriptKeyTypeQuery{
			Type: &taprpc.ScriptKeyTypeQuery_AllTypes{AllTypes: true},
		},
	})

	header := tview.NewTextView().
		SetText(fmt.Sprintf(" Tassilo  |  %s  |  %s", a.nodeInfo.Alias, a.nodeInfo.IdentityPubkey[:16]+"…")).
		SetTextAlign(tview.AlignCenter).
		SetDynamicColors(true)

	offchainBTC := fmt.Sprintf(
		"[yellow]Lightning (BTC)[-]\n"+
			"  Local:  [green]%s sat[-]\n"+
			"  Remote: [grey]%s sat[-]\n",
		formatCommas(chanBal.GetLocalBalance().GetSat()),
		formatCommas(chanBal.GetRemoteBalance().GetSat()),
	)

	allAssets := assetList.GetAssets()
	onchainGroups := buildOnchainAssetGroups(allAssets)
	groupMetas := buildGroupMetaMap(allAssets)

	offchainAssets := buildOffchainAssetText(aggregateAssetChannelBalances(chanList.GetChannels()), groupMetas)

	onchainBTC := fmt.Sprintf(
		"[yellow]Onchain Bitcoin[-]\n"+
			"  Confirmed:   [green]%s sat[-]\n"+
			"  Unconfirmed: [grey]%s sat[-]\n",
		formatCommas(walletBal.GetConfirmedBalance()),
		formatCommas(walletBal.GetUnconfirmedBalance()),
	)

	onchainAssets := buildOnchainAssetText(onchainGroups)

	balanceView := tview.NewTextView().
		SetText(offchainBTC + "\n" + offchainAssets + "\n" + onchainBTC + "\n" + onchainAssets).
		SetDynamicColors(true).
		SetWordWrap(true)
	balanceView.SetBorder(true).SetTitle(" Balances ")

	menu := tview.NewList().
		AddItem("Receive — create invoice", "Create a taproot asset or BTC invoice", 'r', func() { a.showReceive() }).
		AddItem("Send — pay invoice", "Pay a bolt11 or asset invoice", 's', func() { a.showSend() }).
		AddItem("List payments", "Show all incoming and outgoing payments", 'p', func() { a.showPayments() }).
		AddItem("List channels", "Show all BTC and asset channels", 'c', func() { a.showChannels() }).
		AddItem("Open channel", "Open a BTC or asset-denominated channel", 'o', func() { a.showOpenChannel() }).
		AddItem("List assets", "Show all known taproot assets", 'a', func() { a.showAssets() }).
		AddItem("Refresh", "Reload balances from node", 'f', func() {
			_ = a.loadInitialData()
			a.showDashboard()
		}).
		AddItem("Quit", "Exit Tassilo", 'q', func() { a.tapp.Stop() })
	menu.SetBorder(true).SetTitle(" Actions ")

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 1, 0, false).
		AddItem(
			tview.NewFlex().
				AddItem(balanceView, 0, 2, false).
				AddItem(menu, 0, 1, true),
			0, 1, true,
		)

	a.pages.AddAndSwitchToPage("dashboard", flex, true)
}

// jsonAssetChannel matches the JSON tapd encodes into lnrpc.Channel.CustomChannelData.
type jsonAssetChannel struct {
	LocalBalance  uint64 `json:"local_balance"`
	RemoteBalance uint64 `json:"remote_balance"`
	GroupKey      string `json:"group_key,omitempty"`
}

type assetGroupBalance struct {
	local  uint64
	remote uint64
}

func aggregateAssetChannelBalances(channels []*lnrpc.Channel) map[string]*assetGroupBalance {
	result := make(map[string]*assetGroupBalance)
	for _, ch := range channels {
		if len(ch.CustomChannelData) == 0 {
			continue
		}
		var data jsonAssetChannel
		if err := json.Unmarshal(ch.CustomChannelData, &data); err != nil || data.GroupKey == "" {
			continue
		}
		if result[data.GroupKey] == nil {
			result[data.GroupKey] = &assetGroupBalance{}
		}
		result[data.GroupKey].local += data.LocalBalance
		result[data.GroupKey].remote += data.RemoteBalance
	}
	return result
}

// onchainAssetGroup holds aggregated onchain info per group key.
type onchainAssetGroup struct {
	name           string
	amount         uint64
	decimalDisplay uint32
}

type groupMeta struct {
	name           string
	decimalDisplay uint32
	groupKey       []byte // raw bytes; nil for ungrouped assets (keyed by asset ID)
}

// buildGroupMetaMap returns a group-key → {name, decimalDisplay} map for all
// assets (including channel assets) so both sections can look up the right
// display name and scale factor.
func buildGroupMetaMap(assets []*taprpc.Asset) map[string]*groupMeta {
	m := make(map[string]*groupMeta)
	for _, a := range assets {
		var key string
		var gk []byte
		if a.AssetGroup != nil && len(a.AssetGroup.TweakedGroupKey) > 0 {
			gk = a.AssetGroup.TweakedGroupKey
			key = fmt.Sprintf("%x", gk)
		} else {
			key = fmt.Sprintf("%x", a.AssetGenesis.AssetId)
		}
		if _, exists := m[key]; !exists {
			dd := uint32(0)
			if a.DecimalDisplay != nil {
				dd = a.DecimalDisplay.DecimalDisplay
			}
			m[key] = &groupMeta{name: a.AssetGenesis.Name, decimalDisplay: dd, groupKey: gk}
		}
	}
	return m
}

// resolveAssetPayReq decodes a bolt11 pay-req against all known group keys and
// returns the asset name, raw amount, and decimal display on the first match.
// It tries without a group key first (tapd may auto-detect), then each known key.
func (a *App) resolveAssetPayReq(payReq string, metaByKey map[string]*groupMeta) (name string, amt uint64, dd uint32, ok bool) {
	if payReq == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	decodeWith := func(gk []byte) (string, uint64, uint32, bool) {
		resp, err := a.clients.TapChannel.DecodeAssetPayReq(ctx, &tapchannelrpc.AssetPayReq{
			PayReqString: payReq,
			GroupKey:     gk,
		})
		if err != nil {
			return "", 0, 0, false
		}
		var n string
		if resp.GenesisInfo != nil {
			n = resp.GenesisInfo.Name
		}
		var d uint32
		if resp.DecimalDisplay != nil {
			d = resp.DecimalDisplay.DecimalDisplay
		}
		// Require at least a name or a non-zero amount to consider it a match.
		if n == "" && resp.AssetAmount == 0 {
			return "", 0, 0, false
		}
		return n, resp.AssetAmount, d, true
	}

	// Try without a group key — tapd may resolve it automatically.
	if n, amount, d, found := decodeWith(nil); found {
		return n, amount, d, true
	}

	// Fall back to trying each known group key.
	for _, meta := range metaByKey {
		if len(meta.groupKey) == 0 {
			continue
		}
		if n, amount, d, found := decodeWith(meta.groupKey); found {
			// If genesis name is missing, use the local metadata name.
			if n == "" {
				n = meta.name
			}
			if d == 0 {
				d = meta.decimalDisplay
			}
			return n, amount, d, true
		}
	}
	return
}

// buildOnchainAssetGroups aggregates wallet (non-channel) assets by group key
// (or asset ID for ungrouped assets), capturing the name and decimal_display
// from the first asset seen in each group.
func buildOnchainAssetGroups(assets []*taprpc.Asset) map[string]*onchainAssetGroup {
	groups := make(map[string]*onchainAssetGroup)
	for _, a := range assets {
		// Skip assets locked in channels — those belong in the offchain section.
		if a.ScriptKeyType == taprpc.ScriptKeyType_SCRIPT_KEY_CHANNEL {
			continue
		}
		var key string
		if a.AssetGroup != nil && len(a.AssetGroup.TweakedGroupKey) > 0 {
			key = fmt.Sprintf("%x", a.AssetGroup.TweakedGroupKey)
		} else {
			key = fmt.Sprintf("%x", a.AssetGenesis.AssetId)
		}
		if groups[key] == nil {
			dd := uint32(0)
			if a.DecimalDisplay != nil {
				dd = a.DecimalDisplay.DecimalDisplay
			}
			groups[key] = &onchainAssetGroup{
				name:           a.AssetGenesis.Name,
				decimalDisplay: dd,
			}
		}
		groups[key].amount += a.Amount
	}
	return groups
}

func buildOffchainAssetText(balances map[string]*assetGroupBalance, metas map[string]*groupMeta) string {
	if len(balances) == 0 {
		return "[yellow]Lightning (Assets)[-]\n  (none)\n"
	}
	var sb strings.Builder
	sb.WriteString("[yellow]Lightning (Assets)[-]\n")
	for groupKey, bal := range balances {
		name := groupKey
		dd := uint32(0)
		if m, ok := metas[groupKey]; ok {
			name = m.name
			dd = m.decimalDisplay
		}
		sb.WriteString(fmt.Sprintf(
			"  [cyan]%s[-]\n"+
				"    Local:  [green]%s[-]\n"+
				"    Remote: [grey]%s[-]\n",
			name,
			formatAssetAmount(bal.local, dd),
			formatAssetAmount(bal.remote, dd),
		))
	}
	return sb.String()
}

func buildOnchainAssetText(groups map[string]*onchainAssetGroup) string {
	if len(groups) == 0 {
		return "[yellow]Onchain (Assets)[-]\n  (none)\n"
	}
	var sb strings.Builder
	sb.WriteString("[yellow]Onchain (Assets)[-]\n")
	for _, g := range groups {
		sb.WriteString(fmt.Sprintf(
			"  [cyan]%-20s[-]  [green]%s[-]\n",
			g.name, formatAssetAmount(g.amount, g.decimalDisplay),
		))
	}
	return sb.String()
}

// parseScaledAmount parses a decimal string and returns it scaled by 10^scale,
// using integer arithmetic throughout to avoid floating-point rounding.
// Examples: ("21.5", 3) → 21500   ("1000", 3) → 1000000   ("0.001", 3) → 1
func parseScaledAmount(s string, scale uint32) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	parts := strings.SplitN(s, ".", 2)

	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount")
	}

	multiplier := uint64(1)
	for i := uint32(0); i < scale; i++ {
		multiplier *= 10
	}
	result := whole * multiplier

	if len(parts) == 2 && scale > 0 && parts[1] != "" {
		frac := parts[1]
		// Truncate extra digits beyond scale; pad if shorter.
		if uint32(len(frac)) > scale {
			frac = frac[:scale]
		} else {
			for uint32(len(frac)) < scale {
				frac += "0"
			}
		}
		fracVal, err := strconv.ParseUint(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid decimal part")
		}
		result += fracVal
	}
	return result, nil
}

// formatCommas inserts thousand separators into an integer.
// bolt11Desc extracts the description field from a bolt11 payment request
// without making any RPC call. Tries all common networks; returns "" on failure.
func bolt11Desc(payReq string) string {
	nets := []*chaincfg.Params{
		&chaincfg.MainNetParams,
		&chaincfg.TestNet3Params,
		&chaincfg.RegressionNetParams,
		&chaincfg.SimNetParams,
	}
	for _, net := range nets {
		inv, err := zpay32.Decode(payReq, net)
		if err == nil {
			if inv.Description != nil {
				return *inv.Description
			}
			return ""
		}
	}
	return ""
}

func formatCommas[T uint64 | int64](n T) string {
	s := fmt.Sprintf("%d", n)
	out := make([]byte, 0, len(s)+(len(s)-1)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 && s[0] != '-' {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// formatAssetAmount scales amount by 10^decimalDisplay and formats with commas.
// decimalDisplay is the number of decimal places (e.g. 2 → divide by 100).
func formatAssetAmount(amount uint64, decimalDisplay uint32) string {
	if decimalDisplay == 0 {
		return formatCommas(amount)
	}
	div := uint64(1)
	for i := uint32(0); i < decimalDisplay; i++ {
		div *= 10
	}
	whole := amount / div
	frac := amount % div
	return fmt.Sprintf("%s.%0*d", formatCommas(whole), int(decimalDisplay), frac)
}

func (a *App) showReceive() {
	var selectedAssetIDHex  string // genesis asset ID hex; empty = BTC or grouped
	var selectedGroupKeyHex string // tweaked group key hex; non-empty = use GroupKey
	var selectedDecimal     uint32
	var amountStr, memoStr string
	var settingFromPicker bool

	assetField := tview.NewInputField().
		SetLabel("Asset").
		SetFieldWidth(40).
		SetPlaceholder("BTC  (press Enter to pick)")
	assetField.SetChangedFunc(func(t string) {
		if !settingFromPicker {
			// user typed manually — treat as raw hex asset ID, no group key
			selectedAssetIDHex = t
			selectedGroupKeyHex = ""
			selectedDecimal = 0
		}
	})

	form := tview.NewForm()
	form.AddFormItem(assetField).
		AddInputField("Amount", "", 20, nil, func(t string) { amountStr = t }).
		AddInputField("Memo (optional)", "", 60, nil, func(t string) { memoStr = t }).
		AddButton("Generate", func() {
			a.doCreateInvoice(selectedAssetIDHex, selectedGroupKeyHex, selectedDecimal, amountStr, memoStr)
		})
	form.SetBorder(true).SetTitle(" Receive — Create Invoice ")

	assetField.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEnter:
			a.showAssetPicker(func(name, assetIDHex, groupKeyHex string, decimalDisplay uint32) {
				if name == "" {
					// cancelled — leave current selection unchanged
					a.tapp.SetFocus(form)
					return
				}
				settingFromPicker = true
				if assetIDHex == "" && groupKeyHex == "" {
					assetField.SetText("") // BTC: show placeholder
				} else {
					assetField.SetText(name)
				}
				settingFromPicker = false
				selectedAssetIDHex = assetIDHex
				selectedGroupKeyHex = groupKeyHex
				selectedDecimal = decimalDisplay
				a.tapp.SetFocus(form)
			})
			return nil
		case tcell.KeyEscape:
			a.showDashboard()
			return nil
		}
		return event
	})

	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("receive", form, true)
}

func (a *App) showAssetPicker(done func(name, assetIDHex, groupKeyHex string, decimalDisplay uint32)) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, _ := a.clients.Tap.ListAssets(ctx, &taprpc.ListAssetRequest{
		ScriptKeyType: &taprpc.ScriptKeyTypeQuery{
			Type: &taprpc.ScriptKeyTypeQuery_AllTypes{AllTypes: true},
		},
	})

	type assetOption struct {
		name           string
		assetIDHex     string // hex genesis ID
		groupKeyHex    string // hex tweaked group key; empty = ungrouped
		displayID      string // what to show as secondary label
		decimalDisplay uint32
	}
	var options []assetOption
	seen := make(map[string]bool)
	for _, asset := range resp.GetAssets() {
		if asset.ScriptKeyType != taprpc.ScriptKeyType_SCRIPT_KEY_CHANNEL {
			continue
		}
		assetIDHex := fmt.Sprintf("%x", asset.AssetGenesis.AssetId)
		var groupKeyHex, displayID string
		if asset.AssetGroup != nil && len(asset.AssetGroup.TweakedGroupKey) > 0 {
			groupKeyHex = fmt.Sprintf("%x", asset.AssetGroup.TweakedGroupKey)
			displayID = groupKeyHex
		} else {
			displayID = assetIDHex
		}
		dedupeKey := groupKeyHex
		if dedupeKey == "" {
			dedupeKey = assetIDHex
		}
		if seen[dedupeKey] {
			continue
		}
		seen[dedupeKey] = true
		dd := uint32(0)
		if asset.DecimalDisplay != nil {
			dd = asset.DecimalDisplay.DecimalDisplay
		}
		options = append(options, assetOption{
			name:           asset.AssetGenesis.Name,
			assetIDHex:     assetIDHex,
			groupKeyHex:    groupKeyHex,
			displayID:      displayID,
			decimalDisplay: dd,
		})
	}
	sort.Slice(options, func(i, j int) bool {
		return options[i].name < options[j].name
	})

	list := tview.NewList()
	list.AddItem("BTC", "Bitcoin — no asset", 0, func() {
		a.pages.SwitchToPage("receive")
		done("BTC", "", "", 0)
	})
	for _, opt := range options {
		opt := opt
		list.AddItem(opt.name, opt.displayID, 0, func() {
			a.pages.SwitchToPage("receive")
			done(opt.name, opt.assetIDHex, opt.groupKeyHex, opt.decimalDisplay)
		})
	}
	list.SetBorder(true).SetTitle(" Select Asset (Esc=cancel) ")
	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.pages.SwitchToPage("receive")
			done("", "", "", 0) // cancel: name=="" signals no selection
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("assetpicker", list, true)
}

func (a *App) doCreateInvoice(assetIDHex, groupKeyHex string, decimalDisplay uint32, amountStr, memo string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var payReq string

	isBTC := assetIDHex == "" && groupKeyHex == ""
	if isBTC {
		// Parse as satoshis; decimal part maps to millisatoshis (scale=3).
		valueMsat, err := parseScaledAmount(amountStr, 3)
		if err != nil || valueMsat == 0 {
			a.showModal("Invalid amount.", func() { a.showReceive() })
			return
		}
		resp, err := a.clients.LN.AddInvoice(ctx, &lnrpc.Invoice{
			Memo:      memo,
			ValueMsat: int64(valueMsat),
		})
		if err != nil {
			a.showModal(fmt.Sprintf("Error: %v", err), func() { a.showReceive() })
			return
		}
		payReq = resp.PaymentRequest
	} else {
		// Parse display units; decimal part maps to sub-units via decimalDisplay.
		scaledAmount, err := parseScaledAmount(amountStr, decimalDisplay)
		if err != nil || scaledAmount == 0 {
			a.showModal("Invalid amount.", func() { a.showReceive() })
			return
		}

		req := &tapchannelrpc.AddInvoiceRequest{
			AssetAmount: scaledAmount, // already scaled by parseScaledAmount
			InvoiceRequest: &lnrpc.Invoice{
				Memo: memo,
			},
		}
		if groupKeyHex != "" {
			groupKeyBytes, err := hexToBytes(groupKeyHex)
			if err != nil {
				a.showModal("Invalid group key.", func() { a.showReceive() })
				return
			}
			req.GroupKey = groupKeyBytes
		} else {
			assetIDBytes, err := hexToBytes(assetIDHex)
			if err != nil || len(assetIDBytes) != 32 {
				a.showModal("Invalid asset ID (must be 32-byte hex).", func() { a.showReceive() })
				return
			}
			req.AssetId = assetIDBytes
		}

		resp, err := a.clients.TapChannel.AddInvoice(ctx, req)
		if err != nil {
			a.showModal(fmt.Sprintf("Error: %v", err), func() { a.showReceive() })
			return
		}
		if resp.InvoiceResult != nil {
			payReq = resp.InvoiceResult.PaymentRequest
		}
	}

	a.showInvoicePage(payReq)
}

func (a *App) showInvoicePage(payReq string) {
	a.tapp.Suspend(func() {
		fmt.Printf("\nPayment Request:\n\n%s\n\nPress Enter to return...\n", payReq)
		bufio.NewReader(os.Stdin).ReadString('\n')
	})
	a.showDashboard()
}

func (a *App) showSend() {
	form := tview.NewForm()
	var payReqStr string

	form.AddInputField("Payment Request (bolt11)", "", 0, nil, func(t string) { payReqStr = t }).
		AddButton("Pay", func() {
			if strings.TrimSpace(payReqStr) == "" {
				a.showModal("Payment request is empty.", func() { a.showSend() })
				return
			}
			a.showPaymentMethodPicker(payReqStr)
		})

	form.SetBorder(true).SetTitle(" Send — Pay Invoice ")
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("send", form, true)
}

func (a *App) showPaymentMethodPicker(payReq string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	chanBal, _ := a.clients.LN.ChannelBalance(ctx, &lnrpc.ChannelBalanceRequest{})
	chanList, _ := a.clients.LN.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	assetList, _ := a.clients.Tap.ListAssets(ctx, &taprpc.ListAssetRequest{
		ScriptKeyType: &taprpc.ScriptKeyTypeQuery{
			Type: &taprpc.ScriptKeyTypeQuery_AllTypes{AllTypes: true},
		},
	})

	groupMetas := buildGroupMetaMap(assetList.GetAssets())
	assetBals := aggregateAssetChannelBalances(chanList.GetChannels())

	type entry struct {
		name           string
		groupKeyHex    string
		decimalDisplay uint32
		local          uint64
	}
	var entries []entry
	for groupKey, bal := range assetBals {
		meta := groupMetas[groupKey]
		if meta == nil {
			continue
		}
		entries = append(entries, entry{
			name:           meta.name,
			groupKeyHex:    groupKey,
			decimalDisplay: meta.decimalDisplay,
			local:          bal.local,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	list := tview.NewList()
	btcLocal := chanBal.GetLocalBalance().GetSat()
	list.AddItem("Bitcoin", fmt.Sprintf("%s sat local", formatCommas(btcLocal)), 0, func() {
		a.showPaymentConfirmation(payReq, "Bitcoin", "", func() { a.doSendBTC(payReq) })
	})
	for _, e := range entries {
		e := e
		list.AddItem(
			e.name,
			fmt.Sprintf("%s local", formatAssetAmount(e.local, e.decimalDisplay)),
			0,
			func() {
				a.showPaymentConfirmation(payReq, e.name, e.groupKeyHex, func() { a.doSendAsset(payReq, e.groupKeyHex) })
			},
		)
	}

	list.SetBorder(true).SetTitle(" Pay With (Esc=back) ")
	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showSend()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("paywith", list, true)
}

func (a *App) showPaymentConfirmation(payReq, assetName, groupKeyHex string, onConfirm func()) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sb strings.Builder
	sb.WriteString("[yellow]Payment Summary[-]\n\n")

	decoded, err := a.clients.LN.DecodePayReq(ctx, &lnrpc.PayReqString{PayReq: payReq})
	if err != nil {
		sb.WriteString(fmt.Sprintf("[red]Could not decode invoice: %v[-]\n\n", err))
	} else {
		sb.WriteString(fmt.Sprintf("Destination:  [cyan]%s[-]\n", decoded.Destination))
		if decoded.Description != "" {
			sb.WriteString(fmt.Sprintf("Description:  %s\n", decoded.Description))
		}
	}

	sb.WriteString(fmt.Sprintf("Pay with:     [cyan]%s[-]\n", assetName))

	if groupKeyHex == "" {
		// BTC payment — amount comes from the decoded invoice.
		if err == nil {
			sb.WriteString(fmt.Sprintf("Amount:       [green]%s sat[-]\n", formatCommas(uint64(decoded.NumSatoshis))))
		}
	} else {
		// Asset payment — ask tapd to decode the asset-specific fields.
		groupKeyBytes, hexErr := hexToBytes(groupKeyHex)
		if hexErr == nil {
			assetResp, assetErr := a.clients.TapChannel.DecodeAssetPayReq(ctx, &tapchannelrpc.AssetPayReq{
				PayReqString: payReq,
				GroupKey:     groupKeyBytes,
			})
			if assetErr == nil {
				dd := uint32(0)
				if assetResp.DecimalDisplay != nil {
					dd = assetResp.DecimalDisplay.DecimalDisplay
				}
				sb.WriteString(fmt.Sprintf("Amount:       [green]%s[-]\n", formatAssetAmount(assetResp.AssetAmount, dd)))
			} else {
				sb.WriteString(fmt.Sprintf("Amount:       [grey]could not decode: %v[-]\n", assetErr))
			}
		}
	}

	sb.WriteString("\n[grey]Enter to confirm  ·  Esc to go back[-]")

	tv := tview.NewTextView().
		SetText(sb.String()).
		SetDynamicColors(true)
	tv.SetBorder(true).SetTitle(" Confirm Payment ")
	tv.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEnter:
			onConfirm()
			return nil
		case tcell.KeyEscape:
			a.showPaymentMethodPicker(payReq)
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("payconfirm", tv, true)
}

func (a *App) doSendBTC(payReq string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	router := routerrpc.NewRouterClient(a.clients.Conn())
	stream, err := router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest: payReq,
		TimeoutSeconds: 60,
		FeeLimitSat:    10000,
	})
	if err != nil {
		a.showModal(fmt.Sprintf("Payment failed:\n%v", err), func() { a.showSend() })
		return
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			a.showModal(fmt.Sprintf("Stream error: %v", err), func() { a.showSend() })
			return
		}
		if resp.Status == lnrpc.Payment_SUCCEEDED {
			a.showModal(fmt.Sprintf("[green]Payment sent![-]\nPreimage: %s", resp.PaymentPreimage), func() { a.showDashboard() })
			return
		}
		if resp.Status == lnrpc.Payment_FAILED {
			a.showModal(fmt.Sprintf("Payment failed: %s", resp.FailureReason), func() { a.showSend() })
			return
		}
	}
}

func (a *App) doSendAsset(payReq, groupKeyHex string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	groupKeyBytes, err := hexToBytes(groupKeyHex)
	if err != nil {
		a.showModal("Invalid group key.", func() { a.showSend() })
		return
	}

	stream, err := a.clients.TapChannel.SendPayment(ctx, &tapchannelrpc.SendPaymentRequest{
		GroupKey: groupKeyBytes,
		PaymentRequest: &routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			TimeoutSeconds: 60,
			FeeLimitSat:    10000,
		},
	})
	if err != nil {
		a.showModal(fmt.Sprintf("Error: %v", err), func() { a.showSend() })
		return
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			a.showModal(fmt.Sprintf("Stream error: %v", err), func() { a.showSend() })
			return
		}
		if pr := resp.GetPaymentResult(); pr != nil {
			if pr.Status == lnrpc.Payment_SUCCEEDED {
				a.showModal(fmt.Sprintf("[green]Asset payment sent![-]\nPreimage: %s", pr.PaymentPreimage), func() { a.showDashboard() })
				return
			}
			if pr.Status == lnrpc.Payment_FAILED {
				a.showModal(fmt.Sprintf("Asset payment failed: %s", pr.FailureReason), func() { a.showSend() })
				return
			}
		}
	}
}

// parsePaymentAsset tries to decode tapd's JSON-encoded asset payment data from
// a Route or InvoiceHTLC CustomChannelData blob. On success it returns the
// asset name, raw amount, decimal display, and true.
func parsePaymentAsset(data []byte, metaByKey map[string]*groupMeta) (name string, amt uint64, dd uint32, ok bool) {
	if len(data) == 0 {
		return
	}
	var wrapper struct {
		AssetAmounts map[string]uint64 `json:"asset_amounts"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil || len(wrapper.AssetAmounts) == 0 {
		return
	}
	for key, amount := range wrapper.AssetAmounts {
		if m, found := metaByKey[key]; found {
			return m.name, amount, m.decimalDisplay, true
		}
	}
	return
}

// paymentEntry is a unified record from any payment source.
type paymentEntry struct {
	ts        int64  // unix seconds
	incoming  bool
	amtMsat   int64  // for BTC/LN (msat)
	amtAsset  uint64 // for asset transfers (raw pre-decimal units)
	assetName string // "BTC" or the asset genesis name
	decDisp   uint32
	kind      string // "ln_out" | "ln_in" | "onchain" | "asset"
	memo      string
	lnOut     *lnrpc.Payment
	lnIn      *lnrpc.Invoice
	onchain   *lnrpc.Transaction
	assetXfer *taprpc.AssetTransfer
}

func (a *App) showChannels() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	chResp, err := a.clients.LN.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		a.showDashboard()
		return
	}

	// Asset metadata for name + decimal display lookups.
	var allAssets []*taprpc.Asset
	if al, err := a.clients.Tap.ListAssets(ctx, &taprpc.ListAssetRequest{
		ScriptKeyType: &taprpc.ScriptKeyTypeQuery{
			Type: &taprpc.ScriptKeyTypeQuery_AllTypes{AllTypes: true},
		},
	}); err == nil {
		allAssets = al.GetAssets()
	}
	metaByKey := buildGroupMetaMap(allAssets)

	type channelRow struct {
		ch        *lnrpc.Channel
		assetName string
		decDisp   uint32
		capacity  uint64
		local     uint64
		remote    uint64
		isBTC     bool
	}

	var rows []channelRow
	for _, ch := range chResp.GetChannels() {
		row := channelRow{ch: ch}

		if len(ch.CustomChannelData) > 0 {
			var data jsonAssetChannel
			if jsonErr := json.Unmarshal(ch.CustomChannelData, &data); jsonErr == nil && data.GroupKey != "" {
				if meta, ok := metaByKey[data.GroupKey]; ok {
					row.assetName = meta.name
					row.decDisp = meta.decimalDisplay
				} else {
					row.assetName = data.GroupKey[:min(12, len(data.GroupKey))]
				}
				row.local = data.LocalBalance
				row.remote = data.RemoteBalance
				row.capacity = data.LocalBalance + data.RemoteBalance
			}
		}

		if row.assetName == "" {
			row.assetName = "BTC"
			row.local = uint64(ch.LocalBalance)
			row.remote = uint64(ch.RemoteBalance)
			row.capacity = uint64(ch.Capacity)
			row.isBTC = true
		}

		rows = append(rows, row)
	}

	// Sort: BTC channels first, then by asset name.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].isBTC != rows[j].isBTC {
			return rows[i].isBTC
		}
		return rows[i].assetName < rows[j].assetName
	})

	// Resolve peer aliases — one GetNodeInfo call per unique pubkey.
	peerAlias := make(map[string]string)
	for _, row := range rows {
		pk := row.ch.RemotePubkey
		if _, seen := peerAlias[pk]; seen {
			continue
		}
		info, err := a.clients.LN.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{PubKey: pk})
		if err == nil && info.Node != nil && info.Node.Alias != "" {
			peerAlias[pk] = info.Node.Alias
		} else {
			peerAlias[pk] = pk[:12] + "…" + pk[len(pk)-8:]
		}
	}

	table := tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)

	hdr := func(s string) *tview.TableCell {
		return tview.NewTableCell("[yellow]" + s + "[-]").SetSelectable(false).SetExpansion(1)
	}
	table.SetCell(0, 0, hdr("Peer"))
	table.SetCell(0, 1, hdr("Asset"))
	table.SetCell(0, 2, hdr("Capacity"))
	table.SetCell(0, 3, hdr("Local"))
	table.SetCell(0, 4, hdr("Remote"))

	for i, row := range rows {
		r := i + 1

		peer := peerAlias[row.ch.RemotePubkey]
		peerColor := "[white]"
		if !row.ch.Active {
			peerColor = "[grey]"
		}

		capacityStr := formatAssetAmount(row.capacity, row.decDisp)
		localStr := formatAssetAmount(row.local, row.decDisp)
		remoteStr := formatAssetAmount(row.remote, row.decDisp)
		if row.isBTC {
			capacityStr += " sat"
			localStr += " sat"
			remoteStr += " sat"
		}

		table.SetCell(r, 0, tview.NewTableCell(peerColor+peer+"[-]"))
		table.SetCell(r, 1, tview.NewTableCell(row.assetName))
		table.SetCell(r, 2, tview.NewTableCell(capacityStr))
		table.SetCell(r, 3, tview.NewTableCell("[green]"+localStr+"[-]"))
		table.SetCell(r, 4, tview.NewTableCell("[blue]"+remoteStr+"[-]"))
	}

	table.SetSelectedFunc(func(row, _ int) {
		if row < 1 || row > len(rows) {
			return
		}
		a.showChannelDetail(rows[row-1].ch, rows[row-1].assetName, rows[row-1].decDisp,
			rows[row-1].capacity, rows[row-1].local, rows[row-1].remote)
	})

	table.SetBorder(true).SetTitle(fmt.Sprintf(" Channels (%d) ", len(rows)))
	table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("channels", table, true)
}

func (a *App) showChannelDetail(ch *lnrpc.Channel, assetName string, decDisp uint32, capacity, local, remote uint64) {
	var sb strings.Builder

	active := "[green]active[-]"
	if !ch.Active {
		active = "[grey]inactive[-]"
	}
	visibility := "public"
	if ch.Private {
		visibility = "private"
	}

	sb.WriteString(fmt.Sprintf("[yellow]Channel  %s  %s[-]\n\n", active, visibility))
	sb.WriteString(fmt.Sprintf("Peer:      %s\n", ch.RemotePubkey))
	sb.WriteString(fmt.Sprintf("ChanPoint: %s\n", ch.ChannelPoint))
	sb.WriteString(fmt.Sprintf("ChanID:    %d\n\n", ch.ChanId))

	if assetName == "BTC" {
		sb.WriteString(fmt.Sprintf("Capacity:  %s sat\n", formatCommas(capacity)))
		sb.WriteString(fmt.Sprintf("Local:     [green]%s sat[-]\n", formatCommas(local)))
		sb.WriteString(fmt.Sprintf("Remote:    [blue]%s sat[-]\n", formatCommas(remote)))
	} else {
		sb.WriteString(fmt.Sprintf("Asset:     %s\n", assetName))
		sb.WriteString(fmt.Sprintf("Capacity:  %s\n", formatAssetAmount(capacity, decDisp)))
		sb.WriteString(fmt.Sprintf("Local:     [green]%s[-]\n", formatAssetAmount(local, decDisp)))
		sb.WriteString(fmt.Sprintf("Remote:    [blue]%s[-]\n", formatAssetAmount(remote, decDisp)))
		sb.WriteString(fmt.Sprintf("\nBTC capacity: %s sat\n", formatCommas(uint64(ch.Capacity))))
	}

	if ch.CommitFee > 0 {
		sb.WriteString(fmt.Sprintf("\nCommit fee: %s sat\n", formatCommas(uint64(ch.CommitFee))))
	}
	if ch.NumUpdates > 0 {
		sb.WriteString(fmt.Sprintf("Updates:    %d\n", ch.NumUpdates))
	}

	detail := tview.NewTextView().SetText(sb.String()).SetDynamicColors(true)
	detail.SetBorder(true).SetTitle(" Channel Detail ")
	detail.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showChannels()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("channel_detail", detail, true)
}

func (a *App) showPayments() {
	const pageSize = uint64(100)

	newCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 15*time.Second)
	}

	// Asset metadata — fetched once, reused for backfill.
	var allAssets []*taprpc.Asset
	assetIDToMeta := make(map[string]struct {
		name string
		dd   uint32
	})
	func() {
		ctx, cancel := newCtx()
		defer cancel()
		al, err := a.clients.Tap.ListAssets(ctx, &taprpc.ListAssetRequest{
			ScriptKeyType: &taprpc.ScriptKeyTypeQuery{
				Type: &taprpc.ScriptKeyTypeQuery_AllTypes{AllTypes: true},
			},
		})
		if err != nil {
			return
		}
		allAssets = al.GetAssets()
		for _, asset := range allAssets {
			key := fmt.Sprintf("%x", asset.AssetGenesis.AssetId)
			if _, exists := assetIDToMeta[key]; exists {
				continue
			}
			dd := uint32(0)
			if asset.DecimalDisplay != nil {
				dd = asset.DecimalDisplay.DecimalDisplay
			}
			assetIDToMeta[key] = struct {
				name string
				dd   uint32
			}{asset.AssetGenesis.Name, dd}
		}
	}()
	metaByKey := buildGroupMetaMap(allAssets)

	var entries []paymentEntry
	// Cursors: LastIndexOffset from previous page; 0 means exhausted.
	var lnOutCursor uint64
	var lnInCursor uint64
	// Seen-hash sets prevent duplicates in case pages overlap.
	seenLNOut := make(map[string]struct{})
	seenLNIn := make(map[string]struct{})

	loadLNOut := func() {
		ctx, cancel := newCtx()
		defer cancel()
		resp, err := a.clients.LN.ListPayments(ctx, &lnrpc.ListPaymentsRequest{
			IncludeIncomplete: true,
			Reversed:          true,
			IndexOffset:       lnOutCursor,
			MaxPayments:       pageSize,
		})
		if err != nil {
			return
		}
		batch := resp.GetPayments()
		for _, p := range batch {
			if _, seen := seenLNOut[p.PaymentHash]; seen {
				continue
			}
			seenLNOut[p.PaymentHash] = struct{}{}
			entries = append(entries, paymentEntry{
				ts:        p.CreationTimeNs / 1_000_000_000,
				incoming:  false,
				amtMsat:   p.ValueMsat,
				assetName: "BTC",
				kind:      "ln_out",
				lnOut:     p,
			})
		}
		last := resp.GetLastIndexOffset()
		if uint64(len(batch)) < pageSize || last == 0 {
			lnOutCursor = 0
		} else {
			lnOutCursor = last
		}
	}

	loadLNIn := func() {
		ctx, cancel := newCtx()
		defer cancel()
		resp, err := a.clients.LN.ListInvoices(ctx, &lnrpc.ListInvoiceRequest{
			Reversed:       true,
			IndexOffset:    lnInCursor,
			NumMaxInvoices: pageSize,
		})
		if err != nil {
			return
		}
		batch := resp.GetInvoices()
		for _, inv := range batch {
			if inv.GetState() != lnrpc.Invoice_SETTLED {
				continue
			}
			key := fmt.Sprintf("%x", inv.RHash)
			if _, seen := seenLNIn[key]; seen {
				continue
			}
			seenLNIn[key] = struct{}{}
			entries = append(entries, paymentEntry{
				ts:        inv.SettleDate,
				incoming:  true,
				amtMsat:   inv.AmtPaidMsat,
				assetName: "BTC",
				kind:      "ln_in",
				memo:      inv.Memo,
				lnIn:      inv,
			})
		}
		last := resp.GetLastIndexOffset()
		if uint64(len(batch)) < pageSize || last == 0 {
			lnInCursor = 0
		} else {
			lnInCursor = last
		}
	}

	// backfillFrom identifies asset LN payments. CustomChannelData on the wire is
	// TLV-encoded (not JSON), so we try JSON first for forward-compat, then fall
	// back to DecodeAssetPayReq against known group keys.
	backfillFrom := func(start int) {
		for i := start; i < len(entries); i++ {
			e := &entries[i]
			if e.assetName != "BTC" {
				continue
			}

			var customData []byte
			var payReq string

			if e.kind == "ln_out" && e.lnOut != nil {
				payReq = e.lnOut.PaymentRequest
				for _, htlc := range e.lnOut.Htlcs {
					if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED && htlc.Route != nil {
						customData = htlc.Route.CustomChannelData
						break
					}
				}
			} else if e.kind == "ln_in" && e.lnIn != nil {
				payReq = e.lnIn.PaymentRequest
				for _, htlc := range e.lnIn.Htlcs {
					if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
						customData = htlc.CustomChannelData
						break
					}
				}
			}

			if len(customData) == 0 {
				continue
			}

			// Try JSON (works if tapd serialises as JSON in future / different builds).
			if name, amt, dd, found := parsePaymentAsset(customData, metaByKey); found {
				e.assetName, e.amtAsset, e.decDisp, e.amtMsat = name, amt, dd, 0
				continue
			}

			// CustomChannelData is TLV — decode via DecodeAssetPayReq on the invoice.
			if name, amt, dd, found := a.resolveAssetPayReq(payReq, metaByKey); found {
				e.assetName, e.amtAsset, e.decDisp, e.amtMsat = name, amt, dd, 0
			} else {
				// Known TAP payment but asset unresolvable — at least not BTC.
				e.assetName = "Asset"
				e.amtMsat = 0
			}
		}

		// Populate memos for BTC outgoing payments by parsing the bolt11 locally.
		for i := start; i < len(entries); i++ {
			e := &entries[i]
			if e.kind == "ln_out" && e.assetName == "BTC" && e.lnOut != nil && e.lnOut.PaymentRequest != "" {
				e.memo = bolt11Desc(e.lnOut.PaymentRequest)
			}
		}
	}

	// Initial LN page (newest first).
	loadLNOut()
	loadLNIn()

	// Onchain BTC — fetch all (typically small).
	func() {
		ctx, cancel := newCtx()
		defer cancel()
		txns, err := a.clients.LN.GetTransactions(ctx, &lnrpc.GetTransactionsRequest{})
		if err != nil {
			return
		}
		for _, tx := range txns.GetTransactions() {
			amt := tx.Amount
			entries = append(entries, paymentEntry{
				ts:        tx.TimeStamp,
				incoming:  amt > 0,
				amtMsat:   amt * 1000,
				assetName: "BTC",
				kind:      "onchain",
				memo:      tx.Label,
				onchain:   tx,
			})
		}
	}()

	// Asset transfers — fetch all (typically small).
	func() {
		ctx, cancel := newCtx()
		defer cancel()
		xfers, err := a.clients.Tap.ListTransfers(ctx, &taprpc.ListTransfersRequest{})
		if err != nil {
			return
		}
		for _, xfer := range xfers.GetTransfers() {
			outgoing := false
			for _, out := range xfer.Outputs {
				if !out.ScriptKeyIsLocal && out.OutputType == taprpc.OutputType_OUTPUT_TYPE_SIMPLE {
					outgoing = true
					break
				}
			}
			var totalAmt uint64
			var assetName string
			var dd uint32
			if outgoing {
				for _, out := range xfer.Outputs {
					if !out.ScriptKeyIsLocal && out.OutputType == taprpc.OutputType_OUTPUT_TYPE_SIMPLE {
						totalAmt += out.Amount
						if assetName == "" {
							if m, ok := assetIDToMeta[fmt.Sprintf("%x", out.AssetId)]; ok {
								assetName, dd = m.name, m.dd
							}
						}
					}
				}
			} else {
				for _, out := range xfer.Outputs {
					if out.ScriptKeyIsLocal {
						totalAmt += out.Amount
						if assetName == "" {
							if m, ok := assetIDToMeta[fmt.Sprintf("%x", out.AssetId)]; ok {
								assetName, dd = m.name, m.dd
							}
						}
					}
				}
			}
			if assetName == "" {
				assetName = "(unknown)"
			}
			entries = append(entries, paymentEntry{
				ts:        xfer.TransferTimestamp,
				incoming:  !outgoing,
				amtAsset:  totalAmt,
				assetName: assetName,
				decDisp:   dd,
				kind:      "asset",
				assetXfer: xfer,
			})
		}
	}()

	backfillFrom(0)
	sort.Slice(entries, func(i, j int) bool { return entries[i].ts > entries[j].ts })

	// Fixed display widths for each column (in terminal cells).
	const (
		wDate  = 16
		wAmt   = 13 // sign + up to 12 chars (commas, decimal)
		wAsset = 12
		wType  = 2
		wMemo  = 28
	)
	clip := func(s string, w int) string {
		r := []rune(s)
		if len(r) > w {
			return string(r[:w])
		}
		return s
	}

	headers := []string{
		fmt.Sprintf("%-*s", wDate, "Date"),
		fmt.Sprintf("%-*s", wAmt, "Amount"),
		fmt.Sprintf("%-*s", wAsset, "Asset"),
		fmt.Sprintf("%-*s", wType, ""),
		"Memo",
	}
	table := tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)

	setEntryRow := func(row int, e paymentEntry) {
		ts := time.Unix(e.ts, 0).Format("2006-01-02 15:04")
		var rawAmt string
		switch {
		case e.assetName == "BTC":
			sat := e.amtMsat / 1000
			if sat < 0 {
				sat = -sat
			}
			rawAmt = formatCommas(uint64(sat))
		case e.amtAsset > 0:
			rawAmt = formatAssetAmount(e.amtAsset, e.decDisp)
		default:
			rawAmt = "?" // TAP payment detected but amount unresolved
		}
		color := "[green]"
		prefix := "+"
		if !e.incoming {
			color = "[red]"
			prefix = "-"
		}
		// Pad/truncate visible amount (prefix + digits) to fixed width.
		visibleAmt := fmt.Sprintf("%-*s", wAmt, prefix+rawAmt)
		typeEmoji := map[string]string{
			"ln_out": "⚡", "ln_in": "⚡",
			"onchain": "⛓️", "asset": "⛓️",
		}[e.kind]
		table.SetCell(row, 0, tview.NewTableCell(ts).SetMaxWidth(wDate))
		table.SetCell(row, 1, tview.NewTableCell(color+visibleAmt+"[-]").SetMaxWidth(wAmt))
		table.SetCell(row, 2, tview.NewTableCell(fmt.Sprintf("%-*s", wAsset, clip(e.assetName, wAsset))).SetMaxWidth(wAsset))
		table.SetCell(row, 3, tview.NewTableCell(typeEmoji).SetMaxWidth(wType))
		table.SetCell(row, 4, tview.NewTableCell(clip(e.memo, wMemo)).SetMaxWidth(wMemo))
	}

	rebuildTable := func() {
		table.Clear()
		for col, h := range headers {
			table.SetCell(0, col, tview.NewTableCell("[yellow]"+h+"[-]").
				SetSelectable(false))
		}
		for i, e := range entries {
			setEntryRow(i+1, e)
		}
		table.SetTitle(fmt.Sprintf(" Payments (%d) ", len(entries)))
	}

	rebuildTable()

	table.SetSelectedFunc(func(row, _ int) {
		if row < 1 || row > len(entries) {
			return
		}
		a.showPaymentDetail(entries[row-1])
	})

	table.SetBorder(true)
	table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		// Load older payments when the user reaches the last row.
		// Always consume the event at the last row to prevent tview wrap-around.
		if event.Key() == tcell.KeyDown || event.Rune() == 'j' {
			row, _ := table.GetSelection()
			if row >= table.GetRowCount()-1 {
				if lnOutCursor > 0 || lnInCursor > 0 {
					prevLen := len(entries)
					// Keep fetching pages until at least one new visible entry
					// appears or all sources are exhausted. This skips through
					// pages of unsettled invoices (which add 0 entries) without
					// requiring repeated Down presses from the user.
					for len(entries) == prevLen && (lnOutCursor > 0 || lnInCursor > 0) {
						if lnOutCursor > 0 {
							loadLNOut()
						}
						if lnInCursor > 0 {
							loadLNIn()
						}
					}
					if len(entries) > prevLen {
						backfillFrom(prevLen)
						newPart := entries[prevLen:]
						sort.Slice(newPart, func(i, j int) bool { return newPart[i].ts > newPart[j].ts })
						for i := prevLen; i < len(entries); i++ {
							setEntryRow(i+1, entries[i])
						}
						table.SetTitle(fmt.Sprintf(" Payments (%d) ", len(entries)))
					}
				}
				return nil
			}
		}
		return event
	})

	a.pages.AddAndSwitchToPage("payments", table, true)
}

func (a *App) showPaymentDetail(e paymentEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sb strings.Builder
	ts := time.Unix(e.ts, 0).Format("2006-01-02 15:04:05")

	switch e.kind {
	case "ln_out":
		p := e.lnOut
		sb.WriteString("[yellow]Outgoing Lightning Payment[-]\n\n")
		sb.WriteString(fmt.Sprintf("Date:     %s\n", ts))
		if e.assetName != "BTC" {
			sb.WriteString(fmt.Sprintf("Amount:   [red]-%s %s[-]\n", formatAssetAmount(e.amtAsset, e.decDisp), e.assetName))
		} else {
			sb.WriteString(fmt.Sprintf("Amount:   [red]-%s sat[-]\n", formatCommas(uint64(p.ValueSat))))
		}
		sb.WriteString(fmt.Sprintf("Fee:      %s sat\n", formatCommas(uint64(p.FeeSat))))
		sb.WriteString(fmt.Sprintf("Status:   %s\n", p.Status))
		sb.WriteString(fmt.Sprintf("Hash:     %s\n", p.PaymentHash))
		if p.PaymentPreimage != "" {
			sb.WriteString(fmt.Sprintf("Preimage: %s\n", p.PaymentPreimage))
		}
		if p.PaymentRequest != "" {
			sb.WriteString(fmt.Sprintf("Invoice:  %s\n", p.PaymentRequest))
		}

	case "ln_in":
		// Use LookupInvoice to get the freshest data.
		inv := e.lnIn
		if full, err := a.clients.LN.LookupInvoice(ctx, &lnrpc.PaymentHash{RHash: inv.RHash}); err == nil {
			inv = full
		}
		sb.WriteString("[yellow]Incoming Lightning Payment[-]\n\n")
		sb.WriteString(fmt.Sprintf("Date:     %s\n", ts))
		if e.assetName != "BTC" {
			sb.WriteString(fmt.Sprintf("Amount:   [green]+%s %s[-]\n", formatAssetAmount(e.amtAsset, e.decDisp), e.assetName))
		} else {
			sb.WriteString(fmt.Sprintf("Amount:   [green]+%s sat[-]\n", formatCommas(uint64(inv.AmtPaidSat))))
		}
		if inv.Memo != "" {
			sb.WriteString(fmt.Sprintf("Memo:     %s\n", inv.Memo))
		}
		sb.WriteString(fmt.Sprintf("Hash:     %x\n", inv.RHash))
		if len(inv.RPreimage) > 0 {
			sb.WriteString(fmt.Sprintf("Preimage: %x\n", inv.RPreimage))
		}
		if inv.PaymentRequest != "" {
			sb.WriteString(fmt.Sprintf("Invoice:  %s\n", inv.PaymentRequest))
		}

	case "onchain":
		tx := e.onchain
		dir := "[green]+[-]"
		amtColor := "[green]"
		if tx.Amount < 0 {
			dir = "[red]-[-]"
			amtColor = "[red]"
		}
		abs := tx.Amount
		if abs < 0 {
			abs = -abs
		}
		sb.WriteString(fmt.Sprintf("[yellow]Onchain Transaction  %s[-]\n\n", dir))
		sb.WriteString(fmt.Sprintf("Date:     %s\n", ts))
		sb.WriteString(fmt.Sprintf("Amount:   %s%s sat[-]\n", amtColor, formatCommas(uint64(abs))))
		sb.WriteString(fmt.Sprintf("Fee:      %s sat\n", formatCommas(uint64(tx.TotalFees))))
		sb.WriteString(fmt.Sprintf("Confs:    %d\n", tx.NumConfirmations))
		sb.WriteString(fmt.Sprintf("TxID:     %s\n", tx.TxHash))
		if tx.Label != "" {
			sb.WriteString(fmt.Sprintf("Label:    %s\n", tx.Label))
		}

	case "asset":
		xfer := e.assetXfer
		dirColor := "[green]"
		prefix := "+"
		if !e.incoming {
			dirColor = "[red]"
			prefix = "-"
		}
		sb.WriteString("[yellow]Asset Transfer[-]\n\n")
		sb.WriteString(fmt.Sprintf("Date:     %s\n", ts))
		sb.WriteString(fmt.Sprintf("Asset:    %s\n", e.assetName))
		sb.WriteString(fmt.Sprintf("Amount:   %s%s%s[-]\n", dirColor, prefix, formatAssetAmount(e.amtAsset, e.decDisp)))
		sb.WriteString(fmt.Sprintf("TxID:     %x\n", xfer.AnchorTxHash))
		sb.WriteString(fmt.Sprintf("Chain fee: %d sat\n", xfer.AnchorTxChainFees))
		if xfer.Label != "" {
			sb.WriteString(fmt.Sprintf("Label:    %s\n", xfer.Label))
		}
		sb.WriteString(fmt.Sprintf("Inputs:   %d\n", len(xfer.Inputs)))
		sb.WriteString(fmt.Sprintf("Outputs:  %d\n", len(xfer.Outputs)))
	}

	tv := tview.NewTextView().
		SetText(sb.String()).
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true)
	tv.SetBorder(true).SetTitle(" Payment Detail ")
	tv.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showPayments()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("paydetail", tv, true)
}

func (a *App) showOpenChannel() {
	form := tview.NewForm()

	var peerPubkey, localAmtStr, assetID, assetAmtStr, feeRateStr string

	form.
		AddInputField("Peer pubkey (hex)", "", 66, nil, func(t string) { peerPubkey = t }).
		AddInputField("Local BTC amount (sat)", "100000", 20, nil, func(t string) { localAmtStr = t }).
		AddInputField("Asset ID (hex, optional)", "", 64, nil, func(t string) { assetID = t }).
		AddInputField("Asset amount (if asset channel)", "", 20, nil, func(t string) { assetAmtStr = t }).
		AddInputField("Fee rate (sat/vbyte, optional)", "", 10, nil, func(t string) { feeRateStr = t }).
		AddButton("Open", func() {
			a.doOpenChannel(peerPubkey, localAmtStr, assetID, assetAmtStr, feeRateStr)
		})

	form.SetBorder(true).SetTitle(" Open Channel ")
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("openchan", form, true)
}

func (a *App) doOpenChannel(peerPubkey, localAmtStr, assetID, assetAmtStr, feeRateStr string) {
	peerBytes, err := hexToBytes(peerPubkey)
	if err != nil || len(peerBytes) != 33 {
		a.showModal("Invalid peer pubkey (33-byte compressed hex).", func() { a.showOpenChannel() })
		return
	}

	localAmt, err := strconv.ParseInt(localAmtStr, 10, 64)
	if err != nil || localAmt <= 0 {
		a.showModal("Invalid local BTC amount.", func() { a.showOpenChannel() })
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if assetID != "" {
		assetIDBytes, err := hexToBytes(assetID)
		if err != nil || len(assetIDBytes) != 32 {
			a.showModal("Invalid asset ID.", func() { a.showOpenChannel() })
			return
		}
		assetAmt, _ := strconv.ParseUint(assetAmtStr, 10, 64)
		feeRate, _ := strconv.ParseUint(feeRateStr, 10, 64)

		resp, err := a.clients.TapChannel.FundChannel(ctx, &tapchannelrpc.FundChannelRequest{
			AssetId:            assetIDBytes,
			AssetAmount:        assetAmt,
			PeerPubkey:         peerBytes,
			FeeRateSatPerVbyte: uint32(feeRate),
		})
		if err != nil {
			a.showModal(fmt.Sprintf("FundChannel error: %v", err), func() { a.showOpenChannel() })
			return
		}
		a.showModal(fmt.Sprintf("[green]Asset channel opened![-]\nTxid: %s", resp.Txid), func() { a.showDashboard() })
		return
	}

	req := &lnrpc.OpenChannelRequest{
		NodePubkey:         peerBytes,
		LocalFundingAmount: localAmt,
	}
	if feeRateStr != "" {
		if fr, err2 := strconv.ParseUint(feeRateStr, 10, 64); err2 == nil {
			req.SatPerVbyte = fr
		}
	}

	stream, err := a.clients.LN.OpenChannel(ctx, req)
	if err != nil {
		a.showModal(fmt.Sprintf("OpenChannel error: %v", err), func() { a.showOpenChannel() })
		return
	}
	update, err := stream.Recv()
	if err != nil {
		a.showModal(fmt.Sprintf("Channel update error: %v", err), func() { a.showOpenChannel() })
		return
	}
	a.showModal(fmt.Sprintf("[green]Channel opening initiated![-]\n%v", update), func() { a.showDashboard() })
}

func (a *App) showAssets() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := a.clients.Tap.ListAssets(ctx, &taprpc.ListAssetRequest{
		ScriptKeyType: &taprpc.ScriptKeyTypeQuery{
			Type: &taprpc.ScriptKeyTypeQuery_AllTypes{AllTypes: true},
		},
	})

	var text string
	if err != nil {
		text = fmt.Sprintf("[red]Error: %v[-]", err)
	} else if len(resp.Assets) == 0 {
		text = "No taproot assets found."
	} else {
		assets := resp.Assets
		sort.Slice(assets, func(i, j int) bool {
			return assets[i].AssetGenesis.Name < assets[j].AssetGenesis.Name
		})
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("[yellow]%d asset(s) found[-]\n\n", len(assets)))
		for _, asset := range assets {
			dd := uint32(0)
			if asset.DecimalDisplay != nil {
				dd = asset.DecimalDisplay.DecimalDisplay
			}
			groupKey := "(ungrouped)"
			if asset.AssetGroup != nil && len(asset.AssetGroup.TweakedGroupKey) > 0 {
				groupKey = fmt.Sprintf("%x", asset.AssetGroup.TweakedGroupKey)
			}
			sb.WriteString(fmt.Sprintf(
				"[cyan]%s[-]  [green]%s[-]\n  id:     %x\n  group:  %s\n  anchor: %s\n\n",
				asset.AssetGenesis.Name,
				formatAssetAmount(asset.Amount, dd),
				asset.AssetGenesis.AssetId,
				groupKey,
				asset.ChainAnchor.GetAnchorOutpoint(),
			))
		}
		text = sb.String()
	}

	tv := tview.NewTextView().
		SetText(text).
		SetDynamicColors(true).
		SetScrollable(true)
	tv.SetBorder(true).SetTitle(" Taproot Assets ")
	tv.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("assets", tv, true)
}

func (a *App) showModal(msg string, done func()) {
	modal := tview.NewModal().
		SetText(msg).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(_ int, _ string) { done() })
	modal.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			done()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("modal", modal, true)
}

func hexToBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 {
		s = "0" + s
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		n, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		b[i] = byte(n)
	}
	return b, nil
}

