package storage

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/filestream"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fs"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

// blockStreamWriter represents block stream writer.
type blockStreamWriter struct {
	compressLevel int
	path          string

	// Use io.Writer type for timestampsWriter and valuesWriter
	// in order to remove I2I conversion in WriteExternalBlock
	// when passing them to fs.MustWriteData
	timestampsWriter io.Writer
	valuesWriter     io.Writer

	indexWriter     filestream.WriteCloser
	metaindexWriter filestream.WriteCloser

	mr metaindexRow

	timestampsBlockOffset uint64
	valuesBlockOffset     uint64
	indexBlockOffset      uint64

	indexData           []byte
	compressedIndexData []byte

	metaindexData           []byte
	compressedMetaindexData []byte

	// prevTimestamps* is used as an optimization for reducing disk space usage
	// when serially written blocks have identical timestamps.
	// This is usually the case when adjacent blocks contain metrics scraped from the same target,
	// since such metrics have identical timestamps.
	prevTimestampsData        []byte
	prevTimestampsBlockOffset uint64
}

func (bsw *blockStreamWriter) assertWriteClosers() {
	_ = bsw.timestampsWriter.(filestream.WriteCloser)
	_ = bsw.valuesWriter.(filestream.WriteCloser)
}

// Init initializes bsw with the given writers.
func (bsw *blockStreamWriter) reset() {
	bsw.compressLevel = 0
	bsw.path = ""

	bsw.timestampsWriter = nil
	bsw.valuesWriter = nil
	bsw.indexWriter = nil
	bsw.metaindexWriter = nil

	bsw.mr.Reset()

	bsw.timestampsBlockOffset = 0
	bsw.valuesBlockOffset = 0
	bsw.indexBlockOffset = 0

	bsw.indexData = bsw.indexData[:0]
	bsw.compressedIndexData = bsw.compressedIndexData[:0]

	bsw.metaindexData = bsw.metaindexData[:0]
	bsw.compressedMetaindexData = bsw.compressedMetaindexData[:0]

	bsw.prevTimestampsData = bsw.prevTimestampsData[:0]
	bsw.prevTimestampsBlockOffset = 0
}

// InitFromInmemoryPart initializes bsw from inmemory part.
func (bsw *blockStreamWriter) InitFromInmemoryPart(mp *inmemoryPart, compressLevel int) {
	bsw.reset()

	bsw.compressLevel = compressLevel
	bsw.timestampsWriter = &mp.timestampsData
	bsw.valuesWriter = &mp.valuesData
	bsw.indexWriter = &mp.indexData
	bsw.metaindexWriter = &mp.metaindexData

	bsw.assertWriteClosers()
}

// InitFromFilePart initializes bsw from a file-based part on the given path.
//
// The bsw doesn't pollute OS page cache if nocache is set.
func (bsw *blockStreamWriter) InitFromFilePart(path string, nocache bool, compressLevel int) error {
	path = filepath.Clean(path)

	// Create the directory
	if err := fs.MkdirAllFailIfExist(path); err != nil {
		return fmt.Errorf("cannot create directory %q: %w", path, err)
	}

	// Create part files in the directory.
	timestampsPath := filepath.Join(path, timestampsFilename)
	timestampsFile, err := filestream.Create(timestampsPath, nocache)
	if err != nil {
		fs.MustRemoveDirAtomic(path)
		return fmt.Errorf("cannot create timestamps file: %w", err)
	}

	valuesPath := filepath.Join(path, valuesFilename)
	valuesFile, err := filestream.Create(valuesPath, nocache)
	if err != nil {
		timestampsFile.MustClose()
		fs.MustRemoveDirAtomic(path)
		return fmt.Errorf("cannot create values file: %w", err)
	}

	indexPath := filepath.Join(path, indexFilename)
	indexFile, err := filestream.Create(indexPath, nocache)
	if err != nil {
		timestampsFile.MustClose()
		valuesFile.MustClose()
		fs.MustRemoveDirAtomic(path)
		return fmt.Errorf("cannot create index file: %w", err)
	}

	// Always cache metaindex file in OS page cache, since it is immediately
	// read after the merge.
	metaindexPath := filepath.Join(path, metaindexFilename)
	metaindexFile, err := filestream.Create(metaindexPath, false)
	if err != nil {
		timestampsFile.MustClose()
		valuesFile.MustClose()
		indexFile.MustClose()
		fs.MustRemoveDirAtomic(path)
		return fmt.Errorf("cannot create metaindex file: %w", err)
	}

	bsw.reset()
	bsw.compressLevel = compressLevel
	bsw.path = path

	bsw.timestampsWriter = timestampsFile
	bsw.valuesWriter = valuesFile
	bsw.indexWriter = indexFile
	bsw.metaindexWriter = metaindexFile

	bsw.assertWriteClosers()

	return nil
}

