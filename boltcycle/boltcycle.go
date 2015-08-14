package boltcycle

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/boltdb/bolt"
)

// CycleDB allows you to use a bolt.DB as a pseudo-LRU using a cycle of buckets
type CycleDB struct {
	// db is the bolt database values are stored into
	db *bolt.DB

	// bucketTimesIn is the name of the bucket we are putting our rotating values in
	bucketTimesIn []byte

	// minNumOldBuckets ensures you never delete an old bucket during a cycle if you have fewer than
	// these number of buckets
	minNumOldBuckets int
	// Size of read moves to batch into a single transaction
	maxBatchSize int

	// Chan controls backlog of read moves
	readMovements chan readToLocation
	// How large the readMovements chan is when created
	readMovementBacklog int
	// log of errors
	asyncErrors chan<- error

	// wg controls waiting for the read movement loop
	wg sync.WaitGroup
	// stats records useful operation information for reporting back out by the user
	stats Stats
}

// Stats are exported by CycleDB to let users inspect its behavior over time
type Stats struct {
	TotalItemsRecopied            int64
	RecopyTransactionCount        int64
	TotalItemsDeletedDuringRecopy int64
	TotalReadCount                int64
	TotalWriteCount               int64
	TotalDeleteCount              int64
	TotalCycleCount               int64
	TotalErrorsDuringRecopy       int64
	SizeOfBacklogToCopy           int
}

func (s *Stats) atomicClone() Stats {
	return Stats{
		TotalItemsRecopied:            atomic.LoadInt64(&s.TotalItemsRecopied),
		RecopyTransactionCount:        atomic.LoadInt64(&s.RecopyTransactionCount),
		TotalItemsDeletedDuringRecopy: atomic.LoadInt64(&s.TotalItemsDeletedDuringRecopy),
		TotalReadCount:                atomic.LoadInt64(&s.TotalReadCount),
		TotalWriteCount:               atomic.LoadInt64(&s.TotalWriteCount),
		TotalDeleteCount:              atomic.LoadInt64(&s.TotalDeleteCount),
		TotalCycleCount:               atomic.LoadInt64(&s.TotalCycleCount),
		TotalErrorsDuringRecopy:       atomic.LoadInt64(&s.TotalErrorsDuringRecopy),
	}
}

var errUnableToFindRootBucket = errors.New("unable to find root bucket")
var errUnexpectedBucketBytes = errors.New("bucket bytes not in uint64 form")
var errUnexpectedNonBucket = errors.New("unexpected non bucket")
var errNoLastBucket = errors.New("unable to find a last bucket")
var errOrderingWrong = errors.New("ordering wrong")

// KvPair is a pair of key/value that you want to write during a write call
type KvPair struct {
	// Key to write
	Key []byte
	// Value to write for key
	Value []byte
}

var defaultBucketName = []byte("cyc")

// DBConfiguration are callbacks used as optional vardic parameters in New() to configure DB usage
type DBConfiguration func(*CycleDB) error

// CycleLen sets the number of old buckets to keep around
func CycleLen(minNumOldBuckets int) DBConfiguration {
	return func(c *CycleDB) error {
		c.minNumOldBuckets = minNumOldBuckets
		return nil
	}
}

// ReadMovementBacklog sets the size of the channel of read operations to rewrite
func ReadMovementBacklog(readMovementBacklog int) DBConfiguration {
	return func(c *CycleDB) error {
		c.readMovementBacklog = readMovementBacklog
		return nil
	}
}

// AsyncErrors controls where we log async errors into.  If nil, they are silently dropped
func AsyncErrors(asyncErrors chan<- error) DBConfiguration {
	return func(c *CycleDB) error {
		c.asyncErrors = asyncErrors
		return nil
	}
}

// BucketTimesIn is the sub bucket we put our cycled hashmap into
func BucketTimesIn(bucketName []byte) DBConfiguration {
	return func(c *CycleDB) error {
		c.bucketTimesIn = bucketName
		return nil
	}
}

