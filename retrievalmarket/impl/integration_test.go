package retrievalimpl_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/builtin/paych"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-log/v2"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/ipld/go-ipld-prime/traversal/selector/builder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-fil-markets/pieceio/cario"
	"github.com/filecoin-project/go-fil-markets/piecestore"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket"
	retrievalimpl "github.com/filecoin-project/go-fil-markets/retrievalmarket/impl"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket/impl/testnodes"
	rmnet "github.com/filecoin-project/go-fil-markets/retrievalmarket/network"
	"github.com/filecoin-project/go-fil-markets/shared"
	tut "github.com/filecoin-project/go-fil-markets/shared_testutil"
)

func TestClientCanMakeQueryToProvider(t *testing.T) {
	bgCtx := context.Background()
	payChAddr := address.TestAddress

	client, expectedCIDs, missingPiece, expectedQR, retrievalPeer, _ := requireSetupTestClientAndProvider(bgCtx, t, payChAddr)

	t.Run("when piece is found, returns piece and price data", func(t *testing.T) {
		expectedQR.Status = retrievalmarket.QueryResponseAvailable
		actualQR, err := client.Query(bgCtx, retrievalPeer, expectedCIDs[0], retrievalmarket.QueryParams{})

		assert.NoError(t, err)
		assert.Equal(t, expectedQR, actualQR)
	})

	t.Run("when piece is not found, returns unavailable", func(t *testing.T) {
		expectedQR.PieceCIDFound = retrievalmarket.QueryItemUnavailable
		expectedQR.Status = retrievalmarket.QueryResponseUnavailable
		expectedQR.Size = 0
		actualQR, err := client.Query(bgCtx, retrievalPeer, missingPiece, retrievalmarket.QueryParams{})
		assert.NoError(t, err)
		assert.Equal(t, expectedQR, actualQR)
	})

	t.Run("when there is some other error, returns error", func(t *testing.T) {
		unknownPiece := tut.GenerateCids(1)[0]
		expectedQR.Status = retrievalmarket.QueryResponseError
		expectedQR.Message = "get cid info: GetCIDInfo failed"
		actualQR, err := client.Query(bgCtx, retrievalPeer, unknownPiece, retrievalmarket.QueryParams{})
		assert.NoError(t, err)
		assert.Equal(t, expectedQR, actualQR)
	})

}

func TestProvider_Stop(t *testing.T) {
	bgCtx := context.Background()
	payChAddr := address.TestAddress
	client, expectedCIDs, _, _, retrievalPeer, provider := requireSetupTestClientAndProvider(bgCtx, t, payChAddr)
	require.NoError(t, provider.Stop())
	_, err := client.Query(bgCtx, retrievalPeer, expectedCIDs[0], retrievalmarket.QueryParams{})
	assert.EqualError(t, err, "protocol not supported")
}

