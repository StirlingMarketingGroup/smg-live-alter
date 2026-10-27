package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestPacer(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name       string
		max        time.Duration
		known      []string
		reads      []statusRead
		calls      int
		wantSleeps int
		wantLogs   []string
		wantErr    error
		checkSaves bool
		wantSaved  [][]string
	}{
		{
			name: "off when max is zero",
			max:  0,
		},
		{
			name: "not aurora",
			max:  time.Second,
			reads: []statusRead{
				{aurora: false},
			},
			calls:    2,
			wantLogs: []string{"replica lag pacing is off: the server has no Aurora replica status"},
		},
		{
			name: "lag under max",
			max:  time.Second,
			reads: []statusRead{
				{aurora: true, readers: map[string]readerSample{"r": {lag: 100 * time.Millisecond}}},
			},
		},
		{
			name: "lag over max then under",
			max:  time.Second,
			reads: []statusRead{
				{aurora: true, readers: map[string]readerSample{"r": {lag: 2 * time.Second}}},
				{aurora: true, readers: map[string]readerSample{"r": {lag: 2 * time.Second}}},
				{aurora: true, readers: map[string]readerSample{"r": {lag: 2 * time.Second}}},
				{aurora: true, readers: map[string]readerSample{"r": {lag: 100 * time.Millisecond}}},
			},
			wantSleeps: 3,
			wantLogs: []string{
				"pausing the copy: reader r lag 2s is over 1s",
				"resuming the copy after 3s",
			},
		},
		{
			name: "status error",
			max:  time.Second,
			reads: []statusRead{
				{err: errBoom},
			},
			wantErr: errBoom,
		},
		{
			name: "status younger than 10s is not lag",
			max:  time.Second,
			reads: []statusRead{
				{aurora: true, readers: map[string]readerSample{"r": {lag: 100 * time.Millisecond, since: 2 * time.Second}}},
			},
		},
		{
			name: "status older than 10s stopped reporting",
			max:  time.Second,
			reads: []statusRead{
				{aurora: true, readers: map[string]readerSample{"r": {since: 30 * time.Second}}},
				{aurora: true, readers: map[string]readerSample{"r": {}}},
			},
			wantSleeps: 1,
			wantLogs: []string{
				"pausing the copy: reader r stopped reporting 30s ago",
				"resuming the copy after 1s",
			},
		},
		{
			name:  "seeded reader missing holds the copy",
			max:   time.Second,
			known: []string{"reader-a"},
			reads: []statusRead{
				{aurora: true, readers: map[string]readerSample{}},
				{aurora: true, readers: map[string]readerSample{"reader-a": {}}},
			},
			wantSleeps: 1,
			wantLogs: []string{
				"pausing the copy: reader reader-a stopped reporting",
				"resuming the copy after 1s",
			},
			checkSaves: true,
		},
		{
			name:  "save only when the set grows",
			max:   time.Second,
			known: []string{"a"},
			reads: []statusRead{
				{aurora: true, readers: map[string]readerSample{"a": {}, "b": {}}},
				{aurora: true, readers: map[string]readerSample{"a": {}, "b": {}}},
			},
			calls:      2,
			checkSaves: true,
			wantSaved:  [][]string{{"a", "b"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := tt.reads
			var n int
			var sleeps []time.Duration
			var logs []string
			var saved [][]string
			known := make(map[string]bool, len(tt.known))
			for _, id := range tt.known {
				known[id] = true
			}
			p := &pacer{
				max: tt.max,
				status: func(context.Context) (map[string]readerSample, bool, error) {
					if n >= len(reads) {
						t.Fatalf("unexpected status read %d", n)
					}
					r := reads[n]
					n++
					return r.readers, r.aurora, r.err
				},
				sleep: func(d time.Duration) { sleeps = append(sleeps, d) },
				logf:  func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
				known: known,
				save: func(_ context.Context, ids []string) error {
					saved = append(saved, append([]string(nil), ids...))
					return nil
				},
			}
			calls := tt.calls
			if calls == 0 {
				calls = 1
			}
			var err error
			for i := 0; i < calls; i++ {
				err = p.wait(context.Background())
				if err != nil {
					break
				}
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if n != len(reads) {
				t.Fatalf("status reads = %d, want %d", n, len(reads))
			}
			if len(sleeps) != tt.wantSleeps {
				t.Fatalf("sleeps = %d, want %d", len(sleeps), tt.wantSleeps)
			}
			for _, d := range sleeps {
				if d != time.Second {
					t.Fatalf("sleep duration = %s, want 1s", d)
				}
			}
			if tt.wantLogs == nil {
				tt.wantLogs = []string{}
			}
			if logs == nil {
				logs = []string{}
			}
			if !reflect.DeepEqual(logs, tt.wantLogs) {
				t.Fatalf("logs = %#v, want %#v", logs, tt.wantLogs)
			}
			if tt.checkSaves && !reflect.DeepEqual(saved, tt.wantSaved) {
				t.Fatalf("saved = %#v, want %#v", saved, tt.wantSaved)
			}
		})
	}
}

func TestPacerLongPauseRepeats(t *testing.T) {
	const behind = 61
	reads := make([]statusRead, behind+1)
	for i := 0; i < behind; i++ {
		reads[i] = statusRead{aurora: true, readers: map[string]readerSample{}}
	}
	reads[behind] = statusRead{aurora: true, readers: map[string]readerSample{"reader-a": {}}}

	var n int
	var logs []string
	var sleeps int
	p := &pacer{
		max: time.Second,
		status: func(context.Context) (map[string]readerSample, bool, error) {
			r := reads[n]
			n++
			return r.readers, r.aurora, nil
		},
		sleep: func(time.Duration) { sleeps++ },
		logf:  func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		known: map[string]bool{"reader-a": true},
		save:  func(context.Context, []string) error { return nil },
	}
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pausing the copy: reader reader-a stopped reporting",
		"pausing the copy: reader reader-a stopped reporting (paused for 1m0s); if reader-a was removed on purpose, rerun with -forget-readers",
		"resuming the copy after 1m1s",
	}
	if !reflect.DeepEqual(logs, want) {
		t.Fatalf("logs = %#v, want %#v", logs, want)
	}
	if sleeps != behind {
		t.Fatalf("sleeps = %d, want %d", sleeps, behind)
	}
}

type statusRead struct {
	readers map[string]readerSample
	aurora  bool
	err     error
}
