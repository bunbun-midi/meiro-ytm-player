//go:build windows && (amd64 || arm64)

package player

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestWaveFormatMatchesTheWindowsLayout(t *testing.T) {
	if unsafe.Sizeof(waveFormatExtensible{}) != 40 {
		t.Fatalf("WAVEFORMATEXTENSIBLE is %d bytes", unsafe.Sizeof(waveFormatExtensible{}))
	}
	if unsafe.Sizeof(propVariant{}) != 24 {
		t.Fatalf("PROPVARIANT is %d bytes", unsafe.Sizeof(propVariant{}))
	}
}

func TestOutputDevicesListsActiveRenderDevices(t *testing.T) {
	list, err := OutputDevices()
	if err != nil {
		if strings.Contains(err.Error(), "0x80070490") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("no output devices")
	}
	for _, d := range list {
		if d.ID == "" || d.Name == "" {
			t.Fatalf("blank device: %+v", d)
		}
	}
}

func TestWASAPIOpensTheDefaultDevice(t *testing.T) {
	dev, err := newWASAPIDevice("")
	if err != nil {
		if strings.Contains(err.Error(), "0x80070490") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := dev.Err(); err != nil {
		dev.close()
		t.Fatal(err)
	}
	dev.close()
}

func TestMixPlaysQueuedSamples(t *testing.T) {
	p := &wasapiPlayer{volume: 1, playing: true, readDone: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	// One frame: left 1000, right -1000.
	p.buf = []byte{0xe8, 0x03, 0x18, 0xfc}
	acc := make([]int32, 4)
	p.mix(acc)
	if acc[0] != 1000 || acc[1] != -1000 || acc[2] != 0 || acc[3] != 0 {
		t.Fatalf("mixed %v", acc)
	}
	if p.BufferedSize() != 0 {
		t.Fatalf("left %d bytes queued", p.BufferedSize())
	}

	p.buf = []byte{0xe8, 0x03, 0x18, 0xfc}
	p.paused = true
	acc = make([]int32, 2)
	p.mix(acc)
	if acc[0] != 0 || acc[1] != 0 || p.BufferedSize() != 4 {
		t.Fatalf("pause mixed %v and left %d", acc, p.BufferedSize())
	}
}
