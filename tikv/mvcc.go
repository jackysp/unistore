package tikv

import (
	"bytes"
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"

	"github.com/coocood/badger"
	"github.com/cznic/mathutil"
	"github.com/juju/errors"
	"github.com/ngaut/faketikv/lockstore"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/tidb/util/codec"
)

// MVCCStore is a wrapper of badger.DB to provide MVCC functions.
type MVCCStore struct {
	dir             string
	db              *badger.DB
	writeDBWorker   *writeDBWorker
	lockStore       *lockstore.MemStore
	rollbackStore   *lockstore.MemStore
	writeLockWorker *writeLockWorker
	closeCh         chan struct{}
	wg              sync.WaitGroup

	// latestTS records the latest timestamp of requests, used to determine if it is safe to GC rollback key.
	latestTS uint64
}

// NewMVCCStore creates a new MVCCStore
func NewMVCCStore(db *badger.DB, dataDir string) *MVCCStore {
	ls := lockstore.NewMemStore(8 << 20)
	rollbackStore := lockstore.NewMemStore(256 << 10)
	closeCh := make(chan struct{})
	store := &MVCCStore{
		db:  db,
		dir: dataDir,
		writeDBWorker: &writeDBWorker{
			wakeUp:  make(chan struct{}, 1),
			closeCh: closeCh,
		},
		lockStore:     ls,
		rollbackStore: rollbackStore,
		writeLockWorker: &writeLockWorker{
			wakeUp:  make(chan struct{}, 1),
			closeCh: closeCh,
		},
		closeCh: closeCh,
	}
	store.writeDBWorker.store = store
	store.writeLockWorker.store = store
	err := store.loadLocks()
	if err != nil {
		log.Fatal(err)
	}

	// mark worker count
	store.wg.Add(3)
	// run all the workers
	go store.writeDBWorker.run()
	go store.writeLockWorker.run()
	go func() {
		rbGCWorker := rollbackGCWorker{store: store}
		rbGCWorker.run()
	}()

	return store
}

func (store *MVCCStore) Close() error {
	close(store.closeCh)
	store.wg.Wait()

	err := store.dumpMemLocks()
	if err != nil {
		log.Fatal(err)
	}
	return nil
}

func (store *MVCCStore) getLatestTS() uint64 {
	return atomic.LoadUint64(&store.latestTS)
}

func (store *MVCCStore) updateLatestTS(ts uint64) {
	latestTS := store.getLatestTS()
	if ts != math.MaxUint64 && ts > latestTS {
		atomic.CompareAndSwapUint64(&store.latestTS, latestTS, ts)
	}
}

func (store *MVCCStore) Prewrite(reqCtx *requestCtx, mutations []*kvrpcpb.Mutation, primary []byte, startTS uint64, ttl uint64) []error {
	regCtx := reqCtx.regCtx
	hashVals := mutationsToHashVals(mutations)
	errs := make([]error, 0, len(mutations))
	anyError := false

	regCtx.acquireLatches(hashVals)
	reqCtx.trace(eventAcquireLatches)
	defer regCtx.releaseLatches(hashVals)

	// Must check the LockStore first.
	for _, m := range mutations {
		duplicate, err := store.checkPrewriteInLockStore(reqCtx, m, startTS)
		if err != nil {
			anyError = true
		}
		if duplicate {
			return nil
		}
		errs = append(errs, err)
	}
	reqCtx.trace(eventReadLock)
	if anyError {
		return errs
	}

	lockBatch := newWriteLockBatch(reqCtx)
	// Check the DB.
	txn := reqCtx.getDBReader().txn
	for i, m := range mutations {
		hasOldVer, err := store.checkPrewriteInDB(reqCtx, txn, m, startTS)
		if err != nil {
			anyError = true
		}
		errs[i] = err
		if !anyError {
			lock := mvccLock{
				mvccLockHdr: mvccLockHdr{
					startTS:    startTS,
					op:         uint8(m.Op),
					hasOldVer:  hasOldVer,
					ttl:        uint32(ttl),
					primaryLen: uint16(len(primary)),
				},
				primary: primary,
				value:   m.Value,
			}
			lockBatch.set(m.Key, lock.MarshalBinary())
		}
	}
	reqCtx.trace(eventReadDB)
	if anyError {
		return errs
	}
	err := store.writeLocks(lockBatch)
	reqCtx.trace(eventEndWriteLock)
	if err != nil {
		return []error{err}
	}
	return nil
}