// New creates a CycleDB to use a bolt database that cycles minNumOldBuckets buckets
func New(db *bolt.DB, optionalParameters ...DBConfiguration) (*CycleDB, error) {
	ret := &CycleDB{
		db:                  db,
		bucketTimesIn:       defaultBucketName,
		minNumOldBuckets:    2,
		maxBatchSize:        1000,
		readMovementBacklog: 10000,
	}
	for _, config := range optionalParameters {
		if err := config(ret); err != nil {
			return nil, err
		}
	}
	if err := ret.init(); err != nil {
		return ret, err
	}
	if !db.IsReadOnly() {
		ret.wg.Add(1)
		ret.readMovements = make(chan readToLocation, ret.readMovementBacklog)
		go ret.readMovementLoop()
	}
	return ret, nil
}

// Stats returns introspection stats about the Database.  The members are considered alpha and
// subject to change or rename.
func (c *CycleDB) Stats() Stats {
	ret := c.stats.atomicClone()
	ret.SizeOfBacklogToCopy = len(c.readMovements)
	return ret
}

// Close ends the goroutine that moves read items to the latest bucket
func (c *CycleDB) Close() error {
	if !c.db.IsReadOnly() {
		close(c.readMovements)
	}
	c.wg.Wait()
	return nil
}

type stringCursor struct {
	cursor *bolt.Cursor
	head   string
}

type cursorHeap []stringCursor

func (c cursorHeap) Len() int {
	return len(c)
}

func (c cursorHeap) Less(i, j int) bool {
	return c[i].head < c[j].head
}

func (c cursorHeap) Swap(i, j int) {
	c[i], c[j] = c[j], c[i]
}

func (c *cursorHeap) Push(x interface{}) {
	item := x.(stringCursor)
	*c = append(*c, item)
}

func (c *cursorHeap) Pop() interface{} {
	n := len(*c)
	item := (*c)[n-1]
	*c = (*c)[0 : n-1]
	return item
}

var _ heap.Interface = &cursorHeap{}

func (c *CycleDB) init() error {
	if c.db.IsReadOnly() {
		return nil
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(c.bucketTimesIn)
		if err != nil {
			return err
		}
		// If there is no bucket at all, make a first bucket at key=[0,0,0,0, 0,0,0,0]
		if k, _ := bucket.Cursor().First(); k == nil {
			var b [8]byte
			_, err := bucket.CreateBucket(b[:])
			return err
		}
		return nil
	})
}

// VerifyBuckets ensures that the cycle of buckets have the correct names (increasing 8 byte integers)
func (c *CycleDB) VerifyBuckets() error {
	return c.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(c.bucketTimesIn)
		if bucket == nil {
			return errUnableToFindRootBucket
		}

		// Each bucket should be 8 bytes of different uint64
		return bucket.ForEach(func(k, v []byte) error {
			if v != nil {
				return errUnexpectedNonBucket
			}
			if len(k) != 8 {
				return errUnexpectedBucketBytes
			}
			return nil
		})
	})
}

func createHeap(bucket *bolt.Bucket) (cursorHeap, error) {
	var ch cursorHeap
	// Each bucket should be 8 bytes of different uint64
	err := bucket.ForEach(func(k, v []byte) error {
		cursor := bucket.Bucket(k).Cursor()
		firstKey, _ := cursor.First()
		if firstKey != nil {
			ch = append(ch, stringCursor{cursor: cursor, head: string(firstKey)})
		}
		return nil
	})
	return ch, err
}

func verifyHeap(ch cursorHeap) error {
	top := ""
	heap.Init(&ch)
	for len(ch) > 0 {
		nextTop := ch[0].head
		if top != "" && nextTop <= top {
			return errOrderingWrong
		}
		top = nextTop
		headBytes, _ := ch[0].cursor.Next()
		if headBytes == nil {
			heap.Pop(&ch)
		} else {
			ch[0].head = string(headBytes)
			heap.Fix(&ch, 0)
		}
	}
	return nil
}

var createHeapFunc = createHeap

// VerifyCompressed checks that no key is repeated in the database
func (c *CycleDB) VerifyCompressed() error {
	return c.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(c.bucketTimesIn)
		if bucket == nil {
			return errUnableToFindRootBucket
		}

		ch, err := createHeapFunc(bucket)
		if err != nil {
			return err
		}
		return verifyHeap(ch)
	})
}

