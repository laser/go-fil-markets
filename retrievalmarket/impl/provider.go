package retrievalimpl

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-statemachine/fsm"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-fil-markets/pieceio/cario"
	"github.com/filecoin-project/go-fil-markets/piecestore"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket/impl/blockio"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket/impl/blockunsealing"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket/impl/providerstates"
	rmnet "github.com/filecoin-project/go-fil-markets/retrievalmarket/network"
	"github.com/filecoin-project/go-fil-markets/shared"
)

type provider struct {
	bs                      blockstore.Blockstore
	node                    retrievalmarket.RetrievalProviderNode
	network                 rmnet.RetrievalMarketNetwork
	paymentInterval         uint64
	paymentIntervalIncrease uint64
	minerAddress            address.Address
	pieceStore              piecestore.PieceStore
	pricePerByte            abi.TokenAmount
	subscribers             []retrievalmarket.ProviderSubscriber
	subscribersLk           sync.RWMutex
	dealStreams             map[retrievalmarket.ProviderDealIdentifier]rmnet.RetrievalDealStream
	blockReaders            map[retrievalmarket.ProviderDealIdentifier]blockio.BlockReader
	stateMachines           fsm.Group
}

var _ retrievalmarket.RetrievalProvider = &provider{}

// DefaultPricePerByte is the charge per byte retrieved if the miner does
// not specifically set it
var DefaultPricePerByte = abi.NewTokenAmount(2)

// DefaultPaymentInterval is the baseline interval, set to 1Mb
// if the miner does not explicitly set it otherwise
var DefaultPaymentInterval = uint64(1 << 20)

// DefaultPaymentIntervalIncrease is the amount interval increases on each payment,
// set to to 1Mb if the miner does not explicitly set it otherwise
var DefaultPaymentIntervalIncrease = uint64(1 << 20)