func requireSetupTestClientAndProvider(bgCtx context.Context, t *testing.T, payChAddr address.Address) (retrievalmarket.RetrievalClient,
	[]cid.Cid,
	cid.Cid,
	retrievalmarket.QueryResponse,
	retrievalmarket.RetrievalPeer,
	retrievalmarket.RetrievalProvider) {
	testData := tut.NewLibp2pTestData(bgCtx, t)
	nw1 := rmnet.NewFromLibp2pHost(testData.Host1)
	cids := tut.GenerateCids(2)
	rcNode1 := testnodes.NewTestRetrievalClientNode(testnodes.TestRetrievalClientNodeParams{
		PayCh:          payChAddr,
		CreatePaychCID: cids[0],
		AddFundsCID:    cids[1],
	})
	client, err := retrievalimpl.NewClient(nw1, testData.Bs1, rcNode1, &tut.TestPeerResolver{}, testData.Ds1, testData.RetrievalStoredCounter1)
	require.NoError(t, err)
	nw2 := rmnet.NewFromLibp2pHost(testData.Host2)
	providerNode := testnodes.NewTestRetrievalProviderNode()
	pieceStore := tut.NewTestPieceStore()
	expectedCIDs := tut.GenerateCids(3)
	expectedPieceCIDs := tut.GenerateCids(3)
	missingCID := tut.GenerateCids(1)[0]
	expectedQR := tut.MakeTestQueryResponse()

	pieceStore.ExpectMissingCID(missingCID)
	for i, c := range expectedCIDs {
		pieceStore.ExpectCID(c, piecestore.CIDInfo{
			PieceBlockLocations: []piecestore.PieceBlockLocation{
				{
					PieceCID: expectedPieceCIDs[i],
				},
			},
		})
	}
	for i, piece := range expectedPieceCIDs {
		pieceStore.ExpectPiece(piece, piecestore.PieceInfo{
			Deals: []piecestore.DealInfo{
				{
					Length: expectedQR.Size * uint64(i+1),
				},
			},
		})
	}

	paymentAddress := address.TestAddress2
	provider, err := retrievalimpl.NewProvider(paymentAddress, providerNode, nw2, pieceStore, testData.Bs2, testData.Ds2)
	require.NoError(t, err)

	provider.SetPaymentInterval(expectedQR.MaxPaymentInterval, expectedQR.MaxPaymentIntervalIncrease)
	provider.SetPricePerByte(expectedQR.MinPricePerByte)
	require.NoError(t, provider.Start())

	retrievalPeer := retrievalmarket.RetrievalPeer{
		Address: paymentAddress,
		ID:      testData.Host2.ID(),
	}
	return client, expectedCIDs, missingCID, expectedQR, retrievalPeer, provider
}

