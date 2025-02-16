// Copyright 2019 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/humanize"
	"github.com/cockroachdb/pebble/record"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/redact"
	"github.com/prometheus/client_golang/prometheus"
)

// CacheMetrics holds metrics for the block and table cache.
type CacheMetrics = cache.Metrics

// FilterMetrics holds metrics for the filter policy
type FilterMetrics = sstable.FilterMetrics

// ThroughputMetric is a cumulative throughput metric. See the detailed
// comment in base.
type ThroughputMetric = base.ThroughputMetric

func formatCacheMetrics(w redact.SafePrinter, m *CacheMetrics, name redact.SafeString) {
	w.Printf("%7s %9s %7s %6.1f%%  (score == hit-rate)\n",
		name,
		humanize.SI.Int64(m.Count),
		humanize.IEC.Int64(m.Size),
		redact.Safe(hitRate(m.Hits, m.Misses)))
}

// LevelMetrics holds per-level metrics such as the number of files and total
// size of the files, and compaction related metrics.
type LevelMetrics struct {
	// The number of sublevels within the level. The sublevel count corresponds
	// to the read amplification for the level. An empty level will have a
	// sublevel count of 0, implying no read amplification. Only L0 will have
	// a sublevel count other than 0 or 1.
	Sublevels int32
	// The total number of files in the level.
	NumFiles int64
	// The total size in bytes of the files in the level.
	Size int64
	// The level's compaction score.
	Score float64
	// The number of incoming bytes from other levels read during
	// compactions. This excludes bytes moved and bytes ingested. For L0 this is
	// the bytes written to the WAL.
	BytesIn uint64
	// The number of bytes ingested. The sibling metric for tables is
	// TablesIngested.
	BytesIngested uint64
	// The number of bytes moved into the level by a "move" compaction. The
	// sibling metric for tables is TablesMoved.
	BytesMoved uint64
	// The number of bytes read for compactions at the level. This includes bytes
	// read from other levels (BytesIn), as well as bytes read for the level.
	BytesRead uint64
	// The number of bytes written during compactions. The sibling
	// metric for tables is TablesCompacted. This metric may be summed
	// with BytesFlushed to compute the total bytes written for the level.
	BytesCompacted uint64
	// The number of bytes written during flushes. The sibling
	// metrics for tables is TablesFlushed. This metric is always
	// zero for all levels other than L0.
	BytesFlushed uint64
	// The number of sstables compacted to this level.
	TablesCompacted uint64
	// The number of sstables flushed to this level.
	TablesFlushed uint64
	// The number of sstables ingested into the level.
	TablesIngested uint64
	// The number of sstables moved to this level by a "move" compaction.
	TablesMoved uint64
	// Additional contains misc additional metrics that are not always printed.
	Additional struct {
		// The sum of Properties.ValueBlocksSize for all the sstables in this
		// level. Printed by LevelMetrics.format iff there is at least one level
		// with a non-zero value.
		ValueBlocksSize uint64
		// Cumulative metrics about bytes written to data blocks and value blocks,
		// via compactions (except move compactions) or flushes. Not printed by
		// LevelMetrics.format, but are available to sophisticated clients.
		BytesWrittenDataBlocks  uint64
		BytesWrittenValueBlocks uint64
	}
}

// Add updates the counter metrics for the level.
func (m *LevelMetrics) Add(u *LevelMetrics) {
	m.NumFiles += u.NumFiles
	m.Size += u.Size
	m.BytesIn += u.BytesIn
	m.BytesIngested += u.BytesIngested
	m.BytesMoved += u.BytesMoved
	m.BytesRead += u.BytesRead
	m.BytesCompacted += u.BytesCompacted
	m.BytesFlushed += u.BytesFlushed
	m.TablesCompacted += u.TablesCompacted
	m.TablesFlushed += u.TablesFlushed
	m.TablesIngested += u.TablesIngested
	m.TablesMoved += u.TablesMoved
	m.Additional.BytesWrittenDataBlocks += u.Additional.BytesWrittenDataBlocks
	m.Additional.BytesWrittenValueBlocks += u.Additional.BytesWrittenValueBlocks
	m.Additional.ValueBlocksSize += u.Additional.ValueBlocksSize
}

