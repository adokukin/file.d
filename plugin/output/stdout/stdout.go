package stdout

import (
	"fmt"

	"github.com/ozontech/file.d/fd"
	"github.com/ozontech/file.d/pipeline"
	"github.com/ozontech/file.d/plugin"
)

/*{ introduction
It writes events to stdout(also known as console).
}*/

type Plugin struct {
	controller pipeline.OutputPluginController
	plugin.NoMetricsPlugin
}

type Config struct{}

func init() {
	fd.DefaultPluginRegistry.RegisterOutput(&pipeline.PluginStaticInfo{
		Type:    "stdout",
		Factory: Factory,
	})
}

func Factory() (pipeline.AnyPlugin, pipeline.AnyConfig) {
	return &Plugin{}, &Config{}
}

func (p *Plugin) Start(_ pipeline.AnyConfig, params *pipeline.OutputPluginParams) {
	p.controller = params.Controller
}

func (_ *Plugin) Stop() {}

func (p *Plugin) Out(event *pipeline.Event) {
	fmt.Println(event.Root.EncodeToString())
	p.controller.Commit(event)
}
