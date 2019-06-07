package gossip

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	cfg "github.com/chainpoint/tendermint/config"

	abci "github.com/chainpoint/tendermint/abci/types"
	"github.com/chainpoint/tendermint/libs/clist"
	"github.com/chainpoint/tendermint/libs/log"
	"github.com/chainpoint/tendermint/proxy"
	"github.com/chainpoint/tendermint/types"
)

// TxInfo are parameters that get passed when attempting to add a msg to the
// mempool.
type TxInfo struct {
	// We don't use p2p.ID here because it's too big. The gain is to store max 2
	// bytes with each msg to identify the sender rather than 20 bytes.
	PeerID uint16
}

/*

The mempool pushes new msgs onto the proxyAppConn.
It gets a stream of (req, res) tuples from the proxy.
The mempool stores good msgs in a concurrent linked-list.

Multiple concurrent go-routines can traverse this linked-list
safely by calling .NextWait() on each element.

So we have several go-routines:
1. Consensus calling Update() and Reap() synchronously
2. Many mempool reactor's peer routines calling CheckTx()
3. Many mempool reactor's peer routines traversing the msgs linked list
4. Another goroutine calling GarbageCollectTxs() periodically

To manage these goroutines, there are three methods of locking.
1. Mutations to the linked-list is protected by an internal mmsg (CList is goroutine-safe)
2. Mutations to the linked-list elements are atomic
3. CheckTx() calls can be paused upon Update() and Reap(), protected by .proxyMmsg

Garbage collection of old elements from mempool.msgs is handlde via
the DetachPrev() call, which makes old elements not reachable by
peer broadcastMsgRoutine() automatically garbage collected.

TODO: Better handle abci client errors. (make it automatically handle connection errors)

*/

var (
	// ErrTxInCache is returned to the client if we saw msg earlier
	ErrTxInCache = errors.New("Tx already exists in cache")

	// ErrTxTooLarge means the msg is too big to be sent in a message to other peers
	ErrTxTooLarge = fmt.Errorf("Tx too large. Max size is %d", maxTxSize)
)

// TxID is the hex encoded hash of the bytes as a types.Tx.
func TxID(msg []byte) string {
	return fmt.Sprintf("%X", types.Tx(msg).Hash())
}

// msgKey is the fixed length array sha256 hash used as the key in maps.
func msgKey(msg types.Tx) [sha256.Size]byte {
	return sha256.Sum256(msg)
}

// Gossip is an ordered in-memory pool for transactions before they are proposed in a consensus
// round. Transaction validity is checked using the CheckTx abci message before the transaction is
// added to the pool. The Gossip uses a concurrent list structure for storing transactions that
// can be efficiently accessed by multiple concurrent readers.
type Gossip struct {
	config *cfg.MempoolConfig

	proxyMmsg     sync.Mutex
	proxyAppConn proxy.AppConnGossip
	msgs          *clist.CList // concurrent linked-list of good msgs

	// Track whether we're rechecking msgs.
	// These are not protected by a mutex and are expected to be mutated
	// in serial (ie. by abci responses which are called in serial).
	recheckCursor *clist.CElement // next expected response
	recheckEnd    *clist.CElement // re-checking stops here

	// notify listeners (ie. consensus) when msgs are available
	notifiedTxsAvailable bool
	msgsAvailable         chan struct{} // fires once for each height, when the mempool is not empty

	// Map for quick access to msgs to record sender in CheckTx.
	// msgsMap: msgKey -> CElement
	msgsMap sync.Map

	// Atomic integers
	height     int64 // the last block Update()'d to
	rechecking int32 // for re-checking filtered msgs on Update()
	msgsBytes   int64 // total size of mempool, in bytes

	// Keep a cache of already-seen msgs.
	// This reduces the pressure on the proxyApp.
	cache msgCache

	logger log.Logger

	metrics *Metrics
}

// NewMempool returns a new Gossip with the given configuration and connection to an application.
func NewGossip(
	config *cfg.MempoolConfig,
	proxyAppConn proxy.AppConnGossip,
	height int64,
) *Gossip {
	gossip := &Gossip{
		config: 	   config,
		proxyAppConn:  proxyAppConn,
		msgs:          clist.New(),
		height:        height,
		rechecking:    0,
		recheckCursor: nil,
		recheckEnd:    nil,
		logger:        log.NewNopLogger(),
		metrics:       NopMetrics(),
	}
	gossip.cache = newMapTxCache(10000)
	proxyAppConn.SetResponseCallback(gossip.globalCb)
	return gossip
}

// EnableTxsAvailable initializes the TxsAvailable channel,
// ensuring it will trigger once every height when transactions are available.
// NOTE: not thread safe - should only be called once, on startup
func (gos *Gossip) EnableTxsAvailable() {
	gos.msgsAvailable = make(chan struct{}, 1)
}

