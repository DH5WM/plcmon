package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/leoluk/plcmon/src/cmd/plcmon/s7"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProcessStatusPacket(t *testing.T) {
	for _, tc := range []struct {
		packet []byte
		want   StatusMessage
	}{
		{[]byte{0x00, 0x00}, StatusMessage{}},
		// Big endian: the low byte is the second one.
		{[]byte{0x00, 0x01}, StatusMessage{RelaisWerkstatt: true}},
		{[]byte{0x00, 0x02}, StatusMessage{RelaisUKW: true}},
		{[]byte{0x00, 0x04}, StatusMessage{RelaisKW: true}},
		{[]byte{0x00, 0x08}, StatusMessage{RelaisZentralstrom: true}},
		{[]byte{0x00, 0x10}, StatusMessage{RelaisFlurlicht: true}},
		{[]byte{0x00, 0x20}, StatusMessage{StatusDauerlicht: true}},
		{[]byte{0x00, 0x40}, StatusMessage{KopplungAktiv: true}},
		{[]byte{0x00, 0x80}, StatusMessage{Kopplung: true}},
		{[]byte{0x01, 0x00}, StatusMessage{FlurlichtManuell: true}},
		{[]byte{0x02, 0x00}, StatusMessage{RelaisAussenlicht: true}},
		{[]byte{0x04, 0x00}, StatusMessage{AlarmUnscharf: true}},
		{[]byte{0x08, 0x00}, StatusMessage{AlarmAusgeloest: true}},
		// Unused bits are ignored.
		{[]byte{0xf0, 0x00}, StatusMessage{}},
		{[]byte{0x04, 0x0e}, StatusMessage{
			AlarmUnscharf: true, RelaisUKW: true, RelaisKW: true, RelaisZentralstrom: true}},
	} {
		if got := processStatusPacket(tc.packet); got != tc.want {
			t.Errorf("processStatusPacket(%x) = %+v, want %+v", tc.packet, got, tc.want)
		}
	}
}

func TestFormatPushMessage(t *testing.T) {
	for _, tc := range []struct {
		b         uint8
		message   string
		urgent    bool
		status    mqttStatus
		hasStatus bool
	}{
		{0x00, messageDebug, false, mqttStatus{}, false},
		{0x01, messageArmed, false, mqttStatus{Armed: true}, true},
		{0x0f, messageDisarmed, false, mqttStatus{}, true},
		{0xf1, messageTriggered, true, mqttStatus{Armed: true, Triggered: true}, true},
		{0xef, messageReset, false, mqttStatus{}, true},
		{0x02, "Ungültiger Zustand der State Machine: 2", false, mqttStatus{}, false},
		{0xf0, "Ungültiger Zustand der State Machine: f0", false, mqttStatus{}, false},
		{0xff, "Ungültiger Zustand der State Machine: ff", false, mqttStatus{}, false},
	} {
		message, urgent, status, hasStatus := formatPushMessage(tc.b)
		if message != tc.message || urgent != tc.urgent || status != tc.status || hasStatus != tc.hasStatus {
			t.Errorf("formatPushMessage(%#x) = %q, %v, %+v, %v", tc.b, message, urgent, status, hasStatus)
		}
	}
}

type push struct {
	body   string
	urgent bool
}

// mockPush replaces the Pushover sender for the duration of a test.
func mockPush(t *testing.T) *[]push {
	var sent []push
	orig := pushSender
	pushSender = func(body string, urgent bool) {
		sent = append(sent, push{body, urgent})
	}
	t.Cleanup(func() { pushSender = orig })
	return &sent
}

