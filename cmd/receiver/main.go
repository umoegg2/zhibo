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
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

func main() {
	serverURL := flag.String("server", "http://localhost:8080", "control server base URL")
	senderID := flag.String("sender", "default-sender", "target sender identifier")
	action := flag.String("action", "start", "control action: start or stop")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		cancel()
	}()

	switch *action {
	case "start":
		if err := startReceiving(ctx, *serverURL, *senderID); err != nil {
			log.Fatalf("receiver error: %v", err)
		}
	case "stop":
		if err := sendCommand(*serverURL, *senderID, "stop"); err != nil {
			log.Fatalf("failed to send stop command: %v", err)
		}
		log.Println("stop command sent")
	default:
		log.Fatalf("unknown action %q", *action)
	}
}

func startReceiving(ctx context.Context, baseURL, senderID string) error {
	if err := sendCommand(baseURL, senderID, "start"); err != nil {
		return err
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return fmt.Errorf("register default codecs: %w", err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	defer pc.Close()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("connection state: %s", state)
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("remote track started: kind=%s id=%s", track.Kind(), track.ID())
		go func() {
			buf := make([]byte, 1400)
			for {
				_, _, err := track.Read(buf)
				if err != nil {
					if !errors.Is(err, io.EOF) {
						log.Printf("track read error: %v", err)
					}
					return
				}
			}
		}()
	})

	offer, err := waitForOffer(ctx, baseURL, senderID)
	if err != nil {
		return err
	}
	if err := pc.SetRemoteDescription(*offer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	<-webrtc.GatheringCompletePromise(pc)

	if err := postSignal(baseURL, senderID, "answer", pc.LocalDescription()); err != nil {
		return err
	}

	<-ctx.Done()
	return nil
}

func sendCommand(baseURL, id, command string) error {
	payload := map[string]string{"command": command}
	buf, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("%s/receiver/%s/command", baseURL, id), "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("command failed: %s", string(body))
	}
	return nil
}

func waitForOffer(ctx context.Context, baseURL, id string) (*webrtc.SessionDescription, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/signal/%s/offer", baseURL, id), nil)
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
			return nil, fmt.Errorf("offer request failed: %s", string(body))
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

func postSignal(baseURL, id, action string, desc *webrtc.SessionDescription) error {
	payload := map[string]string{
		"sdp":  desc.SDP,
		"type": string(desc.Type),
	}
	buf, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("%s/signal/%s/%s", baseURL, id, action), "application/json", bytes.NewReader(buf))
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