func TestClientCanMakeDealWithProvider(t *testing.T) {
	// -------- SET UP PROVIDER

	ssb := builder.NewSelectorSpecBuilder(basicnode.Style.Any)

	partialSelector := ssb.ExploreFields(func(specBuilder builder.ExploreFieldsSpecBuilder) {
		specBuilder.Insert("Links", ssb.ExploreIndex(0, ssb.ExploreFields(func(specBuilder builder.ExploreFieldsSpecBuilder) {
			specBuilder.Insert("Hash", ssb.Matcher())
		})))
	}).Node()

	testCases := []struct {
		name                          string
		filename                      string
		filesize                      uint64
		voucherAmts                   []abi.TokenAmount
		selector                      ipld.Node
		paramsV1, unsealing, addFunds bool
	}{
		{name: "1 block file retrieval succeeds",
			filename:    "lorem_under_1_block.txt",
			filesize:    410,
			voucherAmts: []abi.TokenAmount{abi.NewTokenAmount(410000)}},
		{name: "1 block file retrieval succeeds with existing payment channel",
			filename:    "lorem_under_1_block.txt",
			filesize:    410,
			voucherAmts: []abi.TokenAmount{abi.NewTokenAmount(410000)},
			addFunds:    true},
		{name: "1 block file retrieval succeeds with unsealing",
			filename:    "lorem_under_1_block.txt",
			filesize:    410,
			voucherAmts: []abi.TokenAmount{abi.NewTokenAmount(410000)},
			unsealing:   true},
		{name: "multi-block file retrieval succeeds",
			filename:    "lorem.txt",
			filesize:    19000,
			voucherAmts: []abi.TokenAmount{abi.NewTokenAmount(10136000), abi.NewTokenAmount(9784000)}},
		{name: "multi-block file retrieval succeeds with unsealing",
			filename:    "lorem.txt",
			filesize:    19000,
			voucherAmts: []abi.TokenAmount{abi.NewTokenAmount(10136000), abi.NewTokenAmount(9784000)},
			unsealing:   true},
		{name: "multi-block file retrieval succeeds with V1 params and AllSelector",
			filename:    "lorem.txt",
			filesize:    19000,
			voucherAmts: []abi.TokenAmount{abi.NewTokenAmount(10136000), abi.NewTokenAmount(9784000)},
			paramsV1:    true,
			selector:    shared.AllSelector()},
		{name: "partial file retrieval succeeds with V1 params and selector recursion depth 1",
			filename:    "lorem.txt",
			filesize:    1024,
			voucherAmts: []abi.TokenAmount{abi.NewTokenAmount(1944000)},
			paramsV1:    true,
			selector:    partialSelector},
	}

	for i, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			bgCtx := context.Background()
			clientPaymentChannel, err := address.NewIDAddress(uint64(i * 10))
			require.NoError(t, err)

			testData := tut.NewLibp2pTestData(bgCtx, t)

			// Inject a unixFS file on the provider side to its blockstore
			// obtained via `ls -laf` on this file

			fpath := filepath.Join("retrievalmarket", "impl", "fixtures", testCase.filename)

			pieceLink := testData.LoadUnixFSFile(t, fpath, true)
			c, ok := pieceLink.(cidlink.Link)
			require.True(t, ok)
			payloadCID := c.Cid
			providerPaymentAddr, err := address.NewIDAddress(uint64(i * 99))
			require.NoError(t, err)
			paymentInterval := uint64(10000)
			paymentIntervalIncrease := uint64(1000)
			pricePerByte := abi.NewTokenAmount(1000)

			expectedQR := retrievalmarket.QueryResponse{
				Size:                       1024,
				PaymentAddress:             providerPaymentAddr,
				MinPricePerByte:            pricePerByte,
				MaxPaymentInterval:         paymentInterval,
				MaxPaymentIntervalIncrease: paymentIntervalIncrease,
			}

			providerNode := testnodes.NewTestRetrievalProviderNode()
			var pieceInfo piecestore.PieceInfo
			if testCase.unsealing {
				pieceInfo = setupUnseal(t, bgCtx, testData, payloadCID, pieceInfo, providerNode)
			} else {
				pieceInfo = piecestore.PieceInfo{
					Deals: []piecestore.DealInfo{
						{
							Length: expectedQR.Size,
						},
					},
				}
			}

			provider := setupProvider(t, testData, payloadCID, pieceInfo, expectedQR, providerPaymentAddr, providerNode)

			retrievalPeer := &retrievalmarket.RetrievalPeer{Address: providerPaymentAddr, ID: testData.Host2.ID()}

			expectedVoucher := tut.MakeTestSignedVoucher()

			// just make sure there is enough to cover the transfer
			expectedTotal := big.Mul(pricePerByte, abi.NewTokenAmount(int64(testCase.filesize*2)))

			// voucherAmts are pulled from the actual answer so the expected keys in the test node match up.
			// later we compare the voucher values.  The last voucherAmt is a remainder
			proof := []byte("")
			for _, voucherAmt := range testCase.voucherAmts {
				require.NoError(t, providerNode.ExpectVoucher(clientPaymentChannel, expectedVoucher, proof, voucherAmt, voucherAmt, nil))
			}

			// ------- SET UP CLIENT
			nw1 := rmnet.NewFromLibp2pHost(testData.Host1)

			createdChan, newLaneAddr, createdVoucher, client, err := setupClient(clientPaymentChannel, expectedVoucher, nw1, testData, testCase.addFunds)
			require.NoError(t, err)

			clientDealStateChan := make(chan retrievalmarket.ClientDealState)
			client.SubscribeToEvents(func(event retrievalmarket.ClientEvent, state retrievalmarket.ClientDealState) {
				switch event {
				case retrievalmarket.ClientEventComplete:
					clientDealStateChan <- state
				default:
					logClientDealState(t, state)
				}
			})

			providerDealStateChan := make(chan retrievalmarket.ProviderDealState)
			provider.SubscribeToEvents(func(event retrievalmarket.ProviderEvent, state retrievalmarket.ProviderDealState) {
				switch event {
				case retrievalmarket.ProviderEventComplete:
					providerDealStateChan <- state
				default:
					logProviderDealState(t, state)
				}
			})

			// **** Send the query for the Piece
			// set up retrieval params
			resp, err := client.Query(bgCtx, *retrievalPeer, payloadCID, retrievalmarket.QueryParams{})
			require.NoError(t, err)
			require.Equal(t, retrievalmarket.QueryResponseAvailable, resp.Status)

			var rmParams retrievalmarket.Params
			if testCase.paramsV1 {
				rmParams = retrievalmarket.NewParamsV1(pricePerByte, paymentInterval, paymentIntervalIncrease, testCase.selector, nil)

			} else {
				rmParams = retrievalmarket.NewParamsV0(pricePerByte, paymentInterval, paymentIntervalIncrease)
			}

			// *** Retrieve the piece, simulating a shutdown in between
			did, err := client.Retrieve(bgCtx, payloadCID, rmParams, expectedTotal, retrievalPeer.ID, clientPaymentChannel, retrievalPeer.Address)
			require.NoError(t, err)

			require.Equal(t, did, retrievalmarket.DealID(0))

			// verify that client subscribers will be notified of state changes
			ctx, cancel := context.WithTimeout(bgCtx, 5*time.Second)
			defer cancel()
			var clientDealState retrievalmarket.ClientDealState
			select {
			case <-ctx.Done():
				t.Error("deal never completed")
				t.FailNow()
			case clientDealState = <-clientDealStateChan:
			}
			assert.Equal(t, clientDealState.PaymentInfo.Lane, expectedVoucher.Lane)
			require.NotNil(t, createdChan)
			require.Equal(t, expectedTotal, createdChan.amt)
			require.Equal(t, clientPaymentChannel, *newLaneAddr)
			// verify that the voucher was saved/seen by the client with correct values
			require.NotNil(t, createdVoucher)
			tut.TestVoucherEquality(t, createdVoucher, expectedVoucher)

			ctx, cancel = context.WithTimeout(bgCtx, 5*time.Second)
			defer cancel()
			var providerDealState retrievalmarket.ProviderDealState
			select {
			case <-ctx.Done():
				t.Error("provider never saw completed deal")
				t.FailNow()
			case providerDealState = <-providerDealStateChan:
			}

			assert.Equal(t, int(retrievalmarket.DealStatusCompleted), int(providerDealState.Status))

			// verify that the provider saved the same voucher values
			providerNode.VerifyExpectations(t)
			testData.VerifyFileTransferred(t, pieceLink, false, testCase.filesize)
		})
	}

}

