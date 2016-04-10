package main

import (
	"fmt"
	"os"

	"github.com/iopred/cannula"
)

func main() {
	c, err := cannula.New()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	c.Listen(":6667")
}
