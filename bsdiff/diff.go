package bsdiff

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"runtime"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/golang/protobuf/proto"
	"github.com/itchio/wharf/state"
	"github.com/jgallagher/gosaca"
)

// MaxFileSize is the largest size bsdiff will diff (for both old and new file): 2GB - 1 bytes
// a different codepath could be used for larger files, at the cost of unreasonable memory usage
// (even in 2016). If a big corporate user is willing to sponsor that part of the code, get in touch!
// Fair warning though: it won't be covered, our CI workers don't have that much RAM :)
const MaxFileSize = int64(math.MaxInt32 - 1)

// MaxMessageSize is the maximum amount of bytes that will be stored
// in a protobuf message generated by bsdiff. This enable friendlier streaming apply
// at a small storage cost
// TODO: actually use
const MaxMessageSize int64 = 16 * 1024 * 1024

type DiffStats struct {
	TimeSpentSorting  time.Duration
	TimeSpentScanning time.Duration
	BiggestAdd        int64
}

// DiffContext holds settings for the diff process, along with some
// internal storage: re-using a diff context is good to avoid GC thrashing
// (but never do it concurrently!)
type DiffContext struct {
	// SuffixSortConcurrency specifies the number of workers to use for suffix sorting.
	// Exceeding the number of cores will only slow it down. A 0 value (default) uses
	// sequential suffix sorting, which uses less RAM and has less overhead (might be faster
	// in some scenarios). A negative value means (number of cores - value).
	SuffixSortConcurrency int

	// MeasureMem enables printing memory usage statistics at various points in the
	// diffing process.
	MeasureMem bool

	// MeasureParallelOverhead prints some stats on the overhead of parallel suffix sorting
	MeasureParallelOverhead bool

	Stats *DiffStats

	db bytes.Buffer
}

// WriteMessageFunc should write a given protobuf message and relay any errors
// No reference to the given message can be kept, as its content may be modified
// after WriteMessageFunc returns. See the `wire` package for an example implementation.
type WriteMessageFunc func(msg proto.Message) (err error)

// Do computes the difference between old and new, according to the bsdiff
// algorithm, and writes the result to patch.
func (ctx *DiffContext) Do(old, new io.Reader, writeMessage WriteMessageFunc, consumer *state.Consumer) error {
	var memstats *runtime.MemStats

	if ctx.MeasureMem {
		memstats = &runtime.MemStats{}
		runtime.ReadMemStats(memstats)
		consumer.Debugf("Allocated bytes at start of bsdiff: %s (%s total)", humanize.IBytes(uint64(memstats.Alloc)), humanize.IBytes(uint64(memstats.TotalAlloc)))
	}

	obuf, err := ioutil.ReadAll(old)
	if err != nil {
		return err
	}

	obuflen := len(obuf)

	nbuf, err := ioutil.ReadAll(new)
	if err != nil {
		return err
	}
	nbuflen := len(nbuf)

	if ctx.MeasureMem {
		runtime.ReadMemStats(memstats)
		consumer.Debugf("Allocated bytes after ReadAll: %s (%s total)", humanize.IBytes(uint64(memstats.Alloc)), humanize.IBytes(uint64(memstats.TotalAlloc)))
	}

	var lenf int
	startTime := time.Now()

	I := make([]int, len(obuf)+1)
	ws := &gosaca.WorkSpace{}
	ws.ComputeSuffixArray(obuf, I[:len(obuf)])

	if ctx.Stats != nil {
		ctx.Stats.TimeSpentSorting += time.Since(startTime)
	}

	if ctx.MeasureMem {
		runtime.ReadMemStats(memstats)
		consumer.Debugf("Allocated bytes after qsufsort: %s (%s total)", humanize.IBytes(uint64(memstats.Alloc)), humanize.IBytes(uint64(memstats.TotalAlloc)))
	}

	bsdc := &Control{}

	consumer.ProgressLabel(fmt.Sprintf("Scanning %s...", humanize.IBytes(uint64(nbuflen))))

	var lastProgressUpdate int
	var updateEvery = 64 * 1024 * 1046 // 64MB

	startTime = time.Now()

	// Compute the differences, writing ctrl as we go
	var scan, pos, length int
	var lastscan, lastpos, lastoffset int
	for scan < nbuflen {
		var oldscore int
		scan += length

		if scan-lastProgressUpdate > updateEvery {
			lastProgressUpdate = scan
			progress := float64(scan) / float64(nbuflen)
			consumer.Progress(progress)
		}

		for scsc := scan; scan < nbuflen; scan++ {
			pos, length = search(I, obuf, nbuf[scan:], 0, obuflen)

			for ; scsc < scan+length; scsc++ {
				if scsc+lastoffset < obuflen &&
					obuf[scsc+lastoffset] == nbuf[scsc] {
					oldscore++
				}
			}

			if (length == oldscore && length != 0) || length > oldscore+8 {
				break
			}

			if scan+lastoffset < obuflen && obuf[scan+lastoffset] == nbuf[scan] {
				oldscore--
			}
		}

		if length != oldscore || scan == nbuflen {
			var s, Sf int
			lenf = 0
			for i := int(0); lastscan+i < scan && lastpos+i < obuflen; {
				if obuf[lastpos+i] == nbuf[lastscan+i] {
					s++
				}
				i++
				if s*2-i > Sf*2-lenf {
					Sf = s
					lenf = i
				}
			}

			lenb := 0
			if scan < nbuflen {
				var s, Sb int
				for i := int(1); (scan >= lastscan+i) && (pos >= i); i++ {
					if obuf[pos-i] == nbuf[scan-i] {
						s++
					}
					if s*2-i > Sb*2-lenb {
						Sb = s
						lenb = i
					}
				}
			}

			if lastscan+lenf > scan-lenb {
				overlap := (lastscan + lenf) - (scan - lenb)
				s := int(0)
				Ss := int(0)
				lens := int(0)
				for i := int(0); i < overlap; i++ {
					if nbuf[lastscan+lenf-overlap+i] == obuf[lastpos+lenf-overlap+i] {
						s++
					}
					if nbuf[scan-lenb+i] == obuf[pos-lenb+i] {
						s--
					}
					if s > Ss {
						Ss = s
						lens = i + 1
					}
				}

				lenf += lens - overlap
				lenb -= lens
			}

			ctx.db.Reset()
			ctx.db.Grow(int(lenf))

			for i := int(0); i < lenf; i++ {
				ctx.db.WriteByte(nbuf[lastscan+i] - obuf[lastpos+i])
			}

			bsdc.Add = ctx.db.Bytes()
			bsdc.Copy = nbuf[(lastscan + lenf):(scan - lenb)]
			bsdc.Seek = int64((pos - lenb) - (lastpos + lenf))

			err := writeMessage(bsdc)
			if err != nil {
				return err
			}

			if ctx.Stats != nil && ctx.Stats.BiggestAdd < int64(lenf) {
				ctx.Stats.BiggestAdd = int64(lenf)
			}

			lastscan = scan - lenb
			lastpos = pos - lenb
			lastoffset = pos - scan
		}
	}

	if ctx.Stats != nil {
		ctx.Stats.TimeSpentScanning += time.Since(startTime)
	}

	if ctx.MeasureMem {
		runtime.ReadMemStats(memstats)
		consumer.Debugf("Allocated bytes after scan: %s (%s total)", humanize.IBytes(uint64(memstats.Alloc)), humanize.IBytes(uint64(memstats.TotalAlloc)))
	}

	bsdc.Reset()
	bsdc.Eof = true
	err = writeMessage(bsdc)
	if err != nil {
		return err
	}

	return nil
}