// SetLogger sets the Logger.
func (gos *Gossip) SetLogger(l log.Logger) {
	gos.logger = l
}

// Lock locks the mempool. The consensus must be able to hold lock to safely update.
func (gos *Gossip) Lock() {
	gos.proxyMmsg.Lock()
}

// Unlock unlocks the mempool.
func (gos *Gossip) Unlock() {
	gos.proxyMmsg.Unlock()
}

// Size returns the number of transactions in the mempool.
func (gos *Gossip) Size() int {
	return gos.msgs.Len()
}

// TxsBytes returns the total size of all msgs in the mempool.
func (gos *Gossip) TxsBytes() int64 {
	return atomic.LoadInt64(&gos.msgsBytes)
}

// FlushAppConn flushes the mempool connection to ensure async reqResCb calls are
// done. E.g. from CheckTx.
func (gos *Gossip) FlushAppConn() error {
	return gos.proxyAppConn.FlushSync()
}

// Flush removes all transactions from the mempool and cache
func (gos *Gossip) Flush() {
	gos.proxyMmsg.Lock()
	defer gos.proxyMmsg.Unlock()

	gos.cache.Reset()

	for e := gos.msgs.Front(); e != nil; e = e.Next() {
		gos.msgs.Remove(e)
		e.DetachPrev()
	}

	gos.msgsMap = sync.Map{}
	_ = atomic.SwapInt64(&gos.msgsBytes, 0)
}

// TxsFront returns the first transaction in the ordered list for peer
// goroutines to call .NextWait() on.
func (gos *Gossip) TxsFront() *clist.CElement {
	return gos.msgs.Front()
}

// TxsWaitChan returns a channel to wait on transactions. It will be closed
// once the mempool is not empty (ie. the internal `gos.msgs` has at least one
// element)
func (gos *Gossip) TxsWaitChan() <-chan struct{} {
	return gos.msgs.WaitChan()
}

// CheckTx executes a new transaction against the application to determine its validity
// and whether it should be added to the mempool.
// It blocks if we're waiting on Update() or Reap().
// cb: A callback from the CheckTx command.
//     It gets called from another goroutine.
// CONTRACT: Either cb will get called, or err returned.
func (gos *Gossip) DeliverMsg(msg types.Tx, cb func(*abci.Response)) (err error) {
	return gos.DeliverMsgWithInfo(msg, cb, TxInfo{PeerID: UnknownPeerID})
}

// CheckTxWithInfo performs the same operation as CheckTx, but with extra meta data about the msg.
// Currently this metadata is the peer who sent it,
// used to prevent the msg from being gossiped back to them.
func (gos *Gossip) DeliverMsgWithInfo(msg types.Tx, cb func(*abci.Response), msgInfo TxInfo) (err error) {
	gos.proxyMmsg.Lock()
	// use defer to unlock mutex because application (*local client*) might panic
	defer gos.proxyMmsg.Unlock()
	// CACHE
	if !gos.cache.Push(msg) {
		// Record a new sender for a msg we've already seen.
		// Note it's possible a msg is still in the cache but no longer in the mempool
		// (eg. after committing a block, msgs are removed from mempool but not cache),
		// so we only record the sender for msgs still in the mempool.
		if e, ok := gos.msgsMap.Load(msgKey(msg)); ok {
			memTx := e.(*clist.CElement).Value.(*gossipTx)
			if _, loaded := memTx.senders.LoadOrStore(msgInfo.PeerID, true); loaded {
				// TODO: consider punishing peer for dups,
				// its non-trivial since invalid msgs can become valid,
				// but they can spam the same msg with little cost to them atm.
			}
		}

		return ErrTxInCache
	}
	// END WAL

	// NOTE: proxyAppConn may error if msg buffer is full
	if err = gos.proxyAppConn.Error(); err != nil {
		return err
	}

	reqRes := gos.proxyAppConn.DeliverMsgAsync(msg)
	reqRes.SetCallback(gos.reqResCb(msg, msgInfo.PeerID, cb))

	return nil
}

// Global callback that will be called after every ABCI response.
// Having a single global callback avoids needing to set a callback for each request.
// However, processing the checkTx response requires the peerID (so we can track which msgs we heard from who),
// and peerID is not included in the ABCI request, so we have to set request-specific callbacks that
// include this information. If we're not in the midst of a recheck, this function will just return,
// so the request specific callback can do the work.
// When rechecking, we don't need the peerID, so the recheck callback happens here.
func (gos *Gossip) globalCb(req *abci.Request, res *abci.Response) {
	if gos.recheckCursor == nil {
		return
	}

	gos.metrics.RecheckTimes.Add(1)
	gos.resCbRecheck(req, res)

	// update metrics
	gos.metrics.Size.Set(float64(gos.Size()))
}

