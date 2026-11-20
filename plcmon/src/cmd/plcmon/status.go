package main

import (
	"encoding/binary"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/klog/v2"
	"net"
	"reflect"
)

var (
	statusMessagesProcessed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "plcmon_status_messages_total",
		Help: "The total number of processed status messages",
	})
	sensorStatus = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plcmon_status_value",
		Help: "Current state of PLCMON status bits",
	}, []string{"name"})
)

// See messages.org. Using the German descriptions appeared to the most
// sensible course of action, given all of the existing German docs.
type StatusMessage struct {
	RelaisWerkstatt    bool
	RelaisUKW          bool
	RelaisKW           bool
	RelaisZentralstrom bool
	RelaisFlurlicht    bool
	StatusDauerlicht   bool
	KopplungAktiv      bool
	Kopplung           bool
	FlurlichtManuell   bool
	RelaisAussenlicht  bool
	AlarmUnscharf      bool
	AlarmAusgeloest    bool
}

func processStatusPacket(buffer []byte) StatusMessage {
	message := binary.BigEndian.Uint16(buffer)

	return StatusMessage{
		RelaisWerkstatt:    message&(1<<0) > 0,
		RelaisUKW:          message&(1<<1) > 0,
		RelaisKW:           message&(1<<2) > 0,
		RelaisZentralstrom: message&(1<<3) > 0,
		RelaisFlurlicht:    message&(1<<4) > 0,
		StatusDauerlicht:   message&(1<<5) > 0,
		KopplungAktiv:      message&(1<<6) > 0,
		Kopplung:           message&(1<<7) > 0,
		FlurlichtManuell:   message&(1<<8) > 0,
		RelaisAussenlicht:  message&(1<<9) > 0,
		AlarmUnscharf:      message&(1<<10) > 0,
		AlarmAusgeloest:    message&(1<<11) > 0,
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

// statusSensor describes a status bit which is exposed to Home Assistant as a
// read-only binary_sensor. The alarm bits (10 and 11) are not listed here,
// they feed the existing plcmon_armed / plcmon_triggered sensors.
type statusSensor struct {
	id   string // unique_id, also part of the MQTT topic
	name string
	get  func(StatusMessage) bool
}

var statusSensors = []statusSensor{
	{"plcmon_relais_werkstatt", "Relais Werkstatt", func(m StatusMessage) bool { return m.RelaisWerkstatt }},
	{"plcmon_relais_ukw", "Relais UKW", func(m StatusMessage) bool { return m.RelaisUKW }},
	{"plcmon_relais_kw", "Relais KW", func(m StatusMessage) bool { return m.RelaisKW }},
	{"plcmon_relais_zentralstrom", "Relais Zentralstrom", func(m StatusMessage) bool { return m.RelaisZentralstrom }},
	{"plcmon_relais_flurlicht", "Relais Flurlicht", func(m StatusMessage) bool { return m.RelaisFlurlicht }},
	{"plcmon_status_dauerlicht", "Status Dauerlicht", func(m StatusMessage) bool { return m.StatusDauerlicht }},
	{"plcmon_kopplung_aktiv", "Außenlichtkopplung aktiv", func(m StatusMessage) bool { return m.KopplungAktiv }},
	{"plcmon_kopplung", "Außenlichtkopplung", func(m StatusMessage) bool { return m.Kopplung }},
	{"plcmon_flurlicht_manuell", "Flurlicht manuell", func(m StatusMessage) bool { return m.FlurlichtManuell }},
	{"plcmon_relais_aussenlicht", "Relais Außenlicht", func(m StatusMessage) bool { return m.RelaisAussenlicht }},
}

func statusServer(addr string, store *stateStore) {
	s, err := net.ListenPacket("udp", addr)
	if err != nil {
		klog.Fatal(err)
	}

	klog.Infof("status server listening on %s", addr)

	defer s.Close()

	buffer := make([]byte, 2)

	for {
		n, addr, err := s.ReadFrom(buffer)
		if err != nil {
			klog.Errorf("failed to receive packet: %v", err)
			continue
		}

		klog.V(2).Infof("recv: bytes=%d from=%s, buf=%x\n",
			n, addr.String(), string(buffer[:n]))

		if n != 2 {
			klog.Warning("ignoring invalid packet")
			continue
		}

		data := processStatusPacket(buffer[:n])

		klog.V(2).Infof("data: %+v", data)

		store.SetStatus(data)

		// Set metrics
		statusMessagesProcessed.Inc()

		{
			v := reflect.ValueOf(data)
			for i := 0; i < v.NumField(); i++ {
				sensorStatus.WithLabelValues(
					v.Type().Field(i).Name).Set(boolToFloat(v.Field(i).Bool()))
			}
		}

		klog.Flush()
	}
}
