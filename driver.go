package computrainer

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jacobsa/go-serial/serial"
)

const (
	// LoadMax is the maximum load that can be set (watts)
	LoadMax int32 = 1500
	// LoadMin is the maximum load that can be set (watts)
	LoadMin int32 = 50
)

// ergoInitCommand puts the computrainer into ergo mode
var ergoInitCommand = []byte{
	0x6D, 0x00, 0x00, 0x0A, 0x08, 0x00, 0xE0,
	0x65, 0x00, 0x00, 0x0A, 0x10, 0x00, 0xE0,
	0x00, 0x00, 0x00, 0x0A, 0x18, 0x5D, 0xC1,
	0x33, 0x00, 0x00, 0x0A, 0x24, 0x1E, 0xE0,
	0x6A, 0x00, 0x00, 0x0A, 0x2C, 0x5F, 0xE0,
	0x41, 0x00, 0x00, 0x0A, 0x34, 0x00, 0xE0,
	0x2D, 0x00, 0x00, 0x0A, 0x38, 0x10, 0xC2,
}

// DisconnectError indicates we've lost the connection to the CompuTrainer
// or it's no longer responding. Reconnection will be necessary to continue.
type DisconnectError struct {
	Cause error
}

func (d DisconnectError) Error() string {
	return fmt.Sprintf("disconnected: %v", d.Cause)
}

// Signals exposes data being published by the CompuTrainer and allows controlling
// CompuTrainer settings
type Signals struct {
	Messages    <-chan Message
	Errors      <-chan error
	load        *int32
	cancelChan  chan struct{}
	connectedWG *sync.WaitGroup
}

// SetLoad sets the load in watts that the CompuTrainer should maintain in erg mode
func (s *Signals) SetLoad(targetLoad int32) {
	atomic.StoreInt32(s.load, targetLoad)
}

// Close disconnects from the CompuTrainer and prevents further reading/writing
func (s *Signals) Close() {
	log.Printf("Signals: close\n")
	close(s.cancelChan)
	s.connectedWG.Wait()
	log.Printf("Signals: closed\n")
}

type signaler struct {
	Messages chan<- Message
	Errors   chan<- error
}

// Driver handles serial communications with the CompuTrainer
type Driver struct {
	portName   string
	com        io.ReadWriteCloser
	targetLoad int32
}

// NewDriver returns a Driver using the specified com port
func NewDriver(comPort string) (*Driver, error) {
	return &Driver{portName: comPort, targetLoad: LoadMin}, nil
}

