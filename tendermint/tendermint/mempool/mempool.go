package mempool

import (
	"bytes"
	"fmt"
	"sync"
	"time"
	"container/list"
	"sync/atomic"

	"github.com/pkg/errors"
	"github.com/tendermint/tmlibs/clist"
	"github.com/tendermint/tmlibs/log"
	"github.com/tendermint/tendermint/proxy"
	"github.com/tendermint/tendermint/types"
	abci "github.com/tendermint/abci/types"
	auto "github.com/tendermint/tmlibs/autofile"
	cmn "github.com/tendermint/tmlibs/common"
	cfg "github.com/tendermint/tendermint/config"

	"third_part/reporttrans"
)

/*

The mempool pushes new txs onto the proxyAppConn.
It gets a stream of (req, res) tuples from the proxy.
The mempool stores good txs in a concurrent linked-list.

Multiple concurrent go-routines can traverse this linked-list
safely by calling .NextWait() on each element.

So we have several go-routines:
1. Consensus calling Update() and Reap() synchronously
2. Many mempool reactor's peer routines calling CheckTx()
3. Many mempool reactor's peer routines traversing the txs linked list
4. Another goroutine calling GarbageCollectTxs() periodically

To manage these goroutines, there are three methods of locking.
1. Mutations to the linked-list is protected by an internal mtx (CList is goroutine-safe)
2. Mutations to the linked-list elements are atomic
3. CheckTx() calls can be paused upon Update() and Reap(), protected by .proxyMtx

Garbage collection of old elements from mempool.txs is handlde via
the DetachPrev() call, which makes old elements not reachable by
peer broadcastTxRoutine() automatically garbage collected.

TODO: Better handle abci client errors. (make it automatically handle connection errors)

*/

const (
	cacheSize = 100000
	futurenum = 9999
)

var (
	statsReportInterval = 8 * time.Second // Time interval to report mempool transaction stats
)

// Mempool is an ordered in-memory pool for transactions before they are proposed in a consensus
// round. Transaction validity is checked using the CheckTx abci message before the transaction is
// added to the pool. The Mempool uses a concurrent list structure for storing transactions that
// can be efficiently accessed by multiple concurrent readers.
type Mempool struct {
	config *cfg.MempoolConfig

	proxyMtx             sync.Mutex
	proxyAppConn         proxy.AppConnMempool
	txs                  *clist.CList    // concurrent linked-list of good txs
	futuretxs            *clist.CList    // concurrent linked-list of future txs
	counter              int64           // simple incrementing counter
	height               int64           // the last block Update()'d to
	rechecking           int32           // for re-checking filtered txs on Update()
	recheckCursor        *clist.CElement // next expected response
	recheckEnd           *clist.CElement // re-checking stops here
	notifiedTxsAvailable bool            // true if fired on txsAvailable for this height
	txsAvailable         chan int64      // fires the next height once for each height, when the mempool is not empty

	// Keep a cache of already-seen txs.
	// This reduces the pressure on the proxyApp.
	cache *txCache

	// A log of mempool txs
	wal *auto.AutoFile

	logger log.Logger

	// notify consensus new tx received if blockchain work over
	notifyTx  bool
}

// NewMempool returns a new Mempool with the given configuration and connection to an application.
// TODO: Extract logger into arguments.
func NewMempool(config *cfg.MempoolConfig, proxyAppConn proxy.AppConnMempool, height int64) *Mempool {
	mempool := &Mempool{
		config:        config,
		proxyAppConn:  proxyAppConn,
		txs:           clist.New(),
		futuretxs:     clist.New(),
		counter:       0,
		height:        height,
		rechecking:    0,
		recheckCursor: nil,
		recheckEnd:    nil,
		logger:        log.NewNopLogger(),
		cache:         newTxCache(cacheSize),
	}
	mempool.initWAL()
	proxyAppConn.SetResponseCallback(mempool.resCb)
	reporttrans.SetTxsWarnRate(config.RateTooManyTxs)
	mempool.logger.Info("[mempool] SetTxsWarnRate: ", "rate", config.RateTooManyTxs)
	go mempool.StatsReport()
	return mempool
}

