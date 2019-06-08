package gossip

import (
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	amino "github.com/tendermint/go-amino"

	cfg "github.com/chainpoint/tendermint/config"
	"github.com/chainpoint/tendermint/libs/clist"
	"github.com/chainpoint/tendermint/libs/log"
	"github.com/chainpoint/tendermint/p2p"
	"github.com/chainpoint/tendermint/types"
)

const (
	GossipChannel = byte(0x50)

	maxMsgSize = 1048576        // 1MB TODO make it configurable
	maxTxSize  = maxMsgSize - 8 // account for amino overhead of Message

	peerCatchupSleepIntervalMS = 100 // If peer is behind, sleep this amount

	// UnknownPeerID is the peer ID to use when running CheckTx when there is
	// no peer (e.g. RPC)
	UnknownPeerID uint16 = 0

	maxActiveIDs = math.MaxUint16
)

// GossipReactor handles mempool tx broadcasting amongst peers.
// It maintains a map from peer ID to counter, to prevent gossiping txs to the
// peers you received it from.
type GossipReactor struct {
	p2p.BaseReactor
	eventBus *types.EventBus
	config *cfg.MempoolConfig
	Gossip *Gossip
	ids    *gossipIDs
}

type gossipIDs struct {
	mtx       sync.RWMutex
	peerMap   map[p2p.ID]uint16
	nextID    uint16              // assumes that a node will never have over 65536 active peers
	activeIDs map[uint16]struct{} // used to check if a given peerID key is used, the value doesn't matter
}

// Reserve searches for the next unused ID and assignes it to the
// peer.
func (ids *gossipIDs) ReserveForPeer(peer p2p.Peer) {
	ids.mtx.Lock()
	defer ids.mtx.Unlock()

	curID := ids.nextPeerID()
	ids.peerMap[peer.ID()] = curID
	ids.activeIDs[curID] = struct{}{}
}

// nextPeerID returns the next unused peer ID to use.
// This assumes that ids's mutex is already locked.
func (ids *gossipIDs) nextPeerID() uint16 {
	if len(ids.activeIDs) == maxActiveIDs {
		panic(fmt.Sprintf("node has maximum %d active IDs and wanted to get one more", maxActiveIDs))
	}

	_, idExists := ids.activeIDs[ids.nextID]
	for idExists {
		ids.nextID++
		_, idExists = ids.activeIDs[ids.nextID]
	}
	curID := ids.nextID
	ids.nextID++
	return curID
}

// Reclaim returns the ID reserved for the peer back to unused pool.
func (ids *gossipIDs) Reclaim(peer p2p.Peer) {
	ids.mtx.Lock()
	defer ids.mtx.Unlock()

	removedID, ok := ids.peerMap[peer.ID()]
	if ok {
		delete(ids.activeIDs, removedID)
		delete(ids.peerMap, peer.ID())
	}
}

// GetForPeer returns an ID reserved for the peer.
func (ids *gossipIDs) GetForPeer(peer p2p.Peer) uint16 {
	ids.mtx.RLock()
	defer ids.mtx.RUnlock()

	return ids.peerMap[peer.ID()]
}

func newGossipIDs() *gossipIDs {
	return &gossipIDs{
		peerMap:   make(map[p2p.ID]uint16),
		activeIDs: map[uint16]struct{}{0: {}},
		nextID:    1, // reserve unknownPeerID(0) for mempoolReactor.BroadcastTx
	}
}

// NewMempoolReactor returns a new GossipReactor with the given config and mempool.
func NewGossipReactor(config *cfg.MempoolConfig, gossip *Gossip) *GossipReactor {
	memR := &GossipReactor{
		config: config,
		Gossip: gossip,
		ids:    newGossipIDs(),
	}
	memR.BaseReactor = *p2p.NewBaseReactor("GossipReactor", memR)
	return memR
}

// SetLogger sets the Logger on the reactor and the underlying Gossip.
func (memR *GossipReactor) SetLogger(l log.Logger) {
	memR.Logger = l
	memR.Gossip.SetLogger(l)
}

// OnStart implements p2p.BaseReactor.
func (memR *GossipReactor) OnStart() error {
	if !memR.config.Broadcast {
		memR.Logger.Info("Tx broadcasting is disabled")
	}
	return nil
}

// GetChannels implements Reactor.
// It returns the list of channels for this reactor.
func (memR *GossipReactor) GetChannels() []*p2p.ChannelDescriptor {
	return []*p2p.ChannelDescriptor{
		{
			ID:       GossipChannel,
			Priority: 5,
			SendQueueCapacity:   100,
			RecvBufferCapacity:  50 * 4096,
			RecvMessageCapacity: maxMsgSize,
		},
	}
}

// AddPeer implements Reactor.
// It starts a broadcast routine ensuring all txs are forwarded to the given peer.
func (memR *GossipReactor) AddPeer(peer p2p.Peer) {
	memR.ids.ReserveForPeer(peer)
	go memR.broadcastMsgRoutine(peer)
}

// SetEventBus sets the bus.
func (memR *GossipReactor) SetEventBus(b *types.EventBus) {
	memR.eventBus = b
}

