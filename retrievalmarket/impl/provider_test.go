package retrievalimpl_test

import (
	"errors"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dss "github.com/ipfs/go-datastore/sync"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-fil-markets/piecestore"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket"
	retrievalimpl "github.com/filecoin-project/go-fil-markets/retrievalmarket/impl"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket/impl/testnodes"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket/network"
	tut "github.com/filecoin-project/go-fil-markets/shared_testutil"
)

func TestHandleQueryStream(t *testing.T) {

	payloadCID := tut.GenerateCids(1)[0]
	expectedPeer := peer.ID("somepeer")
	expectedSize := uint64(1234)

	expectedPieceCID := tut.GenerateCids(1)[0]
	expectedCIDInfo := piecestore.CIDInfo{
		PieceBlockLocations: []piecestore.PieceBlockLocation{
			{
				PieceCID: expectedPieceCID,
			},
		},
	}
	expectedPiece := piecestore.PieceInfo{
		Deals: []piecestore.DealInfo{
			{
				Length: expectedSize,
			},
		},
	}
	expectedAddress := address.TestAddress2
	expectedPricePerByte := abi.NewTokenAmount(4321)
	expectedPaymentInterval := uint64(4567)
	expectedPaymentIntervalIncrease := uint64(100)

	readWriteQueryStream := func() network.RetrievalQueryStream {
		qRead, qWrite := tut.QueryReadWriter()
		qrRead, qrWrite := tut.QueryResponseReadWriter()
		qs := tut.NewTestRetrievalQueryStream(tut.TestQueryStreamParams{
			PeerID:     expectedPeer,
			Reader:     qRead,
			Writer:     qWrite,
			RespReader: qrRead,
			RespWriter: qrWrite,
		})
		return qs
	}

	receiveStreamOnProvider := func(qs network.RetrievalQueryStream, pieceStore piecestore.PieceStore) {
		node := testnodes.NewTestRetrievalProviderNode()
		ds := dss.MutexWrap(datastore.NewMapDatastore())
		bs := bstore.NewBlockstore(ds)
		net := tut.NewTestRetrievalMarketNetwork(tut.TestNetworkParams{})
		c, err := retrievalimpl.NewProvider(expectedAddress, node, net, pieceStore, bs, ds)
		require.NoError(t, err)
		c.SetPricePerByte(expectedPricePerByte)
		c.SetPaymentInterval(expectedPaymentInterval, expectedPaymentIntervalIncrease)
		_ = c.Start()
		net.ReceiveQueryStream(qs)
	}

	testCases := []struct {
		name    string
		query   retrievalmarket.Query
		expResp retrievalmarket.QueryResponse
		expErr  string
		expFunc func(t *testing.T, pieceStore *tut.TestPieceStore)
	}{
		{name: "When PieceCID is not provided and PayloadCID is found",
			expFunc: func(t *testing.T, pieceStore *tut.TestPieceStore) {
				pieceStore.ExpectCID(payloadCID, expectedCIDInfo)
				pieceStore.ExpectPiece(expectedPieceCID, expectedPiece)
			},
			query: retrievalmarket.Query{PayloadCID: payloadCID},
			expResp: retrievalmarket.QueryResponse{
				Status:        retrievalmarket.QueryResponseAvailable,
				PieceCIDFound: retrievalmarket.QueryItemAvailable,
				Size:          expectedSize,
			},
		},
		{name: "When PieceCID is provided and both PieceCID and PayloadCID are found",
			expFunc: func(t *testing.T, pieceStore *tut.TestPieceStore) {
				loadPieceCIDS(t, pieceStore, payloadCID, expectedPieceCID)
			},
			query: retrievalmarket.Query{
				PayloadCID:  payloadCID,
				QueryParams: retrievalmarket.QueryParams{PieceCID: &expectedPieceCID},
			},
			expResp: retrievalmarket.QueryResponse{
				Status:        retrievalmarket.QueryResponseAvailable,
				PieceCIDFound: retrievalmarket.QueryItemAvailable,
				Size:          expectedSize,
			},
		},
		{name: "When QueryParams has PieceCID and is missing",
			expFunc: func(t *testing.T, ps *tut.TestPieceStore) {
				loadPieceCIDS(t, ps, payloadCID, cid.Undef)
				ps.ExpectCID(payloadCID, expectedCIDInfo)
				ps.ExpectMissingPiece(expectedPieceCID)
			},
			query: retrievalmarket.Query{
				PayloadCID:  payloadCID,
				QueryParams: retrievalmarket.QueryParams{PieceCID: &expectedPieceCID},
			},
			expResp: retrievalmarket.QueryResponse{
				Status:        retrievalmarket.QueryResponseUnavailable,
				PieceCIDFound: retrievalmarket.QueryItemUnavailable,
			},
		},
		{name: "When CID info not found",
			expFunc: func(t *testing.T, ps *tut.TestPieceStore) {
				ps.ExpectMissingCID(payloadCID)
			},
			query: retrievalmarket.Query{
				PayloadCID:  payloadCID,
				QueryParams: retrievalmarket.QueryParams{PieceCID: &expectedPieceCID},
			},
			expResp: retrievalmarket.QueryResponse{
				Status:        retrievalmarket.QueryResponseUnavailable,
				PieceCIDFound: retrievalmarket.QueryItemUnavailable,
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			qs := readWriteQueryStream()
			err := qs.WriteQuery(tc.query)
			require.NoError(t, err)
			pieceStore := tut.NewTestPieceStore()
			pieceStore.ExpectCID(payloadCID, expectedCIDInfo)
			pieceStore.ExpectMissingPiece(expectedPieceCID)

			tc.expFunc(t, pieceStore)

			receiveStreamOnProvider(qs, pieceStore)

			actualResp, err := qs.ReadQueryResponse()
			pieceStore.VerifyExpectations(t)
			if tc.expErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tc.expErr)
			}

			tc.expResp.PaymentAddress = expectedAddress
			tc.expResp.MinPricePerByte = expectedPricePerByte
			tc.expResp.MaxPaymentInterval = expectedPaymentInterval
			tc.expResp.MaxPaymentIntervalIncrease = expectedPaymentIntervalIncrease
			assert.Equal(t, tc.expResp, actualResp)
		})
	}

	t.Run("error reading piece", func(t *testing.T) {
		qs := readWriteQueryStream()
		err := qs.WriteQuery(retrievalmarket.Query{
			PayloadCID: payloadCID,
		})
		require.NoError(t, err)
		pieceStore := tut.NewTestPieceStore()

		receiveStreamOnProvider(qs, pieceStore)

		response, err := qs.ReadQueryResponse()
		require.NoError(t, err)
		require.Equal(t, response.Status, retrievalmarket.QueryResponseError)
		require.NotEmpty(t, response.Message)
	})

	t.Run("when ReadQuery fails", func(t *testing.T) {
		qs := readWriteQueryStream()
		pieceStore := tut.NewTestPieceStore()

		receiveStreamOnProvider(qs, pieceStore)

		response, err := qs.ReadQueryResponse()
		require.NotNil(t, err)
		require.Equal(t, response, retrievalmarket.QueryResponseUndefined)
	})

	t.Run("when WriteQueryResponse fails", func(t *testing.T) {
		qRead, qWrite := tut.QueryReadWriter()
		qs := tut.NewTestRetrievalQueryStream(tut.TestQueryStreamParams{
			PeerID:     expectedPeer,
			Reader:     qRead,
			Writer:     qWrite,
			RespWriter: tut.FailResponseWriter,
		})
		err := qs.WriteQuery(retrievalmarket.Query{
			PayloadCID: payloadCID,
		})
		require.NoError(t, err)
		pieceStore := tut.NewTestPieceStore()
		pieceStore.ExpectCID(payloadCID, expectedCIDInfo)
		pieceStore.ExpectPiece(expectedPieceCID, expectedPiece)

		receiveStreamOnProvider(qs, pieceStore)

		pieceStore.VerifyExpectations(t)
	})

}