/// =======================
func TestStartStopProvider(t *testing.T) {
	log.SetDebugLogging()
	filename := "lorem.txt"
	filesize := 19000
	voucherAmts := []abi.TokenAmount{abi.NewTokenAmount(10136000), abi.NewTokenAmount(9784000)}

	bgCtx := context.Background()
	clientPaymentChannel, err := address.NewIDAddress(uint64(9))
	require.NoError(t, err)

	testData := tut.NewLibp2pTestData(bgCtx, t)
	fpath := filepath.Join("retrievalmarket", "impl", "fixtures", filename)

	pieceLink := testData.LoadUnixFSFile(t, fpath, true)
	c, ok := pieceLink.(cidlink.Link)
	require.True(t, ok)
	payloadCID := c.Cid
	providerPaymentAddr, err := address.NewIDAddress(99)
	require.NoError(t, err)
	paymentInterval := uint64(10000)
	paymentIntervalIncrease := uint64(1000)
	pricePerByte := abi.NewTokenAmount(1000)

	expectedQR := retrievalmarket.QueryResponse{
		Size:                       1024,
		PaymentAddress:             providerPaymentAddr,
		MinPricePerByte:            pricePerByte,
		MaxPaymentInterval:         paymentInterval,
		MaxPaymentIntervalIncrease: paymentIntervalIncrease,
	}

	providerNode := testnodes.NewTestRetrievalProviderNode()

	pieceInfo := piecestore.PieceInfo{
		Deals: []piecestore.DealInfo{
			{
				Length: expectedQR.Size,
			},
		},
	}

	nw2 := rmnet.NewFromLibp2pHost(testData.Host2)
	pieceStore := tut.NewTestPieceStore()
	expectedPiece := tut.GenerateCids(1)[0]
	cidInfo := piecestore.CIDInfo{
		PieceBlockLocations: []piecestore.PieceBlockLocation{
			{
				PieceCID: expectedPiece,
			},
		},
	}
	pieceStore.ExpectCID(payloadCID, cidInfo)
	pieceStore.ExpectPiece(expectedPiece, pieceInfo)
	provider, err := retrievalimpl.NewProvider(providerPaymentAddr, providerNode, nw2, pieceStore, testData.Bs2, testData.Ds2)
	require.NoError(t, err)
	provider.SetPaymentInterval(expectedQR.MaxPaymentInterval, expectedQR.MaxPaymentIntervalIncrease)
	provider.SetPricePerByte(expectedQR.MinPricePerByte)
	require.NoError(t, provider.Start())

	retrievalPeer := &retrievalmarket.RetrievalPeer{Address: providerPaymentAddr, ID: testData.Host2.ID()}
	expectedVoucher := tut.MakeTestSignedVoucher()
	expectedTotal := big.Mul(pricePerByte, abi.NewTokenAmount(int64(filesize*2)))

	proof := []byte("")
	for _, voucherAmt := range voucherAmts {
		require.NoError(t, providerNode.ExpectVoucher(clientPaymentChannel, expectedVoucher, proof, voucherAmt, voucherAmt, nil))
	}
	// ------- SET UP CLIENT
	nw1 := rmnet.NewFromLibp2pHost(testData.Host1)

	createdChan, _, createdVoucher, client, err := setupClient(clientPaymentChannel, expectedVoucher, nw1, testData, false)
	require.NoError(t, err)

	// **** Send the query for the Piece
	// set up retrieval params
	resp, err := client.Query(bgCtx, *retrievalPeer, payloadCID, retrievalmarket.QueryParams{})
	require.NoError(t, err)
	require.Equal(t, retrievalmarket.QueryResponseAvailable, resp.Status)

	clientDealStateChan := make(chan retrievalmarket.ClientDealState)
	client.SubscribeToEvents(func(event retrievalmarket.ClientEvent, state retrievalmarket.ClientDealState) {
		switch event {
		case retrievalmarket.ClientEventComplete:
			clientDealStateChan <- state
		}
	})

	rmParams := retrievalmarket.NewParamsV1(pricePerByte, paymentInterval, paymentIntervalIncrease, shared.AllSelector(), nil)
	// *** Retrieve the piece, simulating a shutdown in between
	did, err := client.Retrieve(bgCtx, payloadCID, rmParams, expectedTotal, retrievalPeer.ID, clientPaymentChannel, retrievalPeer.Address)
	require.NoError(t, err)

	require.NoError(t, provider.Stop())

	provider2, err := retrievalimpl.NewProvider(providerPaymentAddr, providerNode, nw2, pieceStore, testData.Bs2, testData.Ds2)
	require.NoError(t, err)
	provider2.SetPaymentInterval(expectedQR.MaxPaymentInterval, expectedQR.MaxPaymentIntervalIncrease)
	provider2.SetPricePerByte(expectedQR.MinPricePerByte)
	require.NoError(t, provider2.Start())

	require.Equal(t, did, retrievalmarket.DealID(0))
	// verify that the voucher was saved/seen by the client with correct values

	// verify that client subscribers will be notified of state changes
	ctx, cancel := context.WithTimeout(bgCtx, 5*time.Second)
	defer cancel()
	var clientDealState retrievalmarket.ClientDealState
	select {
	case <-ctx.Done():
		t.Error("deal never completed")
		t.FailNow()
	case clientDealState = <-clientDealStateChan:
	}
	assert.Equal(t, clientDealState.PaymentInfo.Lane, expectedVoucher.Lane)
	require.NotNil(t, createdChan)
	require.Equal(t, expectedTotal, createdChan.amt)
	// verify that the voucher was saved/seen by the client with correct values
	require.NotNil(t, createdVoucher)
	tut.TestVoucherEquality(t, createdVoucher, expectedVoucher)

	// verify that the provider saved the same voucher values
	providerNode.VerifyExpectations(t)
	testData.VerifyFileTransferred(t, pieceLink, false, uint64(filesize))
}

