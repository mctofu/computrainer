// test app to connect to computrainer and print data to the console
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mctofu/computrainer"
	serial "go.bug.st/serial.v1"
)

func main() {
	ports, err := serial.GetPortsList()
	if err != nil {
		log.Fatalf("Couldn't get port list: %v\n", err)
	}

	for _, port := range ports {
		log.Printf("Port: %v\n", port)
	}

	if len(os.Args) <= 1 {
		log.Printf("Specify port to continue\n")
		return
	}

	port := os.Args[1]

	controller := &computrainer.Controller{}

	ct, err := controller.Start(port)
	if err != nil {
		log.Fatalf("Couldn't create computrainer: %v\n", err)
	}
	defer func() {
		log.Printf("Closing computrainer\n")
		if err := ct.Close(); err != nil {
			log.Printf("Failed to close computrainer: %v", err)
		}
	}()

	log.Printf("Started!\n")

	exitSignal := make(chan os.Signal)
	signal.Notify(exitSignal, syscall.SIGTERM, syscall.SIGINT)

	var targetLoad int32
	targetLoad = 150

	var lastPower uint16
	var lastSpeed float64
	var lastRRC float64

	for {
		select {
		case <-exitSignal:
			return
		case msg := <-ct.Messages:
			switch msg.Type {
			case computrainer.DataSpeed:
				speed := float64(msg.Value) * .01 * .9 * 2.237 // * 2.237 m/s to mph
				if speed != lastSpeed {
					fmt.Printf("Speed %f mph\n", speed)
					lastSpeed = speed
				}
			case computrainer.DataPower:
				power := msg.Value
				if power != lastPower {
					fmt.Printf("Power %d watts\n", power)
					lastPower = power
				}
			case computrainer.DataRRC:
				rrc := float64(msg.Value) / 256
				if rrc != lastRRC {
					fmt.Printf("RRC %f\n", rrc)
					lastRRC = rrc
				}
			}

			switch {
			case msg.Buttons&computrainer.ButtonPlus > 0:
				targetLoad += 5
				ct.SetLoad(targetLoad)
			case msg.Buttons&computrainer.ButtonMinus > 0:
				targetLoad -= 5
				ct.SetLoad(targetLoad)
			}
		}
	}
}
