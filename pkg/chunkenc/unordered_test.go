package chunkenc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/pkg/iter"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql/log"
	"github.com/grafana/loki/pkg/logqlmodel/stats"
)

func iterEq(t *testing.T, exp []entry, got iter.EntryIterator) {
	var i int
	for got.Next() {
		require.Equal(t, logproto.Entry{
			Timestamp: time.Unix(0, exp[i].t),
			Line:      exp[i].s,
		}, got.Entry())
		require.Equal(t, exp[i].nonIndexedLabels.String(), got.Labels())
		i++
	}
	require.Equal(t, i, len(exp))
}

func Test_forEntriesEarlyReturn(t *testing.T) {
	hb := newUnorderedHeadBlock(UnorderedHeadBlockFmt, newSymbolizer())
	for i := 0; i < 10; i++ {
		require.Nil(t, hb.Append(int64(i), fmt.Sprint(i), labels.Labels{{Name: "i", Value: fmt.Sprint(i)}}))
	}

	// forward
	var forwardCt int
	var forwardStop int64
	err := hb.forEntries(
		context.Background(),
		logproto.FORWARD,
		0,
		math.MaxInt64,
		func(_ *stats.Context, ts int64, _ string, _ symbols) error {
			forwardCt++
			forwardStop = ts
			if ts == 5 {
				return errors.New("err")
			}
			return nil
		},
	)
	require.Error(t, err)
	require.Equal(t, int64(5), forwardStop)
	require.Equal(t, 6, forwardCt)

	// backward
	var backwardCt int
	var backwardStop int64
	err = hb.forEntries(
		context.Background(),
		logproto.BACKWARD,
		0,
		math.MaxInt64,
		func(_ *stats.Context, ts int64, _ string, _ symbols) error {
			backwardCt++
			backwardStop = ts
			if ts == 5 {
				return errors.New("err")
			}
			return nil
		},
	)
	require.Error(t, err)
	require.Equal(t, int64(5), backwardStop)
	require.Equal(t, 5, backwardCt)
}

