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

type RXState struct {
	gumbleutil.Listener // default noop implementation

	client         *gumble.Client
	desiredChannel string
	outputDevice   string
	audio          chan<- []int16
}

func (r *RXState) ChangeChannel(newChannel string) {
	r.desiredChannel = newChannel
	if r.client != nil {
		r.client.Do(r.checkChannel)
	}
}

// checkChannel must be called with l held
func (r *RXState) checkChannel() {
	if r.client == nil {
		return
	}

	if r.client.Self.Channel.Name == r.desiredChannel {
		log.Printf("rx: moved into channel: channel=%s", r.desiredChannel)
		return
	}

	channel := r.client.Channels.Find(r.desiredChannel)
	if channel == nil {
		log.Printf("rx: can not find channel: channel=%s", r.desiredChannel)
		return
	}

	r.client.Self.Move(channel)
}

func (r *RXState) OnConnect(e *gumble.ConnectEvent) {
	r.client = e.Client
	r.checkChannel()
}

func (r *RXState) OnDisconnect(e *gumble.DisconnectEvent) {
	r.client = nil
	r.checkChannel()
}

func (r *RXState) OnChannelChange(e *gumble.ChannelChangeEvent) {
	// channel might have gotten created
	r.checkChannel()
}

func (r *RXState) OnUserChange(e *gumble.UserChangeEvent) {
	if e.User == r.client.Self {
		r.checkChannel()
	}
}

func (r *RXState) OnAudioStream(e *gumble.AudioStreamEvent) {
	go func() {
		log.Printf("rx: audio stream opened from: user=%s channel=%s", e.User.Name, e.User.Channel.Name)
		for packet := range e.C {
			if packet.Sender.Channel.Name == r.desiredChannel {
				r.audio <- packet.AudioBuffer
			}
		}
	}()
}

func main() {
	server := mustHaveEnv("MUMBLE_SERVER")
	usernamePrefix := mustHaveEnv("MUMBLE_USERNAME_PREFIX")
	password := os.Getenv("MUMBLE_PASSWORD")

	outputDevice := os.Getenv("RADIO_OUTPUT_DEVICE")
	if outputDevice == "" {
		outputDevice = "default"
	}
	inputDevice := os.Getenv("RADIO_INPUT_DEVICE")
	if inputDevice == "" {
		inputDevice = "default"
	}

	// TODO: replace this with GPIO infrastructure
	channel := mustHaveEnv("MUMBLE_CHANNEL")

	host, port, err := net.SplitHostPort(server)
	if err != nil {
		host = server
		port = strconv.Itoa(gumble.DefaultPort)
	}

	txConfig := gumble.NewConfig()
	txConfig.Username = usernamePrefix + "-tx"
	txConfig.Password = password
	txConfig.Attach(gumbleutil.AutoBitrate)

	go dialLoop("tx", net.JoinHostPort(host, port), txConfig)

	playbackChan := make(chan []int16)

	go PlayAudio(playbackChan)

	rxConfig := gumble.NewConfig()
	rxConfig.Username = usernamePrefix + "-rx"
	rxConfig.Password = password
	rxConfig.Attach(gumbleutil.AutoBitrate)
	rxState := &RXState{
		desiredChannel: channel,
		outputDevice:   outputDevice,
		audio:          playbackChan,
	}
	rxConfig.Attach(rxState)
	rxConfig.AttachAudio(rxState)

	go dialLoop("rx", net.JoinHostPort(host, port), rxConfig)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