// The notification path must keep working without any MQTT consumer: nothing
// reads from the store here, as if the MQTT server were unreachable.
func TestNotifyDoesNotDependOnMQTT(t *testing.T) {
	sent := mockPush(t)
	store := newStateStore()

	messages := []byte{0x01, 0xf1, 0xef, 0x0f, 0x01, 0xf1, 0x00, 0x42}

	done := make(chan struct{})
	go func() {
		for _, b := range messages {
			processNotify([]byte{b}, store)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("processNotify blocked without an MQTT consumer")
	}

	want := []push{
		{messageArmed, false},
		{messageTriggered, true},
		{messageReset, false},
		{messageDisarmed, false},
		{messageArmed, false},
		{messageTriggered, true},
		{messageDebug, false},
		{"Ungültiger Zustand der State Machine: 42", false},
	}
	if len(*sent) != len(want) {
		t.Fatalf("sent %d push messages, want %d: %+v", len(*sent), len(want), *sent)
	}
	for i := range want {
		if (*sent)[i] != want[i] {
			t.Errorf("push %d = %+v, want %+v", i, (*sent)[i], want[i])
		}
	}

	// The debug and invalid messages at the end must not touch the state.
	s := store.Snapshot()
	if !s.AlarmKnown || s.Alarm != (mqttStatus{Armed: true, Triggered: true}) {
		t.Errorf("alarm state = %+v (known: %v)", s.Alarm, s.AlarmKnown)
	}
}

func TestNotifyDebugAndInvalidLeaveStateUnknown(t *testing.T) {
	mockPush(t)
	store := newStateStore()

	processNotify([]byte{0x00}, store)
	processNotify([]byte{0x42}, store)

	if store.Snapshot().AlarmKnown {
		t.Error("debug or invalid message set an alarm state")
	}
	if msgs := stateMessages(store.Snapshot()); len(msgs) != 0 {
		t.Errorf("state messages for unknown state: %+v", msgs)
	}
}

func TestStatusWordSetsAlarmState(t *testing.T) {
	for _, tc := range []struct {
		status StatusMessage
		want   mqttStatus
	}{
		{StatusMessage{AlarmUnscharf: true}, mqttStatus{Armed: false, Triggered: false}},
		{StatusMessage{}, mqttStatus{Armed: true, Triggered: false}},
		{StatusMessage{AlarmAusgeloest: true}, mqttStatus{Armed: true, Triggered: true}},
	} {
		store := newStateStore()
		store.SetStatus(tc.status)
		if s := store.Snapshot(); !s.AlarmKnown || s.Alarm != tc.want {
			t.Errorf("status %+v: alarm = %+v (known: %v), want %+v", tc.status, s.Alarm, s.AlarmKnown, tc.want)
		}
	}
}

func woken(store *stateStore) bool {
	select {
	case <-store.wake:
		return true
	default:
		return false
	}
}

func TestStoreWakesOnlyOnChange(t *testing.T) {
	store := newStateStore()
	status := StatusMessage{AlarmUnscharf: true, RelaisKW: true}

	store.SetStatus(status)
	if !woken(store) {
		t.Error("first status did not wake the consumer")
	}

	store.SetStatus(status)
	store.SetAlarm(mqttStatus{})
	if woken(store) {
		t.Error("unchanged state woke the consumer")
	}

	store.SetAlarm(mqttStatus{Armed: true})
	if !woken(store) {
		t.Error("changed alarm state did not wake the consumer")
	}
}

type fakePublisher struct {
	mu       sync.Mutex
	messages []published
	// Fail every publish while failures > 0.
	failures int
}

type published struct {
	topic    string
	payload  string
	retained bool
}

func (f *fakePublisher) Publish(topic string, retained bool, payload string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return errors.New("not connected")
	}
	f.messages = append(f.messages, published{topic, payload, retained})
	return nil
}

func (f *fakePublisher) take() []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.messages
	f.messages = nil
	return msgs
}

func find(msgs []published, topic string) (published, bool) {
	for _, m := range msgs {
		if m.topic == topic {
			return m, true
		}
	}
	return published{}, false
}

const (
	armedState     = "homeassistant/binary_sensor/plcmon/plcmon_armed/state"
	triggeredState = "homeassistant/binary_sensor/plcmon/plcmon_triggered/state"
	kwState        = "homeassistant/binary_sensor/plcmon/plcmon_relais_kw/state"
)

