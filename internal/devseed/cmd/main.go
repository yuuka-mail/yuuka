// Command devseed loads the synthetic corpus into a development database.
// Dev only; see package devseed.
//
//	YUUKA_DATABASE_URL=postgres://... go run ./internal/devseed/cmd -n 2000
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/devseed"
	"github.com/yuuka-mail/yuuka/internal/store"
)

func main() {
	dir := flag.String("dir", "testdata/eml", "corpus directory")
	n := flag.Int("n", 2000, "generated messages on top of the corpus")
	seed := flag.Uint64("seed", 1, "seed of the generated spread")
	flag.Parse()
	if err := run(*dir, *n, *seed); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

func run(dir string, n int, seed uint64) error {
	url := os.Getenv("YUUKA_DATABASE_URL")
	if url == "" {
		return fmt.Errorf("YUUKA_DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	db := store.New(pool)
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	start := time.Now()
	k, err := devseed.Load(ctx, db, pool, devseed.Options{Dir: dir, Variants: n, Seed: seed})
	fmt.Printf("devseed: %d deliveries in %s\n", k, time.Since(start).Round(time.Millisecond))
	return err
}
