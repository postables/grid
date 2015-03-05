package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	"github.com/lytics/grid"
	metrics "github.com/rcrowley/go-metrics"
)

const (
	Key          = ""
	GridName     = "loadtest-grid"
	ConsumerName = "loadtest-grid-console-consumer"
)

type NumMesg struct {
	Data int
}

func NewNumMesg(i int) *NumMesg {
	return &NumMesg{i}
}

type numcoder struct {
	*json.Encoder
	*json.Decoder
}

func (c *numcoder) New() interface{} {
	return &NumMesg{}
}

func NewNumMesgDecoder(r io.Reader) grid.Decoder {
	return &numcoder{nil, json.NewDecoder(r)}
}

func NewNumMesgEncoder(w io.Writer) grid.Encoder {
	return &numcoder{json.NewEncoder(w), nil}
}

var peercnt = flag.Int("peercnt", 1, "the expected number of peers that will take part in the grid.")
var mode = flag.Int("mode", 1, "the mode to run this process in.  [1] Run as the load_test grid [2] Generate messages (producer)")
var kafka = flag.String("kafka", "localhost:10092", `listof kafka brokers.  example: "localhost:10092,localhost:10093"`)
var khosts []string

/*
	topology map:

	(generateTestMessages)
	 \->add1
	    \->(add)->mulBy2
	              \->(mul)->divBy2
	                	     \->(div)->sub1
	                		            \->(sub)->collector
	                		                       \->(coll)-->StdOut
*/
func main() {
	flag.Parse()

	khosts = strings.Split(*kafka, ",")

	go logMetrics()
	if *mode == 1 {
		runtime.GOMAXPROCS(3)

		kconf := grid.DefaultKafkaConfig()
		kconf.Brokers = khosts

		g, err := grid.NewWithKafkaConfig(GridName, *peercnt, kconf)
		if err != nil {
			log.Fatalf("error: example: failed to create grid: %v", err)
		}

		g.AddDecoder(NewNumMesgDecoder, "add1", "mulBy2", "divBy2", "sub1", "collector")
		g.AddEncoder(NewNumMesgEncoder, "add1", "mulBy2", "divBy2", "sub1", "collector")

		err = g.Add("add1", 3, newAdd(), "add1")
		if err != nil {
			log.Fatalf("error: example: %v", err)
		}

		err = g.Add("mulBy2", 3, newMul(), "mulBy2")
		if err != nil {
			log.Fatalf("error: example: %v", err)
		}

		err = g.Add("divBy2", 3, newDiv(), "divBy2")
		if err != nil {
			log.Fatalf("error: example: %v", err)
		}

		err = g.Add("sub1", 3, newSub(), "sub1")
		if err != nil {
			log.Fatalf("error: example: %v", err)
		}

		err = g.Add("collector", 1, newCollector(), "collector")
		if err != nil {
			log.Fatalf("error: example: %v", err)
		}

		g.Start()

		g.Wait()
	} else if *mode == 2 {
		generateTestMessages()
	}
}

func logMetrics() {
	ticker := time.NewTicker(time.Second * 10)

	for now := range ticker.C {
		fmt.Println("------------ ", now, " ---------------")
		bytes, _ := json.MarshalIndent(metrics.DefaultRegistry, " ", " ")
		fmt.Println(string(bytes))
	}
}

func generateTestMessages() {
	client, err := sarama.NewClient(ConsumerName, khosts, sarama.NewClientConfig())
	if err != nil {
		log.Fatalf("failed to create kafka client: %v", err)
	}
	defer client.Close()

	producer, err := sarama.NewProducer(client, nil)
	if err != nil {
		log.Fatalf("failed to create kafka producer: %v", err)
	}
	defer producer.Close()

	log.Println("Starting to generate test messages.")

	gen := metrics.NewMeter()
	metrics.GetOrRegister("gen.msg.counter", gen)

	for i := 1; i < 10000000; i++ {
		data := struct {
			Data int
		}{
			i,
		}
		if bytes, err := json.Marshal(data); err != nil {
			log.Printf("error: %v", err)
		} else {
			key := fmt.Sprintf("key-%d", i)

			select {
			case producer.Input() <- &sarama.MessageToSend{
				Topic: "add1",
				Key:   sarama.StringEncoder(key),
				Value: sarama.ByteEncoder(bytes),
			}:
				gen.Mark(1)
			case err := <-producer.Errors():
				panic(err.Err)
			}
		}
	}
}

type add struct {
	name string
}

