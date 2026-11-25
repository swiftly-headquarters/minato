package main

import (
	"embed"
	"io/fs"
	"log"
	"os"

	"github.com/swiftly-headquarters/minato/server"
	"github.com/swiftly-headquarters/minato/web"
)

//go:embed dist
var dist embed.FS

func registerWeb(api *server.API) {
	if target := os.Getenv("WEB_PROXY"); target != "" {
		h, err := web.Proxy(target)
		if err != nil {
			log.Fatal(err)
		}
		server.Handle(api, "/", h)
		return
	}
	build, err := fs.Sub(dist, "dist")
	if err != nil {
		log.Fatal(err)
	}
	server.Handle(api, "/", web.Static(build, web.Config{SPA: true, Immutable: "/assets/"}))
}
