package main

import (
	"fmt"
	"k8s.io/klog/v2"
	"net"
)

const (
	messageDebug     = "Testnachricht"
	messageArmed     = "Alarmanlage scharf"
	messageDisarmed  = "Alarmanlage unscharf"
	messageTriggered = "Alarm ausgelöst"
	messageReset     = "Alarm zurückgesetzt und unscharf"
	messageInvalid   = "Ungültiger Zustand der State Machine: %x"
)

// Replaced in tests, so that no real notifications are sent.
var (
	pushSender     = sendPushoverMessage
	telegramSender = sendTelegramMessage
)

// formatPushMessage decodes a notify byte. hasStatus is false for messages
// which do not describe an alarm state (debug and invalid messages); those
// must not be forwarded to MQTT.
func formatPushMessage(message uint8) (pushMessage string, urgent bool, status mqttStatus, hasStatus bool) {
	switch {
	case message == 0x00: // non-standard message for debugging
		klog.Info("notify: debug")
		pushMessage = messageDebug
	case message == 0x01: // armed
		klog.Info("notify: alarm armed")
		pushMessage = messageArmed
		status = mqttStatus{Armed: true, Triggered: false}
		hasStatus = true
	case message == 0x0f: // disarmed
		klog.Info("notify: alarm disarmed")
		pushMessage = messageDisarmed
		status = mqttStatus{Armed: false, Triggered: false}
		hasStatus = true
	case message == 0xf1: // alarm || armed
		klog.Info("notify: alarm triggered")
		pushMessage = messageTriggered
		urgent = true
		status = mqttStatus{Armed: true, Triggered: true}
		hasStatus = true
	case message == 0xef: // reset || disarmed
		klog.Info("notify: alarm reset")
		pushMessage = messageReset
		status = mqttStatus{Armed: false, Triggered: false}
		hasStatus = true
	default:
		klog.Errorf("notify: invalid state machine: %x", message)
		pushMessage = fmt.Sprintf(messageInvalid, message)
	}

	return
}

// processNotify must never block: push notifications are sent asynchronously
// and the state store only keeps the latest value.
func processNotify(buffer []byte, store *stateStore) {
	b := uint8(buffer[0])

	message, urgent, status, hasStatus := formatPushMessage(b)

	pushSender(message, urgent)

	if *telegramKey != "" {
		telegramSender(message)
	}

	if hasStatus {
		store.SetAlarm(status)
	}
}

func notifyServer(addr string, store *stateStore) {
	s, err := net.ListenPacket("udp", addr)
	if err != nil {
		klog.Fatal(err)
	}

	klog.Infof("notify server listening on %s", addr)

	defer s.Close()

	buffer := make([]byte, 1)

	for {
		n, addr, err := s.ReadFrom(buffer)
		if err != nil {
			klog.Errorf("failed to receive packet: %v", err)
			continue
		}

		klog.V(2).Infof("recv: bytes=%d from=%s, buf=%x\n",
			n, addr.String(), string(buffer[:n]))

		if n != 1 {
			klog.Warning("ignoring invalid packet")
			continue
		}

		processNotify(buffer[:n], store)

		klog.Flush()
	}
}
