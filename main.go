package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cocoonlife/goalsa"
	"github.com/kidoman/embd"
	_ "github.com/kidoman/embd/host/all"
	gpio "github.com/stianeikeland/go-rpio"
	"layeh.com/gumble/gumble"
	"layeh.com/gumble/gumbleutil"
	_ "layeh.com/gumble/opus"
)

func mustHaveEnv(name string) string {
	val := os.Getenv(name)
	if val == "" {
		panic(fmt.Sprintf("environment variable %s must be populated", name))
	}
	return val
}

func mustAtoi(strval string) int {
	intval, err := strconv.Atoi(strval)
	if err != nil {
		panic(err)
	}
	return intval
}

func dialLoop(logPrefix string, addr string, config *gumble.Config) {
	disconnected := make(chan struct{})
	retryCount := 0

	config.Attach(gumbleutil.Listener{
		Connect: func(e *gumble.ConnectEvent) {
			retryCount = 0
			log.Printf("%s: connected", logPrefix)
		},

		Disconnect: func(e *gumble.DisconnectEvent) {
			disconnected <- struct{}{}
		},
	})

	for {
		_, err := gumble.Dial(addr, config)
		if err != nil {
			log.Printf("%s: error while connecting: attempt=%d err=%q", logPrefix, retryCount, err)
			retryCount++
			time.Sleep(time.Second)
			continue
		}

		<-disconnected
		log.Printf("%s: disconnected from server", logPrefix)
		time.Sleep(time.Second)
	}
}

type ChannelForcer struct {
	gumbleutil.Listener // default noop implementation

	client *gumble.Client

	desiredChannel string
	logPrefix      string
}

func (cf *ChannelForcer) ChangeChannel(newChannel string) {
	if cf.desiredChannel == newChannel {
		return
	}

	cf.desiredChannel = newChannel
	if cf.client != nil {
		cf.client.Do(cf.checkChannel)
	}
}

func (cf *ChannelForcer) checkChannel() {
	if cf.client == nil {
		return
	}

	if cf.client.Self.Channel.Name == cf.desiredChannel {
		log.Printf("%s: moved into channel: channel=%s", cf.logPrefix, cf.desiredChannel)
		return
	}

	channel := cf.client.Channels.Find(cf.desiredChannel)
	if channel == nil {
		log.Printf("%s: can not find channel: channel=%s", cf.logPrefix, cf.desiredChannel)
		return
	}

	cf.client.Self.Move(channel)
}

func (cf *ChannelForcer) OnConnect(e *gumble.ConnectEvent) {
	cf.client = e.Client
	cf.checkChannel()
}

func (cf *ChannelForcer) OnDisconnect(e *gumble.DisconnectEvent) {
	cf.client = nil
}

func (cf *ChannelForcer) OnChannelChange(e *gumble.ChannelChangeEvent) {
	// channel might have gotten created
	cf.checkChannel()
}

func (cf *ChannelForcer) OnUserChange(e *gumble.UserChangeEvent) {
	if e.User == cf.client.Self {
		cf.checkChannel()
	}
}

type TXStream struct {
	gumbleutil.Listener // default noop implementation

	client *gumble.Client

	// PTT is a channel for activating transmission. To start
	// transmitting, send a channel that is closed when
	// transmitting should stop
	PTT chan (<-chan struct{})
}

func (t *TXStream) talk(done <-chan struct{}) {
	if t.client == nil {
		log.Println("tx: ptt pressed but no open connection")
		return
	}

	deviceName := pollForDevice("CAPTURE")
	if deviceName == "" {
		log.Println("tx: unable to find audio capture device")
		return
	}
	log.Printf("tx: opening audio device: dev=%s", deviceName)

	periodFrames := t.client.Config.AudioFrameSize()
	device, err := alsa.NewCaptureDevice(deviceName, 1, alsa.FormatS16LE, gumble.AudioSampleRate, alsa.BufferParams{PeriodFrames: periodFrames})
	if err != nil {
		log.Printf("tx: error opening audio capture device: dev=%s err=%q", deviceName, err)
		return
	}
	defer device.Close()

	outgoing := t.client.AudioOutgoing()
	defer close(outgoing)

	buf := make([]int16, periodFrames)
	for {
		_, err := device.Read(buf)
		if err != nil && err != alsa.ErrOverrun {
			log.Printf("tx: error reading samples from device: dev=%s err=%q", deviceName, err)
			return
		}

		outgoing <- gumble.AudioBuffer(buf)

		select {
		case <-done:
			return
		default:
		}
	}
}

func (t *TXStream) Run() {
	for done := range t.PTT {
		t.talk(done)
	}
}

func (t *TXStream) OnConnect(e *gumble.ConnectEvent) {
	t.client = e.Client
}

func (t *TXStream) OnDisconnect(e *gumble.DisconnectEvent) {
	t.client = nil
}

type RXStream struct {
	audio chan<- []int16
}

func (r *RXStream) OnAudioStream(e *gumble.AudioStreamEvent) {
	go func() {
		log.Printf("rx: audio stream opened from: user=%s channel=%s", e.User.Name, e.User.Channel.Name)
		for packet := range e.C {
			r.audio <- packet.AudioBuffer
		}
	}()
}

