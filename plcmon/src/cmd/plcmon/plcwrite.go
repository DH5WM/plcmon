package main

import (
	"flag"
	"fmt"
	"github.com/leoluk/plcmon/src/cmd/plcmon/s7"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/klog/v2"
	"time"
)

var (
	flagSpsAddr     = flag.String("spsAddr", s7.DefaultAddr, "PLC address for S7 write access")
	flagEnableWrite = flag.Bool("enableWrite", false, "Allow writing to the PLC via MQTT buttons and switches")

	plcWrites = promauto.NewCounter(prometheus.CounterOpts{
		Name: "plcmon_s7_writes_total",
		Help: "The total number of commands written to the PLC",
	})
	plcErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "plcmon_s7_errors_total",
		Help: "The total number of failed or dropped PLC commands",
	})
)

const (
	// Longer than a PLC cycle.
	plcPulseHold = 300 * time.Millisecond
	// A button press which could not be executed in time is dropped rather
	// than executed late.
	plcCommandMaxAge = 10 * time.Second

	plcAccessHint = "check that the PLC is reachable, PUT/GET access is permitted " +
		"and DB8 is not using optimized block access"
)

// plcControl describes a button or switch in Home Assistant. Both simulate a
// push button via a bit of the control block. Bits which are not listed here
// cannot be written via MQTT; the s7 package additionally enforces its own
// whitelist.
type plcControl struct {
	key       string // last element of the command topic
	id        string // unique_id
	name      string
	component string    // "button" or "switch"
	bit       s7.Merker // push button bit
	// Switches only. The push button toggles a state in the PLC, so it is
	// only pressed if stateBit differs from the requested state.
	stateBit s7.Merker
	// unique_id of the status sensor which reports the state to Home
	// Assistant.
	stateSensor string
}

var plcControls = []plcControl{
	{key: "flurlicht_taster", id: "plcmon_btn_flurlicht_taster", name: "Flurlicht-Taster",
		component: "button", bit: s7.BitFlurlichtTaster},
	{key: "flurlicht_timer_reset", id: "plcmon_btn_flurlicht_timer_reset", name: "Flurlicht-Timer zurücksetzen",
		component: "button", bit: s7.BitFlurlichtTimerReset},
	// On site, the Dauerlicht is toggled with a long press of the Flurlicht
	// push button.
	{key: "flurlicht_dauerlicht", id: "plcmon_sw_flurlicht_dauerlicht", name: "Flurlicht Dauerlicht",
		component: "switch", bit: s7.BitFlurlichtHold,
		stateBit: s7.BitDauerlicht, stateSensor: "plcmon_status_dauerlicht"},
}

const (
	payloadPress = "PRESS"
	payloadOn    = "ON"
	payloadOff   = "OFF"
)

type plcCommand struct {
	name string
	bit  s7.Merker
	// For switches: press the push button only if stateBit is not want.
	toggle   bool
	stateBit s7.Merker
	want     bool
	created  time.Time
}

// parseCommand translates an MQTT command into a PLC command.
func parseCommand(key string, payload string) (plcCommand, error) {
	for _, c := range plcControls {
		if c.key != key {
			continue
		}

		cmd := plcCommand{name: c.key, bit: c.bit}

		switch {
		case c.component == "button" && payload == payloadPress:
		case c.component == "switch" && (payload == payloadOn || payload == payloadOff):
			cmd.toggle = true
			cmd.stateBit = c.stateBit
			cmd.want = payload == payloadOn
		default:
			return plcCommand{}, fmt.Errorf("invalid payload %q for %s", payload, key)
		}

		return cmd, nil
	}

	return plcCommand{}, fmt.Errorf("unknown command %q", key)
}

type plcConn interface {
	s7.BitIO
	Connect() error
	Close() error
}

// plcWriter executes PLC commands one after another, in its own goroutine.
// A slow or unreachable PLC only delays other PLC commands, nothing else.
type plcWriter struct {
	queue chan plcCommand
	dial  func() plcConn
	hold  time.Duration
	now   func() time.Time
}

func newPLCWriter(addr string) *plcWriter {
	return &plcWriter{
		queue: make(chan plcCommand, 8),
		dial:  func() plcConn { return s7.NewA05PLC(addr) },
		hold:  plcPulseHold,
		now:   time.Now,
	}
}

// Enqueue never blocks. It returns false if the command was dropped.
func (w *plcWriter) Enqueue(cmd plcCommand) bool {
	cmd.created = w.now()

	select {
	case w.queue <- cmd:
		return true
	default:
		plcErrors.Inc()
		klog.Errorf("s7: queue full, dropping command %s", cmd.name)
		return false
	}
}

func (w *plcWriter) run() {
	w.probe()

	for cmd := range w.queue {
		w.process(cmd)
		klog.Flush()
	}
}

func (w *plcWriter) process(cmd plcCommand) {
	if age := w.now().Sub(cmd.created); age > plcCommandMaxAge {
		plcErrors.Inc()
		klog.Errorf("s7: dropping stale command %s (%v old)", cmd.name, age)
		return
	}

	if err := w.execute(cmd); err != nil {
		plcErrors.Inc()
		klog.Errorf("s7: command %s failed: %v (%s)", cmd.name, err, plcAccessHint)
		return
	}

	plcWrites.Inc()
	klog.Infof("s7: command %s done", cmd.name)
}

// execute uses a fresh connection for each command. Commands are rare, and
// this way there is no idle connection which could have gone stale.
func (w *plcWriter) execute(cmd plcCommand) error {
	conn := w.dial()
	if err := conn.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	if cmd.toggle {
		pressed, err := s7.SetToggle(conn, cmd.bit, cmd.stateBit, cmd.want, w.hold)
		if err == nil && !pressed {
			klog.Infof("s7: %s is already %v", cmd.name, cmd.want)
		}
		return err
	}

	return s7.Pulse(conn, cmd.bit, w.hold)
}

// probe checks read access at startup, so that a misconfigured PLC shows up
// in the log before the first button is pressed.
func (w *plcWriter) probe() {
	conn := w.dial()
	if err := conn.Connect(); err != nil {
		klog.Errorf("s7: connect failed: %v (%s)", err, plcAccessHint)
		return
	}
	defer conn.Close()

	if _, err := conn.ReadDBBit(s7.BitUDPEnabled); err != nil {
		klog.Errorf("s7: read access failed: %v (%s)", err, plcAccessHint)
		return
	}

	klog.Info("s7: read access to the PLC works, write access is enabled")
}
