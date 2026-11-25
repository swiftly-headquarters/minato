package main

import (
	"log"
	"os"

	"github.com/swiftly-headquarters/minato/example/app"
	"github.com/swiftly-headquarters/minato/server"
)

func main() {
	api, metrics := app.New()
	if os.Getenv("OFFLINE_DOCS") != "" {
		api.Docs = api.OfflineDocs()
	}
	registerExtras(api)
	registerOps(api, metrics)
	registerWeb(api)

	if len(os.Args) > 1 && os.Args[1] == "openapi" {
		if len(os.Args) > 2 {
			if err := api.WriteSpec(os.Args[2]); err != nil {
				log.Fatal(err)
			}
			return
		}
		b, err := api.SpecJSON()
		if err != nil {
			log.Fatal(err)
		}
		os.Stdout.Write(b)
		return
	}
	app.Run(api)
}

func summary(s string) func(*server.Operation) {
	return func(o *server.Operation) { o.Summary = s }
}
