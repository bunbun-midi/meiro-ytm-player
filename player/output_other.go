//go:build !windows

package player

func openOutput() (device, error) { return openOto() }

// OutputDevices lists playback endpoints. This platform keeps oto's default
// device, so there is nothing to choose.
func OutputDevices() ([]OutputDevice, error) { return nil, nil }