func (mem *Mempool) StatsReport() {
	mem.logger.Info("[mempool] StatsReport: starting", "interval", statsReportInterval, "rate", mem.config.RateTooManyTxs)
	defer mem.logger.Info("[mempool] StatsReport: terminated")
	report := time.NewTicker(statsReportInterval)
	defer report.Stop()
	for {
		select {
		case <-report.C:
			goodTxs := mem.txs.Len()
			futureTxs := mem.futuretxs.Len()
			var defaultRate float64 = reporttrans.RateNodeUnconfirmedTxTooMany
			goodTxsTooMany := float64(goodTxs) >= float64(cacheSize) * defaultRate
			futureTxsTooMany := float64(futureTxs) >= float64(cacheSize) * defaultRate
			if goodTxsTooMany || futureTxsTooMany {
				mem.logger.Info("[lktx] Mempool transaction status: too many good or future txs", "goodTxs", goodTxs, "futureTxs", futureTxs, "rate", defaultRate, "cacheSize", cacheSize)
				var msg = struct {
					Module string `json:"module"`
					Executable int `json:"goodTxs"`
					Queued int `json:"futureTxs"`
					CacheSize int `json:"cacheSize"`
					Rate float64 `json:"rate"`
					Reason string `json:"reason"`

				}{
					Module: "mempool",
					Executable: goodTxs,
					Queued: futureTxs,
					CacheSize: cacheSize,
					Rate: defaultRate,
					Reason: "Transaction mempool: too many good or future txs, exceed default rate",

				}
				reporttrans.WarnJson(reporttrans.IdNodeUnconfirmedTxTooMany, msg)
			}

		}
	}
}

// EnableTxsAvailable initializes the TxsAvailable channel,
// ensuring it will trigger once every height when transactions are available.
// NOTE: not thread safe - should only be called once, on startup
func (mem *Mempool) EnableTxsAvailable() {
	mem.txsAvailable = make(chan int64, 2)
}

// SetLogger sets the Logger.
func (mem *Mempool) SetLogger(l log.Logger) {
	mem.logger = l
}

// CloseWAL closes and discards the underlying WAL file.
// Any further writes will not be relayed to disk.
func (mem *Mempool) CloseWAL() bool {
	if mem == nil {
		return false
	}

	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()

	if mem.wal == nil {
		return false
	}
	if err := mem.wal.Close(); err != nil && mem.logger != nil {
		mem.logger.Error("Mempool.CloseWAL", "err", err)
	}
	mem.wal = nil
	return true
}

func (mem *Mempool) initWAL() {
	walDir := mem.config.WalDir()
	if walDir != "" {
		err := cmn.EnsureDir(walDir, 0700)
		if err != nil {
			cmn.PanicSanity(errors.Wrap(err, "Error ensuring Mempool wal dir"))
		}
		af, err := auto.OpenAutoFile(walDir + "/wal")
		if err != nil {
			cmn.PanicSanity(errors.Wrap(err, "Error opening Mempool wal file"))
		}
		mem.wal = af
	}
}

// Lock locks the mempool. The consensus must be able to hold lock to safely update.
func (mem *Mempool) Lock() {
	mem.proxyMtx.Lock()
}

// Unlock unlocks the mempool.
func (mem *Mempool) Unlock() {
	mem.proxyMtx.Unlock()
}

// Size returns the number of transactions in the mempool.
func (mem *Mempool) Size() int {
	return mem.txs.Len()
}

// Flush removes all transactions from the mempool and cache
func (mem *Mempool) Flush() {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()

	mem.cache.Reset()

	for e := mem.txs.Front(); e != nil; e = e.Next() {
		mem.txs.Remove(e)
		e.DetachPrev()
	}

	for e := mem.futuretxs.Front(); e != nil; e = e.Next() {
		mem.futuretxs.Remove(e)
		e.DetachPrev()
	}
}

// TxsFront returns the first transaction in the ordered list for peer
// goroutines to call .NextWait() on.
func (mem *Mempool) TxsFront() *clist.CElement {
	return mem.txs.Front()
}

// TxsWaitChan returns a channel to wait on transactions. It will be closed
// once the mempool is not empty (ie. the internal `mem.txs` has at least one
// element)
func (mem *Mempool) TxsWaitChan() <-chan struct{} {
	return mem.txs.WaitChan()
}

