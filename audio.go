package main

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"regexp"
	"time"

	"layeh.com/gumble/gumble"

	"github.com/cocoonlife/goalsa"
	"github.com/jochenvg/go-udev"
)

var cardRegexp = regexp.MustCompile("^/proc/asound/card([0-9]+)/pcm.*/info")

func pollForDevice() string {
retry:
	pcms, _ := filepath.Glob("/proc/asound/card*/pcm*/info")

	for _, pcm := range pcms {
		info, err := ioutil.ReadFile(pcm)
		if err != nil {
			log.Printf("audio: error reading info for audio device: err=%q device=%s", err, pcm)
			time.Sleep(time.Second)
			goto retry
		}

		if bytes.Contains(info, []byte("stream: PLAYBACK")) &&
			bytes.Contains(info, []byte("id: USB Audio")) {
			card := cardRegexp.FindStringSubmatch(pcm)[1]
			return fmt.Sprintf("default:%s", card)
		}
	}

	return ""
}

func PlayAudio(audio <-chan []int16) {
	ctx := context.Background()

	u := udev.Udev{}
	m := u.NewMonitorFromNetlink("udev")
	err := m.FilterAddMatchSubsystem("sound")
	if err != nil {
		panic(err)
	}

	devs, err := m.DeviceChan(ctx)
	if err != nil {
		panic(err)
	}

	currentDeviceName := ""
	var currentDevice *alsa.PlaybackDevice
	defer func() {
		if currentDevice != nil {
			currentDevice.Close()
		}
	}()
	needScan := true

	for {
		if needScan {
			newDeviceName := pollForDevice()
			if newDeviceName == currentDeviceName {
				needScan = false
				continue
			}

			var newDevice *alsa.PlaybackDevice
			if newDeviceName != "" {
				log.Printf("audio: opening new audio device: dev=%s", newDeviceName)
				newDevice, err = alsa.NewPlaybackDevice(newDeviceName, 1, alsa.FormatS16LE, gumble.AudioSampleRate, alsa.BufferParams{})
				if err != nil {
					log.Printf("audio: error opening playback device: dev=%s err=%q", newDeviceName, err)
					time.Sleep(time.Second)
					continue
				}
			}

			if currentDevice != nil {
				log.Printf("audio: closing audio device: dev=%s", currentDeviceName)
				currentDevice.Close()
				log.Printf("audio: closed")
			}

			currentDevice = newDevice
			currentDeviceName = newDeviceName

			needScan = false
		}

		select {
		case <-devs:
			needScan = true
		case sample := <-audio:
			if currentDeviceName != "" {
				_, err := currentDevice.Write(sample)
				if err != nil {
					log.Printf("audio: error writing to device: dev=%s err=%q", currentDeviceName, err)
				}
			}
		}
	}
}