func TestSyncPublishesOnlyChanges(t *testing.T) {
	pub := &fakePublisher{}
	store := newStateStore()
	m := newMqttSync(store, pub, true, false)

	// Nothing is known yet: discovery and availability only.
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	msgs := pub.take()
	if _, ok := find(msgs, armedState); ok {
		t.Error("published an alarm state before any packet was received")
	}
	if m, ok := find(msgs, availabilityTopic); !ok || m.payload != "online" || !m.retained {
		t.Errorf("availability = %+v", m)
	}

	store.SetStatus(StatusMessage{AlarmUnscharf: true, RelaisKW: true})
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	msgs = pub.take()
	if len(msgs) != 2+len(statusSensors) {
		t.Errorf("published %d states, want %d: %+v", len(msgs), 2+len(statusSensors), msgs)
	}
	for topic, want := range map[string]string{armedState: "false", triggeredState: "false", kwState: "true"} {
		if m, ok := find(msgs, topic); !ok || m.payload != want || !m.retained {
			t.Errorf("%s = %+v, want %s (retained)", topic, m, want)
		}
	}

	// The same status again, as received once per second: no traffic.
	store.SetStatus(StatusMessage{AlarmUnscharf: true, RelaisKW: true})
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	if msgs := pub.take(); len(msgs) != 0 {
		t.Errorf("published without a change: %+v", msgs)
	}

	// Alarm triggered: only the two changed topics are published.
	store.SetAlarm(mqttStatus{Armed: true, Triggered: true})
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	msgs = pub.take()
	if len(msgs) != 2 {
		t.Errorf("published %+v, want armed and triggered only", msgs)
	}
	for topic, want := range map[string]string{armedState: "true", triggeredState: "true"} {
		if m, ok := find(msgs, topic); !ok || m.payload != want {
			t.Errorf("%s = %+v, want %s", topic, m, want)
		}
	}
}

func TestSyncRefresh(t *testing.T) {
	pub := &fakePublisher{}
	store := newStateStore()
	m := newMqttSync(store, pub, true, false)

	store.SetStatus(StatusMessage{AlarmUnscharf: true})
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	first := pub.take()

	// Reconnect or Home Assistant birth message: everything again.
	m.requestRefresh()
	if !woken(store) {
		t.Error("requestRefresh did not wake the consumer")
	}
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	second := pub.take()

	if len(second) != len(first) {
		t.Errorf("refresh published %d messages, want %d", len(second), len(first))
	}
	if _, ok := find(second, "homeassistant/binary_sensor/plcmon/plcmon_armed/config"); !ok {
		t.Error("refresh did not publish discovery")
	}
	if m, ok := find(second, armedState); !ok || m.payload != "false" {
		t.Errorf("refresh did not publish the current state: %+v", m)
	}
}

func TestSyncRetriesAfterError(t *testing.T) {
	pub := &fakePublisher{}
	store := newStateStore()
	m := newMqttSync(store, pub, true, false)
	store.SetStatus(StatusMessage{AlarmUnscharf: true})

	// Fail somewhere in the middle of discovery, then in the middle of the
	// states. In the end everything must have been published.
	for _, failAfter := range []int{3, 20} {
		pub.mu.Lock()
		pub.failures = 0
		pub.mu.Unlock()

		count := 0
		failing := &countingPublisher{inner: pub, failAt: failAfter, count: &count}
		m.pub = failing
		if err := m.sync(); err == nil {
			t.Fatalf("sync succeeded, want error after %d messages", failAfter)
		}
		m.pub = pub
	}

	if err := m.sync(); err != nil {
		t.Fatal(err)
	}

	msgs := pub.take()
	for _, topic := range []string{
		"homeassistant/binary_sensor/plcmon/plcmon_armed/config",
		"homeassistant/binary_sensor/plcmon/plcmon_triggered/config",
		availabilityTopic, armedState, triggeredState, kwState,
	} {
		if _, ok := find(msgs, topic); !ok {
			t.Errorf("%s was never published", topic)
		}
	}
}

type countingPublisher struct {
	inner  mqttPublisher
	failAt int
	count  *int
}

func (c *countingPublisher) Publish(topic string, retained bool, payload string) error {
	*c.count++
	if *c.count > c.failAt {
		return errors.New("connection lost")
	}
	return c.inner.Publish(topic, retained, payload)
}

func TestSyncWithoutRetain(t *testing.T) {
	pub := &fakePublisher{}
	store := newStateStore()
	m := newMqttSync(store, pub, false, true)
	store.SetStatus(StatusMessage{})

	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	for _, msg := range pub.take() {
		if want := msg.topic == availabilityTopic; msg.retained != want {
			t.Errorf("%s: retained = %v, want %v", msg.topic, msg.retained, want)
		}
	}
}

func decodeDiscovery(t *testing.T, writeEnabled bool) map[string]map[string]interface{} {
	configs := map[string]map[string]interface{}{}
	for _, msg := range discoveryMessages(writeEnabled) {
		if msg.payload == "" {
			configs[msg.topic] = nil
			continue
		}
		var cfg map[string]interface{}
		if err := json.Unmarshal([]byte(msg.payload), &cfg); err != nil {
			t.Fatalf("%s: %v", msg.topic, err)
		}
		configs[msg.topic] = cfg
	}
	return configs
}

