// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package keyspan

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/datadriven"
	"github.com/stretchr/testify/require"
)

var spanRe = regexp.MustCompile(`(\d+):\s*(\w+)-*(\w+)\w*([^\n]*)`)

func parseSpan(t *testing.T, s string, kind base.InternalKeyKind) Span {
	m := spanRe.FindStringSubmatch(s)
	if len(m) != 5 {
		t.Fatalf("expected 5 components, but found %d: %s", len(m), s)
	}
	seqNum, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	return Span{
		Start: base.MakeInternalKey([]byte(m[2]), uint64(seqNum), kind),
		End:   []byte(m[3]),
		Value: []byte(strings.TrimSpace(m[4])),
	}
}

func buildSpans(
	t *testing.T, cmp base.Compare, formatKey base.FormatKey, s string, kind base.InternalKeyKind,
) []Span {
	var spans []Span
	f := &Fragmenter{
		Cmp:    cmp,
		Format: formatKey,
		Emit: func(fragmented []Span) {
			spans = append(spans, fragmented...)
		},
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "flush-to ") {
			parts := strings.Split(line, " ")
			if len(parts) != 2 {
				t.Fatalf("expected 2 components, but found %d: %s", len(parts), line)
			}
			f.FlushTo([]byte(parts[1]))
			continue
		} else if strings.HasPrefix(line, "truncate-and-flush-to ") {
			parts := strings.Split(line, " ")
			if len(parts) != 2 {
				t.Fatalf("expected 2 components, but found %d: %s", len(parts), line)
			}
			f.TruncateAndFlushTo([]byte(parts[1]))
			continue
		}

		f.Add(parseSpan(t, line, kind))
	}
	f.Finish()
	return spans
}

func formatSpans(spans []Span) string {
	isLetter := func(b []byte) bool {
		if len(b) != 1 {
			return false
		}
		return b[0] >= 'a' && b[0] <= 'z'
	}

	var buf bytes.Buffer
	for _, v := range spans {
		switch {
		case v.Empty():
			fmt.Fprintf(&buf, "<empty>")
		case !isLetter(v.Start.UserKey) || !isLetter(v.End) || v.Start.UserKey[0] == v.End[0]:
			fmt.Fprintf(&buf, "%d: %s-%s", v.Start.SeqNum(), v.Start.UserKey, v.End)
		default:
			fmt.Fprintf(&buf, "%d: %s%s%s%s",
				v.Start.SeqNum(),
				strings.Repeat(" ", int(v.Start.UserKey[0]-'a')),
				v.Start.UserKey,
				strings.Repeat("-", int(v.End[0]-v.Start.UserKey[0]-1)),
				v.End)
		}
		if len(v.Value) > 0 {
			buf.WriteString(strings.Repeat(" ", int('z'-v.End[0]+1)))
			buf.WriteString(string(v.Value))
		}
		buf.WriteRune('\n')
	}
	return buf.String()
}

func TestFragmenter(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	fmtKey := base.DefaultComparer.FormatKey

	var getRe = regexp.MustCompile(`(\w+)#(\d+)`)

	parseGet := func(t *testing.T, s string) (string, int) {
		m := getRe.FindStringSubmatch(s)
		if len(m) != 3 {
			t.Fatalf("expected 3 components, but found %d", len(m))
		}
		seq, err := strconv.Atoi(m[2])
		require.NoError(t, err)
		return m[1], seq
	}

	var iter base.InternalIterator

	// Returns true if the specified <key,seq> pair is deleted at the specified
	// read sequence number. Get ignores spans newer than the read sequence
	// number. This is a simple version of what full processing of range
	// tombstones looks like.
	deleted := func(key []byte, seq, readSeq uint64) bool {
		s := Get(cmp, iter, key, readSeq)
		return s.Covers(seq)
	}

	datadriven.RunTest(t, "testdata/fragmenter", func(d *datadriven.TestData) string {
		switch d.Cmd {
		case "build":
			return func() (result string) {
				defer func() {
					if r := recover(); r != nil {
						result = fmt.Sprint(r)
					}
				}()

				spans := buildSpans(t, cmp, fmtKey, d.Input, base.InternalKeyKindRangeDelete)
				iter = NewIter(cmp, spans)
				return formatSpans(spans)
			}()

		case "get":
			if len(d.CmdArgs) != 1 {
				return fmt.Sprintf("expected 1 argument, but found %s", d.CmdArgs)
			}
			if d.CmdArgs[0].Key != "t" {
				return fmt.Sprintf("expected timestamp argument, but found %s", d.CmdArgs[0])
			}
			readSeq, err := strconv.Atoi(d.CmdArgs[0].Vals[0])
			require.NoError(t, err)

			var results []string
			for _, p := range strings.Split(d.Input, " ") {
				key, seq := parseGet(t, p)
				if deleted([]byte(key), uint64(seq), uint64(readSeq)) {
					results = append(results, "deleted")
				} else {
					results = append(results, "alive")
				}
			}
			return strings.Join(results, " ")

		default:
			return fmt.Sprintf("unknown command: %s", d.Cmd)
		}
	})
}

