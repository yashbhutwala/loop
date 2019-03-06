package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/nautilus/lndclient"
	"github.com/lightninglabs/nautilus/test"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	testAddr, _ = btcutil.DecodeAddress(
		"rbsHiPKwAgxeo1EQYiyzJTkA8XEmWSVAKx", nil)

	testRequest = &UnchargeRequest{
		Amount:              btcutil.Amount(50000),
		DestAddr:            testAddr,
		MaxMinerFee:         50000,
		SweepConfTarget:     2,
		MaxSwapFee:          1050,
		MaxPrepayAmount:     100,
		MaxPrepayRoutingFee: 75000,
		MaxSwapRoutingFee:   70000,
	}

	swapInvoiceDesc   = "swap"
	prepayInvoiceDesc = "prepay"
)

// TestSuccess tests the uncharge happy flow.
func TestSuccess(t *testing.T) {
	defer test.Guard(t)()

	ctx := createClientTestContext(t, nil)

	// Initiate uncharge.

	hash, err := ctx.swapClient.Uncharge(context.Background(), testRequest)
	if err != nil {
		t.Fatal(err)
	}

	ctx.assertStored()
	ctx.assertStatus(StateInitiated)

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for conf
	confIntent := ctx.AssertRegisterConf()

	testSuccess(ctx, testRequest.Amount, *hash,
		signalPrepaymentResult, signalSwapPaymentResult, false,
		confIntent,
	)
}

// TestFailOffchain tests the handling of swap for which the server failed the
// payments.
func TestFailOffchain(t *testing.T) {
	defer test.Guard(t)()

	ctx := createClientTestContext(t, nil)

	_, err := ctx.swapClient.Uncharge(context.Background(), testRequest)
	if err != nil {
		t.Fatal(err)
	}

	ctx.assertStored()
	ctx.assertStatus(StateInitiated)

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	ctx.AssertRegisterConf()

	signalSwapPaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)
	signalPrepaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)
	ctx.assertStatus(StateFailOffchainPayments)

	ctx.assertStoreFinished(StateFailOffchainPayments)

	ctx.finish()
}

// TestWrongAmount asserts that the client checks the server invoice amounts.
func TestFailWrongAmount(t *testing.T) {
	defer test.Guard(t)()

	test := func(t *testing.T, modifier func(*serverMock),
		expectedErr error) {

		ctx := createClientTestContext(t, nil)

		// Modify mock for this subtest.
		modifier(ctx.serverMock)

		_, err := ctx.swapClient.Uncharge(
			context.Background(), testRequest,
		)
		if err != expectedErr {
			t.Fatalf("Expected %v, but got %v", expectedErr, err)
		}
		ctx.finish()
	}

	t.Run("swap fee too high", func(t *testing.T) {
		test(t, func(m *serverMock) {
			m.swapInvoiceAmt += 10
		}, ErrSwapFeeTooHigh)
	})

	t.Run("prepay amount too high", func(t *testing.T) {
		test(t, func(m *serverMock) {
			// Keep total swap fee unchanged, but increase prepaid
			// portion.
			m.swapInvoiceAmt -= 10
			m.prepayInvoiceAmt += 10
		}, ErrPrepayAmountTooHigh)
	})

}

// TestResume tests that swaps in various states are properly resumed after a
// restart.
func TestResume(t *testing.T) {
	defer test.Guard(t)()

	t.Run("not expired", func(t *testing.T) {
		testResume(t, false, false, true)
	})
	t.Run("expired not revealed", func(t *testing.T) {
		testResume(t, true, false, false)
	})
	t.Run("expired revealed", func(t *testing.T) {
		testResume(t, true, true, true)
	})
}