// Request specific callback that should be set on individual reqRes objects
// to incorporate local information when processing the response.
// This allows us to track the peer that sent us this msg, so we can avoid sending it back to them.
// NOTE: alternatively, we could include this information in the ABCI request itself.
//
// External callers of CheckTx, like the RPC, can also pass an externalCb through here that is called
// when all other response processing is complete.
//
// Used in CheckTxWithInfo to record PeerID who sent us the msg.
func (gos *Gossip) reqResCb(msg []byte, peerID uint16, externalCb func(*abci.Response)) func(res *abci.Response) {
	return func(res *abci.Response) {
		if gos.recheckCursor != nil {
			// this should never happen
			panic("recheck cursor is not nil in reqResCb")
		}

		gos.resCbFirstTime(msg, peerID, res)

		// update metrics
		gos.metrics.Size.Set(float64(gos.Size()))

		// passed in by the caller of CheckTx, eg. the RPC
		if externalCb != nil {
			externalCb(res)
		}
	}
}

// Called from:
//  - resCbFirstTime (lock not held) if msg is valid
func (gos *Gossip) addTx(memTx *gossipTx) {
	e := gos.msgs.PushBack(memTx)
	gos.msgsMap.Store(msgKey(memTx.msg), e)
	atomic.AddInt64(&gos.msgsBytes, int64(len(memTx.msg)))
	gos.metrics.TxSizeBytes.Observe(float64(len(memTx.msg)))
}

// Called from:
//  - Update (lock held) if msg was committed
// 	- resCbRecheck (lock not held) if msg was invalidated
func (gos *Gossip) removeTx(msg types.Tx, elem *clist.CElement, removeFromCache bool) {
	gos.msgs.Remove(elem)
	elem.DetachPrev()
	gos.msgsMap.Delete(msgKey(msg))
	atomic.AddInt64(&gos.msgsBytes, int64(-len(msg)))

	if removeFromCache {
		gos.cache.Remove(msg)
	}
}

// callback, which is called after the app checked the msg for the first time.
//
// The case where the app checks the msg for the second and subsequent times is
// handled by the resCbRecheck callback.
func (gos *Gossip) resCbFirstTime(msg []byte, peerID uint16, res *abci.Response) {
	switch r := res.Value.(type) {
	case *abci.Response_DeliverMsg:
		if (r.DeliverMsg.Code == abci.CodeTypeOK) {
			memTx := &gossipTx{
				height:    gos.height,
				msg:        msg,
			}
			memTx.senders.Store(peerID, true)
			gos.addTx(memTx)
			gos.logger.Info("Added good message",
				"msg", TxID(msg),
				"res", r,
				"height", memTx.height,
				"total", gos.Size(),
			)
			gos.notifyTxsAvailable()
		} else {
			// ignore bad transaction
			gos.logger.Info("Rejected bad message", "msg", TxID(msg), "res", r, "err")
			gos.metrics.FailedTxs.Add(1)
			// remove from cache (it might be good later)
			gos.cache.Remove(msg)
		}
	default:
		// ignore other messages
	}
}

// callback, which is called after the app rechecked the msg.
//
// The case where the app checks the msg for the first time is handled by the
// resCbFirstTime callback.
func (gos *Gossip) resCbRecheck(req *abci.Request, res *abci.Response) {
	switch r := res.Value.(type) {
	case *abci.Response_CheckTx:
		msg := req.GetCheckTx().Tx
		memTx := gos.recheckCursor.Value.(*gossipTx)
		if !bytes.Equal(msg, memTx.msg) {
			panic(fmt.Sprintf(
				"Unexpected msg response from proxy during recheck\nExpected %X, got %X",
				memTx.msg,
				msg))
		}
		if (r.CheckTx.Code == abci.CodeTypeOK) {
			// Good, nothing to do.
		} else {
			// Tx became invalidated due to newly committed block.
			gos.logger.Info("Tx is no longer valid", "msg", TxID(msg), "res", r, "err")
			// NOTE: we remove msg from the cache because it might be good later
			gos.removeTx(msg, gos.recheckCursor, true)
		}
		if gos.recheckCursor == gos.recheckEnd {
			gos.recheckCursor = nil
		} else {
			gos.recheckCursor = gos.recheckCursor.Next()
		}
		if gos.recheckCursor == nil {
			// Done!
			atomic.StoreInt32(&gos.rechecking, 0)
			gos.logger.Info("Done rechecking msgs")

			// incase the recheck removed all msgs
			if gos.Size() > 0 {
				gos.notifyTxsAvailable()
			}
		}
	default:
		// ignore other messages
	}
}

// TxsAvailable returns a channel which fires once for every height,
// and only when transactions are available in the mempool.
// NOTE: the returned channel may be nil if EnableTxsAvailable was not called.
func (gos *Gossip) TxsAvailable() <-chan struct{} {
	return gos.msgsAvailable
}

