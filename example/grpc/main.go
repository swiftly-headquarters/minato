package main

import (
	"log"
	"os"

	"github.com/swiftly-headquarters/minato/example/app"
)

func main() {
	api, _ := app.New()
	if len(os.Args) > 1 && os.Args[1] == "proto" {
		if len(os.Args) > 2 {
			if err := os.WriteFile(os.Args[2], []byte(api.ProtoFile()), 0o644); err != nil {
				log.Fatal(err)
			}
			return
		}
		os.Stdout.WriteString(api.ProtoFile())
		return
	}
	app.Run(api)
}
