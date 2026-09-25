package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"panelpc/internal/audio"
	"panelpc/internal/config"
	"panelpc/internal/desktopapps"
	"panelpc/internal/device"
	"panelpc/internal/obsws"
	"panelpc/internal/shellcmd"
	"panelpc/internal/vumeter"
)

type Status struct {
	Values    [4]int     `json:"values"`
	LastEvent string     `json:"lastEvent,omitempty"`
	LastError string     `json:"lastError,omitempty"`
	VULevel   float64    `json:"vuLevel"`
	VUError   string     `json:"vuError,omitempty"`
	Spectrum  [4]float64 `json:"spectrum"`
}

type Engine struct {
	device *device.Manager
	audio  *audio.Controller
	obs    *obsws.Client
	vu     *vumeter.Meter

	configMu      sync.RWMutex
	config        config.Config
	statusMu      sync.RWMutex
	status        Status
	work          chan job
	switchProfile func(string) error
	lastAppLaunch map[string]time.Time
}

type job struct {
	knob  int
	event device.Event
	cfg   config.Knob
}

func New(dev *device.Manager, aud *audio.Controller, cfg config.Config) *Engine {
	e := &Engine{
		device: dev,
		audio:  aud,
		obs:    obsws.New(cfg.OBS.URL, cfg.OBS.Password),
		config: cfg,
		// Only one action may wait behind the currently running one. Subsequent
		// turns remain coalesced per knob in Run.
		work:          make(chan job, 1),
		lastAppLaunch: make(map[string]time.Time),
	}
	e.vu = vumeter.New(aud.VUCaptureArgs, e.onVUFrame, e.onVUError)
	e.configureLighting(cfg)
	return e
}

func (e *Engine) Configure(cfg config.Config) {
	cfg.Normalize()
	e.configMu.Lock()
	e.config = cfg
	e.configMu.Unlock()
	e.obs.Configure(cfg.OBS.URL, cfg.OBS.Password)
	e.configureLighting(cfg)
}

func (e *Engine) SetProfileSwitcher(switcher func(string) error) {
	e.configMu.Lock()
	e.switchProfile = switcher
	e.configMu.Unlock()
}

func (e *Engine) Status() Status {
	e.statusMu.RLock()
	defer e.statusMu.RUnlock()
	return e.status
}

func (e *Engine) Run(ctx context.Context) {
	go e.worker(ctx)
	go e.vu.Run(ctx)
	turns := newTurnScheduler(turnInterval)
	defer turns.stop()
	for {
		select {
		case <-ctx.Done():
			e.obs.Close()
			return
		case event := <-e.device.Events():
			e.record(event)
			if event.Initial {
				continue
			}
			cfg := e.knobConfig(event.Knob)
			j := job{knob: event.Knob, event: event, cfg: cfg}
			if event.Kind == "turn" {
				turns.add(j, e.work, time.Now())
			} else if event.Value == 1 {
				select {
				case e.work <- j:
				default:
					e.setError("action queue full; click dropped")
				}
			}
		case <-turns.C():
			turns.tick(e.work, time.Now())
		}
	}
}

const turnInterval = 50 * time.Millisecond

// turnScheduler coalesces knob turns: it dispatches at most once per interval
// and keeps only the latest absolute value per knob in between. Its timer runs
// only while turns are arriving, so an idle panel causes no wakeups.
type turnScheduler struct {
	interval  time.Duration
	pending   map[int]job
	nextShell [4]time.Time
	timer     *time.Timer
}

func newTurnScheduler(interval time.Duration) *turnScheduler {
	return &turnScheduler{interval: interval, pending: make(map[int]job)}
}

// C is nil while idle, which disables its select case.
func (s *turnScheduler) C() <-chan time.Time {
	if s.timer == nil {
		return nil
	}
	return s.timer.C
}

// add dispatches immediately when no interval is running; otherwise the turn
// waits for the next tick.
func (s *turnScheduler) add(j job, work chan<- job, now time.Time) {
	s.pending[j.knob] = j
	if s.timer == nil {
		s.flush(work, now)
	}
}

func (s *turnScheduler) tick(work chan<- job, now time.Time) {
	s.timer = nil
	if len(s.pending) > 0 {
		s.flush(work, now)
	}
}

func (s *turnScheduler) flush(work chan<- job, now time.Time) {
	for knob, j := range s.pending {
		if j.cfg.Turn.Kind == "shell" && now.Before(s.nextShell[knob]) {
			continue
		}
		select {
		case work <- j:
			delete(s.pending, knob)
			if j.cfg.Turn.Kind == "shell" {
				s.nextShell[knob] = now.Add(time.Duration(j.cfg.Turn.RateMS) * time.Millisecond)
			}
		default:
			// Retain only the latest absolute value for each knob.
		}
	}
	s.timer = time.NewTimer(s.interval)
}

