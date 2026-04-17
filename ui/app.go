package ui

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
	tapp    *tview.Application
	clients *client.Clients
	pages   *tview.Pages

	assets   []*taprpc.Asset
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

	tapResp, err := a.clients.Tap.ListAssets(ctx, &taprpc.ListAssetRequest{})
	if err == nil && tapResp != nil {
		a.assets = tapResp.Assets
	}
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

	walletBal, _ := a.clients.LN.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
	chanBal, _ := a.clients.LN.ChannelBalance(ctx, &lnrpc.ChannelBalanceRequest{})

	header := tview.NewTextView().
		SetText(fmt.Sprintf(" Tassilo  |  %s  |  %s", a.nodeInfo.Alias, a.nodeInfo.IdentityPubkey[:16]+"…")).
		SetTextAlign(tview.AlignCenter).
		SetDynamicColors(true)

	onchainText := fmt.Sprintf(
		"[yellow]Onchain Bitcoin[-]\n"+
			"  Confirmed:   [green]%d sat[-]\n"+
			"  Unconfirmed: [grey]%d sat[-]\n",
		walletBal.GetConfirmedBalance(),
		walletBal.GetUnconfirmedBalance(),
	)

	offchainText := fmt.Sprintf(
		"[yellow]Lightning (BTC)[-]\n"+
			"  Local:  [green]%d sat[-]\n"+
			"  Remote: [grey]%d sat[-]\n",
		chanBal.GetLocalBalance().GetSat(),
		chanBal.GetRemoteBalance().GetSat(),
	)

	assetLines := buildAssetBalanceText(a.assets)

	balanceView := tview.NewTextView().
		SetText(onchainText + "\n" + offchainText + "\n" + assetLines).
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
	menu.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.tapp.Stop()
			return nil
		}
		return event
	})

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

func buildAssetBalanceText(assets []*taprpc.Asset) string {
	if len(assets) == 0 {
		return "[yellow]Taproot Assets[-]\n  (none)\n"
	}

	type entry struct {
		name   string
		amount uint64
	}
	totals := make(map[string]*entry)
	for _, asset := range assets {
		id := fmt.Sprintf("%x", asset.AssetGenesis.AssetId)
		if len(id) > 16 {
			id = id[:16]
		}
		if _, ok := totals[id]; !ok {
			totals[id] = &entry{name: asset.AssetGenesis.Name}
		}
		totals[id].amount += asset.Amount
	}

	var sb strings.Builder
	sb.WriteString("[yellow]Taproot Assets[-]\n")
	for id, e := range totals {
		sb.WriteString(fmt.Sprintf("  %-20s [green]%d[-]  (id: %s…)\n", e.name, e.amount, id))
	}
	return sb.String()
}

func (a *App) showReceive() {
	form := tview.NewForm()

	var assetID, amountStr, memoStr string

	form.AddInputField("Asset ID (hex, empty=BTC)", "", 64, nil, func(t string) { assetID = t }).
		AddInputField("Amount (asset units or sat)", "", 20, nil, func(t string) { amountStr = t }).
		AddInputField("Memo (optional)", "", 60, nil, func(t string) { memoStr = t }).
		AddButton("Generate", func() {
			a.doCreateInvoice(assetID, amountStr, memoStr)
		}).
		AddButton("Back", func() { a.showDashboard() })

	form.SetBorder(true).SetTitle(" Receive — Create Invoice (Esc=back) ")
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("receive", form, true)
}

