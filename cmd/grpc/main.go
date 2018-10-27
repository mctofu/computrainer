// Connects to a computrainer and starts a grpc server to allow remote
// communications
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/mctofu/computrainer"
	serial "go.bug.st/serial.v1"
	"google.golang.org/grpc"
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

	server := NewServer(ct)

	debugOutput := addDebug(server)
	defer debugOutput.Close()

	// create a listener on TCP port
	lis, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()

	computrainer.RegisterControllerServer(grpcServer, server)

	var serveWG sync.WaitGroup

	serveWG.Add(1)
	go func() {
		defer serveWG.Done()
		err := grpcServer.Serve(lis)
		if err != nil {
			log.Printf("Unexpected server err: %v\n", err)
		}
	}()

	select {
	case <-exitSignal:
		close(server.ShutdownChan)
		grpcServer.GracefulStop()
		serveWG.Wait()
	}
}

func addDebug(server *Server) *MessageBroadcast {
	b := server.NewBroadcast()

	go func() {
		// TODO: dry up code between here and server -> client code.
		var lastPower uint16
		var lastSpeed float64
		var lastRRC float64

		for msg := range b.Messages {
			switch msg.Type {
			case computrainer.DataSpeed:
				speed := float64(msg.Value) * .01 * .9 // m/s * 2.237 for m/s to mph
				if speed != lastSpeed {
					fmt.Printf("Speed %f m/s\n", speed)
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
				fmt.Printf("Plus\n")
			case msg.Buttons&computrainer.ButtonMinus > 0:
				fmt.Printf("Minus\n")
			case msg.Buttons&computrainer.ButtonReset > 0:
				fmt.Printf("reset\n")
			case msg.Buttons&computrainer.ButtonF1 > 0:
				fmt.Printf("f1\n")
			}
		}
	}()

	return b
}

type MessageBroadcast struct {
	Messages    chan computrainer.Message
	broadcaster *MessageBroadcaster
}

func (m *MessageBroadcast) Close() {
	m.broadcaster.removeBroadcast(m)
	close(m.Messages)
}

type MessageBroadcaster struct {
	lck        sync.Mutex
	broadcasts map[*MessageBroadcast]struct{}
	messages   <-chan computrainer.Message
}

func (m *MessageBroadcaster) Start() {
	m.broadcasts = make(map[*MessageBroadcast]struct{})
	go func() {
		for msg := range m.messages {
			func() {
				defer m.lck.Unlock()
				m.lck.Lock()
				for b := range m.broadcasts {
					select {
					case b.Messages <- msg:
						// ok
					default:
						log.Println("MessageBroadcaster: Dropping broadcast message. Channel is full.")
					}
				}
			}()
		}

		log.Println("MessageBroadcaster: channel closed. stopping.")
	}()
}

func (m *MessageBroadcaster) Stop() {
	// TODO: impl
}

func (m *MessageBroadcaster) NewBroadcast() *MessageBroadcast {
	defer m.lck.Unlock()
	m.lck.Lock()
	b := &MessageBroadcast{
		Messages:    make(chan computrainer.Message, 100),
		broadcaster: m,
	}
	m.broadcasts[b] = struct{}{}
	return b
}

func (m *MessageBroadcaster) removeBroadcast(b *MessageBroadcast) {
	defer m.lck.Unlock()
	m.lck.Lock()
	delete(m.broadcasts, b)
}

type Server struct {
	Connection   *computrainer.Connection
	ShutdownChan chan struct{}
	broadcaster  *MessageBroadcaster
}

func NewServer(c *computrainer.Connection) *Server {
	b := &MessageBroadcaster{
		messages: c.Messages,
	}

	b.Start()

	return &Server{
		Connection:   c,
		ShutdownChan: make(chan struct{}),
		broadcaster:  b,
	}
}

func (s *Server) NewBroadcast() *MessageBroadcast {
	return s.broadcaster.NewBroadcast()
}

func (s *Server) GetData(req *computrainer.DataRequest, dataServer computrainer.Controller_GetDataServer) error {
	var lastPower uint16
	var lastSpeed float64

	ctx := dataServer.Context()
	fmt.Printf("Client start\n")

	b := s.broadcaster.NewBroadcast()
	defer b.Close()

	for {
		select {
		case <-s.ShutdownChan:
			fmt.Printf("Client stopping. Server shutting down\n")
			return fmt.Errorf("server shutdown")
		case <-ctx.Done():
			fmt.Printf("Client done\n")
			return ctx.Err()
		case msg := <-b.Messages:
			switch msg.Type {
			case computrainer.DataSpeed:
				speed := float64(msg.Value) * .01 * .9 // m/s * 2.237 for m/s to mph
				if speed != lastSpeed {
					lastSpeed = speed
					err := dataServer.Send(&computrainer.ControllerData{
						PerformanceData: &computrainer.PerformanceData{
							Type:  computrainer.PerformanceData_SPEED,
							Value: float32(speed),
						},
					})
					if err != nil {
						return fmt.Errorf("error sending speed: %v", err)
					}
				}
			case computrainer.DataPower:
				power := msg.Value
				if power != lastPower {
					lastPower = power
					err := dataServer.Send(&computrainer.ControllerData{
						PerformanceData: &computrainer.PerformanceData{
							Type:  computrainer.PerformanceData_POWER,
							Value: float32(power),
						},
					})
					if err != nil {
						return fmt.Errorf("error sending power: %v", err)
					}
				}
			case computrainer.DataRRC:
				rrc := float64(msg.Value) / 256
				err := dataServer.Send(&computrainer.ControllerData{
					PerformanceData: &computrainer.PerformanceData{
						Type:  computrainer.PerformanceData_CALIBRATION,
						Value: float32(rrc),
					},
				})
				if err != nil {
					return fmt.Errorf("error sending calibration: %v", err)
				}
			}

			switch {
			case msg.Buttons&computrainer.ButtonPlus > 0:
				err := dataServer.Send(&computrainer.ControllerData{
					ControlData: &computrainer.ControlData{
						Pressed: []computrainer.ControlData_Button{computrainer.ControlData_PLUS},
					},
				})
				if err != nil {
					return fmt.Errorf("error sending button: %v", err)
				}
			case msg.Buttons&computrainer.ButtonMinus > 0:
				err := dataServer.Send(&computrainer.ControllerData{
					ControlData: &computrainer.ControlData{
						Pressed: []computrainer.ControlData_Button{computrainer.ControlData_MINUS},
					},
				})
				if err != nil {
					return fmt.Errorf("error sending button: %v", err)
				}
			case msg.Buttons&computrainer.ButtonReset > 0:
				err := dataServer.Send(&computrainer.ControllerData{
					ControlData: &computrainer.ControlData{
						Pressed: []computrainer.ControlData_Button{computrainer.ControlData_RESET},
					},
				})
				if err != nil {
					return fmt.Errorf("error sending button: %v", err)
				}
			case msg.Buttons&computrainer.ButtonF1 > 0:
				err := dataServer.Send(&computrainer.ControllerData{
					ControlData: &computrainer.ControlData{
						Pressed: []computrainer.ControlData_Button{computrainer.ControlData_F1},
					},
				})
				if err != nil {
					return fmt.Errorf("error sending button: %v", err)
				}
			}
		}
	}
}

func (s *Server) SetLoad(ctx context.Context, req *computrainer.LoadRequest) (*computrainer.LoadResponse, error) {
	s.Connection.SetLoad(req.TargetLoad)
	return &computrainer.LoadResponse{}, nil
}
