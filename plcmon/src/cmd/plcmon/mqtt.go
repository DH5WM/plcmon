package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"k8s.io/klog/v2"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

var (
	flagMqttAddr   = flag.String("mqttAddr", "", "MQTT server address")
	flagMqttUser   = flag.String("mqttUser", "", "MQTT username")
	flagMqttPass   = flag.String("mqttPass", "", "MQTT password")
	flagMqttRetain = flag.Bool("mqttRetain", true, "MQTT retain discovery and state messages")
)

const (
	discoveryPrefix   = "homeassistant"
	birthTopic        = discoveryPrefix + "/status"
	availabilityTopic = "plcmon/availability"
	commandPrefix     = "plcmon/cmd/"

	payloadOnline  = "online"
	payloadOffline = "offline"

	mqttTimeout = 5 * time.Second
)

func validateMqttFlags() {
	if *flagMqttAddr != "" && (*flagMqttUser == "" || *flagMqttPass == "") {
		klog.Exit("mqttUser and mqttPass must both be set")
	}
	if *flagEnableWrite && *flagMqttAddr == "" {
		klog.Warning("enableWrite is set, but there is no MQTT server to receive commands from")
	}
}

func entityTopic(component, id, suffix string) string {
	return fmt.Sprintf("%s/%s/plcmon/%s/%s", discoveryPrefix, component, id, suffix)
}

type haDevice struct {
	Identifiers  string `json:"identifiers"`
	Name         string `json:"name"`
	Model        string `json:"model"`
	Manufacturer string `json:"manufacturer"`
}

type haEntity struct {
	Name              string   `json:"name"`
	UniqueID          string   `json:"unique_id"`
	DeviceClass       string   `json:"device_class,omitempty"`
	StateTopic        string   `json:"state_topic,omitempty"`
	CommandTopic      string   `json:"command_topic,omitempty"`
	PayloadOn         string   `json:"payload_on,omitempty"`
	PayloadOff        string   `json:"payload_off,omitempty"`
	PayloadPress      string   `json:"payload_press,omitempty"`
	StateOn           string   `json:"state_on,omitempty"`
	StateOff          string   `json:"state_off,omitempty"`
	AvailabilityTopic string   `json:"availability_topic"`
	Device            haDevice `json:"device"`
}

type mqttMessage struct {
	topic   string
	payload string
}

func discoveryMessage(component string, e haEntity) mqttMessage {
	e.AvailabilityTopic = availabilityTopic
	e.Device = haDevice{
		Identifiers:  "PLCMON",
		Name:         "PLCMON",
		Model:        "PLCMON",
		Manufacturer: "PLCMON",
	}

	payload, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}

	return mqttMessage{entityTopic(component, e.UniqueID, "config"), string(payload)}
}

// discoveryMessages returns the Home Assistant discovery configs. If write
// access is disabled, the buttons and switches are removed from Home
// Assistant by publishing an empty config.
func discoveryMessages(writeEnabled bool) []mqttMessage {
	msgs := []mqttMessage{
		// The unique IDs, topics and payloads of these two sensors are relied
		// upon by existing Home Assistant setups, do not change them.
		discoveryMessage("binary_sensor", haEntity{
			Name:        "Alarm Armed",
			UniqueID:    "plcmon_armed",
			DeviceClass: "lock",
			StateTopic:  entityTopic("binary_sensor", "plcmon_armed", "state"),
			PayloadOn:   "false",
			PayloadOff:  "true",
		}),
		discoveryMessage("binary_sensor", haEntity{
			Name:        "Alarm Triggered",
			UniqueID:    "plcmon_triggered",
			DeviceClass: "safety",
			StateTopic:  entityTopic("binary_sensor", "plcmon_triggered", "state"),
			PayloadOn:   "true",
			PayloadOff:  "false",
		}),
	}

	for _, s := range statusSensors {
		msgs = append(msgs, discoveryMessage("binary_sensor", haEntity{
			Name:       s.name,
			UniqueID:   s.id,
			StateTopic: entityTopic("binary_sensor", s.id, "state"),
			PayloadOn:  "true",
			PayloadOff: "false",
		}))
	}

	for _, c := range plcControls {
		if !writeEnabled {
			msgs = append(msgs, mqttMessage{entityTopic(c.component, c.id, "config"), ""})
			continue
		}

		e := haEntity{
			Name:         c.name,
			UniqueID:     c.id,
			CommandTopic: commandPrefix + c.key,
		}
		switch c.component {
		case "button":
			e.PayloadPress = payloadPress
		case "switch":
			// The state is taken from the status word, there is no
			// optimistic mode.
			e.StateTopic = entityTopic("binary_sensor", c.stateSensor, "state")
			e.PayloadOn = payloadOn
			e.PayloadOff = payloadOff
			e.StateOn = "true"
			e.StateOff = "false"
		}
		msgs = append(msgs, discoveryMessage(c.component, e))
	}

	return msgs
}

// stateMessages returns the state messages for everything which is known.
func stateMessages(s snapshot) []mqttMessage {
	var msgs []mqttMessage

	if s.AlarmKnown {
		msgs = append(msgs,
			mqttMessage{entityTopic("binary_sensor", "plcmon_armed", "state"), fmt.Sprintf("%v", s.Alarm.Armed)},
			mqttMessage{entityTopic("binary_sensor", "plcmon_triggered", "state"), fmt.Sprintf("%v", s.Alarm.Triggered)},
		)
	}

	if s.StatusKnown {
		for _, sensor := range statusSensors {
			msgs = append(msgs, mqttMessage{
				entityTopic("binary_sensor", sensor.id, "state"),
				fmt.Sprintf("%v", sensor.get(s.Status)),
			})
		}
	}

	return msgs
}