// CheckTx executes a new transaction against the application to determine its validity
// and whether it should be added to the mempool.
// It blocks if we're waiting on Update() or Reap().
// cb: A callback from the CheckTx command.
//     It gets called from another goroutine.
// CONTRACT: Either cb will get called, or err returned.
func (mem *Mempool) CheckTx(tx types.Tx, cb func(*abci.Response)) (err error) {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()

	// CACHE
	if mem.cache.Exists(tx) {
		return fmt.Errorf("Tx already exists in cache")
	}
	mem.cache.Push(tx)
	// END CACHE

	// NOTE: proxyAppConn may error if tx buffer is full
	if err = mem.proxyAppConn.Error(); err != nil {
		return err
	}
	reqRes := mem.proxyAppConn.CheckTxAsync(tx)
	if cb != nil {
		reqRes.SetCallback(cb)
	}

	return nil
}

// ABCI callback function
func (mem *Mempool) resCb(req *abci.Request, res *abci.Response) {
	if mem.recheckCursor == nil {
		mem.resCbNormal(req, res)
	} else {
		mem.resCbRecheck(req, res)
	}
}

// ABCI callback function
func (mem *Mempool) repeatCb(res *abci.Response) {
	switch r := res.Value.(type) {
	case *abci.Response_CheckTx:
		if r.CheckTx.Code != abci.CodeTypeOK {
			mem.logger.Error("good transaction has serious error in rechecking", "res", r)
		}
	}
}

func (mem *Mempool) resCbNormal(req *abci.Request, res *abci.Response) {
	switch r := res.Value.(type) {
	case *abci.Response_CheckTx:
		tx := req.GetCheckTx().Tx
		if r.CheckTx.Code == abci.CodeTypeOK {
			mem.counter++
			memTx := &mempoolTx{
				counter: mem.counter,
				height:  mem.height,
				tx:      tx,
			}
			mem.txs.PushBack(memTx)
			mem.logger.Info("Added good transaction", "tx", tx, "res", r)
			mem.notifyTxsAvailable()
		} else if r.CheckTx.Code == futurenum {
			memTx := &mempoolTx{
				counter: 0,
				height:  0,
				tx:      tx,
			}
			mem.futuretxs.PushBack(memTx)
			mem.logger.Info("Added future transaction", "tx", tx, "res", r)
		} else {
			// ignore bad transaction
			mem.logger.Info("Rejected bad transaction", "tx", tx, "res", r)

			// remove from cache (it might be good later)
			mem.cache.Remove(tx)

			// TODO: handle other retcodes
		}
	default:
		// ignore other messages
	}
}

func (mem *Mempool) resCbRecheck(req *abci.Request, res *abci.Response) {
	switch r := res.Value.(type) {
	case *abci.Response_CheckTx:
		memTx := mem.recheckCursor.Value.(*mempoolTx)
		if !bytes.Equal(req.GetCheckTx().Tx, memTx.tx) {
			cmn.PanicSanity(cmn.Fmt("Unexpected tx response from proxy during recheck\n"+
				"Expected %X, got %X", req.GetCheckTx().Tx, memTx.tx))
		}

		if r.CheckTx.Code == abci.CodeTypeOK {
			mem.counter++
			newMemTx := &mempoolTx{
				counter: mem.counter,
				height:  mem.height,
				tx:      memTx.tx,
			}
			mem.txs.PushBack(newMemTx)
			mem.futuretxs.Remove(mem.recheckCursor)
			mem.recheckCursor.DetachPrev()
			mem.logger.Info("recheck Added good transaction", "tx", newMemTx.tx, "res", r)
		} else if r.CheckTx.Code == futurenum {
			// nothing to do
		} else {
			// Tx became invalidated due to newly committed block.
			mem.futuretxs.Remove(mem.recheckCursor)
			mem.recheckCursor.DetachPrev()

			// remove from cache (it might be good later)
			mem.cache.Remove(req.GetCheckTx().Tx)
			mem.logger.Info("resCbRecheck", "res", r)
		}

		if mem.recheckCursor == mem.recheckEnd {
			mem.recheckCursor = nil
		} else {
			mem.recheckCursor = mem.recheckCursor.Next()
		}

		if mem.recheckCursor == nil {
			// Done!
			atomic.StoreInt32(&mem.rechecking, 0)
			mem.logger.Info("Done rechecking txs")

			// incase the recheck removed all txs
			if mem.Size() > 0 {
				mem.forceNotifyTxsAvailable()
			}
		}
	default:
		// ignore other messages
	}
}

// TxsAvailable returns a channel which fires once for every height,
// and only when transactions are available in the mempool.
// NOTE: the returned channel may be nil if EnableTxsAvailable was not called.
func (mem *Mempool) TxsAvailable() <-chan int64 {
	return mem.txsAvailable
}