// MustClose closes the bsw.
//
// It closes *Writer files passed to Init*.
func (bsw *blockStreamWriter) MustClose() {
	// Flush remaining data.
	bsw.flushIndexData()

	// Write metaindex data.
	bsw.compressedMetaindexData = encoding.CompressZSTDLevel(bsw.compressedMetaindexData[:0], bsw.metaindexData, bsw.compressLevel)
	fs.MustWriteData(bsw.metaindexWriter, bsw.compressedMetaindexData)

	// Close writers.
	bsw.timestampsWriter.(filestream.WriteCloser).MustClose()
	bsw.valuesWriter.(filestream.WriteCloser).MustClose()
	bsw.indexWriter.MustClose()
	bsw.metaindexWriter.MustClose()

	// Sync bsw.path contents to make sure it doesn't disappear
	// after system crash or power loss.
	if bsw.path != "" {
		fs.MustSyncPath(bsw.path)
	}

	bsw.reset()
}

// WriteExternalBlock writes b to bsw and updates ph and rowsMerged.
func (bsw *blockStreamWriter) WriteExternalBlock(b *Block, ph *partHeader, rowsMerged *uint64) {
	atomic.AddUint64(rowsMerged, uint64(b.rowsCount()))
	b.deduplicateSamplesDuringMerge()
	headerData, timestampsData, valuesData := b.MarshalData(bsw.timestampsBlockOffset, bsw.valuesBlockOffset)
	usePrevTimestamps := len(bsw.prevTimestampsData) > 0 && bytes.Equal(timestampsData, bsw.prevTimestampsData)
	if usePrevTimestamps {
		// The current timestamps block equals to the previous timestamps block.
		// Update headerData so it points to the previous timestamps block. This saves disk space.
		headerData, timestampsData, valuesData = b.MarshalData(bsw.prevTimestampsBlockOffset, bsw.valuesBlockOffset)
		atomic.AddUint64(&timestampsBlocksMerged, 1)
		atomic.AddUint64(&timestampsBytesSaved, uint64(len(timestampsData)))
	}
	bsw.indexData = append(bsw.indexData, headerData...)
	bsw.mr.RegisterBlockHeader(&b.bh)
	if len(bsw.indexData) >= maxBlockSize {
		bsw.flushIndexData()
	}
	if !usePrevTimestamps {
		bsw.prevTimestampsData = append(bsw.prevTimestampsData[:0], timestampsData...)
		bsw.prevTimestampsBlockOffset = bsw.timestampsBlockOffset
		fs.MustWriteData(bsw.timestampsWriter, timestampsData)
		bsw.timestampsBlockOffset += uint64(len(timestampsData))
	}
	fs.MustWriteData(bsw.valuesWriter, valuesData)
	bsw.valuesBlockOffset += uint64(len(valuesData))
	updatePartHeader(b, ph)
}

var (
	timestampsBlocksMerged uint64
	timestampsBytesSaved   uint64
)

func updatePartHeader(b *Block, ph *partHeader) {
	ph.BlocksCount++
	ph.RowsCount += uint64(b.bh.RowsCount)
	if b.bh.MinTimestamp < ph.MinTimestamp {
		ph.MinTimestamp = b.bh.MinTimestamp
	}
	if b.bh.MaxTimestamp > ph.MaxTimestamp {
		ph.MaxTimestamp = b.bh.MaxTimestamp
	}
}

func (bsw *blockStreamWriter) flushIndexData() {
	if len(bsw.indexData) == 0 {
		return
	}

	// Write compressed index block to index data.
	bsw.compressedIndexData = encoding.CompressZSTDLevel(bsw.compressedIndexData[:0], bsw.indexData, bsw.compressLevel)
	indexBlockSize := len(bsw.compressedIndexData)
	if uint64(indexBlockSize) >= 1<<32 {
		logger.Panicf("BUG: indexBlock size must fit uint32; got %d", indexBlockSize)
	}
	fs.MustWriteData(bsw.indexWriter, bsw.compressedIndexData)

	// Write metaindex row to metaindex data.
	bsw.mr.IndexBlockOffset = bsw.indexBlockOffset
	bsw.mr.IndexBlockSize = uint32(indexBlockSize)
	bsw.metaindexData = bsw.mr.Marshal(bsw.metaindexData)

	// Update offsets.
	bsw.indexBlockOffset += uint64(indexBlockSize)

	bsw.indexData = bsw.indexData[:0]
	bsw.mr.Reset()
}

func getBlockStreamWriter() *blockStreamWriter {
	v := bswPool.Get()
	if v == nil {
		return &blockStreamWriter{}
	}
	return v.(*blockStreamWriter)
}

func putBlockStreamWriter(bsw *blockStreamWriter) {
	bsw.reset()
	bswPool.Put(bsw)
}

var bswPool sync.Pool