type RXChannelSwitcher struct {
	Channel0, Channel1 string
	ChannelPin         int
	ChannelForcer      *ChannelForcer
}

func (rxcs *RXChannelSwitcher) updateChannel(pin embd.DigitalPin) {
	v, err := pin.Read()
	if err != nil {
		log.Printf("rx: error reading from gpio pin: pin=%d err=%q", rxcs.ChannelPin, err)
		return
	}

	if v == 0 {
		rxcs.ChannelForcer.ChangeChannel(rxcs.Channel0)
	} else {
		rxcs.ChannelForcer.ChangeChannel(rxcs.Channel1)
	}
}

func (rxcs *RXChannelSwitcher) Start() {
	gpiopin := gpio.Pin(rxcs.ChannelPin)
	gpiopin.Input()
	gpiopin.PullUp()

	embdpin, err := embd.NewDigitalPin(rxcs.ChannelPin)
	if err != nil {
		panic(err)
	}

	err = embdpin.ActiveLow(true)
	if err != nil {
		panic(err)
	}

	rxcs.updateChannel(embdpin)
	err = embdpin.Watch(embd.EdgeBoth, rxcs.updateChannel)
	if err != nil {
		panic(err)
	}
}

type TXPTTHandler struct {
	PTTPin   int
	PTTChan  chan<- <-chan struct{}
	openChan chan struct{}
}

func (ptt *TXPTTHandler) handlePTT(pin embd.DigitalPin) {
	v, err := pin.Read()
	if err != nil {
		log.Printf("tx: error reading from gpio pin: pin=%d err=%q", ptt.PTTPin, err)
	}

	if v == 0 {
		if ptt.openChan == nil {
			return
		}

		close(ptt.openChan)
		ptt.openChan = nil
	} else {
		if ptt.openChan != nil {
			return
		}

		ptt.openChan = make(chan struct{})
		ptt.PTTChan <- ptt.openChan
	}
}

func (ptt *TXPTTHandler) Start() {
	gpiopin := gpio.Pin(ptt.PTTPin)
	gpiopin.Input()
	gpiopin.PullUp()

	embdpin, err := embd.NewDigitalPin(ptt.PTTPin)
	if err != nil {
		panic(err)
	}

	err = embdpin.ActiveLow(true)
	if err != nil {
		panic(err)
	}

	err = embdpin.Watch(embd.EdgeBoth, ptt.handlePTT)
	if err != nil {
		panic(err)
	}
}

func init() {
	// this code seems super goroutine unsafe so just do it here
	err := gpio.Open()
	if err != nil {
		panic(err)
	}

	err = embd.InitGPIO()
	if err != nil {
		panic(err)
	}
}

func main() {
	server := mustHaveEnv("MUMBLE_SERVER")
	usernamePrefix := mustHaveEnv("MUMBLE_USERNAME_PREFIX")
	password := os.Getenv("MUMBLE_PASSWORD")

	pttPin := mustAtoi(mustHaveEnv("GPIO_PIN_PTT"))
	channelPin := mustAtoi(mustHaveEnv("GPIO_PIN_CHANNEL"))

	txChannel := mustHaveEnv("MUMBLE_TX_CHANNEL")
	rxChannel0 := mustHaveEnv("MUMBLE_RX_CHANNEL_0")
	rxChannel1 := mustHaveEnv("MUMBLE_RX_CHANNEL_1")

	host, port, err := net.SplitHostPort(server)
	if err != nil {
		host = server
		port = strconv.Itoa(gumble.DefaultPort)
	}

	ptt := make(chan (<-chan struct{}))

	txStream := &TXStream{PTT: ptt}

	go txStream.Run()

	txConfig := gumble.NewConfig()
	txConfig.Username = usernamePrefix + "-tx"
	txConfig.Password = password
	txConfig.Attach(gumbleutil.AutoBitrate)
	txConfig.Attach(&ChannelForcer{
		desiredChannel: txChannel,
		logPrefix:      "tx",
	})
	txConfig.Attach(txStream)

	txPTTHandler := &TXPTTHandler{
		PTTPin:  pttPin,
		PTTChan: ptt,
	}
	txPTTHandler.Start()

	go dialLoop("tx", net.JoinHostPort(host, port), txConfig)

	playbackChan := make(chan []int16)

	go PlayAudio(playbackChan)

	rxConfig := gumble.NewConfig()
	rxConfig.Username = usernamePrefix + "-rx"
	rxConfig.Password = password
	rxConfig.Attach(gumbleutil.AutoBitrate)
	rxChannelForcer := &ChannelForcer{
		logPrefix: "rx",
	}
	rxChannelSwitcher := &RXChannelSwitcher{
		Channel0:      rxChannel0,
		Channel1:      rxChannel1,
		ChannelPin:    channelPin,
		ChannelForcer: rxChannelForcer,
	}
	rxChannelSwitcher.Start()
	rxConfig.Attach(rxChannelForcer)
	rxConfig.AttachAudio(&RXStream{
		audio: playbackChan,
	})

	go dialLoop("rx", net.JoinHostPort(host, port), rxConfig)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