func Test_Unordered_InsertRetrieval(t *testing.T) {
	for _, tc := range []struct {
		desc       string
		input, exp []entry
		dir        logproto.Direction
	}{
		{
			desc: "simple forward",
			input: []entry{
				{0, "a", nil}, {1, "b", nil}, {2, "c", labels.Labels{{Name: "a", Value: "b"}}},
			},
			exp: []entry{
				{0, "a", nil}, {1, "b", nil}, {2, "c", labels.Labels{{Name: "a", Value: "b"}}},
			},
		},
		{
			desc: "simple backward",
			input: []entry{
				{0, "a", nil}, {1, "b", nil}, {2, "c", labels.Labels{{Name: "a", Value: "b"}}},
			},
			exp: []entry{
				{2, "c", labels.Labels{{Name: "a", Value: "b"}}}, {1, "b", nil}, {0, "a", nil},
			},
			dir: logproto.BACKWARD,
		},
		{
			desc: "unordered forward",
			input: []entry{
				{1, "b", nil}, {0, "a", nil}, {2, "c", labels.Labels{{Name: "a", Value: "b"}}},
			},
			exp: []entry{
				{0, "a", nil}, {1, "b", nil}, {2, "c", labels.Labels{{Name: "a", Value: "b"}}},
			},
		},
		{
			desc: "unordered backward",
			input: []entry{
				{1, "b", nil}, {0, "a", nil}, {2, "c", labels.Labels{{Name: "a", Value: "b"}}},
			},
			exp: []entry{
				{2, "c", labels.Labels{{Name: "a", Value: "b"}}}, {1, "b", nil}, {0, "a", nil},
			},
			dir: logproto.BACKWARD,
		},
		{
			desc: "ts collision forward",
			input: []entry{
				{0, "a", labels.Labels{{Name: "a", Value: "b"}}}, {0, "b", labels.Labels{{Name: "a", Value: "b"}}}, {1, "c", nil},
			},
			exp: []entry{
				{0, "a", labels.Labels{{Name: "a", Value: "b"}}}, {0, "b", labels.Labels{{Name: "a", Value: "b"}}}, {1, "c", nil},
			},
		},
		{
			desc: "ts collision backward",
			input: []entry{
				{0, "a", labels.Labels{{Name: "a", Value: "b"}}}, {0, "b", nil}, {1, "c", nil},
			},
			exp: []entry{
				{1, "c", nil}, {0, "b", nil}, {0, "a", labels.Labels{{Name: "a", Value: "b"}}},
			},
			dir: logproto.BACKWARD,
		},
		{
			desc: "ts remove exact dupe forward",
			input: []entry{
				{0, "a", nil}, {0, "b", nil}, {1, "c", nil}, {0, "b", labels.Labels{{Name: "a", Value: "b"}}},
			},
			exp: []entry{
				{0, "a", nil}, {0, "b", nil}, {1, "c", nil},
			},
			dir: logproto.FORWARD,
		},
		{
			desc: "ts remove exact dupe backward",
			input: []entry{
				{0, "a", nil}, {0, "b", nil}, {1, "c", nil}, {0, "b", labels.Labels{{Name: "a", Value: "b"}}},
			},
			exp: []entry{
				{1, "c", nil}, {0, "b", nil}, {0, "a", nil},
			},
			dir: logproto.BACKWARD,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			for _, format := range []HeadBlockFmt{
				UnorderedHeadBlockFmt,
				UnorderedWithNonIndexedLabelsHeadBlockFmt,
			} {
				t.Run(format.String(), func(t *testing.T) {
					hb := newUnorderedHeadBlock(format, newSymbolizer())
					for _, e := range tc.input {
						require.Nil(t, hb.Append(e.t, e.s, e.nonIndexedLabels))
					}

					itr := hb.Iterator(
						context.Background(),
						tc.dir,
						0,
						math.MaxInt64,
						noopStreamPipeline,
					)

					expected := make([]entry, len(tc.exp))
					copy(expected, tc.exp)
					if format < UnorderedWithNonIndexedLabelsHeadBlockFmt {
						for i := range expected {
							expected[i].nonIndexedLabels = nil
						}
					}

					iterEq(t, expected, itr)
				})
			}
		})
	}
}

func Test_UnorderedBoundedIter(t *testing.T) {
	for _, tc := range []struct {
		desc       string
		mint, maxt int64
		dir        logproto.Direction
		input      []entry
		exp        []entry
	}{
		{
			desc: "simple",
			mint: 1,
			maxt: 4,
			input: []entry{
				{0, "a", nil}, {1, "b", labels.Labels{{Name: "a", Value: "b"}}}, {2, "c", nil}, {3, "d", nil}, {4, "e", nil},
			},
			exp: []entry{
				{1, "b", labels.Labels{{Name: "a", Value: "b"}}}, {2, "c", nil}, {3, "d", nil},
			},
		},
		{
			desc: "simple backward",
			mint: 1,
			maxt: 4,
			input: []entry{
				{0, "a", nil}, {1, "b", labels.Labels{{Name: "a", Value: "b"}}}, {2, "c", nil}, {3, "d", nil}, {4, "e", nil},
			},
			exp: []entry{
				{3, "d", nil}, {2, "c", nil}, {1, "b", labels.Labels{{Name: "a", Value: "b"}}},
			},
			dir: logproto.BACKWARD,
		},
		{
			desc: "unordered",
			mint: 1,
			maxt: 4,
			input: []entry{
				{0, "a", nil}, {2, "c", nil}, {1, "b", labels.Labels{{Name: "a", Value: "b"}}}, {4, "e", nil}, {3, "d", nil},
			},
			exp: []entry{
				{1, "b", labels.Labels{{Name: "a", Value: "b"}}}, {2, "c", nil}, {3, "d", nil},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			for _, format := range []HeadBlockFmt{
				UnorderedHeadBlockFmt,
				UnorderedWithNonIndexedLabelsHeadBlockFmt,
			} {
				t.Run(format.String(), func(t *testing.T) {
					hb := newUnorderedHeadBlock(format, newSymbolizer())
					for _, e := range tc.input {
						require.Nil(t, hb.Append(e.t, e.s, e.nonIndexedLabels))
					}

					itr := hb.Iterator(
						context.Background(),
						tc.dir,
						tc.mint,
						tc.maxt,
						noopStreamPipeline,
					)

					expected := make([]entry, len(tc.exp))
					copy(expected, tc.exp)
					if format < UnorderedWithNonIndexedLabelsHeadBlockFmt {
						for i := range expected {
							expected[i].nonIndexedLabels = nil
						}
					}

					iterEq(t, expected, itr)
				})
			}
		})
	}
}

