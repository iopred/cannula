package main

import (
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/iopred/cannula"
)

func main() {
	c, err := cannula.New()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, os.Kill)
	go func() {
		<-ch
		c.Exit()
		time.Sleep(time.Second)
		os.Exit(0)
	}()

	c.Listen(":6667")
}