func (mem *Mempool) NotifyTx() {
	mem.notifyTx = true
}

func (mem *Mempool) notifyTxsAvailable() {
	if !mem.notifyTx {
		return
	}
	if mem.Size() == 0 {
		panic("notified txs available but mempool is empty!")
	}
	if mem.txsAvailable != nil && !mem.notifiedTxsAvailable {
		mem.notifiedTxsAvailable = true
		mem.txsAvailable <- mem.height + 1
	}
}

func (mem *Mempool) forceNotifyTxsAvailable() {
	if !mem.notifyTx {
		return
	}
	if mem.Size() == 0 {
		panic("notified txs available but mempool is empty!")
	}
	if mem.txsAvailable != nil {
		mem.notifiedTxsAvailable = true
		mem.txsAvailable <- mem.height + 1
	}
}

// Reap returns a list of transactions currently in the mempool.
// If maxTxs is -1, there is no cap on the number of returned transactions.
func (mem *Mempool) Reap(maxTxs int) types.Txs {
	mem.proxyMtx.Lock()
	defer mem.proxyMtx.Unlock()

	for atomic.LoadInt32(&mem.rechecking) > 0 {
		// TODO: Something better?
		time.Sleep(time.Millisecond * 10)
	}

	txs := mem.collectTxs(maxTxs)
	return txs
}

// maxTxs: -1 means uncapped, 0 means none
func (mem *Mempool) collectTxs(maxTxs int) types.Txs {
	if maxTxs == 0 {
		return []types.Tx{}
	} else if maxTxs < 0 {
		maxTxs = mem.txs.Len()
	}
	txs := make([]types.Tx, 0, cmn.MinInt(mem.txs.Len(), maxTxs))
	for e := mem.txs.Front(); e != nil && len(txs) < maxTxs; e = e.Next() {
		memTx := e.Value.(*mempoolTx)
		txs = append(txs, memTx.tx)
	}
	return txs
}

// Update informs the mempool that the given txs were committed and can be discarded.
// NOTE: this should be called *after* block is committed by consensus.
// NOTE: unsafe; Lock/Unlock must be managed by caller
func (mem *Mempool) Update(height int64, txs types.Txs) error {
	if err := mem.proxyAppConn.FlushSync(); err != nil { // To flush async resCb calls e.g. from CheckTx
		return err
	}
	// First, create a lookup map of txns in new txs.
	txsMap := make(map[string]struct{})
	for _, tx := range txs {
		txsMap[string(tx)] = struct{}{}
	}

	// Set height
	mem.height = height
	mem.notifiedTxsAvailable = false

	/*
	// Remove transactions that are already in txs.
	goodTxs := mem.filterTxs(txsMap)
	// Recheck mempool txs if any txs were committed in the block
	// NOTE/XXX: in some apps a tx could be invalidated due to EndBlock,
	//	so we really still do need to recheck, but this is for debugging
	if mem.config.Recheck && (mem.config.RecheckEmpty || len(txs) > 0) {
		mem.logger.Info("Recheck txs", "numtxs", len(goodTxs), "height", height)
		mem.recheckTxs(goodTxs)
		// At this point, mem.txs are being rechecked.
		// mem.recheckCursor re-scans mem.txs and possibly removes some txs.
		// Before mem.Reap(), we should wait for mem.recheckCursor to be nil.
	}
	*/
	mem.filterTxsNoResult(txsMap)
	//mem.recheckTx()
	if mem.notifyTx {
		mem.logger.Info("recheckFutureTxs begin")
		mem.recheckFutureTxs()
		mem.logger.Info("recheckFutureTxs end")
	}
	return nil
}

func (mem *Mempool) filterTxs(blockTxsMap map[string]struct{}) []types.Tx {
	goodTxs := make([]types.Tx, 0, mem.txs.Len())
	for e := mem.txs.Front(); e != nil; e = e.Next() {
		memTx := e.Value.(*mempoolTx)
		// Remove the tx if it's alredy in a block.
		if _, ok := blockTxsMap[string(memTx.tx)]; ok {
			// remove from clist
			mem.txs.Remove(e)
			e.DetachPrev()

			// NOTE: we don't remove committed txs from the cache.
			continue
		}
		// Good tx!
		goodTxs = append(goodTxs, memTx.tx)
	}
	return goodTxs
}