func TestHeadBlockInterop(t *testing.T) {
	unordered, ordered := newUnorderedHeadBlock(UnorderedHeadBlockFmt, nil), &headBlock{}
	unorderedWithNonIndexedLabels := newUnorderedHeadBlock(UnorderedWithNonIndexedLabelsHeadBlockFmt, newSymbolizer())
	for i := 0; i < 100; i++ {
		metaLabels := labels.Labels{{Name: "foo", Value: fmt.Sprint(99 - i)}}
		require.Nil(t, unordered.Append(int64(99-i), fmt.Sprint(99-i), metaLabels))
		require.Nil(t, unorderedWithNonIndexedLabels.Append(int64(99-i), fmt.Sprint(99-i), metaLabels))
		require.Nil(t, ordered.Append(int64(i), fmt.Sprint(i), labels.Labels{{Name: "foo", Value: fmt.Sprint(i)}}))
	}

	// turn to bytes
	orderedCheckpointBytes, err := ordered.CheckpointBytes(nil)
	require.Nil(t, err)
	unorderedCheckpointBytes, err := unordered.CheckpointBytes(nil)
	require.Nil(t, err)
	unorderedWithNonIndexedLabelsCheckpointBytes, err := unorderedWithNonIndexedLabels.CheckpointBytes(nil)
	require.Nil(t, err)

	// Ensure we can recover ordered checkpoint into ordered headblock
	recovered, err := HeadFromCheckpoint(orderedCheckpointBytes, OrderedHeadBlockFmt, nil)
	require.Nil(t, err)
	require.Equal(t, ordered, recovered)

	// Ensure we can recover ordered checkpoint into unordered headblock
	recovered, err = HeadFromCheckpoint(orderedCheckpointBytes, UnorderedHeadBlockFmt, nil)
	require.Nil(t, err)
	require.Equal(t, unordered, recovered)

	// Ensure we can recover ordered checkpoint into unordered headblock with non-indexed labels
	recovered, err = HeadFromCheckpoint(orderedCheckpointBytes, UnorderedWithNonIndexedLabelsHeadBlockFmt, nil)
	require.NoError(t, err)
	require.Equal(t, &unorderedHeadBlock{
		format: UnorderedWithNonIndexedLabelsHeadBlockFmt,
		rt:     unordered.rt,
		lines:  unordered.lines,
		size:   unordered.size,
		mint:   unordered.mint,
		maxt:   unordered.maxt,
	}, recovered)

	// Ensure we can recover unordered checkpoint into unordered headblock
	recovered, err = HeadFromCheckpoint(unorderedCheckpointBytes, UnorderedHeadBlockFmt, nil)
	require.Nil(t, err)
	require.Equal(t, unordered, recovered)

	// Ensure trying to recover unordered checkpoint into unordered with non-indexed labels keeps it in unordered format
	recovered, err = HeadFromCheckpoint(unorderedCheckpointBytes, UnorderedWithNonIndexedLabelsHeadBlockFmt, nil)
	require.NoError(t, err)
	require.Equal(t, unordered, recovered)

	// Ensure trying to recover unordered with non-indexed labels checkpoint into ordered headblock keeps it in unordered with non-indexed labels format
	recovered, err = HeadFromCheckpoint(unorderedWithNonIndexedLabelsCheckpointBytes, OrderedHeadBlockFmt, unorderedWithNonIndexedLabels.symbolizer)
	require.Nil(t, err)
	require.Equal(t, unorderedWithNonIndexedLabels, recovered) // we compare the data with unordered because unordered head block does not contain metaLabels.

	// Ensure trying to recover unordered with non-indexed labels checkpoint into unordered headblock keeps it in unordered with non-indexed labels format
	recovered, err = HeadFromCheckpoint(unorderedWithNonIndexedLabelsCheckpointBytes, UnorderedHeadBlockFmt, unorderedWithNonIndexedLabels.symbolizer)
	require.Nil(t, err)
	require.Equal(t, unorderedWithNonIndexedLabels, recovered) // we compare the data with unordered because unordered head block does not contain metaLabels.

	// Ensure we can recover unordered with non-indexed checkpoint into unordered with non-indexed headblock
	recovered, err = HeadFromCheckpoint(unorderedWithNonIndexedLabelsCheckpointBytes, UnorderedWithNonIndexedLabelsHeadBlockFmt, unorderedWithNonIndexedLabels.symbolizer)
	require.Nil(t, err)
	require.Equal(t, unorderedWithNonIndexedLabels, recovered)
}

