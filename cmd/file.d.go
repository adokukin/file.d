package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/alecthomas/kingpin"
	"github.com/ozontech/file.d/buildinfo"
	"github.com/ozontech/file.d/cfg"
	"github.com/ozontech/file.d/fd"
	"github.com/ozontech/file.d/logger"
	"github.com/ozontech/file.d/longpanic"
	"github.com/ozontech/file.d/pipeline"
	_ "github.com/ozontech/file.d/plugin/action/add_host"
	_ "github.com/ozontech/file.d/plugin/action/convert_date"
	_ "github.com/ozontech/file.d/plugin/action/convert_log_level"
	_ "github.com/ozontech/file.d/plugin/action/debug"
	_ "github.com/ozontech/file.d/plugin/action/discard"
	_ "github.com/ozontech/file.d/plugin/action/flatten"
	_ "github.com/ozontech/file.d/plugin/action/join"
	_ "github.com/ozontech/file.d/plugin/action/join_template"
	_ "github.com/ozontech/file.d/plugin/action/json_decode"
	_ "github.com/ozontech/file.d/plugin/action/json_encode"
	_ "github.com/ozontech/file.d/plugin/action/keep_fields"
	_ "github.com/ozontech/file.d/plugin/action/mask"
	_ "github.com/ozontech/file.d/plugin/action/modify"
	_ "github.com/ozontech/file.d/plugin/action/parse_es"
	_ "github.com/ozontech/file.d/plugin/action/parse_re2"
	_ "github.com/ozontech/file.d/plugin/action/remove_fields"
	_ "github.com/ozontech/file.d/plugin/action/rename"
	_ "github.com/ozontech/file.d/plugin/action/set_time"
	_ "github.com/ozontech/file.d/plugin/action/throttle"
	_ "github.com/ozontech/file.d/plugin/input/dmesg"
	_ "github.com/ozontech/file.d/plugin/input/fake"
	_ "github.com/ozontech/file.d/plugin/input/file"
	_ "github.com/ozontech/file.d/plugin/input/http"
	_ "github.com/ozontech/file.d/plugin/input/journalctl"
	_ "github.com/ozontech/file.d/plugin/input/k8s"
	_ "github.com/ozontech/file.d/plugin/input/kafka"
	_ "github.com/ozontech/file.d/plugin/output/devnull"
	_ "github.com/ozontech/file.d/plugin/output/elasticsearch"
	_ "github.com/ozontech/file.d/plugin/output/file"
	_ "github.com/ozontech/file.d/plugin/output/gelf"
	_ "github.com/ozontech/file.d/plugin/output/kafka"
	_ "github.com/ozontech/file.d/plugin/output/postgres"
	_ "github.com/ozontech/file.d/plugin/output/s3"
	_ "github.com/ozontech/file.d/plugin/output/splunk"
	_ "github.com/ozontech/file.d/plugin/output/stdout"
	insaneJSON "github.com/vitkovskii/insane-json"
	"go.uber.org/automaxprocs/maxprocs"
)

var (
	fileD *fd.FileD
	exit  = make(chan bool)

	config        = kingpin.Flag("config", `Config file name`).Required().ExistingFile()
	http          = kingpin.Flag("http", `HTTP listen addr eg. ":9000", "off" to disable`).Default(":9000").String()
	memLimitRatio = kingpin.Flag(
		"mem-limit-ratio",
		`Value to set GOMEMLIMIT (https://pkg.go.dev/runtime) with the value from the cgroup's memory limit and given ratio. `+
			`If there is a need to reduce the load GC, it is recommended to set 0.9. Default is disabled.`,
	).Default("0").Float64()
)

func main() {
	kingpin.Version(buildinfo.Version)
	kingpin.Parse()

	logger.Infof("Hi! I'm file.d version=%s %s", buildinfo.Version, buildinfo.BuildTime)

	setRuntimeSettings()
	insaneJSON.DisableBeautifulErrors = true
	insaneJSON.StartNodePoolSize = pipeline.DefaultJSONNodePoolSize

	_, _ = maxprocs.Set(maxprocs.Logger(logger.Debugf))

	go listenSignals()
	longpanic.Go(start)

	<-exit
	logger.Infof("see you soon...")
}

func start() {
	appCfg := cfg.NewConfigFromFile(*config)
	longpanic.SetTimeout(appCfg.PanicTimeout)

	fileD = fd.New(appCfg, *http)
	fileD.Start()
}

func listenSignals() {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	for {
		s := <-signalChan

		switch s {
		case syscall.SIGHUP:
			logger.Infof("SIGHUP received")

			ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
			err := fileD.Stop(ctx)
			if err != nil {
				logger.Fatalf("can't stop file.d with SIGHUP: %s", err.Error())
			}
			cancel()

			start()
		case syscall.SIGINT, syscall.SIGTERM:
			logger.Infof("SIGTERM or SIGINT received")

			ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
			err := fileD.Stop(ctx)
			if err != nil {
				logger.Fatalf("can't stop file.d with SIGTERM or SIGINT: %s", err.Error())
			}
			cancel()

			exit <- true
		}
	}
}

func setRuntimeSettings() {
	if *memLimitRatio == 0 {
		return
	}

	memLimit, err := memlimit.SetGoMemLimit(*memLimitRatio)
	if err != nil {
		logger.Fatal("can't set GOMEMLIMIT: %s", err)
	}
	logger.Warnf("GOMEMLIMIT=%v", memLimit)

}
