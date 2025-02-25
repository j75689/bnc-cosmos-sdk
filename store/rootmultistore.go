package store

import (
	"fmt"
	"io"
	"strings"

	"github.com/bnb-chain/ics23"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/merkle"
	"github.com/tendermint/tendermint/crypto/tmhash"
	dbm "github.com/tendermint/tendermint/libs/db"

	sdkproofs "github.com/cosmos/cosmos-sdk/store/proofs"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	latestVersionKey = "s/latest"
	commitInfoKeyFmt = "s/%d" // s/<version>
)

// rootMultiStore is composed of many CommitStores. Name contrasts with
// cacheMultiStore which is for cache-wrapping other MultiStores. It implements
// the CommitMultiStore interface.
type rootMultiStore struct {
	db           dbm.DB
	lastCommitID CommitID
	pruning      sdk.PruningStrategy
	storesParams map[StoreKey]storeParams
	stores       map[StoreKey]CommitStore
	keysByName   map[string]StoreKey

	traceWriter  io.Writer
	traceContext TraceContext
}

var _ CommitMultiStore = (*rootMultiStore)(nil)
var _ Queryable = (*rootMultiStore)(nil)

// nolint
func NewCommitMultiStore(db dbm.DB) *rootMultiStore {
	return &rootMultiStore{
		db:           db,
		storesParams: make(map[StoreKey]storeParams),
		stores:       make(map[StoreKey]CommitStore),
		keysByName:   make(map[string]StoreKey),
	}
}

// Implements CommitMultiStore
func (rs *rootMultiStore) SetPruning(pruning sdk.PruningStrategy) {
	rs.pruning = pruning
	for _, substore := range rs.stores {
		substore.SetPruning(pruning)
	}
}

// Implements Store.
func (rs *rootMultiStore) GetStoreType() StoreType {
	return sdk.StoreTypeMulti
}

// Implements CommitMultiStore.
func (rs *rootMultiStore) MountStoreWithDB(key StoreKey, typ StoreType, db dbm.DB) {
	if key == nil {
		panic("MountIAVLStore() key cannot be nil")
	}
	if _, ok := rs.storesParams[key]; ok {
		panic(fmt.Sprintf("rootMultiStore duplicate store key %v", key))
	}
	if _, ok := rs.keysByName[key.Name()]; ok {
		panic(fmt.Sprintf("rootMultiStore duplicate store key name %v", key))
	}
	rs.storesParams[key] = storeParams{
		key: key,
		typ: typ,
		db:  db,
	}
	rs.keysByName[key.Name()] = key
}

// Implements CommitMultiStore.
func (rs *rootMultiStore) GetCommitStore(key StoreKey) CommitStore {
	return rs.stores[key]
}

// Implements CommitMultiStore.
func (rs *rootMultiStore) GetCommitKVStore(key StoreKey) CommitKVStore {
	return rs.stores[key].(CommitKVStore)
}

func (rs *rootMultiStore) GetCommitKVStores() map[StoreKey]CommitKVStore {
	res := make(map[StoreKey]CommitKVStore)
	for key, store := range rs.stores {
		if s, ok := store.(CommitKVStore); ok {
			res[key] = s
		}
	}
	return res
}

// Implements CommitMultiStore.
func (rs *rootMultiStore) LoadLatestVersion() error {
	ver := getLatestVersion(rs.db)
	return rs.LoadVersion(ver)
}

// Implements CommitMultiStore.
func (rs *rootMultiStore) LoadVersion(ver int64) error {

	// Special logic for version 0
	if ver == 0 {
		for key, storeParams := range rs.storesParams {
			id := CommitID{}
			store, err := rs.loadCommitStoreFromParams(key, id, storeParams)
			if err != nil {
				return fmt.Errorf("failed to load rootMultiStore: %v", err)
			}
			rs.stores[key] = store
		}

		rs.lastCommitID = CommitID{}
		return nil
	}
	// Otherwise, version is 1 or greater

	// Get CommitInfo
	cInfo, err := getCommitInfo(rs.db, ver)
	if err != nil {
		return err
	}

	// Convert StoreInfos slice to map
	infos := make(map[StoreKey]StoreInfo)
	for _, storeInfo := range cInfo.StoreInfos {
		infos[rs.nameToKey(storeInfo.Name)] = storeInfo
	}

	// Load each Store
	var newStores = make(map[StoreKey]CommitStore)
	for key, storeParams := range rs.storesParams {
		var id CommitID
		info, ok := infos[key]
		if ok {
			id = info.Core.CommitID
		}

		store, err := rs.loadCommitStoreFromParams(key, id, storeParams)
		if err != nil {
			return fmt.Errorf("failed to load rootMultiStore: %v", err)
		}
		newStores[key] = store
	}

	// Success.
	rs.lastCommitID = cInfo.CommitID()
	rs.stores = newStores
	return nil
}

