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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/opus"
	"github.com/pion/mediadevices/pkg/codec/vpx"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/webrtc/v4"
)

const (
	commandStart    = "start"
	commandStop     = "stop"
	commandSetTrack = "setTrack"

	trackVideo  = "video"
	trackAudio  = "audio"
	trackScreen = "screen"
)

var (
	httpClient    = &http.Client{Timeout: 10 * time.Second}
	codecInitOnce sync.Once
	codecSelector *mediadevices.CodecSelector
	webrtcAPI     *webrtc.API
	codecInitErr  error
)

type commandRequest struct {
	Command string `json:"command"`
	Track   string `json:"track,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type trackFactory func() (mediadevices.Track, error)

type trackControl struct {
	name    string
	sender  *webrtc.RTPSender
	factory trackFactory

	mu    sync.Mutex
	track mediadevices.Track
}

func (tc *trackControl) setEnabled(enabled bool) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if enabled {
		if tc.track != nil {
			return nil
		}

		track, err := tc.factory()
		if err != nil {
			return err
		}

		if err := tc.sender.ReplaceTrack(track); err != nil {
			_ = track.Close()
			return err
		}

		tc.track = track
		return nil
	}

	if tc.track == nil {
		return nil
	}

	if err := tc.sender.ReplaceTrack(nil); err != nil {
		return err
	}

	if err := tc.track.Close(); err != nil {
		return err
	}

	tc.track = nil
	return nil
}

func (tc *trackControl) shutdown() {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.track == nil {
		return
	}

	if err := tc.sender.ReplaceTrack(nil); err != nil {
		log.Printf("failed to detach %s track: %v", tc.name, err)
	}

	if err := tc.track.Close(); err != nil {
		log.Printf("failed to close %s track: %v", tc.name, err)
	}

	tc.track = nil
}

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

	var streaming bool
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

		switch cmd.Command {
		case "", "noop":
			continue
		case commandStart:
			if streaming {
				log.Println("start command ignored: already streaming")
				continue
			}
			log.Println("received start command")
			streaming = true
			if err := startStreaming(ctx, *serverURL, *senderID, *videoDevice, *audioDevice, *screenDevice); err != nil {
				log.Printf("streaming error: %v", err)
			}
			streaming = false
		case commandStop:
			if streaming {
				log.Println("stop command queued during active session")
			} else {
				log.Println("received stop command but no active session")
			}
		case commandSetTrack:
			normalizedTrack := strings.ToLower(strings.TrimSpace(cmd.Track))
			if !streaming {
				log.Printf("track control command ignored: no active session (track=%s)", normalizedTrack)
			} else {
				log.Printf("track control command pending for active session: track=%s enabled=%v", normalizedTrack, valueOrUnknown(cmd.Enabled))
			}
		default:
			log.Printf("unknown command %q", cmd.Command)
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

func waitForCommand(ctx context.Context, baseURL, id string) (commandRequest, error) {
	var empty commandRequest
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/sender/%s/command", baseURL, id), nil)
	if err != nil {
		return empty, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return empty, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return empty, fmt.Errorf("command request failed: %s", string(body))
	}
	var payload commandRequest
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return empty, err
	}
	return payload, nil
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

	controls, err := attachTracks(pc, selector, videoDevice, audioDevice, screenDevice)
	if err != nil {
		return err
	}
	defer func() {
		for _, ctrl := range controls {
			ctrl.shutdown()
		}
	}()

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

	log.Println("streaming started, waiting for commands")

	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			cmd, err := waitForCommand(commandCtx, baseURL, id)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				log.Printf("command error during streaming: %v", err)
				time.Sleep(2 * time.Second)
				continue
			}

			switch cmd.Command {
			case "", "noop":
				continue
			case commandStop:
				log.Println("received stop command")
				return
			case commandSetTrack:
				normalizedTrack := strings.ToLower(strings.TrimSpace(cmd.Track))
				if normalizedTrack == "" || cmd.Enabled == nil {
					log.Printf("invalid track control command: track=%s enabled=%v", normalizedTrack, valueOrUnknown(cmd.Enabled))
					continue
				}
				ctrl, ok := controls[normalizedTrack]
				if !ok {
					log.Printf("unknown track %q in control command", normalizedTrack)
					continue
				}
				if err := ctrl.setEnabled(*cmd.Enabled); err != nil {
					log.Printf("failed to update track %s: %v", ctrl.name, err)
					continue
				}
				log.Printf("track %s enabled=%t", ctrl.name, *cmd.Enabled)
			case commandStart:
				log.Println("start command ignored: session already active")
			default:
				log.Printf("unknown command during streaming: %q", cmd.Command)
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

func attachTracks(pc *webrtc.PeerConnection, selector *mediadevices.CodecSelector, videoDevice, audioDevice, screenDevice string) (map[string]*trackControl, error) {
	controls := make(map[string]*trackControl)

	cleanup := func() {
		for _, ctrl := range controls {
			ctrl.shutdown()
		}
	}

	attach := func(name string, factory trackFactory) error {
		track, err := factory()
		if err != nil {
			return err
		}

		sender, err := pc.AddTrack(track)
		if err != nil {
			_ = track.Close()
			return err
		}

		controls[name] = &trackControl{
			name:    name,
			sender:  sender,
			factory: factory,
			track:   track,
		}
		return nil
	}

	if err := attach(trackVideo, createVideoFactory(selector, videoDevice)); err != nil {
		cleanup()
		return nil, fmt.Errorf("init video track: %w", err)
	}

	if err := attach(trackAudio, createAudioFactory(selector, audioDevice)); err != nil {
		cleanup()
		return nil, fmt.Errorf("init audio track: %w", err)
	}

	if err := attach(trackScreen, createScreenFactory(selector, screenDevice)); err != nil {
		cleanup()
		return nil, fmt.Errorf("init screen track: %w", err)
	}

	return controls, nil
}

func createVideoFactory(selector *mediadevices.CodecSelector, deviceID string) trackFactory {
	return func() (mediadevices.Track, error) {
		constraints := mediadevices.MediaStreamConstraints{
			Codec: selector,
		}
		constraints.Video = func(v *mediadevices.MediaTrackConstraints) {
			if deviceID != "" {
				v.DeviceID = prop.String(deviceID)
			}
			v.Width = prop.Int(1280)
			v.Height = prop.Int(720)
			v.FrameRate = prop.Float(30)
		}

		stream, err := mediadevices.GetUserMedia(constraints)
		if err != nil {
			return nil, fmt.Errorf("get user media: %w", err)
		}

		tracks := stream.GetVideoTracks()
		if len(tracks) == 0 {
			return nil, errors.New("no video tracks available")
		}

		return tracks[0], nil
	}
}

func createAudioFactory(selector *mediadevices.CodecSelector, deviceID string) trackFactory {
	return func() (mediadevices.Track, error) {
		constraints := mediadevices.MediaStreamConstraints{
			Codec: selector,
		}
		constraints.Audio = func(a *mediadevices.MediaTrackConstraints) {
			if deviceID != "" {
				a.DeviceID = prop.String(deviceID)
			}
		}

		stream, err := mediadevices.GetUserMedia(constraints)
		if err != nil {
			return nil, fmt.Errorf("get user media: %w", err)
		}

		tracks := stream.GetAudioTracks()
		if len(tracks) == 0 {
			return nil, errors.New("no audio tracks available")
		}

		return tracks[0], nil
	}
}

func createScreenFactory(selector *mediadevices.CodecSelector, deviceID string) trackFactory {
	return func() (mediadevices.Track, error) {
		constraints := mediadevices.MediaStreamConstraints{
			Codec: selector,
		}
		constraints.Video = func(v *mediadevices.MediaTrackConstraints) {
			if deviceID != "" {
				v.DeviceID = prop.String(deviceID)
			}
			v.FrameRate = prop.Float(15)
		}

		stream, err := mediadevices.GetDisplayMedia(constraints)
		if err != nil {
			return nil, fmt.Errorf("get display media: %w", err)
		}

		tracks := stream.GetVideoTracks()
		if len(tracks) == 0 {
			return nil, errors.New("no screen tracks available")
		}

		return tracks[0], nil
	}
}

func valueOrUnknown(v *bool) interface{} {
	if v == nil {
		return "unknown"
	}
	return *v
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
