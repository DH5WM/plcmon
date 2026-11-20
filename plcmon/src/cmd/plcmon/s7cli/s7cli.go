// s7cli is a test tool for the control block of the A05 PLC (DB8, byte 0).
//
// Without -set or -pulse it only reads:
//
//	s7cli                      # print all bits
//	s7cli -bit 4               # print bit 0.4
//	s7cli -bit 4 -set=true     # write bit 0.4
//	s7cli -bit 2 -pulse        # simulate the push button at bit 0.2
package main

import (
	"flag"
	"github.com/leoluk/plcmon/src/cmd/plcmon/s7"
	"k8s.io/klog/v2"
	"time"
)

var (
	flagAddr  = flag.String("addr", s7.DefaultAddr, "PLC address")
	flagBit   = flag.Int("bit", -1, "Bit number in DB8 byte 0 (default: read all bits)")
	flagSet   = flag.String("set", "", "Write the bit: true or false")
	flagPulse = flag.Bool("pulse", false, "Simulate a push button on the bit")
	flagHold  = flag.Duration("hold", 300*time.Millisecond, "Pulse duration")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	writing := *flagSet != "" || *flagPulse
	if writing && (*flagBit < 0 || *flagBit > 7) {
		klog.Exit("-set and -pulse require -bit")
	}
	if *flagSet != "" && *flagPulse {
		klog.Exit("-set and -pulse are mutually exclusive")
	}
	if *flagSet != "" && *flagSet != "true" && *flagSet != "false" {
		klog.Exit("-set must be true or false")
	}

	sps := s7.NewA05PLC(*flagAddr)
	if err := sps.Connect(); err != nil {
		klog.Exitf("failed to connect to SPS: %v", err)
	}
	defer sps.Close()

	klog.Info("connected to SPS")

	if *flagBit < 0 {
		b, err := sps.ReadDBByte()
		if err != nil {
			klog.Exitf("failed to read DB byte: %v", err)
		}
		for i := s7.Merker(0); i < 8; i++ {
			klog.Infof("%v is %v", i, b&(1<<i) != 0)
		}
		return
	}

	bit := s7.Merker(*flagBit)

	printBit := func() {
		st, err := sps.ReadDBBit(bit)
		if err != nil {
			klog.Exitf("failed to read DB bit: %v", err)
		}
		klog.Infof("%v is %v", bit, st)
	}

	printBit()

	switch {
	case *flagPulse:
		if err := s7.Pulse(sps, bit, *flagHold); err != nil {
			klog.Exitf("failed to pulse DB bit: %v", err)
		}
		klog.Info("pulsed DB bit")
	case *flagSet != "":
		if err := sps.WriteDBBit(bit, *flagSet == "true"); err != nil {
			klog.Exitf("failed to write DB bit: %v", err)
		}
		klog.Info("wrote DB bit")
	default:
		return
	}

	printBit()
}