// ensure backwards compatibility from when chunk format
// and head block format was split
func TestChunkBlockFmt(t *testing.T) {
	require.Equal(t, chunkFormatV3, byte(OrderedHeadBlockFmt))
}

func BenchmarkHeadBlockWrites(b *testing.B) {
	// ordered, ordered
	// unordered, ordered
	// unordered, unordered

	// current default block size of 256kb with 75b avg log lines =~ 5.2k lines/block
	nWrites := (256 << 10) / 50

	headBlockFn := func() func(int64, string, labels.Labels) {
		hb := &headBlock{}
		return func(ts int64, line string, metaLabels labels.Labels) {
			_ = hb.Append(ts, line, metaLabels)
		}
	}

	unorderedHeadBlockFn := func() func(int64, string, labels.Labels) {
		hb := newUnorderedHeadBlock(UnorderedHeadBlockFmt, nil)
		return func(ts int64, line string, metaLabels labels.Labels) {
			_ = hb.Append(ts, line, metaLabels)
		}
	}

	for _, tc := range []struct {
		desc            string
		fn              func() func(int64, string, labels.Labels)
		unorderedWrites bool
	}{
		{
			desc: "ordered headblock ordered writes",
			fn:   headBlockFn,
		},
		{
			desc: "unordered headblock ordered writes",
			fn:   unorderedHeadBlockFn,
		},
		{
			desc:            "unordered headblock unordered writes",
			fn:              unorderedHeadBlockFn,
			unorderedWrites: true,
		},
	} {
		for _, withNonIndexedLabels := range []bool{false, true} {
			// build writes before we start benchmarking so random number generation, etc,
			// isn't included in our timing info
			writes := make([]entry, 0, nWrites)
			rnd := rand.NewSource(0)
			for i := 0; i < nWrites; i++ {
				ts := int64(i)
				if tc.unorderedWrites {
					ts = rnd.Int63()
				}

				var nonIndexedLabels labels.Labels
				if withNonIndexedLabels {
					nonIndexedLabels = labels.Labels{{Name: "foo", Value: fmt.Sprint(ts)}}
				}

				writes = append(writes, entry{
					t:                ts,
					s:                fmt.Sprint("line:", i),
					nonIndexedLabels: nonIndexedLabels,
				})
			}

			name := tc.desc
			if withNonIndexedLabels {
				name += " with non-indexed labels"
			}
			b.Run(name, func(b *testing.B) {
				for n := 0; n < b.N; n++ {
					writeFn := tc.fn()
					for _, w := range writes {
						writeFn(w.t, w.s, w.nonIndexedLabels)
					}
				}
			})
		}
	}
}