// Connect attempts to establish communications with the CompuTrainer. If successful
// then the returned Signals will allow interacting with the CompuTrainer.
// Signals should be closed before closing the Driver
func (d *Driver) Connect(ctx context.Context) (*Signals, error) {
	log.Printf("Driver: Connect\n")

	if d.com == nil {
		port, err := serial.Open(serial.OpenOptions{
			PortName:              d.portName,
			BaudRate:              2400,
			DataBits:              8,
			StopBits:              1,
			ParityMode:            serial.PARITY_NONE,
			InterCharacterTimeout: 1000,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to open serial: %v", err)
		}
		d.com = port
	}

	// Try to clean out any stale data in the read buffers from previous
	// connections.
	for {
		drainBuf := make([]byte, 6)
		n, err := d.com.Read(drainBuf)
		if err != nil {
			if err != io.EOF {
				return nil, fmt.Errorf("failed to read: %v", err)
			}
		}
		if n != 6 {
			break
		}
	}

	if _, err := io.WriteString(d.com, "RacerMate"); err != nil {
		return nil, fmt.Errorf("failed to send initial hello: %v", err)
	}

	result := make(chan error)
	go func() {
		buf := make([]byte, 6)
		if err := read(ctx, d.com, buf); err != nil {
			result <- fmt.Errorf("failed to read hello response: %w", err)
		}
		bufMsg := string(buf)
		if bufMsg != "LinkUp" {
			result <- fmt.Errorf("unexpected hello response: %s", bufMsg)
			return
		}
		close(result)
	}()

	select {
	case <-ctx.Done():
		// TODO: wait for read goroutine to end or close port?
		return nil, fmt.Errorf("failed to connect before timeout: %w", ctx.Err())
	case err := <-result:
		if err != nil {
			return nil, err
		}
	}

	if _, err := d.com.Write(ergoInitCommand); err != nil {
		return nil, fmt.Errorf("failed to write ergo init: %v", err)
	}

	var load int32
	msgChan := make(chan Message)
	errChan := make(chan error)
	cancelChan := make(chan struct{})
	connectedWG := &sync.WaitGroup{}

	d.startConnectedLoop(&signaler{msgChan, errChan}, &load, cancelChan, connectedWG)

	return &Signals{
		Messages:    msgChan,
		Errors:      errChan,
		load:        &load,
		cancelChan:  cancelChan,
		connectedWG: connectedWG,
	}, nil
}

func (d *Driver) startConnectedLoop(signaler *signaler, targetLoad *int32, cancel <-chan struct{}, connectedWG *sync.WaitGroup) {
	log.Printf("Driver: startConnectedLoop\n")
	buf := make([]byte, 7)
	loopCtx := context.Background()
	readMsgChan := make(chan Message)
	readErrChan := make(chan error)

	connectedWG.Add(1)
	go func() {
		defer connectedWG.Done()

		for i := 0; ; i++ {
			if i%4 == 0 {
				if err := writeControlMessage(d.com, atomic.LoadInt32(targetLoad)); err != nil {
					signaler.Errors <- DisconnectError{fmt.Errorf("failed to write load init: %v", err)}
					return
				}
			}

			ctx, ctxCancel := context.WithCancel(loopCtx)
			exit := func() bool {
				defer ctxCancel()
				go d.readTo(ctx, buf, readMsgChan, readErrChan)

				select {
				case <-cancel:
					ctxCancel()
					return true
				case msg := <-readMsgChan:
					if msg.Type != DataNone {
						signaler.Messages <- msg
					}
				case err := <-readErrChan:
					signaler.Errors <- DisconnectError{fmt.Errorf("failed to read message: %v", err)}
					return true
				}

				return false
			}()
			if exit {
				return
			}
		}
	}()
}

func (d *Driver) readTo(ctx context.Context, buf []byte, msgChan chan<- Message, errChan chan<- error) {
	if err := readWithTimeout(ctx, d.com, buf, 1*time.Second); err != nil {
		if err != context.Canceled {
			select {
			case <-ctx.Done():
				return
			case errChan <- err:
				return
			}
		}
		return
	}

	msg := ParseMessage(buf)
	select {
	case <-ctx.Done():
		return
	case msgChan <- msg:
		return
	}
}

func (d *Driver) setTargetLoad(load int32) {
	switch {
	case load > LoadMax:
		d.targetLoad = LoadMax
	case load < LoadMin:
		d.targetLoad = LoadMin
	default:
		d.targetLoad = load
	}
}

// Close releases the com port opened by the driver
func (d *Driver) Close() error {
	err := d.com.Close()
	d.com = nil
	return err
}

func readWithTimeout(ctx context.Context, r io.Reader, buf []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return read(ctx, r, buf)
}

func read(ctx context.Context, r io.Reader, buf []byte) error {
	for read := 0; read < len(buf); {
		n, err := r.Read(buf[read:])
		if err != nil {
			return fmt.Errorf("read error: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// keep going
		}
		read += n
	}
	return nil
}

func writeControlMessage(w io.Writer, targetLoad int32) error {
	msg := make([]byte, 7)

	crc := calcCRC(targetLoad)

	// BYTE 0 - 49 is b0, 53 is b4, 54 is b5, 55 is b6
	msg[0] = byte(crc >> 1) // set byte 0

	msg[3] = 0x0A

	// BYTE 4 - command and highbyte
	msg[4] = 0x40 // set command
	msg[4] |= byte((targetLoad & (2048 + 1024 + 512)) >> 9)

	// BYTE 5 - low 7
	msg[5] = 0
	msg[5] |= byte((targetLoad & (128 + 64 + 32 + 16 + 8 + 4 + 2)) >> 1)

	// BYTE 6 - sync + z set
	msg[6] = byte(128 + 64)

	// low bit of supplement in bit 6 (32)
	if (crc & 1) > 0 {
		msg[6] |= 32
	}
	// Bit 2 (0x02) is low bit of high byte in load (bit 9 0x256)
	if (targetLoad & 256) > 0 {
		msg[6] |= 2
	}
	// Bit 1 (0x01) is low bit of low byte in load (but 1 0x01)
	msg[6] |= byte(targetLoad & 1)

	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("failed to write control msg: %v", err)
	}

	return nil
}

func calcCRC(value int32) int32 {
	return (0xff & (107 - (value & 0xff) - (value >> 8)))
}