func TestStartStopClient(t *testing.T) {
	log.SetDebugLogging()
	filename := "lorem.txt"
	filesize := 19000
	voucherAmts := []abi.TokenAmount{abi.NewTokenAmount(10136000), abi.NewTokenAmount(9784000)}

	bgCtx := context.Background()
	clientPaymentChannel, err := address.NewIDAddress(uint64(9))
	require.NoError(t, err)

	testData := tut.NewLibp2pTestData(bgCtx, t)
	fpath := filepath.Join("retrievalmarket", "impl", "fixtures", filename)

	pieceLink := testData.LoadUnixFSFile(t, fpath, true)
	c, ok := pieceLink.(cidlink.Link)
	require.True(t, ok)
	payloadCID := c.Cid
	providerPaymentAddr, err := address.NewIDAddress(99)
	require.NoError(t, err)
	paymentInterval := uint64(10000)
	paymentIntervalIncrease := uint64(1000)
	pricePerByte := abi.NewTokenAmount(1000)

	expectedQR := retrievalmarket.QueryResponse{
		Size:                       1024,
		PaymentAddress:             providerPaymentAddr,
		MinPricePerByte:            pricePerByte,
		MaxPaymentInterval:         paymentInterval,
		MaxPaymentIntervalIncrease: paymentIntervalIncrease,
	}

	providerNode := testnodes.NewTestRetrievalProviderNode()

	pieceInfo := piecestore.PieceInfo{
		Deals: []piecestore.DealInfo{
			{
				Length: expectedQR.Size,
			},
		},
	}

	nw2 := rmnet.NewFromLibp2pHost(testData.Host2)
	pieceStore := tut.NewTestPieceStore()
	expectedPiece := tut.GenerateCids(1)[0]
	cidInfo := piecestore.CIDInfo{
		PieceBlockLocations: []piecestore.PieceBlockLocation{
			{
				PieceCID: expectedPiece,
			},
		},
	}
	pieceStore.ExpectCID(payloadCID, cidInfo)
	pieceStore.ExpectPiece(expectedPiece, pieceInfo)
	provider, err := retrievalimpl.NewProvider(providerPaymentAddr, providerNode, nw2, pieceStore, testData.Bs2, testData.Ds2)
	require.NoError(t, err)
	provider.SetPaymentInterval(expectedQR.MaxPaymentInterval, expectedQR.MaxPaymentIntervalIncrease)
	provider.SetPricePerByte(expectedQR.MinPricePerByte)
	require.NoError(t, provider.Start())

	retrievalPeer := &retrievalmarket.RetrievalPeer{Address: providerPaymentAddr, ID: testData.Host2.ID()}
	expectedVoucher := tut.MakeTestSignedVoucher()
	expectedTotal := big.Mul(pricePerByte, abi.NewTokenAmount(int64(filesize*2)))

	proof := []byte("")
	for _, voucherAmt := range voucherAmts {
		require.NoError(t, providerNode.ExpectVoucher(clientPaymentChannel, expectedVoucher, proof, voucherAmt, voucherAmt, nil))
	}
	// ------- SET UP CLIENT
	nw1 := rmnet.NewFromLibp2pHost(testData.Host1)

	createdChan, _, createdVoucher, client, err := setupClient(clientPaymentChannel, expectedVoucher, nw1, testData, false)
	require.NoError(t, err)

	// **** Send the query for the Piece
	// set up retrieval params
	resp, err := client.Query(bgCtx, *retrievalPeer, payloadCID, retrievalmarket.QueryParams{})
	require.NoError(t, err)
	require.Equal(t, retrievalmarket.QueryResponseAvailable, resp.Status)

	clientDealStateChan := make(chan retrievalmarket.ClientDealState)
	client.SubscribeToEvents(func(event retrievalmarket.ClientEvent, state retrievalmarket.ClientDealState) {
		switch event {
		case retrievalmarket.ClientEventComplete:
			clientDealStateChan <- state
		}
	})

	rmParams := retrievalmarket.NewParamsV1(pricePerByte, paymentInterval, paymentIntervalIncrease, shared.AllSelector(), nil)
	// *** Retrieve the piece, simulating a shutdown in between
	did, err := client.Retrieve(bgCtx, payloadCID, rmParams, expectedTotal, retrievalPeer.ID, clientPaymentChannel, retrievalPeer.Address)
	require.NoError(t, err)

	require.NoError(t, provider.Stop())

	provider2, err := retrievalimpl.NewProvider(providerPaymentAddr, providerNode, nw2, pieceStore, testData.Bs2, testData.Ds2)
	require.NoError(t, err)
	provider2.SetPaymentInterval(expectedQR.MaxPaymentInterval, expectedQR.MaxPaymentIntervalIncrease)
	provider2.SetPricePerByte(expectedQR.MinPricePerByte)
	require.NoError(t, provider2.Start())

	require.Equal(t, did, retrievalmarket.DealID(0))
	// verify that the voucher was saved/seen by the client with correct values

	// verify that client subscribers will be notified of state changes
	ctx, cancel := context.WithTimeout(bgCtx, 5*time.Second)
	defer cancel()
	var clientDealState retrievalmarket.ClientDealState
	select {
	case <-ctx.Done():
		t.Error("deal never completed")
		t.FailNow()
	case clientDealState = <-clientDealStateChan:
	}
	assert.Equal(t, clientDealState.PaymentInfo.Lane, expectedVoucher.Lane)
	require.NotNil(t, createdChan)
	require.Equal(t, expectedTotal, createdChan.amt)
	// verify that the voucher was saved/seen by the client with correct values
	require.NotNil(t, createdVoucher)
	tut.TestVoucherEquality(t, createdVoucher, expectedVoucher)

	// verify that the provider saved the same voucher values
	providerNode.VerifyExpectations(t)
	testData.VerifyFileTransferred(t, pieceLink, false, uint64(filesize))
}

