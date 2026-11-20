package main

import "sync"

// mqttStatus is the alarm state as exposed to Home Assistant.
type mqttStatus struct {
	Armed     bool
	Triggered bool
}

// snapshot is a consistent copy of everything we know about the PLC.
// The *Known fields are false until the first valid packet was received.
type snapshot struct {
	Alarm       mqttStatus
	AlarmKnown  bool
	Status      StatusMessage
	StatusKnown bool
}

// stateStore holds the latest known PLC state.
//
// Producers (the UDP servers) only ever overwrite the latest value and never
// block, no matter whether a consumer is running. This keeps the alarm
// notification path independent of MQTT.
type stateStore struct {
	mu   sync.Mutex
	cur  snapshot
	wake chan struct{}
}

func newStateStore() *stateStore {
	return &stateStore{wake: make(chan struct{}, 1)}
}

// Wake signals the consumer that there is something to do. Never blocks.
func (s *stateStore) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SetAlarm stores the alarm state reported by a notify message.
func (s *stateStore) SetAlarm(a mqttStatus) {
	s.mu.Lock()
	changed := s.setAlarmLocked(a)
	s.mu.Unlock()

	if changed {
		s.Wake()
	}
}

// SetStatus stores a status word. The status word also carries the alarm
// inputs, so the alarm state is refreshed about once per second and is
// correct right after a restart, without waiting for the next notify message.
func (s *stateStore) SetStatus(m StatusMessage) {
	s.mu.Lock()
	changed := !s.cur.StatusKnown || s.cur.Status != m
	s.cur.Status = m
	s.cur.StatusKnown = true
	if s.setAlarmLocked(mqttStatus{Armed: !m.AlarmUnscharf, Triggered: m.AlarmAusgeloest}) {
		changed = true
	}
	s.mu.Unlock()

	if changed {
		s.Wake()
	}
}

func (s *stateStore) setAlarmLocked(a mqttStatus) (changed bool) {
	changed = !s.cur.AlarmKnown || s.cur.Alarm != a
	s.cur.Alarm = a
	s.cur.AlarmKnown = true
	return
}

// Disarmed is true only if the alarm system is known to be disarmed.
func (s *stateStore) Disarmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.AlarmKnown && !s.cur.Alarm.Armed
}

func (s *stateStore) Snapshot() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}
