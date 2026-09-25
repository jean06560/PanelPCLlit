package device

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type Event struct {
	Kind    string `json:"kind"`
	Knob    int    `json:"knob"`
	Value   int    `json:"value"`
	Initial bool   `json:"initial,omitempty"`
}

type Status struct {
	Connected bool   `json:"connected"`
	Device    string `json:"device,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Lighting struct {
	GlobalBrightness int
	Knobs            [4]KnobLight
}

type KnobLight struct {
	Color      string
	TrackValue bool
}

type Manager struct {
	events   chan Event
	lighting chan Lighting
	mu       sync.RWMutex
	status   Status
	desired  Lighting
	// resumeCheck and bootClock are fields so tests can simulate a suspend.
	resumeCheck time.Duration
	bootClock   func() (time.Duration, error)
}

const (
	// A USB reset during resume clears the lighting without closing the hidraw
	// node, so the only sign is time that passed while the system slept.
	resumeCheckInterval = 2 * time.Second
	suspendThreshold    = time.Second
	writeTimeout        = 150 * time.Millisecond
)

const productMini = "mini"

var supported = map[[2]uint16]string{
	{0x0483, 0xa3c4}: productMini,
	{0x0483, 0xa3c5}: "pro",
	{0x04d8, 0xeb52}: "rgb",
}

func NewManager() *Manager {
	return &Manager{
		events:      make(chan Event, 32),
		lighting:    make(chan Lighting, 1),
		resumeCheck: resumeCheckInterval,
		bootClock:   bootTime,
	}
}

func (m *Manager) Events() <-chan Event { return m.events }

// Inject feeds the same bounded channel as the hardware. The web interface uses
// it to test a configuration without requiring a connected PCPanel.
func (m *Manager) Inject(event Event) { m.emit(event) }

// SetLighting retains only the latest RGB configuration. The output channel has
// capacity one, like the action queue, to prevent a USB backlog.
func (m *Manager) SetLighting(lighting Lighting) {
	m.mu.Lock()
	m.desired = lighting
	m.mu.Unlock()
	select {
	case <-m.lighting:
	default:
	}
	select {
	case m.lighting <- lighting:
	default:
	}
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *Manager) setStatus(s Status) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

func (m *Manager) emit(event Event) {
	select {
	case m.events <- event:
	default:
		// Physical input must never block the HID reader. If the queue is full,
		// future events are retained because they carry the absolute value.
	}
}

func (m *Manager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		path, name, product, err := findDevice()
		if err != nil {
			m.setStatus(Status{Error: err.Error()})
			if !sleepContext(ctx, 2*time.Second) {
				return
			}
			continue
		}
		if path == "" {
			m.setStatus(Status{})
			if !sleepContext(ctx, 2*time.Second) {
				return
			}
			continue
		}

		// hidraw supports poll, so os.File parks the reader in the runtime poller
		// instead of spinning on EAGAIN.
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			m.setStatus(Status{Device: name, Error: err.Error()})
			if !sleepContext(ctx, 2*time.Second) {
				return
			}
			continue
		}
		m.setStatus(Status{Connected: true, Device: name})
		err = m.serve(ctx, f, product)
		if ctx.Err() != nil {
			return
		}
		m.setStatus(Status{Device: name, Error: err.Error()})
		sleepContext(ctx, time.Second)
	}
}

// initialize sends the startup report and the current lighting. The firmware
// forgets both when it is reset, on connection or after a suspend.
func (m *Manager) initialize(f *os.File, product string) error {
	if err := writeReport(f, []byte{1}); err != nil {
		return fmt.Errorf("initialize HID: %w", err)
	}
	if product != productMini {
		return nil
	}
	m.mu.RLock()
	lighting := m.desired
	m.mu.RUnlock()
	if err := writeReport(f, BuildMiniLightingReport(lighting)); err != nil {
		return fmt.Errorf("configure RGB: %w", err)
	}
	return nil
}

// serve owns every write to the device while a separate goroutine blocks on
// reads. It closes f before returning, which is what interrupts that read.
func (m *Manager) serve(ctx context.Context, f *os.File, product string) error {
	if err := f.SetReadDeadline(time.Time{}); err != nil {
		f.Close()
		return fmt.Errorf("%s is not pollable: %w", f.Name(), err)
	}
	if err := m.initialize(f, product); err != nil {
		f.Close()
		return err
	}
	readDone := make(chan error, 1)
	go func() { readDone <- m.readLoop(f) }()
	stop := func(err error) error {
		f.Close()
		<-readDone
		return err
	}
	resume := newSuspendDetector(m.bootClock)
	ticker := time.NewTicker(m.resumeCheck)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return stop(ctx.Err())
		case err := <-readDone:
			f.Close()
			return err
		case lighting := <-m.lighting:
			if product == productMini {
				if err := writeReport(f, BuildMiniLightingReport(lighting)); err != nil {
					return stop(fmt.Errorf("write RGB: %w", err))
				}
			}
		case <-ticker.C:
			if resume.suspended() {
				log.Printf("resume detected, reinitializing %s", f.Name())
				if err := m.initialize(f, product); err != nil {
					return stop(err)
				}
			}
		}
	}
}

func (m *Manager) readLoop(f *os.File) error {
	buf := make([]byte, 64)
	connectedAt := time.Now()
	var last [4]int
	var seen [4]bool
	for {
		n, err := f.Read(buf)
		if err != nil {
			return err
		}
		event, ok := ParseReport(buf[:n])
		if !ok {
			continue
		}
		// The firmware announces positions when connected. Use them as a baseline
		// and do not execute actions during a short initialization window.
		if time.Since(connectedAt) < 400*time.Millisecond {
			if event.Kind == "turn" && event.Knob < len(last) {
				last[event.Knob], seen[event.Knob] = event.Value, true
				event.Initial = true
				m.emit(event)
			}
			continue
		}
		if event.Kind == "turn" {
			if event.Knob >= len(last) {
				continue
			}
			if seen[event.Knob] && last[event.Knob] == event.Value {
				continue
			}
			last[event.Knob], seen[event.Knob] = event.Value, true
		}
		m.emit(event)
	}
}

func ParseReport(data []byte) (Event, bool) {
	if len(data) < 3 {
		return Event{}, false
	}
	knob := int(data[1])
	if knob < 0 || knob > 3 {
		return Event{}, false
	}
	switch data[0] {
	case 1:
		return Event{Kind: "turn", Knob: knob, Value: int(data[2])}, true
	case 2:
		return Event{Kind: "press", Knob: knob, Value: int(data[2])}, true
	default:
		return Event{}, false
	}
}

func findDevice() (path, name, product string, err error) {
	entries, err := filepath.Glob("/sys/class/hidraw/hidraw*")
	if err != nil {
		return "", "", "", err
	}
	for _, entry := range entries {
		props, readErr := readUevent(filepath.Join(entry, "device", "uevent"))
		if readErr != nil {
			continue
		}
		parts := strings.Split(props["HID_ID"], ":")
		if len(parts) != 3 {
			continue
		}
		vid, errVID := strconv.ParseUint(parts[1], 16, 16)
		pid, errPID := strconv.ParseUint(parts[2], 16, 16)
		product, ok := supported[[2]uint16{uint16(vid), uint16(pid)}]
		if errVID != nil || errPID != nil || !ok {
			continue
		}
		return filepath.Join("/dev", filepath.Base(entry)), props["HID_NAME"], product, nil
	}
	return "", "", "", nil
}

func BuildMiniLightingReport(lighting Lighting) []byte {
	report := make([]byte, 64)
	report[0], report[1] = 6, 2 // Mini, custom knob lighting
	brightness := lighting.GlobalBrightness
	if brightness < 0 {
		brightness = 0
	}
	if brightness > 100 {
		brightness = 100
	}
	for i, light := range lighting.Knobs {
		offset := 2 + i*7
		r, g, b := parseColor(light.Color)
		r = r * brightness / 100
		g = g * brightness / 100
		b = b * brightness / 100
		if light.TrackValue {
			report[offset] = 2 // gradiente de volumen: negro -> color
			report[offset+4], report[offset+5], report[offset+6] = byte(r), byte(g), byte(b)
		} else {
			report[offset] = 1 // static color
			report[offset+1], report[offset+2], report[offset+3] = byte(r), byte(g), byte(b)
		}
	}
	return report
}

func parseColor(color string) (int, int, int) {
	if len(color) != 7 || color[0] != '#' {
		return 0, 0, 0
	}
	r, errR := strconv.ParseUint(color[1:3], 16, 8)
	g, errG := strconv.ParseUint(color[3:5], 16, 8)
	b, errB := strconv.ParseUint(color[5:7], 16, 8)
	if errR != nil || errG != nil || errB != nil {
		return 0, 0, 0
	}
	return int(r), int(g), int(b)
}

func writeReport(f *os.File, data []byte) error {
	report := make([]byte, 64)
	copy(report, data)
	if err := f.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	n, err := f.Write(report)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return errors.New("HID write timed out")
	}
	if err != nil {
		return err
	}
	if n != len(report) {
		return fmt.Errorf("partial HID write: %d of %d bytes", n, len(report))
	}
	return nil
}

// suspendDetector notices system sleep: CLOCK_MONOTONIC, which Go uses for
// time.Since, stops during suspend while CLOCK_BOOTTIME keeps counting.
type suspendDetector struct {
	bootClock func() (time.Duration, error)
	mono      time.Time
	boot      time.Duration
	ok        bool
}

func newSuspendDetector(bootClock func() (time.Duration, error)) *suspendDetector {
	d := &suspendDetector{bootClock: bootClock}
	d.suspended()
	return d
}

// suspended reports whether the system slept since the previous call.
func (d *suspendDetector) suspended() bool {
	mono := time.Now()
	boot, err := d.bootClock()
	if err != nil {
		d.ok = false
		return false
	}
	slept := d.ok && (boot-d.boot)-mono.Sub(d.mono) > suspendThreshold
	d.mono, d.boot, d.ok = mono, boot, true
	return slept
}

func bootTime() (time.Duration, error) {
	const clockBoottime = 7
	var ts syscall.Timespec
	_, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockBoottime, uintptr(unsafe.Pointer(&ts)), 0)
	if errno != 0 {
		return 0, errno
	}
	return time.Duration(ts.Nano()), nil
}

func readUevent(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			result[key] = value
		}
	}
	return result, scanner.Err()
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