func logProviderDealState(t *testing.T, state retrievalmarket.ProviderDealState) {
	msg := `
Provider:
Status:          %s
TotalSent:       %d
FundsReceived:   %s
Message:		 %s
CurrentInterval: %d
`
	t.Logf(msg, retrievalmarket.DealStatuses[state.Status], state.TotalSent, state.FundsReceived.String(), state.Message,
		state.CurrentInterval)
}

func logClientDealState(t *testing.T, state retrievalmarket.ClientDealState) {
	msg := `
Client:
Status:          %s
TotalReceived:   %d
BytesPaidFor:    %d
CurrentInterval: %d
TotalFunds:      %s
Message:         %s
`
	t.Logf(msg, retrievalmarket.DealStatuses[state.Status], state.TotalReceived, state.BytesPaidFor, state.CurrentInterval,
		state.TotalFunds.String(), state.Message)
}

func setupUnseal(t *testing.T, bgCtx context.Context, testData *tut.Libp2pTestData, payloadCID cid.Cid, pieceInfo piecestore.PieceInfo, providerNode *testnodes.TestRetrievalProviderNode) piecestore.PieceInfo {
	cio := cario.NewCarIO()
	var buf bytes.Buffer
	err := cio.WriteCar(bgCtx, testData.Bs2, payloadCID, shared.AllSelector(), &buf)
	require.NoError(t, err)
	carData := buf.Bytes()
	sectorID := uint64(100000)
	offset := uint64(1000)
	pieceInfo = piecestore.PieceInfo{
		Deals: []piecestore.DealInfo{
			{
				SectorID: sectorID,
				Offset:   offset,
				Length:   uint64(len(carData)),
			},
		},
	}
	providerNode.ExpectUnseal(sectorID, offset, uint64(len(carData)), carData)
	// clear out provider blockstore
	allCids, err := testData.Bs2.AllKeysChan(bgCtx)
	require.NoError(t, err)
	for c := range allCids {
		err = testData.Bs2.DeleteBlock(c)
		require.NoError(t, err)
	}
	return pieceInfo
}

