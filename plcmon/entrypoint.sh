#!/usr/bin/with-contenv bashio

CONFIG_PATH=/data/options.json

ARGS=()

# Retain is the default, so that states survive a restart of Home Assistant.
if bashio::config.false 'mqttRetain'; then
  ARGS+=(-mqttRetain=false)
fi

if bashio::config.true 'enableWrite'; then
  ARGS+=(-enableWrite)
fi

if bashio::config.true 'enablePprof'; then
  ARGS+=(-enablePprof)
fi

/plcmon \
  -pushoverKey "$(bashio::config 'pushoverKey')" \
  -pushoverAddr "$(bashio::config 'pushoverAddr')" \
  -telegramKey "$(bashio::config 'telegramKey')" \
  -telegramGroup "$(bashio::config 'telegramGroup')" \
  -mqttAddr "$(bashio::config 'mqttAddr')" \
  -mqttUser "$(bashio::config 'mqttUser')" \
  -mqttPass "$(bashio::config 'mqttPass')" \
  -spsAddr "$(bashio::config 'spsAddr' '192.168.1.10')" \
  "${ARGS[@]}"
