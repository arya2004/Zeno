package index

import (
	"encoding/gob"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var dumpFrequency = 60 // seconds
var walFileOpenFlags int

type Operation int

const (
	OpAdd Operation = iota
	OpPop
)

type WALEntry struct {
	Op       Operation
	Host     string
	BlobID   string
	Position uint64
	Size     uint64
}

type IndexManager struct {
	sync.Mutex
	hostIndex    *Index
	indexFile    *os.File
	queueDirPath string
	indexEncoder *gob.Encoder
	indexDecoder *gob.Decoder
	dumpTicker   *time.Ticker
	lastDumpTime time.Time
	opsSinceDump int
	totalOps     uint64

	// WAL
	walFile    *os.File
	walEncoder *gob.Encoder
	walDecoder *gob.Decoder

	// WAL commit
	useCommit   bool
	walCommit   *atomic.Uint64 // Flying in memory commit id
	walCommited *atomic.Uint64 // Synced to disk commit id
	// Number of listeners waiting for walCommitedNotify.
	// It must be accurate, otherwise walNotifyListeners will be blocked
	walNotifyListeners *atomic.Int64
	walCommitedNotify  chan uint64   // receives the commited id from walCommitsSyncer
	walSyncerRunning   atomic.Bool   // used to prevent multiple walCommitsSyncer running,
	walStopChan        chan struct{} // Syncer will close this channel after stopping
	WalIoPercent       int           // [1, 100] limit max io percentage for WAL sync
	WalMinInterval     time.Duration // minimum interval **between** between after-sync and next sync
	stopChan           chan struct{}
}

// NewIndexManager creates a new IndexManager instance and loads the index from the index file.
func NewIndexManager(walPath, indexPath, queueDirPath string, useCommit bool) (*IndexManager, error) {
	if useCommit {
		walFileOpenFlags = os.O_APPEND | os.O_RDWR
	} else {
		walFileOpenFlags = os.O_APPEND | os.O_RDWR | os.O_SYNC
	}

	walFile, err := os.OpenFile(walPath, os.O_CREATE|walFileOpenFlags, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open WAL file: %w", err)
	}

	indexFile, err := os.OpenFile(indexPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		walFile.Close()
		return nil, fmt.Errorf("failed to open index file: %w", err)
	}

	im := &IndexManager{
		hostIndex:    newIndex(),
		walFile:      walFile,
		indexFile:    indexFile,
		queueDirPath: queueDirPath,
		walEncoder:   gob.NewEncoder(walFile),
		walDecoder:   gob.NewDecoder(walFile),
		indexEncoder: gob.NewEncoder(indexFile),
		indexDecoder: gob.NewDecoder(indexFile),
		dumpTicker:   time.NewTicker(time.Duration(dumpFrequency) * time.Second),
		lastDumpTime: time.Now(),
		useCommit:    useCommit,
	}

	// Init WAL commit if enabled
	if useCommit {
		im.walCommit = new(atomic.Uint64)
		im.walCommited = new(atomic.Uint64)
		im.walNotifyListeners = new(atomic.Int64)
		im.walCommitedNotify = make(chan uint64)
		im.WalIoPercent = 10
		im.WalMinInterval = 10 * time.Millisecond
		im.walStopChan = make(chan struct{})
		im.stopChan = make(chan struct{})
	}

	// Check if WAL file is empty
	im.Lock()
	empty, err := im.unsafeIsWALEmpty() // FIXME: check error
	if err != nil {
		walFile.Close()
		indexFile.Close()
		return nil, fmt.Errorf("failed to check if WAL is empty: %w", err)
	}
	im.Unlock()
	if !empty {
		err := im.RecoverFromCrash()
		if err != nil {
			walFile.Close()
			indexFile.Close()
			return nil, fmt.Errorf("failed to recover from crash: %w", err)
		}
		fmt.Println("Recovered from crash")
	} else {
		err = im.loadIndex()
		if err != nil {
			walFile.Close()
			indexFile.Close()
			return nil, fmt.Errorf("failed to load index: %w", err)
		}
	}

	// Start the periodic dump goroutine
	periodicDumpStopChan := make(chan struct{})
	periodicDumpErrChan := make(chan error)
	go func(im *IndexManager, errChan chan error, stopChan chan struct{}) {
		for {
			select {
			case stop := <-im.stopChan:
				periodicDumpStopChan <- stop
				return
			case err := <-errChan:
				if err != nil {
					slog.Error("Periodic dump failed", "error", err) // No better way to log this, will wait for https://github.com/internetarchive/Zeno/issues/92
				}
			}
		}
	}(im, periodicDumpErrChan, periodicDumpStopChan)

	go im.periodicDump(periodicDumpErrChan, periodicDumpStopChan)
	if useCommit {
		go im.walCommitsSyncer()
	}

	return im, nil
}

func (im *IndexManager) unsafeWalSync() error {
	return im.walFile.Sync()
}

func (im *IndexManager) walCommitsSyncer() {
	if swaped := im.walSyncerRunning.CompareAndSwap(false, true); !swaped {
		slog.Warn("another walCommitsSyncer is running")
		return
	}
	defer im.walSyncerRunning.Store(false)
	defer close(im.walStopChan)

	if im.WalIoPercent < 1 || im.WalIoPercent > 100 {
		slog.Warn("invalid WAL_IO_PERCENT", "value", im.WalIoPercent, "setting to", 10)
		im.WalIoPercent = 10
	}

	lastTrySyncDuration := time.Duration(0)
	stopping := false
	for {
		// Check if we should stop
		if stopping {
			break
		}
		select {
		case <-im.walStopChan:
			slog.Info("walCommitsSyncer performing final sync before stopping")
			stopping = true
		default:
		}

		sleepTime := lastTrySyncDuration * time.Duration((100-im.WalIoPercent)/im.WalIoPercent)
		if sleepTime < im.WalMinInterval {
			sleepTime = im.WalMinInterval
		}
		slog.Debug("walCommitsSyncer sleeping", "sleepTime", sleepTime, "lastTrySyncDuration", lastTrySyncDuration)
		time.Sleep(sleepTime)

		start := time.Now()
		flyingCommit := im.walCommit.Load()
		im.Lock()
		err := im.unsafeWalSync()
		im.Unlock()
		lastTrySyncDuration = time.Since(start)
		if lastTrySyncDuration > 2*time.Second {
			slog.Warn("WAL sync took too long", "lastTrySyncDuration", lastTrySyncDuration)
		}
		if err != nil {
			if stopping {
				slog.Error("failed to sync WAL before stopping", "error", err)
				return // we are stopping, no need to retry
			}
			slog.Error("failed to sync WAL, retrying", "error", err)
			continue // we may infinitely retry, but it's better than losing data
		}
		commited := flyingCommit

		im.walCommited.Store(commited)

		// Clear notify channel before sending, just in case.
		// should never happen if listeners number is accurate.
		for len(im.walCommitedNotify) > 0 {
			<-im.walCommitedNotify
			slog.Warn("unconsumed commited id in walCommitedNotify")
		}

		// Send the commited id to all listeners
		listeners := im.walNotifyListeners.Load()
		for i := int64(0); i < listeners; i++ {
			im.walCommitedNotify <- commited
		}
	}
}

func (im *IndexManager) IsWALCommited(commit uint64) bool {
	return im.walCommited.Load() >= commit
}

// increments the WAL commit counter and returns the new commit ID.
func (im *IndexManager) WALCommit() uint64 {
	return im.walCommit.Add(1)
}

// AwaitWALCommitted blocks until the given commit ID is commited to disk by Syncer.
// DO NOT call this function with im.Lock() held, it will deadlock.
func (im *IndexManager) AwaitWALCommitted(commit uint64) {
	if commit == 0 {
		slog.Warn("AwaitWALCommited called with commit 0")
		return
	}
	if !im.walSyncerRunning.Load() {
		slog.Warn("AwaitWALCommited called without Syncer running, beaware of hanging")
	}
	if im.IsWALCommited(commit) {
		return
	}

	for {
		im.walNotifyListeners.Add(1)
		idFromChan := <-im.walCommitedNotify
		im.walNotifyListeners.Add(-1)

		if idFromChan >= commit {
			return
		}
	}
}

func (im *IndexManager) Add(host string, id string, position uint64, size uint64) (commit uint64, err error) {
	if !im.useCommit {
		return 0, im.add(host, id, position, size)
	}
	return im.addCommitted(host, id, position, size)
}

func (im *IndexManager) addCommitted(host string, id string, position uint64, size uint64) (commit uint64, err error) {
	im.Lock()
	defer im.Unlock()

	// Write to WAL
	err = im.unsafeWriteToWAL(OpAdd, host, id, position, size)
	if err != nil {
		return 0, fmt.Errorf("failed to write to WAL: %w", err)
	}

	commit = im.WALCommit()

	// Update in-memory index
	if err := im.hostIndex.add(host, id, position, size); err != nil {
		return commit, fmt.Errorf("failed to update in-memory index: %w", err)
	}

	im.opsSinceDump++
	im.totalOps++

	return commit, nil
}

func (im *IndexManager) add(host string, id string, position uint64, size uint64) error {
	im.Lock()
	defer im.Unlock()

	// Write to WAL
	err := im.unsafeWriteToWAL(OpAdd, host, id, position, size)
	if err != nil {
		return fmt.Errorf("failed to write to WAL: %w", err)
	}

	// Update in-memory index
	if err := im.hostIndex.add(host, id, position, size); err != nil {
		return fmt.Errorf("failed to update in-memory index: %w", err)
	}

	im.opsSinceDump++
	im.totalOps++

	return nil
}

// Pop removes the oldest blob from the specified host's queue and returns its ID, position, and size.
// Pop is responsible for synchronizing the pop of the blob from the in-memory index and writing to the WAL.
// First it starts a goroutine that waits for the to-be-popped blob infos through blobChan, then writes to the WAL and if successful
// informs index.pop() through WALSuccessChan to either continue as normal or return an error.
func (im *IndexManager) Pop(host string) (commit uint64, id string, position uint64, size uint64, err error) {
	if !im.useCommit {
		id, position, size, err = im.pop(host)
	} else {
		commit, id, position, size, err = im.popCommitted(host)
	}
	return
}

func (im *IndexManager) popCommitted(host string) (commit uint64, id string, position uint64, size uint64, err error) {
	im.Lock()
	defer im.Unlock()
	// Prepare the channels
	blobChan := make(chan *blob)
	WALSuccessChan := make(chan bool)
	errChan := make(chan error)
	defer close(blobChan)
	defer close(WALSuccessChan)
	defer close(errChan)

	go func() {
		// Write to WAL
		blob := <-blobChan
		// If the blob is nil, it means index.pop() returned an error
		if blob == nil {
			return
		}
		err := im.unsafeWriteToWAL(OpPop, host, blob.id, blob.position, blob.size)
		if err != nil {
			errChan <- fmt.Errorf("failed to write to WAL: %w", err)
			WALSuccessChan <- false
		}
		id = blob.id
		position = blob.position
		size = blob.size
		WALSuccessChan <- true
		errChan <- nil
	}()

	// Pop from in-memory index
	err = im.hostIndex.pop(host, blobChan, WALSuccessChan)
	if err != nil {
		return 0, "", 0, 0, err
	}

	if err := <-errChan; err != nil {
		return 0, "", 0, 0, err
	}

	commit = im.WALCommit()

	im.opsSinceDump++
	im.totalOps++

	return commit, id, position, size, nil
}

func (im *IndexManager) pop(host string) (id string, position uint64, size uint64, err error) {
	im.Lock()
	defer im.Unlock()

	// Prepare the channels
	blobChan := make(chan *blob)
	WALSuccessChan := make(chan bool)
	errChan := make(chan error)
	defer close(blobChan)
	defer close(WALSuccessChan)
	defer close(errChan)

	go func() {
		// Write to WAL
		blob := <-blobChan
		// If the blob is nil, it means index.pop() returned an error
		if blob == nil {
			return
		}
		err := im.unsafeWriteToWAL(OpPop, host, blob.id, blob.position, blob.size)
		if err != nil {
			errChan <- fmt.Errorf("failed to write to WAL: %w", err)
			WALSuccessChan <- false
		}
		id = blob.id
		position = blob.position
		size = blob.size
		WALSuccessChan <- true
		errChan <- nil
	}()

	// Pop from in-memory index
	err = im.hostIndex.pop(host, blobChan, WALSuccessChan)
	if err != nil {
		return "", 0, 0, err
	}

	if err := <-errChan; err != nil {
		return "", 0, 0, err
	}

	im.opsSinceDump++
	im.totalOps++

	return id, position, size, nil
}

// Close closes the index manager and performs a final dump of the index to disk.
func (im *IndexManager) Close() error {
	slog.Info("Closing index manager")
	defer slog.Info("Index manager closed")
	im.dumpTicker.Stop()
	im.stopChan <- struct{}{}
	im.walStopChan <- struct{}{}

	// wait for im.walStopChan to be closed by walCommitsSyncer
	<-im.walStopChan

	if err := im.performDump(); err != nil {
		return fmt.Errorf("failed to perform final dump: %w", err)
	}
	if err := im.walFile.Close(); err != nil {
		return fmt.Errorf("failed to close WAL file: %w", err)
	}
	if err := im.indexFile.Close(); err != nil {
		return fmt.Errorf("failed to close index file: %w", err)
	}
	if im.walSyncerRunning.Load() {
		return fmt.Errorf("walCommitsSyncer still running")
	}
	return nil
}

func (im *IndexManager) GetStats() string {
	im.Lock()
	defer im.Unlock()

	return fmt.Sprintf("Total operations: %d, Operations since last dump: %d",
		im.totalOps, im.opsSinceDump)
}

// GetHosts returns a list of all hosts in the index
func (im *IndexManager) GetHosts() []string {
	im.Lock()
	defer im.Unlock()

	return im.hostIndex.getOrderedHosts()
}

func (im *IndexManager) IsEmpty() bool {
	im.Lock()
	defer im.Unlock()

	im.hostIndex.Lock()
	defer im.hostIndex.Unlock()

	return len(im.hostIndex.index) == 0
}
