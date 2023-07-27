package main

import (
	"GuGoTik/src/constant/config"
	"GuGoTik/src/web/about"
	"github.com/gin-gonic/gin"
)

func main() {
	g := gin.Default()
	g.Use()

	// Register Service
	g.GET("/about", about.Handle)

	err := g.Run(config.WebServiceAddr)

	if err != nil {
		panic("Can not run GuGoTik Gateway, binding port: " + config.WebServiceAddr)
	}
}