// CycleNodes deletes the first, oldest node in the primary bucket while there are >= minNumOldBuckets
// and creates a new, empty last node
func (c *CycleDB) CycleNodes() error {
	atomic.AddInt64(&c.stats.TotalCycleCount, int64(1))
	return c.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(c.bucketTimesIn)
		if bucket == nil {
			return errUnableToFindRootBucket
		}

		countBuckets := func() int {
			num := 0
			cursor := bucket.Cursor()
			for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
				num++
			}
			return num
		}()

		cursor := bucket.Cursor()
		for k, _ := cursor.First(); k != nil && countBuckets > c.minNumOldBuckets; k, _ = cursor.Next() {
			if err := bucket.DeleteBucket(k); err != nil {
				return err
			}
			countBuckets--
		}

		lastBucket, _ := cursor.Last()
		nextBucketName := nextKey(lastBucket)
		_, err := bucket.CreateBucket(nextBucketName)

		return err
	})
}

func nextKey(last []byte) []byte {
	lastNum := binary.BigEndian.Uint64(last)
	var ret [8]byte
	binary.BigEndian.PutUint64(ret[:], lastNum+1)
	return ret[:]
}

type readToLocation struct {
	// bucket we found the []byte key in
	bucket uint64
	// Key we searched for
	key []byte
	// Value we found for the key, or nil of it wasn't found
	value []byte
	// needsCopy is true if we detected this item needs to be copied to the last bucket
	needsCopy bool
}

func (c *CycleDB) readMovementLoop() {
	defer c.wg.Done()
	for {
		allMovements := drainAllMovements(c.readMovements, c.maxBatchSize)
		if allMovements == nil {
			return
		}
		if err := c.moveRecentReads(allMovements); err != nil {
			atomic.AddInt64(&c.stats.TotalErrorsDuringRecopy, 1)
			if c.asyncErrors != nil {
				c.asyncErrors <- err
			}
		}
	}
}

func drainAllMovements(readMovements <-chan readToLocation, maxBatchSize int) []readToLocation {
	allMovements := make([]readToLocation, 0, maxBatchSize)
	var rm readToLocation
	var ok bool
	if rm, ok = <-readMovements; !ok {
		return nil
	}
	allMovements = append(allMovements, rm)

	for len(allMovements) < maxBatchSize {
		select {
		case rm, ok := <-readMovements:
			if !ok {
				return allMovements
			}
			allMovements = append(allMovements, rm)
		default:
			return allMovements
		}
	}
	return allMovements
}

func (c *CycleDB) indexToLocation(toread [][]byte) ([]readToLocation, error) {
	res := make([]readToLocation, len(toread))

	indexesToFetch := make(map[int][]byte, len(toread))
	for i, bytes := range toread {
		indexesToFetch[i] = bytes
	}

	err := c.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(c.bucketTimesIn)
		if bucket == nil {
			return errUnableToFindRootBucket
		}
		timeCursor := bucket.Cursor()
		needsCopy := false

		// We read values from the end to the start.  The last bucket is where we expect a read
		// heavy workload to have the key
		for lastKey, _ := timeCursor.Last(); lastKey != nil && len(indexesToFetch) > 0; lastKey, _ = timeCursor.Prev() {

			// All subkeys of our tree should be buckets
			timeBucket := bucket.Bucket(lastKey)
			if timeBucket == nil {
				return errUnexpectedNonBucket
			}
			bucketAsUint := binary.BigEndian.Uint64(lastKey)

			timeBucketCursor := timeBucket.Cursor()
			for index, searchBytes := range indexesToFetch {
				key, value := timeBucketCursor.Seek(searchBytes)
				if key == nil {
					continue
				}

				if bytes.Equal(key, searchBytes) {
					res[index].key = searchBytes
					res[index].value = make([]byte, len(value))
					// Note: The returned value is only valid for the lifetime of the transaction so
					//       we must copy it out
					copy(res[index].value, value)
					res[index].bucket = bucketAsUint
					res[index].needsCopy = needsCopy

					// We can remove this item since we don't need to search for it later
					delete(indexesToFetch, index)
				}
			}
			needsCopy = true
		}
		return nil
	})
	return res, err
}

