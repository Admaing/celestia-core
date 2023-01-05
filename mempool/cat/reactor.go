package cat

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"

	cfg "github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/crypto/tmhash"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/mempool"
	"github.com/tendermint/tendermint/p2p"
	protomem "github.com/tendermint/tendermint/proto/tendermint/mempool"
	"github.com/tendermint/tendermint/types"
)

const (
	// default duration to wait before considering a peer non-responsive
	// and searching for the tx from a new peer
	defaultGossipDelay = 200 * time.Millisecond

	// Content Addressable Tx Pool gossips state based messages (SeenTx and WantTx) on a separate channel
	// for cross compatibility
	MempoolStateChannel = byte(0x31)

	// tx_key + node_id + buffer (for proto encoding)
	maxStateChannelSize = tmhash.Size + (2 * p2p.IDByteLength) + 10
)

// Reactor handles mempool tx broadcasting logic amongst peers. For the main
// logic behind the protocol, refer to `ReceiveEnvelope` or to the english
// spec under /.spec.md
type Reactor struct {
	p2p.BaseReactor
	opts     *ReactorOptions
	mempool  *TxPool
	ids      *mempoolIDs
	requests *requestScheduler
}

type ReactorOptions struct {
	// ListenOnly means that the node will never broadcast any of the transactions that
	// it receives. This is useful for keeping transactions private
	ListenOnly bool

	// MaxTxSize is the maximum size of a transaction that can be received
	MaxTxSize int

	// MaxGossipDelay is the maximum allotted time that the reactor expects a transaction to
	// arrive before issuing a new request to a different peer
	MaxGossipDelay time.Duration
}

func (opts *ReactorOptions) VerifyAndComplete() error {
	if opts.MaxTxSize == 0 {
		opts.MaxTxSize = cfg.DefaultMempoolConfig().MaxTxBytes
	}

	if opts.MaxGossipDelay == 0 {
		opts.MaxGossipDelay = defaultGossipDelay
	}

	if opts.MaxTxSize < 0 {
		return fmt.Errorf("max tx size (%d) cannot be negative", opts.MaxTxSize)
	}

	if opts.MaxGossipDelay < 0 {
		return fmt.Errorf("max gossip delay (%d) cannot be negative", opts.MaxGossipDelay)
	}

	return nil
}

// NewReactor returns a new Reactor with the given config and mempool.
func NewReactor(mempool *TxPool, opts *ReactorOptions) (*Reactor, error) {
	err := opts.VerifyAndComplete()
	if err != nil {
		return nil, err
	}
	memR := &Reactor{
		opts:     opts,
		mempool:  mempool,
		ids:      newMempoolIDs(),
		requests: newRequestScheduler(opts.MaxGossipDelay, defaultGlobalRequestTimeout),
	}
	memR.BaseReactor = *p2p.NewBaseReactor("Mempool", memR)
	return memR, nil
}

// SetLogger sets the Logger on the reactor and the underlying mempool.
func (memR *Reactor) SetLogger(l log.Logger) {
	memR.Logger = l
}

