package chunk

import (
	"reflect"
	"testing"

	"github.com/getkipper/kipper/datamover/internal/manifest"
)

func mkManifest(sizes ...int64) *manifest.Manifest {
	m := &manifest.Manifest{ChunkSize: 10}
	for i, s := range sizes {
		m.Entries = append(m.Entries, manifest.Entry{Path: string(rune('a' + i)), Type: manifest.TypeFile, Size: s})
	}
	return m
}

func TestPlan(t *testing.T) {
	tests := []struct {
		name       string
		m          *manifest.Manifest
		wantTotal  int64
		wantChunks int
		wantSpans  map[int][]Span
	}{
		{
			name:       "empty manifest",
			m:          mkManifest(),
			wantTotal:  0,
			wantChunks: 0,
		},
		{
			name:       "single file under one chunk",
			m:          mkManifest(4),
			wantTotal:  4,
			wantChunks: 1,
			wantSpans: map[int][]Span{
				0: {{FileIndex: 0, Path: "a", FileOffset: 0, Length: 4}},
			},
		},
		{
			name:       "large file spans chunks",
			m:          mkManifest(25),
			wantTotal:  25,
			wantChunks: 3,
			wantSpans: map[int][]Span{
				0: {{FileIndex: 0, Path: "a", FileOffset: 0, Length: 10}},
				1: {{FileIndex: 0, Path: "a", FileOffset: 10, Length: 10}},
				2: {{FileIndex: 0, Path: "a", FileOffset: 20, Length: 5}},
			},
		},
		{
			name:       "small files pack into one chunk",
			m:          mkManifest(3, 4, 3),
			wantTotal:  10,
			wantChunks: 1,
			wantSpans: map[int][]Span{
				0: {
					{FileIndex: 0, Path: "a", FileOffset: 0, Length: 3},
					{FileIndex: 1, Path: "b", FileOffset: 0, Length: 4},
					{FileIndex: 2, Path: "c", FileOffset: 0, Length: 3},
				},
			},
		},
		{
			name:       "file straddles a chunk boundary",
			m:          mkManifest(6, 8),
			wantTotal:  14,
			wantChunks: 2,
			wantSpans: map[int][]Span{
				0: {
					{FileIndex: 0, Path: "a", FileOffset: 0, Length: 6},
					{FileIndex: 1, Path: "b", FileOffset: 0, Length: 4},
				},
				1: {{FileIndex: 1, Path: "b", FileOffset: 4, Length: 4}},
			},
		},
		{
			name:       "zero-size files never appear in spans",
			m:          mkManifest(0, 5, 0, 5, 0),
			wantTotal:  10,
			wantChunks: 1,
			wantSpans: map[int][]Span{
				0: {
					{FileIndex: 1, Path: "b", FileOffset: 0, Length: 5},
					{FileIndex: 3, Path: "d", FileOffset: 0, Length: 5},
				},
			},
		},
		{
			name:       "exact chunk boundary",
			m:          mkManifest(10, 10),
			wantTotal:  20,
			wantChunks: 2,
			wantSpans: map[int][]Span{
				0: {{FileIndex: 0, Path: "a", FileOffset: 0, Length: 10}},
				1: {{FileIndex: 1, Path: "b", FileOffset: 0, Length: 10}},
			},
		},
		{
			name:       "only empty files",
			m:          mkManifest(0, 0),
			wantTotal:  0,
			wantChunks: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPlan(tt.m)
			if p.TotalBytes() != tt.wantTotal {
				t.Errorf("TotalBytes() = %d, want %d", p.TotalBytes(), tt.wantTotal)
			}
			if p.NumChunks() != tt.wantChunks {
				t.Errorf("NumChunks() = %d, want %d", p.NumChunks(), tt.wantChunks)
			}
			var sum int64
			for n := 0; n < p.NumChunks(); n++ {
				sum += p.ChunkLength(n)
				var spanSum int64
				for _, sp := range p.Spans(n) {
					spanSum += sp.Length
				}
				if spanSum != p.ChunkLength(n) {
					t.Errorf("chunk %d: spans cover %d bytes, ChunkLength says %d", n, spanSum, p.ChunkLength(n))
				}
				if want, ok := tt.wantSpans[n]; ok {
					if !reflect.DeepEqual(p.Spans(n), want) {
						t.Errorf("Spans(%d) = %+v, want %+v", n, p.Spans(n), want)
					}
				}
			}
			if sum != tt.wantTotal {
				t.Errorf("chunk lengths sum to %d, want %d", sum, tt.wantTotal)
			}
		})
	}
}
