package core

import (
	"bytes"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	mrand "math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/spruce-solutions/go-quai/common"
	"github.com/spruce-solutions/go-quai/consensus"
	"github.com/spruce-solutions/go-quai/consensus/misc"
	"github.com/spruce-solutions/go-quai/core/rawdb"
	"github.com/spruce-solutions/go-quai/core/state"
	"github.com/spruce-solutions/go-quai/core/types"
	"github.com/spruce-solutions/go-quai/core/vm"
	"github.com/spruce-solutions/go-quai/ethdb"
	"github.com/spruce-solutions/go-quai/event"
	"github.com/spruce-solutions/go-quai/log"
	"github.com/spruce-solutions/go-quai/metrics"
	"github.com/spruce-solutions/go-quai/params"
	"github.com/spruce-solutions/go-quai/rlp"
)

var (
	headBlockGauge     = metrics.NewRegisteredGauge("chain/head/block", nil)
	headHeaderGauge    = metrics.NewRegisteredGauge("chain/head/header", nil)
	headFastBlockGauge = metrics.NewRegisteredGauge("chain/head/receipt", nil)

	blockReorgMeter         = metrics.NewRegisteredMeter("chain/reorg/executes", nil)
	blockReorgAddMeter      = metrics.NewRegisteredMeter("chain/reorg/add", nil)
	blockReorgDropMeter     = metrics.NewRegisteredMeter("chain/reorg/drop", nil)
	blockReorgInvalidatedTx = metrics.NewRegisteredMeter("chain/reorg/invalidTx", nil)
)

const (
	headerCacheLimit = 512
	tdCacheLimit     = 1024
	numberCacheLimit = 2048
)

// WriteStatus status of write
type WriteStatus byte

const (
	NonStatTy WriteStatus = iota
	CanonStatTy
	SideStatTy
	UnknownStatTy
)

// HeaderChain is responsible for maintaining the header chain including the
// header query and updating.
//
// The components maintained by headerchain includes: (1) total difficult
// (2) header (3) block hash -> number mapping (4) canonical number -> hash mapping
// and (5) head header flag.

type HeaderChain struct {
	config *params.ChainConfig

	bc     *BlockChain
	engine consensus.Engine

	chainHeadFeed event.Feed
	scope         event.SubscriptionScope

	headerDb      ethdb.Database
	genesisHeader *types.Header

	currentHeader     atomic.Value // Current head of the header chain (may be above the block chain!)
	currentHeaderHash common.Hash  // Hash of the current head of the header chain (prevent recomputing all the time)

	headerCache *lru.Cache // Cache for the most recent block headers
	tdCache     *lru.Cache // Cache for the most recent block total difficulties
	numberCache *lru.Cache // Cache for the most recent block numbers

	quit          chan struct{}  // headerchain quit channel
	wg            sync.WaitGroup // chain processing wait group for shutting down
	running       int32          // 0 if chain is running, 1 when stopped
	procInterrupt int32          // interrupt signaler for block processing

	rand     *mrand.Rand
	headermu sync.RWMutex
	heads    []*types.Header
}

// NewHeaderChain creates a new HeaderChain structure. ProcInterrupt points
// to the parent's interrupt semaphore.
func NewHeaderChain(db ethdb.Database, engine consensus.Engine, chainConfig *params.ChainConfig, cacheConfig *CacheConfig, vmConfig vm.Config) (*HeaderChain, error) {
	headerCache, _ := lru.New(headerCacheLimit)
	tdCache, _ := lru.New(tdCacheLimit)
	numberCache, _ := lru.New(numberCacheLimit)

	// Seed a fast but crypto originating random generator
	seed, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		return nil, err
	}

	hc := &HeaderChain{
		config:      chainConfig,
		headerDb:    db,
		headerCache: headerCache,
		tdCache:     tdCache,
		numberCache: numberCache,
		rand:        mrand.New(mrand.NewSource(seed.Int64())),
		engine:      engine,
		quit:        make(chan struct{}),
	}

	hc.bc, err = NewBlockChain(db, engine, hc, chainConfig, cacheConfig, vmConfig)
	if err != nil {
		return nil, err
	}

	hc.genesisHeader = hc.GetHeaderByNumber(0)
	fmt.Println(hc.genesisHeader.Hash())
	if hc.genesisHeader == nil {
		return nil, ErrNoGenesis
	}

	// Initialize the heads slice
	heads := make([]*types.Header, 0)
	hc.heads = heads

	//Load any state that is in our db
	if err := hc.loadLastState(); err != nil {
		return nil, err
	}

	return hc, nil
}

