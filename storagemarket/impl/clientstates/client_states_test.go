package clientstates_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/filecoin-project/go-address"
	cborutil "github.com/filecoin-project/go-cbor-util"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/go-statemachine/fsm"
	fsmtest "github.com/filecoin-project/go-statemachine/fsm/testutil"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	"github.com/filecoin-project/specs-actors/actors/runtime/exitcode"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tut "github.com/filecoin-project/go-fil-markets/shared_testutil"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	storageimpl "github.com/filecoin-project/go-fil-markets/storagemarket/impl"
	"github.com/filecoin-project/go-fil-markets/storagemarket/impl/clientstates"
	smnet "github.com/filecoin-project/go-fil-markets/storagemarket/network"
	"github.com/filecoin-project/go-fil-markets/storagemarket/testnodes"
)

var clientDealProposal = tut.MakeTestClientDealProposal()

func TestEnsureFunds(t *testing.T) {
	t.Run("immediately succeeds", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealEnsureClientFunds, clientstates.EnsureClientFunds, testCase{
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealFundsEnsured, deal.State)
			},
		})
	})
	t.Run("succeeds by sending an AddFunds message", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealEnsureClientFunds, clientstates.EnsureClientFunds, testCase{
			nodeParams: nodeParams{AddFundsCid: tut.GenerateCids(1)[0]},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealClientFunding, deal.State)
			},
		})
	})
	t.Run("EnsureClientFunds fails", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealEnsureClientFunds, clientstates.EnsureClientFunds, testCase{
			nodeParams: nodeParams{
				EnsureFundsError: errors.New("Something went wrong"),
			},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealFailing, deal.State)
				assert.Equal(t, "adding market funds failed: Something went wrong", deal.Message)
			},
		})
	})
}

func TestWaitForFunding(t *testing.T) {
	t.Run("succeeds", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealClientFunding, clientstates.WaitForFunding, testCase{
			nodeParams: nodeParams{WaitForMessageExitCode: exitcode.Ok},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealFundsEnsured, deal.State)
			},
		})
	})
	t.Run("EnsureClientFunds fails", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealClientFunding, clientstates.WaitForFunding, testCase{
			nodeParams: nodeParams{WaitForMessageExitCode: exitcode.ErrInsufficientFunds},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealFailing, deal.State)
				assert.Equal(t, "adding market funds failed: AddFunds exit code: 19", deal.Message)
			},
		})
	})
}

func TestProposeDeal(t *testing.T) {
	t.Run("succeeds", func(t *testing.T) {
		ds := tut.NewTestStorageDealStream(tut.TestStorageDealStreamParams{
			ProposalWriter: tut.TrivialStorageDealProposalWriter,
		})
		runAndInspect(t, storagemarket.StorageDealFundsEnsured, clientstates.ProposeDeal, testCase{
			envParams:  envParams{dealStream: ds},
			nodeParams: nodeParams{WaitForMessageExitCode: exitcode.Ok},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealWaitingForDataRequest, deal.State)
				ds.AssertConnectionTagged(t, deal.ProposalCid.String())
			},
		})
	})
	t.Run("write proposal fails fails", func(t *testing.T) {
		ds := tut.NewTestStorageDealStream(tut.TestStorageDealStreamParams{
			ProposalWriter: tut.FailStorageProposalWriter,
		})
		runAndInspect(t, storagemarket.StorageDealFundsEnsured, clientstates.ProposeDeal, testCase{
			envParams:  envParams{dealStream: ds},
			nodeParams: nodeParams{WaitForMessageExitCode: exitcode.Ok},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealError, deal.State)
				assert.Equal(t, "sending proposal to storage provider failed: write proposal failed", deal.Message)
			},
		})
	})
}

func TestWaitingForDataRequest(t *testing.T) {
	t.Run("succeeds", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealWaitingForDataRequest, clientstates.WaitingForDataRequest, testCase{
			envParams: envParams{
				dealStream: testResponseStream(t, responseParams{
					state:    storagemarket.StorageDealWaitingForData,
					proposal: clientDealProposal,
				}),
			},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				assert.Len(t, env.startDataTransferCalls, 1)
				assert.Equal(t, env.startDataTransferCalls[0].to, deal.Miner)
				assert.Equal(t, env.startDataTransferCalls[0].baseCid, deal.DataRef.Root)

				tut.AssertDealState(t, storagemarket.StorageDealTransferring, deal.State)
			},
		})
	})

	t.Run("response contains unexpected state", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealWaitingForDataRequest, clientstates.WaitingForDataRequest, testCase{
			envParams: envParams{
				dealStream: testResponseStream(t, responseParams{
					proposal: clientDealProposal,
					state:    storagemarket.StorageDealProposalNotFound,
				}),
			},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealFailing, deal.State)
				assert.Equal(t, "unexpected deal status while waiting for data request: 1", deal.Message)
			},
		})
	})
	t.Run("read response fails", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealWaitingForDataRequest, clientstates.WaitingForDataRequest, testCase{
			envParams: envParams{
				startDataTransferError: errors.New("failed"),
				dealStream: testResponseStream(t, responseParams{
					proposal: clientDealProposal,
					state:    storagemarket.StorageDealWaitingForData,
				}),
			},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealFailing, deal.State)
				assert.Equal(t, "failed to initiate data transfer: failed to open push data channel: failed", deal.Message)
			},
		})
	})
	t.Run("waits for another response with manual transfers", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealWaitingForDataRequest, clientstates.WaitingForDataRequest, testCase{
			envParams: envParams{
				dealStream: testResponseStream(t, responseParams{
					proposal: clientDealProposal,
					state:    storagemarket.StorageDealWaitingForData,
				}),
				manualTransfer: true,
			},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealValidating, deal.State)
			},
		})
	})
}