func (c *CycleDB) moveRecentReads(readLocations []readToLocation) error {
	bucketIDToReadLocations := make(map[uint64][]readToLocation)
	for _, r := range readLocations {
		bucketIDToReadLocations[r.bucket] = append(bucketIDToReadLocations[r.bucket], r)
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		atomic.AddInt64(&c.stats.RecopyTransactionCount, int64(1))
		bucket := tx.Bucket(c.bucketTimesIn)
		if bucket == nil {
			return errUnableToFindRootBucket
		}
		lastBucketKey, _ := bucket.Cursor().Last()
		if lastBucketKey == nil {
			return errNoLastBucket
		}
		lastBucket := bucket.Bucket(lastBucketKey)
		if lastBucket == nil {
			return errNoLastBucket
		}

		recopyCount := int64(0)
		deletedCount := int64(0)
		for bucketID, readLocs := range bucketIDToReadLocations {
			var bucketName [8]byte
			binary.BigEndian.PutUint64(bucketName[:], bucketID)
			oldBucket := bucket.Bucket(bucketName[:])
			if oldBucket != nil {
				oldBucketCursor := oldBucket.Cursor()
				for _, rs := range readLocs {
					recopyCount++
					var wasDeleted bool
					var err error
					if wasDeleted, err = cleanupBuckets(oldBucketCursor, lastBucket, rs); err != nil {
						return err
					}
					if wasDeleted {
						deletedCount++
					}
				}
			}
		}
		atomic.AddInt64(&c.stats.TotalItemsRecopied, recopyCount)
		atomic.AddInt64(&c.stats.TotalItemsDeletedDuringRecopy, deletedCount)
		return nil
	})
}

var cleanupBuckets = cleanupBucketsFunc

func cleanupBucketsFunc(oldBucketCursor *bolt.Cursor, lastBucket *bolt.Bucket, readLoc readToLocation) (bool, error) {
	k, _ := oldBucketCursor.Seek(readLoc.key)
	wasDeleted := false
	if bytes.Equal(k, readLoc.key) {
		if err := oldBucketCursor.Delete(); err != nil {
			return false, err
		}
		wasDeleted = true
	}
	return wasDeleted, lastBucket.Put(readLoc.key, readLoc.value)
}

// Read bytes from the first available bucket
func (c *CycleDB) Read(toread [][]byte) ([][]byte, error) {
	atomic.AddInt64(&c.stats.TotalReadCount, int64(len(toread)))
	readLocations, err := c.indexToLocation(toread)
	if err != nil {
		return nil, err
	}

	if !c.db.IsReadOnly() {
		for _, readLocation := range readLocations {
			if readLocation.needsCopy {
				c.readMovements <- readLocation
			}
		}
	}

	res := make([][]byte, len(readLocations))
	for i, rl := range readLocations {
		res[i] = rl.value
	}
	return res, nil
}

func (c *CycleDB) Write(towrite []KvPair) error {
	atomic.AddInt64(&c.stats.TotalWriteCount, int64(len(towrite)))
	return c.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(c.bucketTimesIn)
		if bucket == nil {
			return errUnableToFindRootBucket
		}
		lastBucketKey, _ := bucket.Cursor().Last()
		if lastBucketKey == nil {
			return errNoLastBucket
		}
		lastBucket := bucket.Bucket(lastBucketKey)
		if lastBucket == nil {
			return errNoLastBucket
		}
		for _, p := range towrite {
			if err := lastBucket.Put(p.Key, p.Value); err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete all the keys from every bucket that could have the keys.  Returns true/false for each key
// if it exists
func (c *CycleDB) Delete(keys [][]byte) ([]bool, error) {
	atomic.AddInt64(&c.stats.TotalDeleteCount, int64(len(keys)))
	ret := make([]bool, len(keys))
	return ret, c.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(c.bucketTimesIn)
		if bucket == nil {
			return errUnableToFindRootBucket
		}
		return bucket.ForEach(func(k, v []byte) error {
			innerBucket := bucket.Bucket(k)
			if innerBucket == nil {
				return errUnexpectedNonBucket
			}
			cursor := innerBucket.Cursor()
			return deleteKeys(keys, cursor, ret)
		})
	})
}

func deleteKeys(keys [][]byte, cursor *bolt.Cursor, ret []bool) error {
	for index, key := range keys {
		k, _ := cursor.Seek(key)
		if bytes.Equal(k, key) {
			if err := cursor.Delete(); err != nil {
				return err
			}
			ret[index] = true
		}
	}
	return nil
}