func (store *MVCCStore) checkPrewriteInLockStore(
	req *requestCtx, mutation *kvrpcpb.Mutation, startTS uint64) (duplicate bool, err error) {
	req.buf = encodeRollbackKey(req.buf, mutation.Key, startTS)
	if len(store.rollbackStore.Get(req.buf, nil)) > 0 {
		return false, ErrAlreadyRollback
	}
	req.buf = store.lockStore.Get(mutation.Key, req.buf)
	if len(req.buf) == 0 {
		return false, nil
	}
	lock := decodeLock(req.buf)
	if lock.startTS == startTS {
		// Same ts, no need to overwrite.
		return true, nil
	}
	return false, &ErrLocked{
		Key:     mutation.Key,
		StartTS: lock.startTS,
		Primary: lock.primary,
		TTL:     uint64(lock.ttl),
	}
}

// checkPrewrietInDB checks that there is no committed version greater than startTS or return write conflict error.
// And it returns a bool value indicates if there is an old version.
func (store *MVCCStore) checkPrewriteInDB(
	req *requestCtx, txn *badger.Txn, mutation *kvrpcpb.Mutation, startTS uint64) (hasOldVer bool, err error) {
	item, err := txn.Get(mutation.Key)
	if err != nil && err != badger.ErrKeyNotFound {
		return false, errors.Trace(err)
	}
	if item == nil {
		return false, nil
	}
	mvVal, err := decodeValue(item)
	if err != nil {
		return false, errors.Trace(err)
	}
	if mvVal.commitTS > startTS {
		return false, ErrRetryable("write conflict")
	}
	return true, nil
}

const maxSystemTS uint64 = math.MaxUint64

// Commit implements the MVCCStore interface.
func (store *MVCCStore) Commit(req *requestCtx, keys [][]byte, startTS, commitTS uint64) error {
	store.updateLatestTS(commitTS)
	regCtx := req.regCtx
	hashVals := keysToHashVals(keys...)
	dbBatch := newWriteDBBatch(req)

	regCtx.acquireLatches(hashVals)
	req.trace(eventAcquireLatches)
	defer regCtx.releaseLatches(hashVals)

	var buf []byte
	var tmpDiff int
	needMove := make([]bool, len(keys))
	for i, key := range keys {
		buf = store.lockStore.Get(key, buf)
		if len(buf) == 0 {
			// We never commit partial keys in Commit request, so if one lock is not found,
			// the others keys must not be found too.
			return store.handleLockNotFound(req, key, startTS, commitTS)
		}
		lock := decodeLock(buf)
		if lock.startTS != startTS {
			return ErrReplaced
		}
		if lock.op == uint8(kvrpcpb.Op_Lock) {
			continue
		}
		needMove[i] = lock.hasOldVer
		val := lockToValue(lock, commitTS)
		commitVal := val.MarshalBinary()
		tmpDiff += len(key) + len(commitVal)
		dbBatch.set(key, commitVal)
	}
	req.trace(eventReadLock)
	// Move current latest to old.
	txn := req.getDBReader().txn
	for i, key := range keys {
		if !needMove[i] {
			continue
		}
		item, err := txn.Get(key)
		if err != nil && err != badger.ErrKeyNotFound {
			return errors.Trace(err)
		}
		if item == nil {
			continue
		}
		mvVal, err := decodeValue(item)
		if err != nil {
			return errors.Trace(err)
		}
		oldKey := encodeOldKey(key, mvVal.commitTS)
		dbBatch.set(oldKey, mvVal.MarshalBinary())
	}
	req.trace(eventReadDB)
	atomic.AddInt64(&regCtx.diff, int64(tmpDiff))
	err := store.writeDB(dbBatch)
	if err != nil {
		return errors.Trace(err)
	}
	// We must delete lock after commit succeed, or there will be inconsistency.
	lockBatch := newWriteLockBatch(req)
	for _, key := range keys {
		lockBatch.delete(key)
	}
	err = store.writeLocks(lockBatch)
	req.trace(eventEndWriteLock)
	return errors.Trace(err)
}

