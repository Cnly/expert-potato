package main

import (
	"flag"
	"log"
	"math/rand"
	"neo181/core"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {

	rand.Seed(time.Now().UnixNano())

	var positionClient, positionServer bool
	var swapECMPositions bool
	var configFilename string

	flag.BoolVar(&positionClient, "c", false, "start as client")
	flag.BoolVar(&positionServer, "s", false, "start as server")
	flag.BoolVar(&swapECMPositions, "S", false, "swap ECM positions (ECM on server side listens)")
	flag.StringVar(&configFilename, "C", "", "json config file")

	flag.Parse()

	if (positionClient && positionServer) || (!positionClient && !positionServer) {
		log.Fatal("there should be one and only one position flag (-c or -s)")
	}

	if len(configFilename) == 0 {
		log.Fatal("no config file specified")
	}

	var position core.Position
	if positionClient {
		position = core.CLIENT
	} else {
		position = core.SERVER
	}

	conf, err := core.NewConfigFromFile(configFilename)
	if err != nil {
		log.Fatalf("error parsing config file (%v)", err)
	}

	sighupChan := make(chan os.Signal, 2)
	signal.Notify(sighupChan, syscall.SIGHUP, syscall.SIGINT)

	c := core.NewCore(position, swapECMPositions, conf)
	c.Start()

	<-sighupChan
	go func() {
		<-sighupChan
		os.Exit(1)
	}()
	log.Printf("stopping; press ctrl-c again to force stop")

	c.Stop()

}