func setupClient(
	clientPaymentChannel address.Address,
	expectedVoucher *paych.SignedVoucher,
	nw1 rmnet.RetrievalMarketNetwork,
	testData *tut.Libp2pTestData,
	addFunds bool,
) (
	*pmtChan,
	*address.Address,
	*paych.SignedVoucher,
	retrievalmarket.RetrievalClient,
	error) {
	var createdChan pmtChan
	paymentChannelRecorder := func(client, miner address.Address, amt abi.TokenAmount) {
		createdChan = pmtChan{client, miner, amt}
	}

	var newLaneAddr address.Address
	laneRecorder := func(paymentChannel address.Address) {
		newLaneAddr = paymentChannel
	}

	var createdVoucher paych.SignedVoucher
	paymentVoucherRecorder := func(v *paych.SignedVoucher) {
		createdVoucher = *v
	}
	cids := tut.GenerateCids(2)
	clientNode := testnodes.NewTestRetrievalClientNode(testnodes.TestRetrievalClientNodeParams{
		AddFundsOnly:           addFunds,
		PayCh:                  clientPaymentChannel,
		Lane:                   expectedVoucher.Lane,
		Voucher:                expectedVoucher,
		PaymentChannelRecorder: paymentChannelRecorder,
		AllocateLaneRecorder:   laneRecorder,
		PaymentVoucherRecorder: paymentVoucherRecorder,
		CreatePaychCID:         cids[0],
		AddFundsCID:            cids[1],
	})
	client, err := retrievalimpl.NewClient(nw1, testData.Bs1, clientNode, &tut.TestPeerResolver{}, testData.Ds1, testData.RetrievalStoredCounter1)
	return &createdChan, &newLaneAddr, &createdVoucher, client, err
}