// Existing Home Assistant setups depend on these two configs. Apart from the
// added availability_topic they must stay as they were in 1.0.7.
func TestDiscoveryLegacySensorsUnchanged(t *testing.T) {
	configs := decodeDiscovery(t, false)

	device := map[string]interface{}{
		"identifiers": "PLCMON", "name": "PLCMON", "model": "PLCMON", "manufacturer": "PLCMON",
	}
	want := map[string]map[string]interface{}{
		"homeassistant/binary_sensor/plcmon/plcmon_armed/config": {
			"name":               "Alarm Armed",
			"unique_id":          "plcmon_armed",
			"device_class":       "lock",
			"state_topic":        "homeassistant/binary_sensor/plcmon/plcmon_armed/state",
			"payload_on":         "false",
			"payload_off":        "true",
			"availability_topic": "plcmon/availability",
			"device":             device,
		},
		"homeassistant/binary_sensor/plcmon/plcmon_triggered/config": {
			"name":               "Alarm Triggered",
			"unique_id":          "plcmon_triggered",
			"device_class":       "safety",
			"state_topic":        "homeassistant/binary_sensor/plcmon/plcmon_triggered/state",
			"payload_on":         "true",
			"payload_off":        "false",
			"availability_topic": "plcmon/availability",
			"device":             device,
		},
	}

	for topic, w := range want {
		got, _ := json.Marshal(configs[topic])
		wantJSON, _ := json.Marshal(w)
		if string(got) != string(wantJSON) {
			t.Errorf("%s:\n got %s\nwant %s", topic, got, wantJSON)
		}
	}
}

func TestDiscoveryEntities(t *testing.T) {
	configs := decodeDiscovery(t, true)

	// 2 alarm sensors, 10 status sensors, 2 buttons, 1 switch.
	if len(configs) != 15 {
		t.Errorf("%d discovery configs, want 15", len(configs))
	}

	ids := map[string]bool{}
	for topic, cfg := range configs {
		id := cfg["unique_id"].(string)
		if ids[id] {
			t.Errorf("duplicate unique_id %s", id)
		}
		ids[id] = true

		if !strings.HasSuffix(topic, "/plcmon/"+id+"/config") {
			t.Errorf("topic %s does not match unique_id %s", topic, id)
		}
		if cfg["availability_topic"] != availabilityTopic {
			t.Errorf("%s: no availability_topic", id)
		}
		if cfg["device"].(map[string]interface{})["identifiers"] != "PLCMON" {
			t.Errorf("%s: wrong device", id)
		}
	}

	button := configs["homeassistant/button/plcmon/plcmon_btn_flurlicht_taster/config"]
	if button == nil || button["command_topic"] != "plcmon/cmd/flurlicht_taster" || button["payload_press"] != "PRESS" {
		t.Errorf("Flurlicht button = %v", button)
	}

	sw := configs["homeassistant/switch/plcmon/plcmon_sw_flurlicht_dauerlicht/config"]
	if sw == nil ||
		sw["command_topic"] != "plcmon/cmd/flurlicht_dauerlicht" ||
		sw["state_topic"] != "homeassistant/binary_sensor/plcmon/plcmon_status_dauerlicht/state" ||
		sw["payload_on"] != "ON" || sw["payload_off"] != "OFF" ||
		sw["state_on"] != "true" || sw["state_off"] != "false" {
		t.Errorf("Dauerlicht switch = %v", sw)
	}

	// The Außenlichtkopplung is read-only, and the Haltemerker is only
	// reachable through the Dauerlicht switch.
	for topic := range configs {
		if strings.Contains(topic, "haltemerker") || strings.Contains(topic, "sw_aussenlichtkopplung") {
			t.Errorf("unexpected writable entity %s", topic)
		}
	}
}

// With write access disabled, buttons and switches are removed from Home
// Assistant with an empty, retained config.
func TestDiscoveryWriteDisabled(t *testing.T) {
	configs := decodeDiscovery(t, false)

	for _, c := range plcControls {
		topic := fmt.Sprintf("homeassistant/%s/plcmon/%s/config", c.component, c.id)
		cfg, ok := configs[topic]
		if !ok || cfg != nil {
			t.Errorf("%s = %v, want an empty config", topic, cfg)
		}
	}

	pub := &fakePublisher{}
	m := newMqttSync(newStateStore(), pub, false, false)
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	for _, msg := range pub.take() {
		if msg.payload == "" && !msg.retained {
			t.Errorf("%s: removal is not retained", msg.topic)
		}
	}
}

