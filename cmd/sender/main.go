package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/codec/vpx"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/webrtc/v4"

	_ "github.com/pion/mediadevices/pkg/driver/audiotest"
	_ "github.com/pion/mediadevices/pkg/driver/screen"
	_ "github.com/pion/mediadevices/pkg/driver/videotest"
)

var (
	httpClient    = &http.Client{Timeout: 10 * time.Second}
	codecInitOnce sync.Once
	codecSelector *mediadevices.CodecSelector
	webrtcAPI     *webrtc.API
	codecInitErr  error
)

func main() {
	serverURL := flag.String("server", "http://localhost:8080", "control server base URL")
	senderID := flag.String("id", "default-sender", "unique sender identifier")
	videoDevice := flag.String("video", "", "video device ID (empty for default)")
	audioDevice := flag.String("audio", "", "audio device ID (empty for default)")
	screenDevice := flag.String("screen", "", "screen capture device ID (empty for default)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		cancel()
	}()

	if err := registerSender(*serverURL, *senderID); err != nil {
		log.Fatalf("failed to register sender: %v", err)
	}

	log.Printf("sender %s registered, waiting for commands", *senderID)

	for {
		select {
		case <-ctx.Done():
			log.Println("shutdown requested")
			return
		default:
		}

		cmd, err := waitForCommand(ctx, *serverURL, *senderID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("command error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		switch cmd {
		case "noop":
			continue
		case "start":
			log.Println("received start command")
			if err := startStreaming(ctx, *serverURL, *senderID, *videoDevice, *audioDevice, *screenDevice); err != nil {
				log.Printf("streaming error: %v", err)
			}
		case "stop":
			log.Println("received stop command but no active session")
		default:
			log.Printf("unknown command %q", cmd)
		}
	}
}

func registerSender(baseURL, id string) error {
	payload := map[string]string{"id": id}
	buf, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("%s/sender/register", baseURL), "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register failed: %s", string(body))
	}
	return nil
}

func waitForCommand(ctx context.Context, baseURL, id string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/sender/%s/command", baseURL, id), nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("command request failed: %s", string(body))
	}
	var payload struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.Command, nil
}

func startStreaming(ctx context.Context, baseURL, id, videoDevice, audioDevice, screenDevice string) error {
	api, selector, err := ensureAPI()
	if err != nil {
		return err
	}

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	defer pc.Close()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("peer connection state: %s", state)
	})

	streams, err := buildMediaStreams(selector, videoDevice, audioDevice, screenDevice)
	if err != nil {
		return err
	}
	defer func() {
		for _, track := range streams {
			if err := track.Close(); err != nil {
				log.Printf("failed to close track %s: %v", track.ID(), err)
			}
		}
	}()

	for _, track := range streams {
		if _, err := pc.AddTrack(track); err != nil {
			log.Printf("failed to add track %s: %v", track.ID(), err)
		}
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	<-webrtc.GatheringCompletePromise(pc)

	if err := postSignal(baseURL, id, "offer", pc.LocalDescription()); err != nil {
		return err
	}

	answer, err := waitForAnswer(ctx, baseURL, id)
	if err != nil {
		return err
	}
	if err := pc.SetRemoteDescription(*answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	log.Println("streaming started, waiting for stop command")

	stopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for {
			cmd, err := waitForCommand(stopCtx, baseURL, id)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					close(done)
					return
				}
				log.Printf("command error during streaming: %v", err)
				time.Sleep(2 * time.Second)
				continue
			}
			if cmd == "stop" {
				log.Println("received stop command")
				close(done)
				return
			}
		}
	}()

	<-done
	return nil
}

func ensureAPI() (*webrtc.API, *mediadevices.CodecSelector, error) {
	codecInitOnce.Do(func() {
		vp8Params, err := vpx.NewVP8Params()
		if err != nil {
			codecInitErr = fmt.Errorf("create vp8 params: %w", err)
			return
		}
		vp8Params.BitRate = 1_000_000

		opusParams, err := opus.NewParams()
		if err != nil {
			codecInitErr = fmt.Errorf("create opus params: %w", err)
			return
		}
		opusParams.BitRate = 64000

		codecSelector = mediadevices.NewCodecSelector(
			mediadevices.WithVideoEncoders(&vp8Params),
			mediadevices.WithAudioEncoders(&opusParams),
		)

		mediaEngine := &webrtc.MediaEngine{}
		codecSelector.Populate(mediaEngine)

		settingEngine := webrtc.SettingEngine{}

		webrtcAPI = webrtc.NewAPI(
			webrtc.WithMediaEngine(mediaEngine),
			webrtc.WithSettingEngine(settingEngine),
		)
	})
	return webrtcAPI, codecSelector, codecInitErr
}

func buildMediaStreams(selector *mediadevices.CodecSelector, videoDevice, audioDevice, screenDevice string) ([]mediadevices.Track, error) {
	var tracks []mediadevices.Track

	userConstraints := mediadevices.MediaStreamConstraints{
		Codec: selector,
	}
	userConstraints.Video = func(v *mediadevices.MediaTrackConstraints) {
		if videoDevice != "" {
			v.DeviceID = prop.String(videoDevice)
		}
		v.Width = prop.Int(1280)
		v.Height = prop.Int(720)
		v.FrameRate = prop.Float(30)
	}
	userConstraints.Audio = func(a *mediadevices.MediaTrackConstraints) {
		if audioDevice != "" {
			a.DeviceID = prop.String(audioDevice)
		}
	}

	stream, err := mediadevices.GetUserMedia(userConstraints)
	if err != nil {
		return nil, fmt.Errorf("get user media: %w", err)
	}
	tracks = append(tracks, stream.GetTracks()...)

	displayConstraints := mediadevices.MediaStreamConstraints{
		Codec: selector,
	}
	displayConstraints.Video = func(v *mediadevices.MediaTrackConstraints) {
		if screenDevice != "" {
			v.DeviceID = prop.String(screenDevice)
		}
		v.FrameRate = prop.Float(15)
	}

	displayStream, err := mediadevices.GetDisplayMedia(displayConstraints)
	if err != nil {
		log.Printf("screen capture unavailable: %v", err)
	} else {
		tracks = append(tracks, displayStream.GetTracks()...)
	}

	return tracks, nil
}

func postSignal(baseURL, id, action string, desc *webrtc.SessionDescription) error {
	payload := map[string]string{
		"sdp":  desc.SDP,
		"type": string(desc.Type),
	}
	buf, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("%s/signal/%s/%s", baseURL, path.Clean(id), action)
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("signal %s failed: %s", action, string(body))
	}
	return nil
}

func waitForAnswer(ctx context.Context, baseURL, id string) (*webrtc.SessionDescription, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/signal/%s/answer", baseURL, id), nil)
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			time.Sleep(2 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("answer request failed: %s", string(body))
		}
		var payload struct {
			SDP  string `json:"sdp"`
			Type string `json:"type"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		return &webrtc.SessionDescription{Type: parseSDPType(payload.Type), SDP: payload.SDP}, nil
	}
}

func parseSDPType(t string) webrtc.SDPType {
	switch t {
	case webrtc.SDPTypeOffer.String():
		return webrtc.SDPTypeOffer
	case webrtc.SDPTypePranswer.String():
		return webrtc.SDPTypePranswer
	case webrtc.SDPTypeRollback.String():
		return webrtc.SDPTypeRollback
	default:
		return webrtc.SDPTypeAnswer
	}
}