// WriteAmp computes the write amplification for compactions at this
// level. Computed as (BytesFlushed + BytesCompacted) / BytesIn.
func (m *LevelMetrics) WriteAmp() float64 {
	if m.BytesIn == 0 {
		return 0
	}
	return float64(m.BytesFlushed+m.BytesCompacted) / float64(m.BytesIn)
}

// format generates a string of the receiver's metrics, formatting it into the
// supplied buffer.
func (m *LevelMetrics) format(
	w redact.SafePrinter, score redact.SafeValue, includeValueBlocksSize bool,
) {
	w.Printf("%9d %7s %7s %7s %7s %7s %7s %7s %7s %7s %7s %7d %7.1f",
		redact.Safe(m.NumFiles),
		humanize.IEC.Int64(m.Size),
		score,
		humanize.IEC.Uint64(m.BytesIn),
		humanize.IEC.Uint64(m.BytesIngested),
		humanize.SI.Uint64(m.TablesIngested),
		humanize.IEC.Uint64(m.BytesMoved),
		humanize.SI.Uint64(m.TablesMoved),
		humanize.IEC.Uint64(m.BytesFlushed+m.BytesCompacted),
		humanize.SI.Uint64(m.TablesFlushed+m.TablesCompacted),
		humanize.IEC.Uint64(m.BytesRead),
		redact.Safe(m.Sublevels),
		redact.Safe(m.WriteAmp()))
	if includeValueBlocksSize {
		w.Printf(" %7s\n", humanize.IEC.Uint64(m.Additional.ValueBlocksSize))
	} else {
		w.SafeString("\n")
	}
}

// Metrics holds metrics for various subsystems of the DB such as the Cache,
// Compactions, WAL, and per-Level metrics.
//
// TODO(peter): The testing of these metrics is relatively weak. There should
// be testing that performs various operations on a DB and verifies that the
// metrics reflect those operations.
type Metrics struct {
	BlockCache CacheMetrics

	Compact struct {
		// The total number of compactions, and per-compaction type counts.
		Count            int64
		DefaultCount     int64
		DeleteOnlyCount  int64
		ElisionOnlyCount int64
		MoveCount        int64
		ReadCount        int64
		RewriteCount     int64
		MultiLevelCount  int64
		// An estimate of the number of bytes that need to be compacted for the LSM
		// to reach a stable state.
		EstimatedDebt uint64
		// Number of bytes present in sstables being written by in-progress
		// compactions. This value will be zero if there are no in-progress
		// compactions.
		InProgressBytes int64
		// Number of compactions that are in-progress.
		NumInProgress int64
		// MarkedFiles is a count of files that are marked for
		// compaction. Such files are compacted in a rewrite compaction
		// when no other compactions are picked.
		MarkedFiles int
		// Duration records the cumulative duration of all compactions since the
		// database was opened.
		Duration time.Duration
	}

	Ingest struct {
		// The total number of ingestions
		Count uint64
	}

	Flush struct {
		// The total number of flushes.
		Count           int64
		WriteThroughput ThroughputMetric
		// Number of flushes that are in-progress. In the current implementation
		// this will always be zero or one.
		NumInProgress int64
		// AsIngestCount is a monotonically increasing counter of flush operations
		// handling ingested tables.
		AsIngestCount uint64
		// AsIngestCount is a monotonically increasing counter of tables ingested as
		// flushables.
		AsIngestTableCount uint64
		// AsIngestBytes is a monotonically increasing counter of the bytes flushed
		// for flushables that originated as ingestion operations.
		AsIngestBytes uint64
	}

	Filter FilterMetrics

	Levels [numLevels]LevelMetrics

	MemTable struct {
		// The number of bytes allocated by memtables and large (flushable)
		// batches.
		Size uint64
		// The count of memtables.
		Count int64
		// The number of bytes present in zombie memtables which are no longer
		// referenced by the current DB state but are still in use by an iterator.
		ZombieSize uint64
		// The count of zombie memtables.
		ZombieCount int64
	}

	Keys struct {
		// The approximate count of internal range key set keys in the database.
		RangeKeySetsCount uint64
		// The approximate count of internal tombstones (DEL, SINGLEDEL and
		// RANGEDEL key kinds) within the database.
		TombstoneCount uint64
	}

	Snapshots struct {
		// The number of currently open snapshots.
		Count int
		// The sequence number of the earliest, currently open snapshot.
		EarliestSeqNum uint64
		// A running tally of keys written to sstables during flushes or
		// compactions that would've been elided if it weren't for open
		// snapshots.
		PinnedKeys uint64
		// A running cumulative sum of the size of keys and values written to
		// sstables during flushes or compactions that would've been elided if
		// it weren't for open snapshots.
		PinnedSize uint64
	}

	Table struct {
		// The number of bytes present in obsolete tables which are no longer
		// referenced by the current DB state or any open iterators.
		ObsoleteSize uint64
		// The count of obsolete tables.
		ObsoleteCount int64
		// The number of bytes present in zombie tables which are no longer
		// referenced by the current DB state but are still in use by an iterator.
		ZombieSize uint64
		// The count of zombie tables.
		ZombieCount int64
	}

	TableCache CacheMetrics

	// Count of the number of open sstable iterators.
	TableIters int64
	// Uptime is the total time since this DB was opened.
	Uptime time.Duration

	WAL struct {
		// Number of live WAL files.
		Files int64
		// Number of obsolete WAL files.
		ObsoleteFiles int64
		// Physical size of the obsolete WAL files.
		ObsoletePhysicalSize uint64
		// Size of the live data in the WAL files. Note that with WAL file
		// recycling this is less than the actual on-disk size of the WAL files.
		Size uint64
		// Physical size of the WAL files on-disk. With WAL file recycling,
		// this is greater than the live data in WAL files.
		PhysicalSize uint64
		// Number of logical bytes written to the WAL.
		BytesIn uint64
		// Number of bytes written to the WAL.
		BytesWritten uint64
	}

	LogWriter struct {
		FsyncLatency prometheus.Histogram
		record.LogWriterMetrics
	}

	private struct {
		optionsFileSize  uint64
		manifestFileSize uint64
	}
}

