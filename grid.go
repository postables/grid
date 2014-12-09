package grid

import (
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	metrics "github.com/rcrowley/go-metrics"
)

type Decoder interface {
	New() interface{}
	Decode(d interface{}) error
}

type Encoder interface {
	Encode(e interface{}) error
}

type Grid struct {
	log      ReadWriteLog
	gridname string
	cmdtopic string
	npeers   int
	quorum   uint32
	parts    map[string][]int32
	ops      map[string]*op
	wg       *sync.WaitGroup
	exit     chan bool
	registry metrics.Registry
	// Test hook, normaly should be 0.
	maxleadertime int64
}

func DefaultKafkaConfig() *KafkaConfig {
	brokers := []string{"localhost:10092"}

	pconfig := sarama.NewProducerConfig()
	pconfig.FlushMsgCount = 10000
	pconfig.FlushFrequency = 1 * time.Second
	cconfig := sarama.NewConsumerConfig()
	cconfig.DefaultFetchSize = 512000
	cconfig.OffsetMethod = sarama.OffsetMethodNewest

	return &KafkaConfig{
		Brokers:        brokers,
		ClientConfig:   sarama.NewClientConfig(),
		ProducerConfig: pconfig,
		ConsumerConfig: cconfig,
	}
}

func New(gridname string, npeers int) (*Grid, error) {
	return NewWithKafkaConfig(gridname, npeers, DefaultKafkaConfig())
}

func NewWithKafkaConfig(gridname string, npeers int, kconfig *KafkaConfig) (*Grid, error) {
	cmdtopic := gridname + "-cmd"

	kconfig.cmdtopic = cmdtopic
	kconfig.basename = gridname

	rwlog, err := NewKafkaReadWriteLog(buildPeerName(0), kconfig)
	if err != nil {
		return nil, err
	}

	g := &Grid{
		log:      rwlog,
		gridname: gridname,
		cmdtopic: cmdtopic,
		npeers:   npeers,
		quorum:   uint32((npeers / 2) + 1),
		parts:    make(map[string][]int32),
		ops:      make(map[string]*op),
		wg:       new(sync.WaitGroup),
		exit:     make(chan bool),
	}

	g.wg.Add(1)

	g.AddDecoder(NewCmdMesgDecoder, cmdtopic)
	g.AddEncoder(NewCmdMesgEncoder, cmdtopic)

	return g, nil
}

func (g *Grid) Start() error {
	if g.registry == nil {
		g.registry = metrics.DefaultRegistry
	}

	voter := NewVoter(0, g)
	manager := NewManager(0, g)

	// Why are these both read-only channels?
	// Because:
	//  * The reader's output is our input.
	//  * Our output is the writer's input.
	var in <-chan Event
	var out <-chan Event

	// Command topic only uses the 0 partition
	// to make sure all communication has a
	// total ordering.
	cmdpart := []int32{0}

	in = g.log.Read(g.cmdtopic, cmdpart)
	out = voter.startStateMachine(in)
	g.log.Write(g.cmdtopic, out)

	in = g.log.Read(g.cmdtopic, cmdpart)
	out = manager.startStateMachine(in)
	g.log.Write(g.cmdtopic, out)

	return nil
}

func (g *Grid) Wait() {
	g.wg.Wait()
}

func (g *Grid) Stop() {
	close(g.exit)
	g.wg.Done()
}

func (g *Grid) UseMetrics(registry metrics.Registry) {
	g.registry = registry
}

func (g *Grid) AddDecoder(makeDecoder func(io.Reader) Decoder, topics ...string) {
	g.log.AddDecoder(makeDecoder, topics...)
}

func (g *Grid) AddEncoder(makeEncoder func(io.Writer) Encoder, topics ...string) {
	g.log.AddEncoder(makeEncoder, topics...)
}

func (g *Grid) AddPartitioner(p func(key io.Reader, parts int32) int32, topics ...string) {
	g.log.AddPartitioner(p, topics...)
}

func (g *Grid) Add(fname string, n int, f func(in <-chan Event) <-chan Event, topics ...string) error {
	if _, exists := g.ops[fname]; exists {
		return fmt.Errorf("gird: already added: %v", fname)
	}

	op := &op{f: f, n: n, inputs: make(map[string]bool)}

	for _, topic := range topics {
		if _, found := g.log.DecodedTopics()[topic]; !found {
			return fmt.Errorf("grid: topic: %v: no decoder found for topic", topic)
		}
		op.inputs[topic] = true

		// Discover the available partitions for the topic.
		parts, err := g.log.Partitions(topic)
		if err != nil {
			return fmt.Errorf("grid: topic: %v: failed getting partition data: %v", topic, err)
		}
		g.parts[topic] = parts

		if len(parts) > n {
			return fmt.Errorf("grid: topic: %v: parallelism of function is greater than number of partitions: func: %v, partitions: %d", topic, fname, len(parts))
		}
	}

	g.ops[fname] = op

	return nil
}

func (g *Grid) startinst(inst *Instance) {
	fname := inst.Fname

	// Check that this instance was added by the lib user.
	if _, exists := g.ops[fname]; !exists {
		log.Fatalf("fatal: grid: does not exist: %v()", fname)
	}

	// Setup all the topic readers for this instance of the function.
	ins := make([]<-chan Event, 0)
	for topic, parts := range inst.TopicSlices {
		if !g.ops[fname].inputs[topic] {
			log.Fatalf("fatal: grid: %v(): not set as reader of: %v", fname, topic)
		}

		log.Printf("grid: starting: %v: instance: %v: topic: %v: partitions: %v", fname, inst.Id, topic, parts)
		ins = append(ins, g.log.Read(topic, parts))
	}

	// The out channel will be used by this instance so send data to
	// the read-write log.
	out := g.ops[fname].f(g.merge(fname, ins))

	// The messages on the out channel are de-mux'ed and put on
	// topic specific out channels.
	outs := make(map[string]chan Event)
	for topic, _ := range g.log.EncodedTopics() {
		outs[topic] = make(chan Event, 1024)
		g.log.Write(topic, outs[topic])

		go func(fname string, out <-chan Event, outs map[string]chan Event) {
			for event := range out {
				if topicout, found := outs[event.Topic()]; found {
					topicout <- event
				} else {
					log.Fatalf("fatal: grid: %v(): not set as writer of: %v", fname, event.Topic())
				}
			}
		}(fname, out, outs)
	}
}

type op struct {
	n      int
	f      func(in <-chan Event) <-chan Event
	inputs map[string]bool
}

func (g *Grid) merge(fname string, ins []<-chan Event) <-chan Event {
	meter := metrics.GetOrRegisterMeter(fname+"-msg-rate", g.registry)
	merged := make(chan Event, 1024)
	for _, in := range ins {
		go func(in <-chan Event, meter metrics.Meter) {
			for m := range in {
				merged <- m
				meter.Mark(1)
			}
		}(in, meter)
	}
	return merged
}