func (store *MVCCStore) handleLockNotFound(reqCtx *requestCtx, key []byte, startTS, commitTS uint64) error {
	txn := reqCtx.getDBReader().txn
	item, err := txn.Get(key)
	if err != nil && err != badger.ErrKeyNotFound {
		return errors.Trace(err)
	}
	if item == nil {
		return ErrLockNotFound
	}
	mvVal, err := decodeValue(item)
	if err != nil {
		return errors.Trace(err)
	}
	if mvVal.startTS == startTS {
		// Already committed.
		return nil
	} else {
		// The transaction may be committed and moved to old data, we need to look for that.
		oldKey := encodeOldKey(key, commitTS)
		_, err = txn.Get(oldKey)
		if err == nil {
			// Found committed key.
			return nil
		}
	}
	return ErrLockNotFound
}

const (
	rollbackStatusDone    = 0
	rollbackStatusNoLock  = 1
	rollbackStatusNewLock = 2
)

func (store *MVCCStore) Rollback(reqCtx *requestCtx, keys [][]byte, startTS uint64) error {
	store.updateLatestTS(startTS)
	hashVals := keysToHashVals(keys...)
	regCtx := reqCtx.regCtx
	lockBatch := newWriteLockBatch(reqCtx)

	regCtx.acquireLatches(hashVals)
	reqCtx.trace(eventAcquireLatches)
	defer regCtx.releaseLatches(hashVals)

	statuses := make([]int, len(keys))
	for i, key := range keys {
		statuses[i] = store.rollbackKeyReadLock(lockBatch, key, startTS)
	}
	reqCtx.trace(eventReadLock)
	for i, key := range keys {
		if statuses[i] == rollbackStatusDone {
			continue
		}
		err := store.rollbackKeyReadDB(reqCtx, lockBatch, key, startTS, statuses[i] == rollbackStatusNewLock)
		if err != nil {
			return err
		}
	}
	reqCtx.trace(eventReadDB)
	err := store.writeLocks(lockBatch)
	reqCtx.trace(eventEndWriteLock)
	return errors.Trace(err)
}

func (store *MVCCStore) rollbackKeyReadLock(batch *writeLockBatch, key []byte, startTS uint64) (status int) {
	batch.buf = encodeRollbackKey(batch.buf, key, startTS)
	rollbackKey := safeCopy(batch.buf)
	batch.buf = store.rollbackStore.Get(rollbackKey, batch.buf)
	if len(batch.buf) != 0 {
		// Already rollback.
		return rollbackStatusDone
	}
	batch.buf = store.lockStore.Get(key, batch.buf)
	hasLock := len(batch.buf) > 0
	if hasLock {
		lock := decodeLock(batch.buf)
		if lock.startTS < startTS {
			// The lock is old, means this is written by an old transaction, and the current transaction may not arrive.
			// We should write a rollback lock.
			batch.rollback(rollbackKey)
			return rollbackStatusDone
		}
		if lock.startTS == startTS {
			// We can not simply delete the lock because the prewrite may be sent multiple times.
			// To prevent that we update it a rollback lock.
			batch.rollback(rollbackKey)
			batch.delete(key)
			return rollbackStatusDone
		}
		// lock.startTS > startTS, go to DB to check if the key is committed.
		return rollbackStatusNewLock
	}
	return rollbackStatusNoLock
}