func TestProviderRestart(t *testing.T) {
	payloadCID := tut.GenerateCids(1)[0]
	expectedPeer := peer.ID("somepeer")
	expectedSize := uint64(1234)

	expectedPieceCID := tut.GenerateCids(1)[0]
	expectedCIDInfo := piecestore.CIDInfo{
		PieceBlockLocations: []piecestore.PieceBlockLocation{
			{
				PieceCID: expectedPieceCID,
			},
		},
	}
	expectedPiece := piecestore.PieceInfo{
		Deals: []piecestore.DealInfo{
			{
				Length: expectedSize,
			},
		},
	}
	expectedAddress := address.TestAddress2
	expectedPricePerByte := abi.NewTokenAmount(4321)
	expectedPaymentInterval := uint64(4567)
	expectedPaymentIntervalIncrease := uint64(100)

	rwDealStream := func() network.RetrievalDealStream {
		pRead, pWrite := ProposalReadWriter()
		rRead, rWrite := ResponseReadWriter()
		qs := tut.NewTestRetrievalDealStream(tut.TestDealStreamParams{
			PeerID:         expectedPeer,
			ProposalReader: pRead,
			ProposalWriter: pWrite,
			ResponseReader: rRead,
			ResponseWriter: rWrite,
		})
		return qs
	}
}

