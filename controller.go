package computrainer

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// TODO: On shutdown keeps trying to reconnect to computrainer until client disconnects

// Connection allows communications with the Computrainer. Read the Messages
// channel to get metric updates and call SetLoad to adjust resistance.
type Connection struct {
	Messages    <-chan Message
	loadUpdates chan int32
	cancelChan  chan struct{}
	runningWG   sync.WaitGroup
	closeErr    error
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
func (c *Controller) Start(comPort string) (*Connection, error) {
	driver, err := NewDriver(comPort)
	if err != nil {
		return nil, fmt.Errorf("failed to create driver: %v", err)
	}
	c.driver = driver

	msgChan := make(chan Message)
	conn := &Connection{
		Messages:    msgChan,
		loadUpdates: make(chan int32, 1),
		cancelChan:  make(chan struct{}),
	}

	errChan := make(chan error, 1)

	conn.runningWG.Add(1)
	go func() {
		defer conn.runningWG.Done()
		sigsChan := make(chan *Signals)
		for {
			retry := func() bool {
				ctx := context.Background()
				ctx, timeout := context.WithTimeout(ctx, 1*time.Second)
				defer timeout()
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()

				conn.runningWG.Add(1)
				go func() {
					defer conn.runningWG.Done()
					sigs, err := c.driver.Connect(ctx)
					if err != nil {
						errChan <- err
						return
					}
					sigsChan <- sigs
				}()

				// readloop
				for {
					select {
					case <-conn.cancelChan:
						log.Printf("Cancel\n")
						cancel()
						log.Printf("Close driver\n")
						conn.closeErr = c.driver.Close()
						// no more retries as we are stopping
						return false
					case sigs := <-sigsChan:
						// connected. start copying data
						conn.runningWG.Add(1)
						var copyWG sync.WaitGroup
						copyWG.Add(1)
						go func() {
							defer conn.runningWG.Done()
							defer copyWG.Done()
							defer sigs.Close()
							for {
								select {
								case <-conn.cancelChan:
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
						copyWG.Wait()
					case err := <-errChan:
						log.Printf("Closing/reconnect on err: %v", err)
						// retry
						return true
					}
				}
			}()
			if !retry {
				return
			}
		}
	}()

	return conn, nil
}