func TestStatusSensorStateTopics(t *testing.T) {
	all := StatusMessage{
		RelaisWerkstatt: true, RelaisUKW: true, RelaisKW: true, RelaisZentralstrom: true,
		RelaisFlurlicht: true, StatusDauerlicht: true, KopplungAktiv: true, Kopplung: true,
		FlurlichtManuell: true, RelaisAussenlicht: true,
	}

	for i, sensor := range statusSensors {
		// Only this sensor's bit is set.
		word := uint16(1) << i
		only := processStatusPacket([]byte{byte(word >> 8), byte(word)})
		for j, other := range statusSensors {
			if got := other.get(only); got != (i == j) {
				t.Errorf("bit %d: %s = %v", i, other.id, got)
			}
		}
		if !sensor.get(all) {
			t.Errorf("%s is not set", sensor.id)
		}
	}
}

func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		key, payload string
		want         plcCommand
	}{
		{"flurlicht_taster", "PRESS", plcCommand{name: "flurlicht_taster", bit: s7.BitFlurlichtTaster}},
		{"flurlicht_timer_reset", "PRESS", plcCommand{name: "flurlicht_timer_reset", bit: s7.BitFlurlichtTimerReset}},
		{"flurlicht_dauerlicht", "ON", plcCommand{name: "flurlicht_dauerlicht", bit: s7.BitFlurlichtHold,
			toggle: true, stateBit: s7.BitDauerlicht, want: true}},
		{"flurlicht_dauerlicht", "OFF", plcCommand{name: "flurlicht_dauerlicht", bit: s7.BitFlurlichtHold,
			toggle: true, stateBit: s7.BitDauerlicht, want: false}},
	} {
		got, err := parseCommand(tc.key, tc.payload)
		if err != nil {
			t.Errorf("parseCommand(%q, %q): %v", tc.key, tc.payload, err)
		} else if got != tc.want {
			t.Errorf("parseCommand(%q, %q) = %+v, want %+v", tc.key, tc.payload, got, tc.want)
		}
	}

	for _, tc := range []struct{ key, payload string }{
		{"flurlicht_taster", "ON"},
		{"flurlicht_taster", ""},
		{"flurlicht_haltemerker", "PRESS"},
		{"flurlicht_dauerlicht", "PRESS"},
		{"flurlicht_dauerlicht", "true"},
		{"aussenlichtkopplung", "ON"},
		{"aussenlichtkopplung", "OFF"},
		{"aussenlichtkopplung", "PRESS"},
		{"udp", "OFF"},
		{"dauerlicht", "ON"},
		{"", "PRESS"},
		{"flurlicht_taster/extra", "PRESS"},
	} {
		if cmd, err := parseCommand(tc.key, tc.payload); err == nil {
			t.Errorf("parseCommand(%q, %q) = %+v, want error", tc.key, tc.payload, cmd)
		}
	}
}

// Every control must pass the whitelist of the s7 package, and the bit which
// enables the UDP telegrams must not be reachable at all.
func TestControlsAreWhitelisted(t *testing.T) {
	for _, c := range plcControls {
		if !s7.Writable(c.bit) {
			t.Errorf("%s writes %v, which is not writable", c.key, c.bit)
		}
		if c.bit == s7.BitUDPEnabled {
			t.Errorf("%s writes the UDP enable bit", c.key)
		}
		if !s7.IsPulse(c.bit) {
			t.Errorf("%s: %v is not a push button", c.key, c.bit)
		}
		if c.component == "switch" {
			found := false
			for _, s := range statusSensors {
				found = found || s.id == c.stateSensor
			}
			if !found {
				t.Errorf("%s: unknown state sensor %q", c.key, c.stateSensor)
			}
		}
	}
}

