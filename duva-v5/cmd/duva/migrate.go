package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/compose"
	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
)

// importJSON brings v4's state into the database, once: queue.json (what was
// waiting) and detector.json (the watcher's cutoffs and held findings), from
// the directory the database lives in. Each file is renamed .migrated after
// it is read, not deleted, so going back to v4 still has it.
//
// Into an empty database only. A database that already has state is the
// truth, and a stale file copied in later must not overwrite it -- it is
// renamed anyway, with a warning, so it stops being asked about.
func importJSON(st *store.Store, dir, root string, log zerolog.Logger) error {
	if err := importQueue(st, filepath.Join(dir, "queue.json"), root, log); err != nil {
		return fmt.Errorf("importing queue.json: %w", err)
	}
	if err := importWatcher(st, filepath.Join(dir, "detector.json"), log); err != nil {
		return fmt.Errorf("importing detector.json: %w", err)
	}
	return nil
}

func empty(st *store.Store, table string) (bool, error) {
	var n int
	err := st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	return n == 0, err
}

func retire(path string, log zerolog.Logger) {
	if err := os.Rename(path, path+".migrated"); err != nil {
		log.Warn().Msgf("could not rename %s after importing it: %v", path, err)
	}
}

func importQueue(st *store.Store, path, root string, log zerolog.Logger) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fresh, err := empty(st, "queue_entries"); err != nil {
		return err
	} else if !fresh {
		log.Warn().Msgf("%s is here, but the database already has a queue; leaving it unread", path)
		retire(path, log)
		return nil
	}
	var entries map[string]q.Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	// v4 kept no container names; the compose file has them.
	containers := map[string]string{}
	if index, err := compose.ContainerIndex(root); err == nil {
		for container, ref := range index {
			containers[ref.Service] = container
		}
	}
	queue := q.Over(st, func(err error) { log.Error().Msgf("importing an entry: %v", err) })
	for service, e := range entries {
		e.Service = service
		e.Container = containers[service]
		queue.Put(e, time.Now())
	}
	log.Info().Msgf("imported %d waiting entr(ies) from %s", len(entries), path)
	retire(path, log)
	return nil
}

// v4's detector.json, as it wrote it.
type v4Detector struct {
	Services map[string]struct {
		Cutoff  time.Time                `json:"cutoff"`
		Pending map[string]watch.Finding `json:"pending"`
	} `json:"services"`
}

func importWatcher(st *store.Store, path string, log zerolog.Logger) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fresh, err := empty(st, "watch_cutoffs"); err != nil {
		return err
	} else if !fresh {
		log.Warn().Msgf("%s is here, but the database already has the watcher's cutoffs; leaving it unread", path)
		retire(path, log)
		return nil
	}
	var d v4Detector
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	err = st.Tx(func(tx *sql.Tx) error {
		for service, s := range d.Services {
			if !s.Cutoff.IsZero() {
				if _, err := tx.Exec(`INSERT INTO watch_cutoffs (service, cutoff) VALUES (?, ?)`,
					service, s.Cutoff.UTC().Format(time.RFC3339Nano)); err != nil {
					return err
				}
			}
			for tag, f := range s.Pending {
				raw, err := json.Marshal(f)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO watch_held (service, tag, finding) VALUES (?, ?, ?)`,
					service, tag, string(raw)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	log.Info().Msgf("imported the watcher's memory of %d service(s) from %s", len(d.Services), path)
	retire(path, log)
	return nil
}
