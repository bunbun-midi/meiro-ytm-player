package player

import "sync/atomic"

// OutputDevice is one playback endpoint. An empty ID means the system default.
type OutputDevice struct {
	ID   string
	Name string
}

// outputSwitcher can retarget an open device without dropping the streams
// already playing through it.
type outputSwitcher interface {
	UseDevice(id string) error
}

// wantedOutput is the endpoint the next open should use. Empty follows the
// system default. Every Player shares one device, so the choice is
// process-wide. The value is always a string once Store has run.
var wantedOutput atomic.Value

// UseOutputDevice plays through the endpoint id. An empty id follows the
// device the system is using. The choice is remembered before the device
// opens, and an open device is retargeted when it can be. A failed retarget
// leaves the previous endpoint playing.
func (p *Player) UseOutputDevice(id string) error {
	previous, _ := wantedOutput.Load().(string)
	wantedOutput.Store(id)
	if p == nil || p.audio == nil || !p.audio.opened() {
		return nil
	}
	dev, err := p.audio.context()
	if err != nil || dev == nil {
		wantedOutput.Store(previous)
		return err
	}
	switcher, ok := dev.(outputSwitcher)
	if !ok {
		return nil
	}
	if err := switcher.UseDevice(id); err != nil {
		wantedOutput.Store(previous)
		return err
	}
	return nil
}

// OutputDeviceID is the endpoint UseOutputDevice last stored. Empty means
// the system default.
func (p *Player) OutputDeviceID() string {
	id, _ := wantedOutput.Load().(string)
	return id
}
