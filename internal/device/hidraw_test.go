package device

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestParseReport(t *testing.T) {
	tests := []struct {
		data []byte
		want Event
		ok   bool
	}{
		{[]byte{1, 2, 255}, Event{Kind: "turn", Knob: 2, Value: 255}, true},
		{[]byte{2, 0, 1}, Event{Kind: "press", Knob: 0, Value: 1}, true},
		{[]byte{1, 4, 10}, Event{}, false},
		{[]byte{9, 0, 0}, Event{}, false},
		{[]byte{1, 0}, Event{}, false},
	}
	for _, test := range tests {
		got, ok := ParseReport(test.data)
		if ok != test.ok || got != test.want {
			t.Errorf("ParseReport(%v) = %#v, %v; want %#v, %v", test.data, got, ok, test.want, test.ok)
		}
	}
}

func TestInjectIsBounded(t *testing.T) {
	m := NewManager()
	for i := 0; i < 1000; i++ {
		m.Inject(Event{Kind: "turn", Knob: 0, Value: i % 256})
	}
	if got := len(m.events); got != cap(m.events) {
		t.Fatalf("queue = %d, capacity = %d", got, cap(m.events))
	}
}

func TestMiniLightingReport(t *testing.T) {
	lighting := Lighting{GlobalBrightness: 50}
	lighting.Knobs[0] = KnobLight{Color: "#ff8040"}
	lighting.Knobs[1] = KnobLight{Color: "#00ff00", TrackValue: true}
	report := BuildMiniLightingReport(lighting)
	if len(report) != 64 {
		t.Fatalf("length = %d", len(report))
	}
	wantStatic := []byte{6, 2, 1, 127, 64, 32, 0, 0, 0}
	for i, want := range wantStatic {
		if report[i] != want {
			t.Fatalf("report[%d] = %d, want %d; report=%v", i, report[i], want, report[:16])
		}
	}
	offset := 2 + 7
	wantTrack := []byte{2, 0, 0, 0, 0, 127, 0}
	for i, want := range wantTrack {
		if report[offset+i] != want {
			t.Fatalf("track[%d] = %d, want %d", i, report[offset+i], want)
		}
	}
}

func TestLightingQueueKeepsLatest(t *testing.T) {
	m := NewManager()
	for i := 0; i < 100; i++ {
		m.SetLighting(Lighting{GlobalBrightness: i})
	}
	if got := len(m.lighting); got != 1 {
		t.Fatalf("RGB queue = %d, want 1", got)
	}
	if got := (<-m.lighting).GlobalBrightness; got != 99 {
		t.Fatalf("brightness = %d, want 99", got)
	}
}

// fakeBootClock follows the real monotonic clock plus a controllable offset,
// which stands for time spent suspended.
type fakeBootClock struct {
	start time.Time
	slept atomic.Int64
}

func newFakeBootClock() *fakeBootClock { return &fakeBootClock{start: time.Now()} }

func (c *fakeBootClock) now() (time.Duration, error) {
	return time.Since(c.start) + time.Duration(c.slept.Load()), nil
}

func (c *fakeBootClock) sleep(d time.Duration) { c.slept.Add(int64(d)) }

func TestSuspendDetector(t *testing.T) {
	clock := newFakeBootClock()
	d := newSuspendDetector(clock.now)
	if d.suspended() {
		t.Fatal("suspend reported without sleeping")
	}
	clock.sleep(10 * time.Second)
	if !d.suspended() {
		t.Fatal("10 s suspend not detected")
	}
	if d.suspended() {
		t.Fatal("the same suspend was reported twice")
	}
	clock.sleep(suspendThreshold / 2)
	if d.suspended() {
		t.Fatal("a gap below the threshold was reported")
	}
}

