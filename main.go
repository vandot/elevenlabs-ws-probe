// Command elevenlabs-ws-probe characterises the behaviour of ElevenLabs'
// streaming text-to-speech WebSockets: when the server closes an idle socket,
// what close code it sends, whether a keepalive holds it open, and how the
// single-stream and multi-stream endpoints differ.
//
// It is a deliberately minimal client. No agent framework, no SDK, one
// WebSocket library. The point is to isolate server behaviour from whatever a
// larger stack might be doing on top of it.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	host = "api.elevenlabs.io"
	// George, one of the standard voices. Voice Library voices (Rachel,
	// 21m00Tcm4TlvDq8ikWAM, among them) are not available to free accounts over
	// the API, and multi-stream reports that failure with no error at all.
	defaultVoice = "JBFqnCBsd6RMkjVDRZzb"
)

type config struct {
	apiKey    string
	voice     string
	model     string
	language  string
	mode      string
	scenario  string
	idleGrace time.Duration
	keepalive time.Duration
	maxWait   time.Duration
	timeout   int
	text      string
	outPath   string
}

// event is one line of the JSONL trace. Every event carries the elapsed time
// since the probe started, so a trace can be read as a timeline.
type event struct {
	ElapsedMS int64  `json:"elapsed_ms"`
	Kind      string `json:"kind"`
	Detail    string `json:"detail,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	CloseCode int    `json:"close_code,omitempty"`
	Scenario  string `json:"scenario"`
	Mode      string `json:"mode"`
	Model     string `json:"model"`
}

type recorder struct {
	mu    sync.Mutex
	start time.Time
	out   *os.File
	cfg   config

	// shuttingDown marks the point where the probe itself tears the socket
	// down, so the read error that follows is not reported as a server close.
	shuttingDown int32

	contextFinalMS int64 // when the server finalised the context, -1 if it never did
	socketClosedMS int64 // when the server closed the socket, -1 if it never did
}

func (r *recorder) log(kind, detail string, nbytes, closeCode int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ev := event{
		ElapsedMS: time.Since(r.start).Milliseconds(),
		Kind:      kind,
		Detail:    detail,
		Bytes:     nbytes,
		CloseCode: closeCode,
		Scenario:  r.cfg.scenario,
		Mode:      r.cfg.mode,
		Model:     r.cfg.model,
	}

	line := fmt.Sprintf("%7dms  %-10s %s", ev.ElapsedMS, kind, detail)
	if nbytes > 0 {
		line += fmt.Sprintf("  (%d bytes)", nbytes)
	}
	if closeCode != 0 {
		line += fmt.Sprintf("  [code %d]", closeCode)
	}
	fmt.Println(line)

	if r.out != nil {
		b, err := json.Marshal(ev)
		if err == nil {
			fmt.Fprintln(r.out, string(b))
		}
	}
}

func main() {
	cfg := config{}

	flag.StringVar(&cfg.apiKey, "api-key", os.Getenv("ELEVENLABS_API_KEY"), "API key (default: $ELEVENLABS_API_KEY)")
	flag.StringVar(&cfg.voice, "voice", defaultVoice, "voice id")
	flag.StringVar(&cfg.model, "model", "eleven_turbo_v2_5", "model id")
	flag.StringVar(&cfg.language, "language", "", "language code, e.g. tr (optional)")
	flag.StringVar(&cfg.mode, "mode", "single", "endpoint: single (stream-input) or multi (multi-stream-input)")
	flag.StringVar(&cfg.scenario, "scenario", "idle", "idle | keepalive | empty | speak | revive | newctx")
	flag.DurationVar(&cfg.keepalive, "keepalive", 15*time.Second, "keepalive interval (scenario=keepalive)")
	flag.DurationVar(&cfg.idleGrace, "idle-grace", 2*time.Second, "pause before the probe action (scenario=empty)")
	flag.DurationVar(&cfg.maxWait, "max", 5*time.Minute, "give up waiting after this long")
	flag.IntVar(&cfg.timeout, "inactivity-timeout", 0, "inactivity_timeout query param in seconds (0 = omit, server default 20)")
	flag.StringVar(&cfg.text, "text", "The quick brown fox jumps over the lazy dog. ", "text to synthesise (scenario=speak)")
	flag.StringVar(&cfg.outPath, "out", "", "append JSONL trace to this file")
	flag.Parse()

	if cfg.apiKey == "" {
		fmt.Fprintln(os.Stderr, "no API key: set ELEVENLABS_API_KEY or pass -api-key")
		os.Exit(2)
	}
	if cfg.mode != "single" && cfg.mode != "multi" {
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", cfg.mode)
		os.Exit(2)
	}

	rec := &recorder{start: time.Now(), cfg: cfg, contextFinalMS: -1, socketClosedMS: -1}
	if cfg.outPath != "" {
		f, err := os.OpenFile(cfg.outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", cfg.outPath, err)
			os.Exit(1)
		}
		defer f.Close()
		rec.out = f
	}

	if err := run(cfg, rec); err != nil {
		rec.log("error", err.Error(), 0, 0)
		os.Exit(1)
	}
}

func endpoint(cfg config) string {
	path := "/v1/text-to-speech/" + cfg.voice
	if cfg.mode == "multi" {
		path += "/multi-stream-input"
	} else {
		path += "/stream-input"
	}

	q := url.Values{}
	q.Set("model_id", cfg.model)
	if cfg.language != "" {
		q.Set("language_code", cfg.language)
	}
	if cfg.timeout > 0 {
		q.Set("inactivity_timeout", strconv.Itoa(cfg.timeout))
	}

	u := url.URL{Scheme: "wss", Host: host, Path: path, RawQuery: q.Encode()}
	return u.String()
}

func run(cfg config, rec *recorder) error {
	target := endpoint(cfg)
	rec.log("dial", target, 0, 0)

	hdr := http.Header{}
	hdr.Set("xi-api-key", cfg.apiKey)

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second

	conn, resp, err := dialer.Dial(target, hdr)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("handshake failed: %v (http %s)", err, resp.Status)
		}
		return fmt.Errorf("handshake failed: %v", err)
	}
	defer conn.Close()
	rec.log("open", "handshake complete", 0, 0)

	const ctxID = "probe-ctx-1"

	// Reader goroutine. It owns all reads; closed is signalled back here.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				if atomic.LoadInt32(&rec.shuttingDown) == 1 {
					// We closed the socket ourselves; this is not a server event.
					return
				}
				code := 0
				detail := err.Error()
				var ce *websocket.CloseError
				if errorsAs(err, &ce) {
					code = ce.Code
					detail = ce.Text
					if detail == "" {
						detail = "(no reason given)"
					}
				}
				rec.mu.Lock()
				rec.socketClosedMS = time.Since(rec.start).Milliseconds()
				rec.mu.Unlock()
				rec.log("close", detail, 0, code)
				return
			}
			describeServerMessage(rec, data)
		}
	}()

	send := func(label string, payload map[string]interface{}) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
			return err
		}
		rec.log("send", label, len(b), 0)
		return nil
	}

	// Initialisation message. Single-stream carries the key in the payload;
	// multi-stream authenticates by header and needs a context id.
	init := map[string]interface{}{
		"text": " ",
		"voice_settings": map[string]interface{}{
			"stability":         0.5,
			"similarity_boost":  0.8,
			"use_speaker_boost": true,
		},
		"generation_config": map[string]interface{}{
			"chunk_length_schedule": []int{120, 160, 250, 290},
		},
	}
	if cfg.mode == "multi" {
		init["context_id"] = ctxID
	} else {
		init["xi_api_key"] = cfg.apiKey
	}
	if err := send("init (BOS)", init); err != nil {
		return err
	}

	withCtx := func(m map[string]interface{}) map[string]interface{} {
		if cfg.mode == "multi" {
			m["context_id"] = ctxID
		}
		return m
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	deadline := time.After(cfg.maxWait)

	switch cfg.scenario {
	case "idle":
		// Send nothing further. Measure when, and how, the server hangs up.
		rec.log("waiting", "idle after init, nothing further will be sent", 0, 0)

	case "keepalive":
		// A single space is the documented keepalive for stream-input.
		rec.log("waiting", "sending a space every "+cfg.keepalive.String(), 0, 0)
		go func() {
			t := time.NewTicker(cfg.keepalive)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := send("keepalive space", withCtx(map[string]interface{}{"text": " "})); err != nil {
						return
					}
				case <-closed:
					return
				}
			}
		}()

	case "empty":
		// The docs disagree: on stream-input an empty string closes the
		// socket; on multi-stream-input the same payload with a context id is
		// listed as keep-alive. This measures which behaviour is real.
		go func() {
			select {
			case <-time.After(cfg.idleGrace):
			case <-closed:
				return
			}
			_ = send(`empty text {"text":""}`, withCtx(map[string]interface{}{"text": ""}))
			rec.log("waiting", "sent empty text; watching for close", 0, 0)
		}()

	case "revive":
		// Multi-stream finalises an idle context but leaves the socket open.
		// This writes to that context afterwards: does the server error,
		// silently drop the text, or start a fresh generation?
		rec.log("waiting", "idle until the context finalises, then writing to it again", 0, 0)
		go func() {
			deadline := time.After(cfg.maxWait)
			for {
				rec.mu.Lock()
				done := rec.contextFinalMS >= 0
				rec.mu.Unlock()
				if done {
					break
				}
				select {
				case <-time.After(250 * time.Millisecond):
				case <-closed:
					return
				case <-deadline:
					return
				}
			}
			time.Sleep(cfg.idleGrace)
			rec.log("probe", "context is finalised; writing to it anyway", 0, 0)
			if err := send("text after final", withCtx(map[string]interface{}{"text": cfg.text, "flush": true})); err != nil {
				return
			}
			rec.log("waiting", "watching for audio, an error, or nothing at all", 0, 0)
		}()

	case "newctx":
		// Control for the revive scenario: once the first context has been
		// finalised, open a second context on the same socket. If that one
		// produces audio, the socket is fine and only the dead context is
		// unusable.
		rec.log("waiting", "idle until the context finalises, then opening a second context", 0, 0)
		go func() {
			deadline := time.After(cfg.maxWait)
			for {
				rec.mu.Lock()
				done := rec.contextFinalMS >= 0
				rec.mu.Unlock()
				if done {
					break
				}
				select {
				case <-time.After(250 * time.Millisecond):
				case <-closed:
					return
				case <-deadline:
					return
				}
			}
			time.Sleep(cfg.idleGrace)
			const second = "probe-ctx-2"
			rec.log("probe", "first context finalised; opening "+second, 0, 0)
			if err := send("init second context", map[string]interface{}{
				"text":       " ",
				"context_id": second,
				"voice_settings": map[string]interface{}{
					"stability": 0.5, "similarity_boost": 0.8, "use_speaker_boost": true,
				},
			}); err != nil {
				return
			}
			if err := send("text on second context", map[string]interface{}{
				"text": cfg.text, "context_id": second, "flush": true,
			}); err != nil {
				return
			}
			rec.log("waiting", "audio here means the socket is healthy and only the dead context was unusable", 0, 0)
		}()

	case "speak":
		if err := send("text", withCtx(map[string]interface{}{"text": cfg.text, "flush": true})); err != nil {
			return err
		}
		rec.log("waiting", "text sent, then idle until the server closes", 0, 0)

	default:
		return fmt.Errorf("unknown scenario %q", cfg.scenario)
	}

	var outcome string
	select {
	case <-closed:
		outcome = "server closed the socket"
	case <-deadline:
		outcome = "socket still open at -max=" + cfg.maxWait.String()
	case <-stop:
		outcome = "interrupted"
	}

	atomic.StoreInt32(&rec.shuttingDown, 1)
	rec.log("done", outcome, 0, 0)
	rec.summarise()
	return nil
}

// summarise prints the two facts the probe exists to establish: whether the
// context was finalised, and whether the socket itself was ever closed.
func (r *recorder) summarise() {
	r.mu.Lock()
	ctxMS, sockMS := r.contextFinalMS, r.socketClosedMS
	r.mu.Unlock()

	fmt.Println()
	fmt.Printf("  endpoint       %s-stream, model %s\n", r.cfg.mode, r.cfg.model)
	if ctxMS >= 0 {
		fmt.Printf("  context final  %dms\n", ctxMS)
	} else {
		fmt.Printf("  context final  never\n")
	}
	if sockMS >= 0 {
		fmt.Printf("  socket closed  %dms (by server)\n", sockMS)
	} else {
		fmt.Printf("  socket closed  never — still open when the probe stopped\n")
	}
	if ctxMS >= 0 && sockMS < 0 {
		fmt.Printf("  note           the inactivity timeout ended the context but left the socket open\n")
	}
	fmt.Println()
}

// describeServerMessage turns one server frame into a readable event without
// dumping base64 audio into the trace.
func describeServerMessage(rec *recorder, data []byte) {
	var msg struct {
		Audio     *string          `json:"audio"`
		IsFinal   *bool            `json:"isFinal"`
		ContextID string           `json:"contextId"`
		Error     *json.RawMessage `json:"error"`
		Message   string           `json:"message"`
		Code      string           `json:"code"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		rec.log("recv", "unparsed frame: "+truncate(string(data), 120), len(data), 0)
		return
	}

	switch {
	case msg.Audio != nil && *msg.Audio != "":
		raw, err := base64.StdEncoding.DecodeString(*msg.Audio)
		n := len(raw)
		if err != nil {
			n = len(*msg.Audio)
		}
		rec.log("recv", "audio chunk "+ctxSuffix(msg.ContextID), n, 0)
	case msg.Audio != nil:
		rec.log("recv", "empty audio field "+ctxSuffix(msg.ContextID), 0, 0)
	case msg.IsFinal != nil && *msg.IsFinal:
		rec.mu.Lock()
		if rec.contextFinalMS < 0 {
			rec.contextFinalMS = time.Since(rec.start).Milliseconds()
		}
		rec.mu.Unlock()
		rec.log("recv", "isFinal "+ctxSuffix(msg.ContextID), 0, 0)
	case msg.Error != nil || msg.Message != "":
		detail := msg.Message
		if msg.Code != "" {
			detail = msg.Code + ": " + detail
		}
		rec.log("recv", "server error: "+truncate(detail, 200), len(data), 0)
	default:
		rec.log("recv", truncate(string(data), 120), len(data), 0)
	}
}

func ctxSuffix(id string) string {
	if id == "" {
		return ""
	}
	return "(context " + id + ")"
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// errorsAs is a tiny stand-in so the close-code extraction reads clearly.
func errorsAs(err error, target **websocket.CloseError) bool {
	ce, ok := err.(*websocket.CloseError)
	if ok {
		*target = ce
	}
	return ok
}