func TestVerifyDealResponse(t *testing.T) {
	t.Run("succeeds", func(t *testing.T) {
		publishMessage := &(tut.GenerateCids(1)[0])

		runAndInspect(t, storagemarket.StorageDealValidating, clientstates.VerifyDealResponse, testCase{
			envParams: envParams{
				dealStream: testResponseStream(t, responseParams{
					proposal:       clientDealProposal,
					state:          storagemarket.StorageDealProposalAccepted,
					publishMessage: publishMessage,
				}),
			},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealProposalAccepted, deal.State)
				assert.Equal(t, publishMessage, deal.PublishMessage)
			},
		})
	})
	t.Run("read response fails", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealValidating, clientstates.VerifyDealResponse, testCase{
			envParams: envParams{dealStream: tut.NewTestStorageDealStream(tut.TestStorageDealStreamParams{
				ResponseReader: tut.FailStorageResponseReader,
			})},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealError, deal.State)
				assert.Equal(t, "error reading Response message: read response failed", deal.Message)
			},
		})
	})
	t.Run("verify response fails", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealValidating, clientstates.VerifyDealResponse, testCase{
			nodeParams: nodeParams{VerifySignatureFails: true},
			envParams: envParams{dealStream: testResponseStream(t, responseParams{
				proposal: clientDealProposal,
				state:    storagemarket.StorageDealProposalAccepted,
			})},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealFailing, deal.State)
				assert.Equal(t, "unable to verify signature on deal response", deal.Message)
			},
		})
	})
	t.Run("incorrect proposal cid", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealValidating, clientstates.VerifyDealResponse, testCase{
			envParams: envParams{dealStream: testResponseStream(t, responseParams{
				proposal:    clientDealProposal,
				state:       storagemarket.StorageDealProposalAccepted,
				proposalCid: tut.GenerateCids(1)[0],
			})},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealFailing, deal.State)
				assert.Regexp(t, "^miner responded to a wrong proposal:", deal.Message)
			},
		})
	})
	t.Run("deal rejected", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealValidating, clientstates.VerifyDealResponse, testCase{
			envParams: envParams{dealStream: testResponseStream(t, responseParams{
				proposal: clientDealProposal,
				state:    storagemarket.StorageDealProposalRejected,
				message:  "because reasons",
			})},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				expErr := fmt.Sprintf("deal failed: (State=%d) because reasons", storagemarket.StorageDealProposalRejected)

				tut.AssertDealState(t, storagemarket.StorageDealFailing, deal.State)
				assert.Equal(t, deal.Message, expErr)
			},
		})
	})
	t.Run("deal stream close errors", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealValidating, clientstates.VerifyDealResponse, testCase{
			envParams: envParams{dealStream: testResponseStream(t, responseParams{
				proposal: clientDealProposal,
				state:    storagemarket.StorageDealProposalAccepted,
			}), closeStreamErr: errors.New("something went wrong")},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealError, deal.State)
				assert.Equal(t, "error attempting to close stream: something went wrong", deal.Message)
			},
		})
	})
}

func TestValidateDealPublished(t *testing.T) {
	t.Run("succeeds", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealProposalAccepted, clientstates.ValidateDealPublished, testCase{
			nodeParams: nodeParams{ValidatePublishedDealID: abi.DealID(5)},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealSealing, deal.State)
				assert.Equal(t, abi.DealID(5), deal.DealID)
			},
		})
	})
	t.Run("fails", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealProposalAccepted, clientstates.ValidateDealPublished, testCase{
			nodeParams: nodeParams{
				ValidatePublishedDealID: abi.DealID(5),
				ValidatePublishedError:  errors.New("Something went wrong"),
			},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealError, deal.State)
				assert.Equal(t, "error validating deal published: Something went wrong", deal.Message)
			},
		})
	})
}

