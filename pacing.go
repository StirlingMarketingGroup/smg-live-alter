package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// A healthy Aurora reader's replica_host_status row is refreshed about once a
// second, so a second or two of staleness is not lag. Ten seconds without a
// refresh means the reader stopped reporting.
const readerSilentAfter = 10 * time.Second

const replicaLagSQL = "select SERVER_ID, ifnull(REPLICA_LAG_IN_MILLISECONDS,0), timestampdiff(microsecond, LAST_UPDATE_TIMESTAMP, now(6)) div 1000 from information_schema.replica_host_status where SESSION_ID<>'MASTER_SESSION_ID'"

type readerSample struct {
	lag   time.Duration
	since time.Duration
}

func readerStatus(ctx context.Context, conn *sql.Conn) (map[string]readerSample, bool, error) {
	rows, err := conn.QueryContext(ctx, replicaLagSQL)
	if err != nil {
		var me *mysql.MySQLError
		// Plain MySQL has no replica_host_status (error 1109). Pacing does not apply there.
		if errors.As(err, &me) && me.Number == 1109 {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rows.Close()

	readers := make(map[string]readerSample)
	for rows.Next() {
		var id string
		var lagMS, sinceMS int64
		if err := rows.Scan(&id, &lagMS, &sinceMS); err != nil {
			return nil, false, err
		}
		readers[id] = readerSample{
			lag:   time.Duration(lagMS) * time.Millisecond,
			since: time.Duration(sinceMS) * time.Millisecond,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return readers, true, nil
}

type pacer struct {
	max    time.Duration
	status func(context.Context) (map[string]readerSample, bool, error)
	sleep  func(time.Duration)
	logf   func(format string, args ...any)
	known  map[string]bool                       // readers this run has seen, persisted in the run table
	save   func(context.Context, []string) error // persists known when it grows
	off    bool                                  // max is 0, or the server is not Aurora
}

type lagReason struct {
	id      string
	missing bool
	silent  bool
	lag     time.Duration
	since   time.Duration
}

func (p *pacer) wait(ctx context.Context) error {
	if p.off || p.max == 0 {
		p.off = true
		return nil
	}
	var (
		pausing  bool
		paused   time.Duration
		loggedAt time.Duration
	)
	for {
		readers, aurora, err := p.status(ctx)
		if err != nil {
			return err
		}
		if !aurora {
			p.logf("replica lag pacing is off: the server has no Aurora replica status")
			p.off = true
			return nil
		}
		if err := p.noteReaders(ctx, readers); err != nil {
			return err
		}
		reason, behind := p.behind(readers)
		if !behind {
			if pausing {
				p.logf("resuming the copy after %s", paused)
			}
			return nil
		}
		if !pausing {
			p.logf("%s", reason.line(p.max, 0, false))
			pausing = true
			loggedAt = paused
		} else if paused-loggedAt >= 60*time.Second {
			p.logf("%s", reason.line(p.max, paused, true))
			loggedAt = paused
		}
		p.sleep(time.Second)
		paused += time.Second
	}
}

func (p *pacer) noteReaders(ctx context.Context, readers map[string]readerSample) error {
	if p.known == nil {
		p.known = map[string]bool{}
	}
	grown := false
	for id := range readers {
		if !p.known[id] {
			p.known[id] = true
			grown = true
		}
	}
	if !grown {
		return nil
	}
	return p.save(ctx, knownReaderIDs(p.known))
}

// A reader that disappeared is restarting or being replaced. It stays in the
// known set until a run is started with -forget-readers.
func (p *pacer) behind(readers map[string]readerSample) (lagReason, bool) {
	var missing []string
	for id := range p.known {
		if _, ok := readers[id]; !ok {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return lagReason{id: missing[0], missing: true}, true
	}
	var silent, worst lagReason
	for id, sample := range readers {
		if sample.since > readerSilentAfter {
			if silent.id == "" || sample.since > silent.since || (sample.since == silent.since && id < silent.id) {
				silent = lagReason{id: id, silent: true, since: sample.since}
			}
		}
		if sample.lag <= p.max {
			continue
		}
		if worst.id == "" || sample.lag > worst.lag || (sample.lag == worst.lag && id < worst.id) {
			worst = lagReason{id: id, lag: sample.lag}
		}
	}
	if silent.id != "" {
		return silent, true
	}
	if worst.id == "" {
		return lagReason{}, false
	}
	return worst, true
}

func (r lagReason) line(max, paused time.Duration, repeat bool) string {
	if r.missing {
		msg := fmt.Sprintf("pausing the copy: reader %s stopped reporting", r.id)
		if repeat {
			msg += fmt.Sprintf(" (paused for %s); if %s was removed on purpose, rerun with -forget-readers", paused, r.id)
		}
		return msg
	}
	if r.silent {
		msg := fmt.Sprintf("pausing the copy: reader %s stopped reporting %s ago", r.id, r.since)
		if repeat {
			msg += fmt.Sprintf(" (paused for %s)", paused)
		}
		return msg
	}
	msg := fmt.Sprintf("pausing the copy: reader %s lag %s is over %s", r.id, r.lag, max)
	if repeat {
		msg += fmt.Sprintf(" (paused for %s)", paused)
	}
	return msg
}

func knownReaderIDs(known map[string]bool) []string {
	ids := make([]string, 0, len(known))
	for id := range known {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
