package main

import (
	"fmt"
	"log"

	"local-ai-cli/internal/app"
)

func main() {
	cfg := app.DefaultConfig()

	db, lw := app.MustInit(cfg)
	defer lw.Close()
	defer db.Close()

	fmt.Printf("Web listening on http://%s/\n", cfg.HTTPAddr)

	if err := app.StartWeb(cfg, db, lw); err != nil {
		log.Fatal(err)
	}
}