// Append
func (hc *HeaderChain) Append(block *types.Block) error {
	hc.headermu.Lock()
	defer hc.headermu.Unlock()

	fmt.Println("Block information: Hash:", block.Hash(), "Number:", block.NumberU64(), "Location:", block.Header().Location, "Parent:", block.ParentHash())
	err := hc.Appendable(block)
	if err != nil {
		fmt.Println("Error on appendable, err:", err)
		return err
	}

	// Append header to the headerchain
	batch := hc.headerDb.NewBatch()
	rawdb.WriteHeader(batch, block.Header())
	if err := batch.Write(); err != nil {
		return err
	}

	// Append block else revert header append
	logs, err := hc.bc.Append(block)
	if err != nil {
		fmt.Println("Error on Append, err:", err)
		rawdb.DeleteHeader(hc.headerDb, block.Header().Hash(), block.Header().Number64())
		return err
	}

	hc.bc.chainFeed.Send(ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
	if len(logs) > 0 {
		hc.bc.logsFeed.Send(logs)
	}

	/////////////////////////
	// Garbage Collection //
	///////////////////////
	var nilHeader *types.Header
	// check if the size of the queue is at the maxHeadsQueueLimit
	if len(hc.heads) == maxHeadsQueueLimit {
		// Trim the branch before dequeueing
		commonHeader := hc.findCommonHeader(hc.heads[0])
		if commonHeader == nil {
			return errors.New("nil head in hc.heads")
		}
		err = hc.trim(commonHeader, hc.heads[0])
		if err != nil {
			return err
		}

		// dequeue
		hc.heads[0] = nilHeader
		hc.heads = hc.heads[1:]
	}
	// Add to the heads queue
	hc.heads = append(hc.heads, block.Header())

	// Sort the heads by number
	sort.Slice(hc.heads, func(i, j int) bool {
		return hc.heads[i].Number[types.QuaiNetworkContext].Uint64() < hc.heads[j].Number[types.QuaiNetworkContext].Uint64()
	})

	return nil
}

func (hc *HeaderChain) Appendable(block *types.Block) error {
	err := hc.engine.VerifyHeader(hc, block.Header(), true)
	if err != nil {
		return err
	}
	err = hc.bc.Appendable(block)
	return err
}

// SetCurrentHeader sets the in-memory head header marker of the canonical chan
// as the given header.
func (hc *HeaderChain) SetCurrentHeader(head *types.Header) ([]*types.Header, error) {
	fmt.Println("Setting Current Header", head.Hash())
	prevHeader := hc.CurrentHeader()

	sliceHeaders := make([]*types.Header, 3)

	//Update canonical state db
	hc.currentHeader.Store(head)
	hc.currentHeaderHash = head.Hash()
	headHeaderGauge.Update(head.Number[types.QuaiNetworkContext].Int64())

	// write the head block hash to the db
	rawdb.WriteHeadBlockHash(hc.headerDb, head.Hash())

	// If head is the normal extension of canonical head, we can return by just wiring the canonical hash.
	if prevHeader.Hash() == head.Parent() {
		rawdb.WriteCanonicalHash(hc.headerDb, head.Hash(), head.Number64())
		if types.QuaiNetworkContext != params.ZONE {
			sliceHeaders[head.Location[types.QuaiNetworkContext]-1] = head
		}
		return sliceHeaders, nil
	}

	//Find a common header
	commonHeader := hc.findCommonHeader(head)
	newHeader := head

	// Delete each header and rollback state processor until common header
	// Accumulate the hash slice stack
	var hashStack []*types.Header
	for {
		if prevHeader.Hash() == commonHeader.Hash() {
			fmt.Println("appending on prevHeader == commonHeader")
			for {
				if newHeader.Hash() == commonHeader.Hash() {
					break
				}
				newHeader = hc.GetHeader(newHeader.Parent(), newHeader.Number64()-1)
				hashStack = append(hashStack, newHeader)

				// genesis check to not delete the genesis block
				if newHeader.Hash() == hc.config.GenesisHashes[0] {
					break
				}

				if newHeader == nil {
					break
				}
			}
			break
		}

		// Delete the header and the block
		fmt.Println("delete prev", prevHeader.Hash())
		rawdb.DeleteCanonicalHash(hc.headerDb, prevHeader.Number64())
		prevHeader = hc.GetHeader(prevHeader.Parent(), prevHeader.Number64()-1)

		if newHeader.Hash() == commonHeader.Hash() {
			fmt.Println("appending on newHeader == commonHeader")
			for {
				if prevHeader.Hash() == commonHeader.Hash() {
					break
				}
				fmt.Println("delete prev", prevHeader.Hash())
				rawdb.DeleteCanonicalHash(hc.headerDb, prevHeader.Number64())
				prevHeader = hc.GetHeader(prevHeader.Parent(), prevHeader.Number64()-1)

				// genesis check to not delete the genesis block
				if prevHeader.Hash() == hc.config.GenesisHashes[0] {
					break
				}

				if prevHeader == nil {
					break
				}
			}
			break
		}

		// Add to the stack
		hashStack = append(hashStack, newHeader)
		newHeader = hc.GetHeader(newHeader.Parent(), newHeader.Number64()-1)

		// genesis check to not delete the genesis block
		if prevHeader.Hash() == hc.config.GenesisHashes[0] {
			break
		}

		if prevHeader == nil {
			break
		}

		// Setting the appropriate sliceHeader to rollback point
		if types.QuaiNetworkContext != params.ZONE {
			sliceHeaders[prevHeader.Location[types.QuaiNetworkContext]-1] = prevHeader
		}

		fmt.Println("prevheader: ", prevHeader.Hash())
	}

	fmt.Println("Attempting to write canonical hash")
	fmt.Println("hashStack", hashStack)

	// Run through the hash stack to update canonicalHash and forward state processor
	for i := len(hashStack) - 1; i >= 0; i-- {
		fmt.Println("WriteCanonicalHash", hashStack[i].Hash())
		rawdb.WriteCanonicalHash(hc.headerDb, hashStack[i].Hash(), hashStack[i].Number64())

		// Setting the appropriate sliceHeader to rollforward point
		if types.QuaiNetworkContext != params.ZONE {
			if len(hashStack[i].Location) != 0 {
				sliceHeaders[hashStack[i].Location[types.QuaiNetworkContext]-1] = hashStack[i]
			}
		}
	}

	return sliceHeaders, nil
}

// Reset purges the entire blockchain, restoring it to its genesis state.
func (hc *HeaderChain) Reset() error {
	return hc.ResetWithGenesisBlock(hc.genesisHeader)
}

// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
func (hc *HeaderChain) ResetWithGenesisBlock(genesis *types.Header) error {

	hc.headermu.Lock()
	defer hc.headermu.Unlock()

	//Iterate through my heads and trim each back to genesis
	for _, head := range hc.heads {
		hc.trim(hc.genesisHeader, head)
	}

	return nil
}

// Trim
func (hc *HeaderChain) trim(commonHeader *types.Header, startHeader *types.Header) error {
	parent := startHeader
	// Delete each header until common is found
	for {
		if parent.Hash() == commonHeader.Hash() {
			break
		}

		// Delete the header and the block
		rawdb.DeleteHeader(hc.headerDb, parent.Hash(), parent.Number64())
		hc.bc.Trim(parent)

		parent = hc.GetHeader(parent.Parent(), parent.Number64()-1)

		if parent == nil {
			log.Warn("unable to trim blockchain state, one of trimmed blocks not found")
			return nil
		}
	}
	return nil
}

// findCommonHeader
func (hc *HeaderChain) findCommonHeader(header *types.Header) *types.Header {
	for {
		if header == nil {
			return nil
		}
		canonicalHash := rawdb.ReadCanonicalHash(hc.headerDb, header.Number64())
		if canonicalHash == header.Hash() || canonicalHash == hc.config.GenesisHashes[types.QuaiNetworkContext] {
			return hc.GetHeaderByHash(canonicalHash)
		}
		header = hc.GetHeader(header.ParentHash[types.QuaiNetworkContext], header.Number64()-1)
	}

}

// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.
func (hc *HeaderChain) loadLastState() error {
	// TODO: create function to find highest block number and fill Head FIFO
	headsHashes := rawdb.ReadHeadsHashes(hc.headerDb)
	fmt.Println("heads hashes: ", headsHashes)

	if head := rawdb.ReadHeadBlockHash(hc.headerDb); head != (common.Hash{}) {
		fmt.Println("head hash: ", head)
		if chead := hc.GetHeaderByHash(head); chead != nil {
			hc.currentHeader.Store(chead)
			hc.currentHeaderHash = chead.Hash()
		}
	}
	hc.currentHeaderHash = hc.CurrentHeader().Hash()
	headHeaderGauge.Update(hc.CurrentHeader().Number[types.QuaiNetworkContext].Int64())

	heads := make([]*types.Header, 0)
	for _, hash := range headsHashes {
		heads = append(heads, hc.GetHeaderByHash(hash))
	}
	hc.heads = heads

	return nil
}

// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
func (hc *HeaderChain) Stop() {
	if !atomic.CompareAndSwapInt32(&hc.running, 0, 1) {
		return
	}

	hashes := make([]common.Hash, 0)
	for i := 0; i < len(hc.heads); i++ {
		hashes = append(hashes, hc.heads[i].Hash())
	}
	// Save the heads
	rawdb.WriteHeadsHashes(hc.headerDb, hashes)
	rawdb.WriteHeadBlockHash(hc.headerDb, hc.CurrentHeader().Hash())

	// Unsubscribe all subscriptions registered from blockchain
	hc.bc.scope.Close()
	close(hc.quit)
	hc.StopInsert()
	hc.wg.Wait()

	log.Info("headerchain stopped")
}

// empty returns an indicator whether the blockchain is empty.
func (hc *HeaderChain) empty() bool {
	genesis := hc.genesisHeader.Hash()
	if rawdb.ReadHeadBlockHash(hc.headerDb) == genesis {
		return true
	} else {
		return false
	}
}

// StopInsert interrupts all insertion methods, causing them to return
// errInsertionInterrupted as soon as possible. Insertion is permanently disabled after
// calling this method.
func (hc *HeaderChain) StopInsert() {
	atomic.StoreInt32(&hc.procInterrupt, 1)
}

// insertStopped returns true after StopInsert has been called.
func (hc *HeaderChain) insertStopped() bool {
	return atomic.LoadInt32(&hc.procInterrupt) == 1
}

// Blockchain retrieves the blockchain from the headerchain.
func (hc *HeaderChain) BlockChain() *BlockChain {
	return hc.bc
}

// NOTES: Headerchain needs to have head
// Singleton Tds need to get calculated by slice after successful append and then written into headerchain
// Slice uses HLCR to query Headerchains for Tds
// Slice is a collection of references headerchains

// GetBlockNumber retrieves the block number belonging to the given hash
// from the cache or database
func (hc *HeaderChain) GetBlockNumber(hash common.Hash) *uint64 {
	if cached, ok := hc.numberCache.Get(hash); ok {
		number := cached.(uint64)
		return &number
	}
	number := rawdb.ReadHeaderNumber(hc.headerDb, hash)
	if number != nil {
		hc.numberCache.Add(hash, *number)
	}
	return number
}

// GetBlockHashesFromHash retrieves a number of block hashes starting at a given
// hash, fetching towards the genesis block.
func (hc *HeaderChain) GetBlockHashesFromHash(hash common.Hash, max uint64) []common.Hash {
	// Get the origin header from which to fetch
	header := hc.GetHeaderByHash(hash)
	if header == nil {
		return nil
	}
	// Iterate the headers until enough is collected or the genesis reached
	chain := make([]common.Hash, 0, max)
	for i := uint64(0); i < max; i++ {
		next := header.ParentHash[types.QuaiNetworkContext]
		if header = hc.GetHeader(next, header.Number[types.QuaiNetworkContext].Uint64()-1); header == nil {
			break
		}
		chain = append(chain, next)
		if header.Number[types.QuaiNetworkContext].Sign() == 0 {
			break
		}
	}
	return chain
}

// GetAncestor retrieves the Nth ancestor of a given block. It assumes that either the given block or
// a close ancestor of it is canonical. maxNonCanonical points to a downwards counter limiting the
// number of blocks to be individually checked before we reach the canonical chain.
//
// Note: ancestor == 0 returns the same block, 1 returns its parent and so on.
func (hc *HeaderChain) GetAncestor(hash common.Hash, number, ancestor uint64, maxNonCanonical *uint64) (common.Hash, uint64) {
	if ancestor > number {
		return common.Hash{}, 0
	}
	if ancestor == 1 {
		// in this case it is cheaper to just read the header
		if header := hc.GetHeader(hash, number); header != nil {
			return header.ParentHash[types.QuaiNetworkContext], number - 1
		}
		return common.Hash{}, 0
	}
	for ancestor != 0 {
		if rawdb.ReadCanonicalHash(hc.headerDb, number) == hash {
			ancestorHash := rawdb.ReadCanonicalHash(hc.headerDb, number-ancestor)
			if rawdb.ReadCanonicalHash(hc.headerDb, number) == hash {
				number -= ancestor
				return ancestorHash, number
			}
		}
		if *maxNonCanonical == 0 {
			return common.Hash{}, 0
		}
		*maxNonCanonical--
		ancestor--
		header := hc.GetHeader(hash, number)
		if header == nil {
			return common.Hash{}, 0
		}
		hash = header.ParentHash[types.QuaiNetworkContext]
		number--
	}
	return hash, number
}

// GetAncestorByLocation retrieves the first occurrence of a block with a given location from a given block.
//
// Note: location == hash location returns the same block.
func (hc *HeaderChain) GetAncestorByLocation(hash common.Hash, location []byte) (*types.Header, error) {
	header := hc.GetHeaderByHash(hash)
	if header != nil {
		return nil, errors.New("error finding header by hash")
	}

	for !bytes.Equal(header.Location, location) {
		hash = header.ParentHash[types.QuaiNetworkContext]

		header := hc.GetHeaderByHash(hash)
		if header != nil {
			return nil, errors.New("error finding header by hash")
		}
	}
	return header, nil
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash and number, caching it if found.
func (hc *HeaderChain) GetTd(hash common.Hash, number uint64) []*big.Int {
	// Short circuit if the td's already in the cache, retrieve otherwise
	// if cached, ok := hc.tdCache.Get(hash); ok {
	// 	return cached.([]*big.Int)
	// }
	td := rawdb.ReadTd(hc.headerDb, hash, number)
	if td == nil {
		return make([]*big.Int, 3)
	}
	// Cache the found body for next time and return
	hc.tdCache.Add(hash, td)
	return td
}

// GetTdByHash retrieves a block's total difficulty in the canonical chain from the
// database by hash, caching it if found.
func (hc *HeaderChain) GetTdByHash(hash common.Hash) []*big.Int {
	number := hc.GetBlockNumber(hash)
	if number == nil {
		return make([]*big.Int, 3)
	}
	return hc.GetTd(hash, *number)
}

// GetHeader retrieves a block header from the database by hash and number,
// caching it if found.
func (hc *HeaderChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	// Short circuit if the header's already in the cache, retrieve otherwise
	if header, ok := hc.headerCache.Get(hash); ok {
		return header.(*types.Header)
	}
	header := rawdb.ReadHeader(hc.headerDb, hash, number)
	if header == nil {
		return nil
	}
	// Cache the found header for next time and return
	hc.headerCache.Add(hash, header)
	return header
}

// GetHeaderByHash retrieves a block header from the database by hash, caching it if
// found.
func (hc *HeaderChain) GetHeaderByHash(hash common.Hash) *types.Header {
	number := hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	return hc.GetHeader(hash, *number)
}

// HasHeader checks if a block header is present in the database or not.
// In theory, if header is present in the database, all relative components
// like td and hash->number should be present too.
func (hc *HeaderChain) HasHeader(hash common.Hash, number uint64) bool {
	if hc.numberCache.Contains(hash) || hc.headerCache.Contains(hash) {
		return true
	}
	return rawdb.HasHeader(hc.headerDb, hash, number)
}

// GetHeaderByNumber retrieves a block header from the database by number,
// caching it (associated with its hash) if found.
func (hc *HeaderChain) GetHeaderByNumber(number uint64) *types.Header {
	hash := rawdb.ReadCanonicalHash(hc.headerDb, number)
	if hash == (common.Hash{}) {
		return nil
	}
	return hc.GetHeader(hash, number)
}

func (hc *HeaderChain) GetCanonicalHash(number uint64) common.Hash {
	hash := rawdb.ReadCanonicalHash(hc.headerDb, number)
	fmt.Println("GetCanonicalHash", hash)
	return hash
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
func (hc *HeaderChain) CurrentHeader() *types.Header {
	return hc.currentHeader.Load().(*types.Header)
}

// CurrentBlock returns the block for the current header.
func (hc *HeaderChain) CurrentBlock() *types.Block {
	return hc.GetBlockByHash(hc.CurrentHeader().Hash())
}

// SetGenesis sets a new genesis block header for the chain
func (hc *HeaderChain) SetGenesis(head *types.Header) {
	hc.genesisHeader = head
}

// Config retrieves the header chain's chain configuration.
func (hc *HeaderChain) Config() *params.ChainConfig { return hc.config }

// GetBlock implements consensus.ChainReader, and returns nil for every input as
// a header chain does not have blocks available for retrieval.
func (hc *HeaderChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return hc.bc.GetBlock(hash, number)
}

// CheckContext checks to make sure the range of a context or order is valid
func (hc *HeaderChain) CheckContext(context int) error {
	if context < 0 || context > len(params.FullerOntology) {
		return errors.New("the provided path is outside the allowable range")
	}
	return nil
}

// CheckLocationRange checks to make sure the range of r and z are valid
func (hc *HeaderChain) CheckLocationRange(location []byte) error {
	if int(location[0]) < 1 || int(location[0]) > params.FullerOntology[0] {
		return errors.New("the provided location is outside the allowable region range")
	}
	if int(location[1]) < 1 || int(location[1]) > params.FullerOntology[1] {
		return errors.New("the provided location is outside the allowable zone range")
	}
	return nil
}

// GasLimit returns the gas limit of the current HEAD block.
func (hc *HeaderChain) GasLimit() uint64 {
	return hc.CurrentHeader().GasLimit[types.QuaiNetworkContext]
}

// GetUnclesInChain retrieves all the uncles from a given block backwards until
// a specific distance is reached.
func (hc *HeaderChain) GetUnclesInChain(block *types.Block, length int) []*types.Header {
	uncles := []*types.Header{}
	for i := 0; block != nil && i < length; i++ {
		uncles = append(uncles, block.Uncles()...)
		block = hc.GetBlock(block.ParentHash(), block.NumberU64()-1)
	}
	return uncles
}

// GetGasUsedInChain retrieves all the gas used from a given block backwards until
// a specific distance is reached.
func (hc *HeaderChain) GetGasUsedInChain(block *types.Block, length int) int64 {
	gasUsed := 0
	for i := 0; block != nil && i < length; i++ {
		gasUsed += int(block.GasUsed())
		block = hc.GetBlock(block.ParentHash(), block.NumberU64()-1)
	}
	return int64(gasUsed)
}

// GetGasUsedInChain retrieves all the gas used from a given block backwards until
// a specific distance is reached.
func (hc *HeaderChain) CalculateBaseFee(header *types.Header) *big.Int {
	return misc.CalcBaseFee(hc.Config(), header, hc.GetHeaderByNumber, hc.GetUnclesInChain, hc.GetGasUsedInChain)
}

// Export writes the active chain to the given writer.
func (hc *HeaderChain) Export(w io.Writer) error {
	return hc.ExportN(w, uint64(0), hc.CurrentHeader().Number64())
}

// ExportN writes a subset of the active chain to the given writer.
func (hc *HeaderChain) ExportN(w io.Writer, first uint64, last uint64) error {
	hc.headermu.RLock()
	defer hc.headermu.RUnlock()

	if first > last {
		return fmt.Errorf("export failed: first (%d) is greater than last (%d)", first, last)
	}
	log.Info("Exporting batch of blocks", "count", last-first+1)

	start, reported := time.Now(), time.Now()
	for nr := first; nr <= last; nr++ {
		block := hc.GetBlockByNumber(nr)
		if block == nil {
			return fmt.Errorf("export failed on #%d: not found", nr)
		}
		if err := block.EncodeRLP(w); err != nil {
			return err
		}
		if time.Since(reported) >= statsReportLimit {
			log.Info("Exporting blocks", "exported", block.NumberU64()-first, "elapsed", common.PrettyDuration(time.Since(start)))
			reported = time.Now()
		}
	}
	return nil
}

// GetBlockByHash retrieves a block from the database by hash, caching it if found.
func (hc *HeaderChain) GetBlockByHash(hash common.Hash) *types.Block {
	number := hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	return hc.GetBlock(hash, *number)
}

// GetBlockByNumber retrieves a block from the database by number, caching it
// (associated with its hash) if found.
func (hc *HeaderChain) GetBlockByNumber(number uint64) *types.Block {
	hash := rawdb.ReadCanonicalHash(hc.headerDb, number)
	if hash == (common.Hash{}) {
		return nil
	}
	return hc.GetBlock(hash, number)
}

// GetBody retrieves a block body (transactions and uncles) from the database by
// hash, caching it if found.
func (hc *HeaderChain) GetBody(hash common.Hash) *types.Body {
	// Short circuit if the body's already in the cache, retrieve otherwise
	if cached, ok := hc.bc.bodyCache.Get(hash); ok {
		body := cached.(*types.Body)
		return body
	}
	number := hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	body := rawdb.ReadBody(hc.headerDb, hash, *number)
	if body == nil {
		return nil
	}
	// Cache the found body for next time and return
	hc.bc.bodyCache.Add(hash, body)
	return body
}

// GetBodyRLP retrieves a block body in RLP encoding from the database by hash,
// caching it if found.
func (hc *HeaderChain) GetBodyRLP(hash common.Hash) rlp.RawValue {
	// Short circuit if the body's already in the cache, retrieve otherwise
	if cached, ok := hc.bc.bodyRLPCache.Get(hash); ok {
		return cached.(rlp.RawValue)
	}
	number := hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	body := rawdb.ReadBodyRLP(hc.headerDb, hash, *number)
	if len(body) == 0 {
		return nil
	}
	// Cache the found body for next time and return
	hc.bc.bodyRLPCache.Add(hash, body)
	return body
}

// GetBlocksFromHash returns the block corresponding to hash and up to n-1 ancestors.
// [deprecated by eth/62]
func (hc *HeaderChain) GetBlocksFromHash(hash common.Hash, n int) (blocks []*types.Block) {
	number := hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	for i := 0; i < n; i++ {
		block := hc.GetBlock(hash, *number)
		if block == nil {
			break
		}
		blocks = append(blocks, block)
		hash = block.ParentHash()
		*number--
	}
	return
}

// Engine reterives the consensus engine.
func (hc *HeaderChain) Engine() consensus.Engine {
	return hc.engine
}

// SubscribeChainHeadEvent registers a subscription of ChainHeadEvent.
func (hc *HeaderChain) SubscribeChainHeadEvent(ch chan<- ChainHeadEvent) event.Subscription {
	return hc.scope.Track(hc.chainHeadFeed.Subscribe(ch))
}

func (hc *HeaderChain) StateAt(root common.Hash) (*state.StateDB, error) {
	return hc.bc.processor.StateAt(root)
}
