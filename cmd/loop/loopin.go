package main

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
)

var (
	lastHopFlag = cli.StringFlag{
		Name:  "last_hop",
		Usage: "the pubkey of the last hop to use for this swap",
	}

	loopInCommand = cli.Command{
		Name:      "in",
		Usage:     "perform an on-chain to off-chain swap (loop in)",
		ArgsUsage: "amt",
		Description: `
		Send the amount in satoshis specified by the amt argument off-chain.`,
		Flags: []cli.Flag{
			cli.Uint64Flag{
				Name:  "amt",
				Usage: "the amount in satoshis to loop in",
			},
			cli.BoolFlag{
				Name:  "external",
				Usage: "expect htlc to be published externally",
			},
			lastHopFlag,
		},
		Action: loopIn,
	}
)

func loopIn(ctx *cli.Context) error {
	args := ctx.Args()

	var amtStr string
	switch {
	case ctx.IsSet("amt"):
		amtStr = ctx.String("amt")
	case ctx.NArg() > 0:
		amtStr = args[0]
		args = args.Tail()
	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "in")
	}

	amt, err := parseAmt(amtStr)
	if err != nil {
		return err
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	external := ctx.Bool("external")
	quote, err := client.GetLoopInQuote(
		context.Background(),
		&looprpc.QuoteRequest{
			Amt:          int64(amt),
			ExternalHtlc: external,
		},
	)
	if err != nil {
		return err
	}

	// For loop in, the fee estimation is handed to lnd which tries to
	// construct a real transaction to sample realistic fees to pay to the
	// HTLC. If the wallet doesn't have enough funds to create this TX, we
	// know it won't have enough to pay the real transaction either. It
	// makes sense to abort the loop in this case.
	if !external && quote.MinerFee == int64(loop.MinerFeeEstimationFailed) {
		return fmt.Errorf("miner fee estimation not " +
			"possible, lnd has insufficient funds to " +
			"create a sample transaction for selected " +
			"amount")
	}

	limits := getInLimits(amt, quote)
	err = displayLimits(swap.TypeIn, amt, limits, external, "")
	if err != nil {
		return err
	}

	req := &looprpc.LoopInRequest{
		Amt:          int64(amt),
		MaxMinerFee:  int64(limits.maxMinerFee),
		MaxSwapFee:   int64(limits.maxSwapFee),
		ExternalHtlc: external,
	}

	if ctx.IsSet(lastHopFlag.Name) {
		lastHop, err := route.NewVertexFromStr(
			ctx.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}

		req.LastHop = lastHop[:]
	}

	resp, err := client.LoopIn(context.Background(), req)
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated\n")
	fmt.Printf("ID:           %v\n", resp.Id)
	fmt.Printf("HTLC address: %v\n", resp.HtlcAddress)
	fmt.Println()
	fmt.Printf("Run `loop monitor` to monitor progress.\n")

	return nil
}

func getInLimits(amt btcutil.Amount, quote *looprpc.QuoteResponse) *limits {
	return &limits{
		// Apply a multiplier to the estimated miner fee, to not get
		// the swap canceled because fees increased in the mean time.
		maxMinerFee: btcutil.Amount(quote.MinerFee) * 3,
		maxSwapFee:  btcutil.Amount(quote.SwapFee),
	}
}