var (
	// FsyncLatencyBuckets are prometheus histogram buckets suitable for a histogram
	// that records latencies for fsyncs.
	FsyncLatencyBuckets = append(
		prometheus.LinearBuckets(0.0, float64(time.Microsecond*100), 50),
		prometheus.ExponentialBucketsRange(float64(time.Millisecond*5), float64(10*time.Second), 50)...,
	)
)

// DiskSpaceUsage returns the total disk space used by the database in bytes,
// including live and obsolete files.
func (m *Metrics) DiskSpaceUsage() uint64 {
	var usageBytes uint64
	usageBytes += m.WAL.PhysicalSize
	usageBytes += m.WAL.ObsoletePhysicalSize
	for _, lm := range m.Levels {
		usageBytes += uint64(lm.Size)
	}
	usageBytes += m.Table.ObsoleteSize
	usageBytes += m.Table.ZombieSize
	usageBytes += m.private.optionsFileSize
	usageBytes += m.private.manifestFileSize
	usageBytes += uint64(m.Compact.InProgressBytes)
	return usageBytes
}

func (m *Metrics) levelSizes() [numLevels]int64 {
	var sizes [numLevels]int64
	for i := 0; i < len(sizes); i++ {
		sizes[i] = m.Levels[i].Size
	}
	return sizes
}

// ReadAmp returns the current read amplification of the database.
// It's computed as the number of sublevels in L0 + the number of non-empty
// levels below L0.
func (m *Metrics) ReadAmp() int {
	var ramp int32
	for _, l := range m.Levels {
		ramp += l.Sublevels
	}
	return int(ramp)
}