func (store *MVCCStore) rollbackKeyReadDB(req *requestCtx, batch *writeLockBatch, key []byte, startTS uint64, hasLock bool) error {
	batch.buf = encodeRollbackKey(batch.buf, key, startTS)
	rollbackKey := safeCopy(batch.buf)
	reader := req.getDBReader()
	item, err := reader.txn.Get(key)
	if err != nil && err != badger.ErrKeyNotFound {
		return errors.Trace(err)
	}
	hasVal := item != nil
	if !hasVal && !hasLock {
		// The prewrite request is not arrived, we write a rollback lock to prevent the future prewrite.
		batch.rollback(rollbackKey)
		return nil
	}

	if !hasVal {
		// Not committed.
		return nil
	}
	val, err := decodeValue(item)
	if err != nil {
		return errors.Trace(err)
	}
	if val.startTS == startTS {
		return ErrAlreadyCommitted(val.commitTS)
	}
	if val.startTS < startTS && !hasLock {
		// Prewrite and commit have not arrived.
		batch.rollback(rollbackKey)
		return nil
	}
	// val.startTS > startTS, look for the key in the old version to check if the key is committed.
	it := reader.getOldIter()
	oldKey := encodeOldKey(key, val.commitTS)
	// find greater commit version.
	for it.Seek(oldKey); it.ValidForPrefix(oldKey[:len(oldKey)-8]); it.Next() {
		item := it.Item()
		foundKey := item.Key()
		if isVisibleKey(foundKey, startTS) {
			break
		}
		_, ts, err := codec.DecodeUintDesc(foundKey[len(foundKey)-8:])
		if err != nil {
			return errors.Trace(err)
		}
		mvVal, err := decodeValue(item)
		if mvVal.startTS == startTS {
			return ErrAlreadyCommitted(ts)
		}
	}
	return nil
}

func isVisibleKey(key []byte, startTS uint64) bool {
	ts := ^(binary.BigEndian.Uint64(key[len(key)-8:]))
	return startTS >= ts
}

func checkLock(lock mvccLock, key []byte, startTS uint64) error {
	lockVisible := lock.startTS < startTS
	isWriteLock := lock.op == uint8(kvrpcpb.Op_Put) || lock.op == uint8(kvrpcpb.Op_Del)
	isPrimaryGet := startTS == maxSystemTS && bytes.Equal(lock.primary, key)
	if lockVisible && isWriteLock && !isPrimaryGet {
		return &ErrLocked{
			Key:     key,
			StartTS: lock.startTS,
			Primary: lock.primary,
			TTL:     uint64(lock.ttl),
		}
	}
	return nil
}

func (store *MVCCStore) CheckKeysLock(startTS uint64, keys ...[]byte) error {
	var buf []byte
	for _, key := range keys {
		buf = store.lockStore.Get(key, buf)
		if len(buf) == 0 {
			continue
		}
		lock := decodeLock(buf)
		err := checkLock(lock, key, startTS)
		if err != nil {
			return err
		}
	}
	return nil
}

func (store *MVCCStore) CheckRangeLock(startTS uint64, startKey, endKey []byte) error {
	it := store.lockStore.NewIterator()
	for it.Seek(startKey); it.Valid(); it.Next() {
		if exceedEndKey(it.Key(), endKey) {
			break
		}
		lock := decodeLock(it.Value())
		err := checkLock(lock, it.Key(), startTS)
		if err != nil {
			return err
		}
	}
	return nil
}

func (store *MVCCStore) Cleanup(reqCtx *requestCtx, key []byte, startTS uint64) error {
	store.updateLatestTS(startTS)
	hashVals := keysToHashVals(key)
	regCtx := reqCtx.regCtx
	lockBatch := newWriteLockBatch(reqCtx)

	regCtx.acquireLatches(hashVals)
	reqCtx.trace(eventAcquireLatches)
	defer regCtx.releaseLatches(hashVals)

	status := store.rollbackKeyReadLock(lockBatch, key, startTS)
	if status != rollbackStatusDone {
		err := store.rollbackKeyReadDB(reqCtx, lockBatch, key, startTS, status == rollbackStatusNewLock)
		reqCtx.trace(eventReadDB)
		if err != nil {
			return err
		}
	}
	return store.writeLocks(lockBatch)
}

func (store *MVCCStore) ScanLock(reqCtx *requestCtx, maxSystemTS uint64) ([]*kvrpcpb.LockInfo, error) {
	var locks []*kvrpcpb.LockInfo
	it := store.lockStore.NewIterator()
	for it.Seek(reqCtx.regCtx.startKey); it.Valid(); it.Next() {
		if exceedEndKey(it.Key(), reqCtx.regCtx.endKey) {
			return locks, nil
		}
		lock := decodeLock(it.Value())
		if lock.startTS < maxSystemTS {
			locks = append(locks, &kvrpcpb.LockInfo{
				PrimaryLock: lock.primary,
				LockVersion: lock.startTS,
				Key:         codec.EncodeBytes(nil, it.Key()),
				LockTtl:     uint64(lock.ttl),
			})
		}
	}
	reqCtx.trace(eventReadLock)
	return locks, nil
}