func (a *App) doCreateInvoice(assetID, amountStr, memo string) {
	amount, err := strconv.ParseInt(amountStr, 10, 64)
	if err != nil || amount <= 0 {
		a.showModal("Invalid amount.", func() { a.showReceive() })
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var payReq string

	if assetID == "" {
		resp, err := a.clients.LN.AddInvoice(ctx, &lnrpc.Invoice{
			Memo:  memo,
			Value: amount,
		})
		if err != nil {
			a.showModal(fmt.Sprintf("Error: %v", err), func() { a.showReceive() })
			return
		}
		payReq = resp.PaymentRequest
	} else {
		assetIDBytes, err := hexToBytes(assetID)
		if err != nil || len(assetIDBytes) != 32 {
			a.showModal("Invalid asset ID (must be 32-byte hex).", func() { a.showReceive() })
			return
		}
		resp, err := a.clients.TapChannel.AddInvoice(ctx, &tapchannelrpc.AddInvoiceRequest{
			AssetId:     assetIDBytes,
			AssetAmount: uint64(amount),
			InvoiceRequest: &lnrpc.Invoice{
				Memo: memo,
			},
		})
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
	tv := tview.NewTextView().
		SetText(fmt.Sprintf("[yellow]Payment Request[-]\n\n%s\n\n[grey](Esc to go back)[-]", payReq)).
		SetDynamicColors(true).
		SetWordWrap(true).
		SetScrollable(true)
	tv.SetBorder(true).SetTitle(" Invoice ")
	tv.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("invoice", tv, true)
}

func (a *App) showSend() {
	form := tview.NewForm()
	var payReqStr string

	form.AddInputField("Payment Request (bolt11)", "", 0, nil, func(t string) { payReqStr = t }).
		AddButton("Pay", func() {
			a.doSendPayment(payReqStr)
		}).
		AddButton("Back", func() { a.showDashboard() })

	form.SetBorder(true).SetTitle(" Send — Pay Invoice (Esc=back) ")
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.showDashboard()
			return nil
		}
		return event
	})
	a.pages.AddAndSwitchToPage("send", form, true)
}

func (a *App) doSendPayment(payReq string) {
	if strings.TrimSpace(payReq) == "" {
		a.showModal("Payment request is empty.", func() { a.showSend() })
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Try as taproot asset payment first.
	tapStream, err := a.clients.TapChannel.SendPayment(ctx, &tapchannelrpc.SendPaymentRequest{
		PaymentRequest: &routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			TimeoutSeconds: 60,
			FeeLimitSat:    10000,
		},
	})
	if err == nil {
		for {
			resp, err := tapStream.Recv()
			if err != nil {
				break
			}
			if pr := resp.GetPaymentResult(); pr != nil {
				if pr.Status == lnrpc.Payment_SUCCEEDED {
					a.showModal(fmt.Sprintf("[green]Asset payment sent![-]\nPreimage: %x", pr.PaymentPreimage), func() { a.showDashboard() })
					return
				}
				if pr.Status == lnrpc.Payment_FAILED {
					a.showModal(fmt.Sprintf("Asset payment failed: %s", pr.FailureReason), func() { a.showSend() })
					return
				}
			}
		}
	}

	// Fall back to plain BTC payment.
	router := routerrpc.NewRouterClient(a.clients.Conn())
	stream, err2 := router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest: payReq,
		TimeoutSeconds: 60,
		FeeLimitSat:    10000,
	})
	if err2 != nil {
		a.showModal(fmt.Sprintf("Payment failed:\n%v", err2), func() { a.showSend() })
		return
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			a.showModal(fmt.Sprintf("Stream error: %v", err), func() { a.showSend() })
			return
		}
		if resp.Status == lnrpc.Payment_SUCCEEDED {
			a.showModal(fmt.Sprintf("[green]Payment sent![-]\nPreimage: %x", resp.PaymentPreimage), func() { a.showDashboard() })
			return
		}
		if resp.Status == lnrpc.Payment_FAILED {
			a.showModal(fmt.Sprintf("Payment failed: %s", resp.FailureReason), func() { a.showSend() })
			return
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
		}).
		AddButton("Back", func() { a.showDashboard() })

	form.SetBorder(true).SetTitle(" Open Channel (Esc=back) ")
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

	resp, err := a.clients.Tap.ListAssets(ctx, &taprpc.ListAssetRequest{})

	var text string
	if err != nil {
		text = fmt.Sprintf("[red]Error: %v[-]", err)
	} else if len(resp.Assets) == 0 {
		text = "No taproot assets found."
	} else {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("[yellow]%d asset(s) found[-]\n\n", len(resp.Assets)))
		for _, asset := range resp.Assets {
			sb.WriteString(fmt.Sprintf(
				"[cyan]%-24s[-]  amount=[green]%d[-]\n  id:      %x\n  group:   %s\n  anchor:  %s\n\n",
				asset.AssetGenesis.Name,
				asset.Amount,
				asset.AssetGenesis.AssetId,
				groupKeyStr(asset),
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

func groupKeyStr(a *taprpc.Asset) string {
	if a.AssetGroup == nil || len(a.AssetGroup.TweakedGroupKey) == 0 {
		return "(ungrouped)"
	}
	k := fmt.Sprintf("%x", a.AssetGroup.TweakedGroupKey)
	if len(k) > 24 {
		return k[:24] + "…"
	}
	return k
}