func testResume(t *testing.T, expired, preimageRevealed, expectSuccess bool) {
	defer test.Guard(t)()

	preimage := testPreimage
	hash := sha256.Sum256(preimage[:])

	dest := test.GetDestAddr(t, 0)

	amt := btcutil.Amount(50000)

	swapPayReq, err := getInvoice(hash, amt, swapInvoiceDesc)
	if err != nil {
		t.Fatal(err)
	}

	prePayReq, err := getInvoice(hash, 100, prepayInvoiceDesc)
	if err != nil {
		t.Fatal(err)
	}

	_, senderPubKey := test.CreateKey(1)
	var senderKey [33]byte
	copy(senderKey[:], senderPubKey.SerializeCompressed())

	_, receiverPubKey := test.CreateKey(2)
	var receiverKey [33]byte
	copy(receiverKey[:], receiverPubKey.SerializeCompressed())

	state := StateInitiated
	if preimageRevealed {
		state = StatePreimageRevealed
	}
	pendingSwap := &PersistentUncharge{
		Contract: &UnchargeContract{
			DestAddr:          dest,
			SwapInvoice:       swapPayReq,
			SweepConfTarget:   2,
			MaxSwapRoutingFee: 70000,
			SwapContract: SwapContract{
				Preimage:        preimage,
				AmountRequested: amt,
				CltvExpiry:      744,
				ReceiverKey:     receiverKey,
				SenderKey:       senderKey,
				MaxSwapFee:      60000,
				PrepayInvoice:   prePayReq,
				MaxMinerFee:     50000,
			},
		},
		Events: []*PersistentUnchargeEvent{
			{
				State: state,
			},
		},
		Hash: hash,
	}

	if expired {
		// Set cltv expiry so that it has already expired at the test
		// block height.
		pendingSwap.Contract.CltvExpiry = 610
	}

	ctx := createClientTestContext(t, []*PersistentUncharge{pendingSwap})

	if preimageRevealed {
		ctx.assertStatus(StatePreimageRevealed)
	} else {
		ctx.assertStatus(StateInitiated)
	}

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for conf
	confIntent := ctx.AssertRegisterConf()

	signalSwapPaymentResult(nil)
	signalPrepaymentResult(nil)

	if !expectSuccess {
		ctx.assertStatus(StateFailTimeout)
		ctx.assertStoreFinished(StateFailTimeout)
		ctx.finish()
		return
	}

	// Because there is no reliable payment yet, an invoice is assumed to be
	// paid after resume.

	testSuccess(ctx, amt, hash,
		func(r error) {},
		func(r error) {},
		preimageRevealed,
		confIntent,
	)
}

func testSuccess(ctx *testContext, amt btcutil.Amount, hash lntypes.Hash,
	signalPrepaymentResult, signalSwapPaymentResult func(error),
	preimageRevealed bool, confIntent *test.ConfRegistration) {

	htlcOutpoint := ctx.publishHtlc(confIntent.PkScript, amt)

	signalPrepaymentResult(nil)

	ctx.AssertRegisterSpendNtfn(confIntent.PkScript)

	// Publish tick.
	ctx.expiryChan <- testTime

	if !preimageRevealed {
		ctx.assertStatus(StatePreimageRevealed)
		ctx.assertStorePreimageReveal()
	}

	// Expect client on-chain sweep of HTLC.
	sweepTx := ctx.ReceiveTx()

	if !bytes.Equal(sweepTx.TxIn[0].PreviousOutPoint.Hash[:],
		htlcOutpoint.Hash[:]) {
		ctx.T.Fatalf("client not sweeping from htlc tx")
	}

	// Check preimage.
	clientPreImage := sweepTx.TxIn[0].Witness[1]
	clientPreImageHash := sha256.Sum256(clientPreImage)
	if clientPreImageHash != hash {
		ctx.T.Fatalf("incorrect preimage")
	}

	// Simulate server pulling payment.
	signalSwapPaymentResult(nil)

	ctx.NotifySpend(sweepTx, 0)

	ctx.assertStatus(StateSuccess)

	ctx.assertStoreFinished(StateSuccess)

	ctx.finish()
}