func newAdd() grid.NewActor {
	return func(name string, id int) grid.Actor { return &add{name: name} }
}

func (a *add) Act(in <-chan grid.Event, state <-chan grid.Event) <-chan grid.Event {
	fmt.Printf("%v: started", a.name)
	out := make(chan grid.Event)
	go func() {
		defer close(out)
		for {
			select {
			case e := <-state:
				switch mesg := e.Message().(type) {
				case *grid.MinMaxOffset:
					mesg.UseMax()
				}
			case e := <-in:
				switch mesg := e.Message().(type) {
				case *NumMesg:
					outmsg := 1 + mesg.Data
					key := fmt.Sprintf("%d", mesg.Data)
					out <- grid.NewWritable("mulBy2", []byte(key), NewNumMesg(outmsg))
				default:
					log.Printf("example: unknown message: %T :: %v", mesg, mesg)
				}
			}
		}
	}()

	return out
}

type mul struct {
	name string
}

func newMul() grid.NewActor {
	return func(name string, id int) grid.Actor { return &mul{name: name} }
}

func (m *mul) Act(in <-chan grid.Event, state <-chan grid.Event) <-chan grid.Event {
	fmt.Printf("%v: started", m.name)
	out := make(chan grid.Event)
	go func() {
		defer close(out)
		for {
			select {
			case e := <-state:
				switch mesg := e.Message().(type) {
				case *grid.MinMaxOffset:
					mesg.UseMax()
				}
			case e := <-in:
				switch mesg := e.Message().(type) {
				case *NumMesg:
					outmsg := 2 * mesg.Data
					key := fmt.Sprintf("%d", mesg.Data)
					out <- grid.NewWritable("divBy2", []byte(key), NewNumMesg(outmsg))
				default:
					log.Printf("example: unknown message: %T :: %v", mesg, mesg)
				}
			}
		}
	}()

	return out
}

type div struct {
	name string
}

func newDiv() grid.NewActor {
	return func(name string, id int) grid.Actor { return &div{name: name} }
}

func (d *div) Act(in <-chan grid.Event, state <-chan grid.Event) <-chan grid.Event {
	fmt.Printf("%v: started", d.name)
	out := make(chan grid.Event)
	go func() {
		defer close(out)
		for {
			select {
			case e := <-state:
				switch mesg := e.Message().(type) {
				case *grid.MinMaxOffset:
					mesg.UseMax()
				}
			case e := <-in:
				switch mesg := e.Message().(type) {
				case *NumMesg:
					outmsg := mesg.Data / 2
					key := fmt.Sprintf("%d", mesg.Data)
					out <- grid.NewWritable("sub1", []byte(key), NewNumMesg(outmsg))
				default:
					log.Printf("example: unknown message: %T :: %v", mesg, mesg)
				}
			}
		}
	}()

	return out
}

type sub struct {
	name string
}

func newSub() grid.NewActor {
	return func(name string, id int) grid.Actor { return &sub{name: name} }
}

func (s *sub) Act(in <-chan grid.Event, state <-chan grid.Event) <-chan grid.Event {
	fmt.Printf("%v: started", s.name)
	out := make(chan grid.Event)

	go func() {
		defer close(out)
		for {
			select {
			case e := <-state:
				switch mesg := e.Message().(type) {
				case *grid.MinMaxOffset:
					mesg.UseMax()
				}
			case e := <-in:
				switch mesg := e.Message().(type) {
				case *NumMesg:
					outmsg := mesg.Data - 1
					key := fmt.Sprintf("%d", mesg.Data)
					out <- grid.NewWritable("collector", []byte(key), NewNumMesg(outmsg))
				default:
					log.Printf("example: unknown message: %T :: %v", mesg, mesg)
				}
			}
		}
	}()

	return out
}

type collector struct {
	name string
}

func newCollector() grid.NewActor {
	return func(name string, id int) grid.Actor { return &collector{name: name} }
}

func (c *collector) Act(in <-chan grid.Event, state <-chan grid.Event) <-chan grid.Event {
	fmt.Printf("%v: started", c.name)
	out := make(chan grid.Event)

	go func() {
		defer close(out)
		for {
			select {
			case e := <-state:
				switch mesg := e.Message().(type) {
				case *grid.MinMaxOffset:
					mesg.UseMax()
				}
			case e := <-in:
				switch mesg := e.Message().(type) {
				case *NumMesg:
				default:
					log.Printf("example: unknown message: %T :: %v", mesg, mesg)
				}
			}
		}
	}()

	return out
}