// WithTracer sets the tracer for the MultiStore that the underlying
// stores will utilize to trace operations. A MultiStore is returned.
func (rs *rootMultiStore) WithTracer(w io.Writer) MultiStore {
	rs.traceWriter = w
	return rs
}

// WithTracingContext updates the tracing context for the MultiStore by merging
// the given context with the existing context by key. Any existing keys will
// be overwritten. It is implied that the caller should update the context when
// necessary between tracing operations. It returns a modified MultiStore.
func (rs *rootMultiStore) WithTracingContext(tc TraceContext) MultiStore {
	if rs.traceContext != nil {
		for k, v := range tc {
			rs.traceContext[k] = v
		}
	} else {
		rs.traceContext = tc
	}

	return rs
}

// TracingEnabled returns if tracing is enabled for the MultiStore.
func (rs *rootMultiStore) TracingEnabled() bool {
	return rs.traceWriter != nil
}

// ResetTraceContext resets the current tracing context.
func (rs *rootMultiStore) ResetTraceContext() MultiStore {
	rs.traceContext = nil
	return rs
}

//----------------------------------------
// +CommitStore

// Implements Committer/CommitStore.
func (rs *rootMultiStore) LastCommitID() CommitID {
	return rs.lastCommitID
}

// Implements Committer/CommitStore.
func (rs *rootMultiStore) SetVersion(version int64) {}

// Implements Committer/CommitStore.
func (rs *rootMultiStore) Commit() CommitID {
	version := rs.lastCommitID.Version + 1
	// Commit stores.
	commitInfo := commitStores(version, rs.stores)

	// Need to update atomically.
	batch := rs.db.NewBatch()
	defer batch.Close()
	setCommitInfo(batch, version, commitInfo)
	setLatestVersion(batch, version)
	batch.Write()

	// Prepare for next version.
	commitID := CommitID{
		Version: version,
		Hash:    commitInfo.Hash(),
	}
	rs.lastCommitID = commitID
	return commitID
}

// Implements CacheWrapper/Store/CommitStore.
func (rs *rootMultiStore) CacheWrap() CacheWrap {
	return rs.CacheMultiStore().(CacheWrap)
}

// CacheWrapWithTrace implements the CacheWrapper interface.
func (rs *rootMultiStore) CacheWrapWithTrace(_ io.Writer, _ TraceContext) CacheWrap {
	return rs.CacheWrap()
}

//----------------------------------------
// +MultiStore

// Implements MultiStore.
func (rs *rootMultiStore) CacheMultiStore() CacheMultiStore {
	return newCacheMultiStoreFromRMS(rs)
}

// Implements MultiStore.
func (rs *rootMultiStore) GetStore(key StoreKey) Store {
	return rs.stores[key]
}

// GetKVStore implements the MultiStore interface. If tracing is enabled on the
// rootMultiStore, a wrapped TraceKVStore will be returned with the given
// tracer, otherwise, the original KVStore will be returned.
func (rs *rootMultiStore) GetKVStore(key StoreKey) KVStore {
	store := rs.stores[key].(KVStore)

	if rs.TracingEnabled() {
		store = NewTraceKVStore(store, rs.traceWriter, rs.traceContext)
	}

	return store
}

// Implements MultiStore

// getStoreByName will first convert the original name to
// a special key, before looking up the CommitStore.
// This is not exposed to the extensions (which will need the
// StoreKey), but is useful in main, and particularly app.Query,
// in order to convert human strings into CommitStores.
func (rs *rootMultiStore) getStoreByName(name string) Store {
	key := rs.keysByName[name]
	if key == nil {
		return nil
	}
	return rs.stores[key]
}

//---------------------- Query ------------------

