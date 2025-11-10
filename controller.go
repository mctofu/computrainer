package computrainer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// TODO: On shutdown keeps trying to reconnect to computrainer until client disconnects

// Connection allows communications with the Computrainer. Read the Messages
// channel to get metric updates and call SetLoad to adjust resistance.
type Connection struct {
	Messages        <-chan Message
	loadUpdates     chan int32
	cancelChan      chan struct{}
	recalibrateChan chan struct{}
	runningWG       sync.WaitGroup
	closeErr        error
}

// SetLoad sets the load in watts that the CompuTrainer should maintain in erg mode
func (c *Connection) SetLoad(targetLoad int32) {
	select {
	case c.loadUpdates <- targetLoad:
		// ok
	default: // todo: timeout?
		fmt.Println("Connection unable to set load")
	}
}

// Recalibrate allows recalibration by temporarily disconnecting from the CompuTrainer
// for 20 seconds.
func (c *Connection) Recalibrate(ctx context.Context) {
	select {
	case c.recalibrateChan <- struct{}{}:
		// ok
	default:
		log.Println("Ignoring Recalibrate: recalibration already requested")
	}
}

// Close communications with the CompuTrainer
func (c *Connection) Close() error {
	close(c.cancelChan)
	c.runningWG.Wait()
	return c.closeErr
}

// Controller is a higher level interface over Driver that handles auto
// reconnection.
type Controller struct {
	driver *Driver
}

// Start opens the specified comPort and connects to the
// CompuTrainer.
//
//gocyclo:ignore
func (c *Controller) Start(comPort string) (*Connection, error) {
	driver, err := NewDriver(comPort)
	if err != nil {
		return nil, fmt.Errorf("failed to create driver: %v", err)
	}
	c.driver = driver

	msgChan := make(chan Message)
	conn := &Connection{
		Messages:        msgChan,
		loadUpdates:     make(chan int32, 5),
		recalibrateChan: make(chan struct{}, 1),
		cancelChan:      make(chan struct{}),
	}

	errChan := make(chan error, 1)

	conn.runningWG.Add(1)
	go func() {
		defer conn.runningWG.Done()

		sigsChan := make(chan *Signals)
		for {
			var loopWG sync.WaitGroup

			retry := func() bool {
				log.Println("Connect Loop")
				ctx := context.Background()
				connectCtx, connectTimeout := context.WithTimeout(ctx, 1*time.Second)
				defer connectTimeout()
				connectCtx, connectCancel := context.WithCancel(connectCtx)
				defer connectCancel()

				readCtx, readCancel := context.WithCancel(ctx)
				defer readCancel()

				var connectWG sync.WaitGroup
				connectWG.Add(1)
				loopWG.Add(1)
				go func() {
					defer loopWG.Done()
					defer connectWG.Done()
					sigs, err := c.driver.Connect(connectCtx)
					if errors.Is(err, context.Canceled) {
						return
					}
					if err != nil {
						errChan <- err
						return
					}
					sigsChan <- sigs
				}()

				closer := func() error {
					connectCancel()
					connectWG.Wait()
					readCancel()
					log.Printf("Close driver\n")
					return c.driver.Close()
				}

				// readloop
				for {
					select {
					case <-conn.cancelChan:
						log.Printf("Cancel\n")
						conn.closeErr = closer()
						// no more retries as we are stopping
						return false
					case <-conn.recalibrateChan:
						log.Printf("Dropping connection for recalibration\n")
						conn.closeErr = closer()
						t := time.NewTimer(20 * time.Second)
						reconnect := func() bool {
							for {
								select {
								case <-t.C:
									// reconnect
									return true
								case <-conn.cancelChan:
									return false
								case <-conn.recalibrateChan:
									// ignore, already recalibrating
								}
							}
						}()
						if reconnect {
							return true
						}
					case sigs := <-sigsChan:
						log.Println("Got signals")
						// connected. start copying data
						loopWG.Add(1)
						go func() {
							defer loopWG.Done()
							defer sigs.Close()
							for {
								select {
								case <-readCtx.Done():
									return
								case targetLoad := <-conn.loadUpdates:
									sigs.SetLoad(targetLoad)
								case msg := <-sigs.Messages:
									select {
									case <-conn.cancelChan:
										return
									case msgChan <- msg:
										// sent
									}
								case err := <-sigs.Errors:
									select {
									case <-conn.cancelChan:
										return
									case errChan <- err:
										// sent
										return
									}
								}
							}
						}()
					case err := <-errChan:
						log.Printf("Reconnecting on err: %v\n", err)
						// retry
						return true
					}
				}
			}()
			// make sure connector/reader has finished
			loopWG.Wait()
			select {
			case err := <-errChan:
				log.Printf("warning: drained err: %v\n", err)
			default:
				// continue
			}
			if !retry {
				return
			}
		}
	}()

	return conn, nil
}