func TestSuspendDetectorIgnoresClockErrors(t *testing.T) {
	fail := true
	d := newSuspendDetector(func() (time.Duration, error) {
		if fail {
			return 0, errors.New("no clock")
		}
		return time.Hour, nil
	})
	if d.suspended() {
		t.Fatal("suspend reported while the clock fails")
	}
	fail = false
	if d.suspended() {
		t.Fatal("recovering clock was mistaken for a suspend")
	}
}

func TestBootTimeAdvances(t *testing.T) {
	first, err := bootTime()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := bootTime()
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("CLOCK_BOOTTIME did not advance: %v then %v", first, second)
	}
}

// fakeDevice returns a pollable packet socket standing in for the hidraw node,
// and the peer end that plays the firmware.
func fakeDevice(t *testing.T) (device, firmware *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if err := syscall.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
	}
	device, firmware = os.NewFile(uintptr(fds[0]), "hidraw-test"), os.NewFile(uintptr(fds[1]), "firmware")
	t.Cleanup(func() { firmware.Close() })
	return device, firmware
}

func readPacket(t *testing.T, f *os.File) []byte {
	t.Helper()
	buf := make([]byte, 128)
	f.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("read from device: %v", err)
	}
	return buf[:n]
}

func startServe(t *testing.T, m *Manager, device *os.File) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.serve(ctx, device, productMini) }()
	t.Cleanup(cancel)
	return cancel, done
}

func waitServe(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return")
		return nil
	}
}

func TestServeInitializesAndForwardsLighting(t *testing.T) {
	m := NewManager()
	m.SetLighting(Lighting{GlobalBrightness: 100, Knobs: [4]KnobLight{{Color: "#ff0000"}}})
	<-m.lighting // already covered by the initial configuration
	device, firmware := fakeDevice(t)
	cancel, done := startServe(t, m, device)

	if got := readPacket(t, firmware); len(got) != 64 || got[0] != 1 {
		t.Fatalf("first report = %v, want 64-byte initialization", got[:4])
	}
	if got := readPacket(t, firmware); got[0] != 6 || got[3] != 255 {
		t.Fatalf("second report = %v, want stored lighting", got[:6])
	}

	m.SetLighting(Lighting{GlobalBrightness: 100, Knobs: [4]KnobLight{{Color: "#00ff00"}}})
	if got := readPacket(t, firmware); got[0] != 6 || got[4] != 255 {
		t.Fatalf("lighting update = %v", got[:6])
	}

	time.Sleep(450 * time.Millisecond) // presses are ignored during the connection window
	if _, err := firmware.Write([]byte{2, 1, 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-m.Events():
		if event != (Event{Kind: "press", Knob: 1, Value: 1}) {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("press was not forwarded")
	}

	cancel()
	if err := waitServe(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("serve = %v, want context.Canceled", err)
	}
}

func TestServeReinitializesAfterSuspend(t *testing.T) {
	m := NewManager()
	m.SetLighting(Lighting{GlobalBrightness: 100, Knobs: [4]KnobLight{{Color: "#0000ff"}}})
	<-m.lighting
	clock := newFakeBootClock()
	m.bootClock = clock.now
	m.resumeCheck = 10 * time.Millisecond
	device, firmware := fakeDevice(t)
	startServe(t, m, device)
	readPacket(t, firmware)
	readPacket(t, firmware)

	clock.sleep(30 * time.Second)
	if got := readPacket(t, firmware); got[0] != 1 {
		t.Fatalf("after resume = %v, want initialization", got[:4])
	}
	if got := readPacket(t, firmware); got[0] != 6 || got[5] != 255 {
		t.Fatalf("after resume = %v, want stored lighting", got[:6])
	}
}

func TestServeReturnsWhenDeviceDisappears(t *testing.T) {
	m := NewManager()
	device, firmware := fakeDevice(t)
	_, done := startServe(t, m, device)
	readPacket(t, firmware)
	readPacket(t, firmware)
	firmware.Close()
	if err := waitServe(t, done); !errors.Is(err, io.EOF) {
		t.Fatalf("serve = %v, want io.EOF", err)
	}
}