// Query calls substore.Query with the same `req` where `req.Path` is
// modified to remove the substore prefix.
// Ie. `req.Path` here is `/<substore>/<path>`, and trimmed to `/<path>` for the substore.
// TODO: add proof for `multistore -> substore`.
func (rs *rootMultiStore) Query(req abci.RequestQuery) abci.ResponseQuery {
	// Query just routes this to a substore.
	path := req.Path
	storeName, subpath, err := parsePath(path)
	if err != nil {
		return err.QueryResult()
	}

	store := rs.getStoreByName(storeName)
	if store == nil {
		msg := fmt.Sprintf("no such store: %s", storeName)
		return sdk.ErrUnknownRequest(msg).QueryResult()
	}
	queryable, ok := store.(Queryable)
	if !ok {
		msg := fmt.Sprintf("store %s doesn't support queries", storeName)
		return sdk.ErrUnknownRequest(msg).QueryResult()
	}

	// trim the path and make the query
	req.Path = subpath
	res := queryable.Query(req)

	if !req.Prove || !RequireProof(subpath) {
		return res
	}

	commitInfo, errMsg := getCommitInfo(rs.db, res.Height)
	if errMsg != nil {
		return sdk.ErrInternal(errMsg.Error()).QueryResult()
	}

	if subpath == "/ics23-key" {
		res.Proof.Ops = append(res.Proof.Ops, commitInfo.ProofOp(storeName))
	} else {
		// Restore origin path and append proof op.
		res.Proof.Ops = append(res.Proof.Ops, NewMultiStoreProofOp(
			[]byte(storeName),
			NewMultiStoreProof(commitInfo.StoreInfos),
		).ProofOp())
	}

	return res
}

// parsePath expects a format like /<storeName>[/<subpath>]
// Must start with /, subpath may be empty
// Returns error if it doesn't start with /
func parsePath(path string) (storeName string, subpath string, err sdk.Error) {
	if !strings.HasPrefix(path, "/") {
		err = sdk.ErrUnknownRequest(fmt.Sprintf("invalid path: %s", path))
		return
	}
	paths := strings.SplitN(path[1:], "/", 2)
	storeName = paths[0]
	if len(paths) == 2 {
		subpath = "/" + paths[1]
	}
	return
}

//----------------------------------------

func (rs *rootMultiStore) loadCommitStoreFromParams(key sdk.StoreKey, id CommitID, params storeParams) (store CommitStore, err error) {
	var db dbm.DB
	if params.db != nil {
		db = dbm.NewPrefixDB(params.db, []byte("s/_/"))
	} else {
		db = dbm.NewPrefixDB(rs.db, []byte("s/k:"+params.key.Name()+"/"))
	}
	switch params.typ {
	case sdk.StoreTypeMulti:
		panic("recursive MultiStores not yet supported")
		// TODO: id?
		// return NewCommitMultiStore(db, id)
	case sdk.StoreTypeIAVL:
		store, err = LoadIAVLStore(db, id, rs.pruning)
		return
	case sdk.StoreTypeDB:
		panic("dbm.DB is not a CommitStore")
	case sdk.StoreTypeTransient:
		_, ok := key.(*sdk.TransientStoreKey)
		if !ok {
			err = fmt.Errorf("invalid StoreKey for StoreTypeTransient: %s", key.String())
			return
		}
		store = newTransientStore()
		return
	default:
		panic(fmt.Sprintf("unrecognized store type %v", params.typ))
	}
}

func (rs *rootMultiStore) nameToKey(name string) StoreKey {
	for key := range rs.storesParams {
		if key.Name() == name {
			return key
		}
	}
	panic("Unknown name " + name)
}

//----------------------------------------
// storeParams

type storeParams struct {
	key StoreKey
	db  dbm.DB
	typ StoreType
}

//----------------------------------------
// CommitInfo

// NOTE: Keep CommitInfo a simple immutable struct.
type CommitInfo struct {

	// Version
	Version int64

	// Store info for
	StoreInfos []StoreInfo
}

// GetHash returns the GetHash from the CommitID.
// This is used in CommitInfo.Hash()
//
// When we commit to this in a merkle proof, we create a map of storeInfo.Name -> storeInfo.GetHash()
// and build a merkle proof from that.
// This is then chained with the substore proof, so we prove the root hash from the substore before this
// and need to pass that (unmodified) as the leaf value of the multistore proof.
func (si StoreInfo) GetHash() []byte {
	return si.Core.CommitID.Hash
}

// Hash returns the simple merkle root hash of the stores sorted by name.
func (ci CommitInfo) Hash() []byte {
	m := make(map[string][]byte, len(ci.StoreInfos))
	if sdk.IsUpgrade(sdk.BEP171) {
		for _, storeInfo := range ci.StoreInfos {
			m[storeInfo.Name] = storeInfo.GetHash()
		}
	} else {
		for _, storeInfo := range ci.StoreInfos {
			m[storeInfo.Name] = storeInfo.Hash()
		}
	}

	return merkle.SimpleHashFromMap(m)
}

func (ci CommitInfo) CommitID() CommitID {
	return CommitID{
		Version: ci.Version,
		Hash:    ci.Hash(),
	}
}

func (ci CommitInfo) toMap() map[string][]byte {
	m := make(map[string][]byte, len(ci.StoreInfos))
	if sdk.IsUpgrade(sdk.BEP171) {
		for _, storeInfo := range ci.StoreInfos {
			m[storeInfo.Name] = storeInfo.Core.CommitID.Hash
		}
	} else {
		for _, storeInfo := range ci.StoreInfos {
			m[storeInfo.Name] = storeInfo.Hash()
		}
	}

	return m
}