func (s *turnScheduler) stop() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

func (e *Engine) configureLighting(cfg config.Config) {
	if cfg.Lighting.Mode == "vu" || cfg.Lighting.Mode == "spectrum" {
		vu := cfg.Lighting.VU
		e.vu.Set(vumeter.Config{
			Enabled:    true,
			SourceKind: vu.SourceKind,
			Target:     vu.Target,
			MinDB:      vu.MinDB,
			MaxDB:      vu.MaxDB,
			FPS:        vu.FPS,
		})
		e.device.SetLighting(vuLighting(vu, 0))
		return
	}
	e.vu.Set(vumeter.Config{})
	e.device.SetLighting(deviceLighting(cfg))
	e.statusMu.Lock()
	e.status.VULevel = 0
	e.status.VUError = ""
	e.status.Spectrum = [4]float64{}
	e.statusMu.Unlock()
}

func (e *Engine) onVUFrame(frame vumeter.Frame) {
	e.configMu.RLock()
	lighting := e.config.Lighting
	e.configMu.RUnlock()
	if lighting.Mode != "vu" && lighting.Mode != "spectrum" {
		return
	}
	frame.Level = math.Max(0, math.Min(1, frame.Level))
	if lighting.Mode == "spectrum" {
		e.device.SetLighting(spectrumLighting(lighting.VU, frame.Bands))
	} else {
		e.device.SetLighting(vuLighting(lighting.VU, frame.Level))
	}
	e.statusMu.Lock()
	e.status.VULevel = frame.Level
	e.status.Spectrum = frame.Bands
	e.statusMu.Unlock()
}

func (e *Engine) onVUError(err error) {
	e.configMu.RLock()
	enabled := e.config.Lighting.Mode == "vu" || e.config.Lighting.Mode == "spectrum"
	e.configMu.RUnlock()
	e.statusMu.Lock()
	if !enabled || err == nil {
		e.status.VUError = ""
	} else {
		e.status.VUError = err.Error()
	}
	e.statusMu.Unlock()
}

func (e *Engine) Inject(event device.Event) {
	e.device.Inject(event)
}

func (e *Engine) TestOBS() error { return e.obs.Test() }

func (e *Engine) OBSInputs() ([]string, error) {
	raw, err := e.obs.Request("GetInputList", map[string]any{})
	if err != nil {
		return nil, err
	}
	var response struct {
		Inputs []struct {
			Name string `json:"inputName"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(response.Inputs))
	for _, input := range response.Inputs {
		names = append(names, input.Name)
	}
	return names, nil
}

func (e *Engine) OBSFilters(source string) ([]string, error) {
	raw, err := e.obs.Request("GetSourceFilterList", map[string]any{"sourceName": source})
	if err != nil {
		return nil, err
	}
	var response struct {
		Filters []struct {
			Name string `json:"filterName"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(response.Filters))
	for _, filter := range response.Filters {
		names = append(names, filter.Name)
	}
	return names, nil
}

func (e *Engine) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-e.work:
			var err error
			if j.event.Kind == "turn" {
				err = e.turn(j.cfg.Turn, j.event.Value)
			} else {
				err = e.press(j.cfg, j.knob)
			}
			if err != nil {
				e.setError(err.Error())
			} else {
				e.setError("")
			}
		}
	}
}

func (e *Engine) turn(action config.TurnAction, raw int) error {
	if action.Kind == "none" {
		return nil
	}
	fraction := float64(raw) / 255
	percent := action.MinPercent + fraction*(action.MaxPercent-action.MinPercent)
	switch action.Kind {
	case "shell":
		return shellcmd.Run(action.Command, raw)
	case "obs_input":
		// Los porcentajes configurados se interpretan como rango de dB para OBS:
		// 0% = -60 dB, 100% = 0 dB.
		db := -60 + math.Max(0, math.Min(100, percent))*0.6
		return e.obs.Call("SetInputVolume", map[string]any{"inputName": action.Target, "inputVolumeDb": db})
	case "obs_filter":
		value := action.MinValue + fraction*(action.MaxValue-action.MinValue)
		return e.obs.Call("SetSourceFilterSettings", map[string]any{
			"sourceName": action.Source,
			"filterName": action.Filter,
			"filterSettings": map[string]any{
				action.Setting: value,
			},
			"overlay": true,
		})
	}
	return e.audio.SetVolume(action.Kind, action.Target, percent)
}