// Total returns the sum of the per-level metrics and WAL metrics.
func (m *Metrics) Total() LevelMetrics {
	var total LevelMetrics
	for level := 0; level < numLevels; level++ {
		l := &m.Levels[level]
		total.Add(l)
		total.Sublevels += l.Sublevels
	}
	// Compute total bytes-in as the bytes written to the WAL + bytes ingested.
	total.BytesIn = m.WAL.BytesWritten + total.BytesIngested
	// Add the total bytes-in to the total bytes-flushed. This is to account for
	// the bytes written to the log and bytes written externally and then
	// ingested.
	total.BytesFlushed += total.BytesIn
	return total
}

const notApplicable = redact.SafeString("-")

func (m *Metrics) formatWAL(w redact.SafePrinter) {
	var writeAmp float64
	if m.WAL.BytesIn > 0 {
		writeAmp = float64(m.WAL.BytesWritten) / float64(m.WAL.BytesIn)
	}
	w.Printf("    WAL %9d %7s %7s %7s %7s %7s %7s %7s %7s %7s %7s %7s %7.1f\n",
		redact.Safe(m.WAL.Files),
		humanize.Uint64(m.WAL.Size),
		notApplicable,
		humanize.Uint64(m.WAL.BytesIn),
		notApplicable,
		notApplicable,
		notApplicable,
		notApplicable,
		humanize.Uint64(m.WAL.BytesWritten),
		notApplicable,
		notApplicable,
		notApplicable,
		redact.Safe(writeAmp))
}

// String pretty-prints the metrics, showing a line for the WAL, a line per-level, and
// a total:
//
//	  __level_____count____size___score______in__ingest(sz_cnt)____move(sz_cnt)___write(sz_cnt)____read___r-amp___w-amp
//	    WAL         1    28 B       -    17 B       -       -       -       -    56 B       -       -       -     3.3
//	      0         1   770 B    0.25    28 B     0 B       0     0 B       0   770 B       1     0 B       1    27.5
//	      1         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
//	      2         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
//	      3         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
//	      4         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
//	      5         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
//	      6         0     0 B       -     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
//	  total         1   770 B       -    56 B     0 B       0     0 B       0   826 B       1     0 B       1    14.8
//	  flush         1                             0 B       0       0  (ingest = ingested-as-flushable, move = tables-ingested)
//	compact         0     0 B     0 B       0                          (size == estimated-debt, score = in-progress-bytes, in = num-in-progress)
//	  ctype         0       0       0       0       0       0       0  (default, delete, elision, move, read, rewrite, multi-level)
//	 memtbl         1   256 K
//	zmemtbl         1   256 K
//	   ztbl         0     0 B
//	 bcache         4   697 B    0.0%  (score == hit-rate)
//	 tcache         1   696 B    0.0%  (score == hit-rate)
//	  snaps         0       -       0  (score == earliest seq num)
//	 titers         1
//	 filter         -       -    0.0%  (score == utility)
//
// The WAL "in" metric is the size of the batches written to the WAL. The WAL
// "write" metric is the size of the physical data written to the WAL which
// includes record fragment overhead. Write amplification is computed as
// bytes-written / bytes-in, except for the total row where bytes-in is
// replaced with WAL-bytes-written + bytes-ingested.
func (m *Metrics) String() string {
	return redact.StringWithoutMarkers(m)
}

var _ redact.SafeFormatter = &Metrics{}