func (gos *Gossip) notifyTxsAvailable() {
	if gos.Size() == 0 {
		panic("notified msgs available but gossipis empty!")
	}
	if gos.msgsAvailable != nil && !gos.notifiedTxsAvailable {
		// channel cap is 1, so this will send once
		gos.notifiedTxsAvailable = true
		select {
		case gos.msgsAvailable <- struct{}{}:
		default:
		}
	}
}

func (gos *Gossip) removeTxs(msgs types.Txs) []types.Tx {
	// Build a map for faster lookups.
	msgsMap := make(map[string]struct{}, len(msgs))
	for _, msg := range msgs {
		msgsMap[string(msg)] = struct{}{}
	}

	msgsLeft := make([]types.Tx, 0, gos.msgs.Len())
	for e := gos.msgs.Front(); e != nil; e = e.Next() {
		memTx := e.Value.(*gossipTx)
		// Remove the msg if it's already in a block.
		if _, ok := msgsMap[string(memTx.msg)]; ok {
			// NOTE: we don't remove committed msgs from the cache.
			gos.removeTx(memTx.msg, e, false)

			continue
		}
		msgsLeft = append(msgsLeft, memTx.msg)
	}
	return msgsLeft
}

// NOTE: pass in msgs because gos.msgs can mutate concurrently.
func (gos *Gossip) recheckTxs(msgs []types.Tx) {
	if len(msgs) == 0 {
		return
	}
	atomic.StoreInt32(&gos.rechecking, 1)
	gos.recheckCursor = gos.msgs.Front()
	gos.recheckEnd = gos.msgs.Back()

	// Push msgs to proxyAppConn
	// NOTE: globalCb may be called concurrently.
	for _, msg := range msgs {
		gos.proxyAppConn.DeliverMsgAsync(msg)
	}
	gos.proxyAppConn.FlushAsync()
}

//--------------------------------------------------------------------------------

// gossipTx is a transaction that successfully ran
type gossipTx struct {
	height    int64    // height that this msg had been validated in
	msg        types.Tx //

	// ids of peers who've sent us this msg (as a map for quick lookups).
	// senders: PeerID -> bool
	senders sync.Map
}

// Height returns the height for this transaction
func (memTx *gossipTx) Height() int64 {
	return atomic.LoadInt64(&memTx.height)
}

//--------------------------------------------------------------------------------

type msgCache interface {
	Reset()
	Push(msg types.Tx) bool
	Remove(msg types.Tx)
}

// mapTxCache maintains a LRU cache of transactions. This only stores the hash
// of the msg, due to memory concerns.
type mapTxCache struct {
	mmsg  sync.Mutex
	size int
	map_ map[[sha256.Size]byte]*list.Element
	list *list.List
}

var _ msgCache = (*mapTxCache)(nil)

// newMapTxCache returns a new mapTxCache.
func newMapTxCache(cacheSize int) *mapTxCache {
	return &mapTxCache{
		size: cacheSize,
		map_: make(map[[sha256.Size]byte]*list.Element, cacheSize),
		list: list.New(),
	}
}

// Reset resets the cache to an empty state.
func (cache *mapTxCache) Reset() {
	cache.mmsg.Lock()
	cache.map_ = make(map[[sha256.Size]byte]*list.Element, cache.size)
	cache.list.Init()
	cache.mmsg.Unlock()
}

// Push adds the given msg to the cache and returns true. It returns
// false if msg is already in the cache.
func (cache *mapTxCache) Push(msg types.Tx) bool {
	cache.mmsg.Lock()
	defer cache.mmsg.Unlock()

	// Use the msg hash in the cache
	msgHash := msgKey(msg)
	if moved, exists := cache.map_[msgHash]; exists {
		cache.list.MoveToBack(moved)
		return false
	}

	if cache.list.Len() >= cache.size {
		popped := cache.list.Front()
		poppedTxHash := popped.Value.([sha256.Size]byte)
		delete(cache.map_, poppedTxHash)
		if popped != nil {
			cache.list.Remove(popped)
		}
	}
	e := cache.list.PushBack(msgHash)
	cache.map_[msgHash] = e
	return true
}

// Remove removes the given msg from the cache.
func (cache *mapTxCache) Remove(msg types.Tx) {
	cache.mmsg.Lock()
	msgHash := msgKey(msg)
	popped := cache.map_[msgHash]
	delete(cache.map_, msgHash)
	if popped != nil {
		cache.list.Remove(popped)
	}

	cache.mmsg.Unlock()
}

type nopTxCache struct{}

var _ msgCache = (*nopTxCache)(nil)

func (nopTxCache) Reset()             {}
func (nopTxCache) Push(types.Tx) bool { return true }
func (nopTxCache) Remove(types.Tx)    {}