func (e *Engine) press(knob config.Knob, index int) error {
	action := knob.Press
	switch action.Kind {
	case "none":
		return nil
	case "mute_turn":
		if knob.Turn.Kind == "obs_input" {
			return e.obs.Call("ToggleInputMute", map[string]any{"inputName": knob.Turn.Target})
		}
		if knob.Turn.Kind == "obs_filter" {
			return fmt.Errorf("OBS filter controls do not support automatic mute")
		}
		return e.audio.ToggleMute(knob.Turn.Kind, knob.Turn.Target)
	case "obs_scene":
		return e.obs.Call("SetCurrentProgramScene", map[string]any{"sceneName": action.Target})
	case "obs_toggle_record":
		return e.obs.Call("ToggleRecord", map[string]any{})
	case "obs_toggle_stream":
		return e.obs.Call("ToggleStream", map[string]any{})
	case "obs_toggle_input_mute":
		return e.obs.Call("ToggleInputMute", map[string]any{"inputName": action.Target})
	case "profile":
		e.configMu.RLock()
		switcher := e.switchProfile
		e.configMu.RUnlock()
		if switcher == nil {
			return fmt.Errorf("profile switching is not available")
		}
		return switcher(action.Target)
	case "application":
		path := strings.TrimSpace(action.Target)
		if path == "" {
			return fmt.Errorf("no desktop application selected")
		}
		now := time.Now()
		if now.Sub(e.lastAppLaunch[path]) < 3*time.Second {
			return nil
		}
		e.lastAppLaunch[path] = now
		return desktopapps.Launch(path)
	case "shell":
		return shellcmd.Run(action.Command, e.currentValue(index))
	default:
		return fmt.Errorf("unknown click action: %s", action.Kind)
	}
}

func (e *Engine) currentValue(index int) int {
	e.statusMu.RLock()
	defer e.statusMu.RUnlock()
	if index < 0 || index >= len(e.status.Values) {
		return 0
	}
	return e.status.Values[index]
}

func deviceLighting(cfg config.Config) device.Lighting {
	lighting := device.Lighting{GlobalBrightness: cfg.Lighting.GlobalBrightness}
	for i, knob := range cfg.Knobs {
		lighting.Knobs[i] = device.KnobLight{Color: knob.Light.Color, TrackValue: knob.Light.TrackValue}
	}
	return lighting
}

func vuLighting(cfg config.VU, level float64) device.Lighting {
	var levels [4]float64
	for i := range levels {
		levels[i] = math.Max(0, math.Min(1, level*4-float64(i)))
	}
	return reactiveLighting(cfg, levels)
}

func spectrumLighting(cfg config.VU, levels [4]float64) device.Lighting {
	for i := range levels {
		levels[i] = math.Max(0, math.Min(1, levels[i]))
	}
	return reactiveLighting(cfg, levels)
}

func reactiveLighting(cfg config.VU, levels [4]float64) device.Lighting {
	lighting := device.Lighting{GlobalBrightness: cfg.Brightness}
	minColor := parseColor(cfg.MinColor)
	maxColor := parseColor(cfg.MaxColor)
	for i := range lighting.Knobs {
		mix := float64(i) / 3
		var rgb [3]uint8
		for channel := range rgb {
			value := float64(minColor[channel]) + (float64(maxColor[channel])-float64(minColor[channel]))*mix
			rgb[channel] = uint8(math.Round(value * levels[i]))
		}
		lighting.Knobs[i] = device.KnobLight{Color: fmt.Sprintf("#%02x%02x%02x", rgb[0], rgb[1], rgb[2])}
	}
	return lighting
}

func parseColor(color string) [3]uint8 {
	if len(color) != 7 || color[0] != '#' {
		return [3]uint8{}
	}
	value, err := strconv.ParseUint(color[1:], 16, 24)
	if err != nil {
		return [3]uint8{}
	}
	return [3]uint8{uint8(value >> 16), uint8(value >> 8), uint8(value)}
}

func (e *Engine) knobConfig(index int) config.Knob {
	e.configMu.RLock()
	defer e.configMu.RUnlock()
	if index < 0 || index >= len(e.config.Knobs) {
		return config.Knob{}
	}
	return e.config.Knobs[index]
}

func (e *Engine) record(event device.Event) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	if event.Knob >= 0 && event.Knob < len(e.status.Values) && event.Kind == "turn" {
		e.status.Values[event.Knob] = event.Value
	}
	if event.Initial {
		e.status.LastEvent = fmt.Sprintf("initial knob %d position: %d", event.Knob+1, event.Value)
	} else {
		e.status.LastEvent = fmt.Sprintf("%s knob %d: %d", event.Kind, event.Knob+1, event.Value)
	}
}

func (e *Engine) setError(message string) {
	e.statusMu.Lock()
	e.status.LastError = message
	e.statusMu.Unlock()
}
