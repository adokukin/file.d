//go:build e2e_new

package e2e_test

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/ozontech/file.d/cfg"
	"github.com/ozontech/file.d/e2e/file_file"
	"github.com/ozontech/file.d/e2e/http_file"
	"github.com/ozontech/file.d/e2e/kafka_file"
	"github.com/ozontech/file.d/fd"
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
)

// e2eTest is the general interface for e2e tests
// Configure prepares config for your test
// Send sends message in pipeline and waits for the end of processing
// Validate validates result of the work
type e2eTest interface {
	Configure(t *testing.T, conf *cfg.Config, pipelineName string)
	Send(t *testing.T)
	Validate(t *testing.T)
}

func startForTest(t *testing.T, e e2eTest, configPath string) *fd.FileD {
	conf := cfg.NewConfigFromFile(configPath)
	var pipelineName string
	for pipelineName = range conf.Pipelines {
		break
	}
	e.Configure(t, conf, pipelineName)
	filed := fd.New(conf, "off")
	filed.Start()
	return filed
}

func TestE2EStabilityWorkCase(t *testing.T) {
	testsList := []struct {
		e2eTest
		cfgPath string
	}{
		{
			e2eTest: &file_file.Config{
				Count:   10,
				Lines:   500,
				RetTime: "1s",
			},
			cfgPath: "./file_file/config.yml",
		},
		{
			e2eTest: &http_file.Config{
				Count:   10,
				Lines:   500,
				RetTime: "1s",
			},
			cfgPath: "./http_file/config.yml",
		},
		{
			e2eTest: &kafka_file.Config{
				Topic:     "quickstart5",
				Broker:    "localhost:9092",
				Count:     500,
				RetTime:   "1s",
				Partition: 4,
			},
			cfgPath: "./kafka_file/config.yml",
		},
	}

	for _, test := range testsList {
		filed := startForTest(t, test.e2eTest, test.cfgPath)
		test.Send(t)
		test.Validate(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := filed.Stop(ctx)
		cancel()
		if err != nil {
			log.Fatalf("failed to stop filed: %s", err.Error())
		}
	}
}
