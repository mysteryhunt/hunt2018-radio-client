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

type ChannelForcer struct {
	gumbleutil.Listener // default noop implementation

	client *gumble.Client

	desiredChannel string
	logPrefix      string
}

func (cf *ChannelForcer) ChangeChannel(newChannel string) {
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

func main() {
	server := mustHaveEnv("MUMBLE_SERVER")
	usernamePrefix := mustHaveEnv("MUMBLE_USERNAME_PREFIX")
	password := os.Getenv("MUMBLE_PASSWORD")

	txChannel := mustHaveEnv("MUMBLE_TX_CHANNEL")
	// TODO: replace this with GPIO infrastructure
	rxChannel := mustHaveEnv("MUMBLE_RX_CHANNEL")

	host, port, err := net.SplitHostPort(server)
	if err != nil {
		host = server
		port = strconv.Itoa(gumble.DefaultPort)
	}

	txConfig := gumble.NewConfig()
	txConfig.Username = usernamePrefix + "-tx"
	txConfig.Password = password
	txConfig.Attach(gumbleutil.AutoBitrate)
	txConfig.Attach(&ChannelForcer{
		desiredChannel: txChannel,
		logPrefix:      "tx",
	})

	go dialLoop("tx", net.JoinHostPort(host, port), txConfig)

	playbackChan := make(chan []int16)

	go PlayAudio(playbackChan)

	rxConfig := gumble.NewConfig()
	rxConfig.Username = usernamePrefix + "-rx"
	rxConfig.Password = password
	rxConfig.Attach(gumbleutil.AutoBitrate)
	rxConfig.Attach(&ChannelForcer{
		desiredChannel: rxChannel,
		logPrefix:      "rx",
	})
	rxConfig.AttachAudio(&RXStream{
		audio: playbackChan,
	})

	go dialLoop("rx", net.JoinHostPort(host, port), rxConfig)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}
