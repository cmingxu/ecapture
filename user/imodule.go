package user

import (
	"context"
	"ecapture/pkg/event_processor"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
)

type IModule interface {
	// Init 初始化
	Init(context.Context, *log.Logger, IConfig) error

	// Name 获取当前module的名字
	Name() string

	// Run 事件监听感知
	Run() error

	// Start 启动模块
	Start() error

	// Stop 停止模块
	Stop() error

	// Close 关闭退出
	Close() error

	SetChild(module IModule)

	Decode(*ebpf.Map, []byte) (event_processor.IEventStruct, error)

	Events() []*ebpf.Map

	DecodeFun(p *ebpf.Map) (event_processor.IEventStruct, bool)

	Dispatcher(event_processor.IEventStruct)
}

type Module struct {
	opts   *ebpf.CollectionOptions
	reader []IClose
	ctx    context.Context
	logger *log.Logger
	child  IModule
	// probe的名字
	name string

	// module的类型，uprobe,kprobe等
	mType string

	conf IConfig

	processor *event_processor.EventProcessor
}

// Init 对象初始化
func (this *Module) Init(ctx context.Context, logger *log.Logger) {
	this.ctx = ctx
	this.logger = logger
	this.processor = event_processor.NewEventProcessor(logger)
}

func (this *Module) SetChild(module IModule) {
	this.child = module
}

func (this *Module) Start() error {
	panic("Module.Start() not implemented yet")
}

func (this *Module) Events() []*ebpf.Map {
	panic("Module.Events() not implemented yet")
}

func (this *Module) DecodeFun(p *ebpf.Map) (event_processor.IEventStruct, bool) {
	panic("Module.DecodeFun() not implemented yet")
}

func (this *Module) Name() string {
	return this.name
}

func (this *Module) Run() error {
	this.logger.Printf("%s\tModule.Run()", this.Name())
	//  start
	err := this.child.Start()
	if err != nil {
		return err
	}

	go func() {
		this.run()
	}()

	go func() {
		this.processor.Serve()
	}()

	err = this.readEvents()
	if err != nil {
		return err
	}

	return nil
}
func (this *Module) Stop() error {
	return nil
}

// Stop shuts down Module
func (this *Module) run() {
	for {
		select {
		case _ = <-this.ctx.Done():
			err := this.child.Stop()
			if err != nil {
				this.logger.Fatalf("%s\t stop Module error:%v.", this.child.Name(), err)
			}
			return
		}
	}
}

func (this *Module) readEvents() error {
	var errChan = make(chan error, 8)
	for _, event := range this.child.Events() {
		switch {
		case event.Type() == ebpf.RingBuf:
			go this.ringbufEventReader(errChan, event)
		case event.Type() == ebpf.PerfEventArray:
			go this.perfEventReader(errChan, event)
		default:
			errChan <- fmt.Errorf("%s\tNot support mapType:%s , mapinfo:%s", this.child.Name(), event.Type().String(), event.String())
		}
	}

	for {
		select {
		case err := <-errChan:
			return err
		}
	}
}

func (this *Module) perfEventReader(errChan chan error, em *ebpf.Map) {
	rd, err := perf.NewReader(em, os.Getpagesize()*64)
	if err != nil {
		errChan <- fmt.Errorf("creating %s reader dns: %s", em.String(), err)
		return
	}
	defer rd.Close()
	for {
		//判断ctx是不是结束
		select {
		case _ = <-this.ctx.Done():
			this.logger.Printf("%s\tperfEventReader received close signal from context.Done().", this.child.Name())
			return
		default:
		}

		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return
			}
			errChan <- fmt.Errorf("%s\treading from perf event reader: %s", this.child.Name(), err)
			return
		}

		if record.LostSamples != 0 {
			this.logger.Printf("%s\tperf event ring buffer full, dropped %d samples", this.child.Name(), record.LostSamples)
			continue
		}

		var event event_processor.IEventStruct
		event, err = this.child.Decode(em, record.RawSample)
		if err != nil {
			this.logger.Printf("%s\tthis.child.decode error:%v", this.child.Name(), err)
			continue
		}

		// 上报数据
		this.Dispatcher(event)
	}
}

func (this *Module) ringbufEventReader(errChan chan error, em *ebpf.Map) {
	rd, err := ringbuf.NewReader(em)
	if err != nil {
		errChan <- fmt.Errorf("%s\tcreating %s reader dns: %s", this.child.Name(), em.String(), err)
		return
	}
	defer rd.Close()
	for {
		//判断ctx是不是结束
		select {
		case _ = <-this.ctx.Done():
			this.logger.Printf("%s\tringbufEventReader received close signal from context.Done().", this.child.Name())
			return
		default:
		}

		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				this.logger.Printf("%s\tReceived signal, exiting..", this.child.Name())
				return
			}
			errChan <- fmt.Errorf("%s\treading from ringbuf reader: %s", this.child.Name(), err)
			return
		}

		var event event_processor.IEventStruct
		event, err = this.child.Decode(em, record.RawSample)
		if err != nil {
			this.logger.Printf("%s\tthis.child.decode error:%v", this.child.Name(), err)
			continue
		}

		// 上报数据
		this.Dispatcher(event)
	}
}

func (this *Module) Decode(em *ebpf.Map, b []byte) (event event_processor.IEventStruct, err error) {
	es, found := this.child.DecodeFun(em)
	if !found {
		err = fmt.Errorf("%s\tcan't found decode function :%s, address:%p", this.child.Name(), em.String(), em)
		return
	}

	te := es.Clone()
	err = te.Decode(b)
	if err != nil {
		return nil, err
	}
	return te, nil
}

// 写入数据，或者上传到远程数据库，写入到其他chan 等。
func (this *Module) Dispatcher(event event_processor.IEventStruct) {
	switch event.EventType() {
	case event_processor.EventTypeOutput:
		if this.conf.GetHex() {
			this.logger.Println(event.StringHex())
		} else {
			this.logger.Println(event.String())
		}
	case event_processor.EventTypeEventProcessor:
		this.processor.Write(event)
	case event_processor.EventTypeModuleData:
		// Save to cache
		this.child.Dispatcher(event)
	}
}

func (this *Module) Close() error {
	err := this.processor.Close()
	return err
}