// The Dauerlicht can only be toggled, so the switch presses the push button
// only if the PLC reports a different state.
func TestPLCWriterDauerlichtSwitch(t *testing.T) {
	for _, tc := range []struct {
		current bool
		payload string
		want    string
	}{
		{false, "ON", "connect, write 3=true, write 3=false, close"},
		{true, "OFF", "connect, write 3=true, write 3=false, close"},
		{true, "ON", "connect, close"},
		{false, "OFF", "connect, close"},
	} {
		log := &[]string{}
		w, _, _ := newTestWriter(nil)
		w.dial = func() plcConn {
			return &fakeConn{bits: map[s7.Merker]bool{s7.BitDauerlicht: tc.current}, log: log}
		}

		cmd, err := parseCommand("flurlicht_dauerlicht", tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		w.Enqueue(cmd)
		w.process(<-w.queue)

		if got := strings.Join(*log, ", "); got != tc.want {
			t.Errorf("Dauerlicht %v, command %s:\n got %s\nwant %s", tc.current, tc.payload, got, tc.want)
		}
	}
}

type fakeConn struct {
	connectErr error
	bits       map[s7.Merker]bool
	log        *[]string
}

func (f *fakeConn) Connect() error {
	*f.log = append(*f.log, "connect")
	return f.connectErr
}

func (f *fakeConn) Close() error {
	*f.log = append(*f.log, "close")
	return nil
}

func (f *fakeConn) ReadDBBit(bit s7.Merker) (bool, error) {
	return f.bits[bit], nil
}

func (f *fakeConn) WriteDBBit(bit s7.Merker, state bool) error {
	*f.log = append(*f.log, fmt.Sprintf("write %d=%v", bit, state))
	f.bits[bit] = state
	return nil
}

func newTestWriter(connectErr error) (*plcWriter, *[]string, *time.Time) {
	log := &[]string{}
	now := time.Unix(1700000000, 0)
	w := &plcWriter{
		queue: make(chan plcCommand, 2),
		dial: func() plcConn {
			return &fakeConn{connectErr: connectErr, bits: map[s7.Merker]bool{}, log: log}
		},
		now: func() time.Time { return now },
	}
	return w, log, &now
}

func TestPLCWriter(t *testing.T) {
	w, log, _ := newTestWriter(nil)

	press, _ := parseCommand("flurlicht_taster", "PRESS")
	reset, _ := parseCommand("flurlicht_timer_reset", "PRESS")
	w.Enqueue(press)
	w.Enqueue(reset)
	w.process(<-w.queue)
	w.process(<-w.queue)

	want := "connect, write 2=true, write 2=false, close, connect, write 1=true, write 1=false, close"
	if got := strings.Join(*log, ", "); got != want {
		t.Errorf("PLC access:\n got %s\nwant %s", got, want)
	}
}

func TestPLCWriterDropsStaleCommands(t *testing.T) {
	w, log, now := newTestWriter(nil)

	press, _ := parseCommand("flurlicht_taster", "PRESS")
	w.Enqueue(press)
	*now = now.Add(plcCommandMaxAge + time.Second)
	w.process(<-w.queue)

	if len(*log) != 0 {
		t.Errorf("stale command was executed: %v", *log)
	}
}

func TestPLCWriterQueueNeverBlocks(t *testing.T) {
	w, _, _ := newTestWriter(nil)
	press, _ := parseCommand("flurlicht_taster", "PRESS")

	// Nobody is processing the queue.
	results := []bool{w.Enqueue(press), w.Enqueue(press), w.Enqueue(press)}
	if !results[0] || !results[1] || results[2] {
		t.Errorf("Enqueue = %v, want the third command to be dropped", results)
	}
}

func TestPLCWriterConnectError(t *testing.T) {
	w, log, _ := newTestWriter(errors.New("no route to host"))

	press, _ := parseCommand("flurlicht_taster", "PRESS")
	w.Enqueue(press)
	w.process(<-w.queue)

	if got := strings.Join(*log, ", "); got != "connect" {
		t.Errorf("PLC access = %s, want only the failed connect", got)
	}
}

func TestHandleCommandIgnoresRetained(t *testing.T) {
	w, _, _ := newTestWriter(nil)

	handleCommand(w, "plcmon/cmd/flurlicht_taster", "PRESS", true)
	handleCommand(w, "plcmon/cmd/udp", "OFF", false)
	if len(w.queue) != 0 {
		t.Error("retained or unknown command was queued")
	}

	handleCommand(w, "plcmon/cmd/flurlicht_taster", "PRESS", false)
	if len(w.queue) != 1 {
		t.Error("command was not queued")
	}
}