// SafeFormat implements redact.SafeFormatter.
func (m *Metrics) SafeFormat(w redact.SafePrinter, _ rune) {
	// NB: Pebble does not make any assumptions as to which Go primitive types
	// have been registered as safe with redact.RegisterSafeType and does not
	// register any types itself. Some of the calls to `redact.Safe`, etc are
	// superfluous in the context of CockroachDB, which registers all the Go
	// numeric types as safe.

	// TODO(jackson): There are a few places where we use redact.SafeValue
	// instead of redact.RedactableString. This is necessary because of a bug
	// whereby formatting a redact.RedactableString argument does not respect
	// width specifiers. When the issue is fixed, we can convert these to
	// RedactableStrings. https://github.com/cockroachdb/redact/issues/17

	haveValueBlocks := false
	var valueBlocksHeading redact.SafeString
	for level := 0; level < numLevels; level++ {
		if m.Levels[level].Additional.ValueBlocksSize > 0 {
			haveValueBlocks = true
			valueBlocksHeading = "__val-bl"
			break
		}
	}
	var total LevelMetrics
	w.Printf("__level_____count____size___score______in__ingest(sz_cnt)"+
		"____move(sz_cnt)___write(sz_cnt)____read___r-amp___w-amp%s\n", valueBlocksHeading)
	m.formatWAL(w)
	for level := 0; level < numLevels; level++ {
		l := &m.Levels[level]
		w.Printf("%7d ", redact.Safe(level))

		// Format the score.
		var score redact.SafeValue = notApplicable
		if level < numLevels-1 {
			score = redact.Safe(fmt.Sprintf("%0.2f", l.Score))
		}
		l.format(w, score, haveValueBlocks)
		total.Add(l)
		total.Sublevels += l.Sublevels
	}
	// Compute total bytes-in as the bytes written to the WAL + bytes ingested.
	total.BytesIn = m.WAL.BytesWritten + total.BytesIngested
	// Add the total bytes-in to the total bytes-flushed. This is to account for
	// the bytes written to the log and bytes written externally and then
	// ingested.
	total.BytesFlushed += total.BytesIn
	w.SafeString("  total ")
	total.format(w, notApplicable, haveValueBlocks)

	w.Printf("  flush %9d %31s %7d %7d  %s\n",
		redact.Safe(m.Flush.Count),
		humanize.IEC.Uint64(m.Flush.AsIngestBytes),
		redact.Safe(m.Flush.AsIngestTableCount),
		redact.Safe(m.Flush.AsIngestCount),
		redact.SafeString(`(ingest = tables-ingested, move = ingested-as-flushable)`))
	w.Printf("compact %9d %7s %7s %7d %s %s\n",
		redact.Safe(m.Compact.Count),
		humanize.IEC.Uint64(m.Compact.EstimatedDebt),
		humanize.IEC.Int64(m.Compact.InProgressBytes),
		redact.Safe(m.Compact.NumInProgress),
		redact.SafeString(strings.Repeat(" ", 24)),
		redact.SafeString(`(size == estimated-debt, score = in-progress-bytes, in = num-in-progress)`))
	w.Printf("  ctype %9d %7d %7d %7d %7d %7d %7d  %s\n",
		redact.Safe(m.Compact.DefaultCount),
		redact.Safe(m.Compact.DeleteOnlyCount),
		redact.Safe(m.Compact.ElisionOnlyCount),
		redact.Safe(m.Compact.MoveCount),
		redact.Safe(m.Compact.ReadCount),
		redact.Safe(m.Compact.RewriteCount),
		redact.Safe(m.Compact.MultiLevelCount),
		redact.SafeString(`(default, delete, elision, move, read, rewrite, multi-level)`))
	w.Printf(" memtbl %9d %7s\n",
		redact.Safe(m.MemTable.Count),
		humanize.IEC.Uint64(m.MemTable.Size))
	w.Printf("zmemtbl %9d %7s\n",
		redact.Safe(m.MemTable.ZombieCount),
		humanize.IEC.Uint64(m.MemTable.ZombieSize))
	w.Printf("   ztbl %9d %7s\n",
		redact.Safe(m.Table.ZombieCount),
		humanize.IEC.Uint64(m.Table.ZombieSize))
	formatCacheMetrics(w, &m.BlockCache, "bcache")
	formatCacheMetrics(w, &m.TableCache, "tcache")
	w.Printf("  snaps %9d %7s %7d  (score == earliest seq num)\n",
		redact.Safe(m.Snapshots.Count),
		notApplicable,
		redact.Safe(m.Snapshots.EarliestSeqNum))
	w.Printf(" titers %9d\n", redact.Safe(m.TableIters))
	w.Printf(" filter %9s %7s %6.1f%%  (score == utility)\n",
		notApplicable,
		notApplicable,
		redact.Safe(hitRate(m.Filter.Hits, m.Filter.Misses)))
	w.Printf(" ingest %9d\n",
		redact.Safe(m.Ingest.Count),
	)
}

func hitRate(hits, misses int64) float64 {
	sum := hits + misses
	if sum == 0 {
		return 0
	}
	return 100 * float64(hits) / float64(sum)
}