func TestVerifyDealActivated(t *testing.T) {
	t.Run("succeeds", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealSealing, clientstates.VerifyDealActivated, testCase{
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealActive, deal.State)
			},
		})
	})
	t.Run("fails synchronously", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealSealing, clientstates.VerifyDealActivated, testCase{
			nodeParams: nodeParams{DealCommittedSyncError: errors.New("Something went wrong")},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealError, deal.State)
				assert.Equal(t, "error in deal activation: Something went wrong", deal.Message)
			},
		})
	})
	t.Run("fails asynchronously", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealSealing, clientstates.VerifyDealActivated, testCase{
			nodeParams: nodeParams{DealCommittedAsyncError: errors.New("Something went wrong later")},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				tut.AssertDealState(t, storagemarket.StorageDealError, deal.State)
				assert.Equal(t, "error in deal activation: Something went wrong later", deal.Message)
			},
		})
	})
}

func TestFailDeal(t *testing.T) {
	t.Run("closes an open stream", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealFailing, clientstates.FailDeal, testCase{
			stateParams: dealStateParams{connectionClosed: false},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				assert.Equal(t, storagemarket.StorageDealError, deal.State)
			},
		})
	})
	t.Run("unable to close an the open stream", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealFailing, clientstates.FailDeal, testCase{
			stateParams: dealStateParams{connectionClosed: false},
			envParams:   envParams{closeStreamErr: errors.New("unable to close")},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				assert.Equal(t, storagemarket.StorageDealError, deal.State)
				assert.Equal(t, "error attempting to close stream: unable to close", deal.Message)
			},
		})
	})
	t.Run("doesn't try to close a closed stream", func(t *testing.T) {
		runAndInspect(t, storagemarket.StorageDealFailing, clientstates.FailDeal, testCase{
			stateParams: dealStateParams{connectionClosed: true},
			inspector: func(deal storagemarket.ClientDeal, env *fakeEnvironment) {
				assert.Len(t, env.closeStreamCalls, 0)
				assert.Equal(t, storagemarket.StorageDealError, deal.State)
			},
		})
	})
}

func TestFinalityStates(t *testing.T) {
	group, err := storageimpl.NewClientStateMachine(nil, &fakeEnvironment{}, nil)
	require.NoError(t, err)

	for _, status := range []storagemarket.StorageDealStatus{
		storagemarket.StorageDealActive,
		storagemarket.StorageDealError,
	} {
		require.True(t, group.IsTerminated(storagemarket.ClientDeal{State: status}))
	}
}

type envParams struct {
	dealStream             smnet.StorageDealStream
	closeStreamErr         error
	startDataTransferError error
	manualTransfer         bool
}

type dealStateParams struct {
	connectionClosed bool
	addFundsCid      *cid.Cid
}

type executor func(t *testing.T,
	nodeParams nodeParams,
	envParams envParams,
	dealInspector func(deal storagemarket.ClientDeal, env *fakeEnvironment))

func makeExecutor(ctx context.Context,
	eventProcessor fsm.EventProcessor,
	initialState storagemarket.StorageDealStatus,
	stateEntryFunc clientstates.ClientStateEntryFunc,
	dealParams dealStateParams,
	clientDealProposal *market.ClientDealProposal) executor {
	return func(t *testing.T,
		nodeParams nodeParams,
		envParams envParams,
		dealInspector func(deal storagemarket.ClientDeal, env *fakeEnvironment)) {
		node := makeNode(nodeParams)
		dealState, err := tut.MakeTestClientDeal(initialState, clientDealProposal, envParams.manualTransfer)
		assert.NoError(t, err)
		dealState.AddFundsCid = &tut.GenerateCids(1)[0]
		dealState.ConnectionClosed = dealParams.connectionClosed

		if dealParams.addFundsCid != nil {
			dealState.AddFundsCid = dealParams.addFundsCid
		}

		environment := &fakeEnvironment{
			node:                   node,
			dealStream:             envParams.dealStream,
			closeStreamErr:         envParams.closeStreamErr,
			startDataTransferError: envParams.startDataTransferError,
		}
		fsmCtx := fsmtest.NewTestContext(ctx, eventProcessor)
		err = stateEntryFunc(fsmCtx, environment, *dealState)
		assert.NoError(t, err)
		fsmCtx.ReplayEvents(t, dealState)
		dealInspector(*dealState, environment)
	}
}

type nodeParams struct {
	AddFundsCid             cid.Cid
	EnsureFundsError        error
	VerifySignatureFails    bool
	GetBalanceError         error
	GetChainHeadError       error
	WaitForMessageBlocks    bool
	WaitForMessageError     error
	WaitForMessageExitCode  exitcode.ExitCode
	WaitForMessageRetBytes  []byte
	ClientAddr              address.Address
	ValidationError         error
	ValidatePublishedDealID abi.DealID
	ValidatePublishedError  error
	DealCommittedSyncError  error
	DealCommittedAsyncError error
}