func (ci CommitInfo) ProofOp(storeName string) merkle.ProofOp {
	cmap := ci.toMap()
	_, proofs, _ := merkle.SimpleProofsFromMap(cmap)

	proof := proofs[storeName]
	if proof == nil {
		panic(fmt.Sprintf("ProofOp for %s but not registered store name", storeName))
	}

	// convert merkle.SimpleProof to CommitmentProof
	existProof, err := sdkproofs.ConvertExistenceProof(proof, []byte(storeName), cmap[storeName])
	if err != nil {
		panic(fmt.Errorf("could not convert simple proof to existence proof: %w", err))
	}

	commitmentProof := &ics23.CommitmentProof{
		Proof: &ics23.CommitmentProof_Exist{
			Exist: existProof,
		},
	}

	return NewSimpleMerkleCommitmentOp([]byte(storeName), commitmentProof).ProofOp()
}

//----------------------------------------
// StoreInfo

// StoreInfo contains the name and core reference for an
// underlying store.  It is the leaf of the rootMultiStores top
// level simple merkle tree.
type StoreInfo struct {
	Name string
	Core StoreCore
}

type StoreCore struct {
	// StoreType StoreType
	CommitID CommitID
	// ... maybe add more state
}

// Implements merkle.Hasher.
func (si StoreInfo) Hash() []byte {
	// Doesn't write Name, since merkle.SimpleHashFromMap() will
	// include them via the keys.
	bz, _ := cdc.MarshalBinaryLengthPrefixed(si.Core) // Does not error
	hasher := tmhash.New()
	_, err := hasher.Write(bz)
	if err != nil {
		// TODO: Handle with #870
		panic(err)
	}
	return hasher.Sum(nil)
}

//----------------------------------------
// Misc.

func getLatestVersion(db dbm.DB) int64 {
	var latest int64
	latestBytes := db.Get([]byte(latestVersionKey))
	if latestBytes == nil {
		return 0
	}
	err := cdc.UnmarshalBinaryLengthPrefixed(latestBytes, &latest)
	if err != nil {
		panic(err)
	}
	return latest
}

// Set the latest version.
func setLatestVersion(batch dbm.Batch, version int64) {
	latestBytes, _ := cdc.MarshalBinaryLengthPrefixed(version) // Does not error
	batch.Set([]byte(latestVersionKey), latestBytes)
}

// Commits each store and returns a new CommitInfo.
func commitStores(version int64, storeMap map[StoreKey]CommitStore) CommitInfo {
	storeInfos := make([]StoreInfo, 0, len(storeMap))

	for key, store := range storeMap {
		if !sdk.ShouldCommitStore(key.Name()) {
			continue
		}

		// set version for store to commit, just to keep the same as the other stores
		if sdk.ShouldSetStoreVersion(key.Name()) {
			store.SetVersion(version - 1)
		}

		// Commit
		commitID := store.Commit()

		if store.GetStoreType() == sdk.StoreTypeTransient {
			continue
		}

		// Record CommitID
		si := StoreInfo{}
		si.Name = key.Name()
		si.Core.CommitID = commitID
		// si.Core.StoreType = store.GetStoreType()
		storeInfos = append(storeInfos, si)
	}

	ci := CommitInfo{
		Version:    version,
		StoreInfos: storeInfos,
	}
	return ci
}

// Gets CommitInfo from disk.
func getCommitInfo(db dbm.DB, ver int64) (CommitInfo, error) {

	// Get from DB.
	cInfoKey := fmt.Sprintf(commitInfoKeyFmt, ver)
	cInfoBytes := db.Get([]byte(cInfoKey))
	if cInfoBytes == nil {
		return CommitInfo{}, fmt.Errorf("failed to get rootMultiStore: no data")
	}

	// Parse bytes.
	var cInfo CommitInfo
	err := cdc.UnmarshalBinaryLengthPrefixed(cInfoBytes, &cInfo)
	if err != nil {
		return CommitInfo{}, fmt.Errorf("failed to get rootMultiStore: %v", err)
	}
	return cInfo, nil
}

// Set a CommitInfo for given version.
func setCommitInfo(batch dbm.Batch, version int64, cInfo CommitInfo) {
	cInfoBytes, err := cdc.MarshalBinaryLengthPrefixed(cInfo)
	if err != nil {
		panic(err)
	}
	cInfoKey := fmt.Sprintf(commitInfoKeyFmt, version)
	batch.Set([]byte(cInfoKey), cInfoBytes)
}