// RemovePeer implements Reactor.
func (memR *GossipReactor) RemovePeer(peer p2p.Peer, reason interface{}) {
	memR.ids.Reclaim(peer)
	// broadcast routine checks if peer is gone and returns
}

// Receive implements Reactor.
// It adds any received transactions to the mempool.
func (memR *GossipReactor) Receive(chID byte, src p2p.Peer, msgBytes []byte) {
	msg, err := decodeMsg(msgBytes)
	if err != nil {
		fmt.Println(fmt.Sprintf("gossip nsg not decoded: %#v", err))
		memR.Logger.Error("Error decoding message", "src", src, "chId", chID, "msg", msg, "err", err, "bytes", msgBytes)
		memR.Switch.StopPeerForError(src, err)
		return
	}
	memR.Logger.Debug("Receive", "src", src, "chId", chID, "msg", msg)

	switch msg := msg.(type) {
	case *Message:
		fmt.Println(fmt.Sprintf("gossip msg received"))
		peerID := memR.ids.GetForPeer(src)
		err := memR.Gossip.DeliverMsgWithInfo(msg.Tx, nil, TxInfo{PeerID: peerID})
		if err != nil {
			fmt.Println(fmt.Sprintf("gossip msg delivery error"))
			memR.Logger.Info("Could not check tx", "tx", TxID(msg.Tx), "err", err)
		}
		// broadcasting happens from go routines per peer
	default:
		fmt.Println(fmt.Sprintf("gossip msg unknown"))
		memR.Logger.Error(fmt.Sprintf("Unknown message type %v", reflect.TypeOf(msg)))
	}
}

// PeerState describes the state of a peer.
type PeerState interface {
	GetHeight() int64
}

// Send new mempool txs to peer.
func (memR *GossipReactor) broadcastMsgRoutine(peer p2p.Peer) {
	fmt.Println("gossip: starting broadcast routine")
	if !memR.config.Broadcast {
		return
	}

	peerID := memR.ids.GetForPeer(peer)
	var next *clist.CElement
	for {
		// In case of both next.NextWaitChan() and peer.Quit() are variable at the same time
		if !memR.IsRunning() || !peer.IsRunning() {
			return
		}
		// This happens because the CElement we were looking at got garbage
		// collected (removed). That is, .NextWait() returned nil. Go ahead and
		// start from the beginning.
		if next == nil {
			select {
			case <-memR.Gossip.TxsWaitChan(): // Wait until a tx is available
				if next = memR.Gossip.TxsFront(); next == nil {
					continue
				}
			case <-peer.Quit():
				return
			case <-memR.Quit():
				return
			}
		}

		memTx := next.Value.(*gossipTx)

		// make sure the peer is up to date
		peerState, ok := peer.Get(types.PeerStateKey).(PeerState)
		if !ok {
			fmt.Println(fmt.Sprintf("gossip peer not ok: %#v", peerState))
			// Peer does not have a state yet. We set it in the consensus reactor, but
			// when we add peer in Switch, the order we call reactors#AddPeer is
			// different every time due to us using a map. Sometimes other reactors
			// will be initialized before the consensus reactor. We should wait a few
			// milliseconds and retry.
			time.Sleep(peerCatchupSleepIntervalMS * time.Millisecond)
			continue
		}
		if peerState.GetHeight() < memTx.Height()-1 { // Allow for a lag of 1 block
			time.Sleep(peerCatchupSleepIntervalMS * time.Millisecond)
			continue
		}

		// ensure peer hasn't already sent us this tx
		if _, ok := memTx.senders.Load(peerID); !ok {
			// send memTx
			msg := &Message{Tx: memTx.msg}
			success := peer.Send(GossipChannel, cdc.MustMarshalBinaryBare(msg))
			if !success {
				time.Sleep(peerCatchupSleepIntervalMS * time.Millisecond)
				continue
			}
		}

		select {
		case <-next.NextWaitChan():
			// see the start of the for loop for nil check
			next = next.Next()
		case <-peer.Quit():
			return
		case <-memR.Quit():
			return
		}
	}
}

//-----------------------------------------------------------------------------
// Messages

// GossipMessage is a message sent or received by the GossipReactor.
type GossipMessage interface{}

func RegisterGossipMessages(cdc *amino.Codec) {
	cdc.RegisterInterface((*GossipMessage)(nil), nil)
	cdc.RegisterConcrete(&Message{}, "tendermint/Gossip/Message", nil)
}

func decodeMsg(bz []byte) (msg GossipMessage, err error) {
	if len(bz) > maxMsgSize {
		return msg, fmt.Errorf("Msg exceeds max size (%d > %d)", len(bz), maxMsgSize)
	}
	err = cdc.UnmarshalBinaryBare(bz, &msg)
	if err != nil {
		fmt.Println(fmt.Sprintf("gossip decoding error: %#v", err))
	}
	return
}

//-------------------------------------

// Message is a GossipMessage containing a transaction.
type Message struct {
	Tx types.Tx
}

// String returns a string representation of the Message.
func (m *Message) String() string {
	return fmt.Sprintf("[Message %v]", m.Tx)
}