// OnStart implements p2p.BaseReactor.
func (memR *Reactor) OnStart() error {
	if memR.opts.ListenOnly {
		memR.Logger.Info("Tx broadcasting is disabled")
		return nil
	}
	go func() {
		for {
			select {
			case <-memR.Quit():
				return

			// listen in for any newly verified tx via RFC, then immediately
			// broadcasts it to all connected peers.
			case nextTxKey := <-memR.mempool.next():
				wtx := memR.mempool.store.get(nextTxKey)
				// tx may have been removed after it was added to the broadcast queue
				// thus we just skip over it
				if wtx != nil {
					memR.broadcastNewTx(wtx)
				}
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for {
			select {
			case <-memR.Quit():
				return
			case <-ticker.C:
				memR.dumpMetrics()
			}
		}
	}()
	return nil
}

func (memR *Reactor) OnStop() {
	// stop all the timers tracking outbound requests
	memR.requests.Close()
}

func (memR *Reactor) dumpMetrics() {
	memR.mempool.jsonMetrics.Save()
}

// GetChannels implements Reactor by returning the list of channels for this
// reactor.
func (memR *Reactor) GetChannels() []*p2p.ChannelDescriptor {
	largestTx := make([]byte, memR.opts.MaxTxSize)
	batchMsg := protomem.Message{
		Sum: &protomem.Message_Txs{
			Txs: &protomem.Txs{Txs: [][]byte{largestTx}},
		},
	}

	return []*p2p.ChannelDescriptor{
		{
			ID:                  mempool.MempoolChannel,
			Priority:            6,
			RecvMessageCapacity: batchMsg.Size(),
		},
		{
			ID:                  MempoolStateChannel,
			Priority:            5,
			RecvMessageCapacity: maxStateChannelSize,
		},
	}
}

// InitPeer implements Reactor by creating a state for the peer.
func (memR *Reactor) InitPeer(peer p2p.Peer) p2p.Peer {
	memR.ids.ReserveForPeer(peer)
	return peer
}

// AddPeer implements Reactor.
// It starts a broadcast routine ensuring all txs are forwarded to the given peer.
func (memR *Reactor) AddPeer(peer p2p.Peer) {
	if !memR.opts.ListenOnly {
		if memR.mempool.Size() > 0 {
			peerID := memR.ids.GetIDForPeer(peer.ID())
			memR.sendAllTxKeys(peer, peerID)
		}
	}
}

// RemovePeer implements Reactor.
func (memR *Reactor) RemovePeer(peer p2p.Peer, reason interface{}) {
	peerID := memR.ids.Reclaim(peer.ID())
	// remove and rerequest all pending outbound requests to that peer since we know
	// we won't receive any responses from them.
	outboundRequests := memR.requests.ClearAllRequestsFrom(peerID)
	for key := range outboundRequests {
		atomic.AddUint64(&memR.mempool.jsonMetrics.RerequestedTxs, 1)
		memR.findNewPeerToRequestTx(key)
	}
}

// ReceiveEnvelope implements Reactor.
// It adds any received transactions to the mempool.
func (memR *Reactor) Receive(chID byte, peer p2p.Peer, msgBytes []byte) {
	switch chID {
	case mempool.MempoolChannel:
		atomic.AddUint64(&memR.mempool.jsonMetrics.ReceivedTxBytes, uint64(len(msgBytes)))
	case MempoolStateChannel:
		atomic.AddUint64(&memR.mempool.jsonMetrics.ReceivedStateBytes, uint64(len(msgBytes)))
	}
	m := &protomem.Message{}
	err := proto.Unmarshal(msgBytes, m)
	if err != nil {
		panic(err)
	}

	if chID != MempoolStateChannel && chID != mempool.MempoolChannel {
		memR.Logger.Error("unsupported channel ID", "chId", chID)
		return
	}
	memR.Logger.Debug("received Mempool Message", "src", peer, "chId", chID, "msg", fmt.Sprintf("%T", m.Sum))

	switch msg := m.Sum.(type) {

	// A peer has sent us one or more transactions. This could be either because we requested them
	// or because the peer received a new transaction and is broadcasting it to us.
	// NOTE: This setup also means that we can support older mempool implementations that simply
	// flooded the network with transactions.
	case *protomem.Message_Txs:
		protoTxs := msg.Txs.GetTxs()
		if len(protoTxs) == 0 {
			memR.Logger.Error("received empty txs from peer", "src", peer)
			return
		}
		peerID := memR.ids.GetIDForPeer(peer.ID())
		txInfo := mempool.TxInfo{SenderID: peerID}
		txInfo.SenderP2PID = peer.ID()

		var err error
		for _, tx := range protoTxs {
			ntx := types.Tx(tx)
			key := ntx.Key()
			from := ""
			// If we requested the transaction we mark it as received.
			if memR.requests.Has(peerID, key) {
				memR.requests.MarkReceived(peerID, key)
				memR.Logger.Debug("received a response for a requested transaction", "peerID", peer.ID(), "txKey", key)
			} else {
				// If we didn't request the transaction we simply mark the peer as having the
				// tx (we'd have already done it if we were requesting the tx).
				memR.mempool.PeerHasTx(peerID, key)
				// We assume if we hadn't requested it, that this is the original recipient of
				// the transaction and we should notify others in our SeenTx that is broadcasted
				// to them
				from = string(txInfo.SenderP2PID)
				memR.Logger.Debug("received new trasaction", "from", from, "txKey", key)
			}
			_, err = memR.mempool.TryAddNewTx(ntx, key, txInfo)
			if err != nil && err != ErrTxInMempool {
				memR.Logger.Info("Could not add tx", "txKey", key, "err", err)
				return
			}
			if !memR.opts.ListenOnly {
				// We broadcast only transactions that we deem valid and actually have in our mempool.
				memR.broadcastSeenTx(key, from)
			}
		}

	// A peer has indicated to us that it has a transaction. We first verify the txkey and
	// mark that peer as having the transaction. Then we proceed with the following logic:
	//
	// 1. If we have the transaction, we do nothing.
	// 2. If we don't have the transaction, we check if the original sender is a peer we are
	// connected to. The original recipients of a transaction will immediately broadcast it
	// to everyone so if we haven't received it yet we will likely receive it soon. Therefore,
	// we set a timer to check after a certain amount of time if we still don't have the transaction.
	// 3. If we're not connected to the original sender, or we exceed the timeout without
	// receiving the transaction we request it from this peer.
	case *protomem.Message_SeenTx:
		txKey, err := types.TxKeyFromBytes(msg.SeenTx.TxKey)
		if err != nil {
			memR.Logger.Error("peer sent SeenTx with incorrect tx key", "err", err)
			memR.Switch.StopPeerForError(peer, err)
			return
		}
		peerID := memR.ids.GetIDForPeer(peer.ID())
		memR.mempool.PeerHasTx(peerID, txKey)
		// Check if we don't already have the transaction and that it was recently rejected
		if !memR.mempool.Has(txKey) && !memR.mempool.IsRejectedTx(txKey) {
			// If we are already requesting that tx, then we don't need to go any further.
			if memR.requests.ForTx(txKey) != 0 {
				memR.Logger.Debug("received a SeenTx message for a transaction we are already requesting", "txKey", txKey)
				return
			}

			// If it was a low-priority transaction and we don't have capacity, then ignore.
			if memR.mempool.WasRecentlyEvicted(txKey) {
				if !memR.mempool.CanFitEvictedTx(txKey) {
					return
				}
			}

			// Check if `From` is specified and if we are connected to the peer that originally sent the transaction
			from := ""
			if msg.SeenTx.XFrom != nil {
				from = msg.SeenTx.XFrom.(*protomem.SeenTx_From).From
			}
			if from != "" && memR.ids.GetIDForPeer(p2p.ID(from)) != 0 {
				memR.Logger.Debug("received a SeenTx message that originally came from a peer we are connected to. Waiting for the new transaction.",
					"txKey", txKey)
				// We are connected to the peer that originally sent the transaction so we
				// assume there's a high probability that the original sender will also
				// send us the transaction. We set a timeout in case this is not true.
				timer := time.NewTimer(memR.opts.MaxGossipDelay)
				select {
				case <-timer.C:
					// If we still don't have the transaction after the timeout, we find a new peer to request the tx
					if !memR.mempool.Has(txKey) && !memR.mempool.IsRejectedTx(txKey) {
						memR.Logger.Debug("timed out waiting for original sender, requesting tx from another peer...")
						// NOTE: During this period, the peer may, for some reason have disconnected from us.
						if memR.ids.GetPeer(peerID) == nil {
							// Get the first peer from the set
							memR.findNewPeerToRequestTx(txKey)
						} else {
							// We're still connected to the peer that sent us the SeenTx so request
							// the transaction from them
							memR.requestTx(txKey, peer)
						}
					}
				case <-memR.Quit():
					return
				}
			} else {
				// We are not connected to the peer that originally sent the transaction or
				// the sender learned of the transaction through another peer.
				// We therefore request the tx from the peer that sent us the SeenTx message
				memR.requestTx(txKey, peer)
			}
		} else {
			memR.Logger.Debug("received a seen tx for a tx we already have", "txKey", txKey)
		}

	// A peer is requesting a transaction that we have claimed to have. Find the specified
	// transaction and broadcast it to the peer. We may no longer have the transaction
	case *protomem.Message_WantTx:
		txKey, err := types.TxKeyFromBytes(msg.WantTx.TxKey)
		if err != nil {
			memR.Logger.Error("peer sent WantTx with incorrect tx key", "err", err)
			memR.Switch.StopPeerForError(peer, err)
			return
		}
		tx, has := memR.mempool.Get(txKey)
		if has && !memR.opts.ListenOnly {
			peerID := memR.ids.GetIDForPeer(peer.ID())
			memR.Logger.Debug("sending a tx in response to a want msg", "peer", peerID)
			msg := &protomem.Message{
				Sum: &protomem.Message_Txs{
					Txs: &protomem.Txs{Txs: [][]byte{tx}},
				},
			}
			bz, err := msg.Marshal()
			if err != nil {
				panic(err)
			}

			if peer.Send(mempool.MempoolChannel, bz) {
				atomic.AddUint64(&memR.mempool.jsonMetrics.SentTransactionBytes, uint64(len(bz)))
				memR.mempool.PeerHasTx(peerID, txKey)
			}
		}
		if !has {
			atomic.AddUint64(&memR.mempool.jsonMetrics.FailedResponses, 1)
		}

	default:
		memR.Logger.Error("unknown message type", "src", peer, "chId", chID, "msg", fmt.Sprintf("%T", msg))
		memR.Switch.StopPeerForError(peer, fmt.Errorf("mempool cannot handle message of type: %T", msg))
		return
	}
}

func (memR *Reactor) FetchTxsFromKeys(ctx context.Context, compactData [][]byte) ([][]byte, error) {
	txs := make([][]byte, len(compactData))
	for i, key := range compactData {
		txKey, err := types.TxKeyFromBytes(key)
		if err != nil {
			return nil, fmt.Errorf("incorrect compact blocks format: %w", err)
		}
		wtx := memR.mempool.store.get(txKey)
		if wtx != nil {
			txs[i] = wtx.tx
		}

	}
	return txs, nil

}

// PeerState describes the state of a peer.
type PeerState interface {
	GetHeight() int64
}

// broadcastSeenTx broadcasts a SeenTx message to all peers unless we
// know they have already seen the transaction
func (memR *Reactor) broadcastSeenTx(txKey types.TxKey, fromPeer string) {
	memR.Logger.Debug("broadcasting seen tx to all peers", "tx_key", txKey.String())
	msg := &protomem.Message{
		Sum: &protomem.Message_SeenTx{
			SeenTx: &protomem.SeenTx{
				TxKey: txKey[:],
				XFrom: &protomem.SeenTx_From{From: fromPeer},
			},
		},
	}
	bz, err := msg.Marshal()
	if err != nil {
		panic(err)
	}
	for id, peer := range memR.ids.GetAll() {
		if p, ok := peer.Get(types.PeerStateKey).(PeerState); ok {
			// make sure peer isn't too far behind. This can happen
			// if the peer is blocksyncing still and catching up
			// in which case we just skip sending the transaction
			if p.GetHeight() < memR.mempool.Height()-20 {
				memR.Logger.Debug("peer is too far behind us. Skipping broadcast of seen tx")
				continue
			}
		}
		// no need to send a seen tx message to a peer that already
		// has that tx.
		if memR.mempool.seenByPeersSet.Has(txKey, id) {
			continue
		}

		if peer.Send(MempoolStateChannel, bz) {
			atomic.AddUint64(&memR.mempool.jsonMetrics.SentStateBytes, uint64(len(bz)))
		}
	}
}

// broadcastNewTx broadcast new transaction to all peers unless we are already sure they have seen the tx.
func (memR *Reactor) broadcastNewTx(tx *wrappedTx) {
	memR.Logger.Info("broadcasting new tx to all caught up peers", "tx_key", tx.key)
	msg := &protomem.Message{
		Sum: &protomem.Message_Txs{
			Txs: &protomem.Txs{
				Txs: [][]byte{tx.tx},
			},
		},
	}
	bz, err := msg.Marshal()
	if err != nil {
		panic(err)
	}

	for id, peer := range memR.ids.GetAll() {
		if p, ok := peer.Get(types.PeerStateKey).(PeerState); ok {
			// make sure peer isn't too far behind. This can happen
			// if the peer is blocksyncing still and catching up
			// in which case we just skip sending the transaction
			if p.GetHeight() < tx.height-20 {
				memR.Logger.Debug("peer is too far behind us. Skipping broadcast of seen tx")
				continue
			}
		}

		if memR.mempool.seenByPeersSet.Has(tx.key, id) {
			continue
		}

		if peer.Send(mempool.MempoolChannel, bz) {
			atomic.AddUint64(&memR.mempool.jsonMetrics.SentTransactionBytes, uint64(len(bz)))
			memR.mempool.PeerHasTx(id, tx.key)
		}
	}
}

// requestTx requests a transaction from a peer and tracks it,
// requesting it from another peer if the first peer does not respond.
func (memR *Reactor) requestTx(txKey types.TxKey, peer p2p.Peer) {
	if peer == nil {
		// we have disconnected from the peer
		return
	}
	memR.Logger.Debug("requesting tx", "txKey", txKey, "peerID", peer.ID())
	msg := &protomem.Message{
		Sum: &protomem.Message_WantTx{
			WantTx: &protomem.WantTx{TxKey: txKey[:]},
		},
	}
	bz, err := msg.Marshal()
	if err != nil {
		panic(err)
	}

	success := peer.Send(MempoolStateChannel, bz)
	if success {
		atomic.AddUint64(&memR.mempool.jsonMetrics.SentStateBytes, uint64(len(bz)))
		atomic.AddUint64(&memR.mempool.jsonMetrics.RequestedTxs, 1)
		requested := memR.requests.Add(txKey, memR.ids.GetIDForPeer(peer.ID()), memR.findNewPeerToRequestTx)
		if !requested {
			memR.Logger.Error("have already marked a tx as requested", "txKey", txKey, "peerID", peer.ID())
		}
	}
}

// findNewPeerToSendTx finds a new peer that has already seen the transaction to
// request a transaction from.
func (memR *Reactor) findNewPeerToRequestTx(txKey types.TxKey) {
	// ensure that we are connected to peers
	if memR.ids.Len() == 0 {
		return
	}

	// pop the next peer in the list of remaining peers that have seen the tx
	// and does not already have an outbound request for that tx
	seenMap := memR.mempool.seenByPeersSet.Get(txKey)
	var peerID uint16
	for possiblePeer := range seenMap {
		if !memR.requests.Has(possiblePeer, txKey) {
			peerID = possiblePeer
			break
		}
	}

	if peerID == 0 {
		// No other free peer has the transaction we are looking for.
		// We give up 🤷‍♂️ and hope either a peer responds late or the tx
		// is gossiped again
		atomic.AddUint64(&memR.mempool.jsonMetrics.LostTxs, 1)
		memR.Logger.Info("no other peer has the tx we are looking for", "txKey", txKey)
		return
	}
	peer := memR.ids.GetPeer(peerID)
	if peer == nil {
		// we disconnected from that peer, retry again until we exhaust the list
		memR.findNewPeerToRequestTx(txKey)
	} else {
		atomic.AddUint64(&memR.mempool.jsonMetrics.RerequestedTxs, 1)
		memR.requestTx(txKey, peer)
	}
}

// sendAllTxKeys loops through all txs currently in the mempool and iteratively
// sends a `SeenTx` message to the peer. This is added to a queue and will block
// when the queue becomes full.
func (memR *Reactor) sendAllTxKeys(peer p2p.Peer, peerID uint16) {
	memR.Logger.Debug("sending all seen txs to peer", "peerID", peerID)
	txKeys := memR.mempool.store.getAllKeys()
	for _, txKey := range txKeys {
		if memR.mempool.seenByPeersSet.Has(txKey, peerID) {
			continue
		}

		msg := &protomem.Message{
			Sum: &protomem.Message_SeenTx{
				SeenTx: &protomem.SeenTx{TxKey: txKey[:]},
			},
		}
		bz, err := msg.Marshal()
		if err != nil {
			panic(err)
		}

		if peer.Send(MempoolStateChannel, bz) {
			atomic.AddUint64(&memR.mempool.jsonMetrics.SentStateBytes, uint64(len(bz)))
		}
	}
}
