package player

import (
	"bytes"
	"testing"
	"time"
)

func TestNearestZeroPicksTheCrossingClosestToTheTarget(t *testing.T) {
	pcm := sinePCM(440, 100*time.Millisecond)
	frames := len(pcm) / frameBytes
	// A 440 Hz cycle is about 109 frames. A target on a peak should land
	// within half of that, on a sample whose channels sum to about zero.
	target := frames / 5
	at := nearestZero(pcm, target, seekSnapFrames)
	if at < 0 || at >= frames {
		t.Fatalf("zero frame %d is outside 0..%d", at, frames)
	}
	if absInt(at-target) > sampleRate/440/2+2 {
		t.Fatalf("zero frame %d is %d away from %d", at, absInt(at-target), target)
	}
	sum := sampleSum(pcm, at)
	if sum < 0 {
		sum = -sum
	}
	if sum > 2000 {
		t.Fatalf("sample at the crossing sums to %d", sum)
	}
}

func TestNearestZeroUsesTheQuietestSampleWhenNothingCrosses(t *testing.T) {
	pcm := make([]byte, 80*frameBytes)
	for i := range 80 {
		writeFrame(pcm, i, 1000, 1000)
	}
	writeFrame(pcm, 40, 1, 1)
	if got := nearestZero(pcm, 10, 80); got != 40 {
		t.Fatalf("quietest frame = %d, want 40", got)
	}
}

func writeFrame(pcm []byte, frame int, left, right int16) {
	off := frame * frameBytes
	pcm[off] = byte(uint16(left))
	pcm[off+1] = byte(uint16(left) >> 8)
	pcm[off+2] = byte(uint16(right))
	pcm[off+3] = byte(uint16(right) >> 8)
}

func TestSpoolJumpReadsFromTheNewFrame(t *testing.T) {
	s, err := openSpool()
	if err != nil {
		t.Fatal(err)
	}
	pcm := sinePCM(440, 200*time.Millisecond)
	done := make(chan struct{})
	go func() {
		s.copy(bytes.NewReader(pcm))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, frames := s.snapshot()
		if frames == int64(len(pcm)/frameBytes) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the spool did not take the buffer")
		}
		time.Sleep(time.Millisecond)
	}
	const frame = 4000
	at, ok := s.Zero(frame)
	if !ok {
		t.Fatal("no zero crossing in a written sine")
	}
	if !s.Jump(at) {
		t.Fatal("jump inside the written audio failed")
	}
	buf := make([]byte, frameBytes)
	n, err := s.Read(buf)
	if err != nil || n != frameBytes {
		t.Fatalf("read after jump = %d, %v", n, err)
	}
	sum := sampleSum(buf, 0)
	if sum < 0 {
		sum = -sum
	}
	if sum > 2000 {
		t.Fatalf("the landed sample sums to %d", sum)
	}
	<-done
	s.remove()
}

func TestJumpWithNothingPlayingDoesNothing(t *testing.T) {
	p := New(WithNullAudio())
	t.Cleanup(p.Stop)
	if p.Jump(time.Second) {
		t.Fatal("a jump with nothing playing reported success")
	}
}
