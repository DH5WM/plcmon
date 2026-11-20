package main

import (
	"flag"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
	"net/http"
	"net/http/pprof"
	"sync"
)

var (
	statusAddr  = flag.String("statusAddr", "0.0.0.0:7001", "Listen address for PLCMON status")
	notifyAddr  = flag.String("notifyAddr", "0.0.0.0:7002", "Listen address for PLCMON notify")
	debugAddr   = flag.String("debugAddr", "0.0.0.0:6060", "Listen address for debug server")
	enablePprof = flag.Bool("enablePprof", false, "Serve /debug/pprof on the debug server")
)

func debugHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	if *enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	return mux
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	validatePushFlags()
	validateTelegramArgs()
	validateMqttFlags()

	klog.CopyStandardLogTo("INFO")

	go func() {
		klog.Info("debug server listening on ", *debugAddr)
		klog.Error(http.ListenAndServe(*debugAddr, debugHandler()))
	}()

	wg := sync.WaitGroup{}
	wg.Add(2)

	store := newStateStore()

	go func() {
		statusServer(*statusAddr, store)
		wg.Done()
	}()

	go func() {
		notifyServer(*notifyAddr, store)
		wg.Done()
	}()

	var writer *plcWriter
	if *flagEnableWrite {
		writer = newPLCWriter(*flagSpsAddr, store)
		go writer.run()
	}

	if *flagMqttAddr != "" {
		go mqttService(store, writer)
	} else {
		klog.Info("no MQTT server configured, MQTT is disabled")
	}

	wg.Wait()

	klog.Info("exiting")
	klog.Flush()
}
