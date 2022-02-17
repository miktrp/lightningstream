package syncer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/PowerDNS/lmdb-go/lmdb"
	"github.com/c2h5oh/datasize"
	"github.com/sirupsen/logrus"
	"powerdns.com/platform/lightningstream/lmdbenv"
	"powerdns.com/platform/lightningstream/lmdbenv/stats"
	"powerdns.com/platform/lightningstream/snapshot"
	"powerdns.com/platform/lightningstream/utils"
)

// HeaderSize is the size of the timestamp header for each LMDB value in bytes
const HeaderSize = 8

const (
	// SyncDBIPrefix is the shared DBI name prefix for all special tables that
	// must not be synced.
	SyncDBIPrefix = "_sync"
	// SyncDBIShadowPrefix is the DBI name prefix of shadow databases.
	SyncDBIShadowPrefix = "_sync_"
)

// ErrNoTimestamp is returned when an entry does not contain a timestamp, or the
// timestamp is 0.
type ErrNoTimestamp struct {
	DBIName string
	Key     []byte
}

func (e ErrNoTimestamp) Error() string {
	k := utils.DisplayASCII(e.Key)
	return fmt.Sprintf("no timestamp for entry (dbi %s, key %s)", e.DBIName, k)
}

var hostname string

func init() {
	h, err := os.Hostname()
	if err != nil {
		return
	}
	hostname = h
}

var reUnsafe = regexp.MustCompile("[^a-zA-Z0-9-]")

// instanceID returns a safe instance name
func (s *Syncer) instanceID() string {
	n := s.c.Instance
	if n == "" {
		n = hostname
	}
	n = reUnsafe.ReplaceAllString(n, "-")
	return n
}

// instanceID returns a safe instance name
func (s *Syncer) generationID() string {
	return fmt.Sprintf("G-%016x", s.generation)
}

// openEnv opens the LMDB env with the right options
func (s *Syncer) openEnv() (env *lmdb.Env, err error) {
	s.l.WithField("lmdbpath", s.lc.Path).Info("Opening LMDB")
	env, err = lmdbenv.NewWithOptions(s.lc.Path, s.lc.Options)
	if err != nil {
		return nil, err
	}

	// Print some env info
	info, err := env.Info()
	if err != nil {
		return nil, err
	}
	s.l.WithFields(logrus.Fields{
		"MapSize":   datasize.ByteSize(info.MapSize).HumanReadable(),
		"LastTxnID": info.LastTxnID,
	}).Info("Env info")

	// TODO: Perhaps check data if SchemaTracksChanges is set. Check if
	//       the timestamp is in a reasonable range or 0.

	return env, nil
}

// closeEnv closes the LMDB env, logging any unexpected errors for easy defer
func (s *Syncer) closeEnv(env *lmdb.Env) {
	if err := env.Close(); err != nil {
		s.l.WithError(err).Warn("Env close returned error")
	}
}

// readDBI reads a DBI into a snapshot DBI.
// By default, the timestamp of values will be split out to the TimestampNano field.
// If rawValues is true, the value will be stored as is and the timestamp will
// not be extracted. This is useful when reading a database without timestamps.
func (s *Syncer) readDBI(txn *lmdb.Txn, dbiName string, rawValues bool) (dbiMsg *snapshot.DBI, err error) {
	l := s.l.WithField("dbi", dbiName)

	l.Debug("Opening DBI")
	dbi, err := txn.OpenDBI(dbiName, 0)
	if err != nil {
		return nil, err
	}

	stat, err := txn.Stat(dbi)
	if err != nil {
		return nil, err
	}
	l.WithField("entries", stat.Entries).Debug("Reading DBI")

	dbiMsg = new(snapshot.DBI)
	dbiMsg.Name = dbiName
	dbiMsg.Entries = make([]snapshot.KV, 0, stat.Entries)
	// TODO: directly read it into the right structure
	items, err := lmdbenv.ReadDBI(txn, dbi)
	if err != nil {
		return nil, err
	}

	dbiFlags, err := txn.Flags(dbi)
	if err != nil {
		return nil, err
	}
	isDupSort := dbiFlags&lmdb.DupSort > 0
	if isDupSort && !s.lc.DupSortHack {
		return nil, fmt.Errorf("dupsort db %s found and dupsort_hack disabled", dbiName)
	}
	dbiMsg.Flags = uint64(dbiFlags)

	var prev []byte
	for _, item := range items {
		// Not checking wrong order to support native integer and reverse ordering
		if prev != nil && !isDupSort && bytes.Equal(prev, item.Key) {
			return nil, fmt.Errorf(
				"duplicate key detected in DBI %q without dupsort_hack, refusing to continue",
				dbiName)
		}
		prev = item.Key
		val := item.Val
		var ts uint64
		if !rawValues {
			if len(val) < HeaderSize {
				return nil, ErrNoTimestamp{
					DBIName: dbiName,
					Key:     item.Key,
				}
			}
			ts = binary.BigEndian.Uint64(val[:HeaderSize])
			val = val[HeaderSize:]
		}
		dbiMsg.Entries = append(dbiMsg.Entries, snapshot.KV{
			Key:           item.Key,
			Value:         val,
			TimestampNano: ts,
		})
	}

	return dbiMsg, nil
}

func (s *Syncer) startStatsLogger(ctx context.Context, env *lmdb.Env) {
	// Log LMDB stats every configured interval
	interval := s.c.LMDBLogStatsInterval
	if interval <= 0 {
		s.l.Info("LMDB stats logging disabled")
		return
	}
	s.l.WithField("interval", interval).Info("Enabled LMDB stats logging")
	go func() {
		logStatsTicker := time.NewTicker(interval)
		defer logStatsTicker.Stop()
		logStatsCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		for {
			select {
			case <-logStatsCtx.Done():
				return
			case <-logStatsTicker.C:
				// Skip the meta db, not that interesting
				stats.Log(env, nil, s.c.LMDBScrapeSmaps, s.l)
			}
		}
	}()

}