type mqttPublisher interface {
	Publish(topic string, retained bool, payload string) error
}

// mqttSync mirrors the state store to MQTT. It only publishes what changed
// since the last successful publish, and everything after a refresh was
// requested (on connect and on the Home Assistant birth message).
type mqttSync struct {
	store        *stateStore
	pub          mqttPublisher
	retain       bool
	writeEnabled bool

	refresh   atomic.Bool
	published map[string]string
}

func newMqttSync(store *stateStore, pub mqttPublisher, retain bool, writeEnabled bool) *mqttSync {
	m := &mqttSync{
		store:        store,
		pub:          pub,
		retain:       retain,
		writeEnabled: writeEnabled,
		published:    map[string]string{},
	}
	m.refresh.Store(true)
	return m
}

// requestRefresh makes the next sync publish discovery and all states again.
// Safe to call from any goroutine.
func (m *mqttSync) requestRefresh() {
	m.refresh.Store(true)
	m.store.Wake()
}

// sync publishes all pending changes. On error, whatever was not published
// stays pending and is retried by the next call.
func (m *mqttSync) sync() error {
	if m.refresh.Swap(false) {
		if err := m.publishDiscovery(); err != nil {
			m.refresh.Store(true)
			return err
		}
		m.published = map[string]string{}
	}

	for _, msg := range stateMessages(m.store.Snapshot()) {
		if last, ok := m.published[msg.topic]; ok && last == msg.payload {
			continue
		}
		if err := m.pub.Publish(msg.topic, m.retain, msg.payload); err != nil {
			return err
		}
		klog.V(1).Infof("mqtt: %s = %s", msg.topic, msg.payload)
		m.published[msg.topic] = msg.payload
	}

	return nil
}

func (m *mqttSync) publishDiscovery() error {
	for _, msg := range discoveryMessages(m.writeEnabled) {
		// An empty config removes the entity. This has to be retained, so
		// that it also clears a retained config left by an earlier run.
		retain := m.retain || msg.payload == ""
		if err := m.pub.Publish(msg.topic, retain, msg.payload); err != nil {
			return err
		}
	}

	// The availability is always retained, like the last will.
	return m.pub.Publish(availabilityTopic, true, payloadOnline)
}

func (m *mqttSync) run(connected func() bool) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.store.wake:
		case <-ticker.C: // retry after errors
		}

		if !connected() {
			continue
		}

		if err := m.sync(); err != nil {
			klog.Errorf("mqtt: %v", err)
		}
	}
}

type pahoPublisher struct {
	c mqtt.Client
}

func (p pahoPublisher) Publish(topic string, retained bool, payload string) error {
	if !p.c.IsConnectionOpen() {
		return errors.New("publish: not connected")
	}

	token := p.c.Publish(topic, 0, retained, payload)
	if !token.WaitTimeout(mqttTimeout) {
		return fmt.Errorf("publish %s: timeout", topic)
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}

	return nil
}

func mqttSubscribe(c mqtt.Client, topic string, handler mqtt.MessageHandler) {
	token := c.Subscribe(topic, 0, handler)
	if !token.WaitTimeout(mqttTimeout) {
		klog.Errorf("mqtt: subscribe %s: timeout", topic)
	} else if err := token.Error(); err != nil {
		klog.Errorf("mqtt: subscribe %s: %v", topic, err)
	}
}

// mqttService connects to the MQTT server and keeps Home Assistant up to
// date. It never returns; the paho client reconnects by itself. writer is nil
// if write access to the PLC is disabled.
func mqttService(store *stateStore, writer *plcWriter) {
	mqtt.ERROR = log.Default()

	syncer := newMqttSync(store, nil, *flagMqttRetain, writer != nil)

	opts := mqtt.NewClientOptions().AddBroker("tcp://" + *flagMqttAddr).SetClientID("plcmon")
	opts.SetUsername(*flagMqttUser)
	opts.SetPassword(*flagMqttPass)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(30 * time.Second)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetConnectTimeout(mqttTimeout)
	opts.SetWill(availabilityTopic, payloadOffline, 1, true)

	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		klog.Errorf("mqtt: connection lost: %v", err)
	})

	// Subscriptions do not survive a reconnect, so they are set up here.
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		klog.Infof("mqtt connected to %s", *flagMqttAddr)

		// Home Assistant announces its (re)start here. It then needs
		// discovery and states again, unless they are retained.
		mqttSubscribe(c, birthTopic, func(_ mqtt.Client, msg mqtt.Message) {
			if string(msg.Payload()) == payloadOnline {
				klog.Info("mqtt: Home Assistant is online, refreshing")
				syncer.requestRefresh()
			}
		})

		if writer != nil {
			mqttSubscribe(c, commandPrefix+"+", func(_ mqtt.Client, msg mqtt.Message) {
				handleCommand(writer, msg.Topic(), string(msg.Payload()), msg.Retained())
			})
		}

		syncer.requestRefresh()
	})

	c := mqtt.NewClient(opts)
	syncer.pub = pahoPublisher{c}

	klog.Infof("connecting to mqtt server %s", *flagMqttAddr)
	c.Connect() // retries in the background until connected

	syncer.run(c.IsConnectionOpen)
}

// handleCommand must not block, it is called from the MQTT client.
func handleCommand(writer *plcWriter, topic string, payload string, retained bool) {
	// A retained command would be executed again on every reconnect.
	if retained {
		klog.Warningf("mqtt: ignoring retained command on %s", topic)
		return
	}

	cmd, err := parseCommand(strings.TrimPrefix(topic, commandPrefix), payload)
	if err != nil {
		klog.Errorf("mqtt: %v", err)
		return
	}

	klog.Infof("mqtt: command %s (%s)", cmd.name, payload)
	writer.Enqueue(cmd)
}