// NOTE: pass in goodTxs because mem.txs can mutate concurrently.
func (mem *Mempool) recheckTxs(goodTxs []types.Tx) {
	if len(goodTxs) == 0 {
		return
	}
	atomic.StoreInt32(&mem.rechecking, 1)
	mem.recheckCursor = mem.txs.Front()
	mem.recheckEnd = mem.txs.Back()

	// Push txs to proxyAppConn
	// NOTE: resCb() may be called concurrently.
	for _, tx := range goodTxs {
		mem.proxyAppConn.CheckTxAsync(tx)
	}
	mem.proxyAppConn.FlushAsync()
}

func (mem *Mempool) filterTxsNoResult(blockTxsMap map[string]struct{}) {
	for e := mem.txs.Front(); e != nil; e = e.Next() {
		memTx := e.Value.(*mempoolTx)
		// Remove the tx if it's alredy in a block.
		if _, ok := blockTxsMap[string(memTx.tx)]; ok {
			// remove from clist
			mem.txs.Remove(e)
			mem.cache.Remove(memTx.tx)
			e.DetachPrev()
		}
	}
}

func (mem *Mempool) recheckTx() {
	if mem.txs.Len() == 0 {
		return
	}

	for e := mem.txs.Front(); e != nil; e = e.Next() {
		memTx := e.Value.(*mempoolTx)
		reqRes := mem.proxyAppConn.CheckTxAsync(memTx.tx)
		reqRes.SetCallback(mem.repeatCb)
	}

	mem.proxyAppConn.FlushAsync()
}

func (mem *Mempool) recheckFutureTxs() {
	if mem.futuretxs.Len() == 0 {
		if mem.Size() > 0 {
			mem.logger.Info("mem.forceNotifyTxsAvailable start")
			mem.forceNotifyTxsAvailable()
			mem.logger.Info("mem.forceNotifyTxsAvailable end")
		}
		return
	}

	atomic.StoreInt32(&mem.rechecking, 1)
	mem.recheckCursor = mem.futuretxs.Front()
	mem.recheckEnd = mem.futuretxs.Back()

	for e := mem.futuretxs.Front(); e != nil; e = e.Next() {
		memTx := e.Value.(*mempoolTx)
		mem.proxyAppConn.CheckTxAsync(memTx.tx)
	}

	mem.proxyAppConn.FlushAsync()
}

//--------------------------------------------------------------------------------

// mempoolTx is a transaction that successfully ran
type mempoolTx struct {
	counter int64    // a simple incrementing counter
	height  int64    // height that this tx had been validated in
	tx      types.Tx //
}

// Height returns the height for this transaction
func (memTx *mempoolTx) Height() int64 {
	return atomic.LoadInt64(&memTx.height)
}

//--------------------------------------------------------------------------------

// txCache maintains a cache of transactions.
type txCache struct {
	mtx  sync.Mutex
	size int
	map_ map[string]struct{}
	list *list.List // to remove oldest tx when cache gets too big
}

// newTxCache returns a new txCache.
func newTxCache(cacheSize int) *txCache {
	return &txCache{
		size: cacheSize,
		map_: make(map[string]struct{}, cacheSize),
		list: list.New(),
	}
}

// Reset resets the txCache to empty.
func (cache *txCache) Reset() {
	cache.mtx.Lock()
	cache.map_ = make(map[string]struct{}, cacheSize)
	cache.list.Init()
	cache.mtx.Unlock()
}

// Exists returns true if the given tx is cached.
func (cache *txCache) Exists(tx types.Tx) bool {
	cache.mtx.Lock()
	_, exists := cache.map_[string(tx)]
	cache.mtx.Unlock()
	return exists
}

// Push adds the given tx to the txCache. It returns false if tx is already in the cache.
func (cache *txCache) Push(tx types.Tx) bool {
	cache.mtx.Lock()
	defer cache.mtx.Unlock()

	if _, exists := cache.map_[string(tx)]; exists {
		return false
	}

	if cache.list.Len() >= cache.size {
		popped := cache.list.Front()
		poppedTx := popped.Value.(types.Tx)
		// NOTE: the tx may have already been removed from the map
		// but deleting a non-existent element is fine
		delete(cache.map_, string(poppedTx))
		cache.list.Remove(popped)
	}
	cache.map_[string(tx)] = struct{}{}
	cache.list.PushBack(tx)
	return true
}

// Remove removes the given tx from the cache.
func (cache *txCache) Remove(tx types.Tx) {
	cache.mtx.Lock()
	delete(cache.map_, string(tx))
	cache.mtx.Unlock()
}
