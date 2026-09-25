package engine

import (
	"testing"
	"time"

	"panelpc/internal/config"
	"panelpc/internal/device"
)

func TestVULightingSegments(t *testing.T) {
	cfg := config.VU{MinColor: "#00ff00", MaxColor: "#ff0000", Brightness: 75}
	lighting := vuLighting(cfg, 0.375)
	if lighting.GlobalBrightness != 75 {
		t.Fatalf("brightness = %d", lighting.GlobalBrightness)
	}
	if lighting.Knobs[0].Color != "#00ff00" || lighting.Knobs[1].Color != "#2b5500" || lighting.Knobs[2].Color != "#000000" {
		t.Fatalf("unexpected segments: %#v", lighting.Knobs)
	}
}

func TestVULightingGradientAtFullScale(t *testing.T) {
	cfg := config.VU{MinColor: "#00ff00", MaxColor: "#ff0000", Brightness: 100}
	lighting := vuLighting(cfg, 1)
	if lighting.Knobs[0].Color != "#00ff00" || lighting.Knobs[3].Color != "#ff0000" {
		t.Fatalf("unexpected gradient: %#v", lighting.Knobs)
	}
}

func TestSpectrumLightingUsesIndependentBands(t *testing.T) {
	cfg := config.VU{MinColor: "#00ff00", MaxColor: "#ff0000", Brightness: 100}
	lighting := spectrumLighting(cfg, [4]float64{1, 0, 0.5, 0})
	if lighting.Knobs[0].Color != "#00ff00" || lighting.Knobs[1].Color != "#000000" || lighting.Knobs[2].Color != "#552b00" || lighting.Knobs[3].Color != "#000000" {
		t.Fatalf("unexpected bands: %#v", lighting.Knobs)
	}
}

func turnJob(knob, value int, kind string, rateMS int) job {
	cfg := config.Knob{}
	cfg.Turn.Kind = kind
	cfg.Turn.RateMS = rateMS
	return job{knob: knob, event: device.Event{Kind: "turn", Knob: knob, Value: value}, cfg: cfg}
}

func receive(t *testing.T, work chan job) (job, bool) {
	t.Helper()
	select {
	case j := <-work:
		return j, true
	default:
		return job{}, false
	}
}

func TestTurnSchedulerIsIdleWithoutTurns(t *testing.T) {
	s := newTurnScheduler(turnInterval)
	if s.C() != nil {
		t.Fatal("an idle scheduler must not run a timer")
	}
}

func TestTurnSchedulerCoalescesAndGoesIdle(t *testing.T) {
	s := newTurnScheduler(turnInterval)
	defer s.stop()
	work := make(chan job, 1)
	now := time.Now()

	s.add(turnJob(0, 10, "volume", 0), work, now)
	if j, ok := receive(t, work); !ok || j.event.Value != 10 {
		t.Fatalf("first turn = %v, %v; want immediate dispatch of 10", j.event.Value, ok)
	}
	if s.C() == nil {
		t.Fatal("the interval must start after a dispatch")
	}

	s.add(turnJob(0, 20, "volume", 0), work, now)
	s.add(turnJob(0, 30, "volume", 0), work, now)
	if _, ok := receive(t, work); ok {
		t.Fatal("turns inside the interval must wait for the tick")
	}

	s.tick(work, now.Add(turnInterval))
	if j, ok := receive(t, work); !ok || j.event.Value != 30 {
		t.Fatalf("tick dispatched %v, %v; want only the latest value 30", j.event.Value, ok)
	}

	s.tick(work, now.Add(2*turnInterval))
	if s.C() != nil {
		t.Fatal("the timer must stop once nothing is pending")
	}
}

func TestTurnSchedulerKeepsTurnWhileWorkerIsBusy(t *testing.T) {
	s := newTurnScheduler(turnInterval)
	defer s.stop()
	work := make(chan job, 1)
	work <- turnJob(3, 0, "volume", 0) // the worker has not taken this one yet
	now := time.Now()

	s.add(turnJob(1, 50, "volume", 0), work, now)
	<-work
	s.tick(work, now.Add(turnInterval))
	if j, ok := receive(t, work); !ok || j.event.Value != 50 {
		t.Fatalf("pending turn = %v, %v; want 50 once the queue has room", j.event.Value, ok)
	}
}

func TestTurnSchedulerRespectsShellRate(t *testing.T) {
	s := newTurnScheduler(turnInterval)
	defer s.stop()
	work := make(chan job, 1)
	now := time.Now()

	s.add(turnJob(2, 1, "shell", 200), work, now)
	if _, ok := receive(t, work); !ok {
		t.Fatal("first shell turn must run immediately")
	}
	s.add(turnJob(2, 2, "shell", 200), work, now)
	s.tick(work, now.Add(100*time.Millisecond))
	if _, ok := receive(t, work); ok {
		t.Fatal("shell turn ran before its minimum interval")
	}
	if s.C() == nil {
		t.Fatal("a deferred shell turn must keep the timer running")
	}
	s.tick(work, now.Add(200*time.Millisecond))
	if j, ok := receive(t, work); !ok || j.event.Value != 2 {
		t.Fatalf("shell turn = %v, %v; want 2 after the interval", j.event.Value, ok)
	}
}