func TestUnorderedChunkIterators(t *testing.T) {
	c := NewMemChunk(EncSnappy, UnorderedHeadBlockFmt, testBlockSize, testTargetSize)
	for i := 0; i < 100; i++ {
		// push in reverse order
		require.Nil(t, c.Append(&logproto.Entry{
			Timestamp: time.Unix(int64(99-i), 0),
			Line:      fmt.Sprint(99 - i),
		}))

		// ensure we have a mix of cut blocks + head block.
		if i%30 == 0 {
			require.Nil(t, c.cut())
		}
	}

	// ensure head block has data
	require.Equal(t, false, c.head.IsEmpty())

	forward, err := c.Iterator(
		context.Background(),
		time.Unix(0, 0),
		time.Unix(100, 0),
		logproto.FORWARD,
		noopStreamPipeline,
	)
	require.Nil(t, err)

	backward, err := c.Iterator(
		context.Background(),
		time.Unix(0, 0),
		time.Unix(100, 0),
		logproto.BACKWARD,
		noopStreamPipeline,
	)
	require.Nil(t, err)

	smpl := c.SampleIterator(
		context.Background(),
		time.Unix(0, 0),
		time.Unix(100, 0),
		countExtractor,
	)

	for i := 0; i < 100; i++ {
		require.Equal(t, true, forward.Next())
		require.Equal(t, true, backward.Next())
		require.Equal(t, true, smpl.Next())
		require.Equal(t, time.Unix(int64(i), 0), forward.Entry().Timestamp)
		require.Equal(t, time.Unix(int64(99-i), 0), backward.Entry().Timestamp)
		require.Equal(t, float64(1), smpl.Sample().Value)
		require.Equal(t, time.Unix(int64(i), 0).UnixNano(), smpl.Sample().Timestamp)
	}
	require.Equal(t, false, forward.Next())
	require.Equal(t, false, backward.Next())
}

func BenchmarkUnorderedRead(b *testing.B) {
	legacy := NewMemChunk(EncSnappy, OrderedHeadBlockFmt, testBlockSize, testTargetSize)
	fillChunkClose(legacy, false)
	ordered := NewMemChunk(EncSnappy, UnorderedHeadBlockFmt, testBlockSize, testTargetSize)
	fillChunkClose(ordered, false)
	unordered := NewMemChunk(EncSnappy, UnorderedHeadBlockFmt, testBlockSize, testTargetSize)
	fillChunkRandomOrder(unordered, false)

	tcs := []struct {
		desc string
		c    *MemChunk
	}{
		{
			desc: "ordered+legacy hblock",
			c:    legacy,
		},
		{
			desc: "ordered+unordered hblock",
			c:    ordered,
		},
		{
			desc: "unordered+unordered hblock",
			c:    unordered,
		},
	}

	b.Run("itr", func(b *testing.B) {
		for _, tc := range tcs {
			b.Run(tc.desc, func(b *testing.B) {
				for n := 0; n < b.N; n++ {
					iterator, err := tc.c.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), logproto.FORWARD, noopStreamPipeline)
					if err != nil {
						panic(err)
					}
					for iterator.Next() {
						_ = iterator.Entry()
					}
					if err := iterator.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	})

	b.Run("smpl", func(b *testing.B) {
		for _, tc := range tcs {
			b.Run(tc.desc, func(b *testing.B) {
				for n := 0; n < b.N; n++ {
					iterator := tc.c.SampleIterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), countExtractor)
					for iterator.Next() {
						_ = iterator.Sample()
					}
					if err := iterator.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	})
}

func TestUnorderedIteratorCountsAllEntries(t *testing.T) {
	c := NewMemChunk(EncSnappy, UnorderedHeadBlockFmt, testBlockSize, testTargetSize)
	fillChunkRandomOrder(c, false)

	ct := 0
	var i int64
	iterator, err := c.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), logproto.FORWARD, noopStreamPipeline)
	if err != nil {
		panic(err)
	}
	for iterator.Next() {
		next := iterator.Entry().Timestamp.UnixNano()
		require.GreaterOrEqual(t, next, i)
		i = next
		ct++
	}
	if err := iterator.Close(); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, c.Size(), ct)

	ct = 0
	i = 0
	smpl := c.SampleIterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), countExtractor)
	for smpl.Next() {
		next := smpl.Sample().Timestamp
		require.GreaterOrEqual(t, next, i)
		i = next
		ct += int(smpl.Sample().Value)
	}
	require.Equal(t, c.Size(), ct)

	if err := iterator.Close(); err != nil {
		t.Fatal(err)
	}
}

