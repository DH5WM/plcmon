package s7

import (
	"errors"
	"fmt"
	"github.com/robinson/gos7"
	"time"
)

const (
	// A05 S7-1200
	DefaultAddr = "192.168.1.10"
	spsRack     = 0
	spsSlot     = 0
	dbNum       = 8
	dbByte      = 0

	timeout = 5 * time.Second
)

/*
	Merker in DB8.DBX0.0

	0.0	Status Dauerlicht
	0.1	Flurlicht-Timer zurücksetzen	  (push button)
	0.2	Flurlicht-Taster simulieren       (push button)
	0.3	Flurlicht-Haltemerker umschalten  (push button)
	0.4	Außenlichtkopplung aktiv
	0.5	UDP-Datentelegrammversand aktiv
*/

type Merker uint8

const (
	BitDauerlicht Merker = iota
	BitFlurlichtTimerReset
	BitFlurlichtTaster
	BitFlurlichtHold
	BitAussenlichtKopplung
	BitUDPEnabled

	numBits
)

// ErrNotWritable is returned for every bit which is not on the whitelist.
var ErrNotWritable = errors.New("bit is not writable")

// writable is the whitelist of bits which may be written.
//
// BitUDPEnabled must never be written: clearing it stops the PLC from sending
// its UDP telegrams, which takes down the whole alarm notification path.
// BitDauerlicht and BitAussenlichtKopplung are deliberately not writable
// either. The Dauerlicht is switched like on site, with BitFlurlichtHold.
var writable = map[Merker]bool{
	BitFlurlichtTimerReset: true,
	BitFlurlichtTaster:     true,
	BitFlurlichtHold:       true,
}

// pulseBits are the bits which trigger an action when set to 1, rather than
// representing a state (marked "X" in messages.org).
var pulseBits = map[Merker]bool{
	BitFlurlichtTimerReset: true,
	BitFlurlichtTaster:     true,
	BitFlurlichtHold:       true,
}

func Writable(bit Merker) bool { return writable[bit] }
func IsPulse(bit Merker) bool  { return pulseBits[bit] }

func (m Merker) String() string {
	return fmt.Sprintf("DB%d.DBX%d.%d", dbNum, dbByte, uint8(m))
}

// bitAddress returns the address of a bit as expected by AGWriteDBBit, which
// counts in bits from the start of the DB (Snap7 semantics).
func bitAddress(byteOffset int, bit Merker) int {
	return byteOffset*8 + int(bit)
}

// BitIO reads and writes single bits of the control block.
type BitIO interface {
	ReadDBBit(bit Merker) (bool, error)
	WriteDBBit(bit Merker, state bool) error
}

// Pulse simulates a push button: it sets the bit, waits (hold should be longer
// than a PLC cycle) and clears the bit again, unless the PLC already did so.
func Pulse(io BitIO, bit Merker, hold time.Duration) error {
	if !IsPulse(bit) {
		return fmt.Errorf("%v is not a push button", bit)
	}

	if err := io.WriteDBBit(bit, true); err != nil {
		return fmt.Errorf("set %v: %w", bit, err)
	}

	time.Sleep(hold)

	// If the read fails, we do not know the state of the bit. Clear it
	// anyway, so that it is not left set.
	if set, err := io.ReadDBBit(bit); err == nil && !set {
		return nil
	}

	if err := io.WriteDBBit(bit, false); err != nil {
		return fmt.Errorf("clear %v: %w", bit, err)
	}

	return nil
}

// SetToggle brings a state to want, where the state can only be toggled with
// a push button. The button is pressed only if the state bit differs from
// want. If the state cannot be read, nothing is written, because a blind
// press could just as well toggle the state the wrong way.
func SetToggle(io BitIO, button Merker, state Merker, want bool, hold time.Duration) (pressed bool, err error) {
	current, err := io.ReadDBBit(state)
	if err != nil {
		return false, fmt.Errorf("read %v: %w", state, err)
	}
	if current == want {
		return false, nil
	}

	return true, Pulse(io, button, hold)
}

// dbClient is the subset of gos7.Client used here.
type dbClient interface {
	AGReadDB(dbNumber int, start int, size int, buffer []byte) error
	AGWriteDBBit(dbNumber int, start int, size int, buffer []byte) error
}

type A05PLC struct {
	handler *gos7.TCPClientHandler
	client  dbClient
}

// NewA05PLC returns a client for the PLC at addr (host or host:port, the port
// defaults to 102).
func NewA05PLC(addr string) *A05PLC {
	handler := gos7.NewTCPClientHandler(addr, spsRack, spsSlot)
	handler.Timeout = timeout
	handler.IdleTimeout = 30 * time.Second

	return &A05PLC{
		handler: handler,
		client:  gos7.NewClient(handler),
	}
}

func (a *A05PLC) Connect() error {
	return a.handler.Connect()
}

func (a *A05PLC) Close() error {
	return a.handler.Close()
}

// ReadDBByte returns the whole control byte.
func (a *A05PLC) ReadDBByte() (byte, error) {
	var data [1]byte
	if err := a.client.AGReadDB(dbNum, dbByte, 1, data[:]); err != nil {
		return 0, err
	}
	return data[0], nil
}

func (a *A05PLC) ReadDBBit(bit Merker) (bool, error) {
	if bit >= numBits {
		return false, fmt.Errorf("unknown bit %d", uint8(bit))
	}

	b, err := a.ReadDBByte()
	if err != nil {
		return false, err
	}
	return b&(1<<bit) != 0, nil
}

// WriteDBBit writes a single bit. Bits which are not on the whitelist are
// rejected with ErrNotWritable.
func (a *A05PLC) WriteDBBit(bit Merker, state bool) error {
	if !Writable(bit) {
		return fmt.Errorf("%v: %w", bit, ErrNotWritable)
	}

	return a.client.AGWriteDBBit(dbNum, bitAddress(dbByte, bit), 1, boolToByte(state))
}

func boolToByte(b bool) []byte {
	if b {
		return []byte{0x01}
	}
	return []byte{0x00}
}