func setupProvider(t *testing.T, testData *tut.Libp2pTestData, payloadCID cid.Cid, pieceInfo piecestore.PieceInfo, expectedQR retrievalmarket.QueryResponse, providerPaymentAddr address.Address, providerNode retrievalmarket.RetrievalProviderNode) retrievalmarket.RetrievalProvider {
	nw2 := rmnet.NewFromLibp2pHost(testData.Host2)
	pieceStore := tut.NewTestPieceStore()
	expectedPiece := tut.GenerateCids(1)[0]
	cidInfo := piecestore.CIDInfo{
		PieceBlockLocations: []piecestore.PieceBlockLocation{
			{
				PieceCID: expectedPiece,
			},
		},
	}
	pieceStore.ExpectCID(payloadCID, cidInfo)
	pieceStore.ExpectPiece(expectedPiece, pieceInfo)
	provider, err := retrievalimpl.NewProvider(providerPaymentAddr, providerNode, nw2, pieceStore, testData.Bs2, testData.Ds2)
	require.NoError(t, err)
	provider.SetPaymentInterval(expectedQR.MaxPaymentInterval, expectedQR.MaxPaymentIntervalIncrease)
	provider.SetPricePerByte(expectedQR.MinPricePerByte)
	require.NoError(t, provider.Start())
	return provider
}

type pmtChan struct {
	client, miner address.Address
	amt           abi.TokenAmount
}
