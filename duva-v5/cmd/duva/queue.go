package main

import (
	"encoding/json"
	"fmt"
	"os"

	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
)

// dataFile is duva's database, in the one /data volume.
const dataFile = "/data/duva.db"

// listQueue prints what is waiting, by service, as JSON: what the page
// shows, for a terminal or a script.
//
// Read from the database while serve runs. WAL lets a second process read
// without stopping the writer, so this is safe on a live duva.
func listQueue(path string) error {
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	byService := map[string]q.Entry{}
	for _, e := range q.Over(st, nil).List() {
		byService[e.Service] = e
	}
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	if err := out.Encode(byService); err != nil {
		return fmt.Errorf("writing the queue: %w", err)
	}
	return nil
}
