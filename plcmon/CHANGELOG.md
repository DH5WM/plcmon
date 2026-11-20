# Changelog

## 1.1.0

### Fixed

- Alarm notifications no longer depend on MQTT. Previously, the notify server
  blocked after the first message while the MQTT server was unreachable (or
  not configured), and all further Pushover and Telegram messages were lost.
- Debug messages (`0x00`) and invalid messages no longer report the alarm as
  disarmed to Home Assistant.
- The MQTT flag check runs after the flags were parsed. Without an MQTT server
  address, MQTT is now disabled instead of retrying forever.
- A failed Telegram request logs the error, without the bot key.
- The image builds on current Supervisor versions, which no longer pass
  `BUILD_FROM`: the Dockerfile has a default base image.
- `s7cli` builds again (wrong import path), and bit addresses are calculated
  correctly for bytes other than 0.

### Added

- The alarm state is also taken from the status word (bits 10 and 11), which
  the PLC sends about once per second. The state is correct shortly after a
  restart, without waiting for the next change.
- Discovery and state messages are retained. Can be turned off with the
  `mqttRetain` option.
- Discovery and all states are sent again on every (re)connect and when Home
  Assistant announces itself on `homeassistant/status`.
- Availability: `plcmon/availability` (`online`/`offline`, last will). All
  entities become unavailable when PLCMON is not running.
- Read-only binary sensors for status bits 0 to 9 (relays, Dauerlicht,
  Außenlichtkopplung, Flurlicht manuell).
- Optional write access to the PLC (`enableWrite`, off by default, PLC address
  in `spsAddr`): buttons for Flurlicht-Taster and Flurlicht-Timer
  zurücksetzen, and a switch for the Flurlicht Dauerlicht. The switch
  simulates the long press used on site (Flurlicht-Haltemerker), and only if
  the PLC reports a different state. Only these three push button bits of DB8
  can be written; all other bits, including the one which enables the UDP
  telegrams, are locked.
- Prometheus counters `plcmon_s7_writes_total` and `plcmon_s7_errors_total`.

### Changed

- `/debug/pprof` is only served with the `enablePprof` option. `/metrics` is
  unchanged.
- The unique IDs, topics and payloads of the existing sensors `plcmon_armed`
  and `plcmon_triggered` are unchanged.
