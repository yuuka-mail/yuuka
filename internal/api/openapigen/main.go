// Command openapigen writes the REST API's OpenAPI document (§5.1);
// go generate ./internal/api runs it. TestOpenAPI fails while the
// committed file is out of date.
package main

import (
	"flag"
	"log"
	"os"

	"github.com/yuuka-mail/yuuka/internal/api"
)

func main() {
	out := flag.String("o", "openapi.json", "output file")
	flag.Parse()
	doc, err := api.OpenAPI()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		log.Fatal(err)
	}
}
