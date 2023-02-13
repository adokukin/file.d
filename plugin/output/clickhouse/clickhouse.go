package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"
	"strconv"

	sq "github.com/Masterminds/squirrel"
	"database/sql"
	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ozontech/file.d/cfg"
	"github.com/ozontech/file.d/fd"
	"github.com/ozontech/file.d/metric"
	"github.com/ozontech/file.d/pipeline"
	prom "github.com/prometheus/client_golang/prometheus"
	insaneJSON "github.com/vitkovskii/insane-json"
	"go.uber.org/zap"
)

/*{ introduction
It sends the event batches to clickhouse db using clickhouse-go.
}*/

var (
	ErrEventDoesntHaveField             = errors.New("event doesn't have field")
	ErrEventFieldHasWrongType           = errors.New("event field has wrong type")
	ErrTimestampFromDistantPastOrFuture = errors.New("event field contains timestamp < 1970 or > 9000 year")
)

type DBIface interface {
	Ping() (error)
	Close() (error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const (
	outPluginType = "clickhouse"

	nineThousandYear = 221842627200
)

type chType int

const (
	// minimum required types for now.
	unknownType chType = iota 
	chString 
	chInt 
	chTimestamp 
	chTimestring // k8s input produces time as 2022-12-11T20:20:10.037011974Z or 2022-12-11T20:20:10.037Z string 
	             // instead of int, at least in my setup
	chMilliseconds // it is incompatible with Clickhouse and has to be split into two fields: datetime and milliseconds
)

const (
	colTypeInt       = "int"
	colTypeString    = "string"
	colTypeTimestamp = "timestamp"
	colTypeTimestring = "timestring"
)

type Plugin struct {
	controller pipeline.OutputPluginController
	logger     *zap.SugaredLogger
	config     *Config
	batcher    *pipeline.Batcher
	ctx        context.Context
	cancelFunc context.CancelFunc

	queryBuilder ClickhouseQueryBuilder
	pool       DBIface

	// plugin metrics

	discardedEventMetric  *prom.CounterVec
	writtenEventMetric    *prom.CounterVec
}

type ConfigColumn struct {
	Name       string `json:"name" required:"true"`
	ColumnType string `json:"type" required:"true" options:"int|string|bool|timestamp|timestring"`
}

// ! config-params
// ^ config-params
type Config struct {
	// > @3@4@5@6
	// >
	// > In strict mode file.d will crash on events without required columns.
	// Otherwise events will be discarded.
	Strict bool `json:"strict" default:"false"` // *

	// > @3@4@5@6
	// >
	// > ClickhouseSQL connection string in DSN format.
	// >
	// > Example DSN:
	// >
	// > `clickhouse://username:password@host1:9000,host2:9000/database?dial_timeout=200ms&max_execution_time=60`
	ConnString string `json:"conn_string" required:"true"` // *

	// > @3@4@5@6
	// >
	// > Clickhouse target table.
	Table string `json:"table" required:"true"` // *

	// > @3@4@5@6
	// >
	// > Array of DB columns. Each column have:
	// > name and type (int, string, timestamp - which int that will be converted to timestamptz of rfc3339)
	Columns []ConfigColumn `json:"columns" required:"true" slice:"true"` // *

	// > @3@4@5@6
	// >
	// > Retries of insertion.
	Retry int `json:"retry" default:"3"` // *

	// > @3@4@5@6
	// >
	// > Retention milliseconds for retry to DB.
	Retention  cfg.Duration `json:"retention" default:"50ms" parse:"duration"` // *
	Retention_ time.Duration

	// > @3@4@5@6
	// >
	// > Timeout for DB requests in milliseconds.
	DBRequestTimeout  cfg.Duration `json:"db_request_timeout" default:"3000ms" parse:"duration"` // *
	DBRequestTimeout_ time.Duration

	// > @3@4@5@6
	// >
	// > How much workers will be instantiated to send batches.
	WorkersCount  cfg.Expression `json:"workers_count" default:"gomaxprocs*4" parse:"expression"` // *
	WorkersCount_ int

	// > @3@4@5@6
	// >
	// > Maximum quantity of events to pack into one batch.
	BatchSize cfg.Expression `json:"batch_size" default:"capacity/4"  parse:"expression"` // *
	//BatchSize  cfg.Expression `json:"batch_size" default:"capacity/4" parse:"expression"` // *
	BatchSize_ int

	// > @3@4@5@6
	// >
	// > A minimum size of events in a batch to send.
	// > If both batch_size and batch_size_bytes are set, they will work together.
	BatchSizeBytes  cfg.Expression `json:"batch_size_bytes" default:"0" parse:"expression"` // *
	BatchSizeBytes_ int

	// > @3@4@5@6
	// >
	// > After this timeout batch will be sent even if batch isn't completed.
	BatchFlushTimeout  cfg.Duration `json:"batch_flush_timeout" default:"200ms" parse:"duration"` // *
	BatchFlushTimeout_ time.Duration
}

func init() {
	fd.DefaultPluginRegistry.RegisterOutput(&pipeline.PluginStaticInfo{
		Type:    outPluginType,
		Factory: Factory,
	})
}

func Factory() (pipeline.AnyPlugin, pipeline.AnyConfig) {
	return &Plugin{}, &Config{}
}

func (p *Plugin) RegisterMetrics(ctl *metric.Ctl) {
	p.discardedEventMetric = ctl.RegisterCounter("output_clickhouse_event_discarded", "Total ??? discarded messages")
	p.writtenEventMetric = ctl.RegisterCounter("output_clickhouse_event_written", "Total events written to ???")
}

func (p *Plugin) Start(config pipeline.AnyConfig, params *pipeline.OutputPluginParams) {
	p.controller = params.Controller
	p.logger = params.Logger
	p.config = config.(*Config)
	p.ctx = context.Background()
	if len(p.config.Columns) == 0 {
		p.logger.Fatal("can't start plugin, no fields in config")
	}

	if p.config.Retry < 1 {
		p.logger.Fatal("'retry' can't be <1")
	}
	if p.config.Retention_ < 1 {
		p.logger.Fatal("'renetion' can't be <1")
	}
	if p.config.DBRequestTimeout_ < 1 {
		p.logger.Fatal("'db_request_timeout' can't be <1")
	}

	queryBuilder, err := NewQueryBuilder(p.config.Columns, p.config.Table)
	if err != nil {
		p.logger.Fatal(err)
	}
	p.queryBuilder = queryBuilder

	pool, err := sql.Open("clickhouse", p.config.ConnString)
	if err != nil {
		p.logger.Fatalf("can't create sql.DB: %v", err)
	}

	pool.SetConnMaxLifetime(0)
	pool.SetMaxIdleConns(p.config.WorkersCount_)
	pool.SetMaxOpenConns(p.config.WorkersCount_)

	err = pool.Ping()
	if err != nil {
		p.logger.Fatalf("can't connect clickhouse: %v", err)
	}
	p.pool = pool

	p.batcher = pipeline.NewBatcher(pipeline.BatcherOptions{
		PipelineName:   params.PipelineName,
		OutputType:     outPluginType,
		OutFn:          p.out,
		Controller:     p.controller,
		Workers:        p.config.WorkersCount_,
		BatchSizeCount: p.config.BatchSize_,
		BatchSizeBytes: p.config.BatchSizeBytes_,
		FlushTimeout:   p.config.BatchFlushTimeout_,
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.ctx = ctx
	p.cancelFunc = cancel

	p.batcher.Start(ctx)
}

func (p *Plugin) Stop() {
	p.cancelFunc()
	p.batcher.Stop()
	p.pool.Close()
}

func (p *Plugin) Out(event *pipeline.Event) {
	p.batcher.Add(event)
}

func (p *Plugin) out(_ *pipeline.WorkerData, batch *pipeline.Batch) {
	// _ *pipeline.WorkerData - doesn't required in this plugin, we can't parse
	// events for uniques through bytes.
	builder := p.queryBuilder.GetInsertBuilder()
	chFields := p.queryBuilder.GetClickhouseFields()

	// contrary to postgres we don't mess with deduplication
	// it's to be handled on Clickhouse side if necessary

	validEvents := 0
	for _, event := range batch.Events {
		fieldValues, err := p.processEvent(event, chFields)
		if err != nil {
			if errors.Is(err, ErrEventDoesntHaveField) {
				p.discardedEventMetric.WithLabelValues().Inc()
				if p.config.Strict {
					p.logger.Fatal(err)
				}
				p.logger.Error(err)
			} else if errors.Is(err, ErrEventFieldHasWrongType) {
				p.discardedEventMetric.WithLabelValues().Inc()
				if p.config.Strict {
					p.logger.Fatal(err)
				}
				p.logger.Error(err)
			} else if errors.Is(err, ErrTimestampFromDistantPastOrFuture) {
				p.discardedEventMetric.WithLabelValues().Inc()
				if p.config.Strict {
					p.logger.Fatal(err)
				}
				p.logger.Error(err)
			} else if err != nil { // protection from foolproof.
				p.logger.Fatalf("undefined error: %w", err)
			}

			continue
		}

		// passes here only if event valid.
		builder = builder.Values(fieldValues...)
		validEvents += 1
	}

	builder = builder.PlaceholderFormat(sq.Question)

	// no valid events passed.
	if validEvents == 0 {
		return
	}

	query, args, err := builder.ToSql()
	if err != nil {
		p.logger.Fatalf("Invalid SQL. query: %s, args: %v, err: %v", query, args, err)
	}

	var argsSliceInterface = make([]any, len(args)) // TODO: check if args needed

	for i := 0; i < len(args); i++ {
		argsSliceInterface[i] = args[i]
	}

	// Insert into Clickhouse with retry.
	for i := p.config.Retry; i > 0; i-- {
		err = p.try(query, argsSliceInterface)
		if err != nil {
			p.logger.Errorf("can't exec query: %s", err.Error())
			time.Sleep(p.config.Retention_)
			continue
		}
		p.writtenEventMetric.WithLabelValues().Add(float64(len(batch.Events)))
		break
	}

	if err != nil {
		p.pool.Close()
		p.logger.Fatalf("failed insert into %s. query: %s, args: %v, err: %v", p.config.Table, query, args, err)
	}
}

func (p *Plugin) try(query string, argsSliceInterface []any) error {
	ctx, cancel := context.WithTimeout(p.ctx, p.config.DBRequestTimeout_)
	defer cancel()

	_, err := p.pool.ExecContext(ctx, query, argsSliceInterface...) // TODO:
	if err != nil {
		return err
	}

	return err
}

func (p *Plugin) processEvent(event *pipeline.Event, chFields []column) (fieldValues []any, err error) {
	fieldValues = make([]any, 0, len(chFields))

	for _, field := range chFields {
		fieldNode, err := event.Root.DigStrict(field.Name)
		if err != nil {
			return nil, fmt.Errorf("%w. required field %s", ErrEventDoesntHaveField, field.Name)
		}

		lVal, err := p.addFieldToValues(field, fieldNode)
		if err != nil {
			return nil, err
		}

		fieldValues = append(fieldValues, lVal)
	}

	return fieldValues, nil
}

func (p *Plugin) addFieldToValues(field column, sNode *insaneJSON.StrictNode) (any, error) {
	switch field.ColType {
	case chString:
		lVal, err := sNode.AsString()
		if err != nil {
			return nil, fmt.Errorf("%w, can't get %s as string, err: %s", ErrEventFieldHasWrongType, field.Name, err.Error())
		}
		return lVal, nil
	case chInt:
		lVal, err := sNode.AsInt()
		if err != nil {
			return nil, fmt.Errorf("%w, can't get %s as int, err: %s", ErrEventFieldHasWrongType, field.Name, err.Error())
		}
		return lVal, nil
	case chTimestamp:
		tint, err := sNode.AsInt()
		if err != nil {
			return nil, fmt.Errorf("%w, can't get %s as timestamp, err: %s", ErrEventFieldHasWrongType, field.Name, err.Error())
		}
		// if timestamp < 1970-01-00-00 or > 9000-01-00-00
		if tint < 0 || tint > nineThousandYear {
			return nil, fmt.Errorf("%w, %s", ErrTimestampFromDistantPastOrFuture, field.Name)
		}
		return time.Unix(int64(tint), 0).Format(time.RFC3339), nil
	case chTimestring:
		nLen := 19
		lVal, err := sNode.AsString()
		if err != nil {
			return nil, fmt.Errorf("%w, can't get %s as string, err: %s", ErrEventFieldHasWrongType, field.Name, err.Error())
		}
		if len([]rune(lVal)) < nLen {
			return nil, fmt.Errorf("%w, can't get %s as timestring, as it's shorter than %d symbols", ErrEventFieldHasWrongType, field.Name, nLen)
		}
		return lVal[:nLen], nil
	case chMilliseconds:
		nLen := 23
		lVal, err := sNode.AsString()
		if err != nil {
			return nil, fmt.Errorf("%w, can't get %s as string, err: %s", ErrEventFieldHasWrongType, field.Name, err.Error())
		}
		if len([]rune(lVal)) < nLen {
			return nil, fmt.Errorf("%w, can't get %s as nanostring, as it's shorter than %d symbols", ErrEventFieldHasWrongType, field.Name, nLen)
		}
		nVal, err := strconv.ParseInt(lVal[20:23], 10, 16)
		if err != nil {
			return nil, fmt.Errorf("%w, can't get %s as nanostring, can't convert %s to Int32", ErrEventFieldHasWrongType, field.Name, lVal[20:23])
		}
		return int16(nVal), nil
	case unknownType:
		fallthrough
	default:
		return nil, fmt.Errorf("%w, undefined col type: %d, col name: %s", ErrEventFieldHasWrongType, field.ColType, field.Name)
	}
}