// NewProvider returns a new retrieval provider
func NewProvider(minerAddress address.Address, node retrievalmarket.RetrievalProviderNode, network rmnet.RetrievalMarketNetwork, pieceStore piecestore.PieceStore, bs blockstore.Blockstore, ds datastore.Batching) (retrievalmarket.RetrievalProvider, error) {

	p := &provider{
		bs:                      bs,
		node:                    node,
		network:                 network,
		minerAddress:            minerAddress,
		pieceStore:              pieceStore,
		pricePerByte:            DefaultPricePerByte, // TODO: allow setting
		paymentInterval:         DefaultPaymentInterval,
		paymentIntervalIncrease: DefaultPaymentIntervalIncrease,
		dealStreams:             make(map[retrievalmarket.ProviderDealIdentifier]rmnet.RetrievalDealStream),
		blockReaders:            make(map[retrievalmarket.ProviderDealIdentifier]blockio.BlockReader),
	}
	statemachines, err := fsm.New(ds, fsm.Parameters{
		Environment:     p,
		StateType:       retrievalmarket.ProviderDealState{},
		StateKeyField:   "Status",
		Events:          providerstates.ProviderEvents,
		StateEntryFuncs: providerstates.ProviderStateEntryFuncs,
		Notifier:        p.notifySubscribers,
	})
	p.stateMachines = statemachines
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Stop stops handling incoming requests
func (p *provider) Stop() error {
	p.closeDealStreams()
	err := p.suspendDeals()
	if err != nil {
		return err
	}
	err = p.stateMachines.Stop(context.Background())
	if err != nil {
		return err
	}
	return p.network.StopHandlingRequests()
}

// Start begins listening for deals on the given host
func (p *provider) Start() error {
	if err := p.restartDeals(); err != nil {
		return err
	}
	return p.network.SetDelegate(p)
}

// V0
// SetPricePerByte sets the price per byte a miner charges for retrievals
func (p *provider) SetPricePerByte(price abi.TokenAmount) {
	p.pricePerByte = price
}

// SetPaymentInterval sets the maximum number of bytes a a provider will send before
// requesting further payment, and the rate at which that value increases
func (p *provider) SetPaymentInterval(paymentInterval uint64, paymentIntervalIncrease uint64) {
	p.paymentInterval = paymentInterval
	p.paymentIntervalIncrease = paymentIntervalIncrease
}

// unsubscribeAt returns a function that removes an item from the subscribers list by comparing
// their reflect.ValueOf before pulling the item out of the slice.  Does not preserve order.
// Subsequent, repeated calls to the func with the same Subscriber are a no-op.
func (p *provider) unsubscribeAt(sub retrievalmarket.ProviderSubscriber) retrievalmarket.Unsubscribe {
	return func() {
		p.subscribersLk.Lock()
		defer p.subscribersLk.Unlock()
		curLen := len(p.subscribers)
		for i, el := range p.subscribers {
			if reflect.ValueOf(sub) == reflect.ValueOf(el) {
				p.subscribers[i] = p.subscribers[curLen-1]
				p.subscribers = p.subscribers[:curLen-1]
				return
			}
		}
	}
}

func (p *provider) notifySubscribers(eventName fsm.EventName, state fsm.StateType) {
	p.subscribersLk.RLock()
	defer p.subscribersLk.RUnlock()
	evt := eventName.(retrievalmarket.ProviderEvent)
	ds := state.(retrievalmarket.ProviderDealState)
	for _, cb := range p.subscribers {
		cb(evt, ds)
	}
}

// SubscribeToEvents listens for events that happen related to client retrievals
func (p *provider) SubscribeToEvents(subscriber retrievalmarket.ProviderSubscriber) retrievalmarket.Unsubscribe {
	p.subscribersLk.Lock()
	p.subscribers = append(p.subscribers, subscriber)
	p.subscribersLk.Unlock()

	return p.unsubscribeAt(subscriber)
}

// V1
func (p *provider) SetPricePerUnseal(price abi.TokenAmount) {
	panic("not implemented")
}

func (p *provider) ListDeals() map[retrievalmarket.ProviderDealID]retrievalmarket.ProviderDealState {
	panic("not implemented")
}

func (p *provider) HandleQueryStream(stream rmnet.RetrievalQueryStream) {
	defer stream.Close()
	query, err := stream.ReadQuery()
	if err != nil {
		return
	}

	answer := retrievalmarket.QueryResponse{
		Status:                     retrievalmarket.QueryResponseUnavailable,
		PieceCIDFound:              retrievalmarket.QueryItemUnavailable,
		MinPricePerByte:            p.pricePerByte,
		MaxPaymentInterval:         p.paymentInterval,
		MaxPaymentIntervalIncrease: p.paymentIntervalIncrease,
	}

	ctx := context.TODO()

	tok, _, err := p.node.GetChainHead(ctx)
	if err != nil {
		log.Errorf("Retrieval query: GetChainHead: %s", err)
		return
	}

	paymentAddress, err := p.node.GetMinerWorkerAddress(ctx, p.minerAddress, tok)
	if err != nil {
		log.Errorf("Retrieval query: Lookup Payment Address: %s", err)
		answer.Status = retrievalmarket.QueryResponseError
		answer.Message = err.Error()
	} else {
		answer.PaymentAddress = paymentAddress

		pieceCID := cid.Undef
		if query.PieceCID != nil {
			pieceCID = *query.PieceCID
		}
		pieceInfo, err := getPieceInfoFromCid(p.pieceStore, query.PayloadCID, pieceCID)

		if err == nil && len(pieceInfo.Deals) > 0 {
			answer.Status = retrievalmarket.QueryResponseAvailable
			// TODO: get price, look for already unsealed ref to reduce work
			answer.Size = uint64(pieceInfo.Deals[0].Length) // TODO: verify on intermediate
			answer.PieceCIDFound = retrievalmarket.QueryItemAvailable
		}

		if err != nil && !xerrors.Is(err, retrievalmarket.ErrNotFound) {
			log.Errorf("Retrieval query: GetRefs: %s", err)
			answer.Status = retrievalmarket.QueryResponseError
			answer.Message = err.Error()
		}

	}
	if err := stream.WriteQueryResponse(answer); err != nil {
		log.Errorf("Retrieval query: WriteCborRPC: %s", err)
		return
	}
}

func (p *provider) HandleDealStream(stream rmnet.RetrievalDealStream) {
	// read deal proposal (or fail)
	err := p.newProviderDeal(stream)
	if err != nil {
		log.Error(err)
		stream.Close()
	}
}

func (p *provider) newProviderDeal(stream rmnet.RetrievalDealStream) error {
	dealProposal, err := stream.ReadDealProposal()
	if err != nil {
		return err
	}

	pds := retrievalmarket.ProviderDealState{
		DealProposal: dealProposal,
		Receiver:     stream.Receiver(),
	}

	p.dealStreams[pds.Identifier()] = stream

	loaderWithUnsealing := blockunsealing.NewLoaderWithUnsealing(context.TODO(), p.bs, p.pieceStore, cario.NewCarIO(), p.node.UnsealSector, dealProposal.PieceCID)

	// validate the selector, if provided
	var sel ipld.Node
	if dealProposal.Params.Selector != nil {
		sel, err = retrievalmarket.DecodeNode(dealProposal.Params.Selector)
		if err != nil {
			return xerrors.Errorf("selector is invalid: %w", err)
		}
	} else {
		sel = shared.AllSelector()
	}

	br := blockio.NewSelectorBlockReader(cidlink.Link{Cid: dealProposal.PayloadCID}, sel, loaderWithUnsealing.Load)
	p.blockReaders[pds.Identifier()] = br

	// start the deal processing, synchronously so we can log the error and close the stream if it doesn't start
	err = p.stateMachines.Begin(pds.Identifier(), &pds)
	if err != nil {
		return err
	}

	err = p.stateMachines.Send(pds.Identifier(), retrievalmarket.ProviderEventOpen)
	if err != nil {
		return err
	}

	return nil
}

func (p *provider) Node() retrievalmarket.RetrievalProviderNode {
	return p.node
}

func (p *provider) DealStream(id retrievalmarket.ProviderDealIdentifier) rmnet.RetrievalDealStream {
	return p.dealStreams[id]
}

func (p *provider) CheckDealParams(pricePerByte abi.TokenAmount, paymentInterval uint64, paymentIntervalIncrease uint64) error {
	if pricePerByte.LessThan(p.pricePerByte) {
		return errors.New("Price per byte too low")
	}
	if paymentInterval > p.paymentInterval {
		return errors.New("Payment interval too large")
	}
	if paymentIntervalIncrease > p.paymentIntervalIncrease {
		return errors.New("Payment interval increase too large")
	}
	return nil
}

func (p *provider) NextBlock(ctx context.Context, id retrievalmarket.ProviderDealIdentifier) (retrievalmarket.Block, bool, error) {
	br, ok := p.blockReaders[id]
	if !ok {
		return retrievalmarket.Block{}, false, errors.New("Could not read block")
	}
	return br.ReadBlock(ctx)
}

func (p *provider) GetPieceSize(c cid.Cid) (uint64, error) {
	pieceInfo, err := getPieceInfoFromCid(p.pieceStore, c, cid.Undef)
	if err != nil {
		return 0, err
	}
	if len(pieceInfo.Deals) == 0 {
		return 0, errors.New("Not enough piece info")
	}
	return pieceInfo.Deals[0].Length, nil
}

func (p *provider) closeDealStreams() {
	for _, ds := range p.dealStreams {
		if err := ds.Close(); err != nil {
			log.Error(err)
		}
	}
}

func (p *provider) suspendDeals() error {
	return p.foreachDealState(
		func(pds retrievalmarket.ProviderDealState) error {
			if err := p.stateMachines.Send(pds.Identifier(), retrievalmarket.ProviderEventDealSuspended); err != nil {
				return err
			}
			return p.stateMachines.Stop(context.Background())
		})
}

func (p *provider) restartDeals() error {
	return p.foreachDealState(
		func(pds retrievalmarket.ProviderDealState) error {
			return p.stateMachines.Send(pds.Identifier(), retrievalmarket.ProviderEventDealResumed)
		})
}

func (p *provider) foreachDealState(do func(state retrievalmarket.ProviderDealState) error) error {
	var deals []retrievalmarket.ProviderDealState
	err := p.stateMachines.List(&deals)
	if err != nil {
		return err
	}
	for _, pds := range deals {
		if err = do(pds); err != nil {
			return err
		}
	}
	return nil
}

func getPieceInfoFromCid(pieceStore piecestore.PieceStore, payloadCID, pieceCID cid.Cid) (piecestore.PieceInfo, error) {
	cidInfo, err := pieceStore.GetCIDInfo(payloadCID)
	if err != nil {
		return piecestore.PieceInfoUndefined, xerrors.Errorf("get cid info: %w", err)
	}
	var lastErr error
	for _, pieceBlockLocation := range cidInfo.PieceBlockLocations {
		pieceInfo, err := pieceStore.GetPieceInfo(pieceBlockLocation.PieceCID)
		if err == nil {
			if pieceCID.Equals(cid.Undef) || pieceInfo.PieceCID.Equals(pieceCID) {
				return pieceInfo, nil
			}
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = xerrors.Errorf("unknown pieceCID %s", pieceCID.String())
	}
	return piecestore.PieceInfoUndefined, xerrors.Errorf("could not locate piece: %w", lastErr)
}