func chunkFrom(xs []logproto.Entry) ([]byte, error) {
	c := NewMemChunk(EncSnappy, OrderedHeadBlockFmt, testBlockSize, testTargetSize)
	for _, x := range xs {
		if err := c.Append(&x); err != nil {
			return nil, err
		}
	}

	if err := c.Close(); err != nil {
		return nil, err
	}
	return c.Bytes()
}

func TestReorder(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		input    []logproto.Entry
		expected []logproto.Entry
	}{
		{
			desc: "unordered",
			input: []logproto.Entry{
				{
					Timestamp: time.Unix(4, 0),
					Line:      "x",
				},
				{
					Timestamp: time.Unix(2, 0),
					Line:      "x",
				},
				{
					Timestamp: time.Unix(3, 0),
					Line:      "x",
				},
				{
					Timestamp: time.Unix(1, 0),
					Line:      "x",
				},
			},
			expected: []logproto.Entry{
				{
					Timestamp: time.Unix(1, 0),
					Line:      "x",
				},
				{
					Timestamp: time.Unix(2, 0),
					Line:      "x",
				},
				{
					Timestamp: time.Unix(3, 0),
					Line:      "x",
				},
				{
					Timestamp: time.Unix(4, 0),
					Line:      "x",
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			c := NewMemChunk(EncSnappy, UnorderedHeadBlockFmt, testBlockSize, testTargetSize)
			for _, x := range tc.input {
				require.Nil(t, c.Append(&x))
			}
			require.Nil(t, c.Close())
			b, err := c.Bytes()
			require.Nil(t, err)

			exp, err := chunkFrom(tc.expected)
			require.Nil(t, err)

			require.Equal(t, exp, b)
		})
	}
}

func TestReorderAcrossBlocks(t *testing.T) {
	c := NewMemChunk(EncSnappy, UnorderedHeadBlockFmt, testBlockSize, testTargetSize)
	for _, batch := range [][]int{
		// ensure our blocks have overlapping bounds and must be reordered
		// before closing.
		{1, 5},
		{3, 7},
	} {
		for _, x := range batch {
			require.Nil(t, c.Append(&logproto.Entry{
				Timestamp: time.Unix(int64(x), 0),
				Line:      fmt.Sprint(x),
			}))
		}
		require.Nil(t, c.cut())
	}
	// get bounds before it's reordered
	from, to := c.Bounds()
	require.Nil(t, c.Close())

	itr, err := c.Iterator(context.Background(), from, to.Add(time.Nanosecond), logproto.FORWARD, log.NewNoopPipeline().ForStream(nil))
	require.Nil(t, err)

	exp := []entry{
		{
			t: time.Unix(1, 0).UnixNano(),
			s: "1",
		},
		{
			t: time.Unix(3, 0).UnixNano(),
			s: "3",
		},
		{
			t: time.Unix(5, 0).UnixNano(),
			s: "5",
		},
		{
			t: time.Unix(7, 0).UnixNano(),
			s: "7",
		},
	}
	iterEq(t, exp, itr)
}

func Test_HeadIteratorHash(t *testing.T) {
	lbs := labels.Labels{labels.Label{Name: "foo", Value: "bar"}}
	ex, err := log.NewLineSampleExtractor(log.CountExtractor, nil, nil, false, false)
	if err != nil {
		panic(err)
	}

	for name, b := range map[string]HeadBlock{
		"unordered":                         newUnorderedHeadBlock(UnorderedHeadBlockFmt, nil),
		"unordered with non-indexed labels": newUnorderedHeadBlock(UnorderedWithNonIndexedLabelsHeadBlockFmt, newSymbolizer()),
		"ordered":                           &headBlock{},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, b.Append(1, "foo", labels.Labels{{Name: "foo", Value: "bar"}}))
			eit := b.Iterator(context.Background(), logproto.BACKWARD, 0, 2, log.NewNoopPipeline().ForStream(lbs))

			for eit.Next() {
				require.Equal(t, lbs.Hash(), eit.StreamHash())
			}

			sit := b.SampleIterator(context.TODO(), 0, 2, ex.ForStream(lbs))
			for sit.Next() {
				require.Equal(t, lbs.Hash(), sit.StreamHash())
			}
		})
	}
}
