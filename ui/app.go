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

	"github.com/gdamore/tcell/v2"
	"github.com/lightninglabs/tassilo/client"
	"github.com/rivo/tview"

	taprpc "github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
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
}

// buildGroupMetaMap returns a group-key → {name, decimalDisplay} map for all
// assets (including channel assets) so both sections can look up the right
// display name and scale factor.
func buildGroupMetaMap(assets []*taprpc.Asset) map[string]*groupMeta {
	m := make(map[string]*groupMeta)
	for _, a := range assets {
		var key string
		if a.AssetGroup != nil && len(a.AssetGroup.TweakedGroupKey) > 0 {
			key = fmt.Sprintf("%x", a.AssetGroup.TweakedGroupKey)
		} else {
			key = fmt.Sprintf("%x", a.AssetGenesis.AssetId)
		}
		if _, exists := m[key]; !exists {
			dd := uint32(0)
			if a.DecimalDisplay != nil {
				dd = a.DecimalDisplay.DecimalDisplay
			}
			m[key] = &groupMeta{name: a.AssetGenesis.Name, decimalDisplay: dd}
		}
	}
	return m
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

// formatCommas inserts thousand separators into an integer.
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
		AddInputField("Amount (units or sat)", "", 20, nil, func(t string) { amountStr = t }).
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
	amount, err := strconv.ParseUint(amountStr, 10, 64)
	if err != nil || amount == 0 {
		a.showModal("Invalid amount.", func() { a.showReceive() })
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var payReq string

	isBTC := assetIDHex == "" && groupKeyHex == ""
	if isBTC {
		resp, err := a.clients.LN.AddInvoice(ctx, &lnrpc.Invoice{
			Memo:  memo,
			Value: int64(amount),
		})
		if err != nil {
			a.showModal(fmt.Sprintf("Error: %v", err), func() { a.showReceive() })
			return
		}
		payReq = resp.PaymentRequest
	} else {
		// Scale the human-readable amount by 10^decimalDisplay.
		scaledAmount := amount
		for i := uint32(0); i < decimalDisplay; i++ {
			scaledAmount *= 10
		}

		req := &tapchannelrpc.AddInvoiceRequest{
			AssetAmount: scaledAmount,
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