func TestFragmenterDeleted(t *testing.T) {
	datadriven.RunTest(t, "testdata/fragmenter_deleted", func(d *datadriven.TestData) string {
		switch d.Cmd {
		case "build":
			f := &Fragmenter{
				Cmp:    base.DefaultComparer.Compare,
				Format: base.DefaultComparer.FormatKey,
				Emit: func(fragmented []Span) {
				},
			}
			var buf bytes.Buffer
			for _, line := range strings.Split(d.Input, "\n") {
				switch {
				case strings.HasPrefix(line, "add "):
					t := parseSpan(t, strings.TrimPrefix(line, "add "), base.InternalKeyKindRangeDelete)
					f.Add(t)
				case strings.HasPrefix(line, "deleted "):
					key := base.ParseInternalKey(strings.TrimPrefix(line, "deleted "))
					func() {
						defer func() {
							if r := recover(); r != nil {
								fmt.Fprintf(&buf, "%s: %s\n", key, r)
							}
						}()
						fmt.Fprintf(&buf, "%s: %t\n", key, f.Covers(key, base.InternalKeySeqNumMax))
					}()
				}
			}
			return buf.String()

		default:
			return fmt.Sprintf("unknown command: %s", d.Cmd)
		}
	})
}

func TestFragmenterFlushTo(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	fmtKey := base.DefaultComparer.FormatKey

	datadriven.RunTest(t, "testdata/fragmenter_flush_to", func(d *datadriven.TestData) string {
		switch d.Cmd {
		case "build":
			return func() (result string) {
				defer func() {
					if r := recover(); r != nil {
						result = fmt.Sprint(r)
					}
				}()

				spans := buildSpans(t, cmp, fmtKey, d.Input, base.InternalKeyKindRangeDelete)
				return formatSpans(spans)
			}()

		default:
			return fmt.Sprintf("unknown command: %s", d.Cmd)
		}
	})
}

func TestFragmenterTruncateAndFlushTo(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	fmtKey := base.DefaultComparer.FormatKey

	datadriven.RunTest(t, "testdata/fragmenter_truncate_and_flush_to", func(d *datadriven.TestData) string {
		switch d.Cmd {
		case "build":
			return func() (result string) {
				defer func() {
					if r := recover(); r != nil {
						result = fmt.Sprint(r)
					}
				}()

				spans := buildSpans(t, cmp, fmtKey, d.Input, base.InternalKeyKindRangeDelete)
				return formatSpans(spans)
			}()

		default:
			return fmt.Sprintf("unknown command: %s", d.Cmd)
		}
	})
}

func TestFragmenter_Values(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	fmtKey := base.DefaultComparer.FormatKey

	datadriven.RunTest(t, "testdata/fragmenter_values", func(d *datadriven.TestData) string {
		switch d.Cmd {
		case "build":
			return func() (result string) {
				defer func() {
					if r := recover(); r != nil {
						result = fmt.Sprint(r)
					}
				}()

				// TODO(jackson): Keys of kind InternalKeyKindRangeDelete don't
				// have values. Update the call below when we have KindRangeSet,
				// KindRangeUnset.
				spans := buildSpans(t, cmp, fmtKey, d.Input, base.InternalKeyKindRangeDelete)
				return formatSpans(spans)
			}()

		default:
			return fmt.Sprintf("unknown command: %s", d.Cmd)
		}
	})
}