func (store *MVCCStore) ResolveLock(reqCtx *requestCtx, startTS, commitTS uint64) error {
	regCtx := reqCtx.regCtx
	var lockKeys [][]byte
	var lockVals [][]byte
	it := store.lockStore.NewIterator()
	for it.Seek(regCtx.startKey); it.Valid(); it.Next() {
		if exceedEndKey(it.Key(), regCtx.endKey) {
			break
		}
		lock := decodeLock(it.Value())
		if lock.startTS != startTS {
			continue
		}
		lockKeys = append(lockKeys, safeCopy(it.Key()))
		lockVals = append(lockVals, safeCopy(it.Value()))
	}
	reqCtx.trace(eventReadLock)
	if len(lockKeys) == 0 {
		return nil
	}
	hashVals := keysToHashVals(lockKeys...)
	lockBatch := newWriteLockBatch(reqCtx)
	var dbBatch *writeDBBatch
	if commitTS > 0 {
		dbBatch = newWriteDBBatch(reqCtx)
	}

	regCtx.acquireLatches(hashVals)
	reqCtx.trace(eventAcquireLatches)
	defer regCtx.releaseLatches(hashVals)

	var buf []byte
	for i, lockKey := range lockKeys {
		buf = store.lockStore.Get(lockKey, buf)
		// We need to check again make sure the lock is not changed.
		if bytes.Equal(buf, lockVals[i]) {
			if commitTS > 0 {
				lock := decodeLock(lockVals[i])
				mvVal := lockToValue(lock, commitTS)
				dbBatch.set(lockKey, mvVal.MarshalBinary())
			}
			lockBatch.delete(lockKey)
		}
	}
	reqCtx.trace(eventReadLock)
	if len(lockBatch.entries) == 0 {
		return nil
	}
	if dbBatch != nil {
		atomic.AddInt64(&regCtx.diff, dbBatch.size())
		err := store.writeDB(dbBatch)
		if err != nil {
			return errors.Trace(err)
		}
	}
	err := store.writeLocks(lockBatch)
	reqCtx.trace(eventEndWriteLock)
	return errors.Trace(err)
}

const delRangeBatchSize = 4096

func (store *MVCCStore) DeleteRange(reqCtx *requestCtx, startKey, endKey []byte) error {
	keys := make([][]byte, 0, delRangeBatchSize)
	oldStartKey := encodeOldKey(startKey, maxSystemTS)
	oldEndKey := encodeOldKey(endKey, maxSystemTS)
	reader := reqCtx.getDBReader()
	keys = store.collectRangeKeys(reader.getIter(), startKey, endKey, keys)
	keys = store.collectRangeKeys(reader.getIter(), oldStartKey, oldEndKey, keys)
	reqCtx.trace(eventReadDB)
	err := store.deleteKeysInBatch(reqCtx, keys, delRangeBatchSize)
	if err != nil {
		log.Error(err)
	}
	return errors.Trace(err)
}

func (store *MVCCStore) collectRangeKeys(it *badger.Iterator, startKey, endKey []byte, keys [][]byte) [][]byte {
	for it.Seek(startKey); it.Valid(); it.Next() {
		item := it.Item()
		key := item.KeyCopy(nil)
		if exceedEndKey(key, endKey) {
			break
		}
		keys = append(keys, key)
		if len(keys) == delRangeBatchSize {
			break
		}
	}
	return keys
}

func (store *MVCCStore) deleteKeysInBatch(reqCtx *requestCtx, keys [][]byte, batchSize int) error {
	regCtx := reqCtx.regCtx
	for len(keys) > 0 {
		batchSize := mathutil.Min(len(keys), batchSize)
		batchKeys := keys[:batchSize]
		keys = keys[batchSize:]
		hashVals := keysToHashVals(batchKeys...)
		dbBatch := newWriteDBBatch(reqCtx)
		for _, key := range batchKeys {
			dbBatch.delete(key)
		}

		regCtx.acquireLatches(hashVals)
		err := store.writeDB(dbBatch)
		regCtx.releaseLatches(hashVals)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (store *MVCCStore) GC(reqCtx *requestCtx, safePoint uint64) error {
	// TODO: implement GC in badger.
	return nil
}