func makeNode(params nodeParams) storagemarket.StorageClientNode {
	var out testnodes.FakeClientNode
	out.SMState = testnodes.NewStorageMarketState()
	out.AddFundsCid = params.AddFundsCid
	out.EnsureFundsError = params.EnsureFundsError
	out.VerifySignatureFails = params.VerifySignatureFails
	out.GetBalanceError = params.GetBalanceError
	out.GetChainHeadError = params.GetChainHeadError
	out.WaitForMessageBlocks = params.WaitForMessageBlocks
	out.WaitForMessageError = params.WaitForMessageError
	out.WaitForMessageExitCode = params.WaitForMessageExitCode
	out.WaitForMessageRetBytes = params.WaitForMessageRetBytes
	out.ClientAddr = params.ClientAddr
	out.ValidationError = params.ValidationError
	out.ValidatePublishedDealID = params.ValidatePublishedDealID
	out.ValidatePublishedError = params.ValidatePublishedError
	out.DealCommittedSyncError = params.DealCommittedSyncError
	out.DealCommittedAsyncError = params.DealCommittedAsyncError
	return &out
}

type fakeEnvironment struct {
	node                   storagemarket.StorageClientNode
	dealStream             smnet.StorageDealStream
	closeStreamErr         error
	closeStreamCalls       []cid.Cid
	startDataTransferError error
	startDataTransferCalls []dataTransferParams
}

type dataTransferParams struct {
	to       peer.ID
	voucher  datatransfer.Voucher
	baseCid  cid.Cid
	selector ipld.Node
}

func (fe *fakeEnvironment) StartDataTransfer(ctx context.Context, to peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) error {
	fe.startDataTransferCalls = append(fe.startDataTransferCalls, dataTransferParams{
		to:       to,
		voucher:  voucher,
		baseCid:  baseCid,
		selector: selector,
	})
	return fe.startDataTransferError
}

func (fe *fakeEnvironment) Node() storagemarket.StorageClientNode {
	return fe.node
}

func (fe *fakeEnvironment) WriteDealProposal(p peer.ID, proposalCid cid.Cid, proposal smnet.Proposal) error {
	return fe.dealStream.WriteDealProposal(proposal)
}

func (fe *fakeEnvironment) ReadDealResponse(proposalCid cid.Cid) (smnet.SignedResponse, error) {
	return fe.dealStream.ReadDealResponse()
}

func (fe *fakeEnvironment) TagConnection(proposalCid cid.Cid) error {
	fe.dealStream.TagProtectedConnection(proposalCid.String())
	return nil
}

func (fe *fakeEnvironment) CloseStream(proposalCid cid.Cid) error {
	fe.closeStreamCalls = append(fe.closeStreamCalls, proposalCid)
	return fe.closeStreamErr
}

var _ clientstates.ClientDealEnvironment = &fakeEnvironment{}

type responseParams struct {
	proposal       *market.ClientDealProposal
	state          storagemarket.StorageDealStatus
	message        string
	publishMessage *cid.Cid
	proposalCid    cid.Cid
}

func testResponseStream(t *testing.T, params responseParams) smnet.StorageDealStream {
	response := smnet.Response{
		State:          params.state,
		Proposal:       params.proposalCid,
		Message:        params.message,
		PublishMessage: params.publishMessage,
	}

	if response.Proposal == cid.Undef {
		proposalNd, err := cborutil.AsIpld(params.proposal)
		assert.NoError(t, err)
		response.Proposal = proposalNd.Cid()
	}

	reader := tut.StubbedStorageResponseReader(smnet.SignedResponse{
		Response:  response,
		Signature: tut.MakeTestSignature(),
	})

	return tut.NewTestStorageDealStream(tut.TestStorageDealStreamParams{
		ResponseReader: reader,
	})
}

type testCase struct {
	envParams   envParams
	nodeParams  nodeParams
	stateParams dealStateParams
	inspector   func(deal storagemarket.ClientDeal, env *fakeEnvironment)
}

func runAndInspect(t *testing.T, initialState storagemarket.StorageDealStatus, stateFunc clientstates.ClientStateEntryFunc, tc testCase) {
	ctx := context.Background()
	eventProcessor, err := fsm.NewEventProcessor(storagemarket.ClientDeal{}, "State", clientstates.ClientEvents)
	assert.NoError(t, err)
	executor := makeExecutor(ctx, eventProcessor, initialState, stateFunc, tc.stateParams, clientDealProposal)
	executor(t, tc.nodeParams, tc.envParams, tc.inspector)
}
