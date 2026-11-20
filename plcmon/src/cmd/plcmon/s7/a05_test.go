package s7

import (
	"errors"
	"testing"
)

func TestBitAddress(t *testing.T) {
	for _, tc := range []struct {
		byteOffset int
		bit        Merker
		want       int
	}{
		{0, 0, 0},
		{0, BitFlurlichtTaster, 2},
		{0, BitUDPEnabled, 5},
		{1, 0, 8},
		{2, 3, 19},
	} {
		if got := bitAddress(tc.byteOffset, tc.bit); got != tc.want {
			t.Errorf("bitAddress(%d, %d) = %d, want %d", tc.byteOffset, tc.bit, got, tc.want)
		}
	}
}

func TestWhitelist(t *testing.T) {
	want := map[Merker]bool{
		BitDauerlicht:          false,
		BitFlurlichtTimerReset: true,
		BitFlurlichtTaster:     true,
		BitFlurlichtHold:       true,
		BitAussenlichtKopplung: false,
		BitUDPEnabled:          false,
		6:                      false,
		7:                      false,
	}
	for bit, w := range want {
		if got := Writable(bit); got != w {
			t.Errorf("Writable(%v) = %v, want %v", bit, got, w)
		}
	}
}

type write struct {
	db, start, size int
	value           byte
}

type fakeDB struct {
	data   byte
	writes []write
}

func (f *fakeDB) AGReadDB(db, start, size int, buffer []byte) error {
	buffer[0] = f.data
	return nil
}

func (f *fakeDB) AGWriteDBBit(db, start, size int, buffer []byte) error {
	f.writes = append(f.writes, write{db, start, size, buffer[0]})
	return nil
}

func TestWriteDBBit(t *testing.T) {
	db := &fakeDB{}
	plc := &A05PLC{client: db}

	if err := plc.WriteDBBit(BitFlurlichtTaster, true); err != nil {
		t.Fatal(err)
	}
	if err := plc.WriteDBBit(BitFlurlichtTimerReset, false); err != nil {
		t.Fatal(err)
	}

	want := []write{{8, 2, 1, 1}, {8, 1, 1, 0}}
	if len(db.writes) != len(want) {
		t.Fatalf("writes = %+v, want %+v", db.writes, want)
	}
	for i := range want {
		if db.writes[i] != want[i] {
			t.Errorf("write %d = %+v, want %+v", i, db.writes[i], want[i])
		}
	}
}

func TestWriteDBBitRejected(t *testing.T) {
	for _, bit := range []Merker{BitUDPEnabled, BitDauerlicht, BitAussenlichtKopplung, 6, 7, 200} {
		db := &fakeDB{}
		plc := &A05PLC{client: db}

		for _, state := range []bool{false, true} {
			if err := plc.WriteDBBit(bit, state); !errors.Is(err, ErrNotWritable) {
				t.Errorf("WriteDBBit(%v, %v) = %v, want ErrNotWritable", bit, state, err)
			}
		}
		if len(db.writes) != 0 {
			t.Errorf("bit %v was written to the PLC: %+v", bit, db.writes)
		}
	}
}

func TestReadDBBit(t *testing.T) {
	plc := &A05PLC{client: &fakeDB{data: 0b00100100}}

	for bit, want := range map[Merker]bool{
		BitDauerlicht:      false,
		BitFlurlichtTaster: true,
		BitUDPEnabled:      true,
	} {
		got, err := plc.ReadDBBit(bit)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("ReadDBBit(%v) = %v, want %v", bit, got, want)
		}
	}

	if _, err := plc.ReadDBBit(8); err == nil {
		t.Error("ReadDBBit(8) succeeded, want error")
	}
}

// fakeIO simulates the control byte. With selfReset, the PLC clears a push
// button bit by itself after processing it.
type fakeIO struct {
	bits      map[Merker]bool
	selfReset bool
	readErr   error
	writeErr  error
	log       []string
}