func ProposalReadWriter() (tut.DealProposalReader, tut.DealProposalWriter) {
	var p retrievalmarket.DealProposal
	var written bool
	propRead := func() (retrievalmarket.DealProposal, error) {
		if written {
			return p, nil
		}
		return retrievalmarket.DealProposalUndefined, errors.New("unable to read proposal")
	}

	propWrite := func(wp retrievalmarket.DealProposal) error {
		p = wp
		written = true
		return nil
	}
	return propRead, propWrite
}

func ResponseReadWriter() (tut.DealResponseReader, tut.DealResponseWriter) {
	var dr retrievalmarket.DealResponse
	var written bool
	responseRead := func() (retrievalmarket.DealResponse, error) {
		if written {
			return dr, nil
		}
		return retrievalmarket.DealResponseUndefined, errors.New("unabled to read deal response")
	}
	responseWrite := func(wdr retrievalmarket.DealResponse) error {
		dr = wdr
		written = true
		return nil
	}
	return responseRead, responseWrite
}

// loadPieceCIDS sets expectations to receive expectedPieceCID and 3 other random PieceCIDs to
// disinguish the case of a PayloadCID is found but the PieceCID is not
func loadPieceCIDS(t *testing.T, pieceStore *tut.TestPieceStore, expPayloadCID, expectedPieceCID cid.Cid) {

	otherPieceCIDs := tut.GenerateCids(3)
	expectedSize := uint64(1234)

	blockLocs := make([]piecestore.PieceBlockLocation, 4)
	expectedPieceInfo := piecestore.PieceInfo{
		PieceCID: expectedPieceCID,
		Deals: []piecestore.DealInfo{
			{
				Length: expectedSize,
			},
		},
	}

	blockLocs[0] = piecestore.PieceBlockLocation{PieceCID: expectedPieceCID}
	for i, pieceCID := range otherPieceCIDs {
		blockLocs[i+1] = piecestore.PieceBlockLocation{PieceCID: pieceCID}
		pi := expectedPieceInfo
		pi.PieceCID = pieceCID
	}
	if expectedPieceCID != cid.Undef {
		pieceStore.ExpectPiece(expectedPieceCID, expectedPieceInfo)
	}
	expectedCIDInfo := piecestore.CIDInfo{PieceBlockLocations: blockLocs}
	pieceStore.ExpectCID(expPayloadCID, expectedCIDInfo)
}