func (f *fakeIO) ReadDBBit(bit Merker) (bool, error) {
	if f.readErr != nil {
		return false, f.readErr
	}
	return f.bits[bit], nil
}

func (f *fakeIO) WriteDBBit(bit Merker, state bool) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if state {
		f.log = append(f.log, "set")
	} else {
		f.log = append(f.log, "clear")
	}
	if f.bits == nil {
		f.bits = map[Merker]bool{}
	}
	f.bits[bit] = state && !f.selfReset
	return nil
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPulse(t *testing.T) {
	t.Run("PLC does not reset the bit", func(t *testing.T) {
		io := &fakeIO{}
		if err := Pulse(io, BitFlurlichtTaster, 0); err != nil {
			t.Fatal(err)
		}
		if !equal(io.log, []string{"set", "clear"}) {
			t.Errorf("writes = %v", io.log)
		}
		if io.bits[BitFlurlichtTaster] {
			t.Error("bit is still set")
		}
	})

	t.Run("PLC resets the bit itself", func(t *testing.T) {
		io := &fakeIO{selfReset: true}
		if err := Pulse(io, BitFlurlichtTaster, 0); err != nil {
			t.Fatal(err)
		}
		if !equal(io.log, []string{"set"}) {
			t.Errorf("writes = %v", io.log)
		}
	})

	t.Run("read back fails", func(t *testing.T) {
		io := &fakeIO{readErr: errors.New("read failed")}
		if err := Pulse(io, BitFlurlichtTaster, 0); err != nil {
			t.Fatal(err)
		}
		if !equal(io.log, []string{"set", "clear"}) {
			t.Errorf("writes = %v", io.log)
		}
	})

	t.Run("write fails", func(t *testing.T) {
		io := &fakeIO{writeErr: errors.New("write failed")}
		if err := Pulse(io, BitFlurlichtTaster, 0); err == nil {
			t.Error("Pulse succeeded, want error")
		}
	})

	t.Run("state bits cannot be pulsed", func(t *testing.T) {
		for _, bit := range []Merker{BitAussenlichtKopplung, BitDauerlicht, BitUDPEnabled} {
			io := &fakeIO{}
			if err := Pulse(io, bit, 0); err == nil {
				t.Errorf("Pulse(%v) succeeded, want error", bit)
			}
			if len(io.log) != 0 {
				t.Errorf("Pulse(%v) wrote to the PLC: %v", bit, io.log)
			}
		}
	})
}

func TestSetToggle(t *testing.T) {
	for _, tc := range []struct {
		name        string
		current     bool
		want        bool
		wantPressed bool
		wantWrites  []string
	}{
		{"off to on", false, true, true, []string{"set", "clear"}},
		{"on to off", true, false, true, []string{"set", "clear"}},
		{"already on", true, true, false, nil},
		{"already off", false, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			io := &fakeIO{bits: map[Merker]bool{BitDauerlicht: tc.current}}
			pressed, err := SetToggle(io, BitFlurlichtHold, BitDauerlicht, tc.want, 0)
			if err != nil {
				t.Fatal(err)
			}
			if pressed != tc.wantPressed || !equal(io.log, tc.wantWrites) {
				t.Errorf("pressed = %v, writes = %v", pressed, io.log)
			}
			// The state bit itself is never written.
			if io.bits[BitDauerlicht] != tc.current {
				t.Error("state bit was written")
			}
		})
	}

	t.Run("state unknown", func(t *testing.T) {
		io := &fakeIO{readErr: errors.New("read failed")}
		if pressed, err := SetToggle(io, BitFlurlichtHold, BitDauerlicht, true, 0); err == nil || pressed {
			t.Errorf("SetToggle = %v, %v, want an error", pressed, err)
		}
		if len(io.log) != 0 {
			t.Errorf("button was pressed blindly: %v", io.log)
		}
	})
}
